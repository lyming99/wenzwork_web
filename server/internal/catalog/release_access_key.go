package catalog

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const releaseAccessKeyPrefix = "release_"

var (
	ErrReleaseAccessKeyInvalid     = errors.New("release access key is invalid")
	ErrReleaseAccessKeyConflict    = errors.New("release access key settings changed concurrently")
	ErrReleaseAccessKeyUnavailable = errors.New("release access key is not configured")
)

// ReleaseAccessKeySettings is the safe, non-secret projection shown to
// administrators. The bearer value itself is never returned by the API.
type ReleaseAccessKeySettings struct {
	AccessKeyConfigured bool      `json:"accessKeyConfigured"`
	KeyPrefix           string    `json:"keyPrefix"`
	Version             int64     `json:"version"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type UpdateReleaseAccessKeySettingsInput struct {
	AccessKey       string
	ExpectedVersion int64
	ActorUserID     uuid.UUID
}

type releaseAccessKeySettingsRow struct {
	Singleton       bool       `gorm:"column:singleton;primaryKey"`
	AccessKeyDigest string     `gorm:"column:access_key_digest"`
	KeyPrefix       string     `gorm:"column:access_key_prefix"`
	Initialized     bool       `gorm:"column:initialized"`
	Version         int64      `gorm:"column:version"`
	UpdatedBy       *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (releaseAccessKeySettingsRow) TableName() string { return "release_access_key_settings" }

// EnsureReleaseAccessKey imports the legacy environment/file value exactly
// once. A later administrator rotation is authoritative and is never
// overwritten by a subsequent process restart.
func (s *Store) EnsureReleaseAccessKey(ctx context.Context, accessKey string) error {
	digest, ok := releaseAccessKeyDigest(accessKey)
	if !ok {
		return ErrReleaseAccessKeyInvalid
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current releaseAccessKeySettingsRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "singleton = ?", true).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			current = releaseAccessKeySettingsRow{
				Singleton: true, AccessKeyDigest: digest, KeyPrefix: accessKeyPrefix(accessKey),
				Initialized: true, Version: 1, UpdatedAt: time.Now().UTC(),
			}
			if err := tx.Create(&current).Error; err != nil {
				return fmt.Errorf("create release access key settings: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock release access key settings: %w", err)
		}
		if current.Initialized && strings.TrimSpace(current.AccessKeyDigest) != "" {
			return nil
		}
		current.AccessKeyDigest = digest
		current.KeyPrefix = accessKeyPrefix(accessKey)
		current.Initialized = true
		current.UpdatedAt = time.Now().UTC()
		if err := tx.Save(&current).Error; err != nil {
			return fmt.Errorf("seed release access key settings: %w", err)
		}
		return nil
	})
}

func (s *Store) GetReleaseAccessKeySettings(ctx context.Context) (ReleaseAccessKeySettings, error) {
	var row releaseAccessKeySettingsRow
	if err := s.db.WithContext(ctx).First(&row, "singleton = ?", true).Error; err != nil {
		return ReleaseAccessKeySettings{}, fmt.Errorf("load release access key settings: %w", err)
	}
	return releaseAccessKeySettingsFromRow(row), nil
}

// VerifyReleaseAccessKey checks the digest stored in PostgreSQL on every
// request so an administrator rotation takes effect without a restart.
func (s *Store) VerifyReleaseAccessKey(ctx context.Context, accessKey string) (bool, error) {
	digest, ok := releaseAccessKeyDigest(accessKey)
	if !ok {
		return false, nil
	}
	var row releaseAccessKeySettingsRow
	if err := s.db.WithContext(ctx).First(&row, "singleton = ?", true).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, ErrReleaseAccessKeyUnavailable
		}
		return false, fmt.Errorf("load release access key for verification: %w", err)
	}
	if !row.Initialized || strings.TrimSpace(row.AccessKeyDigest) == "" {
		return false, ErrReleaseAccessKeyUnavailable
	}
	return subtle.ConstantTimeCompare([]byte(row.AccessKeyDigest), []byte(digest)) == 1, nil
}

func (s *Store) UpdateReleaseAccessKeySettings(ctx context.Context, input UpdateReleaseAccessKeySettingsInput) (ReleaseAccessKeySettings, error) {
	digest, ok := releaseAccessKeyDigest(input.AccessKey)
	if !ok || input.ExpectedVersion < 1 || input.ActorUserID == uuid.Nil {
		return ReleaseAccessKeySettings{}, ErrReleaseAccessKeyInvalid
	}

	var result ReleaseAccessKeySettings
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current releaseAccessKeySettingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "singleton = ?", true).Error; err != nil {
			return fmt.Errorf("lock release access key settings: %w", err)
		}
		if current.Version != input.ExpectedVersion {
			return ErrReleaseAccessKeyConflict
		}

		beforeJSON, _ := json.Marshal(releaseAccessKeySettingsFromRow(current))
		now := time.Now().UTC()
		current.AccessKeyDigest = digest
		current.KeyPrefix = accessKeyPrefix(input.AccessKey)
		current.Initialized = true
		current.Version++
		current.UpdatedBy = &input.ActorUserID
		current.UpdatedAt = now
		if err := tx.Save(&current).Error; err != nil {
			return fmt.Errorf("update release access key settings: %w", err)
		}

		result = releaseAccessKeySettingsFromRow(current)
		afterJSON, _ := json.Marshal(result)
		audit := catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "release.access_key.update",
			ResourceType: "release_access_key_settings", BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit release access key settings update: %w", err)
		}
		return nil
	})
	return result, err
}

func releaseAccessKeySettingsFromRow(row releaseAccessKeySettingsRow) ReleaseAccessKeySettings {
	configured := row.Initialized && strings.TrimSpace(row.AccessKeyDigest) != ""
	prefix := ""
	if configured {
		prefix = row.KeyPrefix
	}
	return ReleaseAccessKeySettings{
		AccessKeyConfigured: configured,
		KeyPrefix:           prefix,
		Version:             row.Version,
		UpdatedAt:           row.UpdatedAt.UTC(),
	}
}

func releaseAccessKeyDigest(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if !validReleaseAccessKey(value) {
		return "", false
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:]), true
}

func validReleaseAccessKey(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != len(releaseAccessKeyPrefix)+43 || !strings.HasPrefix(value, releaseAccessKeyPrefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, releaseAccessKeyPrefix))
	return err == nil && len(raw) == 32
}

func accessKeyPrefix(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 16 {
		return value
	}
	return value[:16]
}
