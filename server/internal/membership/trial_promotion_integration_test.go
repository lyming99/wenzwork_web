//go:build integration

package membership

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
)

var trialCodePattern = regexp.MustCompile(`WZM-[2-9A-HJ-NP-Z-]+`)

func TestTrialPromotionConcurrentClaimsNeverExceedDailyQuota(t *testing.T) {
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

	codec, err := NewCodeCodec([]byte(strings.Repeat("trial-concurrency-hmac-", 2)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	service, err := NewTrialPromotionService(
		db,
		codec,
		discardPromotionSender{},
		strings.Repeat("trial-concurrency-encryption-", 2),
	)
	if err != nil {
		t.Fatalf("NewTrialPromotionService() error = %v", err)
	}

	var originalSettings trialPromotionSettingsRow
	if err := db.First(&originalSettings, "singleton = 1").Error; err != nil {
		t.Fatalf("load trial settings: %v", err)
	}
	var originalBatch redemptionCodeBatchRow
	if err := db.First(&originalBatch, "id = ?", originalSettings.BatchID).Error; err != nil {
		t.Fatalf("load trial batch: %v", err)
	}

	fixedNow := time.Date(2099, 12, 30, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }
	claimDate := trialPromotionClaimDate(fixedNow)
	emailPrefix := "trial-concurrency-" + uuid.NewString()
	t.Cleanup(func() {
		var codeIDs []uuid.UUID
		_ = db.Model(&trialPromotionClaimRow{}).
			Where("email LIKE ?", emailPrefix+"%").
			Pluck("code_id", &codeIDs).Error
		_ = db.Where("email LIKE ?", emailPrefix+"%").Delete(&trialPromotionClaimRow{}).Error
		if len(codeIDs) > 0 {
			_ = db.Where("id IN ?", codeIDs).Delete(&redemptionCodeRow{}).Error
		}
		_ = db.Where("claim_date = ?", claimDate).Delete(&trialPromotionDayRow{}).Error
		_ = db.Model(&trialPromotionSettingsRow{}).Where("singleton = 1").
			Updates(map[string]any{
				"enabled": originalSettings.Enabled, "daily_quota": originalSettings.DailyQuota,
				"updated_at": originalSettings.UpdatedAt,
			}).Error
		_ = db.Model(&redemptionCodeBatchRow{}).Where("id = ?", originalBatch.ID).
			Updates(map[string]any{
				"quantity": originalBatch.Quantity, "status": originalBatch.Status,
				"updated_at": originalBatch.UpdatedAt,
			}).Error
	})

	if err := db.Model(&trialPromotionSettingsRow{}).Where("singleton = 1").
		Updates(map[string]any{
			"enabled": true, "daily_quota": 3, "updated_at": fixedNow,
		}).Error; err != nil {
		t.Fatalf("configure trial settings: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 12)
	var wait sync.WaitGroup
	for index := range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, claimErr := service.Claim(
				ctx,
				fmt.Sprintf("%s-%02d@example.test", emailPrefix, index),
				fmt.Sprintf("192.0.2.%d", index+1),
			)
			results <- claimErr
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	unavailable := 0
	for claimErr := range results {
		switch {
		case claimErr == nil:
			successes++
		case errors.Is(claimErr, ErrTrialPromotionUnavailable):
			unavailable++
		default:
			t.Fatalf("concurrent Claim() error = %v", claimErr)
		}
	}
	if successes != 3 || unavailable != 9 {
		t.Fatalf(
			"concurrent results: success=%d unavailable=%d, want 3 and 9",
			successes,
			unavailable,
		)
	}
	status, err := service.Status(ctx)
	if err != nil || status.ClaimedToday != 3 || status.RemainingToday != 0 ||
		status.Available {
		t.Fatalf("Status() after concurrent claims = %+v, %v", status, err)
	}
}

func TestTrialPromotionDailyRefreshAdminSettingsAndEmailBoundRedemption(t *testing.T) {
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

	codec, err := NewCodeCodec([]byte(strings.Repeat("trial-integration-hmac-", 2)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	sender := &recordingPromotionSender{}
	service, err := NewTrialPromotionService(
		db,
		codec,
		sender,
		strings.Repeat("trial-integration-encryption-", 2),
	)
	if err != nil {
		t.Fatalf("NewTrialPromotionService() error = %v", err)
	}
	store, err := NewStore(db, codec)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	var originalSettings trialPromotionSettingsRow
	if err := db.First(&originalSettings, "singleton = 1").Error; err != nil {
		t.Fatalf("load trial settings: %v", err)
	}
	var originalBatch redemptionCodeBatchRow
	if err := db.First(&originalBatch, "id = ?", originalSettings.BatchID).Error; err != nil {
		t.Fatalf("load trial batch: %v", err)
	}

	firstDay := time.Date(2099, 12, 28, 8, 0, 0, 0, time.UTC)
	secondDay := firstDay.Add(24 * time.Hour)
	service.now = func() time.Time { return firstDay }
	store.now = func() time.Time { return firstDay }
	firstDate := trialPromotionClaimDate(firstDay)
	secondDate := trialPromotionClaimDate(secondDay)

	firstUserID := uuid.New()
	secondUserID := uuid.New()
	thirdUserID := uuid.New()
	verifiedAt := firstDay.Add(-time.Hour)
	insertTestUsers(t, db, verifiedAt, firstUserID, secondUserID, thirdUserID)
	firstEmail := "membership-integration-" + firstUserID.String() + "@example.test"
	secondEmail := "membership-integration-" + secondUserID.String() + "@example.test"
	thirdEmail := "membership-integration-" + thirdUserID.String() + "@example.test"

	claimEmails := []string{firstEmail, secondEmail, thirdEmail}
	t.Cleanup(func() {
		var codeIDs []uuid.UUID
		_ = db.Model(&trialPromotionClaimRow{}).
			Where("lower(email) IN ?", claimEmails).
			Pluck("code_id", &codeIDs).Error
		_ = db.Exec(
			"DELETE FROM audit_logs WHERE actor_user_id IN ?",
			[]uuid.UUID{firstUserID, secondUserID, thirdUserID},
		).Error
		_ = db.Where(
			"user_id IN ?",
			[]uuid.UUID{firstUserID, secondUserID, thirdUserID},
		).Delete(&membershipEventRow{}).Error
		_ = db.Where(
			"user_id IN ?",
			[]uuid.UUID{firstUserID, secondUserID, thirdUserID},
		).Delete(&membershipRow{}).Error
		_ = db.Where("lower(email) IN ?", claimEmails).Delete(&trialPromotionClaimRow{}).Error
		if len(codeIDs) > 0 {
			_ = db.Where("id IN ?", codeIDs).Delete(&redemptionCodeRow{}).Error
		}
		_ = db.Where(
			"claim_date IN ?",
			[]time.Time{firstDate, secondDate},
		).Delete(&trialPromotionDayRow{}).Error
		_ = db.Model(&trialPromotionSettingsRow{}).Where("singleton = 1").
			Updates(map[string]any{
				"enabled": originalSettings.Enabled, "daily_quota": originalSettings.DailyQuota,
				"updated_at": originalSettings.UpdatedAt,
			}).Error
		_ = db.Model(&redemptionCodeBatchRow{}).Where("id = ?", originalBatch.ID).
			Updates(map[string]any{
				"quantity": originalBatch.Quantity, "status": originalBatch.Status,
				"updated_at": originalBatch.UpdatedAt,
			}).Error
		_ = db.Exec(
			"DELETE FROM user_roles WHERE user_id IN ?",
			[]uuid.UUID{firstUserID, secondUserID, thirdUserID},
		).Error
		_ = db.Exec(
			"DELETE FROM users WHERE id IN ?",
			[]uuid.UUID{firstUserID, secondUserID, thirdUserID},
		).Error
	})

	if err := db.Model(&trialPromotionSettingsRow{}).Where("singleton = 1").
		Updates(map[string]any{
			"enabled": true, "daily_quota": 2, "updated_at": firstDay,
		}).Error; err != nil {
		t.Fatalf("configure trial settings: %v", err)
	}

	firstResult, err := service.Claim(ctx, firstEmail, "192.0.2.1")
	if err != nil || !firstResult.NewClaim || firstResult.Promotion.RemainingToday != 1 {
		t.Fatalf("first Claim() = %+v, %v", firstResult, err)
	}
	duplicateResult, err := service.Claim(ctx, strings.ToUpper(firstEmail), "198.51.100.1")
	if err != nil || !duplicateResult.AlreadyClaimed || len(sender.messages) != 1 {
		t.Fatalf(
			"duplicate Claim() = %+v, %v, messages=%d",
			duplicateResult,
			err,
			len(sender.messages),
		)
	}
	secondResult, err := service.Claim(ctx, secondEmail, "192.0.2.2")
	if err != nil || !secondResult.NewClaim || secondResult.Promotion.RemainingToday != 0 {
		t.Fatalf("second Claim() = %+v, %v", secondResult, err)
	}
	if _, err := service.Claim(ctx, thirdEmail, "192.0.2.3"); !errors.Is(
		err,
		ErrTrialPromotionUnavailable,
	) {
		t.Fatalf("third same-day Claim() error = %v, want unavailable", err)
	}

	service.now = func() time.Time { return secondDay }
	store.now = func() time.Time { return secondDay }
	nextDayResult, err := service.Claim(ctx, thirdEmail, "192.0.2.3")
	if err != nil || !nextDayResult.NewClaim ||
		nextDayResult.Promotion.DailyLimit != 2 ||
		nextDayResult.Promotion.RemainingToday != 1 {
		t.Fatalf("next-day Claim() = %+v, %v", nextDayResult, err)
	}
	if len(sender.messages) != 3 {
		t.Fatalf("sent messages = %d, want 3", len(sender.messages))
	}
	claims, err := service.ListAdminClaims(ctx, TrialPromotionClaimFilter{Limit: 50})
	if err != nil || claims.Total != 3 || len(claims.Items) != 3 ||
		claims.Items[0].ClaimDate != secondDate.Format(time.DateOnly) {
		t.Fatalf("ListAdminClaims() = %+v, %v", claims, err)
	}

	adminOverview, err := service.UpdateAdminSettings(ctx, firstUserID, false, 120)
	if err != nil {
		t.Fatalf("UpdateAdminSettings() error = %v", err)
	}
	if adminOverview.Enabled || adminOverview.DailyQuota != 120 ||
		adminOverview.TodayLimit != 120 || adminOverview.ClaimedToday != 1 {
		t.Fatalf("admin overview = %+v", adminOverview)
	}

	firstCode := trialCodePattern.FindString(sender.messages[0].Text)
	secondCode := trialCodePattern.FindString(sender.messages[1].Text)
	if firstCode == "" || secondCode == "" {
		t.Fatal("trial email did not contain parseable redemption codes")
	}
	if _, err := store.Redeem(ctx, secondUserID, firstCode); !errors.Is(
		err,
		ErrCodeUnavailable,
	) {
		t.Fatalf("Redeem(first code from another email) error = %v, want unavailable", err)
	}
	firstRedemption, err := store.Redeem(ctx, firstUserID, firstCode)
	if err != nil || firstRedemption.Membership.ExpiresAt == nil {
		t.Fatalf("Redeem(first code) = %+v, %v", firstRedemption, err)
	}
	if got := firstRedemption.Membership.ExpiresAt.Sub(secondDay); got != 30*24*time.Hour {
		t.Fatalf("trial membership duration = %s, want 30 days", got)
	}
	if _, err := store.Redeem(ctx, secondUserID, secondCode); err != nil {
		t.Fatalf("Redeem(second code) error = %v", err)
	}
}
