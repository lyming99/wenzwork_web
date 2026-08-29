package relayallocation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalidRequest        = errors.New("Relay allocation request is invalid")
	ErrDeviceForbidden       = errors.New("Relay allocation device is forbidden")
	ErrDeviceInactive        = errors.New("Relay allocation device is inactive")
	ErrStaleConnectionEpoch  = errors.New("Relay allocation connection epoch is stale")
	ErrAllocationNotFound    = errors.New("Relay allocation was not found")
	ErrRequestConflict       = errors.New("Relay allocation idempotency key conflicts with another request")
	ErrAllocationUnavailable = errors.New("Relay allocation is unavailable")
)

const (
	assignmentLease = 24 * time.Hour
	requestKeyTTL   = 24 * time.Hour
)

var allocationIdempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

type ServiceConfig struct {
	Database     *gorm.DB
	Issuer       remoteauth.Issuer
	Region       string
	Pool         string
	Cell         string
	TicketTTL    time.Duration
	AccessPolicy *remoteaccesspolicy.Store
	// DeviceLinkGrantPublicKeys are delivered only to the authenticated Device
	// Agent with its Relay allocation. They let the Device independently verify
	// the v2 DeviceConnectionGrant carried in LINK_INIT.
	DeviceLinkGrantIssuer     string
	DeviceLinkGrantPublicKeys map[string]ed25519.PublicKey
	Now                       func() time.Time
}

type Service struct {
	db                   *gorm.DB
	issuer               remoteauth.Issuer
	region               string
	pool                 string
	cell                 string
	ticketTTL            time.Duration
	deviceLinkGrantTrust DeviceLinkGrantTrustBundle
	accessPolicy         *remoteaccesspolicy.Store
	now                  func() time.Time
}

type CreateInput struct {
	UserID          uuid.UUID
	SessionID       uuid.UUID
	DeviceID        uuid.UUID
	IdempotencyKey  string
	RemoteDeviceID  uuid.UUID
	ProtocolMin     uint32
	ProtocolMax     uint32
	ConnectionEpoch uint64
	PreferredRegion string
}

type RefreshInput struct {
	UserID               uuid.UUID
	SessionID            uuid.UUID
	DeviceID             uuid.UUID
	AssignmentID         uuid.UUID
	IdempotencyKey       string
	Reason               string
	LastEndpointRevision uint64
}

type Endpoint struct {
	CellID           uuid.UUID `json:"cellId"`
	EndpointRevision uint64    `json:"endpointRevision"`
	URL              string    `json:"url"`
}

type RetryPolicy struct {
	InitialDelayMS int `json:"initialDelayMs"`
	MaxDelayMS     int `json:"maxDelayMs"`
}

type Result struct {
	AssignmentID             uuid.UUID                  `json:"assignmentId"`
	AssignmentVersion        uint64                     `json:"assignmentVersion"`
	Scope                    string                     `json:"scope"`
	Primary                  Endpoint                   `json:"primary"`
	Fallbacks                []Endpoint                 `json:"fallbacks"`
	ConnectionTicket         string                     `json:"connectionTicket"`
	TicketExpiresAt          time.Time                  `json:"ticketExpiresAt"`
	AssignmentLeaseExpiresAt time.Time                  `json:"assignmentLeaseExpiresAt"`
	RefreshAfter             time.Time                  `json:"refreshAfter"`
	RetryPolicy              RetryPolicy                `json:"retryPolicy"`
	DeviceLinkGrantTrust     DeviceLinkGrantTrustBundle `json:"deviceLinkGrantTrust"`
}

type DeviceLinkGrantVerificationKey struct {
	KeyID     string `json:"keyId"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"publicKey"`
}

// DeviceLinkGrantTrustBundle contains the only Client-to-Device trust anchors
// delivered to an Agent in remote/v2. Its keys accept only
// WenzWorkDeviceLinkGrantV2 claim envelopes.
type DeviceLinkGrantTrustBundle struct {
	Issuer string                           `json:"issuer"`
	Keys   []DeviceLinkGrantVerificationKey `json:"keys"`
}

// DeviceLinkGrantTrust returns a defensive copy suitable for an authenticated
// Device Agent registration response. Direct mode needs the same trust anchors
// as the Relay allocation path before it can accept a browser Carrier.
func (service *Service) DeviceLinkGrantTrust() DeviceLinkGrantTrustBundle {
	if service == nil {
		return DeviceLinkGrantTrustBundle{}
	}
	result := DeviceLinkGrantTrustBundle{Issuer: service.deviceLinkGrantTrust.Issuer}
	result.Keys = append([]DeviceLinkGrantVerificationKey(nil), service.deviceLinkGrantTrust.Keys...)
	return result
}

func NewService(config ServiceConfig) (*Service, error) {
	config.Region, config.Pool, config.Cell = strings.TrimSpace(config.Region), strings.TrimSpace(config.Pool), strings.TrimSpace(config.Cell)
	if config.Database == nil || config.AccessPolicy == nil || config.Region == "" || config.Pool == "" || config.Cell == "" ||
		config.Issuer.Issuer == "" || config.Issuer.KeyID == "" || len(config.Issuer.PrivateKey) != ed25519.PrivateKeySize ||
		config.TicketTTL < time.Minute || config.TicketTTL > 15*time.Minute ||
		!validDeviceLinkGrantTrust(config.DeviceLinkGrantIssuer, config.DeviceLinkGrantPublicKeys) {
		return nil, errors.New("Relay allocation service configuration is invalid")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	deviceLinkTrust := DeviceLinkGrantTrustBundle{Issuer: config.DeviceLinkGrantIssuer, Keys: make([]DeviceLinkGrantVerificationKey, 0, len(config.DeviceLinkGrantPublicKeys))}
	deviceLinkKeyIDs := make([]string, 0, len(config.DeviceLinkGrantPublicKeys))
	for keyID := range config.DeviceLinkGrantPublicKeys {
		deviceLinkKeyIDs = append(deviceLinkKeyIDs, keyID)
	}
	slices.Sort(deviceLinkKeyIDs)
	for _, keyID := range deviceLinkKeyIDs {
		deviceLinkTrust.Keys = append(deviceLinkTrust.Keys, DeviceLinkGrantVerificationKey{
			KeyID: keyID, Algorithm: "Ed25519", PublicKey: base64.RawURLEncoding.EncodeToString(config.DeviceLinkGrantPublicKeys[keyID]),
		})
	}
	return &Service{
		db: config.Database, issuer: config.Issuer,
		region: config.Region, pool: config.Pool, cell: config.Cell, ticketTTL: config.TicketTTL,
		deviceLinkGrantTrust: deviceLinkTrust, accessPolicy: config.AccessPolicy, now: config.Now,
	}, nil
}

func validDeviceLinkGrantTrust(issuer string, keys map[string]ed25519.PublicKey) bool {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" || len(issuer) > 128 || len(keys) == 0 || len(keys) > 8 {
		return false
	}
	for keyID, publicKey := range keys {
		if len(keyID) == 0 || len(keyID) > 64 || !allocationIdempotencyPattern.MatchString("trustkey-"+keyID) || len(publicKey) != ed25519.PublicKeySize {
			return false
		}
	}
	return true
}

func (service *Service) Create(ctx context.Context, input CreateInput) (Result, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.PreferredRegion = strings.TrimSpace(input.PreferredRegion)
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.DeviceID == uuid.Nil || input.RemoteDeviceID == uuid.Nil ||
		input.RemoteDeviceID != input.DeviceID || !allocationIdempotencyPattern.MatchString(input.IdempotencyKey) ||
		input.ProtocolMin != 2 || input.ProtocolMax != 2 || input.ConnectionEpoch == 0 ||
		(input.PreferredRegion != "" && input.PreferredRegion != service.region) {
		if input.RemoteDeviceID != uuid.Nil && input.DeviceID != uuid.Nil && input.RemoteDeviceID != input.DeviceID {
			return Result{}, ErrDeviceForbidden
		}
		return Result{}, ErrInvalidRequest
	}
	hash := requestDigest(struct {
		RemoteDeviceID           uuid.UUID
		ProtocolMin, ProtocolMax uint32
		ConnectionEpoch          uint64
		PreferredRegion          string
	}{input.RemoteDeviceID, input.ProtocolMin, input.ProtocolMax, input.ConnectionEpoch, input.PreferredRegion})
	prepared, err := service.prepare(ctx, prepareInput{
		UserID: input.UserID, SessionID: input.SessionID, DeviceID: input.DeviceID,
		IdempotencyKey: input.IdempotencyKey, Operation: "allocation", RequestHash: hash,
		ProtocolMin: input.ProtocolMin, ProtocolMax: input.ProtocolMax, ConnectionEpoch: &input.ConnectionEpoch,
	})
	if err != nil {
		return Result{}, err
	}
	return service.projectAndIssue(ctx, prepared)
}

func (service *Service) Refresh(ctx context.Context, input RefreshInput) (Result, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.Reason = strings.TrimSpace(input.Reason)
	validReason := input.Reason == "" || slices.Contains([]string{"scheduled", "goaway", "endpoint_changed", "connection_failed", "cell_unavailable"}, input.Reason)
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.DeviceID == uuid.Nil || input.AssignmentID == uuid.Nil ||
		!allocationIdempotencyPattern.MatchString(input.IdempotencyKey) || !validReason {
		return Result{}, ErrInvalidRequest
	}
	hash := requestDigest(struct {
		AssignmentID         uuid.UUID
		Reason               string
		LastEndpointRevision uint64
	}{input.AssignmentID, input.Reason, input.LastEndpointRevision})
	prepared, err := service.prepare(ctx, prepareInput{
		UserID: input.UserID, SessionID: input.SessionID, DeviceID: input.DeviceID,
		IdempotencyKey: input.IdempotencyKey, Operation: "allocation_refresh", RequestHash: hash,
		ExpectedAssignmentID: input.AssignmentID,
	})
	if err != nil {
		return Result{}, err
	}
	return service.projectAndIssue(ctx, prepared)
}

type prepareInput struct {
	UserID, SessionID, DeviceID            uuid.UUID
	IdempotencyKey, Operation, RequestHash string
	ProtocolMin, ProtocolMax               uint32
	ConnectionEpoch                        *uint64
	ExpectedAssignmentID                   uuid.UUID
}

type preparedAllocation struct {
	AssignmentID uuid.UUID
	OutboxID     uuid.UUID
	Device       allocationCredentialRow
}

func (service *Service) prepare(ctx context.Context, input prepareInput) (preparedAllocation, error) {
	now := service.now().UTC()
	var prepared preparedAllocation
	err := service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", input.UserID.String()).Error; err != nil {
			return fmt.Errorf("lock Relay allocation user: %w", err)
		}
		var session struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Table("app_sessions").Select("id").Where(
			"id = ? AND user_id = ? AND device_id = ? AND revoked_at IS NULL AND idle_expires_at > ?",
			input.SessionID, input.UserID, input.DeviceID, now,
		).Take(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDeviceForbidden
			}
			return fmt.Errorf("verify Relay allocation App Session: %w", err)
		}
		if err := service.accessPolicy.RequireMembershipTx(tx, input.UserID, now); err != nil {
			return err
		}
		var device allocationCredentialRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&device, "device_id = ?", input.DeviceID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDeviceForbidden
			}
			return fmt.Errorf("load Relay allocation device: %w", err)
		}
		if device.UserID != input.UserID {
			return ErrDeviceForbidden
		}
		if device.Status != "active" || device.GrantVersion < 1 {
			return ErrDeviceInactive
		}
		if len(device.IdentityPublicKey) != ed25519.PublicKeySize ||
			remoteauth.PublicKeyThumbprint(ed25519.PublicKey(device.IdentityPublicKey)) != device.PublicKeyThumbprint {
			return ErrDeviceInactive
		}
		if input.ProtocolMin == 0 {
			input.ProtocolMin, input.ProtocolMax = uint32(device.ProtocolMin), uint32(device.ProtocolMax)
		}
		if device.ProtocolMin != 2 || device.ProtocolMax != 2 || int64(input.ProtocolMin) != device.ProtocolMin || int64(input.ProtocolMax) != device.ProtocolMax {
			return ErrRequestConflict
		}

		var prior allocationRequestKeyRow
		priorErr := tx.Where("user_id = ? AND device_id = ? AND operation = ? AND idempotency_key = ?",
			input.UserID, input.DeviceID, input.Operation, input.IdempotencyKey).First(&prior).Error
		if priorErr == nil {
			if prior.RequestHash != input.RequestHash || prior.AssignmentID == nil || prior.OutboxID == nil {
				return ErrRequestConflict
			}
			prepared = preparedAllocation{AssignmentID: *prior.AssignmentID, OutboxID: *prior.OutboxID, Device: device}
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load Relay allocation idempotency record: %w", priorErr)
		}
		if input.ConnectionEpoch != nil {
			if *input.ConnectionEpoch <= uint64(device.LastConnectionEpoch) {
				return ErrStaleConnectionEpoch
			}
			device.LastConnectionEpoch = int64(*input.ConnectionEpoch)
		}

		if input.ExpectedAssignmentID != uuid.Nil {
			var expected allocationAssignmentRow
			if err := tx.First(&expected, "id = ? AND user_id = ?", input.ExpectedAssignmentID, input.UserID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrAllocationNotFound
				}
				return fmt.Errorf("load Relay allocation to refresh: %w", err)
			}
		}

		// Recover a legacy pending assignment created by the former asynchronous
		// projection path. Host now commits assignment authority directly in
		// PostgreSQL before issuing a ticket.
		var pending allocationAssignmentRow
		pendingErr := tx.Where("user_id = ? AND status = 'pending'", input.UserID).Order("assignment_version DESC").First(&pending).Error
		if pendingErr == nil {
			outboxID, err := pendingOutboxID(tx, pending.ID)
			if err != nil {
				return err
			}
			if err := tx.Model(&pending).Updates(map[string]any{
				"status": "current", "effective_at": now, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("activate legacy pending Relay assignment: %w", err)
			}
			if err := tx.Table("relay_outbox").Where("id = ?", outboxID).Updates(map[string]any{
				"published_at": now, "claimed_at": nil, "claim_token": nil, "last_error": nil,
			}).Error; err != nil {
				return fmt.Errorf("retire legacy Relay assignment projection event: %w", err)
			}
			if err := saveAllocationRequestKey(tx, input, pending.ID, outboxID, now); err != nil {
				return err
			}
			if err := tx.Model(&device).Updates(map[string]any{"last_connection_epoch": device.LastConnectionEpoch, "updated_at": now}).Error; err != nil {
				return fmt.Errorf("update remote device connection epoch: %w", err)
			}
			prepared = preparedAllocation{AssignmentID: pending.ID, OutboxID: outboxID, Device: device}
			return nil
		}
		if !errors.Is(pendingErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load pending Relay assignment: %w", pendingErr)
		}

		var latest allocationAssignmentRow
		latestErr := tx.Where("user_id = ?", input.UserID).Order("assignment_version DESC").First(&latest).Error
		if latestErr != nil && !errors.Is(latestErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load current Relay assignment: %w", latestErr)
		}
		cells, err := service.loadCells(tx, now, input.ProtocolMin, input.ProtocolMax)
		if err != nil {
			return err
		}
		var current *Assignment
		if latestErr == nil && latest.Status == "current" && latest.EffectiveAt != nil && latest.LeaseExpiresAt.After(now) {
			converted, err := assignmentFromDatabase(latest, cells)
			if err != nil {
				return err
			}
			current = &converted
		}
		var pinnedID string
		var pin struct {
			CellID uuid.UUID `gorm:"column:cell_id"`
		}
		if err := tx.Table("relay_assignment_pins").Select("cell_id").Where("user_id = ? AND (expires_at IS NULL OR expires_at > ?)", input.UserID, now).Take(&pin).Error; err == nil {
			pinnedID = pin.CellID.String()
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load Relay assignment pin: %w", err)
		}
		chosen, err := (Scheduler{}).Allocate(Request{
			UserID: input.UserID.String(), Region: service.region, Pool: service.pool, ProtocolVersion: input.ProtocolMax,
			PinnedCellID: pinnedID, Current: current, Now: now, LeaseDuration: assignmentLease,
			MinimumHealthyN: 1, AssignmentID: uuid.NewString,
		}, cells)
		if err != nil {
			if errors.Is(err, ErrNoSchedulableCell) {
				return ErrAllocationUnavailable
			}
			return err
		}

		var row allocationAssignmentRow
		renewing := current != nil && chosen.ID == current.ID && chosen.Version == current.Version
		fallbackJSON, _ := json.Marshal(chosen.FallbackCellIDs)
		if renewing {
			row = latest
			row.FallbackCellIDs, row.LeaseExpiresAt, row.UpdatedAt = fallbackJSON, chosen.LeaseExpiresAt, now
			if err := tx.Model(&row).Updates(map[string]any{
				"fallback_cell_ids": fallbackJSON, "lease_expires_at": chosen.LeaseExpiresAt, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("renew Relay assignment: %w", err)
			}
		} else {
			version := int64(chosen.Version)
			if latestErr == nil && version <= latest.AssignmentVersion {
				version = latest.AssignmentVersion + 1
			}
			if latestErr == nil && latest.Status == "current" {
				if err := tx.Model(&latest).Updates(map[string]any{"status": "historical", "superseded_at": now, "updated_at": now}).Error; err != nil {
					return fmt.Errorf("supersede Relay assignment: %w", err)
				}
			}
			cellID, parseErr := uuid.Parse(chosen.CellID)
			if parseErr != nil {
				return ErrAllocationUnavailable
			}
			row = allocationAssignmentRow{
				ID: uuid.New(), UserID: input.UserID, CellID: cellID, AssignmentVersion: version,
				Mode: chosen.Mode, Status: "current", FallbackCellIDs: fallbackJSON,
				LeaseExpiresAt: chosen.LeaseExpiresAt, EffectiveAt: &now, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return fmt.Errorf("create Relay assignment: %w", err)
			}
		}
		var deviceIDs []uuid.UUID
		if err := tx.Table("remote_device_credentials").Where("user_id = ?", input.UserID).Order("device_id").Pluck("device_id", &deviceIDs).Error; err != nil {
			return fmt.Errorf("list Relay assignment devices: %w", err)
		}
		fallbackUUIDs := make([]uuid.UUID, 0, len(chosen.FallbackCellIDs))
		for _, value := range chosen.FallbackCellIDs {
			parsed, parseErr := uuid.Parse(value)
			if parseErr != nil {
				return ErrAllocationUnavailable
			}
			fallbackUUIDs = append(fallbackUUIDs, parsed)
		}
		outboxID := uuid.New()
		payload, _ := json.Marshal(map[string]any{
			"assignmentId": row.ID, "userId": row.UserID, "cellId": row.CellID,
			"assignmentVersion": row.AssignmentVersion, "fallbackCellIds": fallbackUUIDs,
			"deviceIds": deviceIDs, "mode": row.Mode,
		})
		if err := tx.Table("relay_outbox").Create(map[string]any{
			"id": outboxID, "aggregate_type": "relay_assignment", "aggregate_id": row.ID,
			"event_type": "relay.assignment.changed", "payload": json.RawMessage(payload),
			"attempts": 0, "available_at": now, "published_at": now, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("append Relay assignment Outbox event: %w", err)
		}
		if err := saveAllocationRequestKey(tx, input, row.ID, outboxID, now); err != nil {
			return err
		}
		if err := tx.Model(&device).Updates(map[string]any{"last_connection_epoch": device.LastConnectionEpoch, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("update remote device connection epoch: %w", err)
		}
		prepared = preparedAllocation{AssignmentID: row.ID, OutboxID: outboxID, Device: device}
		return nil
	})
	if err != nil {
		return preparedAllocation{}, err
	}
	return prepared, nil
}

func (service *Service) projectAndIssue(ctx context.Context, prepared preparedAllocation) (Result, error) {
	var row allocationAssignmentRow
	if err := service.db.WithContext(ctx).First(&row, "id = ? AND status = 'current' AND effective_at IS NOT NULL", prepared.AssignmentID).Error; err != nil {
		return Result{}, fmt.Errorf("%w: assignment is not current", ErrAllocationUnavailable)
	}
	fallbackIDs, err := decodeFallbacks(row.FallbackCellIDs)
	if err != nil {
		return Result{}, ErrAllocationUnavailable
	}
	allowedUUIDs := append([]uuid.UUID{row.CellID}, fallbackIDs...)
	allowed := make([]string, 0, len(allowedUUIDs))
	for _, cellID := range allowedUUIDs {
		allowed = append(allowed, cellID.String())
	}
	endpoints, err := service.activeEndpoints(ctx, allowedUUIDs)
	if err != nil {
		return Result{}, err
	}
	primary, ok := endpoints[row.CellID]
	if !ok {
		return Result{}, ErrAllocationUnavailable
	}
	fallbacks := make([]Endpoint, 0, len(fallbackIDs))
	for _, cellID := range fallbackIDs {
		if endpoint, exists := endpoints[cellID]; exists {
			fallbacks = append(fallbacks, endpoint)
		}
	}
	now := service.now().UTC()
	ticket, expiresAt, refreshAfter, err := service.signConnectionTicket(now, row, prepared.Device, allowed)
	if err != nil {
		return Result{}, fmt.Errorf("%w: sign Relay ticket", ErrAllocationUnavailable)
	}
	if err := service.db.WithContext(ctx).Model(&allocationCredentialRow{}).Where("device_id = ?", prepared.Device.DeviceID).
		Updates(map[string]any{"last_allocation_at": now, "updated_at": now}).Error; err != nil {
		return Result{}, fmt.Errorf("%w: record Relay allocation", ErrAllocationUnavailable)
	}
	return Result{
		AssignmentID: row.ID, AssignmentVersion: uint64(row.AssignmentVersion), Scope: "user",
		Primary: primary, Fallbacks: fallbacks, ConnectionTicket: ticket, TicketExpiresAt: expiresAt,
		AssignmentLeaseExpiresAt: row.LeaseExpiresAt.UTC(), RefreshAfter: refreshAfter,
		RetryPolicy:          RetryPolicy{InitialDelayMS: 1000, MaxDelayMS: 30000},
		DeviceLinkGrantTrust: service.deviceLinkGrantTrust,
	}, nil
}

func (service *Service) signConnectionTicket(now time.Time, row allocationAssignmentRow, device allocationCredentialRow, allowed []string) (string, time.Time, time.Time, error) {
	now = now.UTC()
	expiresAt := now.Add(service.ticketTTL)
	claims := remoteauth.Claims{
		Audience: "relay", Subject: device.DeviceID.String(), UserID: row.UserID.String(),
		AssignmentID: row.ID.String(), AssignmentVersion: uint64(row.AssignmentVersion), AllowedCellIDs: slices.Clone(allowed),
		GrantVersion: uint64(device.GrantVersion), Scopes: []string{"remote.connect"},
		ProtocolMin: uint32(device.ProtocolMin), ProtocolMax: uint32(device.ProtocolMax),
		Confirmation: device.PublicKeyThumbprint,
		IdentityKey:  base64.RawURLEncoding.EncodeToString(device.IdentityPublicKey), JWTID: uuid.NewString(),
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: expiresAt.Unix(),
	}
	ticket, err := service.issuer.Sign(claims)
	return ticket, expiresAt, now.Add(service.ticketTTL * 4 / 5), err
}

func (service *Service) loadCells(tx *gorm.DB, now time.Time, protocolMin, protocolMax uint32) ([]Cell, error) {
	type row struct {
		ID                                                          uuid.UUID `gorm:"column:id"`
		Region, Pool, Status, Endpoint                              string
		EndpointRevision                                            int64   `gorm:"column:endpoint_revision"`
		ProtocolMin                                                 int64   `gorm:"column:protocol_min"`
		ProtocolMax                                                 int64   `gorm:"column:protocol_max"`
		Weight                                                      float64 `gorm:"column:weight"`
		ActiveConnections, ConnectionSoftLimit, ConnectionHardLimit int64
		EgressMbps, EgressSoftLimitMbps, WriteLoopLagMillisP99      float64
		MemoryBytes                                                 int64
		HealthyNodes                                                int64 `gorm:"column:healthy_nodes"`
	}
	var rows []row
	err := tx.Raw(`
		SELECT cell.id, region.code AS region, pool.code AS pool, cell.status,
		       COALESCE(endpoint.public_endpoint, '') AS endpoint,
		       COALESCE(endpoint.revision, 0) AS endpoint_revision,
		       cell.protocol_min, cell.protocol_max, cell.weight,
		       COALESCE(sum(instance.active_connections) FILTER (WHERE instance.status = 'ready' AND instance.lease_expires_at > ?), 0) AS active_connections,
		       cell.connection_soft_limit, cell.connection_hard_limit,
		       COALESCE(sum(instance.egress_mbps) FILTER (WHERE instance.status = 'ready' AND instance.lease_expires_at > ?), 0) AS egress_mbps,
		       cell.file_bandwidth_soft_limit_mbps AS egress_soft_limit_mbps,
		       COALESCE(sum(instance.memory_bytes) FILTER (WHERE instance.status = 'ready' AND instance.lease_expires_at > ?), 0) AS memory_bytes,
		       COALESCE(max(instance.write_loop_lag_ms) FILTER (WHERE instance.status = 'ready' AND instance.lease_expires_at > ?), 0) AS write_loop_lag_millis_p99,
		       count(instance.id) FILTER (WHERE instance.status = 'ready' AND instance.lease_expires_at > ?) AS healthy_nodes
		FROM relay_cells cell
		JOIN relay_pools pool ON pool.id = cell.pool_id AND pool.status = 'active'
		JOIN relay_regions region ON region.id = pool.region_id AND region.status = 'active'
		LEFT JOIN relay_cell_endpoints endpoint ON endpoint.cell_id = cell.id AND endpoint.status = 'active'
		LEFT JOIN relay_node_instances instance ON instance.cell_id = cell.id
		WHERE region.code = ? AND pool.code = ? AND cell.code = ? AND cell.protocol_max >= ? AND cell.protocol_min <= ?
		GROUP BY cell.id, region.code, pool.code, endpoint.id
		ORDER BY cell.id`, now, now, now, now, now, service.region, service.pool, service.cell, protocolMin, protocolMax).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load schedulable Relay Cells: %w", err)
	}
	cells := make([]Cell, 0, len(rows))
	cellIDs := make([]uuid.UUID, 0, len(rows))
	for _, item := range rows {
		nodes := make([]Node, 0, item.HealthyNodes)
		for index := int64(0); index < item.HealthyNodes; index++ {
			nodes = append(nodes, Node{ID: fmt.Sprintf("%s-%d", item.ID, index), Healthy: true})
		}
		cells = append(cells, Cell{
			ID: item.ID.String(), Region: item.Region, Pool: item.Pool, Status: CellStatus(item.Status),
			Endpoint: item.Endpoint, EndpointRevision: uint64(item.EndpointRevision), EndpointActive: validDirectRelayEndpoint(item.Endpoint),
			ProtocolMin: uint32(item.ProtocolMin), ProtocolMax: uint32(item.ProtocolMax), Weight: item.Weight,
			ActiveConnections: item.ActiveConnections, ConnectionSoftLimit: item.ConnectionSoftLimit,
			ConnectionHardLimit: item.ConnectionHardLimit, EgressMbps: item.EgressMbps,
			EgressSoftLimitMbps: item.EgressSoftLimitMbps, MemoryBytes: item.MemoryBytes,
			MemorySoftLimitBytes: max(item.MemoryBytes*2, 1), WriteLoopLagMillisP99: item.WriteLoopLagMillisP99,
			WriteLoopLagLimit: 1000, Nodes: nodes,
		})
		cellIDs = append(cellIDs, item.ID)
	}
	directEndpoints, err := loadDirectRelayEndpoints(tx, cellIDs, now)
	if err != nil {
		return nil, err
	}
	applyDirectRelayEndpoints(cells, directEndpoints)
	return cells, nil
}

func assignmentFromDatabase(row allocationAssignmentRow, cells []Cell) (Assignment, error) {
	fallbacks, err := decodeFallbacks(row.FallbackCellIDs)
	if err != nil {
		return Assignment{}, err
	}
	var selected Cell
	for _, cell := range cells {
		if cell.ID == row.CellID.String() {
			selected = cell
			break
		}
	}
	if selected.ID == "" {
		// Keep the current fact visible to Scheduler; it will only renew it when
		// the Cell also appears in the eligible candidate set.
		selected.ID = row.CellID.String()
	}
	fallbackStrings := make([]string, 0, len(fallbacks))
	for _, id := range fallbacks {
		fallbackStrings = append(fallbackStrings, id.String())
	}
	return Assignment{
		ID: row.ID.String(), UserID: row.UserID.String(), CellID: row.CellID.String(), Version: uint64(row.AssignmentVersion),
		Mode: row.Mode, LeaseExpiresAt: row.LeaseExpiresAt, Endpoint: selected.Endpoint,
		EndpointRevision: selected.EndpointRevision, FallbackCellIDs: fallbackStrings,
	}, nil
}

func (service *Service) activeEndpoints(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Endpoint, error) {
	type row struct {
		CellID   uuid.UUID `gorm:"column:cell_id"`
		Revision int64     `gorm:"column:revision"`
		URL      string    `gorm:"column:public_endpoint"`
	}
	var rows []row
	if err := service.db.WithContext(ctx).Table("relay_cell_endpoints").Select("cell_id, revision, public_endpoint").
		Where("cell_id IN ? AND status = 'active'", ids).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load active Relay endpoints: %w", err)
	}
	result := make(map[uuid.UUID]Endpoint, len(rows))
	for _, item := range rows {
		result[item.CellID] = Endpoint{CellID: item.CellID, EndpointRevision: uint64(item.Revision), URL: item.URL}
	}
	directEndpoints, err := loadDirectRelayEndpoints(service.db.WithContext(ctx), ids, service.now().UTC())
	if err != nil {
		return nil, err
	}
	for cellID, endpoint := range directEndpoints {
		result[cellID] = endpoint
	}
	return result, nil
}

// loadDirectRelayEndpoints returns one current Access-Key Relay address per
// Cell. A direct Relay installation owns its public endpoint, so it must not
// require a second legacy relay_cell_endpoints row before the Cell can be
// scheduled. The Cell lifecycle remains authoritative: callers still apply
// the draft/active/draining/disabled gate independently.
func loadDirectRelayEndpoints(query *gorm.DB, ids []uuid.UUID, now time.Time) (map[uuid.UUID]Endpoint, error) {
	result := make(map[uuid.UUID]Endpoint)
	if len(ids) == 0 {
		return result, nil
	}
	// Access-Key Relays are directly addressable. When a ready current instance
	// has a management-configured endpoint, use that exact endpoint rather than
	// the Cell's legacy edge endpoint. This lets an operator switch a Relay from
	// WS to WSS (or rotate its certificate) without separately editing topology.
	type directRow struct {
		CellID         uuid.UUID `gorm:"column:cell_id"`
		Version        int64     `gorm:"column:version"`
		PublicEndpoint string    `gorm:"column:public_endpoint"`
	}
	var directRows []directRow
	if err := query.Raw(`
		SELECT instance.cell_id, installation.version, installation.public_endpoint
		FROM relay_node_instances instance
		JOIN relay_node_installations installation
		  ON installation.id = instance.installation_id
		 AND installation.current_instance_id = instance.id
		WHERE instance.cell_id IN ?
		  AND instance.status = 'ready'
		  AND instance.lease_expires_at > ?
		  AND installation.status = 'active'
		  AND installation.public_endpoint <> ''
		ORDER BY instance.cell_id, instance.last_heartbeat_at DESC, instance.id`, ids, now).Scan(&directRows).Error; err != nil {
		return nil, fmt.Errorf("load direct Relay endpoints: %w", err)
	}
	for _, item := range directRows {
		if _, alreadySelected := result[item.CellID]; alreadySelected || !validDirectRelayEndpoint(item.PublicEndpoint) {
			continue
		}
		// The high bit keeps the per-installation configuration revision distinct
		// from relay_cell_endpoints.revision and guarantees a reconnect when the
		// direct endpoint or its TLS material is changed.
		result[item.CellID] = Endpoint{
			CellID: item.CellID, EndpointRevision: (uint64(item.Version) &^ (uint64(1) << 63)) | (uint64(1) << 63),
			URL: item.PublicEndpoint,
		}
	}
	return result, nil
}

func applyDirectRelayEndpoints(cells []Cell, endpoints map[uuid.UUID]Endpoint) {
	for index := range cells {
		cellID, err := uuid.Parse(cells[index].ID)
		if err != nil {
			continue
		}
		endpoint, ok := endpoints[cellID]
		if !ok {
			continue
		}
		cells[index].Endpoint = endpoint.URL
		cells[index].EndpointRevision = endpoint.EndpointRevision
		cells[index].EndpointActive = true
	}
}

func validDirectRelayEndpoint(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "ws" || parsed.Scheme == "wss") && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "/v2/connect" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func pendingOutboxID(tx *gorm.DB, assignmentID uuid.UUID) (uuid.UUID, error) {
	var event struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	if err := tx.Table("relay_outbox").Select("id").Where("aggregate_id = ? AND event_type = ? AND published_at IS NULL AND dead_lettered_at IS NULL", assignmentID, "relay.assignment.changed").Order("created_at DESC").Take(&event).Error; err != nil {
		return uuid.Nil, fmt.Errorf("load pending Relay assignment Outbox event: %w", err)
	}
	return event.ID, nil
}

func saveAllocationRequestKey(tx *gorm.DB, input prepareInput, assignmentID, outboxID uuid.UUID, now time.Time) error {
	row := allocationRequestKeyRow{
		ID: uuid.New(), UserID: input.UserID, DeviceID: input.DeviceID, Operation: input.Operation,
		IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash,
		AssignmentID: &assignmentID, OutboxID: &outboxID, CreatedAt: now, ExpiresAt: now.Add(requestKeyTTL),
	}
	if err := tx.Create(&row).Error; err != nil {
		return fmt.Errorf("save Relay allocation idempotency record: %w", err)
	}
	return nil
}

func requestDigest(value any) string {
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func decodeFallbacks(raw json.RawMessage) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	if len(raw) == 0 {
		return []uuid.UUID{}, nil
	}
	if err := json.Unmarshal(raw, &ids); err != nil || len(ids) > 8 {
		return nil, ErrAllocationUnavailable
	}
	return ids, nil
}

type allocationCredentialRow struct {
	DeviceID            uuid.UUID `gorm:"column:device_id;type:uuid;primaryKey"`
	UserID              uuid.UUID `gorm:"column:user_id;type:uuid"`
	ProtocolMin         int64     `gorm:"column:protocol_min"`
	ProtocolMax         int64     `gorm:"column:protocol_max"`
	IdentityPublicKey   []byte    `gorm:"column:identity_public_key"`
	PublicKeyThumbprint string    `gorm:"column:public_key_thumbprint"`
	GrantVersion        int64     `gorm:"column:grant_version"`
	Status              string    `gorm:"column:status"`
	LastConnectionEpoch int64     `gorm:"column:last_connection_epoch"`
}

func (allocationCredentialRow) TableName() string { return "remote_device_credentials" }

type allocationAssignmentRow struct {
	ID                uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	UserID            uuid.UUID       `gorm:"column:user_id;type:uuid"`
	CellID            uuid.UUID       `gorm:"column:cell_id;type:uuid"`
	AssignmentVersion int64           `gorm:"column:assignment_version"`
	Mode              string          `gorm:"column:mode"`
	Status            string          `gorm:"column:status"`
	FallbackCellIDs   json.RawMessage `gorm:"column:fallback_cell_ids;type:jsonb"`
	LeaseExpiresAt    time.Time       `gorm:"column:lease_expires_at"`
	EffectiveAt       *time.Time      `gorm:"column:effective_at"`
	SupersededAt      *time.Time      `gorm:"column:superseded_at"`
	CreatedAt         time.Time       `gorm:"column:created_at"`
	UpdatedAt         time.Time       `gorm:"column:updated_at"`
}

func (allocationAssignmentRow) TableName() string { return "relay_assignments" }

type allocationRequestKeyRow struct {
	ID             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID         uuid.UUID  `gorm:"column:user_id;type:uuid"`
	DeviceID       uuid.UUID  `gorm:"column:device_id;type:uuid"`
	Operation      string     `gorm:"column:operation"`
	IdempotencyKey string     `gorm:"column:idempotency_key"`
	RequestHash    string     `gorm:"column:request_hash"`
	AssignmentID   *uuid.UUID `gorm:"column:assignment_id;type:uuid"`
	OutboxID       *uuid.UUID `gorm:"column:outbox_id;type:uuid"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	ExpiresAt      time.Time  `gorm:"column:expires_at"`
}

func (allocationRequestKeyRow) TableName() string { return "remote_device_request_keys" }
