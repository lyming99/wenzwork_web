package deviceaccesskey

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	keyPrefix                     = "device_"
	secretBytes                   = 32
	defaultAccessTTL              = 15 * time.Minute
	defaultRefreshTTL             = 30 * 24 * time.Hour
	idempotencyCipherVersion byte = 1
)

var (
	ErrInvalidInput        = errors.New("device access key input is invalid")
	ErrNotFound            = errors.New("device access key was not found")
	ErrConflict            = errors.New("device access key conflicts with current state")
	ErrIdempotencyConflict = errors.New("device access key idempotency key conflicts with another request")
	ErrUnauthorized        = errors.New("device access key is invalid")
	ErrUnavailable         = errors.New("device access key service is unavailable")

	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	allowedScopes      = map[string]struct{}{
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
)

// FullAccessScopes is the single Device Agent permission profile. Device
// enrollment is intentionally an on/off trust decision: callers cannot create
// subtly under-scoped agents whose Carrier stays healthy while individual RPC,
// Event, or Stream operations fail later.
func FullAccessScopes() []string {
	result := make([]string, 0, len(allowedScopes))
	for scope := range allowedScopes {
		result = append(result, scope)
	}
	slices.Sort(result)
	return result
}

type AccessKey struct {
	ID            uuid.UUID  `json:"id"`
	UserID        uuid.UUID  `json:"-"`
	Label         string     `json:"label"`
	Key           string     `json:"key,omitempty"`
	KeyPrefix     string     `json:"keyPrefix"`
	Scopes        []string   `json:"scopes"`
	BoundDeviceID *uuid.UUID `json:"boundDeviceId"`
	Status        string     `json:"status"`
	ExpiresAt     *time.Time `json:"expiresAt"`
	LastUsedAt    *time.Time `json:"lastUsedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type CreateInput struct {
	UserID         uuid.UUID
	IdempotencyKey string
	Label          string
	Scopes         []string
	BoundDeviceID  *uuid.UUID
	ExpiresAt      *time.Time
}

type RotateInput struct {
	UserID         uuid.UUID
	KeyID          uuid.UUID
	IdempotencyKey string
}

type BootstrapInput struct {
	Key        string
	DeviceID   uuid.UUID
	DeviceName string
}

type BootstrapResult struct {
	UserID           uuid.UUID `json:"user_id"`
	SessionID        uuid.UUID `json:"session_id"`
	AccessToken      string    `json:"access_token"`
	AccessExpiresIn  int64     `json:"expires_in"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresIn int64     `json:"refresh_expires_in"`
	Scope            string    `json:"scope"`
}

type Store struct {
	db              *gorm.DB
	now             func() time.Time
	random          io.Reader
	accessTTL       time.Duration
	refreshTTL      time.Duration
	idempotencyAEAD cipher.AEAD
	accessPolicy    *remoteaccesspolicy.Store
}

type Option func(*Store)

func WithClock(now func() time.Time) Option { return func(store *Store) { store.now = now } }
func WithRandom(random io.Reader) Option    { return func(store *Store) { store.random = random } }
func WithIdempotencyEncryptionKey(key []byte) Option {
	return func(store *Store) {
		store.idempotencyAEAD, _ = newIdempotencyAEAD(key)
	}
}
func WithAccessPolicy(policy *remoteaccesspolicy.Store) Option {
	return func(store *Store) { store.accessPolicy = policy }
}
func WithTokenTTLs(access, refresh time.Duration) Option {
	return func(store *Store) { store.accessTTL, store.refreshTTL = access, refresh }
}

func NewStore(db *gorm.DB, options ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("device access key database is required")
	}
	store := &Store{
		db: db, now: func() time.Time { return time.Now().UTC() }, random: rand.Reader,
		accessTTL: defaultAccessTTL, refreshTTL: defaultRefreshTTL,
	}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	if store.now == nil || store.random == nil || store.idempotencyAEAD == nil || store.accessPolicy == nil || store.accessTTL < time.Minute || store.accessTTL > time.Hour ||
		store.refreshTTL < time.Hour || store.refreshTTL > 90*24*time.Hour {
		return nil, errors.New("device access key options are invalid")
	}
	return store, nil
}

func (store *Store) Create(ctx context.Context, input CreateInput) (AccessKey, error) {
	normalized, scopesJSON, err := normalizeCreateRequest(input)
	if err != nil {
		return AccessKey{}, err
	}
	idempotencyKey, ok := ParseIdempotencyKey(normalized.IdempotencyKey)
	if !ok {
		return AccessKey{}, ErrInvalidInput
	}
	normalized.IdempotencyKey = idempotencyKey
	requestDigest := createRequestDigest(normalized)
	resourceID := normalized.UserID // The user's Access Key collection is the create resource.
	now := store.now().UTC()
	var result AccessKey
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockIdempotencyTuple(tx, normalized.UserID, "create", resourceID, idempotencyKey); err != nil {
			return err
		}
		if err := store.accessPolicy.RequireMembershipTx(tx, normalized.UserID, now); err != nil {
			return err
		}
		replayed, found, err := store.loadIdempotentResult(tx, normalized.UserID, "create", resourceID, idempotencyKey, requestDigest)
		if err != nil {
			return err
		}
		if found {
			result = replayed
			return nil
		}
		if err := validateFreshCreateExpiration(normalized.ExpiresAt, now); err != nil {
			return err
		}
		plaintext, digest, err := newKey(store.random)
		if err != nil {
			return fmt.Errorf("generate device access key: %w", err)
		}
		row := accessKeyRow{
			ID: uuid.New(), UserID: normalized.UserID, Label: normalized.Label,
			KeyPrefix: plaintext[:16], KeyDigest: digest, Scopes: scopesJSON,
			BoundDeviceID: cloneUUID(normalized.BoundDeviceID), Status: "active",
			ExpiresAt: utcPointer(normalized.ExpiresAt), CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create key row: %w", err)
		}
		result, err = accessKeyFromRow(row)
		if err != nil {
			return err
		}
		result.Key = plaintext
		return store.saveIdempotentResult(tx, normalized.UserID, "create", resourceID, idempotencyKey, requestDigest, result, now)
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrIdempotencyConflict), errors.Is(err, ErrConflict),
			errors.Is(err, remoteaccesspolicy.ErrMembershipRequired):
			return AccessKey{}, err
		case isUniqueViolation(err):
			return AccessKey{}, ErrConflict
		default:
			return AccessKey{}, fmt.Errorf("%w: create key", ErrUnavailable)
		}
	}
	return result, nil
}

func (store *Store) Rotate(ctx context.Context, input RotateInput) (AccessKey, error) {
	idempotencyKey, ok := ParseIdempotencyKey(input.IdempotencyKey)
	if input.KeyID == uuid.Nil || input.UserID == uuid.Nil || !ok {
		return AccessKey{}, ErrInvalidInput
	}
	input.IdempotencyKey = idempotencyKey
	requestDigest := rotationRequestDigest()
	now := store.now().UTC()
	var replacement accessKeyRow
	var result AccessKey
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockIdempotencyTuple(tx, input.UserID, "rotate", input.KeyID, input.IdempotencyKey); err != nil {
			return err
		}
		if err := store.accessPolicy.RequireMembershipTx(tx, input.UserID, now); err != nil {
			return err
		}
		replayed, found, err := store.loadIdempotentResult(tx, input.UserID, "rotate", input.KeyID, input.IdempotencyKey, requestDigest)
		if err != nil {
			return err
		}
		if found {
			result = replayed
			return nil
		}
		var current accessKeyRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ? AND user_id = ?", input.KeyID, input.UserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("load key for rotation: %w", err)
		}
		if current.Status != "active" || (current.ExpiresAt != nil && !current.ExpiresAt.After(now)) {
			return ErrConflict
		}
		plaintext, digest, err := newKey(store.random)
		if err != nil {
			return fmt.Errorf("generate device access key: %w", err)
		}
		if err := tx.Model(&current).Updates(map[string]any{"status": "revoked", "revoked_at": now, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("revoke rotated key: %w", err)
		}
		fullScopesJSON, _ := json.Marshal(FullAccessScopes())
		replacement = accessKeyRow{
			ID: uuid.New(), UserID: current.UserID, Label: current.Label, KeyPrefix: plaintext[:16], KeyDigest: digest,
			Scopes: fullScopesJSON, BoundDeviceID: cloneUUID(current.BoundDeviceID),
			Status: "active", ExpiresAt: utcPointer(current.ExpiresAt), CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&replacement).Error; err != nil {
			return fmt.Errorf("create rotated key: %w", err)
		}
		result, err = accessKeyFromRow(replacement)
		if err != nil {
			return err
		}
		result.Key = plaintext
		return store.saveIdempotentResult(tx, input.UserID, "rotate", input.KeyID, input.IdempotencyKey, requestDigest, result, now)
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict), errors.Is(err, ErrIdempotencyConflict),
			errors.Is(err, remoteaccesspolicy.ErrMembershipRequired):
			return AccessKey{}, err
		case isUniqueViolation(err):
			return AccessKey{}, ErrConflict
		default:
			return AccessKey{}, fmt.Errorf("%w: rotate key", ErrUnavailable)
		}
	}
	return result, nil
}

func (store *Store) Revoke(ctx context.Context, keyID, userID uuid.UUID) error {
	if keyID == uuid.Nil || userID == uuid.Nil {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	result := store.db.WithContext(ctx).Model(&accessKeyRow{}).
		Where("id = ? AND user_id = ? AND status = 'active'", keyID, userID).
		Updates(map[string]any{"status": "revoked", "revoked_at": now, "updated_at": now})
	if result.Error != nil {
		return fmt.Errorf("%w: revoke key", ErrUnavailable)
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete permanently removes a revoked Access Key owned by the user. Active
// keys must be revoked first so deletion cannot accidentally become an
// authorization-changing operation.
func (store *Store) Delete(ctx context.Context, keyID, userID uuid.UUID) error {
	if keyID == uuid.Nil || userID == uuid.Nil {
		return ErrInvalidInput
	}
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current accessKeyRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "status").
			First(&current, "id = ? AND user_id = ?", keyID, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("load key for deletion: %w", err)
		}
		if current.Status != "revoked" {
			return ErrConflict
		}
		if err := tx.Delete(&current).Error; err != nil {
			return fmt.Errorf("delete revoked key: %w", err)
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict):
			return err
		default:
			return fmt.Errorf("%w: delete key", ErrUnavailable)
		}
	}
	return nil
}

func (store *Store) List(ctx context.Context, userID uuid.UUID) ([]AccessKey, error) {
	if userID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	var rows []accessKeyRow
	if err := store.db.WithContext(ctx).Where("user_id = ?", userID).Order("created_at DESC, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("%w: list keys", ErrUnavailable)
	}
	result := make([]AccessKey, 0, len(rows))
	for _, row := range rows {
		item, err := accessKeyFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

// Bootstrap exchanges a long-lived DeviceKey for the existing short-lived App
// token format. The DeviceKey remains on the Control Plane and is never sent to
// a Relay. First use atomically binds an unbound key to one installation ID.
func (store *Store) Bootstrap(ctx context.Context, input BootstrapInput) (BootstrapResult, error) {
	digest, ok := keyDigest(input.Key)
	input.DeviceName = strings.TrimSpace(input.DeviceName)
	if !ok || input.DeviceID == uuid.Nil || !validText(input.DeviceName, 120) {
		return BootstrapResult{}, ErrUnauthorized
	}
	accessPlaintext, accessDigest, err := auth.NewOpaqueToken()
	if err != nil {
		return BootstrapResult{}, ErrUnavailable
	}
	refreshPlaintext, refreshDigest, err := auth.NewOpaqueToken()
	if err != nil {
		return BootstrapResult{}, ErrUnavailable
	}
	now := store.now().UTC()
	var result BootstrapResult
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var key accessKeyRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&key, "key_digest = ?", digest).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUnauthorized
			}
			return ErrUnavailable
		}
		if key.Status != "active" || (key.ExpiresAt != nil && !key.ExpiresAt.After(now)) ||
			(key.BoundDeviceID != nil && *key.BoundDeviceID != input.DeviceID) {
			return ErrUnauthorized
		}
		var user struct{ ID uuid.UUID }
		if err := tx.Table("users").Select("id").Where("id = ? AND status = 'active' AND email_verified_at IS NOT NULL", key.UserID).Take(&user).Error; err != nil {
			return ErrUnauthorized
		}
		if err := store.accessPolicy.RequireMembershipTx(tx, key.UserID, now); err != nil {
			return err
		}
		if key.BoundDeviceID == nil {
			key.BoundDeviceID = &input.DeviceID
		}
		fullScopes := FullAccessScopes()
		fullScopesJSON, _ := json.Marshal(fullScopes)
		if err := tx.Model(&key).Updates(map[string]any{
			"bound_device_id": key.BoundDeviceID, "scopes": fullScopesJSON, "last_used_at": now, "updated_at": now,
		}).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrUnauthorized
			}
			return ErrUnavailable
		}
		var sessionIDs []uuid.UUID
		if err := tx.Table("app_sessions").Where(
			"user_id = ? AND client_id = ? AND device_id = ? AND revoked_at IS NULL", key.UserID, auth.DesktopClientID, input.DeviceID,
		).Pluck("id", &sessionIDs).Error; err != nil {
			return ErrUnavailable
		}
		if len(sessionIDs) > 0 {
			if err := tx.Table("app_sessions").Where("id IN ?", sessionIDs).Updates(map[string]any{
				"revoked_at": now, "revoked_reason": "device_key_reauthorized", "updated_at": now,
			}).Error; err != nil {
				return ErrUnavailable
			}
			if err := tx.Table("app_refresh_tokens").Where("session_id IN ? AND status = 'active'", sessionIDs).
				Update("status", "revoked").Error; err != nil {
				return ErrUnavailable
			}
		}
		scope := strings.Join(fullScopes, " ")
		sessionID := uuid.New()
		refreshExpiresAt := now.Add(store.refreshTTL)
		if err := tx.Table("app_sessions").Create(map[string]any{
			"id": sessionID, "user_id": key.UserID, "client_id": auth.DesktopClientID,
			"device_id": input.DeviceID, "device_name": input.DeviceName, "scope": scope,
			"last_seen_at": now, "idle_expires_at": refreshExpiresAt, "created_at": now, "updated_at": now,
		}).Error; err != nil {
			return ErrUnavailable
		}
		if err := tx.Table("app_access_tokens").Create(map[string]any{
			"id": uuid.New(), "session_id": sessionID, "token_hash": accessDigest,
			"expires_at": now.Add(store.accessTTL), "created_at": now,
		}).Error; err != nil {
			return ErrUnavailable
		}
		if err := tx.Table("app_refresh_tokens").Create(map[string]any{
			"id": uuid.New(), "session_id": sessionID, "token_hash": refreshDigest, "status": "active",
			"expires_at": refreshExpiresAt, "created_at": now,
		}).Error; err != nil {
			return ErrUnavailable
		}
		result = BootstrapResult{
			UserID: key.UserID, SessionID: sessionID, AccessToken: accessPlaintext,
			AccessExpiresIn: int64(store.accessTTL.Seconds()), RefreshToken: refreshPlaintext,
			RefreshExpiresIn: int64(store.refreshTTL.Seconds()), Scope: scope,
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			return BootstrapResult{}, ErrUnauthorized
		}
		if errors.Is(err, remoteaccesspolicy.ErrMembershipRequired) {
			return BootstrapResult{}, err
		}
		return BootstrapResult{}, fmt.Errorf("%w: bootstrap", ErrUnavailable)
	}
	return result, nil
}

func ParseIdempotencyKey(value string) (string, bool) {
	normalized := strings.TrimSpace(value)
	return normalized, normalized == value && idempotencyPattern.MatchString(normalized)
}

func normalizeCreate(input CreateInput, now time.Time) (CreateInput, json.RawMessage, error) {
	normalized, scopes, err := normalizeCreateRequest(input)
	if err != nil || validateFreshCreateExpiration(normalized.ExpiresAt, now) != nil {
		return CreateInput{}, nil, ErrInvalidInput
	}
	return normalized, scopes, nil
}

func normalizeCreateRequest(input CreateInput) (CreateInput, json.RawMessage, error) {
	input.Label = strings.TrimSpace(input.Label)
	if input.UserID == uuid.Nil || !validText(input.Label, 120) ||
		(input.BoundDeviceID != nil && *input.BoundDeviceID == uuid.Nil) {
		return CreateInput{}, nil, ErrInvalidInput
	}
	// Scopes remains on CreateInput for wire compatibility with older clients,
	// but enrollment always receives the complete canonical profile.
	input.Scopes = FullAccessScopes()
	input.ExpiresAt = utcPointer(input.ExpiresAt)
	encoded, _ := json.Marshal(input.Scopes)
	return input, encoded, nil
}

func validateFreshCreateExpiration(expiresAt *time.Time, now time.Time) error {
	if expiresAt != nil && (!expiresAt.After(now) || expiresAt.After(now.Add(366*24*time.Hour))) {
		return ErrInvalidInput
	}
	return nil
}

func createRequestDigest(input CreateInput) string {
	payload, _ := json.Marshal(struct {
		Label         string     `json:"label"`
		Scopes        []string   `json:"scopes"`
		BoundDeviceID *uuid.UUID `json:"boundDeviceId"`
		ExpiresAt     *time.Time `json:"expiresAt"`
	}{input.Label, input.Scopes, input.BoundDeviceID, input.ExpiresAt})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func rotationRequestDigest() string {
	digest := sha256.Sum256([]byte("wenzwork-device-access-key-rotation-request:v1"))
	return hex.EncodeToString(digest[:])
}

func lockIdempotencyTuple(tx *gorm.DB, userID uuid.UUID, operation string, resourceID uuid.UUID, key string) error {
	lockKey := strings.Join([]string{"device-access-key", userID.String(), operation, resourceID.String(), key}, "|")
	if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", lockKey).Error; err != nil {
		return fmt.Errorf("lock device access key idempotency tuple: %w", err)
	}
	return nil
}

func (store *Store) saveIdempotentResult(tx *gorm.DB, userID uuid.UUID, operation string, resourceID uuid.UUID, key, requestDigest string, result AccessKey, now time.Time) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode device access key idempotency result: %w", err)
	}
	ciphertext, err := store.sealIdempotentResult(payload, idempotencyAAD(userID, operation, resourceID, key, requestDigest))
	if err != nil {
		return err
	}
	row := accessKeyRequestRow{
		UserID: userID, Operation: operation, ResourceID: resourceID, IdempotencyKey: key,
		RequestDigest: requestDigest, ResponseCiphertext: ciphertext, CreatedAt: now,
	}
	if err := tx.Create(&row).Error; err != nil {
		return fmt.Errorf("save device access key idempotency result: %w", err)
	}
	return nil
}

func (store *Store) loadIdempotentResult(tx *gorm.DB, userID uuid.UUID, operation string, resourceID uuid.UUID, key, requestDigest string) (AccessKey, bool, error) {
	var row accessKeyRequestRow
	err := tx.Where("user_id = ? AND operation = ? AND resource_id = ? AND idempotency_key = ?", userID, operation, resourceID, key).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AccessKey{}, false, nil
	}
	if err != nil {
		return AccessKey{}, false, fmt.Errorf("load device access key idempotency result: %w", err)
	}
	if row.RequestDigest != requestDigest {
		return AccessKey{}, false, ErrIdempotencyConflict
	}
	payload, err := store.openIdempotentResult(row.ResponseCiphertext, idempotencyAAD(userID, operation, resourceID, key, requestDigest))
	if err != nil {
		return AccessKey{}, false, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var result AccessKey
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return AccessKey{}, false, errors.New("device access key idempotency result is invalid")
	}
	result.UserID = userID
	digest, validKey := keyDigest(result.Key)
	encodedScopes, _ := json.Marshal(result.Scopes)
	if !validKey || result.ID == uuid.Nil || result.KeyPrefix != result.Key[:16] || !validText(result.Label, 120) ||
		result.CreatedAt.IsZero() || decodeResultScopes(encodedScopes) != nil {
		return AccessKey{}, false, errors.New("device access key idempotency result is invalid")
	}
	var persisted struct {
		UserID    uuid.UUID `gorm:"column:user_id"`
		KeyDigest string    `gorm:"column:key_digest"`
	}
	if err := tx.Table("remote_device_access_keys").Select("user_id, key_digest").Where("id = ?", result.ID).Take(&persisted).Error; err != nil || persisted.UserID != userID || persisted.KeyDigest != digest {
		return AccessKey{}, false, errors.New("device access key idempotency target is unavailable")
	}
	return result, true, nil
}

func decodeResultScopes(raw json.RawMessage) error {
	_, err := decodeScopes(raw)
	return err
}

func newIdempotencyAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) < 32 {
		return nil, errors.New("device access key idempotency encryption key must contain at least 32 bytes")
	}
	derivation := sha256.New()
	_, _ = derivation.Write([]byte("wenzwork-device-access-key-idempotency-key:v1\x00"))
	_, _ = derivation.Write(key)
	block, err := aes.NewCipher(derivation.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create device access key idempotency cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (store *Store) sealIdempotentResult(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, store.idempotencyAEAD.NonceSize())
	if _, err := io.ReadFull(store.random, nonce); err != nil {
		return nil, fmt.Errorf("generate device access key idempotency nonce: %w", err)
	}
	result := make([]byte, 1+len(nonce))
	result[0] = idempotencyCipherVersion
	copy(result[1:], nonce)
	return store.idempotencyAEAD.Seal(result, nonce, plaintext, aad), nil
}

func (store *Store) openIdempotentResult(ciphertext, aad []byte) ([]byte, error) {
	nonceSize := store.idempotencyAEAD.NonceSize()
	if len(ciphertext) < 1+nonceSize+store.idempotencyAEAD.Overhead() || ciphertext[0] != idempotencyCipherVersion {
		return nil, errors.New("device access key idempotency ciphertext is invalid")
	}
	plaintext, err := store.idempotencyAEAD.Open(nil, ciphertext[1:1+nonceSize], ciphertext[1+nonceSize:], aad)
	if err != nil {
		return nil, errors.New("device access key idempotency ciphertext authentication failed")
	}
	return plaintext, nil
}

func idempotencyAAD(userID uuid.UUID, operation string, resourceID uuid.UUID, key, requestDigest string) []byte {
	result := []byte("wenzwork-device-access-key-idempotency-response:v1")
	for _, field := range [][]byte{userID[:], []byte(operation), resourceID[:], []byte(key), []byte(requestDigest)} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		result = append(result, length[:]...)
		result = append(result, field...)
	}
	return result
}

func newKey(random io.Reader) (string, string, error) {
	secret := make([]byte, secretBytes)
	if _, err := io.ReadFull(random, secret); err != nil {
		return "", "", err
	}
	plaintext := keyPrefix + base64.RawURLEncoding.EncodeToString(secret)
	digest, _ := keyDigest(plaintext)
	return plaintext, digest, nil
}

func keyDigest(plaintext string) (string, bool) {
	if len(plaintext) != len(keyPrefix)+43 || !strings.HasPrefix(plaintext, keyPrefix) {
		return "", false
	}
	encoded := plaintext[len(keyPrefix):]
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != secretBytes || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return "", false
	}
	digest := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(digest[:]), true
}

func decodeScopes(raw json.RawMessage) ([]string, error) {
	var scopes []string
	if json.Unmarshal(raw, &scopes) != nil || len(scopes) == 0 || len(scopes) > len(allowedScopes) {
		return nil, ErrUnavailable
	}
	for _, scope := range scopes {
		if _, ok := allowedScopes[scope]; !ok {
			return nil, ErrUnavailable
		}
	}
	slices.Sort(scopes)
	return scopes, nil
}

func accessKeyFromRow(row accessKeyRow) (AccessKey, error) {
	scopes, err := decodeScopes(row.Scopes)
	if err != nil || row.ID == uuid.Nil || row.UserID == uuid.Nil || row.KeyPrefix == "" ||
		(row.Status != "active" && row.Status != "revoked") {
		return AccessKey{}, ErrUnavailable
	}
	return AccessKey{
		ID: row.ID, UserID: row.UserID, Label: row.Label, KeyPrefix: row.KeyPrefix, Scopes: scopes,
		BoundDeviceID: cloneUUID(row.BoundDeviceID), Status: row.Status, ExpiresAt: utcPointer(row.ExpiresAt),
		LastUsedAt: utcPointer(row.LastUsedAt), CreatedAt: row.CreatedAt.UTC(),
	}, nil
}

func validText(value string, maxRunes int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxRunes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func cloneUUID(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate key")
}

type accessKeyRow struct {
	ID            uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	UserID        uuid.UUID       `gorm:"column:user_id;type:uuid"`
	Label         string          `gorm:"column:label"`
	KeyPrefix     string          `gorm:"column:key_prefix"`
	KeyDigest     string          `gorm:"column:key_digest"`
	Scopes        json.RawMessage `gorm:"column:scopes;type:jsonb"`
	BoundDeviceID *uuid.UUID      `gorm:"column:bound_device_id;type:uuid"`
	Status        string          `gorm:"column:status"`
	ExpiresAt     *time.Time      `gorm:"column:expires_at"`
	LastUsedAt    *time.Time      `gorm:"column:last_used_at"`
	RevokedAt     *time.Time      `gorm:"column:revoked_at"`
	CreatedAt     time.Time       `gorm:"column:created_at"`
	UpdatedAt     time.Time       `gorm:"column:updated_at"`
}

func (accessKeyRow) TableName() string { return "remote_device_access_keys" }

type accessKeyRequestRow struct {
	UserID             uuid.UUID `gorm:"column:user_id;type:uuid;primaryKey"`
	Operation          string    `gorm:"column:operation;primaryKey"`
	ResourceID         uuid.UUID `gorm:"column:resource_id;type:uuid;primaryKey"`
	IdempotencyKey     string    `gorm:"column:idempotency_key;primaryKey"`
	RequestDigest      string    `gorm:"column:request_digest"`
	ResponseCiphertext []byte    `gorm:"column:response_ciphertext"`
	CreatedAt          time.Time `gorm:"column:created_at"`
}

func (accessKeyRequestRow) TableName() string { return "remote_device_access_key_request_keys" }
