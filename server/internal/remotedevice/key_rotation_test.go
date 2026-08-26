package remotedevice

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestKeyRotationRequiresProofFromOldAndNewPrivateKeys(t *testing.T) {
	oldPublic, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, deviceID := uuid.New(), uuid.New()
	oldProof, err := SignKeyRotationProof(oldPrivate, sessionID, deviceID, 7, oldPublic, newPublic)
	if err != nil {
		t.Fatal(err)
	}
	newProof, err := SignKeyRotationProof(newPrivate, sessionID, deviceID, 7, oldPublic, newPublic)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyKeyRotationProofs(oldPublic, newPublic, sessionID, deviceID, 7, oldProof, newProof); err != nil {
		t.Fatal(err)
	}

	otherPublic, otherPrivate, _ := ed25519.GenerateKey(rand.Reader)
	for name, verify := range map[string]func() error{
		"wrong session": func() error {
			return VerifyKeyRotationProofs(oldPublic, newPublic, uuid.New(), deviceID, 7, oldProof, newProof)
		},
		"wrong version": func() error {
			return VerifyKeyRotationProofs(oldPublic, newPublic, sessionID, deviceID, 8, oldProof, newProof)
		},
		"new proof signed by old key": func() error {
			forged, _ := SignKeyRotationProof(oldPrivate, sessionID, deviceID, 7, oldPublic, newPublic)
			return VerifyKeyRotationProofs(oldPublic, newPublic, sessionID, deviceID, 7, oldProof, forged)
		},
		"substituted new key": func() error {
			forged, _ := SignKeyRotationProof(otherPrivate, sessionID, deviceID, 7, oldPublic, otherPublic)
			return VerifyKeyRotationProofs(oldPublic, otherPublic, sessionID, deviceID, 7, oldProof, forged)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verify(); !errors.Is(err, ErrKeyRotationProof) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestKeyRotationTranscriptIsStableAndHasNoTrailingNewline(t *testing.T) {
	oldPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	newPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	transcript, err := KeyRotationTranscript(
		uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		uuid.MustParse("22222222-2222-4222-8222-222222222222"), 3, oldPublic, newPublic,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transcript[len(transcript)-1] == '\n' {
		t.Fatal("key rotation transcript has a trailing newline")
	}
}
