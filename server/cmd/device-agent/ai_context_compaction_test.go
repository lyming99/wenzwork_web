package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type contextCompactionProvider struct {
	mu        sync.Mutex
	calls     int
	histories [][]chatMessage
	prompts   []aiProviderPrompt
	step      func(int, []chatMessage, aiProviderPrompt, func(aiProviderStreamEvent) error) error
}

func (*contextCompactionProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (*contextCompactionProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProvider
}

func (provider *contextCompactionProvider) CompletePromptEventStream(
	_ context.Context,
	_ aiConfig,
	history []chatMessage,
	prompt aiProviderPrompt,
	onEvent func(aiProviderStreamEvent) error,
) error {
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	provider.histories = append(provider.histories, append([]chatMessage(nil), history...))
	provider.prompts = append(provider.prompts, cloneAIProviderPrompt(prompt))
	step := provider.step
	provider.mu.Unlock()
	if step == nil {
		return errAIProvider
	}
	return step(index, history, prompt, onEvent)
}

func TestAIContextCompactionPrunesLargeToolResultsBeforeSummarizing(t *testing.T) {
	large := strings.Repeat("界", aiContextToolResultThreshold+500)
	prompt := aiProviderPrompt{Text: "continue", ToolExchanges: []aiProviderToolExchange{{
		Calls:   []aiProviderToolCall{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"large.txt"}`)}},
		Results: []aiProviderToolResult{{ToolCallID: "call-1", Name: "read_file", Content: large}},
	}}}
	config := aiConfig{MaxActiveContextTokens: 10000}
	result, err := compactAIProviderContext(
		t.Context(),
		&contextCompactionProvider{},
		config,
		nil,
		prompt,
		uuid.NewString(),
		time.Now().UTC(),
		aiContextPressure,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.PrunedToolResult != 1 || result.Summary != nil {
		t.Fatalf("unexpected compaction result: %+v", result)
	}
	content := result.Prompt.ToolExchanges[0].Results[0].Content
	if !strings.Contains(content, aiContextToolResultPruneMarker) ||
		len([]rune(content)) != aiContextToolResultHead+len([]rune(aiContextToolResultPruneMarker))+aiContextToolResultTail {
		t.Fatalf("tool result was not deterministically pruned: runes=%d", len([]rune(content)))
	}
	if prompt.ToolExchanges[0].Results[0].Content != large {
		t.Fatal("the caller-owned prompt was mutated")
	}
}

func TestAIContextCompactionCheckpointsOlderToolExchangesAsWholePairs(t *testing.T) {
	exchanges := make([]aiProviderToolExchange, 0, 3)
	for index := 0; index < 3; index++ {
		callID := "call-" + string(rune('a'+index))
		exchanges = append(exchanges, aiProviderToolExchange{
			Calls: []aiProviderToolCall{{ID: callID, Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}},
			Results: []aiProviderToolResult{{
				ToolCallID: callID,
				Name:       "read_file",
				Content:    strings.Repeat("界", 3000),
			}},
		})
	}
	result, err := compactAIProviderContext(
		t.Context(),
		&contextCompactionProvider{},
		aiConfig{MaxActiveContextTokens: 8000},
		nil,
		aiProviderPrompt{Text: "continue", ToolExchanges: exchanges},
		uuid.NewString(),
		time.Now().UTC(),
		aiContextPressure,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Summary != nil || len(result.Prompt.ToolExchanges) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if !strings.Contains(result.Prompt.Text, `<turn-checkpoint source="completed-tool-exchanges"`) ||
		result.Prompt.ToolExchanges[0].Calls[0].ID != "call-c" {
		t.Fatalf("checkpoint did not preserve the newest complete pair: %+v", result.Prompt)
	}
}

func TestAIContextCompactionUsesOriginalPrefixForModelSummary(t *testing.T) {
	provider := &contextCompactionProvider{}
	provider.step = func(_ int, _ []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if prompt.Text != aiContextCompactionInstruction || len(prompt.Tools) != 1 {
			return errAIProvider
		}
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "## Primary Request and Intent\n- Preserve the exact implementation constraints."},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	}
	history := make([]chatMessage, 0, 12)
	for index := 0; index < 12; index++ {
		role := "user"
		if index%2 == 1 {
			role = "assistant"
		}
		history = append(history, chatMessage{Sequence: uint64(index + 1), Role: role, Content: strings.Repeat("界", 500)})
	}
	tools := []aiWorkspaceToolDefinition{{Name: "read_file", Description: "Read one file.", InputSchema: map[string]any{"type": "object"}}}
	result, err := compactAIProviderContext(
		t.Context(),
		provider,
		aiConfig{MaxActiveContextTokens: 6000, MaxTurnOutputTokens: 1024},
		history,
		aiProviderPrompt{Text: "continue", Tools: tools},
		uuid.NewString(),
		time.Now().UTC(),
		aiContextPressure,
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || result.Summary == nil || !result.Changed {
		t.Fatalf("calls=%d result=%+v", provider.calls, result)
	}
	if len(provider.histories[0]) == 0 || provider.histories[0][0].Content != history[0].Content {
		t.Fatal("the summarizer did not receive the original leading prefix")
	}
	if !strings.Contains(result.History[0].Content, "Preserve the exact implementation constraints") ||
		!strings.HasPrefix(result.Summary.Content, aiContextSummaryPrefix) {
		t.Fatalf("generated summary was not materialized: %+v", result.Summary)
	}
}

func TestAIConversationRetriesOneContextOverflowAfterRealCompaction(t *testing.T) {
	provider := &contextCompactionProvider{}
	provider.step = func(index int, _ []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: strings.Repeat("seed-one ", 30)},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: strings.Repeat("seed-two ", 30)},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case 2:
			return errors.New("context_length_exceeded")
		case 3:
			if prompt.Text != aiContextCompactionInstruction {
				return errAIProvider
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "## Primary Request and Intent\n- Continue the seeded conversation."},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case 4:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "recovered"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	for _, prompt := range []string{
		strings.Repeat("first seed ", 30),
		strings.Repeat("second seed ", 30),
		"recover this turn",
	} {
		if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
			"conversationId": fixture.conversation.ID,
			"messageId":      uuid.NewString(),
			"prompt":         prompt,
		}); err != nil {
			t.Fatalf("send %q: %v (calls=%d)", prompt, err, provider.calls)
		}
	}
	if provider.calls != 5 {
		t.Fatalf("provider calls=%d, want 5", provider.calls)
	}
	summary, err := fixture.state.business.loadAIContextSummary(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || summary == nil || !strings.Contains(summary.Content, "Continue the seeded conversation") {
		t.Fatalf("persisted summary=%+v error=%v", summary, err)
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].Content != "recovered" {
		t.Fatalf("latest message=%+v error=%v", page.Items, err)
	}
}

func TestAIContextOverflowClassificationKeepsProviderDetailsPrivate(t *testing.T) {
	err := aiHTTPError{contextOverflow: true, statusCode: 400}
	if !isAIContextOverflowError(err) || err.Error() != errAIProvider.Error() {
		t.Fatalf("classification=%v public error=%q", isAIContextOverflowError(err), err.Error())
	}
	if !isAIContextOverflowResponse(400, []byte(`{"error":{"code":"context_length_exceeded"}}`)) ||
		isAIContextOverflowResponse(500, []byte(`context_length_exceeded`)) {
		t.Fatal("unexpected HTTP context overflow classification")
	}
}
