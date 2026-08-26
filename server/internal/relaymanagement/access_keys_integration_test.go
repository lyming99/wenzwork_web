//go:build integration

package relaymanagement

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

func TestAccessKeyLifecycleAndRevokedInstallationDeletion(t *testing.T) {
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

	now := time.Now().UTC().Truncate(time.Second)
	store, err := NewStore(db, nil, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	store.SetAgentRuntimeConfiguration(AgentRuntimeConfiguration{
		ProtocolVersion: 2, ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
		RedisURL: "redis://127.0.0.1:6379/0", TicketIssuer: "wenzwork-control",
		TicketPublicKeys:          map[string]string{"connection": strings.Repeat("a", 43)},
		DeviceLinkGrantPublicKeys: map[string]string{"device-link": strings.Repeat("b", 43)},
		HandshakeConcurrency:      128,
	})
	actorID := uuid.New()
	installationID := uuid.Nil
	instanceID := uuid.New()
	if err := db.Exec(`INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-test-hash', 'Relay Access Key Admin', 'active', now())`,
		actorID, "relay-access-key-"+actorID.String()+"@example.test").Error; err != nil {
		t.Fatalf("insert Relay Access Key actor: %v", err)
	}
	t.Cleanup(func() {
		if installationID != uuid.Nil {
			_ = db.Exec("DELETE FROM relay_outbox WHERE aggregate_id = ? OR aggregate_id IN (SELECT id FROM relay_node_instances WHERE installation_id = ?)", installationID, installationID).Error
			_ = db.Exec("UPDATE relay_node_installations SET current_instance_id = NULL WHERE id = ?", installationID).Error
			_ = db.Exec("DELETE FROM relay_node_instances WHERE installation_id = ?", installationID).Error
			_ = db.Exec("DELETE FROM relay_node_access_keys WHERE installation_id = ?", installationID).Error
			_ = db.Exec("DELETE FROM relay_node_installations WHERE id = ?", installationID).Error
			_ = db.Exec("DELETE FROM audit_logs WHERE resource_id = ?", installationID).Error
		}
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})

	var cellIDText string
	if err := db.Raw("SELECT id::text FROM relay_cells WHERE code = 'r017'").Scan(&cellIDText).Error; err != nil {
		t.Fatalf("load seeded Relay Cell: %v", err)
	}
	cellID, err := uuid.Parse(cellIDText)
	if err != nil {
		t.Fatalf("parse seeded Relay Cell %q: %v", cellIDText, err)
	}
	installation, err := store.CreateInstallation(ctx, CreateInstallationInput{
		CellID: cellID, DisplayName: "relay-access-key-integration", PublicEndpoint: "wss://relay-integration.example.test/v2/connect",
		ListenerPort: 18443,
		Platform:     "linux", Architecture: "amd64", ActorUserID: actorID,
	})
	if err != nil {
		t.Fatalf("CreateInstallation() error = %v", err)
	}
	installationID = installation.ID

	firstKey, err := store.CreateAccessKey(ctx, installationID, actorID)
	if err != nil {
		t.Fatalf("CreateAccessKey(first) error = %v", err)
	}
	var stored accessKeyRow
	if err := db.First(&stored, "id = ?", firstKey.ID).Error; err != nil {
		t.Fatalf("load stored Access Key: %v", err)
	}
	wantDigest, _ := accessKeyDigest(firstKey.Key)
	if stored.KeyDigest != wantDigest || stored.KeyDigest == firstKey.Key || stored.KeyPrefix != firstKey.KeyPrefix {
		t.Fatal("Access Key plaintext was persisted or its digest/prefix is incorrect")
	}
	if binding, err := store.ResolveAccessKey(ctx, firstKey.Key); err != nil || binding.InstallationID != installationID || binding.CellID != cellID ||
		binding.Configuration.PublicEndpoint != installation.PublicEndpoint || binding.Configuration.ListenAddress != ":18443" ||
		binding.Configuration.TicketPublicKeys["connection"] == "" ||
		binding.Configuration.DeviceLinkGrantPublicKeys["device-link"] == "" || binding.Configuration.ConnectionHardLimit < 1 {
		t.Fatalf("ResolveAccessKey(first) = %+v, %v", binding, err)
	}

	secondKey, err := store.CreateAccessKey(ctx, installationID, actorID)
	if err != nil {
		t.Fatalf("CreateAccessKey(second) error = %v", err)
	}
	if _, err := store.ResolveAccessKey(ctx, firstKey.Key); !errors.Is(err, ErrAccessKeyInvalid) {
		t.Fatalf("ResolveAccessKey(rotated key) error = %v, want ErrAccessKeyInvalid", err)
	}
	if _, err := store.RegisterInstanceWithAccessKey(ctx, secondKey.Key, RegisterInstanceInput{
		InstanceID: instanceID, Version: "0.0.0", ProtocolVersion: 2, StartedAt: now,
	}); err != nil {
		t.Fatalf("RegisterInstanceWithAccessKey() error = %v", err)
	}
	heartbeat, err := store.HeartbeatWithAccessKey(ctx, secondKey.Key, HeartbeatInput{InstanceID: instanceID})
	if err != nil || !heartbeat.RoutingReady {
		t.Fatalf("HeartbeatWithAccessKey() = %+v, %v", heartbeat, err)
	}
	current, err := store.GetInstallation(ctx, installationID)
	if err != nil {
		t.Fatalf("GetInstallation(before endpoint update) error = %v", err)
	}
	const updatedEndpoint = "wss://relay-updated.example.test/v2/connect"
	updated, err := store.UpdateInstallation(ctx, installationID, UpdateInstallationInput{
		DisplayName: current.DisplayName, Region: current.Region, Group: current.Group,
		FailureDomain: current.FailureDomain, OperationsNote: current.OperationsNote,
		PublicEndpoint: updatedEndpoint, ListenerPort: current.ListenerPort, DeploymentChecklist: current.DeploymentChecklist,
		ExpectedVersion: current.Version, ActorUserID: actorID,
	})
	if err != nil || updated.Status != "active" || updated.PublicEndpoint != updatedEndpoint {
		t.Fatalf("UpdateInstallation(active endpoint) = %+v, %v", updated, err)
	}
	heartbeat, err = store.HeartbeatWithAccessKey(ctx, secondKey.Key, HeartbeatInput{
		InstanceID: instanceID, ConfigurationVersion: current.Version,
	})
	if err != nil || heartbeat.ConfigurationVersion != updated.Version ||
		heartbeat.Configuration.PublicEndpoint != updatedEndpoint || heartbeat.RestartRequired {
		t.Fatalf("HeartbeatWithAccessKey(updated configuration) = %+v, %v", heartbeat, err)
	}
	updated, err = store.GetInstallation(ctx, installationID)
	if err != nil || updated.CurrentInstance == nil || len(updated.CurrentInstance.Addresses) != 1 ||
		updated.CurrentInstance.Addresses[0] != updatedEndpoint {
		t.Fatalf("updated Relay rendezvous projection = %+v, %v", updated.CurrentInstance, err)
	}
	if err := db.Model(&installationRow{}).Where("id = ?", installationID).Update("status", "draining").Error; err != nil {
		t.Fatalf("mark Access Key installation draining: %v", err)
	}
	restartedInstanceID := uuid.New()
	if _, err := store.RegisterInstanceWithAccessKey(ctx, secondKey.Key, RegisterInstanceInput{
		InstanceID: restartedInstanceID, Version: "0.0.0", ProtocolVersion: 2, StartedAt: now,
	}); err != nil {
		t.Fatalf("RegisterInstanceWithAccessKey(during drain) error = %v", err)
	}
	draining, err := store.GetInstallation(ctx, installationID)
	if err != nil || draining.Status != "draining" || draining.CurrentInstance == nil || draining.CurrentInstance.ID != restartedInstanceID {
		t.Fatalf("draining installation after restart = %+v, %v", draining, err)
	}
	heartbeat, err = store.HeartbeatWithAccessKey(ctx, secondKey.Key, HeartbeatInput{InstanceID: restartedInstanceID})
	if err != nil || !heartbeat.Drain || heartbeat.RoutingReady {
		t.Fatalf("HeartbeatWithAccessKey(during drain) = %+v, %v", heartbeat, err)
	}

	if err := store.RevokeInstallation(ctx, installationID, actorID, "revoke_relay_installation"); err != nil {
		t.Fatalf("RevokeInstallation() error = %v", err)
	}
	if _, err := store.ResolveAccessKey(ctx, secondKey.Key); !errors.Is(err, ErrInstallationRevoked) {
		t.Fatalf("ResolveAccessKey(revoked installation) error = %v, want ErrInstallationRevoked", err)
	}
	if err := store.DeleteInstallation(ctx, installationID, actorID); err != nil {
		t.Fatalf("DeleteInstallation(revoked) error = %v", err)
	}
	if _, err := store.GetInstallation(ctx, installationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetInstallation(deleted) error = %v, want ErrNotFound", err)
	}
	var keyCount int64
	if err := db.Model(&accessKeyRow{}).Where("installation_id = ?", installationID).Count(&keyCount).Error; err != nil || keyCount != 0 {
		t.Fatalf("Access Key rows after delete = %d, %v, want 0", keyCount, err)
	}
}
