package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
)

type terminalTestFixture struct {
	state   *agentState
	project registeredProject
	service *terminalService
	starter *fakePTYStarter
}

func TestTerminalShellArgumentsUseUTF8OnWindows(t *testing.T) {
	for _, shell := range []string{"pwsh", "powershell"} {
		arguments := terminalShellArgumentsForOS(shell, "windows")
		if !slices.Contains(arguments, "-NoExit") || !slices.Contains(arguments, windowsPowerShellUTF8Bootstrap) {
			t.Fatalf("%s arguments do not initialize UTF-8: %#v", shell, arguments)
		}
	}
	arguments := terminalShellArgumentsForOS("cmd", "windows")
	if !slices.Contains(arguments, "/K") || !slices.Contains(arguments, windowsCmdUTF8Bootstrap) {
		t.Fatalf("cmd arguments do not initialize UTF-8: %#v", arguments)
	}
	if strings.Contains(windowsPowerShellUTF8Bootstrap, "chcp 65001") {
		t.Fatal("PowerShell bootstrap must not depend on a bare chcp PATH lookup")
	}
	if !strings.Contains(windowsPowerShellUTF8Bootstrap, "SystemDirectory") {
		t.Fatal("PowerShell bootstrap must resolve chcp from the Windows system directory")
	}
}

func TestTerminalShellArgumentsSkipWindowsBootstrapOutsideWindows(t *testing.T) {
	for _, operatingSystem := range []string{"linux", "darwin"} {
		arguments := terminalShellArgumentsForOS("pwsh", operatingSystem)
		if !slices.Contains(arguments, "-NoExit") {
			t.Fatalf("pwsh arguments on %s do not keep the terminal interactive: %#v", operatingSystem, arguments)
		}
		if slices.Contains(arguments, "-Command") || slices.Contains(arguments, windowsPowerShellUTF8Bootstrap) {
			t.Fatalf("pwsh arguments on %s injected the Windows bootstrap: %#v", operatingSystem, arguments)
		}
	}
	if arguments := terminalShellArgumentsForOS("cmd", "linux"); arguments != nil {
		t.Fatalf("cmd arguments on Linux = %#v, want nil", arguments)
	}
}

func TestAvailableShellsForOSUsesPlatformDefaultOrder(t *testing.T) {
	tests := []struct {
		name              string
		operatingSystem   string
		installed         []string
		expectedAvailable []string
	}{
		{
			name:            "macOS prefers zsh over the deprecated system Bash",
			operatingSystem: "darwin", installed: []string{"bash", "zsh"},
			expectedAvailable: []string{"zsh", "bash"},
		},
		{
			name:            "macOS falls back when zsh is unavailable",
			operatingSystem: "darwin", installed: []string{"bash"},
			expectedAvailable: []string{"bash"},
		},
		{
			name:            "Linux keeps Bash as its first choice",
			operatingSystem: "linux", installed: []string{"bash", "zsh"},
			expectedAvailable: []string{"bash", "zsh"},
		},
		{
			name:            "Windows keeps PowerShell before cmd",
			operatingSystem: "windows", installed: []string{"powershell", "cmd"},
			expectedAvailable: []string{"powershell", "cmd"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installed := make(map[string]bool, len(test.installed))
			for _, shell := range test.installed {
				installed[shell] = true
			}
			actual := availableShellsForOS(test.operatingSystem, func(shell string) (string, error) {
				if installed[shell] {
					return shell, nil
				}
				return "", os.ErrNotExist
			})
			if !slices.Equal(actual, test.expectedAvailable) {
				t.Fatalf("available shells on %s = %#v, want %#v", test.operatingSystem, actual, test.expectedAvailable)
			}
		})
	}
}

func newTerminalTestFixture(t *testing.T) terminalTestFixture {
	t.Helper()
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(directory, "interactive-project")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := state.business.addProject(context.Background(), projectRoot, "Interactive", "", projectPolicy{AllowInteractiveTerminal: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(setInteractiveTerminalRuntimeProbe(func() bool { return true }))
	starter := new(fakePTYStarter)
	supervisor := newProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, maximumTerminalSessions)
	supervisor.hostEnvironment = func() []string {
		return []string{"PATH=trusted-system-path", "TERMINAL_TEST_SYSTEM_ENV=preserved"}
	}
	service := newTerminalService(state, supervisor)
	service.shellArgv = func(string) ([]string, string, error) {
		return []string{filepath.Join(projectRoot, "fake-shell")}, "fake", nil
	}
	state.servicesMu.Lock()
	state.processes, state.terminals = supervisor, service
	state.servicesMu.Unlock()
	t.Cleanup(func() { _ = state.close() })
	return terminalTestFixture{state: state, project: project, service: service, starter: starter}
}

func (fixture terminalTestFixture) open(t *testing.T) (map[string]any, uuid.UUID, *terminalSession, *fakeProcessPTY) {
	t.Helper()
	requestID := uuid.New()
	result, err := fixture.service.Open(fixture.project, rpcInput{
		"clientRequestId": requestID.String(), "cwd": "", "shell": "fake", "rows": float64(30), "columns": float64(100),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := uuid.Parse(result["sessionId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.mu.Lock()
	session := fixture.service.sessions[sessionID]
	fixture.service.mu.Unlock()
	if session == nil || fixture.starter.latest() == nil {
		t.Fatal("terminal session or fake PTY was not created")
	}
	if specs := fixture.starter.specs; len(specs) != 1 ||
		!slices.Contains(specs[0].Environment, "TERMINAL_TEST_SYSTEM_ENV=preserved") {
		t.Fatalf("terminal did not inherit the host environment: %#v", specs)
	}
	replayed, err := fixture.service.Open(fixture.project, rpcInput{
		"clientRequestId": requestID.String(), "cwd": "", "shell": "fake", "rows": float64(30), "columns": float64(100),
	})
	if err != nil || replayed["sessionId"] != sessionID.String() || replayed["replayed"] != true || len(fixture.starter.processes) != 1 {
		t.Fatalf("idempotent Open() = %#v, %v", replayed, err)
	}
	return result, sessionID, session, fixture.starter.latest()
}

func TestInteractiveTerminalIdleAttachHoldsUntilTimeout(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	_, sessionID, _, _ := fixture.open(t)
	started := time.Now()
	completion, err := fixture.service.StreamAttach(context.Background(), fixture.project, rpcInput{
		"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(1),
	}, func(terminalBufferedEvent, uint64) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if completion.Reason != "timeout" || completion.EventCount != 0 || completion.Held < 900*time.Millisecond || time.Since(started) < 900*time.Millisecond {
		t.Fatalf("idle attach completion = %+v elapsed=%v", completion, time.Since(started))
	}
}

func TestInteractiveTerminalAttachCancellationAndConcurrencyAreBounded(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	_, sessionID, session, _ := fixture.open(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := fixture.service.StreamAttach(ctx, fixture.project, rpcInput{
			"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(30),
		}, func(terminalBufferedEvent, uint64) error { return nil })
		result <- err
	}()
	eventually(t, time.Second, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.attachActive
	})
	if _, err := fixture.service.StreamAttach(context.Background(), fixture.project, rpcInput{
		"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(0),
	}, func(terminalBufferedEvent, uint64) error { return nil }); !errors.Is(err, errRPCBusy) {
		t.Fatalf("concurrent attach error = %v, want BUSY", err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled attach error = %v", err)
	}
	eventually(t, time.Second, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return !session.attachActive
	})
}

func TestInteractiveTerminalAttachTokenBucketRejectsBurst(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	_, sessionID, _, _ := fixture.open(t)
	input := rpcInput{"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(0)}
	for attempt := 0; attempt < terminalAttachTokenBurst; attempt++ {
		completion, err := fixture.service.StreamAttach(context.Background(), fixture.project, input, func(terminalBufferedEvent, uint64) error { return nil })
		if err != nil || completion.Reason != "timeout" {
			t.Fatalf("attach attempt %d = %+v, %v", attempt, completion, err)
		}
	}
	if _, err := fixture.service.StreamAttach(context.Background(), fixture.project, input, func(terminalBufferedEvent, uint64) error { return nil }); !errors.Is(err, errRPCBusy) {
		t.Fatalf("burst attach error = %v, want BUSY", err)
	}
}

func TestInteractiveTerminalInputReplayOutputResumeExitAndAudit(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	opened, sessionID, session, process := fixture.open(t)
	if opened["cwd"] != "" || opened["rows"] != uint16(30) || opened["columns"] != uint16(100) || opened["running"] != true {
		t.Fatalf("Open() = %#v", opened)
	}
	input := []byte("echo hello\r")
	writeInput := rpcInput{
		"sessionId": sessionID.String(), "inputSequence": float64(1), "encoding": "base64url",
		"data": base64.RawURLEncoding.EncodeToString(input),
	}
	firstWrite, err := fixture.service.Write(fixture.project, writeInput)
	if err != nil || firstWrite["replayed"] != false {
		t.Fatalf("first Write() = %#v, %v", firstWrite, err)
	}
	replayedWrite, err := fixture.service.Write(fixture.project, writeInput)
	if err != nil || replayedWrite["replayed"] != true || len(process.snapshotWrites()) != 1 {
		t.Fatalf("replayed Write() = %#v, %v, writes=%q", replayedWrite, err, process.snapshotWrites())
	}
	conflict := cloneRPCInput(writeInput)
	conflict["data"] = base64.RawURLEncoding.EncodeToString([]byte("different"))
	if _, err := fixture.service.Write(fixture.project, conflict); err != errRPCRevision {
		t.Fatalf("conflicting replay error = %v", err)
	}
	gap := cloneRPCInput(writeInput)
	gap["inputSequence"] = float64(3)
	if _, err := fixture.service.Write(fixture.project, gap); err != errRPCRevision {
		t.Fatalf("input gap error = %v", err)
	}
	if _, err := fixture.service.Signal(fixture.project, rpcInput{
		"sessionId": sessionID.String(), "signal": "interrupt", "inputSequence": float64(2),
	}); err != nil {
		t.Fatal(err)
	}
	resizeInput := rpcInput{
		"sessionId": sessionID.String(), "resizeSequence": float64(1), "rows": float64(40), "columns": float64(120),
	}
	if resized, err := fixture.service.Resize(fixture.project, resizeInput); err != nil || resized["replayed"] != false {
		t.Fatalf("Resize() = %#v, %v", resized, err)
	}
	if resized, err := fixture.service.Resize(fixture.project, resizeInput); err != nil || resized["replayed"] != true || len(process.resizes) != 1 {
		t.Fatalf("replayed Resize() = %#v, %v", resized, err)
	}

	if err := process.emit([]byte("hello\r\n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.sequence >= 1
	})
	attachInput := rpcInput{"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(0)}
	attached, err := fixture.service.Attach(fixture.project, attachInput)
	if err != nil || attached["resetRequired"] != false || attached["highWatermark"] != uint64(1) {
		t.Fatalf("Attach() = %#v, %v", attached, err)
	}
	var events []terminalBufferedEvent
	if completion, err := fixture.service.StreamAttach(context.Background(), fixture.project, attachInput, func(event terminalBufferedEvent, _ uint64) error {
		events = append(events, event)
		return nil
	}); err != nil || completion.Reason != "events" || completion.EventCount != 1 || len(events) != 1 || string(events[0].Data) != "hello\r\n" || events[0].Sequence != 1 {
		t.Fatalf("StreamAttach() completion=%+v events=%#v error=%v", completion, events, err)
	}

	process.finish(7)
	eventually(t, time.Second, func() bool { return !session.isRunning() })
	events = nil
	if completion, err := fixture.service.StreamAttach(context.Background(), fixture.project, rpcInput{
		"sessionId": sessionID.String(), "lastSequence": float64(1), "waitSeconds": float64(0),
	}, func(event terminalBufferedEvent, _ uint64) error {
		events = append(events, event)
		return nil
	}); err != nil || completion.Reason != "exit" || completion.EventCount != 1 || len(events) != 1 || events[0].Kind != "exit" || events[0].ExitCode != 7 || events[0].Sequence != 2 {
		t.Fatalf("exit completion=%+v events=%#v error=%v", completion, events, err)
	}

	eventually(t, time.Second, func() bool {
		db, openErr := fixture.state.business.openDB()
		if openErr != nil {
			return false
		}
		defer db.Close()
		var inputBytes, outputBytes uint64
		var exitCode int
		var reason string
		queryErr := db.QueryRow(`SELECT input_bytes, output_bytes, exit_code, close_reason FROM terminal_session_audit WHERE session_id = ?`, sessionID.String()).Scan(&inputBytes, &outputBytes, &exitCode, &reason)
		return queryErr == nil && inputBytes == uint64(len(input)+1) && outputBytes == 7 && exitCode == 7 && reason == "process_exit"
	})
	database, err := os.ReadFile(fixture.state.business.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{input, []byte("hello\r\n")} {
		if bytes.Contains(database, forbidden) {
			t.Fatalf("terminal input/output body leaked into audit database: %q", forbidden)
		}
	}
}

func TestInteractiveTerminalRingEvictionRequiresResetAndPolicyRevocationClosesProcess(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	_, sessionID, session, process := fixture.open(t)
	now := fixture.service.now()
	session.mu.Lock()
	for index := 0; index < 700; index++ {
		session.appendEventLocked(terminalBufferedEvent{Kind: "output", Data: make([]byte, 2048), OccurredAt: now})
	}
	firstSequence := session.events[0].Sequence
	session.mu.Unlock()
	if firstSequence <= 1 {
		t.Fatalf("ring did not evict old output: firstSequence=%d", firstSequence)
	}
	attached, err := fixture.service.Attach(fixture.project, rpcInput{
		"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(0),
	})
	if err != nil || attached["resetRequired"] != true || attached["firstSequence"] != firstSequence {
		t.Fatalf("evicted Attach() = %#v, %v", attached, err)
	}
	emitted := 0
	if completion, err := fixture.service.StreamAttach(context.Background(), fixture.project, rpcInput{
		"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(0),
	}, func(terminalBufferedEvent, uint64) error { emitted++; return nil }); err != nil || completion.Reason != "reset" || emitted != 0 {
		t.Fatalf("reset stream completion=%+v emitted=%d error=%v", completion, emitted, err)
	}
	policy := fixture.project.Policy
	policy.AllowInteractiveTerminal = false
	revision := fixture.project.Revision
	if _, err := fixture.state.business.updateProject(context.Background(), fixture.project.ID, nil, nil, &policy, &revision); err != nil {
		t.Fatal(err)
	}
	fixture.service.cleanup(fixture.service.now())
	eventually(t, time.Second, process.isClosed)
}

func TestInteractiveTerminalAttachKeepsStreamingForTheWholeWindow(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	_, sessionID, session, process := fixture.open(t)
	type result struct {
		completion terminalAttachCompletion
		events     []terminalBufferedEvent
		err        error
	}
	completed := make(chan result, 1)
	go func() {
		var events []terminalBufferedEvent
		completion, err := fixture.service.StreamAttach(context.Background(), fixture.project, rpcInput{
			"sessionId": sessionID.String(), "lastSequence": float64(0), "waitSeconds": float64(1),
		}, func(event terminalBufferedEvent, _ uint64) error {
			events = append(events, event)
			return nil
		})
		completed <- result{completion: completion, events: events, err: err}
	}()
	eventually(t, time.Second, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.attachActive
	})
	if err := process.emit([]byte("first")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.sequence >= 1
	})
	select {
	case early := <-completed:
		t.Fatalf("attach ended after its first batch: %+v", early)
	case <-time.After(100 * time.Millisecond):
	}
	if err := process.emit([]byte("second")); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-completed:
		if outcome.err != nil || outcome.completion.Reason != "events" || outcome.completion.EventCount != 2 ||
			len(outcome.events) != 2 || string(outcome.events[0].Data) != "first" || string(outcome.events[1].Data) != "second" ||
			outcome.completion.Held < 900*time.Millisecond {
			t.Fatalf("whole-window attach = %+v", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("whole-window attach did not complete")
	}
}

func TestInteractiveTerminalEnforcesSessionCountsAndAgentExitCleanup(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	projects := []registeredProject{fixture.project}
	for index := 0; index < 2; index++ {
		root := filepath.Join(filepath.Dir(fixture.project.LocalPath), "additional-project-"+uuid.NewString())
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		project, err := fixture.state.business.addProject(
			context.Background(), root, "Additional", "", projectPolicy{AllowInteractiveTerminal: true},
		)
		if err != nil {
			t.Fatal(err)
		}
		projects = append(projects, project)
	}
	var sessions []*terminalSession
	open := func(project registeredProject) error {
		result, err := fixture.service.Open(project, rpcInput{
			"clientRequestId": uuid.NewString(), "shell": "fake", "rows": float64(24), "columns": float64(80),
		})
		if err != nil {
			return err
		}
		id, parseErr := uuid.Parse(result["sessionId"].(string))
		if parseErr != nil {
			return parseErr
		}
		fixture.service.mu.Lock()
		sessions = append(sessions, fixture.service.sessions[id])
		fixture.service.mu.Unlock()
		return nil
	}
	for index := 0; index < maximumTerminalSessionsPerProject; index++ {
		if err := open(projects[0]); err != nil {
			t.Fatal(err)
		}
	}
	if err := open(projects[0]); err != errRPCBusy {
		t.Fatalf("per-project session limit error = %v", err)
	}
	for index := maximumTerminalSessionsPerProject; index < maximumTerminalSessions; index++ {
		if err := open(projects[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := open(projects[2]); err != errRPCBusy {
		t.Fatalf("global session limit error = %v", err)
	}
	if len(fixture.starter.processes) != maximumTerminalSessions {
		t.Fatalf("started process count = %d", len(fixture.starter.processes))
	}
	if err := fixture.state.close(); err != nil {
		t.Fatal(err)
	}
	for index, session := range sessions {
		reason := ""
		if session != nil {
			reason = session.process.reason()
		}
		if session == nil || !fixture.starter.processes[index].isClosed() || reason != "agent_exit" {
			t.Fatalf("session %d survived Agent exit: session=%v closed=%v reason=%q", index, session != nil, fixture.starter.processes[index].isClosed(), reason)
		}
	}
}

func TestInteractiveTerminalEnforcesInputAndOutputRates(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		fixture := newTerminalTestFixture(t)
		_, sessionID, session, process := fixture.open(t)
		chunk := make([]byte, maximumTerminalInputBytes)
		for sequence := uint64(1); sequence <= maximumTerminalInputRateBytes/maximumTerminalInputBytes; sequence++ {
			if _, err := fixture.service.Write(fixture.project, rpcInput{
				"sessionId": sessionID.String(), "inputSequence": float64(sequence), "encoding": "base64url",
				"data": base64.RawURLEncoding.EncodeToString(chunk),
			}); err != nil {
				t.Fatalf("input sequence %d: %v", sequence, err)
			}
		}
		if _, err := fixture.service.Write(fixture.project, rpcInput{
			"sessionId":     sessionID.String(),
			"inputSequence": float64(maximumTerminalInputRateBytes/maximumTerminalInputBytes + 1),
			"encoding":      "base64url", "data": base64.RawURLEncoding.EncodeToString([]byte("x")),
		}); err != errRPCBusy {
			t.Fatalf("input rate limit error = %v", err)
		}
		eventually(t, time.Second, process.isClosed)
		if reason := session.process.reason(); reason != "input_rate_limit" {
			t.Fatalf("input rate close reason = %q", reason)
		}
	})

	t.Run("output", func(t *testing.T) {
		fixture := newTerminalTestFixture(t)
		_, _, session, process := fixture.open(t)
		session.mu.Lock()
		session.outputRateWindow = fixture.service.now()
		session.outputRateBytes = maximumTerminalOutputRateBytes
		session.mu.Unlock()
		if err := process.emit([]byte("x")); err != nil {
			t.Fatal(err)
		}
		eventually(t, time.Second, process.isClosed)
		if reason := session.process.reason(); reason != "output_rate_limit" {
			t.Fatalf("output rate close reason = %q", reason)
		}
	})
}

func TestInteractiveTerminalCleanupKeepsHealthyIdleSessionRunning(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	fixture.service.now = func() time.Time { return now }
	opened, _, session, process := fixture.open(t)
	if terminalIdleTimeout != 0 || terminalMaximumLifetime != 0 || terminalDisconnectGrace != 5*time.Minute ||
		terminalDuplexKeepAliveInterval != 2*time.Minute {
		t.Fatalf("terminal liveness policy = idle %s, lifetime %s, grace %s, keepalive %s",
			terminalIdleTimeout, terminalMaximumLifetime, terminalDisconnectGrace, terminalDuplexKeepAliveInterval)
	}
	if opened["expiresAt"] != nil || opened["idleExpiresAt"] != nil {
		t.Fatalf("unlimited terminal expiry fields = %#v, %#v", opened["expiresAt"], opened["idleExpiresAt"])
	}
	if limits := fixture.starter.specs[len(fixture.starter.specs)-1].Limits; limits.MaximumLifetime != 0 {
		t.Fatalf("terminal process maximum lifetime = %s", limits.MaximumLifetime)
	}

	// A content-idle terminal remains alive indefinitely as long as its Stream
	// heartbeat is fresh. Simulate far beyond the removed ten-minute idle and
	// two-hour lifetime deadlines without sending input, output, or resize.
	now = now.Add(24 * time.Hour)
	session.markAttached(now)
	fixture.service.cleanup(now)
	if process.isClosed() {
		t.Fatalf("healthy idle terminal was closed as %q", session.process.reason())
	}
}

func TestInteractiveTerminalDuplexLivenessRefreshesDisconnectGrace(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	now := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	fixture.service.now = func() time.Time { return now }
	_, _, session, process := fixture.open(t)

	session.mu.Lock()
	session.lastAttachAt = now.Add(-terminalDisconnectGrace)
	session.mu.Unlock()
	generation := session.registerDuplex(func() {})
	if generation == 0 {
		t.Fatal("duplex registration did not produce a generation")
	}
	fixture.service.cleanup(now)
	if process.isClosed() {
		t.Fatalf("fresh duplex Hello was closed as %q", session.process.reason())
	}

	now = now.Add(terminalDisconnectGrace - time.Second)
	if !session.acceptDuplexKeepAlive(&remotev2.TerminalOutputAck{}) {
		t.Fatal("zero-credit duplex keepalive was rejected")
	}
	if session.acceptDuplexKeepAlive(&remotev2.TerminalOutputAck{CreditBytes: 1}) {
		t.Fatal("credit-bearing output ACK was mistaken for a keepalive")
	}
	fixture.service.cleanup(now)
	if process.isClosed() {
		t.Fatalf("healthy duplex keepalive was closed as %q", session.process.reason())
	}

	session.unregisterDuplex(generation)
	now = now.Add(terminalDisconnectGrace)
	fixture.service.cleanup(now)
	eventually(t, time.Second, process.isClosed)
	if reason := session.process.reason(); reason != "disconnect_timeout" {
		t.Fatalf("expired duplex disconnect reason = %q", reason)
	}
}

func TestInteractiveTerminalDispatcherEmitsTypedOutputEvent(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	dispatch := dispatcher{
		state: fixture.state, now: time.Now, scope: "remote.peer.terminal.interactive",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	openEnvelope, err := newCallEnvelope(uuid.NewString(), "terminal.open", []byte(`{"clientRequestId":"`+uuid.NewString()+`","shell":"fake","rows":24,"columns":80}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	openEnvelope.GetRequest().Header.ProjectId = fixture.project.ID.String()
	openResponse := dispatch.dispatch(context.Background(), openEnvelope).GetResponse()
	if openResponse.GetError() != nil {
		t.Fatalf("terminal.open error = %+v", openResponse.GetError())
	}
	var opened map[string]any
	if err := json.Unmarshal(openResponse.GetJsonPayload(), &opened); err != nil {
		t.Fatal(err)
	}
	sessionID := opened["sessionId"].(string)
	process := fixture.starter.latest()
	if err := process.emit([]byte("typed")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool {
		id, _ := uuid.Parse(sessionID)
		fixture.service.mu.Lock()
		session := fixture.service.sessions[id]
		fixture.service.mu.Unlock()
		if session == nil {
			return false
		}
		session.mu.Lock()
		defer session.mu.Unlock()
		return session.sequence == 1
	})
	attachEnvelope, err := newCallEnvelope(uuid.NewString(), "terminal.attach", []byte(`{"sessionId":"`+sessionID+`","lastSequence":0,"waitSeconds":0}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attachEnvelope.GetRequest().Header.ProjectId = fixture.project.ID.String()
	var emitted []*remotev1.RpcEnvelope
	response := dispatch.dispatchLive(context.Background(), attachEnvelope, func(event *remotev1.RpcEnvelope) error {
		emitted = append(emitted, event)
		return nil
	})
	if response.GetResponse().GetError() != nil || len(emitted) != 1 {
		t.Fatalf("terminal.attach response=%+v events=%d", response.GetResponse(), len(emitted))
	}
	var completion map[string]any
	if err := json.Unmarshal(response.GetResponse().GetJsonPayload(), &completion); err != nil ||
		completion["completionReason"] != "events" || completion["eventCount"] != float64(1) || completion["heldMilliseconds"] == nil {
		t.Fatalf("terminal.attach completion=%#v error=%v", completion, err)
	}
	event := emitted[0].GetEvent()
	if event.GetKind() != remotev1.RpcEventKind_RPC_EVENT_KIND_TERMINAL_OUTPUT || event.GetSequence() != 1 {
		t.Fatalf("terminal event = %+v", event)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.GetJsonPayload(), &payload); err != nil || payload["type"] != "output" || payload["data"] != base64.RawURLEncoding.EncodeToString([]byte("typed")) {
		t.Fatalf("terminal event payload = %#v, %v", payload, err)
	}
}

func TestInteractiveTerminalRealPTYRunsAndResizesOnCurrentDevice(t *testing.T) {
	if !interactiveTerminalRuntimeAvailable() {
		t.Skip("current device has no supported PTY runtime")
	}
	shells := availableShells()
	if len(shells) == 0 {
		t.Skip("current device has no supported shell")
	}
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	projectRoot := filepath.Join(directory, "real-pty-project")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := state.business.addProject(context.Background(), projectRoot, "Real PTY", "", projectPolicy{AllowInteractiveTerminal: true})
	if err != nil {
		t.Fatal(err)
	}
	service, err := state.terminalService()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := service.Open(project, rpcInput{
		"clientRequestId": uuid.NewString(), "shell": shells[0], "rows": float64(24), "columns": float64(80),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := uuid.Parse(opened["sessionId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resize(project, rpcInput{
		"sessionId": sessionID.String(), "resizeSequence": float64(1), "rows": float64(37), "columns": float64(119),
	}); err != nil {
		t.Fatal(err)
	}
	command := "printf 'WENZWORK_PTY_SMOKE\\n'\nexit\n"
	if runtime.GOOS == "windows" {
		command = "echo WENZWORK_PTY_SMOKE\r\nexit\r\n"
	}
	if _, err := service.Write(project, rpcInput{
		"sessionId": sessionID.String(), "inputSequence": float64(1), "encoding": "base64url",
		"data": base64.RawURLEncoding.EncodeToString([]byte(command)),
	}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	session := service.sessions[sessionID]
	service.mu.Unlock()
	if session == nil {
		t.Fatal("real PTY session was not retained")
	}
	eventually(t, 15*time.Second, func() bool { return !session.isRunning() })
	session.mu.Lock()
	var output []byte
	for _, event := range session.events {
		if event.Kind == "output" {
			output = append(output, event.Data...)
		}
	}
	rows, columns := session.rows, session.columns
	session.mu.Unlock()
	if !bytes.Contains(output, []byte("WENZWORK_PTY_SMOKE")) {
		t.Fatalf("real PTY output did not contain smoke marker: %q", output)
	}
	if rows != 37 || columns != 119 {
		t.Fatalf("real PTY size = %dx%d", rows, columns)
	}
}

func cloneRPCInput(input rpcInput) rpcInput {
	result := make(rpcInput, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
