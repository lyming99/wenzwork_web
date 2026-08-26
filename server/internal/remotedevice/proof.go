package remotedevice

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/google/uuid"
)

var ErrRegistrationProof = errors.New("remote device registration proof is invalid")

const registrationDomain = "wenzwork-device-registration-v1"

// RegistrationTranscript is deliberately line-oriented and stable across
// implementations. The final public-key line has no trailing newline.
func RegistrationTranscript(sessionID, deviceID uuid.UUID, publicKey ed25519.PublicKey) ([]byte, error) {
	if sessionID == uuid.Nil || deviceID == uuid.Nil || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrRegistrationProof
	}
	encodedKey := base64.RawURLEncoding.EncodeToString(publicKey)
	return []byte(strings.Join([]string{registrationDomain, sessionID.String(), deviceID.String(), encodedKey}, "\n")), nil
}

func SignRegistration(privateKey ed25519.PrivateKey, sessionID, deviceID uuid.UUID) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", ErrRegistrationProof
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return "", ErrRegistrationProof
	}
	transcript, err := RegistrationTranscript(sessionID, deviceID, publicKey)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, transcript)), nil
}

func VerifyRegistration(publicKey ed25519.PublicKey, sessionID, deviceID uuid.UUID, encodedProof string) error {
	proof, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encodedProof))
	if err != nil || len(proof) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(proof) != strings.TrimSpace(encodedProof) {
		return ErrRegistrationProof
	}
	transcript, err := RegistrationTranscript(sessionID, deviceID, publicKey)
	if err != nil || !ed25519.Verify(publicKey, transcript, proof) {
		return ErrRegistrationProof
	}
	return nil
}
