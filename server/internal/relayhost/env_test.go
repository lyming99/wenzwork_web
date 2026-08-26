package relayhost

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

func TestLoadEnvironmentBuildsAccessKeyRuntimeConfig(t *testing.T) {
	for _, name := range []string{
		"RELAY_ENV_FILE", "RELAY_MANAGEMENT_URL", "RELAY_ACCESS_KEY", "RELAY_VERSION",
		"RELAY_PROTOCOL_VERSION", "RELAY_PUBLIC_ENDPOINT", "RELAY_LISTEN_ADDRESS", "RELAY_HEALTH_ADDRESS", "RELAY_REDIS_URL",
		"RELAY_TICKET_ISSUER", "RELAY_TICKET_PUBLIC_KEY_FILES",
		"RELAY_PEER_TICKET_PUBLIC_KEY_FILES", "RELAY_CONNECTION_HARD_LIMIT", "RELAY_HANDSHAKE_CONCURRENCY",
	} {
		t.Setenv(name, "")
	}
	key := "relay_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("RELAY_ACCESS_KEY", key)
	t.Setenv("RELAY_VERSION", "1.2.3")
	// Legacy local runtime settings must not override management-owned values.
	t.Setenv("RELAY_PROTOCOL_VERSION", "invalid")
	t.Setenv("RELAY_PUBLIC_ENDPOINT", "wss://legacy.example.test/v2/connect")

	environment, err := LoadEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if environment.AccessKey != key || environment.Version != "1.2.3" || environment.ManagementURL != "" {
		t.Fatalf("LoadEnvironment() = %+v", environment)
	}
	deviceLinkKey := make([]byte, 32)
	deviceLinkKey[0] = 1
	binding := relaymanagement.AccessKeyBinding{
		InstallationID: uuid.New(), CellID: uuid.New(), Status: "active",
		Configuration: relaymanagement.AgentRuntimeConfiguration{
			ProtocolVersion: 2, PublicEndpoint: "ws://203.0.113.17:9443/v2/connect",
			ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
			RedisURL: "rediss://redis.example.test:6379/0", TicketIssuer: "wenzwork-control",
			TicketPublicKeys: map[string]string{
				"connection": base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			},
			DeviceLinkGrantPublicKeys: map[string]string{
				"device-link": base64.RawURLEncoding.EncodeToString(deviceLinkKey),
			},
			ConnectionHardLimit: 2000, HandshakeConcurrency: 64,
		},
	}
	runtimeConfig := environment.RuntimeConfig(binding)
	if !runtimeConfig.AccessKeyMode || runtimeConfig.InstallationID != binding.InstallationID || runtimeConfig.CellID != binding.CellID ||
		runtimeConfig.DirectoryURL != environment.ManagementURL || runtimeConfig.PublicEndpoint != binding.Configuration.PublicEndpoint ||
		runtimeConfig.ConnectionHardLimit != 2000 || runtimeConfig.HandshakeConcurrency != 64 {
		t.Fatalf("RuntimeConfig() = %+v", runtimeConfig)
	}
	if err := runtimeConfig.Validate(); err != nil {
		t.Fatalf("RuntimeConfig().Validate() error = %v", err)
	}
	if err := runtimeConfig.ValidateDataPlane(); err != nil {
		t.Fatalf("RuntimeConfig().ValidateDataPlane() error = %v", err)
	}
	if len(runtimeConfig.TicketPublicKeyFiles) != 0 || len(runtimeConfig.DeviceLinkGrantPublicKeyFiles) != 0 {
		t.Fatal("management-owned Ticket keys unexpectedly reference local files")
	}
	persistedPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := Save(persistedPath, runtimeConfig); err == nil {
		t.Fatal("Save persisted an Access Key runtime configuration")
	}
	if _, err := os.Stat(persistedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Access Key runtime configuration created a file: %v", err)
	}

	fileBacked := runtimeConfig
	fileBacked.TicketPublicKeyFiles = map[string]string{"legacy": filepath.Join(t.TempDir(), "legacy.pem")}
	if err := fileBacked.ValidateDataPlane(); err == nil {
		t.Fatal("Access Key mode accepted a Ticket verification key file")
	}
}

func TestLoadEnvironmentRequiresOnlyAccessKey(t *testing.T) {
	for _, name := range []string{
		"RELAY_ENV_FILE", "RELAY_MANAGEMENT_URL", "RELAY_ACCESS_KEY", "RELAY_PROTOCOL_VERSION",
		"RELAY_PUBLIC_ENDPOINT", "RELAY_CONNECTION_HARD_LIMIT", "RELAY_HANDSHAKE_CONCURRENCY",
	} {
		t.Setenv(name, "")
	}
	if _, err := LoadEnvironment(); err == nil {
		t.Fatal("LoadEnvironment accepted a missing Access Key")
	}
	t.Setenv("RELAY_ACCESS_KEY", "relay_placeholder")
	if environment, err := LoadEnvironment(); err != nil || environment.AccessKey != "relay_placeholder" {
		t.Fatalf("LoadEnvironment() = %+v, %v", environment, err)
	}
}
