//go:build integration

package catalog

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
)

func TestStoreFiltersPublicCatalogAndResolvesPublishedAssets(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	firstReleaseID := uuid.New()
	newerReleaseID := uuid.New()
	draftReleaseID := uuid.New()
	assetID := uuid.New()
	newerAssetID := uuid.New()
	draftAssetID := uuid.New()
	randomSuffix := uuid.NewString()[:16]
	pricingCode := "integration-hidden-" + randomSuffix
	versionPrefix := "integration-" + randomSuffix
	publishedAt := time.Date(2026, 7, 21, 4, 0, 0, 0, time.UTC)

	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM release_assets WHERE release_id IN ?", []uuid.UUID{firstReleaseID, newerReleaseID, draftReleaseID}).Error
		_ = db.Exec("DELETE FROM releases WHERE id IN ?", []uuid.UUID{firstReleaseID, newerReleaseID, draftReleaseID}).Error
		_ = db.Exec("DELETE FROM pricing_plans WHERE code = ?", pricingCode).Error
	})

	if err := db.Exec(`
		INSERT INTO pricing_plans
			(code, name, description, price_minor, currency, billing_period, features_json, status, published_at)
		VALUES (?, 'Hidden', '', 123, 'CNY', 'month', '[]'::jsonb, 'archived', ?)
	`, pricingCode, publishedAt).Error; err != nil {
		t.Fatalf("insert hidden pricing plan: %v", err)
	}

	if err := db.Exec(`
		INSERT INTO releases (id, version, channel, title, summary, release_notes, status, published_at)
		VALUES
			(?, ?, 'beta', 'Windows release', 'Windows summary', 'Notes', 'published', ?),
			(?, ?, 'beta', 'Linux release', 'Linux summary', 'Notes', 'published', ?),
			(?, ?, 'beta', 'Draft release', 'Draft summary', 'Notes', 'draft', NULL)
	`,
		firstReleaseID, versionPrefix+"-windows", publishedAt,
		newerReleaseID, versionPrefix+"-linux", publishedAt.Add(time.Hour),
		draftReleaseID, versionPrefix+"-draft",
	).Error; err != nil {
		t.Fatalf("insert releases: %v", err)
	}

	if err := db.Exec(`
		INSERT INTO release_assets
			(id, release_id, platform, architecture, file_name, file_size_bytes, sha256, signature_status, object_key, download_url, status)
		VALUES
			(?, ?, 'windows', 'x64', 'wenzwork.exe', 1024, ?, 'valid', ?, 'https://downloads.example.test/wenzwork.exe', 'published'),
			(?, ?, 'linux', 'x64', 'wenzwork.AppImage', 2048, ?, 'valid', ?, 'https://downloads.example.test/wenzwork.AppImage', 'published'),
			(?, ?, 'windows', 'x64', 'draft.exe', 1024, ?, 'unknown', ?, 'https://downloads.example.test/draft.exe', 'published')
	`,
		assetID, firstReleaseID, strings.Repeat("a", 64), "releases/"+assetID.String()+"/wenzwork.exe",
		newerAssetID, newerReleaseID, strings.Repeat("b", 64), "releases/"+newerAssetID.String()+"/wenzwork.AppImage",
		draftAssetID, draftReleaseID, strings.Repeat("c", 64), "releases/"+draftAssetID.String()+"/draft.exe",
	).Error; err != nil {
		t.Fatalf("insert release assets: %v", err)
	}

	plans, err := store.ListPricingPlans(ctx)
	if err != nil {
		t.Fatalf("ListPricingPlans() error = %v", err)
	}
	if len(plans) != 2 || plans[0].Code != "free" || plans[1].Code != "pro" {
		t.Fatalf("published plans = %+v, want only free and pro", plans)
	}

	// Use the beta channel so the assertion is isolated from fixtures created
	// concurrently by other integration-test packages in the shared database.
	latest, err := store.LatestRelease(ctx, ReleaseFilter{Channel: "beta", Platform: "windows", Architecture: "x64"})
	if err != nil {
		t.Fatalf("LatestRelease() error = %v", err)
	}
	if latest.ID != firstReleaseID || len(latest.Assets) != 1 || latest.Assets[0].ID != assetID {
		t.Fatalf("filtered latest release = %+v, want published Windows release", latest)
	}

	unfiltered, err := store.LatestRelease(ctx, ReleaseFilter{Channel: "beta"})
	if err != nil {
		t.Fatalf("LatestRelease(unfiltered) error = %v", err)
	}
	if unfiltered.ID != newerReleaseID {
		t.Fatalf("unfiltered latest ID = %s, want newer release %s", unfiltered.ID, newerReleaseID)
	}

	target, err := store.ReleaseAssetDownload(ctx, assetID)
	if err != nil || target.ObjectKey != "releases/"+assetID.String()+"/wenzwork.exe" || target.FileName != "wenzwork.exe" {
		t.Fatalf("ReleaseAssetDownload() = %+v, %v", target, err)
	}
	if _, err := store.ReleaseAssetDownload(ctx, draftAssetID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("draft release asset error = %v, want ErrAssetNotFound", err)
	}
}

func TestStoreAdminReleaseLifecyclePublishesFilesAndAnnouncement(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	actorID := uuid.New()
	actorEmail := "release-admin-" + actorID.String() + "@example.test"
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-test-hash', 'Release Admin', 'active', now())
	`, actorID, actorEmail).Error; err != nil {
		t.Fatalf("insert release actor: %v", err)
	}
	var releaseID uuid.UUID
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ? OR resource_id = ?", actorID, releaseID).Error
		_ = db.Exec("DELETE FROM release_assets WHERE release_id = ?", releaseID).Error
		_ = db.Exec("DELETE FROM releases WHERE id = ?", releaseID).Error
		_ = db.Exec("DELETE FROM user_roles WHERE user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})

	version := "admin-integration-" + uuid.NewString()[:12]
	input := SaveReleaseInput{
		Version: version, Channel: "stable", Title: "Initial announcement", Summary: "Summary",
		ReleaseNotes: "First notes", Status: "draft", ActorUserID: actorID,
		Assets: []SaveReleaseAssetInput{{
			Platform: "windows", Architecture: "x64", FileName: "WenzWork.exe", FileSizeBytes: 4096,
			SHA256: strings.Repeat("d", 64), SignatureStatus: "valid", Source: "s3",
			ObjectKey:   "releases/" + version + "/windows/x64/id/WenzWork.exe",
			DownloadURL: "https://downloads.example.test/" + version + "/WenzWork.exe",
		}},
	}
	created, err := store.CreateRelease(ctx, input)
	if err != nil || created.Status != "draft" || len(created.Assets) != 1 || created.Assets[0].Status != "pending" {
		t.Fatalf("CreateRelease() = %+v, %v", created, err)
	}
	releaseID = created.ID
	input.Title = "Published announcement"
	input.ReleaseNotes = "Important fixes and improvements"
	updated, err := store.UpdateRelease(ctx, releaseID, input)
	if err != nil || updated.Status != "draft" || updated.Assets[0].Status != "pending" {
		t.Fatalf("UpdateRelease(draft) = %+v, %v", updated, err)
	}
	published, err := store.PublishRelease(ctx, releaseID, actorID)
	if err != nil || published.Status != "published" || published.PublishedAt == nil || published.Assets[0].Status != "published" {
		t.Fatalf("PublishRelease() = %+v, %v", published, err)
	}
	if published.Assets[0].ID != created.Assets[0].ID {
		t.Fatalf("release file ID changed while editing announcement: %s -> %s", created.Assets[0].ID, published.Assets[0].ID)
	}
	target, err := store.ReleaseAssetDownload(ctx, published.Assets[0].ID)
	if err != nil || target.ObjectKey != input.Assets[0].ObjectKey || target.FileName != input.Assets[0].FileName {
		t.Fatalf("ReleaseAssetDownload(published) = %+v, %v", target, err)
	}
	items, err := store.ListAdminReleases(ctx, 100)
	if err != nil {
		t.Fatalf("ListAdminReleases() error = %v", err)
	}
	var found *AdminRelease
	for index := range items {
		if items[index].ID == releaseID {
			found = &items[index]
			break
		}
	}
	if found == nil || found.ReleaseNotes != input.ReleaseNotes || len(found.Assets) != 1 {
		t.Fatalf("admin release list item = %+v", found)
	}
	if err := store.WithdrawRelease(ctx, releaseID, actorID); err != nil {
		t.Fatalf("WithdrawRelease() error = %v", err)
	}
	if _, err := store.ReleaseAssetDownload(ctx, published.Assets[0].ID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("withdrawn asset error = %v", err)
	}
	if err := store.DeleteRelease(ctx, releaseID, actorID); err != nil {
		t.Fatalf("DeleteRelease() error = %v", err)
	}
	var releaseCount, assetCount int64
	if err := db.Table("releases").Where("id = ?", releaseID).Count(&releaseCount).Error; err != nil || releaseCount != 0 {
		t.Fatalf("deleted release count = %d, %v", releaseCount, err)
	}
	if err := db.Table("release_assets").Where("release_id = ?", releaseID).Count(&assetCount).Error; err != nil || assetCount != 0 {
		t.Fatalf("deleted release asset count = %d, %v", assetCount, err)
	}
	if err := store.DeleteRelease(ctx, releaseID, actorID); !errors.Is(err, ErrReleaseNotFound) {
		t.Fatalf("repeated DeleteRelease() error = %v, want ErrReleaseNotFound", err)
	}
	var auditCount int64
	if err := db.Table("audit_logs").Where("resource_id = ? AND action IN ?", releaseID, []string{"release.create", "release.update", "release.publish", "release.withdraw", "release.delete"}).Count(&auditCount).Error; err != nil || auditCount != 5 {
		t.Fatalf("release audit count = %d, %v", auditCount, err)
	}
}

func TestStorePersistsReleaseDeliverySettingsWithVersionAndAudit(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	var original releaseDeliverySettingsRow
	if err := db.First(&original, "singleton = ?", true).Error; err != nil {
		t.Fatalf("load original delivery settings: %v", err)
	}
	actorID := uuid.New()
	actorEmail := "delivery-admin-" + actorID.String() + "@example.test"
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-test-hash', 'Delivery Admin', 'active', now())
	`, actorID, actorEmail).Error; err != nil {
		t.Fatalf("insert delivery actor: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Model(&releaseDeliverySettingsRow{}).Where("singleton = ?", true).Updates(map[string]any{
			"download_mode": original.DownloadMode,
			"s3_url_prefix": original.S3URLPrefix,
			"version":       original.Version,
			"updated_by":    original.UpdatedBy,
			"updated_at":    original.UpdatedAt,
		}).Error
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM user_roles WHERE user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})

	updated, err := store.UpdateReleaseDeliverySettings(ctx, UpdateReleaseDeliverySettingsInput{
		DownloadMode: ReleaseDownloadS3Redirect, S3URLPrefix: "https://downloads.example.test/wenzwork",
		ExpectedVersion: original.Version, ActorUserID: actorID,
	})
	if err != nil || updated.DownloadMode != ReleaseDownloadS3Redirect || updated.Version != original.Version+1 {
		t.Fatalf("UpdateReleaseDeliverySettings() = %+v, %v", updated, err)
	}
	persisted, err := store.GetReleaseDeliverySettings(ctx)
	if err != nil || persisted.DownloadMode != updated.DownloadMode || persisted.S3URLPrefix != updated.S3URLPrefix || persisted.Version != updated.Version {
		t.Fatalf("GetReleaseDeliverySettings() = %+v, %v, want %+v", persisted, err, updated)
	}
	if _, err := store.UpdateReleaseDeliverySettings(ctx, UpdateReleaseDeliverySettingsInput{
		DownloadMode: ReleaseDownloadProxyCached, ExpectedVersion: original.Version, ActorUserID: actorID,
	}); !errors.Is(err, ErrReleaseDeliveryConflict) {
		t.Fatalf("stale update error = %v, want ErrReleaseDeliveryConflict", err)
	}
	var auditCount int64
	if err := db.Table("audit_logs").Where("actor_user_id = ? AND action = 'release.delivery.update'", actorID).Count(&auditCount).Error; err != nil || auditCount != 1 {
		t.Fatalf("delivery audit count = %d, %v", auditCount, err)
	}
}

func TestStorePersistsReleaseSourceSettingsWithVersionAuditAndOneTimeSeed(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := NewStore(db, WithReleaseSourceTokenEncryptionKey("integration-release-source-key-at-least-32-bytes"))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	var original releaseSourceSettingsRow
	if err := db.First(&original, "singleton = ?", true).Error; err != nil {
		t.Fatalf("load original source settings: %v", err)
	}
	actorID := uuid.New()
	actorEmail := "source-admin-" + actorID.String() + "@example.test"
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-test-hash', 'Source Admin', 'active', now())
	`, actorID, actorEmail).Error; err != nil {
		t.Fatalf("insert source actor: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Model(&releaseSourceSettingsRow{}).Where("singleton = ?", true).Updates(map[string]any{
			"github_repository":        original.GitHubRepository,
			"github_token_ciphertext":  original.GitHubTokenCiphertext,
			"github_token_initialized": original.GitHubTokenInitialized,
			"version":                  original.Version,
			"updated_by":               original.UpdatedBy,
			"updated_at":               original.UpdatedAt,
		}).Error
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM user_roles WHERE user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})

	if err := db.Model(&releaseSourceSettingsRow{}).Where("singleton = ?", true).Updates(map[string]any{
		"github_repository":        "",
		"github_token_ciphertext":  nil,
		"github_token_initialized": false,
		"version":                  original.Version,
	}).Error; err != nil {
		t.Fatalf("clear source settings for seed test: %v", err)
	}
	if err := store.EnsureReleaseSourceSettings(ctx, "seed/initial", "legacy-token"); err != nil {
		t.Fatalf("EnsureReleaseSourceSettings(initial) error = %v", err)
	}
	seeded, err := store.GetReleaseSourceSettings(ctx)
	if err != nil || seeded.GitHubRepository != "seed/initial" || !seeded.GitHubTokenConfigured || seeded.Version != original.Version {
		t.Fatalf("seeded source settings = %+v, %v", seeded, err)
	}
	seededCredentials, err := store.GetReleaseSourceCredentials(ctx)
	if err != nil || seededCredentials.GitHubToken != "legacy-token" {
		t.Fatalf("seeded source credentials = %+v, %v", seededCredentials, err)
	}

	replacementToken := "github_pat_replacement"
	updated, err := store.UpdateReleaseSourceSettings(ctx, UpdateReleaseSourceSettingsInput{
		GitHubRepository: "acme/wenzwork", GitHubToken: &replacementToken,
		ExpectedVersion: seeded.Version, ActorUserID: actorID,
	})
	if err != nil || updated.GitHubRepository != "acme/wenzwork" || !updated.GitHubTokenConfigured || updated.Version != seeded.Version+1 {
		t.Fatalf("UpdateReleaseSourceSettings() = %+v, %v", updated, err)
	}
	if err := store.EnsureReleaseSourceSettings(ctx, "seed/ignored", "legacy-ignored"); err != nil {
		t.Fatalf("EnsureReleaseSourceSettings() error = %v", err)
	}
	persistedCredentials, err := store.GetReleaseSourceCredentials(ctx)
	if err != nil || persistedCredentials.GitHubRepository != updated.GitHubRepository || persistedCredentials.GitHubToken != replacementToken {
		t.Fatalf("GetReleaseSourceCredentials() = %+v, %v", persistedCredentials, err)
	}

	cleared, err := store.UpdateReleaseSourceSettings(ctx, UpdateReleaseSourceSettingsInput{
		GitHubRepository: "acme/wenzwork", ClearGitHubToken: true,
		ExpectedVersion: updated.Version, ActorUserID: actorID,
	})
	if err != nil || cleared.GitHubTokenConfigured || cleared.Version != updated.Version+1 {
		t.Fatalf("clear release source token = %+v, %v", cleared, err)
	}
	if err := store.EnsureReleaseSourceSettings(ctx, "seed/ignored", "legacy-must-not-return"); err != nil {
		t.Fatalf("EnsureReleaseSourceSettings(after clear) error = %v", err)
	}
	clearedCredentials, err := store.GetReleaseSourceCredentials(ctx)
	if err != nil || clearedCredentials.GitHubToken != "" {
		t.Fatalf("cleared source credentials = %+v, %v", clearedCredentials, err)
	}
	if _, err := store.UpdateReleaseSourceSettings(ctx, UpdateReleaseSourceSettingsInput{
		GitHubRepository: "acme/other", ExpectedVersion: original.Version, ActorUserID: actorID,
	}); !errors.Is(err, ErrReleaseSourceConflict) {
		t.Fatalf("stale source update error = %v, want ErrReleaseSourceConflict", err)
	}
	var auditCount int64
	if err := db.Table("audit_logs").Where("actor_user_id = ? AND action = 'release.source.update'", actorID).Count(&auditCount).Error; err != nil || auditCount != 2 {
		t.Fatalf("source audit count = %d, %v", auditCount, err)
	}
	var leakedTokenCount int64
	if err := db.Table("audit_logs").Where("actor_user_id = ? AND (before_json::text LIKE ? OR after_json::text LIKE ?)", actorID, "%replacement%", "%replacement%").Count(&leakedTokenCount).Error; err != nil || leakedTokenCount != 0 {
		t.Fatalf("source token audit leak count = %d, %v", leakedTokenCount, err)
	}
}

func TestStoreAdminPricingLifecycleKeepsPublishedSnapshotsVersionsAndAudit(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	actorID := uuid.New()
	actorEmail := "pricing-admin-" + actorID.String() + "@example.test"
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-test-hash', 'Pricing Admin', 'active', now())
	`, actorID, actorEmail).Error; err != nil {
		t.Fatalf("insert pricing actor: %v", err)
	}
	var planID uuid.UUID
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ? OR resource_id = ?", actorID, planID).Error
		_ = db.Exec("DELETE FROM pricing_plan_versions WHERE pricing_plan_id = ?", planID).Error
		_ = db.Exec("DELETE FROM pricing_plans WHERE id = ?", planID).Error
		_ = db.Exec("DELETE FROM user_roles WHERE user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})

	code := "pricing-integration-" + uuid.NewString()[:12]
	trafficLimit := int64(250)
	input := SavePricingPlanInput{
		Code: code, Name: "Integration Pro", Description: "Initial public copy", PriceMinor: nil,
		Currency: "CNY", BillingPeriod: "redemption", Features: []string{"First feature"},
		RemoteAccessEnabled: true, DeviceLimit: 42, MonthlyTrafficLimitGB: &trafficLimit,
		SortOrder: 90, ActorUserID: actorID,
	}
	created, err := store.CreatePricingPlan(ctx, input)
	if err != nil || created.Status != "draft" || created.Version != 1 || !created.HasUnpublishedChanges {
		t.Fatalf("CreatePricingPlan() = %+v, %v", created, err)
	}
	planID = created.ID

	price := int64(12800)
	originalPrice := int64(16800)
	input.PriceMinor, input.OriginalPriceMinor = &price, &originalPrice
	input.BillingPeriod, input.ExpectedVersion = "year", created.Version
	if _, err := store.UpdatePricingPlan(ctx, planID, input); !errors.Is(err, ErrPricingPlanConfirmationRequired) {
		t.Fatalf("unconfirmed price update error = %v", err)
	}
	input.ConfirmPriceChange = true
	updated, err := store.UpdatePricingPlan(ctx, planID, input)
	if err != nil || updated.Version != 2 {
		t.Fatalf("UpdatePricingPlan() = %+v, %v", updated, err)
	}
	published, err := store.PublishPricingPlan(ctx, planID, PricingPlanActionInput{
		ExpectedVersion: updated.Version, Confirm: true, ActorUserID: actorID,
	})
	if err != nil || published.Status != "published" || published.Version != 3 || published.PublishedVersion == nil || *published.PublishedVersion != 3 || published.HasUnpublishedChanges {
		t.Fatalf("PublishPricingPlan() = %+v, %v", published, err)
	}

	publicPlans, err := store.ListPricingPlans(ctx)
	if err != nil {
		t.Fatalf("ListPricingPlans() after publish error = %v", err)
	}
	publicPlan := findPublicPricingPlan(publicPlans, code)
	if publicPlan == nil || publicPlan.Description != "Initial public copy" || publicPlan.PriceMinor == nil ||
		*publicPlan.PriceMinor != price || publicPlan.OriginalPriceMinor == nil ||
		*publicPlan.OriginalPriceMinor != originalPrice || !publicPlan.RemoteAccessEnabled ||
		publicPlan.DeviceLimit != 42 || publicPlan.MonthlyTrafficLimitGB == nil ||
		*publicPlan.MonthlyTrafficLimitGB != trafficLimit {
		t.Fatalf("published pricing plan = %+v", publicPlan)
	}

	input.Description, input.ExpectedVersion, input.ConfirmPriceChange = "Staged copy", published.Version, false
	input.RemoteAccessEnabled, input.DeviceLimit, input.MonthlyTrafficLimitGB = false, 7, nil
	staged, err := store.UpdatePricingPlan(ctx, planID, input)
	if err != nil || staged.Version != 4 || !staged.HasUnpublishedChanges {
		t.Fatalf("UpdatePricingPlan(staged) = %+v, %v", staged, err)
	}
	publicPlans, _ = store.ListPricingPlans(ctx)
	if current := findPublicPricingPlan(publicPlans, code); current == nil || current.Description != "Initial public copy" ||
		!current.RemoteAccessEnabled || current.DeviceLimit != 42 || current.MonthlyTrafficLimitGB == nil {
		t.Fatalf("draft edit leaked into public catalog: %+v", current)
	}

	republished, err := store.PublishPricingPlan(ctx, planID, PricingPlanActionInput{
		ExpectedVersion: staged.Version, Confirm: true, ActorUserID: actorID,
	})
	if err != nil || republished.Version != 5 {
		t.Fatalf("PublishPricingPlan(staged) = %+v, %v", republished, err)
	}
	if _, err := store.ArchivePricingPlan(ctx, planID, PricingPlanActionInput{
		ExpectedVersion: staged.Version, Confirm: true, ActorUserID: actorID,
	}); !errors.Is(err, ErrPricingPlanVersionConflict) {
		t.Fatalf("stale archive error = %v", err)
	}
	archived, err := store.ArchivePricingPlan(ctx, planID, PricingPlanActionInput{
		ExpectedVersion: republished.Version, Confirm: true, ActorUserID: actorID,
	})
	if err != nil || archived.Status != "archived" || archived.Version != 6 {
		t.Fatalf("ArchivePricingPlan() = %+v, %v", archived, err)
	}
	publicPlans, _ = store.ListPricingPlans(ctx)
	if findPublicPricingPlan(publicPlans, code) != nil {
		t.Fatal("archived pricing plan remained public")
	}
	relisted, err := store.PublishPricingPlan(ctx, planID, PricingPlanActionInput{
		ExpectedVersion: archived.Version, Confirm: true, ActorUserID: actorID,
	})
	if err != nil || relisted.Status != "published" || relisted.Version != 7 {
		t.Fatalf("PublishPricingPlan(archived) = %+v, %v", relisted, err)
	}
	publicPlans, _ = store.ListPricingPlans(ctx)
	if current := findPublicPricingPlan(publicPlans, code); current == nil || current.Description != "Staged copy" ||
		current.RemoteAccessEnabled || current.DeviceLimit != 7 || current.MonthlyTrafficLimitGB != nil {
		t.Fatalf("relisted pricing plan = %+v", current)
	}

	var versionCount, auditCount int64
	if err := db.Table("pricing_plan_versions").Where("pricing_plan_id = ?", planID).Count(&versionCount).Error; err != nil || versionCount != 7 {
		t.Fatalf("pricing version count = %d, %v", versionCount, err)
	}
	if err := db.Table("audit_logs").Where("resource_id = ? AND resource_type = 'pricing_plan'", planID).Count(&auditCount).Error; err != nil || auditCount != 7 {
		t.Fatalf("pricing audit count = %d, %v", auditCount, err)
	}
}

func findPublicPricingPlan(plans []PricingPlan, code string) *PricingPlan {
	for index := range plans {
		if plans[index].Code == code {
			return &plans[index]
		}
	}
	return nil
}
