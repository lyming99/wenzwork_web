package remotecontrol

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestCursorCodecRoundTripRejectsTamperingAndCrossResourceUse(t *testing.T) {
	codec, err := newCursorCodec([]byte("cursor-test-key-with-enough-entropy"))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	timestamp := time.Date(2026, 8, 8, 12, 30, 0, 123, time.FixedZone("CST", 8*60*60))
	encoded, err := codec.encode("tasks:"+id.String(), timestamp, id)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.decode(encoded, "tasks:"+id.String())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != id || !decoded.Time.Equal(timestamp) {
		t.Fatalf("decoded cursor = %+v", decoded)
	}
	if _, err := codec.decode(encoded, "projects:"+id.String()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cross-resource cursor error = %v", err)
	}
	tampered := []byte(encoded)
	if tampered[3] == 'a' {
		tampered[3] = 'b'
	} else {
		tampered[3] = 'a'
	}
	if _, err := codec.decode(string(tampered), "tasks:"+id.String()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("tampered cursor error = %v", err)
	}
}

func TestValidateTaskInputKeepsSafeTypedMetadataOutOfSensitiveCloudFields(t *testing.T) {
	safe, err := validateTaskInput(json.RawMessage(`{"depth":3,"includeHidden":false,"labels":["a","b"]}`))
	if err != nil || string(safe) != `{"depth":3,"includeHidden":false,"labels":["a","b"]}` {
		t.Fatalf("safe input = %s, %v", safe, err)
	}
	for _, raw := range []string{
		`{"prompt":"summarize"}`,
		`{"nested":{"api_key":"secret"}}`,
		`{"filePath":"C:/private/file.txt"}`,
		`{"messages":[{"text":"private"}]}`,
		`{"content":"file body"}`,
		`{"environment":{"TOKEN":"private"}}`,
		`{"attachedFilePaths":["customer.txt"]}`,
		`{"stdout":"command output"}`,
		`{"toolResult":"tool output"}`,
	} {
		if _, err := validateTaskInput(json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("validateTaskInput(%s) error = %v", raw, err)
		}
	}
	if _, err := validateTaskInput(json.RawMessage(`[]`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("array input error = %v", err)
	}
}

func TestTerminalTaskStatusAllowsOnlyFinishedTasksToBeRetried(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "cancelled", "rejected", "expired", "timed_out"} {
		if !terminalTaskStatus(status) {
			t.Errorf("terminalTaskStatus(%q) = false", status)
		}
	}
	for _, status := range []string{"queued", "dispatched", "accepted", "running", "cancel_requested", ""} {
		if terminalTaskStatus(status) {
			t.Errorf("terminalTaskStatus(%q) = true", status)
		}
	}
}

func TestControllerProofBindsUserControllerKeyAndVersion(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	userID, controllerID := uuid.New(), uuid.New()
	encoded := base64.RawURLEncoding.EncodeToString(publicKey)
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, controllerProofTranscript(userID, controllerID, encoded, 1)))
	if err := verifyControllerProof(userID, controllerID, publicKey, encoded, proof, 1); err != nil {
		t.Fatalf("valid proof error = %v", err)
	}
	if err := verifyControllerProof(userID, controllerID, publicKey, encoded, proof, 2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong version error = %v", err)
	}
	if err := verifyControllerProof(uuid.New(), controllerID, publicKey, encoded, proof, 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("wrong user error = %v", err)
	}
}

func TestNormalizeScopesIsCanonicalAndClosed(t *testing.T) {
	result, err := normalizeScopes([]string{"remote.task.write", "remote.project.read"}, allowedDeviceScopes)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0] != "remote.project.read" || result[1] != "remote.task.write" {
		t.Fatalf("normalized scopes = %#v", result)
	}
	if _, err := normalizeScopes([]string{"remote.task.write", "remote.task.write"}, allowedDeviceScopes); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate scope error = %v", err)
	}
	if _, err := normalizeScopes([]string{"remote.shell"}, allowedDeviceScopes); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown scope error = %v", err)
	}
}

func TestBrowserPeerScopeAllowlistIsExact(t *testing.T) {
	want := []string{"remote.peer.ai.chat", "remote.peer.ai.config", "remote.peer.ai.tools", "remote.peer.file.receive", "remote.peer.file.send", "remote.peer.query", "remote.peer.task.control", "remote.peer.terminal", "remote.peer.terminal.interactive"}
	got, err := normalizeScopes([]string{
		"remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.ai.tools", "remote.peer.terminal", "remote.peer.terminal.interactive", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.task.control",
	}, allowedControllerScopes)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("controller scopes = %#v, want %#v", got, want)
	}
	for _, rejected := range []string{"remote.connect", "remote.project.read", "remote.task.write", "remote.peer.shell"} {
		if _, err := normalizeScopes([]string{rejected}, allowedControllerScopes); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("scope %q error = %v", rejected, err)
		}
	}
}

func TestDeviceProjectionValidationIsClosed(t *testing.T) {
	now := time.Now().UTC()
	valid := DeviceChange{
		Sequence: 1, Kind: "project", Operation: "upsert", ResourceID: uuid.New(), Revision: 1,
		OccurredAt: now, DisplayName: "Project", Capabilities: []string{"project.read"}, State: "available",
	}
	if err := validateDeviceChange(valid, now); err != nil {
		t.Fatalf("valid change error = %v", err)
	}
	valid.Metadata = json.RawMessage(`{"prompt":"leak"}`)
	if err := validateDeviceChange(valid, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("open metadata error = %v", err)
	}
	validTask := DeviceChange{
		Sequence: 2, Kind: "task", Operation: "upsert", ResourceID: uuid.New(), Revision: 1,
		OccurredAt: now, TaskType: "codex", Title: TaskProjectionDisplayName("codex"), Status: "running",
	}
	if err := validateDeviceChange(validTask, now); err != nil {
		t.Fatalf("valid task projection error = %v", err)
	}
	validTask.Title = "Review private/customer.txt"
	if err := validateDeviceChange(validTask, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("user-authored task title error = %v", err)
	}
	validTask.TaskType = "private_customer_name"
	validTask.Title = TaskProjectionDisplayName(validTask.TaskType)
	if err := validateDeviceChange(validTask, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unreviewed task type error = %v", err)
	}
	logEvent := DeviceEventInput{
		EventID: uuid.New(), TaskID: uuid.New(), DeviceSequence: 1, Type: "task.log", OccurredAt: now,
		Log: &TaskLog{Stream: "system", Sequence: 1, OccurredAt: now, Content: "safe bounded log"},
	}
	if err := validateDeviceEvent(logEvent, now); !errors.Is(err, ErrPeerRequired) {
		t.Fatalf("cloud log event error = %v", err)
	}
	statusWithLog := logEvent
	statusWithLog.Type = "task.running"
	if err := validateDeviceEvent(statusWithLog, now); !errors.Is(err, ErrPeerRequired) {
		t.Fatalf("status event with hidden log error = %v", err)
	}
}

func TestPeerEndpointRequiresExactWebSocketURL(t *testing.T) {
	for _, endpoint := range []string{
		"ws://relay.example.test/v1/connect",
		"wss://relay.example.test/v1/connect",
	} {
		valid, err := validateRelayEndpoint(endpoint)
		if err != nil || valid != endpoint {
			t.Fatalf("valid endpoint %q = %q, %v", endpoint, valid, err)
		}
	}
	for _, candidate := range []string{
		"https://relay.example.test/v1/connect",
		"wss://relay.example.test/other",
		"wss://user@relay.example.test/v1/connect",
		"wss://relay.example.test/v1/connect?ticket=secret",
	} {
		if _, err := validateRelayEndpoint(candidate); !errors.Is(err, ErrUnavailable) {
			t.Errorf("validateRelayEndpoint(%q) error = %v", candidate, err)
		}
	}
}

func TestBrowserTicketUsesOnlyEncodedOneTimeSubprotocol(t *testing.T) {
	ticket := "eyJhbGciOiJFZERTQSJ9.eyJqdGkiOiJvbmUtdGltZSJ9.signature"
	protocols := browserWebSocketSubprotocols(ticket)
	if len(protocols) != 2 || protocols[0] != "wenzwork-relay.v1" {
		t.Fatalf("protocols = %#v", protocols)
	}
	const prefix = "wenzwork-peer-ticket."
	if !strings.HasPrefix(protocols[1], prefix) || strings.Contains(protocols[1], ticket) {
		t.Fatalf("ticket protocol = %q", protocols[1])
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(protocols[1], prefix))
	if err != nil || string(decoded) != ticket {
		t.Fatalf("decoded ticket = %q, %v", decoded, err)
	}
}

func TestBrowserPeerClaimsAreSelfContainedAndVersionFenced(t *testing.T) {
	controllerPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	targetPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	input := PeerIssueInput{
		UserID: uuid.New(), ControllerID: uuid.New(), TargetDeviceID: uuid.New(), Scope: "remote.peer.ai.chat",
		ControllerPublicKey: controllerPublic, ControllerKeyThumbprint: remoteauth.PublicKeyThumbprint(controllerPublic),
		ControllerKeyVersion: 7, ControllerGrantVersion: 11, TargetPublicKey: targetPublic,
		TargetKeyThumbprint: remoteauth.PublicKeyThumbprint(targetPublic), TargetKeyVersion: 5, TargetGrantVersion: 13,
	}
	nodeID, cellID := uuid.New(), uuid.New()
	now := time.Date(2026, 8, 8, 5, 0, 0, 0, time.UTC)
	claims := newBrowserPeerClaims(input, nodeID, cellID, 17, now, time.Minute, 10*time.Minute, 8<<20)
	if claims.SourceCredentialType != "controller" || claims.SourceIdentityKey != base64.RawURLEncoding.EncodeToString(controllerPublic) ||
		claims.TargetIdentityKey != base64.RawURLEncoding.EncodeToString(targetPublic) || claims.SourceKeyVersion != 7 ||
		claims.SourceGrantVersion != 11 || claims.TargetKeyVersion != 5 || claims.TargetGrantVersion != 13 ||
		claims.TargetConnectionEpoch != 17 || claims.RelayNodeID != nodeID.String() || claims.RelayCellID != cellID.String() ||
		!claims.HasScope("remote.peer.ai.chat") || claims.ExpiresAt != now.Add(time.Minute).Unix() {
		t.Fatalf("browser Peer claims = %+v", claims)
	}
}
