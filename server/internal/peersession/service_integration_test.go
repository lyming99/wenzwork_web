//go:build integration

package peersession

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

func TestPostgresEndpointReaderReturnsOnlyTheCurrentReadyExactRelay(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	installationID, nodeID := uuid.New(), uuid.New()
	cellID := uuid.MustParse("01700000-0000-4000-8000-000000000017")
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.Exec(`
		INSERT INTO relay_node_installations
			(id, cell_id, display_name, platform, architecture, status, activated_at)
		VALUES (?, ?, ?, 'linux', 'amd64', 'active', ?)
	`, installationID, cellID, "peer-endpoint-"+installationID.String()[:8], now).Error; err != nil {
		t.Fatalf("insert installation: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("UPDATE relay_node_installations SET current_instance_id = NULL WHERE id = ?", installationID).Error
		_ = db.Exec("DELETE FROM relay_node_instances WHERE installation_id = ?", installationID).Error
		_ = db.Exec("DELETE FROM relay_node_installations WHERE id = ?", installationID).Error
	})
	if err := db.Exec(`
		INSERT INTO relay_node_instances
			(id, installation_id, cell_id, status, version, protocol_version, addresses,
			 started_at, last_heartbeat_at, lease_expires_at)
		VALUES (?, ?, ?, 'ready', 'integration', 1, ?::jsonb, ?, ?, ?)
	`, nodeID, installationID, cellID, `["wss://relay-b.example.test/v1/connect"]`, now, now, now.Add(time.Minute)).Error; err != nil {
		t.Fatalf("insert Relay instance: %v", err)
	}
	if err := db.Exec("UPDATE relay_node_installations SET current_instance_id = ? WHERE id = ?", nodeID, installationID).Error; err != nil {
		t.Fatalf("select current Relay instance: %v", err)
	}

	reader := postgresEndpointReader{db: db}
	endpoint, err := reader.LoadRelayEndpoint(context.Background(), nodeID, cellID, now)
	if err != nil || endpoint != "wss://relay-b.example.test/v1/connect" {
		t.Fatalf("LoadRelayEndpoint() = %q, %v", endpoint, err)
	}
	if _, err := reader.LoadRelayEndpoint(context.Background(), nodeID, uuid.New(), now); !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("wrong Cell error = %v", err)
	}
	if err := db.Exec("UPDATE relay_node_instances SET addresses = ?::jsonb WHERE id = ?", `["https://relay-b.example.test/v1/connect"]`, nodeID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := reader.LoadRelayEndpoint(context.Background(), nodeID, cellID, now); !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("unsafe endpoint error = %v", err)
	}
	if err := db.Exec("UPDATE relay_node_instances SET addresses = ?::jsonb, last_heartbeat_at = ?, lease_expires_at = ? WHERE id = ?",
		`["wss://relay-b.example.test/v1/connect"]`, now.Add(-2*time.Minute), now.Add(-time.Minute), nodeID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := reader.LoadRelayEndpoint(context.Background(), nodeID, cellID, now); !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("expired Lease error = %v", err)
	}
}
