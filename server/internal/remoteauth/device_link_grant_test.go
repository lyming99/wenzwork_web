package remoteauth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func validDeviceLinkGrantClaims(now time.Time, clientKey ed25519.PublicKey) DeviceLinkGrantClaims {
	deviceKey := ed25519.PublicKey(bytes.Repeat([]byte{0x42}, ed25519.PublicKeySize))
	return DeviceLinkGrantClaims{
		Audience:                 DeviceLinkGrantAudience,
		GrantID:                  "grant-v2",
		ClientID:                 "controller-v2",
		DeviceID:                 "device-v2",
		RelayNodeID:              "node-v2",
		RelayCellID:              "cell-v2",
		TargetConnectionEpoch:    7,
		ClientIdentityKey:        base64Raw(clientKey),
		ClientKeyThumbprint:      PublicKeyThumbprint(clientKey),
		ClientIdentityKeyVersion: 2,
		DeviceKeyThumbprint:      PublicKeyThumbprint(deviceKey),
		DeviceIdentityKeyVersion: 3,
		ClientGrantVersion:       4,
		DeviceGrantVersion:       5,
		AllowedScopes:            []string{"remote.peer.query"},
		MaximumLifetimeSeconds:   90,
		IssuedAt:                 now.Unix(),
		NotBefore:                now.Add(-time.Second).Unix(),
		ExpiresAt:                now.Add(90 * time.Second).Unix(),
	}
}

func TestDeviceLinkGrantSeparatesV2ClaimsAndRequiresCarrierProof(t *testing.T) {
	now := time.Date(2026, time.August, 17, 0, 0, 0, 0, time.UTC)
	issuerPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x10}, ed25519.SeedSize))
	clientPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x11}, ed25519.SeedSize))
	issuer := DeviceLinkGrantIssuer{Issuer: "control-v2", KeyID: "device-link-v2", PrivateKey: issuerPrivate}
	grant, err := issuer.Sign(validDeviceLinkGrantClaims(now, clientPrivate.Public().(ed25519.PublicKey)))
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	verifier := DeviceLinkGrantVerifier{Issuer: "control-v2", Keys: map[string]ed25519.PublicKey{"device-link-v2": issuerPrivate.Public().(ed25519.PublicKey)}}
	claims, err := verifier.Verify(grant, now.Add(time.Second))
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if claims.GrantID != "grant-v2" || claims.ClientID != "controller-v2" || claims.DeviceID != "device-v2" {
		t.Fatalf("claims = %+v", claims)
	}
	proof := CarrierProof{GrantID: claims.GrantID, CarrierID: "carrier-v2", CarrierEpoch: 1, Challenge: bytes.Repeat([]byte{0x22}, 32)}
	signature, err := SignCarrierProof(clientPrivate, proof)
	if err != nil {
		t.Fatalf("SignCarrierProof() error = %v", err)
	}
	if err := VerifyCarrierProof(clientPrivate.Public().(ed25519.PublicKey), proof, signature); err != nil {
		t.Fatalf("VerifyCarrierProof() error = %v", err)
	}
	proof.CarrierEpoch++
	if err := VerifyCarrierProof(clientPrivate.Public().(ed25519.PublicKey), proof, signature); !errors.Is(err, ErrDeviceLinkGrant) {
		t.Fatalf("VerifyCarrierProof(altered) error = %v, want ErrDeviceLinkGrant", err)
	}
	if _, err := verifier.Verify(grant, now.Add(2*time.Minute)); !errors.Is(err, ErrDeviceLinkGrant) {
		t.Fatalf("expired Verify() error = %v, want ErrDeviceLinkGrant", err)
	}
}

func TestDeviceLinkGrantRejectsV1TokenTypeAndProjectFieldsAreAbsent(t *testing.T) {
	now := time.Date(2026, time.August, 17, 0, 0, 0, 0, time.UTC)
	issuerPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x31}, ed25519.SeedSize))
	clientPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x32}, ed25519.SeedSize))
	claims := validDeviceLinkGrantClaims(now, clientPrivate.Public().(ed25519.PublicKey))
	legacy, err := (Issuer{Issuer: "control-v2", KeyID: "device-link-v2", PrivateKey: issuerPrivate}).Sign(Claims{
		Audience: "relay-peer", JWTID: "legacy", IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier := DeviceLinkGrantVerifier{Issuer: "control-v2", Keys: map[string]ed25519.PublicKey{"device-link-v2": issuerPrivate.Public().(ed25519.PublicKey)}}
	if _, err := verifier.Verify(legacy, now); !errors.Is(err, ErrDeviceLinkGrant) {
		t.Fatalf("v1 token Verify() error = %v, want ErrDeviceLinkGrant", err)
	}
	claims.MaximumLifetimeSeconds = 16 * 60
	if _, err := (DeviceLinkGrantIssuer{Issuer: "control-v2", KeyID: "device-link-v2", PrivateKey: issuerPrivate}).Sign(claims); !errors.Is(err, ErrDeviceLinkGrant) {
		t.Fatalf("oversized grant Sign() error = %v, want ErrDeviceLinkGrant", err)
	}
}

func TestPersistentDeviceLinkGrantHasNoTimeBasedRenewalDeadline(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	issuerPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x41}, ed25519.SeedSize))
	clientPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	claims := validDeviceLinkGrantClaims(now, clientPrivate.Public().(ed25519.PublicKey))
	claims.MaximumLifetimeSeconds = 0
	claims.ExpiresAt = PersistentDeviceLinkGrantExpiresAtUnix
	issuer := DeviceLinkGrantIssuer{Issuer: "control-v2", KeyID: "persistent-key", PrivateKey: issuerPrivate}
	grant, err := issuer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	verifier := DeviceLinkGrantVerifier{Issuer: "control-v2", Keys: map[string]ed25519.PublicKey{"persistent-key": issuerPrivate.Public().(ed25519.PublicKey)}}
	verified, err := verifier.Verify(grant, now.AddDate(50, 0, 0))
	if err != nil || !verified.Persistent() || !IsPersistentDeviceLinkGrantExpiry(time.Unix(verified.ExpiresAt, 0)) {
		t.Fatalf("persistent Verify() = %+v, %v", verified, err)
	}

	claims.ExpiresAt = now.Add(time.Hour).Unix()
	if _, err := issuer.Sign(claims); !errors.Is(err, ErrDeviceLinkGrant) {
		t.Fatalf("zero-lifetime non-sentinel Sign() error = %v, want ErrDeviceLinkGrant", err)
	}
}

func base64Raw(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}
