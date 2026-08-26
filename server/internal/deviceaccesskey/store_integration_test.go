//go:build integration

package deviceaccesskey

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
)

func TestCreateAndRotatePersistExactEncryptedIdempotencyResults(t *testing.T) {
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

	userID, suffix := uuid.New(), uuid.NewString()
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-only', 'Device Access Key User', 'active', now())`,
		userID, "device-access-key-"+suffix+"@example.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO memberships (user_id, plan_id, starts_at, expires_at, source, status)
		SELECT ?, id, now() - interval '1 hour', now() + interval '1 day', 'system', 'active'
		FROM membership_plans WHERE code = 'pro'`, userID).Error; err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM remote_device_access_key_request_keys WHERE user_id = ?", userID).Error
		_ = db.Exec("DELETE FROM remote_device_access_keys WHERE user_id = ?", userID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", userID).Error
	})

	encryptionKey := bytes.Repeat([]byte("i"), 32)
	policy, err := remoteaccesspolicy.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(db, WithIdempotencyEncryptionKey(encryptionKey), WithAccessPolicy(policy))
	if err != nil {
		t.Fatal(err)
	}
	create := CreateInput{
		UserID: userID, IdempotencyKey: "create-" + suffix, Label: "desktop",
		Scopes: []string{"remote.peer.ai.config", "remote.connect"},
	}
	first, err := store.Create(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewStore(db, WithIdempotencyEncryptionKey(encryptionKey), WithAccessPolicy(policy))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Create(ctx, create)
	if err != nil || !reflect.DeepEqual(replayed, first) {
		t.Fatalf("restarted Create() = %+v, %v; want %+v", replayed, err, first)
	}
	changed := create
	changed.Label = "different"
	if _, err := restarted.Create(ctx, changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed digest error = %v", err)
	}

	var ciphertext []byte
	if err := db.Table("remote_device_access_key_request_keys").Select("response_ciphertext").
		Where("user_id = ? AND operation = 'create' AND resource_id = ? AND idempotency_key = ?", userID, userID, create.IdempotencyKey).
		Row().Scan(&ciphertext); err != nil {
		t.Fatalf("load encrypted response: %v", err)
	}
	if len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(first.Key)) {
		t.Fatal("idempotency record is empty or contains the plaintext key")
	}
	var persistedDigest string
	if err := db.Table("remote_device_access_keys").Select("key_digest").Where("id = ?", first.ID).Row().Scan(&persistedDigest); err != nil {
		t.Fatal(err)
	}
	if persistedDigest == first.Key || strings.Contains(persistedDigest, first.Key) {
		t.Fatal("credential row contains the plaintext key")
	}
	wrongKeyStore, _ := NewStore(db, WithIdempotencyEncryptionKey(bytes.Repeat([]byte("w"), 32)), WithAccessPolicy(policy))
	if _, err := wrongKeyStore.Create(ctx, create); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong server encryption key error = %v", err)
	}

	rotation := RotateInput{UserID: userID, KeyID: first.ID, IdempotencyKey: "rotate-" + suffix}
	rotated, err := store.Rotate(ctx, rotation)
	if err != nil {
		t.Fatal(err)
	}
	rotatedReplay, err := restarted.Rotate(ctx, rotation)
	if err != nil || !reflect.DeepEqual(rotatedReplay, rotated) {
		t.Fatalf("restarted Rotate() = %+v, %v; want %+v", rotatedReplay, err, rotated)
	}

	concurrentInput := create
	concurrentInput.IdempotencyKey = "concurrent-create-" + suffix
	concurrentInput.Label = "concurrent"
	createResults := make(chan AccessKey, 8)
	createErrors := make(chan error, 8)
	var createWait sync.WaitGroup
	for range 8 {
		createWait.Add(1)
		go func() {
			defer createWait.Done()
			result, createErr := store.Create(ctx, concurrentInput)
			if createErr != nil {
				createErrors <- createErr
				return
			}
			createResults <- result
		}()
	}
	createWait.Wait()
	close(createResults)
	close(createErrors)
	for createErr := range createErrors {
		t.Errorf("concurrent Create() error = %v", createErr)
	}
	var concurrentCreated AccessKey
	for result := range createResults {
		if concurrentCreated.ID == uuid.Nil {
			concurrentCreated = result
		} else if !reflect.DeepEqual(result, concurrentCreated) {
			t.Errorf("concurrent create results differ: %+v and %+v", concurrentCreated, result)
		}
	}
	if concurrentCreated.ID == uuid.Nil || concurrentCreated.Key == "" {
		t.Fatal("concurrent create returned no result")
	}
	concurrentRotation := RotateInput{UserID: userID, KeyID: concurrentCreated.ID, IdempotencyKey: "concurrent-rotate-" + suffix}
	results := make(chan AccessKey, 8)
	errorsSeen := make(chan error, 8)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, rotateErr := store.Rotate(ctx, concurrentRotation)
			if rotateErr != nil {
				errorsSeen <- rotateErr
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for rotateErr := range errorsSeen {
		t.Errorf("concurrent Rotate() error = %v", rotateErr)
	}
	var canonical AccessKey
	for result := range results {
		if canonical.ID == uuid.Nil {
			canonical = result
		} else if !reflect.DeepEqual(result, canonical) {
			t.Errorf("concurrent rotation results differ: %+v and %+v", canonical, result)
		}
	}
	if canonical.ID == uuid.Nil || canonical.Key == "" {
		t.Fatal("concurrent rotation returned no result")
	}
	var replacementCount int64
	if err := db.Table("remote_device_access_keys").Where("user_id = ? AND label = ?", userID, concurrentInput.Label).Count(&replacementCount).Error; err != nil {
		t.Fatal(err)
	}
	if replacementCount != 2 {
		t.Fatalf("concurrent rotation created %d rows; want original plus one replacement", replacementCount)
	}

	deletable, err := store.Create(ctx, CreateInput{
		UserID: userID, IdempotencyKey: "delete-" + suffix, Label: "revoked then deleted",
		Scopes: []string{"remote.connect"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, deletable.ID, userID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete active key error = %v", err)
	}
	if err := store.Delete(ctx, deletable.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete another user's key error = %v", err)
	}
	if err := store.Revoke(ctx, deletable.ID, userID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, deletable.ID, userID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, deletable.ID, userID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing key error = %v", err)
	}
	var deletedCount int64
	if err := db.Table("remote_device_access_keys").Where("id = ?", deletable.ID).Count(&deletedCount).Error; err != nil {
		t.Fatal(err)
	}
	if deletedCount != 0 {
		t.Fatalf("deleted key still has %d rows", deletedCount)
	}
}
