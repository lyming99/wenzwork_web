package relayhost

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestSaveAndLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	want := Config{
		InstallationID: uuid.New(), CellID: uuid.New(), Version: "1.2.3", ProtocolVersion: 2,
		DirectoryURL: "https://directory.example.test", ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
		IdentityPrivateKeyFile: "/var/lib/wenzwork-relay/identity/identity.key",
		CertificateFile:        "/etc/wenzwork-relay/tls/node.crt", CACertificateFile: "/etc/wenzwork-relay/tls/ca.crt",
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.InstallationID != want.InstallationID || got.CellID != want.CellID || got.DirectoryURL != want.DirectoryURL {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestConfigRejectsInsecureDirectoryURL(t *testing.T) {
	config := Config{
		InstallationID: uuid.New(), CellID: uuid.New(), Version: "1", ProtocolVersion: 2,
		DirectoryURL: "http://directory.example.test", ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
		IdentityPrivateKeyFile: "identity.key", CertificateFile: "node.crt", CACertificateFile: "ca.crt",
	}
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted an insecure Directory URL")
	}
}

func TestAccessKeyConfigRequiresPublicWebSocketEndpoint(t *testing.T) {
	config := Config{
		AccessKeyMode: true, InstallationID: uuid.New(), CellID: uuid.New(), Version: "1", ProtocolVersion: 2,
		DirectoryURL: "https://control.example.test", ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
	}
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted Access Key mode without a public endpoint")
	}
	config.PublicEndpoint = "ws://203.0.113.17:8443/v2/connect"
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() rejected a valid WS public endpoint: %v", err)
	}
	config.PublicEndpoint = "http://203.0.113.17:8443/v2/connect"
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted a non-WebSocket public endpoint")
	}
	config.PublicEndpoint = "wss://203.0.113.17:8443/v2/connect"
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() rejected a WSS endpoint terminated by a reverse proxy: %v", err)
	}
	if got := config.AdvertisedAddresses(); len(got) != 1 || got[0] != config.PublicEndpoint {
		t.Fatalf("AdvertisedAddresses() = %v", got)
	}
}

func TestDataPlaneConfigRequiresIndependentDeviceLinkGrantKeys(t *testing.T) {
	valid := Config{
		RedisURL:     "rediss://relay.example.test:6379/0",
		TicketIssuer: "wenzwork-control",
		TicketPublicKeyFiles: map[string]string{
			"connection-key": "/etc/wenzwork-relay/ticket-keys/connection.pem",
		},
		DeviceLinkGrantPublicKeyFiles: map[string]string{
			"device-link-key": "/etc/wenzwork-relay/ticket-keys/device-link.pem",
		},
	}
	if err := valid.ValidateDataPlane(); err != nil {
		t.Fatalf("ValidateDataPlane(valid) error = %v", err)
	}

	missingDeviceLink := valid
	missingDeviceLink.DeviceLinkGrantPublicKeyFiles = nil
	if err := missingDeviceLink.ValidateDataPlane(); err == nil {
		t.Fatal("ValidateDataPlane accepted missing Device Link Grant verification keys")
	}

	reusedID := valid
	reusedID.DeviceLinkGrantPublicKeyFiles = map[string]string{
		"connection-key": "/etc/wenzwork-relay/ticket-keys/device-link.pem",
	}
	if err := reusedID.ValidateDataPlane(); err == nil {
		t.Fatal("ValidateDataPlane accepted a Device Link Grant Key ID reused by a connection Ticket")
	}

	reusedFile := valid
	reusedFile.DeviceLinkGrantPublicKeyFiles = map[string]string{
		"device-link-key": "/etc/wenzwork-relay/ticket-keys/connection.pem",
	}
	if err := reusedFile.ValidateDataPlane(); err == nil {
		t.Fatal("ValidateDataPlane accepted a Device Link Grant public key file reused by a connection Ticket")
	}
}
