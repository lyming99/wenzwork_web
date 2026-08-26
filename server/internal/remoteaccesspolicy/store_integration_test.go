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

	memberID, freeID, sessionID := uuid.New(), uuid.New(), uuid.New()
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
		VALUES (?, ?, 'wenzwork-desktop', ?, 'policy-device', 'remote.connect', now(), now() + interval '1 day')`,
		sessionID, memberID, uuid.New()).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM remote_device_credentials WHERE user_id = ?", memberID).Error
		_ = db.Exec("DELETE FROM app_sessions WHERE id = ?", sessionID).Error
		_ = db.Exec("DELETE FROM users WHERE id IN ?", []uuid.UUID{memberID, freeID}).Error
	})

	now := time.Now().UTC()
	if err := db.Transaction(func(tx *gorm.DB) error { return store.RequireMembershipTx(tx, freeID, now) }); !errors.Is(err, ErrMembershipRequired) {
		t.Fatalf("free membership check error = %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return store.RequireMembershipTx(tx, memberID, now) }); err != nil {
		t.Fatalf("paid membership check error = %v", err)
	}

	settings, err := store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.DeviceLimit > 1000 {
		t.Skipf("configured device limit %d is too large for an integration fixture", settings.DeviceLimit)
	}
	for index := 0; index < settings.DeviceLimit; index++ {
		deviceID := uuid.New()
		if err := db.Exec(`
			INSERT INTO remote_device_credentials
			    (device_id, user_id, registered_session_id, device_name, platform, agent_version,
			     protocol_min, protocol_max, capabilities, identity_public_key, public_key_thumbprint,
			     grant_version, status, key_version, scopes)
			VALUES (?, ?, ?, ?, 'linux', 'integration', 2, 2, '["relay.ping"]'::jsonb,
			        decode(?, 'hex'), ?, 1, 'active', 1, '["remote.connect"]'::jsonb)`,
			deviceID, memberID, sessionID, "device-"+uuid.NewString(), strings.Repeat("11", 32), strings.Repeat("A", 43)).Error; err != nil {
			t.Fatal(err)
		}
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		_, capacityErr := store.RequireDeviceCapacityTx(tx, memberID)
		return capacityErr
	})
	if !errors.Is(err, ErrDeviceLimitReached) {
		t.Fatalf("capacity check error = %v", err)
	}
}
