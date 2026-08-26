package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestAgentEventHubResetsSlowSubscriberWithoutBlockingOthers(t *testing.T) {
	hub := newAgentEventHub()
	projectID := uuid.New()
	slow, err := hub.subscribe(projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.close()
	slow.beginLiveAt(0)
	fast, err := hub.subscribe(projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.close()
	fast.beginLiveAt(0)

	for sequence := 1; sequence <= maximumAgentEventQueueCount; sequence++ {
		hub.publish(agentEventRecord{ProjectID: projectID, Sequence: uint64(sequence), SafePayloadJSON: []byte(`{"type":"task.changed"}`)})
	}
	for sequence := 1; sequence <= maximumAgentEventQueueCount; sequence++ {
		event, ok := fast.next(t.Context())
		if !ok || event.Sequence != uint64(sequence) {
			t.Fatalf("fast subscriber received sequence %d, want %d", event.Sequence, sequence)
		}
	}

	hub.publish(agentEventRecord{ProjectID: projectID, Sequence: maximumAgentEventQueueCount + 1, SafePayloadJSON: []byte(`{"type":"task.changed"}`)})
	if got := slow.resetReasonValue(); got != "slowConsumer" {
		t.Fatalf("slow subscriber reset reason = %q, want slowConsumer", got)
	}
	if got := fast.resetReasonValue(); got != "" {
		t.Fatalf("fast subscriber unexpectedly reset: %q", got)
	}
	event, ok := fast.next(t.Context())
	if !ok || event.Sequence != maximumAgentEventQueueCount+1 {
		t.Fatalf("fast subscriber did not receive the later event: %+v", event)
	}
}

func TestAgentEventReplayBoundaryOnlySuppressesTheNewSubscriber(t *testing.T) {
	hub := newAgentEventHub()
	projectID := uuid.New()
	existing, err := hub.subscribe(projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer existing.close()
	existing.beginLiveAt(0)

	joining, err := hub.subscribe(projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer joining.close()
	joining.beginLiveAt(3)

	for sequence := 1; sequence <= 3; sequence++ {
		hub.publish(agentEventRecord{ProjectID: projectID, Sequence: uint64(sequence), SafePayloadJSON: []byte(`{"type":"task.changed"}`)})
	}
	for sequence := 1; sequence <= 3; sequence++ {
		event, ok := existing.next(t.Context())
		if !ok || event.Sequence != uint64(sequence) {
			t.Fatalf("existing subscriber missed sequence %d: %+v", sequence, event)
		}
	}
	select {
	case event := <-joining.events:
		t.Fatalf("joining subscriber received replay-suppressed event: %+v", event)
	default:
	}

	hub.publish(agentEventRecord{ProjectID: projectID, Sequence: 4, SafePayloadJSON: []byte(`{"type":"task.changed"}`)})
	for _, subscriber := range []*agentEventSubscriber{existing, joining} {
		event, ok := subscriber.next(t.Context())
		if !ok || event.Sequence != 4 {
			t.Fatalf("subscriber did not receive the live event: %+v", event)
		}
	}
}

func TestAgentEventPumpWakesAfterCommittedOutboxWrite(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	subscriber, err := fixture.state.eventHub.subscribe(fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer subscriber.close()
	info, err := fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	subscriber.beginLiveAt(info.HighWatermark)

	created, err := fixture.store.Create(
		t.Context(),
		normalizeTaskV2TestDefinition(t, fixture.project, uuid.New()),
		fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	event, ok := subscriber.next(ctx)
	if !ok || event.EventType != "task.changed" || event.AggregateID != created.Definition.ID.String() {
		t.Fatalf("commit-triggered Agent event = %+v, received=%v", event, ok)
	}
}

func TestAgentEventShutdownDoesNotLeakAnInvalidResetReason(t *testing.T) {
	if validAgentEventResetReason("agentShutdown") {
		t.Fatal("agent shutdown must close the stream, not become a reset reason")
	}
	if !validAgentEventResetReason("slowConsumer") {
		t.Fatal("slow consumer must remain a valid reset reason")
	}
}

func TestTaskLogHintCoalescingKeyIncludesRun(t *testing.T) {
	taskID := uuid.New()
	firstRun := uuid.New()
	secondRun := uuid.New()
	if taskLogHintKey(taskID, &firstRun) == taskLogHintKey(taskID, &secondRun) {
		t.Fatal("separate task runs must not share a log-hint debounce key")
	}
	if taskLogHintKey(taskID, nil) == taskLogHintKey(uuid.New(), nil) {
		t.Fatal("separate tasks must not share a log-hint debounce key")
	}
}

func TestAgentEventSubscriptionReplaysOnlyOnEventScope(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	if _, err := fixture.store.Create(t.Context(), normalizeTaskV2TestDefinition(t, fixture.project, uuid.New()), fixture.now); err != nil {
		t.Fatal(err)
	}
	if !methodAllowsScope("event.subscribe", "remote.peer.events") || methodAllowsScope("task.list", "remote.peer.events") {
		t.Fatal("event scope method isolation is invalid")
	}
	requestID := uuid.NewString()
	envelope := &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
			Header:      &remotev1.RpcRequestHeader{RequestId: requestID, ProjectId: fixture.project.ID.String()},
			Method:      "event.subscribe",
			JsonPayload: []byte(`{"afterSequence":0,"heartbeatSeconds":15}`),
		}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	emitted := make([]*remotev1.RpcEnvelope, 0, 2)
	response := (dispatcher{state: fixture.state, scope: "remote.peer.events"}).streamAgentEvents(ctx, envelope, func(value *remotev1.RpcEnvelope) error {
		emitted = append(emitted, value)
		if value.GetEvent().GetKind() == remotev1.RpcEventKind_RPC_EVENT_KIND_AGENT_STATE_CHANGED {
			cancel()
		}
		return nil
	})
	if response.GetResponse().GetError() != nil {
		t.Fatalf("event subscription failed: %+v", response.GetResponse().GetError())
	}
	if len(emitted) != 2 || emitted[0].GetEvent().GetKind() != remotev1.RpcEventKind_RPC_EVENT_KIND_EVENT_SUBSCRIPTION_CONTROL ||
		emitted[1].GetEvent().GetKind() != remotev1.RpcEventKind_RPC_EVENT_KIND_AGENT_STATE_CHANGED {
		t.Fatalf("unexpected subscription frames: %+v", emitted)
	}
	if emitted[1].GetEvent().GetRequestId() != requestID || emitted[1].GetEvent().GetSequence() != 1 {
		t.Fatalf("replayed event is not bound to the subscription: %+v", emitted[1].GetEvent())
	}
}

func TestTaskLogHintsAreCoalescedAndContainNoLogContent(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	created, err := fixture.store.Create(t.Context(), normalizeTaskV2TestDefinition(t, fixture.project, uuid.New()), fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	firstLogAt := fixture.now.Add(time.Second)
	if _, err := fixture.store.AppendLog(t.Context(), created.Definition.ID, nil, "stdout", []byte("private terminal output"), firstLogAt); err != nil {
		t.Fatal(err)
	}
	info, err := fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.HighWatermark != 2 { // task.created, then the first log availability hint
		t.Fatalf("event high watermark = %d, want 2", info.HighWatermark)
	}
	events, err := fixture.state.business.listAgentEvents(t.Context(), fixture.project.ID, 0, info.HighWatermark)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].EventType != "task.logs.available" {
		t.Fatalf("unexpected journal events: %+v", events)
	}
	if strings.Contains(string(events[1].SafePayloadJSON), "private terminal output") {
		t.Fatal("log content leaked into the event payload")
	}
	var payload map[string]any
	if err := json.Unmarshal(events[1].SafePayloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok || data["highWatermark"] != float64(1) {
		t.Fatalf("unexpected safe event data: %#v", payload["data"])
	}

	if _, err := fixture.store.AppendLog(t.Context(), created.Definition.ID, nil, "stdout", []byte("more output"), firstLogAt.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	info, err = fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.HighWatermark != 2 {
		t.Fatalf("coalesced log hint advanced high watermark to %d", info.HighWatermark)
	}
	if _, err := fixture.store.AppendLog(t.Context(), created.Definition.ID, nil, "stdout", []byte("later output"), firstLogAt.Add(agentEventTaskLogHintInterval+time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	info, err = fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.HighWatermark != 3 {
		t.Fatalf("later log hint high watermark = %d, want 3", info.HighWatermark)
	}
}

func TestTaskLogHintEmitsTrailingCommittedWatermark(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	created, err := fixture.store.Create(t.Context(), normalizeTaskV2TestDefinition(t, fixture.project, uuid.New()), fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	first := fixture.now.Add(time.Second)
	if _, err := fixture.store.AppendLog(t.Context(), created.Definition.ID, nil, "stdout", []byte("first"), first); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendLog(t.Context(), created.Definition.ID, nil, "stdout", []byte("second"), first.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() bool {
		info, infoErr := fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
		if infoErr != nil || info.HighWatermark != 3 {
			return false
		}
		events, listErr := fixture.state.business.listAgentEvents(t.Context(), fixture.project.ID, 2, info.HighWatermark)
		if listErr != nil || len(events) != 1 || events[0].EventType != "task.logs.available" {
			return false
		}
		var payload struct {
			Data struct {
				HighWatermark uint64 `json:"highWatermark"`
			} `json:"data"`
		}
		return json.Unmarshal(events[0].SafePayloadJSON, &payload) == nil && payload.Data.HighWatermark == 2
	})
}

func TestRemovingProjectPurgesItsAgentEventStream(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	if _, err := fixture.store.Create(t.Context(), normalizeTaskV2TestDefinition(t, fixture.project, uuid.New()), fixture.now); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.HighWatermark == 0 {
		t.Fatal("expected the created task to produce an Agent event")
	}
	revision := fixture.project.Revision
	if _, err := fixture.state.business.removeProject(t.Context(), fixture.project.ID, &revision); !errors.Is(err, errRPCProjectHasActiveTasks) {
		t.Fatalf("remove project with a task error = %v", err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM tasks WHERE project_id = ?`, fixture.project.ID.String()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.business.removeProject(t.Context(), fixture.project.ID, &revision); err != nil {
		t.Fatal(err)
	}
	after, err := fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HighWatermark != 0 || after.MinimumAvailableSequence != 1 {
		t.Fatalf("removed project retained event stream: %+v", after)
	}
}

func TestConversationAvailabilityHintsAreCoalescedAndTerminalIsImmediate(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	created, err := fixture.state.business.createAIConversation(t.Context(), fixture.project.ID, "", "Event hints", "readOnly", config, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := fixture.state.business.beginAIConversationTurn(t.Context(), fixture.project.ID, created.ID, uuid.NewString(), "start", "readOnly", nil, config, fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	firstDeltaAt := fixture.now.Add(2 * time.Second)
	if _, _, err := fixture.state.business.appendAIConversationTextDelta(t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, "private delta", firstDeltaAt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.state.business.appendAIConversationReasoningDelta(t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, "private reasoning", firstDeltaAt.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	info, err := fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := fixture.state.business.listAgentEvents(t.Context(), fixture.project.ID, 0, info.HighWatermark)
	if err != nil {
		t.Fatal(err)
	}
	availability := countAgentEvents(events, "conversation.events.available")
	if availability != 1 {
		t.Fatalf("availability hints after adjacent deltas = %d, want 1", availability)
	}
	if strings.Contains(string(events[len(events)-1].SafePayloadJSON), "private") {
		t.Fatal("conversation content leaked into the event payload")
	}
	if _, _, _, err := fixture.state.business.finishAIConversationTurn(t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, chatUsage{}, chatProviderRun{}, firstDeltaAt.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	info, err = fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err = fixture.state.business.listAgentEvents(t.Context(), fixture.project.ID, 0, info.HighWatermark)
	if err != nil {
		t.Fatal(err)
	}
	if got := countAgentEvents(events, "conversation.events.available"); got != 2 {
		t.Fatalf("terminal event did not produce an immediate hint: got %d, want 2", got)
	}
}

func TestConversationAvailabilityHintsStayCompactAndContentFreeAcrossManyDeltas(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	created, err := fixture.state.business.createAIConversation(t.Context(), fixture.project.ID, "", "High volume hints", "readOnly", config, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := fixture.state.business.beginAIConversationTurn(t.Context(), fixture.project.ID, created.ID, uuid.NewString(), "start", "readOnly", nil, config, fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 100; index++ {
		delta := fmt.Sprintf("private delta %03d", index)
		if _, _, err := fixture.state.business.appendAIConversationTextDelta(
			t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, delta,
			fixture.now.Add(2*time.Second+time.Duration(index)*time.Millisecond),
		); err != nil {
			t.Fatalf("append delta %d: %v", index, err)
		}
	}
	info, err := fixture.state.business.agentEventStreamInfo(t.Context(), fixture.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := fixture.state.business.listAgentEvents(t.Context(), fixture.project.ID, 0, info.HighWatermark)
	if err != nil {
		t.Fatal(err)
	}
	if got := countAgentEvents(events, "conversation.events.available"); got != 1 {
		t.Fatalf("availability hints for 100 deltas = %d, want 1", got)
	}
	for _, event := range events {
		if event.EventType != "conversation.events.available" {
			continue
		}
		payload := string(event.SafePayloadJSON)
		if strings.Contains(payload, "private delta") || strings.Contains(payload, "delta 000") {
			t.Fatalf("conversation body leaked into project hint payload: %s", payload)
		}
	}
}

func countAgentEvents(events []agentEventRecord, eventType string) int {
	count := 0
	for _, event := range events {
		if event.EventType == eventType {
			count++
		}
	}
	return count
}
