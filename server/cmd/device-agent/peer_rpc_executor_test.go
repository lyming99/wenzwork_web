package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/peerprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type recordedPeerRPC struct {
	query    *remotev1.PeerCiphertext
	response *remotev1.RpcEnvelope
	events   []*remotev1.RpcEnvelope
}

func TestPeerRPCExecutorSendsLiveDeltasBeforeCompletionAndReplaysThem(t *testing.T) {
	completed := make(chan recordedPeerRPC, 2)
	deltas := make(chan *remotev1.RpcEnvelope, 2)
	dispatch := func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
	}
	live := func(_ context.Context, request *remotev1.RpcEnvelope, emit func(*remotev1.RpcEnvelope) error) *remotev1.RpcEnvelope {
		if err := emit(peerRPCEvent(request.GetRequest().GetHeader().GetRequestId(), "first")); err != nil {
			t.Fatal(err)
		}
		if err := emit(peerRPCEvent(request.GetRequest().GetHeader().GetRequestId(), "second")); err != nil {
			t.Fatal(err)
		}
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`))
	}
	executor := newLivePeerRPCExecutor(context.Background(), time.Now().Add(time.Hour), dispatch, live, recordPeerRPC(completed), func(_ *remotev1.PeerCiphertext, event *remotev1.RpcEnvelope) error { deltas <- event; return nil }, nil)
	defer executor.close()
	key := "live-delta-key-01"
	request := peerRPCRequest(uuid.NewString(), key, "conversation.send", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(2*time.Minute)), request); err != nil {
		t.Fatal(err)
	}
	if first, second := <-deltas, <-deltas; first.GetEvent().GetEventId() != "first" || second.GetEvent().GetEventId() != "second" {
		t.Fatalf("live events = %v, %v", first, second)
	}
	if result := receivePeerRPC(t, completed); len(result.events) != 0 {
		t.Fatalf("completion duplicated live events: %d", len(result.events))
	}
	replay := peerRPCRequest(uuid.NewString(), key, "conversation.send", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(2*time.Minute)), replay); err != nil {
		t.Fatal(err)
	}
	if result := receivePeerRPC(t, completed); !result.response.GetResponse().GetHeader().GetReplayed() || len(result.events) != 2 {
		t.Fatalf("replay = %+v", result)
	}
}

func TestPeerRPCExecutorReplaysAndRejectsConflictingKey(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan recordedPeerRPC, 8)
	var calls atomic.Int32
	dispatch := func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		calls.Add(1)
		requestID := request.GetRequest().GetHeader().GetRequestId()
		return peerRPCSuccess(requestID, []byte(`{"value":1}`)), []*remotev1.RpcEnvelope{peerRPCEvent(requestID, "stable-event")}
	}
	executor := newPeerRPCExecutor(ctx, time.Now().Add(time.Hour), dispatch, recordPeerRPC(sent), nil)
	defer executor.close()

	key := "replay-key-0001"
	firstRequest := peerRPCRequest(uuid.NewString(), key, "project.list", []byte(`{"page":1}`), time.Now().Add(time.Minute))
	firstQuery := peerRPCQuery(uuid.NewString(), time.Now().Add(2*time.Minute))
	if err := executor.submit(firstQuery, firstRequest); err != nil {
		t.Fatal(err)
	}
	first := receivePeerRPC(t, sent)
	if first.response.GetResponse().GetHeader().GetReplayed() || calls.Load() != 1 || len(first.events) != 1 {
		t.Fatalf("first result replayed=%v calls=%d events=%d", first.response.GetResponse().GetHeader().GetReplayed(), calls.Load(), len(first.events))
	}

	secondRequestID := uuid.NewString()
	secondRequest := peerRPCRequest(secondRequestID, key, "project.list", []byte(`{"page":1}`), time.Now().Add(3*time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(4*time.Minute)), secondRequest); err != nil {
		t.Fatal(err)
	}
	replayed := receivePeerRPC(t, sent)
	if calls.Load() != 1 || !replayed.response.GetResponse().GetHeader().GetReplayed() ||
		replayed.response.GetResponse().GetHeader().GetRequestId() != secondRequestID || len(replayed.events) != 1 ||
		replayed.events[0].GetEvent().GetRequestId() != secondRequestID || replayed.events[0].GetEvent().GetEventId() != "stable-event" {
		t.Fatalf("unexpected replay: calls=%d response=%v events=%v", calls.Load(), replayed.response, replayed.events)
	}

	conflicting := peerRPCRequest(uuid.NewString(), key, "project.list", []byte(`{"page":1}`), time.Now().Add(time.Minute))
	expectedRevision := uint64(2)
	conflicting.GetRequest().GetHeader().ExpectedRevision = &expectedRevision
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(2*time.Minute)), conflicting); err != nil {
		t.Fatal(err)
	}
	conflict := receivePeerRPC(t, sent).response.GetResponse().GetError()
	if conflict.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_IDEMPOTENCY_CONFLICT || conflict.GetRetryable() || calls.Load() != 1 {
		t.Fatalf("conflict=%v calls=%d", conflict, calls.Load())
	}
}

func TestPeerRPCReplayCacheIsBounded(t *testing.T) {
	sent := make(chan recordedPeerRPC, 1)
	var calls atomic.Int32
	dispatch := func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		calls.Add(1)
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
	}
	executor := newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour), dispatch, recordPeerRPC(sent), nil)
	defer executor.close()
	for index := 0; index <= maximumPeerRPCReplay; index++ {
		request := peerRPCRequest(uuid.NewString(), fmt.Sprintf("cache-key-%03d", index), "project.list", []byte(`{}`), time.Now().Add(time.Minute))
		if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), request); err != nil {
			t.Fatal(err)
		}
		_ = receivePeerRPC(t, sent)
	}
	executor.mu.Lock()
	cacheSize := len(executor.replay)
	_, oldestPresent := executor.replay["cache-key-000"]
	executor.mu.Unlock()
	if cacheSize != maximumPeerRPCReplay || oldestPresent {
		t.Fatalf("cache size=%d oldestPresent=%v", cacheSize, oldestPresent)
	}
	request := peerRPCRequest(uuid.NewString(), "cache-key-000", "project.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), request); err != nil {
		t.Fatal(err)
	}
	result := receivePeerRPC(t, sent)
	if result.response.GetResponse().GetHeader().GetReplayed() || calls.Load() != maximumPeerRPCReplay+2 {
		t.Fatalf("evicted request replayed=%v calls=%d", result.response.GetResponse().GetHeader().GetReplayed(), calls.Load())
	}
}

func TestPeerRPCExecutorBoundsInFlightAndMarksDuplicateBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, maximumPeerRPCInFlight)
	release := make(chan struct{})
	sent := make(chan recordedPeerRPC, maximumPeerRPCInFlight+4)
	dispatch := func(ctx context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
	}
	executor := newPeerRPCExecutor(ctx, time.Now().Add(time.Hour), dispatch, recordPeerRPC(sent), nil)
	defer executor.close()

	for index := 0; index < maximumPeerRPCInFlight; index++ {
		query := peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute))
		request := peerRPCRequest(uuid.NewString(), "bounded-key-"+string(rune('a'+index)), "project.list", []byte(`{}`), time.Now().Add(time.Minute))
		if err := executor.submit(query, request); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < maximumPeerRPCInFlight; index++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("accepted request did not start")
		}
	}

	ninth := peerRPCRequest(uuid.NewString(), "bounded-key-z", "project.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), ninth); err != nil {
		t.Fatal(err)
	}
	busy := receivePeerRPC(t, sent).response.GetResponse().GetError()
	if busy.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED || !busy.GetRetryable() || busy.GetRetryAfterSeconds() != 1 {
		t.Fatalf("limit response = %v", busy)
	}

	duplicate := peerRPCRequest(uuid.NewString(), "bounded-key-a", "project.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), duplicate); err != nil {
		t.Fatal(err)
	}
	duplicateBusy := receivePeerRPC(t, sent).response.GetResponse().GetError()
	if duplicateBusy.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY {
		t.Fatalf("duplicate response = %v", duplicateBusy)
	}
	close(release)
	for index := 0; index < maximumPeerRPCInFlight; index++ {
		_ = receivePeerRPC(t, sent)
	}
	retryRequestID := uuid.NewString()
	retry := peerRPCRequest(retryRequestID, "bounded-key-a", "project.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), retry); err != nil {
		t.Fatal(err)
	}
	replayed := receivePeerRPC(t, sent).response.GetResponse().GetHeader()
	if !replayed.GetReplayed() || replayed.GetRequestId() != retryRequestID {
		t.Fatalf("completed duplicate was not replayed: %v", replayed)
	}
}

func TestPeerRPCExecutorTaskFailureDoesNotBlockAIRequest(t *testing.T) {
	taskStarted := make(chan struct{}, 1)
	releaseTask := make(chan struct{})
	sent := make(chan recordedPeerRPC, 2)
	dispatch := func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		switch request.GetRequest().GetMethod() {
		case "task.list":
			taskStarted <- struct{}{}
			<-releaseTask
			return peerRPCError(request.GetRequest().GetHeader().GetRequestId(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "task read failed", true, 0), nil
		case "conversation.list":
			return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{"items":[]}`)), nil
		default:
			t.Fatalf("unexpected method %q", request.GetRequest().GetMethod())
			return nil, nil
		}
	}
	executor := newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour), dispatch, recordPeerRPC(sent), nil)
	defer executor.close()

	task := peerRPCRequest(uuid.NewString(), "task-failure-key", "task.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), task); err != nil {
		t.Fatal(err)
	}
	select {
	case <-taskStarted:
	case <-time.After(time.Second):
		t.Fatal("task request did not start")
	}

	aiRequestID := uuid.NewString()
	ai := peerRPCRequest(aiRequestID, "ai-list-key-0001", "conversation.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), ai); err != nil {
		t.Fatal(err)
	}
	aiResult := receivePeerRPC(t, sent).response.GetResponse()
	if aiResult.GetHeader().GetRequestId() != aiRequestID || aiResult.GetError() != nil {
		t.Fatalf("AI request waited behind failed task request: %+v", aiResult)
	}

	close(releaseTask)
	taskResult := receivePeerRPC(t, sent).response.GetResponse()
	if taskResult.GetError().GetSafeMessage() != "task read failed" {
		t.Fatalf("task failure response = %+v", taskResult)
	}
}

func TestPeerRPCExecutorConvertsDispatcherPanicToQueryInternal(t *testing.T) {
	sent := make(chan recordedPeerRPC, 2)
	var calls atomic.Int32
	dispatch := func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		if calls.Add(1) == 1 {
			panic("simulated provider panic")
		}
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
	}
	executor := newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour), dispatch, recordPeerRPC(sent), nil)
	defer executor.close()

	panicRequestID := uuid.NewString()
	panicRequest := peerRPCRequest(panicRequestID, "panic-key-0001", "project.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), panicRequest); err != nil {
		t.Fatal(err)
	}
	panicResult := receivePeerRPC(t, sent).response.GetResponse()
	if panicResult.GetHeader().GetRequestId() != panicRequestID || panicResult.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL {
		t.Fatalf("panic response = %+v", panicResult)
	}

	// The executor remains usable after the failed query; a worker panic must
	// never take down the Agent process, session, or its shared Relay carrier.
	secondRequestID := uuid.NewString()
	secondRequest := peerRPCRequest(secondRequestID, "panic-key-0002", "project.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), secondRequest); err != nil {
		t.Fatal(err)
	}
	secondResult := receivePeerRPC(t, sent).response.GetResponse()
	if secondResult.GetHeader().GetRequestId() != secondRequestID || secondResult.GetError() != nil {
		t.Fatalf("executor did not recover after panic: %+v", secondResult)
	}
}

func TestPeerRPCConnectionLimiterCapsWorkAcrossSessions(t *testing.T) {
	limiter := newPeerRPCConnectionLimiter(1)
	firstStarted := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	firstSent := make(chan recordedPeerRPC, 1)
	secondSent := make(chan recordedPeerRPC, 2)
	first := newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour), func(ctx context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		firstStarted <- struct{}{}
		select {
		case <-releaseFirst:
		case <-ctx.Done():
		}
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
	}, recordPeerRPC(firstSent), nil)
	second := newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour), func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
	}, recordPeerRPC(secondSent), nil)
	first.setConnectionLimiter(limiter)
	second.setConnectionLimiter(limiter)
	defer first.close()
	defer second.close()

	if err := first.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), peerRPCRequest(uuid.NewString(), "connection-cap-a", "project.list", []byte(`{}`), time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first session request did not start")
	}

	busyRequestID := uuid.NewString()
	if err := second.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), peerRPCRequest(busyRequestID, "connection-cap-b", "project.list", []byte(`{}`), time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	busy := receivePeerRPC(t, secondSent).response.GetResponse()
	if busy.GetHeader().GetRequestId() != busyRequestID || busy.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED {
		t.Fatalf("connection cap response = %+v", busy)
	}

	close(releaseFirst)
	_ = receivePeerRPC(t, firstSent)
	readyRequestID := uuid.NewString()
	if err := second.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), peerRPCRequest(readyRequestID, "connection-cap-b", "project.list", []byte(`{}`), time.Now().Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	ready := receivePeerRPC(t, secondSent).response.GetResponse()
	if ready.GetHeader().GetRequestId() != readyRequestID || ready.GetError() != nil {
		t.Fatalf("connection slot was not released: %+v", ready)
	}
}

func TestInvalidPeerCiphertextIsClassifiedAsSessionScoped(t *testing.T) {
	sessionID := uuid.NewString()
	keys := peerprotocol.SessionKeys{SourceToTarget: make([]byte, 32), TargetToSource: make([]byte, 32)}
	for index := range keys.SourceToTarget {
		keys.SourceToTarget[index] = byte(index + 1)
		keys.TargetToSource[index] = byte(index + 33)
	}
	session := &targetPeerSession{
		claims: remoteauth.Claims{SessionID: sessionID}, keys: keys, expiresAt: time.Now().Add(time.Hour),
		executor: newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour),
			func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
				return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
			}, func(_ *remotev1.PeerCiphertext, _ *remotev1.RpcEnvelope, _ []*remotev1.RpcEnvelope) error { return nil }, nil),
	}
	defer session.executor.close()

	err := handlePeerRPC(session, &remotev1.PeerCiphertext{
		SessionId: sessionID, QueryId: uuid.NewString(), Generation: 1, MessageSequence: 1,
		Deadline: timestamppb.New(time.Now().Add(time.Minute)), Ciphertext: []byte{1, 2, 3},
	})
	if !errors.Is(err, errPeerSessionProtocol) {
		t.Fatalf("invalid ciphertext error = %v, want session-scoped protocol error", err)
	}
}

func TestRetiringPeerSessionLeavesOtherSessionsUsable(t *testing.T) {
	newExecutor := func() *peerRPCExecutor {
		return newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour),
			func(_ context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
				return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
			}, func(_ *remotev1.PeerCiphertext, _ *remotev1.RpcEnvelope, _ []*remotev1.RpcEnvelope) error { return nil }, nil)
	}
	firstID, secondID := uuid.NewString(), uuid.NewString()
	first := &targetPeerSession{claims: remoteauth.Claims{SessionID: firstID}, executor: newExecutor()}
	second := &targetPeerSession{claims: remoteauth.Claims{SessionID: secondID}, executor: newExecutor()}
	defer second.executor.close()
	sessions := map[string]*targetPeerSession{firstID: first, secondID: second}

	retired := retireTargetPeerSession(sessions, firstID)
	if retired != first || !first.closing.Load() || !first.executor.isClosed() {
		t.Fatalf("invalid session was not retired: %+v", retired)
	}
	if sessions[secondID] != second || second.closing.Load() || second.executor.isClosed() {
		t.Fatal("retiring one Peer session interrupted another logical session")
	}
}

func TestTargetPeerSessionInboundQueueIsBoundedPerSession(t *testing.T) {
	first := &targetPeerSession{inbound: make(chan targetPeerInboundFrame, maximumTargetPeerSessionInbound)}
	second := &targetPeerSession{inbound: make(chan targetPeerInboundFrame, maximumTargetPeerSessionInbound)}
	for index := 0; index < maximumTargetPeerSessionInbound; index++ {
		if !first.enqueueInbound(targetPeerInboundFrame{query: &remotev1.PeerCiphertext{QueryId: uuid.NewString()}}) {
			t.Fatalf("first session rejected frame %d before its own bound", index)
		}
	}
	if first.enqueueInbound(targetPeerInboundFrame{query: &remotev1.PeerCiphertext{QueryId: uuid.NewString()}}) {
		t.Fatal("first session accepted an unbounded inbound frame")
	}
	if !second.enqueueInbound(targetPeerInboundFrame{query: &remotev1.PeerCiphertext{QueryId: uuid.NewString()}}) {
		t.Fatal("first session saturation blocked an independent session")
	}
}

func TestPeerCancelAbortsOpenAIRequestWithoutMutation(t *testing.T) {
	providerStarted := make(chan struct{})
	providerRelease := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		startedOnce.Do(func() { close(providerStarted) })
		select {
		case <-request.Context().Done():
		case <-providerRelease:
		}
	}))
	t.Cleanup(server.Close)

	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.AIConfigs["default"] = aiConfig{ID: "default", Name: "Test", Provider: "openai-compatible", BaseURL: server.URL, Model: "model-a", Enabled: true, Revision: 1}
	state.mu.Unlock()

	sent := make(chan recordedPeerRPC, 2)
	dispatch := dispatcher{state: state, scope: "remote.peer.ai.chat", now: func() time.Time { return time.Now().UTC() }}
	created := dispatchJSON(t, dispatch, "conversation.create", `{"title":"Cancel"}`)
	conversationID := created["id"].(string)
	dispatchContext := make(chan context.Context, 1)
	dispatchStream := func(ctx context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		dispatchContext <- ctx
		return dispatch.dispatchStream(ctx, request)
	}
	sessionID, generation := uuid.NewString(), uint64(9)
	keys := peerprotocol.SessionKeys{SourceToTarget: make([]byte, 32), TargetToSource: make([]byte, 32)}
	for index := range keys.SourceToTarget {
		keys.SourceToTarget[index] = byte(index + 1)
		keys.TargetToSource[index] = byte(index + 33)
	}
	session := &targetPeerSession{claims: remoteauth.Claims{SessionID: sessionID}, keys: keys, expiresAt: time.Now().Add(time.Hour)}
	session.executor = newPeerRPCExecutor(context.Background(), session.expiresAt, dispatchStream, recordPeerRPC(sent), nil)
	defer session.executor.close()
	sourceSealer, err := peerprotocol.NewCipherState(keys.SourceToTarget, peerprotocol.DirectionSourceToTarget, peerprotocol.CipherModeSeal, generation)
	if err != nil {
		t.Fatal(err)
	}

	queryID := uuid.NewString()
	request := peerRPCRequest(uuid.NewString(), "cancel-chat-key", "conversation.send", []byte(`{"conversationId":"`+conversationID+`","content":"wait"}`), time.Now().Add(time.Minute))
	query := encryptedPeerRPC(t, sourceSealer, "PEER_QUERY", sessionID, queryID, time.Now().Add(time.Minute), request)
	if err := handlePeerRPC(session, query); err != nil {
		t.Fatal(err)
	}
	select {
	case <-providerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("OpenAI request did not start")
	}
	cancelRequest := peerRPCRequest(queryID, "cancel-wire-key", "rpc.cancel", []byte(`{}`), time.Now().Add(time.Minute))
	cancelFrame := encryptedPeerRPC(t, sourceSealer, "PEER_CANCEL", sessionID, queryID, time.Time{}, cancelRequest)
	if err := handlePeerCancel(session, cancelFrame); err != nil {
		t.Fatal(err)
	}
	requestContext := <-dispatchContext
	select {
	case <-requestContext.Done():
	case <-time.After(time.Second):
		t.Fatal("Peer cancel did not cancel the RPC context")
	}
	response := receivePeerRPC(t, sent).response.GetResponse()
	if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED {
		t.Fatalf("response = %v", response)
	}
	// The RPC can only complete after client.Do has returned, proving the
	// in-progress provider call was interrupted. Release the test handler too;
	// Windows may defer its HTTP/1 context notification until connection reuse.
	close(providerRelease)
	stored := dispatchJSON(t, dispatch, "conversation.get", `{"conversationId":"`+conversationID+`"}`)
	conversation := stored["conversation"].(map[string]any)
	messages := stored["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["content"] != "wait" || messages[1].(map[string]any)["status"] != "stopped" || conversation["state"] != "idle" {
		t.Fatalf("cancelled request conversation = %#v messages=%#v", conversation, messages)
	}
}

func TestPeerRPCUsesEarliestDeadline(t *testing.T) {
	now := time.Now().UTC()
	observed := make(chan time.Time, 1)
	sent := make(chan recordedPeerRPC, 1)
	dispatch := func(ctx context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		deadline, _ := ctx.Deadline()
		observed <- deadline
		return peerRPCSuccess(request.GetRequest().GetHeader().GetRequestId(), []byte(`{}`)), nil
	}
	executor := newPeerRPCExecutor(context.Background(), now.Add(4*time.Minute), dispatch, recordPeerRPC(sent), nil)
	defer executor.close()
	executor.now = func() time.Time { return now }
	request := peerRPCRequest(uuid.NewString(), "deadline-key-01", "project.list", []byte(`{}`), now.Add(2*time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), now.Add(3*time.Minute)), request); err != nil {
		t.Fatal(err)
	}
	deadline := <-observed
	if difference := deadline.Sub(now.Add(2 * time.Minute)); difference < -time.Millisecond || difference > time.Millisecond {
		t.Fatalf("deadline=%v expected=%v", deadline, now.Add(2*time.Minute))
	}
	_ = receivePeerRPC(t, sent)
}

func TestPeerRPCConcurrentSealingHasContiguousSequence(t *testing.T) {
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	const generation = uint64(17)
	sealer, err := peerprotocol.NewCipherState(key, peerprotocol.DirectionTargetToSource, peerprotocol.CipherModeSeal, generation)
	if err != nil {
		t.Fatal(err)
	}
	opener, err := peerprotocol.NewCipherState(key, peerprotocol.DirectionTargetToSource, peerprotocol.CipherModeOpen, generation)
	if err != nil {
		t.Fatal(err)
	}
	session := &targetPeerSession{sealer: sealer}
	const count = 64
	envelopes := make(chan *remotev1.Envelope, count)
	errorsChannel := make(chan error, count)
	var group sync.WaitGroup
	for index := 0; index < count; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			query := &remotev1.PeerCiphertext{SessionId: uuid.NewString(), QueryId: uuid.NewString()}
			rpc := peerRPCSuccess(uuid.NewString(), []byte(`{}`))
			session.sendMu.Lock()
			envelope, sealErr := sealPeerRPCLocked(3, session, query, "PEER_COMPLETE", rpc)
			session.sendMu.Unlock()
			if sealErr != nil {
				errorsChannel <- sealErr
				return
			}
			envelopes <- envelope
		}()
	}
	group.Wait()
	close(envelopes)
	close(errorsChannel)
	for sealErr := range errorsChannel {
		t.Fatal(sealErr)
	}
	frames := make([]*remotev1.PeerCiphertext, 0, count)
	for envelope := range envelopes {
		frames = append(frames, envelope.GetPeerComplete())
	}
	sort.Slice(frames, func(left, right int) bool {
		return frames[left].GetMessageSequence() < frames[right].GetMessageSequence()
	})
	if len(frames) != count {
		t.Fatalf("frames=%d", len(frames))
	}
	for index, frame := range frames {
		if frame.GetMessageSequence() != uint64(index+1) {
			t.Fatalf("sequence[%d]=%d", index, frame.GetMessageSequence())
		}
		plaintext, openErr := opener.OpenNext(frame.GetCiphertext(), peerprotocol.CiphertextMetadata{
			FrameType: "PEER_COMPLETE", SessionID: frame.GetSessionId(), QueryID: frame.GetQueryId(), Generation: generation,
			MessageSequence: frame.GetMessageSequence(), Direction: peerprotocol.DirectionTargetToSource,
		})
		if openErr != nil {
			t.Fatal(openErr)
		}
		decoded := new(remotev1.RpcEnvelope)
		if proto.Unmarshal(plaintext, decoded) != nil || decoded.GetResponse() == nil {
			t.Fatal("sealed response did not round-trip")
		}
	}
}

func recordPeerRPC(destination chan<- recordedPeerRPC) peerRPCSend {
	return func(query *remotev1.PeerCiphertext, response *remotev1.RpcEnvelope, events []*remotev1.RpcEnvelope) error {
		destination <- recordedPeerRPC{
			query: proto.Clone(query).(*remotev1.PeerCiphertext), response: cloneRPCEnvelope(response), events: cloneRPCEnvelopes(events),
		}
		return nil
	}
}

func receivePeerRPC(t *testing.T, source <-chan recordedPeerRPC) recordedPeerRPC {
	t.Helper()
	select {
	case value := <-source:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Peer RPC response")
		return recordedPeerRPC{}
	}
}

func peerRPCRequest(requestID, key, method string, payload []byte, deadline time.Time) *remotev1.RpcEnvelope {
	return &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
			Header: &remotev1.RpcRequestHeader{RequestId: requestID, IdempotencyKey: key, Deadline: timestamppb.New(deadline)},
			Method: method, JsonPayload: payload,
		}},
	}
}

func peerRPCQuery(queryID string, deadline time.Time) *remotev1.PeerCiphertext {
	return &remotev1.PeerCiphertext{SessionId: uuid.NewString(), QueryId: queryID, Deadline: timestamppb.New(deadline)}
}

func peerRPCSuccess(requestID string, payload []byte) *remotev1.RpcEnvelope {
	return &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Response{Response: &remotev1.RpcResponse{
			Header: &remotev1.RpcResponseHeader{RequestId: requestID, Revision: 1}, JsonPayload: payload,
		}},
	}
}

func peerRPCEvent(requestID, eventID string) *remotev1.RpcEnvelope {
	return &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Event{Event: &remotev1.RpcEvent{
			EventId: eventID, RequestId: requestID, Kind: remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_DELTA, Sequence: 1,
		}},
	}
}

func encryptedPeerRPC(t *testing.T, sealer *peerprotocol.CipherState, frameType, sessionID, queryID string, deadline time.Time, rpc *remotev1.RpcEnvelope) *remotev1.PeerCiphertext {
	t.Helper()
	plaintext, err := proto.Marshal(rpc)
	if err != nil {
		t.Fatal(err)
	}
	generation, sequence, exhausted := sealer.NextSequence()
	if exhausted {
		t.Fatal("source cipher exhausted")
	}
	metadata := peerprotocol.CiphertextMetadata{
		FrameType: frameType, SessionID: sessionID, QueryID: queryID, Generation: generation,
		MessageSequence: sequence, Deadline: deadline, Direction: peerprotocol.DirectionSourceToTarget,
	}
	ciphertext, err := sealer.SealNext(plaintext, metadata)
	if err != nil {
		t.Fatal(err)
	}
	frame := &remotev1.PeerCiphertext{
		SessionId: sessionID, QueryId: queryID, Generation: generation, MessageSequence: sequence, Ciphertext: ciphertext,
	}
	if !deadline.IsZero() {
		frame.Deadline = timestamppb.New(deadline)
	}
	return frame
}

func TestPeerRPCExecutorCloseCancelsAll(t *testing.T) {
	started := make(chan struct{})
	dispatch := func(ctx context.Context, request *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
		close(started)
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("context error = %v", ctx.Err())
		}
		return peerRPCError(request.GetRequest().GetHeader().GetRequestId(), remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED, "request cancelled", true, 0), nil
	}
	executor := newPeerRPCExecutor(context.Background(), time.Now().Add(time.Hour), dispatch, func(*remotev1.PeerCiphertext, *remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) error {
		t.Error("closed executor sent a response")
		return nil
	}, nil)
	request := peerRPCRequest(uuid.NewString(), "close-all-key", "project.list", []byte(`{}`), time.Now().Add(time.Minute))
	if err := executor.submit(peerRPCQuery(uuid.NewString(), time.Now().Add(time.Minute)), request); err != nil {
		t.Fatal(err)
	}
	<-started
	executor.close()
}
