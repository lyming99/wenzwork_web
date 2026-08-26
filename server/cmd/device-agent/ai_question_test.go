package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParseAIQuestionsValidation(t *testing.T) {
	valid := []any{
		map[string]any{"id": "mode", "question": "Choose a mode.", "header": "Confirm", "options": []any{
			map[string]any{"label": "Fast (Recommended)", "description": "Faster, less thorough."},
			map[string]any{"label": "Thorough"},
		}},
		map[string]any{"id": "confirm", "question": "Proceed?", "multi_select": true},
	}
	if _, err := parseAIQuestions(valid); err != nil {
		t.Fatalf("valid questions rejected: %v", err)
	}
	cases := []struct {
		name string
		raw  any
	}{
		{"empty", []any{}},
		{"missing id", []any{map[string]any{"question": "Proceed?"}}},
		{"blank question", []any{map[string]any{"id": "a", "question": "  "}}},
		{"duplicate ids", []any{map[string]any{"id": "a", "question": "x"}, map[string]any{"id": "a", "question": "y"}}},
		{"too many options", []any{map[string]any{"id": "a", "question": "x", "options": []any{
			map[string]any{"label": "1"}, map[string]any{"label": "2"}, map[string]any{"label": "3"},
			map[string]any{"label": "4"}, map[string]any{"label": "5"}, map[string]any{"label": "6"},
			map[string]any{"label": "7"}, map[string]any{"label": "8"}, map[string]any{"label": "9"},
		}}}},
		{"option missing label", []any{map[string]any{"id": "a", "question": "x", "options": []any{map[string]any{"description": "d"}}}}},
		{"not an array", map[string]any{"id": "a", "question": "x"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseAIQuestions(test.raw); err == nil {
				t.Fatalf("invalid questions accepted: %+v", test.raw)
			}
		})
	}
}

func TestAIConversationToolLoopAsksAndReceivesAnswers(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{"questions": []any{
				map[string]any{"id": "mode", "question": "选择优化模式。", "options": []any{
					map[string]any{"label": "快速 (Recommended)", "description": "更快但覆盖较少。"},
					map[string]any{"label": "彻底"},
				}},
				map[string]any{"id": "confirm", "question": "是否继续？"},
			}})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "question-call-1", Name: "ask_user_question", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "已按用户的回答继续。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	var runningRun *chatToolRun
	var answeredErr error
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		if event.Kind != "chat.tool.status" {
			return nil
		}
		encoded, err := json.Marshal(event.Payload["toolRun"])
		if err != nil {
			return err
		}
		var run chatToolRun
		if json.Unmarshal(encoded, &run) != nil {
			return err
		}
		if run.Tool == "ask_user_question" && run.Status == "running" && runningRun == nil {
			copy := run
			runningRun = &copy
			// The running status event precedes question registration, so poll
			// the registry briefly and answer as soon as it appears.
			go func() {
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					fixture.state.aiQuestionMu.Lock()
					pending := fixture.state.aiQuestions[run.ID]
					fixture.state.aiQuestionMu.Unlock()
					if pending != nil {
						answers := []any{map[string]any{"id": "mode", "answer": "彻底"}, map[string]any{"id": "confirm", "answer": "继续"}}
						_, _, answeredErr = fixture.dispatch.answerAIConversationQuestionRPC(context.Background(), fixture.project.ID, rpcInput{
							"conversationId": pending.ConversationID,
							"generationId":   pending.GenerationID,
							"toolCallId":     pending.ToolCallID,
							"answers":        answers,
						})
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}()
		}
		return nil
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "询问用户",
	}); err != nil {
		t.Fatal(err)
	}
	if answeredErr != nil || runningRun == nil {
		t.Fatalf("answered=%v running=%+v", answeredErr, runningRun)
	}
	_, prompts := provider.snapshot()
	result := prompts[1].ToolExchanges[0].Results[0]
	if result.IsError || !strings.Contains(result.Content, `\"answered\"`) ||
		!strings.Contains(result.Content, `\"answer\":\"彻底\"`) || !strings.Contains(result.Content, `\"answer\":\"继续\"`) {
		t.Fatalf("question result = %+v", result)
	}
}

func TestAnswerAIConversationQuestionRPCGuardsMismatches(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, "readOnly", &scriptedConversationToolProvider{})
	pending, err := fixture.state.registerAIQuestion(fixture.project.ID, fixture.conversation.ID,
		uuid.NewString(), "call-1", []string{"a", "b"}, fixture.dispatch.now().UTC().Add(defaultAIApprovalTimeout))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.state.removeAIQuestion("call-1", pending)
	generation := pending.GenerationID
	// Unknown tool call id fails closed.
	if _, _, err := fixture.dispatch.answerAIConversationQuestionRPC(t.Context(), fixture.project.ID, rpcInput{
		"conversationId": fixture.conversation.ID, "generationId": generation, "toolCallId": "unknown-call",
		"answers": []any{map[string]any{"id": "a", "answer": "x"}, map[string]any{"id": "b", "answer": "y"}},
	}); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("unknown call error = %v", err)
	}
	// Wrong generation fails closed.
	if _, _, err := fixture.dispatch.answerAIConversationQuestionRPC(t.Context(), fixture.project.ID, rpcInput{
		"conversationId": fixture.conversation.ID, "generationId": uuid.NewString(), "toolCallId": "call-1",
		"answers": []any{map[string]any{"id": "a", "answer": "x"}, map[string]any{"id": "b", "answer": "y"}},
	}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("wrong generation error = %v", err)
	}
	// Partial answers are rejected wholesale.
	if _, _, err := fixture.dispatch.answerAIConversationQuestionRPC(t.Context(), fixture.project.ID, rpcInput{
		"conversationId": fixture.conversation.ID, "generationId": generation, "toolCallId": "call-1",
		"answers": []any{map[string]any{"id": "a", "answer": "x"}},
	}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("partial answers error = %v", err)
	}
	// The exact answer set resolves and clears the pending question.
	if _, _, err := fixture.dispatch.answerAIConversationQuestionRPC(t.Context(), fixture.project.ID, rpcInput{
		"conversationId": fixture.conversation.ID, "generationId": generation, "toolCallId": "call-1",
		"answers": []any{map[string]any{"id": "a", "answer": "x"}, map[string]any{"id": "b", "answer": "y"}},
	}); err != nil {
		t.Fatalf("valid answers error = %v", err)
	}
	fixture.state.aiQuestionMu.Lock()
	pendingCount := len(fixture.state.aiQuestions)
	fixture.state.aiQuestionMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pending questions = %d", pendingCount)
	}
}

func TestAIConversationQuestionClearsOnGenerationEnd(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, "readOnly", &scriptedConversationToolProvider{})
	generation := uuid.NewString()
	if _, err := fixture.state.registerAIQuestion(fixture.project.ID, fixture.conversation.ID,
		generation, "call-1", []string{"a"}, fixture.dispatch.now().UTC().Add(defaultAIApprovalTimeout)); err != nil {
		t.Fatal(err)
	}
	fixture.state.clearAIGenerationQuestions(generation)
	fixture.state.aiQuestionMu.Lock()
	pendingCount := len(fixture.state.aiQuestions)
	fixture.state.aiQuestionMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pending questions after generation end = %d", pendingCount)
	}
}
