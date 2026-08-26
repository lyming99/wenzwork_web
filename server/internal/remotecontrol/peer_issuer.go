package remotecontrol

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/peersession"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"gorm.io/gorm"
)

const (
	// Peer tickets are admission credentials for a long-lived encrypted
	// session. Relay and Agent verify them at PEER_OPEN only; an accepted
	// session is not periodically revalidated with Host.
	DefaultPeerTicketTTL   = 30 * 24 * time.Hour
	DefaultPeerMaxDuration = 30 * 24 * time.Hour
	DefaultPeerMaxBytes    = 16 << 20
)

type PeerRouteResolver interface {
	Resolve(string, time.Time) (relayrouter.Route, error)
}

// ContextPeerRouteResolver lets latency-sensitive ticket issuance propagate
// its single end-to-end deadline into the route lookup while preserving the
// older resolver interface for test doubles and other callers.
type ContextPeerRouteResolver interface {
	PeerRouteResolver
	ResolveContext(context.Context, string, time.Time) (relayrouter.Route, error)
}

type PeerIdempotencyStore interface {
	Reserve(context.Context, string, string, string, string, remoteauth.Claims, time.Duration) (remoteauth.Claims, error)
}

type BrowserPeerTicketIssuerConfig struct {
	Database    *gorm.DB
	Routes      PeerRouteResolver
	Signer      remoteauth.Issuer
	Idempotency PeerIdempotencyStore
	TicketTTL   time.Duration
	MaxDuration time.Duration
	MaxBytes    uint64
	Now         func() time.Time
	// ProtocolDiagnostic receives only reviewed, content-free dimensions. The
	// signed ticket and raw identifiers are intentionally absent from its type.
	ProtocolDiagnostic func(HostProtocolDiagnostic)
}

type HostProtocolDiagnostic struct {
	OccurredAt       time.Time `json:"occurredAt"`
	Result           string    `json:"result"`
	Reason           string    `json:"reason"`
	FaultLevel       string    `json:"faultLevel"`
	Scope            string    `json:"scope"`
	ProtocolMinimum  uint32    `json:"protocolMinimum"`
	ProtocolMaximum  uint32    `json:"protocolMaximum"`
	ObservedProtocol uint32    `json:"observedProtocol,omitempty"`
	TargetHash       string    `json:"targetHash"`
	ProjectHash      string    `json:"projectHash,omitempty"`
	RequestHash      string    `json:"requestHash"`
	SessionHash      string    `json:"sessionHash,omitempty"`
	DurationBucket   string    `json:"durationBucket"`
}

type BrowserPeerTicketIssuer struct {
	db                 *gorm.DB
	routes             PeerRouteResolver
	signer             remoteauth.Issuer
	idempotency        PeerIdempotencyStore
	ticketTTL          time.Duration
	maxDuration        time.Duration
	maxBytes           uint64
	now                func() time.Time
	protocolDiagnostic func(HostProtocolDiagnostic)
}

func NewBrowserPeerTicketIssuer(config BrowserPeerTicketIssuerConfig) (*BrowserPeerTicketIssuer, error) {
	ticketTTL := config.TicketTTL
	if ticketTTL == 0 {
		ticketTTL = DefaultPeerTicketTTL
	}
	maxDuration := config.MaxDuration
	if maxDuration == 0 {
		maxDuration = DefaultPeerMaxDuration
	}
	maxBytes := config.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultPeerMaxBytes
	}
	if config.Database == nil || config.Routes == nil || config.Idempotency == nil ||
		config.Signer.Issuer == "" || config.Signer.KeyID == "" || len(config.Signer.PrivateKey) != ed25519.PrivateKeySize ||
		ticketTTL < time.Second || ticketTTL > DefaultPeerTicketTTL || maxDuration < time.Second || maxDuration > DefaultPeerMaxDuration ||
		maxBytes == 0 || maxBytes > DefaultPeerMaxBytes {
		return nil, errors.New("browser Peer ticket issuer configuration is invalid")
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &BrowserPeerTicketIssuer{
		db: config.Database, routes: config.Routes, signer: config.Signer,
		idempotency: config.Idempotency, ticketTTL: ticketTTL, maxDuration: maxDuration, maxBytes: maxBytes, now: now,
		protocolDiagnostic: config.ProtocolDiagnostic,
	}, nil
}

func (issuer *BrowserPeerTicketIssuer) IssueBrowserPeer(ctx context.Context, input PeerIssueInput) (session PeerSession, returnErr error) {
	startedAt := issuer.now().UTC()
	var observedProtocol uint32
	defer func() {
		issuer.recordHostProtocolDiagnostic(input, session, returnErr, observedProtocol, issuer.now().UTC().Sub(startedAt))
	}()
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.ControllerID == uuid.Nil || input.TargetDeviceID == uuid.Nil ||
		len(input.ControllerPublicKey) != ed25519.PublicKeySize || len(input.TargetPublicKey) != ed25519.PublicKeySize ||
		input.ControllerGrantVersion == 0 || input.ControllerKeyVersion == 0 || input.TargetGrantVersion == 0 || input.TargetKeyVersion == 0 || len(input.ControllerKeyThumbprint) != 43 ||
		len(input.TargetKeyThumbprint) != 43 || remoteauth.PublicKeyThumbprint(input.ControllerPublicKey) != input.ControllerKeyThumbprint ||
		remoteauth.PublicKeyThumbprint(input.TargetPublicKey) != input.TargetKeyThumbprint || !validIdempotencyKey(input.IdempotencyKey) ||
		(input.ProjectID != nil && *input.ProjectID == uuid.Nil) || (peerScopeRequiresProject(input.Scope) && input.ProjectID == nil) ||
		(!peerScopeRequiresProject(input.Scope) && input.ProjectID != nil) {
		return PeerSession{}, ErrInvalidInput
	}
	if _, ok := allowedControllerScopes[input.Scope]; !ok {
		return PeerSession{}, ErrInvalidInput
	}
	maxDuration := issuer.maxDuration
	if input.RequestedMaxDurationSeconds != nil {
		requested := time.Duration(*input.RequestedMaxDurationSeconds) * time.Second
		if requested < time.Second || requested > DefaultPeerMaxDuration {
			return PeerSession{}, ErrInvalidInput
		}
		if requested < maxDuration {
			maxDuration = requested
		}
	}
	maxBytes := issuer.maxBytes
	if input.RequestedMaxBytes != nil {
		if *input.RequestedMaxBytes == 0 || *input.RequestedMaxBytes > DefaultPeerMaxBytes {
			return PeerSession{}, ErrInvalidInput
		}
		if *input.RequestedMaxBytes < maxBytes {
			maxBytes = *input.RequestedMaxBytes
		}
	}
	now := issuer.now().UTC()
	var route relayrouter.Route
	var routeErr error
	if contextualRoutes, ok := issuer.routes.(ContextPeerRouteResolver); ok {
		route, routeErr = contextualRoutes.ResolveContext(ctx, input.TargetDeviceID.String(), now)
	} else {
		route, routeErr = issuer.routes.Resolve(input.TargetDeviceID.String(), now)
	}
	if routeErr != nil {
		if errors.Is(routeErr, relayrouter.ErrRouteNotFound) {
			return PeerSession{}, ErrNotFound
		}
		return PeerSession{}, ErrUnavailable
	}
	observedProtocol = route.ProtocolVersion
	relayNodeID, nodeErr := uuid.Parse(route.NodeID)
	relayCellID, cellErr := uuid.Parse(route.CellID)
	if nodeErr != nil || cellErr != nil || route.DeviceID != input.TargetDeviceID.String() || route.UserID != input.UserID.String() ||
		route.GrantVersion != input.TargetGrantVersion || route.ConnectionEpoch == 0 {
		return PeerSession{}, ErrUnavailable
	}
	if route.ProtocolVersion != 1 {
		return PeerSession{}, ErrProtocolVersion
	}
	relayURL, err := issuer.loadRelayEndpoint(ctx, relayNodeID, relayCellID, now)
	if err != nil {
		return PeerSession{}, err
	}
	claims := newBrowserPeerClaims(input, relayNodeID, relayCellID, route.ConnectionEpoch, now, issuer.ticketTTL, maxDuration, maxBytes)
	hash := requestHash(struct {
		TargetDeviceID  uuid.UUID  `json:"targetDeviceId"`
		Scope           string     `json:"scope"`
		ProjectID       *uuid.UUID `json:"projectId"`
		DurationSeconds uint32     `json:"durationSeconds"`
		MaxBytes        uint64     `json:"maxBytes"`
		ControllerGrant uint64     `json:"controllerGrantVersion"`
		TargetGrant     uint64     `json:"targetGrantVersion"`
		ControllerKey   uint64     `json:"controllerKeyVersion"`
		TargetKey       uint64     `json:"targetKeyVersion"`
	}{input.TargetDeviceID, input.Scope, input.ProjectID, uint32(maxDuration / time.Second), maxBytes, input.ControllerGrantVersion, input.TargetGrantVersion, input.ControllerKeyVersion, input.TargetKeyVersion})
	claims, err = issuer.idempotency.Reserve(ctx, input.UserID.String(), input.ControllerID.String(), input.IdempotencyKey, hash, claims, issuer.ticketTTL)
	if err != nil {
		if errors.Is(err, peersession.ErrIdempotencyConflict) {
			return PeerSession{}, ErrIdempotencyConflict
		}
		if errors.Is(err, peersession.ErrInvalidRequest) {
			return PeerSession{}, ErrInvalidInput
		}
		return PeerSession{}, ErrUnavailable
	}
	if claims.SourceDeviceID != input.ControllerID.String() || claims.TargetDeviceID != input.TargetDeviceID.String() ||
		claims.SourceGrantVersion != input.ControllerGrantVersion || claims.TargetGrantVersion != input.TargetGrantVersion ||
		claims.SourceKeyThumbprint != input.ControllerKeyThumbprint || claims.TargetKeyThumbprint != input.TargetKeyThumbprint ||
		claims.SourceCredentialType != "controller" || claims.SourceIdentityKey != base64RawURL(input.ControllerPublicKey) ||
		claims.TargetIdentityKey != base64RawURL(input.TargetPublicKey) || claims.SourceKeyVersion != input.ControllerKeyVersion ||
		claims.TargetKeyVersion != input.TargetKeyVersion ||
		claims.RelayNodeID != relayNodeID.String() || claims.RelayCellID != relayCellID.String() ||
		claims.TargetConnectionEpoch != route.ConnectionEpoch || !claims.HasScope(input.Scope) || claims.ProjectID != remoteProjectIDString(input.ProjectID) || claims.ExpiresAt <= now.Unix() {
		return PeerSession{}, ErrIdempotencyConflict
	}
	ticket, err := issuer.signer.Sign(claims)
	if err != nil {
		return PeerSession{}, ErrUnavailable
	}
	sessionID, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return PeerSession{}, ErrUnavailable
	}
	return PeerSession{
		SessionID: sessionID, PeerSessionTicket: ticket,
		WebSocketSubprotocols: browserWebSocketSubprotocols(ticket),
		ExpiresAt:             time.Unix(claims.ExpiresAt, 0).UTC(), MaxDurationSeconds: claims.MaxDurationSeconds, MaxBytes: claims.MaxBytes,
		TargetIdentityAlgorithm: "Ed25519", TargetIdentityPublicKey: base64RawURL(input.TargetPublicKey),
		TargetKeyThumbprint: input.TargetKeyThumbprint, TargetKeyVersion: input.TargetKeyVersion,
		RelayURL: relayURL, RelayNodeID: relayNodeID, RelayCellID: relayCellID,
		TargetConnectionEpoch: route.ConnectionEpoch,
		ProtocolMinimum:       1,
		ProtocolMaximum:       1,
		NegotiatedProtocol:    1,
		Limits: PeerProtocolLimits{
			PeerPlaintextBytes:    60 << 10,
			RPCJSONBytes:          56 << 10,
			PreferredPageBytes:    48 << 10,
			TaskPayloadBytes:      512 << 10,
			TaskPayloadChunkBytes: 32 << 10,
		},
		EventCapabilities: PeerEventCapabilities{
			ContractVersion: 1,
			AcceptedKinds: []string{
				"chat.goal.changed", "chat.plan_mode.changed", "chat.todo.updated",
				"chat.subagent.started", "chat.subagent.status", "chat.subagent.message",
			},
			CollaborationV1: true,
		},
	}, nil
}

func (issuer *BrowserPeerTicketIssuer) recordHostProtocolDiagnostic(input PeerIssueInput, session PeerSession, issueErr error, observedProtocol uint32, duration time.Duration) {
	if issuer == nil || issuer.protocolDiagnostic == nil {
		return
	}
	result, reason, faultLevel := "failed", hostPeerIssueReason(issueErr), "session"
	if issueErr == nil {
		result, reason = "issued", "peer_session_issued"
	}
	diagnostic := HostProtocolDiagnostic{
		OccurredAt: issuer.now().UTC(), Result: result, Reason: reason, FaultLevel: faultLevel,
		Scope: safeHostPeerScope(input.Scope), ProtocolMinimum: 1, ProtocolMaximum: 1, ObservedProtocol: observedProtocol,
		TargetHash:  issuer.hostIdentifierHash("target", input.TargetDeviceID.String()),
		RequestHash: issuer.hostIdentifierHash("request", input.IdempotencyKey), DurationBucket: hostDurationBucket(duration),
	}
	if session.SessionID != uuid.Nil {
		diagnostic.SessionHash = issuer.hostIdentifierHash("session", session.SessionID.String())
	}
	if input.ProjectID != nil {
		diagnostic.ProjectHash = issuer.hostIdentifierHash("project", input.ProjectID.String())
	}
	issuer.protocolDiagnostic(diagnostic)
}

func hostPeerIssueReason(err error) string {
	switch {
	case err == nil:
		return "peer_session_issued"
	case errors.Is(err, ErrProtocolVersion):
		return "relay_protocol_version_invalid"
	case errors.Is(err, ErrInvalidInput):
		return "host_session_request_invalid"
	case errors.Is(err, ErrForbidden):
		return "host_session_forbidden"
	case errors.Is(err, ErrNotFound):
		return "host_route_not_found"
	case errors.Is(err, ErrIdempotencyConflict):
		return "host_session_idempotency_conflict"
	default:
		return "host_session_unavailable"
	}
}

func (issuer *BrowserPeerTicketIssuer) hostIdentifierHash(kind, value string) string {
	value = strings.TrimSpace(value)
	if issuer == nil || value == "" || len(issuer.signer.PrivateKey) == 0 {
		return ""
	}
	digest := hmac.New(sha256.New, issuer.signer.PrivateKey)
	_, _ = digest.Write([]byte(kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(value))
	return hex.EncodeToString(digest.Sum(nil)[:12])
}

func safeHostPeerScope(scope string) string {
	if _, ok := allowedControllerScopes[scope]; ok {
		return scope
	}
	return "unknown"
}

func hostDurationBucket(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	switch {
	case duration < 10*time.Millisecond:
		return "under_10ms"
	case duration < 100*time.Millisecond:
		return "10_to_100ms"
	case duration < time.Second:
		return "100ms_to_1s"
	default:
		return "at_or_over_1s"
	}
}

func (issuer *BrowserPeerTicketIssuer) loadRelayEndpoint(ctx context.Context, nodeID, cellID uuid.UUID, now time.Time) (string, error) {
	var row struct {
		Addresses jsonBytes `gorm:"column:addresses"`
	}
	if err := issuer.db.WithContext(ctx).Raw(`
		SELECT instance.addresses
		FROM relay_node_instances instance
		JOIN relay_node_installations installation
		  ON installation.id = instance.installation_id AND installation.current_instance_id = instance.id
		WHERE instance.id = ? AND instance.cell_id = ? AND instance.status = 'ready'
		  AND instance.lease_expires_at > ? AND installation.status = 'active'`, nodeID, cellID, now).Take(&row).Error; err != nil {
		return "", ErrUnavailable
	}
	var addresses []string
	if err := jsonUnmarshal(row.Addresses, &addresses); err != nil || len(addresses) == 0 || len(addresses) > 16 {
		return "", ErrUnavailable
	}
	for _, address := range addresses {
		if normalized, err := validateRelayEndpoint(address); err == nil {
			return normalized, nil
		}
	}
	return "", ErrUnavailable
}

func validateRelayEndpoint(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil || parsed.Path != "/v1/connect" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrUnavailable
	}
	return parsed.String(), nil
}

// Local aliases keep the ticket issuer's database boundary narrow and avoid
// exposing cloud metadata through the end-to-end RPC protobuf package.
type jsonBytes []byte

func jsonUnmarshal(data []byte, destination any) error {
	if len(data) == 0 {
		return fmt.Errorf("empty JSON")
	}
	return json.Unmarshal(data, destination)
}

func base64RawURL(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func browserWebSocketSubprotocols(ticket string) []string {
	return []string{"wenzwork-relay.v1", "wenzwork-peer-ticket." + base64RawURL([]byte(ticket))}
}

func newBrowserPeerClaims(input PeerIssueInput, relayNodeID, relayCellID uuid.UUID, targetEpoch uint64, now time.Time, ticketTTL, maxDuration time.Duration, maxBytes uint64) remoteauth.Claims {
	claims := remoteauth.Claims{
		Audience: "relay-peer", Subject: input.ControllerID.String(), UserID: input.UserID.String(), SessionID: uuid.NewString(),
		SourceDeviceID: input.ControllerID.String(), TargetDeviceID: input.TargetDeviceID.String(),
		SourceGrantVersion: input.ControllerGrantVersion, TargetGrantVersion: input.TargetGrantVersion,
		SourceKeyThumbprint: input.ControllerKeyThumbprint, TargetKeyThumbprint: input.TargetKeyThumbprint,
		SourceCredentialType: "controller", SourceIdentityKey: base64RawURL(input.ControllerPublicKey),
		TargetIdentityKey: base64RawURL(input.TargetPublicKey), SourceKeyVersion: input.ControllerKeyVersion,
		TargetKeyVersion: input.TargetKeyVersion,
		Confirmation:     input.ControllerKeyThumbprint, RelayNodeID: relayNodeID.String(), RelayCellID: relayCellID.String(),
		TargetConnectionEpoch: targetEpoch, Scopes: []string{input.Scope},
		MaxDurationSeconds: uint32(maxDuration / time.Second), MaxBytes: maxBytes, JWTID: uuid.NewString(),
		IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(ticketTTL).Unix(),
	}
	claims.ProjectID = remoteProjectIDString(input.ProjectID)
	return claims
}

func remoteProjectIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}
