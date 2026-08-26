package relayserver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	Subprotocol                 = "wenzwork-relay.v1"
	peerTicketSubprotocolPrefix = "wenzwork-peer-ticket."
	maximumTicketBytes          = 16 << 10
)

type TicketVerifier interface {
	Verify(token, expectedAudience string, now time.Time) (remoteauth.Claims, error)
}

type DeviceKeyResolver interface {
	ResolveDeviceKey(context.Context, string, string) (ed25519.PublicKey, error)
}

type RouteRegistry interface {
	Register(relayrouter.Route, time.Duration, time.Time) error
	Renew(string, string, uint64, time.Duration, time.Time) error
	CompareAndDelete(string, string, uint64) bool
}

type Handler struct {
	CellID string
	NodeID string
	// BrowserOriginPatterns lists the management UI origins allowed to open a
	// direct peer WebSocket. Relay credentials are never ambient browser
	// credentials, but retaining an explicit allow-list prevents other sites
	// from initiating a browser connection to the Relay.
	BrowserOriginPatterns []string
	browserOriginMu       sync.RWMutex
	Verifier              TicketVerifier
	PeerVerifier          TicketVerifier
	// DeviceKeys, PeerDevices and Routes are legacy embedding hooks. Production
	// admission uses Host-signed self-contained tickets and reports resident
	// routes through the authenticated Relay heartbeat.
	DeviceKeys         DeviceKeyResolver
	PeerDevices        PeerDeviceStateResolver
	Routes             RouteRegistry
	Connections        *ConnectionManager
	Peers              *PeerForwarder
	Files              *FileForwarder
	Now                func() time.Time
	Random             io.Reader
	ChallengeTTL       time.Duration
	RouteTTL           time.Duration
	HeartbeatSeconds   uint32
	MaxFramesPerSecond int
	MaxBytesPerSecond  int
	// ProtocolFailure receives content-free structured diagnostics. It must not
	// receive tickets, frame bodies, URLs, device IDs, or raw parser errors.
	ProtocolFailure func(RelayProtocolFailure)
	// ConnectionLifecycle receives content-free carrier lifecycle events. It is
	// intentionally separate from ProtocolFailure so normal open/close activity
	// can be logged without being mistaken for a protocol fault.
	ConnectionLifecycle func(RelayConnectionLifecycle)
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handshakeStarted := time.Now()
	if request.Method != http.MethodGet || request.URL.Path != "/v1/connect" {
		http.NotFound(writer, request)
		return
	}
	if request.URL.RawQuery != "" {
		writer.Header().Set("X-Wenzwork-Relay-Reason", relayReasonAuthorizationRequired)
		http.Error(writer, "tickets are not accepted in the URL", http.StatusBadRequest)
		return
	}
	if h.Verifier == nil || h.Connections == nil || h.CellID == "" || h.NodeID == "" {
		http.Error(writer, "relay unavailable", http.StatusServiceUnavailable)
		return
	}
	if !h.Connections.Accepting() {
		writer.Header().Set("Retry-After", "5")
		http.Error(writer, "relay draining", http.StatusServiceUnavailable)
		return
	}
	if err := h.Connections.AcquireHandshake(request.Context()); err != nil {
		writer.Header().Set("Retry-After", "1")
		http.Error(writer, "relay overloaded", http.StatusServiceUnavailable)
		return
	}
	handshakeHeld := true
	handshakeSucceeded := false
	defer func() {
		if handshakeHeld {
			h.Connections.ReleaseHandshake()
		}
		if !handshakeSucceeded {
			h.Connections.RecordHandshake(false)
		}
	}()
	token, directPeer, ok := requestAuthorization(request)
	if !ok {
		reason := relayReasonAuthorizationRequired
		if len(request.Header.Values("Sec-WebSocket-Protocol")) > 0 {
			reason = relayReasonSubprotocolMismatch
		}
		writer.Header().Set("X-Wenzwork-Relay-Reason", reason)
		h.recordProtocolFailure("relayHandshake", reason, "connection", "unauthenticated", 0, 0, "")
		http.Error(writer, "relay authorization required", http.StatusUnauthorized)
		return
	}
	now := h.now()
	var claims remoteauth.Claims
	var publicKey ed25519.PublicKey
	var err error
	endpointID := ""
	if directPeer {
		if h.PeerVerifier == nil || h.Peers == nil {
			http.Error(writer, "direct Peer access unavailable", http.StatusServiceUnavailable)
			return
		}
		claims, err = h.PeerVerifier.Verify(token, "relay-peer", now)
		if err == nil && uuid.Validate(claims.SessionID) == nil && uuid.Validate(claims.SourceDeviceID) == nil &&
			uuid.Validate(claims.TargetDeviceID) == nil && claims.SourceDeviceID != claims.TargetDeviceID &&
			claims.ValidatePeerRelay(h.NodeID, h.CellID, claims.TargetConnectionEpoch) == nil &&
			h.Connections.HasResident(claims.TargetDeviceID, claims.TargetConnectionEpoch) {
			claimedKey, claimErr := remoteauth.DecodeIdentityPublicKey(claims.SourceIdentityKey, claims.SourceKeyThumbprint)
			if claimErr != nil || claims.SourceKeyVersion == 0 || claims.SourceGrantVersion == 0 {
				err = remoteauth.ErrTicketClaims
			} else if claims.SourceCredentialType == "controller" {
				if claims.ExpiresAt-claims.IssuedAt > int64(maxPeerTicketLifetime/time.Second) {
					err = remoteauth.ErrTicketClaims
				} else {
					publicKey = claimedKey
				}
			} else if claims.SourceCredentialType == "device" {
				if claims.ExpiresAt-claims.IssuedAt > int64(maxPeerTicketLifetime/time.Second) {
					err = remoteauth.ErrTicketClaims
				} else {
					publicKey = claimedKey
				}
			} else {
				err = remoteauth.ErrTicketClaims
			}
		} else if err == nil {
			err = remoteauth.ErrTicketClaims
		}
		if err != nil || len(publicKey) != ed25519.PublicKeySize {
			http.Error(writer, "direct Peer ticket invalid", http.StatusForbidden)
			return
		}
		endpointID = "peer:" + claims.SessionID
	} else {
		claims, err = h.Verifier.Verify(token, "relay", now)
		if err != nil {
			http.Error(writer, "relay ticket invalid", http.StatusUnauthorized)
			return
		}
		publicKey, err = remoteauth.DecodeIdentityPublicKey(claims.IdentityKey, claims.Confirmation)
		validationClaims := claims
		if err != nil && claims.IdentityKey == "" && h.DeviceKeys != nil {
			// Compatibility for old test harnesses during a rolling Host/Relay
			// upgrade. New Host tickets always carry identity_key.
			publicKey, err = h.DeviceKeys.ResolveDeviceKey(request.Context(), claims.Subject, claims.Confirmation)
			if err == nil {
				validationClaims.IdentityKey = base64.RawURLEncoding.EncodeToString(publicKey)
			}
		}
		if err != nil || validationClaims.ValidateConnection(claims.Subject, claims.UserID, h.CellID, remoteauth.PublicKeyThumbprint(publicKey), claims.AssignmentVersion, claims.GrantVersion, 1) != nil {
			http.Error(writer, "relay ticket claims invalid", http.StatusForbidden)
			return
		}
		endpointID = claims.Subject
	}

	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols: []string{Subprotocol}, OriginPatterns: h.browserOrigins(),
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	if connection.Subprotocol() != Subprotocol {
		h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonSubprotocolMismatch, "relayHandshake", "connection", roleForRelayConnection(directPeer), 0, 0, endpointID)
		return
	}
	connection.SetReadLimit(relayprotocol.AbsoluteFrameLimit)

	challengeTTL := h.ChallengeTTL
	if challengeTTL <= 0 {
		challengeTTL = 10 * time.Second
	}
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(h.random(), nonce); err != nil {
		h.closeForProtocolFailure(connection, websocket.StatusInternalError, relayReasonBackendUnavailable, "relayHandshake", "connection", roleForRelayConnection(directPeer), 1, 0, endpointID)
		return
	}
	deadline := now.Add(challengeTTL).UTC()
	// The injected clock defines signed protocol timestamps, while the context
	// timeout must use Go's real monotonic clock. Coupling both makes a fixed
	// test clock (or a wall-clock correction) expire the handshake immediately.
	handshakeContext, cancel := context.WithTimeout(request.Context(), challengeTTL)
	defer cancel()
	if err := writeEnvelope(handshakeContext, connection, &remotev1.Envelope{
		ProtocolVersion: 1,
		Frame: &remotev1.Envelope_AuthChallenge{AuthChallenge: &remotev1.AuthChallenge{
			Nonce: nonce, RelayNodeId: h.NodeID, RelayCellId: h.CellID, Deadline: timestamppb.New(deadline),
		}},
	}); err != nil {
		return
	}

	proofEnvelope, err := readEnvelope(handshakeContext, connection)
	if err != nil {
		reason, protocolFailure := relayProtocolReason(err)
		if !protocolFailure {
			reason = relayReasonHandshakeFrameInvalid
		}
		h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, reason, "relayHandshake", "connection", roleForRelayConnection(directPeer), 0, 0, endpointID)
		return
	}
	if proofEnvelope.GetProtocolVersion() != 1 {
		h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonProtocolVersionInvalid, "relayHandshake", "connection", roleForRelayConnection(directPeer), proofEnvelope.GetProtocolVersion(), proto.Size(proofEnvelope), endpointID)
		return
	}
	if proofEnvelope.GetAuthProof() == nil {
		h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonHandshakeFrameInvalid, "relayHandshake", "connection", roleForRelayConnection(directPeer), 1, proto.Size(proofEnvelope), endpointID)
		return
	}
	proof := proofEnvelope.GetAuthProof()
	if proof.GetTicketJti() != claims.JWTID || proof.GetConnectionEpoch() == 0 || proofEnvelope.GetConnectionEpoch() != proof.GetConnectionEpoch() {
		h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonPeerBindingInvalid, "relayHandshake", "connection", roleForRelayConnection(directPeer), 1, proto.Size(proofEnvelope), endpointID)
		return
	}
	challenge := remoteauth.Challenge{
		Nonce: nonce, TicketJWTID: claims.JWTID, CellID: h.CellID, NodeID: h.NodeID,
		ConnectionEpoch: proof.GetConnectionEpoch(), Deadline: deadline,
	}
	if err := remoteauth.VerifyChallenge(publicKey, claims.Confirmation, challenge, proof.GetDeviceSignature(), h.now()); err != nil {
		h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonAuthenticationFailed, "relayHandshake", "connection", roleForRelayConnection(directPeer), 1, proto.Size(proofEnvelope), endpointID)
		return
	}
	connectionID := uuid.NewString()
	routeTTL := h.RouteTTL
	if routeTTL <= 0 {
		routeTTL = 60 * time.Second
	}
	if !directPeer && h.Routes != nil {
		if err := h.Routes.Register(relayrouter.Route{
			DeviceID: claims.Subject, UserID: claims.UserID, CellID: h.CellID, NodeID: h.NodeID,
			ConnectionID: connectionID, ConnectionEpoch: proof.GetConnectionEpoch(), AssignmentVersion: claims.AssignmentVersion,
			GrantVersion: claims.GrantVersion, ProtocolVersion: 1,
		}, routeTTL, h.now()); err != nil {
			h.Connections.RecordRouteRejection()
			h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonRouteUnavailable, "relayHandshake", "connection", roleForRelayConnection(directPeer), 1, 0, endpointID)
			return
		}
		defer h.Routes.CompareAndDelete(claims.Subject, connectionID, proof.GetConnectionEpoch())
	}
	session, err := h.Connections.AttachEndpoint(endpointID, claims.Subject, connectionID, proof.GetConnectionEpoch(), connection)
	if err != nil {
		h.closeForProtocolFailure(connection, websocket.StatusTryAgainLater, relayReasonBackendUnavailable, "relayHandshake", "connection", roleForRelayConnection(directPeer), 1, 0, endpointID)
		return
	}
	if !directPeer {
		if err := h.Connections.BindResidentRoute(endpointID, connectionID, proof.GetConnectionEpoch(), relayrouter.Route{
			DeviceID: claims.Subject, UserID: claims.UserID, CellID: h.CellID, NodeID: h.NodeID,
			ConnectionID: connectionID, ConnectionEpoch: proof.GetConnectionEpoch(), AssignmentVersion: claims.AssignmentVersion,
			GrantVersion: claims.GrantVersion, ProtocolVersion: 1,
		}); err != nil {
			h.Connections.DetachEndpoint(endpointID, connectionID, proof.GetConnectionEpoch())
			h.closeForProtocolFailure(connection, websocket.StatusInternalError, relayReasonRouteUnavailable, "relayHandshake", "connection", roleForRelayConnection(directPeer), 1, 0, endpointID)
			return
		}
	}
	defer h.Connections.DetachEndpoint(endpointID, connectionID, proof.GetConnectionEpoch())
	if h.Peers != nil {
		defer func() {
			disconnectContext, disconnectCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer disconnectCancel()
			h.Peers.DisconnectEndpoint(disconnectContext, endpointID, claims.Subject)
		}()
	}
	if h.Files != nil && !directPeer {
		defer h.Files.DisconnectDevice(session.Context(), claims.Subject)
	}
	heartbeatSeconds := h.HeartbeatSeconds
	if heartbeatSeconds == 0 {
		heartbeatSeconds = 25
	}
	if err := session.Enqueue(&remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: proof.GetConnectionEpoch(),
		Frame: &remotev1.Envelope_Ready{Ready: &remotev1.Ready{
			ConnectionId: connectionID, AcceptedConnectionEpoch: proof.GetConnectionEpoch(), HeartbeatIntervalSeconds: heartbeatSeconds,
			ControlFrameLimitBytes: relayprotocol.ControlFrameLimit, AbsoluteFrameLimitBytes: relayprotocol.AbsoluteFrameLimit,
		}},
	}); err != nil {
		h.recordConnectionLifecycle(RelayConnectionLifecycle{
			Event: "relay_ready_write_failed", Reason: relayReasonBackendUnavailable,
			Role: roleForRelayConnection(directPeer), ConnectionEpoch: proof.GetConnectionEpoch(),
			CorrelationID: relayCorrelationID(endpointID), HandshakeMilliseconds: time.Since(handshakeStarted).Milliseconds(),
		})
		return
	}
	h.Connections.ReleaseHandshake()
	handshakeHeld = false
	handshakeSucceeded = true
	h.Connections.RecordHandshake(true)
	connectionStarted := time.Now()
	h.recordConnectionLifecycle(RelayConnectionLifecycle{
		Event: "relay_connection_ready", Reason: "authenticated",
		Role: roleForRelayConnection(directPeer), ConnectionEpoch: proof.GetConnectionEpoch(),
		CorrelationID: relayCorrelationID(endpointID), HandshakeMilliseconds: time.Since(handshakeStarted).Milliseconds(),
		HeartbeatSeconds: heartbeatSeconds,
	})
	defer func() {
		h.recordConnectionLifecycle(RelayConnectionLifecycle{
			Event: "relay_connection_closed", Reason: "handler_finished",
			Role: roleForRelayConnection(directPeer), ConnectionEpoch: proof.GetConnectionEpoch(),
			CorrelationID: relayCorrelationID(endpointID), ConnectionLifetimeMilliseconds: time.Since(connectionStarted).Milliseconds(),
		})
	}()

	windowStarted := h.now()
	framesThisWindow, bytesThisWindow := 0, 0
	// Admission tickets have a bounded lifetime, while an authenticated
	// WebSocket is a warm carrier for many independently authorised logical
	// Peer sessions.
	// Keep the carrier alive as long as the client supplies heartbeats; each
	// PEER_OPEN still verifies a fresh one-time ticket, scope, project and route
	// fence. This avoids tearing down every mobile conversation merely because
	// the ticket used to establish an already accepted Peer later expires.
	readTimeout := relayClientReadTimeout(heartbeatSeconds)
	for {
		readContext, cancelRead := context.WithTimeout(session.Context(), readTimeout)
		envelope, err := readEnvelope(readContext, connection)
		cancelRead()
		if err != nil {
			if reason, protocolFailure := relayProtocolReason(err); protocolFailure {
				h.closeForProtocolFailure(connection, websocket.StatusUnsupportedData, reason, "relayEnvelope", "connection", roleForRelayConnection(directPeer), 0, 0, endpointID)
			}
			return
		}
		now = h.now()
		if now.Sub(windowStarted) >= time.Second {
			windowStarted, framesThisWindow, bytesThisWindow = now, 0, 0
		}
		framesThisWindow++
		frameBytes := proto.Size(envelope)
		bytesThisWindow += frameBytes
		h.Connections.RecordIngress(frameBytes)
		maxFrames, maxBytes := h.MaxFramesPerSecond, h.MaxBytesPerSecond
		if maxFrames <= 0 {
			maxFrames = 100
		}
		if maxBytes <= 0 {
			maxBytes = 8 << 20
		}
		if framesThisWindow > maxFrames || bytesThisWindow > maxBytes {
			h.Connections.RecordRateLimit()
			h.recordConnectionLifecycle(RelayConnectionLifecycle{
				Event: "relay_connection_rate_limited", Reason: relayReasonRateLimited,
				Role: roleForRelayConnection(directPeer), ConnectionEpoch: proof.GetConnectionEpoch(),
				CorrelationID: relayCorrelationID(endpointID), FramesInWindow: framesThisWindow,
				BytesInWindow: bytesThisWindow, MaxFramesPerSecond: maxFrames, MaxBytesPerSecond: maxBytes,
			})
			h.closeForProtocolFailure(connection, websocket.StatusTryAgainLater, relayReasonRateLimited, "relayEnvelope", "connection", roleForRelayConnection(directPeer), envelope.GetProtocolVersion(), frameBytes, endpointID)
			return
		}
		if envelope.GetProtocolVersion() != 1 {
			h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonProtocolVersionInvalid, "relayEnvelope", "connection", roleForRelayConnection(directPeer), envelope.GetProtocolVersion(), frameBytes, endpointID)
			return
		}
		if envelope.GetConnectionEpoch() != proof.GetConnectionEpoch() {
			h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonPeerEpochStale, "relayEnvelope", "connection", roleForRelayConnection(directPeer), 1, frameBytes, endpointID)
			return
		}
		if !directPeer && h.Routes != nil {
			if err := h.Routes.Renew(claims.Subject, connectionID, proof.GetConnectionEpoch(), routeTTL, now); err != nil {
				h.Connections.RecordRouteRejection()
				h.closeForProtocolFailure(connection, websocket.StatusPolicyViolation, relayReasonRouteUnavailable, "relayEnvelope", "connection", roleForRelayConnection(directPeer), 1, frameBytes, endpointID)
				return
			}
		}
		_, frameClass := envelopeClass(envelope)
		if frameClass == relayprotocol.FrameClassPeerPayload {
			if h.Peers == nil {
				h.closeForProtocolFailure(connection, websocket.StatusUnsupportedData, relayReasonForwardingUnavailable, "peerBinding", "connection", roleForRelayConnection(directPeer), 1, frameBytes, endpointID)
				return
			}
			// A direct controller WebSocket is authenticated by the ticket used
			// for its challenge/proof, but it intentionally is not a ticket-scoped
			// RPC session. A warm controller channel may carry several narrow Peer
			// sessions (for example task and conversation lists) without repeating
			// a TLS/WebSocket handshake. PeerForwarder validates every PEER_OPEN
			// ticket (including one-time use, source identity, target, scope and
			// project binding), and binds all later encrypted frames to this exact
			// endpoint. Do not reintroduce a session-ID equality check here: it
			// would make a second, independently authorised ticket impossible to
			// use on the already authenticated controller channel.
			if err := h.Peers.HandleFromEndpoint(session.Context(), endpointID, claims.Subject, proof.GetConnectionEpoch(), envelope); err != nil {
				// A PeerError is a session/query-level protocol result. Keep the
				// physical carrier healthy for all other scopes unless the source
				// writer itself is known to be unavailable.
				if IsPeerConnectionFatal(err) {
					h.recordProtocolFailure("peerBinding", relayReasonPeerBindingInvalid, "connection", roleForRelayConnection(directPeer), 1, frameBytes, endpointID)
					return
				}
			}
			continue
		}
		if frameClass == relayprotocol.FrameClassFileControl || frameClass == relayprotocol.FrameClassFilePayload {
			if directPeer || h.Files == nil {
				h.closeForProtocolFailure(connection, websocket.StatusUnsupportedData, relayReasonForwardingUnavailable, "relayEnvelope", "connection", roleForRelayConnection(directPeer), 1, frameBytes, endpointID)
				return
			}
			// File protocol errors terminate only the logical transfer. They must
			// not tear down the resident control connection used by heartbeats,
			// commands and unrelated Peer sessions.
			_ = h.Files.HandleFromDevice(session.Context(), claims.Subject, proof.GetConnectionEpoch(), envelope)
			continue
		}
		ping := envelope.GetPing()
		if ping == nil {
			h.closeForProtocolFailure(connection, websocket.StatusUnsupportedData, relayReasonFrameKindUnknown, "relayEnvelope", "connection", roleForRelayConnection(directPeer), 1, frameBytes, endpointID)
			return
		}
		if err := session.Enqueue(&remotev1.Envelope{
			ProtocolVersion: 1, ConnectionEpoch: proof.GetConnectionEpoch(), Sequence: envelope.GetSequence(),
			Frame: &remotev1.Envelope_Pong{Pong: &remotev1.Pong{MonotonicMillis: ping.GetMonotonicMillis()}},
		}); err != nil {
			return
		}
	}
}

func relayClientReadTimeout(heartbeatSeconds uint32) time.Duration {
	if heartbeatSeconds == 0 {
		heartbeatSeconds = 25
	}
	// One missed interval plus a small scheduling allowance tolerates transient
	// radio/CPU stalls while still reclaiming a half-open socket before the
	// normal 60-second resident Route TTL expires.
	timeout := (2*time.Duration(heartbeatSeconds) + 3) * time.Second
	if timeout < 15*time.Second {
		return 15 * time.Second
	}
	return timeout
}

// UpdateBrowserOriginPatterns atomically replaces the management UI origin
// allow-list after a Relay configuration heartbeat.
func (h *Handler) UpdateBrowserOriginPatterns(patterns []string) {
	h.browserOriginMu.Lock()
	defer h.browserOriginMu.Unlock()
	h.BrowserOriginPatterns = append([]string(nil), patterns...)
}

func (h *Handler) browserOrigins() []string {
	h.browserOriginMu.RLock()
	defer h.browserOriginMu.RUnlock()
	return append([]string(nil), h.BrowserOriginPatterns...)
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) random() io.Reader {
	if h.Random != nil {
		return h.Random
	}
	return rand.Reader
}

func roleForRelayConnection(directPeer bool) string {
	if directPeer {
		return "peer"
	}
	return "device"
}

func (h *Handler) closeForProtocolFailure(connection *websocket.Conn, status websocket.StatusCode, reason, stage, faultLevel, role string, protocolVersion uint32, frameBytes int, correlation string) {
	h.recordProtocolFailure(stage, reason, faultLevel, role, protocolVersion, frameBytes, correlation)
	_ = connection.Close(status, reason)
}

func (h *Handler) recordProtocolFailure(stage, reason, faultLevel, role string, protocolVersion uint32, frameBytes int, correlation string) {
	if h.ProtocolFailure == nil {
		return
	}
	h.ProtocolFailure(RelayProtocolFailure{
		Stage: stage, Reason: reason, FaultLevel: faultLevel, Role: role,
		ProtocolVersion: protocolVersion, FrameSizeBucket: relayFrameSizeBucket(frameBytes),
		CorrelationID: relayCorrelationID(correlation),
	})
}

func (h *Handler) recordConnectionLifecycle(event RelayConnectionLifecycle) {
	if h.ConnectionLifecycle == nil {
		return
	}
	h.ConnectionLifecycle(event)
}

func relayAuthorization(header string) (string, bool) {
	token, directPeer, ok := connectionAuthorization(header)
	return token, ok && !directPeer
}

func connectionAuthorization(header string) (token string, directPeer bool, ok bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || len(parts[1]) == 0 || len(parts[1]) > maximumTicketBytes {
		return "", false, false
	}
	switch parts[0] {
	case "Relay":
		return parts[1], false, true
	case "Peer":
		return parts[1], true, true
	default:
		return "", false, false
	}
}

// requestAuthorization accepts the Authorization header for native clients and
// a narrowly scoped WebSocket subprotocol carrier for browser direct-Peer
// connections. Browsers cannot set Authorization on a WebSocket handshake. The
// credential-bearing protocol is intentionally never included in AcceptOptions,
// so the server only echoes the fixed public protocol name.
func requestAuthorization(request *http.Request) (token string, directPeer bool, ok bool) {
	if request == nil {
		return "", false, false
	}
	protocols, protocolsOK := offeredSubprotocols(request.Header.Values("Sec-WebSocket-Protocol"))
	if !protocolsOK {
		return "", false, false
	}
	authorization := request.Header.Values("Authorization")
	if len(authorization) > 0 {
		if len(authorization) != 1 || len(protocols) != 1 || protocols[0] != Subprotocol {
			return "", false, false
		}
		return connectionAuthorization(authorization[0])
	}
	if len(protocols) != 2 {
		return "", false, false
	}
	fixedFound := false
	carrier := ""
	for _, protocol := range protocols {
		switch {
		case protocol == Subprotocol:
			if fixedFound {
				return "", false, false
			}
			fixedFound = true
		case strings.HasPrefix(protocol, peerTicketSubprotocolPrefix):
			if carrier != "" {
				return "", false, false
			}
			carrier = strings.TrimPrefix(protocol, peerTicketSubprotocolPrefix)
		default:
			return "", false, false
		}
	}
	if !fixedFound || carrier == "" || len(carrier) > base64.RawURLEncoding.EncodedLen(maximumTicketBytes) {
		return "", false, false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(carrier)
	if err != nil || len(decoded) == 0 || len(decoded) > maximumTicketBytes ||
		base64.RawURLEncoding.EncodeToString(decoded) != carrier {
		return "", false, false
	}
	for _, character := range decoded {
		if character <= ' ' || character == 0x7f {
			return "", false, false
		}
	}
	return string(decoded), true, true
}

func offeredSubprotocols(headerValues []string) ([]string, bool) {
	protocols := make([]string, 0, 2)
	seen := make(map[string]struct{}, 2)
	for _, headerValue := range headerValues {
		for _, part := range strings.Split(headerValue, ",") {
			protocol := strings.TrimSpace(part)
			if protocol == "" || len(protocol) > peerTicketSubprotocolPrefixLength()+base64.RawURLEncoding.EncodedLen(maximumTicketBytes) {
				return nil, false
			}
			for _, character := range protocol {
				if !websocketProtocolCharacter(character) {
					return nil, false
				}
			}
			if _, duplicate := seen[protocol]; duplicate {
				return nil, false
			}
			seen[protocol] = struct{}{}
			protocols = append(protocols, protocol)
			if len(protocols) > 2 {
				return nil, false
			}
		}
	}
	return protocols, true
}

func peerTicketSubprotocolPrefixLength() int { return len(peerTicketSubprotocolPrefix) }

func websocketProtocolCharacter(character rune) bool {
	if character < 0x21 || character > 0x7e {
		return false
	}
	switch character {
	case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=', '{', '}':
		return false
	default:
		return true
	}
}

func readEnvelope(ctx context.Context, connection *websocket.Conn) (*remotev1.Envelope, error) {
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		return nil, err
	}
	if messageType != websocket.MessageBinary {
		return nil, newRelayProtocolError(relayReasonEnvelopeInvalid, errors.New("relay frame must be binary"))
	}
	envelope := new(remotev1.Envelope)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		return nil, newRelayProtocolError(relayReasonEnvelopeInvalid, err)
	}
	frameName, class := envelopeClass(envelope)
	if class == relayprotocol.FrameClassUnknown {
		return nil, newRelayProtocolError(relayReasonFrameKindUnknown, relayprotocol.ErrUnknownFrame)
	}
	if err := relayprotocol.ValidateFrameSize(frameName, len(payload)); err != nil {
		return nil, newRelayProtocolError(relayReasonFrameSizeInvalid, err)
	}
	return envelope, nil
}

func writeEnvelope(ctx context.Context, connection *websocket.Conn, envelope *remotev1.Envelope) error {
	payload, err := proto.Marshal(envelope)
	if err != nil {
		return err
	}
	if len(payload) > relayprotocol.AbsoluteFrameLimit {
		return relayprotocol.ErrFrameTooLarge
	}
	return connection.Write(ctx, websocket.MessageBinary, payload)
}
