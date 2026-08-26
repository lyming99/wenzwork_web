package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumAITerminalSessions         = 8
	maximumAITerminalSessionsPerOwner = 4
	maximumAITerminalInputBytes       = 32 << 10
	maximumAITerminalScrollbackBytes  = 1 << 20
	maximumAITerminalReadBytes        = 24 << 10
	maximumAITerminalProcessOutput    = 64 << 20
	maximumAITerminalMemoryBytes      = 512 << 20
	aiTerminalMaximumLifetime         = 2 * time.Hour
	aiTerminalStartupWait             = 2 * time.Second
	aiTerminalQuietPeriod             = 500 * time.Millisecond
)

type aiTerminalOwner struct {
	ProjectID      uuid.UUID
	ProjectRoot    string
	ConversationID string
	WorkspaceMode  string
}

func (owner aiTerminalOwner) matches(other aiTerminalOwner) bool {
	return owner.ProjectID == other.ProjectID && owner.ProjectRoot == other.ProjectRoot &&
		owner.ConversationID == other.ConversationID && owner.WorkspaceMode == other.WorkspaceMode
}

type aiTerminalOpenRequest struct {
	Owner           aiTerminalOwner
	Name            string
	CWD             string
	DisplayCWD      string
	Shell           string
	Launch          aiCommandSandboxLaunch
	NetworkHosts    []string
	Environment     []string
	MaximumLifetime time.Duration
	MaximumOutput   uint64
	MaximumMemory   uint64
}

type aiTerminalSessionView struct {
	SessionID      string         `json:"session_id"`
	Type           string         `json:"type"`
	Name           string         `json:"name,omitempty"`
	CWD            string         `json:"cwd"`
	PID            int            `json:"pid"`
	Status         map[string]any `json:"status"`
	SandboxStatus  string         `json:"sandbox_status"`
	NetworkAllowed bool           `json:"network_allowed"`
	NetworkHosts   []string       `json:"network_hosts,omitempty"`
}

type aiTerminalSendResult struct {
	Viewport      string         `json:"viewport"`
	WaitReason    string         `json:"wait_reason"`
	SessionStatus map[string]any `json:"session_status"`
	Truncated     bool           `json:"truncated"`
}

type aiTerminalReadResult struct {
	Text       string `json:"text"`
	TotalLines int    `json:"total_lines"`
	LineBegin  int    `json:"line_begin"`
	LineEnd    int    `json:"line_end"`
	Truncated  bool   `json:"truncated"`
}

type aiPersistentTerminalManager struct {
	supervisor *processSupervisor
	now        func() time.Time
	openMu     sync.Mutex

	mu       sync.Mutex
	sessions map[uuid.UUID]*aiPersistentTerminalSession
	opening  int
	closed   bool
	openWG   sync.WaitGroup
	wg       sync.WaitGroup
}

type aiPersistentTerminalSession struct {
	manager        *aiPersistentTerminalManager
	process        *supervisedProcess
	id             uuid.UUID
	owner          aiTerminalOwner
	name           string
	cwd            string
	shell          string
	sandboxStatus  string
	networkAllowed bool
	networkHosts   []string
	openedAt       time.Time

	mu            sync.Mutex
	running       bool
	closing       bool
	exitCode      int
	exitReason    string
	sendActive    bool
	output        []byte
	outputBase    uint64
	outputEnd     uint64
	outputDropped bool
	notify        chan struct{}
	done          chan struct{}
}

func newAIPersistentTerminalManager(supervisor *processSupervisor) *aiPersistentTerminalManager {
	return &aiPersistentTerminalManager{
		supervisor: supervisor,
		now:        func() time.Time { return time.Now().UTC() },
		sessions:   make(map[uuid.UUID]*aiPersistentTerminalSession),
	}
}

func (manager *aiPersistentTerminalManager) Open(ctx context.Context, request aiTerminalOpenRequest) (aiTerminalSessionView, string, error) {
	if manager == nil || manager.supervisor == nil || ctx.Err() != nil || request.Owner.ProjectID == uuid.Nil ||
		request.Owner.ConversationID == "" || request.Owner.ProjectRoot == "" || request.CWD == "" ||
		request.Shell == "" || len(request.Launch.Argv) == 0 ||
		request.Launch.SandboxMode != request.Owner.WorkspaceMode && !aiWorkspaceModeWider(request.Launch.SandboxMode, request.Owner.WorkspaceMode) ||
		len(request.Name) > 80 || !utf8.ValidString(request.Name) {
		return aiTerminalSessionView{}, "", firstError(ctx.Err(), errRPCInvalid)
	}
	manager.openMu.Lock()
	defer manager.openMu.Unlock()
	maximumLifetime := request.MaximumLifetime
	if maximumLifetime <= 0 {
		maximumLifetime = aiTerminalMaximumLifetime
	}
	maximumOutput := request.MaximumOutput
	if maximumOutput == 0 {
		maximumOutput = maximumAITerminalProcessOutput
	}
	maximumMemory := request.MaximumMemory
	if maximumMemory == 0 {
		maximumMemory = maximumAITerminalMemoryBytes
	}

	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return aiTerminalSessionView{}, "", errRPCCapability
	}
	stale := make([]*aiPersistentTerminalSession, 0)
	owned := 0
	activeTotal := 0
	for id, session := range manager.sessions {
		view := session.snapshot()
		if view.Status["kind"] == "exited" {
			delete(manager.sessions, id)
			continue
		}
		if session.owner.ConversationID == request.Owner.ConversationID && !session.owner.matches(request.Owner) {
			stale = append(stale, session)
			delete(manager.sessions, id)
			continue
		}
		activeTotal++
		if session.owner.matches(request.Owner) {
			owned++
			if request.Name != "" && session.name == request.Name {
				manager.mu.Unlock()
				return aiTerminalSessionView{}, "", errRPCRevision
			}
		}
	}
	if activeTotal+manager.opening >= maximumAITerminalSessions || owned >= maximumAITerminalSessionsPerOwner {
		manager.mu.Unlock()
		return aiTerminalSessionView{}, "", errRPCBusy
	}
	manager.opening++
	manager.openWG.Add(1)
	manager.mu.Unlock()
	defer manager.openWG.Done()
	for _, session := range stale {
		if _, err := manager.closeSession(session, "owner_scope_changed"); err != nil {
			manager.mu.Lock()
			manager.opening--
			manager.mu.Unlock()
			return aiTerminalSessionView{}, "", err
		}
	}

	validationRoot := request.Owner.ProjectRoot
	if request.Launch.SandboxMode == aiWorkspaceModeFullAccess {
		validationRoot = request.CWD
	}
	process, err := manager.supervisor.Start(processLaunchSpec{
		ProjectID: request.Owner.ProjectID, ProjectRoot: validationRoot, WorkingDirectory: request.Launch.WorkingDirectory,
		Argv: request.Launch.Argv, Environment: request.Environment, Rows: 24, Columns: 80,
		Limits: processResourceLimits{MaximumLifetime: maximumLifetime, MaximumMemoryBytes: maximumMemory, MaximumOutputBytes: maximumOutput},
	})
	if err != nil {
		manager.mu.Lock()
		manager.opening--
		manager.mu.Unlock()
		return aiTerminalSessionView{}, "", err
	}
	session := &aiPersistentTerminalSession{
		manager: manager, process: process, id: uuid.New(), owner: request.Owner, name: request.Name, cwd: request.DisplayCWD,
		shell: request.Shell, sandboxStatus: request.Launch.Status, networkAllowed: request.Launch.NetworkAllowed,
		networkHosts: append([]string(nil), request.NetworkHosts...), openedAt: manager.now(), running: true, exitCode: -1,
		notify: make(chan struct{}), done: make(chan struct{}),
	}
	manager.mu.Lock()
	manager.opening--
	if manager.closed {
		manager.mu.Unlock()
		manager.wg.Add(1)
		go session.run()
		_, closeErr := manager.closeSession(session, "manager_closed")
		return aiTerminalSessionView{}, "", firstError(closeErr, errRPCCapability)
	}
	manager.sessions[session.id] = session
	manager.wg.Add(1)
	manager.mu.Unlock()
	go session.run()

	cursor := session.outputCursor()
	reason, err := session.waitForOutput(ctx, cursor, aiTerminalStartupWait, false)
	if err != nil {
		_, _ = manager.closeSession(session, "open_cancelled")
		return aiTerminalSessionView{}, "", err
	}
	_ = reason
	motd, _ := session.outputAfter(0, maximumAITerminalReadBytes)
	return session.snapshot(), motd, nil
}

func (manager *aiPersistentTerminalManager) Inspect(owner aiTerminalOwner, sessionID uuid.UUID) (aiTerminalSessionView, error) {
	session, err := manager.owned(owner, sessionID)
	if err != nil {
		return aiTerminalSessionView{}, err
	}
	return session.snapshot(), nil
}

func (manager *aiPersistentTerminalManager) List(owner aiTerminalOwner) ([]aiTerminalSessionView, error) {
	if manager == nil {
		return nil, errRPCCapability
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil, errRPCCapability
	}
	result := make([]aiTerminalSessionView, 0)
	for _, session := range manager.sessions {
		if session.owner.matches(owner) {
			result = append(result, session.snapshot())
		}
	}
	manager.mu.Unlock()
	return result, nil
}

func (manager *aiPersistentTerminalManager) Reconcile(owner aiTerminalOwner) error {
	if manager == nil {
		return errRPCCapability
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return errRPCCapability
	}
	stale := make([]*aiPersistentTerminalSession, 0)
	for id, session := range manager.sessions {
		if session.owner.ConversationID == owner.ConversationID && !session.owner.matches(owner) {
			stale = append(stale, session)
			delete(manager.sessions, id)
		}
	}
	manager.mu.Unlock()
	var result error
	for _, session := range stale {
		_, err := manager.closeSession(session, "owner_scope_changed")
		result = errors.Join(result, err)
	}
	return result
}

func (manager *aiPersistentTerminalManager) CloseProject(projectID uuid.UUID, reason string) error {
	if manager == nil || projectID == uuid.Nil {
		return errRPCInvalid
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	stale := make([]*aiPersistentTerminalSession, 0)
	for id, session := range manager.sessions {
		if session.owner.ProjectID == projectID {
			stale = append(stale, session)
			delete(manager.sessions, id)
		}
	}
	manager.mu.Unlock()
	var result error
	for _, session := range stale {
		_, err := manager.closeSession(session, reason)
		result = errors.Join(result, err)
	}
	return result
}

func (manager *aiPersistentTerminalManager) Send(ctx context.Context, owner aiTerminalOwner, sessionID uuid.UUID, text string, submit bool, timeout time.Duration) (aiTerminalSendResult, error) {
	if len(text) > maximumAITerminalInputBytes || !utf8.ValidString(text) || timeout <= 0 || timeout > 2*time.Minute {
		return aiTerminalSendResult{}, errRPCInvalid
	}
	session, err := manager.owned(owner, sessionID)
	if err != nil {
		return aiTerminalSendResult{}, err
	}
	return session.send(ctx, text, submit, timeout)
}

func (manager *aiPersistentTerminalManager) Read(owner aiTerminalOwner, sessionID uuid.UUID, offset, count int) (aiTerminalReadResult, error) {
	if offset < 0 || count < 1 || count > 2000 {
		return aiTerminalReadResult{}, errRPCInvalid
	}
	session, err := manager.owned(owner, sessionID)
	if err != nil {
		return aiTerminalReadResult{}, err
	}
	return session.read(offset, count), nil
}

func (manager *aiPersistentTerminalManager) Signal(owner aiTerminalOwner, sessionID uuid.UUID, signal string) (map[string]any, error) {
	session, err := manager.owned(owner, sessionID)
	if err != nil {
		return nil, err
	}
	switch signal {
	case "SIGINT":
		if err := writeAITerminalInput(session.process, []byte{3}); err != nil {
			return nil, err
		}
	case "SIGTERM", "SIGHUP":
		if _, err := manager.closeSession(session, "model_"+signal); err != nil {
			return nil, err
		}
	default:
		return nil, errRPCInvalid
	}
	return map[string]any{"session_id": session.id.String(), "signal": signal, "delivered": true, "target_pid": session.process.Pid()}, nil
}

func (manager *aiPersistentTerminalManager) CloseSession(owner aiTerminalOwner, sessionID uuid.UUID) (bool, error) {
	session, err := manager.owned(owner, sessionID)
	if err != nil {
		return false, err
	}
	return manager.closeSession(session, "model_request")
}

func (manager *aiPersistentTerminalManager) closeSession(session *aiPersistentTerminalSession, reason string) (bool, error) {
	if session == nil {
		return false, errRPCNotFound
	}
	session.mu.Lock()
	started := !session.closing
	session.closing = true
	session.mu.Unlock()
	err := session.process.Close(reason)
	<-session.done
	manager.mu.Lock()
	if manager.sessions[session.id] == session {
		delete(manager.sessions, session.id)
	}
	manager.mu.Unlock()
	return started, err
}

func (manager *aiPersistentTerminalManager) owned(owner aiTerminalOwner, sessionID uuid.UUID) (*aiPersistentTerminalSession, error) {
	if manager == nil || sessionID == uuid.Nil {
		return nil, errRPCInvalid
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil, errRPCCapability
	}
	session := manager.sessions[sessionID]
	manager.mu.Unlock()
	if session == nil {
		return nil, errRPCNotFound
	}
	if !session.owner.matches(owner) {
		if session.owner.ConversationID == owner.ConversationID {
			go func() { _, _ = manager.closeSession(session, "owner_scope_changed") }()
		}
		return nil, errRPCForbidden
	}
	return session, nil
}

func (manager *aiPersistentTerminalManager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		manager.openWG.Wait()
		manager.wg.Wait()
		return nil
	}
	manager.closed = true
	manager.mu.Unlock()
	manager.openWG.Wait()
	manager.mu.Lock()
	sessions := make([]*aiPersistentTerminalSession, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	manager.mu.Unlock()
	var result error
	for _, session := range sessions {
		result = errors.Join(result, session.process.Close("executor_close"))
	}
	manager.wg.Wait()
	manager.mu.Lock()
	clear(manager.sessions)
	manager.mu.Unlock()
	return result
}

func (session *aiPersistentTerminalSession) run() {
	defer session.manager.wg.Done()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		decoder := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
		buffer := make([]byte, 8<<10)
		binaryNotice := false
		appendResults := func(results []CommandTextDecodeResult) {
			for _, result := range results {
				if result.IsBinary {
					if !binaryNotice {
						session.appendOutput("\n[binary terminal output omitted]\n")
						binaryNotice = true
					}
					continue
				}
				session.appendOutput(result.DisplayText)
			}
		}
		for {
			n, err := session.process.Read(buffer)
			if n > 0 {
				appendResults(decoder.Feed(buffer[:n]))
			}
			if err != nil {
				appendResults(decoder.Flush())
				return
			}
		}
	}()
	exitCode := session.process.Wait()
	select {
	case <-readDone:
	case <-time.After(750 * time.Millisecond):
		_ = session.process.Close("process_exit")
		<-readDone
	}
	_ = session.process.Close("process_exit")
	reason := session.process.reason()
	if reason == "" {
		reason = "process_exit"
	}
	session.mu.Lock()
	session.running = false
	session.exitCode = exitCode
	session.exitReason = reason
	close(session.notify)
	session.notify = make(chan struct{})
	session.mu.Unlock()
	session.process.release()
	close(session.done)
}

func (session *aiPersistentTerminalSession) appendOutput(text string) {
	if text == "" {
		return
	}
	data := []byte(text)
	session.mu.Lock()
	session.outputEnd += uint64(len(data))
	session.output = append(session.output, data...)
	if len(session.output) > maximumAITerminalScrollbackBytes {
		drop := len(session.output) - maximumAITerminalScrollbackBytes
		for drop < len(session.output) && !utf8.RuneStart(session.output[drop]) {
			drop++
		}
		session.output = append([]byte(nil), session.output[drop:]...)
		session.outputBase += uint64(drop)
		session.outputDropped = true
	}
	close(session.notify)
	session.notify = make(chan struct{})
	session.mu.Unlock()
}

func (session *aiPersistentTerminalSession) snapshot() aiTerminalSessionView {
	session.mu.Lock()
	defer session.mu.Unlock()
	status := map[string]any{"kind": "running"}
	if !session.running {
		status = map[string]any{"kind": "exited", "exit_code": session.exitCode, "reason": session.exitReason}
	}
	return aiTerminalSessionView{
		SessionID: session.id.String(), Type: "shell", Name: session.name, CWD: session.cwd, PID: session.process.Pid(),
		Status: status, SandboxStatus: session.sandboxStatus, NetworkAllowed: session.networkAllowed,
		NetworkHosts: append([]string(nil), session.networkHosts...),
	}
}

func (session *aiPersistentTerminalSession) outputCursor() uint64 {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.outputEnd
}

func (session *aiPersistentTerminalSession) outputAfter(cursor uint64, maximum int) (string, bool) {
	session.mu.Lock()
	defer session.mu.Unlock()
	truncated := cursor < session.outputBase
	if cursor < session.outputBase {
		cursor = session.outputBase
	}
	if cursor > session.outputEnd {
		cursor = session.outputEnd
	}
	start := int(cursor - session.outputBase)
	data := append([]byte(nil), session.output[start:]...)
	if len(data) > maximum {
		data = trimAITerminalUTF8Tail(data, maximum)
		truncated = true
	}
	return string(data), truncated
}

func (session *aiPersistentTerminalSession) send(ctx context.Context, text string, submit bool, timeout time.Duration) (aiTerminalSendResult, error) {
	session.mu.Lock()
	if !session.running || session.closing {
		session.mu.Unlock()
		return aiTerminalSendResult{}, errRPCRevision
	}
	if session.sendActive {
		session.mu.Unlock()
		return aiTerminalSendResult{}, errRPCBusy
	}
	session.sendActive = true
	cursor := session.outputEnd
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.sendActive = false
		session.mu.Unlock()
	}()
	payload := []byte(text)
	if submit {
		payload = append(payload, '\r')
	}
	if err := writeAITerminalInput(session.process, payload); err != nil {
		return aiTerminalSendResult{}, err
	}
	reason, err := session.waitForOutput(ctx, cursor, timeout, true)
	if err != nil {
		return aiTerminalSendResult{}, err
	}
	viewport, truncated := session.outputAfter(cursor, maximumAITerminalReadBytes)
	view := session.snapshot()
	return aiTerminalSendResult{Viewport: viewport, WaitReason: reason, SessionStatus: view.Status, Truncated: truncated}, nil
}

func (session *aiPersistentTerminalSession) waitForOutput(ctx context.Context, cursor uint64, maximum time.Duration, interruptOnCancel bool) (string, error) {
	deadline := time.NewTimer(maximum)
	defer deadline.Stop()
	for {
		session.mu.Lock()
		running, notify := session.running, session.notify
		session.mu.Unlock()
		if !running {
			return "session_exit", nil
		}
		quiet := time.NewTimer(aiTerminalQuietPeriod)
		select {
		case <-notify:
			if !quiet.Stop() {
				<-quiet.C
			}
			cursor = session.outputCursor()
			_ = cursor
			continue
		case <-quiet.C:
			return "inferred_idle", nil
		case <-deadline.C:
			if !quiet.Stop() {
				<-quiet.C
			}
			return "timeout", nil
		case <-ctx.Done():
			if !quiet.Stop() {
				<-quiet.C
			}
			if interruptOnCancel {
				_ = writeAITerminalInput(session.process, []byte{3})
			}
			return "", ctx.Err()
		}
	}
}

func (session *aiPersistentTerminalSession) read(offset, count int) aiTerminalReadResult {
	session.mu.Lock()
	contents := string(append([]byte(nil), session.output...))
	dropped := session.outputDropped
	session.mu.Unlock()
	if contents == "" {
		return aiTerminalReadResult{LineBegin: offset, LineEnd: offset, Truncated: dropped}
	}
	lines := strings.Split(contents, "\n")
	total := len(lines)
	if offset >= total {
		return aiTerminalReadResult{TotalLines: total, LineBegin: total, LineEnd: total, Truncated: dropped}
	}
	end := total - offset
	start := end - count
	if start < 0 {
		start = 0
	}
	data := []byte(strings.Join(lines[start:end], "\n"))
	truncated := dropped
	lineBegin := start
	if len(data) > maximumAITerminalReadBytes {
		data = trimAITerminalUTF8Tail(data, maximumAITerminalReadBytes)
		truncated = true
		returnedLines := 0
		if len(data) > 0 {
			returnedLines = strings.Count(string(data), "\n") + 1
		}
		lineBegin = end - returnedLines
	}
	return aiTerminalReadResult{Text: string(data), TotalLines: total, LineBegin: lineBegin, LineEnd: end, Truncated: truncated}
}

func writeAITerminalInput(process *supervisedProcess, data []byte) error {
	for len(data) > 0 {
		n, err := process.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func trimAITerminalUTF8Tail(data []byte, maximum int) []byte {
	if len(data) <= maximum {
		return data
	}
	start := len(data) - maximum
	for start < len(data) && !utf8.RuneStart(data[start]) {
		start++
	}
	return append([]byte(nil), data[start:]...)
}

func marshalAITerminalResult(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil || len(payload) > maximumAIWorkspaceToolResult {
		return "", firstError(err, fmt.Errorf("terminal result exceeds tool limit: %w", errRPCCapability))
	}
	return string(payload), nil
}
