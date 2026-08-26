package relaydirectory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

func TestAccessKeyClientLifecycle(t *testing.T) {
	key := testRelayAccessKey()
	installationID, cellID, instanceID := uuid.New(), uuid.New(), uuid.New()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.Header.Get("Authorization") != "RelayKey "+key {
			t.Error("management request did not use RelayKey authorization")
		}
		if request.Header.Get("Accept") != "application/json" || request.URL.RawQuery != "" {
			t.Errorf("unexpected management request headers or query: %v %q", request.Header, request.URL.RawQuery)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/relay/agent/configuration":
			_ = json.NewEncoder(writer).Encode(relaymanagement.AccessKeyBinding{
				InstallationID: installationID, CellID: cellID, Status: "active",
				Configuration: relaymanagement.AgentRuntimeConfiguration{
					ProtocolVersion: 2, PublicEndpoint: "wss://relay.example.test/v2/connect",
					ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
					RedisURL: "redis://redis.example.test:6379/0", TicketIssuer: "wenzwork-control",
					TicketPublicKeys:          map[string]string{"connection": strings.Repeat("a", 43)},
					DeviceLinkGrantPublicKeys: map[string]string{"device-link": strings.Repeat("b", 43)},
					ConnectionHardLimit:       10_000, HandshakeConcurrency: 128,
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/relay/agent/instances":
			var input relaymanagement.RegisterInstanceInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.InstanceID != instanceID {
				t.Errorf("register request = %+v, %v", input, err)
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(relaymanagement.NodeInstance{ID: instanceID, InstallationID: installationID, CellID: cellID, Status: "starting"})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/relay/agent/instances/"+instanceID.String()+"/heartbeats":
			var input relaymanagement.HeartbeatInput
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.InstanceID != instanceID || input.ActiveConnections != 3 {
				t.Errorf("heartbeat request = %+v, %v", input, err)
			}
			_ = json.NewEncoder(writer).Encode(relaymanagement.HeartbeatResult{LeaseExpiresAt: time.Now().Add(time.Minute), RoutingReady: true})
		case request.Method == http.MethodDelete && request.URL.Path == "/api/v1/relay/agent/instances/"+instanceID.String():
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client, err := NewAccessKeyClient(server.URL, key)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	binding, err := client.Resolve(context.Background())
	if err != nil || binding.InstallationID != installationID || binding.CellID != cellID ||
		binding.Configuration.TicketPublicKeys["connection"] == "" || binding.Configuration.DeviceLinkGrantPublicKeys["device-link"] == "" {
		t.Fatalf("Resolve() = %+v, %v", binding, err)
	}
	instance, err := client.Register(context.Background(), relaymanagement.RegisterInstanceInput{InstanceID: instanceID, Version: "1.0.0", ProtocolVersion: 2})
	if err != nil || instance.ID != instanceID {
		t.Fatalf("Register() = %+v, %v", instance, err)
	}
	heartbeat, err := client.Heartbeat(context.Background(), relaymanagement.HeartbeatInput{InstanceID: instanceID, ActiveConnections: 3})
	if err != nil || !heartbeat.RoutingReady {
		t.Fatalf("Heartbeat() = %+v, %v", heartbeat, err)
	}
	if err := client.Unregister(context.Background(), instanceID); err != nil {
		t.Fatalf("Unregister() error = %v", err)
	}
	if calls != 4 {
		t.Fatalf("management request count = %d, want 4", calls)
	}
}

func TestAccessKeyClientMapsManagementErrors(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{status: http.StatusBadRequest, want: relaymanagement.ErrInvalidInput},
		{status: http.StatusUnauthorized, want: relaymanagement.ErrAccessKeyInvalid},
		{status: http.StatusForbidden, want: relaymanagement.ErrInstallationRevoked},
		{status: http.StatusNotFound, want: relaymanagement.ErrNotFound},
		{status: http.StatusConflict, want: relaymanagement.ErrConflict},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
			}))
			defer server.Close()
			client, err := NewAccessKeyClient(server.URL, testRelayAccessKey())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if _, err := client.Resolve(context.Background()); !errors.Is(err, test.want) {
				t.Fatalf("Resolve() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestNewAccessKeyClientValidatesOriginAndCredential(t *testing.T) {
	key := testRelayAccessKey()
	for _, valid := range []string{"https://control.example.test", "http://control.example.test:8080", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := NewAccessKeyClient(valid, key); err != nil {
			t.Fatalf("NewAccessKeyClient(%q) error = %v", valid, err)
		}
	}
	for _, invalid := range []string{
		"https://user@control.example.test", "https://control.example.test/path",
		"https://control.example.test?key=value", "ftp://control.example.test",
	} {
		if _, err := NewAccessKeyClient(invalid, key); err == nil {
			t.Fatalf("NewAccessKeyClient accepted URL %q", invalid)
		}
	}
	for _, invalid := range []string{"relay_short", "relay_" + strings.Repeat("!", 43), strings.Repeat("a", 49)} {
		if _, err := NewAccessKeyClient("https://control.example.test", invalid); err == nil {
			t.Fatal("NewAccessKeyClient accepted an invalid Access Key")
		}
	}
}

func TestAccessKeyClientDoesNotFollowRedirects(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled = true
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, err := NewAccessKeyClient(redirect.URL, testRelayAccessKey())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Resolve(context.Background()); err == nil {
		t.Fatal("Resolve followed or accepted a redirect")
	}
	if targetCalled {
		t.Fatal("Access Key request was forwarded to a redirect target")
	}
}

func testRelayAccessKey() string {
	return "relay_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
}
