// Package remoteaccesspolicy owns the server-side entitlement and account
// device-limit checks for Device Agent access.
package remoteaccesspolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	DefaultDeviceLimit = 10
	MaximumDeviceLimit = 100000
)

var (
	ErrMembershipRequired = errors.New("an active paid membership is required for remote device access")
	ErrDeviceLimitReached = errors.New("the account remote device limit has been reached")
	ErrSettingsInvalid    = errors.New("remote access policy settings are invalid")
	ErrSettingsConflict   = errors.New("remote access policy settings changed concurrently")
	ErrUnavailable        = errors.New("remote access policy is unavailable")
)

type Settings struct {
	DeviceLimit int       `json:"deviceLimit"`
	Version     int64     `json:"version"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type UpdateSettingsInput struct {
	DeviceLimit     int
	ExpectedVersion int64
	ActorUserID     uuid.UUID
}

type settingsRow struct {
	Singleton   bool       `gorm:"column:singleton;primaryKey"`
	DeviceLimit int        `gorm:"column:device_limit"`
	Version     int64      `gorm:"column:version"`
	UpdatedBy   *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

func (settingsRow) TableName() string { return "remote_access_policy_settings" }

type auditLogRow struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	ActorUserID  *uuid.UUID `gorm:"column:actor_user_id"`
	Action       string     `gorm:"column:action"`
	ResourceType string     `gorm:"column:resource_type"`
	BeforeJSON   []byte     `gorm:"column:before_json;type:jsonb"`
	AfterJSON    []byte     `gorm:"column:after_json;type:jsonb"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

func (auditLogRow) TableName() string { return "audit_logs" }

type Store struct {
	db  *gorm.DB
	now func() time.Time
}

func NewStore(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("remote access policy database is required")
	}
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (store *Store) GetSettings(ctx context.Context) (Settings, error) {
	if store == nil || store.db == nil {
		return Settings{}, ErrUnavailable
	}
	var row settingsRow
	if err := store.db.WithContext(ctx).First(&row, "singleton = ?", true).Error; err != nil {
		return Settings{}, fmt.Errorf("%w: load settings: %v", ErrUnavailable, err)
	}
	return settingsFromRow(row), nil
}

func (store *Store) UpdateSettings(ctx context.Context, input UpdateSettingsInput) (Settings, error) {
	if store == nil || store.db == nil {
		return Settings{}, ErrUnavailable
	}
	if !validDeviceLimit(input.DeviceLimit) || input.ExpectedVersion < 1 || input.ActorUserID == uuid.Nil {
		return Settings{}, ErrSettingsInvalid
	}

	var result Settings
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current settingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "singleton = ?", true).Error; err != nil {
			return fmt.Errorf("%w: lock settings: %v", ErrUnavailable, err)
		}
		if current.Version != input.ExpectedVersion {
			return ErrSettingsConflict
		}
		beforeJSON, _ := json.Marshal(settingsFromRow(current))
		now := store.now().UTC()
		current.DeviceLimit = input.DeviceLimit
		current.Version++
		current.UpdatedBy = &input.ActorUserID
		current.UpdatedAt = now
		if err := tx.Save(&current).Error; err != nil {
			return fmt.Errorf("%w: update settings: %v", ErrUnavailable, err)
		}
		result = settingsFromRow(current)
		afterJSON, _ := json.Marshal(result)
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "remote_access.policy.update",
			ResourceType: "remote_access_policy_settings", BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("%w: audit settings update: %v", ErrUnavailable, err)
		}
		return nil
	})
	return result, err
}

// RequireMembershipTx fails closed unless the account has a currently active,
// non-free membership. A membership row whose expiry has passed is not active
// even if its asynchronously-maintained status column still says active.
func (store *Store) RequireMembershipTx(tx *gorm.DB, userID uuid.UUID, now time.Time) error {
	if store == nil || tx == nil || userID == uuid.Nil {
		return ErrUnavailable
	}
	var allowed bool
	err := tx.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM memberships membership
			JOIN membership_plans plan ON plan.id = membership.plan_id
			JOIN users account ON account.id = membership.user_id
			WHERE membership.user_id = ?
			  AND account.status = 'active'
			  AND membership.status = 'active'
			  AND plan.rank > 0
			  AND membership.starts_at <= ?
			  AND (membership.expires_at IS NULL OR membership.expires_at > ?)
		)`, userID, now.UTC(), now.UTC()).Scan(&allowed).Error
	if err != nil {
		return fmt.Errorf("%w: check membership: %v", ErrUnavailable, err)
	}
	if !allowed {
		return ErrMembershipRequired
	}
	return nil
}

// RequireDeviceCapacityTx is called only when a credential does not already
// exist. All credential rows count toward the enrolled-device limit; revoking
// remote access does not free a slot, while permanently deleting a device does.
func (store *Store) RequireDeviceCapacityTx(tx *gorm.DB, userID uuid.UUID) (Settings, error) {
	if store == nil || tx == nil || userID == uuid.Nil {
		return Settings{}, ErrUnavailable
	}
	var row settingsRow
	if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&row, "singleton = ?", true).Error; err != nil {
		return Settings{}, fmt.Errorf("%w: load device limit: %v", ErrUnavailable, err)
	}
	var count int64
	if err := tx.Table("remote_device_credentials").Where("user_id = ?", userID).Count(&count).Error; err != nil {
		return Settings{}, fmt.Errorf("%w: count enrolled devices: %v", ErrUnavailable, err)
	}
	if count >= int64(row.DeviceLimit) {
		return settingsFromRow(row), ErrDeviceLimitReached
	}
	return settingsFromRow(row), nil
}

func settingsFromRow(row settingsRow) Settings {
	return Settings{DeviceLimit: row.DeviceLimit, Version: row.Version, UpdatedAt: row.UpdatedAt.UTC()}
}

func validDeviceLimit(value int) bool { return value >= 1 && value <= MaximumDeviceLimit }
