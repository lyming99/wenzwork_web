package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/peerprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayserver"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"github.com/wenzwork/wenzwork-web/server/internal/remotedevice"
	"google.golang.org/protobuf/proto"
)

type targetConfig struct {
	controlURL      *url.URL
	accessKey       string
	directAccessKey string
	tlsCAFile       string
	direct          directV2Config
	state           *agentState
}

type allocationEndpoint struct {
	CellID uuid.UUID `json:"cellId"`
	URL    string    `json:"url"`
}

type allocationResponse struct {
	AssignmentID             uuid.UUID                  `json:"assignmentId"`
	Primary                  allocationEndpoint         `json:"primary"`
	Fallbacks                []allocationEndpoint       `json:"fallbacks"`
	ConnectionTicket         string                     `json:"connectionTicket"`
	TicketExpiresAt          time.Time                  `json:"ticketExpiresAt"`
	AssignmentLeaseExpiresAt time.Time                  `json:"assignmentLeaseExpiresAt"`
	PeerTicketTrust          peerTicketTrustBundle      `json:"peerTicketTrust"`
	DeviceLinkGrantTrust     deviceLinkGrantTrustBundle `json:"deviceLinkGrantTrust"`
}

type peerTicketTrustKey struct {
	KeyID     string `json:"keyId"`
	Algorithm string `json:"algorithm"`
	PublicKey string `json:"publicKey"`
}

type peerTicketTrustBundle struct {
	Issuer string               `json:"issuer"`
	Keys   []peerTicketTrustKey `json:"keys"`
}

type relayConnection struct {
	socket   *websocket.Conn
	epoch    uint64
	writeMu  sync.Mutex
	writer   *relayWriteScheduler
	sequence atomic.Uint64
}

const (
	maximumTargetPeerSessions        = 8
	maximumTargetPeerSessionInbound  = 32
	maximumTargetPeerSessionDuration = 30 * 24 * time.Hour
)

func newRelayConnection(socket *websocket.Conn, epoch uint64) *relayConnection {
	connection := &relayConnection{socket: socket, epoch: epoch}
	if socket != nil {
		connection.writer = newRelayWriteScheduler(socket)
	}
	return connection
}

func (connection *relayConnection) close() {
	if connection == nil {
		return
	}
	if connection.writer != nil {
		connection.writer.close()
	}
	if connection.socket != nil {
		connection.socket.CloseNow()
	}
}

type targetPeerSession struct {
	state       *agentState
	claims      remoteauth.Claims
	opener      *peerprotocol.CipherState
	sealer      *peerprotocol.CipherState
	keys        peerprotocol.SessionKeys
	generation  uint64
	expiresAt   time.Time
	sendMu      sync.Mutex
	closing     atomic.Bool
	executor    *peerRPCExecutor
	inbound     chan targetPeerInboundFrame
	actorCancel context.CancelFunc
	actorDone   chan struct{}
}

type targetPeerInboundFrame struct {
	query  *remotev1.PeerCiphertext
	cancel *remotev1.PeerCiphertext
}

type targetPeerSessionFailure struct {
	sessionID string
}

var (
	errPeerSessionProtocol   = errors.New("encrypted Peer session is invalid")
	errRelaySocketWrite      = errors.New("Relay socket write failed")
	errRelayHeartbeatTimeout = errors.New("Relay heartbeat timed out")
)

// isRelayConnectionFault is intentionally a narrow whitelist. A malformed
// encrypted request, a session sequence failure, or a locally rejected
// response must never take down unrelated Peer sessions sharing this Relay
// WebSocket.
func isRelayConnectionFault(err error) bool {
	return errors.Is(err, errRelaySocketWrite) || errors.Is(err, errRelayHeartbeatTimeout)
}

func peerSessionProtocolError(cause error) error {
	if cause == nil {
		return errPeerSessionProtocol
	}
	return fmt.Errorf("%w: %v", errPeerSessionProtocol, cause)
}

func runTarget(ctx context.Context, config targetConfig) error {
	if config.controlURL == nil || config.state == nil {
		return errors.New("target configuration is incomplete")
	}
	client, err := targetHTTPClient(config.tlsCAFile)
	if err != nil {
		return err
	}
	controlStore, err := loadControlState(config.state)
	if err != nil {
		return err
	}
	tokens, err := newDeviceTokenManager(client, config.controlURL, controlStore)
	if err != nil {
		return err
	}
	deviceName, _ := os.Hostname()
	deviceName = strings.TrimSpace(deviceName)
	if deviceName == "" {
		deviceName = "WenzWork device agent"
	}
	sessionID, err := tokens.bootstrapOrResume(ctx, config.accessKey, deviceName)
	if err != nil {
		return err
	}
	if err := config.state.setSessionID(sessionID); err != nil {
		return err
	}
	directRuntime, err := prepareDirectV2Runtime(config.direct, config.controlURL, config.state)
	if err != nil {
		return err
	}
	if directRuntime != nil {
		defer directRuntime.close()
	}
	publicKey := config.state.identity.Public().(ed25519.PublicKey)
	proof, err := remotedevice.SignRegistration(config.state.identity, sessionID, config.state.DeviceID)
	if err != nil {
		return err
	}
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	var registration struct {
		PublicKeyThumbprint  string                     `json:"publicKeyThumbprint"`
		DeviceLinkGrantTrust deviceLinkGrantTrustBundle `json:"deviceLinkGrantTrust"`
		Device               struct {
			DirectModeEnabled bool `json:"directModeEnabled"`
		} `json:"device"`
	}
	directRegistration := directV2Registration{}
	capabilities := agentRegistrationCapabilities(config.state)
	if directRuntime != nil {
		directRegistration = directRuntime.registration()
		capabilities = append(capabilities, "direct.connect")
	}
	if err := tokens.doJSON(ctx, http.MethodPost, "/v1/device/registrations", "registration-"+uuid.NewString(), map[string]any{
		"deviceName": deviceName, "platform": platform, "agentVersion": version, "protocolMin": 2, "protocolMax": 2,
		"capabilities":      capabilities,
		"identityAlgorithm": "ed25519", "identityPublicKey": base64.RawURLEncoding.EncodeToString(publicKey), "proof": proof,
		"directEnabled": directRegistration.Enabled, "directIp": directRegistration.IP,
		"directPort": directRegistration.Port, "directConnectionEpoch": directRegistration.ConnectionEpoch,
		"directTlsEnabled": directRegistration.TLSEnabled,
	}, &registration); err != nil {
		return fmt.Errorf("device registration failed: %w", err)
	}
	if registration.PublicKeyThumbprint != remoteauth.PublicKeyThumbprint(publicKey) {
		return errors.New("device registration identity mismatch")
	}
	var directVerifier remoteauth.DeviceLinkGrantVerifier
	if directRuntime != nil {
		directVerifier, err = verifierFromDeviceLinkGrantTrustBundle(registration.DeviceLinkGrantTrust)
		if err != nil {
			return errors.New("device direct connection trust bundle is invalid")
		}
	}
	controlLoop, err := newDeviceControlLoop(config.state, controlStore, tokens, client)
	if err != nil {
		return err
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	controlLoop.runContext = runContext
	config.state.controlStore = controlStore
	config.state.controlLoop = controlLoop
	controlResult := make(chan error, 1)
	relayResult := make(chan error, 1)
	var directResult <-chan error
	var hostRoutes *hostV2RouteCoordinator
	if directRuntime != nil {
		hostRoutes = newHostV2RouteCoordinator(registration.Device.DirectModeEnabled)
	}
	go func() { controlResult <- controlLoop.run(runContext) }()
	// remote/v2 is deliberately not a negotiated fallback. A v2 Agent obtains
	// only a /v2 Carrier allocation and rejects the legacy v1 subprotocol.
	go func() {
		if hostRoutes == nil {
			relayResult <- runTargetRelayLoopV2(runContext, client, tokens, config.state)
			return
		}
		relayResult <- hostRoutes.run(runContext, func(relayContext context.Context) error {
			return runTargetRelayLoopV2(relayContext, client, tokens, config.state)
		})
	}()
	if directRuntime != nil {
		result := make(chan error, 1)
		directResult = result
		go func() {
			result <- directRuntime.run(runContext, config.state, directVerifier, tokens, hostRoutes, registration.Device.DirectModeEnabled, config.directAccessKey, directV2DeviceMetadata{
				Name: deviceName, Platform: platform, AgentVersion: version,
			})
		}()
	}
	select {
	case <-ctx.Done():
		cancel()
		return nil
	case err := <-controlResult:
		cancel()
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("device control loop stopped: %w", err)
	case err := <-relayResult:
		cancel()
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("Relay connection loop stopped: %w", err)
	case err := <-directResult:
		cancel()
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("direct connection listener stopped: %w", err)
	}
}

func agentRegistrationCapabilities(state *agentState) []string {
	capabilities := []string{
		"relay.ping", "remote.project.sync", "remote.task.workspace.inspect", "remote.task.markdown.render", "remote.task.ai.summarize",
		"remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.file.send", "remote.peer.file.receive",
	}
	if agentFeatureFlags(state)["terminal.interactive"] {
		capabilities = append(capabilities, "remote.peer.terminal.interactive")
	}
	if agentFeatureFlags(state)["tasks.v2"] {
		capabilities = append(capabilities, "remote.peer.task.control")
	}
	if agentFeatureFlags(state)["ai.workspaceTools"] {
		capabilities = append(capabilities, "remote.peer.ai.tools")
	}
	if agentFeatureFlags(state)["events.v1"] {
		capabilities = append(capabilities, "remote.peer.events")
	}
	return capabilities
}

func runTargetRelayLoop(ctx context.Context, client *http.Client, tokens *deviceTokenManager, state *agentState) error {
	return runTargetRelayLoopWithHooks(ctx, client, tokens, state, targetRelayLoopHooks{
		dial: dialTargetRelay,
		serve: func(ctx context.Context, connection *relayConnection, heartbeat time.Duration, state *agentState, verifier remoteauth.Verifier) error {
			return serveTargetPeer(ctx, connection, heartbeat, state, verifier)
		},
		close: func(connection *relayConnection) { connection.close() },
	})
}

type targetRelayLoopHooks struct {
	dial  func(context.Context, *http.Client, allocationResponse, *agentState) (*relayConnection, time.Duration, error)
	serve func(context.Context, *relayConnection, time.Duration, *agentState, remoteauth.Verifier) error
	close func(*relayConnection)
}

func runTargetRelayLoopWithHooks(ctx context.Context, client *http.Client, tokens *deviceTokenManager, state *agentState, hooks targetRelayLoopHooks) error {
	if hooks.dial == nil || hooks.serve == nil || hooks.close == nil {
		return errors.New("Relay loop hooks are invalid")
	}
	backoff := 500 * time.Millisecond
	reconnectAttempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		connectionEpoch, err := state.advanceConnectionEpoch()
		if err != nil {
			return err
		}
		state.recordConnectionDiagnostic(
			"relay_allocation_requested", "requested", connectionEpoch,
			reconnectAttempt, 0, 0,
		)
		var allocation allocationResponse
		err = tokens.doJSON(ctx, http.MethodPost, "/v1/device/relay-allocations", "allocation-"+uuid.NewString(), map[string]any{
			"remoteDeviceId": state.DeviceID, "protocolMin": 1, "protocolMax": 1,
			"fileProtocolVersion": 1, "connectionEpoch": connectionEpoch,
		}, &allocation)
		if errors.Is(err, errDeviceAuthentication) {
			state.recordConnectionDiagnostic(
				"relay_allocation_failed", "authentication_failed", connectionEpoch,
				reconnectAttempt, 0, 0,
			)
			return err
		}
		if err == nil && (allocation.AssignmentID == uuid.Nil || allocation.Primary.CellID == uuid.Nil || allocation.ConnectionTicket == "" ||
			!allocation.TicketExpiresAt.After(time.Now().UTC().Add(time.Second))) {
			err = errors.New("Relay allocation response is invalid")
		}
		if err != nil {
			state.recordConnectionDiagnostic(
				"relay_allocation_failed", remoteConnectionDiagnosticReason(err), connectionEpoch,
				reconnectAttempt, 0, 0,
			)
		} else {
			state.recordConnectionDiagnostic(
				"relay_allocation_received", "allocated", connectionEpoch,
				reconnectAttempt, 0, 0,
			)
		}
		var peerVerifier remoteauth.Verifier
		if err == nil {
			// A trust bundle is allocation-scoped. It is validated and rebuilt on
			// every reconnect; a previous bundle is never retained as a fallback.
			peerVerifier, err = verifierFromTrustBundle(allocation.PeerTicketTrust)
			if err != nil {
				state.recordConnectionDiagnostic(
					"relay_allocation_invalid", "peer_ticket_trust_invalid", connectionEpoch,
					reconnectAttempt, 0, 0,
				)
			}
		}
		if err == nil {
			var connection *relayConnection
			var heartbeat time.Duration
			state.recordConnectionDiagnostic(
				"relay_dial_started", "attempting", connectionEpoch,
				reconnectAttempt, 0, 0,
			)
			connection, heartbeat, err = hooks.dial(ctx, client, allocation, state)
			if err == nil {
				backoff = 500 * time.Millisecond
				reconnectAttempt = 0
				state.recordConnectionDiagnostic(
					"relay_connected", "ready", connectionEpoch,
					reconnectAttempt, 0, heartbeat,
				)
				// The connection ticket is short-lived admission material, not a
				// lease on an established WebSocket. Relay continuously fences the
				// resident route and sends GOAWAY when an assignment is revoked or
				// moved. Tying the socket to TicketExpiresAt forced every Agent to
				// disconnect (normally every five minutes), interrupting all logical
				// Peer sessions even though the route was still healthy. A newer Host
				// supplies the much longer assignment lease as the carrier bound so
				// trust and placement are still periodically refreshed.
				serveContext, cancelServe := targetRelayServeContext(ctx, allocation.AssignmentLeaseExpiresAt)
				err = hooks.serve(serveContext, connection, heartbeat, state, peerVerifier)
				cancelServe()
				hooks.close(connection)
				state.recordConnectionDiagnostic(
					"relay_disconnected", remoteConnectionDiagnosticReason(err), connectionEpoch,
					reconnectAttempt, 0, heartbeat,
				)
			} else {
				state.recordConnectionDiagnostic(
					"relay_dial_failed", remoteConnectionDiagnosticReason(err), connectionEpoch,
					reconnectAttempt, 0, 0,
				)
			}
		}
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return ctx.Err()
		}
		state.recordConnectionDiagnostic(
			"relay_reconnect_scheduled", remoteConnectionDiagnosticReason(err), connectionEpoch,
			reconnectAttempt, backoff, 0,
		)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		reconnectAttempt++
		backoff = min(backoff*2, 30*time.Second)
	}
}

func targetRelayServeContext(parent context.Context, assignmentLeaseExpiresAt time.Time) (context.Context, context.CancelFunc) {
	if assignmentLeaseExpiresAt.IsZero() {
		return context.WithCancel(parent)
	}
	// Leave enough time to close the old socket and request a fresh allocation
	// before the assignment lease itself becomes invalid.
	deadline := assignmentLeaseExpiresAt.UTC().Add(-time.Minute)
	if !deadline.After(time.Now().UTC()) {
		deadline = assignmentLeaseExpiresAt.UTC()
	}
	return context.WithDeadline(parent, deadline)
}

func verifierFromTrustBundle(bundle peerTicketTrustBundle) (remoteauth.Verifier, error) {
	if bundle.Issuer == "" || bundle.Issuer != strings.TrimSpace(bundle.Issuer) || len(bundle.Issuer) > 128 || len(bundle.Keys) == 0 || len(bundle.Keys) > 8 {
		return remoteauth.Verifier{}, errors.New("Peer Ticket trust bundle is invalid")
	}
	keys := make(map[string]ed25519.PublicKey, len(bundle.Keys))
	for _, entry := range bundle.Keys {
		if entry.Algorithm != "Ed25519" || entry.KeyID == "" || len(entry.KeyID) > 64 || !validTrustKeyID(entry.KeyID) {
			return remoteauth.Verifier{}, errors.New("Peer Ticket trust bundle is invalid")
		}
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(entry.PublicKey)
		if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != entry.PublicKey {
			return remoteauth.Verifier{}, errors.New("Peer Ticket trust bundle is invalid")
		}
		if _, duplicate := keys[entry.KeyID]; duplicate {
			return remoteauth.Verifier{}, errors.New("Peer Ticket trust bundle is invalid")
		}
		keys[entry.KeyID] = ed25519.PublicKey(append([]byte(nil), decoded...))
	}
	return remoteauth.Verifier{Issuer: bundle.Issuer, Keys: keys, Leeway: 5 * time.Second}, nil
}

func validTrustKeyID(value string) bool {
	for _, character := range value {
		if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') && character != '.' && character != '_' && character != ':' && character != '-' {
			return false
		}
	}
	return true
}

func dialTargetRelay(ctx context.Context, client *http.Client, allocation allocationResponse, state *agentState) (*relayConnection, time.Duration, error) {
	endpoints := append([]allocationEndpoint{allocation.Primary}, allocation.Fallbacks...)
	var lastErr error
	for _, endpoint := range endpoints {
		connection, heartbeat, err := dialTargetEndpoint(ctx, client, endpoint, allocation.AssignmentID, allocation.ConnectionTicket, state)
		if err == nil {
			return connection, heartbeat, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("allocation contains no Relay endpoint")
	}
	return nil, 0, lastErr
}

func dialTargetEndpoint(ctx context.Context, client *http.Client, endpoint allocationEndpoint, assignmentID uuid.UUID, ticket string, state *agentState) (*relayConnection, time.Duration, error) {
	if !validRelayEndpoint(endpoint.URL) {
		return nil, 0, errors.New("Relay endpoint is invalid")
	}
	state.recordConnectionDiagnostic("relay_socket_dial_started", "attempting", state.ConnectionEpoch, 0, 0, 0)
	parsed, _ := url.Parse(endpoint.URL)
	header := make(http.Header)
	header.Set("Authorization", "Relay "+ticket)
	socket, response, err := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{
		HTTPClient: client, HTTPHeader: header, Subprotocols: []string{relayserver.Subprotocol}, CompressionMode: websocket.CompressionDisabled,
	})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		state.recordConnectionDiagnostic("relay_socket_dial_failed", remoteConnectionDiagnosticReason(err), state.ConnectionEpoch, 0, 0, 0)
		return nil, 0, errors.New("Relay WebSocket connection failed")
	}
	state.recordConnectionDiagnostic("relay_socket_connected", "websocket_open", state.ConnectionEpoch, 0, 0, 0)
	var connection *relayConnection
	fail := func(cause error) (*relayConnection, time.Duration, error) {
		if connection != nil {
			connection.close()
		} else {
			socket.CloseNow()
		}
		state.recordConnectionDiagnostic("relay_handshake_failed", remoteConnectionDiagnosticReason(cause), state.ConnectionEpoch, 0, 0, 0)
		return nil, 0, cause
	}
	socket.SetReadLimit(relayprotocol.AbsoluteFrameLimit)
	challengeEnvelope, err := readTargetEnvelope(ctx, socket)
	if err != nil {
		return fail(err)
	}
	challengeFrame := challengeEnvelope.GetAuthChallenge()
	if challengeEnvelope.GetProtocolVersion() != 1 || challengeFrame == nil || len(challengeFrame.GetNonce()) != 32 ||
		challengeFrame.GetRelayCellId() != endpoint.CellID.String() || challengeFrame.GetDeadline() == nil ||
		!challengeFrame.GetDeadline().AsTime().After(time.Now().UTC()) {
		return fail(errors.New("Relay challenge is invalid"))
	}
	claims, err := parseSignedClaims(ticket)
	if err != nil || claims.JWTID == "" || claims.AssignmentID != assignmentID.String() {
		return fail(errors.New("Relay ticket is invalid"))
	}
	challenge := remoteauth.Challenge{
		Nonce: challengeFrame.GetNonce(), TicketJWTID: claims.JWTID, CellID: challengeFrame.GetRelayCellId(), NodeID: challengeFrame.GetRelayNodeId(),
		ConnectionEpoch: state.ConnectionEpoch, Deadline: challengeFrame.GetDeadline().AsTime(),
	}
	signature, err := remoteauth.SignChallenge(state.identity, challenge)
	if err != nil {
		return fail(err)
	}
	connection = newRelayConnection(socket, state.ConnectionEpoch)
	if err := connection.write(ctx, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: state.ConnectionEpoch,
		Frame: &remotev1.Envelope_AuthProof{AuthProof: &remotev1.AuthProof{TicketJti: claims.JWTID, ConnectionEpoch: state.ConnectionEpoch, DeviceSignature: signature}},
	}); err != nil {
		return fail(err)
	}
	readyEnvelope, err := readTargetEnvelope(ctx, socket)
	if err != nil {
		return fail(err)
	}
	ready := readyEnvelope.GetReady()
	if ready == nil || ready.GetAcceptedConnectionEpoch() != state.ConnectionEpoch || ready.GetHeartbeatIntervalSeconds() == 0 {
		return fail(errors.New("Relay Ready is invalid"))
	}
	heartbeat := time.Duration(ready.GetHeartbeatIntervalSeconds()) * time.Second
	state.recordConnectionDiagnostic("relay_handshake_ready", "authenticated", state.ConnectionEpoch, 0, 0, heartbeat)
	return connection, heartbeat, nil
}

func validRelayEndpoint(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "ws" || parsed.Scheme == "wss") && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "/v1/connect" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func serveTargetPeer(ctx context.Context, connection *relayConnection, heartbeat time.Duration, state *agentState, peerVerifier remoteauth.Verifier) error {
	if heartbeat < time.Second {
		heartbeat = 25 * time.Second
	}
	heartbeatTimeout := targetRelayHeartbeatTimeout(heartbeat)
	serveContext, cancelServe := context.WithCancel(ctx)
	fatalErrors := make(chan error, 1)
	var fatalOnce sync.Once
	reportFatal := func(err error) {
		if err == nil || !isRelayConnectionFault(err) {
			return
		}
		fatalOnce.Do(func() {
			fatalErrors <- err
			cancelServe()
		})
	}
	peerSessions := make(map[string]*targetPeerSession)
	connectionLimiter := newPeerRPCConnectionLimiter(maximumPeerRPCInFlightPerConnection)
	sessionFailures := make(chan targetPeerSessionFailure, maximumTargetPeerSessions*2)
	drainSessionFailures := func() {
		for {
			select {
			case failure := <-sessionFailures:
				retireTargetPeerSession(peerSessions, failure.sessionID)
			default:
				return
			}
		}
	}
	defer func() {
		cancelServe()
		for _, session := range peerSessions {
			// AI generations are durable Agent work and deliberately outlive a
			// Relay socket. Stop accepting frames immediately, but do not block
			// the Relay reconnect loop while detached work reaches a terminal
			// persisted state.
			session.stop()
		}
	}()
	pingContext, cancelPings := context.WithCancel(serveContext)
	defer cancelPings()
	var lastPingSequence atomic.Uint64
	var lastPongSequence atomic.Uint64
	pongObserved := make(chan struct{}, 1)
	go func() {
		timer := time.NewTimer(heartbeatTimeout)
		defer timer.Stop()
		for {
			select {
			case <-pingContext.Done():
				return
			case <-pongObserved:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(heartbeatTimeout)
			case <-timer.C:
				state.recordConnectionDiagnostic(
					"relay_heartbeat_timeout", "heartbeat_timeout", connection.epoch, 0, 0, heartbeat,
				)
				reportFatal(errRelayHeartbeatTimeout)
				return
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(max(time.Second, heartbeat/2))
		defer ticker.Stop()
		for {
			select {
			case <-pingContext.Done():
				return
			case now := <-ticker.C:
				sequence := connection.sequence.Add(1)
				lastPingSequence.Store(sequence)
				if err := connection.write(pingContext, &remotev1.Envelope{
					ProtocolVersion: 1, ConnectionEpoch: connection.epoch, Sequence: sequence,
					Frame: &remotev1.Envelope_Ping{Ping: &remotev1.Ping{MonotonicMillis: uint64(now.UnixMilli())}},
				}); err != nil {
					reportFatal(err)
					return
				}
			}
		}
	}()
	baseDispatch := dispatcher{
		state: state, controlStore: state.controlStore, controlLoop: state.controlLoop, tasks: newEncryptedControlTaskRepository(state.controlStore),
		now: func() time.Time { return time.Now().UTC() },
	}
	for {
		drainSessionFailures()
		envelope, err := readTargetEnvelope(serveContext, connection.socket)
		if err != nil {
			select {
			case fatal := <-fatalErrors:
				return fatal
			default:
			}
			return err
		}
		drainSessionFailures()
		if envelope.GetProtocolVersion() != 1 || envelope.GetConnectionEpoch() != connection.epoch {
			return errors.New("Relay frame epoch is invalid")
		}
		if envelope.GetPong() != nil {
			sequence := envelope.GetSequence()
			for sequence > lastPongSequence.Load() && sequence <= lastPingSequence.Load() {
				previous := lastPongSequence.Load()
				if sequence <= previous || !lastPongSequence.CompareAndSwap(previous, sequence) {
					continue
				}
				select {
				case pongObserved <- struct{}{}:
				default:
				}
				break
			}
			continue
		}
		if envelope.GetGoAway() != nil {
			state.recordConnectionDiagnostic(
				"relay_goaway", "relay_requested_reconnect", connection.epoch, 0, 0, heartbeat,
			)
			return errors.New("Relay requested reconnect")
		}
		if open := envelope.GetPeerOpen(); open != nil {
			session, ready, err := acceptPeerOpen(open, state, peerVerifier)
			if err != nil {
				if rejectErr := sendPeerError(serveContext, connection, open.GetSessionId(), "", remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, false); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
				continue
			}
			session.state = state
			if _, duplicate := peerSessions[session.claims.SessionID]; duplicate {
				if rejectErr := sendPeerError(serveContext, connection, session.claims.SessionID, "", remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, false); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
				continue
			}
			if len(peerSessions) >= maximumTargetPeerSessions {
				if rejectErr := sendPeerError(serveContext, connection, session.claims.SessionID, "", remotev1.ErrorCode_ERROR_CODE_RATE_LIMITED, true); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
				continue
			}
			sessionDispatch := baseDispatch
			sessionDispatch.scope = session.claims.Scopes[0]
			sessionDispatch.ticketProjectID = session.claims.ProjectID
			sessionDispatch.enforceProjectBinding = true
			retireFromWorker := func() {
				if !session.closing.CompareAndSwap(false, true) {
					return
				}
				// A write/seal failure can occur while the socket reader is idle.
				// Stop the actor immediately instead of waiting for the next inbound
				// frame to drain sessionFailures; otherwise its expiry timer and
				// mailbox goroutine could survive for the rest of the ticket lease.
				session.stop()
				select {
				case sessionFailures <- targetPeerSessionFailure{sessionID: session.claims.SessionID}:
				default:
				}
			}
			session.executor = newLivePeerRPCExecutor(serveContext, session.expiresAt, sessionDispatch.dispatchStream, sessionDispatch.dispatchLive,
				func(query *remotev1.PeerCiphertext, response *remotev1.RpcEnvelope, events []*remotev1.RpcEnvelope) error {
					return writePeerRPCBatch(serveContext, connection, session, query, response, events)
				}, func(query *remotev1.PeerCiphertext, event *remotev1.RpcEnvelope) error {
					session.sendMu.Lock()
					defer session.sendMu.Unlock()
					err := writePeerRPCLocked(serveContext, connection, session, query, "PEER_DELTA", event)
					if err != nil && !session.closing.Load() {
						if isRelayConnectionFault(err) {
							reportFatal(err)
						} else {
							// A seal/response-local error has already consumed this
							// session's sequence. Mark only this session terminal; the
							// read loop will retire and reject it on its next frame.
							retireFromWorker()
						}
					}
					return err
				}, func(err error) {
					if isRelayConnectionFault(err) {
						reportFatal(err)
						return
					}
					retireFromWorker()
				})
			session.executor.setConnectionLimiter(connectionLimiter)
			peerSessions[session.claims.SessionID] = session
			if err := connection.write(serveContext, &remotev1.Envelope{
				ProtocolVersion: 1, ConnectionEpoch: connection.epoch,
				Frame: &remotev1.Envelope_PeerReady{PeerReady: ready},
			}); err != nil {
				retireTargetPeerSession(peerSessions, session.claims.SessionID)
				if isRelayConnectionFault(err) {
					return err
				}
				continue
			}
			startTargetPeerSessionActor(serveContext, connection, session, sessionFailures, reportFatal)
			continue
		}
		if query := envelope.GetPeerQuery(); query != nil {
			session := peerSessions[query.GetSessionId()]
			if session == nil {
				if rejectErr := sendPeerError(serveContext, connection, query.GetSessionId(), query.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
				continue
			}
			if session.closing.Load() || targetPeerSessionExpired(session, time.Now().UTC()) {
				retireTargetPeerSession(peerSessions, query.GetSessionId())
				if rejectErr := sendPeerError(serveContext, connection, query.GetSessionId(), query.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_TIMEOUT, true); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
				continue
			}
			if !session.enqueueInbound(targetPeerInboundFrame{query: query}) {
				retireTargetPeerSession(peerSessions, query.GetSessionId())
				if rejectErr := sendPeerError(serveContext, connection, query.GetSessionId(), query.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
			}
			continue
		}
		if cancel := envelope.GetPeerCancel(); cancel != nil {
			session := peerSessions[cancel.GetSessionId()]
			if session == nil {
				if rejectErr := sendPeerError(serveContext, connection, cancel.GetSessionId(), cancel.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
				continue
			}
			if session.closing.Load() || targetPeerSessionExpired(session, time.Now().UTC()) {
				retireTargetPeerSession(peerSessions, cancel.GetSessionId())
				if rejectErr := sendPeerError(serveContext, connection, cancel.GetSessionId(), cancel.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_TIMEOUT, true); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
				continue
			}
			if !session.enqueueInbound(targetPeerInboundFrame{cancel: cancel}) {
				retireTargetPeerSession(peerSessions, cancel.GetSessionId())
				if rejectErr := sendPeerError(serveContext, connection, cancel.GetSessionId(), cancel.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true); isRelayConnectionFault(rejectErr) {
					return rejectErr
				}
			}
			continue
		}
		if peerError := envelope.GetPeerError(); peerError != nil {
			// A PEER_ERROR without a query id is the controller's logical-session
			// close. Relay has already removed its route; release the Agent-side
			// executor as well so an LRU session pool cannot accumulate idle
			// sessions on a long-lived WebSocket.
			if peerError.GetQueryId() == "" {
				retireTargetPeerSession(peerSessions, peerError.GetSessionId())
			}
			continue
		}
		// A syntactically valid but currently unsupported Relay frame carries no
		// evidence that the physical socket is broken. Keep the carrier alive;
		// connection-level parser, epoch, I/O, heartbeat and GoAway failures are
		// handled explicitly above.
		continue
	}
}

func targetRelayHeartbeatTimeout(heartbeat time.Duration) time.Duration {
	if heartbeat < time.Second {
		heartbeat = 25 * time.Second
	}
	return max(15*time.Second, 2*heartbeat+3*time.Second)
}

// remoteConnectionDiagnosticReason classifies expected lifecycle failures
// without forwarding arbitrary network, TLS, server, or WebSocket error text
// into logs. More granular protocol violations use the existing protocol
// diagnostics sink.
func remoteConnectionDiagnosticReason(err error) string {
	switch {
	case err == nil:
		return "completed"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, errDeviceAuthentication):
		return "authentication_failed"
	case errors.Is(err, errRelayHeartbeatTimeout):
		return "heartbeat_timeout"
	case errors.Is(err, errRelaySocketWrite):
		return "socket_write_failed"
	}
	var controlFailure *controlHTTPError
	if errors.As(err, &controlFailure) {
		// Problem codes are parsed from an authenticated Control Plane response
		// and syntax-bounded by safeControlCode. Keep this allowlist narrow so a
		// remote server cannot create arbitrary high-cardinality log reasons.
		switch controlFailure.Code {
		case "relay_unavailable", "remote_unavailable", "auth_unavailable",
			"relay_allocation_invalid", "remote_device_forbidden", "remote_device_inactive",
			"connection_epoch_stale", "idempotency_conflict":
			return controlFailure.Code
		}
		switch {
		case controlFailure.Status == http.StatusTooManyRequests:
			return "control_rate_limited"
		case controlFailure.Status >= http.StatusInternalServerError:
			return "control_unavailable"
		default:
			return "control_request_rejected"
		}
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "Relay requested reconnect"):
		return "relay_requested_reconnect"
	case strings.Contains(message, "Relay frame epoch"):
		return "relay_epoch_invalid"
	case strings.Contains(message, "Relay WebSocket connection"):
		return "websocket_connect_failed"
	case strings.Contains(message, "Relay challenge"):
		return "relay_challenge_invalid"
	case strings.Contains(message, "Relay Ready"):
		return "relay_ready_invalid"
	case strings.Contains(message, "Relay ticket"):
		return "relay_ticket_invalid"
	case strings.Contains(message, "Relay endpoint"):
		return "relay_endpoint_invalid"
	case strings.Contains(message, "Relay allocation"):
		return "relay_allocation_invalid"
	case strings.Contains(message, "Peer Ticket trust"):
		return "peer_ticket_trust_invalid"
	default:
		return "transport_error"
	}
}

func retireTargetPeerSession(sessions map[string]*targetPeerSession, sessionID string) *targetPeerSession {
	if sessions == nil {
		return nil
	}
	session := sessions[sessionID]
	if session == nil {
		return nil
	}
	delete(sessions, sessionID)
	session.stop()
	return session
}

func targetPeerSessionExpired(session *targetPeerSession, now time.Time) bool {
	return session == nil || (!session.expiresAt.IsZero() && !session.expiresAt.After(now))
}

func (session *targetPeerSession) enqueueInbound(frame targetPeerInboundFrame) bool {
	if session == nil || session.closing.Load() || session.inbound == nil {
		return false
	}
	select {
	case session.inbound <- frame:
		return true
	default:
		return false
	}
}

func (session *targetPeerSession) stop() {
	if session == nil {
		return
	}
	session.closing.Store(true)
	if session.actorCancel != nil {
		session.actorCancel()
	}
	if session.executor != nil {
		session.executor.stop()
		go session.executor.close()
	}
}

// startTargetPeerSessionActor serialises incoming ciphertext for one logical
// session while leaving other sessions and the physical WebSocket reader free
// to make progress. Its bounded mailbox is also the slow-consumer boundary:
// an overflow closes only this session, never the shared Relay connection.
func startTargetPeerSessionActor(ctx context.Context, connection *relayConnection, session *targetPeerSession, failures chan<- targetPeerSessionFailure, reportFatal func(error)) {
	if session == nil || session.executor == nil || session.inbound != nil {
		return
	}
	actorContext, cancel := context.WithCancel(ctx)
	session.inbound = make(chan targetPeerInboundFrame, maximumTargetPeerSessionInbound)
	session.actorCancel = cancel
	session.actorDone = make(chan struct{})
	go func() {
		defer close(session.actorDone)
		var expiry <-chan time.Time
		var timer *time.Timer
		if !session.expiresAt.IsZero() {
			remaining := time.Until(session.expiresAt)
			if remaining < 0 {
				remaining = 0
			}
			timer = time.NewTimer(remaining)
			expiry = timer.C
			defer timer.Stop()
		}
		failSession := func(queryID string, code remotev1.ErrorCode, retryable bool) {
			if !session.closing.CompareAndSwap(false, true) {
				return
			}
			session.executor.stop()
			if err := sendPeerError(actorContext, connection, session.claims.SessionID, queryID, code, retryable); isRelayConnectionFault(err) && reportFatal != nil {
				reportFatal(err)
			}
			select {
			case failures <- targetPeerSessionFailure{sessionID: session.claims.SessionID}:
			default:
			}
		}
		for {
			select {
			case <-actorContext.Done():
				return
			case <-expiry:
				failSession("", remotev1.ErrorCode_ERROR_CODE_PEER_TIMEOUT, true)
				return
			case frame := <-session.inbound:
				if session.closing.Load() {
					return
				}
				if targetPeerSessionExpired(session, time.Now().UTC()) {
					failSession("", remotev1.ErrorCode_ERROR_CODE_PEER_TIMEOUT, true)
					return
				}
				if frame.query != nil {
					if err := handlePeerRPC(session, frame.query); err != nil {
						if errors.Is(err, errPeerSessionProtocol) {
							failSession("", remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, false)
						} else if isRelayConnectionFault(err) {
							if reportFatal != nil {
								reportFatal(err)
							}
							failSession(frame.query.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true)
						} else {
							failSession(frame.query.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true)
						}
						return
					}
					continue
				}
				if frame.cancel != nil {
					if err := handlePeerCancel(session, frame.cancel); err != nil {
						if errors.Is(err, errPeerSessionProtocol) {
							failSession("", remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, false)
						} else if isRelayConnectionFault(err) && reportFatal != nil {
							reportFatal(err)
							failSession(frame.cancel.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true)
						} else {
							failSession(frame.cancel.GetQueryId(), remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true)
						}
						return
					}
				}
			}
		}
	}()
}

// sendPeerError deliberately does not require an active targetPeerSession.
// Open rejection and an unknown/expired session are still routable to the
// controller by the session id carried on the inbound frame.
func sendPeerError(ctx context.Context, connection *relayConnection, sessionID, queryID string, code remotev1.ErrorCode, retryable bool) error {
	if connection == nil {
		return errors.New("Peer error connection is unavailable")
	}
	return connection.write(ctx, &remotev1.Envelope{
		ProtocolVersion: 1,
		ConnectionEpoch: connection.epoch,
		Frame: &remotev1.Envelope_PeerError{PeerError: &remotev1.PeerError{
			SessionId: sessionID,
			QueryId:   queryID,
			Code:      code,
			Retryable: retryable,
		}},
	})
}

func rejectTargetPeerSession(ctx context.Context, connection *relayConnection, sessionID string) error {
	return sendPeerError(ctx, connection, sessionID, "", remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, false)
}

func acceptPeerOpen(open *remotev1.PeerOpen, state *agentState, verifier remoteauth.Verifier) (*targetPeerSession, *remotev1.PeerReady, error) {
	if open == nil || uuid.Validate(open.GetSessionId()) != nil || len(open.GetEphemeralPublicKey()) != 32 || len(open.GetIdentitySignature()) != ed25519.SignatureSize {
		return nil, nil, errors.New("PEER_OPEN is invalid")
	}
	claims, err := verifier.Verify(open.GetSessionTicket(), "relay-peer", time.Now().UTC())
	if err != nil || claims.Audience != "relay-peer" || claims.SessionID != open.GetSessionId() || len(claims.Scopes) != 1 ||
		claims.Subject != claims.SourceDeviceID || claims.Confirmation != claims.SourceKeyThumbprint ||
		claims.TargetDeviceID != state.DeviceID.String() || claims.TargetKeyVersion != state.KeyVersion ||
		claims.MaxDurationSeconds == 0 || time.Duration(claims.MaxDurationSeconds)*time.Second > maximumTargetPeerSessionDuration || claims.MaxBytes == 0 || claims.MaxBytes > 16<<20 ||
		claims.ExpiresAt-claims.IssuedAt > int64(maximumTargetPeerSessionDuration/time.Second) || time.Unix(claims.ExpiresAt, 0).Before(time.Now().UTC()) {
		return nil, nil, errors.New("Peer ticket claims are invalid")
	}
	projectRequired := agentPeerScopeRequiresProject(claims.Scopes[0])
	if (projectRequired && claims.ProjectID == "") || (!projectRequired && claims.ProjectID != "") ||
		(claims.ProjectID != "" && (uuid.Validate(claims.ProjectID) != nil || claims.ProjectID != strings.ToLower(claims.ProjectID) || state.business == nil)) {
		return nil, nil, errors.New("Peer ticket project binding is invalid")
	}
	if claims.ProjectID != "" {
		projectID, _ := uuid.Parse(claims.ProjectID)
		project, projectErr := state.business.projectByID(context.Background(), projectID)
		if projectErr != nil || project.State != "available" {
			return nil, nil, errors.New("Peer ticket project is unavailable")
		}
	}
	targetKey, err := remoteauth.DecodeIdentityPublicKey(claims.TargetIdentityKey, claims.TargetKeyThumbprint)
	if err != nil || !targetKey.Equal(state.identity.Public().(ed25519.PublicKey)) {
		return nil, nil, errors.New("Peer ticket target identity is invalid")
	}
	sourceKey, err := remoteauth.DecodeIdentityPublicKey(claims.SourceIdentityKey, claims.SourceKeyThumbprint)
	if err != nil || claims.ValidatePeer(claims.SourceDeviceID, claims.TargetDeviceID, claims.SourceKeyThumbprint, claims.TargetKeyThumbprint, claims.Scopes[0], claims.SourceGrantVersion, claims.TargetGrantVersion) != nil {
		return nil, nil, errors.New("Peer ticket source identity is invalid")
	}
	if err := remoteauth.VerifyPeerOpenIdentity(sourceKey, claims.SourceKeyThumbprint, remoteauth.PeerOpenIdentityProof{
		TicketJWTID: claims.JWTID, SessionID: claims.SessionID, SourceDeviceID: claims.SourceDeviceID,
		TargetDeviceID: claims.TargetDeviceID, EphemeralPublicKey: open.GetEphemeralPublicKey(),
	}, open.GetIdentitySignature()); err != nil {
		return nil, nil, errors.New("PEER_OPEN proof is invalid")
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	readyProof := remoteauth.PeerReadyIdentityProof{
		TicketJWTID: claims.JWTID, SessionID: claims.SessionID, SourceDeviceID: claims.SourceDeviceID, TargetDeviceID: claims.TargetDeviceID,
		SourceEphemeralPublicKey: open.GetEphemeralPublicKey(), TargetEphemeralPublicKey: ephemeral.PublicKey().Bytes(),
	}
	signature, err := remoteauth.SignPeerReadyIdentity(state.identity, readyProof)
	if err != nil {
		return nil, nil, err
	}
	secret, err := peerprotocol.X25519SharedSecret(ephemeral, open.GetEphemeralPublicKey())
	if err != nil {
		return nil, nil, err
	}
	keys, err := peerprotocol.DeriveSessionKeys(secret, claims.JWTID, claims.SessionID, claims.SourceDeviceID, claims.TargetDeviceID)
	if err != nil {
		return nil, nil, err
	}
	// The ticket has been verified for this one-time PEER_OPEN. Keeping an
	// established cipher alive must not depend on refreshing that ticket with
	// Host; route/connection loss, explicit close, and protocol faults still
	// retire the session immediately.
	return &targetPeerSession{claims: claims, keys: keys}, &remotev1.PeerReady{
		SessionId: claims.SessionID, EphemeralPublicKey: ephemeral.PublicKey().Bytes(), IdentitySignature: signature,
	}, nil
}

func agentPeerScopeRequiresProject(scope string) bool {
	switch scope {
	case "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.terminal.interactive", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.task.control", "remote.peer.ai.tools", "remote.peer.events":
		return true
	default:
		return false
	}
}

func handlePeerRPC(session *targetPeerSession, query *remotev1.PeerCiphertext) error {
	if session == nil || session.executor == nil || query == nil || query.GetSessionId() != session.claims.SessionID ||
		uuid.Validate(query.GetQueryId()) != nil || query.GetGeneration() == 0 || query.GetMessageSequence() == 0 ||
		query.GetDeadline() == nil || query.GetDeadline().CheckValid() != nil || !query.GetDeadline().AsTime().After(time.Now().UTC()) {
		recordPeerProtocolDiagnostic(session, "peerBinding", "peer_binding_invalid", "session", query, peerCiphertextSize(query))
		return errPeerSessionProtocol
	}
	if session.opener == nil {
		var err error
		session.generation = query.GetGeneration()
		session.opener, err = peerprotocol.NewCipherState(session.keys.SourceToTarget, peerprotocol.DirectionSourceToTarget, peerprotocol.CipherModeOpen, session.generation)
		if err != nil {
			return peerSessionProtocolError(err)
		}
		session.sealer, err = peerprotocol.NewCipherState(session.keys.TargetToSource, peerprotocol.DirectionTargetToSource, peerprotocol.CipherModeSeal, session.generation)
		if err != nil {
			return peerSessionProtocolError(err)
		}
	} else if query.GetGeneration() != session.generation {
		return errPeerSessionProtocol
	}
	metadata := peerprotocol.CiphertextMetadata{
		FrameType: "PEER_QUERY", SessionID: query.GetSessionId(), QueryID: query.GetQueryId(), Generation: query.GetGeneration(),
		MessageSequence: query.GetMessageSequence(), Deadline: query.GetDeadline().AsTime(), Direction: peerprotocol.DirectionSourceToTarget,
	}
	plaintext, err := session.opener.OpenNext(query.GetCiphertext(), metadata)
	if err != nil {
		recordPeerProtocolDiagnostic(session, "peerCiphertext", peerCiphertextFailureReason(err), "session", query, len(query.GetCiphertext()))
		return peerSessionProtocolError(err)
	}
	request := new(remotev1.RpcEnvelope)
	if proto.Unmarshal(plaintext, request) != nil {
		recordPeerProtocolDiagnostic(session, "rpcEnvelope", "rpc_envelope_invalid", "channel", query, len(plaintext))
		return errPeerSessionProtocol
	}
	return session.executor.submit(query, request)
}

func handlePeerCancel(session *targetPeerSession, cancel *remotev1.PeerCiphertext) error {
	if session == nil || session.executor == nil || session.opener == nil || cancel.GetGeneration() != session.generation || cancel.GetMessageSequence() == 0 ||
		cancel.GetSessionId() != session.claims.SessionID || uuid.Validate(cancel.GetQueryId()) != nil {
		recordPeerProtocolDiagnostic(session, "peerBinding", "peer_binding_invalid", "session", cancel, peerCiphertextSize(cancel))
		return errPeerSessionProtocol
	}
	metadata := peerprotocol.CiphertextMetadata{
		FrameType: "PEER_CANCEL", SessionID: cancel.GetSessionId(), QueryID: cancel.GetQueryId(), Generation: cancel.GetGeneration(),
		MessageSequence: cancel.GetMessageSequence(), Direction: peerprotocol.DirectionSourceToTarget,
	}
	plaintext, err := session.opener.OpenNext(cancel.GetCiphertext(), metadata)
	if err != nil {
		recordPeerProtocolDiagnostic(session, "peerCiphertext", peerCiphertextFailureReason(err), "session", cancel, len(cancel.GetCiphertext()))
		return peerSessionProtocolError(err)
	}
	request := new(remotev1.RpcEnvelope)
	var input map[string]any
	if proto.Unmarshal(plaintext, request) != nil ||
		request.GetProtocolVersion() != 1 || request.GetRequest() == nil || request.GetRequest().GetHeader() == nil ||
		request.GetRequest().GetMethod() != "rpc.cancel" || request.GetRequest().GetHeader().GetRequestId() != cancel.GetQueryId() ||
		!validPeerIdempotencyKey(request.GetRequest().GetHeader().GetIdempotencyKey()) || request.GetRequest().GetHeader().ExpectedRevision != nil ||
		request.GetRequest().GetHeader().GetDeadline() == nil || request.GetRequest().GetHeader().GetDeadline().CheckValid() != nil ||
		!request.GetRequest().GetHeader().GetDeadline().AsTime().After(time.Now().UTC()) ||
		json.Unmarshal(request.GetRequest().GetJsonPayload(), &input) != nil || len(input) != 0 {
		recordPeerProtocolDiagnostic(session, "rpcEnvelope", "rpc_envelope_invalid", "channel", cancel, len(plaintext))
		return errPeerSessionProtocol
	}
	session.executor.cancelQuery(cancel.GetQueryId())
	return nil
}

func writePeerRPCBatch(ctx context.Context, connection *relayConnection, session *targetPeerSession, query *remotev1.PeerCiphertext, response *remotev1.RpcEnvelope, events []*remotev1.RpcEnvelope) error {
	if session == nil || response == nil {
		return errors.New("Peer response is invalid")
	}
	session.sendMu.Lock()
	defer session.sendMu.Unlock()
	if session.closing.Load() {
		return context.Canceled
	}
	for _, event := range events {
		if err := writePeerRPCLocked(ctx, connection, session, query, "PEER_DELTA", event); err != nil {
			return err
		}
	}
	return writePeerRPCLocked(ctx, connection, session, query, "PEER_COMPLETE", response)
}

func writePeerRPCLocked(ctx context.Context, connection *relayConnection, session *targetPeerSession, query *remotev1.PeerCiphertext, frameType string, rpc *remotev1.RpcEnvelope) error {
	if session != nil && session.closing.Load() {
		return context.Canceled
	}
	envelope, err := sealPeerRPCLocked(connection.epoch, session, query, frameType, rpc)
	if err != nil {
		return err
	}
	return connection.write(ctx, envelope)
}

func sealPeerRPCLocked(connectionEpoch uint64, session *targetPeerSession, query *remotev1.PeerCiphertext, frameType string, rpc *remotev1.RpcEnvelope) (*remotev1.Envelope, error) {
	if session == nil || session.sealer == nil || query == nil || rpc == nil {
		return nil, errors.New("Peer response is invalid")
	}
	// Dispatch normally replaces an oversized result with the compact
	// RPC_PAYLOAD_TOO_LARGE response. Keep the same JSON boundary immediately
	// before encryption so a future response/event path cannot consume a cipher
	// sequence with data that the clients are required to reject.
	var jsonPayload []byte
	switch {
	case rpc.GetRequest() != nil:
		jsonPayload = rpc.GetRequest().GetJsonPayload()
	case rpc.GetResponse() != nil:
		jsonPayload = rpc.GetResponse().GetJsonPayload()
	case rpc.GetEvent() != nil:
		jsonPayload = rpc.GetEvent().GetJsonPayload()
	}
	if len(jsonPayload) > maximumRPCPayload {
		recordPeerProtocolDiagnostic(session, "rpcJson", "rpc_json_too_large", "operation", query, len(jsonPayload))
		return nil, fmt.Errorf("rpc_json_too_large: %d > %d", len(jsonPayload), maximumRPCPayload)
	}
	responseBytes, err := proto.Marshal(rpc)
	if err != nil {
		return nil, err
	}
	if !peerRPCPlaintextWithinLimit(responseBytes) {
		recordPeerProtocolDiagnostic(session, "rpcEnvelope", "rpc_envelope_too_large", "operation", query, len(responseBytes))
		return nil, fmt.Errorf("rpc_plaintext_too_large: %d > %d", len(responseBytes), maximumPeerRPCPlaintext)
	}
	generation, sequence, exhausted := session.sealer.NextSequence()
	if exhausted {
		return nil, peerprotocol.ErrSequence
	}
	responseMetadata := peerprotocol.CiphertextMetadata{
		FrameType: frameType, SessionID: query.GetSessionId(), QueryID: query.GetQueryId(), Generation: generation,
		MessageSequence: sequence, Direction: peerprotocol.DirectionTargetToSource,
	}
	ciphertext, err := session.sealer.SealNext(responseBytes, responseMetadata)
	if err != nil {
		return nil, err
	}
	peerCiphertext := &remotev1.PeerCiphertext{
		SessionId: query.GetSessionId(), QueryId: query.GetQueryId(), Generation: generation, MessageSequence: sequence, Ciphertext: ciphertext,
	}
	envelope := &remotev1.Envelope{ProtocolVersion: 1, ConnectionEpoch: connectionEpoch}
	switch frameType {
	case "PEER_DELTA":
		envelope.Frame = &remotev1.Envelope_PeerDelta{PeerDelta: peerCiphertext}
	case "PEER_COMPLETE":
		envelope.Frame = &remotev1.Envelope_PeerComplete{PeerComplete: peerCiphertext}
	default:
		return nil, errors.New("Peer response frame is invalid")
	}
	return envelope, nil
}

func peerRPCPlaintextWithinLimit(value []byte) bool {
	return len(value) > 0 && len(value) <= maximumPeerRPCPlaintext
}

func (connection *relayConnection) write(ctx context.Context, envelope *remotev1.Envelope) error {
	if connection == nil || connection.socket == nil {
		return errors.New("Relay connection is unavailable")
	}
	payload, err := proto.Marshal(envelope)
	if err != nil || len(payload) > relayprotocol.AbsoluteFrameLimit {
		return errors.New("Relay frame is invalid")
	}
	if connection.writer != nil {
		return connection.writer.enqueue(ctx, payload, relayWritePriorityForEnvelope(envelope))
	}
	// Test-only and pre-scheduler fallback. Production connections are created
	// through newRelayConnection and use the bounded priority writer above.
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	if err := connection.socket.Write(ctx, websocket.MessageBinary, payload); err != nil {
		return fmt.Errorf("%w: %v", errRelaySocketWrite, err)
	}
	return nil
}

func readTargetEnvelope(ctx context.Context, socket *websocket.Conn) (*remotev1.Envelope, error) {
	messageType, payload, err := socket.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageBinary || len(payload) > relayprotocol.AbsoluteFrameLimit {
		return nil, errors.New("Relay frame is invalid")
	}
	envelope := new(remotev1.Envelope)
	if proto.Unmarshal(payload, envelope) != nil {
		return nil, errors.New("Relay protobuf is invalid")
	}
	return envelope, nil
}

func recordPeerProtocolDiagnostic(session *targetPeerSession, stage, reason, faultLevel string, query *remotev1.PeerCiphertext, payloadBytes int) {
	if session == nil || session.state == nil {
		return
	}
	requestID, sessionID := "", session.claims.SessionID
	if query != nil {
		requestID = query.GetQueryId()
		if query.GetSessionId() != "" {
			sessionID = query.GetSessionId()
		}
	}
	scope := ""
	if len(session.claims.Scopes) == 1 {
		scope = session.claims.Scopes[0]
	}
	session.state.recordProtocolDiagnostic(stage, reason, faultLevel, "inbound", "", scope, payloadBytes, requestID, sessionID)
}

func peerCiphertextFailureReason(err error) string {
	switch {
	case errors.Is(err, peerprotocol.ErrAuthentication):
		return "peer_ciphertext_auth_failed"
	case errors.Is(err, peerprotocol.ErrSequence):
		return "peer_nonce_invalid"
	default:
		return "peer_ciphertext_size_invalid"
	}
}

func peerCiphertextSize(value *remotev1.PeerCiphertext) int {
	if value == nil {
		return 0
	}
	return len(value.GetCiphertext())
}

func parseSignedClaims(token string) (remoteauth.Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 16<<10 {
		return remoteauth.Claims{}, errors.New("ticket is malformed")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(payload) > 12<<10 {
		return remoteauth.Claims{}, errors.New("ticket is malformed")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var claims remoteauth.Claims
	if decoder.Decode(&claims) != nil || claims.JWTID == "" {
		return remoteauth.Claims{}, errors.New("ticket claims are invalid")
	}
	return claims, nil
}

func targetHTTPClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		contents, err := os.ReadFile(caFile)
		if err != nil || !pool.AppendCertsFromPEM(contents) {
			return nil, errors.New("TLS CA file is invalid")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func validateTargetControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("control URL is invalid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("control URL must use HTTP or HTTPS")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func targetEndpoint(base *url.URL, path string) string {
	copy := *base
	copy.Path = strings.TrimRight(base.Path, "/") + path
	copy.RawPath, copy.RawQuery, copy.Fragment = "", "", ""
	return copy.String()
}

func targetJSON(ctx context.Context, client *http.Client, method, target, authorization, idempotency string, requestBody, destination any) error {
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, application/problem+json")
	request.Header.Set("Authorization", authorization)
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &problem)
		if problem.Code == "" {
			problem.Code = "request_rejected"
		}
		return fmt.Errorf("request rejected (%s)", problem.Code)
	}
	if json.Unmarshal(body, destination) != nil {
		return errors.New("response JSON is invalid")
	}
	return nil
}

func containsField(fields, wanted string) bool {
	for _, field := range strings.Fields(fields) {
		if field == wanted {
			return true
		}
	}
	return false
}
