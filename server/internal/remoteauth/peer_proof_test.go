package remoteauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
)

func TestPeerIdentityProofsBindTicketDevicesAndBothEphemeralKeys(t *testing.T) {
	sourcePublic, sourcePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	targetPublic, targetPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sourceEphemeral := make([]byte, 32)
	targetEphemeral := make([]byte, 32)
	for index := range sourceEphemeral {
		sourceEphemeral[index] = byte(index + 1)
		targetEphemeral[index] = byte(index + 33)
	}
	open := PeerOpenIdentityProof{
		TicketJWTID: "ticket-1", SessionID: "session-1", SourceDeviceID: "source-1", TargetDeviceID: "target-1",
		EphemeralPublicKey: sourceEphemeral,
	}
	openSignature, err := SignPeerOpenIdentity(sourcePrivate, open)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPeerOpenIdentity(sourcePublic, PublicKeyThumbprint(sourcePublic), open, openSignature); err != nil {
		t.Fatal(err)
	}
	tamperedOpen := open
	tamperedOpen.TargetDeviceID = "target-2"
	if err := VerifyPeerOpenIdentity(sourcePublic, PublicKeyThumbprint(sourcePublic), tamperedOpen, openSignature); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("tampered PEER_OPEN proof error = %v", err)
	}

	ready := PeerReadyIdentityProof{
		TicketJWTID: "ticket-1", SessionID: "session-1", SourceDeviceID: "source-1", TargetDeviceID: "target-1",
		SourceEphemeralPublicKey: sourceEphemeral, TargetEphemeralPublicKey: targetEphemeral,
	}
	readySignature, err := SignPeerReadyIdentity(targetPrivate, ready)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPeerReadyIdentity(targetPublic, PublicKeyThumbprint(targetPublic), ready, readySignature); err != nil {
		t.Fatal(err)
	}
	tamperedReady := ready
	tamperedReady.SourceEphemeralPublicKey = append([]byte(nil), sourceEphemeral...)
	tamperedReady.SourceEphemeralPublicKey[0] ^= 0xff
	if err := VerifyPeerReadyIdentity(targetPublic, PublicKeyThumbprint(targetPublic), tamperedReady, readySignature); !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("tampered PEER_READY proof error = %v", err)
	}
}
