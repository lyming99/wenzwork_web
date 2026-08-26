package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func streamTestEvent(conversationID string, sequence uint64) aiConversationEvent {
	return aiConversationEvent{
		EventID:        uuid.NewString(),
		ConversationID: conversationID,
		GenerationID:   uuid.NewString(),
		MessageID:      uuid.NewString(),
		Kind:           "chat.text.delta",
		Sequence:       sequence,
		Payload:        map[string]any{"delta": "x"},
		OccurredAt:     time.Now().UTC(),
	}
}

func TestConversationStreamHubReplaysOnlyPostSnapshotEstablishmentEvents(t *testing.T) {
	t.Parallel()
	hub := newConversationStreamHub()
	projectID := uuid.New()
	conversationID := uuid.NewString()
	subscriber, err := hub.subscribe(projectID, conversationID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer subscriber.close()

	// This event simulates a post-commit notification arriving between
	// subscriber registration and capturedHighWatermark. It belongs to the
	// snapshot and must not be delivered as a duplicate live delta.
	hub.publishFor(projectID, streamTestEvent(conversationID, 1))
	if got := subscriber.sortedPendingSequences(); len(got) != 0 {
		t.Fatalf("pre-live subscriber queue = %v, want empty", got)
	}
	hub.beginLiveAt(subscriber, 1)
	if got := subscriber.sortedPendingSequences(); len(got) != 0 {
		t.Fatalf("snapshot event leaked into live queue: %v", got)
	}

	// This event was committed after the captured snapshot but before attach
	// replay starts. The hub retains it and emits it after the durable replay.
	hub.publishFor(projectID, streamTestEvent(conversationID, 2))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, reset, gap, available := subscriber.next(ctx, 2)
	if !available || reset != "" || gap != 0 || event.Sequence != 2 {
		t.Fatalf("post-snapshot live event = (%d,%q,%d,%t), want (2,empty,0,true)", event.Sequence, reset, gap, available)
	}
}

func TestConversationStreamHubDrainsPostCaptureRaceWithoutSkippingSequence(t *testing.T) {
	t.Parallel()
	hub := newConversationStreamHub()
	projectID := uuid.New()
	conversationID := uuid.NewString()
	subscriber, err := hub.subscribe(projectID, conversationID)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer subscriber.close()

	// Sequence 2 arrives while setup is in progress. Sequence 1 is the
	// captured snapshot boundary; beginLiveAt must drain 2 after setting the
	// subscriber live, rather than silently losing it.
	hub.publishFor(projectID, streamTestEvent(conversationID, 2))
	hub.beginLiveAt(subscriber, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, reset, gap, available := subscriber.next(ctx, 2)
	if !available || reset != "" || gap != 0 || event.Sequence != 2 {
		t.Fatalf("establishment race event = (%d,%q,%d,%t), want (2,empty,0,true)", event.Sequence, reset, gap, available)
	}
}

func TestConversationStreamHubExtendsDurableReplayAcrossExistingLiveHubRace(t *testing.T) {
	t.Parallel()
	hub := newConversationStreamHub()
	projectID := uuid.New()
	conversationID := uuid.NewString()
	existing, err := hub.subscribe(projectID, conversationID)
	if err != nil {
		t.Fatalf("subscribe existing: %v", err)
	}
	defer existing.close()
	hub.beginLiveAt(existing, 0)
	hub.publishFor(projectID, streamTestEvent(conversationID, 1))

	joining, err := hub.subscribe(projectID, conversationID)
	if err != nil {
		t.Fatalf("subscribe joining: %v", err)
	}
	defer joining.close()

	// Sequence 2 is persisted after the joining page's snapshot watermark was
	// captured, but before it starts receiving live queue entries. Because an
	// existing observer had already initialized the hub, it is not retained in
	// a hub history buffer. beginLiveAt must therefore extend the durable replay
	// boundary through the hub's published sequence instead of losing it.
	hub.publishFor(projectID, streamTestEvent(conversationID, 2))
	if through := hub.beginLiveAt(joining, 1); through != 2 {
		t.Fatalf("durable replay boundary = %d, want 2", through)
	}
	if got := joining.sortedPendingSequences(); len(got) != 0 {
		t.Fatalf("pre-live event should be recovered from durable replay, queue=%v", got)
	}

	// Once the boundary is installed, later content goes directly to the new
	// observer and follows the replayed sequence without a duplicate.
	hub.publishFor(projectID, streamTestEvent(conversationID, 3))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	event, reset, gap, available := joining.next(ctx, 3)
	if !available || reset != "" || gap != 0 || event.Sequence != 3 {
		t.Fatalf("post-boundary event = (%d,%q,%d,%t), want (3,empty,0,true)", event.Sequence, reset, gap, available)
	}
}

func TestConversationStreamHubOverflowResetsOnlySlowSubscriber(t *testing.T) {
	t.Parallel()
	hub := newConversationStreamHub()
	projectID := uuid.New()
	conversationID := uuid.NewString()
	slow, err := hub.subscribe(projectID, conversationID)
	if err != nil {
		t.Fatalf("subscribe slow: %v", err)
	}
	defer slow.close()
	fast, err := hub.subscribe(projectID, conversationID)
	if err != nil {
		t.Fatalf("subscribe fast: %v", err)
	}
	defer fast.close()
	hub.beginLiveAt(slow, 0)
	hub.beginLiveAt(fast, 0)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for sequence := uint64(1); sequence <= maximumConversationStreamQueueCount+1; sequence++ {
		hub.publishFor(projectID, streamTestEvent(conversationID, sequence))
		event, reset, gap, available := fast.next(ctx, sequence)
		if !available || reset != "" || gap != 0 || event.Sequence != sequence {
			t.Fatalf("fast subscriber sequence %d = (%d,%q,%d,%t)", sequence, event.Sequence, reset, gap, available)
		}
	}
	_, reset, _, available := slow.next(ctx, 1)
	if available || reset != "slowConsumer" {
		t.Fatalf("slow subscriber overflow = (reset=%q, available=%t), want slowConsumer false", reset, available)
	}
}

func TestConversationGenerationAttachReplaysDurableTerminalAndReturns(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn := beginLongReplayTurn(t, state, projectID, config, conversation, now)
	if _, _, err := state.business.appendAIConversationTextDelta(
		t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, "persisted delta", now.Add(time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := state.business.finishAIConversationTurn(
		t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, chatUsage{}, chatProviderRun{}, now.Add(2*time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}
	snapshot, err := state.business.getAIConversationSnapshot(t.Context(), projectID, conversation.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EventHighWatermark == 0 || snapshot.EarliestAvailableEventSequence != 1 {
		t.Fatalf("unexpected durable snapshot event bounds: high=%d earliest=%d", snapshot.EventHighWatermark, snapshot.EarliestAvailableEventSequence)
	}

	requestID := uuid.NewString()
	payload, err := json.Marshal(map[string]any{
		"conversationId": conversation.ID,
		"generationId":   turn.GenerationID,
		"afterSequence":  float64(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
			Header:      &remotev1.RpcRequestHeader{RequestId: requestID, ProjectId: projectID.String()},
			Method:      "conversation.generation.attach",
			JsonPayload: payload,
		}},
	}
	emitted := make([]*remotev1.RpcEnvelope, 0, 2)
	dispatch := dispatcher{state: state, scope: "remote.peer.ai.chat"}
	response := dispatch.dispatchLive(t.Context(), envelope, func(event *remotev1.RpcEnvelope) error {
		emitted = append(emitted, event)
		return nil
	})
	if response.GetResponse().GetError() != nil {
		t.Fatalf("attach failed: %+v", response.GetResponse().GetError())
	}
	if len(emitted) != int(snapshot.EventHighWatermark) || emitted[len(emitted)-1].GetEvent().GetKind() != remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_COMPLETED {
		t.Fatalf("durable attach event count/terminal mismatch: count=%d high=%d terminal=%v", len(emitted), snapshot.EventHighWatermark, emitted[len(emitted)-1].GetEvent().GetKind())
	}
	for index, event := range emitted {
		if event.GetEvent().GetSequence() != uint64(index+1) {
			t.Fatalf("durable attach sequence[%d]=%d", index, event.GetEvent().GetSequence())
		}
	}
	var result map[string]any
	if err := json.Unmarshal(response.GetResponse().GetJsonPayload(), &result); err != nil {
		t.Fatal(err)
	}
	if result["accepted"] != true || result["resetRequired"] != false || result["highWatermark"] != float64(snapshot.EventHighWatermark) {
		t.Fatalf("attach completion = %#v", result)
	}

	// The normal dispatch/stdio path must reject this long-lived method rather
	// than creating an observer with no PEER_CANCEL route.
	normal := dispatch.dispatch(t.Context(), envelope)
	if normal.GetResponse().GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE {
		t.Fatalf("ordinary attach response = %+v", normal.GetResponse())
	}
}
