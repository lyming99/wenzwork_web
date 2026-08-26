package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

// Conversation streams are intentionally much smaller than the project event
// subscription limits. They carry decrypted AI content, so only the currently
// visible conversation is allowed to consume one of these queues.
const (
	maximumConversationStreamSubscriptions                = 8
	maximumConversationStreamSubscriptionsPerConversation = 4
	maximumConversationStreamQueueCount                   = 256
	maximumConversationStreamQueueBytes                   = 512 << 10
	conversationStreamPumpInterval                        = 100 * time.Millisecond
)

type conversationStreamKey struct {
	projectID      uuid.UUID
	conversationID string
}

// conversationStreamHub is a process-local low-latency fan-out. It never owns
// history: ai_conversation_events is the recovery authority. In particular, a
// queue overflow resets only its subscriber and never blocks the provider.
type conversationStreamHub struct {
	mu            sync.Mutex
	conversations map[conversationStreamKey]*conversationStreamConversation
	closed        bool
}

type conversationStreamConversation struct {
	publishedSequence uint64
	initialized       bool
	pending           map[uint64]aiConversationEvent
	subscribers       map[*conversationStreamSubscriber]struct{}
}

type conversationStreamSubscriber struct {
	hub *conversationStreamHub
	key conversationStreamKey

	mu              sync.Mutex
	closed          bool
	reset           bool
	resetReason     string
	live            bool
	suppressThrough uint64
	queueBytes      int
	pending         map[uint64]aiConversationEvent
	wake            chan struct{}
}

func newConversationStreamHub() *conversationStreamHub {
	return &conversationStreamHub{conversations: make(map[conversationStreamKey]*conversationStreamConversation)}
}

func (hub *conversationStreamHub) subscribe(projectID uuid.UUID, conversationID string) (*conversationStreamSubscriber, error) {
	if hub == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil {
		return nil, errRPCInvalid
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return nil, context.Canceled
	}
	key := conversationStreamKey{projectID: projectID, conversationID: conversationID}
	conversation := hub.conversationLocked(key)
	if hub.subscriptionCountLocked() >= maximumConversationStreamSubscriptions ||
		len(conversation.subscribers) >= maximumConversationStreamSubscriptionsPerConversation {
		return nil, errRPCBusy
	}
	subscriber := &conversationStreamSubscriber{
		hub: hub, key: key, pending: make(map[uint64]aiConversationEvent), wake: make(chan struct{}, 1),
	}
	conversation.subscribers[subscriber] = struct{}{}
	return subscriber, nil
}

func (hub *conversationStreamHub) conversationLocked(key conversationStreamKey) *conversationStreamConversation {
	conversation := hub.conversations[key]
	if conversation == nil {
		conversation = &conversationStreamConversation{
			pending: make(map[uint64]aiConversationEvent), subscribers: make(map[*conversationStreamSubscriber]struct{}),
		}
		hub.conversations[key] = conversation
	}
	return conversation
}

func (hub *conversationStreamHub) subscriptionCountLocked() int {
	count := 0
	for _, conversation := range hub.conversations {
		count += len(conversation.subscribers)
	}
	return count
}

// beginLiveAt is called after the attach handler has captured a durable
// watermark. Events already queued while the handler was establishing its
// snapshot are retained only when they are newer than that watermark.
func (hub *conversationStreamHub) beginLiveAt(subscriber *conversationStreamSubscriber, sequence uint64) uint64 {
	if hub == nil || subscriber == nil {
		return sequence
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	conversation := hub.conversations[subscriber.key]
	if hub.closed || conversation == nil {
		subscriber.markReset("agentShutdown")
		return sequence
	}
	// Take the hub lock before exposing the subscriber as live. A publisher that
	// won the lock before us has already advanced publishedSequence and is
	// included in the durable replay below; a publisher that follows us queues
	// directly to this subscriber. That leaves no post-snapshot interval in
	// which a persisted delta can disappear between capture and delivery.
	subscriber.beginLiveAt(sequence)
	if !conversation.initialized || sequence > conversation.publishedSequence {
		conversation.initialized = true
		conversation.publishedSequence = sequence
		for pendingSequence := range conversation.pending {
			if pendingSequence <= sequence {
				delete(conversation.pending, pendingSequence)
			}
		}
		hub.drainPendingLocked(conversation)
	}
	if conversation.publishedSequence > sequence {
		return conversation.publishedSequence
	}
	return sequence
}

// publishFor keeps project ownership out of the event payload. The store and
// dispatcher already verified this binding before invoking it.
func (hub *conversationStreamHub) publishFor(projectID uuid.UUID, event aiConversationEvent) {
	if hub == nil || projectID == uuid.Nil || uuid.Validate(event.ConversationID) != nil || event.Sequence == 0 {
		return
	}
	key := conversationStreamKey{projectID: projectID, conversationID: event.ConversationID}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.closed {
		return
	}
	conversation := hub.conversations[key]
	if conversation == nil {
		// No visible page is attached. Retaining an in-process copy would turn
		// the hub into a second history store, so durable replay remains sole
		// authority until a subscriber registers.
		return
	}
	if !conversation.initialized {
		conversation.pending[event.Sequence] = event
		hub.enqueueAllLocked(conversation, event)
		return
	}
	if event.Sequence <= conversation.publishedSequence {
		return
	}
	if event.Sequence > conversation.publishedSequence+1 {
		conversation.pending[event.Sequence] = event
		hub.enqueueAllLocked(conversation, event)
		return
	}
	conversation.publishedSequence = event.Sequence
	hub.enqueueAllLocked(conversation, event)
	hub.drainPendingLocked(conversation)
}

func (hub *conversationStreamHub) drainPendingLocked(conversation *conversationStreamConversation) {
	if conversation == nil || !conversation.initialized {
		return
	}
	for {
		next, found := conversation.pending[conversation.publishedSequence+1]
		if !found {
			return
		}
		delete(conversation.pending, next.Sequence)
		conversation.publishedSequence = next.Sequence
		hub.enqueueAllLocked(conversation, next)
	}
}

func (hub *conversationStreamHub) enqueueAllLocked(conversation *conversationStreamConversation, event aiConversationEvent) {
	for subscriber := range conversation.subscribers {
		subscriber.enqueue(event)
	}
}

func (subscriber *conversationStreamSubscriber) enqueue(event aiConversationEvent) {
	if subscriber == nil {
		return
	}
	bytes, err := json.Marshal(event)
	if err != nil {
		subscriber.markReset("sequenceGap")
		return
	}
	subscriber.mu.Lock()
	if subscriber.closed || subscriber.reset || !subscriber.live || event.Sequence <= subscriber.suppressThrough {
		subscriber.mu.Unlock()
		return
	}
	if _, exists := subscriber.pending[event.Sequence]; exists {
		subscriber.mu.Unlock()
		return
	}
	if len(subscriber.pending) >= maximumConversationStreamQueueCount || subscriber.queueBytes+len(bytes) > maximumConversationStreamQueueBytes {
		subscriber.reset = true
		subscriber.resetReason = "slowConsumer"
		subscriber.mu.Unlock()
		subscriber.signal()
		return
	}
	subscriber.pending[event.Sequence] = event
	subscriber.queueBytes += len(bytes)
	subscriber.mu.Unlock()
	subscriber.signal()
}

func (subscriber *conversationStreamSubscriber) beginLiveAt(sequence uint64) {
	if subscriber == nil {
		return
	}
	subscriber.mu.Lock()
	if !subscriber.closed {
		subscriber.live = true
		subscriber.suppressThrough = sequence
		for pendingSequence, event := range subscriber.pending {
			if pendingSequence <= sequence {
				delete(subscriber.pending, pendingSequence)
				if encoded, err := json.Marshal(event); err == nil {
					subscriber.queueBytes -= len(encoded)
				}
			}
		}
		if subscriber.queueBytes < 0 {
			subscriber.queueBytes = 0
		}
	}
	subscriber.mu.Unlock()
	subscriber.signal()
}

// next yields only the expected next sequence. A newer queued event is a
// compensable gap, not an instruction to skip content. The caller reads the
// missing durable range before trying again.
func (subscriber *conversationStreamSubscriber) next(ctx context.Context, expected uint64) (aiConversationEvent, string, uint64, bool) {
	if subscriber == nil {
		return aiConversationEvent{}, "agentShutdown", 0, false
	}
	for {
		subscriber.mu.Lock()
		if subscriber.closed {
			subscriber.mu.Unlock()
			return aiConversationEvent{}, "agentShutdown", 0, false
		}
		if subscriber.reset {
			reason := subscriber.resetReason
			subscriber.mu.Unlock()
			return aiConversationEvent{}, reason, 0, false
		}
		if event, found := subscriber.pending[expected]; found {
			delete(subscriber.pending, expected)
			if encoded, err := json.Marshal(event); err == nil {
				subscriber.queueBytes -= len(encoded)
			}
			if subscriber.queueBytes < 0 {
				subscriber.queueBytes = 0
			}
			subscriber.mu.Unlock()
			return event, "", 0, true
		}
		var minimum uint64
		for sequence := range subscriber.pending {
			if minimum == 0 || sequence < minimum {
				minimum = sequence
			}
		}
		subscriber.mu.Unlock()
		if minimum > expected {
			return aiConversationEvent{}, "", minimum, false
		}
		select {
		case <-ctx.Done():
			return aiConversationEvent{}, "", 0, false
		case <-subscriber.wake:
		}
	}
}

func (subscriber *conversationStreamSubscriber) markReset(reason string) {
	if subscriber == nil {
		return
	}
	subscriber.mu.Lock()
	if !subscriber.closed && !subscriber.reset {
		subscriber.reset = true
		subscriber.resetReason = reason
	}
	subscriber.mu.Unlock()
	subscriber.signal()
}

func (subscriber *conversationStreamSubscriber) signal() {
	if subscriber == nil {
		return
	}
	select {
	case subscriber.wake <- struct{}{}:
	default:
	}
}

func (subscriber *conversationStreamSubscriber) close() {
	if subscriber != nil && subscriber.hub != nil {
		subscriber.hub.unsubscribe(subscriber)
	}
}

func (hub *conversationStreamHub) unsubscribe(subscriber *conversationStreamSubscriber) {
	if hub == nil || subscriber == nil {
		return
	}
	hub.mu.Lock()
	if conversation := hub.conversations[subscriber.key]; conversation != nil {
		delete(conversation.subscribers, subscriber)
		if len(conversation.subscribers) == 0 {
			delete(hub.conversations, subscriber.key)
		}
	}
	hub.mu.Unlock()
	subscriber.mu.Lock()
	subscriber.closed = true
	subscriber.mu.Unlock()
	subscriber.signal()
}

func (hub *conversationStreamHub) activeKeys() []conversationStreamKey {
	if hub == nil {
		return nil
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	keys := make([]conversationStreamKey, 0, len(hub.conversations))
	for key, conversation := range hub.conversations {
		if len(conversation.subscribers) > 0 {
			keys = append(keys, key)
		}
	}
	return keys
}

func (hub *conversationStreamHub) publishedSequence(key conversationStreamKey) uint64 {
	if hub == nil {
		return 0
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if conversation := hub.conversations[key]; conversation != nil {
		return conversation.publishedSequence
	}
	return 0
}

func (hub *conversationStreamHub) reset(key conversationStreamKey, reason string) {
	if hub == nil {
		return
	}
	hub.mu.Lock()
	conversation := hub.conversations[key]
	subscribers := make([]*conversationStreamSubscriber, 0)
	if conversation != nil {
		for subscriber := range conversation.subscribers {
			subscribers = append(subscribers, subscriber)
		}
	}
	hub.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.markReset(reason)
	}
}

func (hub *conversationStreamHub) close() {
	if hub == nil {
		return
	}
	hub.mu.Lock()
	if hub.closed {
		hub.mu.Unlock()
		return
	}
	hub.closed = true
	subscribers := make([]*conversationStreamSubscriber, 0, hub.subscriptionCountLocked())
	for _, conversation := range hub.conversations {
		for subscriber := range conversation.subscribers {
			subscribers = append(subscribers, subscriber)
		}
	}
	hub.conversations = make(map[conversationStreamKey]*conversationStreamConversation)
	hub.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.markReset("agentShutdown")
	}
}

// conversationStreamPump is a post-commit safety net. It scans only
// conversations with a live attach and fills notification loss or concurrent
// post-commit publication reordering from the durable event journal.
type conversationStreamPump struct {
	store *businessStore
	hub   *conversationStreamHub

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func newConversationStreamPump(store *businessStore, hub *conversationStreamHub) *conversationStreamPump {
	ctx, cancel := context.WithCancel(context.Background())
	return &conversationStreamPump{store: store, hub: hub, ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

func (pump *conversationStreamPump) start() {
	if pump == nil || pump.store == nil || pump.hub == nil {
		return
	}
	pump.once.Do(func() { go pump.run() })
}

func (pump *conversationStreamPump) run() {
	defer close(pump.done)
	ticker := time.NewTicker(conversationStreamPumpInterval)
	defer ticker.Stop()
	for {
		pump.syncOnce()
		select {
		case <-pump.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (pump *conversationStreamPump) syncOnce() {
	if pump == nil || pump.store == nil || pump.hub == nil {
		return
	}
	for _, key := range pump.hub.activeKeys() {
		after := pump.hub.publishedSequence(key)
		latest, earliest, err := pump.store.aiConversationEventWatermarks(pump.ctx, key.projectID, key.conversationID)
		if err != nil || latest <= after {
			continue
		}
		if earliest > 1 && after < earliest-1 {
			pump.hub.reset(key, "cursorExpired")
			continue
		}
		for after < latest {
			page, err := pump.store.listAIConversationEventsPage(
				pump.ctx, key.projectID, key.conversationID, after, maximumAIEventPage, defaultAIEventResponseBytes,
			)
			if err != nil || page.ResetRequired || len(page.Items) == 0 {
				pump.hub.reset(key, "sequenceGap")
				break
			}
			expected := after + 1
			failed := false
			for _, event := range page.Items {
				if event.Sequence != expected {
					failed = true
					break
				}
				pump.hub.publishFor(key.projectID, event)
				after = event.Sequence
				expected++
				if after >= latest {
					break
				}
			}
			if failed || after < latest && !page.HasMore && page.NextSequence <= after {
				pump.hub.reset(key, "sequenceGap")
				break
			}
		}
	}
}

func (pump *conversationStreamPump) close() {
	if pump == nil {
		return
	}
	pump.cancel()
	select {
	case <-pump.done:
	case <-time.After(time.Second):
	}
}

// streamAIConversationGeneration is deliberately separate from dispatch(): a
// stdio request has no Relay query to cancel and must never create a hidden
// infinite subscription.
func (d dispatcher) streamAIConversationGeneration(ctx context.Context, envelope *remotev1.RpcEnvelope, emit func(*remotev1.RpcEnvelope) error) *remotev1.RpcEnvelope {
	requestID := ""
	if envelope != nil && envelope.GetRequest() != nil && envelope.GetRequest().GetHeader() != nil {
		requestID = envelope.GetRequest().GetHeader().GetRequestId()
	}
	response := newConversationStreamRPCResponse(requestID)
	if envelope == nil || envelope.GetProtocolVersion() != 1 || envelope.GetRequest() == nil || envelope.GetRequest().GetHeader() == nil ||
		envelope.GetRequest().GetMethod() != "conversation.generation.attach" || uuid.Validate(requestID) != nil || emit == nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid conversation stream subscription", false)
		return response
	}
	request := envelope.GetRequest()
	if !methodAllowsScope(request.GetMethod(), d.scope) || len(request.GetJsonPayload()) > maximumRPCPayload {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN, "method is outside the ticket scope", false)
		return response
	}
	var input rpcInput
	if len(request.GetJsonPayload()) == 0 || json.Unmarshal(request.GetJsonPayload(), &input) != nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid JSON input", false)
		return response
	}
	streamDispatcher := d
	streamDispatcher.requestProjectID = strings.TrimSpace(request.GetHeader().GetProjectId())
	if err := streamDispatcher.validateProjectBinding(request.GetMethod(), streamDispatcher.requestProjectID); err != nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH, "project is not authorized for this session", false)
		return response
	}
	projectID, err := streamDispatcher.aiConversationProjectID()
	if err != nil || streamDispatcher.state == nil || streamDispatcher.state.business == nil || streamDispatcher.state.conversationStreamHub == nil {
		conversationStreamSetError(response, firstError(err, errRPCCapability))
		return response
	}
	conversationID, generationID, afterSequence, negotiation, err := parseAIConversationAttachInput(input)
	if err != nil {
		conversationStreamSetError(response, err)
		return response
	}
	streamDispatcher.eventContract = negotiation
	return streamDispatcher.runAIConversationGenerationAttach(ctx, response, emit, projectID, conversationID, generationID, afterSequence)
}

func parseAIConversationAttachInput(input rpcInput) (string, string, uint64, rpcEventNegotiation, error) {
	if !onlyInputFields(input, "conversationId", "generationId", "afterSequence", "eventContractVersion", "acceptedEventKinds", "event_contract_version", "accepted_event_kinds") {
		return "", "", 0, rpcEventNegotiation{}, errRPCInvalid
	}
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	generationID, generationOK := inputString(input, "generationId", 80)
	after, present, afterOK := optionalUint64(input, "afterSequence")
	if !conversationOK || !generationOK || !afterOK || !present || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil {
		return "", "", 0, rpcEventNegotiation{}, errRPCInvalid
	}
	negotiation, err := parseRPCEventNegotiation(input)
	if err != nil {
		return "", "", 0, rpcEventNegotiation{}, err
	}
	return conversationID, generationID, after, negotiation, nil
}

func (d dispatcher) runAIConversationGenerationAttach(
	ctx context.Context,
	response *remotev1.RpcEnvelope,
	emit func(*remotev1.RpcEnvelope) error,
	projectID uuid.UUID,
	conversationID, generationID string,
	afterSequence uint64,
) *remotev1.RpcEnvelope {
	requestID := response.GetResponse().GetHeader().GetRequestId()
	state, revision, err := d.state.business.getAIConversationGenerationState(ctx, projectID, conversationID, generationID)
	if err != nil || state.GenerationID != generationID {
		conversationStreamSetError(response, firstError(err, errRPCRevision))
		return response
	}
	subscriber, err := d.state.conversationStreamHub.subscribe(projectID, conversationID)
	if err != nil {
		conversationStreamSetError(response, err)
		return response
	}
	defer subscriber.close()

	capturedHighWatermark, earliest, err := d.state.business.aiConversationEventWatermarks(ctx, projectID, conversationID)
	if err != nil {
		conversationStreamSetError(response, err)
		return response
	}
	replayThrough := d.state.conversationStreamHub.beginLiveAt(subscriber, capturedHighWatermark)
	if afterSequence > capturedHighWatermark || earliest > 1 && afterSequence < earliest-1 {
		return conversationStreamCompletion(response, revision, conversationID, generationID, replayThrough, false, true, "cursor_expired")
	}
	last, resetReason, emitErr := d.replayAIConversationStream(ctx, requestID, emit, projectID, conversationID, afterSequence, replayThrough)
	if emitErr != nil {
		// The durable journal already contains every emitted event. The relay
		// cancellation/close merely detaches this observer; it never affects the
		// provider generation.
		return conversationStreamCompletion(response, revision, conversationID, generationID, replayThrough, last > afterSequence, false, "client_cancel")
	}
	if resetReason != "" {
		return conversationStreamCompletion(response, revision, conversationID, generationID, replayThrough, last > afterSequence, true, resetReason)
	}

	// A generation may have completed between the initial state check and the
	// replay. Replaying through the captured watermark is sufficient in that
	// case; do not incorrectly report it as a failed attach.
	state, revision, err = d.state.business.getAIConversationGenerationState(ctx, projectID, conversationID, generationID)
	if err == nil && state.GenerationID == generationID && !state.StatusIsActive() {
		latest, _, watermarkErr := d.state.business.aiConversationEventWatermarks(ctx, projectID, conversationID)
		if watermarkErr == nil && latest > last {
			last, resetReason, emitErr = d.replayAIConversationStream(ctx, requestID, emit, projectID, conversationID, last, latest)
			if emitErr != nil {
				return conversationStreamCompletion(response, revision, conversationID, generationID, latest, last > afterSequence, false, "client_cancel")
			}
			if resetReason != "" {
				return conversationStreamCompletion(response, revision, conversationID, generationID, latest, last > afterSequence, true, resetReason)
			}
		}
		return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, false, "")
	}

	for {
		expected := last + 1
		event, subscriberReset, gapThrough, available := subscriber.next(ctx, expected)
		if subscriberReset != "" {
			return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, true, subscriberReset)
		}
		if ctx.Err() != nil || !available && gapThrough == 0 {
			return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, false, "client_cancel")
		}
		if gapThrough > expected {
			var reset string
			last, reset, emitErr = d.replayAIConversationStream(ctx, requestID, emit, projectID, conversationID, last, gapThrough-1)
			if emitErr != nil {
				return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, false, "client_cancel")
			}
			if reset != "" {
				return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, true, reset)
			}
			continue
		}
		if !available {
			continue
		}
		if event.Sequence != expected {
			return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, true, "sequence_gap")
		}
		if d.eventContract.allows(event.Kind) {
			frame, err := rpcEnvelopeForAIConversationEvent(event, requestID)
			if err != nil || emit(frame) != nil {
				return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, false, "client_cancel")
			}
		}
		last = event.Sequence
		if event.GenerationID == generationID && aiConversationEventIsTerminal(event) {
			latest, _, watermarkErr := d.state.business.aiConversationEventWatermarks(ctx, projectID, conversationID)
			if watermarkErr == nil && latest > last {
				var reset string
				last, reset, emitErr = d.replayAIConversationStream(ctx, requestID, emit, projectID, conversationID, last, latest)
				if emitErr != nil {
					return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, false, "client_cancel")
				}
				if reset != "" {
					return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, true, reset)
				}
			}
			return conversationStreamCompletion(response, revision, conversationID, generationID, last, last > afterSequence, false, "")
		}
	}
}

func (state aiConversationGenerationState) StatusIsActive() bool {
	return state.Status == "running" || state.Status == "awaitingApproval"
}

func aiConversationEventIsTerminal(event aiConversationEvent) bool {
	return event.Kind == "chat.completed" || event.Kind == "chat.failed" || event.Kind == "chat.cancelled"
}

// replayAIConversationStream emits a fixed durable range in strict sequence
// order. A range that cannot be reconstructed is a reset, never a silent gap.
func (d dispatcher) replayAIConversationStream(
	ctx context.Context,
	requestID string,
	emit func(*remotev1.RpcEnvelope) error,
	projectID uuid.UUID,
	conversationID string,
	after, through uint64,
) (uint64, string, error) {
	last := after
	for last < through {
		page, err := d.state.business.listAIConversationEventsPage(
			ctx, projectID, conversationID, last, maximumAIEventPage, defaultAIEventResponseBytes,
		)
		if err != nil {
			if errors.Is(err, errRPCEventCursorExpired) {
				return last, "cursor_expired", nil
			}
			return last, "", err
		}
		if page.ResetRequired {
			return last, "cursor_expired", nil
		}
		if len(page.Items) == 0 {
			return last, "sequence_gap", nil
		}
		for _, event := range page.Items {
			if event.Sequence > through {
				break
			}
			if event.Sequence != last+1 {
				return last, "sequence_gap", nil
			}
			if d.eventContract.allows(event.Kind) {
				frame, frameErr := rpcEnvelopeForAIConversationEvent(event, requestID)
				if frameErr != nil {
					return last, "", frameErr
				}
				if err := emit(frame); err != nil {
					return last, "", err
				}
			}
			last = event.Sequence
		}
		if last < through && (!page.HasMore || page.NextSequence <= last) {
			return last, "sequence_gap", nil
		}
	}
	return last, "", nil
}

func newConversationStreamRPCResponse(requestID string) *remotev1.RpcEnvelope {
	return &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Response{Response: &remotev1.RpcResponse{
		Header: &remotev1.RpcResponseHeader{RequestId: requestID},
	}}}
}

func conversationStreamCompletion(
	response *remotev1.RpcEnvelope,
	revision uint64,
	conversationID, generationID string,
	highWatermark uint64,
	replayed bool,
	resetRequired bool,
	reason string,
) *remotev1.RpcEnvelope {
	payload := map[string]any{
		"accepted": true, "conversationId": conversationID, "generationId": generationID,
		"revision": revision, "replayed": replayed, "resetRequired": resetRequired, "highWatermark": highWatermark,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		conversationStreamSetError(response, err)
		return response
	}
	response.GetResponse().Header.Revision = revision
	response.GetResponse().JsonPayload = encoded
	return response
}

func conversationStreamSetError(response *remotev1.RpcEnvelope, err error) {
	if response == nil || response.GetResponse() == nil {
		return
	}
	switch {
	case errors.Is(err, errRPCInvalid):
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid conversation stream subscription", false)
	case errors.Is(err, errRPCNotFound):
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND, "resource not found", false)
	case errors.Is(err, errRPCProject):
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH, "project is not authorized for this session", false)
	case errors.Is(err, errRPCBusy):
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "conversation stream is unavailable", true)
	case errors.Is(err, errRPCRevision):
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT, "generation does not belong to this conversation", false)
	case errors.Is(err, errRPCCapability):
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE, "CAPABILITY_UNSUPPORTED", false)
	default:
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "conversation stream is unavailable", true)
	}
}

// sortedPendingSequences is intentionally tiny and only used by tests and
// diagnostics that need a deterministic snapshot without content logging.
func (subscriber *conversationStreamSubscriber) sortedPendingSequences() []uint64 {
	if subscriber == nil {
		return nil
	}
	subscriber.mu.Lock()
	defer subscriber.mu.Unlock()
	sequences := make([]uint64, 0, len(subscriber.pending))
	for sequence := range subscriber.pending {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(left, right int) bool { return sequences[left] < sequences[right] })
	return sequences
}
