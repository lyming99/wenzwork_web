package relayserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type staticDeviceKeys map[string]ed25519.PublicKey

func (s staticDeviceKeys) ResolveDeviceKey(_ context.Context, deviceID, thumbprint string) (ed25519.PublicKey, error) {
	key, ok := s[deviceID]
	if !ok || remoteauth.PublicKeyThumbprint(key) != thumbprint {
		return nil, errors.New("device key not found")
	}
	return key, nil
}

func TestHandlerPerformsBinaryTicketBoundProofAndRegistersRoute(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	signerPublic, signerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	devicePublic, devicePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	claims := remoteauth.Claims{
		Audience: "relay", Subject: "device-1", UserID: "user-1", AssignmentID: "assignment-1",
		AssignmentVersion: 7, AllowedCellIDs: []string{"r017"}, GrantVersion: 3,
		Scopes: []string{"remote.connect", "remote.device.read"}, ProtocolMin: 1, ProtocolMax: 1,
		Confirmation: remoteauth.PublicKeyThumbprint(devicePublic), JWTID: "ticket-1",
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}
	token, err := (remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "key-1", PrivateKey: signerPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	registry := relayrouter.NewRegistry()
	registry.PutAssignmentFence("user-1", relayrouter.AssignmentFence{Version: 7, AllowedCellIDs: []string{"r017"}})
	registry.PutGrantFence("device-1", relayrouter.GrantFence{Version: 3, Status: relayrouter.DeviceActive})
	connections, err := NewConnectionManager(100, 10)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleEvents := make(chan RelayConnectionLifecycle, 2)
	handler := &Handler{
		CellID: "r017", NodeID: "r017-node-0", Routes: registry, DeviceKeys: staticDeviceKeys{"device-1": devicePublic},
		Verifier:    remoteauth.Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"key-1": signerPublic}},
		Connections: connections, Now: func() time.Time { return now }, ChallengeTTL: 10 * time.Second, RouteTTL: time.Minute,
		ConnectionLifecycle: func(event RelayConnectionLifecycle) {
			lifecycleEvents <- event
		},
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	header := make(http.Header)
	header.Set("Authorization", "Relay "+token)
	connection, response, err := websocket.Dial(context.Background(), "wss"+strings.TrimPrefix(server.URL, "https")+"/v1/connect", &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: header, Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		if response != nil {
			t.Fatalf("Dial() status=%d error=%v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	defer connection.CloseNow()
	challengeEnvelope := readTestEnvelope(t, connection)
	challengeFrame := challengeEnvelope.GetAuthChallenge()
	if challengeFrame == nil || challengeFrame.GetRelayCellId() != "r017" || challengeFrame.GetRelayNodeId() != "r017-node-0" {
		t.Fatalf("challenge = %+v", challengeEnvelope)
	}
	challenge := remoteauth.Challenge{
		Nonce: challengeFrame.GetNonce(), TicketJWTID: "ticket-1", CellID: "r017", NodeID: "r017-node-0",
		ConnectionEpoch: 11, Deadline: challengeFrame.GetDeadline().AsTime(),
	}
	proof, err := remoteauth.SignChallenge(devicePrivate, challenge)
	if err != nil {
		t.Fatal(err)
	}
	writeTestEnvelope(t, connection, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 11,
		Frame: &remotev1.Envelope_AuthProof{AuthProof: &remotev1.AuthProof{TicketJti: "ticket-1", ConnectionEpoch: 11, DeviceSignature: proof}},
	})
	ready := readTestEnvelope(t, connection).GetReady()
	if ready == nil || ready.GetAcceptedConnectionEpoch() != 11 || ready.GetConnectionId() == "" {
		t.Fatalf("READY = %+v", ready)
	}
	select {
	case event := <-lifecycleEvents:
		if event.Event != "relay_connection_ready" || event.Role != "device" || event.ConnectionEpoch != 11 || event.CorrelationID == "" || event.HeartbeatSeconds == 0 {
			t.Fatalf("lifecycle event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("Relay ready lifecycle event was not recorded")
	}
	route, err := registry.Resolve("device-1", now)
	if err != nil || route.NodeID != "r017-node-0" || route.ConnectionEpoch != 11 {
		t.Fatalf("route = %+v, %v", route, err)
	}
	writeTestEnvelope(t, connection, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 11, Sequence: 9,
		Frame: &remotev1.Envelope_Ping{Ping: &remotev1.Ping{MonotonicMillis: 1234}},
	})
	pong := readTestEnvelope(t, connection)
	if pong.GetPong() == nil || pong.GetPong().GetMonotonicMillis() != 1234 || pong.GetSequence() != 9 {
		t.Fatalf("PONG = %+v", pong)
	}
}

func TestHandlerRejectsTicketInURL(t *testing.T) {
	handler := &Handler{}
	request := httptest.NewRequest(http.MethodGet, "/v1/connect?ticket=secret", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("URL ticket status = %d", response.Code)
	}
}

func TestHandlerAcceptsBrowserPeerTicketCarrierWithoutEchoingCredential(t *testing.T) {
	now := time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC)
	signingPublic, signingPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sourcePublic, sourcePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	targetPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sourceID, targetID, sessionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	claims := remoteauth.Claims{
		Audience: "relay-peer", Subject: sourceID, UserID: uuid.NewString(), SessionID: sessionID,
		SourceDeviceID: sourceID, TargetDeviceID: targetID, SourceGrantVersion: 3, TargetGrantVersion: 4,
		SourceKeyThumbprint: remoteauth.PublicKeyThumbprint(sourcePublic), TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic),
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourcePublic), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetPublic),
		SourceKeyVersion: 3, TargetKeyVersion: 4, SourceCredentialType: "controller",
		Confirmation: remoteauth.PublicKeyThumbprint(sourcePublic), RelayNodeID: "node-browser", RelayCellID: "cell-browser",
		TargetConnectionEpoch: 22, Scopes: []string{"remote.peer.query"}, MaxDurationSeconds: 1, MaxBytes: 1 << 20,
		JWTID: uuid.NewString(), IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	ticket, err := (remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "peer-key", PrivateKey: signingPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	verifier := remoteauth.Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"peer-key": signingPublic}}
	devices := peerDeviceResolverStub{keys: map[string]ed25519.PublicKey{targetID: targetPublic}}
	connections, err := NewConnectionManager(16, 4)
	if err != nil {
		t.Fatal(err)
	}
	targetConnection := &peerFrameConnection{frames: make(chan *remotev1.Envelope, 4)}
	if _, err := connections.Attach(targetID, "target-connection", 22, targetConnection); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connections.Detach(targetID, "target-connection", 22) })
	peerRegistry := &peerRegistryStub{}
	forwarder, err := NewPeerForwarder(PeerForwarderConfig{
		NodeID: "node-browser", CellID: "cell-browser", Verifier: verifier, Devices: devices,
		Routes: peerRegistry, Connections: connections, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{
		CellID: "cell-browser", NodeID: "node-browser", BrowserOriginPatterns: []string{"https://control.example.test"},
		Verifier: verifier, PeerVerifier: verifier,
		DeviceKeys: staticDeviceKeys{}, PeerDevices: devices, Routes: relayrouter.NewRegistry(), Connections: connections,
		Peers: forwarder, Now: func() time.Time { return now }, ChallengeTTL: 10 * time.Second,
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	carrier := peerTicketSubprotocolPrefix + base64.RawURLEncoding.EncodeToString([]byte(ticket))
	connection, response, err := websocket.Dial(context.Background(), "wss"+strings.TrimPrefix(server.URL, "https")+"/v1/connect", &websocket.DialOptions{
		HTTPClient: server.Client(), HTTPHeader: http.Header{"Origin": []string{"https://control.example.test"}},
		Subprotocols: []string{Subprotocol, carrier},
	})
	if err != nil {
		if response != nil {
			t.Fatalf("Dial() status=%d error=%v", response.StatusCode, err)
		}
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if got := connection.Subprotocol(); got != Subprotocol || strings.Contains(got, carrier) {
		t.Fatalf("negotiated subprotocol = %q", got)
	}
	challengeFrame := readTestEnvelope(t, connection).GetAuthChallenge()
	proof, err := remoteauth.SignChallenge(sourcePrivate, remoteauth.Challenge{
		Nonce: challengeFrame.GetNonce(), TicketJWTID: claims.JWTID, CellID: "cell-browser", NodeID: "node-browser",
		ConnectionEpoch: 33, Deadline: challengeFrame.GetDeadline().AsTime(),
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestEnvelope(t, connection, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 33,
		Frame: &remotev1.Envelope_AuthProof{AuthProof: &remotev1.AuthProof{
			TicketJti: claims.JWTID, ConnectionEpoch: 33, DeviceSignature: proof,
		}},
	})
	if ready := readTestEnvelope(t, connection).GetReady(); ready == nil || ready.GetAcceptedConnectionEpoch() != 33 {
		t.Fatalf("READY = %+v", ready)
	}
	// The ticket used for the WebSocket challenge authenticates the controller
	// channel. Each PEER_OPEN below still has its own one-time ticket and
	// identity proof; a warm channel must therefore accept a second, differently
	// scoped session without making the first ticket reusable or broader.
	writeTestEnvelope(t, connection, peerOpenForHandlerTest(t, claims, ticket, sourcePrivate, 33))
	if opened := receivePeerFrame(t, targetConnection.frames).GetPeerOpen(); opened == nil || opened.GetSessionId() != claims.SessionID {
		t.Fatalf("first peer open = %+v", opened)
	}
	secondClaims := claims
	secondClaims.SessionID = uuid.NewString()
	secondClaims.JWTID = uuid.NewString()
	secondClaims.Scopes = []string{"remote.peer.task.control"}
	secondClaims.ProjectID = uuid.NewString()
	secondTicket, err := (remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "peer-key", PrivateKey: signingPrivate}).Sign(secondClaims)
	if err != nil {
		t.Fatal(err)
	}
	// The initial logical ticket lasts one second. Its duration must not be
	// reused as the lifetime of this authenticated carrier WebSocket.
	time.Sleep(1100 * time.Millisecond)
	writeTestEnvelope(t, connection, peerOpenForHandlerTest(t, secondClaims, secondTicket, sourcePrivate, 33))
	if opened := receivePeerFrame(t, targetConnection.frames).GetPeerOpen(); opened == nil || opened.GetSessionId() != secondClaims.SessionID {
		t.Fatalf("second peer open = %+v", opened)
	}

	// A malformed Peer frame has a precise session/query scope. Relay responds
	// with PeerError and keeps the authenticated carrier alive for the next
	// control frame instead of closing this WebSocket.
	invalidQueryID := uuid.NewString()
	writeTestEnvelope(t, connection, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 33,
		Frame: &remotev1.Envelope_PeerQuery{PeerQuery: &remotev1.PeerCiphertext{
			SessionId: claims.SessionID, QueryId: invalidQueryID, Generation: 1, MessageSequence: 1,
			Deadline: timestamppb.New(now.Add(time.Minute)),
		}},
	})
	peerError := readTestEnvelope(t, connection).GetPeerError()
	if peerError == nil || peerError.GetSessionId() != claims.SessionID || peerError.GetQueryId() != invalidQueryID || peerError.GetCode() != remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID {
		t.Fatalf("invalid Peer frame response = %+v", peerError)
	}
	writeTestEnvelope(t, connection, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 33, Sequence: 7,
		Frame: &remotev1.Envelope_Ping{Ping: &remotev1.Ping{MonotonicMillis: 7}},
	})
	if pong := readTestEnvelope(t, connection).GetPong(); pong == nil || pong.GetMonotonicMillis() != 7 {
		t.Fatalf("carrier closed after PeerError: %+v", pong)
	}
}

func TestRelayClientReadTimeoutAllowsTransientMissedHeartbeats(t *testing.T) {
	if got := relayClientReadTimeout(25); got != 53*time.Second {
		t.Fatalf("default read timeout = %s", got)
	}
	if got := relayClientReadTimeout(1); got != 15*time.Second {
		t.Fatalf("minimum read timeout = %s", got)
	}
}

func peerOpenForHandlerTest(t *testing.T, claims remoteauth.Claims, ticket string, sourcePrivate ed25519.PrivateKey, epoch uint64) *remotev1.Envelope {
	t.Helper()
	ephemeral := make([]byte, 32)
	for index := range ephemeral {
		ephemeral[index] = byte(index + 1)
	}
	signature, err := remoteauth.SignPeerOpenIdentity(sourcePrivate, remoteauth.PeerOpenIdentityProof{
		TicketJWTID: claims.JWTID, SessionID: claims.SessionID, SourceDeviceID: claims.SourceDeviceID,
		TargetDeviceID: claims.TargetDeviceID, EphemeralPublicKey: ephemeral,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: epoch,
		Frame: &remotev1.Envelope_PeerOpen{PeerOpen: &remotev1.PeerOpen{
			SessionTicket: ticket, SessionId: claims.SessionID, EphemeralPublicKey: ephemeral, IdentitySignature: signature,
		}},
	}
}

func TestRequestAuthorizationRejectsMixedOrMalformedCredentialCarriers(t *testing.T) {
	ticket := "header.payload.signature"
	carrier := peerTicketSubprotocolPrefix + base64.RawURLEncoding.EncodeToString([]byte(ticket))
	tests := []struct {
		name          string
		authorization string
		protocols     []string
	}{
		{name: "mixed header and carrier", authorization: "Peer " + ticket, protocols: []string{Subprotocol, carrier}},
		{name: "unknown protocol", protocols: []string{Subprotocol, carrier, "another-protocol"}},
		{name: "duplicate fixed", protocols: []string{Subprotocol, Subprotocol}},
		{name: "non canonical base64", protocols: []string{Subprotocol, peerTicketSubprotocolPrefix + "AAAA="}},
		{name: "decoded whitespace", protocols: []string{Subprotocol, peerTicketSubprotocolPrefix + base64.RawURLEncoding.EncodeToString([]byte("bad ticket"))}},
		{name: "carrier without fixed", protocols: []string{carrier}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/connect", nil)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			for _, protocol := range test.protocols {
				request.Header.Add("Sec-WebSocket-Protocol", protocol)
			}
			if token, directPeer, ok := requestAuthorization(request); ok || token != "" || directPeer {
				t.Fatalf("requestAuthorization() = %q, %t, %t", token, directPeer, ok)
			}
		})
	}
}

func readTestEnvelope(t *testing.T, connection *websocket.Conn) *remotev1.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageBinary {
		t.Fatalf("message type = %v", messageType)
	}
	envelope := new(remotev1.Envelope)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func writeTestEnvelope(t *testing.T, connection *websocket.Conn, envelope *remotev1.Envelope) {
	t.Helper()
	payload, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := connection.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
}
