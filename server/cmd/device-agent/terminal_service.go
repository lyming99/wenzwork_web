package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maximumTerminalSessions               = 8
	maximumTerminalSessionsPerProject     = 4
	maximumTerminalInputBytes             = 32 << 10
	maximumTerminalInputRateBytes         = 128 << 10
	maximumTerminalOutputRateBytes        = 512 << 10
	maximumInteractiveTerminalOutputBytes = 64 << 20
	maximumTerminalRingBytes              = 1 << 20
	maximumTerminalRingEvents             = 4096
	maximumTerminalMemoryBytes            = 512 << 20
	// Interactive terminals stay alive while the client keeps the terminal
	// Stream healthy. Zero advertises that byte-idle and fixed-lifetime expiry
	// are disabled for this process class.
	terminalIdleTimeout             time.Duration = 0
	terminalMaximumLifetime         time.Duration = 0
	terminalDisconnectGrace                       = 5 * time.Minute
	terminalDuplexKeepAliveInterval               = 2 * time.Minute
	terminalExitedRetention                       = 2 * time.Minute
	terminalCleanupInterval                       = 5 * time.Second
	terminalMaximumAttachWait                     = 30 * time.Second
	terminalDefaultAttachWait                     = 25 * time.Second
	terminalAttachTokensPerMinute                 = 6
	terminalAttachTokenBurst                      = 2
	terminalRecentInputRecords                    = 1024
	terminalRecentResizeRecords                   = 32
)

// Windows terminal clients send and render UTF-8.  Configure the shell before
// it begins accepting user input so the ConPTY byte stream has the same
// encoding.  This mirrors the local WenzMark/WenzWork terminal launch path.
const (
	// The supervised terminal environment is an explicit host snapshot rather
	// than implicit process inheritance. Do not rely on a bare `chcp` command
	// being discoverable through PATH: an otherwise healthy PowerShell session
	// would print a CommandNotFoundException before the user types anything.
	// Resolve the built-in executable explicitly when it exists, and still
	// configure .NET's UTF-8 streams when it does not.
	windowsCmdUTF8Bootstrap        = "if exist \"%SystemRoot%\\System32\\chcp.com\" \"%SystemRoot%\\System32\\chcp.com\" 65001 >nul"
	windowsPowerShellUTF8Bootstrap = "$__wenzworkChcp = [System.IO.Path]::Combine([System.Environment]::SystemDirectory, 'chcp.com'); " +
		"if ([System.IO.File]::Exists($__wenzworkChcp)) { & $__wenzworkChcp 65001 *> $null }; " +
		"$__wenzworkUtf8 = [System.Text.UTF8Encoding]::new($false); " +
		"[Console]::InputEncoding = $__wenzworkUtf8; " +
		"[Console]::OutputEncoding = $__wenzworkUtf8; " +
		"$OutputEncoding = $__wenzworkUtf8"
)

type terminalService struct {
	state      *agentState
	supervisor *processSupervisor
	now        func() time.Time
	shellArgv  func(string) ([]string, string, error)

	mu           sync.Mutex
	sessions     map[uuid.UUID]*terminalSession
	openRequests map[uuid.UUID]uuid.UUID
	closed       bool
	closeOnce    sync.Once
	closeCh      chan struct{}
	wg           sync.WaitGroup
}

type terminalSession struct {
	service         *terminalService
	process         *supervisedProcess
	id              uuid.UUID
	projectID       uuid.UUID
	projectRevision uint64
	shell           string
	cwd             string
	rows            uint16
	columns         uint16
	openedAt        time.Time

	mu               sync.Mutex
	running          bool
	exitCode         int
	exitReason       string
	exitedAt         time.Time
	lastAttachAt     time.Time
	sequence         uint64
	events           []terminalBufferedEvent
	ringBytes        uint64
	notify           chan struct{}
	inputSequence    uint64
	inputRecords     map[uint64][sha256.Size]byte
	resizeSequence   uint64
	resizeRecords    map[uint64]terminalSize
	inputRateWindow  time.Time
	inputRateBytes   uint64
	outputRateWindow time.Time
	outputRateBytes  uint64
	attachActive     bool
	attachTokens     float64
	attachTokenAt    time.Time
	duplexGeneration uint64
	duplexCancel     context.CancelFunc
}

type terminalSize struct {
	Rows    uint16
	Columns uint16
}

type terminalBufferedEvent struct {
	Sequence   uint64
	Kind       string
	Data       []byte
	ExitCode   int
	ExitReason string
	OccurredAt time.Time
}

type terminalEventBatch struct {
	Events        []terminalBufferedEvent
	FirstSequence uint64
	HighWatermark uint64
	ResetRequired bool
	Running       bool
	Notify        <-chan struct{}
}

func newTerminalService(state *agentState, supervisor *processSupervisor) *terminalService {
	service := &terminalService{
		state: state, supervisor: supervisor, now: func() time.Time { return time.Now().UTC() },
		shellArgv: terminalShellArgv,
		sessions:  make(map[uuid.UUID]*terminalSession), openRequests: make(map[uuid.UUID]uuid.UUID),
		closeCh: make(chan struct{}),
	}
	service.wg.Add(1)
	go func() {
		defer service.wg.Done()
		service.cleanupLoop()
	}()
	return service
}

func (service *terminalService) Open(project registeredProject, input rpcInput) (map[string]any, error) {
	return service.OpenContext(context.Background(), project, input)
}

func (service *terminalService) OpenContext(ctx context.Context, project registeredProject, input rpcInput) (map[string]any, error) {
	if service == nil || service.supervisor == nil || project.ID == uuid.Nil || project.State != "available" ||
		!project.Policy.AllowInteractiveTerminal || !agentFeatureFlags(service.state)["terminal.interactive"] {
		return nil, errRPCCapability
	}
	if !inputHasOnly(input, "clientRequestId", "cwd", "shell", "rows", "columns") {
		return nil, errRPCInvalid
	}
	requestText, ok := inputString(input, "clientRequestId", 80)
	requestID, parseErr := uuid.Parse(requestText)
	if !ok || parseErr != nil || requestID == uuid.Nil {
		return nil, errRPCInvalid
	}
	rows, okRows := terminalDimension(input, "rows", 24, 2, 500)
	columns, okColumns := terminalDimension(input, "columns", 80, 10, 1000)
	cwd, okCWD := optionalFilePathInput(input, "cwd")
	shell, okShell := optionalInputString(input, "shell", 32)
	if !okRows || !okColumns || !okCWD || !okShell {
		return nil, errRPCInvalid
	}
	workingDirectory, normalizedCWD, err := secureExistingProjectPath(project, cwd)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(workingDirectory)
	if err != nil || !info.IsDir() {
		return nil, errRPCNotFound
	}
	argv, shellName, err := service.shellArgv(shell)
	if err != nil {
		return nil, err
	}

	service.mu.Lock()
	if service.closed {
		service.mu.Unlock()
		return nil, errRPCCapability
	}
	if sessionID, found := service.openRequests[requestID]; found {
		session := service.sessions[sessionID]
		service.mu.Unlock()
		if session == nil || session.projectID != project.ID {
			return nil, errRPCRevision
		}
		if err := completeV2WithoutSideEffect(ctx); err != nil {
			return nil, err
		}
		return session.view(true), nil
	}
	activeTotal, activeForProject := 0, 0
	for _, session := range service.sessions {
		if session.isRunning() {
			activeTotal++
			if session.projectID == project.ID {
				activeForProject++
			}
		}
	}
	if activeTotal >= maximumTerminalSessions || activeForProject >= maximumTerminalSessionsPerProject {
		service.mu.Unlock()
		return nil, errRPCBusy
	}
	// Starting under the service lock keeps the count and clientRequestId
	// reservation atomic. The starter never calls back into terminalService.
	if err := beginV2SideEffect(ctx); err != nil {
		service.mu.Unlock()
		return nil, err
	}
	process, err := service.supervisor.Start(processLaunchSpec{
		ProjectID: project.ID, ProjectRoot: project.LocalPath, WorkingDirectory: workingDirectory,
		Argv: argv, InheritHostEnvironment: true, Rows: rows, Columns: columns,
		Limits: processResourceLimits{
			MaximumLifetime: terminalMaximumLifetime, MaximumMemoryBytes: maximumTerminalMemoryBytes,
			MaximumOutputBytes: maximumInteractiveTerminalOutputBytes,
		},
	})
	if err != nil {
		service.mu.Unlock()
		if rollbackErr := rollbackV2SideEffect(ctx); rollbackErr != nil {
			return nil, errors.Join(err, rollbackErr)
		}
		return nil, err
	}
	now := service.now()
	session := &terminalSession{
		service: service, process: process, id: uuid.New(), projectID: project.ID, projectRevision: project.Revision,
		shell: shellName, cwd: normalizedCWD, rows: rows, columns: columns, openedAt: now,
		running: true, exitCode: -1, lastAttachAt: now, notify: make(chan struct{}),
		attachTokens: terminalAttachTokenBurst, attachTokenAt: now,
		inputRecords: make(map[uint64][sha256.Size]byte), resizeRecords: make(map[uint64]terminalSize),
		inputRateWindow: now, outputRateWindow: now,
	}
	service.sessions[session.id] = session
	service.openRequests[requestID] = session.id
	service.mu.Unlock()
	if err := service.state.business.recordTerminalSessionOpened(context.WithoutCancel(ctx), terminalSessionAudit{
		SessionID: session.id, ProjectID: project.ID, OpenedAt: now,
	}); err != nil {
		_ = process.Close("audit_unavailable")
		process.release()
		service.mu.Lock()
		delete(service.sessions, session.id)
		delete(service.openRequests, requestID)
		service.mu.Unlock()
		return nil, err
	}
	if err := commitV2SideEffect(ctx); err != nil {
		_ = process.Close("operation_store_unavailable")
		process.release()
		service.mu.Lock()
		delete(service.sessions, session.id)
		delete(service.openRequests, requestID)
		service.mu.Unlock()
		return nil, err
	}
	service.wg.Add(1)
	go func() {
		defer service.wg.Done()
		session.run()
	}()
	return session.view(false), nil
}

func terminalShellArgv(requested string) ([]string, string, error) {
	available := availableShells()
	if requested == "" {
		if len(available) == 0 {
			return nil, "", errRPCCapability
		}
		requested = available[0]
	}
	found := false
	for _, candidate := range available {
		if candidate == requested {
			found = true
			break
		}
	}
	if !found {
		return nil, "", errRPCCapability
	}
	executable, err := resolveSupervisedExecutable(requested)
	if err != nil {
		return nil, "", err
	}
	arguments := append([]string{executable}, terminalShellArguments(requested)...)
	return arguments, requested, nil
}

func terminalShellArguments(requested string) []string {
	return terminalShellArgumentsForOS(requested, runtime.GOOS)
}

// terminalShellArgumentsForOS keeps Windows-only console initialization out of
// PowerShell sessions running on Unix.  pwsh is commonly installed on Linux
// and macOS too, where `chcp` does not exist and an unconditional bootstrap
// writes a CommandNotFoundException into every newly opened terminal.
//
// Keeping the operating-system branch in this small helper also lets tests
// cover both launch contracts independently of the host they run on.
func terminalShellArgumentsForOS(requested, operatingSystem string) []string {
	switch requested {
	case "pwsh", "powershell":
		arguments := []string{"-NoLogo", "-NoProfile", "-NoExit"}
		if operatingSystem == "windows" {
			arguments = append(arguments, "-Command", windowsPowerShellUTF8Bootstrap)
		}
		return arguments
	case "cmd":
		if operatingSystem == "windows" {
			return []string{"/D", "/Q", "/V:OFF", "/K", windowsCmdUTF8Bootstrap}
		}
		// `cmd` is never advertised outside Windows, but keep this pure helper
		// safe if a caller validates an unavailable shell before spawning it.
		return nil
	default:
		return []string{"-i"}
	}
}

func (session *terminalSession) run() {
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		session.readOutput()
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
	session.appendExit(exitCode, reason)
	session.process.release()
	_ = session.service.state.business.recordTerminalSessionFinished(context.Background(), terminalSessionAudit{
		SessionID: session.id, ProjectID: session.projectID, OpenedAt: session.openedAt,
		ClosedAt: session.exitedAt, InputBytes: session.process.inputBytes.Load(), OutputBytes: session.process.outputBytes.Load(),
		ExitCode: exitCode, CloseReason: reason,
	})
}

func (session *terminalSession) readOutput() {
	buffer := make([]byte, 8<<10)
	for {
		n, err := session.process.Read(buffer)
		if n > 0 {
			contents := append([]byte(nil), buffer[:n]...)
			if !session.appendOutput(contents) {
				_ = session.process.Close("output_rate_limit")
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && session.process.reason() == "" {
				_ = session.process.Close("pty_read_error")
			}
			return
		}
	}
}

func (session *terminalSession) appendOutput(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	now := session.service.now()
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.running {
		return false
	}
	if now.Sub(session.outputRateWindow) >= time.Second {
		session.outputRateWindow, session.outputRateBytes = now, 0
	}
	if session.outputRateBytes+uint64(len(data)) > maximumTerminalOutputRateBytes {
		return false
	}
	session.outputRateBytes += uint64(len(data))
	session.appendEventLocked(terminalBufferedEvent{Kind: "output", Data: data, OccurredAt: now})
	return true
}

func (session *terminalSession) appendExit(exitCode int, reason string) {
	now := session.service.now()
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.running {
		return
	}
	session.running, session.exitCode, session.exitReason, session.exitedAt = false, exitCode, reason, now
	session.appendEventLocked(terminalBufferedEvent{Kind: "exit", ExitCode: exitCode, ExitReason: reason, OccurredAt: now})
}

func (session *terminalSession) appendEventLocked(event terminalBufferedEvent) {
	session.sequence++
	event.Sequence = session.sequence
	session.events = append(session.events, event)
	session.ringBytes += terminalEventSize(event)
	for len(session.events) > maximumTerminalRingEvents || session.ringBytes > maximumTerminalRingBytes {
		session.ringBytes -= terminalEventSize(session.events[0])
		session.events[0].Data = nil
		session.events = session.events[1:]
	}
	close(session.notify)
	session.notify = make(chan struct{})
}

func terminalEventSize(event terminalBufferedEvent) uint64 {
	return uint64(len(event.Data) + len(event.ExitReason) + 64)
}

func (session *terminalSession) snapshotAfter(lastSequence uint64) (terminalEventBatch, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	first := session.sequence + 1
	if len(session.events) > 0 {
		first = session.events[0].Sequence
	}
	if lastSequence > session.sequence {
		return terminalEventBatch{}, errRPCRevision
	}
	reset := first > 0 && lastSequence < first-1
	batch := terminalEventBatch{
		FirstSequence: first, HighWatermark: session.sequence, ResetRequired: reset,
		Running: session.running, Notify: session.notify,
	}
	if reset {
		return batch, nil
	}
	for _, event := range session.events {
		if event.Sequence <= lastSequence {
			continue
		}
		copyEvent := event
		copyEvent.Data = append([]byte(nil), event.Data...)
		batch.Events = append(batch.Events, copyEvent)
	}
	return batch, nil
}

func (session *terminalSession) view(replayed bool) map[string]any {
	session.mu.Lock()
	defer session.mu.Unlock()
	first := session.sequence + 1
	if len(session.events) > 0 {
		first = session.events[0].Sequence
	}
	return map[string]any{
		"sessionId": session.id.String(), "projectId": session.projectID.String(), "projectRevision": session.projectRevision,
		"shell": session.shell, "cwd": session.cwd, "rows": session.rows, "columns": session.columns,
		"firstSequence": first, "highWatermark": session.sequence, "nextInputSequence": session.inputSequence + 1,
		"nextResizeSequence": session.resizeSequence + 1, "openedAt": session.openedAt,
		"expiresAt": nil, "idleExpiresAt": nil,
		"running": session.running, "exitCode": session.exitCode, "exitReason": session.exitReason, "replayed": replayed,
	}
}

func (session *terminalSession) isRunning() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.running
}

func (service *terminalService) cleanupLoop() {
	ticker := time.NewTicker(terminalCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-service.closeCh:
			return
		case <-ticker.C:
			service.cleanup(service.now())
		}
	}
}

func (service *terminalService) cleanup(now time.Time) {
	service.mu.Lock()
	sessions := make([]*terminalSession, 0, len(service.sessions))
	for _, session := range service.sessions {
		sessions = append(sessions, session)
	}
	service.mu.Unlock()
	for _, session := range sessions {
		project, err := service.state.business.projectByID(context.Background(), session.projectID)
		if err != nil || project.State != "available" || !project.Policy.AllowInteractiveTerminal ||
			!agentFeatureFlags(service.state)["terminal.interactive"] {
			_ = session.process.Close("policy_revoked")
		}
		session.mu.Lock()
		running := session.running
		disconnected := now.Sub(session.lastAttachAt) >= terminalDisconnectGrace
		exitedExpired := !session.exitedAt.IsZero() && now.Sub(session.exitedAt) >= terminalExitedRetention
		session.mu.Unlock()
		if running && disconnected {
			_ = session.process.Close("disconnect_timeout")
		} else if exitedExpired {
			service.removeSession(session.id)
		}
	}
}

func (service *terminalService) removeSession(sessionID uuid.UUID) {
	service.mu.Lock()
	delete(service.sessions, sessionID)
	for requestID, id := range service.openRequests {
		if id == sessionID {
			delete(service.openRequests, requestID)
		}
	}
	service.mu.Unlock()
}

func (service *terminalService) Close() error {
	if service == nil {
		return nil
	}
	var result error
	service.closeOnce.Do(func() {
		service.mu.Lock()
		service.closed = true
		close(service.closeCh)
		service.mu.Unlock()
		result = service.supervisor.Close()
		service.wg.Wait()
	})
	return result
}

func terminalDimension(input rpcInput, key string, fallback, minimum, maximum uint16) (uint16, bool) {
	raw, found := input[key]
	if !found || raw == nil {
		return fallback, true
	}
	number, ok := raw.(float64)
	if !ok || number < float64(minimum) || number > float64(maximum) || number != float64(uint16(number)) {
		return 0, false
	}
	return uint16(number), true
}

func inputHasOnly(input rpcInput, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range input {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}

func terminalEventPayload(event terminalBufferedEvent, sessionID uuid.UUID) map[string]any {
	payload := map[string]any{
		"type": event.Kind, "sessionId": sessionID.String(), "sequence": event.Sequence, "occurredAt": event.OccurredAt,
	}
	if event.Kind == "output" {
		payload["encoding"] = "base64url"
		payload["data"] = base64.RawURLEncoding.EncodeToString(event.Data)
	} else {
		payload["exitCode"] = event.ExitCode
		payload["reason"] = event.ExitReason
	}
	return payload
}

func terminalSessionID(input rpcInput) (uuid.UUID, error) {
	text, ok := inputString(input, "sessionId", 80)
	id, err := uuid.Parse(text)
	if !ok || err != nil || id == uuid.Nil {
		return uuid.Nil, errRPCInvalid
	}
	return id, nil
}
