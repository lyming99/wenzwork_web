package relayruntime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayhost"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
)

type directoryClientStub struct {
	register  func(context.Context, relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error)
	heartbeat func(context.Context, relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error)
}

func (stub *directoryClientStub) Register(ctx context.Context, input relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
	if stub.register != nil {
		return stub.register(ctx, input)
	}
	return relaymanagement.NodeInstance{}, nil
}

func (stub *directoryClientStub) Heartbeat(ctx context.Context, input relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
	if stub.heartbeat != nil {
		return stub.heartbeat(ctx, input)
	}
	return relaymanagement.HeartbeatResult{}, nil
}

func (*directoryClientStub) Unregister(context.Context, uuid.UUID) error { return nil }

type readyDataPlaneDependency struct{}

func (readyDataPlaneDependency) Start(context.Context) error { return nil }
func (readyDataPlaneDependency) Ready(context.Context) error { return nil }
func (readyDataPlaneDependency) Close()                      {}
func (readyDataPlaneDependency) Ping(context.Context) error  { return nil }

func TestHeartbeatControlsReadinessAndRevocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &directoryClientStub{}
	client.heartbeat = func(context.Context, relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
		cancel()
		return relaymanagement.HeartbeatResult{RoutingReady: true, LeaseExpiresAt: time.Now().Add(time.Minute)}, nil
	}
	dependency := readyDataPlaneDependency{}
	hub := relayserver.NewV2Hub()
	relay, err := New(validRuntimeConfig(), client, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDataPlaneV2(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), hub, dependency))
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.heartbeatLoop(ctx); err != nil || !relay.routingReady.Load() {
		t.Fatalf("heartbeatLoop() error=%v routingReady=%t", err, relay.routingReady.Load())
	}

	revokedClient := &directoryClientStub{heartbeat: func(context.Context, relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
		return relaymanagement.HeartbeatResult{Revoked: true}, nil
	}}
	revokedRelay, err := New(validRuntimeConfig(), revokedClient, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := revokedRelay.heartbeatLoop(context.Background()); !errors.Is(err, ErrRevoked) || revokedRelay.routingReady.Load() {
		t.Fatalf("revoked heartbeat error=%v routingReady=%t", err, revokedRelay.routingReady.Load())
	}
}

func TestRevokedRegistrationStopsRetryLoop(t *testing.T) {
	client := &directoryClientStub{register: func(context.Context, relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
		return relaymanagement.NodeInstance{}, relaymanagement.ErrInstallationRevoked
	}}
	relay, err := New(validRuntimeConfig(), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.directoryLoop(context.Background()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("directoryLoop() error = %v, want ErrRevoked", err)
	}
}

func TestInvalidAccessKeyStopsRetryLoop(t *testing.T) {
	client := &directoryClientStub{register: func(context.Context, relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
		return relaymanagement.NodeInstance{}, relaymanagement.ErrAccessKeyInvalid
	}}
	relay, err := New(validRuntimeConfig(), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.directoryLoop(context.Background()); !errors.Is(err, relaymanagement.ErrAccessKeyInvalid) {
		t.Fatalf("directoryLoop() error = %v, want ErrAccessKeyInvalid", err)
	}
}

func TestRuntimeReportsConfiguredPublicEndpoint(t *testing.T) {
	const publicEndpoint = "wss://203.0.113.17:8443/v2/connect"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var registeredAddresses, heartbeatAddresses []string
	client := &directoryClientStub{
		register: func(_ context.Context, input relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
			registeredAddresses = append([]string(nil), input.Addresses...)
			return relaymanagement.NodeInstance{}, nil
		},
		heartbeat: func(_ context.Context, input relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
			heartbeatAddresses = append([]string(nil), input.Addresses...)
			cancel()
			return relaymanagement.HeartbeatResult{LeaseExpiresAt: time.Now().Add(time.Minute)}, nil
		},
	}
	config := validRuntimeConfig()
	config.PublicEndpoint = publicEndpoint
	relay, err := New(config, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.directoryLoop(ctx); err != nil {
		t.Fatalf("directoryLoop() error = %v", err)
	}
	for name, addresses := range map[string][]string{"registration": registeredAddresses, "heartbeat": heartbeatAddresses} {
		if len(addresses) != 1 || addresses[0] != publicEndpoint {
			t.Fatalf("%s addresses = %v, want %q", name, addresses, publicEndpoint)
		}
	}
}

func TestRuntimeHotAppliesPublicEndpointAndFlagsRestartOnlyChanges(t *testing.T) {
	config := validRuntimeConfig()
	config.AccessKeyMode = true
	config.ConfigurationVersion = 7
	config.PublicEndpoint = "ws://relay-old.example.test/v2/connect"
	config.RedisURL = "redis://127.0.0.1:6379/0"
	config.TicketIssuer = "wenzwork-control"
	config.TicketPublicKeys = map[string]string{"connection": base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
	deviceLinkKey := make([]byte, 32)
	deviceLinkKey[0] = 1
	config.DeviceLinkGrantPublicKeys = map[string]string{"device-link": base64.RawURLEncoding.EncodeToString(deviceLinkKey)}
	config.ConnectionHardLimit = 1000
	config.HandshakeConcurrency = 64

	relay, err := New(config, &directoryClientStub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	update := relaymanagement.AgentRuntimeConfiguration{
		ProtocolVersion: config.ProtocolVersion, PublicEndpoint: "ws://relay-new.example.test/v2/connect",
		ListenAddress: config.ListenAddress, HealthAddress: config.HealthAddress,
		RedisURL: config.RedisURL, TicketIssuer: config.TicketIssuer,
		TicketPublicKeys: config.TicketPublicKeys, DeviceLinkGrantPublicKeys: config.DeviceLinkGrantPublicKeys,
		ConnectionHardLimit: config.ConnectionHardLimit, HandshakeConcurrency: config.HandshakeConcurrency,
	}
	if err := relay.applyConfiguration(8, update, false); err != nil {
		t.Fatalf("applyConfiguration(public endpoint) error = %v", err)
	}
	if got := relay.advertisedAddresses(); len(got) != 1 || got[0] != update.PublicEndpoint {
		t.Fatalf("advertisedAddresses() = %v", got)
	}
	if relay.restartRequired.Load() {
		t.Fatal("public endpoint-only update unexpectedly requires restart")
	}

	update.ListenAddress = ":9443"
	update.PublicEndpoint = "ws://relay-next.example.test/v2/connect"
	if err := relay.applyConfiguration(9, update, false); err != nil {
		t.Fatalf("applyConfiguration(listener) error = %v", err)
	}
	if !relay.restartRequired.Load() || relay.currentPublicEndpoint() != update.PublicEndpoint {
		t.Fatalf("listener update restart=%v endpoint=%q", relay.restartRequired.Load(), relay.currentPublicEndpoint())
	}
	select {
	case <-relay.listenerRestart:
	default:
		t.Fatal("listener update did not request an automatic listener restart")
	}

	// The full same-version payload is compared to the configuration that is
	// currently in use, so moving the listener back also restarts it.
	update.ListenAddress = config.ListenAddress
	update.PublicEndpoint = "ws://relay-same-version.example.test/v2/connect"
	if err := relay.applyConfiguration(9, update, false); err != nil {
		t.Fatalf("applyConfiguration(same version) error = %v", err)
	}
	if !relay.restartRequired.Load() || relay.currentPublicEndpoint() != update.PublicEndpoint {
		t.Fatalf("same-version update restart=%v endpoint=%q", relay.restartRequired.Load(), relay.currentPublicEndpoint())
	}
}

func TestHealthAndDataPlaneFailClosed(t *testing.T) {
	relay, err := New(validRuntimeConfig(), &directoryClientStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	response := httptest.NewRecorder()
	relay.healthHandler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready health status = %d", response.Code)
	}
	relay.routingReady.Store(true)
	response = httptest.NewRecorder()
	relay.healthHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ready health status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	relay.publicHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v2/connect", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured data plane status = %d, want fail-closed 503", response.Code)
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("v2 not-ready response is missing Retry-After")
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("security headers are missing: %v", response.Header())
	}
	response = httptest.NewRecorder()
	relay.publicHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/connect", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("legacy v1 carrier endpoint status = %d, want 404", response.Code)
	}
}

func TestMetricsExposeBoundedConnectionSignals(t *testing.T) {
	connections, err := relayserver.NewConnectionManager(10, 2)
	if err != nil {
		t.Fatal(err)
	}
	connections.RecordHandshake(true)
	connections.RecordHandshake(false)
	connections.RecordRouteRejection()
	connections.RecordRateLimit()
	connections.RecordIngress(123)
	connections.RecordEgress(456, 2*time.Millisecond)
	relay, err := New(validRuntimeConfig(), &directoryClientStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	relay.connections = connections
	response := httptest.NewRecorder()
	relay.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, metric := range []string{
		"wenzwork_relay_handshakes_succeeded_total 1", "wenzwork_relay_handshakes_failed_total 1",
		"wenzwork_relay_route_rejected_total 1", "wenzwork_relay_ingress_bytes_total 123",
		"wenzwork_relay_egress_bytes_total 456", "wenzwork_relay_write_loop_lag_seconds 0.002000",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("metrics output does not contain %q:\n%s", metric, body)
		}
	}
	for _, forbidden := range []string{"device_id", "user_id", "installation_id", "instance_id"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics output contains high-cardinality label %q", forbidden)
		}
	}
}

func TestMetricsExposeV2GlobalQueueBudgetWithoutIdentifiers(t *testing.T) {
	budget, err := relayserver.NewV2QueueBudget(1<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	handler := &relayserver.V2Handler{QueueBudget: budget}
	relay, err := New(validRuntimeConfig(), &directoryClientStub{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	relay.dataPlaneV2 = handler
	relay.v2Hub = relayserver.NewV2Hub()
	response := httptest.NewRecorder()
	relay.healthHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, metric := range []string{
		"wenzwork_relay_v2_carriers 0", "wenzwork_relay_v2_link_routes 0",
		"wenzwork_relay_v2_queue_bytes 0", "wenzwork_relay_v2_queue_frames 0",
		"wenzwork_relay_v2_queue_budget_bytes 1048576", "wenzwork_relay_v2_queue_rejected_total 0",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("v2 metrics output does not contain %q:\n%s", metric, body)
		}
	}
}

func TestEndpointAttestationRequiresReadinessAndBindsRuntimeIdentity(t *testing.T) {
	publicKey, privateKey, err := relayidentity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	relay, err := New(validRuntimeConfig(), &directoryClientStub{}, nil, WithIdentityPrivateKey(privateKey))
	if err != nil {
		t.Fatal(err)
	}
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	request := httptest.NewRequest(http.MethodGet, "/.well-known/wenzwork-relay?nonce="+nonce, nil)
	response := httptest.NewRecorder()
	relay.publicHandler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready attestation status = %d", response.Code)
	}
	relay.routingReady.Store(true)
	response = httptest.NewRecorder()
	relay.publicHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("attestation status = %d body=%s", response.Code, response.Body.String())
	}
	var attestation relayidentity.EndpointAttestation
	if err := json.Unmarshal(response.Body.Bytes(), &attestation); err != nil {
		t.Fatal(err)
	}
	if attestation.Nonce != nonce || attestation.InstanceID != relay.instanceID ||
		attestation.InstallationID != relay.config.InstallationID || attestation.CellID != relay.config.CellID {
		t.Fatalf("attestation = %+v", attestation)
	}
	if err := relayidentity.VerifyEndpointAttestation(publicKey, attestation); err != nil {
		t.Fatalf("VerifyEndpointAttestation() error = %v", err)
	}
}

func validRuntimeConfig() relayhost.Config {
	return relayhost.Config{
		InstallationID: uuid.New(), CellID: uuid.New(), Version: "1.0.0", ProtocolVersion: 2,
		DirectoryURL: "https://directory.example.test", ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
		IdentityPrivateKeyFile: "/var/lib/wenzwork-relay/identity/identity.key",
		CertificateFile:        "/etc/wenzwork-relay/tls/node.crt", CACertificateFile: "/etc/wenzwork-relay/tls/ca.crt",
	}
}
