package remotecontrol

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store struct{ db *gorm.DB }

// A browser Peer ticket is only a bootstrap step before the long-lived Relay
// channel.  On a LAN it must fail fast rather than leaving the first screen
// waiting behind a degraded control-plane dependency.
const browserPeerIssueBudget = 750 * time.Millisecond

// Direct presence is refreshed by the Agent every 15 seconds. Three missed
// heartbeats fence a stale listener without coupling direct mode to Relay
// route publication.
const directPresenceTTL = 45 * time.Second

func NewStore(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("remote control database is required")
	}
	return &Store{db: db}, nil
}

type ServiceConfig struct {
	Database          *gorm.DB
	Store             *Store
	CursorKey         []byte
	PeerIssuer        PeerIssuer
	DeviceLinkIssuer  DeviceLinkIssuer
	DeviceLinkRevoker DeviceLinkGrantRevoker
	RouteResolver     DeviceRouteResolver
	Now               func() time.Time
}

// DeviceRouteResolver reports the current, Relay-maintained route for a
// device. A successful resolution denotes a live Relay connection: the
// registry removes expired routes and renews them from the Agent's Ping
// frames. It deliberately does not use allocation timestamps, because an
// allocation ticket may outlive the short UI presence window.
type DeviceRouteResolver interface {
	Resolve(deviceID string, now time.Time) (relayrouter.Route, error)
}

type Service struct {
	store             *Store
	cursors           cursorCodec
	peerIssuer        PeerIssuer
	deviceLinkIssuer  DeviceLinkIssuer
	deviceLinkRevoker DeviceLinkGrantRevoker
	routeResolver     DeviceRouteResolver
	now               func() time.Time
}

func NewService(config ServiceConfig) (*Service, error) {
	store := config.Store
	var err error
	if store == nil {
		store, err = NewStore(config.Database)
		if err != nil {
			return nil, err
		}
	}
	cursors, err := newCursorCodec(config.CursorKey)
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		store: store, cursors: cursors, peerIssuer: config.PeerIssuer, deviceLinkIssuer: config.DeviceLinkIssuer,
		deviceLinkRevoker: config.DeviceLinkRevoker, routeResolver: config.RouteResolver, now: now,
	}, nil
}

type deviceRow struct {
	DeviceID              uuid.UUID       `gorm:"column:device_id"`
	UserID                uuid.UUID       `gorm:"column:user_id"`
	DeviceName            string          `gorm:"column:device_name"`
	Platform              string          `gorm:"column:platform"`
	AgentVersion          string          `gorm:"column:agent_version"`
	CredentialStatus      string          `gorm:"column:credential_status"`
	GrantStatus           *string         `gorm:"column:grant_status"`
	Capabilities          json.RawMessage `gorm:"column:capabilities"`
	Scopes                json.RawMessage `gorm:"column:scopes"`
	GrantVersion          int64           `gorm:"column:grant_version"`
	LastAllocationAt      *time.Time      `gorm:"column:last_allocation_at"`
	LastSyncAt            *time.Time      `gorm:"column:last_sync_at"`
	RemoteEnabledAt       *time.Time      `gorm:"column:remote_enabled_at"`
	DirectModeEnabled     bool            `gorm:"column:direct_mode_enabled"`
	DirectEndpointEnabled bool            `gorm:"column:direct_endpoint_enabled"`
	DirectTLSEnabled      bool            `gorm:"column:direct_tls_enabled"`
	DirectIP              string          `gorm:"column:direct_ip"`
	DirectPort            int64           `gorm:"column:direct_port"`
	DirectConnectionEpoch int64           `gorm:"column:direct_connection_epoch"`
	DirectLastSeenAt      *time.Time      `gorm:"column:direct_last_seen_at"`
	UpdatedAt             time.Time       `gorm:"column:updated_at"`
}

func (service *Service) ListDevices(ctx context.Context, userID uuid.UUID, page PageRequest) (DevicePage, error) {
	if userID == uuid.Nil {
		return DevicePage{}, ErrInvalidInput
	}
	limit, err := normalizeLimit(page.Limit)
	if err != nil {
		return DevicePage{}, err
	}
	cursor, err := service.cursors.decode(page.Cursor, "devices")
	if err != nil {
		return DevicePage{}, err
	}
	query := service.store.db.WithContext(ctx).Raw(`
		SELECT credential.device_id, credential.user_id, credential.device_name, credential.platform, credential.agent_version,
		       credential.status AS credential_status, access_grant.status AS grant_status,
		       credential.capabilities, COALESCE(access_grant.scopes, '[]'::jsonb) AS scopes,
		       credential.grant_version, credential.last_allocation_at, sync.last_sync_at,
		       access_grant.enabled_at AS remote_enabled_at,
		       credential.direct_mode_enabled, credential.direct_endpoint_enabled, credential.direct_tls_enabled, credential.direct_ip,
		       credential.direct_port, credential.direct_connection_epoch, credential.direct_last_seen_at,
		       credential.updated_at
		FROM remote_device_credentials credential
		LEFT JOIN remote_access_grants access_grant ON access_grant.device_id = credential.device_id AND access_grant.user_id = credential.user_id
		LEFT JOIN remote_device_sync_state sync ON sync.device_id = credential.device_id
		WHERE credential.user_id = ?
		  AND (?::timestamptz IS NULL OR (credential.updated_at, credential.device_id) < (?, ?))
		ORDER BY credential.updated_at DESC, credential.device_id DESC
		LIMIT ?`, userID, nullableCursorTime(cursor), nullableCursorTime(cursor), nullableCursorID(cursor), limit+1)
	var rows []deviceRow
	if err := query.Scan(&rows).Error; err != nil {
		return DevicePage{}, fmt.Errorf("%w: list devices: %v", ErrUnavailable, err)
	}
	result := DevicePage{Items: make([]Device, 0, min(len(rows), limit)), ObservedAt: service.now().UTC()}
	for index, row := range rows {
		if index == limit {
			encoded, encodeErr := service.cursors.encode("devices", rows[index-1].UpdatedAt, rows[index-1].DeviceID)
			if encodeErr != nil {
				return DevicePage{}, ErrUnavailable
			}
			result.NextCursor = &encoded
			break
		}
		device, convertErr := service.deviceFromRow(row)
		if convertErr != nil {
			return DevicePage{}, convertErr
		}
		result.Items = append(result.Items, device)
	}
	return result, nil
}

func (service *Service) GetDevice(ctx context.Context, userID, deviceID uuid.UUID) (Device, error) {
	if userID == uuid.Nil || deviceID == uuid.Nil {
		return Device{}, ErrInvalidInput
	}
	var row deviceRow
	err := service.store.db.WithContext(ctx).Raw(`
		SELECT credential.device_id, credential.user_id, credential.device_name, credential.platform, credential.agent_version,
		       credential.status AS credential_status, access_grant.status AS grant_status,
		       credential.capabilities, COALESCE(access_grant.scopes, '[]'::jsonb) AS scopes,
		       credential.grant_version, credential.last_allocation_at, sync.last_sync_at,
		       access_grant.enabled_at AS remote_enabled_at,
		       credential.direct_mode_enabled, credential.direct_endpoint_enabled, credential.direct_tls_enabled, credential.direct_ip,
		       credential.direct_port, credential.direct_connection_epoch, credential.direct_last_seen_at,
		       credential.updated_at
		FROM remote_device_credentials credential
		LEFT JOIN remote_access_grants access_grant ON access_grant.device_id = credential.device_id AND access_grant.user_id = credential.user_id
		LEFT JOIN remote_device_sync_state sync ON sync.device_id = credential.device_id
		WHERE credential.user_id = ? AND credential.device_id = ?`, userID, deviceID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("%w: get device: %v", ErrUnavailable, err)
	}
	return service.deviceFromRow(row)
}

// UpdateDevice changes account-owned display and connection preferences.
// Device identity, platform, Agent version and direct endpoint coordinates
// remain authoritative Agent projections.
func (service *Service) UpdateDevice(ctx context.Context, input DeviceUpdateInput) (Device, error) {
	deviceName := strings.TrimSpace(input.DeviceName)
	if input.UserID == uuid.Nil || input.DeviceID == uuid.Nil || !utf8.ValidString(deviceName) ||
		deviceName == "" || utf8.RuneCountInString(deviceName) > 120 {
		return Device{}, ErrInvalidInput
	}
	now := service.now().UTC()
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row struct {
			DirectEndpointEnabled bool       `gorm:"column:direct_endpoint_enabled"`
			DirectLastSeenAt      *time.Time `gorm:"column:direct_last_seen_at"`
		}
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_device_credentials").
			Select("direct_endpoint_enabled, direct_last_seen_at").
			Where("user_id = ? AND device_id = ?", input.UserID, input.DeviceID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if input.DirectModeEnabled != nil && *input.DirectModeEnabled &&
			(!row.DirectEndpointEnabled || row.DirectLastSeenAt == nil || !row.DirectLastSeenAt.After(now.Add(-directPresenceTTL))) {
			return ErrDirectUnavailable
		}
		updates := map[string]any{"device_name": deviceName, "updated_at": now}
		if input.DirectModeEnabled != nil {
			updates["direct_mode_enabled"] = *input.DirectModeEnabled
		}
		result := tx.Table("remote_device_credentials").Where("user_id = ? AND device_id = ?", input.UserID, input.DeviceID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDirectUnavailable) {
			return Device{}, err
		}
		return Device{}, fmt.Errorf("%w: update device: %v", ErrUnavailable, err)
	}
	return service.GetDevice(ctx, input.UserID, input.DeviceID)
}

// DeleteDevice permanently removes a device's server-side remote-control
// projection. It also revokes the device's app sessions and any Access Key
// that was bound to it, so the removed agent must be explicitly enrolled again.
func (service *Service) DeleteDevice(ctx context.Context, input DeviceDeletionInput) error {
	if input.UserID == uuid.Nil || input.DeviceID == uuid.Nil {
		return ErrInvalidInput
	}
	now := service.now().UTC()
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var credential credentialLockRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_device_credentials").
			Where("device_id = ?", input.DeviceID).Take(&credential).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && credential.UserID != input.UserID) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load device for deletion: %w", err)
		}

		grantVersion := credential.GrantVersion + 1
		payload, _ := json.Marshal(map[string]any{
			"deviceId": input.DeviceID, "userId": input.UserID, "grantVersion": grantVersion,
			"status": "revoked", "identityPublicKey": base64.RawURLEncoding.EncodeToString(credential.IdentityPublicKey),
			"publicKeyThumbprint": credential.PublicKeyThumbprint,
		})
		if err := tx.Table("relay_outbox").Create(map[string]any{
			"id": uuid.New(), "aggregate_type": "remote_device", "aggregate_id": input.DeviceID,
			"event_type": "remote.device.changed", "payload": json.RawMessage(payload), "attempts": 0,
			"available_at": now, "published_at": now, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("append device deletion outbox event: %w", err)
		}
		if err := tx.Table("app_sessions").Where("user_id = ? AND device_id = ? AND revoked_at IS NULL", input.UserID, input.DeviceID).
			Updates(map[string]any{"revoked_at": now, "revoked_reason": "remote_device_deleted", "updated_at": now}).Error; err != nil {
			return fmt.Errorf("revoke deleted device sessions: %w", err)
		}
		if err := tx.Exec(`
			UPDATE app_refresh_tokens SET status = 'revoked'
			WHERE status = 'active' AND session_id IN (
				SELECT id FROM app_sessions WHERE user_id = ? AND device_id = ?
			)`, input.UserID, input.DeviceID).Error; err != nil {
			return fmt.Errorf("revoke deleted device refresh tokens: %w", err)
		}
		if err := tx.Table("remote_device_access_keys").
			Where("user_id = ? AND bound_device_id = ? AND status = 'active'", input.UserID, input.DeviceID).
			Updates(map[string]any{"status": "revoked", "revoked_at": now, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("revoke deleted device access keys: %w", err)
		}
		if err := tx.Exec("DELETE FROM remote_device_request_keys WHERE user_id = ? AND device_id = ?", input.UserID, input.DeviceID).Error; err != nil {
			return fmt.Errorf("delete device request keys: %w", err)
		}
		if err := tx.Exec("DELETE FROM remote_control_request_keys WHERE user_id = ? AND resource_id = ?", input.UserID, input.DeviceID).Error; err != nil {
			return fmt.Errorf("delete remote control request keys: %w", err)
		}
		result := tx.Exec("DELETE FROM remote_device_credentials WHERE user_id = ? AND device_id = ?", input.UserID, input.DeviceID)
		if result.Error != nil {
			return fmt.Errorf("delete device credential: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}
		return nil
	})
	return mapStoreError(err)
}

func (service *Service) deviceFromRow(row deviceRow) (Device, error) {
	var capabilities, scopes []string
	if json.Unmarshal(row.Capabilities, &capabilities) != nil || json.Unmarshal(row.Scopes, &scopes) != nil || row.GrantVersion < 1 {
		return Device{}, ErrUnavailable
	}
	var directIP *string
	var directPort *uint32
	if row.DirectEndpointEnabled {
		address, err := netip.ParseAddr(row.DirectIP)
		if err != nil || address.IsUnspecified() || address.IsMulticast() || row.DirectPort < 1 || row.DirectPort > 65535 ||
			row.DirectConnectionEpoch < 1 || row.DirectLastSeenAt == nil {
			return Device{}, ErrUnavailable
		}
		canonical := address.Unmap().String()
		port := uint32(row.DirectPort)
		directIP, directPort = &canonical, &port
	} else if row.DirectTLSEnabled || row.DirectIP != "" || row.DirectPort != 0 || row.DirectConnectionEpoch != 0 || row.DirectLastSeenAt != nil {
		return Device{}, ErrUnavailable
	}
	status := "pending_approval"
	if row.CredentialStatus == "quarantined" {
		status = "quarantined"
	} else if row.CredentialStatus == "revoked" || (row.GrantStatus != nil && *row.GrantStatus == "revoked") {
		status = "revoked"
	} else if row.GrantStatus != nil && *row.GrantStatus == "enabled" {
		status = "active"
	}
	presence, lastSeenAt := service.devicePresence(row)
	connectionMode := "relay"
	if row.DirectModeEnabled {
		connectionMode = "direct"
	}
	return Device{
		ID: row.DeviceID, InstallationDeviceID: row.DeviceID, DeviceName: row.DeviceName, Platform: row.Platform,
		AgentVersion: row.AgentVersion, Status: status, Presence: presence, Capabilities: capabilities, Scopes: scopes,
		GrantVersion: uint64(row.GrantVersion), LastSeenAt: lastSeenAt, LastSyncAt: utcPointer(row.LastSyncAt),
		RemoteEnabledAt: utcPointer(row.RemoteEnabledAt), ConnectionMode: connectionMode,
		DirectModeEnabled: row.DirectModeEnabled, DirectAvailable: service.directEndpointAvailable(row), DirectTLSEnabled: row.DirectTLSEnabled,
		DirectIP: directIP, DirectPort: directPort, UpdatedAt: row.UpdatedAt.UTC(),
	}, nil
}

func (service *Service) devicePresence(row deviceRow) (string, *time.Time) {
	// Preserve the most recent allocation as historical context when the
	// device is offline, but only a live Relay route may mark it online.
	lastSeenAt := latestTimePointer(row.LastAllocationAt, row.DirectLastSeenAt)
	if service.routeResolver == nil || row.DeviceID == uuid.Nil || row.UserID == uuid.Nil || row.CredentialStatus != "active" ||
		row.GrantStatus == nil || *row.GrantStatus != "enabled" {
		if row.DirectModeEnabled && service.directEndpointAvailable(row) && row.CredentialStatus == "active" && row.GrantStatus != nil && *row.GrantStatus == "enabled" {
			return "online", lastSeenAt
		}
		return "offline", lastSeenAt
	}
	if row.DirectModeEnabled {
		if row.CredentialStatus == "active" && row.GrantStatus != nil && *row.GrantStatus == "enabled" && service.directEndpointAvailable(row) {
			return "online", lastSeenAt
		}
		return "offline", lastSeenAt
	}
	now := service.now().UTC()
	route, err := service.routeResolver.Resolve(row.DeviceID.String(), now)
	if err != nil || route.DeviceID != row.DeviceID.String() || route.UserID != row.UserID.String() ||
		route.LastHeartbeatAt.IsZero() || !route.ExpiresAt.After(now) {
		return "offline", lastSeenAt
	}
	return "online", utcPointer(&route.LastHeartbeatAt)
}

func (service *Service) directEndpointAvailable(row deviceRow) bool {
	return row.DirectEndpointEnabled && row.DirectLastSeenAt != nil && row.DirectLastSeenAt.After(service.now().UTC().Add(-directPresenceTTL))
}

func latestTimePointer(values ...*time.Time) *time.Time {
	var latest *time.Time
	for _, value := range values {
		if value == nil || latest != nil && !value.After(*latest) {
			continue
		}
		copy := value.UTC()
		latest = &copy
	}
	return latest
}

type credentialLockRow struct {
	DeviceID            uuid.UUID `gorm:"column:device_id"`
	UserID              uuid.UUID `gorm:"column:user_id"`
	Status              string    `gorm:"column:status"`
	GrantVersion        int64     `gorm:"column:grant_version"`
	IdentityPublicKey   []byte    `gorm:"column:identity_public_key"`
	PublicKeyThumbprint string    `gorm:"column:public_key_thumbprint"`
}

type requestKeyRow struct {
	RequestHash   string `gorm:"column:request_hash"`
	ResultVersion int64  `gorm:"column:result_version"`
}

func (service *Service) EnableAccess(ctx context.Context, input AccessInput) (AccessResult, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.UserID == uuid.Nil || input.DeviceID == uuid.Nil || input.Confirmation != "enable_remote_access" || !validIdempotencyKey(input.IdempotencyKey) {
		return AccessResult{}, ErrInvalidInput
	}
	// Access is an on/off switch for the authenticated account's device.  Keep
	// the legacy scopes column empty for compatibility, but never use submitted
	// operation labels as a permission decision.
	scopes := []string{}
	hash := requestHash(struct {
		Enabled bool `json:"enabled"`
	}{true})
	return service.changeAccess(ctx, input.UserID, input.DeviceID, "access.enable", input.IdempotencyKey, hash, scopes)
}

func (service *Service) RevokeAccess(ctx context.Context, input AccessInput) (AccessResult, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.UserID == uuid.Nil || input.DeviceID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) {
		return AccessResult{}, ErrInvalidInput
	}
	return service.changeAccess(ctx, input.UserID, input.DeviceID, "access.revoke", input.IdempotencyKey, requestHash(struct{}{}), nil)
}

func (service *Service) changeAccess(ctx context.Context, userID, deviceID uuid.UUID, operation, idempotencyKey, hash string, scopes []string) (AccessResult, error) {
	var result AccessResult
	now := service.now().UTC()
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var credential credentialLockRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_device_credentials").
			Where("device_id = ?", deviceID).Take(&credential).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && credential.UserID != userID) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load access credential: %w", err)
		}
		if credential.Status == "quarantined" {
			return ErrForbidden
		}
		var prior requestKeyRow
		priorErr := tx.Table("remote_control_request_keys").Select("request_hash, result_version").
			Where("user_id = ? AND resource_id = ? AND operation = ? AND idempotency_key = ?",
				userID, deviceID, operation, idempotencyKey).Take(&prior).Error
		if priorErr == nil {
			if prior.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			var grantScopes json.RawMessage
			var grantStatus string
			if err := tx.Table("remote_access_grants").Select("scopes, status").Where("device_id = ?", deviceID).
				Row().Scan(&grantScopes, &grantStatus); err != nil {
				return fmt.Errorf("load replayed access grant: %w", err)
			}
			var decoded []string
			_ = json.Unmarshal(grantScopes, &decoded)
			result = AccessResult{DeviceID: deviceID, Status: grantStatus, Scopes: decoded, GrantVersion: uint64(prior.ResultVersion), Replayed: true}
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load access idempotency key: %w", priorErr)
		}

		grantVersion := credential.GrantVersion + 1
		status := "enabled"
		credentialStatus := "active"
		var enabledAt, revokedAt any = now, nil
		if operation == "access.revoke" {
			status, credentialStatus = "revoked", "revoked"
			scopes, enabledAt, revokedAt = []string{}, nil, now
		}
		scopesJSON, _ := json.Marshal(scopes)
		if err := tx.Exec(`
			INSERT INTO remote_access_grants
			    (device_id, user_id, scopes, status, grant_version, enabled_at, revoked_at, created_at, updated_at)
			VALUES (?, ?, ?::jsonb, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (device_id) DO UPDATE SET scopes = EXCLUDED.scopes, status = EXCLUDED.status,
			    grant_version = EXCLUDED.grant_version, enabled_at = EXCLUDED.enabled_at,
			    revoked_at = EXCLUDED.revoked_at, updated_at = EXCLUDED.updated_at`,
			deviceID, userID, string(scopesJSON), status, grantVersion, enabledAt, revokedAt, now, now).Error; err != nil {
			return fmt.Errorf("save access grant: %w", err)
		}
		if err := tx.Table("remote_device_credentials").Where("device_id = ?", deviceID).
			Updates(map[string]any{"grant_version": grantVersion, "status": credentialStatus, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("fence device credential: %w", err)
		}
		if operation == "access.revoke" {
			if err := tx.Table("remote_commands").Where("device_id = ? AND status IN ?", deviceID, []string{"queued", "leased", "accepted"}).
				Updates(map[string]any{"status": "cancelled", "completed_at": now, "lease_token": nil, "lease_expires_at": nil, "updated_at": now}).Error; err != nil {
				return fmt.Errorf("cancel revoked commands: %w", err)
			}
			if err := tx.Table("remote_tasks").Where("device_id = ? AND status NOT IN ?", deviceID,
				[]string{"cancelled", "succeeded", "failed", "rejected", "expired", "timed_out"}).
				Updates(map[string]any{"status": "cancelled", "finished_at": now, "updated_at": now}).Error; err != nil {
				return fmt.Errorf("cancel revoked tasks: %w", err)
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"deviceId": deviceID, "userId": userID, "grantVersion": grantVersion,
			"status": credentialStatus, "identityPublicKey": base64.RawURLEncoding.EncodeToString(credential.IdentityPublicKey),
			"publicKeyThumbprint": credential.PublicKeyThumbprint,
		})
		if err := tx.Table("relay_outbox").Create(map[string]any{
			"id": uuid.New(), "aggregate_type": "remote_device", "aggregate_id": deviceID,
			"event_type": "remote.device.changed", "payload": json.RawMessage(payload), "attempts": 0,
			"available_at": now, "published_at": now, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("append access Outbox event: %w", err)
		}
		if err := tx.Table("remote_control_request_keys").Create(map[string]any{
			"user_id": userID, "resource_id": deviceID, "operation": operation, "idempotency_key": idempotencyKey,
			"request_hash": hash, "result_version": grantVersion, "created_at": now, "expires_at": now.Add(24 * time.Hour),
		}).Error; err != nil {
			return fmt.Errorf("save access idempotency key: %w", err)
		}
		result = AccessResult{DeviceID: deviceID, Status: status, Scopes: scopes, GrantVersion: uint64(grantVersion)}
		return nil
	})
	if err != nil {
		return AccessResult{}, mapStoreError(err)
	}
	return result, nil
}

type controllerRow struct {
	ControllerID      uuid.UUID       `gorm:"column:controller_id"`
	UserID            uuid.UUID       `gorm:"column:user_id"`
	IdentityPublicKey []byte          `gorm:"column:identity_public_key"`
	Thumbprint        string          `gorm:"column:public_key_thumbprint"`
	KeyVersion        int64           `gorm:"column:key_version"`
	GrantVersion      int64           `gorm:"column:grant_version"`
	Scopes            json.RawMessage `gorm:"column:scopes"`
	Status            string          `gorm:"column:status"`
	LastUsedAt        *time.Time      `gorm:"column:last_used_at"`
	CreatedAt         time.Time       `gorm:"column:created_at"`
	UpdatedAt         time.Time       `gorm:"column:updated_at"`
}

// browserPeerAuthorizationRow deliberately joins the independent controller
// and target checks.  Issuing a ticket used to make those two point reads in
// series, which added a full database round trip to every cold Peer channel.
// They are both required to authorize the same ticket and return the same
// externally visible not-found result, so one consistent query is sufficient.
type browserPeerAuthorizationRow struct {
	ControllerIdentityPublicKey []byte    `gorm:"column:controller_identity_public_key"`
	ControllerThumbprint        string    `gorm:"column:controller_key_thumbprint"`
	ControllerKeyVersion        int64     `gorm:"column:controller_key_version"`
	ControllerGrantVersion      int64     `gorm:"column:controller_grant_version"`
	ControllerStatus            string    `gorm:"column:controller_status"`
	TargetUserID                uuid.UUID `gorm:"column:target_user_id"`
	TargetIdentityPublicKey     []byte    `gorm:"column:target_identity_public_key"`
	TargetThumbprint            string    `gorm:"column:target_key_thumbprint"`
	TargetGrantVersion          int64     `gorm:"column:target_grant_version"`
	TargetKeyVersion            int64     `gorm:"column:target_key_version"`
	TargetCredentialStatus      string    `gorm:"column:target_credential_status"`
	TargetGrantStatus           string    `gorm:"column:target_grant_status"`
	ProjectAvailable            bool      `gorm:"column:project_available"`
}

func (service *Service) RegisterController(ctx context.Context, input RegisterControllerInput) (ControllerIdentity, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	// Controller keys authenticate an encrypted peer; requested operation
	// labels are not grants and are intentionally ignored.
	scopes := []string{}
	publicKey, thumbprint, keyErr := decodePublicKey(input.IdentityPublicKey)
	if keyErr != nil || input.UserID == uuid.Nil || input.ControllerID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) {
		return ControllerIdentity{}, ErrInvalidInput
	}
	if err := verifyControllerProof(input.UserID, input.ControllerID, publicKey, strings.TrimSpace(input.IdentityPublicKey), input.Proof, 1); err != nil {
		return ControllerIdentity{}, err
	}
	hash := requestHash(struct {
		Thumbprint string   `json:"thumbprint"`
		Scopes     []string `json:"scopes"`
	}{thumbprint, scopes})
	now := service.now().UTC()
	var row controllerRow
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", input.ControllerID.String()).Error; err != nil {
			return fmt.Errorf("lock controller identity: %w", err)
		}
		var prior requestKeyRow
		priorErr := tx.Table("remote_control_request_keys").Select("request_hash, result_version").
			Where("user_id = ? AND resource_id = ? AND operation = 'controller.register' AND idempotency_key = ?",
				input.UserID, input.ControllerID, input.IdempotencyKey).Take(&prior).Error
		if priorErr == nil {
			if prior.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			return tx.Table("remote_controller_identities").Where("controller_id = ? AND user_id = ?", input.ControllerID, input.UserID).Take(&row).Error
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return priorErr
		}
		var existing controllerRow
		existingErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_controller_identities").Where("controller_id = ?", input.ControllerID).Take(&existing).Error
		if existingErr == nil {
			if existing.UserID != input.UserID {
				return ErrForbidden
			}
			return ErrConflict
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}
		scopesJSON, _ := json.Marshal(scopes)
		row = controllerRow{
			ControllerID: input.ControllerID, UserID: input.UserID, IdentityPublicKey: append([]byte(nil), publicKey...),
			Thumbprint: thumbprint, KeyVersion: 1, GrantVersion: 1, Scopes: scopesJSON, Status: "active", CreatedAt: now, UpdatedAt: now,
		}
		registration := map[string]any{
			"controller_id": input.ControllerID, "user_id": input.UserID,
			"identity_public_key": []byte(publicKey), "public_key_thumbprint": thumbprint, "key_version": 1,
			"grant_version": 1, "scopes": string(scopesJSON), "status": "active", "created_at": now, "updated_at": now,
		}
		// This column references browser sessions. Native bearer sessions use
		// app_sessions, so they intentionally leave it unset.
		if input.SessionID != uuid.Nil {
			registration["registered_session_id"] = input.SessionID
		}
		if err := tx.Table("remote_controller_identities").Create(registration).Error; err != nil {
			return fmt.Errorf("create controller identity: %w", err)
		}
		if err := service.saveControlRequestKey(tx, input.UserID, input.ControllerID, "controller.register", input.IdempotencyKey, hash, 1, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return ControllerIdentity{}, mapStoreError(err)
	}
	return controllerFromRow(row)
}

func (service *Service) RotateController(ctx context.Context, input RotateControllerInput) (ControllerIdentity, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	publicKey, thumbprint, err := decodePublicKey(input.IdentityPublicKey)
	if err != nil || input.UserID == uuid.Nil || input.ControllerID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) {
		return ControllerIdentity{}, ErrInvalidInput
	}
	now := service.now().UTC()
	var result controllerRow
	err = service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_controller_identities").
			Where("controller_id = ?", input.ControllerID).Take(&result).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if result.UserID != input.UserID {
			return ErrNotFound
		}
		var prior requestKeyRow
		priorErr := tx.Table("remote_control_request_keys").Select("request_hash, result_version").
			Where("user_id = ? AND resource_id = ? AND operation = 'controller.rotate' AND idempotency_key = ?",
				input.UserID, input.ControllerID, input.IdempotencyKey).Take(&prior).Error
		if priorErr == nil {
			version := uint64(prior.ResultVersion)
			if err := verifyControllerProof(input.UserID, input.ControllerID, publicKey, strings.TrimSpace(input.IdentityPublicKey), input.Proof, version); err != nil {
				return err
			}
			if prior.RequestHash != requestHash(struct {
				Thumbprint string `json:"thumbprint"`
				Version    uint64 `json:"version"`
			}{thumbprint, version}) {
				return ErrIdempotencyConflict
			}
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return priorErr
		}
		nextVersion := uint64(result.KeyVersion + 1)
		if err := verifyControllerProof(input.UserID, input.ControllerID, publicKey, strings.TrimSpace(input.IdentityPublicKey), input.Proof, nextVersion); err != nil {
			return err
		}
		hash := requestHash(struct {
			Thumbprint string `json:"thumbprint"`
			Version    uint64 `json:"version"`
		}{thumbprint, nextVersion})
		if result.Status != "active" {
			return ErrForbidden
		}
		result.IdentityPublicKey = append([]byte(nil), publicKey...)
		result.Thumbprint = thumbprint
		result.KeyVersion++
		result.GrantVersion++
		result.UpdatedAt = now
		updates := map[string]any{
			"identity_public_key": []byte(publicKey), "public_key_thumbprint": thumbprint,
			"key_version": result.KeyVersion, "grant_version": result.GrantVersion, "updated_at": now,
		}
		if input.SessionID != uuid.Nil {
			updates["registered_session_id"] = input.SessionID
		}
		if err := tx.Table("remote_controller_identities").Where("controller_id = ?", input.ControllerID).Updates(updates).Error; err != nil {
			return err
		}
		return service.saveControlRequestKey(tx, input.UserID, input.ControllerID, "controller.rotate", input.IdempotencyKey, hash, result.KeyVersion, now)
	})
	if err != nil {
		return ControllerIdentity{}, mapStoreError(err)
	}
	return controllerFromRow(result)
}

func (service *Service) RevokeController(ctx context.Context, input RevokeControllerInput) (ControllerIdentity, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.UserID == uuid.Nil || input.ControllerID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) {
		return ControllerIdentity{}, ErrInvalidInput
	}
	now := service.now().UTC()
	hash := requestHash(struct{}{})
	var result controllerRow
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_controller_identities").Where("controller_id = ?", input.ControllerID).Take(&result).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if result.UserID != input.UserID {
			return ErrNotFound
		}
		var prior requestKeyRow
		priorErr := tx.Table("remote_control_request_keys").Select("request_hash, result_version").
			Where("user_id = ? AND resource_id = ? AND operation = 'controller.revoke' AND idempotency_key = ?",
				input.UserID, input.ControllerID, input.IdempotencyKey).Take(&prior).Error
		if priorErr == nil {
			if prior.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return priorErr
		}
		result.Status = "revoked"
		result.GrantVersion++
		result.UpdatedAt = now
		if err := tx.Table("remote_controller_identities").Where("controller_id = ?", input.ControllerID).Updates(map[string]any{
			"status": "revoked", "grant_version": result.GrantVersion, "revoked_at": now, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		return service.saveControlRequestKey(tx, input.UserID, input.ControllerID, "controller.revoke", input.IdempotencyKey, hash, result.GrantVersion, now)
	})
	if err != nil {
		return ControllerIdentity{}, mapStoreError(err)
	}
	return controllerFromRow(result)
}

func (service *Service) GetController(ctx context.Context, userID, controllerID uuid.UUID) (ControllerIdentity, error) {
	if userID == uuid.Nil || controllerID == uuid.Nil {
		return ControllerIdentity{}, ErrInvalidInput
	}
	var row controllerRow
	if err := service.store.db.WithContext(ctx).Table("remote_controller_identities").Where("controller_id = ? AND user_id = ?", controllerID, userID).Take(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return ControllerIdentity{}, ErrNotFound
	} else if err != nil {
		return ControllerIdentity{}, ErrUnavailable
	}
	return controllerFromRow(row)
}

func (service *Service) IssueBrowserPeer(ctx context.Context, input BrowserPeerInput) (PeerSession, error) {
	input.Scope = strings.TrimSpace(input.Scope)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if service.peerIssuer == nil {
		return PeerSession{}, ErrUnavailable
	}
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.ControllerID == uuid.Nil || input.TargetDeviceID == uuid.Nil ||
		!validIdempotencyKey(input.IdempotencyKey) || (input.ProjectID != nil && *input.ProjectID == uuid.Nil) ||
		(peerScopeRequiresProject(input.Scope) && input.ProjectID == nil) || (!peerScopeRequiresProject(input.Scope) && input.ProjectID != nil) {
		return PeerSession{}, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, browserPeerIssueBudget)
	defer cancel()
	var projectID any
	if input.ProjectID != nil {
		projectID = *input.ProjectID
	}
	var authorization browserPeerAuthorizationRow
	err := service.store.db.WithContext(ctx).Raw(`
		SELECT controller.identity_public_key AS controller_identity_public_key,
		       controller.public_key_thumbprint AS controller_key_thumbprint,
		       controller.key_version AS controller_key_version,
		       controller.grant_version AS controller_grant_version,
		       controller.status AS controller_status,
		       credential.user_id AS target_user_id,
		       credential.identity_public_key AS target_identity_public_key,
		       credential.public_key_thumbprint AS target_key_thumbprint,
		       credential.grant_version AS target_grant_version,
		       credential.key_version AS target_key_version,
		       credential.status AS target_credential_status,
		       access_grant.status AS target_grant_status,
		       CASE WHEN ?::uuid IS NULL THEN TRUE ELSE EXISTS (
		           SELECT 1 FROM remote_projects project
		           WHERE project.device_id = credential.device_id
		             AND project.user_id = controller.user_id
		             AND project.project_id = ?::uuid
		             AND project.state = 'available'
		       ) END AS project_available
		FROM remote_controller_identities controller
		JOIN remote_device_credentials credential ON credential.device_id = ?
		JOIN remote_access_grants access_grant
		  ON access_grant.device_id = credential.device_id AND access_grant.user_id = credential.user_id
		WHERE controller.controller_id = ? AND controller.user_id = ?`,
		projectID, projectID, input.TargetDeviceID, input.ControllerID, input.UserID,
	).Take(&authorization).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PeerSession{}, ErrNotFound
	}
	if err != nil {
		return PeerSession{}, ErrUnavailable
	}
	// A controller identity authenticates the E2EE source. Operation-specific
	// Relay scopes are deliberately not an authorization boundary: once the
	// signed-in account owns an enabled target device, every Peer RPC is
	// available through that encrypted connection.
	if authorization.ControllerStatus != "active" {
		return PeerSession{}, ErrForbidden
	}
	if authorization.TargetUserID != input.UserID {
		return PeerSession{}, ErrNotFound
	}
	if authorization.TargetCredentialStatus != "active" || authorization.TargetGrantStatus != "enabled" || authorization.TargetKeyVersion < 1 || authorization.TargetGrantVersion < 1 {
		return PeerSession{}, ErrForbidden
	}
	if !authorization.ProjectAvailable {
		return PeerSession{}, ErrNotFound
	}
	result, err := service.peerIssuer.IssueBrowserPeer(ctx, PeerIssueInput{
		UserID: input.UserID, SessionID: input.SessionID, ControllerID: input.ControllerID,
		ControllerPublicKey:     ed25519.PublicKey(append([]byte(nil), authorization.ControllerIdentityPublicKey...)),
		ControllerKeyThumbprint: authorization.ControllerThumbprint, ControllerKeyVersion: uint64(authorization.ControllerKeyVersion), ControllerGrantVersion: uint64(authorization.ControllerGrantVersion),
		TargetDeviceID: input.TargetDeviceID, TargetPublicKey: ed25519.PublicKey(append([]byte(nil), authorization.TargetIdentityPublicKey...)),
		TargetKeyThumbprint: authorization.TargetThumbprint, TargetKeyVersion: uint64(authorization.TargetKeyVersion), TargetGrantVersion: uint64(authorization.TargetGrantVersion), Scope: input.Scope, ProjectID: input.ProjectID,
		RequestedMaxDurationSeconds: input.RequestedMaxDurationSeconds, RequestedMaxBytes: input.RequestedMaxBytes,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		return PeerSession{}, err
	}
	now := service.now().UTC()
	// last_used_at is audit metadata, not an authorization condition.  Never
	// hold an already-issued ticket behind this write: the ticket, Relay
	// challenge, and Agent all enforce the grant/key fence independently.
	go service.touchControllerLastUsed(input.ControllerID, input.UserID, authorization.ControllerGrantVersion, now)
	return result, nil
}

// IssueDeviceLink creates a reusable, proof-of-possession v2 Carrier Grant.
// Unlike the former Peer ticket API it has no project or operation scope: the
// caller can only use it together with its Controller private key to establish
// an encrypted Link to this Device. It remains valid until explicit revocation
// or an identity/authorization version change unless a bounded TTL is requested.
func (service *Service) IssueDeviceLink(ctx context.Context, input DeviceLinkInput) (DeviceLink, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if service.deviceLinkIssuer == nil || service.deviceLinkRevoker == nil {
		return DeviceLink{}, ErrUnavailable
	}
	if input.UserID == uuid.Nil || input.ControllerID == uuid.Nil || input.TargetDeviceID == uuid.Nil || input.ClientIdentityKeyVersion == 0 || !validIdempotencyKey(input.IdempotencyKey) {
		return DeviceLink{}, ErrInvalidInput
	}
	if input.RequestedMaximumLifetimeSec != nil && (*input.RequestedMaximumLifetimeSec == 0 || *input.RequestedMaximumLifetimeSec > 15*60) {
		return DeviceLink{}, ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, browserPeerIssueBudget)
	defer cancel()
	var authorization browserPeerAuthorizationRow
	err := service.store.db.WithContext(ctx).Raw(`
		SELECT controller.identity_public_key AS controller_identity_public_key,
		       controller.public_key_thumbprint AS controller_key_thumbprint,
		       controller.key_version AS controller_key_version,
		       controller.grant_version AS controller_grant_version,
		       controller.status AS controller_status,
		       credential.user_id AS target_user_id,
		       credential.identity_public_key AS target_identity_public_key,
		       credential.public_key_thumbprint AS target_key_thumbprint,
		       credential.grant_version AS target_grant_version,
		       credential.key_version AS target_key_version,
		       credential.status AS target_credential_status,
		       access_grant.status AS target_grant_status
		FROM remote_controller_identities controller
		JOIN remote_device_credentials credential ON credential.device_id = ?
		JOIN remote_access_grants access_grant
		  ON access_grant.device_id = credential.device_id AND access_grant.user_id = credential.user_id
		WHERE controller.controller_id = ? AND controller.user_id = ?`,
		input.TargetDeviceID, input.ControllerID, input.UserID,
	).Take(&authorization).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DeviceLink{}, ErrNotFound
	}
	if err != nil {
		return DeviceLink{}, ErrUnavailable
	}
	if authorization.ControllerStatus != "active" || authorization.TargetUserID != input.UserID {
		return DeviceLink{}, ErrForbidden
	}
	if authorization.ControllerKeyVersion < 1 || uint64(authorization.ControllerKeyVersion) != input.ClientIdentityKeyVersion || authorization.ControllerGrantVersion < 1 ||
		authorization.TargetCredentialStatus != "active" || authorization.TargetGrantStatus != "enabled" ||
		authorization.TargetKeyVersion < 1 || authorization.TargetGrantVersion < 1 {
		return DeviceLink{}, ErrForbidden
	}
	result, err := service.deviceLinkIssuer.IssueDeviceLink(ctx, DeviceLinkIssueInput{
		UserID: input.UserID, SessionID: input.SessionID, ControllerID: input.ControllerID,
		ControllerPublicKey:     ed25519.PublicKey(append([]byte(nil), authorization.ControllerIdentityPublicKey...)),
		ControllerKeyThumbprint: authorization.ControllerThumbprint, ControllerKeyVersion: uint64(authorization.ControllerKeyVersion), ControllerGrantVersion: uint64(authorization.ControllerGrantVersion),
		TargetDeviceID: input.TargetDeviceID, TargetPublicKey: ed25519.PublicKey(append([]byte(nil), authorization.TargetIdentityPublicKey...)),
		TargetKeyThumbprint: authorization.TargetThumbprint, TargetKeyVersion: uint64(authorization.TargetKeyVersion), TargetGrantVersion: uint64(authorization.TargetGrantVersion),
		AllowedScopes:               deviceLinkAllowedScopes(),
		RequestedMaximumLifetimeSec: input.RequestedMaximumLifetimeSec, IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		return DeviceLink{}, err
	}
	if err := service.recordDeviceLinkGrant(ctx, input, result); err != nil {
		return DeviceLink{}, ErrUnavailable
	}
	go service.touchControllerLastUsed(input.ControllerID, input.UserID, authorization.ControllerGrantVersion, service.now().UTC())
	return result, nil
}

// RevokeDeviceLink prevents a still-valid DeviceConnectionGrant from being
// accepted by any Relay. It deliberately accepts only the non-bearer GrantID:
// the persistent record ties that ID to the authenticated controller, while
// the Relay store receives only a digest of the ID.
func (service *Service) RevokeDeviceLink(ctx context.Context, input DeviceLinkRevocationInput) error {
	if service == nil || service.store == nil || service.store.db == nil || service.deviceLinkRevoker == nil ||
		input.UserID == uuid.Nil || input.ControllerID == uuid.Nil || input.GrantID == uuid.Nil {
		return ErrInvalidInput
	}
	ctx, cancel := context.WithTimeout(ctx, browserPeerIssueBudget)
	defer cancel()
	var grant struct {
		DeviceID  uuid.UUID `gorm:"column:device_id"`
		ExpiresAt time.Time `gorm:"column:expires_at"`
	}
	err := service.store.db.WithContext(ctx).Raw(`
		SELECT device_id, expires_at
		FROM remote_device_link_grants
		WHERE grant_id = ? AND user_id = ? AND controller_id = ?`,
		input.GrantID, input.UserID, input.ControllerID,
	).Take(&grant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	if err != nil || grant.DeviceID == uuid.Nil || grant.ExpiresAt.IsZero() {
		return ErrUnavailable
	}
	now := service.now().UTC()
	if grant.ExpiresAt.After(now) {
		if err := service.deviceLinkRevoker.RevokeDeviceLinkGrant(input.GrantID.String(), grant.ExpiresAt.UTC()); err != nil {
			return ErrUnavailable
		}
	}
	if err := service.store.db.WithContext(ctx).Table("remote_device_link_grants").
		Where("grant_id = ? AND user_id = ? AND controller_id = ? AND revoked_at IS NULL", input.GrantID, input.UserID, input.ControllerID).
		Updates(map[string]any{"revoked_at": now, "updated_at": now}).Error; err != nil {
		return ErrUnavailable
	}
	return nil
}

func (service *Service) recordDeviceLinkGrant(ctx context.Context, input DeviceLinkInput, link DeviceLink) error {
	if service == nil || service.store == nil || service.store.db == nil || link.GrantID == uuid.Nil || link.ExpiresAt.IsZero() ||
		!link.ExpiresAt.After(service.now().UTC()) {
		return ErrUnavailable
	}
	// Prune only bounded legacy Grants. Persistent PoP Grants use the protocol
	// maximum timestamp and remain addressable for explicit revocation without
	// retaining Grant tokens or Link secrets.
	now := service.now().UTC()
	if err := service.store.db.WithContext(ctx).Exec("DELETE FROM remote_device_link_grants WHERE expires_at <= ?", now).Error; err != nil {
		return ErrUnavailable
	}
	result := service.store.db.WithContext(ctx).Exec(`
		INSERT INTO remote_device_link_grants
		    (grant_id, user_id, controller_id, device_id, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		link.GrantID, input.UserID, input.ControllerID, input.TargetDeviceID, link.ExpiresAt.UTC(), now, now)
	if result.Error != nil || result.RowsAffected != 1 {
		return ErrUnavailable
	}
	return nil
}

func (service *Service) touchControllerLastUsed(controllerID, userID uuid.UUID, grantVersion int64, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = service.store.db.WithContext(ctx).Table("remote_controller_identities").
		Where("controller_id = ? AND user_id = ? AND grant_version = ? AND status = 'active'", controllerID, userID, grantVersion).
		Updates(map[string]any{"last_used_at": now, "updated_at": now}).Error
}

func (service *Service) saveControlRequestKey(tx *gorm.DB, userID, resourceID uuid.UUID, operation, key, hash string, resultVersion int64, now time.Time) error {
	if err := tx.Table("remote_control_request_keys").Create(map[string]any{
		"user_id": userID, "resource_id": resourceID, "operation": operation, "idempotency_key": key,
		"request_hash": hash, "result_version": resultVersion, "created_at": now, "expires_at": now.Add(24 * time.Hour),
	}).Error; err != nil {
		return fmt.Errorf("save remote control idempotency key: %w", err)
	}
	return nil
}

func controllerFromRow(row controllerRow) (ControllerIdentity, error) {
	var scopes []string
	if row.ControllerID == uuid.Nil || len(row.IdentityPublicKey) != ed25519.PublicKeySize || row.KeyVersion < 1 || row.GrantVersion < 1 || json.Unmarshal(row.Scopes, &scopes) != nil {
		return ControllerIdentity{}, ErrUnavailable
	}
	return ControllerIdentity{
		ID: row.ControllerID, IdentityAlgorithm: "Ed25519", IdentityPublicKey: base64.RawURLEncoding.EncodeToString(row.IdentityPublicKey),
		PublicKeyThumbprint: row.Thumbprint, KeyVersion: uint64(row.KeyVersion), GrantVersion: uint64(row.GrantVersion),
		Scopes: scopes, Status: row.Status, LastUsedAt: utcPointer(row.LastUsedAt), CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}, nil
}

func nullableCursorTime(cursor cursorPayload) any {
	if cursor.ID == uuid.Nil {
		return nil
	}
	return cursor.Time.UTC()
}

func nullableCursorID(cursor cursorPayload) any {
	if cursor.ID == uuid.Nil {
		return nil
	}
	return cursor.ID
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func mapStoreError(err error) error {
	if err == nil || errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrConflict) || errors.Is(err, ErrIdempotencyConflict) || errors.Is(err, ErrSequenceGap) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}
