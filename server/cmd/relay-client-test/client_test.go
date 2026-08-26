package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/peerprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

type clientDeviceKeyResolver struct {
	deviceID string
	key      ed25519.PublicKey
}

func (resolver clientDeviceKeyResolver) ResolveDeviceKey(_ context.Context, deviceID, thumbprint string) (ed25519.PublicKey, error) {
	if deviceID != resolver.deviceID || thumbprint != remoteauth.PublicKeyThumbprint(resolver.key) {
		return nil, errors.New("device key not found")
	}
	return resolver.key, nil
}

func TestStateRoundTripPersistsIdentityAndMonotonicEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "relay-client-state.json")
	created, err := loadOrCreateState(path)
	if err != nil {
		t.Fatal(err)
	}
	if created.DeviceID.String() == "00000000-0000-0000-0000-000000000000" || len(created.identity) == 0 || created.ConnectionEpoch != 0 {
		t.Fatalf("created state = %+v", created)
	}
	created.ConnectionEpoch = 9
	created.RefreshToken = "refresh-token-is-state-only"
	if err := writeState(path, created); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadOrCreateState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DeviceID != created.DeviceID || loaded.ConnectionEpoch != 9 || !loaded.identity.Equal(created.identity) || loaded.RefreshToken != created.RefreshToken {
		t.Fatalf("loaded state does not match: %+v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state permissions = %o", info.Mode().Perm())
	}
}

func TestClientOptionsRequireSecureURLsAndExposeNoInsecureFlag(t *testing.T) {
	var stderr bytes.Buffer
	opts, err := parseOptions([]string{
		"run", "--control-url", "https://control.example.test", "--state-file", "state.json",
	}, &stderr)
	if err != nil || opts.pingCount != 5 || opts.timeout != 2*time.Minute {
		t.Fatalf("parseOptions() = %+v, %v, stderr=%s", opts, err, stderr.String())
	}
	for name, arguments := range map[string][]string{
		"plaintext remote": {"run", "--control-url", "http://control.example.test", "--state-file", "state.json"},
		"insecure flag":    {"run", "--control-url", "https://control.example.test", "--state-file", "state.json", "--insecure"},
		"too few pings":    {"run", "--control-url", "https://control.example.test", "--state-file", "state.json", "--ping-count", "4"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOptions(arguments, &bytes.Buffer{}); err == nil {
				t.Fatal("parseOptions() accepted unsafe/invalid arguments")
			}
		})
	}
}

func TestPeerOptionsRequireTwoDistinctStateFilesAndOneHundredMessages(t *testing.T) {
	opts, err := parseOptions([]string{
		"peer", "--control-url", "https://control.example.test", "--state-file", "source.json",
		"--target-state-file", "target.json", "--message-count", "100",
	}, &bytes.Buffer{})
	if err != nil || opts.mode != "peer" || opts.messageCount != 100 || opts.targetStateFile != "target.json" {
		t.Fatalf("parseOptions(peer) = %+v, %v", opts, err)
	}
	for _, arguments := range [][]string{
		{"peer", "--control-url", "https://control.example.test", "--state-file", "same.json", "--target-state-file", "same.json"},
		{"peer", "--control-url", "https://control.example.test", "--state-file", "source.json", "--target-state-file", "target.json", "--message-count", "99"},
	} {
		if _, err := parseOptions(arguments, &bytes.Buffer{}); err == nil {
			t.Fatalf("parseOptions accepted invalid Peer arguments: %v", arguments)
		}
	}
}

func TestPongValidationBindsEpochSequenceAndMonotonicValue(t *testing.T) {
	envelope := &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 11, Sequence: 5,
		Frame: &remotev1.Envelope_Pong{Pong: &remotev1.Pong{MonotonicMillis: 1234}},
	}
	if err := validatePong(envelope, 11, 5, 1234); err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string]*remotev1.Envelope{
		"epoch":    {ProtocolVersion: 1, ConnectionEpoch: 10, Sequence: 5, Frame: &remotev1.Envelope_Pong{Pong: &remotev1.Pong{MonotonicMillis: 1234}}},
		"sequence": {ProtocolVersion: 1, ConnectionEpoch: 11, Sequence: 4, Frame: &remotev1.Envelope_Pong{Pong: &remotev1.Pong{MonotonicMillis: 1234}}},
		"value":    {ProtocolVersion: 1, ConnectionEpoch: 11, Sequence: 5, Frame: &remotev1.Envelope_Pong{Pong: &remotev1.Pong{MonotonicMillis: 1233}}},
		"frame":    {ProtocolVersion: 1, ConnectionEpoch: 11, Sequence: 5, Frame: &remotev1.Envelope_Ping{Ping: &remotev1.Ping{MonotonicMillis: 1234}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePong(candidate, 11, 5, 1234); err == nil {
				t.Fatal("validatePong() accepted a mismatched frame")
			}
		})
	}
}

func TestClientCompletesBinaryWSSProofReadyAndFivePongs(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	signerPublic, signerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	devicePublic, devicePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	deviceID, userID, assignmentID, cellID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	claims := remoteauth.Claims{
		Audience: "relay", Subject: deviceID.String(), UserID: userID.String(), AssignmentID: assignmentID.String(),
		AssignmentVersion: 7, AllowedCellIDs: []string{cellID.String()}, GrantVersion: 3,
		Scopes: []string{"remote.connect"}, ProtocolMin: 1, ProtocolMax: 1,
		Confirmation: remoteauth.PublicKeyThumbprint(devicePublic), JWTID: uuid.NewString(),
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}
	ticket, err := (remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "client-e2e", PrivateKey: signerPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	routes := relayrouter.NewRegistry()
	routes.PutAssignmentFence(userID.String(), relayrouter.AssignmentFence{Version: 7, AllowedCellIDs: []string{cellID.String()}})
	routes.PutGrantFence(deviceID.String(), relayrouter.GrantFence{Version: 3, Status: relayrouter.DeviceActive})
	connections, err := relayserver.NewConnectionManager(8, 2)
	if err != nil {
		t.Fatal(err)
	}
	handler := &relayserver.Handler{
		CellID: cellID.String(), NodeID: "client-e2e-node", Routes: routes,
		DeviceKeys:  clientDeviceKeyResolver{deviceID: deviceID.String(), key: devicePublic},
		Verifier:    remoteauth.Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"client-e2e": signerPublic}},
		Connections: connections, Now: func() time.Time { return now }, ChallengeTTL: 10 * time.Second,
		RouteTTL: time.Minute, HeartbeatSeconds: 1,
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	endpointURL := "wss" + strings.TrimPrefix(server.URL, "https") + "/v1/connect"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := connectEndpoint(ctx, server.Client(), allocationEndpoint{
		CellID: cellID, EndpointRevision: 1, URL: endpointURL,
	}, assignmentID, ticket, clientState{
		SchemaVersion: 1, DeviceID: deviceID, SessionID: uuid.New(), ConnectionEpoch: 11,
		identity: devicePrivate,
	}, 5, reporter{stdout: io.Discard, stderr: io.Discard})
	if err != nil {
		t.Fatalf("connectEndpoint() error = %v", err)
	}
	if result.HeartbeatSeconds != 1 || len(result.RTTs) != 5 || result.Max < result.Min || result.Average < result.Min || result.Average > result.Max {
		t.Fatalf("Relay result = %+v", result)
	}
}

func TestTicketClaimsAndGoAwayParsing(t *testing.T) {
	claims := remoteauth.Claims{Audience: "relay", AssignmentID: "assignment-1", JWTID: "ticket-1"}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	ticket := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	parsed, err := parseTicketClaims(ticket)
	if err != nil || parsed.JWTID != claims.JWTID || parsed.AssignmentID != claims.AssignmentID {
		t.Fatalf("parseTicketClaims() = %+v, %v", parsed, err)
	}
	malformed := "header." + base64.RawURLEncoding.EncodeToString(append(payload, []byte(`{}`)...)) + ".signature"
	if _, err := parseTicketClaims(malformed); err == nil {
		t.Fatal("parseTicketClaims() accepted trailing JSON")
	}

	message := goAwayMessage(&remotev1.GoAway{
		Reason:            remotev1.GoAwayReason_GO_AWAY_REASON_ASSIGNMENT_CHANGED,
		RefreshAssignment: true, ReconnectAfterMillis: 2500,
	})
	if message == nil || !strings.Contains(message.Error(), "ASSIGNMENT_CHANGED") ||
		!strings.Contains(message.Error(), "refresh_assignment=true") || !strings.Contains(message.Error(), "2.5s") {
		t.Fatalf("goAwayMessage() = %v", message)
	}
}

type peerAcceptanceRegistry struct {
	*relayrouter.Registry
	mu       sync.Mutex
	keys     map[string]ed25519.PublicKey
	versions map[string]uint64
	consumed map[string]bool
}

func (registry *peerAcceptanceRegistry) VerifyPeerDeviceState(_ context.Context, deviceID string, version uint64, thumbprint string) (ed25519.PublicKey, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	key := registry.keys[deviceID]
	if len(key) != ed25519.PublicKeySize || registry.versions[deviceID] != version || remoteauth.PublicKeyThumbprint(key) != thumbprint {
		return nil, relayrouter.ErrGrantStale
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

func (registry *peerAcceptanceRegistry) ConsumePeerTicket(_ context.Context, jwtID string, expiresAt, now time.Time) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if jwtID == "" || !expiresAt.After(now) || registry.consumed[jwtID] {
		return relayrouter.ErrPeerTicketReplay
	}
	registry.consumed[jwtID] = true
	return nil
}

type peerAcceptanceKeyResolver struct {
	keys map[string]ed25519.PublicKey
}

func (resolver peerAcceptanceKeyResolver) ResolveDeviceKey(_ context.Context, deviceID, thumbprint string) (ed25519.PublicKey, error) {
	key := resolver.keys[deviceID]
	if len(key) != ed25519.PublicKeySize || remoteauth.PublicKeyThumbprint(key) != thumbprint {
		return nil, errors.New("device key not found")
	}
	return key, nil
}

func TestControllerConnectsDirectlyToTargetRelayAndExchangesOneHundredEncryptedMessages(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	connectionPublic, connectionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerPublic, peerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sourcePublic, sourcePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	targetPublic, targetPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userID, cellID, sourceNodeID, targetNodeID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	sourceID, targetID := uuid.New(), uuid.New()
	sourceAssignment, targetAssignment := uuid.New(), uuid.New()
	connectionIssuer := remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "connection-key", PrivateKey: connectionPrivate}
	connectionTicket := func(deviceID, assignmentID uuid.UUID, publicKey ed25519.PublicKey) string {
		ticket, signErr := connectionIssuer.Sign(remoteauth.Claims{
			Audience: "relay", Subject: deviceID.String(), UserID: userID.String(), AssignmentID: assignmentID.String(),
			AssignmentVersion: 7, AllowedCellIDs: []string{cellID.String()}, GrantVersion: 3,
			Scopes: []string{"remote.connect"}, ProtocolMin: 1, ProtocolMax: 1,
			Confirmation: remoteauth.PublicKeyThumbprint(publicKey), JWTID: uuid.NewString(),
			IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
		})
		if signErr != nil {
			t.Fatal(signErr)
		}
		return ticket
	}
	verifier := remoteauth.Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{
		"connection-key": connectionPublic, "peer-key": peerPublic,
	}}
	routes := &peerAcceptanceRegistry{
		Registry: relayrouter.NewRegistry(),
		keys:     map[string]ed25519.PublicKey{sourceID.String(): sourcePublic, targetID.String(): targetPublic},
		versions: map[string]uint64{sourceID.String(): 3, targetID.String(): 3}, consumed: make(map[string]bool),
	}
	routes.PutAssignmentFence(userID.String(), relayrouter.AssignmentFence{Version: 7, AllowedCellIDs: []string{cellID.String()}})
	routes.PutGrantFence(sourceID.String(), relayrouter.GrantFence{Version: 3, Status: relayrouter.DeviceActive})
	routes.PutGrantFence(targetID.String(), relayrouter.GrantFence{Version: 3, Status: relayrouter.DeviceActive})
	sourceConnections, _ := relayserver.NewConnectionManager(8, 2)
	targetConnections, _ := relayserver.NewConnectionManager(8, 2)
	sourceForwarder, err := relayserver.NewPeerForwarder(relayserver.PeerForwarderConfig{
		NodeID: sourceNodeID.String(), CellID: cellID.String(), Verifier: verifier, Devices: routes, Routes: routes,
		Connections: sourceConnections,
	})
	if err != nil {
		t.Fatal(err)
	}
	targetForwarder, err := relayserver.NewPeerForwarder(relayserver.PeerForwarderConfig{
		NodeID: targetNodeID.String(), CellID: cellID.String(), Verifier: verifier, Devices: routes, Routes: routes,
		Connections: targetConnections,
	})
	if err != nil {
		t.Fatal(err)
	}
	keyResolver := peerAcceptanceKeyResolver{keys: routes.keys}
	sourceServer := httptest.NewTLSServer(&relayserver.Handler{
		CellID: cellID.String(), NodeID: sourceNodeID.String(), Verifier: verifier, PeerVerifier: verifier,
		DeviceKeys: keyResolver, PeerDevices: routes,
		Routes: routes, Connections: sourceConnections, Peers: sourceForwarder, RouteTTL: time.Minute,
		ChallengeTTL: 10 * time.Second, HeartbeatSeconds: 5, MaxFramesPerSecond: 200,
	})
	defer sourceServer.Close()
	targetServer := httptest.NewTLSServer(&relayserver.Handler{
		CellID: cellID.String(), NodeID: targetNodeID.String(), Verifier: verifier, PeerVerifier: verifier,
		DeviceKeys: keyResolver, PeerDevices: routes,
		Routes: routes, Connections: targetConnections, Peers: targetForwarder, RouteTTL: time.Minute,
		ChallengeTTL: 10 * time.Second, HeartbeatSeconds: 5, MaxFramesPerSecond: 200,
	})
	defer targetServer.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	sourceState := clientState{SchemaVersion: 1, DeviceID: sourceID, SessionID: uuid.New(), ConnectionEpoch: 11, identity: sourcePrivate}
	targetState := clientState{SchemaVersion: 1, DeviceID: targetID, SessionID: uuid.New(), ConnectionEpoch: 22, identity: targetPrivate}
	sourceRelay, err := dialRelayEndpoint(ctx, sourceServer.Client(), allocationEndpoint{
		CellID: cellID, EndpointRevision: 1, URL: "wss" + strings.TrimPrefix(sourceServer.URL, "https") + "/v1/connect",
	}, sourceAssignment, connectionTicket(sourceID, sourceAssignment, sourcePublic), sourceState)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRelay.socket.CloseNow()
	targetRelay, err := dialRelayEndpoint(ctx, targetServer.Client(), allocationEndpoint{
		CellID: cellID, EndpointRevision: 1, URL: "wss" + strings.TrimPrefix(targetServer.URL, "https") + "/v1/connect",
	}, targetAssignment, connectionTicket(targetID, targetAssignment, targetPublic), targetState)
	if err != nil {
		t.Fatal(err)
	}
	defer targetRelay.socket.CloseNow()
	sourceRouteBeforeDirect, err := routes.Resolve(sourceID.String(), time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve source resident route before direct connect: %v", err)
	}
	if sourceRouteBeforeDirect.NodeID != sourceNodeID.String() || sourceRouteBeforeDirect.ConnectionEpoch != sourceState.ConnectionEpoch {
		t.Fatalf("source resident route before direct connect = %+v", sourceRouteBeforeDirect)
	}

	sessionID, ticketJWTID := uuid.NewString(), uuid.NewString()
	peerTicket, err := (remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "peer-key", PrivateKey: peerPrivate}).Sign(remoteauth.Claims{
		Audience: "relay-peer", Subject: sourceID.String(), UserID: userID.String(), SessionID: sessionID,
		SourceDeviceID: sourceID.String(), TargetDeviceID: targetID.String(), SourceGrantVersion: 3, TargetGrantVersion: 3,
		SourceKeyThumbprint: remoteauth.PublicKeyThumbprint(sourcePublic), TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic),
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourcePublic), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetPublic),
		SourceKeyVersion: 1, TargetKeyVersion: 1, SourceCredentialType: "device",
		Confirmation: remoteauth.PublicKeyThumbprint(sourcePublic), RelayNodeID: targetNodeID.String(), RelayCellID: cellID.String(),
		TargetConnectionEpoch: targetState.ConnectionEpoch,
		Scopes:                []string{"remote.peer.query"}, MaxDurationSeconds: 900, MaxBytes: 16 << 20, JWTID: ticketJWTID,
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	directState := sourceState
	directState.ConnectionEpoch++
	directRelay, err := dialDirectPeerRelay(ctx, targetServer.Client(), peerTicketResponse{
		SessionID: uuid.MustParse(sessionID), PeerSessionTicket: peerTicket, ExpiresAt: now.Add(time.Minute),
		MaxDurationSeconds: 900, MaxBytes: 16 << 20, TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic),
		RelayURL:    "wss" + strings.TrimPrefix(targetServer.URL, "https") + "/v1/connect",
		RelayNodeID: targetNodeID, RelayCellID: cellID, TargetConnectionEpoch: targetState.ConnectionEpoch,
	}, directState)
	if err != nil {
		t.Fatal(err)
	}
	defer directRelay.socket.CloseNow()
	sourceEphemeral, _ := ecdh.X25519().GenerateKey(rand.Reader)
	openProof := remoteauth.PeerOpenIdentityProof{
		TicketJWTID: ticketJWTID, SessionID: sessionID, SourceDeviceID: sourceID.String(), TargetDeviceID: targetID.String(),
		EphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(),
	}
	openSignature, _ := remoteauth.SignPeerOpenIdentity(sourcePrivate, openProof)
	if err := writeRelayEnvelope(ctx, directRelay.socket, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: directState.ConnectionEpoch,
		Frame: &remotev1.Envelope_PeerOpen{PeerOpen: &remotev1.PeerOpen{
			SessionTicket: peerTicket, SessionId: sessionID, EphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(), IdentitySignature: openSignature,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	targetOpenEnvelope, err := readPeerEnvelope(ctx, targetRelay.socket, targetState.ConnectionEpoch)
	if err != nil || targetOpenEnvelope.GetPeerOpen() == nil {
		t.Fatalf("target PEER_OPEN = %+v, %v", targetOpenEnvelope, err)
	}
	targetEphemeral, _ := ecdh.X25519().GenerateKey(rand.Reader)
	readyProof := remoteauth.PeerReadyIdentityProof{
		TicketJWTID: ticketJWTID, SessionID: sessionID, SourceDeviceID: sourceID.String(), TargetDeviceID: targetID.String(),
		SourceEphemeralPublicKey: sourceEphemeral.PublicKey().Bytes(), TargetEphemeralPublicKey: targetEphemeral.PublicKey().Bytes(),
	}
	readySignature, _ := remoteauth.SignPeerReadyIdentity(targetPrivate, readyProof)
	if err := writeRelayEnvelope(ctx, targetRelay.socket, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: targetState.ConnectionEpoch,
		Frame: &remotev1.Envelope_PeerReady{PeerReady: &remotev1.PeerReady{
			SessionId: sessionID, EphemeralPublicKey: targetEphemeral.PublicKey().Bytes(), IdentitySignature: readySignature,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	sourceReadyEnvelope, err := readPeerEnvelope(ctx, directRelay.socket, directState.ConnectionEpoch)
	if err != nil || sourceReadyEnvelope.GetPeerReady() == nil {
		t.Fatalf("source PEER_READY = %+v, %v", sourceReadyEnvelope, err)
	}
	sourceSecret, _ := peerprotocol.X25519SharedSecret(sourceEphemeral, targetEphemeral.PublicKey().Bytes())
	targetSecret, _ := peerprotocol.X25519SharedSecret(targetEphemeral, sourceEphemeral.PublicKey().Bytes())
	sourceKeys, _ := peerprotocol.DeriveSessionKeys(sourceSecret, ticketJWTID, sessionID, sourceID.String(), targetID.String())
	targetKeys, _ := peerprotocol.DeriveSessionKeys(targetSecret, ticketJWTID, sessionID, sourceID.String(), targetID.String())
	samples := &peerExchangeSamples{}
	if err := exchangePeerMessages(ctx, directRelay.socket, targetRelay.socket, directState.ConnectionEpoch, targetState.ConnectionEpoch, sessionID, sourceKeys, targetKeys, 100, samples); err != nil {
		t.Fatal(err)
	}
	if len(samples.sourceToTarget) != 100 || len(samples.targetToSource) != 100 {
		t.Fatalf("Peer latency samples = source:%d target:%d", len(samples.sourceToTarget), len(samples.targetToSource))
	}
	sourceRouteAfterDirect, err := routes.Resolve(sourceID.String(), time.Now().UTC())
	if err != nil {
		t.Fatalf("resolve source resident route after direct connect: %v", err)
	}
	if sourceRouteAfterDirect.NodeID != sourceRouteBeforeDirect.NodeID ||
		sourceRouteAfterDirect.ConnectionID != sourceRouteBeforeDirect.ConnectionID ||
		sourceRouteAfterDirect.ConnectionEpoch != sourceRouteBeforeDirect.ConnectionEpoch {
		t.Fatalf("direct control connection overwrote resident route: before=%+v after=%+v", sourceRouteBeforeDirect, sourceRouteAfterDirect)
	}
}

func TestStageErrorPreservesDocumentedExitCode(t *testing.T) {
	err := stageError{code: 41, err: errors.New("proof failed")}
	var staged stageError
	if !errors.As(err, &staged) || staged.code != 41 || staged.Error() != "proof failed" {
		t.Fatalf("stage error = %+v", staged)
	}
}
