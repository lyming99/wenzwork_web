package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrReleaseInvalid         = errors.New("release input is invalid")
	ErrReleaseAssetMismatch   = errors.New("deployment asset does not match its release")
	ErrReleaseVersionConflict = errors.New("release version already exists")
	ErrReleaseWithdrawn       = errors.New("withdrawn release cannot be edited")
)

var (
	sha256Pattern          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	deploymentAssetPattern = regexp.MustCompile(
		`^wenzwork-(host|relay|device-agent)-deployment-([A-Za-z0-9._+-]+)-(linux|windows|darwin)-(amd64|arm64)\.tar\.gz$`,
	)
)

type AdminReleaseAsset struct {
	ID              uuid.UUID `json:"id"`
	Platform        string    `json:"platform"`
	Architecture    string    `json:"architecture"`
	FileName        string    `json:"fileName"`
	FileSizeBytes   int64     `json:"fileSizeBytes"`
	SHA256          string    `json:"sha256"`
	SignatureStatus string    `json:"signatureStatus"`
	Source          string    `json:"source"`
	ObjectKey       string    `json:"objectKey"`
	DownloadURL     string    `json:"downloadUrl"`
	Status          string    `json:"status"`
}

type AdminRelease struct {
	ID           uuid.UUID           `json:"id"`
	Project      string              `json:"project"`
	Version      string              `json:"version"`
	Channel      string              `json:"channel"`
	Title        string              `json:"title"`
	Summary      string              `json:"summary"`
	ReleaseNotes string              `json:"releaseNotes"`
	Status       string              `json:"status"`
	PublishedAt  *time.Time          `json:"publishedAt"`
	CreatedAt    time.Time           `json:"createdAt"`
	UpdatedAt    time.Time           `json:"updatedAt"`
	Assets       []AdminReleaseAsset `json:"assets"`
}

type SaveReleaseAssetInput struct {
	Platform        string
	Architecture    string
	FileName        string
	FileSizeBytes   int64
	SHA256          string
	SignatureStatus string
	Source          string
	ObjectKey       string
	DownloadURL     string
}

type SaveReleaseInput struct {
	Project      string
	Version      string
	Channel      string
	Title        string
	Summary      string
	ReleaseNotes string
	Status       string
	Assets       []SaveReleaseAssetInput
	ActorUserID  uuid.UUID
}

type adminReleaseRow struct {
	ID           uuid.UUID              `gorm:"column:id;type:uuid;primaryKey"`
	Project      string                 `gorm:"column:project"`
	Version      string                 `gorm:"column:version"`
	Channel      string                 `gorm:"column:channel"`
	Title        string                 `gorm:"column:title"`
	Summary      string                 `gorm:"column:summary"`
	ReleaseNotes string                 `gorm:"column:release_notes"`
	Status       string                 `gorm:"column:status"`
	PublishedAt  *time.Time             `gorm:"column:published_at"`
	CreatedAt    time.Time              `gorm:"column:created_at"`
	UpdatedAt    time.Time              `gorm:"column:updated_at"`
	Assets       []adminReleaseAssetRow `gorm:"foreignKey:ReleaseID"`
}

func (adminReleaseRow) TableName() string { return "releases" }

type adminReleaseAssetRow struct {
	ID              uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ReleaseID       uuid.UUID `gorm:"column:release_id;type:uuid"`
	Platform        string    `gorm:"column:platform"`
	Architecture    string    `gorm:"column:architecture"`
	FileName        string    `gorm:"column:file_name"`
	FileSizeBytes   int64     `gorm:"column:file_size_bytes"`
	SHA256          string    `gorm:"column:sha256"`
	SignatureStatus string    `gorm:"column:signature_status"`
	ObjectKey       string    `gorm:"column:object_key"`
	DownloadURL     string    `gorm:"column:download_url"`
	Status          string    `gorm:"column:status"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

func (adminReleaseAssetRow) TableName() string { return "release_assets" }

type catalogAuditLogRow struct {
	ID           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	ActorUserID  *uuid.UUID `gorm:"column:actor_user_id;type:uuid"`
	Action       string     `gorm:"column:action"`
	ResourceType string     `gorm:"column:resource_type"`
	ResourceID   *uuid.UUID `gorm:"column:resource_id;type:uuid"`
	BeforeJSON   []byte     `gorm:"column:before_json;type:jsonb"`
	AfterJSON    []byte     `gorm:"column:after_json;type:jsonb"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

func (catalogAuditLogRow) TableName() string { return "audit_logs" }

func (s *Store) ListAdminReleases(ctx context.Context, limit int) ([]AdminRelease, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	var rows []adminReleaseRow
	if err := s.db.WithContext(ctx).Preload("Assets", func(db *gorm.DB) *gorm.DB {
		return db.Order("platform ASC, architecture ASC, id ASC")
	}).Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list admin releases: %w", err)
	}
	items := make([]AdminRelease, 0, len(rows))
	for _, row := range rows {
		items = append(items, adminReleaseFromRow(row))
	}
	return items, nil
}

func (s *Store) CreateRelease(ctx context.Context, input SaveReleaseInput) (AdminRelease, error) {
	input, err := validateReleaseInput(input)
	if err != nil {
		return AdminRelease{}, err
	}
	now := time.Now().UTC()
	row := adminReleaseRow{
		ID: uuid.New(), Project: input.Project, Version: input.Version, Channel: input.Channel, Title: input.Title,
		Summary: input.Summary, ReleaseNotes: input.ReleaseNotes, Status: input.Status,
		CreatedAt: now, UpdatedAt: now,
	}
	if input.Status == "published" {
		row.PublishedAt = &now
	}
	row.Assets = buildAdminAssetRows(row.ID, input.Assets, input.Status, now, nil)
	result := adminReleaseFromRow(row)
	afterJSON, _ := json.Marshal(result)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Assets").Create(&row).Error; err != nil {
			if isCatalogUniqueViolation(err) {
				return ErrReleaseVersionConflict
			}
			return fmt.Errorf("create release: %w", err)
		}
		if len(row.Assets) > 0 {
			if err := tx.Create(&row.Assets).Error; err != nil {
				return fmt.Errorf("create release files: %w", err)
			}
		}
		if err := tx.Create(&catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "release.create",
			ResourceType: "release", ResourceID: &row.ID, AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit release creation: %w", err)
		}
		return nil
	})
	if err != nil {
		return AdminRelease{}, err
	}
	return result, nil
}

func (s *Store) UpdateRelease(ctx context.Context, releaseID uuid.UUID, input SaveReleaseInput) (AdminRelease, error) {
	if releaseID == uuid.Nil {
		return AdminRelease{}, ErrReleaseNotFound
	}
	input, err := validateReleaseInput(input)
	if err != nil {
		return AdminRelease{}, err
	}
	var result AdminRelease
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current adminReleaseRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Assets").
			First(&current, "id = ?", releaseID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrReleaseNotFound
			}
			return fmt.Errorf("lock release: %w", err)
		}
		if current.Status == "withdrawn" {
			return ErrReleaseWithdrawn
		}
		if current.Project != input.Project {
			return ErrReleaseInvalid
		}
		if current.Status == "published" && input.Status == "draft" {
			return ErrReleaseInvalid
		}
		beforeJSON, _ := json.Marshal(adminReleaseFromRow(current))
		now := time.Now().UTC()
		publishedAt := current.PublishedAt
		if input.Status == "published" && publishedAt == nil {
			publishedAt = &now
		}
		if err := tx.Model(&adminReleaseRow{}).Where("id = ?", releaseID).Updates(map[string]any{
			"project": input.Project, "version": input.Version, "channel": input.Channel, "title": input.Title,
			"summary": input.Summary, "release_notes": input.ReleaseNotes, "status": input.Status,
			"published_at": publishedAt, "updated_at": now,
		}).Error; err != nil {
			if isCatalogUniqueViolation(err) {
				return ErrReleaseVersionConflict
			}
			return fmt.Errorf("update release: %w", err)
		}
		if err := tx.Where("release_id = ?", releaseID).Delete(&adminReleaseAssetRow{}).Error; err != nil {
			return fmt.Errorf("replace release files: %w", err)
		}
		assets := buildAdminAssetRows(releaseID, input.Assets, input.Status, now, current.Assets)
		if len(assets) > 0 {
			if err := tx.Create(&assets).Error; err != nil {
				return fmt.Errorf("store replacement release files: %w", err)
			}
		}
		updated := adminReleaseRow{
			ID: releaseID, Project: input.Project, Version: input.Version, Channel: input.Channel, Title: input.Title,
			Summary: input.Summary, ReleaseNotes: input.ReleaseNotes, Status: input.Status,
			PublishedAt: publishedAt, CreatedAt: current.CreatedAt, UpdatedAt: now, Assets: assets,
		}
		result = adminReleaseFromRow(updated)
		afterJSON, _ := json.Marshal(result)
		if err := tx.Create(&catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "release.update",
			ResourceType: "release", ResourceID: &releaseID, BeforeJSON: beforeJSON,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit release update: %w", err)
		}
		return nil
	})
	if err != nil {
		return AdminRelease{}, err
	}
	return result, nil
}

// PublishRelease performs the explicit confirmation step for a saved draft.
// Keeping this separate from draft updates lets browser and CLI clients require
// an intentional action without resending or accidentally changing file data.
func (s *Store) PublishRelease(ctx context.Context, releaseID, actorUserID uuid.UUID) (AdminRelease, error) {
	if releaseID == uuid.Nil || actorUserID == uuid.Nil {
		return AdminRelease{}, ErrReleaseNotFound
	}
	var result AdminRelease
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current adminReleaseRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Assets").
			First(&current, "id = ?", releaseID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrReleaseNotFound
			}
			return fmt.Errorf("lock release for publication: %w", err)
		}
		if current.Status == "withdrawn" {
			return ErrReleaseWithdrawn
		}
		if current.Status == "published" {
			result = adminReleaseFromRow(current)
			return nil
		}
		if len(current.Assets) == 0 {
			return ErrReleaseInvalid
		}
		for _, asset := range current.Assets {
			if !deploymentAssetMatchesRelease(
				current.Project, current.Version, asset.Platform, asset.Architecture, asset.FileName,
			) {
				return ErrReleaseAssetMismatch
			}
		}
		beforeJSON, _ := json.Marshal(adminReleaseFromRow(current))
		now := time.Now().UTC()
		if err := tx.Model(&adminReleaseRow{}).Where("id = ?", releaseID).Updates(map[string]any{
			"status": "published", "published_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("publish release: %w", err)
		}
		if err := tx.Model(&adminReleaseAssetRow{}).Where("release_id = ?", releaseID).
			Updates(map[string]any{"status": "published", "updated_at": now}).Error; err != nil {
			return fmt.Errorf("publish release files: %w", err)
		}
		for index := range current.Assets {
			current.Assets[index].Status = "published"
			current.Assets[index].UpdatedAt = now
		}
		current.Status, current.PublishedAt, current.UpdatedAt = "published", &now, now
		result = adminReleaseFromRow(current)
		afterJSON, _ := json.Marshal(result)
		if err := tx.Create(&catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "release.publish",
			ResourceType: "release", ResourceID: &releaseID, BeforeJSON: beforeJSON,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit release publication: %w", err)
		}
		return nil
	})
	if err != nil {
		return AdminRelease{}, err
	}
	return result, nil
}

func (s *Store) WithdrawRelease(ctx context.Context, releaseID, actorUserID uuid.UUID) error {
	if releaseID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrReleaseNotFound
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current adminReleaseRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Assets").
			First(&current, "id = ?", releaseID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrReleaseNotFound
			}
			return fmt.Errorf("lock release withdrawal: %w", err)
		}
		if current.Status == "withdrawn" {
			return nil
		}
		beforeJSON, _ := json.Marshal(adminReleaseFromRow(current))
		if err := tx.Model(&adminReleaseRow{}).Where("id = ?", releaseID).
			Updates(map[string]any{"status": "withdrawn", "updated_at": now}).Error; err != nil {
			return fmt.Errorf("withdraw release: %w", err)
		}
		if err := tx.Model(&adminReleaseAssetRow{}).Where("release_id = ?", releaseID).
			Updates(map[string]any{"status": "withdrawn", "updated_at": now}).Error; err != nil {
			return fmt.Errorf("withdraw release files: %w", err)
		}
		afterJSON, _ := json.Marshal(map[string]any{"status": "withdrawn"})
		if err := tx.Create(&catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "release.withdraw",
			ResourceType: "release", ResourceID: &releaseID, BeforeJSON: beforeJSON,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit release withdrawal: %w", err)
		}
		return nil
	})
}

// DeleteRelease permanently removes a release and its database asset records.
// The referenced S3, GitHub, mirror, or locally pushed objects are intentionally
// retained because they are managed outside the release catalog and may be
// shared with other systems or restored by a later catalog entry.
func (s *Store) DeleteRelease(ctx context.Context, releaseID, actorUserID uuid.UUID) error {
	if releaseID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrReleaseNotFound
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current adminReleaseRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Assets").
			First(&current, "id = ?", releaseID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrReleaseNotFound
			}
			return fmt.Errorf("lock release deletion: %w", err)
		}
		beforeJSON, _ := json.Marshal(adminReleaseFromRow(current))
		if err := tx.Where("release_id = ?", releaseID).Delete(&adminReleaseAssetRow{}).Error; err != nil {
			return fmt.Errorf("delete release files: %w", err)
		}
		if err := tx.Where("id = ?", releaseID).Delete(&adminReleaseRow{}).Error; err != nil {
			return fmt.Errorf("delete release: %w", err)
		}
		afterJSON, _ := json.Marshal(map[string]any{"deleted": true})
		if err := tx.Create(&catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "release.delete",
			ResourceType: "release", ResourceID: &releaseID, BeforeJSON: beforeJSON,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit release deletion: %w", err)
		}
		return nil
	})
}

func validateReleaseInput(input SaveReleaseInput) (SaveReleaseInput, error) {
	input.Project = strings.ToLower(strings.TrimSpace(input.Project))
	if input.Project == "" {
		input.Project = ReleaseProjectDesktop
	}
	input.Version = strings.TrimSpace(input.Version)
	input.Channel = strings.TrimSpace(input.Channel)
	input.Title = strings.TrimSpace(input.Title)
	input.Summary = strings.TrimSpace(input.Summary)
	input.ReleaseNotes = strings.TrimSpace(input.ReleaseNotes)
	input.Status = strings.TrimSpace(input.Status)
	if input.ActorUserID == uuid.Nil || !ValidReleaseProject(input.Project) || !validPlainText(input.Version, 50) ||
		!validPlainText(input.Title, 120) || utf8.RuneCountInString(input.Summary) > 1000 ||
		utf8.RuneCountInString(input.ReleaseNotes) > 50000 {
		return SaveReleaseInput{}, ErrReleaseInvalid
	}
	if input.Channel != "stable" && input.Channel != "beta" {
		return SaveReleaseInput{}, ErrReleaseInvalid
	}
	if input.Status != "draft" && input.Status != "published" {
		return SaveReleaseInput{}, ErrReleaseInvalid
	}
	if input.Status == "published" && len(input.Assets) == 0 {
		return SaveReleaseInput{}, ErrReleaseInvalid
	}
	if len(input.Assets) > 100 {
		return SaveReleaseInput{}, ErrReleaseInvalid
	}
	seen := make(map[string]struct{}, len(input.Assets))
	for index := range input.Assets {
		asset := &input.Assets[index]
		asset.Platform = strings.TrimSpace(asset.Platform)
		asset.Architecture = strings.TrimSpace(asset.Architecture)
		asset.FileName = strings.TrimSpace(asset.FileName)
		asset.SHA256 = strings.ToLower(strings.TrimSpace(asset.SHA256))
		asset.SignatureStatus = strings.TrimSpace(asset.SignatureStatus)
		asset.Source = strings.ToLower(strings.TrimSpace(asset.Source))
		asset.ObjectKey = strings.TrimSpace(asset.ObjectKey)
		asset.DownloadURL = strings.TrimSpace(asset.DownloadURL)
		if asset.Source == "" {
			asset.Source = "s3"
		}
		if asset.Platform != "web" && asset.Platform != "windows" && asset.Platform != "macos" && asset.Platform != "linux" &&
			asset.Platform != "android" && asset.Platform != "ios" {
			return SaveReleaseInput{}, ErrReleaseInvalid
		}
		if asset.Architecture != "x64" && asset.Architecture != "arm64" && asset.Architecture != "universal" {
			return SaveReleaseInput{}, ErrReleaseInvalid
		}
		if !deploymentAssetMatchesRelease(
			input.Project, input.Version, asset.Platform, asset.Architecture, asset.FileName,
		) {
			return SaveReleaseInput{}, ErrReleaseAssetMismatch
		}
		if !validPlainText(asset.FileName, 255) || asset.FileSizeBytes <= 0 || !sha256Pattern.MatchString(asset.SHA256) {
			return SaveReleaseInput{}, ErrReleaseInvalid
		}
		if asset.SignatureStatus != "unknown" && asset.SignatureStatus != "unsigned" && asset.SignatureStatus != "valid" {
			return SaveReleaseInput{}, ErrReleaseInvalid
		}
		switch asset.Source {
		case "s3":
			if !validReleaseObjectKey(asset.ObjectKey, asset.FileName) || !validHTTPDownloadURL(asset.DownloadURL) {
				return SaveReleaseInput{}, ErrReleaseInvalid
			}
		case "github":
			if !validGitHubReleaseAsset(asset.ObjectKey, asset.FileName, asset.DownloadURL) {
				return SaveReleaseInput{}, ErrReleaseInvalid
			}
		case "mirror":
			if !validMirrorReleaseAsset(asset.ObjectKey, asset.FileName, asset.DownloadURL) {
				return SaveReleaseInput{}, ErrReleaseInvalid
			}
		case "local":
			if !validLocalReleaseAsset(asset.ObjectKey, input.Project, input.Version, asset.Platform, asset.Architecture, asset.FileName, asset.SHA256, asset.DownloadURL) {
				return SaveReleaseInput{}, ErrReleaseInvalid
			}
		default:
			return SaveReleaseInput{}, ErrReleaseInvalid
		}
		key := asset.ObjectKey
		if _, exists := seen[key]; exists {
			return SaveReleaseInput{}, ErrReleaseInvalid
		}
		seen[key] = struct{}{}
	}
	return input, nil
}

func deploymentAssetMatchesRelease(project, version, platform, architecture, fileName string) bool {
	match := deploymentAssetPattern.FindStringSubmatch(fileName)
	if match == nil {
		lowerName := strings.ToLower(fileName)
		return !strings.HasPrefix(lowerName, "wenzwork-host-deployment-") &&
			!strings.HasPrefix(lowerName, "wenzwork-relay-deployment-") &&
			!strings.HasPrefix(lowerName, "wenzwork-device-agent-deployment-")
	}
	if project != ReleaseProjectWeb || canonicalDeploymentVersion(match[2]) != canonicalDeploymentVersion(version) {
		return false
	}
	expectedPlatform := map[string]string{"linux": "linux", "windows": "windows", "darwin": "macos"}[match[3]]
	expectedArchitecture := map[string]string{"amd64": "x64", "arm64": "arm64"}[match[4]]
	return platform == expectedPlatform && architecture == expectedArchitecture
}

func canonicalDeploymentVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1 && (value[0] == 'v' || value[0] == 'V') && value[1] >= '0' && value[1] <= '9' {
		return value[1:]
	}
	return value
}

func validReleaseObjectKey(objectKey, fileName string) bool {
	if objectKey == "" || len(objectKey) > 1024 || !utf8.ValidString(objectKey) ||
		!strings.HasPrefix(objectKey, "releases/") || path.Clean(objectKey) != objectKey ||
		path.Base(objectKey) != fileName {
		return false
	}
	for _, r := range objectKey {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validLocalReleaseAsset(objectKey, project, version, platform, architecture, fileName, digest, downloadURL string) bool {
	segments := strings.Split(objectKey, "/")
	return downloadURL == "" && len(segments) == 7 && segments[0] == "local" &&
		segments[1] == project && segments[2] == releaseVersionSegment(version) &&
		segments[3] == platform && segments[4] == architecture && segments[5] == digest &&
		segments[6] == fileName && path.Clean(objectKey) == objectKey
}

func releaseVersionSegment(version string) string {
	var builder strings.Builder
	lastWasDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(version)) {
		allowed := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if allowed {
			builder.WriteRune(r)
			lastWasDash = r == '-'
			continue
		}
		if !lastWasDash {
			builder.WriteByte('-')
			lastWasDash = true
		}
	}
	result := strings.Trim(builder.String(), ".-_")
	if result == "" {
		return "release"
	}
	return result
}

func validHTTPDownloadURL(rawURL string) bool {
	parsed, err := url.ParseRequestURI(rawURL)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil
}

func validGitHubReleaseAsset(objectKey, fileName, downloadURL string) bool {
	if len(objectKey) > 1024 || !utf8.ValidString(objectKey) || path.Clean(objectKey) != objectKey {
		return false
	}
	segments := strings.Split(objectKey, "/")
	if len(segments) != 6 || segments[0] != "github" || segments[3] != "assets" || segments[5] != fileName {
		return false
	}
	repository := segments[1] + "/" + segments[2]
	assetID, err := strconv.ParseInt(segments[4], 10, 64)
	if err != nil || assetID < 1 || !githubRepositoryPattern.MatchString(repository) || strings.Contains(repository, "..") {
		return false
	}
	parsed, err := url.ParseRequestURI(downloadURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || path.Base(parsed.Path) != fileName {
		return false
	}
	return strings.HasPrefix(parsed.Path, "/"+repository+"/releases/download/")
}

func validMirrorReleaseAsset(objectKey, fileName, downloadURL string) bool {
	if len(objectKey) > 1024 || !utf8.ValidString(objectKey) || path.Clean(objectKey) != objectKey {
		return false
	}
	segments := strings.Split(objectKey, "/")
	if len(segments) != 3 || segments[0] != "mirror" || !sha256Pattern.MatchString(segments[1]) ||
		segments[2] != fileName || path.Base(objectKey) != fileName {
		return false
	}
	parsed, err := url.ParseRequestURI(downloadURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	digest := sha256.Sum256([]byte(parsed.String()))
	return segments[1] == hex.EncodeToString(digest[:])
}

func validPlainText(value string, maxRunes int) bool {
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

func buildAdminAssetRows(releaseID uuid.UUID, inputs []SaveReleaseAssetInput, releaseStatus string, now time.Time, existing []adminReleaseAssetRow) []adminReleaseAssetRow {
	status := "pending"
	if releaseStatus == "published" {
		status = "published"
	}
	existingByObjectKey := make(map[string]adminReleaseAssetRow, len(existing))
	for _, asset := range existing {
		existingByObjectKey[asset.ObjectKey] = asset
	}
	rows := make([]adminReleaseAssetRow, 0, len(inputs))
	for _, input := range inputs {
		id := uuid.New()
		createdAt := now
		if current, ok := existingByObjectKey[input.ObjectKey]; ok {
			id = current.ID
			createdAt = current.CreatedAt
		}
		rows = append(rows, adminReleaseAssetRow{
			ID: id, ReleaseID: releaseID, Platform: input.Platform, Architecture: input.Architecture,
			FileName: input.FileName, FileSizeBytes: input.FileSizeBytes, SHA256: input.SHA256,
			SignatureStatus: input.SignatureStatus,
			ObjectKey:       input.ObjectKey,
			DownloadURL:     input.DownloadURL, Status: status, CreatedAt: createdAt, UpdatedAt: now,
		})
	}
	return rows
}

func adminReleaseFromRow(row adminReleaseRow) AdminRelease {
	assets := make([]AdminReleaseAsset, 0, len(row.Assets))
	for _, asset := range row.Assets {
		assets = append(assets, AdminReleaseAsset{
			ID: asset.ID, Platform: asset.Platform, Architecture: asset.Architecture,
			FileName: asset.FileName, FileSizeBytes: asset.FileSizeBytes, SHA256: asset.SHA256,
			SignatureStatus: asset.SignatureStatus, Source: releaseAssetSource(asset.ObjectKey),
			ObjectKey: asset.ObjectKey, DownloadURL: asset.DownloadURL, Status: asset.Status,
		})
	}
	return AdminRelease{
		ID: row.ID, Project: row.Project, Version: row.Version, Channel: row.Channel, Title: row.Title,
		Summary: row.Summary, ReleaseNotes: row.ReleaseNotes, Status: row.Status,
		PublishedAt: row.PublishedAt, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(), Assets: assets,
	}
}

func releaseAssetSource(objectKey string) string {
	if strings.HasPrefix(objectKey, "releases/") {
		return "s3"
	}
	if strings.HasPrefix(objectKey, "github/") {
		return "github"
	}
	if strings.HasPrefix(objectKey, "mirror/") {
		return "mirror"
	}
	if strings.HasPrefix(objectKey, "local/") {
		return "local"
	}
	return "custom"
}

func isCatalogUniqueViolation(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "23505") || strings.Contains(message, "duplicate key") || strings.Contains(message, "unique constraint")
}
