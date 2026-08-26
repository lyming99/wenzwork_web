package remotedevice

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeRegistrationAcceptsDeviceAgentCapabilities(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	input := RegisterInput{
		UserID: uuid.New(), SessionID: uuid.New(), DeviceID: uuid.New(), IdempotencyKey: "registration-test-123",
		DeviceName: "workstation", Platform: "windows", AgentVersion: "dev", ProtocolMin: 2, ProtocolMax: 2,
		Capabilities: []string{
			"relay.ping", "remote.project.sync", "remote.task.workspace.inspect", "remote.task.markdown.render", "remote.task.ai.summarize",
			"remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.file.send", "remote.peer.file.receive",
		},
		IdentityAlgorithm: "ed25519", IdentityPublicKey: base64.RawURLEncoding.EncodeToString(publicKey), Proof: "proof",
	}

	normalized, _, _, err := normalizeRegistration(input)
	if err != nil {
		t.Fatalf("normalize registration: %v", err)
	}
	if len(normalized.Capabilities) != len(input.Capabilities) {
		t.Fatalf("capability count = %d, want %d", len(normalized.Capabilities), len(input.Capabilities))
	}
}

func TestRefreshCredentialAgentMetadataPreservesAccountDeviceName(t *testing.T) {
	now := time.Date(2026, 8, 26, 4, 41, 0, 0, time.UTC)
	row := credentialRow{
		DeviceName:   "设计工作站",
		Platform:     "windows",
		AgentVersion: "v1",
		ProtocolMin:  2,
		ProtocolMax:  2,
		Capabilities: json.RawMessage(`["relay.ping"]`),
		Scopes:       json.RawMessage(`["remote.connect"]`),
	}
	registration := RegisterInput{
		DeviceName:   "DESKTOP-INITIAL-NAME",
		Platform:     "linux",
		AgentVersion: "v2",
		ProtocolMin:  2,
		ProtocolMax:  2,
	}
	capabilities := json.RawMessage(`["relay.ping","remote.peer.query"]`)
	scopes := json.RawMessage(`["remote.connect","remote.peer.query"]`)

	refreshCredentialAgentMetadata(&row, registration, capabilities, scopes, now)

	if row.DeviceName != "设计工作站" {
		t.Fatalf("DeviceName = %q, want account-owned custom name", row.DeviceName)
	}
	if row.Platform != "linux" || row.AgentVersion != "v2" || row.ProtocolMin != 2 || row.ProtocolMax != 2 ||
		string(row.Capabilities) != string(capabilities) || string(row.Scopes) != string(scopes) || !row.UpdatedAt.Equal(now) {
		t.Fatalf("Agent-owned metadata was not refreshed: %+v", row)
	}
}
