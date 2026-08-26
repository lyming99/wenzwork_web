//go:build integration

package relaymanagement

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
)

func TestEnrollmentIsAtomicAndInstanceLifecycleIsStable(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
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

	currentTime := time.Now().UTC().Truncate(time.Second)
	authority, err := relayidentity.LoadOrCreateDevelopmentCA(t.TempDir(), currentTime)
	if err != nil {
		t.Fatalf("create test Relay CA: %v", err)
	}
	store, err := NewStore(db, authority,
		WithClock(func() time.Time { return currentTime }),
		WithTokenTTL(10*time.Minute),
		WithNodeLeaseDuration(45*time.Second),
	)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	actorID := uuid.New()
	releaseID := uuid.New()
	artifactID := uuid.New()
	installationID := uuid.Nil
	version := "relay-integration-" + uuid.NewString()[:12]
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-test-hash', 'Relay Integration Admin', 'active', now())
	`, actorID, "relay-integration-"+actorID.String()+"@example.test").Error; err != nil {
		t.Fatalf("insert Relay test actor: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO relay_server_releases
			(id, version, platform, architecture, protocol_min, protocol_max, build_commit,
			 build_time, signing_key_id, manifest_sha256, manifest_signature, status, created_by)
		VALUES (?, ?, 'linux', 'amd64', 1, 1, ?, ?, 'integration-key', ?, 'integration-signature', 'published', ?)
	`, releaseID, version, strings.Repeat("a", 40), currentTime, strings.Repeat("b", 64), actorID).Error; err != nil {
		t.Fatalf("insert Relay test release: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO relay_server_release_artifacts
			(id, release_id, file_name, file_size_bytes, sha256, signature, object_key)
		VALUES (?, ?, ?, 4096, ?, 'integration-signature', ?)
	`, artifactID, releaseID, "wenzwork-relay-"+version+"-linux-amd64.tar.gz", strings.Repeat("c", 64), "relay/"+version+"/wenzwork-relay.tar.gz").Error; err != nil {
		t.Fatalf("insert Relay test artifact: %v", err)
	}
	t.Cleanup(func() {
		if installationID != uuid.Nil {
			_ = db.Exec("DELETE FROM relay_outbox WHERE aggregate_id = ? OR aggregate_id IN (SELECT id FROM relay_node_instances WHERE installation_id = ?)", installationID, installationID).Error
			_ = db.Exec("DELETE FROM relay_connection_audit WHERE installation_id = ?", installationID).Error
			_ = db.Exec("UPDATE relay_node_installations SET current_instance_id = NULL WHERE id = ?", installationID).Error
			_ = db.Exec("DELETE FROM relay_node_instances WHERE installation_id = ?", installationID).Error
			_ = db.Exec("DELETE FROM relay_node_installations WHERE id = ?", installationID).Error
			_ = db.Exec("DELETE FROM audit_logs WHERE resource_id = ?", installationID).Error
		}
		_ = db.Exec("DELETE FROM relay_server_release_artifacts WHERE release_id = ?", releaseID).Error
		_ = db.Exec("DELETE FROM relay_server_releases WHERE id = ?", releaseID).Error
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})

	var cellIDText string
	if err := db.Raw("SELECT id::text FROM relay_cells WHERE code = 'r017'").Scan(&cellIDText).Error; err != nil {
		t.Fatalf("load seeded Relay Cell: %v", err)
	}
	cellID, err := uuid.Parse(cellIDText)
	if err != nil {
		t.Fatalf("parse seeded Relay Cell ID %q: %v", cellIDText, err)
	}
	var originalCellStatus string
	if err := db.Model(&cellRow{}).Where("id = ?", cellID).Pluck("status", &originalCellStatus).Error; err != nil {
		t.Fatalf("load seeded Relay Cell status: %v", err)
	}
	if err := db.Model(&cellRow{}).Where("id = ?", cellID).Update("status", "draft").Error; err != nil {
		t.Fatalf("prepare draft Relay Cell: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Model(&cellRow{}).Where("id = ?", cellID).Update("status", originalCellStatus).Error
	})
	installation, err := store.CreateInstallation(ctx, CreateInstallationInput{
		CellID: cellID, ReleaseID: &releaseID, DisplayName: "relay-integration-node",
		Region: "华东", Group: "production", FailureDomain: "integration-a",
		ListenerPort: 8443,
		Platform:     "linux", Architecture: "amd64", ActorUserID: actorID,
	})
	if err != nil {
		t.Fatalf("CreateInstallation() error = %v", err)
	}
	installationID = installation.ID
	if installation.Region != "华东" || installation.Group != "production" {
		t.Fatalf("Relay labels = region %q group %q", installation.Region, installation.Group)
	}

	if _, err := store.CreateInstallSession(ctx, CreateInstallSessionInput{
		InstallationID: installationID, ReleaseID: releaseID, Mode: "script", ActorUserID: actorID,
	}); err != nil {
		t.Fatalf("CreateInstallSession() error = %v", err)
	}
	token, err := store.CreateEnrollmentToken(ctx, installationID, actorID)
	if err != nil {
		t.Fatalf("CreateEnrollmentToken() error = %v", err)
	}
	var storedDigest string
	if err := db.Raw("SELECT token_digest FROM relay_node_enrollment_tokens WHERE id = ?", token.ID).Scan(&storedDigest).Error; err != nil {
		t.Fatalf("load stored Enrollment Token digest: %v", err)
	}
	if storedDigest != relayidentity.TokenDigest(token.Token) || storedDigest == token.Token {
		t.Fatalf("Enrollment Token was not stored as its SHA-256 digest")
	}
	var leakedAuditCount int64
	if err := db.Raw("SELECT count(*) FROM audit_logs WHERE resource_id = ? AND (before_json::text LIKE ? OR after_json::text LIKE ?)", installationID, "%"+token.Token+"%", "%"+token.Token+"%").Scan(&leakedAuditCount).Error; err != nil {
		t.Fatalf("scan Relay audit log for token leakage: %v", err)
	}
	if leakedAuditCount != 0 {
		t.Fatalf("plaintext Enrollment Token appeared in audit data")
	}

	publicKey, privateKey, err := relayidentity.Generate()
	if err != nil {
		t.Fatalf("generate Relay identity: %v", err)
	}
	encodedPublicKey, err := relayidentity.EncodePublicKey(publicKey)
	if err != nil {
		t.Fatalf("encode Relay identity: %v", err)
	}
	nonceBytes := make([]byte, 24)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatalf("generate enrollment nonce: %v", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	proof := relayidentity.EnrollmentProof{
		InstallationID: installationID.String(), CellID: cellID.String(), PublicKey: encodedPublicKey,
		TokenDigest: relayidentity.TokenDigest(token.Token), Nonce: nonce, Timestamp: currentTime,
	}
	signature, err := relayidentity.SignEnrollment(privateKey, proof)
	if err != nil {
		t.Fatalf("sign Relay enrollment: %v", err)
	}
	request := EnrollmentRequest{
		InstallationID: installationID.String(), CellID: cellID.String(), PublicKey: encodedPublicKey,
		Nonce: nonce, Timestamp: currentTime, Signature: signature, Version: version, ProtocolVersion: 1,
		Addresses: []string{"relay-integration.example.test:8443"}, Capabilities: map[string]any{"wss": true},
	}

	wrongPublicKey, wrongPrivateKey, err := relayidentity.Generate()
	if err != nil {
		t.Fatalf("generate wrong Relay identity: %v", err)
	}
	_ = wrongPublicKey
	wrongSignature, err := relayidentity.SignEnrollment(wrongPrivateKey, proof)
	if err != nil {
		t.Fatalf("sign wrong Relay enrollment: %v", err)
	}
	wrongRequest := request
	wrongRequest.Signature = wrongSignature
	if _, err := store.Enroll(ctx, token.Token, wrongRequest); !errors.Is(err, ErrEnrollmentInvalid) {
		t.Fatalf("wrong-key enrollment error = %v, want ErrEnrollmentInvalid", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, enrollErr := store.Enroll(ctx, token.Token, request)
			results <- enrollErr
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	successCount := 0
	for enrollErr := range results {
		if enrollErr == nil {
			successCount++
			continue
		}
		if !errors.Is(enrollErr, ErrEnrollmentConsumed) && !errors.Is(enrollErr, ErrEnrollmentInvalid) {
			t.Fatalf("concurrent Enroll() error = %v", enrollErr)
		}
	}
	if successCount != 1 {
		t.Fatalf("concurrent enrollment successes = %d, want exactly 1", successCount)
	}
	if _, err := store.Enroll(ctx, token.Token, request); !errors.Is(err, ErrEnrollmentConsumed) {
		t.Fatalf("replayed Enrollment Token error = %v, want ErrEnrollmentConsumed", err)
	}

	identity := NodeCertificateIdentity{InstallationID: installationID, CellID: cellID, Thumbprint: relayidentity.Thumbprint(publicKey)}
	firstInstanceID := uuid.New()
	if _, err := store.RegisterInstance(ctx, identity, RegisterInstanceInput{
		InstanceID: firstInstanceID, Version: version, ProtocolVersion: 1,
		Addresses: []string{"relay-integration.example.test:8443"}, StartedAt: currentTime,
	}); err != nil {
		t.Fatalf("RegisterInstance(first) error = %v", err)
	}
	heartbeat, err := store.Heartbeat(ctx, identity, HeartbeatInput{InstanceID: firstInstanceID})
	if err != nil || heartbeat.RoutingReady {
		t.Fatalf("Heartbeat(before activation) = %+v, %v", heartbeat, err)
	}
	activationInput := ActivateInstallationInput{
		ExpectedThumbprint: identity.Thumbprint,
		Checklist:          DeploymentChecklist{LoadBalancer: true, DNS: true, Port: true, TLS: true},
		Confirmation:       "activate_relay_installation", ActorUserID: actorID,
	}
	if _, err := store.ActivateInstallation(ctx, installationID, activationInput); !errors.Is(err, ErrActivationBlocked) {
		t.Fatalf("ActivateInstallation(draft Cell) error = %v, want ErrActivationBlocked", err)
	}
	if err := db.Model(&cellRow{}).Where("id = ?", cellID).Update("status", "active").Error; err != nil {
		t.Fatalf("activate Relay Cell for installation: %v", err)
	}
	activated, err := store.ActivateInstallation(ctx, installationID, activationInput)
	if err != nil || activated.Status != "active" {
		t.Fatalf("ActivateInstallation() = %+v, %v", activated, err)
	}
	heartbeat, err = store.Heartbeat(ctx, identity, HeartbeatInput{InstanceID: firstInstanceID})
	if err != nil || !heartbeat.RoutingReady {
		t.Fatalf("Heartbeat(after activation) = %+v, %v", heartbeat, err)
	}
	if err := db.Model(&installationRow{}).Where("id = ?", installationID).Update("status", "draining").Error; err != nil {
		t.Fatalf("mark Relay installation draining: %v", err)
	}
	if err := db.Model(&instanceRow{}).Where("id = ?", firstInstanceID).Update("status", "draining").Error; err != nil {
		t.Fatalf("mark Relay instance draining: %v", err)
	}
	resumed, err := store.ActivateInstallation(ctx, installationID, activationInput)
	if err != nil || resumed.Status != "active" || resumed.CurrentInstance == nil || resumed.CurrentInstance.Status != "ready" {
		t.Fatalf("ActivateInstallation(resume) = %+v, %v", resumed, err)
	}
	var resumeEvents int64
	if err := db.Model(&outboxRow{}).Where("aggregate_id = ? AND event_type = ?", firstInstanceID, "relay.node.resume").Count(&resumeEvents).Error; err != nil || resumeEvents != 1 {
		t.Fatalf("relay.node.resume events = %d, %v, want 1", resumeEvents, err)
	}

	currentTime = currentTime.Add(46 * time.Second)
	if expired, err := store.ExpireLeases(ctx); err != nil || expired != 1 {
		t.Fatalf("ExpireLeases() = %d, %v, want 1", expired, err)
	}
	afterExpiry, err := store.GetInstallation(ctx, installationID)
	if err != nil || afterExpiry.CurrentInstance != nil || len(afterExpiry.Instances) != 1 || afterExpiry.Instances[0].Status != "offline" {
		t.Fatalf("installation after lease expiry = %+v, %v", afterExpiry, err)
	}
	secondInstanceID := uuid.New()
	if _, err := store.RegisterInstance(ctx, identity, RegisterInstanceInput{
		InstanceID: secondInstanceID, Version: version, ProtocolVersion: 1,
		Addresses: []string{"relay-integration.example.test:8443"}, StartedAt: currentTime,
	}); err != nil {
		t.Fatalf("RegisterInstance(second) error = %v", err)
	}
	afterRestart, err := store.GetInstallation(ctx, installationID)
	if err != nil || afterRestart.CurrentInstance == nil || afterRestart.CurrentInstance.ID != secondInstanceID || len(afterRestart.Instances) != 2 {
		t.Fatalf("installation after restart = %+v, %v", afterRestart, err)
	}
	if err := store.RevokeInstallation(ctx, installationID, actorID, "revoke_relay_installation"); err != nil {
		t.Fatalf("RevokeInstallation() error = %v", err)
	}
	revoked, err := store.GetInstallation(ctx, installationID)
	if err != nil || revoked.Status != "revoked" || revoked.CurrentInstance != nil {
		t.Fatalf("revoked installation = %+v, %v", revoked, err)
	}
	if heartbeat, err := store.Heartbeat(ctx, identity, HeartbeatInput{InstanceID: secondInstanceID}); err != nil || !heartbeat.Revoked || heartbeat.LeaseExpiresAt.After(currentTime) {
		t.Fatalf("Heartbeat(after revocation) = %+v, %v", heartbeat, err)
	}
	if _, err := store.RegisterInstance(ctx, identity, RegisterInstanceInput{
		InstanceID: uuid.New(), Version: version, ProtocolVersion: 1,
		Addresses: []string{"relay-integration.example.test:8443"}, StartedAt: currentTime,
	}); !errors.Is(err, ErrInstallationRevoked) {
		t.Fatalf("RegisterInstance(after revocation) error = %v, want ErrInstallationRevoked", err)
	}
	var instanceStatus, certificateStatus string
	if err := db.Model(&instanceRow{}).Where("id = ?", secondInstanceID).Pluck("status", &instanceStatus).Error; err != nil || instanceStatus != "forced_offline" {
		t.Fatalf("revoked instance status = %q, %v", instanceStatus, err)
	}
	if err := db.Model(&certificateRow{}).Where("installation_id = ?", installationID).Pluck("status", &certificateStatus).Error; err != nil || certificateStatus != "revoked" {
		t.Fatalf("revoked certificate status = %q, %v", certificateStatus, err)
	}
	var revokeEvents int64
	if err := db.Model(&outboxRow{}).Where("aggregate_id = ? AND event_type = ?", secondInstanceID, "relay.node.revoke").Count(&revokeEvents).Error; err != nil || revokeEvents != 1 {
		t.Fatalf("relay.node.revoke events = %d, %v, want 1", revokeEvents, err)
	}
}
