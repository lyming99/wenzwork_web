package relayidentity

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
)

func TestEndpointAttestationBindsNonceAndNodeIdentity(t *testing.T) {
	publicKey, privateKey, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	attestation, err := SignEndpointAttestation(privateKey, EndpointAttestation{
		SchemaVersion: 1, Nonce: base64.RawURLEncoding.EncodeToString(nonce),
		InstallationID: uuid.New(), CellID: uuid.New(), InstanceID: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEndpointAttestation(publicKey, attestation); err != nil {
		t.Fatalf("VerifyEndpointAttestation() error = %v", err)
	}
	attestation.CellID = uuid.New()
	if err := VerifyEndpointAttestation(publicKey, attestation); err == nil {
		t.Fatal("VerifyEndpointAttestation() accepted a changed Cell")
	}
}
