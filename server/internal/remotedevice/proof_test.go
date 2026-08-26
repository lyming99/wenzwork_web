package remotedevice

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRegistrationProofBindsSessionDeviceAndKey(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, deviceID := uuid.New(), uuid.New()
	proof, err := SignRegistration(privateKey, sessionID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRegistration(publicKey, sessionID, deviceID, proof); err != nil {
		t.Fatalf("VerifyRegistration() error = %v", err)
	}
	for name, verify := range map[string]func() error{
		"session": func() error { return VerifyRegistration(publicKey, uuid.New(), deviceID, proof) },
		"device":  func() error { return VerifyRegistration(publicKey, sessionID, uuid.New(), proof) },
		"key": func() error {
			other, _, _ := ed25519.GenerateKey(rand.Reader)
			return VerifyRegistration(other, sessionID, deviceID, proof)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verify(); !errors.Is(err, ErrRegistrationProof) {
				t.Fatalf("error = %v, want ErrRegistrationProof", err)
			}
		})
	}
}

func TestRegistrationTranscriptHasNoTrailingNewline(t *testing.T) {
	publicKey, _, _ := ed25519.GenerateKey(rand.Reader)
	transcript, err := RegistrationTranscript(uuid.MustParse("11111111-1111-4111-8111-111111111111"), uuid.MustParse("22222222-2222-4222-8222-222222222222"), publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if transcript[len(transcript)-1] == '\n' {
		t.Fatal("registration transcript has a trailing newline")
	}
}
