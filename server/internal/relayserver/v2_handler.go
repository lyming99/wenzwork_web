package relayserver

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
)

// V2DevicePrincipal is the Relay-visible device identity after a device
// Carrier ticket and Ed25519 proof have been verified.
type V2DevicePrincipal struct {
	DeviceID          string
	UserID            string
	ConnectionEpoch   uint64
	AssignmentVersion uint64
	GrantVersion      uint64
}

type V2DeviceAuthenticator interface {
	AuthenticateV2Device(context.Context, *remotev2.CarrierEnvelope, *remotev2.CarrierHello, time.Time) (V2DevicePrincipal, error)
}

// V2GrantUseStore must be shared by Relay processes in production. It keeps
// bounded legacy Grants single-use and lets a persistent PoP Grant be reused
// until explicit revocation. A Grant ID is safe to store because it is not a
// bearer credential; implementations must never retain the Grant string.
type V2GrantUseStore interface {
	ConsumeDeviceLinkGrant(grantID string, expiresAt time.Time) (bool, error)
}

type InMemoryV2GrantUseStore struct {
	mu      sync.Mutex
	used    map[string]time.Time
	revoked map[string]time.Time
}

func NewInMemoryV2GrantUseStore() *InMemoryV2GrantUseStore {
	return &InMemoryV2GrantUseStore{used: make(map[string]time.Time), revoked: make(map[string]time.Time)}
}

func (store *InMemoryV2GrantUseStore) ConsumeDeviceLinkGrant(grantID string, expiresAt time.Time) (bool, error) {
	if store == nil || grantID == "" || expiresAt.IsZero() || !expiresAt.After(time.Now().UTC()) {
		return false, ErrV2Route
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	now := time.Now().UTC()
	for id, expiry := range store.used {
		if !expiry.After(now) {
			delete(store.used, id)
		}
	}
	for id, expiry := range store.revoked {
		if !expiry.After(now) {
			delete(store.revoked, id)
		}
	}
	if _, revoked := store.revoked[grantID]; revoked {
		return false, nil
	}
	if remoteauth.IsPersistentDeviceLinkGrantExpiry(expiresAt) {
		return true, nil
	}
	if _, replayed := store.used[grantID]; replayed {
		return false, nil
	}
	store.used[grantID] = expiresAt.UTC()
	return true, nil
}

// RevokeDeviceLinkGrant mirrors the Redis-backed Control Plane fence in
// tests. The store retains only a non-bearer grant ID, never its JWT.
func (store *InMemoryV2GrantUseStore) RevokeDeviceLinkGrant(grantID string, expiresAt time.Time) error {
	if store == nil || grantID == "" || expiresAt.IsZero() || !expiresAt.After(time.Now().UTC()) {
		return ErrV2Route
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.revoked[grantID] = expiresAt.UTC()
	return nil
}

type V2ProtocolFailure struct {
	Stage          string
	Reason         string
	Role           string
	ProtocolMajor  uint32
	FrameSizeBytes int
}

// V2Handler exposes only /v2/connect and only wenzwork-relay.v2. v1 frames,
// text WebSocket messages, query credentials and alternate subprotocols are
// rejected before they can share a Carrier state machine.
type V2Handler struct {
	CellID                string
	NodeID                string
	BrowserOriginPatterns []string
	ClientGrantVerifier   remoteauth.DeviceLinkGrantVerifier
	DeviceAuthenticator   V2DeviceAuthenticator
	GrantUses             V2GrantUseStore
	Hub                   *V2Hub
	QueueLimits           V2QueueLimits
	QueueBudget           *V2QueueBudget
	HeartbeatSeconds      uint32
	Now                   func() time.Time
	ProtocolFailure       func(V2ProtocolFailure)

	hubMu   sync.Mutex
	stateMu sync.Mutex
}

func (handler *V2Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.URL.Path != "/v2/connect" {
		http.NotFound(writer, request)
		return
	}
	if request.URL.RawQuery != "" || request.Header.Get("Authorization") != "" {
		http.Error(writer, "remote/v2 credentials are accepted only in CARRIER_HELLO", http.StatusBadRequest)
		return
	}
	if !hasOnlyV2Subprotocol(request.Header.Values("Sec-WebSocket-Protocol")) {
		http.Error(writer, "remote/v2 subprotocol required", http.StatusBadRequest)
		return
	}
	if handler == nil || handler.CellID == "" || handler.NodeID == "" || len(handler.ClientGrantVerifier.Keys) == 0 || handler.DeviceAuthenticator == nil {
		http.Error(writer, "remote/v2 relay unavailable", http.StatusServiceUnavailable)
		return
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols: []string{V2Subprotocol}, OriginPatterns: handler.browserOriginPatterns(), CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	if connection.Subprotocol() != V2Subprotocol {
		handler.recordFailure("carrier_handshake", "subprotocol_invalid", "unknown", 0, 0)
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 subprotocol required")
		return
	}
	connection.SetReadLimit(v2MaximumCarrierFrame)
	context, cancel := context.WithCancel(request.Context())
	defer cancel()
	first, size, err := readV2CarrierEnvelope(context, connection)
	if err != nil {
		handler.recordFailure("carrier_handshake", "hello_invalid", "unknown", 0, size)
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 hello required")
		return
	}
	if err := validateV2HelloEnvelope(first); err != nil {
		handler.recordFailure("carrier_handshake", "hello_invalid", "unknown", first.GetProtocolMajor(), size)
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 hello invalid")
		return
	}
	now := handler.now()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	hello := first.GetHello()
	var carrier *v2Carrier
	var claims remoteauth.DeviceLinkGrantClaims
	if hello.GetGrant() != "" {
		grantStoreUnavailable := false
		claims, err = handler.authenticateClient(first, hello, now)
		// A Device Carrier may be between allocations.  The signed Grant still
		// authenticates this Client, while the Hub keeps Link routes in their
		// recovery grace. The Hub emits a Link-scoped ROUTE_STALE when the target
		// becomes resident again; rejecting the Carrier here would turn a
		// transient handoff into a Carrier-wide re-authorisation failure.
		if err == nil {
			uses := handler.grantUseStore()
			consumed, consumeErr := uses.ConsumeDeviceLinkGrant(claims.GrantID, time.Unix(claims.ExpiresAt, 0).UTC())
			if consumeErr != nil {
				grantStoreUnavailable = true
				err = ErrV2Route
			} else if !consumed {
				err = ErrV2Route
			}
		}
		if err == nil {
			carrier, err = newV2Carrier(context, first.GetCarrierId(), first.GetCarrierEpoch(), v2CarrierClient, claims.ClientID, claims.DeviceID, connection, handler.QueueLimits, handler.queueBudget())
			if err == nil {
				err = handler.hub().attachClient(carrier)
			}
		}
		if err != nil {
			if grantStoreUnavailable {
				handler.recordFailure("carrier_handshake", "grant_store_unavailable", "client", 2, size)
				_ = connection.Close(websocket.StatusTryAgainLater, "remote/v2 grant store unavailable")
				return
			}
			handler.recordFailure("carrier_handshake", "client_authentication_failed", "client", 2, size)
			_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 client authentication failed")
			return
		}
		defer func() { handler.hub().detach(carrier); carrier.close() }()
		if err := carrier.acceptIncoming(first); err != nil {
			handler.recordFailure("carrier_handshake", "packet_sequence_invalid", "client", 2, size)
			return
		}
		if err := handler.sendReady(carrier); err != nil {
			return
		}
		heartbeat := handler.HeartbeatSeconds
		if heartbeat == 0 {
			heartbeat = v2DefaultHeartbeatSeconds
		}
		carrier.startWatchdog(heartbeat, func(reason string) {
			handler.recordFailure("carrier", reason, "client", 2, 0)
		})
		handler.serveClientCarrier(context, carrier, claims, connection)
		return
	}

	principal, authErr := handler.DeviceAuthenticator.AuthenticateV2Device(context, first, hello, now)
	if authErr != nil || principal.DeviceID == "" || principal.ConnectionEpoch == 0 || principal.DeviceID != hello.GetDeviceId() || principal.ConnectionEpoch != hello.GetDeviceConnectionEpoch() || principal.ConnectionEpoch != first.GetCarrierEpoch() {
		handler.recordFailure("carrier_handshake", "device_authentication_failed", "device", 2, size)
		_ = connection.Close(websocket.StatusPolicyViolation, "remote/v2 device authentication failed")
		return
	}
	carrier, err = newV2Carrier(context, first.GetCarrierId(), first.GetCarrierEpoch(), v2CarrierDevice, "", principal.DeviceID, connection, handler.QueueLimits, handler.queueBudget())
	route := relayrouter.Route{DeviceID: principal.DeviceID, UserID: principal.UserID, CellID: handler.CellID, NodeID: handler.NodeID, ConnectionID: first.GetCarrierId(), ConnectionEpoch: principal.ConnectionEpoch, AssignmentVersion: principal.AssignmentVersion, GrantVersion: principal.GrantVersion, ProtocolVersion: 2}
	// The authenticated route is resident in this Relay first. Runtime reports
	// it immediately in the authenticated heartbeat; Host validates the current
	// PostgreSQL assignment/grant authority and only then publishes the global
	// negotiated Redis route. Registering here would reintroduce the retired
	// projection-fence dependency and reject every device when those legacy
	// Redis keys are (correctly) absent.
	if err != nil || principal.UserID == "" || principal.AssignmentVersion == 0 || principal.GrantVersion == 0 {
		handler.recordFailure("carrier_handshake", "device_route_failed", "device", 2, size)
		_ = connection.Close(websocket.StatusTryAgainLater, "remote/v2 device route unavailable")
		return
	}
	if err := carrier.acceptIncoming(first); err != nil {
		handler.recordFailure("carrier_handshake", "packet_sequence_invalid", "device", 2, size)
		carrier.close()
		return
	}
	if err := handler.hub().attachDeviceBeforePublish(carrier, route, func() error { return handler.sendReady(carrier) }); err != nil {
		handler.recordFailure("carrier_handshake", "device_route_failed", "device", 2, size)
		carrier.close()
		_ = connection.Close(websocket.StatusTryAgainLater, "remote/v2 device route unavailable")
		return
	}
	defer func() {
		handler.hub().detach(carrier)
		carrier.close()
	}()
	heartbeat := handler.HeartbeatSeconds
	if heartbeat == 0 {
		heartbeat = v2DefaultHeartbeatSeconds
	}
	carrier.startWatchdog(heartbeat, func(reason string) {
		handler.recordFailure("carrier", reason, "device", 2, 0)
	})
	handler.serveDeviceCarrier(context, carrier, connection)
}

func (handler *V2Handler) authenticateClient(envelope *remotev2.CarrierEnvelope, hello *remotev2.CarrierHello, now time.Time) (remoteauth.DeviceLinkGrantClaims, error) {
	claims, err := handler.ClientGrantVerifier.Verify(hello.GetGrant(), now)
	if err != nil || claims.RelayNodeID != handler.NodeID || claims.RelayCellID != handler.CellID || claims.GrantID != hello.GetGrantId() ||
		claims.ClientID != hello.GetClientId() || claims.ClientIdentityKeyVersion != hello.GetClientIdentityKeyVersion() || envelope.GetCarrierEpoch() == 0 || len(hello.GetClientChallenge()) != 32 {
		return remoteauth.DeviceLinkGrantClaims{}, ErrV2Route
	}
	clientKey, err := remoteauth.DecodeIdentityPublicKey(claims.ClientIdentityKey, claims.ClientKeyThumbprint)
	if err != nil {
		return remoteauth.DeviceLinkGrantClaims{}, ErrV2Route
	}
	proof := remoteauth.CarrierProof{GrantID: claims.GrantID, CarrierID: envelope.GetCarrierId(), CarrierEpoch: envelope.GetCarrierEpoch(), Challenge: hello.GetClientChallenge()}
	if err := remoteauth.VerifyCarrierProof(ed25519.PublicKey(clientKey), proof, hello.GetClientProof()); err != nil {
		return remoteauth.DeviceLinkGrantClaims{}, ErrV2Route
	}
	return claims, nil
}

func (handler *V2Handler) serveClientCarrier(ctx context.Context, carrier *v2Carrier, claims remoteauth.DeviceLinkGrantClaims, connection *websocket.Conn) {
	for {
		envelope, size, err := readV2CarrierEnvelope(ctx, connection)
		if err != nil || carrier.acceptIncoming(envelope) != nil {
			handler.recordFailure("carrier", "frame_invalid", "client", 2, size)
			return
		}
		switch {
		case envelope.GetPing() != nil:
			if carrier.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Pong{Pong: &remotev2.CarrierPong{MonotonicMillis: envelope.GetPing().GetMonotonicMillis()}}}, v2PriorityControl) != nil {
				return
			}
		case envelope.GetLink() != nil:
			if init := envelope.GetLink().GetLinkInit(); init != nil && claims.Persistent() {
				if reason := handler.persistentGrantRejection(claims); reason != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
					if linkID, _, _ := v2LinkMetadata(envelope.GetLink()); linkID != "" {
						_ = carrier.rejectStreamWithReason(linkID, "", "", reason)
					}
					continue
				}
			}
			if routeErr := handler.hub().routeClientLink(carrier, claims, envelope.GetLink()); routeErr != nil {
				if errors.Is(routeErr, ErrV2TransientRoute) {
					handler.recordFailure("link_route", "route_stale", "client", 2, size)
					continue
				}
				handler.recordFailure("link_route", "route_invalid", "client", 2, size)
				linkID, channelID, streamID := v2LinkMetadata(envelope.GetLink())
				handler.hub().dropLinkFromCarrier(carrier, linkID)
				if linkID != "" {
					_ = carrier.rejectStream(linkID, channelID, streamID)
				}
				continue
			}
		case envelope.GetResume() != nil:
			if resumeErr := handler.hub().resumeClient(carrier, claims, envelope.GetResume()); resumeErr != nil {
				if errors.Is(resumeErr, ErrV2TransientRoute) {
					handler.recordFailure("carrier_resume", "route_stale", "client", 2, size)
					continue
				}
				handler.recordFailure("carrier_resume", "resume_invalid", "client", 2, size)
				if linkID := envelope.GetResume().GetLinkId(); linkID != "" {
					_ = carrier.rejectStreamWithReason(linkID, "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED)
				}
				continue
			}
		case envelope.GetStreamRejected() != nil:
			if handler.hub().routeStreamRejected(carrier, envelope.GetStreamRejected()) != nil {
				handler.recordFailure("stream_rejected", "route_invalid", "client", 2, size)
			}
		case envelope.GetPong() != nil:
			// A Carrier pong is liveness metadata only.
		default:
			handler.recordFailure("carrier", "frame_invalid", "client", 2, size)
			return
		}
	}
}

// persistentGrantRejection rechecks explicit revocation for every new LinkInit
// on an already healthy Client Carrier. Revocation intentionally leaves an
// established Link alone, but it must prevent that Carrier from minting a
// later Link with the cached long-lived Grant.
func (handler *V2Handler) persistentGrantRejection(claims remoteauth.DeviceLinkGrantClaims) remotev2.ProtocolErrorCode {
	if handler == nil || !claims.Persistent() {
		return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED
	}
	accepted, err := handler.grantUseStore().ConsumeDeviceLinkGrant(claims.GrantID, time.Unix(claims.ExpiresAt, 0).UTC())
	if err != nil {
		// A revocation-store outage is an availability failure, not evidence
		// that the signed Grant changed. Ask the Client to retry without
		// discarding its protected in-memory credential.
		return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_BACKPRESSURE
	}
	if !accepted {
		return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_REVOKED
	}
	return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED
}

func (handler *V2Handler) serveDeviceCarrier(ctx context.Context, carrier *v2Carrier, connection *websocket.Conn) {
	for {
		envelope, size, err := readV2CarrierEnvelope(ctx, connection)
		if err != nil || carrier.acceptIncoming(envelope) != nil {
			handler.recordFailure("carrier", "frame_invalid", "device", 2, size)
			return
		}
		switch {
		case envelope.GetPing() != nil:
			if carrier.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Pong{Pong: &remotev2.CarrierPong{MonotonicMillis: envelope.GetPing().GetMonotonicMillis()}}}, v2PriorityControl) != nil {
				return
			}
		case envelope.GetLink() != nil:
			if routeErr := handler.hub().routeDeviceLink(carrier, envelope.GetLink()); routeErr != nil {
				if errors.Is(routeErr, ErrV2TransientRoute) {
					handler.recordFailure("link_route", "route_stale", "device", 2, size)
					continue
				}
				handler.recordFailure("link_route", "route_invalid", "device", 2, size)
				linkID, channelID, streamID := v2LinkMetadata(envelope.GetLink())
				handler.hub().dropLinkFromCarrier(carrier, linkID)
				if linkID != "" {
					_ = carrier.rejectStream(linkID, channelID, streamID)
				}
				continue
			}
		case envelope.GetResume() != nil:
			if resumeErr := handler.hub().resumeDevice(carrier, envelope.GetResume()); resumeErr != nil {
				if errors.Is(resumeErr, ErrV2TransientRoute) {
					handler.recordFailure("carrier_resume", "route_stale", "device", 2, size)
					continue
				}
				handler.recordFailure("carrier_resume", "resume_invalid", "device", 2, size)
				if linkID := envelope.GetResume().GetLinkId(); linkID != "" {
					_ = carrier.rejectStreamWithReason(linkID, "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED)
				}
				continue
			}
		case envelope.GetStreamRejected() != nil:
			if handler.hub().routeStreamRejected(carrier, envelope.GetStreamRejected()) != nil {
				handler.recordFailure("stream_rejected", "route_invalid", "device", 2, size)
			}
		case envelope.GetPong() != nil:
		default:
			handler.recordFailure("carrier", "frame_invalid", "device", 2, size)
			return
		}
	}
}

func (handler *V2Handler) sendReady(carrier *v2Carrier) error {
	heartbeat := handler.HeartbeatSeconds
	if heartbeat == 0 {
		heartbeat = 25
	}
	limits := handler.QueueLimits.normalized()
	return carrier.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ready{Ready: &remotev2.CarrierReady{
		CarrierId: carrier.id, CarrierEpoch: carrier.epoch, HeartbeatIntervalSeconds: heartbeat,
		ControlQueueByteLimit: uint32(limits.ControlBytes), InteractiveQueueByteLimit: uint32(limits.InteractiveBytes), BulkQueueByteLimit: uint32(limits.BulkBytes),
	}}}, v2PriorityControl)
}

func (handler *V2Handler) hub() *V2Hub {
	handler.hubMu.Lock()
	defer handler.hubMu.Unlock()
	if handler.Hub == nil {
		handler.Hub = NewV2Hub()
	}
	return handler.Hub
}

func (handler *V2Handler) grantUseStore() V2GrantUseStore {
	handler.stateMu.Lock()
	defer handler.stateMu.Unlock()
	if handler.GrantUses == nil {
		handler.GrantUses = NewInMemoryV2GrantUseStore()
	}
	return handler.GrantUses
}

func (handler *V2Handler) queueBudget() *V2QueueBudget {
	if handler == nil {
		return nil
	}
	handler.stateMu.Lock()
	defer handler.stateMu.Unlock()
	if handler.QueueBudget == nil {
		handler.QueueBudget = newDefaultV2QueueBudget()
	}
	return handler.QueueBudget
}

func (handler *V2Handler) QueueBudgetSnapshot() V2QueueBudgetSnapshot {
	if handler == nil {
		return V2QueueBudgetSnapshot{}
	}
	return handler.queueBudget().Snapshot()
}

func (handler *V2Handler) browserOriginPatterns() []string {
	handler.stateMu.Lock()
	defer handler.stateMu.Unlock()
	return append([]string(nil), handler.BrowserOriginPatterns...)
}

func (handler *V2Handler) now() time.Time {
	if handler.Now != nil {
		return handler.Now().UTC()
	}
	return time.Now().UTC()
}

func (handler *V2Handler) UpdateBrowserOriginPatterns(patterns []string) {
	if handler == nil {
		return
	}
	handler.stateMu.Lock()
	defer handler.stateMu.Unlock()
	handler.BrowserOriginPatterns = append([]string(nil), patterns...)
}

func (handler *V2Handler) recordFailure(stage, reason, role string, protocolMajor uint32, frameSize int) {
	if handler != nil && handler.ProtocolFailure != nil {
		handler.ProtocolFailure(V2ProtocolFailure{Stage: stage, Reason: reason, Role: role, ProtocolMajor: protocolMajor, FrameSizeBytes: frameSize})
	}
}

func readV2CarrierEnvelope(ctx context.Context, connection *websocket.Conn) (*remotev2.CarrierEnvelope, int, error) {
	messageType, payload, err := connection.Read(ctx)
	if err != nil {
		return nil, 0, err
	}
	if messageType != websocket.MessageBinary || len(payload) == 0 || len(payload) > v2MaximumCarrierFrame {
		return nil, len(payload), ErrV2Route
	}
	envelope := new(remotev2.CarrierEnvelope)
	if err := proto.Unmarshal(payload, envelope); err != nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return nil, len(payload), ErrV2Route
	}
	return envelope, len(payload), nil
}

func validateV2HelloEnvelope(envelope *remotev2.CarrierEnvelope) error {
	if envelope == nil || envelope.GetProtocolMajor() != 2 || envelope.GetCarrierId() == "" || uuid.Validate(envelope.GetCarrierId()) != nil || envelope.GetCarrierEpoch() == 0 ||
		envelope.GetPacketSequence() != 1 || envelope.GetAcknowledgedSequence() != 0 || envelope.GetHello() == nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return ErrV2Route
	}
	hello := envelope.GetHello()
	isClient := strings.TrimSpace(hello.GetGrant()) != ""
	isDevice := strings.TrimSpace(hello.GetDeviceConnectionTicket()) != ""
	if isClient == isDevice {
		return ErrV2Route
	}
	if isClient {
		if hello.GetGrantId() == "" || hello.GetClientId() == "" || hello.GetClientIdentityKeyVersion() == 0 || len(hello.GetClientChallenge()) != 32 || len(hello.GetClientProof()) != ed25519.SignatureSize ||
			hello.GetDeviceId() != "" || hello.GetDeviceConnectionEpoch() != 0 || len(hello.GetDeviceProof()) != 0 {
			return ErrV2Route
		}
		return nil
	}
	if hello.GetDeviceId() == "" || hello.GetDeviceConnectionEpoch() == 0 || len(hello.GetClientChallenge()) != 32 || len(hello.GetDeviceProof()) != ed25519.SignatureSize ||
		hello.GetGrantId() != "" || hello.GetClientId() != "" || hello.GetClientIdentityKeyVersion() != 0 || len(hello.GetClientProof()) != 0 {
		return ErrV2Route
	}
	return nil
}

func hasOnlyV2Subprotocol(values []string) bool {
	var protocols []string
	for _, value := range values {
		for _, protocol := range strings.Split(value, ",") {
			if protocol = strings.TrimSpace(protocol); protocol != "" {
				protocols = append(protocols, protocol)
			}
		}
	}
	return len(protocols) == 1 && protocols[0] == V2Subprotocol
}
