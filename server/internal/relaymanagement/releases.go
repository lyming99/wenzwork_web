package relaymanagement

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrelease"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	relayReleaseVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	relayReleaseKeyIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)
	relayReleaseHexPattern     = regexp.MustCompile(`^[0-9a-f]+$`)
	relayPackagePattern        = regexp.MustCompile(`^wenzwork-relay(?:-deployment)?-([A-Za-z0-9._+-]+)-(linux|windows|darwin)-(amd64|arm64)\.tar\.gz$`)
)

func (store *Store) ListManagedReleases(ctx context.Context) ([]Release, error) {
	var rows []releaseRow
	if err := store.db.WithContext(ctx).Order("build_time DESC, version DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list managed Relay releases: %w", err)
	}
	items := make([]Release, 0, len(rows))
	for _, row := range rows {
		item, err := loadRelease(ctx, store.db, row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (store *Store) CreateRelease(ctx context.Context, input SaveReleaseInput) (Release, error) {
	input, err := validateSaveRelease(input, store.now())
	if err != nil {
		return Release{}, err
	}
	now := store.now().UTC()
	row := releaseRow{
		ID: uuid.New(), Version: input.Version, Platform: input.Platform, Architecture: input.Architecture,
		ProtocolMin: input.ProtocolMin, ProtocolMax: input.ProtocolMax, BuildCommit: input.BuildCommit,
		BuildTime: input.BuildTime, SigningKeyID: input.SigningKeyID, ManifestSHA256: input.ManifestSHA256,
		ManifestSignature: input.ManifestSignature, Status: "draft", CreatedBy: uuidPointer(input.ActorUserID),
	}
	var result Release
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("create Relay release: %w", err)
		}
		if err := createReleaseArtifacts(tx, row.ID, input.Artifacts); err != nil {
			return err
		}
		result, err = loadRelease(ctx, tx, row)
		if err != nil {
			return err
		}
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay.release.create", "relay_server_release", row.ID, nil, result, now)
	})
	return result, err
}

func (store *Store) UpdateRelease(ctx context.Context, releaseID uuid.UUID, input SaveReleaseInput) (Release, error) {
	if releaseID == uuid.Nil {
		return Release{}, ErrInvalidInput
	}
	input, err := validateSaveRelease(input, store.now())
	if err != nil {
		return Release{}, err
	}
	now := store.now().UTC()
	var result Release
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row releaseRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", releaseID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay release: %w", err)
		}
		if row.Status != "draft" {
			return ErrConflict
		}
		before, err := loadRelease(ctx, tx, row)
		if err != nil {
			return err
		}
		updates := map[string]any{
			"version": input.Version, "platform": input.Platform, "architecture": input.Architecture,
			"protocol_min": input.ProtocolMin, "protocol_max": input.ProtocolMax,
			"build_commit": input.BuildCommit, "build_time": input.BuildTime,
			"signing_key_id": input.SigningKeyID, "manifest_sha256": input.ManifestSHA256,
			"manifest_signature": input.ManifestSignature, "updated_at": now,
		}
		if err := tx.Model(&row).Updates(updates).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("update Relay release: %w", err)
		}
		if err := tx.Where("release_id = ?", row.ID).Delete(&artifactRow{}).Error; err != nil {
			return fmt.Errorf("replace Relay release artifacts: %w", err)
		}
		if err := createReleaseArtifacts(tx, row.ID, input.Artifacts); err != nil {
			return err
		}
		row.Version, row.Platform, row.Architecture = input.Version, input.Platform, input.Architecture
		row.ProtocolMin, row.ProtocolMax = input.ProtocolMin, input.ProtocolMax
		row.BuildCommit, row.BuildTime, row.SigningKeyID = input.BuildCommit, input.BuildTime, input.SigningKeyID
		row.ManifestSHA256, row.ManifestSignature = input.ManifestSHA256, input.ManifestSignature
		result, err = loadRelease(ctx, tx, row)
		if err != nil {
			return err
		}
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay.release.update", "relay_server_release", row.ID, before, result, now)
	})
	return result, err
}

func (store *Store) PublishRelease(ctx context.Context, releaseID, actorUserID uuid.UUID) (Release, error) {
	return store.changeReleaseStatus(ctx, releaseID, actorUserID, "published")
}

func (store *Store) RetireRelease(ctx context.Context, releaseID, actorUserID uuid.UUID) (Release, error) {
	return store.changeReleaseStatus(ctx, releaseID, actorUserID, "retired")
}

func (store *Store) changeReleaseStatus(ctx context.Context, releaseID, actorUserID uuid.UUID, target string) (Release, error) {
	if releaseID == uuid.Nil || actorUserID == uuid.Nil || (target != "published" && target != "retired") {
		return Release{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var result Release
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row releaseRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", releaseID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay release status: %w", err)
		}
		before, err := loadRelease(ctx, tx, row)
		if err != nil {
			return err
		}
		if target == "published" {
			if row.Status != "draft" {
				return ErrConflict
			}
			if _, err := validateSaveRelease(saveInputFromRelease(before, actorUserID), now); err != nil {
				return err
			}
		} else if row.Status != "published" {
			return ErrConflict
		}
		if err := tx.Model(&row).Updates(map[string]any{"status": target, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("change Relay release status: %w", err)
		}
		row.Status = target
		result = before
		result.Status = target
		return appendAudit(tx, uuidPointer(actorUserID), "relay.release."+target, "relay_server_release", row.ID, before, result, now)
	})
	return result, err
}

func (store *Store) DeleteRelease(ctx context.Context, releaseID, actorUserID uuid.UUID) error {
	if releaseID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row releaseRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", releaseID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay release deletion: %w", err)
		}
		if row.Status != "draft" && row.Status != "retired" && row.Status != "revoked" {
			return ErrConflict
		}
		var references int64
		if err := tx.Model(&installationRow{}).Where("release_id = ? AND status <> ?", row.ID, "deleted").Count(&references).Error; err != nil {
			return fmt.Errorf("count Relay release references: %w", err)
		}
		if references != 0 {
			return ErrConflict
		}
		before, err := loadRelease(ctx, tx, row)
		if err != nil {
			return err
		}
		if err := tx.Where("release_id = ?", row.ID).Delete(&artifactRow{}).Error; err != nil {
			return fmt.Errorf("delete Relay release artifacts: %w", err)
		}
		if err := tx.Delete(&row).Error; err != nil {
			return fmt.Errorf("delete Relay release: %w", err)
		}
		return appendAudit(tx, uuidPointer(actorUserID), "relay.release.delete", "relay_server_release", row.ID, before, nil, now)
	})
}

func loadRelease(ctx context.Context, db *gorm.DB, row releaseRow) (Release, error) {
	var artifacts []artifactRow
	if err := db.WithContext(ctx).Where("release_id = ?", row.ID).Order("file_name").Find(&artifacts).Error; err != nil {
		return Release{}, fmt.Errorf("list Relay release artifacts: %w", err)
	}
	result := releaseFromRow(row)
	result.Artifacts = make([]Artifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		result.Artifacts = append(result.Artifacts, Artifact{
			ID: artifact.ID, FileName: artifact.FileName, FileSizeBytes: artifact.FileSizeBytes,
			SHA256: artifact.SHA256, Signature: artifact.Signature, ObjectKey: artifact.ObjectKey,
		})
	}
	return result, nil
}

func createReleaseArtifacts(tx *gorm.DB, releaseID uuid.UUID, inputs []SaveReleaseArtifactInput) error {
	rows := make([]artifactRow, 0, len(inputs))
	for _, artifact := range inputs {
		rows = append(rows, artifactRow{
			ID: uuid.New(), ReleaseID: releaseID, FileName: artifact.FileName,
			FileSizeBytes: artifact.FileSizeBytes, SHA256: artifact.SHA256,
			Signature: artifact.Signature, ObjectKey: artifact.ObjectKey,
		})
	}
	if err := tx.Create(&rows).Error; err != nil {
		return fmt.Errorf("create Relay release artifacts: %w", err)
	}
	return nil
}

func validateSaveRelease(input SaveReleaseInput, now time.Time) (SaveReleaseInput, error) {
	input.Version = strings.TrimSpace(input.Version)
	input.Platform = strings.ToLower(strings.TrimSpace(input.Platform))
	input.Architecture = strings.ToLower(strings.TrimSpace(input.Architecture))
	input.BuildCommit = strings.ToLower(strings.TrimSpace(input.BuildCommit))
	input.SigningKeyID = strings.TrimSpace(input.SigningKeyID)
	input.ManifestSHA256 = strings.ToLower(strings.TrimSpace(input.ManifestSHA256))
	input.ManifestSignature = strings.TrimSpace(input.ManifestSignature)
	if input.ActorUserID == uuid.Nil || !relayReleaseVersionPattern.MatchString(input.Version) ||
		!relayrelease.SupportsTarget(input.Platform, input.Architecture) || input.ProtocolMin != 2 || input.ProtocolMax != 2 ||
		len(input.BuildCommit) < 40 || len(input.BuildCommit) > 64 ||
		!relayReleaseHexPattern.MatchString(input.BuildCommit) || input.BuildTime.IsZero() ||
		input.BuildTime.After(now.UTC().Add(5*time.Minute)) || !relayReleaseKeyIDPattern.MatchString(input.SigningKeyID) ||
		len(input.ManifestSHA256) != 64 || !relayReleaseHexPattern.MatchString(input.ManifestSHA256) ||
		!validReleaseSignature(input.ManifestSignature) || len(input.Artifacts) < 1 || len(input.Artifacts) > 16 {
		return SaveReleaseInput{}, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(input.Artifacts))
	packages := 0
	for index := range input.Artifacts {
		artifact := &input.Artifacts[index]
		artifact.FileName = strings.TrimSpace(artifact.FileName)
		artifact.SHA256 = strings.ToLower(strings.TrimSpace(artifact.SHA256))
		artifact.Signature = strings.TrimSpace(artifact.Signature)
		artifact.ObjectKey = strings.TrimSpace(artifact.ObjectKey)
		if !validReleaseFileName(artifact.FileName) || artifact.FileSizeBytes < 1 ||
			len(artifact.SHA256) != 64 || !relayReleaseHexPattern.MatchString(artifact.SHA256) ||
			!validReleaseSignature(artifact.Signature) || !validReleaseObjectKey(artifact.ObjectKey, artifact.FileName) {
			return SaveReleaseInput{}, ErrInvalidInput
		}
		if _, exists := seen[artifact.FileName]; exists {
			return SaveReleaseInput{}, ErrInvalidInput
		}
		seen[artifact.FileName] = struct{}{}
		if strings.HasSuffix(strings.ToLower(artifact.FileName), ".tar.gz") {
			if !relayPackageMatchesRelease(artifact.FileName, input.Version, input.Platform, input.Architecture) {
				return SaveReleaseInput{}, ErrInvalidInput
			}
			packages++
		}
	}
	if packages != 1 {
		return SaveReleaseInput{}, ErrInvalidInput
	}
	input.BuildTime = input.BuildTime.UTC()
	return input, nil
}

func relayPackageMatchesRelease(fileName, version, platform, architecture string) bool {
	match := relayPackagePattern.FindStringSubmatch(fileName)
	return match != nil && canonicalRelayVersion(match[1]) == canonicalRelayVersion(version) &&
		match[2] == platform && match[3] == architecture
}

func canonicalRelayVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1 && (value[0] == 'v' || value[0] == 'V') && value[1] >= '0' && value[1] <= '9' {
		return value[1:]
	}
	return value
}

func validReleaseSignature(value string) bool {
	if len(value) < 16 || len(value) > 4096 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func validReleaseFileName(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "/\\\x00\r\n")
}

func validReleaseObjectKey(objectKey, fileName string) bool {
	if objectKey == "" || len(objectKey) > 1024 || !utf8.ValidString(objectKey) || path.IsAbs(objectKey) ||
		path.Clean(objectKey) != objectKey || path.Base(objectKey) != fileName ||
		strings.ContainsAny(objectKey, "\\?#'\" \t\r\n\x00") {
		return false
	}
	for _, segment := range strings.Split(objectKey, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func saveInputFromRelease(release Release, actorUserID uuid.UUID) SaveReleaseInput {
	artifacts := make([]SaveReleaseArtifactInput, 0, len(release.Artifacts))
	for _, artifact := range release.Artifacts {
		artifacts = append(artifacts, SaveReleaseArtifactInput{
			FileName: artifact.FileName, FileSizeBytes: artifact.FileSizeBytes, SHA256: artifact.SHA256,
			Signature: artifact.Signature, ObjectKey: artifact.ObjectKey,
		})
	}
	return SaveReleaseInput{
		Version: release.Version, Platform: release.Platform, Architecture: release.Architecture,
		ProtocolMin: release.ProtocolMin, ProtocolMax: release.ProtocolMax, BuildCommit: release.BuildCommit,
		BuildTime: release.BuildTime, SigningKeyID: release.SigningKeyID,
		ManifestSHA256: release.ManifestSHA256, ManifestSignature: release.ManifestSignature,
		Artifacts: artifacts, ActorUserID: actorUserID,
	}
}
