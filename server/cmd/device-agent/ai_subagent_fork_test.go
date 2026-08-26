package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAISubagentForkContextBoundsAndOrders(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "父级回答。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "父级上下文",
	}); err != nil {
		t.Fatal(err)
	}
	contextBlock, err := fixture.dispatch.aiSubagentForkContext(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextBlock, "父级上下文") || !strings.Contains(contextBlock, "父级回答") ||
		!strings.Contains(contextBlock, "<user>") || !strings.Contains(contextBlock, "<assistant>") {
		t.Fatalf("fork context = %q", contextBlock)
	}
	if strings.Index(contextBlock, "<user>") > strings.Index(contextBlock, "<assistant>") {
		t.Fatalf("fork context must be oldest-first: %q", contextBlock)
	}
	if len(contextBlock) > maximumAISubagentForkContextBytes {
		t.Fatalf("fork context exceeds budget: %d", len(contextBlock))
	}
	// A fresh conversation has no completed turns.
	empty := newAIConversationToolTestFixture(t, "readOnly", &scriptedConversationToolProvider{})
	emptyBlock, err := empty.dispatch.aiSubagentForkContext(t.Context(), empty.project.ID, empty.conversation.ID)
	if err != nil || emptyBlock != "" {
		t.Fatalf("empty fork context = %q error=%v", emptyBlock, err)
	}
}

func TestAISubagentForkContextRetainsNewestCompletedWork(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: fmt.Sprintf("reply-%02d-%s", index, strings.Repeat("r", 2800))},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	for index := 0; index < 8; index++ {
		prompt := fmt.Sprintf("request-%02d-%s", index, strings.Repeat("q", 800))
		if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
			"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": prompt,
		}); err != nil {
			t.Fatal(err)
		}
	}

	contextBlock, err := fixture.dispatch.aiSubagentForkContext(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextBlock, "request-07-") || !strings.Contains(contextBlock, "reply-07-") {
		t.Fatalf("fork context dropped newest completed turn: %q", contextBlock)
	}
	if strings.Contains(contextBlock, "request-00-") || strings.Contains(contextBlock, "reply-00-") {
		t.Fatalf("fork context retained oldest work ahead of newest work: %q", contextBlock)
	}
	if len(contextBlock) > maximumAISubagentForkContextBytes {
		t.Fatalf("fork context exceeds budget: %d", len(contextBlock))
	}
}

func TestAIConversationRecoveryInterruptsStaleSubagentDescriptor(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	config, err := fixture.dispatch.conversationAIConfig(fixture.conversation.ConfigID, fixture.conversation.ModelBinding.Model)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	child, err := fixture.state.business.createAIConversation(
		t.Context(), fixture.project.ID, "", "restart child", fixture.conversation.WorkspaceMode, config, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := &aiSubagentDescriptor{
		ParentConversationID: fixture.conversation.ID,
		Label:                "restart child",
		Depth:                1,
		Status:               "running",
		Background:           true,
		Kind:                 "spawn",
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	child, _, err = fixture.state.business.updateAIConversationCollaboration(
		t.Context(), fixture.project.ID, child.ID, "", "", "chat.subagent.started",
		map[string]any{"agentId": child.ID, "parentConversationId": fixture.conversation.ID, "depth": 1},
		func(collaboration *aiConversationCollaboration) error {
			collaboration.Subagent = descriptor
			return nil
		}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	generationID := uuid.NewString()
	if _, err := fixture.state.business.beginAIConversationTurnWithGeneration(
		t.Context(), fixture.project.ID, child.ID, uuid.NewString(), generationID, "unfinished child work",
		child.WorkspaceMode, []chatAttachmentReference{}, config, now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}

	recovered, err := fixture.state.business.recoverInterruptedAIConversations(t.Context(), now.Add(time.Second))
	if err != nil || recovered != 1 {
		t.Fatalf("recovered=%d error=%v", recovered, err)
	}
	resolved, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, child.ID)
	if err != nil || resolved.State != "idle" || resolved.Subagent == nil || resolved.Subagent.Status != "interrupted" ||
		!strings.Contains(resolved.Subagent.Error, "restart") {
		t.Fatalf("recovered child=%+v error=%v", resolved, err)
	}
	revision := resolved.Revision
	if recovered, err = fixture.state.business.recoverInterruptedAIConversations(t.Context(), now.Add(2*time.Second)); err != nil || recovered != 0 {
		t.Fatalf("second recovery=%d error=%v", recovered, err)
	}
	resolved, err = fixture.state.business.getAIConversation(context.Background(), fixture.project.ID, child.ID)
	if err != nil || resolved.Revision != revision {
		t.Fatalf("recovery was not idempotent: child=%+v error=%v", resolved, err)
	}
}

func TestAIConversationToolLoopForksSubagentWithInheritedContext(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "父级回答。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case 1:
			arguments, _ := json.Marshal(map[string]any{"description": "分析并总结这段历史。", "background": false})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "fork-call-1", Name: "subagent_fork", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 2:
			// Child agent's first turn: it must carry the inherited transcript.
			if !strings.Contains(prompt.Text, "父级上下文") || !strings.Contains(prompt.Text, "父级回答") ||
				!strings.Contains(prompt.Text, "分析并总结这段历史") || !strings.Contains(prompt.Text, "<inherited-context>") {
				t.Errorf("child prompt missing inherited context: %q", prompt.Text)
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "子代理总结完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case 3:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "fork 完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "父级上下文",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "fork 一个子代理",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	if len(prompts) < 4 {
		t.Fatalf("provider calls = %d", len(prompts))
	}
	result := prompts[3].ToolExchanges[0].Results[0]
	if result.IsError || !strings.Contains(result.Content, "子代理总结完成") || !strings.Contains(result.Content, `\"status\":\"completed\"`) {
		t.Fatalf("fork result = %+v", result)
	}
	children, err := fixture.state.business.listAISubagents(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || len(children) != 1 || children[0].Subagent == nil || children[0].Subagent.Kind != "fork" {
		t.Fatalf("children=%+v error=%v", children, err)
	}
}

func TestAISubagentApprovalPinnedDeny(t *testing.T) {
	combined := &scriptedConversationToolProvider{}
	combined.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{"task": "尝试写入文件", "background": false})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "spawn-call-1", Name: "spawn_agent", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			arguments, _ := json.Marshal(map[string]any{
				"path": "child-write.txt", "content": "child body\n", "sandbox_permissions": aiWorkspaceModeWorkspaceWrite,
			})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "child-write", Name: "write_file", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 2:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "子代理已处理拒绝。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case 3:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "父级完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", combined)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "让子代理尝试写入",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := combined.snapshot()
	if len(prompts) != 4 {
		t.Fatalf("provider calls = %d", len(prompts))
	}
	childWrite := prompts[2].ToolExchanges[0].Results[0]
	if !childWrite.IsError || !strings.Contains(childWrite.Content, "subagent_approval_denied") {
		t.Fatalf("child write result = %+v", childWrite)
	}
	if _, err := os.Stat(filepath.Join(fixture.project.LocalPath, "child-write.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied child write created file: %v", err)
	}
	spawnResult := prompts[3].ToolExchanges[0].Results[0]
	if spawnResult.IsError || !strings.Contains(spawnResult.Content, "子代理已处理拒绝") {
		t.Fatalf("spawn result = %+v", spawnResult)
	}
}
