package relayruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayhost"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
)

var ErrRevoked = relaymanagement.ErrInstallationRevoked

type DirectoryClient interface {
	Register(context.Context, relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error)
	Heartbeat(context.Context, relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error)
	Unregister(context.Context, uuid.UUID) error
}

type DataPlaneProbe interface {
	Ping(context.Context) error
}

type browserOriginPatternUpdater interface {
	UpdateBrowserOriginPatterns([]string)
}

type listenerServerError struct {
	generation int
	listener   string
	err        error
}

type Runtime struct {
	configMu             sync.RWMutex
	config               relayhost.Config
	client               DirectoryClient
	log                  *slog.Logger
	instanceID           uuid.UUID
	startedAt            time.Time
	registered           atomic.Bool
	routingReady         atomic.Bool
	draining             atomic.Bool
	lastHeartbeatUnix    atomic.Int64
	leaseExpiresUnix     atomic.Int64
	configurationVersion atomic.Int64
	publicEndpoint       atomic.Value
	restartRequired      atomic.Bool
	listenerRestart      chan struct{}
	identityKey          ed25519.PrivateKey
	dataPlane            http.Handler
	dataPlaneV2          http.Handler
	connections          *relayserver.ConnectionManager
	v2Hub                *relayserver.V2Hub
	dataPlaneProbe       DataPlaneProbe
}

type v2QueueBudgetProvider interface {
	QueueBudgetSnapshot() relayserver.V2QueueBudgetSnapshot
}

type Option func(*Runtime) error

func WithIdentityPrivateKey(privateKey ed25519.PrivateKey) Option {
	return func(runtime *Runtime) error {
		if len(privateKey) != ed25519.PrivateKeySize {
			return errors.New("Relay identity private key is invalid")
		}
		runtime.identityKey = append(ed25519.PrivateKey(nil), privateKey...)
		return nil
	}
}

func WithInstanceID(instanceID uuid.UUID) Option {
	return func(runtime *Runtime) error {
		if instanceID == uuid.Nil {
			return errors.New("Relay instance ID is invalid")
		}
		runtime.instanceID = instanceID
		return nil
	}
}

func WithDataPlane(handler http.Handler, connections *relayserver.ConnectionManager, probe DataPlaneProbe) Option {
	return func(runtime *Runtime) error {
		if handler == nil || connections == nil || probe == nil {
			return errors.New("Relay data-plane dependencies are invalid")
		}
		runtime.dataPlane, runtime.connections, runtime.dataPlaneProbe = handler, connections, probe
		return nil
	}
}

// WithDataPlaneV2 installs the sole remote/v2 Carrier endpoint. The probe is
// normally the shared Route registry and is deliberately independent from the
// retired v1 ConnectionManager.
func WithDataPlaneV2(handler http.Handler, hub *relayserver.V2Hub, probe DataPlaneProbe) Option {
	return func(runtime *Runtime) error {
		if handler == nil || hub == nil || probe == nil {
			return errors.New("Relay v2 data-plane dependencies are invalid")
		}
		runtime.dataPlaneV2, runtime.v2Hub, runtime.dataPlaneProbe = handler, hub, probe
		return nil
	}
}

func New(config relayhost.Config, client DirectoryClient, logger *slog.Logger, options ...Option) (*Runtime, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("Relay Directory client is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	result := &Runtime{
		config: config, client: client, log: logger, instanceID: uuid.New(), startedAt: time.Now().UTC(),
		listenerRestart: make(chan struct{}, 1),
	}
	result.configurationVersion.Store(config.ConfigurationVersion)
	result.publicEndpoint.Store(config.PublicEndpoint)
	for _, option := range options {
		if option == nil {
			return nil, errors.New("Relay runtime option is invalid")
		}
		if err := option(result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (relay *Runtime) Run(ctx context.Context) error {
	serverErrors := make(chan listenerServerError, 8)
	generation := 1
	publicServer, healthServer := relay.startServers(generation, relay.currentConfig(), serverErrors)
	defer func() {
		relay.routingReady.Store(false)
		if relay.connections != nil {
			relay.connections.BeginDrain(remotev1.GoAwayReason_GO_AWAY_REASON_NODE_DRAINING, time.Second, false, 10*time.Second)
		}
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = publicServer.Shutdown(shutdownContext)
		_ = healthServer.Shutdown(shutdownContext)
		if relay.registered.Load() {
			_ = relay.client.Unregister(shutdownContext, relay.instanceID)
		}
		if relay.connections != nil {
			// Give queued GOAWAY frames a bounded chance to leave before forcing
			// hijacked WebSocket sessions closed.
			timer := time.NewTimer(250 * time.Millisecond)
			<-timer.C
			timer.Stop()
			relay.connections.CloseAll(websocket.StatusGoingAway, "relay shutting down")
		}
		if relay.v2Hub != nil {
			relay.v2Hub.CloseAll()
		}
	}()

	loopErrors := make(chan error, 1)
	go func() { loopErrors <- relay.directoryLoop(ctx) }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-serverErrors:
			if event.generation != generation && errors.Is(event.err, http.ErrServerClosed) {
				continue
			}
			if errors.Is(event.err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("Relay %s listener failed: %w", event.listener, event.err)
		case err := <-loopErrors:
			return err
		case <-relay.listenerRestart:
			relay.log.Info("Restarting Relay listeners to apply management configuration")
			relay.routingReady.Store(false)
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = publicServer.Shutdown(shutdownContext)
			_ = healthServer.Shutdown(shutdownContext)
			cancel()
			generation++
			publicServer, healthServer = relay.startServers(generation, relay.currentConfig(), serverErrors)
		}
	}
}

func (relay *Runtime) startServers(generation int, config relayhost.Config, errors chan<- listenerServerError) (*http.Server, *http.Server) {
	publicServer := relay.server(config.ListenAddress, relay.publicHandler())
	healthServer := relay.server(config.HealthAddress, relay.healthHandler())
	go func() {
		errors <- listenerServerError{generation: generation, listener: "public", err: publicServer.ListenAndServe()}
	}()
	go func() {
		errors <- listenerServerError{generation: generation, listener: "health", err: healthServer.ListenAndServe()}
	}()
	return publicServer, healthServer
}

func (relay *Runtime) directoryLoop(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		config := relay.currentConfig()
		registerContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := relay.client.Register(registerContext, relaymanagement.RegisterInstanceInput{
			InstanceID: relay.instanceID, Version: config.Version, ProtocolVersion: config.ProtocolVersion,
			Addresses: relay.advertisedAddresses(), Capabilities: map[string]any{
				"os": runtime.GOOS, "architecture": runtime.GOARCH, "healthAddress": config.HealthAddress,
			}, StartedAt: relay.startedAt,
		})
		cancel()
		if err != nil {
			relay.registered.Store(false)
			relay.routingReady.Store(false)
			if errors.Is(err, ErrRevoked) || errors.Is(err, relaymanagement.ErrAccessKeyInvalid) {
				return err
			}
			relay.log.Warn("Relay Directory registration failed", "error", err, "retry_after", backoff.String())
			if !wait(ctx, backoff) {
				return nil
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		relay.registered.Store(true)
		backoff = time.Second
		relay.log.Info("Relay instance registered", "instance_id", relay.instanceID, "installation_id", config.InstallationID, "cell_id", config.CellID)
		if err := relay.heartbeatLoop(ctx); err != nil {
			if errors.Is(err, ErrRevoked) || errors.Is(err, relaymanagement.ErrAccessKeyInvalid) {
				return err
			}
			relay.registered.Store(false)
			relay.routingReady.Store(false)
			relay.log.Warn("Relay Directory heartbeat interrupted", "error", err)
			if !wait(ctx, time.Second) {
				return nil
			}
		}
	}
}

func (relay *Runtime) heartbeatLoop(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		activeConnections := int64(0)
		residentRoutes := []relaymanagement.HeartbeatRoute{}
		if relay.connections != nil {
			activeConnections = relay.connections.Metrics().Active
			for _, route := range relay.connections.ResidentRoutes() {
				residentRoutes = append(residentRoutes, relaymanagement.HeartbeatRoute{
					DeviceID: route.DeviceID, UserID: route.UserID, ConnectionID: route.ConnectionID,
					ConnectionEpoch: route.ConnectionEpoch, AssignmentVersion: route.AssignmentVersion,
					GrantVersion: route.GrantVersion, ProtocolVersion: route.ProtocolVersion,
				})
			}
			slices.SortFunc(residentRoutes, func(left, right relaymanagement.HeartbeatRoute) int {
				return strings.Compare(left.DeviceID, right.DeviceID)
			})
			// A connection can finish its handshake between the metrics and route
			// snapshots. Keep the authenticated route count internally consistent
			// so that Host does not reject the entire heartbeat for that benign race.
			if int64(len(residentRoutes)) > activeConnections {
				activeConnections = int64(len(residentRoutes))
			}
		}
		if relay.v2Hub != nil {
			for _, route := range relay.v2Hub.ResidentRoutes() {
				residentRoutes = append(residentRoutes, relaymanagement.HeartbeatRoute{
					DeviceID: route.DeviceID, UserID: route.UserID, ConnectionID: route.ConnectionID,
					ConnectionEpoch: route.ConnectionEpoch, AssignmentVersion: route.AssignmentVersion,
					GrantVersion: route.GrantVersion, ProtocolVersion: route.ProtocolVersion,
				})
			}
			activeConnections += relay.v2Hub.ActiveCarriers()
		}
		slices.SortFunc(residentRoutes, func(left, right relaymanagement.HeartbeatRoute) int {
			return strings.Compare(left.DeviceID, right.DeviceID)
		})
		heartbeatContext, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, err := relay.client.Heartbeat(heartbeatContext, relaymanagement.HeartbeatInput{
			InstanceID: relay.instanceID, ConfigurationVersion: relay.configurationVersion.Load(),
			MemoryBytes: int64(memory.Alloc), Addresses: relay.advertisedAddresses(),
			ActiveConnections: activeConnections,
			ResidentRoutes:    residentRoutes,
			Capabilities: map[string]any{
				"os": runtime.GOOS, "architecture": runtime.GOARCH, "remoteV2": relay.dataPlaneV2 != nil,
				"configurationVersion": relay.configurationVersion.Load(), "restartRequired": relay.restartRequired.Load(),
			},
		})
		cancel()
		if err != nil {
			if errors.Is(err, ErrRevoked) || errors.Is(err, relaymanagement.ErrAccessKeyInvalid) {
				relay.routingReady.Store(false)
				if relay.connections != nil {
					relay.connections.Revoke()
				}
				if relay.v2Hub != nil {
					relay.v2Hub.CloseAll()
				}
			}
			return err
		}
		// Compare the full payload even when a management process restart kept the
		// installation-backed version unchanged. This still detects global
		// listener/key/limit changes and marks them as restart-required.
		currentConfigurationVersion := relay.configurationVersion.Load()
		if result.ConfigurationVersion > currentConfigurationVersion ||
			(result.ConfigurationVersion > 0 && result.ConfigurationVersion == currentConfigurationVersion) {
			if err := relay.applyConfiguration(result.ConfigurationVersion, result.Configuration, result.RestartRequired); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		relay.lastHeartbeatUnix.Store(now.Unix())
		relay.leaseExpiresUnix.Store(result.LeaseExpiresAt.Unix())
		relay.draining.Store(result.Drain)
		if relay.connections != nil {
			for _, rejected := range result.RejectedRoutes {
				reason := remotev1.GoAwayReason_GO_AWAY_REASON_ASSIGNMENT_CHANGED
				if rejected.Reason == "grant_revoked" {
					reason = remotev1.GoAwayReason_GO_AWAY_REASON_GRANT_REVOKED
				}
				relay.connections.RejectResident(rejected.DeviceID, rejected.ConnectionID, rejected.ConnectionEpoch, reason)
			}
		}
		if relay.v2Hub != nil {
			for _, rejected := range result.RejectedRoutes {
				relay.v2Hub.RejectResident(rejected.DeviceID, rejected.ConnectionID, rejected.ConnectionEpoch)
			}
		}
		dataPlaneReady := relay.dataPlaneHealthy(ctx)
		relay.routingReady.Store(result.RoutingReady && !result.Drain && !result.Revoked && dataPlaneReady)
		if result.Revoked {
			if relay.connections != nil {
				relay.connections.Revoke()
			}
			if relay.v2Hub != nil {
				relay.v2Hub.CloseAll()
			}
		} else if result.Drain && relay.connections != nil {
			relay.connections.BeginDrain(remotev1.GoAwayReason_GO_AWAY_REASON_NODE_DRAINING, time.Second, false, 15*time.Minute)
		} else if relay.connections != nil {
			relay.connections.Resume()
		}
		if result.Revoked {
			return ErrRevoked
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-relayRouteChanges(relay.connections):
		case <-relayV2RouteChanges(relay.v2Hub):
		}
	}
}

func relayRouteChanges(connections *relayserver.ConnectionManager) <-chan struct{} {
	if connections == nil {
		return nil
	}
	return connections.RouteChanges()
}

func relayV2RouteChanges(hub *relayserver.V2Hub) <-chan struct{} {
	if hub == nil {
		return nil
	}
	return hub.RouteChanges()
}

func (relay *Runtime) publicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/health/live", relay.healthHandler())
	mux.Handle("/health/ready", relay.healthHandler())
	mux.HandleFunc("/.well-known/wenzwork-relay", relay.endpointAttestation)
	mux.HandleFunc("/v2/connect", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", "5")
		if !relay.routingReady.Load() {
			http.Error(writer, "Relay is not routing-ready", http.StatusServiceUnavailable)
			return
		}
		if relay.dataPlaneV2 == nil {
			http.Error(writer, "Relay v2 data plane is not configured", http.StatusServiceUnavailable)
			return
		}
		relay.dataPlaneV2.ServeHTTP(writer, request)
	})
	return securityHeaders(mux)
}

func (relay *Runtime) endpointAttestation(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || !relay.routingReady.Load() || len(relay.identityKey) != ed25519.PrivateKeySize {
		http.Error(writer, "Relay is not routing-ready", http.StatusServiceUnavailable)
		return
	}
	values := request.URL.Query()
	nonce := values.Get("nonce")
	decoded, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(decoded) != 32 || len(values) != 1 {
		http.Error(writer, "invalid validation nonce", http.StatusBadRequest)
		return
	}
	config := relay.currentConfig()
	attestation, err := relayidentity.SignEndpointAttestation(relay.identityKey, relayidentity.EndpointAttestation{
		SchemaVersion: 1, Nonce: nonce, InstallationID: config.InstallationID,
		CellID: config.CellID, InstanceID: relay.instanceID,
	})
	if err != nil {
		http.Error(writer, "endpoint attestation unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(writer, http.StatusOK, attestation)
}

func (relay *Runtime) healthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/health/ready", func(writer http.ResponseWriter, _ *http.Request) {
		if !relay.routingReady.Load() {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/status", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, relay.status())
	})
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(writer, "# TYPE wenzwork_relay_registered gauge\nwenzwork_relay_registered %s\n", boolMetric(relay.registered.Load()))
		fmt.Fprintf(writer, "# TYPE wenzwork_relay_routing_ready gauge\nwenzwork_relay_routing_ready %s\n", boolMetric(relay.routingReady.Load()))
		fmt.Fprintf(writer, "# TYPE wenzwork_relay_draining gauge\nwenzwork_relay_draining %s\n", boolMetric(relay.draining.Load()))
		fmt.Fprintf(writer, "# TYPE wenzwork_relay_last_heartbeat_seconds gauge\nwenzwork_relay_last_heartbeat_seconds %d\n", relay.lastHeartbeatUnix.Load())
		if relay.connections != nil {
			metrics := relay.connections.Metrics()
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_connections gauge\nwenzwork_relay_connections %d\n", metrics.Active)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_connections_peak gauge\nwenzwork_relay_connections_peak %d\n", metrics.Peak)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_handshakes_active gauge\nwenzwork_relay_handshakes_active %d\n", metrics.ActiveHandshakes)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_handshakes_succeeded_total counter\nwenzwork_relay_handshakes_succeeded_total %d\n", metrics.HandshakeSucceeded)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_handshakes_failed_total counter\nwenzwork_relay_handshakes_failed_total %d\n", metrics.HandshakeFailed)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_queue_rejected_total counter\nwenzwork_relay_queue_rejected_total %d\n", metrics.QueueRejected)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_queue_frames gauge\nwenzwork_relay_queue_frames %d\n", metrics.QueueFrames)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_queue_bytes gauge\nwenzwork_relay_queue_bytes %d\n", metrics.QueueBytes)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_handshake_rejected_total counter\nwenzwork_relay_handshake_rejected_total %d\n", metrics.HandshakeRejected)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_superseded_sessions_total counter\nwenzwork_relay_superseded_sessions_total %d\n", metrics.SupersededSessions)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_drain_started_total counter\nwenzwork_relay_drain_started_total %d\n", metrics.DrainStarted)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_route_rejected_total counter\nwenzwork_relay_route_rejected_total %d\n", metrics.RouteRejected)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_rate_limited_total counter\nwenzwork_relay_rate_limited_total %d\n", metrics.RateLimited)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_ingress_frames_total counter\nwenzwork_relay_ingress_frames_total %d\n", metrics.IngressFrames)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_ingress_bytes_total counter\nwenzwork_relay_ingress_bytes_total %d\n", metrics.IngressBytes)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_egress_frames_total counter\nwenzwork_relay_egress_frames_total %d\n", metrics.EgressFrames)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_egress_bytes_total counter\nwenzwork_relay_egress_bytes_total %d\n", metrics.EgressBytes)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_write_loop_lag_seconds gauge\nwenzwork_relay_write_loop_lag_seconds %.6f\n", float64(metrics.WriteLagMicros)/1_000_000)
		}
		if relay.v2Hub != nil {
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_v2_carriers gauge\nwenzwork_relay_v2_carriers %d\n", relay.v2Hub.ActiveCarriers())
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_v2_link_routes gauge\nwenzwork_relay_v2_link_routes %d\n", relay.v2Hub.ActiveLinkRoutes())
		}
		if provider, ok := relay.dataPlaneV2.(v2QueueBudgetProvider); ok {
			budget := provider.QueueBudgetSnapshot()
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_v2_queue_bytes gauge\nwenzwork_relay_v2_queue_bytes %d\n", budget.Bytes)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_v2_queue_frames gauge\nwenzwork_relay_v2_queue_frames %d\n", budget.Frames)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_v2_queue_budget_bytes gauge\nwenzwork_relay_v2_queue_budget_bytes %d\n", budget.MaxBytes)
			fmt.Fprintf(writer, "# TYPE wenzwork_relay_v2_queue_rejected_total counter\nwenzwork_relay_v2_queue_rejected_total %d\n", budget.Rejected)
		}
	})
	return securityHeaders(mux)
}

func (relay *Runtime) dataPlaneHealthy(ctx context.Context) bool {
	if relay.dataPlaneV2 == nil || relay.v2Hub == nil || relay.dataPlaneProbe == nil {
		return false
	}
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return relay.dataPlaneProbe.Ping(probeContext) == nil
}

func (relay *Runtime) status() map[string]any {
	config := relay.currentConfig()
	lastHeartbeat := relay.lastHeartbeatUnix.Load()
	leaseExpires := relay.leaseExpiresUnix.Load()
	result := map[string]any{
		"installationId": config.InstallationID, "cellId": config.CellID,
		"instanceId": relay.instanceID, "version": config.Version,
		"protocolVersion": config.ProtocolVersion, "registered": relay.registered.Load(),
		"routingReady": relay.routingReady.Load(), "draining": relay.draining.Load(),
		"configurationVersion": relay.configurationVersion.Load(),
		"publicEndpoint":       relay.currentPublicEndpoint(), "restartRequired": relay.restartRequired.Load(),
		"startedAt": relay.startedAt,
	}
	if lastHeartbeat > 0 {
		result["lastHeartbeatAt"] = time.Unix(lastHeartbeat, 0).UTC()
	}
	if leaseExpires > 0 {
		result["leaseExpiresAt"] = time.Unix(leaseExpires, 0).UTC()
	}
	return result
}

func (relay *Runtime) advertisedAddresses() []string {
	if endpoint := relay.currentPublicEndpoint(); endpoint != "" {
		return []string{endpoint}
	}
	return []string{relay.currentConfig().ListenAddress}
}

func (relay *Runtime) currentPublicEndpoint() string {
	value := relay.publicEndpoint.Load()
	if endpoint, ok := value.(string); ok {
		return endpoint
	}
	return ""
}

func (relay *Runtime) currentConfig() relayhost.Config {
	relay.configMu.RLock()
	defer relay.configMu.RUnlock()
	return relay.config
}

func (relay *Runtime) applyConfiguration(version int64, update relaymanagement.AgentRuntimeConfiguration, serverRestartRequired bool) error {
	currentVersion := relay.configurationVersion.Load()
	if version < currentVersion {
		return nil
	}
	current := relay.currentConfig()
	candidate := current
	candidate.PublicEndpoint = update.PublicEndpoint
	candidate.BrowserOriginPatterns = append([]string(nil), update.BrowserOriginPatterns...)
	candidate.ProtocolVersion = update.ProtocolVersion
	candidate.ListenAddress = update.ListenAddress
	candidate.HealthAddress = update.HealthAddress
	candidate.RedisURL = update.RedisURL
	candidate.TicketIssuer = update.TicketIssuer
	candidate.TicketPublicKeys = cloneStringMap(update.TicketPublicKeys)
	candidate.DeviceLinkGrantPublicKeys = cloneStringMap(update.DeviceLinkGrantPublicKeys)
	candidate.ConnectionHardLimit = update.ConnectionHardLimit
	candidate.HandshakeConcurrency = update.HandshakeConcurrency
	if err := candidate.Validate(); err != nil {
		return fmt.Errorf("Relay management configuration update is invalid: %w", err)
	}
	if err := candidate.ValidateDataPlane(); err != nil {
		return fmt.Errorf("Relay management data-plane update is invalid: %w", err)
	}
	configurationRequiresRestart := relayConfigurationRequiresRestart(current, candidate)
	restartRequired := serverRestartRequired || configurationRequiresRestart
	endpointChanged := relay.currentPublicEndpoint() != candidate.PublicEndpoint
	restartChanged := relay.restartRequired.Load() != restartRequired
	if version == currentVersion && !endpointChanged && !restartChanged && !configurationRequiresRestart {
		return nil
	}
	relay.configMu.Lock()
	relay.config = candidate
	relay.configMu.Unlock()
	if updater, ok := relay.dataPlane.(browserOriginPatternUpdater); ok {
		updater.UpdateBrowserOriginPatterns(candidate.BrowserOriginPatterns)
	}
	if updater, ok := relay.dataPlaneV2.(browserOriginPatternUpdater); ok {
		updater.UpdateBrowserOriginPatterns(candidate.BrowserOriginPatterns)
	}
	relay.publicEndpoint.Store(candidate.PublicEndpoint)
	relay.restartRequired.Store(restartRequired)
	relay.configurationVersion.Store(version)
	relay.log.Info("Relay management configuration updated", "configuration_version", version,
		"public_endpoint", candidate.PublicEndpoint, "restart_required", restartRequired)
	if configurationRequiresRestart {
		select {
		case relay.listenerRestart <- struct{}{}:
		default:
		}
	}
	return nil
}

func relayConfigurationRequiresRestart(current, candidate relayhost.Config) bool {
	return current.ProtocolVersion != candidate.ProtocolVersion ||
		current.ListenAddress != candidate.ListenAddress || current.HealthAddress != candidate.HealthAddress ||
		!slices.Equal(current.BrowserOriginPatterns, candidate.BrowserOriginPatterns) ||
		current.RedisURL != candidate.RedisURL || current.TicketIssuer != candidate.TicketIssuer ||
		!maps.Equal(current.TicketPublicKeys, candidate.TicketPublicKeys) ||
		!maps.Equal(current.DeviceLinkGrantPublicKeys, candidate.DeviceLinkGrantPublicKeys) ||
		current.ConnectionHardLimit != candidate.ConnectionHardLimit ||
		current.HandshakeConcurrency != candidate.HandshakeConcurrency
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (relay *Runtime) server(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(writer, request)
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func boolMetric(value bool) string {
	return strconv.Itoa(map[bool]int{false: 0, true: 1}[value])
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
