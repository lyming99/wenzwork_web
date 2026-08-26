package relayidentity

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/google/uuid"
)

const endpointAttestationContext = "wenzwork-relay-endpoint-attestation-v1"

type EndpointAttestation struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Nonce          string    `json:"nonce"`
	InstallationID uuid.UUID `json:"installationId"`
	CellID         uuid.UUID `json:"cellId"`
	InstanceID     uuid.UUID `json:"instanceId"`
	Signature      string    `json:"signature"`
}

func SignEndpointAttestation(privateKey ed25519.PrivateKey, attestation EndpointAttestation) (EndpointAttestation, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return EndpointAttestation{}, errors.New("invalid Relay endpoint attestation private key")
	}
	message, err := endpointAttestationMessage(attestation)
	if err != nil {
		return EndpointAttestation{}, err
	}
	attestation.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message))
	return attestation, nil
}

func VerifyEndpointAttestation(publicKey ed25519.PublicKey, attestation EndpointAttestation) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return errors.New("invalid Relay endpoint attestation public key")
	}
	message, err := endpointAttestationMessage(attestation)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(attestation.Signature))
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, message, signature) {
		return errors.New("invalid Relay endpoint attestation signature")
	}
	return nil
}

func endpointAttestationMessage(attestation EndpointAttestation) ([]byte, error) {
	nonce := strings.TrimSpace(attestation.Nonce)
	decodedNonce, err := base64.RawURLEncoding.DecodeString(nonce)
	if attestation.SchemaVersion != 1 || err != nil || len(decodedNonce) != 32 || attestation.InstallationID == uuid.Nil ||
		attestation.CellID == uuid.Nil || attestation.InstanceID == uuid.Nil {
		return nil, errors.New("invalid Relay endpoint attestation fields")
	}
	return []byte(strings.Join([]string{
		endpointAttestationContext, nonce, attestation.InstallationID.String(),
		attestation.CellID.String(), attestation.InstanceID.String(),
	}, "\n")), nil
}
