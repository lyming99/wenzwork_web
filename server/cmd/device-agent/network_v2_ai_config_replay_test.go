package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	peerv2 "github.com/wenzwork/wenzwork-web/server/internal/peerprotocol/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestV2AIConfigFirstCreateReplaysAfterLostResponse(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	directory := t.TempDir()
	state, err := loadOrCreateAgentState(
		filepath.Join(directory, "state.json"),
		filepath.Join(directory, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })

	linkID := uuid.NewString()
	controllerID := uuid.NewString()
	root := randomV2E2EBytes(t, peerv2.RootKeySize)
	keys, err := peerv2.NewLinkState(linkID, root)
	if err != nil {
		t.Fatal(err)
	}
	linkContext, closeLink := context.WithCancel(context.Background())
	registry := newV2AgentLinkRegistry(state)
	link := &v2AgentLink{
		registry: registry,
		state:    state,
		id:       linkID,
		binding: peerv2.HandshakeBinding{
			ClientID: controllerID,
		},
		keys:          keys,
		sequencer:     peerv2.NewSequencer(128),
		context:       linkContext,
		cancel:        closeLink,
		allowedScopes: map[string]struct{}{"remote.peer.ai.config": {}},
		active:        true,
		channels:      make(map[string]*v2AgentChannel),
		operations:    make(map[string]v2AgentOperation),
		files:         make(map[string]*v2AgentFileTransfer),
		downloads:     make(map[string]*v2AgentDownloadTransfer),
		events:        make(map[string]*v2AgentEventSubscription),
	}
	registry.links[linkID] = link
	t.Cleanup(registry.close)

	verifier := remoteauth.DeviceLinkGrantVerifier{
		Issuer: "ai-config-replay-test",
		Keys: map[string]ed25519.PublicKey{
			"unused": make(ed25519.PublicKey, ed25519.PublicKeySize),
		},
	}
	linkSequencer := peerv2.NewSequencer(128)

	firstCarrier := newV2AIConfigReplayCarrier(t, ctx, registry, verifier, linkID, 1)
	firstChannelID := uuid.NewString()
	firstCarrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, firstChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: firstChannelID,
		Kind:      remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY,
		Scopes:    []string{"remote.peer.ai.config"},
	})
	firstCarrier.controller.expectChannelAccept(root, linkSequencer, firstChannelID)

	payload := []byte(`{"id":"first-replay","expectedRevision":0,"name":"First replay","provider":"openai","model":"gpt-test","enabled":true,"secretAction":"replace","secret":"private-test-marker"}`)
	operationID := uuid.NewString()
	firstAttemptID := uuid.NewString()
	firstStreamID := uuid.NewString()
	request := &remotev2.RpcRequest{
		OperationId: operationID,
		AttemptId:   firstAttemptID,
		Method:      "ai.config.update",
		Deadline:    timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload:     append([]byte(nil), payload...),
	}
	digest := v2RPCOperationDigest(controllerID, "", "remote.peer.ai.config", request)
	// Seed a prepared row through a fresh store descriptor. This models an Agent
	// restart after durable admission but before the handler crossed its first
	// SecretStore side-effect boundary.
	restartedStore := &businessStore{path: state.business.path, deviceID: state.business.deviceID}
	preparedMutation := v2OperationMutationContext{
		OperationID: operationID, Digest: digest, Controller: controllerID,
		Method: request.GetMethod(), Now: time.Now().UTC(),
	}
	if sideEffectState, prepareErr := restartedStore.prepareV2SideEffect(ctx, preparedMutation); prepareErr != nil || sideEffectState != v2SideEffectPrepared {
		t.Fatalf("seed prepared side effect state=%q error=%v", sideEffectState, prepareErr)
	}
	firstCarrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, firstChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId:   firstChannelID,
		StreamId:    firstStreamID,
		Kind:        remotev2.StreamKind_STREAM_KIND_RPC,
		OperationId: operationID,
	})
	firstCarrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, firstChannelID, firstStreamID, request)

	storedResponse := waitForV2AIConfigOperation(t, ctx, state, operationID, digest)
	if storedResponse.GetRevision() != 1 || storedResponse.GetReplayed() {
		t.Fatalf("first committed response revision=%d replayed=%v", storedResponse.GetRevision(), storedResponse.GetReplayed())
	}
	if found, claimErr := state.business.loadV2OperationClaim(ctx, operationID, digest, time.Now().UTC()); claimErr != nil || found {
		t.Fatalf("completed operation retained claim=%v error=%v", found, claimErr)
	}
	// The first response is intentionally never read. Closing this Carrier after
	// the mutation and operation journal are durable models a response lost in
	// transit while preserving the end-to-end Link for recovery.
	firstCarrier.close()
	link.mu.Lock()
	delete(link.operations, operationID)
	link.mu.Unlock()

	secondCarrier := newV2AIConfigReplayCarrier(t, ctx, registry, verifier, linkID, 2)
	secondChannelID := uuid.NewString()
	secondCarrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, secondChannelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: secondChannelID,
		Kind:      remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY,
		Scopes:    []string{"remote.peer.ai.config"},
	})
	secondCarrier.controller.expectChannelAccept(root, linkSequencer, secondChannelID)

	secondAttemptID := uuid.NewString()
	secondStreamID := uuid.NewString()
	retry := &remotev2.RpcRequest{
		OperationId: operationID,
		AttemptId:   secondAttemptID,
		Method:      request.GetMethod(),
		Deadline:    timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload:     append([]byte(nil), payload...),
	}
	secondCarrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, secondChannelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId:   secondChannelID,
		StreamId:    secondStreamID,
		Kind:        remotev2.StreamKind_STREAM_KIND_RPC,
		OperationId: operationID,
	})
	secondCarrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, secondChannelID, secondStreamID, retry)
	replayed := secondCarrier.controller.expectRPCResponse(root, linkSequencer, secondStreamID, operationID)
	if replayed.GetAttemptId() != secondAttemptID || !replayed.GetReplayed() || replayed.GetRevision() != 1 ||
		replayed.GetErrorCode() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("replayed response = %#v", replayed)
	}
	var responsePayload struct {
		ID       string `json:"id"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal(replayed.GetPayload(), &responsePayload); err != nil || responsePayload.ID != "first-replay" || responsePayload.Revision != 1 {
		t.Fatalf("replayed payload = %s, %v", replayed.GetPayload(), err)
	}
	if strings.Contains(string(replayed.GetPayload()), "private-test-marker") {
		t.Fatal("replayed AI configuration exposed its credential")
	}

	configs, err := state.business.listAIConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.mu.RLock()
	current, exists := state.AIConfigs["first-replay"]
	state.mu.RUnlock()
	if len(configs) != 1 || configs[0].ID != "first-replay" || configs[0].Revision != 1 || !exists || current.Revision != 1 || !current.CredentialConfigured {
		t.Fatalf("AI configs = %+v; current exists=%v revision=%d", configs, exists, current.Revision)
	}
}

func TestV2AIConfigJournalFailureDoesNotRepeatCommittedMutation(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	directory := t.TempDir()
	state, err := loadOrCreateAgentState(
		filepath.Join(directory, "state.json"),
		filepath.Join(directory, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	var saveAttempts atomic.Int32
	state.business.operationJournalSaveHook = func() error {
		saveAttempts.Add(1)
		return errors.New("injected operation journal failure")
	}

	linkID := uuid.NewString()
	controllerID := uuid.NewString()
	root := randomV2E2EBytes(t, peerv2.RootKeySize)
	keys, err := peerv2.NewLinkState(linkID, root)
	if err != nil {
		t.Fatal(err)
	}
	linkContext, closeLink := context.WithCancel(context.Background())
	registry := newV2AgentLinkRegistry(state)
	link := &v2AgentLink{
		registry: registry,
		state:    state,
		id:       linkID,
		binding: peerv2.HandshakeBinding{
			ClientID: controllerID,
		},
		keys:          keys,
		sequencer:     peerv2.NewSequencer(128),
		context:       linkContext,
		cancel:        closeLink,
		allowedScopes: map[string]struct{}{"remote.peer.ai.config": {}},
		active:        true,
		channels:      make(map[string]*v2AgentChannel),
		operations:    make(map[string]v2AgentOperation),
		files:         make(map[string]*v2AgentFileTransfer),
		downloads:     make(map[string]*v2AgentDownloadTransfer),
		events:        make(map[string]*v2AgentEventSubscription),
	}
	registry.links[linkID] = link
	t.Cleanup(registry.close)

	verifier := remoteauth.DeviceLinkGrantVerifier{
		Issuer: "ai-config-journal-failure-test",
		Keys: map[string]ed25519.PublicKey{
			"unused": make(ed25519.PublicKey, ed25519.PublicKeySize),
		},
	}
	linkSequencer := peerv2.NewSequencer(128)
	carrier := newV2AIConfigReplayCarrier(t, ctx, registry, verifier, linkID, 1)
	channelID := uuid.NewString()
	carrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, channelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: channelID,
		Kind:      remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY,
		Scopes:    []string{"remote.peer.ai.config"},
	})
	carrier.controller.expectChannelAccept(root, linkSequencer, channelID)

	payload := []byte(`{"id":"journal-failure","expectedRevision":0,"name":"Journal failure","provider":"openai","model":"gpt-test","enabled":true,"secretAction":"replace","secret":"private-test-marker"}`)
	operationID := uuid.NewString()
	request := &remotev2.RpcRequest{
		OperationId: operationID,
		AttemptId:   uuid.NewString(),
		Method:      "ai.config.update",
		Deadline:    timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload:     append([]byte(nil), payload...),
	}
	send := func(request *remotev2.RpcRequest) *remotev2.RpcResponse {
		streamID := uuid.NewString()
		carrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, channelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
			ChannelId:   channelID,
			StreamId:    streamID,
			Kind:        remotev2.StreamKind_STREAM_KIND_RPC,
			OperationId: request.GetOperationId(),
		})
		carrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, channelID, streamID, request)
		return carrier.controller.expectRPCResponse(root, linkSequencer, streamID, operationID)
	}

	first := send(request)
	if first.GetSafeErrorCode() != "operation_commit_unknown" || !first.GetRetryable() || first.GetReplayed() {
		t.Fatalf("first response = %#v", first)
	}
	digest := v2RPCOperationDigest(controllerID, "", "remote.peer.ai.config", request)
	if _, found, loadErr := state.business.loadV2Operation(ctx, operationID, digest, time.Now().UTC()); loadErr != nil || found {
		t.Fatalf("completed journal found=%v error=%v", found, loadErr)
	}
	if found, claimErr := state.business.loadV2OperationClaim(ctx, operationID, digest, time.Now().UTC()); claimErr != nil || !found {
		t.Fatalf("committed claim found=%v error=%v", found, claimErr)
	}

	retry := proto.Clone(request).(*remotev2.RpcRequest)
	retry.AttemptId = uuid.NewString()
	second := send(retry)
	if second.GetSafeErrorCode() != "operation_commit_unknown" || !second.GetRetryable() || !second.GetReplayed() ||
		second.GetAttemptId() != retry.GetAttemptId() {
		t.Fatalf("retry response = %#v", second)
	}
	if attempts := saveAttempts.Load(); attempts != 1 {
		t.Fatalf("journal save attempts = %d, want 1", attempts)
	}
	configs, err := state.business.listAIConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 || configs[0].ID != "journal-failure" || configs[0].Revision != 1 {
		t.Fatalf("AI configs after retry = %+v", configs)
	}
}

func TestV2StartedSideEffectReturnsUnknownWithoutDispatch(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	directory := t.TempDir()
	state, err := loadOrCreateAgentState(
		filepath.Join(directory, "state.json"),
		filepath.Join(directory, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	var saveAttempts atomic.Int32
	state.business.operationJournalSaveHook = func() error {
		saveAttempts.Add(1)
		return nil
	}

	linkID := uuid.NewString()
	controllerID := uuid.NewString()
	root := randomV2E2EBytes(t, peerv2.RootKeySize)
	keys, err := peerv2.NewLinkState(linkID, root)
	if err != nil {
		t.Fatal(err)
	}
	linkContext, closeLink := context.WithCancel(context.Background())
	registry := newV2AgentLinkRegistry(state)
	link := &v2AgentLink{
		registry: registry,
		state:    state,
		id:       linkID,
		binding: peerv2.HandshakeBinding{
			ClientID: controllerID,
		},
		keys:          keys,
		sequencer:     peerv2.NewSequencer(128),
		context:       linkContext,
		cancel:        closeLink,
		allowedScopes: map[string]struct{}{"remote.peer.ai.config": {}},
		active:        true,
		channels:      make(map[string]*v2AgentChannel),
		operations:    make(map[string]v2AgentOperation),
		files:         make(map[string]*v2AgentFileTransfer),
		downloads:     make(map[string]*v2AgentDownloadTransfer),
		events:        make(map[string]*v2AgentEventSubscription),
	}
	registry.links[linkID] = link
	t.Cleanup(registry.close)

	verifier := remoteauth.DeviceLinkGrantVerifier{
		Issuer: "started-side-effect-test",
		Keys: map[string]ed25519.PublicKey{
			"unused": make(ed25519.PublicKey, ed25519.PublicKeySize),
		},
	}
	linkSequencer := peerv2.NewSequencer(128)
	carrier := newV2AIConfigReplayCarrier(t, ctx, registry, verifier, linkID, 1)
	channelID := uuid.NewString()
	carrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN, channelID, v2ChannelControlStreamID, &remotev2.ChannelOpen{
		ChannelId: channelID,
		Kind:      remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY,
		Scopes:    []string{"remote.peer.ai.config"},
	})
	carrier.controller.expectChannelAccept(root, linkSequencer, channelID)

	payload := []byte(`{"id":"must-not-run","expectedRevision":0,"name":"Must not run","provider":"openai","model":"gpt-test","enabled":true,"secretAction":"replace","secret":"private-test-marker"}`)
	operationID := uuid.NewString()
	request := &remotev2.RpcRequest{
		OperationId: operationID,
		AttemptId:   uuid.NewString(),
		Method:      "ai.config.update",
		Deadline:    timestamppb.New(time.Now().UTC().Add(time.Minute)),
		Payload:     append([]byte(nil), payload...),
	}
	digest := v2RPCOperationDigest(controllerID, "", "remote.peer.ai.config", request)
	mutation := v2OperationMutationContext{
		OperationID: operationID, Digest: digest, Controller: controllerID,
		Method: request.GetMethod(), Now: time.Now().UTC(),
	}
	if _, err := state.business.prepareV2SideEffect(ctx, mutation); err != nil {
		t.Fatal(err)
	}
	if err := state.business.transitionV2SideEffect(ctx, mutation, v2SideEffectPrepared, v2SideEffectStarted); err != nil {
		t.Fatal(err)
	}

	streamID := uuid.NewString()
	carrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_STREAM_OPEN, channelID, v2ChannelControlStreamID, &remotev2.StreamOpen{
		ChannelId: channelID, StreamId: streamID, Kind: remotev2.StreamKind_STREAM_KIND_RPC, OperationId: operationID,
	})
	carrier.controller.sendRecord(root, linkSequencer, remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, channelID, streamID, request)
	response := carrier.controller.expectRPCResponse(root, linkSequencer, streamID, operationID)
	if response.GetSafeErrorCode() != "operation_commit_unknown" || !response.GetRetryable() || !response.GetReplayed() {
		t.Fatalf("started side-effect response = %#v", response)
	}
	if attempts := saveAttempts.Load(); attempts != 0 {
		t.Fatalf("operation journal save attempts = %d, want 0", attempts)
	}
	configs, err := state.business.listAIConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range configs {
		if config.ID == "must-not-run" {
			t.Fatalf("started side effect dispatched handler: %+v", config)
		}
	}
}

func waitForV2AIConfigOperation(
	t *testing.T,
	ctx context.Context,
	state *agentState,
	operationID string,
	digest [32]byte,
) *remotev2.RpcResponse {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, found, err := state.business.loadV2Operation(ctx, operationID, digest, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if found {
			return response
		}
		select {
		case <-ctx.Done():
			t.Fatalf("operation %s was not journaled: %v", operationID, ctx.Err())
		case <-ticker.C:
		}
	}
}

type v2AIConfigReplayCarrier struct {
	t          *testing.T
	controller *v2E2EController
	client     *websocket.Conn
	carrier    *v2AgentCarrier
	cancel     context.CancelFunc
	served     chan error
	release    chan struct{}
	server     *httptest.Server
	closeOnce  sync.Once
}

func newV2AIConfigReplayCarrier(
	t *testing.T,
	ctx context.Context,
	registry *v2AgentLinkRegistry,
	verifier remoteauth.DeviceLinkGrantVerifier,
	linkID string,
	epoch uint64,
) *v2AIConfigReplayCarrier {
	t.Helper()
	type acceptResult struct {
		socket *websocket.Conn
		err    error
	}
	accepted := make(chan acceptResult, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		socket, err := websocket.Accept(writer, request, nil)
		accepted <- acceptResult{socket: socket, err: err}
		if err != nil {
			return
		}
		<-release
		socket.CloseNow()
	}))
	client, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		server.Close()
		if response != nil {
			t.Fatalf("dial v2 replay Carrier status=%d: %v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	result := <-accepted
	if result.err != nil {
		client.CloseNow()
		close(release)
		server.Close()
		t.Fatal(result.err)
	}
	carrierID := uuid.NewString()
	carrier, err := newV2AgentCarrier(result.socket, carrierID, epoch)
	if err != nil {
		client.CloseNow()
		close(release)
		server.Close()
		t.Fatal(err)
	}
	serveContext, cancel := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		served <- serveTargetV2(serveContext, carrier, time.Hour, registry, verifier)
	}()
	fixture := &v2AIConfigReplayCarrier{
		t: t,
		controller: &v2E2EController{
			t: t, ctx: ctx, socket: client, carrierID: carrierID, epoch: epoch, linkID: linkID, keyID: 1,
		},
		client: client, carrier: carrier, cancel: cancel, served: served, release: release, server: server,
	}
	t.Cleanup(fixture.close)
	return fixture
}

func (fixture *v2AIConfigReplayCarrier) close() {
	if fixture == nil {
		return
	}
	fixture.closeOnce.Do(func() {
		fixture.client.CloseNow()
		fixture.cancel()
		fixture.carrier.close()
		close(fixture.release)
		select {
		case <-fixture.served:
		case <-time.After(2 * time.Second):
			fixture.t.Error("v2 replay Carrier did not stop")
		}
		fixture.server.Close()
	})
}
