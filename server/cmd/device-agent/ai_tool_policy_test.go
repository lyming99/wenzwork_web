package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testAIToolPreExecuteRequest() aiToolPreExecuteRequest {
	arguments := `{"path":"notes.txt"}`
	return aiToolPreExecuteRequest{
		CallID: "call-1", ToolName: "read_file", ArgumentsJSON: arguments,
		ArgumentsSHA256: aiWorkspaceBytesHash([]byte(arguments)), ConversationID: uuid.NewString(),
		GenerationID: uuid.NewString(), WorkspaceRoot: `C:\workspace`, WorkspaceMode: aiWorkspaceModeReadOnly,
		Preview: aiWorkspaceApprovalPreview{
			Title: "读取文件", Description: "读取 notes.txt", RelativePaths: []string{"notes.txt"},
			ArgumentsSHA256: aiWorkspaceBytesHash([]byte(arguments)), Risk: "readOnly",
		},
	}
}

func testAIApprovalRequest(allowForSession bool) aiApprovalRequest {
	return aiApprovalRequest{
		ID: uuid.NewString(), ConversationID: uuid.NewString(), GenerationID: uuid.NewString(), MessageID: uuid.NewString(),
		ToolCallID: "call-1", ToolName: "read_file", ExpiresAt: time.Now().UTC().Add(time.Minute),
		AllowForSession: allowForSession,
		Preview: aiWorkspaceApprovalPreview{
			Title: "读取文件", Description: "读取 notes.txt", RelativePaths: []string{"notes.txt"},
			ArgumentsSHA256: aiWorkspaceBytesHash([]byte(`{"path":"notes.txt"}`)), Risk: "readOnly",
		},
	}
}

func TestAIToolPreExecuteWaterfallDelegatesShortCircuitsAndSnapshots(t *testing.T) {
	waterfall := &aiToolPreExecuteWaterfall{}
	trace := make([]string, 0)
	var disposeSecond func()
	waterfall.register(func(_ context.Context, request aiToolPreExecuteRequest, next aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
		trace = append(trace, "first")
		request.Preview.RelativePaths[0] = "listener-only.txt"
		disposeSecond()
		return next()
	}, false)
	disposeSecond = waterfall.register(func(_ context.Context, _ aiToolPreExecuteRequest, _ aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
		trace = append(trace, "second")
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteDeny, Reason: "blocked by policy"}, nil
	}, false)
	waterfall.register(func(_ context.Context, _ aiToolPreExecuteRequest, _ aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
		trace = append(trace, "must-not-run")
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteAllow}, nil
	}, false)

	request := testAIToolPreExecuteRequest()
	decision := waterfall.decide(t.Context(), request)
	if decision.Kind != aiToolPreExecuteDeny || !slices.Equal(trace, []string{"first", "second"}) {
		t.Fatalf("decision=%+v trace=%v", decision, trace)
	}
	if request.Preview.RelativePaths[0] != "notes.txt" {
		t.Fatalf("listener mutated caller request: %+v", request.Preview)
	}

	trace = trace[:0]
	decision = waterfall.decide(t.Context(), request)
	if decision.Kind != aiToolPreExecuteAllow || !slices.Equal(trace, []string{"first", "must-not-run"}) {
		t.Fatalf("second dispatch decision=%+v trace=%v", decision, trace)
	}
	disposeSecond()
}

func TestAIToolPreExecuteWaterfallFailsClosed(t *testing.T) {
	for name, gate := range map[string]aiToolPreExecuteGate{
		"panic": func(context.Context, aiToolPreExecuteRequest, aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
			panic("boom")
		},
		"error": func(context.Context, aiToolPreExecuteRequest, aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
			return aiToolPreExecuteDecision{Kind: aiToolPreExecuteAllow}, errors.New("unavailable")
		},
		"invalid": func(context.Context, aiToolPreExecuteRequest, aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
			return aiToolPreExecuteDecision{Kind: "unexpected"}, nil
		},
		"next twice": func(_ context.Context, _ aiToolPreExecuteRequest, next aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
			_, _ = next()
			_, _ = next()
			return aiToolPreExecuteDecision{Kind: aiToolPreExecuteAllow}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			waterfall := &aiToolPreExecuteWaterfall{}
			waterfall.register(gate, false)
			if decision := waterfall.decide(t.Context(), testAIToolPreExecuteRequest()); decision.Kind != aiToolPreExecuteDeny {
				t.Fatalf("decision=%+v", decision)
			}
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if decision := (&aiToolPreExecuteWaterfall{}).decide(ctx, testAIToolPreExecuteRequest()); decision.Kind != aiToolPreExecuteDeny {
		t.Fatalf("cancelled decision=%+v", decision)
	}
}

func TestAIApprovalWaterfallDelegatesAndFailsClosed(t *testing.T) {
	request := testAIApprovalRequest(false)
	waterfall := &aiApprovalWaterfall{}
	trace := make([]string, 0)
	waterfall.register(func(_ context.Context, _ aiApprovalRequest, next aiApprovalNext) (aiApprovalResolution, error) {
		trace = append(trace, "first")
		return next()
	}, false)
	waterfall.register(func(_ context.Context, _ aiApprovalRequest, _ aiApprovalNext) (aiApprovalResolution, error) {
		trace = append(trace, "answer")
		return aiApprovalResolution{Decision: "allowOnce", Approved: true}, nil
	}, false)
	responderCalls := 0
	resolution := waterfall.resolve(t.Context(), request, func(context.Context, aiApprovalRequest) (aiApprovalResolution, error) {
		responderCalls++
		return aiApprovalResolution{Decision: "deny"}, nil
	})
	if resolution.Decision != "allowOnce" || !resolution.Approved || responderCalls != 0 || !slices.Equal(trace, []string{"first", "answer"}) {
		t.Fatalf("resolution=%+v responderCalls=%d trace=%v", resolution, responderCalls, trace)
	}

	if unavailable := (&aiApprovalWaterfall{}).resolve(t.Context(), request, nil); unavailable.Decision != "unavailable" || unavailable.Approved {
		t.Fatalf("missing responder resolution=%+v", unavailable)
	}

	panicking := &aiApprovalWaterfall{}
	panicking.register(func(context.Context, aiApprovalRequest, aiApprovalNext) (aiApprovalResolution, error) {
		panic("boom")
	}, false)
	if unavailable := panicking.resolve(t.Context(), request, nil); unavailable.Decision != "unavailable" {
		t.Fatalf("panic resolution=%+v", unavailable)
	}

	invalidSession := &aiApprovalWaterfall{}
	invalidSession.register(func(context.Context, aiApprovalRequest, aiApprovalNext) (aiApprovalResolution, error) {
		return aiApprovalResolution{Decision: "allowForSession", Approved: true, AllowForSession: true}, nil
	}, false)
	if unavailable := invalidSession.resolve(t.Context(), request, nil); unavailable.Decision != "unavailable" {
		t.Fatalf("invalid session resolution=%+v", unavailable)
	}
}

func TestAIConversationPreExecuteDenyPreventsRemoteToolBody(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{"path": "blocked.txt", "content": "must not exist", "expected_hash": "absent"})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "blocked-write", Name: "write_file", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "已停止。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeFullAccess, provider)
	gateCalls, approvalEvents := 0, 0
	fixture.state.aiToolPolicyRuntime().preExecute.register(func(_ context.Context, request aiToolPreExecuteRequest, _ aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
		gateCalls++
		if request.ToolName != "write_file" || request.requiresPlannedApproval() {
			t.Fatalf("pre-execute request=%+v", request)
		}
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteDeny, Reason: "设备策略拒绝写入。"}, nil
	}, false)
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		if event.Kind == "chat.approval.requested" {
			approvalEvents++
		}
		return nil
	}

	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "写入 blocked.txt",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.project.LocalPath, "blocked.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked file stat error=%v", err)
	}
	_, prompts := provider.snapshot()
	if gateCalls != 1 || approvalEvents != 0 || len(prompts) != 2 ||
		!strings.Contains(prompts[1].ToolExchanges[0].Results[0].Content, "pre_execute_denied") {
		t.Fatalf("gateCalls=%d approvalEvents=%d prompts=%+v", gateCalls, approvalEvents, prompts)
	}
}

func TestAIConversationPreExecuteAskEscalatesReadToRemoteApproval(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "gated-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "读取完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "notes.txt"), []byte("approved read body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.state.aiToolPolicyRuntime().preExecute.register(func(context.Context, aiToolPreExecuteRequest, aiToolPreExecuteNext) (aiToolPreExecuteDecision, error) {
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteAsk, Reason: "设备策略要求确认读取。"}, nil
	}, false)
	var approval aiApprovalRequest
	var approvalErr error
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		if event.Kind != "chat.approval.requested" {
			return nil
		}
		encoded, err := json.Marshal(event.Payload["approval"])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &approval); err != nil {
			return err
		}
		_, _, approvalErr = fixture.dispatch.respondAIConversationApprovalRPC(t.Context(), fixture.project.ID, rpcInput{
			"approvalId": approval.ID, "conversationId": approval.ConversationID, "generationId": approval.GenerationID,
			"toolCallId": approval.ToolCallID, "decision": "allowOnce",
		})
		return approvalErr
	}

	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "读取 notes.txt",
	}); err != nil || approvalErr != nil {
		t.Fatalf("send=%v approval=%v", err, approvalErr)
	}
	_, prompts := provider.snapshot()
	if approval.ID == "" || approval.AllowForSession || approval.Reason != "设备策略要求确认读取。" ||
		len(prompts) != 2 || !strings.Contains(prompts[1].ToolExchanges[0].Results[0].Content, "approved read body") {
		t.Fatalf("approval=%+v prompts=%+v", approval, prompts)
	}
}
