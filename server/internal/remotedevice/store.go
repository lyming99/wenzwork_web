package remotedevice

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/deviceaccesskey"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalidInput        = errors.New("remote device input is invalid")
	ErrForbidden           = errors.New("remote device does not belong to authenticated user")
	ErrKeyRotationRequired = errors.New("remote device key rotation is required")
	ErrIdempotencyConflict = errors.New("remote device idempotency key conflicts with another request")
	ErrUnavailable         = errors.New("remote device service is unavailable")
)

var idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

var allowedDeviceCapabilities = map[string]struct{}{
	"relay.ping":                       {},
	"remote.project.sync":              {},
	"remote.task.workspace.inspect":    {},
	"remote.task.markdown.render":      {},
	"remote.task.ai.summarize":         {},
	"remote.peer.query":                {},
	"remote.peer.ai.config":            {},
	"remote.peer.ai.chat":              {},
	"remote.peer.ai.tools":             {},
	"remote.peer.terminal":             {},
	"remote.peer.terminal.interactive": {},
	"remote.peer.file.send":            {},
	"remote.peer.file.receive":         {},
	"remote.peer.task.control":         {},
	"remote.peer.events":               {},
}

var allowedDeviceScopes = map[string]struct{}{
	"remote.connect":                   {},
	"remote.peer.query":                {},
	"remote.peer.ai.config":            {},
	"remote.peer.ai.chat":              {},
	"remote.peer.ai.tools":             {},
	"remote.peer.terminal":             {},
	"remote.peer.terminal.interactive": {},
	"remote.peer.file.send":            {},
	"remote.peer.file.receive":         {},
	"remote.peer.task.control":         {},
	"remote.peer.events":               {},
}

type Credential struct {
	DeviceID            uuid.UUID
	UserID              uuid.UUID
	RegisteredSessionID uuid.UUID
	DeviceName          string
	Platform            string
	AgentVersion        string
	ProtocolMin         uint32
	ProtocolMax         uint32
	Capabilities        []string
	Scopes              []string
	IdentityPublicKey   ed25519.PublicKey
	PublicKeyThumbprint string
	KeyVersion          uint64
	GrantVersion        uint64
	Status              string
	LastConnectionEpoch uint64
	LastAllocationAt    *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type RegisterInput struct {
	UserID            uuid.UUID
	SessionID         uuid.UUID
	DeviceID          uuid.UUID
	IdempotencyKey    string
	DeviceName        string
	Platform          string
	AgentVersion      string
	ProtocolMin       uint32
	ProtocolMax       uint32
	Capabilities      []string
	IdentityAlgorithm string
	IdentityPublicKey string
	Proof             string
}

type Registration struct {
	Credential Credential
	Created    bool
}

type Store struct {
	db         *gorm.DB
	now        func() time.Time
	accessKeys *deviceaccesskey.Store
	policy     *remoteaccesspolicy.Store
}

type Service struct {
	store *Store
}

type StoreOption func(*storeOptions)

type storeOptions struct {
	accessKeyIdempotencyEncryptionKey []byte
	accessPolicy                      *remoteaccesspolicy.Store
}

func WithAccessKeyIdempotencyEncryptionKey(key string) StoreOption {
	return func(options *storeOptions) {
		options.accessKeyIdempotencyEncryptionKey = []byte(key)
	}
}

func WithAccessPolicy(policy *remoteaccesspolicy.Store) StoreOption {
	return func(options *storeOptions) {
		options.accessPolicy = policy
	}
}

func NewStore(db *gorm.DB, configure ...StoreOption) (*Store, error) {
	if db == nil {
		return nil, errors.New("remote device database is required")
	}
	options := storeOptions{}
	for _, apply := range configure {
		if apply != nil {
			apply(&options)
		}
	}
	if options.accessPolicy == nil {
		return nil, errors.New("remote device access policy is required")
	}
	accessKeys, err := deviceaccesskey.NewStore(db,
		deviceaccesskey.WithIdempotencyEncryptionKey(options.accessKeyIdempotencyEncryptionKey),
		deviceaccesskey.WithAccessPolicy(options.accessPolicy),
	)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }, accessKeys: accessKeys, policy: options.accessPolicy}, nil
}

func NewService(store *Store) (*Service, error) {
	if store == nil {
		return nil, errors.New("remote device store is required")
	}
	return &Service{store: store}, nil
}

func (service *Service) Register(ctx context.Context, input RegisterInput) (Registration, error) {
	registration, _, err := service.store.Register(ctx, input)
	if err != nil {
		return Registration{}, err
	}
	return registration, nil
}

// BootstrapAccessKey exchanges a management-issued DeviceKey for short-lived
// App credentials. Keeping this delegate on the existing device service lets
// deployments add the bootstrap route without introducing another singleton or
// sending the long-lived key to a Relay.
func (service *Service) BootstrapAccessKey(ctx context.Context, input deviceaccesskey.BootstrapInput) (deviceaccesskey.BootstrapResult, error) {
	if service == nil || service.store == nil || service.store.accessKeys == nil {
		return deviceaccesskey.BootstrapResult{}, deviceaccesskey.ErrUnavailable
	}
	return service.store.accessKeys.Bootstrap(ctx, input)
}

func (service *Service) CreateAccessKey(ctx context.Context, input deviceaccesskey.CreateInput) (deviceaccesskey.AccessKey, error) {
	if service == nil || service.store == nil || service.store.accessKeys == nil {
		return deviceaccesskey.AccessKey{}, deviceaccesskey.ErrUnavailable
	}
	return service.store.accessKeys.Create(ctx, input)
}

func (service *Service) RotateAccessKey(ctx context.Context, input deviceaccesskey.RotateInput) (deviceaccesskey.AccessKey, error) {
	if service == nil || service.store == nil || service.store.accessKeys == nil {
		return deviceaccesskey.AccessKey{}, deviceaccesskey.ErrUnavailable
	}
	return service.store.accessKeys.Rotate(ctx, input)
}

func (service *Service) RevokeAccessKey(ctx context.Context, keyID, userID uuid.UUID) error {
	if service == nil || service.store == nil || service.store.accessKeys == nil {
		return deviceaccesskey.ErrUnavailable
	}
	return service.store.accessKeys.Revoke(ctx, keyID, userID)
}

func (service *Service) DeleteAccessKey(ctx context.Context, keyID, userID uuid.UUID) error {
	if service == nil || service.store == nil || service.store.accessKeys == nil {
		return deviceaccesskey.ErrUnavailable
	}
	return service.store.accessKeys.Delete(ctx, keyID, userID)
}

func (service *Service) ListAccessKeys(ctx context.Context, userID uuid.UUID) ([]deviceaccesskey.AccessKey, error) {
	if service == nil || service.store == nil || service.store.accessKeys == nil {
		return nil, deviceaccesskey.ErrUnavailable
	}
	return service.store.accessKeys.List(ctx, userID)
}

// ListDevices returns the authenticated user's active device identities. Raw
// Ed25519 public keys, key versions and grant versions are required by clients
// to verify the end-to-end Peer handshake without an out-of-band state file.
func (service *Service) ListDevices(ctx context.Context, userID uuid.UUID) ([]Credential, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	return service.store.ListDevices(ctx, userID)
}

func (store *Store) ListDevices(ctx context.Context, userID uuid.UUID) ([]Credential, error) {
	if userID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	var rows []credentialRow
	if err := store.db.WithContext(ctx).Where("user_id = ? AND status = 'active'", userID).
		Order("updated_at DESC, device_id DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("%w: list remote devices", ErrUnavailable)
	}
	result := make([]Credential, 0, len(rows))
	for _, row := range rows {
		credential, err := credentialFromRow(row)
		if err != nil {
			return nil, ErrUnavailable
		}
		result = append(result, credential)
	}
	return result, nil
}

func (store *Store) Register(ctx context.Context, input RegisterInput) (Registration, uuid.UUID, error) {
	normalized, publicKey, requestHash, err := normalizeRegistration(input)
	if err != nil {
		return Registration{}, uuid.Nil, err
	}
	if err := VerifyRegistration(publicKey, normalized.SessionID, normalized.DeviceID, normalized.Proof); err != nil {
		return Registration{}, uuid.Nil, err
	}
	now := store.now().UTC()
	var result Registration
	var eventID uuid.UUID
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "remote-device-account:"+normalized.UserID.String()).Error; err != nil {
			return fmt.Errorf("lock remote device account: %w", err)
		}
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", normalized.DeviceID.String()).Error; err != nil {
			return fmt.Errorf("lock remote device: %w", err)
		}
		var activeSession struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Table("app_sessions").Select("id").Where(
			"id = ? AND user_id = ? AND device_id = ? AND revoked_at IS NULL AND idle_expires_at > ?",
			normalized.SessionID, normalized.UserID, normalized.DeviceID, now,
		).Take(&activeSession).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrForbidden
			}
			return fmt.Errorf("verify remote device App Session: %w", err)
		}
		deviceScopes := deviceaccesskey.FullAccessScopes()
		if err := store.policy.RequireMembershipTx(tx, normalized.UserID, now); err != nil {
			return err
		}
		scopesJSON, _ := json.Marshal(deviceScopes)
		var prior requestKeyRow
		priorErr := tx.Where("user_id = ? AND device_id = ? AND operation = ? AND idempotency_key = ?",
			normalized.UserID, normalized.DeviceID, "registration", normalized.IdempotencyKey).First(&prior).Error
		if priorErr == nil {
			if prior.RequestHash != requestHash {
				return ErrIdempotencyConflict
			}
			credential, err := loadCredential(tx, normalized.DeviceID)
			if err != nil {
				return err
			}
			if credential.UserID != normalized.UserID {
				return ErrForbidden
			}
			result = Registration{Credential: credential, Created: false}
			if prior.OutboxID != nil {
				eventID = *prior.OutboxID
			}
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load remote device idempotency record: %w", priorErr)
		}

		var row credentialRow
		rowErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "device_id = ?", normalized.DeviceID).Error
		created := false
		if errors.Is(rowErr, gorm.ErrRecordNotFound) {
			if _, err := store.policy.RequireDeviceCapacityTx(tx, normalized.UserID); err != nil {
				return err
			}
			capabilities, _ := json.Marshal(normalized.Capabilities)
			row = credentialRow{
				DeviceID: normalized.DeviceID, UserID: normalized.UserID, RegisteredSessionID: normalized.SessionID,
				DeviceName: normalized.DeviceName, Platform: normalized.Platform, AgentVersion: normalized.AgentVersion,
				ProtocolMin: int64(normalized.ProtocolMin), ProtocolMax: int64(normalized.ProtocolMax), Capabilities: capabilities,
				Scopes: scopesJSON, KeyVersion: 1,
				IdentityPublicKey: append([]byte(nil), publicKey...), PublicKeyThumbprint: remoteauth.PublicKeyThumbprint(publicKey),
				GrantVersion: 1, Status: "active", LastConnectionEpoch: 0, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return fmt.Errorf("create remote device credential: %w", err)
			}
			if err := createDefaultRemoteAccessGrant(tx, row, now); err != nil {
				return err
			}
			created = true
		} else if rowErr != nil {
			return fmt.Errorf("load remote device credential: %w", rowErr)
		} else {
			if row.UserID != normalized.UserID {
				return ErrForbidden
			}
			if !slices.Equal(row.IdentityPublicKey, publicKey) {
				return ErrKeyRotationRequired
			}
			capabilities, _ := json.Marshal(normalized.Capabilities)
			refreshCredentialAgentMetadata(&row, normalized, capabilities, scopesJSON, now)
			if err := tx.Save(&row).Error; err != nil {
				return fmt.Errorf("refresh remote device metadata: %w", err)
			}
		}

		eventID = uuid.New()
		payload, _ := json.Marshal(map[string]any{
			"deviceId": row.DeviceID, "userId": row.UserID, "grantVersion": row.GrantVersion,
			"status": row.Status, "identityPublicKey": base64.RawURLEncoding.EncodeToString(row.IdentityPublicKey),
			"publicKeyThumbprint": row.PublicKeyThumbprint,
		})
		if err := tx.Table("relay_outbox").Create(map[string]any{
			"id": eventID, "aggregate_type": "remote_device", "aggregate_id": row.DeviceID,
			"event_type": "remote.device.changed", "payload": json.RawMessage(payload),
			"attempts": 0, "available_at": now, "published_at": now, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("append remote device Outbox event: %w", err)
		}
		if err := tx.Create(&requestKeyRow{
			ID: uuid.New(), UserID: normalized.UserID, DeviceID: normalized.DeviceID, Operation: "registration",
			IdempotencyKey: normalized.IdempotencyKey, RequestHash: requestHash, OutboxID: &eventID,
			CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		}).Error; err != nil {
			return fmt.Errorf("save remote device idempotency record: %w", err)
		}
		credential, err := credentialFromRow(row)
		if err != nil {
			return err
		}
		result = Registration{Credential: credential, Created: created}
		return nil
	})
	if err != nil {
		return Registration{}, uuid.Nil, err
	}
	return result, eventID, nil
}

// refreshCredentialAgentMetadata updates facts owned by the Agent while
// preserving DeviceName, which is an account-owned display name after the
// initial registration. An Agent restart or upgrade must not reset it to the
// host name reported during registration.
func refreshCredentialAgentMetadata(row *credentialRow, registration RegisterInput, capabilities, scopes json.RawMessage, now time.Time) {
	row.Platform, row.AgentVersion = registration.Platform, registration.AgentVersion
	row.ProtocolMin, row.ProtocolMax, row.Capabilities = int64(registration.ProtocolMin), int64(registration.ProtocolMax), capabilities
	row.Scopes = scopes
	row.UpdatedAt = now
}

func createDefaultRemoteAccessGrant(tx *gorm.DB, credential credentialRow, now time.Time) error {
	if tx == nil || credential.DeviceID == uuid.Nil || credential.UserID == uuid.Nil || credential.GrantVersion < 1 || credential.Status != "active" {
		return ErrInvalidInput
	}
	result := tx.Exec(`
		INSERT INTO remote_access_grants
		    (device_id, user_id, scopes, status, grant_version, enabled_at, revoked_at, created_at, updated_at)
		VALUES (?, ?, '[]'::jsonb, 'enabled', ?, ?, NULL, ?, ?)
		ON CONFLICT (device_id) DO NOTHING`,
		credential.DeviceID, credential.UserID, credential.GrantVersion, now, now, now)
	if result.Error != nil {
		return fmt.Errorf("create default remote access grant: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("create default remote access grant: %w", ErrUnavailable)
	}
	return nil
}

func normalizeRegistration(input RegisterInput) (RegisterInput, ed25519.PublicKey, string, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.DeviceName = strings.TrimSpace(input.DeviceName)
	input.Platform = strings.ToLower(strings.TrimSpace(input.Platform))
	input.AgentVersion = strings.TrimSpace(input.AgentVersion)
	input.IdentityAlgorithm = strings.ToLower(strings.TrimSpace(input.IdentityAlgorithm))
	input.IdentityPublicKey = strings.TrimSpace(input.IdentityPublicKey)
	input.Proof = strings.TrimSpace(input.Proof)
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.DeviceID == uuid.Nil || !idempotencyPattern.MatchString(input.IdempotencyKey) ||
		!validText(input.DeviceName, 120) || !validText(input.AgentVersion, 64) ||
		(input.Platform != "windows" && input.Platform != "macos" && input.Platform != "linux") || input.IdentityAlgorithm != "ed25519" ||
		input.ProtocolMin != 2 || input.ProtocolMax != 2 {
		return RegisterInput{}, nil, "", ErrInvalidInput
	}
	if len(input.Capabilities) == 0 {
		input.Capabilities = []string{"relay.ping"}
	}
	normalizedCapabilities := make([]string, 0, len(input.Capabilities))
	seenCapabilities := make(map[string]struct{}, len(input.Capabilities))
	if len(input.Capabilities) > len(allowedDeviceCapabilities) {
		return RegisterInput{}, nil, "", ErrInvalidInput
	}
	for _, capability := range input.Capabilities {
		capability = strings.TrimSpace(capability)
		if _, allowed := allowedDeviceCapabilities[capability]; !allowed {
			return RegisterInput{}, nil, "", ErrInvalidInput
		}
		if _, duplicate := seenCapabilities[capability]; duplicate {
			return RegisterInput{}, nil, "", ErrInvalidInput
		}
		seenCapabilities[capability] = struct{}{}
		normalizedCapabilities = append(normalizedCapabilities, capability)
	}
	slices.Sort(normalizedCapabilities)
	input.Capabilities = normalizedCapabilities
	rawKey, err := base64.RawURLEncoding.DecodeString(input.IdentityPublicKey)
	if err != nil || len(rawKey) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(rawKey) != input.IdentityPublicKey {
		return RegisterInput{}, nil, "", ErrInvalidInput
	}
	requestBytes, _ := json.Marshal(struct {
		DeviceName, Platform, AgentVersion    string
		ProtocolMin, ProtocolMax              uint32
		Capabilities                          []string
		IdentityAlgorithm, IdentityKey, Proof string
	}{input.DeviceName, input.Platform, input.AgentVersion, input.ProtocolMin, input.ProtocolMax, input.Capabilities, input.IdentityAlgorithm, input.IdentityPublicKey, input.Proof})
	digest := sha256.Sum256(requestBytes)
	return input, ed25519.PublicKey(rawKey), hex.EncodeToString(digest[:]), nil
}

func validText(value string, maxRunes int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func loadCredential(db *gorm.DB, deviceID uuid.UUID) (Credential, error) {
	var row credentialRow
	if err := db.First(&row, "device_id = ?", deviceID).Error; err != nil {
		return Credential{}, fmt.Errorf("load remote device credential: %w", err)
	}
	return credentialFromRow(row)
}

func credentialFromRow(row credentialRow) (Credential, error) {
	var capabilities []string
	var scopes []string
	if json.Unmarshal(row.Capabilities, &capabilities) != nil || json.Unmarshal(row.Scopes, &scopes) != nil ||
		len(row.IdentityPublicKey) != ed25519.PublicKeySize || row.KeyVersion < 1 || row.GrantVersion < 1 ||
		row.ProtocolMin != 2 || row.ProtocolMax != 2 || row.LastConnectionEpoch < 0 {
		return Credential{}, ErrInvalidInput
	}
	if _, err := normalizeCapabilities(capabilities); err != nil {
		return Credential{}, ErrInvalidInput
	}
	scopes, err := normalizeDeviceScopes(scopes)
	if err != nil {
		return Credential{}, ErrInvalidInput
	}
	return Credential{
		DeviceID: row.DeviceID, UserID: row.UserID, RegisteredSessionID: row.RegisteredSessionID,
		DeviceName: row.DeviceName, Platform: row.Platform, AgentVersion: row.AgentVersion,
		ProtocolMin: uint32(row.ProtocolMin), ProtocolMax: uint32(row.ProtocolMax), Capabilities: capabilities, Scopes: scopes,
		IdentityPublicKey: ed25519.PublicKey(append([]byte(nil), row.IdentityPublicKey...)), PublicKeyThumbprint: row.PublicKeyThumbprint,
		KeyVersion: uint64(row.KeyVersion), GrantVersion: uint64(row.GrantVersion), Status: row.Status, LastConnectionEpoch: uint64(row.LastConnectionEpoch),
		LastAllocationAt: utcPointer(row.LastAllocationAt), CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}, nil
}

func normalizeCapabilities(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > len(allowedDeviceCapabilities) {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, allowed := allowedDeviceCapabilities[value]; !allowed {
			return nil, ErrInvalidInput
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, ErrInvalidInput
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result, nil
}

func normalizeDeviceScopes(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 32 {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(allowedDeviceScopes))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, allowed := allowedDeviceScopes[value]; !allowed {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	if _, connected := seen["remote.connect"]; !connected {
		return nil, ErrInvalidInput
	}
	slices.Sort(result)
	return result, nil
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

type credentialRow struct {
	DeviceID            uuid.UUID       `gorm:"column:device_id;type:uuid;primaryKey"`
	UserID              uuid.UUID       `gorm:"column:user_id;type:uuid"`
	RegisteredSessionID uuid.UUID       `gorm:"column:registered_session_id;type:uuid"`
	DeviceName          string          `gorm:"column:device_name"`
	Platform            string          `gorm:"column:platform"`
	AgentVersion        string          `gorm:"column:agent_version"`
	ProtocolMin         int64           `gorm:"column:protocol_min"`
	ProtocolMax         int64           `gorm:"column:protocol_max"`
	Capabilities        json.RawMessage `gorm:"column:capabilities;type:jsonb"`
	Scopes              json.RawMessage `gorm:"column:scopes;type:jsonb"`
	IdentityPublicKey   []byte          `gorm:"column:identity_public_key"`
	PublicKeyThumbprint string          `gorm:"column:public_key_thumbprint"`
	KeyVersion          int64           `gorm:"column:key_version"`
	GrantVersion        int64           `gorm:"column:grant_version"`
	Status              string          `gorm:"column:status"`
	LastConnectionEpoch int64           `gorm:"column:last_connection_epoch"`
	LastAllocationAt    *time.Time      `gorm:"column:last_allocation_at"`
	CreatedAt           time.Time       `gorm:"column:created_at"`
	UpdatedAt           time.Time       `gorm:"column:updated_at"`
}

func (credentialRow) TableName() string { return "remote_device_credentials" }

type requestKeyRow struct {
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

func (requestKeyRow) TableName() string { return "remote_device_request_keys" }
