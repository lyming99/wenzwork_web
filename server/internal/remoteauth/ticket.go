package remoteauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

var (
	ErrTicketMalformed = errors.New("ticket is malformed")
	ErrTicketSignature = errors.New("ticket signature is invalid")
	ErrTicketClaims    = errors.New("ticket claims are invalid")
	ErrTicketExpired   = errors.New("ticket is expired")
	ErrTicketNotActive = errors.New("ticket is not active")
)

const maxTicketBytes = 16 << 10

type Claims struct {
	Issuer                string   `json:"iss"`
	Audience              string   `json:"aud"`
	Subject               string   `json:"sub,omitempty"`
	UserID                string   `json:"user_id,omitempty"`
	AssignmentID          string   `json:"assignment_id,omitempty"`
	AssignmentVersion     uint64   `json:"assignment_version,omitempty"`
	AllowedCellIDs        []string `json:"allowed_cell_ids,omitempty"`
	GrantVersion          uint64   `json:"grant_version,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	ProtocolMin           uint32   `json:"protocol_min,omitempty"`
	ProtocolMax           uint32   `json:"protocol_max,omitempty"`
	Confirmation          string   `json:"cnf,omitempty"`
	IdentityKey           string   `json:"identity_key,omitempty"`
	SessionID             string   `json:"session_id,omitempty"`
	TransferID            string   `json:"transfer_id,omitempty"`
	SourceDeviceID        string   `json:"source_device_id,omitempty"`
	TargetDeviceID        string   `json:"target_device_id,omitempty"`
	SourceGrantVersion    uint64   `json:"source_grant_version,omitempty"`
	TargetGrantVersion    uint64   `json:"target_grant_version,omitempty"`
	SourceKeyThumbprint   string   `json:"source_key_thumbprint,omitempty"`
	TargetKeyThumbprint   string   `json:"target_key_thumbprint,omitempty"`
	SourceIdentityKey     string   `json:"source_identity_key,omitempty"`
	TargetIdentityKey     string   `json:"target_identity_key,omitempty"`
	SourceKeyVersion      uint64   `json:"source_key_version,omitempty"`
	TargetKeyVersion      uint64   `json:"target_key_version,omitempty"`
	SourceCredentialType  string   `json:"source_credential_type,omitempty"`
	RelayNodeID           string   `json:"relay_node_id,omitempty"`
	RelayCellID           string   `json:"relay_cell_id,omitempty"`
	TargetConnectionEpoch uint64   `json:"target_connection_epoch,omitempty"`
	Direction             string   `json:"direction,omitempty"`
	ProjectID             string   `json:"project_id,omitempty"`
	MaxDurationSeconds    uint32   `json:"max_duration_seconds,omitempty"`
	MaxBytes              uint64   `json:"max_bytes,omitempty"`
	MaxFileCount          uint32   `json:"max_file_count,omitempty"`
	MaxManifestBytes      uint32   `json:"max_manifest_bytes,omitempty"`
	AllowedChunkSize      uint32   `json:"allowed_chunk_size,omitempty"`
	RequireLocalApproval  bool     `json:"require_local_approval,omitempty"`
	JWTID                 string   `json:"jti"`
	IssuedAt              int64    `json:"iat"`
	NotBefore             int64    `json:"nbf"`
	ExpiresAt             int64    `json:"exp"`
}

func (c Claims) HasScope(scope string) bool {
	return slices.Contains(c.Scopes, scope)
}

func (c Claims) ValidateConnection(deviceID, userID, cellID, keyThumbprint string, assignmentVersion, grantVersion uint64, protocolVersion uint32) error {
	if deviceID == "" || userID == "" || cellID == "" || keyThumbprint == "" || assignmentVersion == 0 || grantVersion == 0 || protocolVersion == 0 ||
		c.Audience != "relay" || c.Subject != deviceID || c.UserID != userID || c.AssignmentID == "" || c.Confirmation != keyThumbprint ||
		c.AssignmentVersion != assignmentVersion || c.GrantVersion != grantVersion ||
		!identityClaimMatches(c.IdentityKey, keyThumbprint) ||
		!c.HasScope("remote.connect") || c.ProtocolMin == 0 || c.ProtocolMax < c.ProtocolMin ||
		!slices.Contains(c.AllowedCellIDs, cellID) || protocolVersion < c.ProtocolMin || protocolVersion > c.ProtocolMax {
		return ErrTicketClaims
	}
	return nil
}

func (c Claims) ValidatePeer(sourceDeviceID, targetDeviceID, sourceThumbprint, targetThumbprint, requiredScope string, sourceGrantVersion, targetGrantVersion uint64) error {
	if sourceDeviceID == "" || targetDeviceID == "" || sourceThumbprint == "" || targetThumbprint == "" || requiredScope == "" ||
		sourceGrantVersion == 0 || targetGrantVersion == 0 || c.Audience != "relay-peer" || c.SessionID == "" ||
		c.SourceDeviceID != sourceDeviceID || c.TargetDeviceID != targetDeviceID || c.SourceGrantVersion != sourceGrantVersion ||
		c.TargetGrantVersion != targetGrantVersion || c.SourceKeyThumbprint != sourceThumbprint ||
		c.TargetKeyThumbprint != targetThumbprint || (c.SourceCredentialType != "device" && c.SourceCredentialType != "controller") ||
		c.SourceKeyVersion == 0 || c.TargetKeyVersion == 0 ||
		!identityClaimMatches(c.SourceIdentityKey, sourceThumbprint) || !identityClaimMatches(c.TargetIdentityKey, targetThumbprint) ||
		!c.HasScope(requiredScope) || c.MaxDurationSeconds == 0 || c.MaxBytes == 0 {
		return ErrTicketClaims
	}
	return nil
}

func identityClaimMatches(encoded, thumbprint string) bool {
	_, err := DecodeIdentityPublicKey(encoded, thumbprint)
	return err == nil
}

func DecodeIdentityPublicKey(encoded, thumbprint string) (ed25519.PublicKey, error) {
	key, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(key) != encoded ||
		PublicKeyThumbprint(ed25519.PublicKey(key)) != thumbprint {
		return nil, ErrTicketClaims
	}
	return ed25519.PublicKey(key), nil
}

// ValidatePeerRelay binds a direct-control ticket to the exact Relay process
// and resident target connection selected by the management service. A source
// client may present this ticket only to that Relay; it cannot be replayed at a
// different node or after the target reconnects with a new connection epoch.
func (c Claims) ValidatePeerRelay(nodeID, cellID string, targetConnectionEpoch uint64) error {
	if nodeID == "" || cellID == "" || targetConnectionEpoch == 0 ||
		c.Audience != "relay-peer" || c.Subject == "" || c.Subject != c.SourceDeviceID ||
		c.Confirmation == "" || c.Confirmation != c.SourceKeyThumbprint ||
		c.RelayNodeID != nodeID || c.RelayCellID != cellID ||
		c.TargetConnectionEpoch != targetConnectionEpoch {
		return ErrTicketClaims
	}
	return nil
}

func (c Claims) ValidateFile(sourceDeviceID, targetDeviceID, sourceThumbprint, targetThumbprint string, sourceGrantVersion, targetGrantVersion uint64) error {
	if sourceDeviceID == "" || targetDeviceID == "" || sourceThumbprint == "" || targetThumbprint == "" ||
		sourceGrantVersion == 0 || targetGrantVersion == 0 || c.Audience != "relay-file" || c.TransferID == "" ||
		c.Subject != sourceDeviceID || c.Confirmation != sourceThumbprint || c.SourceCredentialType != "device" ||
		c.SourceDeviceID != sourceDeviceID || c.TargetDeviceID != targetDeviceID || c.SourceGrantVersion != sourceGrantVersion ||
		c.TargetGrantVersion != targetGrantVersion || c.SourceKeyThumbprint != sourceThumbprint || c.TargetKeyThumbprint != targetThumbprint ||
		c.SourceKeyVersion == 0 || c.TargetKeyVersion == 0 || !identityClaimMatches(c.SourceIdentityKey, sourceThumbprint) ||
		!identityClaimMatches(c.TargetIdentityKey, targetThumbprint) || c.Direction != "push" ||
		!c.HasScope("remote.peer.file.send") || !c.HasScope("remote.peer.file.receive") || c.MaxDurationSeconds == 0 ||
		c.MaxBytes == 0 || c.MaxFileCount == 0 || c.MaxManifestBytes == 0 || c.AllowedChunkSize == 0 {
		return ErrTicketClaims
	}
	return nil
}

// ValidateFileRelay binds a File Ticket to the Relay process and exact target
// resident connection selected at issuance. A reconnect therefore fences an
// unopened or resumed transfer without exposing any file metadata.
func (c Claims) ValidateFileRelay(nodeID, cellID string, targetConnectionEpoch uint64) error {
	if nodeID == "" || cellID == "" || targetConnectionEpoch == 0 || c.Audience != "relay-file" ||
		c.Subject == "" || c.Subject != c.SourceDeviceID || c.Confirmation == "" || c.Confirmation != c.SourceKeyThumbprint ||
		c.RelayNodeID != nodeID || c.RelayCellID != cellID || c.TargetConnectionEpoch != targetConnectionEpoch {
		return ErrTicketClaims
	}
	return nil
}

func (c Claims) Validate(expectedIssuer, expectedAudience string, now time.Time, leeway time.Duration) error {
	if c.Issuer == "" || c.Audience == "" || c.JWTID == "" || c.IssuedAt <= 0 || c.NotBefore <= 0 || c.ExpiresAt <= 0 || c.ExpiresAt <= c.IssuedAt {
		return ErrTicketClaims
	}
	if expectedIssuer != "" && c.Issuer != expectedIssuer {
		return ErrTicketClaims
	}
	if expectedAudience != "" && c.Audience != expectedAudience {
		return ErrTicketClaims
	}
	if now.Add(leeway).Unix() < c.NotBefore {
		return ErrTicketNotActive
	}
	if now.Add(-leeway).Unix() >= c.ExpiresAt {
		return ErrTicketExpired
	}
	return nil
}

type Issuer struct {
	Issuer     string
	KeyID      string
	PrivateKey ed25519.PrivateKey
}

type ticketHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

func (i Issuer) Sign(claims Claims) (string, error) {
	if i.Issuer == "" || i.KeyID == "" || len(i.PrivateKey) != ed25519.PrivateKeySize {
		return "", errors.New("ticket issuer is not configured")
	}
	if claims.Issuer == "" {
		claims.Issuer = i.Issuer
	}
	if claims.Issuer != i.Issuer {
		return "", ErrTicketClaims
	}
	headerBytes, err := json.Marshal(ticketHeader{Algorithm: "EdDSA", KeyID: i.KeyID, Type: "JWT"})
	if err != nil {
		return "", fmt.Errorf("marshal ticket header: %w", err)
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal ticket claims: %w", err)
	}
	headerPart := base64.RawURLEncoding.EncodeToString(headerBytes)
	claimsPart := base64.RawURLEncoding.EncodeToString(claimsBytes)
	signed := headerPart + "." + claimsPart
	signature := ed25519.Sign(i.PrivateKey, []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type Verifier struct {
	Issuer string
	Keys   map[string]ed25519.PublicKey
	Leeway time.Duration
}

func (v Verifier) Verify(token, expectedAudience string, now time.Time) (Claims, error) {
	if len(token) == 0 || len(token) > maxTicketBytes {
		return Claims{}, ErrTicketMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrTicketMalformed
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrTicketMalformed
	}
	var header ticketHeader
	if err := decodeStrict(headerBytes, &header); err != nil || header.Algorithm != "EdDSA" || header.Type != "JWT" || header.KeyID == "" {
		return Claims{}, ErrTicketMalformed
	}
	publicKey, ok := v.Keys[header.KeyID]
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return Claims{}, ErrTicketSignature
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return Claims{}, ErrTicketSignature
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrTicketMalformed
	}
	var claims Claims
	if err := decodeStrict(claimsBytes, &claims); err != nil {
		return Claims{}, ErrTicketMalformed
	}
	if err := claims.Validate(v.Issuer, expectedAudience, now, v.Leeway); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("unexpected JSON value")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON")
		}
		return err
	}
	return nil
}

func PublicKeyThumbprint(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
