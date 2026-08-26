//go:build integration

package membership

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
)

type discardPromotionSender struct{}

func (discardPromotionSender) Send(context.Context, mailer.Message) error { return nil }

func TestBetaPromotionQuotaDuplicateAndDeliveryRetry(t *testing.T) {
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

	codec, err := NewCodeCodec([]byte(strings.Repeat("promotion-integration-hmac-", 2)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	sender := &recordingPromotionSender{}
	service, err := NewBetaPromotionService(
		db, codec, sender, strings.Repeat("promotion-integration-encryption-", 2),
	)
	if err != nil {
		t.Fatalf("NewBetaPromotionService() error = %v", err)
	}
	fixedNow := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }
	service.campaignCode = "beta-test-" + uuid.NewString()

	var plan membershipPlanRow
	if err := db.Where("code = 'pro' AND status = 'active'").First(&plan).Error; err != nil {
		t.Fatalf("load Pro plan: %v", err)
	}
	days := 365
	batch := redemptionCodeBatchRow{
		ID: uuid.New(), Name: "promotion-integration-" + uuid.NewString(), PlanID: plan.ID,
		GrantType: string(GrantDuration), GrantDays: &days, Quantity: 2, Status: "active",
		Note: "integration test", CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatalf("create promotion batch: %v", err)
	}
	campaign := betaPromotionCampaignRow{
		Code: service.campaignCode, BatchID: batch.ID, Quota: 2, Status: "active",
		RedemptionPolicy: betaPromotionRedemptionPolicy,
		CreatedAt:        fixedNow, UpdatedAt: fixedNow,
	}
	if err := db.Create(&campaign).Error; err != nil {
		t.Fatalf("create promotion campaign: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Where("campaign_code = ?", service.campaignCode).Delete(&betaPromotionClaimRow{}).Error
		_ = db.Where("batch_id = ?", batch.ID).Delete(&redemptionCodeRow{}).Error
		_ = db.Where("code = ?", service.campaignCode).Delete(&betaPromotionCampaignRow{}).Error
		_ = db.Where("id = ?", batch.ID).Delete(&redemptionCodeBatchRow{}).Error
	})

	sender.err = errors.New("temporary SMTP failure")
	firstResult, err := service.Claim(ctx, "First@Example.test", "192.0.2.1")
	if !errors.Is(err, ErrBetaPromotionDelivery) {
		t.Fatalf("first Claim() error = %v, want delivery error", err)
	}
	if !firstResult.NewClaim || firstResult.Promotion.Remaining != 1 || len(sender.messages) != 1 {
		t.Fatalf("first claim result = %+v, messages = %d", firstResult, len(sender.messages))
	}
	firstMessage := sender.messages[0].Text

	var failedClaim betaPromotionClaimRow
	if err := db.First(&failedClaim, "campaign_code = ? AND email = ?", service.campaignCode, "first@example.test").Error; err != nil {
		t.Fatalf("load failed claim: %v", err)
	}
	if failedClaim.DeliveryStatus != "failed" || len(failedClaim.CodeCiphertext) == 0 {
		t.Fatalf("failed claim delivery state = %q ciphertext bytes = %d", failedClaim.DeliveryStatus, len(failedClaim.CodeCiphertext))
	}

	sender.err = nil
	retryResult, err := service.Claim(ctx, "first@example.test", "198.51.100.1")
	if err != nil {
		t.Fatalf("retry Claim() error = %v", err)
	}
	if !retryResult.AlreadyClaimed || retryResult.DeliveryStatus != "sent" || len(sender.messages) != 2 {
		t.Fatalf("retry result = %+v, messages = %d", retryResult, len(sender.messages))
	}
	if sender.messages[1].Text != firstMessage {
		t.Fatal("delivery retry generated a different redemption code")
	}

	duplicateResult, err := service.Claim(ctx, "FIRST@example.test", "203.0.113.1")
	if err != nil || !duplicateResult.AlreadyClaimed || len(sender.messages) != 2 {
		t.Fatalf("duplicate result = %+v error = %v messages = %d", duplicateResult, err, len(sender.messages))
	}

	if _, err := service.Claim(ctx, "second@example.test", "192.0.2.1"); err != nil {
		t.Fatalf("second Claim() error = %v", err)
	}
	if _, err := service.Claim(ctx, "third@example.test", "192.0.2.1"); !errors.Is(err, ErrBetaPromotionExhausted) {
		t.Fatalf("third Claim() error = %v, want exhausted", err)
	}

	status, err := service.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Limit != 2 || status.Claimed != 2 || status.Remaining != 0 || status.Available {
		t.Fatalf("promotion status = %+v", status)
	}

	var sentClaim betaPromotionClaimRow
	if err := db.First(&sentClaim, "id = ?", failedClaim.ID).Error; err != nil {
		t.Fatalf("reload sent claim: %v", err)
	}
	if sentClaim.DeliveryStatus != "sent" || sentClaim.SentAt == nil || len(sentClaim.CodeCiphertext) != 0 {
		t.Fatalf("sent claim delivery state = %q sentAt = %v ciphertext bytes = %d", sentClaim.DeliveryStatus, sentClaim.SentAt, len(sentClaim.CodeCiphertext))
	}

	var codeCount int64
	if err := db.Model(&redemptionCodeRow{}).Where("batch_id = ?", batch.ID).Count(&codeCount).Error; err != nil {
		t.Fatalf("count promotion codes: %v", err)
	}
	if codeCount != 2 {
		t.Fatalf("promotion code count = %d, want 2", codeCount)
	}
}

func TestBetaPromotionConcurrentClaimsNeverExceedQuota(t *testing.T) {
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

	codec, err := NewCodeCodec([]byte(strings.Repeat("promotion-concurrency-hmac-", 2)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	service, err := NewBetaPromotionService(
		db, codec, discardPromotionSender{}, strings.Repeat("promotion-concurrency-encryption-", 2),
	)
	if err != nil {
		t.Fatalf("NewBetaPromotionService() error = %v", err)
	}
	fixedNow := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }
	service.campaignCode = "beta-concurrent-" + uuid.NewString()

	var plan membershipPlanRow
	if err := db.Where("code = 'pro' AND status = 'active'").First(&plan).Error; err != nil {
		t.Fatalf("load Pro plan: %v", err)
	}
	days := 365
	batch := redemptionCodeBatchRow{
		ID: uuid.New(), Name: "promotion-concurrency-" + uuid.NewString(), PlanID: plan.ID,
		GrantType: string(GrantDuration), GrantDays: &days, Quantity: 5, Status: "active",
		Note: "integration concurrency test", CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatalf("create promotion batch: %v", err)
	}
	if err := db.Create(&betaPromotionCampaignRow{
		Code: service.campaignCode, BatchID: batch.ID, Quota: 5, Status: "active",
		RedemptionPolicy: betaPromotionRedemptionPolicy,
		CreatedAt:        fixedNow, UpdatedAt: fixedNow,
	}).Error; err != nil {
		t.Fatalf("create promotion campaign: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Where("campaign_code = ?", service.campaignCode).Delete(&betaPromotionClaimRow{}).Error
		_ = db.Where("batch_id = ?", batch.ID).Delete(&redemptionCodeRow{}).Error
		_ = db.Where("code = ?", service.campaignCode).Delete(&betaPromotionCampaignRow{}).Error
		_ = db.Where("id = ?", batch.ID).Delete(&redemptionCodeBatchRow{}).Error
	})

	start := make(chan struct{})
	results := make(chan error, 12)
	var wait sync.WaitGroup
	for index := 0; index < 12; index++ {
		wait.Add(1)
		go func(claimIndex int) {
			defer wait.Done()
			<-start
			_, claimErr := service.Claim(
				ctx,
				fmt.Sprintf("concurrent-%02d@example.test", claimIndex),
				fmt.Sprintf("192.0.2.%d", claimIndex+1),
			)
			results <- claimErr
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	exhausted := 0
	for claimErr := range results {
		switch {
		case claimErr == nil:
			successes++
		case errors.Is(claimErr, ErrBetaPromotionExhausted):
			exhausted++
		default:
			t.Fatalf("concurrent Claim() unexpected error = %v", claimErr)
		}
	}
	if successes != 5 || exhausted != 7 {
		t.Fatalf("concurrent claims: success=%d exhausted=%d, want 5 and 7", successes, exhausted)
	}

	var claimCount int64
	if err := db.Model(&betaPromotionClaimRow{}).Where("campaign_code = ?", service.campaignCode).Count(&claimCount).Error; err != nil {
		t.Fatalf("count concurrent claims: %v", err)
	}
	var codeCount int64
	if err := db.Model(&redemptionCodeRow{}).Where("batch_id = ?", batch.ID).Count(&codeCount).Error; err != nil {
		t.Fatalf("count concurrent codes: %v", err)
	}
	if claimCount != 5 || codeCount != 5 {
		t.Fatalf("stored concurrent rows: claims=%d codes=%d, want 5 and 5", claimCount, codeCount)
	}
}

func TestBetaPromotionAdminCanClearAndRestoreRemainingQuota(t *testing.T) {
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

	codec, err := NewCodeCodec([]byte(strings.Repeat("promotion-admin-hmac-", 2)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	service, err := NewBetaPromotionService(
		db, codec, discardPromotionSender{}, strings.Repeat("promotion-admin-encryption-", 2),
	)
	if err != nil {
		t.Fatalf("NewBetaPromotionService() error = %v", err)
	}
	fixedNow := time.Date(2026, 7, 23, 13, 30, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }
	service.campaignCode = "beta-admin-" + uuid.NewString()

	actorID := uuid.New()
	verifiedAt := fixedNow.Add(-time.Hour)
	insertTestUsers(t, db, verifiedAt, actorID)
	var plan membershipPlanRow
	if err := db.Where("code = 'pro' AND status = 'active'").First(&plan).Error; err != nil {
		t.Fatalf("load Pro plan: %v", err)
	}
	days := 365
	batch := redemptionCodeBatchRow{
		ID: uuid.New(), Name: "promotion-admin-" + uuid.NewString(), PlanID: plan.ID,
		GrantType: string(GrantDuration), GrantDays: &days, Quantity: 3, Status: "active",
		Note: "integration admin test", CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}
	if err := db.Create(&batch).Error; err != nil {
		t.Fatalf("create promotion batch: %v", err)
	}
	campaign := betaPromotionCampaignRow{
		Code: service.campaignCode, BatchID: batch.ID, Quota: 3, ClaimedCount: 1, Status: "active",
		RedemptionPolicy: betaPromotionRedemptionPolicy, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}
	if err := db.Create(&campaign).Error; err != nil {
		t.Fatalf("create promotion campaign: %v", err)
	}
	issued, err := codec.Generate()
	if err != nil {
		t.Fatalf("generate promotion code: %v", err)
	}
	codeID := uuid.New()
	if err := db.Create(&redemptionCodeRow{
		ID: codeID, BatchID: batch.ID, CodeDigest: issued.Digest, CodeHint: issued.Hint,
		Status: "active", CreatedAt: fixedNow,
	}).Error; err != nil {
		t.Fatalf("create promotion code: %v", err)
	}
	claimID := uuid.New()
	if err := db.Create(&betaPromotionClaimRow{
		ID: claimID, CampaignCode: service.campaignCode, Email: "member@example.test", CodeID: codeID,
		ClientIPDigest: codec.Digest("admin-test-ip"), DeliveryStatus: "sent", DeliveryAttempts: 1,
		LastDeliveryAttemptAt: fixedNow, SentAt: &fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}).Error; err != nil {
		t.Fatalf("create promotion claim: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Where("campaign_code = ?", service.campaignCode).Delete(&betaPromotionClaimRow{}).Error
		_ = db.Where("batch_id = ?", batch.ID).Delete(&redemptionCodeRow{}).Error
		_ = db.Where("code = ?", service.campaignCode).Delete(&betaPromotionCampaignRow{}).Error
		_ = db.Where("id = ?", batch.ID).Delete(&redemptionCodeBatchRow{}).Error
		cleanupTestMembershipData(db, actorID)
	})

	overview, err := service.AdminOverview(ctx)
	if err != nil {
		t.Fatalf("AdminOverview() error = %v", err)
	}
	if overview.Remaining != 2 || overview.SentDeliveryCount != 1 || overview.ActiveCodeCount != 1 {
		t.Fatalf("AdminOverview() = %+v", overview)
	}
	claims, err := service.ListAdminClaims(ctx, BetaPromotionClaimFilter{
		Query: "MEMBER", DeliveryStatus: "sent", RedemptionStatus: "active", Limit: 50,
	})
	if err != nil || claims.Total != 1 || len(claims.Items) != 1 || claims.Items[0].CodeHint != issued.Hint {
		t.Fatalf("ListAdminClaims() = %+v, %v", claims, err)
	}

	groupQRCode := encodePromotionTestPNG(t, 128, 128)
	configured, err := service.UpdateAdminGroupQRCode(ctx, actorID, "image/png", groupQRCode)
	if err != nil {
		t.Fatalf("UpdateAdminGroupQRCode() error = %v", err)
	}
	if !configured.GroupQRCodeConfigured || configured.GroupQRCodeURL == nil ||
		configured.GroupQRCodeUpdatedAt == nil {
		t.Fatalf("configured group QR overview = %+v", configured)
	}
	storedGroupQRCode, err := service.GroupQRCode(ctx)
	if err != nil || storedGroupQRCode.ContentType != "image/png" ||
		!bytes.Equal(storedGroupQRCode.Content, groupQRCode) {
		t.Fatalf("GroupQRCode() = contentType %q bytes %d, %v", storedGroupQRCode.ContentType, len(storedGroupQRCode.Content), err)
	}

	removed, err := service.RemoveAdminGroupQRCode(ctx, actorID)
	if err != nil {
		t.Fatalf("RemoveAdminGroupQRCode() error = %v", err)
	}
	if removed.GroupQRCodeConfigured || removed.GroupQRCodeURL != nil ||
		removed.GroupQRCodeUpdatedAt != nil {
		t.Fatalf("removed group QR overview = %+v", removed)
	}
	if _, err := service.GroupQRCode(ctx); !errors.Is(err, ErrBetaPromotionGroupQRCodeNotConfigured) {
		t.Fatalf("GroupQRCode() after removal error = %v, want not configured", err)
	}

	cleared, err := service.UpdateAdminRemaining(ctx, actorID, 0)
	if err != nil {
		t.Fatalf("UpdateAdminRemaining(0) error = %v", err)
	}
	if cleared.Status != "disabled" || cleared.Remaining != 0 || cleared.Available || cleared.Limit != 1 {
		t.Fatalf("cleared overview = %+v", cleared)
	}
	var batchStatus string
	if err := db.Model(&redemptionCodeBatchRow{}).Where("id = ?", batch.ID).Pluck("status", &batchStatus).Error; err != nil {
		t.Fatalf("load cleared batch status: %v", err)
	}
	if batchStatus != "active" {
		t.Fatalf("cleared batch status = %q, want active so issued codes remain redeemable", batchStatus)
	}

	if err := db.Model(&redemptionCodeBatchRow{}).Where("id = ?", batch.ID).Update("status", "exhausted").Error; err != nil {
		t.Fatalf("simulate exhausted promotion batch: %v", err)
	}
	restored, err := service.UpdateAdminRemaining(ctx, actorID, 2)
	if err != nil {
		t.Fatalf("UpdateAdminRemaining(2) error = %v", err)
	}
	if restored.Status != "active" || restored.Remaining != 2 || !restored.Available || restored.Limit != 3 {
		t.Fatalf("restored overview = %+v", restored)
	}
	if err := db.Model(&redemptionCodeBatchRow{}).Where("id = ?", batch.ID).Pluck("status", &batchStatus).Error; err != nil {
		t.Fatalf("load restored batch status: %v", err)
	}
	if batchStatus != "active" {
		t.Fatalf("restored batch status = %q, want active", batchStatus)
	}
	var auditCount int64
	if err := db.Model(&auditLogRow{}).Where("actor_user_id = ? AND action = 'beta_promotion.remaining.update'", actorID).Count(&auditCount).Error; err != nil {
		t.Fatalf("count promotion admin audits: %v", err)
	}
	if auditCount != 2 {
		t.Fatalf("promotion admin audit count = %d, want 2", auditCount)
	}
	var groupQRAuditCount int64
	if err := db.Model(&auditLogRow{}).
		Where("actor_user_id = ? AND action IN ?", actorID, []string{
			"beta_promotion.group_qr.update",
			"beta_promotion.group_qr.remove",
		}).
		Count(&groupQRAuditCount).Error; err != nil {
		t.Fatalf("count promotion group QR audits: %v", err)
	}
	if groupQRAuditCount != 2 {
		t.Fatalf("promotion group QR audit count = %d, want 2", groupQRAuditCount)
	}
}

func TestBetaPromotionRedemptionIsEmailBoundSingleUseAndRejectsLifetimeMembers(t *testing.T) {
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

	codec, err := NewCodeCodec([]byte(strings.Repeat("promotion-redemption-hmac-", 2)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	store, err := NewStore(db, codec)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	fixedNow := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return fixedNow }

	matchingUserID := uuid.New()
	otherUserID := uuid.New()
	lifetimeUserID := uuid.New()
	verifiedAt := fixedNow.Add(-time.Hour)
	insertTestUsers(t, db, verifiedAt, matchingUserID, otherUserID, lifetimeUserID)
	matchingEmail := "membership-integration-" + matchingUserID.String() + "@example.test"
	lifetimeEmail := "membership-integration-" + lifetimeUserID.String() + "@example.test"

	var plan membershipPlanRow
	if err := db.Where("code = 'pro' AND status = 'active'").First(&plan).Error; err != nil {
		t.Fatalf("load Pro plan: %v", err)
	}
	if err := db.Create(&membershipRow{
		UserID: lifetimeUserID, PlanID: plan.ID, StartsAt: fixedNow.AddDate(-1, 0, 0),
		ExpiresAt: nil, Source: "admin_adjustment", Status: "active", Version: 1,
		CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}).Error; err != nil {
		t.Fatalf("create lifetime membership: %v", err)
	}

	var campaignCodes []string
	var batchIDs []uuid.UUID
	t.Cleanup(func() {
		_ = db.Where("campaign_code IN ?", campaignCodes).Delete(&betaPromotionClaimRow{}).Error
		_ = db.Where("batch_id IN ?", batchIDs).Delete(&redemptionCodeRow{}).Error
		_ = db.Where("code IN ?", campaignCodes).Delete(&betaPromotionCampaignRow{}).Error
		_ = db.Where("id IN ?", batchIDs).Delete(&redemptionCodeBatchRow{}).Error
		cleanupTestMembershipData(db, matchingUserID, otherUserID, lifetimeUserID)
	})

	createBoundCode := func(email string) string {
		t.Helper()
		issued, generateErr := codec.Generate()
		if generateErr != nil {
			t.Fatalf("generate beta code: %v", generateErr)
		}
		days := 365
		batch := redemptionCodeBatchRow{
			ID: uuid.New(), Name: "promotion-redemption-" + uuid.NewString(), PlanID: plan.ID,
			GrantType: string(GrantDuration), GrantDays: &days, Quantity: 1, Status: "active",
			Note: "integration email binding test", CreatedAt: fixedNow, UpdatedAt: fixedNow,
		}
		if createErr := db.Create(&batch).Error; createErr != nil {
			t.Fatalf("create promotion batch: %v", createErr)
		}
		batchIDs = append(batchIDs, batch.ID)
		campaignCode := "beta-redemption-" + uuid.NewString()
		if createErr := db.Create(&betaPromotionCampaignRow{
			Code: campaignCode, BatchID: batch.ID, Quota: 1, ClaimedCount: 1, Status: "exhausted",
			RedemptionPolicy: betaPromotionRedemptionPolicy,
			CreatedAt:        fixedNow, UpdatedAt: fixedNow,
		}).Error; createErr != nil {
			t.Fatalf("create promotion campaign: %v", createErr)
		}
		campaignCodes = append(campaignCodes, campaignCode)
		codeID := uuid.New()
		if createErr := db.Create(&redemptionCodeRow{
			ID: codeID, BatchID: batch.ID, CodeDigest: issued.Digest, CodeHint: issued.Hint,
			Status: "active", CreatedAt: fixedNow,
		}).Error; createErr != nil {
			t.Fatalf("create promotion redemption code: %v", createErr)
		}
		claimID := uuid.New()
		if createErr := db.Create(&betaPromotionClaimRow{
			ID: claimID, CampaignCode: campaignCode, Email: email, CodeID: codeID,
			ClientIPDigest: codec.Digest(campaignCode + ":ip:192.0.2.1"), DeliveryStatus: "sent",
			DeliveryAttempts: 1, LastDeliveryAttemptAt: fixedNow, SentAt: &fixedNow,
			CreatedAt: fixedNow, UpdatedAt: fixedNow,
		}).Error; createErr != nil {
			t.Fatalf("create promotion claim: %v", createErr)
		}
		return issued.Plaintext
	}

	firstCode := createBoundCode(strings.ToUpper(matchingEmail))
	secondCode := createBoundCode(matchingEmail)
	lifetimeCode := createBoundCode(lifetimeEmail)
	if _, err := store.Redeem(ctx, otherUserID, firstCode); !errors.Is(err, ErrCodeUnavailable) {
		t.Fatalf("Redeem(beta code from another email) error = %v, want unavailable", err)
	}
	if _, err := store.Redeem(ctx, matchingUserID, firstCode); err != nil {
		t.Fatalf("Redeem(first beta code with matching email) error = %v", err)
	}
	if _, err := store.Redeem(ctx, matchingUserID, secondCode); !errors.Is(err, ErrCodeUnavailable) {
		t.Fatalf("Redeem(second beta code for same email) error = %v, want unavailable", err)
	}
	if _, err := store.Redeem(ctx, lifetimeUserID, lifetimeCode); !errors.Is(err, ErrMembershipNotExtended) {
		t.Fatalf("Redeem(beta code as lifetime member) error = %v, want membership not extended", err)
	}

	ordinaryBatchID := uuid.New()
	ordinaryDays := 30
	if err := db.Create(&redemptionCodeBatchRow{
		ID: ordinaryBatchID, Name: "ordinary-regression-" + uuid.NewString(), PlanID: plan.ID,
		GrantType: string(GrantDuration), GrantDays: &ordinaryDays, Quantity: 2, Status: "active",
		Note: "ordinary code isolation regression", CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}).Error; err != nil {
		t.Fatalf("create ordinary redemption batch: %v", err)
	}
	batchIDs = append(batchIDs, ordinaryBatchID)
	ordinaryCodes := make([]string, 0, 2)
	for range 2 {
		issued, generateErr := codec.Generate()
		if generateErr != nil {
			t.Fatalf("generate ordinary code: %v", generateErr)
		}
		if err := db.Create(&redemptionCodeRow{
			ID: uuid.New(), BatchID: ordinaryBatchID, CodeDigest: issued.Digest, CodeHint: issued.Hint,
			Status: "active", CreatedAt: fixedNow,
		}).Error; err != nil {
			t.Fatalf("create ordinary redemption code: %v", err)
		}
		ordinaryCodes = append(ordinaryCodes, issued.Plaintext)
	}
	if _, err := store.Redeem(ctx, matchingUserID, ordinaryCodes[0]); err != nil {
		t.Fatalf("Redeem(ordinary code after beta code for same email) error = %v", err)
	}
	if _, err := store.Redeem(ctx, otherUserID, ordinaryCodes[1]); err != nil {
		t.Fatalf("Redeem(ordinary code without email binding) error = %v", err)
	}
	var ordinaryBatchStatus string
	if err := db.Model(&redemptionCodeBatchRow{}).Where("id = ?", ordinaryBatchID).Pluck("status", &ordinaryBatchStatus).Error; err != nil {
		t.Fatalf("load ordinary redemption batch status: %v", err)
	}
	if ordinaryBatchStatus != "exhausted" {
		t.Fatalf("ordinary redemption batch status = %q, want exhausted", ordinaryBatchStatus)
	}

	var activeUnusedCodes int64
	if err := db.Model(&redemptionCodeRow{}).
		Where("batch_id IN ? AND status = 'active'", batchIDs).
		Count(&activeUnusedCodes).Error; err != nil {
		t.Fatalf("count unconsumed beta codes: %v", err)
	}
	if activeUnusedCodes != 2 {
		t.Fatalf("active unused beta codes = %d, want 2", activeUnusedCodes)
	}
}
