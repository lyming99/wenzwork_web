package relayserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

type v2TestDeviceAuthenticator struct {
	deviceID          string
	userID            string
	epoch             uint64
	assignmentVersion uint64
	grantVersion      uint64
}

func (authenticator v2TestDeviceAuthenticator) AuthenticateV2Device(_ context.Context, _ *remotev2.CarrierEnvelope, _ *remotev2.CarrierHello, _ time.Time) (V2DevicePrincipal, error) {
	return V2DevicePrincipal{
		DeviceID: authenticator.deviceID, UserID: authenticator.userID, ConnectionEpoch: authenticator.epoch,
		AssignmentVersion: authenticator.assignmentVersion, GrantVersion: authenticator.grantVersion,
	}, nil
}

func TestV2CarrierForwardsOpaqueLinkFramesAndRejectsGrantReplay(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuerPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x51}, ed25519.SeedSize))
	clientPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x52}, ed25519.SeedSize))
	deviceIdentity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x53}, ed25519.SeedSize))
	clientID, deviceID, userID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	nodeID, cellID := uuid.NewString(), uuid.NewString()
	claims := remoteauth.DeviceLinkGrantClaims{
		Audience: remoteauth.DeviceLinkGrantAudience, GrantID: uuid.NewString(), ClientID: clientID, DeviceID: deviceID,
		RelayNodeID: nodeID, RelayCellID: cellID, TargetConnectionEpoch: 1,
		ClientIdentityKey:   base64.RawURLEncoding.EncodeToString(clientPrivate.Public().(ed25519.PublicKey)),
		ClientKeyThumbprint: remoteauth.PublicKeyThumbprint(clientPrivate.Public().(ed25519.PublicKey)), ClientIdentityKeyVersion: 3,
		DeviceKeyThumbprint: remoteauth.PublicKeyThumbprint(deviceIdentity.Public().(ed25519.PublicKey)), DeviceIdentityKeyVersion: 4,
		ClientGrantVersion: 5, DeviceGrantVersion: 6, AllowedScopes: []string{"remote.peer.query"}, MaximumLifetimeSeconds: 90,
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(90 * time.Second).Unix(),
	}
	grant, err := (remoteauth.DeviceLinkGrantIssuer{Issuer: "control", KeyID: "v2", PrivateKey: issuerPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	handler := &V2Handler{
		CellID: cellID, NodeID: nodeID,
		ClientGrantVerifier: remoteauth.DeviceLinkGrantVerifier{Issuer: "control", Keys: map[string]ed25519.PublicKey{"v2": issuerPrivate.Public().(ed25519.PublicKey)}},
		DeviceAuthenticator: v2TestDeviceAuthenticator{
			deviceID: deviceID, userID: userID, epoch: 1, assignmentVersion: 7, grantVersion: 6,
		},
		GrantUses:             NewInMemoryV2GrantUseStore(),
		BrowserOriginPatterns: []string{"*"},
		Now:                   func() time.Time { return now },
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/connect"

	deviceCarrierID := uuid.NewString()
	device, _, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{Subprotocols: []string{V2Subprotocol}})
	if err != nil {
		t.Fatalf("device Dial() error = %v", err)
	}
	defer device.CloseNow()
	if err := writeV2Envelope(context.Background(), device, &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: deviceCarrierID, CarrierEpoch: 1, PacketSequence: 1,
		Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
			DeviceConnectionTicket: "device-ticket", DeviceId: deviceID, DeviceConnectionEpoch: 1,
			ClientChallenge: bytes.Repeat([]byte{0x01}, 32), DeviceProof: bytes.Repeat([]byte{0x02}, ed25519.SignatureSize),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	deviceReady := readV2Envelope(t, device)
	if deviceReady.GetReady() == nil || deviceReady.GetCarrierId() != deviceCarrierID {
		t.Fatalf("device ready = %+v", deviceReady)
	}
	resident := handler.Hub.ResidentRoutes()
	if len(resident) != 1 || resident[0].DeviceID != deviceID || resident[0].UserID != userID ||
		resident[0].ConnectionID != deviceCarrierID || resident[0].ConnectionEpoch != 1 ||
		resident[0].AssignmentVersion != 7 || resident[0].GrantVersion != 6 || resident[0].ProtocolVersion != 2 {
		t.Fatalf("resident route after device ready = %+v", resident)
	}

	clientCarrierID := uuid.NewString()
	client, _, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{Subprotocols: []string{V2Subprotocol}})
	if err != nil {
		t.Fatalf("client Dial() error = %v", err)
	}
	defer client.CloseNow()
	challenge := bytes.Repeat([]byte{0x03}, 32)
	proof, err := remoteauth.SignCarrierProof(clientPrivate, remoteauth.CarrierProof{GrantID: claims.GrantID, CarrierID: clientCarrierID, CarrierEpoch: 1, Challenge: challenge})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeV2Envelope(context.Background(), client, &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: clientCarrierID, CarrierEpoch: 1, PacketSequence: 1,
		Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
			Grant: grant, GrantId: claims.GrantID, ClientId: clientID, ClientIdentityKeyVersion: 3, ClientChallenge: challenge, ClientProof: proof,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	clientReady := readV2Envelope(t, client)
	if clientReady.GetReady() == nil || clientReady.GetCarrierId() != clientCarrierID || clientReady.GetAcknowledgedSequence() != 1 {
		t.Fatalf("client ready = %+v", clientReady)
	}

	linkID := uuid.NewString()
	linkInit := &remotev2.LinkEnvelope{LinkId: linkID, Body: &remotev2.LinkEnvelope_LinkInit{LinkInit: &remotev2.LinkInit{
		GrantId: claims.GrantID, LinkId: linkID, ClientId: clientID, DeviceId: deviceID, RelayNodeId: nodeID, RelayCellId: cellID,
		TargetConnectionEpoch: 1, ClientIdentityKeyVersion: 3, ClientEphemeralPublicKey: bytes.Repeat([]byte{0x61}, 32), ClientChallenge: bytes.Repeat([]byte{0x62}, 32),
		IdentitySignature: bytes.Repeat([]byte{0x63}, ed25519.SignatureSize), DeviceConnectionGrant: grant,
	}}}
	if err := writeV2Envelope(context.Background(), client, &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: clientCarrierID, CarrierEpoch: 1, PacketSequence: 2, AcknowledgedSequence: 1,
		Body: &remotev2.CarrierEnvelope_Link{Link: linkInit},
	}); err != nil {
		t.Fatal(err)
	}
	forwardedInit := readV2Envelope(t, device)
	if forwardedInit.GetCarrierId() != deviceCarrierID || forwardedInit.GetPacketSequence() != 2 || forwardedInit.GetLink() == nil || forwardedInit.GetLink().GetLinkInit().GetLinkId() != linkID {
		t.Fatalf("forwarded init = %+v", forwardedInit)
	}

	accept := &remotev2.LinkEnvelope{LinkId: linkID, Body: &remotev2.LinkEnvelope_LinkAccept{LinkAccept: &remotev2.LinkAccept{
		GrantId: claims.GrantID, LinkId: linkID, ClientId: clientID, DeviceId: deviceID, RelayNodeId: nodeID, RelayCellId: cellID,
		TargetConnectionEpoch: 1, DeviceIdentityKeyVersion: 4, ClientEphemeralPublicKey: bytes.Repeat([]byte{0x61}, 32), DeviceEphemeralPublicKey: bytes.Repeat([]byte{0x64}, 32),
		ClientChallenge: bytes.Repeat([]byte{0x62}, 32), DeviceChallenge: bytes.Repeat([]byte{0x65}, 32), IdentitySignature: bytes.Repeat([]byte{0x66}, ed25519.SignatureSize),
	}}}
	if err := writeV2Envelope(context.Background(), device, &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: deviceCarrierID, CarrierEpoch: 1, PacketSequence: 2, AcknowledgedSequence: 1,
		Body: &remotev2.CarrierEnvelope_Link{Link: accept},
	}); err != nil {
		t.Fatal(err)
	}
	forwardedAccept := readV2Envelope(t, client)
	if forwardedAccept.GetCarrierId() != clientCarrierID || forwardedAccept.GetPacketSequence() != 2 || forwardedAccept.GetLink() == nil || forwardedAccept.GetLink().GetLinkAccept().GetLinkId() != linkID {
		t.Fatalf("forwarded accept = %+v", forwardedAccept)
	}

	// An Agent can reject only an expired Link after Carrier resume. The Relay
	// must forward this transport-only feedback to that Link's controller,
	// without treating it as a connection-wide protocol fault.
	if err := writeV2Envelope(context.Background(), device, &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: deviceCarrierID, CarrierEpoch: 1, PacketSequence: 3, AcknowledgedSequence: 2,
		Body: &remotev2.CarrierEnvelope_StreamRejected{StreamRejected: &remotev2.CarrierStreamRejected{
			LinkId: linkID, Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if rejected := readV2Envelope(t, client).GetStreamRejected(); rejected == nil || rejected.GetLinkId() != linkID || rejected.GetReason() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED {
		t.Fatalf("forwarded expired-link feedback = %+v", rejected)
	}

	// A second carrier presenting the observed grant cannot complete hello.
	replayed, _, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{Subprotocols: []string{V2Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer replayed.CloseNow()
	replayCarrierID := uuid.NewString()
	replayProof, err := remoteauth.SignCarrierProof(clientPrivate, remoteauth.CarrierProof{GrantID: claims.GrantID, CarrierID: replayCarrierID, CarrierEpoch: 1, Challenge: challenge})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeV2Envelope(context.Background(), replayed, &remotev2.CarrierEnvelope{ProtocolMajor: 2, CarrierId: replayCarrierID, CarrierEpoch: 1, PacketSequence: 1, Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
		Grant: grant, GrantId: claims.GrantID, ClientId: clientID, ClientIdentityKeyVersion: 3, ClientChallenge: challenge, ClientProof: replayProof,
	}}}); err != nil {
		t.Fatal(err)
	}
	_, _, replayErr := replayed.Read(context.Background())
	if replayErr == nil {
		t.Fatal("replayed grant carrier was accepted")
	}
}

func TestV2CarrierRejectsV1Subprotocol(t *testing.T) {
	handler := &V2Handler{CellID: "cell", NodeID: "node", ClientGrantVerifier: remoteauth.DeviceLinkGrantVerifier{Keys: map[string]ed25519.PublicKey{"key": ed25519.PublicKey(bytes.Repeat([]byte{0x01}, 32))}}, DeviceAuthenticator: v2TestDeviceAuthenticator{}, BrowserOriginPatterns: []string{"*"}}
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v2/connect"
	connection, response, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{Subprotocols: []string{Subprotocol}})
	if connection != nil {
		connection.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("v1 subprotocol dial = connection=%v response=%v err=%v", connection != nil, response, err)
	}
}

func TestV2CarrierConnectsOverWSS(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	issuerPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x71}, ed25519.SeedSize))
	clientPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x72}, ed25519.SeedSize))
	deviceIdentity := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x73}, ed25519.SeedSize))
	clientID, deviceID, userID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	nodeID, cellID := uuid.NewString(), uuid.NewString()
	claims := remoteauth.DeviceLinkGrantClaims{
		Audience: remoteauth.DeviceLinkGrantAudience, GrantID: uuid.NewString(), ClientID: clientID, DeviceID: deviceID,
		RelayNodeID: nodeID, RelayCellID: cellID, TargetConnectionEpoch: 1,
		ClientIdentityKey:   base64.RawURLEncoding.EncodeToString(clientPrivate.Public().(ed25519.PublicKey)),
		ClientKeyThumbprint: remoteauth.PublicKeyThumbprint(clientPrivate.Public().(ed25519.PublicKey)), ClientIdentityKeyVersion: 1,
		DeviceKeyThumbprint: remoteauth.PublicKeyThumbprint(deviceIdentity.Public().(ed25519.PublicKey)), DeviceIdentityKeyVersion: 1,
		ClientGrantVersion: 1, DeviceGrantVersion: 1, AllowedScopes: []string{"remote.peer.query"}, MaximumLifetimeSeconds: 90,
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(90 * time.Second).Unix(),
	}
	grant, err := (remoteauth.DeviceLinkGrantIssuer{Issuer: "control", KeyID: "v2", PrivateKey: issuerPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	handler := &V2Handler{
		CellID: cellID, NodeID: nodeID,
		ClientGrantVerifier: remoteauth.DeviceLinkGrantVerifier{Issuer: "control", Keys: map[string]ed25519.PublicKey{"v2": issuerPrivate.Public().(ed25519.PublicKey)}},
		DeviceAuthenticator: v2TestDeviceAuthenticator{
			deviceID: deviceID, userID: userID, epoch: 1, assignmentVersion: 1, grantVersion: 1,
		},
		GrantUses:             NewInMemoryV2GrantUseStore(),
		BrowserOriginPatterns: []string{"*"},
		Now:                   func() time.Time { return now },
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https") + "/v2/connect"
	dialOptions := &websocket.DialOptions{Subprotocols: []string{V2Subprotocol}, HTTPClient: server.Client()}

	deviceCarrierID := uuid.NewString()
	device, _, err := websocket.Dial(context.Background(), endpoint, dialOptions)
	if err != nil {
		t.Fatalf("device WSS Dial() error = %v", err)
	}
	defer device.CloseNow()
	if err := writeV2Envelope(context.Background(), device, &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: deviceCarrierID, CarrierEpoch: 1, PacketSequence: 1,
		Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
			DeviceConnectionTicket: "device-ticket", DeviceId: deviceID, DeviceConnectionEpoch: 1,
			ClientChallenge: bytes.Repeat([]byte{0x01}, 32), DeviceProof: bytes.Repeat([]byte{0x02}, ed25519.SignatureSize),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if ready := readV2Envelope(t, device); ready.GetReady() == nil || ready.GetCarrierId() != deviceCarrierID {
		t.Fatalf("device WSS ready = %+v", ready)
	}

	clientCarrierID := uuid.NewString()
	client, _, err := websocket.Dial(context.Background(), endpoint, dialOptions)
	if err != nil {
		t.Fatalf("client WSS Dial() error = %v", err)
	}
	defer client.CloseNow()
	challenge := bytes.Repeat([]byte{0x03}, 32)
	proof, err := remoteauth.SignCarrierProof(clientPrivate, remoteauth.CarrierProof{
		GrantID: claims.GrantID, CarrierID: clientCarrierID, CarrierEpoch: 1, Challenge: challenge,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeV2Envelope(context.Background(), client, &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: clientCarrierID, CarrierEpoch: 1, PacketSequence: 1,
		Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
			Grant: grant, GrantId: claims.GrantID, ClientId: clientID, ClientIdentityKeyVersion: 1, ClientChallenge: challenge, ClientProof: proof,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if ready := readV2Envelope(t, client); ready.GetReady() == nil || ready.GetCarrierId() != clientCarrierID {
		t.Fatalf("client WSS ready = %+v", ready)
	}
}

func TestInMemoryV2GrantUseStoreRejectsRevokedGrant(t *testing.T) {
	store := NewInMemoryV2GrantUseStore()
	expiresAt := time.Now().UTC().Add(time.Minute)
	if err := store.RevokeDeviceLinkGrant("grant-revocation-test", expiresAt); err != nil {
		t.Fatalf("RevokeDeviceLinkGrant() error = %v", err)
	}
	consumed, err := store.ConsumeDeviceLinkGrant("grant-revocation-test", expiresAt)
	if err != nil || consumed {
		t.Fatalf("ConsumeDeviceLinkGrant() = %v, %v; want false, nil", consumed, err)
	}
}

func TestInMemoryV2GrantUseStoreReusesPersistentProofBoundGrant(t *testing.T) {
	store := NewInMemoryV2GrantUseStore()
	expiresAt := remoteauth.PersistentDeviceLinkGrantExpiry()
	for attempt := range 3 {
		accepted, err := store.ConsumeDeviceLinkGrant("persistent-grant", expiresAt)
		if err != nil || !accepted {
			t.Fatalf("persistent use %d = %v, %v; want true, nil", attempt, accepted, err)
		}
	}
	if err := store.RevokeDeviceLinkGrant("persistent-grant", expiresAt); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.ConsumeDeviceLinkGrant("persistent-grant", expiresAt)
	if err != nil || accepted {
		t.Fatalf("revoked persistent use = %v, %v; want false, nil", accepted, err)
	}
}

func TestV2HandlerRechecksPersistentGrantRevocationForNewLink(t *testing.T) {
	store := NewInMemoryV2GrantUseStore()
	claims := remoteauth.DeviceLinkGrantClaims{
		GrantID: "persistent-link-grant", MaximumLifetimeSeconds: 0,
		ExpiresAt: remoteauth.PersistentDeviceLinkGrantExpiresAtUnix,
	}
	handler := &V2Handler{GrantUses: store}
	if got := handler.persistentGrantRejection(claims); got != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		t.Fatalf("usable persistent Grant rejection = %v", got)
	}
	if err := store.RevokeDeviceLinkGrant(claims.GrantID, remoteauth.PersistentDeviceLinkGrantExpiry()); err != nil {
		t.Fatal(err)
	}
	if got := handler.persistentGrantRejection(claims); got != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_REVOKED {
		t.Fatalf("revoked persistent Grant rejection = %v, want REVOKED", got)
	}
}

func writeV2Envelope(ctx context.Context, connection *websocket.Conn, envelope *remotev2.CarrierEnvelope) error {
	payload, err := proto.Marshal(envelope)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageBinary, payload)
}

func readV2Envelope(t *testing.T, connection *websocket.Conn) *remotev2.CarrierEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if messageType != websocket.MessageBinary {
		t.Fatalf("message type = %v", messageType)
	}
	envelope := new(remotev2.CarrierEnvelope)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return envelope
}
