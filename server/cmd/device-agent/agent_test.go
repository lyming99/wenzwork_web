package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/peerprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

type staticAIProvider struct{}

func (staticAIProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return 7 * time.Millisecond, nil
}

func (staticAIProvider) Complete(_ context.Context, _ aiConfig, _ []chatMessage, prompt string) (string, error) {
	return "answer: " + prompt, nil
}

type blockingAIProvider struct {
	started chan struct{}
}

func (provider blockingAIProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return 0, nil
}

func (provider blockingAIProvider) Complete(ctx context.Context, _ aiConfig, _ []chatMessage, _ string) (string, error) {
	close(provider.started)
	<-ctx.Done()
	return "", ctx.Err()
}

type releasableAIProvider struct {
	started chan struct{}
	release <-chan struct{}
}

func (provider releasableAIProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return 0, nil
}

func (provider releasableAIProvider) Complete(ctx context.Context, _ aiConfig, _ []chatMessage, _ string) (string, error) {
	close(provider.started)
	select {
	case <-provider.release:
		return "durable answer", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestLoadAgentEnvironmentReadsExplicitFileWithoutOverridingProcessEnvironment(t *testing.T) {
	directory := t.TempDir()
	envFile := filepath.Join(directory, "agent.env")
	if err := os.WriteFile(envFile, []byte("WENZWORK_AGENT_TEST_FILE_VALUE=from-file\nWENZWORK_AGENT_TEST_INHERITED=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fileValue := "WENZWORK_AGENT_TEST_FILE_VALUE"
	inheritedValue := "WENZWORK_AGENT_TEST_INHERITED"
	previous, existed := os.LookupEnv(fileValue)
	if err := os.Unsetenv(fileValue); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(fileValue, previous)
		} else {
			_ = os.Unsetenv(fileValue)
		}
	})
	t.Setenv(inheritedValue, "from-process")

	if err := loadAgentEnvironment(envFile); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(fileValue); got != "from-file" {
		t.Fatalf("file environment = %q, want from-file", got)
	}
	if got := os.Getenv(inheritedValue); got != "from-process" {
		t.Fatalf("inherited environment = %q, want from-process", got)
	}
}

func TestValueOrEnvironmentPrefersCommandLineValue(t *testing.T) {
	t.Setenv("WENZWORK_DEVICE_WORKSPACE", "from-environment")
	if got := valueOrEnvironment(" from-command-line ", "WENZWORK_DEVICE_WORKSPACE"); got != "from-command-line" {
		t.Fatalf("command-line value = %q", got)
	}
	if got := valueOrEnvironment("", "WENZWORK_DEVICE_WORKSPACE"); got != "from-environment" {
		t.Fatalf("environment value = %q", got)
	}
}

func TestGenericRPCDispatcherPersistsSecretsLocallyAndRedactsResponses(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	dispatch := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.config"}
	request, err := newCallEnvelope(uuid.NewString(), "ai.config.update", []byte(`{
		"id":"primary","expectedRevision":0,"name":"Primary","provider":"openai-compatible","baseUrl":"https://api.example.test/v1","model":"gpt","enabled":true,"secret":"secret-value"
	}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response := dispatch.dispatch(context.Background(), request).GetResponse()
	if response.GetError() != nil || bytes.Contains(response.GetJsonPayload(), []byte("secret-value")) ||
		!bytes.Contains(response.GetJsonPayload(), []byte(`"secretConfigured":true`)) {
		t.Fatalf("response = %+v payload=%s", response, response.GetJsonPayload())
	}
	stateContents, err := os.ReadFile(state.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateContents, []byte("secret-value")) || bytes.Contains(stateContents, []byte(`"credential"`)) {
		t.Fatal("identity state contains an AI credential")
	}
	reloaded, err := loadOrCreateAgentState(state.path, state.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AIConfigs["primary"].Credential != "secret-value" {
		t.Fatal("device-local credential was not persisted")
	}
}

func TestGenericRPCShapesPaginationAndTicketScopeAreStable(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	query := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.query"}
	config := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.config"}
	defaultConfig := dispatchJSON(t, config, "ai.config.get", `{}`)
	if defaultConfig["id"] != "default" || defaultConfig["secretConfigured"] != false || defaultConfig["revision"] != float64(0) {
		t.Fatalf("default config = %#v", defaultConfig)
	}
	if response := dispatchEnvelope(t, query, "ai.config.update", `{"id":"blocked"}`); response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN {
		t.Fatalf("ai.config.update query-scope error = %+v", response.GetError())
	}
	if response := dispatchEnvelope(t, query, "ai.config.get", `{}`); response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN {
		t.Fatalf("ai.config.get query-scope error = %+v", response.GetError())
	}
	for _, forbidden := range []string{"conversation.list", "file.write-text"} {
		response := dispatchEnvelope(t, query, forbidden, `{}`)
		if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN {
			t.Fatalf("%s error = %+v", forbidden, response.GetError())
		}
	}
	state.AIConfigs["default"] = aiConfig{
		ID: "default", Name: "Test", Provider: "openai-compatible", BaseURL: "https://api.example.test/v1",
		Model: "test-model", Enabled: true, Revision: state.Revision,
	}

	chat := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat", ai: staticAIProvider{}}
	created := dispatchJSON(t, chat, "conversation.create", `{"title":"First"}`)
	conversationID, ok := created["id"].(string)
	if !ok || uuid.Validate(conversationID) != nil || created["messageCount"] != float64(0) || created["state"] != "idle" {
		t.Fatalf("created conversation = %#v", created)
	}
	accepted := dispatchJSON(t, chat, "conversation.send", `{"conversationId":"`+conversationID+`","content":"hello"}`)
	if accepted["accepted"] != true || accepted["generationId"] == "" {
		t.Fatalf("send response = %#v", accepted)
	}
	messages := dispatchJSON(t, chat, "conversation.messages.before", `{"conversationId":"`+conversationID+`","limit":10}`)
	items, ok := messages["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("message page = %#v", messages)
	}
	message := items[0].(map[string]any)
	if message["content"] != "hello" || message["sequence"] != float64(1) || message["status"] != "complete" {
		t.Fatalf("message = %#v", message)
	}
	assistant := items[1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["sequence"] != float64(2) || assistant["status"] != "complete" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	streamRequest, err := newCallEnvelope(uuid.NewString(), "conversation.send", []byte(`{"conversationId":"`+conversationID+`","content":"again"}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	final, events := chat.dispatchStream(context.Background(), streamRequest)
	if final.GetResponse().GetError() != nil || len(events) != 3 {
		t.Fatalf("stream result = %+v events=%d", final, len(events))
	}
	var previousSequence uint64
	for index, event := range events {
		if event.GetEvent().GetRequestId() != streamRequest.GetRequest().GetHeader().GetRequestId() || event.GetEvent().GetSequence() <= previousSequence {
			t.Fatalf("stream event %d = %+v", index, event.GetEvent())
		}
		previousSequence = event.GetEvent().GetSequence()
	}
	var completedEvent aiConversationEvent
	if err := json.Unmarshal(events[len(events)-1].GetEvent().GetJsonPayload(), &completedEvent); err != nil || completedEvent.Kind != "chat.completed" {
		t.Fatalf("completed event = %+v, %v", completedEvent, err)
	}
	streamed, ok := completedEvent.Payload["message"].(map[string]any)
	if !ok || streamed["role"] != "assistant" || streamed["content"] != "answer: again" || streamed["status"] != "complete" {
		t.Fatalf("streamed assistant = %+v", streamed)
	}
	page := dispatchJSON(t, chat, "conversation.list", `{"limit":1}`)
	if _, ok := page["items"].([]any); !ok || page["highWatermark"] == nil || page["resetRequired"] != false {
		t.Fatalf("conversation page = %#v", page)
	}
	watermark := uint64(page["highWatermark"].(float64))
	unchanged := dispatchJSON(t, chat, "conversation.list", `{"afterRevision":`+strconv.FormatUint(watermark, 10)+`}`)
	if len(unchanged["items"].([]any)) != 0 || unchanged["nextCursor"] != nil || unchanged["resetRequired"] != false {
		t.Fatalf("unchanged conversation page = %#v", unchanged)
	}
	reset := dispatchJSON(t, chat, "conversation.list", `{"afterRevision":1}`)
	if reset["resetRequired"] != false || len(reset["changes"].([]any)) == 0 {
		t.Fatalf("incremental conversation page = %#v", reset)
	}
	currentMessages := dispatchJSON(t, chat, "conversation.messages.before", `{"conversationId":"`+conversationID+`"}`)
	messageWatermark := uint64(currentMessages["highWatermark"].(float64))
	unchangedMessages := dispatchJSON(t, chat, "conversation.messages.before", `{"conversationId":"`+conversationID+`","afterRevision":`+strconv.FormatUint(messageWatermark, 10)+`}`)
	if len(unchangedMessages["items"].([]any)) != 0 || unchangedMessages["nextCursor"] != nil || unchangedMessages["resetRequired"] != false {
		t.Fatalf("unchanged message page = %#v", unchangedMessages)
	}
}

func TestFreshAgentConversationCreateReportsMissingAIConfiguration(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	chat := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.chat"}

	required := dispatchEnvelope(t, chat, "conversation.create", `{"title":"First"}`).GetError()
	if required.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND || required.GetSafeMessage() != "AI_CONFIG_REQUIRED" || required.GetRetryable() {
		t.Fatalf("empty configuration create error = %+v", required)
	}

	missing := dispatchEnvelope(t, chat, "conversation.create", `{"title":"First","configId":"removed","model":"removed-model"}`).GetError()
	if missing.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND || missing.GetSafeMessage() != "AI_CONFIG_NOT_FOUND" || missing.GetRetryable() {
		t.Fatalf("missing configuration create error = %+v", missing)
	}

	page, err := state.business.listAIConversations(t.Context(), aiConversationListOptions{
		ProjectID: stableProjectID(state.DeviceID, ""),
		Limit:     30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("failed creates persisted %d conversations", len(page.Items))
	}
}

func TestConversationSendPersistsStoppedTurnAfterCancellation(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state.AIConfigs["default"] = aiConfig{ID: "default", Name: "Test", Provider: "openai-compatible", BaseURL: "https://api.example.test/v1", Model: "test-model", Enabled: true, Revision: state.Revision}
	provider := blockingAIProvider{started: make(chan struct{})}
	dispatch := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat", ai: provider}
	created := dispatchJSON(t, dispatch, "conversation.create", `{"title":"Cancellation"}`)
	conversationID := created["id"].(string)
	request, err := newCallEnvelope(uuid.NewString(), "conversation.send", []byte(`{"conversationId":"`+conversationID+`","content":"keep this prompt"}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := make(chan *remotev1.RpcEnvelope, 1)
	go func() { response <- dispatch.dispatch(ctx, request) }()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	cancelled := dispatchJSON(t, dispatch, "conversation.cancel", `{"conversationId":"`+conversationID+`"}`)
	if cancelled["cancelled"] != true {
		t.Fatalf("cancel response = %#v", cancelled)
	}
	cancel()
	if result := <-response; result.GetResponse().GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED {
		t.Fatalf("send cancellation response = %+v", result.GetResponse().GetError())
	}
	stored := dispatchJSON(t, dispatch, "conversation.get", `{"conversationId":"`+conversationID+`"}`)
	if stored["conversation"].(map[string]any)["state"] != "idle" {
		t.Fatalf("conversation state = %#v", stored)
	}
	messages := stored["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["content"] != "keep this prompt" || messages[1].(map[string]any)["status"] != "stopped" {
		t.Fatalf("persisted messages = %#v", messages)
	}
}

func TestConversationSendSurvivesPeerContextAndLiveDeliveryLoss(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state.AIConfigs["default"] = aiConfig{
		ID: "default", Name: "Test", Provider: "openai-compatible", BaseURL: "https://api.example.test/v1",
		Model: "test-model", Enabled: true, Revision: state.Revision,
	}
	started, release := make(chan struct{}), make(chan struct{})
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat",
		ai: releasableAIProvider{started: started, release: release},
	}
	created := dispatchJSON(t, dispatch, "conversation.create", `{"title":"Offline generation"}`)
	conversationID := created["id"].(string)
	request, err := newCallEnvelope(uuid.NewString(), "conversation.send", []byte(`{"conversationId":"`+conversationID+`","content":"finish on device"}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	requestContext, disconnect := context.WithCancel(context.Background())
	requestContext, _ = withPeerRPCExplicitCancellation(requestContext)
	response := make(chan *remotev1.RpcEnvelope, 1)
	go func() {
		response <- dispatch.dispatchLive(requestContext, request, func(*remotev1.RpcEnvelope) error {
			return context.Canceled
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	disconnect()
	close(release)
	select {
	case result := <-response:
		if rpcErr := result.GetResponse().GetError(); rpcErr != nil {
			t.Fatalf("detached generation response error = %+v", rpcErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("detached generation did not finish")
	}

	stored := dispatchJSON(t, dispatch, "conversation.get", `{"conversationId":"`+conversationID+`"}`)
	messages := stored["messages"].([]any)
	if stored["conversation"].(map[string]any)["state"] != "idle" || len(messages) != 2 ||
		messages[1].(map[string]any)["status"] != "complete" || messages[1].(map[string]any)["content"] != "durable answer" {
		t.Fatalf("persisted detached generation = %#v", stored)
	}
}

func TestConversationMessagePaginationStartsWithNewestWindow(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state.AIConfigs["default"] = aiConfig{ID: "default", Name: "Test", Provider: "openai-compatible", BaseURL: "https://api.example.test/v1", Model: "test-model", Enabled: true, Revision: 1}
	dispatch := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat", ai: staticAIProvider{}}
	created := dispatchJSON(t, dispatch, "conversation.create", `{"title":"Paged"}`)
	conversationID := created["id"].(string)
	for index := 0; index < 3; index++ {
		dispatchJSON(t, dispatch, "conversation.send", `{"conversationId":"`+conversationID+`","content":"`+strconv.Itoa(index+1)+`"}`)
	}
	first := dispatchJSON(t, dispatch, "conversation.messages.before", `{"conversationId":"`+conversationID+`","limit":2}`)
	firstItems := first["items"].([]any)
	if firstItems[0].(map[string]any)["sequence"] != float64(5) || firstItems[1].(map[string]any)["sequence"] != float64(6) || first["nextCursor"] == nil {
		t.Fatalf("first newest page = %#v", first)
	}
	second := dispatchJSON(t, dispatch, "conversation.messages.before", `{"conversationId":"`+conversationID+`","limit":2,"cursor":"`+first["nextCursor"].(string)+`"}`)
	secondItems := second["items"].([]any)
	if secondItems[0].(map[string]any)["sequence"] != float64(3) || secondItems[1].(map[string]any)["sequence"] != float64(4) || second["nextCursor"] == nil {
		t.Fatalf("second older page = %#v", second)
	}
}

func dispatchEnvelope(t *testing.T, dispatch dispatcher, method, input string) *remotev1.RpcResponse {
	t.Helper()
	envelope, err := newCallEnvelope(uuid.NewString(), method, []byte(input), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return dispatch.dispatch(context.Background(), envelope).GetResponse()
}

func dispatchJSON(t *testing.T, dispatch dispatcher, method, input string) map[string]any {
	t.Helper()
	response := dispatchEnvelope(t, dispatch, method, input)
	if response.GetError() != nil {
		t.Fatalf("%s error = %+v", method, response.GetError())
	}
	var result map[string]any
	if err := json.Unmarshal(response.GetJsonPayload(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTargetAcceptsControllerProofAndDerivesMatchingDirectionalKeys(t *testing.T) {
	targetPublic, targetPrivate, _ := ed25519.GenerateKey(rand.Reader)
	sourcePublic, sourcePrivate, _ := ed25519.GenerateKey(rand.Reader)
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	state := &agentState{DeviceID: uuid.New(), KeyVersion: 5, identity: targetPrivate}
	sourceID, sessionID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	claims := remoteauth.Claims{
		Audience: "relay-peer", Subject: sourceID, UserID: uuid.NewString(), SessionID: sessionID,
		SourceDeviceID: sourceID, TargetDeviceID: state.DeviceID.String(), SourceCredentialType: "controller",
		SourceGrantVersion: 2, TargetGrantVersion: 7, SourceKeyVersion: 3, TargetKeyVersion: state.KeyVersion,
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourcePublic), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetPublic),
		SourceKeyThumbprint: remoteauth.PublicKeyThumbprint(sourcePublic), TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic),
		Confirmation: remoteauth.PublicKeyThumbprint(sourcePublic), Scopes: []string{"remote.peer.query"},
		MaxDurationSeconds: uint32(maximumTargetPeerSessionDuration / time.Second), MaxBytes: 1 << 20, JWTID: uuid.NewString(), IssuedAt: now.Unix(),
		NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(maximumTargetPeerSessionDuration).Unix(),
	}
	ticket, err := (remoteauth.Issuer{Issuer: "control", KeyID: "key", PrivateKey: signerPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	sourceEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	openProof := remoteauth.PeerOpenIdentityProof{
		TicketJWTID: claims.JWTID, SessionID: sessionID, SourceDeviceID: sourceID,
		TargetDeviceID: state.DeviceID.String(), EphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(),
	}
	openSignature, err := remoteauth.SignPeerOpenIdentity(sourcePrivate, openProof)
	if err != nil {
		t.Fatal(err)
	}
	session, ready, err := acceptPeerOpen(&remotev1.PeerOpen{
		SessionTicket: ticket, SessionId: sessionID, EphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(), IdentitySignature: openSignature,
	}, state, remoteauth.Verifier{Issuer: "control", Keys: map[string]ed25519.PublicKey{"key": signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteauth.VerifyPeerReadyIdentity(targetPublic, claims.TargetKeyThumbprint, remoteauth.PeerReadyIdentityProof{
		TicketJWTID: claims.JWTID, SessionID: sessionID, SourceDeviceID: sourceID, TargetDeviceID: state.DeviceID.String(),
		SourceEphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(), TargetEphemeralPublicKey: ready.GetEphemeralPublicKey(),
	}, ready.GetIdentitySignature()); err != nil {
		t.Fatal(err)
	}
	secret, err := peerprotocol.X25519SharedSecret(sourceEphemeral, ready.GetEphemeralPublicKey())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := peerprotocol.DeriveSessionKeys(secret, claims.JWTID, sessionID, sourceID, state.DeviceID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keys.SourceToTarget, session.keys.SourceToTarget) || !bytes.Equal(keys.TargetToSource, session.keys.TargetToSource) {
		t.Fatal("directional session keys differ")
	}
	if !session.expiresAt.IsZero() {
		t.Fatalf("accepted Peer must not inherit ticket expiry: %v", session.expiresAt)
	}
}

func TestTargetEnforcesProjectBoundPeerTicketAtAgentBoundary(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := state.business.addProject(t.Context(), t.TempDir(), "Bound", "", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	sourcePublic, sourcePrivate, _ := ed25519.GenerateKey(rand.Reader)
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	sourceID := uuid.NewString()
	verifier := remoteauth.Verifier{Issuer: "control", Keys: map[string]ed25519.PublicKey{"key": signerPublic}}

	openSession := func(scope, projectID string) error {
		t.Helper()
		now := time.Now().UTC().Truncate(time.Second)
		sessionID := uuid.NewString()
		targetPublic := state.identity.Public().(ed25519.PublicKey)
		claims := remoteauth.Claims{
			Audience: "relay-peer", Subject: sourceID, UserID: uuid.NewString(), SessionID: sessionID,
			SourceDeviceID: sourceID, TargetDeviceID: state.DeviceID.String(), SourceCredentialType: "controller",
			SourceGrantVersion: 2, TargetGrantVersion: 7, SourceKeyVersion: 3, TargetKeyVersion: state.KeyVersion,
			SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourcePublic), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetPublic),
			SourceKeyThumbprint: remoteauth.PublicKeyThumbprint(sourcePublic), TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic),
			Confirmation: remoteauth.PublicKeyThumbprint(sourcePublic), Scopes: []string{scope}, ProjectID: projectID,
			MaxDurationSeconds: 60, MaxBytes: 1 << 20, JWTID: uuid.NewString(), IssuedAt: now.Unix(),
			NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
		}
		ticket, err := (remoteauth.Issuer{Issuer: "control", KeyID: "key", PrivateKey: signerPrivate}).Sign(claims)
		if err != nil {
			return err
		}
		ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		signature, err := remoteauth.SignPeerOpenIdentity(sourcePrivate, remoteauth.PeerOpenIdentityProof{
			TicketJWTID: claims.JWTID, SessionID: sessionID, SourceDeviceID: sourceID,
			TargetDeviceID: state.DeviceID.String(), EphemeralPublicKey: ephemeral.PublicKey().Bytes(),
		})
		if err != nil {
			return err
		}
		_, _, err = acceptPeerOpen(&remotev1.PeerOpen{
			SessionTicket: ticket, SessionId: sessionID, EphemeralPublicKey: ephemeral.PublicKey().Bytes(), IdentitySignature: signature,
		}, state, verifier)
		return err
	}

	if err := openSession("remote.peer.file.send", project.ID.String()); err != nil {
		t.Fatalf("valid project-bound ticket was rejected: %v", err)
	}
	for name, testCase := range map[string][2]string{
		"missing project":      {"remote.peer.file.send", ""},
		"unknown project":      {"remote.peer.file.send", uuid.NewString()},
		"unexpected project":   {"remote.peer.query", project.ID.String()},
		"noncanonical project": {"remote.peer.file.send", strings.ToUpper(project.ID.String())},
	} {
		t.Run(name, func(t *testing.T) {
			if err := openSession(testCase[0], testCase[1]); err == nil {
				t.Fatal("invalid project-bound ticket was accepted")
			}
		})
	}
}

func TestPeerTicketTrustBundleIsBoundedCanonicalAndSupportsRotation(t *testing.T) {
	first, _, _ := ed25519.GenerateKey(rand.Reader)
	second, _, _ := ed25519.GenerateKey(rand.Reader)
	valid := peerTicketTrustBundle{Issuer: "control", Keys: []peerTicketTrustKey{
		{KeyID: "peer-2026-a", Algorithm: "Ed25519", PublicKey: base64.RawURLEncoding.EncodeToString(first)},
		{KeyID: "peer-2026-b", Algorithm: "Ed25519", PublicKey: base64.RawURLEncoding.EncodeToString(second)},
	}}
	verifier, err := verifierFromTrustBundle(valid)
	if err != nil || len(verifier.Keys) != 2 {
		t.Fatalf("rotation trust bundle = %+v, %v", verifier, err)
	}
	invalid := []peerTicketTrustBundle{
		{},
		{Issuer: " control", Keys: valid.Keys[:1]},
		{Issuer: "control", Keys: []peerTicketTrustKey{{KeyID: "peer key", Algorithm: "Ed25519", PublicKey: valid.Keys[0].PublicKey}}},
		{Issuer: "control", Keys: []peerTicketTrustKey{{KeyID: "peer", Algorithm: "ed25519", PublicKey: valid.Keys[0].PublicKey}}},
		{Issuer: "control", Keys: []peerTicketTrustKey{valid.Keys[0], valid.Keys[0]}},
		{Issuer: "control", Keys: []peerTicketTrustKey{{KeyID: "peer", Algorithm: "Ed25519", PublicKey: valid.Keys[0].PublicKey + "="}}},
	}
	for index, bundle := range invalid {
		if _, err := verifierFromTrustBundle(bundle); err == nil {
			t.Fatalf("invalid trust bundle %d was accepted", index)
		}
	}
}

func TestTargetRejectsUntrustedUnknownIssuerAndExpiredPeerTickets(t *testing.T) {
	targetPublic, targetPrivate, _ := ed25519.GenerateKey(rand.Reader)
	sourcePublic, sourcePrivate, _ := ed25519.GenerateKey(rand.Reader)
	trustedPublic, trustedPrivate, _ := ed25519.GenerateKey(rand.Reader)
	_, attackerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	state := &agentState{DeviceID: uuid.New(), KeyVersion: 1, identity: targetPrivate}
	sourceID, sessionID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	baseClaims := remoteauth.Claims{
		Audience: "relay-peer", Subject: sourceID, UserID: uuid.NewString(), SessionID: sessionID,
		SourceDeviceID: sourceID, TargetDeviceID: state.DeviceID.String(), SourceCredentialType: "controller",
		SourceGrantVersion: 1, TargetGrantVersion: 1, SourceKeyVersion: 1, TargetKeyVersion: 1,
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourcePublic), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetPublic),
		SourceKeyThumbprint: remoteauth.PublicKeyThumbprint(sourcePublic), TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic),
		Confirmation: remoteauth.PublicKeyThumbprint(sourcePublic), Scopes: []string{"remote.peer.query"},
		MaxDurationSeconds: 60, MaxBytes: 1 << 20, JWTID: uuid.NewString(), IssuedAt: now.Unix(),
		NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	verifier := remoteauth.Verifier{Issuer: "control", Keys: map[string]ed25519.PublicKey{"trusted": trustedPublic}}
	for name, issue := range map[string]func(remoteauth.Claims) (string, remoteauth.Claims){
		"forged signature": func(claims remoteauth.Claims) (string, remoteauth.Claims) {
			ticket, _ := (remoteauth.Issuer{Issuer: "control", KeyID: "trusted", PrivateKey: attackerPrivate}).Sign(claims)
			return ticket, claims
		},
		"unknown kid": func(claims remoteauth.Claims) (string, remoteauth.Claims) {
			ticket, _ := (remoteauth.Issuer{Issuer: "control", KeyID: "unknown", PrivateKey: trustedPrivate}).Sign(claims)
			return ticket, claims
		},
		"wrong issuer": func(claims remoteauth.Claims) (string, remoteauth.Claims) {
			claims.Issuer = "other-control"
			ticket, _ := (remoteauth.Issuer{Issuer: "other-control", KeyID: "trusted", PrivateKey: trustedPrivate}).Sign(claims)
			return ticket, claims
		},
		"expired": func(claims remoteauth.Claims) (string, remoteauth.Claims) {
			claims.IssuedAt, claims.NotBefore, claims.ExpiresAt = now.Add(-2*time.Minute).Unix(), now.Add(-2*time.Minute).Unix(), now.Add(-time.Minute).Unix()
			ticket, _ := (remoteauth.Issuer{Issuer: "control", KeyID: "trusted", PrivateKey: trustedPrivate}).Sign(claims)
			return ticket, claims
		},
	} {
		t.Run(name, func(t *testing.T) {
			ticket, claims := issue(baseClaims)
			ephemeral, _ := ecdh.X25519().GenerateKey(rand.Reader)
			signature, _ := remoteauth.SignPeerOpenIdentity(sourcePrivate, remoteauth.PeerOpenIdentityProof{
				TicketJWTID: claims.JWTID, SessionID: claims.SessionID, SourceDeviceID: claims.SourceDeviceID,
				TargetDeviceID: claims.TargetDeviceID, EphemeralPublicKey: ephemeral.PublicKey().Bytes(),
			})
			if _, _, err := acceptPeerOpen(&remotev1.PeerOpen{
				SessionTicket: ticket, SessionId: sessionID, EphemeralPublicKey: ephemeral.PublicKey().Bytes(), IdentitySignature: signature,
			}, state, verifier); err == nil {
				t.Fatal("untrusted Peer Ticket was accepted")
			}
		})
	}
}

func TestCallModeEmitsGenericRpcEnvelopeAndNeverEchoesDeviceKeyOnError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"call", "--method", "file.list", "--input", `{"path":"."}`}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(stdout.String()))
	if err != nil {
		t.Fatal(err)
	}
	envelope := new(remotev1.RpcEnvelope)
	if proto.Unmarshal(payload, envelope) != nil || envelope.GetRequest().GetMethod() != "file.list" {
		t.Fatal("call output is invalid")
	}
	secret := "device_" + strings.Repeat("A", 43)
	err = run([]string{"serve", "--access-key", secret}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(stderr.String(), secret) {
		t.Fatalf("serve error disclosed credential: %v / %s", err, stderr.String())
	}
}
