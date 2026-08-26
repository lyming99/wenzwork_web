package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/proto"
)

const (
	maximumPeerRPCInFlight              = 8
	maximumPeerRPCInFlightPerConnection = 32
	maximumPeerRPCReplay                = 128
)

// peerRPCConnectionLimiter caps work across independently authorised logical
// Peer sessions that share one physical Relay connection. It complements (and
// never replaces) the per-session limit above.
type peerRPCConnectionLimiter struct {
	slots chan struct{}
}

func newPeerRPCConnectionLimiter(limit int) *peerRPCConnectionLimiter {
	if limit < 1 {
		limit = maximumPeerRPCInFlightPerConnection
	}
	return &peerRPCConnectionLimiter{slots: make(chan struct{}, limit)}
}

func (limiter *peerRPCConnectionLimiter) tryAcquire() bool {
	if limiter == nil {
		return true
	}
	select {
	case limiter.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (limiter *peerRPCConnectionLimiter) release() {
	if limiter == nil {
		return
	}
	select {
	case <-limiter.slots:
	default:
	}
}

type peerRPCDispatch func(context.Context, *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope)
type peerRPCSend func(*remotev1.PeerCiphertext, *remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) error
type peerRPCLiveDispatch func(context.Context, *remotev1.RpcEnvelope, func(*remotev1.RpcEnvelope) error) *remotev1.RpcEnvelope
type peerRPCSendDelta func(*remotev1.PeerCiphertext, *remotev1.RpcEnvelope) error

// Traits keep a long-lived subscription out of the ordinary idempotency
// replay cache. Its own durable event journal is the replay source.
type peerRPCMethodTraits struct {
	live              bool
	cacheReplayEvents bool
	durableAfterRoute bool
}

func peerRPCMethodTraitsFor(method string) peerRPCMethodTraits {
	if method == "event.subscribe" || method == "conversation.generation.attach" {
		return peerRPCMethodTraits{live: true, cacheReplayEvents: false, durableAfterRoute: false}
	}
	return peerRPCMethodTraits{cacheReplayEvents: true}
}

type peerRPCInFlight struct {
	queryID        string
	digest         [sha256.Size]byte
	cancel         context.CancelFunc
	explicitCancel *peerRPCExplicitCancellation
	traits         peerRPCMethodTraits
	limiter        *peerRPCConnectionLimiter
	releaseOnce    sync.Once
}

func (inFlight *peerRPCInFlight) releaseConnectionSlot() {
	if inFlight == nil || inFlight.limiter == nil {
		return
	}
	inFlight.releaseOnce.Do(inFlight.limiter.release)
}

type peerRPCExplicitCancellation struct {
	done chan struct{}
	once sync.Once
}

type peerRPCExplicitCancellationContextKey struct{}
type peerRPCTransportCancellationContextKey struct{}

func newPeerRPCExplicitCancellation() *peerRPCExplicitCancellation {
	return &peerRPCExplicitCancellation{done: make(chan struct{})}
}

func (cancellation *peerRPCExplicitCancellation) cancel() {
	if cancellation != nil {
		cancellation.once.Do(func() { close(cancellation.done) })
	}
}

func withPeerRPCExplicitCancellation(ctx context.Context) (context.Context, *peerRPCExplicitCancellation) {
	cancellation := newPeerRPCExplicitCancellation()
	return context.WithValue(ctx, peerRPCExplicitCancellationContextKey{}, cancellation), cancellation
}

// withPeerRPCTransportCancellation marks a request whose Context is owned only
// by its encrypted transport Stream. remote/v2 generation cancellation is a
// separate conversation.cancel RPC, so closing an obsolete Stream must detach
// its observer without stopping the durable device-side generation.
func withPeerRPCTransportCancellation(ctx context.Context) context.Context {
	return context.WithValue(ctx, peerRPCTransportCancellationContextKey{}, struct{}{})
}

// contextWithoutPeerTransportCancellation detaches durable device work from
// its logical Peer route. Legacy Peer requests preserve authenticated
// PEER_CANCEL; marked remote/v2 requests use conversation.cancel instead.
// Unmarked direct calls keep their original Context semantics.
func contextWithoutPeerTransportCancellation(ctx context.Context) (context.Context, context.CancelFunc) {
	cancellation, ok := ctx.Value(peerRPCExplicitCancellationContextKey{}).(*peerRPCExplicitCancellation)
	_, transportOnly := ctx.Value(peerRPCTransportCancellationContextKey{}).(struct{})
	if (!ok || cancellation == nil) && !transportOnly {
		return ctx, func() {}
	}
	durable, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if ok && cancellation != nil {
		go func() {
			select {
			case <-cancellation.done:
				cancel()
			case <-durable.Done():
			}
		}()
	}
	return durable, cancel
}

type peerRPCReplay struct {
	digest   [sha256.Size]byte
	response *remotev1.RpcEnvelope
	events   []*remotev1.RpcEnvelope
}

// peerRPCExecutor owns the bounded, session-scoped execution state. Ciphertext
// opening remains on the Relay read loop so inbound sequence validation is
// strictly ordered; accepted requests execute independently here.
type peerRPCExecutor struct {
	ctx               context.Context
	cancel            context.CancelFunc
	expiresAt         time.Time
	now               func() time.Time
	dispatch          peerRPCDispatch
	liveDispatch      peerRPCLiveDispatch
	send              peerRPCSend
	sendDelta         peerRPCSendDelta
	onFatal           func(error)
	connectionLimiter *peerRPCConnectionLimiter

	mu              sync.Mutex
	closed          bool
	inFlightByKey   map[string]*peerRPCInFlight
	inFlightByQuery map[string]*peerRPCInFlight
	replay          map[string]peerRPCReplay
	replayOrder     []string
	wg              sync.WaitGroup
}

func newLivePeerRPCExecutor(parent context.Context, expiresAt time.Time, dispatch peerRPCDispatch, live peerRPCLiveDispatch, send peerRPCSend, sendDelta peerRPCSendDelta, onFatal func(error)) *peerRPCExecutor {
	executor := newPeerRPCExecutor(parent, expiresAt, dispatch, send, onFatal)
	executor.liveDispatch, executor.sendDelta = live, sendDelta
	return executor
}

func newPeerRPCExecutor(parent context.Context, expiresAt time.Time, dispatch peerRPCDispatch, send peerRPCSend, onFatal func(error)) *peerRPCExecutor {
	ctx, cancel := context.WithCancel(parent)
	return &peerRPCExecutor{
		ctx: ctx, cancel: cancel, expiresAt: expiresAt, now: func() time.Time { return time.Now().UTC() },
		dispatch: dispatch, send: send, onFatal: onFatal,
		inFlightByKey: make(map[string]*peerRPCInFlight), inFlightByQuery: make(map[string]*peerRPCInFlight),
		replay: make(map[string]peerRPCReplay), replayOrder: make([]string, 0, maximumPeerRPCReplay),
	}
}

func (executor *peerRPCExecutor) setConnectionLimiter(limiter *peerRPCConnectionLimiter) {
	if executor == nil {
		return
	}
	executor.mu.Lock()
	executor.connectionLimiter = limiter
	executor.mu.Unlock()
}

func (executor *peerRPCExecutor) submit(query *remotev1.PeerCiphertext, envelope *remotev1.RpcEnvelope) error {
	if executor == nil {
		return errors.New("Peer RPC executor is unavailable")
	}
	var request *remotev1.RpcRequest
	var header *remotev1.RpcRequestHeader
	requestID := ""
	if envelope != nil {
		request = envelope.GetRequest()
	}
	if request != nil {
		header = request.GetHeader()
	}
	if header != nil {
		requestID = header.GetRequestId()
	}
	if query == nil || request == nil || header == nil || uuid.Validate(requestID) != nil ||
		!validPeerIdempotencyKey(header.GetIdempotencyKey()) {
		return executor.sendImmediate(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid request", false, 0))
	}

	deadline, ok := executor.requestDeadline(query, header)
	if !ok {
		return executor.sendImmediate(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid request deadline", false, 0))
	}
	digest := peerRPCRequestDigest(request)
	key := header.GetIdempotencyKey()
	traits := peerRPCMethodTraitsFor(request.GetMethod())

	executor.mu.Lock()
	if executor.closed {
		executor.mu.Unlock()
		return context.Canceled
	}
	if traits.cacheReplayEvents {
		if cached, found := executor.replay[key]; found {
			executor.mu.Unlock()
			if cached.digest != digest {
				return executor.sendImmediate(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_IDEMPOTENCY_CONFLICT, "idempotency key conflicts with another request", false, 0))
			}
			response, events := replayPeerRPC(cached, requestID)
			return executor.send(query, response, events)
		}
	}
	if active, found := executor.inFlightByKey[key]; found {
		executor.mu.Unlock()
		if active.digest != digest {
			return executor.sendImmediate(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_IDEMPOTENCY_CONFLICT, "idempotency key conflicts with another request", false, 0))
		}
		return executor.sendImmediate(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "request is still in progress", true, 1))
	}
	if _, duplicateQuery := executor.inFlightByQuery[query.GetQueryId()]; duplicateQuery || len(executor.inFlightByQuery) >= maximumPeerRPCInFlight {
		executor.mu.Unlock()
		return executor.sendImmediate(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED, "session request limit reached", true, 1))
	}
	limiter := executor.connectionLimiter
	if limiter != nil && !limiter.tryAcquire() {
		executor.mu.Unlock()
		return executor.sendImmediate(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED, "connection request limit reached", true, 1))
	}
	requestContext, cancel := context.WithDeadline(executor.ctx, deadline)
	requestContext, explicitCancel := withPeerRPCExplicitCancellation(requestContext)
	active := &peerRPCInFlight{queryID: query.GetQueryId(), digest: digest, cancel: cancel, explicitCancel: explicitCancel, traits: traits, limiter: limiter}
	executor.inFlightByKey[key] = active
	executor.inFlightByQuery[active.queryID] = active
	executor.wg.Add(1)
	executor.mu.Unlock()

	queryCopy := proto.Clone(query).(*remotev1.PeerCiphertext)
	requestCopy := proto.Clone(envelope).(*remotev1.RpcEnvelope)
	go executor.execute(requestContext, key, active, queryCopy, requestCopy)
	return nil
}

func (executor *peerRPCExecutor) execute(ctx context.Context, key string, active *peerRPCInFlight, query *remotev1.PeerCiphertext, request *remotev1.RpcEnvelope) {
	defer executor.wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			executor.finishPanickedQuery(key, active, query, request)
		}
	}()
	var response *remotev1.RpcEnvelope
	var events []*remotev1.RpcEnvelope
	if executor.liveDispatch != nil && executor.sendDelta != nil {
		response = executor.liveDispatch(ctx, request, func(event *remotev1.RpcEnvelope) error {
			if err := executor.sendDelta(query, event); err != nil {
				return err
			}
			if active.traits.cacheReplayEvents {
				events = append(events, cloneRPCEnvelope(event))
			}
			return nil
		})
	} else {
		response, events = executor.dispatch(ctx, request)
	}
	active.cancel()
	terminal := peerRPCReplay{digest: active.digest, response: cloneRPCEnvelope(response), events: cloneRPCEnvelopes(events)}

	executor.mu.Lock()
	delete(executor.inFlightByKey, key)
	delete(executor.inFlightByQuery, active.queryID)
	closed := executor.closed
	if !closed && active.traits.cacheReplayEvents {
		executor.storeReplayLocked(key, terminal)
	}
	executor.mu.Unlock()
	active.releaseConnectionSlot()
	if closed {
		return
	}
	finalEvents := events
	if executor.liveDispatch != nil && executor.sendDelta != nil {
		// Live events were already delivered as PEER_DELTA frames. They remain
		// in the replay entry, but must not be duplicated before this completion.
		finalEvents = nil
	}
	if err := executor.send(query, response, finalEvents); err != nil {
		// A session-scoped protocol rejection closes the executor before any
		// in-flight work is cancelled. A late completion must not escalate that
		// expected per-session shutdown into a physical Relay disconnect.
		if !executor.isClosed() && executor.onFatal != nil {
			executor.onFatal(err)
		}
	}
}

// finishPanickedQuery keeps a fault in one dispatcher/stream worker inside its
// query boundary. In particular, a provider panic must not tear down the Peer
// session or the physical Relay connection shared by other workloads.
func (executor *peerRPCExecutor) finishPanickedQuery(key string, active *peerRPCInFlight, query *remotev1.PeerCiphertext, request *remotev1.RpcEnvelope) {
	if executor == nil || active == nil {
		return
	}
	active.cancel()
	executor.mu.Lock()
	delete(executor.inFlightByKey, key)
	delete(executor.inFlightByQuery, active.queryID)
	closed := executor.closed
	executor.mu.Unlock()
	active.releaseConnectionSlot()
	if closed || executor.send == nil {
		return
	}
	requestID := ""
	if request != nil {
		if rpcRequest := request.GetRequest(); rpcRequest != nil {
			if header := rpcRequest.GetHeader(); header != nil {
				requestID = header.GetRequestId()
			}
		}
	}
	// A bad sender must not turn panic recovery itself into another process-wide
	// panic. Normal sender errors remain scoped by the configured onFatal hook.
	func() {
		defer func() { _ = recover() }()
		if err := executor.send(query, peerRPCError(requestID, remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "device request failed", true, 0), nil); err != nil && executor.onFatal != nil {
			executor.onFatal(err)
		}
	}()
}

func (executor *peerRPCExecutor) cancelQuery(queryID string) {
	if executor == nil {
		return
	}
	executor.mu.Lock()
	active := executor.inFlightByQuery[queryID]
	executor.mu.Unlock()
	if active != nil {
		active.explicitCancel.cancel()
		active.cancel()
	}
}

func (executor *peerRPCExecutor) close() {
	if executor == nil {
		return
	}
	executor.stop()
	executor.wg.Wait()
}

// stop makes a logical Peer session terminal without waiting for providers or
// other cancellable work to unwind. The Relay read loop uses this to isolate a
// malformed encrypted request to its own session rather than interrupting all
// sessions on the physical device connection.
func (executor *peerRPCExecutor) stop() {
	if executor == nil {
		return
	}
	executor.mu.Lock()
	if executor.closed {
		executor.mu.Unlock()
		return
	}
	executor.closed = true
	executor.cancel()
	cancels := make([]context.CancelFunc, 0, len(executor.inFlightByQuery))
	for _, active := range executor.inFlightByQuery {
		cancels = append(cancels, active.cancel)
	}
	executor.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (executor *peerRPCExecutor) isClosed() bool {
	if executor == nil {
		return true
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.closed
}

func (executor *peerRPCExecutor) requestDeadline(query *remotev1.PeerCiphertext, header *remotev1.RpcRequestHeader) (time.Time, bool) {
	if query.GetDeadline() == nil || query.GetDeadline().CheckValid() != nil {
		return time.Time{}, false
	}
	deadline := query.GetDeadline().AsTime().UTC()
	if header.GetDeadline() != nil {
		if header.GetDeadline().CheckValid() != nil {
			return time.Time{}, false
		}
		deadline = earlierTime(deadline, header.GetDeadline().AsTime().UTC())
	}
	// A zero expiry means the established Peer is intentionally long-lived.
	// Its ticket was validated during PEER_OPEN, so only the encrypted request
	// deadlines constrain individual RPCs from this point onward.
	if !executor.expiresAt.IsZero() {
		deadline = earlierTime(deadline, executor.expiresAt.UTC())
	}
	return deadline, deadline.After(executor.now().UTC())
}

func (executor *peerRPCExecutor) sendImmediate(query *remotev1.PeerCiphertext, response *remotev1.RpcEnvelope) error {
	if executor == nil || executor.send == nil {
		return errors.New("Peer RPC executor is unavailable")
	}
	return executor.send(query, response, nil)
}

func (executor *peerRPCExecutor) storeReplayLocked(key string, value peerRPCReplay) {
	if _, exists := executor.replay[key]; exists {
		executor.replay[key] = value
		return
	}
	if len(executor.replayOrder) == maximumPeerRPCReplay {
		oldest := executor.replayOrder[0]
		delete(executor.replay, oldest)
		copy(executor.replayOrder, executor.replayOrder[1:])
		executor.replayOrder = executor.replayOrder[:len(executor.replayOrder)-1]
	}
	executor.replay[key] = value
	executor.replayOrder = append(executor.replayOrder, key)
}

func peerRPCError(requestID string, code remotev1.RpcErrorCode, message string, retryable bool, retryAfter uint32) *remotev1.RpcEnvelope {
	return &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Response{Response: &remotev1.RpcResponse{
			Header: &remotev1.RpcResponseHeader{RequestId: requestID},
			Error:  &remotev1.RpcError{Code: code, SafeMessage: message, Retryable: retryable, RetryAfterSeconds: retryAfter},
		}},
	}
}

func peerRPCRequestDigest(request *remotev1.RpcRequest) [sha256.Size]byte {
	encoded := make([]byte, 0, len(request.GetMethod())+len(request.GetJsonPayload())+64)
	encoded = appendDigestField(encoded, []byte("wenzwork-peer-rpc-idempotency-v1"))
	encoded = appendDigestField(encoded, []byte(request.GetMethod()))
	encoded = appendDigestField(encoded, request.GetJsonPayload())
	if request.GetHeader().ExpectedRevision == nil {
		encoded = append(encoded, 0)
	} else {
		encoded = append(encoded, 1)
		encoded = binary.BigEndian.AppendUint64(encoded, request.GetHeader().GetExpectedRevision())
	}
	return sha256.Sum256(encoded)
}

func appendDigestField(destination, value []byte) []byte {
	destination = binary.BigEndian.AppendUint32(destination, uint32(len(value)))
	return append(destination, value...)
}

func validPeerIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') && character != '.' && character != '_' && character != ':' && character != '-' {
			return false
		}
	}
	return true
}

func replayPeerRPC(cached peerRPCReplay, requestID string) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
	response := cloneRPCEnvelope(cached.response)
	events := cloneRPCEnvelopes(cached.events)
	if header := response.GetResponse().GetHeader(); header != nil {
		header.RequestId = requestID
		header.Replayed = true
	}
	for _, event := range events {
		if value := event.GetEvent(); value != nil {
			value.RequestId = requestID
		}
	}
	return response, events
}

func cloneRPCEnvelope(value *remotev1.RpcEnvelope) *remotev1.RpcEnvelope {
	if value == nil {
		return nil
	}
	return proto.Clone(value).(*remotev1.RpcEnvelope)
}

func cloneRPCEnvelopes(values []*remotev1.RpcEnvelope) []*remotev1.RpcEnvelope {
	result := make([]*remotev1.RpcEnvelope, 0, len(values))
	for _, value := range values {
		result = append(result, cloneRPCEnvelope(value))
	}
	return result
}

func earlierTime(left, right time.Time) time.Time {
	if right.Before(left) {
		return right
	}
	return left
}
