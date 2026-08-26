package remoteauth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	DeviceLinkGrantAudience = "relay-device-link-v2"
	DeviceLinkGrantType     = "WenzWorkDeviceLinkGrantV2"
	deviceLinkGrantMaxBytes = 8 << 10

	// PersistentDeviceLinkGrantExpiresAtUnix is the protobuf/RFC 3339 maximum
	// timestamp.  A Grant with this exact exp and a zero maximum lifetime is a
	// reusable, proof-of-possession credential: it has no operational renewal
	// deadline and remains valid until explicit revocation or an identity/grant
	// version change.  Keeping an exp value preserves wire compatibility with
	// clients that predate persistent Grant semantics.
	PersistentDeviceLinkGrantExpiresAtUnix int64 = 253402300799
)

var ErrDeviceLinkGrant = errors.New("device link grant is invalid")

// DeviceLinkGrantClaims is intentionally independent from v1 Claims. It
// cannot be accepted by a v1 Peer endpoint and it contains no project scope.
type DeviceLinkGrantClaims struct {
	Issuer                   string `json:"iss"`
	Audience                 string `json:"aud"`
	GrantID                  string `json:"grant_id"`
	ClientID                 string `json:"client_id"`
	DeviceID                 string `json:"device_id"`
	RelayNodeID              string `json:"relay_node_id"`
	RelayCellID              string `json:"relay_cell_id"`
	TargetConnectionEpoch    uint64 `json:"target_connection_epoch"`
	ClientIdentityKey        string `json:"client_identity_key"`
	ClientKeyThumbprint      string `json:"client_key_thumbprint"`
	ClientIdentityKeyVersion uint64 `json:"client_identity_key_version"`
	DeviceKeyThumbprint      string `json:"device_key_thumbprint"`
	DeviceIdentityKeyVersion uint64 `json:"device_identity_key_version"`
	ClientGrantVersion       uint64 `json:"client_grant_version"`
	DeviceGrantVersion       uint64 `json:"device_grant_version"`
	// AllowedScopes is the device-enforced authorization upper bound. It is
	// deliberately device-scoped (no project IDs); project binding remains in
	// encrypted CHANNEL_OPEN records after the Link is established.
	AllowedScopes          []string `json:"allowed_scopes"`
	MaximumLifetimeSeconds uint32   `json:"maximum_lifetime_seconds"`
	IssuedAt               int64    `json:"iat"`
	NotBefore              int64    `json:"nbf"`
	ExpiresAt              int64    `json:"exp"`
}

func (claims DeviceLinkGrantClaims) Validate(expectedIssuer string, now time.Time, leeway time.Duration) error {
	persistent := claims.Persistent()
	if claims.Issuer == "" || claims.Audience != DeviceLinkGrantAudience || claims.GrantID == "" || claims.ClientID == "" ||
		claims.DeviceID == "" || claims.ClientID == claims.DeviceID || claims.RelayNodeID == "" || claims.RelayCellID == "" ||
		claims.TargetConnectionEpoch == 0 || claims.ClientIdentityKeyVersion == 0 || claims.DeviceIdentityKeyVersion == 0 ||
		claims.ClientGrantVersion == 0 || claims.DeviceGrantVersion == 0 || !validDeviceLinkScopes(claims.AllowedScopes) ||
		claims.IssuedAt <= 0 || claims.NotBefore <= 0 || claims.ExpiresAt <= claims.IssuedAt ||
		(expectedIssuer != "" && claims.Issuer != expectedIssuer) {
		return ErrDeviceLinkGrant
	}
	if !persistent && (claims.MaximumLifetimeSeconds == 0 || claims.MaximumLifetimeSeconds > 15*60 || claims.ExpiresAt-claims.IssuedAt > int64(claims.MaximumLifetimeSeconds)+5) {
		return ErrDeviceLinkGrant
	}
	if now.Add(leeway).Unix() < claims.NotBefore || (!persistent && now.Add(-leeway).Unix() >= claims.ExpiresAt) {
		return ErrDeviceLinkGrant
	}
	clientKey, err := DecodeIdentityPublicKey(claims.ClientIdentityKey, claims.ClientKeyThumbprint)
	if err != nil || len(clientKey) != ed25519.PublicKeySize || len(claims.ClientKeyThumbprint) != 43 || len(claims.DeviceKeyThumbprint) != 43 {
		return ErrDeviceLinkGrant
	}
	return nil
}

// Persistent reports the append-only wire representation of a Grant that is
// reusable until it is explicitly revoked.  Both fields are required so an
// arbitrary far-future exp cannot silently bypass the bounded legacy rules.
func (claims DeviceLinkGrantClaims) Persistent() bool {
	return claims.MaximumLifetimeSeconds == 0 && claims.ExpiresAt == PersistentDeviceLinkGrantExpiresAtUnix
}

func PersistentDeviceLinkGrantExpiry() time.Time {
	return time.Unix(PersistentDeviceLinkGrantExpiresAtUnix, 0).UTC()
}

func IsPersistentDeviceLinkGrantExpiry(value time.Time) bool {
	return !value.IsZero() && value.UTC().Unix() == PersistentDeviceLinkGrantExpiresAtUnix
}

func validDeviceLinkScopes(scopes []string) bool {
	if len(scopes) == 0 || len(scopes) > 16 {
		return false
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if !strings.HasPrefix(scope, "remote.peer.") || len(scope) > 80 || strings.TrimSpace(scope) != scope || strings.ContainsAny(scope, "\r\n\x00") {
			return false
		}
		if _, duplicate := seen[scope]; duplicate {
			return false
		}
		seen[scope] = struct{}{}
	}
	return true
}

type DeviceLinkGrantIssuer struct {
	Issuer     string
	KeyID      string
	PrivateKey ed25519.PrivateKey
}

func (issuer DeviceLinkGrantIssuer) Sign(claims DeviceLinkGrantClaims) (string, error) {
	if issuer.Issuer == "" || issuer.KeyID == "" || len(issuer.PrivateKey) != ed25519.PrivateKeySize {
		return "", ErrDeviceLinkGrant
	}
	if claims.Issuer == "" {
		claims.Issuer = issuer.Issuer
	}
	if claims.Issuer != issuer.Issuer || claims.Validate(issuer.Issuer, time.Unix(claims.IssuedAt, 0).UTC(), 0) != nil {
		return "", ErrDeviceLinkGrant
	}
	header, err := json.Marshal(deviceLinkGrantHeader{Algorithm: "EdDSA", KeyID: issuer.KeyID, Type: DeviceLinkGrantType})
	if err != nil {
		return "", ErrDeviceLinkGrant
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", ErrDeviceLinkGrant
	}
	protected := base64.RawURLEncoding.EncodeToString(header)
	encodedClaims := base64.RawURLEncoding.EncodeToString(payload)
	signed := protected + "." + encodedClaims
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(issuer.PrivateKey, []byte(signed))), nil
}

type DeviceLinkGrantVerifier struct {
	Issuer string
	Keys   map[string]ed25519.PublicKey
	Leeway time.Duration
}

func (verifier DeviceLinkGrantVerifier) Verify(grant string, now time.Time) (DeviceLinkGrantClaims, error) {
	if len(grant) == 0 || len(grant) > deviceLinkGrantMaxBytes {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	parts := strings.Split(grant, ".")
	if len(parts) != 3 {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	headerBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	var header deviceLinkGrantHeader
	if decodeStrict(headerBytes, &header) != nil || header.Algorithm != "EdDSA" || header.Type != DeviceLinkGrantType || header.KeyID == "" {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	publicKey := verifier.Keys[header.KeyID]
	if len(publicKey) != ed25519.PublicKeySize {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	var claims DeviceLinkGrantClaims
	if decodeStrict(payload, &claims) != nil || claims.Validate(verifier.Issuer, now.UTC(), verifier.Leeway) != nil {
		return DeviceLinkGrantClaims{}, ErrDeviceLinkGrant
	}
	return claims, nil
}

type deviceLinkGrantHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

// CarrierProof is signed by the Client identity and prevents an observed
// grant from being usable without the private key. It is sent only in the
// first binary CARRIER_HELLO frame, never in a URL or subprotocol.
type CarrierProof struct {
	GrantID      string
	CarrierID    string
	CarrierEpoch uint64
	Challenge    []byte
}

func SignCarrierProof(privateKey ed25519.PrivateKey, proof CarrierProof) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize || !validCarrierProof(proof) {
		return nil, ErrDeviceLinkGrant
	}
	return ed25519.Sign(privateKey, canonicalCarrierProof(proof)), nil
}

func VerifyCarrierProof(publicKey ed25519.PublicKey, proof CarrierProof, signature []byte) error {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || !validCarrierProof(proof) || !ed25519.Verify(publicKey, canonicalCarrierProof(proof), signature) {
		return ErrDeviceLinkGrant
	}
	return nil
}

func canonicalCarrierProof(proof CarrierProof) []byte {
	encoded := appendGrantField(nil, "wenzwork-remote-v2/carrier-proof")
	encoded = appendGrantField(encoded, proof.GrantID)
	encoded = appendGrantField(encoded, proof.CarrierID)
	encoded = binary.BigEndian.AppendUint64(encoded, proof.CarrierEpoch)
	return appendGrantBytes(encoded, proof.Challenge)
}

func validCarrierProof(proof CarrierProof) bool {
	return validGrantField(proof.GrantID) && validGrantField(proof.CarrierID) && proof.CarrierEpoch > 0 && len(proof.Challenge) == 32
}

func validGrantField(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 256 && !strings.ContainsRune(value, '\x00')
}

func appendGrantField(destination []byte, value string) []byte {
	return appendGrantBytes(destination, []byte(value))
}

func appendGrantBytes(destination, value []byte) []byte {
	destination = binary.BigEndian.AppendUint32(destination, uint32(len(value)))
	return append(destination, value...)
}

// GrantFingerprint produces a safe, short diagnostic correlation value. It
// must never be used as a bearer credential or a stable user identifier.
func GrantFingerprint(grant string) string {
	if grant == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(grant))
	return base64.RawURLEncoding.EncodeToString(digest[:12])
}
