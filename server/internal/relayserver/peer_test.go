package relayserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type peerFrameConnection struct {
	frames chan *remotev1.Envelope
}

func (connection *peerFrameConnection) Write(_ context.Context, _ websocket.MessageType, payload []byte) error {
	envelope := new(remotev1.Envelope)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		return err
	}
	connection.frames <- envelope
	return nil
}

func (*peerFrameConnection) Close(websocket.StatusCode, string) error { return nil }

type peerRegistryStub struct {
	mu       sync.Mutex
	consumed map[string]bool
}

func (registry *peerRegistryStub) ConsumePeerTicket(_ context.Context, jwtID string, _ time.Time, _ time.Time) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.consumed == nil {
		registry.consumed = make(map[string]bool)
	}
	if registry.consumed[jwtID] {
		return relayrouter.ErrPeerTicketReplay
	}
	registry.consumed[jwtID] = true
	return nil
}

type peerDeviceResolverStub struct {
	keys map[string]ed25519.PublicKey
}

func (resolver peerDeviceResolverStub) VerifyPeerDeviceState(_ context.Context, deviceID string, _ uint64, thumbprint string) (ed25519.PublicKey, error) {
	key, ok := resolver.keys[deviceID]
	if !ok || remoteauth.PublicKeyThumbprint(key) != thumbprint {
		return nil, relayrouter.ErrGrantStale
	}
	return key, nil
}

type peerFixture struct {
	now                time.Time
	clock              *time.Time
	sourceID, targetID string
	sourceEpoch        uint64
	targetEpoch        uint64
	sourcePrivate      ed25519.PrivateKey
	targetPrivate      ed25519.PrivateKey
	sourceEphemeral    []byte
	targetEphemeral    []byte
	ticketJWTID        string
	sourceForwarder    *PeerForwarder
	targetForwarder    *PeerForwarder
	sourceFrames       chan *remotev1.Envelope
	targetFrames       chan *remotev1.Envelope
	ticket             string
	sessionID          string
	registry           *peerRegistryStub
}

func newPeerFixture(t *testing.T, targetOnline bool) peerFixture {
	return newPeerFixtureWithMaxBytes(t, targetOnline, 16<<20)
}

func newPeerFixtureWithMaxBytes(t *testing.T, targetOnline bool, maxBytes uint64) peerFixture {
	return newPeerFixtureWithScope(t, targetOnline, maxBytes, "remote.peer.query")
}

func newPeerFixtureWithScope(t *testing.T, targetOnline bool, maxBytes uint64, scope string) peerFixture {
	return newPeerFixtureWithScopeAndProject(t, targetOnline, maxBytes, scope, nil)
}

func newPeerFixtureWithScopeAndProject(t *testing.T, targetOnline bool, maxBytes uint64, scope string, projectOverride *string) peerFixture {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clock := now
	sourceID, targetID := uuid.NewString(), uuid.NewString()
	sourceKey, sourcePrivate, _ := ed25519.GenerateKey(rand.Reader)
	targetKey, targetPrivate, _ := ed25519.GenerateKey(rand.Reader)
	ticketPublic, ticketPrivate, _ := ed25519.GenerateKey(rand.Reader)
	sessionID := uuid.NewString()
	ticketJWTID := uuid.NewString()
	projectID := ""
	if relayPeerScopeRequiresProject(scope) {
		projectID = uuid.NewString()
	}
	if projectOverride != nil {
		projectID = *projectOverride
	}
	issuer := remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "peer-key", PrivateKey: ticketPrivate}
	ticket, err := issuer.Sign(remoteauth.Claims{
		Audience: "relay-peer", Subject: sourceID, UserID: uuid.NewString(), SessionID: sessionID,
		SourceDeviceID: sourceID, TargetDeviceID: targetID, SourceGrantVersion: 1, TargetGrantVersion: 1,
		SourceKeyThumbprint: remoteauth.PublicKeyThumbprint(sourceKey), TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetKey),
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourceKey), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetKey),
		SourceKeyVersion: 1, TargetKeyVersion: 1, SourceCredentialType: "device",
		Confirmation: remoteauth.PublicKeyThumbprint(sourceKey), RelayNodeID: "node-b", RelayCellID: "cell-b", TargetConnectionEpoch: 22,
		Scopes: []string{scope}, ProjectID: projectID, MaxDurationSeconds: 900, MaxBytes: maxBytes,
		JWTID: ticketJWTID, IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := &peerRegistryStub{}
	resolver := peerDeviceResolverStub{keys: map[string]ed25519.PublicKey{sourceID: sourceKey, targetID: targetKey}}
	verifier := remoteauth.Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"peer-key": ticketPublic}}
	manager, _ := NewConnectionManager(16, 4)
	sourceConnection := &peerFrameConnection{frames: make(chan *remotev1.Envelope, 512)}
	targetConnection := &peerFrameConnection{frames: make(chan *remotev1.Envelope, 512)}
	if _, err := manager.Attach(sourceID, "source-connection", 11, sourceConnection); err != nil {
		t.Fatal(err)
	}
	if targetOnline {
		if _, err := manager.Attach(targetID, "target-connection", 22, targetConnection); err != nil {
			t.Fatal(err)
		}
	}
	forwarder, err := NewPeerForwarder(PeerForwarderConfig{
		NodeID: "node-b", CellID: "cell-b", Verifier: verifier, Devices: resolver, Routes: registry,
		Connections: manager, Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Detach(sourceID, "source-connection", 11)
		if targetOnline {
			manager.Detach(targetID, "target-connection", 22)
		}
	})
	return peerFixture{
		now: now, clock: &clock, sourceID: sourceID, targetID: targetID, sourceEpoch: 11, targetEpoch: 22,
		sourcePrivate: sourcePrivate, targetPrivate: targetPrivate,
		sourceEphemeral: bytesOf(32, 1), targetEphemeral: bytesOf(32, 33), ticketJWTID: ticketJWTID,
		sourceForwarder: forwarder, targetForwarder: forwarder,
		sourceFrames: sourceConnection.frames, targetFrames: targetConnection.frames,
		ticket: ticket, sessionID: sessionID, registry: registry,
	}
}

func TestPeerForwarderRejectsMissingUnexpectedAndNonCanonicalProjectClaims(t *testing.T) {
	empty := ""
	missing := newPeerFixtureWithScopeAndProject(t, true, 16<<20, "remote.peer.file.receive", &empty)
	if _, _, err := missing.sourceForwarder.validateOpen(t.Context(), missing.sourceID, missing.openEnvelope(t).GetPeerOpen(), missing.now); !errors.Is(err, remoteauth.ErrTicketClaims) {
		t.Fatalf("missing project claim error = %v", err)
	}
	projectID := uuid.NewString()
	deviceQuery := newPeerFixtureWithScopeAndProject(t, true, 16<<20, "remote.peer.query", &projectID)
	if _, _, err := deviceQuery.sourceForwarder.validateOpen(t.Context(), deviceQuery.sourceID, deviceQuery.openEnvelope(t).GetPeerOpen(), deviceQuery.now); !errors.Is(err, remoteauth.ErrTicketClaims) {
		t.Fatalf("unexpected project claim error = %v", err)
	}
	nonCanonical := strings.ToUpper(uuid.NewString())
	nonCanonicalTicket := newPeerFixtureWithScopeAndProject(t, true, 16<<20, "remote.peer.file.receive", &nonCanonical)
	if _, _, err := nonCanonicalTicket.sourceForwarder.validateOpen(t.Context(), nonCanonicalTicket.sourceID, nonCanonicalTicket.openEnvelope(t).GetPeerOpen(), nonCanonicalTicket.now); !errors.Is(err, remoteauth.ErrTicketClaims) {
		t.Fatalf("non-canonical project claim error = %v", err)
	}
}

func TestPeerErrorBackpressureDoesNotEscalateConnectionFault(t *testing.T) {
	manager, err := NewConnectionManager(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := NewBoundedQueue(relayprotocol.AbsoluteFrameLimit, 1)
	if err != nil {
		t.Fatal(err)
	}
	endpointID := uuid.NewString()
	manager.sessions[endpointID] = &Session{endpointID: endpointID, deviceID: endpointID, connectionID: "test", epoch: 1, queue: queue}
	if err := queue.Enqueue(&remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 1,
		Frame: &remotev1.Envelope_Ping{Ping: &remotev1.Ping{}},
	}); err != nil {
		t.Fatal(err)
	}

	forwarder := &PeerForwarder{connections: manager}
	if err := forwarder.sendError(endpointID, 1, uuid.NewString(), uuid.NewString(), remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, false); err != nil {
		t.Fatalf("saturated error response = %v, want recoverable drop", err)
	}
	if IsPeerConnectionFatal(ErrPeerFrameInvalid) {
		t.Fatal("Peer protocol error was classified as connection-fatal")
	}
	if !IsPeerConnectionFatal(ErrPeerConnectionUnwritable) {
		t.Fatal("unwritable source endpoint was not classified as connection-fatal")
	}
}

func TestPeerForwarderAcceptsLegacyTerminalScope(t *testing.T) {
	fixture := newPeerFixtureWithScope(t, true, 16<<20, "remote.peer.terminal")
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, fixture.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	if received := receivePeerFrame(t, fixture.targetFrames); received.GetPeerOpen() == nil {
		t.Fatalf("terminal peer open = %+v", received)
	}
}

func TestPeerForwarderAcceptsProjectBoundInteractiveTerminalScope(t *testing.T) {
	fixture := newPeerFixtureWithScope(t, true, 16<<20, "remote.peer.terminal.interactive")
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, fixture.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
}

func TestPeerForwarderAcceptsEveryHighRiskProjectScope(t *testing.T) {
	for _, scope := range []string{"remote.peer.ai.tools", "remote.peer.task.control"} {
		t.Run(scope, func(t *testing.T) {
			fixture := newPeerFixtureWithScope(t, true, 16<<20, scope)
			if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, fixture.openEnvelope(t)); err != nil {
				t.Fatal(err)
			}
			if received := receivePeerFrame(t, fixture.targetFrames); received.GetPeerOpen() == nil {
				t.Fatalf("%s peer open = %+v", scope, received)
			}
		})
	}
}

func (fixture peerFixture) openEnvelope(t *testing.T) *remotev1.Envelope {
	t.Helper()
	signature, err := remoteauth.SignPeerOpenIdentity(fixture.sourcePrivate, remoteauth.PeerOpenIdentityProof{
		TicketJWTID: fixture.ticketJWTID, SessionID: fixture.sessionID, SourceDeviceID: fixture.sourceID,
		TargetDeviceID: fixture.targetID, EphemeralPublicKey: fixture.sourceEphemeral,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: fixture.sourceEpoch,
		Frame: &remotev1.Envelope_PeerOpen{PeerOpen: &remotev1.PeerOpen{
			SessionTicket: fixture.ticket, SessionId: fixture.sessionID,
			EphemeralPublicKey: append([]byte(nil), fixture.sourceEphemeral...), IdentitySignature: signature,
		}},
	}
}

func (fixture peerFixture) readyEnvelope(t *testing.T) *remotev1.Envelope {
	t.Helper()
	signature, err := remoteauth.SignPeerReadyIdentity(fixture.targetPrivate, remoteauth.PeerReadyIdentityProof{
		TicketJWTID: fixture.ticketJWTID, SessionID: fixture.sessionID, SourceDeviceID: fixture.sourceID,
		TargetDeviceID: fixture.targetID, SourceEphemeralPublicKey: fixture.sourceEphemeral,
		TargetEphemeralPublicKey: fixture.targetEphemeral,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: fixture.targetEpoch,
		Frame: &remotev1.Envelope_PeerReady{PeerReady: &remotev1.PeerReady{
			SessionId: fixture.sessionID, EphemeralPublicKey: append([]byte(nil), fixture.targetEphemeral...), IdentitySignature: signature,
		}},
	}
}

func bytesOf(size int, start byte) []byte {
	value := make([]byte, size)
	for index := range value {
		value[index] = start + byte(index)
	}
	return value
}

func TestPeerForwarderRoutesOneHundredOrderedMessagesInBothDirectionsOnTargetRelay(t *testing.T) {
	fixture := newPeerFixture(t, true)
	open := fixture.openEnvelope(t)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, open); err != nil {
		t.Fatal(err)
	}
	if received := receivePeerFrame(t, fixture.targetFrames); received.GetPeerOpen() == nil || received.GetConnectionEpoch() != fixture.targetEpoch {
		t.Fatalf("target open = %+v", received)
	}
	ready := fixture.readyEnvelope(t)
	if err := fixture.targetForwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, ready); err != nil {
		t.Fatal(err)
	}
	if received := receivePeerFrame(t, fixture.sourceFrames); received.GetPeerReady() == nil || received.GetConnectionEpoch() != fixture.sourceEpoch {
		t.Fatalf("source ready = %+v", received)
	}

	for sequence := uint64(1); sequence <= 100; sequence++ {
		query := peerCiphertextEnvelope(fixture.sessionID, fixture.sourceEpoch, sequence, fixture.now.Add(time.Minute), true)
		if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, query); err != nil {
			t.Fatalf("source sequence %d: %v", sequence, err)
		}
		delta := peerCiphertextEnvelope(fixture.sessionID, fixture.targetEpoch, sequence, fixture.now.Add(time.Minute), false)
		if err := fixture.targetForwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, delta); err != nil {
			t.Fatalf("target sequence %d: %v", sequence, err)
		}
	}
	for sequence := uint64(1); sequence <= 100; sequence++ {
		if got := receivePeerFrame(t, fixture.targetFrames).GetPeerQuery().GetMessageSequence(); got != sequence {
			t.Fatalf("target sequence = %d, want %d", got, sequence)
		}
		if got := receivePeerFrame(t, fixture.sourceFrames).GetPeerDelta().GetMessageSequence(); got != sequence {
			t.Fatalf("source sequence = %d, want %d", got, sequence)
		}
	}
}

func TestPeerForwarderKeepsAcceptedSessionAfterTicketExpires(t *testing.T) {
	fixture := newPeerFixture(t, true)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, fixture.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, fixture.targetFrames)
	if err := fixture.targetForwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, fixture.readyEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, fixture.sourceFrames)

	// The fixture ticket expires one minute after issue. Its successful
	// PEER_OPEN/PEER_READY admission must still leave the existing encrypted
	// session usable; only a new session needs a fresh ticket.
	*fixture.clock = fixture.now.Add(2 * time.Minute)
	query := peerCiphertextEnvelope(fixture.sessionID, fixture.sourceEpoch, 1, fixture.now.Add(3*time.Minute), true)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, query); err != nil {
		t.Fatal(err)
	}
	if received := receivePeerFrame(t, fixture.targetFrames); received.GetPeerQuery() == nil {
		t.Fatalf("long-lived Peer query = %+v", received)
	}
}

func TestPeerForwarderReturnsOfflineAndRejectsTicketReplay(t *testing.T) {
	offline := newPeerFixture(t, false)
	open := offline.openEnvelope(t)
	if err := offline.sourceForwarder.HandleFromDevice(context.Background(), offline.sourceID, offline.sourceEpoch, open); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, offline.sourceFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_PEER_OFFLINE {
		t.Fatalf("offline code = %s", code)
	}

	online := newPeerFixture(t, true)
	open = online.openEnvelope(t)
	if err := online.sourceForwarder.HandleFromDevice(context.Background(), online.sourceID, online.sourceEpoch, open); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, online.targetFrames)
	if err := online.sourceForwarder.HandleFromDevice(context.Background(), online.sourceID, online.sourceEpoch, open); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, online.sourceFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_TICKET_INVALID {
		t.Fatalf("replay code = %s", code)
	}
}

func TestPeerForwarderRejectsExpiredTicketAndAuthenticatedSourceSpoof(t *testing.T) {
	expired := newPeerFixture(t, true)
	*expired.clock = expired.now.Add(2 * time.Minute)
	if err := expired.sourceForwarder.HandleFromDevice(context.Background(), expired.sourceID, expired.sourceEpoch, expired.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, expired.sourceFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_TICKET_EXPIRED {
		t.Fatalf("expired ticket code = %s", code)
	}

	spoofed := newPeerFixture(t, true)
	open := spoofed.openEnvelope(t)
	open.ConnectionEpoch = spoofed.targetEpoch
	if err := spoofed.targetForwarder.HandleFromDevice(context.Background(), spoofed.targetID, spoofed.targetEpoch, open); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, spoofed.targetFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_TICKET_INVALID {
		t.Fatalf("source spoof code = %s", code)
	}
}

func TestPeerForwarderRejectsSequenceGapsAndSignalsDisconnect(t *testing.T) {
	fixture := newPeerFixture(t, true)
	open := fixture.openEnvelope(t)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, open); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, fixture.targetFrames)
	ready := fixture.readyEnvelope(t)
	if err := fixture.targetForwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, ready); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, fixture.sourceFrames)
	gap := peerCiphertextEnvelope(fixture.sessionID, fixture.sourceEpoch, 2, fixture.now.Add(time.Minute), true)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, gap); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, fixture.sourceFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID {
		t.Fatalf("gap error = %s", code)
	}
	fixture.sourceForwarder.Disconnect(context.Background(), fixture.sourceID)
	if code := receivePeerFrame(t, fixture.targetFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED {
		t.Fatalf("disconnect error = %s", code)
	}
}

func TestPeerForwarderRejectsForgedEphemeralIdentityProofs(t *testing.T) {
	fixture := newPeerFixture(t, true)
	open := fixture.openEnvelope(t)
	open.GetPeerOpen().EphemeralPublicKey[0] ^= 0xff
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, open); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, fixture.sourceFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_TICKET_INVALID {
		t.Fatalf("forged PEER_OPEN code = %s", code)
	}

	fixture = newPeerFixture(t, true)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, fixture.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, fixture.targetFrames)
	ready := fixture.readyEnvelope(t)
	ready.GetPeerReady().IdentitySignature[0] ^= 0xff
	if err := fixture.targetForwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, ready); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, fixture.targetFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID {
		t.Fatalf("forged PEER_READY code = %s", code)
	}
}

func TestPeerForwarderEnforcesTicketByteLimit(t *testing.T) {
	fixture := newPeerFixtureWithMaxBytes(t, true, 3)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, fixture.openEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, fixture.targetFrames)
	if err := fixture.targetForwarder.HandleFromDevice(context.Background(), fixture.targetID, fixture.targetEpoch, fixture.readyEnvelope(t)); err != nil {
		t.Fatal(err)
	}
	_ = receivePeerFrame(t, fixture.sourceFrames)

	query := peerCiphertextEnvelope(fixture.sessionID, fixture.sourceEpoch, 1, fixture.now.Add(time.Minute), true)
	if err := fixture.sourceForwarder.HandleFromDevice(context.Background(), fixture.sourceID, fixture.sourceEpoch, query); err != nil {
		t.Fatal(err)
	}
	if code := receivePeerFrame(t, fixture.sourceFrames).GetPeerError().GetCode(); code != remotev1.ErrorCode_ERROR_CODE_RATE_LIMITED {
		t.Fatalf("byte limit code = %s", code)
	}
}

func peerCiphertextEnvelope(sessionID string, epoch, sequence uint64, deadline time.Time, query bool) *remotev1.Envelope {
	payload := &remotev1.PeerCiphertext{
		SessionId: sessionID, QueryId: "query-1", Generation: 1, MessageSequence: sequence,
		Deadline: timestamppb.New(deadline), Ciphertext: []byte{1, 2, 3, byte(sequence)},
	}
	envelope := &remotev1.Envelope{ProtocolVersion: 1, ConnectionEpoch: epoch}
	if query {
		envelope.Frame = &remotev1.Envelope_PeerQuery{PeerQuery: payload}
	} else {
		envelope.Frame = &remotev1.Envelope_PeerDelta{PeerDelta: payload}
	}
	return envelope
}

func receivePeerFrame(t *testing.T, frames <-chan *remotev1.Envelope) *remotev1.Envelope {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Peer frame")
		return nil
	}
}
