//go:build integration

package remotedevice

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
)

func TestRemoteDeviceRegistrationIsConcurrentAndKeySafe(t *testing.T) {
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
	policy, err := remoteaccesspolicy.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db, WithAccessKeyIdempotencyEncryptionKey(strings.Repeat("d", 32)), WithAccessPolicy(policy))
	if err != nil {
		t.Fatal(err)
	}

	userID, otherUserID := uuid.New(), uuid.New()
	deviceID, concurrentDeviceID := uuid.New(), uuid.New()
	sessionID, concurrentSessionID := uuid.New(), uuid.New()
	suffix := uuid.NewString()
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-only', 'Remote Device User', 'active', now()),
		       (?, ?, 'integration-only', 'Other Remote Device User', 'active', now())`,
		userID, "remote-device-"+suffix+"@example.test", otherUserID, "remote-device-other-"+suffix+"@example.test").Error; err != nil {
		t.Fatalf("seed remote device users: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO memberships (user_id, plan_id, starts_at, expires_at, source, status)
		SELECT ?, id, now() - interval '1 hour', now() + interval '1 day', 'system', 'active'
		FROM membership_plans WHERE code = 'pro'`, userID).Error; err != nil {
		t.Fatalf("seed remote device membership: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO app_sessions (id, user_id, client_id, device_id, device_name, scope, last_seen_at, idle_expires_at)
		VALUES (?, ?, 'wenzwork-desktop', ?, 'device-one', 'profile.read membership.read remote.connect', now(), now() + interval '1 hour'),
		       (?, ?, 'wenzwork-desktop', ?, 'device-two', 'profile.read membership.read remote.connect', now(), now() + interval '1 hour')`,
		sessionID, userID, deviceID, concurrentSessionID, userID, concurrentDeviceID).Error; err != nil {
		t.Fatalf("seed remote device sessions: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM remote_device_request_keys WHERE user_id IN ?", []uuid.UUID{userID, otherUserID}).Error
		_ = db.Exec("DELETE FROM remote_access_grants WHERE user_id IN ?", []uuid.UUID{userID, otherUserID}).Error
		_ = db.Exec("DELETE FROM remote_device_credentials WHERE user_id IN ?", []uuid.UUID{userID, otherUserID}).Error
		_ = db.Exec("DELETE FROM relay_outbox WHERE aggregate_id IN ?", []uuid.UUID{deviceID, concurrentDeviceID}).Error
		_ = db.Exec("DELETE FROM app_sessions WHERE id IN ?", []uuid.UUID{sessionID, concurrentSessionID}).Error
		_ = db.Exec("DELETE FROM users WHERE id IN ?", []uuid.UUID{userID, otherUserID}).Error
	})

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignRegistration(privateKey, sessionID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	input := RegisterInput{
		UserID: userID, SessionID: sessionID, DeviceID: deviceID, IdempotencyKey: "registration-" + suffix,
		DeviceName: "device-one", Platform: "linux", AgentVersion: "integration", ProtocolMin: 1, ProtocolMax: 1,
		Capabilities: []string{"relay.ping"}, IdentityAlgorithm: "ed25519",
		IdentityPublicKey: base64.RawURLEncoding.EncodeToString(publicKey), Proof: proof,
	}
	first, firstEvent, err := store.Register(ctx, input)
	if err != nil || !first.Created || firstEvent == uuid.Nil {
		t.Fatalf("first Register() = %+v, %s, %v", first, firstEvent, err)
	}
	var defaultGrant struct {
		Status       string `gorm:"column:status"`
		GrantVersion int64  `gorm:"column:grant_version"`
	}
	if err := db.Table("remote_access_grants").Select("status, grant_version").Where("device_id = ? AND user_id = ?", deviceID, userID).Take(&defaultGrant).Error; err != nil || defaultGrant.Status != "enabled" || defaultGrant.GrantVersion != int64(first.Credential.GrantVersion) {
		t.Fatalf("default remote access grant = %+v, %v", defaultGrant, err)
	}
	replayed, replayEvent, err := store.Register(ctx, input)
	if err != nil || replayed.Created || replayEvent != firstEvent || replayed.Credential.PublicKeyThumbprint != first.Credential.PublicKeyThumbprint {
		t.Fatalf("replayed Register() = %+v, %s, %v", replayed, replayEvent, err)
	}

	otherPublic, otherPrivate, _ := ed25519.GenerateKey(rand.Reader)
	rotationProof, _ := SignRegistration(otherPrivate, sessionID, deviceID)
	rotation := input
	rotation.IdempotencyKey = "rotation-" + suffix
	rotation.IdentityPublicKey = base64.RawURLEncoding.EncodeToString(otherPublic)
	rotation.Proof = rotationProof
	if _, _, err := store.Register(ctx, rotation); !errors.Is(err, ErrKeyRotationRequired) {
		t.Fatalf("key rotation Register() error = %v", err)
	}
	forbidden := input
	forbidden.UserID = otherUserID
	forbidden.IdempotencyKey = "forbidden-" + suffix
	if _, _, err := store.Register(ctx, forbidden); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-user Register() error = %v", err)
	}

	concurrentProof, _ := SignRegistration(privateKey, concurrentSessionID, concurrentDeviceID)
	concurrentInput := input
	concurrentInput.SessionID, concurrentInput.DeviceID = concurrentSessionID, concurrentDeviceID
	concurrentInput.IdempotencyKey = "concurrent-" + suffix
	concurrentInput.IdentityPublicKey, concurrentInput.Proof = base64.RawURLEncoding.EncodeToString(publicKey), concurrentProof
	var createdCount atomic.Int32
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 8)
	events := make(chan uuid.UUID, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			registration, eventID, registerErr := store.Register(ctx, concurrentInput)
			if registerErr != nil {
				errorsSeen <- registerErr
				return
			}
			if registration.Created {
				createdCount.Add(1)
			}
			events <- eventID
		}()
	}
	wait.Wait()
	close(errorsSeen)
	close(events)
	for registerErr := range errorsSeen {
		t.Errorf("concurrent Register() error = %v", registerErr)
	}
	if createdCount.Load() != 1 || len(events) != 8 {
		t.Fatalf("concurrent registrations created=%d successes=%d", createdCount.Load(), len(events))
	}
	var eventID uuid.UUID
	for candidate := range events {
		if eventID == uuid.Nil {
			eventID = candidate
		} else if candidate != eventID {
			t.Fatalf("concurrent idempotency returned multiple Outbox IDs: %s and %s", eventID, candidate)
		}
	}
	var credentialCount, outboxCount int64
	if err := db.Table("remote_device_credentials").Where("device_id = ?", concurrentDeviceID).Count(&credentialCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("relay_outbox").Where("aggregate_id = ? AND event_type = 'remote.device.changed'", concurrentDeviceID).Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if credentialCount != 1 || outboxCount != 1 {
		t.Fatalf("concurrent facts credentials=%d outbox=%d", credentialCount, outboxCount)
	}
	revokedAt := time.Now().UTC()
	if err := db.Table("app_sessions").Where("id = ?", sessionID).Updates(map[string]any{
		"revoked_at": revokedAt, "revoked_reason": "integration_test", "updated_at": revokedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	revokedInput := input
	revokedInput.IdempotencyKey = "revoked-" + suffix
	if _, _, err := store.Register(ctx, revokedInput); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked App Session Register() error = %v", err)
	}
}
