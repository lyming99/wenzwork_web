package relayserver

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

const (
	maxPeerSessionsPerDevice        = 8
	maxPeerTicketLifetime           = 30 * 24 * time.Hour
	maxPeerDuration                 = 30 * 24 * time.Hour
	peerOpenTimeout                 = 15 * time.Second
	maxPeerBytes             uint64 = 16 << 20
)

var (
	ErrPeerFrameInvalid = errors.New("Peer frame is invalid")
	ErrPeerUnavailable  = errors.New("Peer forwarding is unavailable")
	ErrPeerOffline      = errors.New("Peer target is offline")
	ErrPeerInterrupted  = errors.New("Peer Session was interrupted")
	// ErrPeerConnectionUnwritable is reserved for the narrow case where the
	// source endpoint cannot accept a protocol error at all. Session/query
	// validation failures are deliberately encoded as PeerError and do not use
	// this sentinel.
	ErrPeerConnectionUnwritable = errors.New("Peer source connection is unwritable")
)

// IsPeerConnectionFatal is the Handler's explicit escalation boundary. Peer
// routing, ticket, sequence and query failures are recoverable protocol
// results; only a writer that cannot reach the source endpoint can justify
// ending the physical WebSocket loop.
func IsPeerConnectionFatal(err error) bool {
	return errors.Is(err, ErrPeerConnectionUnwritable) || errors.Is(err, context.Canceled)
}

type PeerRouteRegistry interface {
	ConsumePeerTicket(context.Context, string, time.Time, time.Time) error
}

type PeerDeviceStateResolver interface {
	VerifyPeerDeviceState(context.Context, string, uint64, string) (ed25519.PublicKey, error)
}

type PeerForwarderConfig struct {
	NodeID   string
	CellID   string
	Verifier TicketVerifier
	// Devices is retained for source compatibility with older embedders. Host
	// signed tickets now carry both identity keys, so Relay does not consult a
	// projected credential store during session admission.
	Devices     PeerDeviceStateResolver
	Routes      PeerRouteRegistry
	Connections *ConnectionManager
	Now         func() time.Time
}

type PeerForwarder struct {
	nodeID      string
	cellID      string
	verifier    TicketVerifier
	routes      PeerRouteRegistry
	connections *ConnectionManager
	now         func() time.Time

	mu       sync.Mutex
	sessions map[string]*peerSession
}

type peerSession struct {
	id                       string
	ticketJWTID              string
	sourceDeviceID           string
	targetDeviceID           string
	sourceEndpointID         string
	sourceConnectionEpoch    uint64
	targetEndpointID         string
	targetConnectionEpoch    uint64
	targetKeyThumbprint      string
	targetIdentityPublicKey  ed25519.PublicKey
	sourceEphemeralPublicKey []byte
	scope                    string
	state                    relayprotocol.PeerSessionState
	expiresAt                time.Time
	openExpiresAt            time.Time
	maxBytes                 uint64
	forwardedBytes           uint64
	lastSequences            map[string]uint64
}

func NewPeerForwarder(config PeerForwarderConfig) (*PeerForwarder, error) {
	if config.NodeID == "" || config.CellID == "" || config.Verifier == nil || config.Routes == nil ||
		config.Connections == nil {
		return nil, errors.New("Peer forwarder dependencies are required")
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &PeerForwarder{
		nodeID: config.NodeID, cellID: config.CellID, verifier: config.Verifier, routes: config.Routes,
		connections: config.Connections, now: now,
		sessions: make(map[string]*peerSession),
	}, nil
}

func (forwarder *PeerForwarder) HandleFromDevice(ctx context.Context, deviceID string, epoch uint64, envelope *remotev1.Envelope) error {
	return forwarder.HandleFromEndpoint(ctx, deviceID, deviceID, epoch, envelope)
}

// HandleFromEndpoint processes a frame from either a resident connection or a
// ticket-scoped direct-controller connection. Endpoint IDs are Relay-local and
// keep a controller connection from replacing the source device's resident
// route.
func (forwarder *PeerForwarder) HandleFromEndpoint(ctx context.Context, endpointID, deviceID string, epoch uint64, envelope *remotev1.Envelope) error {
	if ctx.Err() != nil || endpointID == "" || deviceID == "" || epoch == 0 || envelope == nil || envelope.GetConnectionEpoch() != epoch {
		return ErrPeerFrameInvalid
	}
	if open := envelope.GetPeerOpen(); open != nil {
		return forwarder.handleOpen(ctx, endpointID, deviceID, epoch, envelope, open)
	}
	now := forwarder.now().UTC()
	sessionID, queryID, frameBytes, err := peerFrameMetadata(envelope, now)
	if err != nil {
		return forwarder.sendError(endpointID, epoch, sessionID, queryID, remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, false)
	}
	peerEndpointID, peerEpoch, code, err := forwarder.acceptDeviceFrame(endpointID, deviceID, envelope, sessionID, frameBytes, now)
	if err != nil {
		return forwarder.sendError(endpointID, epoch, sessionID, queryID, code, code == remotev1.ErrorCode_ERROR_CODE_PEER_TIMEOUT)
	}
	if err := forwarder.forward(peerEndpointID, peerEpoch, envelope); err != nil {
		return forwarder.sendError(endpointID, epoch, sessionID, queryID, remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, true)
	}
	return nil
}

func (forwarder *PeerForwarder) handleOpen(ctx context.Context, sourceEndpointID, sourceDeviceID string, sourceEpoch uint64, envelope *remotev1.Envelope, open *remotev1.PeerOpen) error {
	now := forwarder.now().UTC()
	claims, targetIdentityPublicKey, err := forwarder.validateOpen(ctx, sourceDeviceID, open, now)
	if err != nil {
		return forwarder.sendError(sourceEndpointID, sourceEpoch, open.GetSessionId(), "", peerTicketErrorCode(err), false)
	}
	if err := forwarder.routes.ConsumePeerTicket(ctx, claims.JWTID, time.Unix(claims.ExpiresAt, 0).UTC(), now); err != nil {
		return forwarder.sendError(sourceEndpointID, sourceEpoch, claims.SessionID, "", remotev1.ErrorCode_ERROR_CODE_TICKET_INVALID, false)
	}
	session := sessionFromClaims(claims, open, targetIdentityPublicKey, sourceEndpointID, sourceEpoch, now)
	created, err := forwarder.registerSession(session)
	if err != nil {
		return forwarder.sendError(sourceEndpointID, sourceEpoch, claims.SessionID, "", remotev1.ErrorCode_ERROR_CODE_RATE_LIMITED, true)
	}
	if !created {
		return forwarder.sendError(sourceEndpointID, sourceEpoch, claims.SessionID, "", remotev1.ErrorCode_ERROR_CODE_TICKET_INVALID, false)
	}
	if err := forwarder.forward(session.targetEndpointID, session.targetConnectionEpoch, envelope); err != nil {
		forwarder.deleteSession(claims.SessionID)
		return forwarder.sendError(sourceEndpointID, sourceEpoch, claims.SessionID, "", remotev1.ErrorCode_ERROR_CODE_PEER_OFFLINE, true)
	}
	return nil
}

func (forwarder *PeerForwarder) validateOpen(_ context.Context, sourceDeviceID string, open *remotev1.PeerOpen, now time.Time) (remoteauth.Claims, ed25519.PublicKey, error) {
	if open == nil || len(open.GetSessionTicket()) < 32 || len(open.GetSessionTicket()) > 16<<10 ||
		uuid.Validate(open.GetSessionId()) != nil || len(open.GetEphemeralPublicKey()) != 32 || len(open.GetIdentitySignature()) != ed25519.SignatureSize {
		return remoteauth.Claims{}, nil, ErrPeerFrameInvalid
	}
	claims, err := forwarder.verifier.Verify(open.GetSessionTicket(), "relay-peer", now)
	if err != nil {
		return remoteauth.Claims{}, nil, err
	}
	if claims.SessionID != open.GetSessionId() || claims.Subject != sourceDeviceID || claims.SourceDeviceID != sourceDeviceID ||
		uuid.Validate(claims.SourceDeviceID) != nil || uuid.Validate(claims.TargetDeviceID) != nil || claims.SourceDeviceID == claims.TargetDeviceID ||
		claims.MaxDurationSeconds == 0 ||
		time.Duration(claims.MaxDurationSeconds)*time.Second > maxPeerDuration || claims.MaxBytes == 0 || claims.MaxBytes > maxPeerBytes ||
		!time.Unix(claims.ExpiresAt, 0).After(now) ||
		(len(claims.Scopes) != 1 || (claims.Scopes[0] != "remote.peer.query" && claims.Scopes[0] != "remote.peer.ai.config" && claims.Scopes[0] != "remote.peer.ai.chat" && claims.Scopes[0] != "remote.peer.ai.tools" && claims.Scopes[0] != "remote.peer.events" &&
			claims.Scopes[0] != "remote.peer.terminal" && claims.Scopes[0] != "remote.peer.terminal.interactive" &&
			claims.Scopes[0] != "remote.peer.file.send" && claims.Scopes[0] != "remote.peer.file.receive" && claims.Scopes[0] != "remote.peer.task.control")) {
		return remoteauth.Claims{}, nil, remoteauth.ErrTicketClaims
	}
	claimedSourceKey, err := remoteauth.DecodeIdentityPublicKey(claims.SourceIdentityKey, claims.SourceKeyThumbprint)
	if err != nil || claims.SourceKeyVersion == 0 {
		return remoteauth.Claims{}, nil, remoteauth.ErrTicketClaims
	}
	var sourceIdentityPublicKey ed25519.PublicKey
	switch claims.SourceCredentialType {
	case "controller":
		if claims.ExpiresAt-claims.IssuedAt > int64(maxPeerTicketLifetime/time.Second) {
			return remoteauth.Claims{}, nil, remoteauth.ErrTicketClaims
		}
		sourceIdentityPublicKey = claimedSourceKey
	case "device":
		if claims.ExpiresAt-claims.IssuedAt > int64(maxPeerTicketLifetime/time.Second) {
			return remoteauth.Claims{}, nil, remoteauth.ErrTicketClaims
		}
		sourceIdentityPublicKey = claimedSourceKey
	default:
		return remoteauth.Claims{}, nil, remoteauth.ErrTicketClaims
	}
	targetIdentityPublicKey, err := remoteauth.DecodeIdentityPublicKey(claims.TargetIdentityKey, claims.TargetKeyThumbprint)
	if err != nil || claims.TargetKeyVersion == 0 {
		return remoteauth.Claims{}, nil, remoteauth.ErrTicketClaims
	}
	if err := claims.ValidatePeer(claims.SourceDeviceID, claims.TargetDeviceID, claims.SourceKeyThumbprint, claims.TargetKeyThumbprint, claims.Scopes[0], claims.SourceGrantVersion, claims.TargetGrantVersion); err != nil {
		return remoteauth.Claims{}, nil, err
	}
	projectRequired := relayPeerScopeRequiresProject(claims.Scopes[0])
	if (projectRequired && claims.ProjectID == "") || (!projectRequired && claims.ProjectID != "") ||
		(claims.ProjectID != "" && (uuid.Validate(claims.ProjectID) != nil || claims.ProjectID != strings.ToLower(claims.ProjectID))) {
		return remoteauth.Claims{}, nil, remoteauth.ErrTicketClaims
	}
	if err := claims.ValidatePeerRelay(forwarder.nodeID, forwarder.cellID, claims.TargetConnectionEpoch); err != nil ||
		!forwarder.connections.HasResident(claims.TargetDeviceID, claims.TargetConnectionEpoch) {
		return remoteauth.Claims{}, nil, ErrPeerOffline
	}
	if err := remoteauth.VerifyPeerOpenIdentity(sourceIdentityPublicKey, claims.SourceKeyThumbprint, remoteauth.PeerOpenIdentityProof{
		TicketJWTID: claims.JWTID, SessionID: claims.SessionID, SourceDeviceID: claims.SourceDeviceID,
		TargetDeviceID: claims.TargetDeviceID, EphemeralPublicKey: open.GetEphemeralPublicKey(),
	}, open.GetIdentitySignature()); err != nil {
		return remoteauth.Claims{}, nil, err
	}
	return claims, append(ed25519.PublicKey(nil), targetIdentityPublicKey...), nil
}

func relayPeerScopeRequiresProject(scope string) bool {
	switch scope {
	case "remote.peer.ai.chat", "remote.peer.ai.tools", "remote.peer.terminal", "remote.peer.terminal.interactive", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.task.control", "remote.peer.events":
		return true
	default:
		return false
	}
}

func sessionFromClaims(claims remoteauth.Claims, open *remotev1.PeerOpen, targetIdentityPublicKey ed25519.PublicKey, sourceEndpointID string, sourceEpoch uint64, now time.Time) *peerSession {
	return &peerSession{
		id: claims.SessionID, ticketJWTID: claims.JWTID, sourceDeviceID: claims.SourceDeviceID,
		targetDeviceID: claims.TargetDeviceID, sourceEndpointID: sourceEndpointID, sourceConnectionEpoch: sourceEpoch,
		targetEndpointID: claims.TargetDeviceID, targetConnectionEpoch: claims.TargetConnectionEpoch,
		targetKeyThumbprint:      claims.TargetKeyThumbprint,
		targetIdentityPublicKey:  append(ed25519.PublicKey(nil), targetIdentityPublicKey...),
		sourceEphemeralPublicKey: append([]byte(nil), open.GetEphemeralPublicKey()...),
		scope:                    claims.Scopes[0], state: relayprotocol.PeerOpening,
		// A ticket is consumed and verified before this point. Do not turn its
		// lifetime into a recurring session lease: a healthy encrypted Peer can
		// remain on the existing Relay carrier without another Host round-trip.
		expiresAt: time.Time{}, openExpiresAt: now.Add(peerOpenTimeout), maxBytes: claims.MaxBytes,
		lastSequences: make(map[string]uint64),
	}
}

func (forwarder *PeerForwarder) registerSession(candidate *peerSession) (bool, error) {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	forwarder.removeExpiredLocked(forwarder.now().UTC())
	if current := forwarder.sessions[candidate.id]; current != nil {
		if current.ticketJWTID == candidate.ticketJWTID && current.sourceDeviceID == candidate.sourceDeviceID && current.targetDeviceID == candidate.targetDeviceID {
			return false, nil
		}
		return false, ErrPeerFrameInvalid
	}
	if forwarder.sessionCountLocked(candidate.sourceDeviceID) >= maxPeerSessionsPerDevice ||
		forwarder.sessionCountLocked(candidate.targetDeviceID) >= maxPeerSessionsPerDevice {
		return false, ErrPeerUnavailable
	}
	forwarder.sessions[candidate.id] = candidate
	return true, nil
}

func (forwarder *PeerForwarder) acceptDeviceFrame(endpointID, deviceID string, envelope *remotev1.Envelope, sessionID string, frameBytes uint64, now time.Time) (string, uint64, remotev1.ErrorCode, error) {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	session := forwarder.sessions[sessionID]
	isSource := session != nil && deviceID == session.sourceDeviceID && endpointID == session.sourceEndpointID
	isTarget := session != nil && deviceID == session.targetDeviceID && endpointID == session.targetEndpointID
	if session == nil || (!isSource && !isTarget) {
		return "", 0, remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, ErrPeerFrameInvalid
	}
	if !session.expiresAt.IsZero() && !session.expiresAt.After(now) {
		delete(forwarder.sessions, sessionID)
		return "", 0, remotev1.ErrorCode_ERROR_CODE_PEER_TIMEOUT, ErrPeerInterrupted
	}
	if session.state == relayprotocol.PeerOpening && !session.openExpiresAt.After(now) {
		delete(forwarder.sessions, sessionID)
		return "", 0, remotev1.ErrorCode_ERROR_CODE_PEER_TIMEOUT, ErrPeerInterrupted
	}
	if ready := envelope.GetPeerReady(); ready != nil {
		if !isTarget || session.state != relayprotocol.PeerOpening ||
			validatePeerReadyIdentity(session, ready) != nil {
			return "", 0, remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, ErrPeerFrameInvalid
		}
		session.state = relayprotocol.PeerActive
	} else if session.state != relayprotocol.PeerActive {
		return "", 0, remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, ErrPeerFrameInvalid
	}
	if err := validatePeerSequence(session, deviceID, envelope); err != nil {
		return "", 0, remotev1.ErrorCode_ERROR_CODE_FRAME_INVALID, err
	}
	if frameBytes > 0 {
		if frameBytes > session.maxBytes-session.forwardedBytes {
			return "", 0, remotev1.ErrorCode_ERROR_CODE_RATE_LIMITED, ErrPeerUnavailable
		}
		session.forwardedBytes += frameBytes
	}
	peerEndpointID, peerEpoch := session.sourceEndpointID, session.sourceConnectionEpoch
	if isSource {
		peerEndpointID, peerEpoch = session.targetEndpointID, session.targetConnectionEpoch
	}
	if peerError := envelope.GetPeerError(); peerError != nil && peerError.GetQueryId() == "" {
		delete(forwarder.sessions, sessionID)
	}
	return peerEndpointID, peerEpoch, remotev1.ErrorCode_ERROR_CODE_UNSPECIFIED, nil
}

func validatePeerReadyIdentity(session *peerSession, ready *remotev1.PeerReady) error {
	if session == nil || ready == nil || ready.GetSessionId() != session.id || len(ready.GetEphemeralPublicKey()) != 32 ||
		len(ready.GetIdentitySignature()) != ed25519.SignatureSize {
		return ErrPeerFrameInvalid
	}
	if err := remoteauth.VerifyPeerReadyIdentity(session.targetIdentityPublicKey, session.targetKeyThumbprint, remoteauth.PeerReadyIdentityProof{
		TicketJWTID: session.ticketJWTID, SessionID: session.id, SourceDeviceID: session.sourceDeviceID,
		TargetDeviceID: session.targetDeviceID, SourceEphemeralPublicKey: session.sourceEphemeralPublicKey,
		TargetEphemeralPublicKey: ready.GetEphemeralPublicKey(),
	}, ready.GetIdentitySignature()); err != nil {
		return ErrPeerFrameInvalid
	}
	return nil
}

func peerFrameMetadata(envelope *remotev1.Envelope, now time.Time) (sessionID, queryID string, payloadBytes uint64, err error) {
	if ready := envelope.GetPeerReady(); ready != nil {
		if uuid.Validate(ready.GetSessionId()) != nil {
			return ready.GetSessionId(), "", 0, ErrPeerFrameInvalid
		}
		return ready.GetSessionId(), "", 0, nil
	}
	if peerError := envelope.GetPeerError(); peerError != nil {
		if uuid.Validate(peerError.GetSessionId()) != nil || peerError.GetCode() == remotev1.ErrorCode_ERROR_CODE_UNSPECIFIED || len(peerError.GetQueryId()) > 128 {
			return peerError.GetSessionId(), peerError.GetQueryId(), 0, ErrPeerFrameInvalid
		}
		return peerError.GetSessionId(), peerError.GetQueryId(), 0, nil
	}
	var ciphertext *remotev1.PeerCiphertext
	switch {
	case envelope.GetPeerQuery() != nil:
		ciphertext = envelope.GetPeerQuery()
	case envelope.GetPeerDelta() != nil:
		ciphertext = envelope.GetPeerDelta()
	case envelope.GetPeerComplete() != nil:
		ciphertext = envelope.GetPeerComplete()
	case envelope.GetPeerCancel() != nil:
		ciphertext = envelope.GetPeerCancel()
	default:
		return "", "", 0, ErrPeerFrameInvalid
	}
	if uuid.Validate(ciphertext.GetSessionId()) != nil || ciphertext.GetQueryId() == "" || len(ciphertext.GetQueryId()) > 128 ||
		ciphertext.GetGeneration() == 0 || ciphertext.GetMessageSequence() == 0 || len(ciphertext.GetCiphertext()) == 0 ||
		len(ciphertext.GetCiphertext()) > relayprotocol.ControlFrameLimit {
		return ciphertext.GetSessionId(), ciphertext.GetQueryId(), 0, ErrPeerFrameInvalid
	}
	if envelope.GetPeerQuery() != nil {
		deadline := ciphertext.GetDeadline()
		if deadline == nil || !deadline.IsValid() || !deadline.AsTime().After(now) {
			return ciphertext.GetSessionId(), ciphertext.GetQueryId(), 0, ErrPeerFrameInvalid
		}
	}
	return ciphertext.GetSessionId(), ciphertext.GetQueryId(), uint64(len(ciphertext.GetCiphertext())), nil
}

func validatePeerSequence(session *peerSession, deviceID string, envelope *remotev1.Envelope) error {
	var ciphertext *remotev1.PeerCiphertext
	switch {
	case envelope.GetPeerQuery() != nil:
		ciphertext = envelope.GetPeerQuery()
	case envelope.GetPeerDelta() != nil:
		ciphertext = envelope.GetPeerDelta()
	case envelope.GetPeerComplete() != nil:
		ciphertext = envelope.GetPeerComplete()
	case envelope.GetPeerCancel() != nil:
		ciphertext = envelope.GetPeerCancel()
	default:
		return nil
	}
	key := fmt.Sprintf("%s:%d", deviceID, ciphertext.GetGeneration())
	wanted := session.lastSequences[key] + 1
	if ciphertext.GetMessageSequence() != wanted {
		return ErrPeerFrameInvalid
	}
	session.lastSequences[key] = wanted
	return nil
}

func (forwarder *PeerForwarder) forward(targetEndpointID string, targetEpoch uint64, envelope *remotev1.Envelope) error {
	if targetEndpointID == "" || targetEpoch == 0 || envelope == nil {
		return ErrPeerFrameInvalid
	}
	targetEnvelope, ok := proto.Clone(envelope).(*remotev1.Envelope)
	if !ok {
		return ErrPeerFrameInvalid
	}
	targetEnvelope.ConnectionEpoch = targetEpoch
	if err := forwarder.connections.EnqueueEndpoint(targetEndpointID, targetEpoch, targetEnvelope); err != nil {
		return ErrPeerInterrupted
	}
	return nil
}

func (forwarder *PeerForwarder) sendError(endpointID string, epoch uint64, sessionID, queryID string, code remotev1.ErrorCode, retryable bool) error {
	if endpointID == "" || epoch == 0 {
		return ErrPeerFrameInvalid
	}
	err := forwarder.connections.EnqueueEndpoint(endpointID, epoch, &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: epoch,
		Frame: &remotev1.Envelope_PeerError{PeerError: &remotev1.PeerError{
			SessionId: sessionID, QueryId: queryID, Code: code, Retryable: retryable,
		}},
	})
	if err != nil {
		// A saturated endpoint writer is backpressure on this response, not an
		// excuse to evict every unrelated logical Peer session on the socket.
		// The queue metric records it; the source can recover or reopen the
		// affected session independently.
		if errors.Is(err, ErrQueueFull) {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrPeerConnectionUnwritable, err)
	}
	return nil
}

func peerTicketErrorCode(err error) remotev1.ErrorCode {
	if errors.Is(err, remoteauth.ErrTicketExpired) {
		return remotev1.ErrorCode_ERROR_CODE_TICKET_EXPIRED
	}
	if errors.Is(err, ErrPeerOffline) {
		return remotev1.ErrorCode_ERROR_CODE_PEER_OFFLINE
	}
	return remotev1.ErrorCode_ERROR_CODE_TICKET_INVALID
}

func (forwarder *PeerForwarder) Disconnect(ctx context.Context, deviceID string) {
	forwarder.DisconnectEndpoint(ctx, deviceID, deviceID)
}

func (forwarder *PeerForwarder) DisconnectEndpoint(_ context.Context, endpointID, deviceID string) {
	forwarder.mu.Lock()
	interrupted := make([]*peerSession, 0)
	for id, session := range forwarder.sessions {
		if (session.sourceEndpointID == endpointID && session.sourceDeviceID == deviceID) ||
			(session.targetEndpointID == endpointID && session.targetDeviceID == deviceID) {
			copy := *session
			interrupted = append(interrupted, &copy)
			delete(forwarder.sessions, id)
		}
	}
	forwarder.mu.Unlock()
	for _, session := range interrupted {
		peerEndpointID, peerEpoch := session.sourceEndpointID, session.sourceConnectionEpoch
		if session.sourceEndpointID == endpointID && session.sourceDeviceID == deviceID {
			peerEndpointID, peerEpoch = session.targetEndpointID, session.targetConnectionEpoch
		}
		envelope := &remotev1.Envelope{
			ProtocolVersion: 1, ConnectionEpoch: peerEpoch,
			Frame: &remotev1.Envelope_PeerError{PeerError: &remotev1.PeerError{
				SessionId: session.id, Code: remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, Retryable: true,
			}},
		}
		_ = forwarder.connections.EnqueueEndpoint(peerEndpointID, peerEpoch, envelope)
	}
}

func (forwarder *PeerForwarder) deleteSession(sessionID string) {
	forwarder.mu.Lock()
	delete(forwarder.sessions, sessionID)
	forwarder.mu.Unlock()
}

func (forwarder *PeerForwarder) sessionCountLocked(deviceID string) int {
	count := 0
	for _, session := range forwarder.sessions {
		if session.sourceDeviceID == deviceID || session.targetDeviceID == deviceID {
			count++
		}
	}
	return count
}

func (forwarder *PeerForwarder) removeExpiredLocked(now time.Time) {
	for id, session := range forwarder.sessions {
		if (!session.expiresAt.IsZero() && !session.expiresAt.After(now)) ||
			(session.state == relayprotocol.PeerOpening && !session.openExpiresAt.After(now)) {
			delete(forwarder.sessions, id)
		}
	}
}
