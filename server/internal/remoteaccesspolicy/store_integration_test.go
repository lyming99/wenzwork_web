//go:build integration

package remoteaccesspolicy

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"gorm.io/gorm"
)

func TestMembershipAndConfiguredDeviceLimitAreEnforced(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}

	memberID, freeID := uuid.New(), uuid.New()
	memberSessionID, freeSessionID := uuid.New(), uuid.New()
	suffix := uuid.NewString()
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-only', 'Policy Member', 'active', now()),
		       (?, ?, 'integration-only', 'Policy Free User', 'active', now())`,
		memberID, "policy-member-"+suffix+"@example.test", freeID, "policy-free-"+suffix+"@example.test").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO memberships (user_id, plan_id, starts_at, expires_at, source, status)
		SELECT ?, id, now() - interval '1 hour', now() + interval '1 day', 'system', 'active'
		FROM membership_plans WHERE code = 'pro'`, memberID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO app_sessions (id, user_id, client_id, device_id, device_name, scope, last_seen_at, idle_expires_at)
		VALUES (?, ?, 'wenzwork-desktop', ?, 'policy-member-device', 'remote.connect', now(), now() + interval '1 day'),
		       (?, ?, 'wenzwork-desktop', ?, 'policy-free-device', 'remote.connect', now(), now() + interval '1 day')`,
		memberSessionID, memberID, uuid.New(), freeSessionID, freeID, uuid.New()).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM remote_device_credentials WHERE user_id IN ?", []uuid.UUID{memberID, freeID}).Error
		_ = db.Exec("DELETE FROM app_sessions WHERE id IN ?", []uuid.UUID{memberSessionID, freeSessionID}).Error
		_ = db.Exec("DELETE FROM users WHERE id IN ?", []uuid.UUID{memberID, freeID}).Error
	})

	now := time.Now().UTC()
	if err := db.Transaction(func(tx *gorm.DB) error { return store.RequireMembershipTx(tx, freeID, now) }); !errors.Is(err, ErrMembershipRequired) {
		t.Fatalf("free membership check error = %v", err)
	}
	var freeSnapshot struct {
		PricingPlanID         uuid.UUID `gorm:"column:pricing_plan_id"`
		Version               int64     `gorm:"column:version"`
		RemoteAccessEnabled   bool      `gorm:"column:remote_access_enabled"`
		DeviceLimit           int       `gorm:"column:device_limit"`
		MonthlyTrafficLimitGB *int64    `gorm:"column:monthly_traffic_limit_gb"`
	}
	if err := db.Raw(`
		SELECT version.pricing_plan_id, version.version, version.remote_access_enabled,
		       version.device_limit, version.monthly_traffic_limit_gb
		FROM pricing_plans plan
		JOIN pricing_plan_versions version
		  ON version.pricing_plan_id = plan.id AND version.version = plan.published_version
		WHERE plan.code = 'free' AND plan.status = 'published'`).Scan(&freeSnapshot).Error; err != nil || freeSnapshot.PricingPlanID == uuid.Nil {
		t.Fatalf("load published Free access settings: %+v, %v", freeSnapshot, err)
	}
	t.Cleanup(func() {
		_ = db.Table("pricing_plan_versions").
			Where("pricing_plan_id = ? AND version = ?", freeSnapshot.PricingPlanID, freeSnapshot.Version).
			Updates(map[string]any{
				"remote_access_enabled":    freeSnapshot.RemoteAccessEnabled,
				"device_limit":             freeSnapshot.DeviceLimit,
				"monthly_traffic_limit_gb": freeSnapshot.MonthlyTrafficLimitGB,
			}).Error
	})
	if err := db.Table("pricing_plan_versions").
		Where("pricing_plan_id = ? AND version = ?", freeSnapshot.PricingPlanID, freeSnapshot.Version).
		Updates(map[string]any{"remote_access_enabled": true, "device_limit": 2, "monthly_traffic_limit_gb": int64(5)}).Error; err != nil {
		t.Fatalf("temporarily enable Free access: %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return store.RequireMembershipTx(tx, freeID, now) }); err != nil {
		t.Fatalf("temporarily opened Free access error = %v", err)
	}
	var freeAccess AccessPlan
	if err := db.Transaction(func(tx *gorm.DB) error {
		var capacityErr error
		freeAccess, capacityErr = store.RequireDeviceCapacityTx(tx, freeID, now)
		return capacityErr
	}); err != nil || freeAccess.PlanCode != "free" || freeAccess.DeviceLimit != 2 ||
		freeAccess.MonthlyTrafficLimitGB == nil || *freeAccess.MonthlyTrafficLimitGB != 5 {
		t.Fatalf("Free access plan = %+v, %v", freeAccess, err)
	}
	for index := 0; index < freeAccess.DeviceLimit; index++ {
		if err := insertPolicyTestCredential(db, freeID, freeSessionID); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, capacityErr := store.RequireDeviceCapacityTx(tx, freeID, now)
		return capacityErr
	}); !errors.Is(err, ErrDeviceLimitReached) {
		t.Fatalf("Free capacity check error = %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return store.RequireMembershipTx(tx, memberID, now) }); err != nil {
		t.Fatalf("paid membership check error = %v", err)
	}

	var memberAccess AccessPlan
	if err := db.Transaction(func(tx *gorm.DB) error {
		var capacityErr error
		memberAccess, capacityErr = store.RequireDeviceCapacityTx(tx, memberID, now)
		return capacityErr
	}); err != nil {
		t.Fatal(err)
	}
	if memberAccess.PlanCode != "pro" {
		t.Fatalf("paid access plan = %+v, want Pro", memberAccess)
	}
	if memberAccess.DeviceLimit > 1000 {
		t.Skipf("configured Pro device limit %d is too large for an integration fixture", memberAccess.DeviceLimit)
	}
	for index := 0; index < memberAccess.DeviceLimit; index++ {
		if err := insertPolicyTestCredential(db, memberID, memberSessionID); err != nil {
			t.Fatal(err)
		}
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		_, capacityErr := store.RequireDeviceCapacityTx(tx, memberID, now)
		return capacityErr
	})
	if !errors.Is(err, ErrDeviceLimitReached) {
		t.Fatalf("capacity check error = %v", err)
	}
}

func insertPolicyTestCredential(db *gorm.DB, userID, sessionID uuid.UUID) error {
	return db.Exec(`
		INSERT INTO remote_device_credentials
		    (device_id, user_id, registered_session_id, device_name, platform, agent_version,
		     protocol_min, protocol_max, capabilities, identity_public_key, public_key_thumbprint,
		     grant_version, status, key_version, scopes)
		VALUES (?, ?, ?, ?, 'linux', 'integration', 2, 2, '["relay.ping"]'::jsonb,
		        decode(?, 'hex'), ?, 1, 'active', 1, '["remote.connect"]'::jsonb)`,
		uuid.New(), userID, sessionID, "device-"+uuid.NewString(), strings.Repeat("11", 32), strings.Repeat("A", 43)).Error
}
