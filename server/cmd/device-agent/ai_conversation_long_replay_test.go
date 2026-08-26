package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func newLongReplayFixture(t *testing.T) (*agentState, uuid.UUID, aiConfig, conversationView, time.Time) {
	t.Helper()
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC().Truncate(time.Millisecond)
	conversation, err := state.business.createAIConversation(t.Context(), projectID, "", "Long replay", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	return state, projectID, config, conversation, now
}

func beginLongReplayTurn(t *testing.T, state *agentState, projectID uuid.UUID, config aiConfig, conversation conversationView, now time.Time) aiConversationTurn {
	t.Helper()
	turn, err := state.business.beginAIConversationTurn(
		t.Context(), projectID, conversation.ID, uuid.NewString(), "long replay", "readOnly", nil, config, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}

func TestAIConversationEventPageUsesUTF8ByteBudgetAndStableWatermark(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn := beginLongReplayTurn(t, state, projectID, config, conversation, now)
	for index := 0; index < 24; index++ {
		// Deliberately use multi-byte characters: a character-count pager would
		// substantially under-estimate this response.
		delta := strings.Repeat("你好🙂", 130)
		if _, _, err := state.business.appendAIConversationTextDelta(
			t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, delta, now.Add(time.Duration(index+1)*time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
	}
	const budget = 4096
	var after, highWatermark uint64
	pages := 0
	for {
		page, err := state.business.listAIConversationEventsPage(t.Context(), projectID, conversation.ID, after, 200, budget)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page.responsePayload())
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > budget {
			t.Fatalf("page bytes=%d, budget=%d", len(encoded), budget)
		}
		if pages == 0 {
			highWatermark = page.HighWatermark
		} else if page.HighWatermark != highWatermark {
			t.Fatalf("watermark changed from %d to %d", highWatermark, page.HighWatermark)
		}
		if len(page.Items) == 0 || page.NextSequence <= after {
			t.Fatalf("page did not advance: after=%d page=%+v", after, page)
		}
		after = page.NextSequence
		pages++
		if !page.HasMore {
			break
		}
		if pages > 100 {
			t.Fatal("event pager did not converge")
		}
	}
	if after != highWatermark || pages < 2 {
		t.Fatalf("after=%d highWatermark=%d pages=%d", after, highWatermark, pages)
	}
}

func TestAIConversationEventPageSignalsOversizedItemAndCursorReset(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn := beginLongReplayTurn(t, state, projectID, config, conversation, now)
	if _, _, err := state.business.appendAIConversationTextDelta(
		t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID,
		strings.Repeat("界", maximumAIPersistentDeltaBytes/3), now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := state.business.listAIConversationEventsPage(t.Context(), projectID, conversation.ID, 0, 100, 512); !errors.Is(err, errRPCEventItemTooLarge) {
		t.Fatalf("oversized event error=%v, want EVENT_ITEM_TOO_LARGE", err)
	}
	if _, _, err := state.business.appendAIConversationTextDelta(
		t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, "later", now.Add(2*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	state.business.mu.Lock()
	db, err := state.business.openDB()
	if err == nil {
		_, err = db.ExecContext(t.Context(), `DELETE FROM ai_conversation_events WHERE conversation_id=? AND sequence=1`, conversation.ID)
		db.Close()
	}
	state.business.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	page, err := state.business.listAIConversationEventsPage(t.Context(), projectID, conversation.ID, 0, 100, defaultAIEventResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !page.ResetRequired || page.EarliestAvailableSequence < 2 || len(page.Items) != 0 {
		t.Fatalf("expired cursor page=%+v", page)
	}
}

type fragmentedReplayProvider struct{}

func (fragmentedReplayProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (fragmentedReplayProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", nil
}

func (fragmentedReplayProvider) CompleteEventStream(_ context.Context, _ aiConfig, _ []chatMessage, _ string, onEvent func(aiProviderStreamEvent) error) error {
	for index := 0; index < 240; index++ {
		if err := onEvent(aiProviderStreamEvent{Kind: "text", Delta: "x"}); err != nil {
			return err
		}
	}
	return onEvent(aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"})
}

type timedFragmentedReplayProvider struct{ advance func() }

func (timedFragmentedReplayProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (timedFragmentedReplayProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", nil
}

func (provider timedFragmentedReplayProvider) CompleteEventStream(_ context.Context, _ aiConfig, _ []chatMessage, _ string, onEvent func(aiProviderStreamEvent) error) error {
	if err := onEvent(aiProviderStreamEvent{Kind: "text", Delta: "first"}); err != nil {
		return err
	}
	provider.advance()
	if err := onEvent(aiProviderStreamEvent{Kind: "text", Delta: "second"}); err != nil {
		return err
	}
	return onEvent(aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"})
}

func TestAIConversationCoalescesPersistentDeltas(t *testing.T) {
	state, projectID, _, conversation, now := newLongReplayFixture(t)
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat",
		requestProjectID: projectID.String(), ai: fragmentedReplayProvider{},
	}
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": conversation.ID, "messageId": uuid.NewString(), "prompt": "stream",
	}); err != nil {
		t.Fatal(err)
	}
	page, err := state.business.listAIConversationEventsPage(t.Context(), projectID, conversation.ID, 0, 100, defaultAIEventResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	textEvents := 0
	for _, event := range page.Items {
		if event.Kind == "chat.text.delta" {
			textEvents++
			if len(event.Payload["delta"].(string)) > maximumAIPersistentDeltaBytes {
				t.Fatalf("delta event exceeds persistent bound: %d", len(event.Payload["delta"].(string)))
			}
		}
	}
	if textEvents != 1 {
		t.Fatalf("persisted %d text deltas for 240 fragments", textEvents)
	}
}

func TestAIConversationFlushesPersistentDeltaAtVisualCadence(t *testing.T) {
	state, projectID, _, conversation, now := newLongReplayFixture(t)
	current := now
	dispatch := dispatcher{
		state: state, now: func() time.Time { return current }, scope: "remote.peer.ai.chat",
		requestProjectID: projectID.String(),
	}
	dispatch.ai = timedFragmentedReplayProvider{advance: func() {
		current = current.Add(80 * time.Millisecond)
	}}
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": conversation.ID, "messageId": uuid.NewString(), "prompt": "stream",
	}); err != nil {
		t.Fatal(err)
	}
	page, err := state.business.listAIConversationEventsPage(t.Context(), projectID, conversation.ID, 0, 100, defaultAIEventResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	for _, event := range page.Items {
		if event.Kind == "chat.text.delta" {
			deltas = append(deltas, event.Payload["delta"].(string))
		}
	}
	if len(deltas) != 2 || deltas[0] != "first" || deltas[1] != "second" {
		t.Fatalf("cadence deltas = %#v, want [first second]", deltas)
	}
}

func TestAIConversationGenerationStateAndConflictUseStableSemantics(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn := beginLongReplayTurn(t, state, projectID, config, conversation, now)
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat",
		requestProjectID: projectID.String(),
	}
	value, _, err := dispatch.getAIConversationGenerationRPC(t.Context(), projectID, rpcInput{
		"conversationId": conversation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	if result["generationId"] != turn.GenerationID || result["status"] != "running" || result["canAcceptNewTurn"] != false {
		t.Fatalf("generation state=%#v", result)
	}
	if _, err := state.business.beginAIConversationTurn(
		t.Context(), projectID, conversation.ID, uuid.NewString(), "second", "readOnly", nil, config, now.Add(time.Millisecond),
	); !errors.Is(err, errRPCConversationGenerationActive) {
		t.Fatalf("second turn error=%v, want CONVERSATION_GENERATION_ACTIVE", err)
	}
	if _, _, err := state.business.appendAIConversationTextDelta(
		t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID,
		strings.Repeat("x", maximumAIPersistentDeltaBytes), now.Add(2*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	response := dispatchEnvelope(
		t,
		dispatch,
		"conversation.events",
		`{"conversationId":"`+conversation.ID+`","afterSequence":0,"limit":100,"maxResponseBytes":512}`,
	)
	if response.GetError() == nil || response.GetError().GetSafeMessage() != "EVENT_ITEM_TOO_LARGE" {
		t.Fatalf("event replay failure=%+v", response.GetError())
	}
}

func TestAIConversationMessageContentReadsUTF8SafeChunks(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn := beginLongReplayTurn(t, state, projectID, config, conversation, now)
	want := strings.Repeat("你好🙂", 2300)
	for start := 0; start < len(want); {
		end := min(start+maximumAIPersistentDeltaBytes, len(want))
		for end > start && !utf8.ValidString(want[start:end]) {
			end--
		}
		if _, _, err := state.business.appendAIConversationTextDelta(
			t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, want[start:end], now.Add(time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
		start = end
	}
	var offset uint64
	var got strings.Builder
	for chunks := 0; ; chunks++ {
		chunk, err := state.business.readAIConversationMessageContent(
			t.Context(), projectID, conversation.ID, turn.Assistant.ID, "content", offset, 1024,
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Content) > 1024 || !utf8.ValidString(chunk.Content) || chunk.Offset != offset {
			t.Fatalf("invalid chunk=%+v", chunk)
		}
		got.WriteString(chunk.Content)
		if !chunk.HasMore {
			if chunk.NextOffset != uint64(len(want)) {
				t.Fatalf("final next offset=%d want=%d", chunk.NextOffset, len(want))
			}
			break
		}
		if chunk.NextOffset <= offset || chunks > 100 {
			t.Fatalf("chunk cursor did not advance: %+v", chunk)
		}
		offset = chunk.NextOffset
	}
	if got.String() != want {
		t.Fatal("message chunk reconstruction changed content")
	}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat",
		requestProjectID: projectID.String(),
	}
	value, _, err := dispatch.listAIConversationMessagesRPC(t.Context(), projectID, rpcInput{
		"conversationId": conversation.ID, "limit": float64(50),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > defaultAIEventResponseBytes {
		t.Fatalf("bounded message page bytes=%d error=%v", len(encoded), err)
	}
	items := value.(map[string]any)["items"].([]map[string]any)
	if len(items) != 2 || items[1]["contentRef"] == nil {
		t.Fatalf("large message was not exposed through a content reference: %#v", items)
	}
}

func TestAIConversationLargeTerminalMessageUsesReferenceAndChunkedRead(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn := beginLongReplayTurn(t, state, projectID, config, conversation, now)
	want := strings.Repeat("long-answer-", 7000)
	if len(want) <= maximumRPCPayload {
		t.Fatalf("test answer is not larger than a Peer RPC frame: %d", len(want))
	}
	for start := 0; start < len(want); {
		end := min(start+maximumAIPersistentDeltaBytes, len(want))
		for end > start && !utf8.ValidString(want[start:end]) {
			end--
		}
		if _, _, err := state.business.appendAIConversationTextDelta(
			t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, want[start:end], now.Add(time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
		start = end
	}
	_, _, events, err := state.business.finishAIConversationTurn(
		t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, chatUsage{}, chatProviderRun{}, now.Add(2*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	terminal := events[len(events)-1]
	if terminal.Kind != "chat.completed" || terminal.Payload["message"] != nil || terminal.Payload["messageRef"] == nil {
		t.Fatalf("large terminal payload=%#v", terminal.Payload)
	}
	var offset uint64
	var got strings.Builder
	for chunks := 0; ; chunks++ {
		chunk, err := state.business.readAIConversationMessageContent(
			t.Context(), projectID, conversation.ID, turn.Assistant.ID, "content", offset, maximumAIMessageContentChunkBytes,
		)
		if err != nil {
			t.Fatal(err)
		}
		got.WriteString(chunk.Content)
		if !chunk.HasMore {
			break
		}
		if chunk.NextOffset <= offset || chunks > 100 {
			t.Fatalf("large content cursor did not advance: %+v", chunk)
		}
		offset = chunk.NextOffset
	}
	if got.String() != want {
		t.Fatal("large terminal content changed during chunked read")
	}
}

func TestAIConversationPendingApprovalRecoversOutsideReplayPage(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn := beginLongReplayTurn(t, state, projectID, config, conversation, now)
	argumentsHash := aiWorkspaceBytesHash([]byte("approval arguments"))
	request := aiApprovalRequest{
		ID: uuid.NewString(), ConversationID: conversation.ID, GenerationID: turn.GenerationID, MessageID: turn.Assistant.ID,
		ToolCallID: "tool-call", ToolName: "run_command", ExpiresAt: now.Add(time.Minute),
		Preview: aiWorkspaceApprovalPreview{
			Title: "Review command", Command: strings.Repeat("x", maximumAIPersistentEventPayload+1024),
			ArgumentsSHA256: argumentsHash, Risk: "openWorld",
		},
	}
	pending, err := state.registerAIApproval(projectID, request, argumentsHash)
	if err != nil {
		t.Fatal(err)
	}
	defer state.removeAIApproval(request.ID, pending)
	event, err := state.business.appendAIConversationApprovalRequested(
		t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, request, now.Add(time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	encodedEvent, err := json.Marshal(event.Payload)
	if err != nil || len(encodedEvent) > maximumAIPersistentEventPayload || event.Payload["approval"] != nil || event.Payload["approvalRef"] == nil {
		t.Fatalf("approval replay payload bytes=%d payload=%#v error=%v", len(encodedEvent), event.Payload, err)
	}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat", requestProjectID: projectID.String(),
	}
	value, _, err := dispatch.getAIConversationGenerationRPC(t.Context(), projectID, rpcInput{"conversationId": conversation.ID})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	recovered, ok := result["pendingApproval"].(*aiApprovalRequest)
	if !ok || recovered.ID != request.ID || recovered.Preview.Command != request.Preview.Command || result["status"] != "awaitingApproval" {
		t.Fatalf("pending approval recovery=%#v", result)
	}
}
