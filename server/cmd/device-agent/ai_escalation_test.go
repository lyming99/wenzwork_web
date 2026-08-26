package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAIWorkspaceCallModeEscalation(t *testing.T) {
	cases := []struct {
		name        string
		session     string
		tool        string
		arguments   map[string]any
		wantMode    string
		wantEsc     bool
		wantEscFrom string
		wantEscTo   string
		wantErr     error
	}{
		{
			name:    "readOnly write without escalation is forbidden",
			session: aiWorkspaceModeReadOnly, tool: "write_file",
			arguments: map[string]any{"path": "a.txt", "content": "x"}, wantErr: errRPCForbidden,
		},
		{
			name:    "readOnly write escalates to workspaceWrite",
			session: aiWorkspaceModeReadOnly, tool: "write_file",
			arguments: map[string]any{"path": "a.txt", "content": "x", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite},
			wantMode:  aiWorkspaceModeWorkspaceWrite, wantEsc: true, wantEscFrom: aiWorkspaceModeReadOnly, wantEscTo: aiWorkspaceModeWorkspaceWrite,
		},
		{
			name:    "readOnly command escalates to fullAccess",
			session: aiWorkspaceModeReadOnly, tool: "run_command",
			arguments: map[string]any{"command": "echo hi", "sandbox_permissions": aiWorkspaceModeFullAccess},
			wantMode:  aiWorkspaceModeFullAccess, wantEsc: true, wantEscFrom: aiWorkspaceModeReadOnly, wantEscTo: aiWorkspaceModeFullAccess,
		},
		{
			name:    "same tier escalation is invalid",
			session: aiWorkspaceModeWorkspaceWrite, tool: "write_file",
			arguments: map[string]any{"path": "a.txt", "content": "x", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite},
			wantErr:   errRPCInvalid,
		},
		{
			name:    "lower tier escalation is invalid",
			session: aiWorkspaceModeFullAccess, tool: "run_command",
			arguments: map[string]any{"command": "echo hi", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite},
			wantErr:   errRPCInvalid,
		},
		{
			name:    "read tools cannot escalate",
			session: aiWorkspaceModeReadOnly, tool: "read_file",
			arguments: map[string]any{"path": "a.txt", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite},
			wantErr:   errRPCInvalid,
		},
		{
			name:    "unknown tier is invalid",
			session: aiWorkspaceModeReadOnly, tool: "write_file",
			arguments: map[string]any{"path": "a.txt", "content": "x", "sandbox_permissions": "danger-full-access"},
			wantErr:   errRPCInvalid,
		},
		{
			name:    "workspaceWrite write needs no escalation",
			session: aiWorkspaceModeWorkspaceWrite, tool: "write_file",
			arguments: map[string]any{"path": "a.txt", "content": "x"},
			wantMode:  aiWorkspaceModeWorkspaceWrite,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mode, escalation, err := aiWorkspaceCallMode(test.session, test.tool, test.arguments)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("mode=%q escalation=%+v error=%v want=%v", mode, escalation, err, test.wantErr)
				}
				return
			}
			if err != nil || mode != test.wantMode || (escalation != nil) != test.wantEsc ||
				escalation != nil && (escalation.From != test.wantEscFrom || escalation.To != test.wantEscTo) {
				t.Fatalf("mode=%q escalation=%+v error=%v", mode, escalation, err)
			}
		})
	}
}

func TestAIWorkspaceCallModeTreatsProviderDefaultSentinelsAsNoEscalation(t *testing.T) {
	for _, value := range []any{nil, "", "  ", "none", "use_default"} {
		t.Run(strings.TrimSpace(stringValueForTest(value)), func(t *testing.T) {
			mode, escalation, err := aiWorkspaceCallMode(aiWorkspaceModeFullAccess, "run_command", map[string]any{
				"command": "git status --short", "sandbox_permissions": value,
			})
			if err != nil || mode != aiWorkspaceModeFullAccess || escalation != nil {
				t.Fatalf("value=%#v mode=%q escalation=%+v error=%v", value, mode, escalation, err)
			}
		})
	}

	// Full-access sessions have no wider tier. Provider-specific spellings are
	// therefore inert for escalation-capable tools and must not prevent an
	// otherwise valid command from running.
	for _, value := range []string{"disabled", "read-only", "unexpected-provider-default"} {
		t.Run(value, func(t *testing.T) {
			mode, escalation, err := aiWorkspaceCallMode(aiWorkspaceModeFullAccess, "run_command", map[string]any{
				"command": "git status --short", "sandbox_permissions": value,
			})
			if err != nil || mode != aiWorkspaceModeFullAccess || escalation != nil {
				t.Fatalf("value=%q mode=%q escalation=%+v error=%v", value, mode, escalation, err)
			}
		})
	}
}

func stringValueForTest(value any) string {
	if value == nil {
		return "null"
	}
	return value.(string)
}

func TestAIWorkspaceReadOnlyWriteEscalationApprovalFlow(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeReadOnly)
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "write_file", Arguments: map[string]any{"path": "escalated.txt", "content": "escalated body\n"},
	}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("unelevated write plan error = %v", err)
	}
	plan := planAIWorkspaceTool(t, fixture, "write_file", map[string]any{
		"path": "escalated.txt", "content": "escalated body\n", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite,
	})
	if !plan.RequiresApproval || plan.workspaceMode != aiWorkspaceModeWorkspaceWrite || plan.escalation == nil ||
		plan.escalation.From != aiWorkspaceModeReadOnly || plan.escalation.To != aiWorkspaceModeWorkspaceWrite ||
		plan.Preview.AllowForSession || !strings.Contains(plan.Preview.Description, "升级") {
		t.Fatalf("escalated write plan = %+v", plan)
	}
	denied := fixture.executor.Execute(t.Context(), fixture.context, plan, false)
	if !denied.IsError || denied.Metadata["error_code"] != "approval_required" {
		t.Fatalf("denied escalation = %+v", denied)
	}
	if _, err := os.Stat(filepath.Join(fixture.project.LocalPath, "escalated.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied escalation created file: %v", err)
	}
	// Each execution consumes its own plan/audit identity, so plan again.
	approvedPlan := planAIWorkspaceTool(t, fixture, "write_file", map[string]any{
		"path": "escalated.txt", "content": "escalated body\n", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite,
	})
	approved := fixture.executor.Execute(t.Context(), fixture.context, approvedPlan, true)
	if approved.IsError {
		t.Fatalf("approved escalation = %+v", approved)
	}
	contents, err := os.ReadFile(filepath.Join(fixture.project.LocalPath, "escalated.txt"))
	if err != nil || string(contents) != "escalated body\n" {
		t.Fatalf("escalated write contents=%q error=%v", contents, err)
	}
	// A plan built for the readOnly→workspaceWrite escalation must not execute
	// under a fullAccess session.
	mismatch := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	mismatched := mismatch.executor.Execute(t.Context(), mismatch.context, approvedPlan, true)
	if !mismatched.IsError || mismatched.Metadata["error_code"] != "permission_mode_mismatch" {
		t.Fatalf("cross-mode execution = %+v", mismatched)
	}
}

func TestAIWorkspaceWriteRunCommandEscalationToFullAccess(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	starter := new(fakeRawStarter)
	fixture.executor.supervisor = newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	fixture.executor.supervisor.memoryPollInterval = time.Hour
	t.Cleanup(func() { _ = fixture.executor.supervisor.Close() })
	fixture.executor.sandbox = func(request aiCommandSandboxRequest) (aiCommandSandboxLaunch, error) {
		return aiCommandSandboxLaunch{
			Argv: append([]string(nil), request.Argv...), WorkingDirectory: request.WorkingDirectory,
			SandboxMode: request.Mode, Status: "test " + request.Mode + " sandbox",
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: request.Mode != aiWorkspaceModeFullAccess && !request.AllowNetwork,
		}, nil
	}
	normal := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{"command": "echo hi"})
	if normal.workspaceMode != aiWorkspaceModeWorkspaceWrite || normal.commandLaunch.SandboxMode != aiWorkspaceModeWorkspaceWrite {
		t.Fatalf("normal command plan = %+v", normal)
	}
	escalated := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{"command": "echo hi", "sandbox_permissions": aiWorkspaceModeFullAccess})
	if !escalated.RequiresApproval || escalated.workspaceMode != aiWorkspaceModeFullAccess ||
		escalated.commandLaunch.SandboxMode != aiWorkspaceModeFullAccess || escalated.commandLaunch.HardNetworkIsolation ||
		escalated.escalation == nil || escalated.Preview.AllowForSession || !strings.Contains(escalated.Preview.Description, "升级") {
		t.Fatalf("escalated command plan = %+v", escalated)
	}

	resultChannel := make(chan aiWorkspaceToolResult, 1)
	go func() {
		resultChannel <- fixture.executor.Execute(context.Background(), fixture.context, escalated, true)
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for starter.latest() == nil {
		select {
		case result := <-resultChannel:
			t.Fatalf("escalated command returned before process launch: %+v", result)
		case <-deadline.C:
			t.Fatal("escalated command did not start")
		case <-time.After(time.Millisecond):
		}
	}
	starter.latest().finish(0)
	if result := <-resultChannel; result.IsError || result.Metadata["sandbox_mode"] != aiWorkspaceModeFullAccess {
		t.Fatalf("escalated command result = %+v", result)
	}
}

func TestAIWorkspaceWriteTerminalOpenEscalationToFullAccess(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	fixture.executor.sandbox = func(request aiCommandSandboxRequest) (aiCommandSandboxLaunch, error) {
		return aiCommandSandboxLaunch{
			Argv: append([]string(nil), request.Argv...), WorkingDirectory: request.WorkingDirectory,
			SandboxMode: request.Mode, Status: "test " + request.Mode + " sandbox",
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: request.Mode != aiWorkspaceModeFullAccess && !request.AllowNetwork,
		}, nil
	}
	plan := planAIWorkspaceTool(t, fixture, "terminal_open", map[string]any{
		"type": "shell", "name": "elevated", "sandbox_permissions": aiWorkspaceModeFullAccess,
	})
	if !plan.RequiresApproval || plan.workspaceMode != aiWorkspaceModeFullAccess ||
		plan.commandLaunch.SandboxMode != aiWorkspaceModeFullAccess || plan.escalation == nil ||
		!plan.terminalOwner.matches(aiTerminalOwnerFor(fixture.context)) {
		t.Fatalf("escalated terminal plan = %+v", plan)
	}

	resultChannel := make(chan aiWorkspaceToolResult, 1)
	go func() {
		resultChannel <- fixture.executor.Execute(context.Background(), fixture.context, plan, true)
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for fixture.terminalStarter.latest() == nil {
		select {
		case result := <-resultChannel:
			t.Fatalf("escalated terminal returned before process launch: %+v", result)
		case <-deadline.C:
			t.Fatal("escalated terminal did not start")
		case <-time.After(time.Millisecond):
		}
	}
	if err := fixture.terminalStarter.latest().emit([]byte("prompt> ")); err != nil {
		t.Fatal(err)
	}
	opened := <-resultChannel
	if opened.IsError {
		t.Fatalf("escalated terminal open = %+v", opened)
	}
	var payload struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(opened.Content), &payload); err != nil || uuid.Validate(payload.SessionID) != nil {
		t.Fatalf("open payload=%s error=%v", opened.Content, err)
	}
	closePlan := planAIWorkspaceTool(t, fixture, "terminal_close", map[string]any{"session_id": payload.SessionID})
	if closed := fixture.executor.Execute(t.Context(), fixture.context, closePlan, false); closed.IsError {
		t.Fatalf("close escalated terminal = %+v", closed)
	}
}

func TestAIConversationToolLoopReadOnlyEscalationWrite(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{
				"path": "escalated.txt", "content": "loop escalation body\n", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite,
			})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "escalate-write", Name: "write_file", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "升级写入完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	approvals := 0
	var approvalErr error
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		if event.Kind != "chat.approval.requested" {
			return nil
		}
		encoded, err := json.Marshal(event.Payload["approval"])
		if err != nil {
			return err
		}
		var request aiApprovalRequest
		if json.Unmarshal(encoded, &request) != nil {
			return err
		}
		approvals++
		if request.AllowForSession || !strings.Contains(request.Preview.Description, "升级") {
			t.Errorf("escalation approval preview = %+v", request.Preview)
		}
		_, _, approvalErr = fixture.dispatch.respondAIConversationApprovalRPC(t.Context(), fixture.project.ID, rpcInput{
			"approvalId": request.ID, "conversationId": request.ConversationID, "generationId": request.GenerationID,
			"toolCallId": request.ToolCallID, "decision": "allowOnce",
		})
		return approvalErr
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "只读会话升级写入",
	}); err != nil || approvalErr != nil {
		t.Fatalf("send=%v approval=%v", err, approvalErr)
	}
	if approvals != 1 {
		t.Fatalf("approvals = %d", approvals)
	}
	contents, err := os.ReadFile(filepath.Join(fixture.project.LocalPath, "escalated.txt"))
	if err != nil || string(contents) != "loop escalation body\n" {
		t.Fatalf("contents=%q error=%v", contents, err)
	}
}
