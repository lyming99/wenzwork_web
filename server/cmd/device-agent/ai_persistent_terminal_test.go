package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAIWorkspacePersistentTerminalSurvivesToolCalls(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	openPlan := planAIWorkspaceTool(t, fixture, "terminal_open", map[string]any{
		"allow_network": false, "name": "main", "network_hosts": []any{}, "sandbox_permissions": "",
		"type": "shell", "working_directory": "",
	})
	opened := fixture.executor.Execute(t.Context(), fixture.context, openPlan, false)
	if opened.IsError {
		t.Fatalf("open terminal: %+v", opened)
	}
	var openPayload struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(opened.Content), &openPayload); err != nil || uuid.Validate(openPayload.SessionID) != nil {
		t.Fatalf("open payload=%s error=%v", opened.Content, err)
	}
	process := fixture.terminalStarter.latest()
	if process == nil {
		t.Fatal("persistent terminal did not start a supervised PTY")
	}

	emitDone := make(chan error, 1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		emitDone <- process.emit([]byte("\x1b[32mstate-kept\x1b[0m\r\nprompt> "))
	}()
	sendPlan := planAIWorkspaceTool(t, fixture, "terminal_send", map[string]any{
		"session_id": openPayload.SessionID, "text": "export KEEP=1", "timeout_seconds": float64(2),
	})
	sent := fixture.executor.Execute(t.Context(), fixture.context, sendPlan, false)
	if sent.IsError || !slices.Contains([]string{"inferred_idle", "session_exit", "timeout"}, sent.Metadata["wait_reason"].(string)) {
		t.Fatalf("send terminal: %+v", sent)
	}
	if err := <-emitDone; err != nil {
		t.Fatal(err)
	}
	readPlan := planAIWorkspaceTool(t, fixture, "terminal_read", map[string]any{"session_id": openPayload.SessionID})
	read := fixture.executor.Execute(t.Context(), fixture.context, readPlan, false)
	var readPayload struct {
		Text string `json:"text"`
	}
	readErr := json.Unmarshal([]byte(read.Content), &readPayload)
	if read.IsError || readErr != nil || !containsAll(readPayload.Text, "state-kept", "prompt> ") || containsAll(readPayload.Text, "\x1b[") {
		t.Fatalf("read terminal: %+v", read)
	}

	secondPlan := planAIWorkspaceTool(t, fixture, "terminal_send", map[string]any{"session_id": openPayload.SessionID, "text": "echo $KEEP"})
	second := fixture.executor.Execute(t.Context(), fixture.context, secondPlan, false)
	if second.IsError {
		t.Fatalf("second send: %+v", second)
	}
	writes := process.snapshotWrites()
	if len(writes) != 2 || string(writes[0]) != "export KEEP=1\r" || string(writes[1]) != "echo $KEEP\r" {
		t.Fatalf("PTY writes=%q", writes)
	}

	signalPlan := planAIWorkspaceTool(t, fixture, "terminal_signal", map[string]any{"session_id": openPayload.SessionID, "signal": "SIGINT"})
	if result := fixture.executor.Execute(t.Context(), fixture.context, signalPlan, false); result.IsError {
		t.Fatalf("signal terminal: %+v", result)
	}
	writes = process.snapshotWrites()
	if len(writes) != 3 || len(writes[2]) != 1 || writes[2][0] != 3 {
		t.Fatalf("signal writes=%q", writes)
	}

	closePlan := planAIWorkspaceTool(t, fixture, "terminal_close", map[string]any{"session_id": openPayload.SessionID})
	if result := fixture.executor.Execute(t.Context(), fixture.context, closePlan, false); result.IsError || !process.isClosed() {
		t.Fatalf("close terminal: result=%+v closed=%v", result, process.isClosed())
	}
	listPlan := planAIWorkspaceTool(t, fixture, "terminal_list", map[string]any{})
	listed := fixture.executor.Execute(t.Context(), fixture.context, listPlan, false)
	if listed.IsError || listed.Metadata["session_count"] != 0 {
		t.Fatalf("list terminals: %+v", listed)
	}
}

func TestAIPersistentTerminalOwnerIsolationSingleSendAndCancellation(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	openPlan := planAIWorkspaceTool(t, fixture, "terminal_open", map[string]any{"type": "shell"})
	opened := fixture.executor.Execute(t.Context(), fixture.context, openPlan, false)
	if opened.IsError {
		t.Fatalf("open terminal: %+v", opened)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(opened.Content), &payload); err != nil {
		t.Fatal(err)
	}
	sessionID := uuid.MustParse(payload["session_id"].(string))
	owner := aiTerminalOwnerFor(fixture.context)
	other := owner
	other.ConversationID = uuid.NewString()
	if _, err := fixture.executor.terminals.Inspect(other, sessionID); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("cross-conversation inspect error=%v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := fixture.executor.terminals.Send(t.Context(), owner, sessionID, "long", true, 2*time.Second)
		firstDone <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(fixture.terminalStarter.latest().snapshotWrites()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("first send did not reach PTY")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := fixture.executor.terminals.Send(t.Context(), owner, sessionID, "overlap", true, time.Second); !errors.Is(err, errRPCBusy) {
		t.Fatalf("overlapping send error=%v", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first send: %v", err)
	}

	cancelContext, cancel := context.WithCancel(t.Context())
	cancelDone := make(chan error, 1)
	go func() {
		_, err := fixture.executor.terminals.Send(cancelContext, owner, sessionID, "cancel-me", true, time.Minute)
		cancelDone <- err
	}()
	deadline = time.Now().Add(time.Second)
	for len(fixture.terminalStarter.latest().snapshotWrites()) < 2 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("cancellable send did not reach PTY")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-cancelDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel send error=%v", err)
	}
	view, err := fixture.executor.terminals.Inspect(owner, sessionID)
	if err != nil || view.Status["kind"] != "running" {
		t.Fatalf("cancel killed persistent session: view=%+v error=%v", view, err)
	}
	writes := fixture.terminalStarter.latest().snapshotWrites()
	if len(writes) < 1 || len(writes[len(writes)-1]) != 1 || writes[len(writes)-1][0] != 3 {
		t.Fatalf("cancel did not send interrupt: %q", writes)
	}
	downgraded := owner
	downgraded.WorkspaceMode = aiWorkspaceModeReadOnly
	if err := fixture.executor.terminals.Reconcile(downgraded); err != nil {
		t.Fatalf("reconcile permission downgrade: %v", err)
	}
	if !fixture.terminalStarter.latest().isClosed() {
		t.Fatal("permission downgrade left privileged terminal running")
	}
}

func TestAIConversationToolRuntimeClosesProjectTerminalsAfterPolicyRevocation(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	fixture.state.servicesMu.Lock()
	fixture.state.aiTools = fixture.executor
	fixture.state.servicesMu.Unlock()

	opened := fixture.executor.Execute(
		t.Context(),
		fixture.context,
		planAIWorkspaceTool(t, fixture, "terminal_open", map[string]any{"type": "shell"}),
		false,
	)
	if opened.IsError || fixture.terminalStarter.latest() == nil {
		t.Fatalf("open terminal before revocation: %+v", opened)
	}
	policy := fixture.project.Policy
	policy.AllowAIWorkspaceTools = false
	revision := fixture.project.Revision
	if _, err := fixture.state.business.updateProject(t.Context(), fixture.project.ID, nil, nil, &policy, &revision); err != nil {
		t.Fatal(err)
	}

	runtime, err := (dispatcher{state: fixture.state, scope: "remote.peer.ai.tools"}).conversationToolRuntime(
		t.Context(),
		fixture.project.ID,
		aiConversationTurn{Conversation: conversationView{
			ID: fixture.context.ConversationID, WorkspaceMode: fixture.context.WorkspaceMode,
		}},
		aiConfig{},
	)
	if err != nil || runtime == nil || runtime.executor != nil || runtime.exposes("get_goal") ||
		!runtime.exposes("todo_write") || !runtime.exposes("spawn_agent") {
		t.Fatalf("runtime after revocation=%+v error=%v", runtime, err)
	}
	if !fixture.terminalStarter.latest().isClosed() {
		t.Fatal("project policy revocation left persistent terminal running")
	}
}

func containsAll(value string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(value, needle) {
			return false
		}
	}
	return true
}
