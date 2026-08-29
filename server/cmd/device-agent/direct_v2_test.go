package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

func TestParseDirectV2Config(t *testing.T) {
	for name, testCase := range map[string]struct {
		enabled, ip, port string
		want              directV2Config
		wantError         bool
	}{
		"disabled ignores staged endpoint": {"false", "192.0.2.10", "9443", directV2Config{}, false},
		"ipv4":                             {"true", "192.0.2.10", "9443", directV2Config{Enabled: true, IP: "192.0.2.10", Port: 9443}, false},
		"ipv4 mapped":                      {"true", "::ffff:192.0.2.10", "9443", directV2Config{Enabled: true, IP: "192.0.2.10", Port: 9443}, false},
		"unspecified":                      {"true", "0.0.0.0", "9443", directV2Config{}, true},
		"missing port":                     {"true", "192.0.2.10", "", directV2Config{}, true},
		"invalid switch":                   {"enabled", "192.0.2.10", "9443", directV2Config{}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parseDirectV2Config(testCase.enabled, testCase.ip, testCase.port, "", "")
			if (err != nil) != testCase.wantError || got != testCase.want {
				t.Fatalf("parseDirectV2Config() = %+v, %v; want %+v, error=%v", got, err, testCase.want, testCase.wantError)
			}
		})
	}
	if _, err := parseDirectV2Config("true", "192.0.2.10", "9443", "direct.crt", ""); err == nil {
		t.Fatal("incomplete direct TLS key pair was accepted")
	}
	fallback := "device_" + strings.Repeat("f", 43)
	configured := "device_" + strings.Repeat("d", 43)
	if key, err := selectDirectV2AccessKey(true, "", fallback); err != nil || key != fallback {
		t.Fatalf("direct Access Key fallback = %q, %v", key, err)
	}
	if key, err := selectDirectV2AccessKey(true, configured, fallback); err != nil || key != configured {
		t.Fatalf("dedicated direct Access Key = %q, %v", key, err)
	}
	if key, err := selectDirectV2AccessKey(false, "invalid", fallback); err != nil || key != "" {
		t.Fatalf("disabled direct Access Key = %q, %v", key, err)
	}
	if _, err := selectDirectV2AccessKey(true, "invalid", fallback); err == nil {
		t.Fatal("invalid direct Access Key was accepted")
	}
}

func TestDirectV2CarrierAuthenticationUsesBoundShortGrant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	_, deviceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	state := &agentState{DeviceID: uuid.New(), KeyVersion: 3, identity: deviceKey}
	runtime := &directV2Runtime{
		connectionEpoch: 17,
		nodeID:          uuid.NewSHA1(uuid.NameSpaceOID, []byte("wenzwork-direct-node:"+state.DeviceID.String())),
		cellID:          uuid.NewSHA1(uuid.NameSpaceOID, []byte("wenzwork-direct-cell:"+state.DeviceID.String())),
	}
	issuer := remoteauth.DeviceLinkGrantIssuer{Issuer: "control", KeyID: "direct-test", PrivateKey: signingKey}
	verifier := remoteauth.DeviceLinkGrantVerifier{
		Issuer: "control", Keys: map[string]ed25519.PublicKey{"direct-test": signingKey.Public().(ed25519.PublicKey)},
	}
	handler := newDirectV2Handler(state, runtime, verifier)
	handler.setEnabled(true)
	defer handler.close()
	baseClaims := remoteauth.DeviceLinkGrantClaims{
		Audience: remoteauth.DeviceLinkGrantAudience, GrantID: uuid.NewString(), ClientID: uuid.NewString(), DeviceID: state.DeviceID.String(),
		RelayNodeID: runtime.nodeID.String(), RelayCellID: runtime.cellID.String(), TargetConnectionEpoch: runtime.connectionEpoch,
		ClientIdentityKey:   base64.RawURLEncoding.EncodeToString(clientKey.Public().(ed25519.PublicKey)),
		ClientKeyThumbprint: remoteauth.PublicKeyThumbprint(clientKey.Public().(ed25519.PublicKey)), ClientIdentityKeyVersion: 2,
		DeviceKeyThumbprint: remoteauth.PublicKeyThumbprint(deviceKey.Public().(ed25519.PublicKey)), DeviceIdentityKeyVersion: state.KeyVersion,
		ClientGrantVersion: 1, DeviceGrantVersion: 1, AllowedScopes: []string{"remote.peer.query"},
		MaximumLifetimeSeconds: 300, IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}
	makeHello := func(claims remoteauth.DeviceLinkGrantClaims) (*remotev2.CarrierEnvelope, *remotev2.CarrierHello) {
		grant, signErr := issuer.Sign(claims)
		if signErr != nil {
			t.Fatal(signErr)
		}
		carrierID, carrierEpoch := uuid.NewString(), uint64(9)
		challenge := make([]byte, 32)
		if _, randomErr := rand.Read(challenge); randomErr != nil {
			t.Fatal(randomErr)
		}
		proof, proofErr := remoteauth.SignCarrierProof(clientKey, remoteauth.CarrierProof{
			GrantID: claims.GrantID, CarrierID: carrierID, CarrierEpoch: carrierEpoch, Challenge: challenge,
		})
		if proofErr != nil {
			t.Fatal(proofErr)
		}
		hello := &remotev2.CarrierHello{
			Grant: grant, GrantId: claims.GrantID, ClientId: claims.ClientID, ClientIdentityKeyVersion: claims.ClientIdentityKeyVersion,
			ClientChallenge: challenge, ClientProof: proof,
		}
		return &remotev2.CarrierEnvelope{ProtocolMajor: 2, CarrierId: carrierID, CarrierEpoch: carrierEpoch, PacketSequence: 1,
			Body: &remotev2.CarrierEnvelope_Hello{Hello: hello}}, hello
	}

	envelope, hello := makeHello(baseClaims)
	claims, err := handler.authenticate(envelope, hello, now)
	if err != nil || claims.GrantID != baseClaims.GrantID {
		t.Fatalf("direct authentication = %+v, %v", claims, err)
	}

	server := httptest.NewServer(handler)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	socket, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{Subprotocols: []string{directV2Subprotocol}})
	if err != nil {
		cancel()
		server.Close()
		t.Fatal(err)
	}
	envelope, _ = makeHello(baseClaims)
	payload, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := socket.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	ready, err := readV2AgentEnvelope(ctx, socket)
	if err != nil || ready.GetReady() == nil || ready.GetReady().GetCarrierId() != envelope.GetCarrierId() ||
		ready.GetReady().GetCarrierEpoch() != envelope.GetCarrierEpoch() {
		t.Fatalf("direct Carrier ready = %+v, %v", ready, err)
	}
	_ = socket.Close(websocket.StatusNormalClosure, "test complete")
	cancel()
	server.Close()

	wrongRoute := baseClaims
	wrongRoute.GrantID = uuid.NewString()
	wrongRoute.RelayNodeID = uuid.NewString()
	envelope, hello = makeHello(wrongRoute)
	if _, err := handler.authenticate(envelope, hello, now); err == nil {
		t.Fatal("Relay-bound Grant was accepted by direct listener")
	}

	persistent := baseClaims
	persistent.GrantID = uuid.NewString()
	persistent.MaximumLifetimeSeconds = 0
	persistent.ExpiresAt = remoteauth.PersistentDeviceLinkGrantExpiresAtUnix
	envelope, hello = makeHello(persistent)
	if _, err := handler.authenticate(envelope, hello, now); err == nil {
		t.Fatal("persistent Grant was accepted by direct listener")
	}
}

func TestDirectV2LocalAccessKeyIssuesBoundGrantAndOpensCarrier(t *testing.T) {
	handler, clientKey, accessKey := newDirectV2LocalTestHandler(t)
	defer handler.close()
	controllerID := uuid.NewString()
	clientPublicKey := base64.RawURLEncoding.EncodeToString(clientKey.Public().(ed25519.PublicKey))
	proof := ed25519.Sign(clientKey, directV2ControllerProofTranscript(controllerID, clientPublicKey, 4))
	body, err := json.Marshal(directV2DeviceLinkRequest{
		ControllerID: controllerID, ClientIdentityAlgorithm: "Ed25519", ClientIdentityPublicKey: clientPublicKey,
		ClientIdentityKeyVersion: 4, Proof: base64.RawURLEncoding.EncodeToString(proof),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v2/direct/device-links", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" ||
		!strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("local device link response = %d %#v %s", response.Code, response.Header(), response.Body.String())
	}
	var issued directV2DeviceLinkResponse
	decoder := json.NewDecoder(response.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&issued); err != nil {
		t.Fatal(err)
	}
	if issued.Device.ID != handler.state.DeviceID.String() || issued.Device.Name != "Local workstation" || issued.Device.Platform != "linux" ||
		issued.Device.AgentVersion != "test" || issued.Device.IdentityAlgorithm != "Ed25519" || issued.Device.IdentityKeyVersion != handler.state.KeyVersion ||
		issued.DeviceLink.ConnectionMode != "direct" || issued.DeviceLink.ConnectionURL != "ws://192.0.2.80:9443/v2/connect" ||
		issued.DeviceLink.RelayURL != issued.DeviceLink.ConnectionURL || issued.DeviceLink.MaximumLifetimeSeconds != 300 ||
		issued.DeviceLink.TargetConnectionEpoch != handler.epoch || issued.DeviceLink.RelayNodeID != handler.nodeID.String() || issued.DeviceLink.RelayCellID != handler.cellID.String() {
		t.Fatalf("issued local device link = %+v", issued)
	}
	now := time.Now().UTC()
	claims, err := handler.local.verifier.Verify(issued.DeviceLink.Grant, now)
	if err != nil || claims.Persistent() || claims.ClientID != controllerID || claims.DeviceID != handler.state.DeviceID.String() ||
		claims.MaximumLifetimeSeconds != 300 || claims.ExpiresAt-claims.IssuedAt != 300 || claims.ClientIdentityKeyVersion != 4 ||
		claims.DeviceIdentityKeyVersion != handler.state.KeyVersion || claims.RelayNodeID != handler.nodeID.String() || claims.RelayCellID != handler.cellID.String() ||
		claims.TargetConnectionEpoch != handler.epoch {
		t.Fatalf("local grant claims = %+v, %v", claims, err)
	}
	if _, err := handler.hostVerifier.Verify(issued.DeviceLink.Grant, now); err == nil {
		t.Fatal("Host verifier accepted an Agent-local Grant")
	}
	for _, scope := range claims.AllowedScopes {
		if scope == "remote.connect" || !strings.HasPrefix(scope, "remote.peer.") {
			t.Fatalf("local grant contains non-peer scope %q", scope)
		}
	}

	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/connect"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{Subprotocols: []string{directV2Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.CloseNow()
	carrierID, carrierEpoch := uuid.NewString(), uint64(12)
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		t.Fatal(err)
	}
	carrierProof, err := remoteauth.SignCarrierProof(clientKey, remoteauth.CarrierProof{
		GrantID: claims.GrantID, CarrierID: carrierID, CarrierEpoch: carrierEpoch, Challenge: challenge,
	})
	if err != nil {
		t.Fatal(err)
	}
	hello := &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: carrierID, CarrierEpoch: carrierEpoch, PacketSequence: 1,
		Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
			Grant: issued.DeviceLink.Grant, GrantId: claims.GrantID, ClientId: controllerID, ClientIdentityKeyVersion: 4,
			ClientChallenge: challenge, ClientProof: carrierProof,
		}},
	}
	payload, err := proto.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := socket.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	ready, err := readV2AgentEnvelope(ctx, socket)
	if err != nil || ready.GetReady() == nil || ready.GetReady().GetCarrierId() != carrierID || ready.GetReady().GetCarrierEpoch() != carrierEpoch {
		t.Fatalf("local direct Carrier ready = %+v, %v", ready, err)
	}
}

func TestDirectV2LocalAccessKeyEndpointFailsClosed(t *testing.T) {
	handler, clientKey, accessKey := newDirectV2LocalTestHandler(t)
	defer handler.close()
	controllerID := uuid.NewString()
	publicKey := base64.RawURLEncoding.EncodeToString(clientKey.Public().(ed25519.PublicKey))
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(clientKey, directV2ControllerProofTranscript(controllerID, publicKey, 1)))
	validBody := `{"controllerId":"` + controllerID + `","clientIdentityAlgorithm":"Ed25519","clientIdentityPublicKey":"` + publicKey + `","clientIdentityKeyVersion":1,"proof":"` + proof + `"}`

	tests := []struct {
		name, path, authorization, body, contentType string
		wantStatus                                   int
	}{
		{name: "wrong key", path: "/v2/direct/device-links", authorization: "Bearer device_" + strings.Repeat("b", 43), body: validBody, contentType: "application/json", wantStatus: http.StatusUnauthorized},
		{name: "key in query", path: "/v2/direct/device-links?accessKey=" + accessKey, authorization: "", body: validBody, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "unknown field", path: "/v2/direct/device-links", authorization: "Bearer " + accessKey, body: strings.TrimSuffix(validBody, "}") + `,"accessKey":"` + accessKey + `"}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "trailing object", path: "/v2/direct/device-links", authorization: "Bearer " + accessKey, body: validBody + `{}`, contentType: "application/json", wantStatus: http.StatusBadRequest},
		{name: "wrong proof", path: "/v2/direct/device-links", authorization: "Bearer " + accessKey, body: strings.Replace(validBody, proof, base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)), 1), contentType: "application/json", wantStatus: http.StatusUnauthorized},
		{name: "wrong content type", path: "/v2/direct/device-links", authorization: "Bearer " + accessKey, body: validBody, contentType: "text/plain", wantStatus: http.StatusUnsupportedMediaType},
		{name: "oversized", path: "/v2/direct/device-links", authorization: "Bearer " + accessKey, body: strings.Repeat("x", directV2LinkRequestLimit+1), contentType: "application/json", wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", testCase.contentType)
			if testCase.authorization != "" {
				request.Header.Set("Authorization", testCase.authorization)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != testCase.wantStatus || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("response = %d %#v %s", response.Code, response.Header(), response.Body.String())
			}
			contents, _ := io.ReadAll(response.Body)
			if bytes.Contains(contents, []byte(accessKey)) {
				t.Fatal("Access Key leaked into error response")
			}
		})
	}
}

func TestDirectV2HostSwitchDoesNotCloseLocalRealm(t *testing.T) {
	handler, _, _ := newDirectV2LocalTestHandler(t)
	defer handler.close()
	handler.setEnabled(true)
	clientID := uuid.NewString()
	hostCarrier, localCarrier := &v2AgentCarrier{}, &v2AgentCarrier{}
	if handler.bindRegistry(directV2HostGrant, clientID, hostCarrier) == nil || handler.bindRegistry(directV2LocalGrant, clientID, localCarrier) == nil {
		t.Fatal("independent Host/local registries were not created")
	}
	handler.releaseRegistry(directV2HostGrant, clientID, hostCarrier)
	handler.releaseRegistry(directV2LocalGrant, clientID, localCarrier)
	if len(handler.registries) != 2 || handler.registries[directV2RegistryKey(directV2HostGrant, clientID)] == handler.registries[directV2RegistryKey(directV2LocalGrant, clientID)] {
		t.Fatalf("Host/local registry isolation = %+v", handler.registries)
	}
	handler.setEnabled(false)
	if len(handler.registries) != 1 || handler.registries[directV2RegistryKey(directV2LocalGrant, clientID)] == nil ||
		handler.registries[directV2RegistryKey(directV2HostGrant, clientID)] != nil {
		t.Fatalf("Host switch changed local registry = %+v", handler.registries)
	}
}

func TestHostV2RouteCoordinatorFencesRelayBeforeDirect(t *testing.T) {
	coordinator := newHostV2RouteCoordinator(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan int32, 2)
	stopped := make(chan int32, 2)
	var generation atomic.Int32
	runResult := make(chan error, 1)
	go func() {
		runResult <- coordinator.run(ctx, func(relayContext context.Context) error {
			current := generation.Add(1)
			started <- current
			<-relayContext.Done()
			stopped <- current
			return relayContext.Err()
		})
	}()
	if current := <-started; current != 1 {
		t.Fatalf("initial Relay generation = %d", current)
	}
	transitionContext, transitionCancel := context.WithTimeout(ctx, 5*time.Second)
	defer transitionCancel()
	if err := coordinator.setDirect(transitionContext, true); err != nil {
		t.Fatal(err)
	}
	select {
	case current := <-stopped:
		if current != 1 {
			t.Fatalf("stopped Relay generation = %d", current)
		}
	default:
		t.Fatal("direct transition completed before the Relay loop stopped")
	}
	if err := coordinator.setDirect(transitionContext, true); err != nil {
		t.Fatal(err)
	}
	select {
	case current := <-started:
		t.Fatalf("idempotent direct transition restarted Relay generation %d", current)
	default:
	}
	if err := coordinator.setDirect(transitionContext, false); err != nil {
		t.Fatal(err)
	}
	if current := <-started; current != 2 {
		t.Fatalf("resumed Relay generation = %d", current)
	}
	if err := coordinator.setDirect(transitionContext, true); err != nil {
		t.Fatal(err)
	}
	if current := <-stopped; current != 2 {
		t.Fatalf("second stopped Relay generation = %d", current)
	}
	cancel()
	if err := <-runResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("coordinator shutdown = %v", err)
	}
}

func TestHostV2RouteCoordinatorHonorsInitialDirectSelection(t *testing.T) {
	coordinator := newHostV2RouteCoordinator(true)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	runResult := make(chan error, 1)
	go func() {
		runResult <- coordinator.run(ctx, func(relayContext context.Context) error {
			started <- struct{}{}
			<-relayContext.Done()
			return relayContext.Err()
		})
	}()
	transitionContext, transitionCancel := context.WithTimeout(ctx, 5*time.Second)
	defer transitionCancel()
	if err := coordinator.setDirect(transitionContext, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("initial Host-direct selection started a Relay loop")
	default:
	}
	if err := coordinator.setDirect(transitionContext, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-transitionContext.Done():
		t.Fatal(transitionContext.Err())
	}
	cancel()
	if err := <-runResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("coordinator shutdown = %v", err)
	}
}

func newDirectV2LocalTestHandler(t *testing.T) (*directV2Handler, ed25519.PrivateKey, string) {
	t.Helper()
	_, deviceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, hostSigningKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	state := &agentState{DeviceID: uuid.New(), KeyVersion: 2, identity: deviceKey}
	runtime := &directV2Runtime{
		config: directV2Config{Enabled: true, IP: "192.0.2.80", Port: 9443}, connectionEpoch: 23,
		nodeID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("wenzwork-direct-node:"+state.DeviceID.String())),
		cellID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("wenzwork-direct-cell:"+state.DeviceID.String())),
	}
	hostVerifier := remoteauth.DeviceLinkGrantVerifier{
		Issuer: "control", Keys: map[string]ed25519.PublicKey{"host": hostSigningKey.Public().(ed25519.PublicKey)},
	}
	handler := newDirectV2Handler(state, runtime, hostVerifier)
	accessKey := "device_" + strings.Repeat("a", 43)
	if err := handler.configureLocalAuthorization(accessKey, directV2DeviceMetadata{Name: "Local workstation", Platform: "linux", AgentVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	return handler, clientKey, accessKey
}
