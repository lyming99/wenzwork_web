package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PushReleaseInput struct {
	Project      string
	Version      string
	Channel      string
	SoftwareName string
	Title        string
	Summary      string
	ReleaseNotes string
	Publish      bool
	Assets       []SaveReleaseAssetInput
}

// PushRelease atomically merges locally uploaded artifacts into one project
// version. This lets native builds arrive from different operating systems
// without deleting artifacts already pushed for the same version.
func (s *Store) PushRelease(ctx context.Context, input PushReleaseInput) (AdminRelease, bool, error) {
	input.Project = strings.ToLower(strings.TrimSpace(input.Project))
	input.Version = strings.TrimSpace(input.Version)
	input.Channel = strings.TrimSpace(input.Channel)
	input.SoftwareName = strings.TrimSpace(input.SoftwareName)
	input.Title = strings.TrimSpace(input.Title)
	input.Summary = strings.TrimSpace(input.Summary)
	input.ReleaseNotes = strings.TrimSpace(input.ReleaseNotes)
	if !ValidReleaseProject(input.Project) || !validPlainText(input.Version, 50) ||
		input.SoftwareName != "" && !validPlainText(input.SoftwareName, 80) || len(input.Assets) == 0 || len(input.Assets) > 100 {
		return AdminRelease{}, false, ErrReleaseInvalid
	}
	for _, asset := range input.Assets {
		if strings.ToLower(strings.TrimSpace(asset.Source)) != "local" {
			return AdminRelease{}, false, ErrReleaseInvalid
		}
	}

	var result AdminRelease
	created := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current adminReleaseRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Assets").
			Where("project = ? AND version = ?", input.Project, input.Version).First(&current).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("lock pushed release: %w", err)
		}
		exists := err == nil
		if exists && current.Status == "withdrawn" {
			return ErrReleaseWithdrawn
		}

		mergedAssets := mergePushedReleaseAssets(current.Assets, input.Assets)
		var beforeJSON []byte
		if exists {
			beforeJSON, _ = json.Marshal(adminReleaseFromRow(current))
		}
		status := "draft"
		if input.Publish || exists && current.Status == "published" {
			status = "published"
		}
		channel := input.Channel
		if channel == "" {
			if exists {
				channel = current.Channel
			} else {
				channel = "stable"
			}
		}
		softwareName := input.SoftwareName
		if softwareName == "" {
			softwareName = defaultReleaseSoftwareName(input.Project)
		}
		defaultNotice := softwareName + " " + input.Version + "更新啦~"
		title := pushedReleaseText(input.Title, current.Title, defaultNotice, exists)
		summary := pushedReleaseText(input.Summary, current.Summary, defaultNotice, exists)
		releaseNotes := pushedReleaseText(input.ReleaseNotes, current.ReleaseNotes, defaultNotice, exists)
		validated, err := validateReleaseInput(SaveReleaseInput{
			Project: input.Project, Version: input.Version, Channel: channel,
			Title: title, Summary: summary, ReleaseNotes: releaseNotes, Status: status,
			Assets: mergedAssets, ActorUserID: uuid.New(),
		})
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		if !exists {
			current = adminReleaseRow{
				ID: uuid.New(), Project: validated.Project, Version: validated.Version, Channel: validated.Channel,
				Title: validated.Title, Summary: validated.Summary, ReleaseNotes: validated.ReleaseNotes,
				Status: validated.Status, CreatedAt: now, UpdatedAt: now,
			}
			if status == "published" {
				current.PublishedAt = &now
			}
			current.Assets = buildAdminAssetRows(current.ID, validated.Assets, status, now, nil)
			if err := tx.Omit("Assets").Create(&current).Error; err != nil {
				if isCatalogUniqueViolation(err) {
					return ErrReleaseVersionConflict
				}
				return fmt.Errorf("create pushed release: %w", err)
			}
			created = true
		} else {
			publishedAt := current.PublishedAt
			if status == "published" && publishedAt == nil {
				publishedAt = &now
			}
			if err := tx.Model(&adminReleaseRow{}).Where("id = ?", current.ID).Updates(map[string]any{
				"channel": validated.Channel, "title": validated.Title, "summary": validated.Summary,
				"release_notes": validated.ReleaseNotes, "status": status, "published_at": publishedAt, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("update pushed release: %w", err)
			}
			if err := tx.Where("release_id = ?", current.ID).Delete(&adminReleaseAssetRow{}).Error; err != nil {
				return fmt.Errorf("replace pushed release files: %w", err)
			}
			current.Assets = buildAdminAssetRows(current.ID, validated.Assets, status, now, current.Assets)
			current.Channel, current.Title, current.Summary = validated.Channel, validated.Title, validated.Summary
			current.ReleaseNotes, current.Status, current.PublishedAt, current.UpdatedAt = validated.ReleaseNotes, status, publishedAt, now
		}
		if len(current.Assets) > 0 {
			if err := tx.Create(&current.Assets).Error; err != nil {
				return fmt.Errorf("store pushed release files: %w", err)
			}
		}
		result = adminReleaseFromRow(current)
		afterJSON, _ := json.Marshal(result)
		action := "release.push.update"
		if created {
			action = "release.push.create"
		}
		if err := tx.Create(&catalogAuditLogRow{
			ID: uuid.New(), ActorUserID: nil, Action: action, ResourceType: "release",
			ResourceID: &current.ID, BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit pushed release: %w", err)
		}
		return nil
	})
	if err != nil {
		return AdminRelease{}, false, err
	}
	return result, created, nil
}

func mergePushedReleaseAssets(current []adminReleaseAssetRow, pushed []SaveReleaseAssetInput) []SaveReleaseAssetInput {
	merged := make([]SaveReleaseAssetInput, 0, len(current)+len(pushed))
	indexByFileName := make(map[string]int, len(current)+len(pushed))
	for _, asset := range current {
		indexByFileName[asset.FileName] = len(merged)
		merged = append(merged, SaveReleaseAssetInput{
			Platform: asset.Platform, Architecture: asset.Architecture, FileName: asset.FileName,
			FileSizeBytes: asset.FileSizeBytes, SHA256: asset.SHA256, SignatureStatus: asset.SignatureStatus,
			Source: releaseAssetSource(asset.ObjectKey), ObjectKey: asset.ObjectKey, DownloadURL: asset.DownloadURL,
		})
	}
	for _, asset := range pushed {
		if index, ok := indexByFileName[strings.TrimSpace(asset.FileName)]; ok {
			merged[index] = asset
			continue
		}
		indexByFileName[strings.TrimSpace(asset.FileName)] = len(merged)
		merged = append(merged, asset)
	}
	return merged
}

func pushedReleaseText(requested, current, fallback string, exists bool) string {
	if requested != "" {
		return requested
	}
	if exists && current != "" {
		return current
	}
	return fallback
}

func defaultReleaseSoftwareName(project string) string {
	switch project {
	case ReleaseProjectWeb:
		return "WenzWork 服务端"
	case ReleaseProjectMobile:
		return "WenzWork 手机端"
	default:
		return "WenzWork 桌面端"
	}
}
