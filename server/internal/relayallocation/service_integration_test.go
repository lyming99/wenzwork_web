//go:build integration

package relayallocation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"github.com/wenzwork/wenzwork-web/server/internal/remotedevice"
)

func TestMVPAllocationCommitsCurrentAssignmentAndSelfContainedTicket(t *testing.T) {
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
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	userID, sessionID, deviceID := uuid.New(), uuid.New(), uuid.New()
	regionID, poolID, cellID := uuid.New(), uuid.New(), uuid.New()
	installationID, instanceID := uuid.New(), uuid.New()
	regionCode, poolCode, cellCode := "it-region-"+suffix, "it-pool-"+suffix, "it-cell-"+suffix
	endpointURL := "ws://203.0.113.17:3091/v2/connect"

	if err := db.Exec(`INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-only', 'Allocation User', 'active', now())`, userID, "relay-allocation-"+suffix+"@example.test").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO memberships (user_id, plan_id, starts_at, expires_at, source, status)
		SELECT ?, id, now() - interval '1 hour', now() + interval '1 day', 'system', 'active'
		FROM membership_plans WHERE code = 'pro'`, userID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO app_sessions (id, user_id, client_id, device_id, device_name, scope, last_seen_at, idle_expires_at)
		VALUES (?, ?, 'wenzwork-desktop', ?, 'allocation-device', 'profile.read membership.read remote.connect', ?, ?)`,
		sessionID, userID, deviceID, now, now.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO relay_regions (id, code, name, data_residency, status) VALUES (?, ?, 'Integration Region', 'TEST', 'active')`, regionID, regionCode).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO relay_pools (id, region_id, code, name, status) VALUES (?, ?, ?, 'Integration Pool', 'active')`, poolID, regionID, poolCode).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO relay_cells (id, pool_id, code, name, failure_domain, status, protocol_min, protocol_max)
		VALUES (?, ?, ?, 'Integration Cell', 'integration-a', 'draft', 2, 2)`, cellID, poolID, cellCode).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO relay_node_installations
		(id, cell_id, display_name, public_endpoint, platform, architecture, status, created_at, updated_at)
		VALUES (?, ?, 'Integration Relay', ?, 'linux', 'amd64', 'active', ?, ?)`, installationID, cellID, endpointURL, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO relay_node_instances
		(id, installation_id, cell_id, status, version, protocol_version, addresses, capabilities,
		 active_connections, active_file_transfers, memory_bytes, ingress_mbps, egress_mbps, write_loop_lag_ms,
		 started_at, last_heartbeat_at, lease_expires_at)
		VALUES (?, ?, ?, 'ready', 'integration', 2, '[]', '{}', 0, 0, 1024, 0, 0, 0, ?, ?, ?)`,
		instanceID, installationID, cellID, now, now, now.Add(10*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("relay_node_installations").Where("id = ?", installationID).Update("current_instance_id", instanceID).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM remote_device_request_keys WHERE user_id = ?", userID).Error
		_ = db.Exec("DELETE FROM relay_outbox WHERE aggregate_id = ? OR payload->>'userId' = ?", deviceID, userID.String()).Error
		_ = db.Exec("DELETE FROM relay_assignments WHERE user_id = ?", userID).Error
		_ = db.Exec("DELETE FROM remote_device_credentials WHERE device_id = ?", deviceID).Error
		_ = db.Exec("DELETE FROM app_sessions WHERE id = ?", sessionID).Error
		_ = db.Exec("DELETE FROM relay_node_instances WHERE id = ?", instanceID).Error
		_ = db.Exec("DELETE FROM relay_node_installations WHERE id = ?", installationID).Error
		_ = db.Exec("DELETE FROM relay_cells WHERE id = ?", cellID).Error
		_ = db.Exec("DELETE FROM relay_pools WHERE id = ?", poolID).Error
		_ = db.Exec("DELETE FROM relay_regions WHERE id = ?", regionID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", userID).Error
	})

	policy, _ := remoteaccesspolicy.NewStore(db)
	deviceStore, _ := remotedevice.NewStore(db,
		remotedevice.WithAccessKeyIdempotencyEncryptionKey(strings.Repeat("d", 32)),
		remotedevice.WithAccessPolicy(policy),
	)
	deviceService, _ := remotedevice.NewService(deviceStore)
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	registrationProof, _ := remotedevice.SignRegistration(privateKey, sessionID, deviceID)
	registration, err := deviceService.Register(ctx, remotedevice.RegisterInput{
		UserID: userID, SessionID: sessionID, DeviceID: deviceID, IdempotencyKey: "registration-" + suffix,
		DeviceName: "allocation-device", Platform: "linux", AgentVersion: "integration", ProtocolMin: 2, ProtocolMax: 2,
		Capabilities: []string{"relay.ping"}, IdentityAlgorithm: "ed25519",
		IdentityPublicKey: base64.RawURLEncoding.EncodeToString(publicKey), Proof: registrationProof,
	})
	if err != nil || !registration.Created {
		t.Fatalf("Register() = %+v, %v", registration, err)
	}

	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	grantPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	service, err := NewService(ServiceConfig{
		Database:                  db,
		AccessPolicy:              policy,
		Issuer:                    remoteauth.Issuer{Issuer: "wenzwork-control", KeyID: "integration-key", PrivateKey: signerPrivate},
		DeviceLinkGrantIssuer:     "wenzwork-control",
		DeviceLinkGrantPublicKeys: map[string]ed25519.PublicKey{"device-link-integration-key": grantPublic},
		Region:                    regionCode, Pool: poolCode, Cell: cellCode, TicketTTL: 5 * time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	input := CreateInput{
		UserID: userID, SessionID: sessionID, DeviceID: deviceID, RemoteDeviceID: deviceID,
		IdempotencyKey: "allocation-" + suffix, ProtocolMin: 2, ProtocolMax: 2, ConnectionEpoch: 1,
	}
	if _, err := service.Create(ctx, input); !errors.Is(err, ErrAllocationUnavailable) {
		t.Fatalf("draft Cell Create() error = %v, want %v", err, ErrAllocationUnavailable)
	}
	if err := db.Table("relay_cells").Where("id = ?", cellID).Update("status", "active").Error; err != nil {
		t.Fatal(err)
	}
	result, err := service.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := (remoteauth.Verifier{Issuer: "wenzwork-control", Keys: map[string]ed25519.PublicKey{"integration-key": signerPublic}}).Verify(result.ConnectionTicket, "relay", now)
	if err != nil || result.AssignmentVersion != 1 || result.Primary.CellID != cellID || result.Primary.URL != endpointURL ||
		len(result.Fallbacks) != 0 || claims.Subject != deviceID.String() || !claims.HasScope("remote.connect") ||
		result.DeviceLinkGrantTrust.Issuer != "wenzwork-control" || len(result.DeviceLinkGrantTrust.Keys) != 1 ||
		result.DeviceLinkGrantTrust.Keys[0].KeyID != "device-link-integration-key" || result.DeviceLinkGrantTrust.Keys[0].Algorithm != "Ed25519" ||
		result.DeviceLinkGrantTrust.Keys[0].PublicKey != base64.RawURLEncoding.EncodeToString(grantPublic) {
		t.Fatalf("allocation/ticket = %+v, claims=%+v, error=%v", result, claims, err)
	}
	if err := claims.ValidateConnection(
		deviceID.String(), userID.String(), cellID.String(), registration.Credential.PublicKeyThumbprint,
		result.AssignmentVersion, registration.Credential.GrantVersion, 2,
	); err != nil {
		t.Fatalf("Relay admission rejected API Ticket: %v", err)
	}
	var currentCount, publishedCount int64
	if err := db.Table("relay_assignments").Where("id = ? AND status = 'current' AND effective_at IS NOT NULL", result.AssignmentID).Count(&currentCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("relay_outbox").Where("aggregate_id = ? AND published_at IS NOT NULL", result.AssignmentID).Count(&publishedCount).Error; err != nil {
		t.Fatal(err)
	}
	if currentCount != 1 || publishedCount != 1 {
		t.Fatalf("direct assignment current=%d audit_event_published=%d", currentCount, publishedCount)
	}

	replayed, err := service.Create(ctx, input)
	if err != nil || replayed.AssignmentID != result.AssignmentID || replayed.AssignmentVersion != result.AssignmentVersion {
		t.Fatalf("idempotent allocation = %+v, %v", replayed, err)
	}
	stale := input
	stale.IdempotencyKey = "stale-" + suffix
	if _, err := service.Create(ctx, stale); !errors.Is(err, ErrStaleConnectionEpoch) {
		t.Fatalf("stale epoch Create() error = %v", err)
	}
	refreshed, err := service.Refresh(ctx, RefreshInput{
		UserID: userID, SessionID: sessionID, DeviceID: deviceID, AssignmentID: result.AssignmentID,
		IdempotencyKey: "refresh-" + suffix, Reason: "scheduled", LastEndpointRevision: 1,
	})
	if err != nil || refreshed.AssignmentID != result.AssignmentID || refreshed.AssignmentVersion != result.AssignmentVersion || !refreshed.AssignmentLeaseExpiresAt.After(result.AssignmentLeaseExpiresAt.Add(-time.Second)) {
		t.Fatalf("Refresh() = %+v, %v", refreshed, err)
	}

	revokedAt := now.Add(time.Minute)
	if err := db.Table("app_sessions").Where("id = ?", sessionID).Updates(map[string]any{
		"revoked_at": revokedAt, "revoked_reason": "integration_test", "updated_at": revokedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(ctx, RefreshInput{
		UserID: userID, SessionID: sessionID, DeviceID: deviceID, AssignmentID: result.AssignmentID,
		IdempotencyKey: "revoked-" + suffix, Reason: "scheduled",
	}); !errors.Is(err, ErrDeviceForbidden) {
		t.Fatalf("revoked App Session Refresh() error = %v", err)
	}
}
