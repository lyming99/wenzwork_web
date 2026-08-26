//go:build integration

package membership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"gorm.io/gorm"
)

func TestStoreRedeemAllowsOnlyOneConcurrentUse(t *testing.T) {
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

	codec, err := NewCodeCodec([]byte(strings.Repeat("integration-key-", 3)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	store, err := NewStore(db, codec)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	fixedNow := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return fixedNow }

	adminID := uuid.New()
	firstUserID := uuid.New()
	secondUserID := uuid.New()
	verifiedAt := fixedNow.Add(-time.Hour)
	insertTestUsers(t, db, verifiedAt, adminID, firstUserID, secondUserID)
	t.Cleanup(func() {
		cleanupTestMembershipData(db, adminID, firstUserID, secondUserID)
	})

	days := 30
	batch, err := store.CreateBatch(ctx, CreateBatchInput{
		Name:      "integration-concurrency-" + uuid.NewString(),
		PlanCode:  "pro",
		GrantType: GrantDuration,
		GrantDays: &days,
		Quantity:  1,
		CreatedBy: adminID,
	})
	if err != nil {
		t.Fatalf("CreateBatch() error = %v", err)
	}
	if len(batch.Plaintext) != 1 {
		t.Fatalf("plaintext code count = %d, want 1", len(batch.Plaintext))
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, userID := range []uuid.UUID{firstUserID, secondUserID} {
		wait.Add(1)
		go func(id uuid.UUID) {
			defer wait.Done()
			<-start
			_, redeemErr := store.Redeem(ctx, id, batch.Plaintext[0])
			results <- redeemErr
		}(userID)
	}
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	unavailable := 0
	for redeemErr := range results {
		switch {
		case redeemErr == nil:
			successes++
		case errors.Is(redeemErr, ErrCodeUnavailable):
			unavailable++
		default:
			t.Fatalf("Redeem() unexpected error = %v", redeemErr)
		}
	}
	if successes != 1 || unavailable != 1 {
		t.Fatalf("concurrent results: success=%d unavailable=%d, want 1 and 1", successes, unavailable)
	}

	var membershipCount int64
	if err := db.Model(&membershipRow{}).
		Where("user_id IN ?", []uuid.UUID{firstUserID, secondUserID}).
		Count(&membershipCount).Error; err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if membershipCount != 1 {
		t.Fatalf("membership count = %d, want 1", membershipCount)
	}

	var code redemptionCodeRow
	if err := db.Where("batch_id = ?", batch.ID).First(&code).Error; err != nil {
		t.Fatalf("load redemption code: %v", err)
	}
	if code.Status != "redeemed" || code.RedeemedBy == nil {
		t.Fatalf("code status = %q redeemedBy = %v", code.Status, code.RedeemedBy)
	}
	if code.CodeDigest == batch.Plaintext[0] || strings.Contains(code.CodeDigest, "WZM-") {
		t.Fatal("database contains redemption code plaintext instead of HMAC digest")
	}

	var event membershipEventRow
	if err := db.Where("source_id = ? AND event_type = 'redemption'", code.ID).First(&event).Error; err != nil {
		t.Fatalf("load membership event: %v", err)
	}
	var eventState Membership
	if err := json.Unmarshal(event.AfterJSON, &eventState); err != nil {
		t.Fatalf("decode membership event after state: %v", err)
	}
	if eventState.PlanCode != "pro" || eventState.ExpiresAt == nil {
		t.Fatalf("membership event after state = %+v, want timed pro plan", eventState)
	}
}

func TestStoreBatchLifecycleQueriesRateLimitAndAudit(t *testing.T) {
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
	codec, err := NewCodeCodec([]byte(strings.Repeat("integration-key-", 3)))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	store, err := NewStore(db, codec)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	currentNow := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return currentNow }

	adminID := uuid.New()
	userID := uuid.New()
	verifiedAt := currentNow.Add(-time.Hour)
	insertTestUsers(t, db, verifiedAt, adminID, userID)
	clientIP := "192.0.2.80"
	limitKeys := redemptionLimitKeys(userID, clientIP)
	t.Cleanup(func() {
		for _, key := range limitKeys {
			_ = db.Delete(&redemptionRateLimitRow{}, "scope = ? AND key_digest = ?", key.scope, key.digest).Error
		}
		cleanupTestMembershipData(db, adminID, userID)
	})

	days := 30
	created, err := store.CreateBatch(ctx, CreateBatchInput{
		Name: "integration-lifecycle-" + uuid.NewString(), PlanCode: "pro", GrantType: GrantDuration,
		GrantDays: &days, Quantity: 2, CreatedBy: adminID,
	})
	if err != nil || len(created.Plaintext) != 2 || created.Batch.Quantity != 2 {
		t.Fatalf("CreateBatch() = %+v, %v", created, err)
	}
	var createAudit auditLogRow
	if err := db.Where("resource_id = ? AND action = 'redemption_batch.create'", created.ID).First(&createAudit).Error; err != nil {
		t.Fatalf("load batch creation audit: %v", err)
	}
	for _, plaintext := range created.Plaintext {
		if bytes.Contains(createAudit.AfterJSON, []byte(plaintext)) {
			t.Fatal("batch audit contains a plaintext redemption code")
		}
	}

	for attempt := 0; attempt < redemptionAttemptLimit; attempt++ {
		_, err := store.RedeemFromIP(ctx, userID, "WZM-INVALID-CODE", clientIP)
		if !errors.Is(err, ErrCodeUnavailable) {
			t.Fatalf("invalid redemption attempt %d error = %v", attempt+1, err)
		}
	}
	if _, err := store.RedeemFromIP(ctx, userID, created.Plaintext[0], clientIP); !errors.Is(err, ErrRedemptionRateLimit) {
		t.Fatalf("rate-limited redemption error = %v", err)
	}
	var rateRows []redemptionRateLimitRow
	if err := db.Where("key_digest IN ?", []string{limitKeys[0].digest, limitKeys[1].digest}).Find(&rateRows).Error; err != nil {
		t.Fatalf("load redemption rate rows: %v", err)
	}
	if len(rateRows) != 2 {
		t.Fatalf("redemption rate row count = %d, want 2", len(rateRows))
	}
	for _, row := range rateRows {
		if strings.Contains(row.KeyDigest, userID.String()) || strings.Contains(row.KeyDigest, clientIP) || len(row.KeyDigest) != 64 {
			t.Fatalf("redemption rate key leaks identifier: %q", row.KeyDigest)
		}
	}

	currentNow = currentNow.Add(redemptionBlock + time.Minute)
	result, err := store.RedeemFromIP(ctx, userID, created.Plaintext[0], clientIP)
	if err != nil || result.Membership.PlanCode != "pro" {
		t.Fatalf("RedeemFromIP(after block) = %+v, %v", result, err)
	}
	status, err := store.GetMembership(ctx, userID)
	if err != nil || status.PlanCode != "pro" || status.PlanName != "Pro" || status.ExpiresAt == nil {
		t.Fatalf("GetMembership() = %+v, %v", status, err)
	}
	records, err := store.ListRedemptions(ctx, userID, 50)
	if err != nil || len(records) != 1 || records[0].CodeHint != result.CodeHint {
		t.Fatalf("ListRedemptions() = %+v, %v", records, err)
	}
	batches, err := store.ListBatches(ctx, 100)
	if err != nil {
		t.Fatalf("ListBatches() error = %v", err)
	}
	var found *BatchSummary
	for index := range batches {
		if batches[index].ID == created.ID {
			found = &batches[index]
			break
		}
	}
	if found == nil || found.RedeemedCount != 1 || found.Status != "active" {
		t.Fatalf("batch summary = %+v", found)
	}
	codes, err := store.ListRedemptionCodes(ctx, RedemptionCodeFilter{BatchID: created.ID, Limit: 100})
	if err != nil || codes.Total != 2 || len(codes.Items) != 2 {
		t.Fatalf("ListRedemptionCodes() = %+v, %v", codes, err)
	}
	var activeCodeID uuid.UUID
	for _, code := range codes.Items {
		if code.Status == "active" {
			activeCodeID = code.ID
		}
	}
	if activeCodeID == uuid.Nil {
		t.Fatalf("code status list has no active code: %+v", codes.Items)
	}
	if err := store.RevokeRedemptionCode(ctx, activeCodeID, adminID); err != nil {
		t.Fatalf("RevokeRedemptionCode() error = %v", err)
	}
	codes, err = store.ListRedemptionCodes(ctx, RedemptionCodeFilter{BatchID: created.ID, Limit: 100})
	if err != nil || codes.Items[0].Status == "active" || codes.Items[1].Status == "active" {
		t.Fatalf("code statuses after revocation = %+v, %v", codes.Items, err)
	}
	batches, err = store.ListBatches(ctx, 100)
	if err != nil {
		t.Fatalf("ListBatches(after code revocation) error = %v", err)
	}
	found = nil
	for index := range batches {
		if batches[index].ID == created.ID {
			found = &batches[index]
			break
		}
	}
	if found == nil || found.ActiveCount != 0 || found.RedeemedCount != 1 || found.RevokedCount != 1 || found.Status != "exhausted" {
		t.Fatalf("batch summary after code revocation = %+v", found)
	}

	if err := store.RevokeBatch(ctx, created.ID, adminID); err != nil {
		t.Fatalf("RevokeBatch() error = %v", err)
	}
	if _, err := store.Redeem(ctx, userID, created.Plaintext[1]); !errors.Is(err, ErrCodeUnavailable) {
		t.Fatalf("Redeem(revoked code) error = %v", err)
	}
	var revokeAudit auditLogRow
	if err := db.Where("resource_id = ? AND action = 'redemption_batch.revoke'", created.ID).First(&revokeAudit).Error; err != nil {
		t.Fatalf("load batch revocation audit: %v", err)
	}
	for _, plaintext := range created.Plaintext {
		if bytes.Contains(revokeAudit.AfterJSON, []byte(plaintext)) || bytes.Contains(revokeAudit.BeforeJSON, []byte(plaintext)) {
			t.Fatal("batch revocation audit contains a plaintext redemption code")
		}
	}
}

func TestStoreAdminSetsAndCancelsMembership(t *testing.T) {
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
	codec, err := NewCodeCodec([]byte("integration-test-redemption-hmac-key-with-32-bytes"))
	if err != nil {
		t.Fatalf("NewCodeCodec() error = %v", err)
	}
	store, err := NewStore(db, codec)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	adminID := uuid.New()
	userID := uuid.New()
	insertTestUsers(t, db, now.Add(-time.Hour), adminID, userID)
	t.Cleanup(func() { cleanupTestMembershipData(db, adminID, userID) })

	expiresAt := now.AddDate(0, 0, 90)
	status, err := store.SetUserMembership(ctx, userID, SetMembershipInput{
		PlanCode: "pro", ExpiresAt: &expiresAt, Reason: "integration adjustment", ActorUserID: adminID,
	})
	if err != nil || status.PlanCode != "pro" || status.ExpiresAt == nil || !status.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("SetUserMembership() = %+v, %v", status, err)
	}
	loaded, err := store.GetMembership(ctx, userID)
	if err != nil || loaded.PlanCode != "pro" {
		t.Fatalf("GetMembership(after set) = %+v, %v", loaded, err)
	}
	if err := store.CancelUserMembership(ctx, userID, adminID, "integration cancellation"); err != nil {
		t.Fatalf("CancelUserMembership() error = %v", err)
	}
	loaded, err = store.GetMembership(ctx, userID)
	if err != nil || loaded.PlanCode != "free" {
		t.Fatalf("GetMembership(after cancel) = %+v, %v", loaded, err)
	}
	var eventCount int64
	if err := db.Model(&membershipEventRow{}).Where("user_id = ? AND event_type IN ?", userID, []string{"admin_adjustment", "revocation"}).Count(&eventCount).Error; err != nil || eventCount != 2 {
		t.Fatalf("membership event count = %d, %v", eventCount, err)
	}
}

func insertTestUsers(t *testing.T, db *gorm.DB, verifiedAt time.Time, ids ...uuid.UUID) {
	t.Helper()
	for index, id := range ids {
		email := "membership-integration-" + id.String() + "@example.test"
		if err := db.Exec(`
			INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
			VALUES (?, ?, ?, ?, 'active', ?)
		`, id, email, "test-only-password-hash", "Integration User", verifiedAt).Error; err != nil {
			t.Fatalf("insert test user %d: %v", index, err)
		}
	}
}

func cleanupTestMembershipData(db *gorm.DB, ids ...uuid.UUID) {
	_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id IN ?", ids).Error
	_ = db.Exec("DELETE FROM membership_events WHERE user_id IN ?", ids).Error
	_ = db.Exec("DELETE FROM redemption_codes WHERE batch_id IN (SELECT id FROM redemption_code_batches WHERE created_by = ?)", ids[0]).Error
	_ = db.Exec("DELETE FROM redemption_code_batches WHERE created_by = ?", ids[0]).Error
	_ = db.Exec("DELETE FROM memberships WHERE user_id IN ?", ids).Error
	_ = db.Exec("DELETE FROM user_roles WHERE user_id IN ?", ids).Error
	_ = db.Exec("DELETE FROM users WHERE id IN ?", ids).Error
}
