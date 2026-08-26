package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ReleaseDownloadProxyCached    = "proxy_cached"
	ReleaseDownloadS3Redirect     = "s3_redirect"
	ReleaseDownloadGitHubRedirect = "github_redirect"
)

var (
	ErrReleaseDeliveryInvalid  = errors.New("release delivery settings are invalid")
	ErrReleaseDeliveryConflict = errors.New("release delivery settings changed concurrently")
)

type ReleaseDeliverySettings struct {
	DownloadMode string    `json:"downloadMode"`
	S3URLPrefix  string    `json:"s3UrlPrefix"`
	Version      int64     `json:"version"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type UpdateReleaseDeliverySettingsInput struct {
	DownloadMode    string
	S3URLPrefix     string
	ExpectedVersion int64
	ActorUserID     uuid.UUID
}

type releaseDeliverySettingsRow struct {
	Singleton    bool       `gorm:"column:singleton;primaryKey"`
	DownloadMode string     `gorm:"column:download_mode"`
	S3URLPrefix  string     `gorm:"column:s3_url_prefix"`
	Version      int64      `gorm:"column:version"`
	UpdatedBy    *uuid.UUID `gorm:"column:updated_by;type:uuid"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (releaseDeliverySettingsRow) TableName() string { return "release_delivery_settings" }

func (s *Store) GetReleaseDeliverySettings(ctx context.Context) (ReleaseDeliverySettings, error) {
	var row releaseDeliverySettingsRow
	if err := s.db.WithContext(ctx).First(&row, "singleton = ?", true).Error; err != nil {
		return ReleaseDeliverySettings{}, fmt.Errorf("load release delivery settings: %w", err)
	}
	return releaseDeliverySettingsFromRow(row), nil
}

func (s *Store) UpdateReleaseDeliverySettings(ctx context.Context, input UpdateReleaseDeliverySettingsInput) (ReleaseDeliverySettings, error) {
	input.DownloadMode = strings.TrimSpace(input.DownloadMode)
	input.S3URLPrefix = strings.TrimRight(strings.TrimSpace(input.S3URLPrefix), "/")
	if input.ActorUserID == uuid.Nil || input.ExpectedVersion < 1 || !validReleaseDeliverySettings(input.DownloadMode, input.S3URLPrefix) {
		return ReleaseDeliverySettings{}, ErrReleaseDeliveryInvalid
	}

	var result ReleaseDeliverySettings
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current releaseDeliverySettingsRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "singleton = ?", true).Error; err != nil {
			return fmt.Errorf("lock release delivery settings: %w", err)
		}
		if current.Version != input.ExpectedVersion {
			return ErrReleaseDeliveryConflict
		}
		beforeJSON, _ := json.Marshal(releaseDeliverySettingsFromRow(current))
		now := time.Now().UTC()
		current.DownloadMode = input.DownloadMode
		current.S3URLPrefix = input.S3URLPrefix
		current.Version++
		current.UpdatedBy = &input.ActorUserID
		current.UpdatedAt = now
		if err := tx.Save(&current).Error; err != nil {
			return fmt.Errorf("update release delivery settings: %w", err)
		}
		result = releaseDeliverySettingsFromRow(current)
		afterJSON, _ := json.Marshal(result)
		audit := catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "release.delivery.update",
			ResourceType: "release_delivery_settings", BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}
		if err := tx.Create(&audit).Error; err != nil {
			return fmt.Errorf("audit release delivery settings update: %w", err)
		}
		return nil
	})
	return result, err
}

func validReleaseDeliverySettings(mode, prefix string) bool {
	if mode != ReleaseDownloadProxyCached && mode != ReleaseDownloadS3Redirect && mode != ReleaseDownloadGitHubRedirect {
		return false
	}
	if prefix == "" {
		return mode != ReleaseDownloadS3Redirect
	}
	parsed, err := url.Parse(prefix)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https") &&
		parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func releaseDeliverySettingsFromRow(row releaseDeliverySettingsRow) ReleaseDeliverySettings {
	return ReleaseDeliverySettings{
		DownloadMode: row.DownloadMode, S3URLPrefix: row.S3URLPrefix,
		Version: row.Version, UpdatedAt: row.UpdatedAt.UTC(),
	}
}
