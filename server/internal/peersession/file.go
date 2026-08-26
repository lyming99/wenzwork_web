package peersession

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/fileprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

const (
	DefaultFileMaxCount       uint32 = 500
	DefaultFileManifestBytes  uint32 = 1 << 20
	DefaultFileResumeMaxAge          = 24 * time.Hour
	DefaultFileTicketDuration        = 15 * time.Minute
)

type FileRequestedLimits struct {
	TotalBytes uint64 `json:"totalBytes"`
	FileCount  uint32 `json:"fileCount"`
}

type FileIssueInput struct {
	UserID             uuid.UUID
	SessionID          uuid.UUID
	SourceDeviceID     uuid.UUID
	TargetDeviceID     uuid.UUID
	Direction          string
	ProjectID          *uuid.UUID
	RequestedLimits    FileRequestedLimits
	PreviousFileTicket string
	IdempotencyKey     string
}

type FileLimits struct {
	MaxTotalBytes    uint64 `json:"maxTotalBytes"`
	MaxFileCount     uint32 `json:"maxFileCount"`
	MaxManifestBytes uint32 `json:"maxManifestBytes"`
	AllowedChunkSize uint32 `json:"allowedChunkSize"`
}

type FileResult struct {
	TransferID           uuid.UUID  `json:"transferId"`
	FileTicket           string     `json:"fileTicket"`
	ExpiresAt            time.Time  `json:"expiresAt"`
	MaxDurationSeconds   uint32     `json:"maxDurationSeconds"`
	Limits               FileLimits `json:"limits"`
	RequireLocalApproval bool       `json:"requireLocalApproval"`
	TargetKeyThumbprint  string     `json:"targetKeyThumbprint"`
}

// IssueFile signs a short-lived capability. It intentionally accepts only
// aggregate limits: file names, paths, manifests and content digests remain in
// the end-to-end encrypted device protocol.
func (service *Service) IssueFile(ctx context.Context, input FileIssueInput) (FileResult, error) {
	input.Direction = strings.TrimSpace(input.Direction)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.PreviousFileTicket = strings.TrimSpace(input.PreviousFileTicket)
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.SourceDeviceID == uuid.Nil || input.TargetDeviceID == uuid.Nil ||
		input.SourceDeviceID == input.TargetDeviceID || input.Direction != "push" || input.ProjectID != nil ||
		!idempotencyPattern.MatchString(input.IdempotencyKey) || input.RequestedLimits.TotalBytes == 0 ||
		input.RequestedLimits.TotalBytes > service.maxBytes || input.RequestedLimits.FileCount == 0 ||
		input.RequestedLimits.FileCount > DefaultFileMaxCount || len(input.PreviousFileTicket) > 16<<10 {
		return FileResult{}, ErrInvalidRequest
	}

	credentials, err := service.credentials.LoadPeerCredentials(ctx, input.SourceDeviceID, input.TargetDeviceID)
	if err != nil {
		return FileResult{}, err
	}
	source, sourceOK := credentials[input.SourceDeviceID]
	target, targetOK := credentials[input.TargetDeviceID]
	if !sourceOK {
		return FileResult{}, ErrSourceForbidden
	}
	if !targetOK {
		return FileResult{}, ErrTargetNotFound
	}
	if source.UserID != input.UserID {
		return FileResult{}, ErrSourceForbidden
	}
	if target.UserID != input.UserID {
		return FileResult{}, ErrTargetForbidden
	}
	if source.Status != "active" || target.Status != "active" {
		return FileResult{}, ErrDeviceInactive
	}
	now := service.now().UTC()
	route, err := service.routes.Resolve(target.DeviceID.String(), now)
	if err != nil {
		if errors.Is(err, relayrouter.ErrRouteNotFound) {
			return FileResult{}, ErrTargetOffline
		}
		return FileResult{}, fmt.Errorf("%w: resolve target route", ErrRelayUnavailable)
	}
	relayNodeID, nodeErr := uuid.Parse(route.NodeID)
	relayCellID, cellErr := uuid.Parse(route.CellID)
	if nodeErr != nil || cellErr != nil || route.DeviceID != target.DeviceID.String() || route.UserID != input.UserID.String() ||
		route.GrantVersion != target.GrantVersion || route.ConnectionEpoch == 0 {
		return FileResult{}, ErrRelayUnavailable
	}

	transferID, err := service.fileTransferID(input, now)
	if err != nil {
		return FileResult{}, err
	}
	duration := service.maxDuration
	if duration > DefaultFileTicketDuration {
		duration = DefaultFileTicketDuration
	}
	// File Tickets remain deliberately short-lived even when the generic Peer
	// admission Ticket is configured for a long-lived encrypted session.
	ticketTTL := service.ticketTTL
	if ticketTTL > duration {
		ticketTTL = duration
	}
	claims := remoteauth.Claims{
		Audience: "relay-file", Subject: source.DeviceID.String(), UserID: input.UserID.String(), TransferID: transferID.String(),
		SourceDeviceID: source.DeviceID.String(), TargetDeviceID: target.DeviceID.String(), Direction: "push",
		SourceGrantVersion: source.GrantVersion, TargetGrantVersion: target.GrantVersion,
		SourceKeyThumbprint: source.PublicKeyThumbprint, TargetKeyThumbprint: target.PublicKeyThumbprint,
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(source.IdentityPublicKey),
		TargetIdentityKey: base64.RawURLEncoding.EncodeToString(target.IdentityPublicKey),
		SourceKeyVersion:  source.KeyVersion, TargetKeyVersion: target.KeyVersion, SourceCredentialType: "device",
		Confirmation: source.PublicKeyThumbprint, RelayNodeID: relayNodeID.String(), RelayCellID: relayCellID.String(),
		TargetConnectionEpoch: route.ConnectionEpoch,
		Scopes:                []string{"remote.peer.file.send", "remote.peer.file.receive"}, ProjectID: "",
		MaxDurationSeconds: uint32(duration / time.Second), MaxBytes: input.RequestedLimits.TotalBytes,
		MaxFileCount: input.RequestedLimits.FileCount, MaxManifestBytes: DefaultFileManifestBytes,
		AllowedChunkSize: fileprotocol.DefaultChunkSize, RequireLocalApproval: true,
		JWTID: uuid.NewString(), IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(ticketTTL).Unix(),
	}
	requestHash := fileIssueRequestHash(input, duration)
	claims, err = service.idempotency.Reserve(ctx, input.UserID.String(), input.SourceDeviceID.String(), input.IdempotencyKey, requestHash, claims, ticketTTL)
	if err != nil {
		return FileResult{}, err
	}
	actualTransferID, parseErr := uuid.Parse(claims.TransferID)
	if parseErr != nil || actualTransferID == uuid.Nil || (input.PreviousFileTicket != "" && actualTransferID != transferID) ||
		claims.SourceDeviceID != source.DeviceID.String() || claims.TargetDeviceID != target.DeviceID.String() ||
		claims.Direction != "push" || claims.SourceGrantVersion != source.GrantVersion || claims.TargetGrantVersion != target.GrantVersion ||
		claims.SourceKeyThumbprint != source.PublicKeyThumbprint || claims.TargetKeyThumbprint != target.PublicKeyThumbprint ||
		claims.SourceIdentityKey != base64.RawURLEncoding.EncodeToString(source.IdentityPublicKey) ||
		claims.TargetIdentityKey != base64.RawURLEncoding.EncodeToString(target.IdentityPublicKey) ||
		claims.SourceKeyVersion != source.KeyVersion || claims.TargetKeyVersion != target.KeyVersion || claims.SourceCredentialType != "device" ||
		claims.RelayNodeID != relayNodeID.String() || claims.RelayCellID != relayCellID.String() || claims.TargetConnectionEpoch != route.ConnectionEpoch ||
		claims.MaxBytes != input.RequestedLimits.TotalBytes || claims.MaxFileCount != input.RequestedLimits.FileCount ||
		claims.MaxManifestBytes != DefaultFileManifestBytes || claims.AllowedChunkSize != fileprotocol.DefaultChunkSize ||
		!claims.RequireLocalApproval || claims.ExpiresAt <= now.Unix() || claims.ValidateFile(source.DeviceID.String(), target.DeviceID.String(), source.PublicKeyThumbprint, target.PublicKeyThumbprint, source.GrantVersion, target.GrantVersion) != nil {
		return FileResult{}, ErrIdempotencyConflict
	}
	ticket, err := service.issuer.Sign(claims)
	if err != nil {
		return FileResult{}, fmt.Errorf("%w: sign File Ticket", ErrUnavailable)
	}
	return FileResult{
		TransferID: actualTransferID, FileTicket: ticket, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
		MaxDurationSeconds:   claims.MaxDurationSeconds,
		Limits:               FileLimits{MaxTotalBytes: claims.MaxBytes, MaxFileCount: claims.MaxFileCount, MaxManifestBytes: claims.MaxManifestBytes, AllowedChunkSize: claims.AllowedChunkSize},
		RequireLocalApproval: claims.RequireLocalApproval, TargetKeyThumbprint: claims.TargetKeyThumbprint,
	}, nil
}

func (service *Service) fileTransferID(input FileIssueInput, now time.Time) (uuid.UUID, error) {
	if input.PreviousFileTicket == "" {
		id, err := uuid.NewV7()
		if err != nil {
			return uuid.Nil, ErrUnavailable
		}
		return id, nil
	}
	publicKey, ok := service.issuer.PrivateKey.Public().(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return uuid.Nil, ErrUnavailable
	}
	verifier := remoteauth.Verifier{
		Issuer: service.issuer.Issuer, Keys: map[string]ed25519.PublicKey{service.issuer.KeyID: publicKey},
		Leeway: DefaultFileResumeMaxAge,
	}
	previous, err := verifier.Verify(input.PreviousFileTicket, "relay-file", now)
	if err != nil || previous.IssuedAt > now.Unix() || previous.NotBefore > now.Add(5*time.Second).Unix() || previous.ExpiresAt <= now.Add(-DefaultFileResumeMaxAge).Unix() ||
		previous.UserID != input.UserID.String() || previous.SourceDeviceID != input.SourceDeviceID.String() ||
		previous.TargetDeviceID != input.TargetDeviceID.String() || previous.Direction != input.Direction || previous.ProjectID != "" {
		return uuid.Nil, ErrInvalidRequest
	}
	id, err := uuid.Parse(previous.TransferID)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, ErrInvalidRequest
	}
	return id, nil
}

func fileIssueRequestHash(input FileIssueInput, duration time.Duration) string {
	previousDigest := ""
	if input.PreviousFileTicket != "" {
		digest := sha256.Sum256([]byte(input.PreviousFileTicket))
		previousDigest = hex.EncodeToString(digest[:])
	}
	payload, _ := json.Marshal(struct {
		Kind               string `json:"kind"`
		PreviousTicket     string `json:"previousTicket,omitempty"`
		TargetDeviceID     string `json:"targetDeviceId"`
		Direction          string `json:"direction"`
		MaxTotalBytes      uint64 `json:"maxTotalBytes"`
		MaxFileCount       uint32 `json:"maxFileCount"`
		MaxDurationSeconds uint32 `json:"maxDurationSeconds"`
	}{"file", previousDigest, input.TargetDeviceID.String(), input.Direction, input.RequestedLimits.TotalBytes, input.RequestedLimits.FileCount, uint32(duration / time.Second)})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
