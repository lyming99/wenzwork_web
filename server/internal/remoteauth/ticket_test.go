package remoteauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func testTicketKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, privateKey
}

func TestTicketAndDeviceProofBindIdentityCellAndVersions(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	signerPublic, signerPrivate := testTicketKeys(t)
	devicePublic, devicePrivate := testTicketKeys(t)
	claims := Claims{
		Audience: "relay", Subject: "device-1", UserID: "user-1", AssignmentID: "assignment-1",
		AssignmentVersion: 7, AllowedCellIDs: []string{"r017", "r018"}, GrantVersion: 3,
		Scopes: []string{"remote.connect", "remote.task.read"}, ProtocolMin: 1, ProtocolMax: 1,
		Confirmation: PublicKeyThumbprint(devicePublic), IdentityKey: base64.RawURLEncoding.EncodeToString(devicePublic), JWTID: "ticket-1",
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	}
	token, err := (Issuer{Issuer: "wenzwork-control", KeyID: "ticket-key-1", PrivateKey: signerPrivate}).Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := (Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"ticket-key-1": signerPublic}}).Verify(token, "relay", now)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Subject != "device-1" || verified.AssignmentVersion != 7 || !verified.HasScope("remote.task.read") {
		t.Fatalf("claims = %+v", verified)
	}
	if err := verified.ValidateConnection("device-1", "user-1", "r017", PublicKeyThumbprint(devicePublic), 7, 3, 1); err != nil {
		t.Fatal(err)
	}
	if err := verified.ValidateConnection("device-1", "user-1", "r999", PublicKeyThumbprint(devicePublic), 7, 3, 1); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("wrong Cell error = %v", err)
	}
	if err := verified.ValidateConnection("device-1", "user-1", "r017", PublicKeyThumbprint(devicePublic), 8, 3, 1); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("stale assignment error = %v", err)
	}
	withoutConnect := verified
	withoutConnect.Scopes = []string{"remote.task.read"}
	if err := withoutConnect.ValidateConnection("device-1", "user-1", "r017", PublicKeyThumbprint(devicePublic), 7, 3, 1); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("missing remote.connect error = %v", err)
	}
	challenge, err := NewChallenge("ticket-1", "r017", "r017-node-0", 11, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignChallenge(devicePrivate, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChallenge(devicePublic, verified.Confirmation, challenge, proof, now); err != nil {
		t.Fatal(err)
	}

	otherPublic, _ := testTicketKeys(t)
	if err := VerifyChallenge(otherPublic, verified.Confirmation, challenge, proof, now); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("copied proof error = %v", err)
	}
	challenge.CellID = "r999"
	if err := VerifyChallenge(devicePublic, verified.Confirmation, challenge, proof, now); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("modified challenge error = %v", err)
	}
}

func TestTicketRejectsTamperWrongAudienceAndExpiry(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	publicKey, privateKey := testTicketKeys(t)
	issuer := Issuer{Issuer: "wenzwork-control", KeyID: "key-1", PrivateKey: privateKey}
	claims := Claims{Audience: "relay", JWTID: "ticket-1", IssuedAt: now.Add(-time.Minute).Unix(), NotBefore: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	token, err := issuer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	verifier := Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"key-1": publicKey}}
	if _, err := verifier.Verify(token, "relay-file", now); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("wrong audience error = %v", err)
	}
	if _, err := verifier.Verify(token, "relay", now.Add(2*time.Minute)); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("expired error = %v", err)
	}
	parts := strings.Split(token, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "A"
	if _, err := verifier.Verify(strings.Join(parts, "."), "relay", now); !errors.Is(err, ErrTicketSignature) {
		t.Fatalf("tampered error = %v", err)
	}
}

func TestPeerAndFileTicketsFenceBothGrantVersions(t *testing.T) {
	sourceKey, _ := testTicketKeys(t)
	targetKey, _ := testTicketKeys(t)
	peer := Claims{
		Audience: "relay-peer", Subject: "source-1", Confirmation: PublicKeyThumbprint(sourceKey),
		SessionID: "session-1", SourceDeviceID: "source-1", TargetDeviceID: "target-1",
		SourceGrantVersion: 4, TargetGrantVersion: 9, SourceKeyThumbprint: PublicKeyThumbprint(sourceKey), TargetKeyThumbprint: PublicKeyThumbprint(targetKey),
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourceKey), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetKey),
		SourceKeyVersion: 2, TargetKeyVersion: 3, SourceCredentialType: "device",
		RelayNodeID: "node-1", RelayCellID: "cell-1", TargetConnectionEpoch: 12,
		Scopes: []string{"remote.peer.rpc"}, MaxDurationSeconds: 60, MaxBytes: 1 << 20,
	}
	if err := peer.ValidatePeer("source-1", "target-1", PublicKeyThumbprint(sourceKey), PublicKeyThumbprint(targetKey), "remote.peer.rpc", 4, 9); err != nil {
		t.Fatal(err)
	}
	if err := peer.ValidatePeer("source-1", "target-1", PublicKeyThumbprint(sourceKey), PublicKeyThumbprint(targetKey), "remote.peer.rpc", 4, 10); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("stale peer target grant error = %v", err)
	}
	if err := peer.ValidatePeerRelay("node-1", "cell-1", 12); err != nil {
		t.Fatal(err)
	}
	if err := peer.ValidatePeerRelay("node-2", "cell-1", 12); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("wrong direct Relay error = %v", err)
	}
	if err := peer.ValidatePeerRelay("node-1", "cell-1", 13); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("stale target connection error = %v", err)
	}

	file := Claims{
		Audience: "relay-file", Subject: "source-1", Confirmation: PublicKeyThumbprint(sourceKey),
		TransferID: "transfer-1", SourceDeviceID: "source-1", TargetDeviceID: "target-1",
		SourceGrantVersion: 4, TargetGrantVersion: 9,
		SourceKeyThumbprint: PublicKeyThumbprint(sourceKey), TargetKeyThumbprint: PublicKeyThumbprint(targetKey),
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(sourceKey), TargetIdentityKey: base64.RawURLEncoding.EncodeToString(targetKey),
		SourceKeyVersion: 2, TargetKeyVersion: 3, SourceCredentialType: "device",
		RelayNodeID: "node-1", RelayCellID: "cell-1", TargetConnectionEpoch: 12,
		Direction: "push", Scopes: []string{"remote.peer.file.send", "remote.peer.file.receive"},
		MaxDurationSeconds: 60, MaxBytes: 1 << 20, MaxFileCount: 2, MaxManifestBytes: 16 << 10, AllowedChunkSize: 64 << 10,
	}
	if err := file.ValidateFile("source-1", "target-1", PublicKeyThumbprint(sourceKey), PublicKeyThumbprint(targetKey), 4, 9); err != nil {
		t.Fatal(err)
	}
	if err := file.ValidateFile("source-1", "target-1", PublicKeyThumbprint(sourceKey), PublicKeyThumbprint(targetKey), 0, 9); !errors.Is(err, ErrTicketClaims) {
		t.Fatalf("zero file source grant error = %v", err)
	}
	if err := file.ValidateFileRelay("node-1", "cell-1", 12); err != nil {
		t.Fatal(err)
	}
}

func TestChallengeExpires(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	publicKey, privateKey := testTicketKeys(t)
	challenge, err := NewChallenge("ticket-1", "r017", "node-1", 1, now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignChallenge(privateKey, challenge)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChallenge(publicKey, PublicKeyThumbprint(publicKey), challenge, proof, now.Add(time.Second)); !errors.Is(err, ErrProofExpired) {
		t.Fatalf("VerifyChallenge() error = %v", err)
	}
}
