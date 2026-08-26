package main

// The v2 target transport intentionally lives beside (rather than inside) the
// legacy Peer implementation.  The Agent starts only this path; retaining the
// old code for migration fixtures does not give the running device a protocol
// downgrade path.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	peerv2 "github.com/wenzwork/wenzwork-web/server/internal/peerprotocol/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	v2AgentMaximumCarrierFrame   = 4 << 20
	v2AgentControlQueueBytes     = 512 << 10
	v2AgentInteractiveBytes      = 2 << 20
	v2AgentBulkQueueBytes        = 8 << 20
	v2AgentQueueFrames           = 256
	v2LinkRecoveryTTL            = 5 * time.Minute
	v2LinkLeaseRenewalInterval   = 30 * time.Minute
	v2LinkLeaseDuration          = 90 * time.Minute
	v2ControlChannelID           = "v2-control"
	v2ControlStreamID            = "v2-control"
	v2ChannelControlStreamID     = "v2-channel-control"
	v2MaximumLinksPerDevice      = 8
	v2MaximumChannelsPerLink     = 32
	v2MaximumStreamsPerChannel   = 64
	v2MaximumCachedOperations    = 128
	v2CachedOperationTTL         = 5 * time.Minute
	v2MaximumRPCsPerLink         = 128
	v2MaximumRPCsPerController   = 128
	v2MaximumRPCsPerDevice       = 512
	v2MaximumOperationBytes      = 4 << 20
	v2MaximumOperationEvents     = 64
	v2MaximumOperationEventBytes = 256 << 10
)

// Carrier Ping/Pong owns physical liveness. Event heartbeats only reconcile a
// quiet durable cursor, so the maximum negotiated cadence avoids needless
// AEAD/JSON/socket wakeups without delaying real events (which remain push).
const v2EventHeartbeatSeconds = 60

var v2EventHeartbeatInterval = v2EventHeartbeatSeconds * time.Second

var (
	errV2AgentCarrier      = errors.New("remote/v2 carrier is invalid")
	errV2AgentLink         = errors.New("remote/v2 link is invalid")
	errV2AgentLinkConflict = errors.New("remote/v2 controller link registry conflict")
	errV2AgentLinkCapacity = errors.New("remote/v2 device link capacity is exhausted")
	errV2AgentLinkAuth     = errors.New("remote/v2 link authentication failed")
	errV2AgentStream       = errors.New("remote/v2 stream is unavailable")
	errV2AgentReplay       = errors.New("remote/v2 authenticated record was replayed")
	errV2AgentBackpressure = errors.New("remote/v2 carrier queue is full")
)

type v2AgentGoAwayError struct {
	reason     remotev2.ProtocolErrorCode
	retryAfter time.Duration
}

func (failure *v2AgentGoAwayError) Error() string {
	return "remote/v2 Relay requested reconnect"
}

func v2FailureRetryAfter(err error) time.Duration {
	var httpFailure *controlHTTPError
	if errors.As(err, &httpFailure) && httpFailure.RetryAfter > 0 {
		return httpFailure.RetryAfter
	}
	var goAway *v2AgentGoAwayError
	if errors.As(err, &goAway) && goAway.retryAfter > 0 {
		return goAway.retryAfter
	}
	return 0
}

// deviceLinkGrantTrustBundle is delivered over the authenticated Device
// control channel as part of the allocation response. It is never read from a
// browser response or Relay URL.
type deviceLinkGrantTrustBundle struct {
	Issuer string               `json:"issuer"`
	Keys   []peerTicketTrustKey `json:"keys"`
}

func verifierFromDeviceLinkGrantTrustBundle(bundle deviceLinkGrantTrustBundle) (remoteauth.DeviceLinkGrantVerifier, error) {
	if bundle.Issuer == "" || bundle.Issuer != strings.TrimSpace(bundle.Issuer) || len(bundle.Issuer) > 128 || len(bundle.Keys) == 0 || len(bundle.Keys) > 8 {
		return remoteauth.DeviceLinkGrantVerifier{}, errV2AgentCarrier
	}
	keys := make(map[string]ed25519.PublicKey, len(bundle.Keys))
	for _, entry := range bundle.Keys {
		if entry.Algorithm != "Ed25519" || entry.KeyID == "" || len(entry.KeyID) > 64 || !validTrustKeyID(entry.KeyID) {
			return remoteauth.DeviceLinkGrantVerifier{}, errV2AgentCarrier
		}
		decoded, err := decodeV2TrustKey(entry.PublicKey)
		if err != nil {
			return remoteauth.DeviceLinkGrantVerifier{}, errV2AgentCarrier
		}
		if _, exists := keys[entry.KeyID]; exists {
			return remoteauth.DeviceLinkGrantVerifier{}, errV2AgentCarrier
		}
		keys[entry.KeyID] = decoded
	}
	return remoteauth.DeviceLinkGrantVerifier{Issuer: bundle.Issuer, Keys: keys, Leeway: 5 * time.Second}, nil
}

func decodeV2TrustKey(encoded string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, errV2AgentCarrier
	}
	return ed25519.PublicKey(append([]byte(nil), decoded...)), nil
}

// v2AgentCarrier has exactly one WebSocket writer. Packet sequence numbers
// are allocated by that writer after priority selection, so a control frame
// cannot overtake a bulk frame while retaining the bulk frame's old sequence.
type v2AgentCarrier struct {
	socket               *websocket.Conn
	id                   string
	epoch                uint64
	packetMu             sync.Mutex
	nextOut              uint64
	lastIn               uint64
	lastPeerAcknowledged uint64
	lastAckProgress      atomic.Int64
	writer               *v2AgentWriter
	closeOnce            sync.Once
}

type v2AgentPriority uint8

const (
	v2AgentControl v2AgentPriority = iota + 1
	v2AgentInteractive
	v2AgentBulk
)

type v2AgentWriteRequest struct {
	envelope  *remotev2.CarrierEnvelope
	priority  v2AgentPriority
	bytes     int
	result    chan error
	cancelled atomic.Bool
}

type v2AgentWriter struct {
	carrier       *v2AgentCarrier
	mu            sync.Mutex
	queues        map[v2AgentPriority][]*v2AgentWriteRequest
	bytes         map[v2AgentPriority]int
	frames        map[v2AgentPriority]int
	notify        chan struct{}
	done          chan struct{}
	closed        bool
	scheduleIndex int
}

var v2AgentPrioritySchedule = [...]v2AgentPriority{
	v2AgentControl, v2AgentControl, v2AgentControl, v2AgentControl,
	v2AgentInteractive, v2AgentInteractive,
	v2AgentBulk,
}

var v2AgentMonotonicOrigin = time.Now()

func v2AgentMonotonicNanos() int64 {
	return time.Since(v2AgentMonotonicOrigin).Nanoseconds()
}

func newV2AgentCarrier(socket *websocket.Conn, id string, epoch uint64) (*v2AgentCarrier, error) {
	if socket == nil || uuid.Validate(id) != nil || epoch == 0 {
		return nil, errV2AgentCarrier
	}
	carrier := &v2AgentCarrier{socket: socket, id: id, epoch: epoch}
	carrier.lastAckProgress.Store(v2AgentMonotonicNanos())
	carrier.writer = &v2AgentWriter{
		carrier: carrier, queues: make(map[v2AgentPriority][]*v2AgentWriteRequest), bytes: make(map[v2AgentPriority]int), frames: make(map[v2AgentPriority]int),
		notify: make(chan struct{}, 1), done: make(chan struct{}),
	}
	go carrier.writer.run()
	return carrier, nil
}

func (carrier *v2AgentCarrier) close() {
	if carrier == nil {
		return
	}
	carrier.closeOnce.Do(func() {
		if carrier.writer != nil {
			carrier.writer.close()
		}
		if carrier.socket != nil {
			carrier.socket.CloseNow()
		}
	})
}

func (carrier *v2AgentCarrier) send(ctx context.Context, envelope *remotev2.CarrierEnvelope, priority v2AgentPriority) error {
	if carrier == nil || carrier.writer == nil || envelope == nil || envelope.Body == nil {
		return errV2AgentCarrier
	}
	return carrier.writer.enqueue(ctx, envelope, priority)
}

func (carrier *v2AgentCarrier) sendLink(ctx context.Context, link *remotev2.LinkEnvelope) error {
	if link == nil {
		return errV2AgentCarrier
	}
	priority := v2AgentControl
	if encrypted := link.GetEncrypted(); encrypted != nil {
		switch encrypted.GetFrameType() {
		case remotev2.FrameType_FRAME_TYPE_FILE_CHUNK:
			priority = v2AgentBulk
		case remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, remotev2.FrameType_FRAME_TYPE_RPC_EVENT, remotev2.FrameType_FRAME_TYPE_STREAM_DATA:
			priority = v2AgentInteractive
		}
	}
	return carrier.send(ctx, &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: link}}, priority)
}

func (carrier *v2AgentCarrier) acceptIncoming(envelope *remotev2.CarrierEnvelope) error {
	if carrier == nil || envelope == nil || envelope.GetProtocolMajor() != 2 || envelope.GetCarrierId() != carrier.id || envelope.GetCarrierEpoch() != carrier.epoch ||
		envelope.GetPacketSequence() == 0 || envelope.Body == nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return errV2AgentCarrier
	}
	carrier.packetMu.Lock()
	defer carrier.packetMu.Unlock()
	if envelope.GetPacketSequence() != carrier.lastIn+1 || envelope.GetAcknowledgedSequence() > carrier.nextOut {
		return errV2AgentCarrier
	}
	carrier.lastIn = envelope.GetPacketSequence()
	if acknowledged := envelope.GetAcknowledgedSequence(); acknowledged > carrier.lastPeerAcknowledged {
		carrier.lastPeerAcknowledged = acknowledged
		carrier.lastAckProgress.Store(v2AgentMonotonicNanos())
	}
	return nil
}

func (writer *v2AgentWriter) enqueue(ctx context.Context, envelope *remotev2.CarrierEnvelope, priority v2AgentPriority) error {
	if writer == nil || envelope == nil || envelope.Body == nil || priority < v2AgentControl || priority > v2AgentBulk {
		return errV2AgentCarrier
	}
	if ctx == nil {
		ctx = context.Background()
	}
	bytes := proto.Size(envelope) + 96
	if bytes <= 0 || bytes > v2AgentMaximumCarrierFrame {
		return errV2AgentCarrier
	}
	writer.mu.Lock()
	if writer.closed {
		writer.mu.Unlock()
		return errV2AgentCarrier
	}
	if writer.frames[priority] >= v2AgentQueueFrames || writer.bytes[priority]+bytes > v2AgentQueueByteLimit(priority) {
		writer.mu.Unlock()
		return errV2AgentBackpressure
	}
	clone, ok := proto.Clone(envelope).(*remotev2.CarrierEnvelope)
	if !ok {
		writer.mu.Unlock()
		return errV2AgentCarrier
	}
	request := &v2AgentWriteRequest{envelope: clone, priority: priority, bytes: bytes, result: make(chan error, 1)}
	writer.queues[priority] = append(writer.queues[priority], request)
	writer.bytes[priority] += bytes
	writer.frames[priority]++
	writer.mu.Unlock()
	writer.signal()
	select {
	case err := <-request.result:
		return err
	case <-writer.done:
		return errV2AgentCarrier
	case <-ctx.Done():
		request.cancelled.Store(true)
		writer.signal()
		return ctx.Err()
	}
}

func (writer *v2AgentWriter) run() {
	for {
		request, ok := writer.dequeue()
		if !ok {
			select {
			case <-writer.done:
				return
			case <-writer.notify:
				continue
			}
		}
		if request.cancelled.Load() {
			writer.deliver(request, context.Canceled)
			continue
		}
		carrier := writer.carrier
		carrier.packetMu.Lock()
		if carrier.nextOut == ^uint64(0) {
			carrier.packetMu.Unlock()
			writer.deliver(request, errV2AgentCarrier)
			carrier.close()
			return
		}
		hadNoOutstandingPackets := carrier.lastPeerAcknowledged == carrier.nextOut
		carrier.nextOut++
		if hadNoOutstandingPackets {
			carrier.lastAckProgress.Store(v2AgentMonotonicNanos())
		}
		sequence, acknowledged := carrier.nextOut, carrier.lastIn
		carrier.packetMu.Unlock()
		envelope := request.envelope
		if envelope == nil {
			writer.deliver(request, errV2AgentCarrier)
			carrier.close()
			return
		}
		envelope.ProtocolMajor, envelope.CarrierId, envelope.CarrierEpoch = 2, carrier.id, carrier.epoch
		envelope.PacketSequence, envelope.AcknowledgedSequence = sequence, acknowledged
		payload, err := proto.Marshal(envelope)
		if err == nil && (len(payload) == 0 || len(payload) > v2AgentMaximumCarrierFrame) {
			err = errV2AgentCarrier
		}
		if err == nil {
			err = carrier.socket.Write(context.Background(), websocket.MessageBinary, payload)
		}
		if err != nil {
			writer.deliver(request, errV2AgentCarrier)
			carrier.close()
			return
		}
		writer.deliver(request, nil)
	}
}

func (writer *v2AgentWriter) dequeue() (*v2AgentWriteRequest, bool) {
	if writer == nil {
		return nil, false
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	for offset := 0; offset < len(v2AgentPrioritySchedule); offset++ {
		index := (writer.scheduleIndex + offset) % len(v2AgentPrioritySchedule)
		priority := v2AgentPrioritySchedule[index]
		queue := writer.queues[priority]
		if len(queue) == 0 {
			continue
		}
		request := queue[0]
		writer.queues[priority] = queue[1:]
		writer.bytes[priority] -= request.bytes
		writer.frames[priority]--
		writer.scheduleIndex = (index + 1) % len(v2AgentPrioritySchedule)
		return request, true
	}
	return nil, false
}

func (writer *v2AgentWriter) close() {
	if writer == nil {
		return
	}
	writer.mu.Lock()
	if !writer.closed {
		writer.closed = true
		writer.queues = make(map[v2AgentPriority][]*v2AgentWriteRequest)
		writer.bytes = make(map[v2AgentPriority]int)
		writer.frames = make(map[v2AgentPriority]int)
		close(writer.done)
	}
	writer.mu.Unlock()
}

func (writer *v2AgentWriter) signal() {
	select {
	case writer.notify <- struct{}{}:
	default:
	}
}

func (writer *v2AgentWriter) deliver(request *v2AgentWriteRequest, err error) {
	if request == nil {
		return
	}
	select {
	case request.result <- err:
	default:
	}
}

func v2AgentQueueByteLimit(priority v2AgentPriority) int {
	switch priority {
	case v2AgentControl:
		return v2AgentControlQueueBytes
	case v2AgentInteractive:
		return v2AgentInteractiveBytes
	default:
		return v2AgentBulkQueueBytes
	}
}

func runTargetRelayLoopV2(ctx context.Context, client *http.Client, tokens *deviceTokenManager, state *agentState) error {
	if ctx == nil || client == nil || tokens == nil || state == nil {
		return errV2AgentCarrier
	}
	links := newV2AgentLinkRegistry(state)
	defer links.close()
	var pendingEpoch uint64
	var allocationRequestID string
	for attempt := 0; ; {
		connectedThisAttempt := false
		if err := ctx.Err(); err != nil {
			return err
		}
		if pendingEpoch == 0 {
			var err error
			pendingEpoch, err = state.advanceConnectionEpoch()
			if err != nil {
				return err
			}
			allocationRequestID = "allocation-v2-" + uuid.NewString()
		}
		epoch := pendingEpoch
		state.recordConnectionDiagnostic("relay_allocation_requested", "requested", epoch, attempt, 0, 0)
		var allocation allocationResponse
		var err error
		for allocationAttempt := 0; allocationAttempt < 2; allocationAttempt++ {
			err = tokens.doJSON(ctx, http.MethodPost, "/v1/device/relay-allocations", allocationRequestID, map[string]any{
				"remoteDeviceId": state.DeviceID, "protocolMin": 2, "protocolMax": 2, "connectionEpoch": epoch,
			}, &allocation)
			if err == nil || !errors.Is(err, errControlRetryable) || allocationAttempt == 1 {
				break
			}
			var httpErr *controlHTTPError
			wait := time.Duration(0)
			if errors.As(err, &httpErr) {
				wait = httpErr.RetryAfter
			}
			if wait <= 0 {
				wait = v2FullJitter(500 * time.Millisecond)
			} else {
				wait += v2FullJitter(time.Second)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if err == nil && (allocation.AssignmentID == uuid.Nil || allocation.Primary.CellID == uuid.Nil || allocation.ConnectionTicket == "" ||
			!allocation.TicketExpiresAt.After(time.Now().UTC().Add(time.Second))) {
			err = errV2AgentCarrier
		}
		grantVerifier := remoteauth.DeviceLinkGrantVerifier{}
		if err == nil {
			grantVerifier, err = verifierFromDeviceLinkGrantTrustBundle(allocation.DeviceLinkGrantTrust)
		}
		if err == nil || !errors.Is(err, errControlRetryable) {
			// A valid response, including a definite rejection, completes this
			// idempotent allocation operation. Unknown/retryable outcomes retain
			// the same Epoch and key across outer retries so a lost response cannot
			// amplify allocation rows or fencing epochs.
			pendingEpoch = 0
			allocationRequestID = ""
		}
		if err != nil {
			state.recordConnectionDiagnostic(
				"relay_allocation_failed", remoteConnectionDiagnosticReason(err), epoch,
				attempt, 0, 0,
			)
		}
		if err == nil {
			state.recordConnectionDiagnostic("relay_allocation_received", "allocated", epoch, attempt, 0, 0)
			carrier, heartbeat, dialErr := dialTargetRelayV2(ctx, client, allocation, state, epoch)
			if dialErr == nil {
				connectedThisAttempt = true
				state.recordConnectionDiagnostic("relay_connected", "ready", epoch, attempt, 0, heartbeat)
				links.bindCarrier(carrier)
				serveContext, cancel := targetRelayServeContext(ctx, allocation.AssignmentLeaseExpiresAt)
				dialErr = serveTargetV2(serveContext, carrier, heartbeat, links, grantVerifier)
				cancel()
				links.unbindCarrier(carrier)
				carrier.close()
				err = dialErr
				state.recordConnectionDiagnostic("relay_disconnected", remoteConnectionDiagnosticReason(err), epoch, attempt, 0, heartbeat)
			} else {
				err = dialErr
				state.recordConnectionDiagnostic("relay_dial_failed", remoteConnectionDiagnosticReason(err), epoch, attempt, 0, 0)
			}
		}
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errDeviceAuthentication) {
			return err
		}
		backoffAttempt := attempt
		if connectedThisAttempt {
			// A successful Carrier proves the route is healthy. A later network
			// break must regain the fast first-retry slot instead of inheriting a
			// months-old capped attempt counter.
			backoffAttempt = 0
		}
		backoff := v2FullJitter(v2BackoffCap(backoffAttempt))
		if serverMinimum := v2FailureRetryAfter(err); serverMinimum > 0 {
			// Retry-After and GOAWAY are lower bounds. Add a small spread after the
			// boundary so a drained Relay does not release every Agent at once.
			backoff = serverMinimum + v2FullJitter(time.Second)
		}
		state.recordConnectionDiagnostic("relay_reconnect_scheduled", remoteConnectionDiagnosticReason(err), epoch, attempt, backoff, 0)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if connectedThisAttempt {
			attempt = 0
		} else if attempt < 16 {
			attempt++
		}
	}
}

func v2BackoffCap(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 6 {
		attempt = 6
	}
	cap := 500 * time.Millisecond * time.Duration(1<<attempt)
	if cap > 30*time.Second {
		return 30 * time.Second
	}
	return cap
}

func v2FullJitter(bound time.Duration) time.Duration {
	if bound <= 0 {
		return 0
	}
	floor := min(100*time.Millisecond, bound)
	if floor == bound {
		return bound
	}
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return floor + (bound-floor)/2
	}
	value := binary.LittleEndian.Uint64(bytes[:])
	return floor + time.Duration(value%uint64(bound-floor+1))
}

func dialTargetRelayV2(ctx context.Context, client *http.Client, allocation allocationResponse, state *agentState, epoch uint64) (*v2AgentCarrier, time.Duration, error) {
	endpoints := append([]allocationEndpoint{allocation.Primary}, allocation.Fallbacks...)
	var lastErr error
	for _, endpoint := range endpoints {
		carrier, heartbeat, err := dialTargetV2Endpoint(ctx, client, endpoint, allocation.ConnectionTicket, state, epoch)
		if err == nil {
			return carrier, heartbeat, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errV2AgentCarrier
	}
	return nil, 0, lastErr
}

func dialTargetV2Endpoint(ctx context.Context, client *http.Client, endpoint allocationEndpoint, ticket string, state *agentState, epoch uint64) (*v2AgentCarrier, time.Duration, error) {
	if !validV2RelayEndpoint(endpoint.URL) || state == nil || epoch == 0 {
		return nil, 0, errV2AgentCarrier
	}
	claims, err := parseSignedClaims(ticket)
	if err != nil || claims.JWTID == "" || claims.ProtocolMin > 2 || claims.ProtocolMax < 2 {
		return nil, 0, errV2AgentCarrier
	}
	carrierID := uuid.NewString()
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return nil, 0, err
	}
	proof, err := remoteauth.SignCarrierProof(state.identity, remoteauth.CarrierProof{GrantID: claims.JWTID, CarrierID: carrierID, CarrierEpoch: epoch, Challenge: challenge})
	if err != nil {
		return nil, 0, err
	}
	parsed, _ := url.Parse(endpoint.URL)
	socket, response, err := websocket.Dial(ctx, parsed.String(), &websocket.DialOptions{
		HTTPClient: client, Subprotocols: []string{"wenzwork-relay.v2"}, CompressionMode: websocket.CompressionDisabled,
	})
	dialFailure := v2RelayDialFailure(response)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, 0, dialFailure
	}
	socket.SetReadLimit(v2AgentMaximumCarrierFrame)
	carrier, err := newV2AgentCarrier(socket, carrierID, epoch)
	if err != nil {
		socket.CloseNow()
		return nil, 0, err
	}
	fail := func(cause error) (*v2AgentCarrier, time.Duration, error) {
		carrier.close()
		return nil, 0, cause
	}
	if err := carrier.send(ctx, &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Hello{Hello: &remotev2.CarrierHello{
		DeviceConnectionTicket: ticket, DeviceId: state.DeviceID.String(), DeviceConnectionEpoch: epoch,
		ClientChallenge: challenge, DeviceProof: proof,
	}}}, v2AgentControl); err != nil {
		return fail(err)
	}
	readyEnvelope, err := readV2AgentEnvelope(ctx, socket)
	if err != nil || carrier.acceptIncoming(readyEnvelope) != nil || readyEnvelope.GetReady() == nil ||
		readyEnvelope.GetReady().GetCarrierId() != carrierID || readyEnvelope.GetReady().GetCarrierEpoch() != epoch ||
		readyEnvelope.GetReady().GetHeartbeatIntervalSeconds() == 0 {
		return fail(errV2AgentCarrier)
	}
	return carrier, time.Duration(readyEnvelope.GetReady().GetHeartbeatIntervalSeconds()) * time.Second, nil
}

func v2RelayDialFailure(response *http.Response) error {
	if response == nil || (response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500) {
		return errV2AgentCarrier
	}
	return &controlHTTPError{
		Status: response.StatusCode, Code: "relay_connect_rejected",
		RetryAfter: parseControlRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC()),
	}
}

func validV2RelayEndpoint(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	return err == nil && (parsed.Scheme == "ws" || parsed.Scheme == "wss") && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "/v2/connect" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func readV2AgentEnvelope(ctx context.Context, socket *websocket.Conn) (*remotev2.CarrierEnvelope, error) {
	if socket == nil {
		return nil, errV2AgentCarrier
	}
	messageType, payload, err := socket.Read(ctx)
	if err != nil || messageType != websocket.MessageBinary || len(payload) == 0 || len(payload) > v2AgentMaximumCarrierFrame {
		return nil, errV2AgentCarrier
	}
	envelope := new(remotev2.CarrierEnvelope)
	if err := proto.Unmarshal(payload, envelope); err != nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return nil, errV2AgentCarrier
	}
	return envelope, nil
}

type v2AgentLinkRegistry struct {
	state                   *agentState
	mu                      sync.Mutex
	carrier                 *v2AgentCarrier
	links                   map[string]*v2AgentLink
	rpcInFlight             int
	rpcInFlightByLink       map[string]int
	rpcInFlightByController map[string]int
}

func newV2AgentLinkRegistry(state *agentState) *v2AgentLinkRegistry {
	registry := &v2AgentLinkRegistry{
		state: state, links: make(map[string]*v2AgentLink),
		rpcInFlightByLink: make(map[string]int), rpcInFlightByController: make(map[string]int),
	}
	if state != nil {
		state.servicesMu.Lock()
		state.v2Links = registry
		state.servicesMu.Unlock()
	}
	return registry
}

// v2AgentResourceSnapshot is a content-free, bounded view used by capability
// diagnostics. It contains counts only: no Link/Channel/Stream identifiers,
// methods, payloads, tickets or cryptographic material.
type v2AgentResourceSnapshot struct {
	LinkCount                   int `json:"linkCount"`
	ChannelCount                int `json:"channelCount"`
	ActiveStreamCount           int `json:"activeStreamCount"`
	SequencerOutboundEntries    int `json:"sequencerOutboundEntries"`
	SequencerInboundEntries     int `json:"sequencerInboundEntries"`
	SequencerTombstones         int `json:"sequencerTombstones"`
	SequencerActiveStreams      int `json:"sequencerActiveStreams"`
	SequencerUsedStreamIDs      int `json:"sequencerUsedStreamIds"`
	SequencerKeyCount           int `json:"sequencerKeyCount"`
	OutboundStreamLocks         int `json:"outboundStreamLocks"`
	CachedOperationCount        int `json:"cachedOperationCount"`
	CachedOperationBytes        int `json:"cachedOperationBytes"`
	CarrierQueuedFrames         int `json:"carrierQueuedFrames"`
	CarrierQueuedBytes          int `json:"carrierQueuedBytes"`
	RPCInFlight                 int `json:"rpcInFlight"`
	OperationInFlight           int `json:"operationInFlight"`
	FileTransferCount           int `json:"fileTransferCount"`
	EventSubscriptionCount      int `json:"eventSubscriptionCount"`
	EventQueuedBytes            int `json:"eventQueuedBytes"`
	SequencerTombstoneHardLimit int `json:"sequencerTombstoneHardLimit"`
	SequencerActiveHardLimit    int `json:"sequencerActiveHardLimit"`
	SequencerStreamHardLimit    int `json:"sequencerStreamHardLimit"`
}

func (registry *v2AgentLinkRegistry) resourceSnapshot() v2AgentResourceSnapshot {
	snapshot := v2AgentResourceSnapshot{
		SequencerTombstoneHardLimit: peerv2.DefaultSequencerTombstoneLimit,
		SequencerActiveHardLimit:    peerv2.DefaultSequencerActiveLimit,
		SequencerStreamHardLimit:    peerv2.DefaultSequencerStreamLimit,
	}
	if registry == nil {
		return snapshot
	}
	registry.mu.Lock()
	snapshot.RPCInFlight = registry.rpcInFlight
	carrier := registry.carrier
	links := make([]*v2AgentLink, 0, len(registry.links))
	for _, link := range registry.links {
		if link != nil {
			links = append(links, link)
		}
	}
	registry.mu.Unlock()
	if carrier != nil && carrier.writer != nil {
		carrier.writer.mu.Lock()
		for _, frames := range carrier.writer.frames {
			snapshot.CarrierQueuedFrames += frames
		}
		for _, bytes := range carrier.writer.bytes {
			snapshot.CarrierQueuedBytes += bytes
		}
		carrier.writer.mu.Unlock()
	}
	snapshot.LinkCount = len(links)
	for _, link := range links {
		link.addResourceSnapshot(&snapshot)
	}
	return snapshot
}

func (registry *v2AgentLinkRegistry) tryAcquireRPC(controllerID, linkID string) (func(), bool) {
	if registry == nil || controllerID == "" || linkID == "" {
		return nil, false
	}
	registry.mu.Lock()
	if registry.rpcInFlight >= v2MaximumRPCsPerDevice ||
		registry.rpcInFlightByLink[linkID] >= v2MaximumRPCsPerLink ||
		registry.rpcInFlightByController[controllerID] >= v2MaximumRPCsPerController {
		registry.mu.Unlock()
		return nil, false
	}
	registry.rpcInFlight++
	registry.rpcInFlightByLink[linkID]++
	registry.rpcInFlightByController[controllerID]++
	registry.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.mu.Lock()
			registry.rpcInFlight--
			registry.rpcInFlightByLink[linkID]--
			registry.rpcInFlightByController[controllerID]--
			if registry.rpcInFlightByLink[linkID] == 0 {
				delete(registry.rpcInFlightByLink, linkID)
			}
			if registry.rpcInFlightByController[controllerID] == 0 {
				delete(registry.rpcInFlightByController, controllerID)
			}
			registry.mu.Unlock()
		})
	}, true
}

func (link *v2AgentLink) addResourceSnapshot(snapshot *v2AgentResourceSnapshot) {
	if link == nil || snapshot == nil {
		return
	}
	link.mu.Lock()
	subscribers := make([]*agentEventSubscriber, 0, len(link.events))
	snapshot.ChannelCount += len(link.channels)
	for _, channel := range link.channels {
		if channel != nil {
			snapshot.ActiveStreamCount += len(channel.streams)
		}
	}
	snapshot.CachedOperationCount += len(link.operations)
	snapshot.CachedOperationBytes += link.operationBytes
	snapshot.FileTransferCount += len(link.files) + len(link.downloads)
	snapshot.EventSubscriptionCount += len(link.events)
	for _, subscription := range link.events {
		if subscription != nil && subscription.subscriber != nil {
			subscribers = append(subscribers, subscription.subscriber)
		}
	}
	link.mu.Unlock()
	for _, subscriber := range subscribers {
		subscriber.mu.Lock()
		snapshot.EventQueuedBytes += subscriber.queueBytes
		subscriber.mu.Unlock()
	}
	link.sendLocksMu.Lock()
	snapshot.OutboundStreamLocks += len(link.sendLocks)
	link.sendLocksMu.Unlock()
	if link.sequencer != nil {
		stats := link.sequencer.Stats()
		snapshot.SequencerOutboundEntries += stats.OutboundEntries
		snapshot.SequencerInboundEntries += stats.InboundEntries
		snapshot.SequencerTombstones += stats.Tombstones
		snapshot.SequencerActiveStreams += stats.ActiveStreams
		snapshot.SequencerUsedStreamIDs += stats.UsedStreamIDs
		snapshot.SequencerKeyCount += stats.KeyCount
	}
}

func (state *agentState) v2ResourceSnapshot() v2AgentResourceSnapshot {
	if state == nil {
		return v2AgentResourceSnapshot{
			SequencerTombstoneHardLimit: peerv2.DefaultSequencerTombstoneLimit,
			SequencerActiveHardLimit:    peerv2.DefaultSequencerActiveLimit,
			SequencerStreamHardLimit:    peerv2.DefaultSequencerStreamLimit,
		}
	}
	state.servicesMu.Lock()
	registry := state.v2Links
	state.servicesMu.Unlock()
	snapshot := registry.resourceSnapshot()
	snapshot.OperationInFlight = state.v2InFlightOperationCount()
	return snapshot
}

func (registry *v2AgentLinkRegistry) bindCarrier(carrier *v2AgentCarrier) {
	if registry == nil || carrier == nil {
		return
	}
	registry.mu.Lock()
	registry.carrier = carrier
	links := make([]*v2AgentLink, 0, len(registry.links))
	for _, link := range registry.links {
		links = append(links, link)
	}
	registry.mu.Unlock()
	now := time.Now().UTC()
	for _, link := range links {
		if link.expiredRecovery(now) {
			registry.mu.Lock()
			if registry.links[link.id] == link {
				delete(registry.links, link.id)
			}
			registry.mu.Unlock()
			link.close()
			continue
		}
		link.bindCarrier(carrier)
	}
}

func (registry *v2AgentLinkRegistry) unbindCarrier(carrier *v2AgentCarrier) {
	if registry == nil || carrier == nil {
		return
	}
	registry.mu.Lock()
	if registry.carrier == carrier {
		registry.carrier = nil
	}
	links := make([]*v2AgentLink, 0, len(registry.links))
	for _, link := range registry.links {
		links = append(links, link)
	}
	registry.mu.Unlock()
	for _, link := range links {
		link.unbindCarrier(carrier)
	}
}

func (registry *v2AgentLinkRegistry) get(linkID string) *v2AgentLink {
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.links[linkID]
}

func (registry *v2AgentLinkRegistry) remove(linkID string) {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	link := registry.links[linkID]
	delete(registry.links, linkID)
	registry.mu.Unlock()
	if link != nil {
		link.close()
	}
}

func (registry *v2AgentLinkRegistry) handleStreamRejected(rejected *remotev2.CarrierStreamRejected) error {
	if registry == nil || rejected == nil || rejected.GetLinkId() == "" || rejected.GetReason() == remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		return errV2AgentCarrier
	}
	channelID, streamID := rejected.GetChannelId(), rejected.GetStreamId()
	if (channelID == "") != (streamID == "") {
		return errV2AgentCarrier
	}
	link := registry.get(rejected.GetLinkId())
	if link == nil {
		return nil
	}
	if channelID == "" {
		registry.remove(rejected.GetLinkId())
		return nil
	}
	link.closeStream(channelID, streamID)
	return nil
}

func (registry *v2AgentLinkRegistry) close() {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	links := make([]*v2AgentLink, 0, len(registry.links))
	for _, link := range registry.links {
		links = append(links, link)
	}
	registry.links = make(map[string]*v2AgentLink)
	registry.carrier = nil
	registry.mu.Unlock()
	for _, link := range links {
		link.close()
	}
}

func (registry *v2AgentLinkRegistry) acceptInit(ctx context.Context, carrier *v2AgentCarrier, verifier remoteauth.DeviceLinkGrantVerifier, init *remotev2.LinkInit) error {
	if registry == nil || registry.state == nil || carrier == nil || init == nil {
		return errV2AgentLink
	}
	registry.mu.Lock()
	if existing := registry.links[init.GetLinkId()]; existing != nil {
		registry.mu.Unlock()
		return existing.resendAccept(ctx)
	}
	registry.mu.Unlock()
	// Authentication must finish before an existing Link is considered for
	// replacement. A reusable Grant plus a fresh Controller proof lets the same
	// persisted Controller recover after it lost local Link keys (for example,
	// an app restart during LINK_ACCEPT). Independently authorised Controllers
	// keep separate Links and must never evict one another.
	link, accept, err := newV2AgentLink(registry, carrier, verifier, init, time.Now().UTC())
	if err != nil {
		return err
	}
	replaced, err := registry.installAuthenticatedLink(link)
	if err != nil {
		link.close()
		return err
	}
	if replaced != nil {
		replaced.close()
	}
	if err := carrier.sendLink(ctx, &remotev2.LinkEnvelope{LinkId: link.id, Body: &remotev2.LinkEnvelope_LinkAccept{LinkAccept: accept}}); err != nil {
		registry.remove(link.id)
		return err
	}
	return nil
}

// installAuthenticatedLink enforces one Link per Controller only after
// newV2AgentLink has verified the signed Grant, all endpoint bindings and the
// Controller's Ed25519 proof. The same persisted Controller may therefore
// supersede an orphaned Link, while desktop, web and mobile Controllers retain
// independent Links on the shared Device Carrier.
func (registry *v2AgentLinkRegistry) installAuthenticatedLink(link *v2AgentLink) (*v2AgentLink, error) {
	if registry == nil || link == nil || link.id == "" || link.binding.ClientID == "" {
		return nil, errV2AgentLink
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.links[link.id] != nil {
		return nil, errV2AgentLink
	}
	var replaced *v2AgentLink
	var replacedID string
	for id, existing := range registry.links {
		if existing == nil || existing.id != id || existing.binding.ClientID == "" {
			return nil, errV2AgentLinkConflict
		}
		if existing.binding.ClientID != link.binding.ClientID {
			continue
		}
		if replaced != nil {
			// A Controller owns at most one Link. Preserve every entry when the
			// invariant is already broken instead of performing a partial commit.
			return nil, errV2AgentLinkConflict
		}
		replaced = existing
		replacedID = id
	}
	if replaced == nil && len(registry.links) >= v2MaximumLinksPerDevice {
		return nil, errV2AgentLinkCapacity
	}
	if replaced != nil {
		delete(registry.links, replacedID)
	}
	registry.links[link.id] = link
	return replaced, nil
}

type v2AgentLink struct {
	registry      *v2AgentLinkRegistry
	state         *agentState
	id            string
	binding       peerv2.HandshakeBinding
	clientPublic  ed25519.PublicKey
	keys          *peerv2.LinkState
	sequencer     *peerv2.Sequencer
	context       context.Context
	cancel        context.CancelFunc
	allowedScopes map[string]struct{}
	sendLocksMu   sync.Mutex
	sendLocks     map[string]*v2AgentSendLock
	eventResumeMu sync.Mutex

	mu                   sync.Mutex
	carrier              *v2AgentCarrier
	accept               *remotev2.LinkAccept
	active               bool
	closed               bool
	suspendedAt          time.Time
	leaseEnabled         bool
	leaseExpiresAt       time.Time
	leaseRenewalSequence uint64
	leaseTimer           *time.Timer
	channels             map[string]*v2AgentChannel
	operations           map[string]v2AgentOperation
	operationBytes       int
	files                map[string]*v2AgentFileTransfer
	downloads            map[string]*v2AgentDownloadTransfer
	events               map[string]*v2AgentEventSubscription
}

type v2AgentSendLock struct {
	mu    sync.Mutex
	users int
}

type v2AgentChannel struct {
	id        string
	kind      remotev2.ChannelKind
	projectID string
	scopes    map[string]struct{}
	streams   map[string]*v2AgentStream
}

type v2AgentStream struct {
	id          string
	kind        remotev2.StreamKind
	operationID string
	context     context.Context
	cancel      context.CancelFunc
	cleanup     func()
	terminal    *v2AgentTerminalStream
}

// v2AgentOperation is an in-memory hot cache.  The durable operation table is
// used as the recovery source after an Agent restart; keeping this tiny cache
// avoids a SQLite read on ordinary Carrier retransmits.
type v2AgentOperation struct {
	digest   [sha256.Size]byte
	response *remotev2.RpcResponse
	events   []*remotev2.RpcEvent
	cachedAt time.Time
	bytes    int
}

type v2AgentFileTransfer struct {
	transferID         string
	channelID          string
	streamID           string
	projectID          string
	relativePathHandle string
	totalLength        uint64
	chunkSize          uint32
	sha256             [sha256.Size]byte
	stagingPath        string
	file               *os.File
	managedUpload      bool
	// confirmedThrough is the first chunk index not yet known to be durable.
	// confirmedSparse contains only the small, bounded reordering window ahead
	// of it, so transfer memory does not grow with file length.
	confirmedThrough uint64
	confirmedSparse  map[uint64]struct{}
	expectedRevision uint64
	committed        bool
	createdAt        time.Time
}

// v2AgentDownloadTransfer retains only file metadata and the single sequential
// chunk currently awaiting acknowledgement. The source stays in the TTL-bound
// file manager, so Link memory is constant regardless of file length.
type v2AgentDownloadTransfer struct {
	transferID       string
	sourceKind       string
	peerSessionID    string
	channelID        string
	streamID         string
	projectID        string
	relativePath     string
	path             string
	totalLength      uint64
	chunkSize        uint32
	sha256           [sha256.Size]byte
	revision         uint64
	projectRevision  uint64
	expectedRevision uint64
	taskID           uuid.UUID
	runID            uuid.UUID
	generation       uint64
	sealed           bool
	nextIndex        uint64
	sentIndex        uint64
	sent             bool
	committed        bool
}

type v2AgentEventSubscription struct {
	subscriptionID string
	channelID      string
	streamID       string
	projectID      uuid.UUID
	highWatermark  uint64
	acknowledged   uint64
	subscriber     *agentEventSubscriber
	cancel         context.CancelFunc
	sendMu         sync.Mutex
	resumePending  bool
	resumeReady    chan struct{}
}

func newV2AgentLink(registry *v2AgentLinkRegistry, carrier *v2AgentCarrier, verifier remoteauth.DeviceLinkGrantVerifier, init *remotev2.LinkInit, now time.Time) (*v2AgentLink, *remotev2.LinkAccept, error) {
	state := registry.state
	if state == nil || init == nil || uuid.Validate(init.GetLinkId()) != nil || uuid.Validate(init.GetGrantId()) != nil ||
		uuid.Validate(init.GetClientId()) != nil || init.GetDeviceId() != state.DeviceID.String() || init.GetRelayNodeId() == "" || init.GetRelayCellId() == "" ||
		init.GetTargetConnectionEpoch() == 0 || init.GetClientIdentityKeyVersion() == 0 || len(init.GetClientEphemeralPublicKey()) != peerv2.X25519PublicKeySize ||
		len(init.GetClientChallenge()) != 32 || len(init.GetIdentitySignature()) != ed25519.SignatureSize || init.GetExpiresAt() == nil || init.GetExpiresAt().CheckValid() != nil {
		return nil, nil, errV2AgentLink
	}
	claims, err := verifier.Verify(init.GetDeviceConnectionGrant(), now)
	initExpiry := init.GetExpiresAt().AsTime()
	epochMatches := v2DeviceLinkGrantMatchesCarrierEpoch(claims, carrier.epoch)
	if err != nil || claims.GrantID != init.GetGrantId() || claims.ClientID != init.GetClientId() || claims.DeviceID != state.DeviceID.String() ||
		claims.RelayNodeID != init.GetRelayNodeId() || claims.RelayCellID != init.GetRelayCellId() ||
		claims.TargetConnectionEpoch != init.GetTargetConnectionEpoch() || !epochMatches ||
		claims.ClientIdentityKeyVersion != init.GetClientIdentityKeyVersion() || claims.DeviceIdentityKeyVersion != state.KeyVersion ||
		claims.DeviceKeyThumbprint != remoteauth.PublicKeyThumbprint(state.identity.Public().(ed25519.PublicKey)) ||
		initExpiry.Unix() != claims.ExpiresAt || initExpiry.Nanosecond() != 0 || !initExpiry.After(now) {
		return nil, nil, errV2AgentLinkAuth
	}
	clientPublic, err := remoteauth.DecodeIdentityPublicKey(claims.ClientIdentityKey, claims.ClientKeyThumbprint)
	if err != nil {
		return nil, nil, errV2AgentLinkAuth
	}
	initialBinding := peerv2.HandshakeBinding{
		GrantID: claims.GrantID, LinkID: init.GetLinkId(), ClientID: claims.ClientID, DeviceID: claims.DeviceID,
		RelayNodeID: claims.RelayNodeID, RelayCellID: claims.RelayCellID, TargetConnectionEpoch: claims.TargetConnectionEpoch,
		ClientIdentityVersion: claims.ClientIdentityKeyVersion, ClientEphemeralPublic: append([]byte(nil), init.GetClientEphemeralPublicKey()...),
		ClientChallenge: append([]byte(nil), init.GetClientChallenge()...), ExpiresAtUnixMilli: init.GetExpiresAt().AsTime().UnixMilli(),
	}
	if err := peerv2.VerifyLinkInit(clientPublic, initialBinding, init.GetIdentitySignature()); err != nil {
		return nil, nil, errV2AgentLinkAuth
	}
	allowedScopes := make(map[string]struct{}, len(claims.AllowedScopes))
	for _, scope := range claims.AllowedScopes {
		allowedScopes[scope] = struct{}{}
	}
	if len(allowedScopes) != len(claims.AllowedScopes) {
		return nil, nil, errV2AgentLinkAuth
	}
	deviceEphemeral, err := peerv2.GenerateEphemeralKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	deviceChallenge := make([]byte, 32)
	if _, err := rand.Read(deviceChallenge); err != nil {
		peerv2.ClearPrivateKey(&deviceEphemeral)
		return nil, nil, err
	}
	binding := initialBinding
	binding.DeviceIdentityVersion = state.KeyVersion
	binding.DeviceEphemeralPublic = append([]byte(nil), deviceEphemeral.PublicKey().Bytes()...)
	binding.DeviceChallenge = deviceChallenge
	signature, err := peerv2.SignLinkAccept(state.identity, binding)
	if err != nil {
		peerv2.ClearPrivateKey(&deviceEphemeral)
		return nil, nil, err
	}
	shared, err := peerv2.X25519SharedSecret(deviceEphemeral, init.GetClientEphemeralPublicKey())
	peerv2.ClearPrivateKey(&deviceEphemeral)
	if err != nil {
		return nil, nil, errV2AgentLinkAuth
	}
	rootKey, err := peerv2.DeriveRootKey(shared, binding)
	zeroV2Bytes(shared)
	if err != nil {
		return nil, nil, err
	}
	keys, err := peerv2.NewLinkState(init.GetLinkId(), rootKey)
	zeroV2Bytes(rootKey)
	if err != nil {
		return nil, nil, err
	}
	accept := &remotev2.LinkAccept{
		GrantId: claims.GrantID, LinkId: init.GetLinkId(), ClientId: claims.ClientID, DeviceId: claims.DeviceID,
		RelayNodeId: claims.RelayNodeID, RelayCellId: claims.RelayCellID, TargetConnectionEpoch: claims.TargetConnectionEpoch,
		DeviceIdentityKeyVersion: state.KeyVersion, ClientEphemeralPublicKey: append([]byte(nil), init.GetClientEphemeralPublicKey()...),
		DeviceEphemeralPublicKey: append([]byte(nil), binding.DeviceEphemeralPublic...), ClientChallenge: append([]byte(nil), init.GetClientChallenge()...),
		DeviceChallenge: append([]byte(nil), deviceChallenge...), ExpiresAt: timestamppb.New(init.GetExpiresAt().AsTime()), IdentitySignature: signature,
	}
	linkContext, cancel := context.WithCancel(context.Background())
	return &v2AgentLink{
		registry: registry, state: state, id: init.GetLinkId(), binding: binding, clientPublic: append(ed25519.PublicKey(nil), clientPublic...),
		keys: keys, sequencer: peerv2.NewSequencer(4096), context: linkContext, cancel: cancel, allowedScopes: allowedScopes, carrier: carrier, accept: accept,
		channels: make(map[string]*v2AgentChannel), operations: make(map[string]v2AgentOperation), files: make(map[string]*v2AgentFileTransfer), downloads: make(map[string]*v2AgentDownloadTransfer), events: make(map[string]*v2AgentEventSubscription),
	}, accept, nil
}

func v2DeviceLinkGrantMatchesCarrierEpoch(claims remoteauth.DeviceLinkGrantClaims, carrierEpoch uint64) bool {
	return carrierEpoch > 0 && (claims.TargetConnectionEpoch == carrierEpoch || (claims.Persistent() && claims.TargetConnectionEpoch <= carrierEpoch))
}

func (link *v2AgentLink) close() {
	if link == nil {
		return
	}
	link.mu.Lock()
	if link.closed {
		link.mu.Unlock()
		return
	}
	link.closed = true
	if link.leaseTimer != nil {
		link.leaseTimer.Stop()
		link.leaseTimer = nil
	}
	cancel := link.cancel
	streams := make([]*v2AgentStream, 0)
	for _, channel := range link.channels {
		for _, stream := range channel.streams {
			streams = append(streams, stream)
		}
	}
	link.channels = make(map[string]*v2AgentChannel)
	link.carrier = nil
	keys := link.keys
	link.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, stream := range streams {
		if stream.cancel != nil {
			stream.cancel()
		}
		if stream.cleanup != nil {
			stream.cleanup()
		}
	}
	if keys != nil {
		keys.Close()
	}
	if link.sequencer != nil {
		link.sequencer.Reset()
	}
}

func (link *v2AgentLink) bindCarrier(carrier *v2AgentCarrier) {
	if link == nil || carrier == nil {
		return
	}
	link.mu.Lock()
	if !link.closed {
		link.carrier, link.suspendedAt = carrier, time.Time{}
	}
	link.mu.Unlock()
}

func (link *v2AgentLink) unbindCarrier(carrier *v2AgentCarrier) {
	if link == nil || carrier == nil {
		return
	}
	link.mu.Lock()
	if !link.closed && link.carrier == carrier {
		link.carrier, link.suspendedAt = nil, time.Now().UTC()
	}
	link.mu.Unlock()
}

func (link *v2AgentLink) resendAccept(ctx context.Context) error {
	if link == nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	carrier, accept, closed := link.carrier, link.accept, link.closed
	link.mu.Unlock()
	if closed || carrier == nil || accept == nil {
		return errV2AgentLink
	}
	clone := proto.Clone(accept).(*remotev2.LinkAccept)
	return carrier.sendLink(ctx, &remotev2.LinkEnvelope{LinkId: link.id, Body: &remotev2.LinkEnvelope_LinkAccept{LinkAccept: clone}})
}

func (link *v2AgentLink) expiredRecovery(now time.Time) bool {
	if link == nil {
		return true
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	// A renewable Lease governs an active Link's authorization lifetime, but it
	// must not turn disconnected key/Stream state into a 90-minute cache. Any
	// physical outage has one uniform five-minute recovery window.
	if !link.suspendedAt.IsZero() && !link.suspendedAt.Add(v2LinkRecoveryTTL).After(now) {
		return true
	}
	if link.leaseEnabled {
		return link.leaseExpiresAt.IsZero() || !link.leaseExpiresAt.After(now)
	}
	return false
}

func zeroV2Bytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// serveTargetV2 keeps Carrier liveness separate from Link state. A transient
// socket failure returns to the allocation loop, which rebinds the registry to
// the next Carrier without discarding healthy Link keys, Channels or Streams.
func serveTargetV2(ctx context.Context, carrier *v2AgentCarrier, heartbeat time.Duration, links *v2AgentLinkRegistry, verifier remoteauth.DeviceLinkGrantVerifier) error {
	if ctx == nil || carrier == nil || links == nil || verifier.Issuer == "" || len(verifier.Keys) == 0 {
		return errV2AgentCarrier
	}
	if heartbeat < time.Second {
		heartbeat = 25 * time.Second
	}
	serveContext, cancel := context.WithCancel(ctx)
	defer cancel()
	links.bindCarrier(carrier)
	defer links.unbindCarrier(carrier)

	var lastInbound atomic.Int64
	nowMonotonic := v2AgentMonotonicNanos()
	lastInbound.Store(nowMonotonic)
	fatal := make(chan error, 1)
	reportFatal := func(err error) {
		if err == nil {
			return
		}
		select {
		case fatal <- err:
			cancel()
		default:
		}
	}
	go func() {
		timeout := targetRelayHeartbeatTimeout(heartbeat)
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-serveContext.Done():
				return
			case <-timer.C:
				current := v2AgentMonotonicNanos()
				activityAge := time.Duration(max(int64(0), current-lastInbound.Load()))
				if activityAge > timeout {
					reportFatal(errRelayHeartbeatTimeout)
					return
				}
				carrier.packetMu.Lock()
				outstanding := carrier.nextOut > carrier.lastPeerAcknowledged
				carrier.packetMu.Unlock()
				ackAge := time.Duration(max(int64(0), current-carrier.lastAckProgress.Load()))
				if outstanding && ackAge > timeout {
					reportFatal(errRelayHeartbeatTimeout)
					return
				}
				delay := max(time.Millisecond, timeout-activityAge)
				if outstanding {
					delay = max(time.Millisecond, min(delay, timeout-ackAge))
				}
				timer.Reset(delay)
			}
		}
	}()

	for {
		envelope, err := readV2AgentEnvelope(serveContext, carrier.socket)
		if err != nil || carrier.acceptIncoming(envelope) != nil {
			select {
			case cause := <-fatal:
				return cause
			default:
			}
			return errV2AgentCarrier
		}
		lastInbound.Store(v2AgentMonotonicNanos())
		switch {
		case envelope.GetPing() != nil:
			if err := carrier.send(serveContext, &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Pong{Pong: &remotev2.CarrierPong{
				MonotonicMillis: envelope.GetPing().GetMonotonicMillis(),
			}}}, v2AgentControl); err != nil {
				return err
			}
		case envelope.GetPong() != nil:
			// Relay is the only periodic prober. Tolerate a valid sequenced Pong
			// for wire compatibility; lastInbound already records its liveness.
			continue
		case envelope.GetStreamRejected() != nil:
			if err := links.handleStreamRejected(envelope.GetStreamRejected()); err != nil {
				return err
			}
		case envelope.GetGoAway() != nil:
			goAway := envelope.GetGoAway()
			return &v2AgentGoAwayError{
				reason:     goAway.GetReason(),
				retryAfter: time.Duration(goAway.GetReconnectAfterMillis()) * time.Millisecond,
			}
		case envelope.GetResume() != nil:
			link := links.get(envelope.GetResume().GetLinkId())
			if link == nil {
				_ = carrier.rejectLinkStream(envelope.GetResume().GetLinkId(), "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_NOT_FOUND)
				continue
			}
			if link.expiredRecovery(time.Now().UTC()) {
				links.remove(link.id)
				_ = carrier.rejectLinkStream(envelope.GetResume().GetLinkId(), "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED)
				continue
			}
			link.applyCarrierResume(envelope.GetResume().GetLastAckByStream())
			// The encrypted operation and file layers retain their own ACK state.
			// Re-sending the signed LinkAccept makes a client-side resume idempotent
			// even when the previous Carrier died before it observed the accept.
			_ = link.resendAccept(serveContext)
			_ = link.retryRekey(serveContext)
		case envelope.GetLink() != nil:
			linkEnvelope := envelope.GetLink()
			if init := linkEnvelope.GetLinkInit(); init != nil {
				if err := links.acceptInit(serveContext, carrier, verifier, init); err != nil {
					_ = carrier.rejectLinkStream(init.GetLinkId(), "", "", v2AgentLinkRejectCode(err))
				}
				continue
			}
			link := links.get(linkEnvelope.GetLinkId())
			if link == nil {
				// The Link itself is unknown (for example after an Agent restart).
				// Keep the rejection Link-scoped; carrying the encrypted frame's
				// channel/stream IDs would make a Controller ignore it as a harmless
				// stream-level failure and leave the stale Link permanently active.
				_ = carrier.rejectLinkStream(linkEnvelope.GetLinkId(), "", "", remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_NOT_FOUND)
				continue
			}
			if err := link.handleEnvelope(serveContext, linkEnvelope); err != nil {
				if errors.Is(err, errV2AgentLinkAuth) {
					// AEAD failures are Link-fatal. The Carrier remains usable for a
					// fresh, explicitly authorised Link only.
					links.remove(link.id)
					continue
				}
				if errors.Is(err, errV2AgentCarrier) {
					return err
				}
				// An invalid Link control frame cannot poison unrelated Links or
				// Carrier packet processing.
				links.remove(link.id)
			}
		default:
			return errV2AgentCarrier
		}
	}
}

func v2AgentLinkRejectCode(err error) remotev2.ProtocolErrorCode {
	switch {
	case errors.Is(err, errV2AgentLinkCapacity):
		return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_BACKPRESSURE
	case errors.Is(err, errV2AgentLinkConflict):
		return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_AUTHENTICATION_FAILED
	case errors.Is(err, errV2AgentLinkAuth):
		return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_IDENTITY_INVALID
	default:
		return remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID
	}
}

func (carrier *v2AgentCarrier) rejectLinkStream(ctxLinkID, channelID, streamID string, reason remotev2.ProtocolErrorCode) error {
	if carrier == nil || ctxLinkID == "" {
		return errV2AgentCarrier
	}
	return carrier.send(context.Background(), &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_StreamRejected{StreamRejected: &remotev2.CarrierStreamRejected{
		LinkId: ctxLinkID, ChannelId: channelID, StreamId: streamID, Reason: reason,
	}}}, v2AgentControl)
}

// handleEnvelope opens one Link record. Authentication is intentionally
// distinguished from message validation: only an AEAD failure destroys the
// Link; malformed RPC/file/event payloads are isolated to their Stream.
func (link *v2AgentLink) handleEnvelope(ctx context.Context, envelope *remotev2.LinkEnvelope) error {
	if link == nil || envelope == nil || envelope.GetLinkId() != link.id || envelope.GetLinkAccept() != nil || envelope.GetLinkInit() != nil {
		return errV2AgentLink
	}
	record := envelope.GetEncrypted()
	if record == nil || record.GetLinkId() != link.id {
		return errV2AgentLink
	}
	if link.leaseExpired(time.Now().UTC()) {
		return errV2AgentLink
	}
	plaintext, err := link.openRecord(record)
	if err != nil {
		if errors.Is(err, errV2AgentReplay) {
			return nil
		}
		if errors.Is(err, peerv2.ErrAuthentication) {
			return errV2AgentLinkAuth
		}
		channelID, streamID := record.GetChannelId(), record.GetStreamId()
		if streamID != "" && streamID != v2ControlStreamID && streamID != v2ChannelControlStreamID {
			link.failStream(ctx, channelID, streamID, remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
			return nil
		}
		return errV2AgentLink
	}
	switch record.GetFrameType() {
	case remotev2.FrameType_FRAME_TYPE_LINK_CONFIRM:
		return link.handleConfirm(ctx, record, plaintext)
	case remotev2.FrameType_FRAME_TYPE_LINK_LEASE_RENEW:
		return link.handleLeaseRenew(ctx, record, plaintext)
	case remotev2.FrameType_FRAME_TYPE_REKEY_INIT:
		return link.handleRekeyInit(ctx, record, plaintext)
	case remotev2.FrameType_FRAME_TYPE_REKEY_ACK:
		return link.handleRekeyAck(ctx, record, plaintext)
	case remotev2.FrameType_FRAME_TYPE_REKEY_COMMIT:
		return link.handleRekeyCommit(record, plaintext)
	case remotev2.FrameType_FRAME_TYPE_CHANNEL_OPEN:
		if err := link.handleChannelOpen(ctx, record, plaintext); err != nil {
			link.closeChannel(record.GetChannelId())
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_CHANNEL_CLOSE:
		link.closeChannel(record.GetChannelId())
		return nil
	case remotev2.FrameType_FRAME_TYPE_STREAM_OPEN:
		if err := link.handleStreamOpen(record, plaintext); err != nil {
			if errors.Is(err, peerv2.ErrSequenceLimit) || errors.Is(err, peerv2.ErrStreamReuse) {
				// A Link-wide Stream ID exhaustion or reuse cannot be repaired at
				// Channel scope without risking nonce/lifecycle ambiguity.
				go link.close()
			} else {
				link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
			}
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_STREAM_ACK:
		return nil // ACK watermarks are retained by the individual stream owner.
	case remotev2.FrameType_FRAME_TYPE_STREAM_CLOSE:
		if err := link.handleStreamClose(record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_RPC_REQUEST:
		if err := link.handleRPCRequest(ctx, record, plaintext); err != nil {
			reason := remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID
			if errors.Is(err, errV2AgentStream) {
				reason = remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_NOT_FOUND
			}
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), reason)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_FILE_MANIFEST:
		if err := link.handleFileManifest(ctx, record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_FILE_CHUNK:
		if err := link.handleFileChunk(ctx, record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_FILE_ACK:
		if err := link.handleFileAck(ctx, record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_FILE_COMMIT:
		if err := link.handleFileCommit(ctx, record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_EVENT_SUBSCRIBE:
		if err := link.handleEventSubscribe(ctx, record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_EVENT_ACK:
		if err := link.handleEventAck(record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_EVENT_RESUME:
		if err := link.handleEventResume(ctx, record, plaintext); err != nil {
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID)
		}
		return nil
	case remotev2.FrameType_FRAME_TYPE_STREAM_DATA:
		if err := link.handleTerminalStreamData(ctx, record, plaintext); err != nil {
			reason := remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID
			if errors.Is(err, errV2AgentStream) {
				reason = remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_NOT_FOUND
			}
			link.failStream(ctx, record.GetChannelId(), record.GetStreamId(), reason)
		}
		return nil
	default:
		return errV2AgentLink
	}
}

func (link *v2AgentLink) openRecord(record *remotev2.EncryptedRecord) ([]byte, error) {
	if link == nil || record == nil || record.GetLinkId() != link.id || record.GetDirection() != remotev2.Direction_DIRECTION_CLIENT_TO_DEVICE ||
		record.GetKeyId() == 0 || record.GetStreamSequence() == 0 || len(record.GetCiphertext()) == 0 {
		return nil, errV2AgentLink
	}
	metadata := peerv2.RecordMetadata{
		LinkID: link.id, ChannelID: record.GetChannelId(), StreamID: record.GetStreamId(), KeyID: record.GetKeyId(),
		Direction: peerv2.DirectionClientToDevice, FrameType: peerv2.FrameType(record.GetFrameType()), StreamSequence: record.GetStreamSequence(),
	}
	rootKey, err := link.keys.RootKey(record.GetKeyId())
	if err != nil {
		return nil, errV2AgentLink
	}
	streamKey, err := v2AgentRecordKey(rootKey, metadata)
	zeroV2Bytes(rootKey)
	if err != nil {
		return nil, errV2AgentLink
	}
	defer zeroV2Bytes(streamKey)
	plaintext, err := peerv2.Open(streamKey, record.GetCiphertext(), metadata)
	if err != nil {
		return nil, err
	}
	if err := link.sequencer.AcceptInbound(metadata); err != nil {
		zeroV2Bytes(plaintext)
		if errors.Is(err, peerv2.ErrReplay) {
			return nil, errV2AgentReplay
		}
		return nil, errV2AgentLink
	}
	return plaintext, nil
}

func v2AgentRecordKey(rootKey []byte, metadata peerv2.RecordMetadata) ([]byte, error) {
	frameType := metadata.FrameType
	switch frameType {
	case peerv2.FrameLinkConfirm, peerv2.FrameLinkReady, peerv2.FrameLinkLeaseRenew, peerv2.FrameLinkLeaseRenewed, peerv2.FrameRekeyInit, peerv2.FrameRekeyAck, peerv2.FrameRekeyCommit:
		if metadata.ChannelID != v2ControlChannelID || metadata.StreamID != v2ControlStreamID {
			return nil, errV2AgentLink
		}
		return peerv2.DeriveControlKey(rootKey, metadata.LinkID, metadata.KeyID, metadata.Direction)
	case peerv2.FrameChannelOpen, peerv2.FrameChannelAccept, peerv2.FrameChannelClose, peerv2.FrameStreamOpen, peerv2.FrameStreamAck, peerv2.FrameStreamClose:
		if metadata.ChannelID == v2ControlChannelID || metadata.StreamID != v2ChannelControlStreamID {
			return nil, errV2AgentLink
		}
		return peerv2.DeriveChannelKey(rootKey, metadata.LinkID, metadata.KeyID, metadata.Direction, metadata.ChannelID)
	case peerv2.FrameStreamData, peerv2.FrameRPCRequest, peerv2.FrameRPCResponse, peerv2.FrameRPCEvent, peerv2.FrameFileManifest, peerv2.FrameFileChunk,
		peerv2.FrameFileAck, peerv2.FrameFileCommit, peerv2.FrameEventSubscribe, peerv2.FrameEventAck, peerv2.FrameEventResume, peerv2.FrameEventResetRequired:
		if metadata.ChannelID == v2ControlChannelID || metadata.StreamID == v2ControlStreamID || metadata.StreamID == v2ChannelControlStreamID {
			return nil, errV2AgentLink
		}
		return peerv2.DeriveStreamKey(rootKey, metadata.LinkID, metadata.KeyID, metadata.Direction, metadata.ChannelID, metadata.StreamID)
	default:
		return nil, errV2AgentLink
	}
}

func (link *v2AgentLink) handleConfirm(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if link == nil || record.GetKeyId() != 1 {
		return errV2AgentLink
	}
	confirm := new(remotev2.LinkConfirm)
	if !unmarshalV2AgentMessage(plaintext, confirm) || confirm.GetLinkId() != link.id || len(confirm.GetTranscriptMac()) != sha256.Size {
		return errV2AgentLink
	}
	rootKey, err := link.keys.RootKey(1)
	if err != nil {
		return errV2AgentLink
	}
	err = peerv2.VerifyLinkConfirmationMAC(rootKey, link.binding, confirm.GetTranscriptMac())
	zeroV2Bytes(rootKey)
	if err != nil {
		return errV2AgentLinkAuth
	}
	link.mu.Lock()
	if link.closed {
		link.mu.Unlock()
		return errV2AgentLink
	}
	link.active = true
	keyID := link.keys.ActiveKeyID()
	link.mu.Unlock()
	return link.sendEncrypted(ctx, keyID, remotev2.FrameType_FRAME_TYPE_LINK_READY, v2ControlChannelID, v2ControlStreamID, &remotev2.LinkReady{
		LinkId: link.id, ActiveKeyId: keyID,
		LeaseRenewalIntervalSeconds: uint32(v2LinkLeaseRenewalInterval / time.Second),
		LeaseDurationSeconds:        uint32(v2LinkLeaseDuration / time.Second),
	})
}

// handleLeaseRenew maintains a renewable end-to-end Link lease after the
// encrypted handshake is active. Relay sees routing metadata and ciphertext;
// it cannot mint or extend this lease. Disconnected recovery remains capped at
// five minutes independently of the active authorization lease.
func (link *v2AgentLink) handleLeaseRenew(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if link == nil || record == nil || !link.isActive() {
		return errV2AgentLink
	}
	renew := new(remotev2.LinkLeaseRenew)
	if !unmarshalV2AgentMessage(plaintext, renew) || renew.GetLinkId() != link.id || renew.GetRenewalSequence() == 0 ||
		!link.renewLease(renew.GetRenewalSequence(), time.Now().UTC()) {
		return errV2AgentLink
	}
	return link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_LINK_LEASE_RENEWED, v2ControlChannelID, v2ControlStreamID, &remotev2.LinkLeaseRenewed{
		LinkId: link.id, RenewalSequence: renew.GetRenewalSequence(),
		LeaseRenewalIntervalSeconds: uint32(v2LinkLeaseRenewalInterval / time.Second),
		LeaseDurationSeconds:        uint32(v2LinkLeaseDuration / time.Second),
	})
}

func (link *v2AgentLink) renewLease(sequence uint64, now time.Time) bool {
	if link == nil || sequence == 0 || now.IsZero() {
		return false
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	if link.closed || !link.active {
		return false
	}
	if link.leaseEnabled && !link.leaseExpiresAt.After(now.UTC()) {
		return false
	}
	if link.leaseEnabled && sequence == link.leaseRenewalSequence {
		return link.leaseExpiresAt.After(now.UTC())
	}
	if sequence != link.leaseRenewalSequence+1 {
		return false
	}
	if link.leaseTimer != nil {
		link.leaseTimer.Stop()
	}
	link.leaseEnabled = true
	link.leaseRenewalSequence = sequence
	link.leaseExpiresAt = now.UTC().Add(v2LinkLeaseDuration)
	expiresAt := link.leaseExpiresAt
	link.leaseTimer = time.AfterFunc(v2LinkLeaseDuration, func() {
		link.expireLease(sequence, expiresAt)
	})
	return true
}

func (link *v2AgentLink) expireLease(sequence uint64, expiresAt time.Time) {
	if link == nil {
		return
	}
	link.mu.Lock()
	if link.closed || !link.leaseEnabled || link.leaseRenewalSequence != sequence || !link.leaseExpiresAt.Equal(expiresAt) || link.leaseExpiresAt.After(time.Now().UTC()) {
		link.mu.Unlock()
		return
	}
	// Fence a concurrent renewal before releasing the Link from its registry.
	link.active = false
	link.leaseTimer = nil
	link.mu.Unlock()
	if link.registry != nil {
		link.registry.remove(link.id)
	} else {
		link.close()
	}
}

func (link *v2AgentLink) handleRekeyInit(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if !link.isActive() {
		return errV2AgentLink
	}
	init := new(remotev2.RekeyInit)
	if !unmarshalV2AgentMessage(plaintext, init) || init.GetLinkId() != link.id {
		return errV2AgentLink
	}
	ack, err := link.keys.ReceiveRekeyInit(peerv2.RekeyInit{
		LinkID: init.GetLinkId(), RekeyID: init.GetRekeyId(), NextKeyID: init.GetNextKeyId(), EphemeralPublic: init.GetEphemeralPublicKey(), IdentitySignature: init.GetIdentitySignature(),
	}, link.clientPublic, link.state.identity, rand.Reader)
	if err != nil {
		return errV2AgentLink
	}
	return link.sendEncrypted(ctx, record.GetKeyId(), remotev2.FrameType_FRAME_TYPE_REKEY_ACK, v2ControlChannelID, v2ControlStreamID, &remotev2.RekeyAck{
		LinkId: ack.LinkID, RekeyId: ack.RekeyID, NextKeyId: ack.NextKeyID, EphemeralPublicKey: ack.EphemeralPublic, IdentitySignature: ack.IdentitySignature,
	})
}

func (link *v2AgentLink) handleRekeyAck(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if !link.isActive() {
		return errV2AgentLink
	}
	ack := new(remotev2.RekeyAck)
	if !unmarshalV2AgentMessage(plaintext, ack) || ack.GetLinkId() != link.id {
		return errV2AgentLink
	}
	if err := link.keys.ReceiveRekeyAck(peerv2.RekeyAck{
		LinkID: ack.GetLinkId(), RekeyID: ack.GetRekeyId(), NextKeyID: ack.GetNextKeyId(), EphemeralPublic: ack.GetEphemeralPublicKey(), IdentitySignature: ack.GetIdentitySignature(),
	}, link.clientPublic); err != nil {
		return errV2AgentLink
	}
	// Commit is protected by the currently active (old) control key. The
	// caller activates its new root only after that ciphertext has been sealed,
	// so the peer can verify it before changing generations as well.
	oldKeyID := record.GetKeyId()
	commit, err := link.keys.CommitRekey(link.streamBoundaries())
	if err != nil {
		return errV2AgentLink
	}
	link.retireSequencerKeys()
	boundaries := make([]*remotev2.StreamKeyBoundary, 0, len(commit.Boundaries))
	for _, boundary := range commit.Boundaries {
		boundaries = append(boundaries, &remotev2.StreamKeyBoundary{StreamId: boundary.StreamID, NextSequence: boundary.NextSequence})
	}
	return link.sendEncrypted(ctx, oldKeyID, remotev2.FrameType_FRAME_TYPE_REKEY_COMMIT, v2ControlChannelID, v2ControlStreamID, &remotev2.RekeyCommit{
		LinkId: commit.LinkID, RekeyId: commit.RekeyID, NextKeyId: commit.NextKeyID, Boundaries: boundaries,
	})
}

func (link *v2AgentLink) handleRekeyCommit(record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if !link.isActive() {
		return errV2AgentLink
	}
	commit := new(remotev2.RekeyCommit)
	if !unmarshalV2AgentMessage(plaintext, commit) || commit.GetLinkId() != link.id {
		return errV2AgentLink
	}
	boundaries := make([]peerv2.StreamBoundary, 0, len(commit.GetBoundaries()))
	for _, boundary := range commit.GetBoundaries() {
		if boundary == nil {
			return errV2AgentLink
		}
		boundaries = append(boundaries, peerv2.StreamBoundary{StreamID: boundary.GetStreamId(), NextSequence: boundary.GetNextSequence()})
	}
	if err := link.keys.ReceiveRekeyCommit(peerv2.RekeyCommit{LinkID: commit.GetLinkId(), RekeyID: commit.GetRekeyId(), NextKeyID: commit.GetNextKeyId(), Boundaries: boundaries}); err != nil {
		return errV2AgentLink
	}
	link.retireSequencerKeys()
	return nil
}

func (link *v2AgentLink) retireSequencerKeys() {
	if link == nil || link.keys == nil || link.sequencer == nil {
		return
	}
	active := link.keys.ActiveKeyID()
	if active > 2 {
		link.sequencer.RetireKey(active - 2)
	}
}

// retryRekey retransmits the newest completed control operation after a
// Carrier resume. The LinkState history contains only public metadata, so this
// cannot recreate a competing key generation or expose key material.
func (link *v2AgentLink) retryRekey(ctx context.Context) error {
	if link == nil || link.keys == nil {
		return nil
	}
	retry, err := link.keys.LastRekeyRetry()
	if errors.Is(err, peerv2.ErrKeyUnavailable) {
		return nil
	}
	if err != nil || retry == nil {
		return err
	}
	if retry.Initiator {
		if retry.Commit == nil {
			return nil
		}
		return link.sendEncrypted(ctx, retry.OldKeyID, remotev2.FrameType_FRAME_TYPE_REKEY_COMMIT, v2ControlChannelID, v2ControlStreamID, &remotev2.RekeyCommit{
			LinkId: retry.Commit.LinkID, RekeyId: retry.Commit.RekeyID, NextKeyId: retry.Commit.NextKeyID,
			Boundaries: func() []*remotev2.StreamKeyBoundary {
				result := make([]*remotev2.StreamKeyBoundary, 0, len(retry.Commit.Boundaries))
				for _, boundary := range retry.Commit.Boundaries {
					result = append(result, &remotev2.StreamKeyBoundary{StreamId: boundary.StreamID, NextSequence: boundary.NextSequence})
				}
				return result
			}(),
		})
	}
	if retry.Ack == nil {
		return nil
	}
	return link.sendEncrypted(ctx, retry.OldKeyID, remotev2.FrameType_FRAME_TYPE_REKEY_ACK, v2ControlChannelID, v2ControlStreamID, &remotev2.RekeyAck{
		LinkId: retry.Ack.LinkID, RekeyId: retry.Ack.RekeyID, NextKeyId: retry.Ack.NextKeyID,
		EphemeralPublicKey: retry.Ack.EphemeralPublic, IdentitySignature: retry.Ack.IdentitySignature,
	})
}

func (link *v2AgentLink) streamBoundaries() []peerv2.StreamBoundary {
	if link == nil {
		return nil
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	boundaries := make([]peerv2.StreamBoundary, 0)
	for _, channel := range link.channels {
		for _, stream := range channel.streams {
			if stream != nil && stream.id != "" {
				next, err := link.sequencer.NextOutboundSequence(link.keys.ActiveKeyID(), peerv2.DirectionDeviceToClient, stream.id)
				if err != nil {
					continue
				}
				boundaries = append(boundaries, peerv2.StreamBoundary{StreamID: stream.id, NextSequence: next})
			}
		}
	}
	return boundaries
}

func (link *v2AgentLink) isActive() bool {
	if link == nil {
		return false
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	return !link.closed && link.active && (!link.leaseEnabled || link.leaseExpiresAt.After(time.Now().UTC()))
}

func (link *v2AgentLink) leaseExpired(now time.Time) bool {
	if link == nil {
		return true
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	return link.leaseEnabled && (link.leaseExpiresAt.IsZero() || !link.leaseExpiresAt.After(now))
}

func (link *v2AgentLink) handleChannelOpen(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if !link.isActive() {
		return errV2AgentLink
	}
	open := new(remotev2.ChannelOpen)
	if !unmarshalV2AgentMessage(plaintext, open) || open.GetChannelId() != record.GetChannelId() || uuid.Validate(open.GetChannelId()) != nil {
		return errV2AgentLink
	}
	scopes, err := validateV2ChannelScopes(link.allowedScopes, open.GetKind(), open.GetProjectId(), open.GetScopes())
	if err != nil {
		return err
	}
	projectID := strings.TrimSpace(open.GetProjectId())
	if open.GetKind() == remotev2.ChannelKind_CHANNEL_KIND_PROJECT {
		project, projectErr := uuid.Parse(projectID)
		if projectErr != nil || project == uuid.Nil || link.state == nil || link.state.business == nil {
			return errV2AgentLink
		}
		registered, projectErr := link.state.business.projectByID(ctx, project)
		if projectErr != nil || registered.State != "available" {
			return errV2AgentLink
		}
	}
	channel := &v2AgentChannel{id: open.GetChannelId(), kind: open.GetKind(), projectID: projectID, scopes: scopes, streams: make(map[string]*v2AgentStream)}
	link.mu.Lock()
	if link.closed || !link.active {
		link.mu.Unlock()
		return errV2AgentLink
	}
	if existing := link.channels[channel.id]; existing != nil {
		same := existing.kind == channel.kind && existing.projectID == channel.projectID && equalV2AgentScopes(existing.scopes, channel.scopes)
		link.mu.Unlock()
		if !same {
			return errV2AgentLink
		}
		return link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_CHANNEL_ACCEPT, channel.id, v2ChannelControlStreamID, &remotev2.ChannelAccept{
			ChannelId: channel.id, GrantedScopes: mapV2ScopeKeys(existing.scopes), CapabilityRevision: []byte(strconv.FormatUint(link.state.revisionValue(), 10)),
		})
	}
	if len(link.channels) >= v2MaximumChannelsPerLink {
		link.mu.Unlock()
		return errV2AgentLink
	}
	link.channels[channel.id] = channel
	link.mu.Unlock()
	return link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_CHANNEL_ACCEPT, channel.id, v2ChannelControlStreamID, &remotev2.ChannelAccept{
		ChannelId: channel.id, GrantedScopes: mapV2ScopeKeys(scopes), CapabilityRevision: []byte(strconv.FormatUint(link.state.revisionValue(), 10)),
	})
}

func equalV2AgentScopes(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for scope := range left {
		if _, ok := right[scope]; !ok {
			return false
		}
	}
	return true
}

func validateV2ChannelScopes(allowedScopes map[string]struct{}, kind remotev2.ChannelKind, projectID string, values []string) (map[string]struct{}, error) {
	if len(values) == 0 || len(values) > 16 || (kind != remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY && kind != remotev2.ChannelKind_CHANNEL_KIND_PROJECT) {
		return nil, errV2AgentLink
	}
	result := make(map[string]struct{}, len(values))
	for _, scope := range values {
		scope = strings.TrimSpace(scope)
		if scope == "" || len(scope) > 80 || !strings.HasPrefix(scope, "remote.peer.") {
			return nil, errV2AgentLink
		}
		if _, duplicate := result[scope]; duplicate {
			return nil, errV2AgentLink
		}
		if _, allowed := allowedScopes[scope]; !allowed {
			return nil, errV2AgentLink
		}
		result[scope] = struct{}{}
	}
	if kind == remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY {
		if projectID != "" || len(result) != 1 {
			return nil, errV2AgentLink
		}
		// CAPABILITIES_GET is deliberately fixed to remote.peer.query. AI
		// configuration is the sole non-project capability and receives its
		// own least-privilege device Channel, rather than borrowing query
		// authorization or forcing an unrelated project binding.
		if _, query := result["remote.peer.query"]; !query {
			if _, config := result["remote.peer.ai.config"]; !config {
				return nil, errV2AgentLink
			}
		}
		if len(result) != 1 {
			return nil, errV2AgentLink
		}
		return result, nil
	}
	if uuid.Validate(projectID) != nil || projectID != strings.ToLower(projectID) {
		return nil, errV2AgentLink
	}
	for scope := range result {
		if !agentPeerScopeRequiresProject(scope) && scope != "remote.peer.ai.config" {
			return nil, errV2AgentLink
		}
	}
	return result, nil
}

func mapV2ScopeKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for scope := range values {
		result = append(result, scope)
	}
	// Scope order is observable only inside the encrypted Link; make it stable
	// anyway so reconnect tests and telemetry do not depend on Go map order.
	for index := 1; index < len(result); index++ {
		for previous := index; previous > 0 && result[previous] < result[previous-1]; previous-- {
			result[previous], result[previous-1] = result[previous-1], result[previous]
		}
	}
	return result
}

func (link *v2AgentLink) handleStreamOpen(record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if link == nil || !link.isActive() {
		return errV2AgentLink
	}
	open := new(remotev2.StreamOpen)
	if !unmarshalV2AgentMessage(plaintext, open) || open.GetChannelId() != record.GetChannelId() || uuid.Validate(open.GetStreamId()) != nil ||
		(open.GetKind() != remotev2.StreamKind_STREAM_KIND_RPC && open.GetKind() != remotev2.StreamKind_STREAM_KIND_FILE && open.GetKind() != remotev2.StreamKind_STREAM_KIND_EVENT && open.GetKind() != remotev2.StreamKind_STREAM_KIND_TERMINAL) ||
		(open.GetOperationId() != "" && uuid.Validate(open.GetOperationId()) != nil) {
		return errV2AgentLink
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	channel := link.channels[open.GetChannelId()]
	if channel == nil || link.closed {
		return errV2AgentLink
	}
	if existing := channel.streams[open.GetStreamId()]; existing != nil {
		// A byte-for-byte Carrier retransmission is filtered by the replay
		// window before dispatch. A fresh, metadata-identical StreamOpen is also
		// an idempotent existence assertion: it resolves the unavoidable case in
		// which a Carrier dies after accepting STREAM_OPEN but before the Client
		// can know whether Device received it. It never resets Stream state, while
		// changed metadata and tombstoned/closed IDs remain strict reuse errors.
		if existing.kind == open.GetKind() && existing.operationID == open.GetOperationId() {
			return nil
		}
		return peerv2.ErrStreamReuse
	}
	if len(channel.streams) >= v2MaximumStreamsPerChannel {
		return errV2AgentLink
	}
	if err := link.sequencer.OpenStream(open.GetStreamId()); err != nil {
		return err
	}
	streamContext, cancel := context.WithCancel(link.context)
	channel.streams[open.GetStreamId()] = &v2AgentStream{id: open.GetStreamId(), kind: open.GetKind(), operationID: open.GetOperationId(), context: streamContext, cancel: cancel}
	return nil
}

// handleStreamClose reads the target from the encrypted Channel-control
// payload.  The enclosing record always uses v2ChannelControlStreamID to
// derive its Channel key, so using record.StreamID here would accidentally
// close the control stream rather than the requested RPC/File/Event Stream.
func (link *v2AgentLink) handleStreamClose(record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	close := new(remotev2.StreamClose)
	if link == nil || !unmarshalV2AgentMessage(plaintext, close) || record == nil ||
		close.GetChannelId() != record.GetChannelId() || uuid.Validate(close.GetChannelId()) != nil || uuid.Validate(close.GetStreamId()) != nil ||
		close.GetStreamId() == v2ChannelControlStreamID || close.GetStreamId() == v2ControlStreamID {
		return errV2AgentLink
	}
	link.closeStream(close.GetChannelId(), close.GetStreamId())
	return nil
}

func (link *v2AgentLink) handleRPCRequest(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	if link == nil || !link.isActive() {
		return errV2AgentLink
	}
	request := new(remotev2.RpcRequest)
	if !unmarshalV2AgentMessage(plaintext, request) || uuid.Validate(request.GetOperationId()) != nil || uuid.Validate(request.GetAttemptId()) != nil ||
		!validMethod(request.GetMethod()) || request.GetDeadline() == nil || request.GetDeadline().CheckValid() != nil || !request.GetDeadline().AsTime().After(time.Now().UTC()) ||
		len(request.GetPayload()) > maximumRPCPayload {
		return errV2AgentLink
	}
	link.mu.Lock()
	channel := link.channels[record.GetChannelId()]
	var stream *v2AgentStream
	if channel != nil {
		stream = channel.streams[record.GetStreamId()]
	}
	if channel == nil || stream == nil || stream.kind != remotev2.StreamKind_STREAM_KIND_RPC ||
		(stream.operationID != "" && stream.operationID != request.GetOperationId()) {
		link.mu.Unlock()
		return errV2AgentStream
	}
	channelScope := v2ChannelScopeForMethod(channel, request.GetMethod())
	if channelScope == "" {
		link.mu.Unlock()
		go link.sendRPCProtocolError(ctx, channel.id, stream.id, request, "scope_forbidden", false)
		return nil
	}
	channelID, channelProjectID, streamID := channel.id, channel.projectID, stream.id
	stream.operationID = request.GetOperationId()
	streamContext := stream.context
	if streamContext == nil {
		streamContext = link.context
	}
	link.mu.Unlock()
	releaseRPC := func() {}
	if link.registry != nil {
		var acquired bool
		releaseRPC, acquired = link.registry.tryAcquireRPC(link.binding.ClientID, link.id)
		if !acquired {
			requestCopy := proto.Clone(request).(*remotev2.RpcRequest)
			go func() {
				defer link.closeStream(channelID, streamID)
				link.sendRPCProtocolError(ctx, channelID, streamID, requestCopy, "rpc_capacity_exhausted", false)
			}()
			return nil
		}
	}
	var deadlineCancel context.CancelFunc
	if deadline := request.GetDeadline().AsTime(); deadline.Before(time.Now().Add(24 * time.Hour)) {
		streamContext, deadlineCancel = context.WithDeadline(streamContext, deadline)
	}
	requestCopy := proto.Clone(request).(*remotev2.RpcRequest)
	go func() {
		defer releaseRPC()
		if deadlineCancel != nil {
			defer deadlineCancel()
		}
		link.executeRPC(streamContext, channelID, streamID, channelProjectID, channelScope, requestCopy)
	}()
	return nil
}

// v2ChannelAllowsMethod keeps the Device-query Channel deliberately narrow,
// while preserving the established task read/write aliases behind the single
// encrypted remote.peer.task.control grant.  The legacy dispatcher receives
// the concrete method scope only after this v2 Channel authorization check.
func v2ChannelAllowsMethod(channel *v2AgentChannel, method string) bool {
	return v2ChannelScopeForMethod(channel, method) != ""
}

func v2ChannelScopeForMethod(channel *v2AgentChannel, method string) string {
	if channel == nil || methodScope(method) == "" {
		return ""
	}
	if method == "agent.capabilities.get" {
		_, allowed := channel.scopes["remote.peer.query"]
		if channel.kind == remotev2.ChannelKind_CHANNEL_KIND_DEVICE_QUERY && channel.projectID == "" && allowed {
			return "remote.peer.query"
		}
		return ""
	}
	if required := methodScope(method); required != "" {
		if _, allowed := channel.scopes[required]; allowed {
			return required
		}
	}
	for scope := range channel.scopes {
		if methodAllowsScope(method, scope) {
			return scope
		}
	}
	return ""
}

func (link *v2AgentLink) executeRPC(ctx context.Context, channelID, streamID, channelProjectID, channelScope string, request *remotev2.RpcRequest) {
	if link == nil || request == nil {
		return
	}
	// A completed RPC no longer needs server-side Stream state. Releasing it
	// here is authoritative; the peer's best-effort STREAM_CLOSE remains an
	// idempotent acknowledgement rather than the only cleanup path. This keeps
	// a long-lived Channel below its 64-Stream cap even when close frames are
	// lost during a Carrier transition.
	defer link.closeStream(channelID, streamID)
	if link.state == nil {
		link.sendRPCProtocolError(ctx, channelID, streamID, request, "device_unavailable", true)
		return
	}

	// Read-only calls are safe to execute again and should return a fresh
	// snapshot. Persisting every catalog poll, event replay and 56 KiB message
	// page in the 24-hour idempotency journal caused continuous SQLite writes
	// and retained the same responses for the whole Link lifetime. Reserve both
	// caches for operations whose side effects require exactly-once replay.
	readOnly := v2RPCReadOnlyMethod(request.GetMethod())
	var digest [sha256.Size]byte
	var operationClaim *v2InFlightOperation
	var mutationContext v2OperationMutationContext
	var sideEffectTracker *v2SideEffectTracker
	if !readOnly {
		digest = v2RPCOperationDigest(
			link.binding.ClientID,
			channelProjectID,
			channelScope,
			request,
		)
		mutationContext = v2OperationMutationContext{
			OperationID: request.GetOperationId(),
			Digest:      digest,
			Controller:  link.binding.ClientID,
			Project:     channelProjectID,
			Method:      request.GetMethod(),
			Now:         time.Now().UTC(),
		}
		claim, owner, claimErr := link.state.claimV2Operation(request.GetOperationId(), digest)
		if errors.Is(claimErr, errRPCIdempotency) {
			link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_conflict", false)
			return
		}
		if claimErr != nil {
			link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_capacity_exhausted", false)
			return
		}
		if !owner {
			response, waitErr := claim.wait(ctx)
			if waitErr != nil {
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_in_progress", true)
				return
			}
			response.AttemptId = request.GetAttemptId()
			response.Replayed = true
			_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, channelID, streamID, response)
			return
		}
		operationClaim = claim
		defer link.state.finishV2Operation(request.GetOperationId(), operationClaim, nil)
		if cached, found := link.cachedOperation(request.GetOperationId(), digest); found {
			link.state.finishV2Operation(request.GetOperationId(), operationClaim, cached.response)
			link.sendCachedOperation(ctx, channelID, streamID, request.GetAttemptId(), cached)
			return
		}
		if link.state != nil && link.state.business != nil {
			lookupNow := time.Now().UTC()
			stored, found, lookupErr := link.state.business.loadV2Operation(ctx, request.GetOperationId(), digest, lookupNow)
			if lookupErr == nil && found {
				cached := v2AgentOperation{digest: digest, response: stored, cachedAt: time.Now().UTC()}
				link.cacheOperation(request.GetOperationId(), digest, stored, nil)
				link.state.finishV2Operation(request.GetOperationId(), operationClaim, stored)
				link.sendCachedOperation(ctx, channelID, streamID, request.GetAttemptId(), cached)
				return
			}
			if lookupErr != nil && !errors.Is(lookupErr, errRPCIdempotency) {
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_store_unavailable", true)
				return
			}
			if errors.Is(lookupErr, errRPCIdempotency) {
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_conflict", false)
				return
			}
			committed, claimErr := link.state.business.loadV2OperationClaim(
				ctx,
				request.GetOperationId(),
				digest,
				lookupNow,
			)
			if errors.Is(claimErr, errRPCIdempotency) {
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_conflict", false)
				return
			}
			if claimErr != nil {
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_store_unavailable", true)
				return
			}
			if committed {
				response := v2RPCUnknownCommitResponse(request)
				response.Replayed = true
				link.state.finishV2Operation(request.GetOperationId(), operationClaim, response)
				_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, channelID, streamID, response)
				return
			}
		}
		if v2RPCDurableSideEffectMethod(request.GetMethod()) {
			if link.state.business == nil {
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_store_unavailable", true)
				return
			}
			state, prepareErr := link.state.business.prepareV2SideEffect(ctx, mutationContext)
			switch {
			case errors.Is(prepareErr, errRPCIdempotency):
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_conflict", false)
				return
			case errors.Is(prepareErr, errV2OperationJournalCapacity):
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_capacity_exhausted", false)
				return
			case prepareErr != nil:
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_store_unavailable", true)
				return
			case state == v2SideEffectStarted || state == v2SideEffectCommitted:
				response := v2RPCUnknownCommitResponse(request)
				response.Replayed = true
				link.state.finishV2Operation(request.GetOperationId(), operationClaim, response)
				_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, channelID, streamID, response)
				return
			case state != v2SideEffectPrepared:
				link.sendRPCProtocolError(ctx, channelID, streamID, request, "operation_store_unavailable", true)
				return
			default:
				sideEffectTracker = &v2SideEffectTracker{
					store: link.state.business, value: mutationContext, state: v2SideEffectPrepared,
				}
			}
		}
	}
	legacy, err := link.legacyRPCRequest(channelID, request)
	if err != nil {
		if sideEffectTracker != nil {
			_ = sideEffectTracker.discardPrepared(ctx)
		}
		link.sendRPCProtocolError(ctx, channelID, streamID, request, "request_invalid", false)
		return
	}
	dispatch := dispatcher{
		state: link.state, controlStore: link.state.controlStore, controlLoop: link.state.controlLoop, tasks: newEncryptedControlTaskRepository(link.state.controlStore),
		now: func() time.Time { return time.Now().UTC() }, scope: channelScope, ticketProjectID: channelProjectID, enforceProjectBinding: true,
		peerSessionID: link.id,
	}
	ctx = v2RPCDispatchContext(ctx, request.GetMethod())
	if !readOnly {
		ctx = withV2OperationMutationContext(ctx, mutationContext)
		ctx = withV2SideEffectTracker(ctx, sideEffectTracker)
	}
	// Live RPCs must retain their Stream context while events are produced,
	// rather than being flattened into the one-shot dispatchStream compatibility
	// path. In particular, terminal.attach is a bounded long poll: dispatchStream
	// only runs the initial snapshot and would therefore return immediately
	// without ever forwarding terminal.output/terminal.exit. That made a remote
	// xterm send input successfully while receiving no output and spin on empty
	// attach calls. The callback encrypts every event immediately with the active
	// Link generation so rekeying does not rebuild the business Stream.
	if v2RPCLiveMethod(request.GetMethod()) {
		responseEnvelope := dispatch.dispatchLive(ctx, legacy, func(eventEnvelope *remotev1.RpcEnvelope) error {
			value := eventEnvelope.GetEvent()
			if value == nil {
				return errV2AgentLink
			}
			return link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_EVENT, channelID, streamID, &remotev2.RpcEvent{
				OperationId: request.GetOperationId(), EventSequence: value.GetSequence(), Payload: append([]byte(nil), value.GetJsonPayload()...),
				EventId: value.GetEventId(), HighWatermark: value.GetHighWatermark(),
			})
		})
		response := v2RPCResponseFromLegacy(request, responseEnvelope)
		if !readOnly {
			response = resolveV2SideEffectResponse(ctx, request, response, sideEffectTracker)
			response = link.persistV2Operation(ctx, request, channelProjectID, digest, response, nil)
			link.state.finishV2Operation(request.GetOperationId(), operationClaim, response)
		}
		_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, channelID, streamID, response)
		return
	}
	responseEnvelope, events := dispatch.dispatchStream(ctx, legacy)
	response := v2RPCResponseFromLegacy(request, responseEnvelope)
	v2Events := make([]*remotev2.RpcEvent, 0, len(events))
	for _, event := range events {
		if value := event.GetEvent(); value != nil {
			v2Events = append(v2Events, &remotev2.RpcEvent{
				OperationId:   request.GetOperationId(),
				EventSequence: value.GetSequence(),
				Payload:       append([]byte(nil), value.GetJsonPayload()...),
				EventId:       value.GetEventId(),
				HighWatermark: value.GetHighWatermark(),
			})
		}
	}
	if !readOnly {
		response = resolveV2SideEffectResponse(ctx, request, response, sideEffectTracker)
		response = link.persistV2Operation(ctx, request, channelProjectID, digest, response, v2Events)
		link.state.finishV2Operation(request.GetOperationId(), operationClaim, response)
	}
	for _, event := range v2Events {
		if err := link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_EVENT, channelID, streamID, event); err != nil {
			return
		}
	}
	_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, channelID, streamID, response)
}

func resolveV2SideEffectResponse(ctx context.Context, request *remotev2.RpcRequest, response *remotev2.RpcResponse, tracker *v2SideEffectTracker) *remotev2.RpcResponse {
	if tracker == nil || request == nil || response == nil {
		return response
	}
	state, outcome, uncertain := tracker.responseDisposition()
	if v2RPCDurableOutcome(response) {
		if !uncertain && (state == v2SideEffectCommitted || outcome == v2SideEffectNoEffect) {
			return response
		}
		// A successful handler that forgot to cross the explicit boundary is not
		// safe to replay. Fence it as started before returning unknown-commit.
		if !uncertain && state == v2SideEffectPrepared && outcome == v2SideEffectActive {
			_ = beginV2SideEffect(ctx)
		}
		return v2RPCUnknownCommitResponse(request)
	}
	if outcome == v2SideEffectRolledBack {
		return response
	}
	if uncertain || state == v2SideEffectStarted || state == v2SideEffectCommitted {
		return v2RPCUnknownCommitResponse(request)
	}
	if err := tracker.discardPrepared(ctx); err != nil {
		return v2RPCOperationStoreUnavailableResponse(request)
	}
	return response
}

func (link *v2AgentLink) persistV2Operation(ctx context.Context, request *remotev2.RpcRequest, projectID string, digest [sha256.Size]byte, response *remotev2.RpcResponse, events []*remotev2.RpcEvent) *remotev2.RpcResponse {
	if link == nil || link.state == nil || request == nil || response == nil || !v2RPCDurableOutcome(response) {
		return response
	}
	if link.state.business == nil {
		return v2RPCUnknownCommitResponse(request)
	}
	storeContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := link.state.business.saveV2OperationScoped(
		storeContext,
		request.GetOperationId(),
		digest,
		response,
		time.Now().UTC(),
		link.binding.ClientID,
		projectID,
	)
	cancel()
	if err != nil {
		return v2RPCUnknownCommitResponse(request)
	}
	link.cacheOperation(request.GetOperationId(), digest, response, events)
	return response
}

func v2RPCDurableOutcome(response *remotev2.RpcResponse) bool {
	return response != nil && response.GetErrorCode() == remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED && response.GetSafeErrorCode() == ""
}

func v2RPCUnknownCommitResponse(request *remotev2.RpcRequest) *remotev2.RpcResponse {
	return &remotev2.RpcResponse{
		OperationId: request.GetOperationId(), AttemptId: request.GetAttemptId(),
		ErrorCode:     remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID,
		SafeErrorCode: "operation_commit_unknown", Retryable: true, RetryAfterSeconds: 1,
	}
}

func v2RPCOperationStoreUnavailableResponse(request *remotev2.RpcRequest) *remotev2.RpcResponse {
	return &remotev2.RpcResponse{
		OperationId: request.GetOperationId(), AttemptId: request.GetAttemptId(),
		ErrorCode:     remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID,
		SafeErrorCode: "operation_store_unavailable", Retryable: true, RetryAfterSeconds: 1,
	}
}

func v2RPCLiveMethod(method string) bool {
	if v2RPCDurableGenerationMethod(method) {
		return true
	}
	switch method {
	case "conversation.generation.attach", "terminal.attach":
		return true
	default:
		return false
	}
}

// v2RPCDurableGenerationMethod includes the legacy send aliases still exposed
// by the Agent contract. Every entry ultimately starts the same persistent AI
// driver and therefore must have identical disconnect semantics.
func v2RPCDurableGenerationMethod(method string) bool {
	switch method {
	case "conversation.send", "conversation.chat.send", "chat.send", "conversation.regenerate":
		return true
	default:
		return false
	}
}

// v2RPCDispatchContext distinguishes closing an obsolete transport Stream from
// the explicit conversation.cancel business operation. A Carrier interruption
// makes the controller abandon its old send/regenerate Stream and, after Link
// recovery, best-effort close it. The durable Agent generation must keep
// running so a new generation.attach Stream can resume from its event cursor.
func v2RPCDispatchContext(ctx context.Context, method string) context.Context {
	if v2RPCDurableGenerationMethod(method) {
		return withPeerRPCTransportCancellation(ctx)
	}
	return ctx
}

// v2RPCReadOnlyMethod must stay aligned with the three v2 clients' timeout
// and unknown-commit classification. These operations are observations: a
// retry may return a newer snapshot but cannot duplicate a side effect.
func v2RPCReadOnlyMethod(method string) bool {
	return strings.HasSuffix(method, ".get") || strings.HasSuffix(method, ".list") ||
		strings.HasSuffix(method, ".stat") || strings.HasSuffix(method, ".query") ||
		method == "conversation.search" || method == "conversation.messages.before" ||
		method == "conversation.message.content" || method == "conversation.events" ||
		method == "conversation.generation.attach" || method == "event.subscribe" ||
		method == "terminal.attach" || method == "task.logs" || method == "task.logs.download.prepare"
}

func (link *v2AgentLink) sendCachedOperation(ctx context.Context, channelID, streamID, attemptID string, cached v2AgentOperation) {
	if link == nil || cached.response == nil {
		return
	}
	response := proto.Clone(cached.response).(*remotev2.RpcResponse)
	response.AttemptId, response.Replayed = attemptID, true
	for _, event := range cached.events {
		clone := proto.Clone(event).(*remotev2.RpcEvent)
		_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_EVENT, channelID, streamID, clone)
	}
	_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, channelID, streamID, response)
}

func (link *v2AgentLink) legacyRPCRequest(channelID string, request *remotev2.RpcRequest) (*remotev1.RpcEnvelope, error) {
	if link == nil || request == nil || link.state == nil {
		return nil, errV2AgentLink
	}
	projectID := channelIDProject(link, channelID)
	header := &remotev1.RpcRequestHeader{
		RequestId: request.GetAttemptId(), IdempotencyKey: request.GetOperationId(), Deadline: timestamppb.New(request.GetDeadline().AsTime()), ProjectId: projectID,
	}
	if request.ExpectedRevision != nil {
		value := request.GetExpectedRevision()
		header.ExpectedRevision = &value
	}
	return &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
		Header: header, Method: request.GetMethod(), JsonPayload: append([]byte(nil), request.GetPayload()...),
	}}}, nil
}

func channelIDProject(link *v2AgentLink, channelID string) string {
	if link == nil {
		return ""
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	if channel := link.channels[channelID]; channel != nil {
		return channel.projectID
	}
	return ""
}

func v2RPCResponseFromLegacy(request *remotev2.RpcRequest, envelope *remotev1.RpcEnvelope) *remotev2.RpcResponse {
	response := &remotev2.RpcResponse{OperationId: request.GetOperationId(), AttemptId: request.GetAttemptId()}
	legacy := envelope.GetResponse()
	if legacy == nil {
		response.ErrorCode, response.SafeErrorCode, response.Retryable = remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID, "device_response_invalid", true
		return response
	}
	if header := legacy.GetHeader(); header != nil {
		response.Revision, response.Replayed = header.GetRevision(), header.GetReplayed()
	}
	if failure := legacy.GetError(); failure != nil {
		response.ErrorCode = remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID
		// safe_error_code is consumed as a stable machine code by every v2
		// client. Forwarding the v1 human-readable message (for example
		// "resource not found") made all missing-resource failures ambiguous
		// and prevented the UI from recovering a deleted conversation.
		response.SafeErrorCode = v2RPCSafeErrorCode(failure)
		response.Retryable, response.RetryAfterSeconds = failure.GetRetryable(), failure.GetRetryAfterSeconds()
		return response
	}
	response.Payload = append([]byte(nil), legacy.GetJsonPayload()...)
	return response
}

func v2RPCSafeErrorCode(failure *remotev1.RpcError) string {
	if failure == nil {
		return "remote_operation_failed"
	}
	if semantic := strings.TrimSpace(failure.GetSafeMessage()); v2RPCSemanticErrorCode(semantic) {
		return semantic
	}
	return v2RPCErrorCodeName(failure.GetCode())
}

func v2RPCSemanticErrorCode(value string) bool {
	if len(value) < 3 || len(value) > 80 || !strings.Contains(value, "_") {
		return false
	}
	for _, character := range value {
		if character != '_' && (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func v2RPCErrorCodeName(code remotev1.RpcErrorCode) string {
	switch code {
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT:
		return "invalid_argument"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND:
		return "not_found"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN:
		return "forbidden"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT:
		return "revision_conflict"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_IDEMPOTENCY_CONFLICT:
		return "idempotency_conflict"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY:
		return "busy"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED:
		return "cancelled"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL:
		return "internal"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH:
		return "project_mismatch"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE:
		return "capability_unavailable"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_DEADLINE_EXCEEDED:
		return "deadline_exceeded"
	case remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED:
		return "resource_exhausted"
	default:
		return "remote_operation_failed"
	}
}

func (link *v2AgentLink) sendRPCProtocolError(ctx context.Context, channelID, streamID string, request *remotev2.RpcRequest, safeCode string, retryable bool) {
	if link == nil || request == nil {
		return
	}
	_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, channelID, streamID, &remotev2.RpcResponse{
		OperationId: request.GetOperationId(), AttemptId: request.GetAttemptId(), ErrorCode: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID,
		SafeErrorCode: safeCode, Retryable: retryable,
	})
}

func v2RPCOperationDigest(controllerID, projectID, scope string, request *remotev2.RpcRequest) [sha256.Size]byte {
	encoded := make([]byte, 0, len(controllerID)+len(projectID)+len(scope)+len(request.GetMethod())+len(request.GetPayload())+128)
	encoded = append(encoded, []byte("wenzwork-remote-v2/operation-v2")...)
	encoded = append(encoded, 0)
	encoded = append(encoded, controllerID...)
	encoded = append(encoded, 0)
	encoded = append(encoded, projectID...)
	encoded = append(encoded, 0)
	encoded = append(encoded, scope...)
	encoded = append(encoded, 0)
	encoded = append(encoded, request.GetMethod()...)
	encoded = append(encoded, 0)
	encoded = append(encoded, request.GetPayload()...)
	if request.ExpectedRevision != nil {
		encoded = append(encoded, 1)
		encoded = strconv.AppendUint(encoded, request.GetExpectedRevision(), 10)
	} else {
		encoded = append(encoded, 0)
	}
	return sha256.Sum256(encoded)
}

func (link *v2AgentLink) cachedOperation(operationID string, digest [sha256.Size]byte) (v2AgentOperation, bool) {
	if link == nil || operationID == "" {
		return v2AgentOperation{}, false
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	operation, ok := link.operations[operationID]
	if ok && (operation.cachedAt.IsZero() || time.Since(operation.cachedAt) > v2CachedOperationTTL) {
		link.operationBytes -= operation.bytes
		delete(link.operations, operationID)
		return v2AgentOperation{}, false
	}
	return operation, ok && operation.digest == digest && operation.response != nil
}

func (link *v2AgentLink) cacheOperation(operationID string, digest [sha256.Size]byte, response *remotev2.RpcResponse, events []*remotev2.RpcEvent) {
	if link == nil || operationID == "" || response == nil {
		return
	}
	if len(events) > v2MaximumOperationEvents {
		return
	}
	eventBytes := 0
	for _, event := range events {
		if event != nil {
			eventBytes += proto.Size(event)
		}
	}
	weight := proto.Size(response) + eventBytes
	if eventBytes > v2MaximumOperationEventBytes || weight <= 0 || weight > v2MaximumOperationBytes {
		return
	}
	clonedEvents := make([]*remotev2.RpcEvent, 0, len(events))
	for _, event := range events {
		if event != nil {
			clonedEvents = append(clonedEvents, proto.Clone(event).(*remotev2.RpcEvent))
		}
	}
	now := time.Now().UTC()
	link.mu.Lock()
	if !link.closed {
		for id, operation := range link.operations {
			if operation.cachedAt.IsZero() || now.Sub(operation.cachedAt) > v2CachedOperationTTL {
				link.operationBytes -= operation.bytes
				delete(link.operations, id)
			}
		}
		if previous, exists := link.operations[operationID]; exists {
			link.operationBytes -= previous.bytes
			delete(link.operations, operationID)
		}
		for len(link.operations) >= v2MaximumCachedOperations || link.operationBytes+weight > v2MaximumOperationBytes {
			oldestID := ""
			var oldest time.Time
			for id, operation := range link.operations {
				if oldestID == "" || operation.cachedAt.Before(oldest) {
					oldestID, oldest = id, operation.cachedAt
				}
			}
			if oldestID == "" {
				break
			}
			link.operationBytes -= link.operations[oldestID].bytes
			delete(link.operations, oldestID)
		}
		link.operations[operationID] = v2AgentOperation{
			digest: digest, response: proto.Clone(response).(*remotev2.RpcResponse), events: clonedEvents, cachedAt: now, bytes: weight,
		}
		link.operationBytes += weight
	}
	link.mu.Unlock()
}

func (link *v2AgentLink) closeChannel(channelID string) {
	if link == nil || channelID == "" {
		return
	}
	link.mu.Lock()
	channel := link.channels[channelID]
	delete(link.channels, channelID)
	streams := make([]*v2AgentStream, 0)
	if channel != nil {
		for _, stream := range channel.streams {
			streams = append(streams, stream)
		}
		channel.streams = make(map[string]*v2AgentStream)
	}
	link.mu.Unlock()
	if channel == nil {
		return
	}
	for _, stream := range streams {
		if stream == nil {
			continue
		}
		unlock := link.lockOutboundStream(stream.id)
		link.finishStreamLifecycle(stream)
		unlock()
	}
}

func (link *v2AgentLink) closeStream(channelID, streamID string) {
	if link == nil || channelID == "" || streamID == "" {
		return
	}
	unlock := link.lockOutboundStream(streamID)
	defer unlock()
	link.mu.Lock()
	channel := link.channels[channelID]
	var stream *v2AgentStream
	if channel != nil {
		stream = channel.streams[streamID]
		delete(channel.streams, streamID)
	}
	link.mu.Unlock()
	link.finishStreamLifecycle(stream)
}

func (link *v2AgentLink) finishStreamLifecycle(stream *v2AgentStream) {
	if link == nil || stream == nil {
		return
	}
	if stream != nil && stream.cancel != nil {
		stream.cancel()
	}
	if stream != nil && stream.cleanup != nil {
		stream.cleanup()
	}
	if stream.id != "" && link.sequencer != nil {
		if err := link.sequencer.CloseStream(stream.id, time.Now().UTC()); err != nil {
			// Never evict replay state while a Key is valid. The client normally
			// rekeys before this hard limit; if it does not, rebuilding the Link is
			// the only bounded and nonce-safe fallback.
			go link.close()
		} else if link.sequencer.ShouldRollover() {
			// Used IDs are Link-lifetime state. Close while there is still ample
			// headroom so the controller can establish a fresh Link cleanly.
			go link.close()
		}
	}
}

// failStream confines a malformed business frame to its owning Stream and
// gives the peer an encrypted protocol error. The Link/Carrier remain usable,
// while a failed send (for example during a Carrier suspension) is harmless:
// the local Stream is already cancelled and the resumed operation can be
// retried with its stable operation_id.
func (link *v2AgentLink) failStream(ctx context.Context, channelID, streamID string, reason remotev2.ProtocolErrorCode) {
	if link == nil || channelID == "" || streamID == "" || streamID == v2ControlStreamID || streamID == v2ChannelControlStreamID {
		return
	}
	if reason == remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		reason = remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_FRAME_INVALID
	}
	link.closeStream(channelID, streamID)
	if uuid.Validate(channelID) != nil || uuid.Validate(streamID) != nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_STREAM_CLOSE, channelID, v2ChannelControlStreamID, &remotev2.StreamClose{
		ChannelId: channelID,
		StreamId:  streamID,
		Reason:    reason,
	})
}

func (link *v2AgentLink) sendEncrypted(ctx context.Context, keyID uint64, frameType remotev2.FrameType, channelID, streamID string, message proto.Message) error {
	if link == nil || message == nil || keyID == 0 {
		return errV2AgentLink
	}
	unlock := link.lockOutboundStream(streamID)
	defer unlock()
	link.mu.Lock()
	carrier, closed := link.carrier, link.closed
	link.mu.Unlock()
	if closed || carrier == nil {
		return errV2AgentCarrier
	}
	plaintext, err := proto.Marshal(message)
	if err != nil || len(plaintext) == 0 || len(plaintext) > peerv2.MaximumPlaintextBytes {
		return errV2AgentLink
	}
	defer zeroV2Bytes(plaintext)
	metadata := peerv2.RecordMetadata{
		LinkID: link.id, ChannelID: channelID, StreamID: streamID, KeyID: keyID, Direction: peerv2.DirectionDeviceToClient,
		FrameType: peerv2.FrameType(frameType),
	}
	sequence, err := link.sequencer.Next(keyID, peerv2.DirectionDeviceToClient, streamID)
	if err != nil {
		return errV2AgentLink
	}
	metadata.StreamSequence = sequence
	rootKey, err := link.keys.RootKey(keyID)
	if err != nil {
		return errV2AgentLink
	}
	streamKey, err := v2AgentRecordKey(rootKey, metadata)
	zeroV2Bytes(rootKey)
	if err != nil {
		return errV2AgentLink
	}
	ciphertext, err := peerv2.Seal(streamKey, plaintext, metadata)
	zeroV2Bytes(streamKey)
	if err != nil {
		return errV2AgentLink
	}
	return carrier.sendLink(ctx, &remotev2.LinkEnvelope{LinkId: link.id, Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
		LinkId: link.id, ChannelId: channelID, StreamId: streamID, KeyId: keyID, Direction: remotev2.Direction_DIRECTION_DEVICE_TO_CLIENT,
		FrameType: frameType, StreamSequence: sequence, Ciphertext: ciphertext,
	}}})
}

// lockOutboundStream keeps encryption, sequence allocation and Carrier
// enqueue ordered for one Stream. Different Streams can still prepare records
// concurrently, while the Carrier remains the sole WebSocket writer.
func (link *v2AgentLink) lockOutboundStream(streamID string) func() {
	link.sendLocksMu.Lock()
	if link.sendLocks == nil {
		link.sendLocks = make(map[string]*v2AgentSendLock)
	}
	gate := link.sendLocks[streamID]
	if gate == nil {
		gate = &v2AgentSendLock{}
		link.sendLocks[streamID] = gate
	}
	gate.users++
	link.sendLocksMu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		link.sendLocksMu.Lock()
		gate.users--
		if gate.users == 0 && link.sendLocks[streamID] == gate {
			delete(link.sendLocks, streamID)
		}
		link.sendLocksMu.Unlock()
	}
}

func unmarshalV2AgentMessage(payload []byte, message proto.Message) bool {
	if len(payload) == 0 || message == nil || proto.Unmarshal(payload, message) != nil {
		return false
	}
	return len(message.ProtoReflect().GetUnknown()) == 0
}

const (
	v2MinimumFileChunkSize = 4 << 10
	// An encrypted FileChunk includes its protobuf transfer ID and hash in
	// addition to payload bytes. Leave a conservative header margin below the
	// one-MiB AEAD plaintext ceiling so a Manifest-approved chunk can always be
	// sealed instead of failing the Link after allocation.
	v2MaximumFileChunkSize = (1 << 20) - (4 << 10)
	// The browser sends at most eight chunks concurrently. A larger allowance
	// absorbs retransmission reordering while keeping adversarial sparse state
	// strictly bounded independently of total file length.
	v2MaximumFileChunkReordering = 64
)

func (link *v2AgentLink) handleFileManifest(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	manifest := new(remotev2.FileManifest)
	if link == nil || !unmarshalV2AgentMessage(plaintext, manifest) || uuid.Validate(manifest.GetTransferId()) != nil ||
		manifest.GetTotalLength() > maximumManagedFileBytes ||
		manifest.GetChunkSize() < v2MinimumFileChunkSize || manifest.GetChunkSize() > v2MaximumFileChunkSize ||
		len(manifest.GetSha256()) != sha256.Size || !validV2RelativePathHandle(manifest.GetRelativePathHandle()) {
		return errV2AgentLink
	}
	stream, channel := link.streamAndChannel(record.GetChannelId(), record.GetStreamId())
	if stream == nil || channel == nil || stream.kind != remotev2.StreamKind_STREAM_KIND_FILE || channel.kind != remotev2.ChannelKind_CHANNEL_KIND_PROJECT {
		return errV2AgentLink
	}
	_, uploadScope := channel.scopes["remote.peer.file.send"]
	_, downloadScope := channel.scopes["remote.peer.file.receive"]
	_, taskLogScope := channel.scopes["remote.peer.task.control"]
	if uploadScope && (downloadScope || taskLogScope) || !uploadScope && !downloadScope && !taskLogScope {
		return errV2AgentLink
	}
	if downloadScope || taskLogScope {
		return link.handleV2DownloadManifest(ctx, record, manifest, stream, channel)
	}
	link.mu.Lock()
	if existing := link.files[manifest.GetTransferId()]; existing != nil {
		same := existing.channelID == record.GetChannelId() && existing.streamID == record.GetStreamId() && existing.totalLength == manifest.GetTotalLength() &&
			existing.chunkSize == manifest.GetChunkSize() && existing.relativePathHandle == manifest.GetRelativePathHandle() &&
			existing.expectedRevision == manifest.GetExpectedRevision() && string(existing.sha256[:]) == string(manifest.GetSha256())
		link.mu.Unlock()
		if !same {
			return errV2AgentLink
		}
		return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{TransferId: manifest.GetTransferId()})
	}
	link.mu.Unlock()

	file, stagingPath, acceptedOffset, err := link.managedV2UploadFile(channel.projectID, manifest)
	if err != nil {
		return errV2AgentLink
	}
	confirmedThrough := acceptedOffset / uint64(manifest.GetChunkSize())
	if acceptedOffset == manifest.GetTotalLength() {
		confirmedThrough = (manifest.GetTotalLength() + uint64(manifest.GetChunkSize()) - 1) / uint64(manifest.GetChunkSize())
	}
	transfer := &v2AgentFileTransfer{
		transferID: manifest.GetTransferId(), channelID: record.GetChannelId(), streamID: record.GetStreamId(), projectID: channel.projectID,
		relativePathHandle: manifest.GetRelativePathHandle(),
		totalLength:        manifest.GetTotalLength(), chunkSize: manifest.GetChunkSize(), stagingPath: stagingPath, file: file, managedUpload: true,
		confirmedThrough: confirmedThrough, confirmedSparse: make(map[uint64]struct{}),
		expectedRevision: manifest.GetExpectedRevision(), createdAt: time.Now().UTC(),
	}
	copy(transfer.sha256[:], manifest.GetSha256())
	link.mu.Lock()
	if link.closed || link.files[transfer.transferID] != nil {
		link.mu.Unlock()
		_ = file.Close()
		return errV2AgentLink
	}
	link.files[transfer.transferID] = transfer
	stream.cleanup = func() { link.removeFileTransfer(transfer.transferID) }
	link.mu.Unlock()
	return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{TransferId: transfer.transferID})
}

// managedV2UploadFile resolves the opaque manifest handle through a preceding
// encrypted file.upload.prepare RPC.  The handle is the transfer ID, never a
// path: the already-authorised RPC owns the project, destination and revision
// fence, while this bulk Stream only receives the persisted staging file.
func (link *v2AgentLink) managedV2UploadFile(projectID string, manifest *remotev2.FileManifest) (*os.File, string, uint64, error) {
	if link == nil || link.state == nil || manifest == nil || manifest.GetRelativePathHandle() != manifest.GetTransferId() ||
		uuid.Validate(projectID) != nil || manifest.GetTotalLength() > math.MaxInt64 {
		return nil, "", 0, errV2AgentLink
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.cleanup(time.Now().UTC())
	upload := manager.uploads[manifest.GetTransferId()]
	if upload == nil || upload.ProjectID.String() != projectID || upload.Size != int64(manifest.GetTotalLength()) ||
		upload.SHA256 != base64.RawURLEncoding.EncodeToString(manifest.GetSha256()) || upload.ExpectedRevision != manifest.GetExpectedRevision() ||
		upload.Offset < 0 || upload.Offset > upload.Size {
		return nil, "", 0, errV2AgentLink
	}
	info, err := os.Stat(upload.Temporary)
	if err != nil || info.Size() < upload.Offset || info.Size() > upload.Size {
		return nil, "", 0, errV2AgentLink
	}
	file, err := os.OpenFile(upload.Temporary, os.O_RDWR, 0)
	if err != nil {
		return nil, "", 0, errV2AgentLink
	}
	upload.ExpiresAt = time.Now().UTC().Add(fileTransferTTL)
	return file, upload.Temporary, uint64(upload.Offset), nil
}

func validV2RelativePathHandle(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "/\\:\r\n\x00") || strings.Contains(value, "..") {
		return false
	}
	for _, character := range value {
		if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func (link *v2AgentLink) createFileStaging(transferID string) (string, *os.File, error) {
	if link == nil || link.state == nil || strings.TrimSpace(link.state.path) == "" || uuid.Validate(transferID) != nil {
		return "", nil, errV2AgentLink
	}
	directory := filepath.Join(filepath.Dir(link.state.path), "remote-v2-staging")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", nil, err
	}
	path := filepath.Join(directory, transferID+".part")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	return path, file, nil
}

func (link *v2AgentLink) handleFileChunk(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	chunk := new(remotev2.FileChunk)
	if link == nil || !unmarshalV2AgentMessage(plaintext, chunk) || uuid.Validate(chunk.GetTransferId()) != nil || len(chunk.GetChunkHash()) != sha256.Size || len(chunk.GetPayload()) == 0 {
		return errV2AgentLink
	}
	transfer := link.fileTransfer(chunk.GetTransferId(), record.GetChannelId(), record.GetStreamId())
	if transfer == nil || uint64(len(chunk.GetPayload())) > uint64(transfer.chunkSize) {
		return errV2AgentLink
	}
	chunks := (transfer.totalLength + uint64(transfer.chunkSize) - 1) / uint64(transfer.chunkSize)
	if chunk.GetIndex() >= chunks {
		return errV2AgentLink
	}
	expectedSize := uint64(transfer.chunkSize)
	if chunk.GetIndex()+1 == chunks {
		expectedSize = transfer.totalLength - chunk.GetIndex()*uint64(transfer.chunkSize)
	}
	if uint64(len(chunk.GetPayload())) != expectedSize || sha256.Sum256(chunk.GetPayload()) != bytesToSHA256(chunk.GetChunkHash()) {
		return errV2AgentLink
	}
	offset := chunk.GetIndex() * uint64(transfer.chunkSize)
	if offset > math.MaxInt64 || transfer.file == nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	if link.closed || link.files[transfer.transferID] != transfer || transfer.committed ||
		(chunk.GetIndex() >= transfer.confirmedThrough && chunk.GetIndex()-transfer.confirmedThrough >= v2MaximumFileChunkReordering) {
		link.mu.Unlock()
		return errV2AgentLink
	}
	_, sparseDuplicate := transfer.confirmedSparse[chunk.GetIndex()]
	contiguousDuplicate := chunk.GetIndex() < transfer.confirmedThrough
	confirmedThrough := transfer.confirmedThrough
	link.mu.Unlock()
	if contiguousDuplicate || sparseDuplicate {
		existing := make([]byte, len(chunk.GetPayload()))
		if _, err := transfer.file.ReadAt(existing, int64(offset)); err != nil || !bytes.Equal(existing, chunk.GetPayload()) {
			zeroV2Bytes(existing)
			return errV2AgentLink
		}
		zeroV2Bytes(existing)
		link.touchManagedV2Upload(transfer, confirmedThrough)
		return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{
			TransferId: transfer.transferID, ConfirmedIndexes: []uint64{chunk.GetIndex()},
		})
	}
	if _, err := transfer.file.WriteAt(chunk.GetPayload(), int64(offset)); err != nil {
		return errV2AgentLink
	}
	if err := transfer.file.Sync(); err != nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	if link.closed || link.files[transfer.transferID] != transfer || transfer.committed {
		link.mu.Unlock()
		return errV2AgentLink
	}
	transfer.confirmedSparse[chunk.GetIndex()] = struct{}{}
	for {
		if _, confirmed := transfer.confirmedSparse[transfer.confirmedThrough]; !confirmed {
			break
		}
		delete(transfer.confirmedSparse, transfer.confirmedThrough)
		transfer.confirmedThrough++
	}
	confirmedThrough = transfer.confirmedThrough
	link.mu.Unlock()
	link.touchManagedV2Upload(transfer, confirmedThrough)
	return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{
		TransferId: transfer.transferID, ConfirmedIndexes: []uint64{chunk.GetIndex()},
	})
}

func bytesToSHA256(value []byte) [sha256.Size]byte {
	var result [sha256.Size]byte
	copy(result[:], value)
	return result
}

func (link *v2AgentLink) handleFileCommit(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	commit := new(remotev2.FileCommit)
	if link == nil || !unmarshalV2AgentMessage(plaintext, commit) || uuid.Validate(commit.GetTransferId()) != nil || len(commit.GetSha256()) != sha256.Size {
		return errV2AgentLink
	}
	if download := link.v2DownloadTransfer(commit.GetTransferId(), record.GetChannelId(), record.GetStreamId()); download != nil {
		return link.handleV2DownloadCommit(ctx, record, commit, download)
	}
	transfer := link.fileTransfer(commit.GetTransferId(), record.GetChannelId(), record.GetStreamId())
	if transfer == nil || string(commit.GetSha256()) != string(transfer.sha256[:]) || commit.GetExpectedRevision() != transfer.expectedRevision {
		return errV2AgentLink
	}
	link.mu.Lock()
	committed := transfer.committed
	link.mu.Unlock()
	if committed {
		return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{TransferId: transfer.transferID})
	}
	if transfer.file == nil {
		return errV2AgentLink
	}
	chunks := (transfer.totalLength + uint64(transfer.chunkSize) - 1) / uint64(transfer.chunkSize)
	link.mu.Lock()
	confirmedThrough := transfer.confirmedThrough
	link.mu.Unlock()
	if confirmedThrough != chunks || transfer.file.Sync() != nil {
		return errV2AgentLink
	}
	info, err := transfer.file.Stat()
	if err != nil || info.Size() != int64(transfer.totalLength) {
		return errV2AgentLink
	}
	if _, err := transfer.file.Seek(0, io.SeekStart); err != nil {
		return errV2AgentLink
	}
	digest := sha256.New()
	if _, err := io.CopyN(digest, transfer.file, int64(transfer.totalLength)); err != nil || string(digest.Sum(nil)) != string(transfer.sha256[:]) {
		return errV2AgentLink
	}
	if err := link.commitManagedV2Upload(ctx, transfer); err != nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	transfer.committed = true
	link.mu.Unlock()
	return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{TransferId: transfer.transferID})
}

// sendV2FileAck keeps a file Stream resumable if a Carrier disappears after
// persistence but before the acknowledgement can leave the Device. The Client
// re-sends the same manifest/chunk/commit after resume, so an Agent must retain
// the persisted state instead of treating the transport loss as a Stream error.
func (link *v2AgentLink) sendV2FileAck(ctx context.Context, channelID, streamID string, ack *remotev2.FileAck) error {
	if link == nil || ack == nil {
		return errV2AgentLink
	}
	err := link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_FILE_ACK, channelID, streamID, ack)
	if errors.Is(err, errV2AgentCarrier) {
		return nil
	}
	return err
}

func (link *v2AgentLink) touchManagedV2Upload(transfer *v2AgentFileTransfer, confirmedThrough uint64) {
	if link == nil || link.state == nil || transfer == nil || !transfer.managedUpload {
		return
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	if upload := manager.uploads[transfer.transferID]; upload != nil && upload.ProjectID.String() == transfer.projectID && upload.Temporary == transfer.stagingPath {
		confirmedOffset := min(transfer.totalLength, confirmedThrough*uint64(transfer.chunkSize))
		if confirmedOffset <= math.MaxInt64 && upload.Offset < int64(confirmedOffset) {
			upload.Offset = int64(confirmedOffset)
		}
		upload.ExpiresAt = time.Now().UTC().Add(fileTransferTTL)
	}
	manager.mu.Unlock()
}

func (link *v2AgentLink) commitManagedV2Upload(ctx context.Context, transfer *v2AgentFileTransfer) error {
	if link == nil || link.state == nil || transfer == nil || !transfer.managedUpload || transfer.file == nil {
		return errV2AgentLink
	}
	if err := transfer.file.Close(); err != nil {
		return errV2AgentLink
	}
	transfer.file = nil
	dispatch := dispatcher{state: link.state, now: func() time.Time { return time.Now().UTC() }, requestProjectID: transfer.projectID}
	project, err := dispatch.fileProject()
	if err != nil {
		return errV2AgentLink
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.cleanup(time.Now().UTC())
	upload := manager.uploads[transfer.transferID]
	if upload == nil || upload.ProjectID != project.ID || upload.Temporary != transfer.stagingPath || upload.Size != int64(transfer.totalLength) ||
		upload.SHA256 != base64.RawURLEncoding.EncodeToString(transfer.sha256[:]) || upload.ExpectedRevision != transfer.expectedRevision {
		return errV2AgentLink
	}
	upload.Offset = upload.Size
	upload.ExpiresAt = time.Now().UTC().Add(fileTransferTTL)
	_, _, err = dispatch.fileUploadComplete(ctx, manager, project, rpcInput{
		"transferId": transfer.transferID, "size": float64(transfer.totalLength), "sha256": base64.RawURLEncoding.EncodeToString(transfer.sha256[:]),
	})
	if err != nil {
		return errV2AgentLink
	}
	return nil
}

func (link *v2AgentLink) fileTransfer(transferID, channelID, streamID string) *v2AgentFileTransfer {
	if link == nil {
		return nil
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	transfer := link.files[transferID]
	if transfer == nil || transfer.channelID != channelID || transfer.streamID != streamID {
		return nil
	}
	return transfer
}

func (link *v2AgentLink) removeFileTransfer(transferID string) {
	if link == nil || transferID == "" {
		return
	}
	link.mu.Lock()
	transfer := link.files[transferID]
	delete(link.files, transferID)
	link.mu.Unlock()
	if transfer != nil {
		if transfer.file != nil {
			_ = transfer.file.Close()
		}
		if !transfer.managedUpload && transfer.stagingPath != "" {
			_ = os.Remove(transfer.stagingPath)
		}
	}
}

// handleV2DownloadManifest starts the Device-to-Client half of the dedicated
// bulk Stream. file.download.prepare already resolved the plaintext path and
// revision inside an encrypted RPC; this manifest carries only that opaque
// transfer handle and the signed-in client-visible integrity metadata.
func (link *v2AgentLink) handleV2DownloadManifest(ctx context.Context, record *remotev2.EncryptedRecord, manifest *remotev2.FileManifest, stream *v2AgentStream, channel *v2AgentChannel) error {
	if link == nil || link.state == nil || manifest == nil || stream == nil || channel == nil || manifest.GetRelativePathHandle() != manifest.GetTransferId() {
		return errV2AgentLink
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	manager.cleanup(time.Now().UTC())
	source := manager.downloads[manifest.GetTransferId()]
	_, fileScope := channel.scopes["remote.peer.file.receive"]
	_, taskScope := channel.scopes["remote.peer.task.control"]
	authorizedSource := source != nil && ((source.SourceKind == downloadSourceProjectFile && fileScope) ||
		(source.SourceKind == downloadSourceTaskLog && taskScope && source.PeerSessionID == link.id))
	validRevision := source != nil && (source.SourceKind == downloadSourceProjectFile &&
		(manifest.GetExpectedRevision() == 0 || manifest.GetExpectedRevision() == source.Revision) ||
		source.SourceKind == downloadSourceTaskLog && manifest.GetExpectedRevision() == source.Generation)
	if !authorizedSource || !validRevision || source.ProjectID.String() != channel.projectID || source.Size < 0 || uint64(source.Size) != manifest.GetTotalLength() ||
		source.SHA256 != base64.RawURLEncoding.EncodeToString(manifest.GetSha256()) {
		manager.mu.Unlock()
		return errV2AgentLink
	}
	source.ExpiresAt = time.Now().UTC().Add(fileTransferTTL)
	copySource := *source
	manager.mu.Unlock()

	transfer := &v2AgentDownloadTransfer{
		transferID: manifest.GetTransferId(), sourceKind: copySource.SourceKind, peerSessionID: copySource.PeerSessionID,
		channelID: record.GetChannelId(), streamID: record.GetStreamId(), projectID: channel.projectID,
		relativePath: copySource.RelativePath, path: copySource.Path, totalLength: uint64(copySource.Size), chunkSize: manifest.GetChunkSize(),
		revision: copySource.Revision, projectRevision: copySource.ProjectRevision, expectedRevision: manifest.GetExpectedRevision(),
		taskID: copySource.TaskID, runID: copySource.RunID, generation: copySource.Generation,
		sealed: copySource.Sealed,
	}
	copy(transfer.sha256[:], manifest.GetSha256())
	link.mu.Lock()
	if existing := link.downloads[transfer.transferID]; existing != nil {
		same := existing.sourceKind == transfer.sourceKind && existing.peerSessionID == transfer.peerSessionID &&
			existing.channelID == transfer.channelID && existing.streamID == transfer.streamID && existing.projectID == transfer.projectID &&
			existing.relativePath == transfer.relativePath && existing.path == transfer.path && existing.totalLength == transfer.totalLength &&
			existing.chunkSize == transfer.chunkSize && existing.revision == transfer.revision && existing.projectRevision == transfer.projectRevision &&
			existing.expectedRevision == transfer.expectedRevision && existing.taskID == transfer.taskID && existing.runID == transfer.runID &&
			existing.generation == transfer.generation && existing.sealed == transfer.sealed && string(existing.sha256[:]) == string(transfer.sha256[:])
		link.mu.Unlock()
		if !same {
			return errV2AgentLink
		}
		return link.sendNextV2DownloadChunk(ctx, existing)
	}
	if link.closed {
		link.mu.Unlock()
		return errV2AgentLink
	}
	link.downloads[transfer.transferID] = transfer
	stream.cleanup = func() { link.removeV2DownloadTransfer(transfer.transferID) }
	link.mu.Unlock()
	return link.sendNextV2DownloadChunk(ctx, transfer)
}

func (link *v2AgentLink) v2DownloadTransfer(transferID, channelID, streamID string) *v2AgentDownloadTransfer {
	if link == nil {
		return nil
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	transfer := link.downloads[transferID]
	if transfer == nil || transfer.channelID != channelID || transfer.streamID != streamID {
		return nil
	}
	return transfer
}

func (link *v2AgentLink) removeV2DownloadTransfer(transferID string) {
	if link == nil || transferID == "" {
		return
	}
	link.mu.Lock()
	delete(link.downloads, transferID)
	link.mu.Unlock()
}

// handleFileAck advances a constant-memory sequential cursor. Stale ACKs are
// idempotent; a future or never-sent index is invalid because it would let a
// client skip integrity verification for a chunk it has not actually received.
func (link *v2AgentLink) handleFileAck(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	ack := new(remotev2.FileAck)
	if link == nil || !unmarshalV2AgentMessage(plaintext, ack) || uuid.Validate(ack.GetTransferId()) != nil || len(ack.GetConfirmedBitmap()) != 0 || len(ack.GetConfirmedIndexes()) != 1 {
		return errV2AgentLink
	}
	transfer := link.v2DownloadTransfer(ack.GetTransferId(), record.GetChannelId(), record.GetStreamId())
	if transfer == nil {
		return errV2AgentLink
	}
	chunks := (transfer.totalLength + uint64(transfer.chunkSize) - 1) / uint64(transfer.chunkSize)
	index := ack.GetConfirmedIndexes()[0]
	if index >= chunks {
		return errV2AgentLink
	}
	link.mu.Lock()
	if link.closed || transfer.committed {
		link.mu.Unlock()
		return errV2AgentLink
	}
	if index < transfer.nextIndex {
		link.mu.Unlock()
		return nil
	}
	if index != transfer.nextIndex || !transfer.sent || transfer.sentIndex != index {
		link.mu.Unlock()
		return errV2AgentLink
	}
	transfer.nextIndex++
	transfer.sent = false
	link.mu.Unlock()
	return link.sendNextV2DownloadChunk(ctx, transfer)
}

func (link *v2AgentLink) sendNextV2DownloadChunk(ctx context.Context, transfer *v2AgentDownloadTransfer) error {
	if link == nil || transfer == nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	if link.closed || link.downloads[transfer.transferID] != transfer || transfer.committed {
		link.mu.Unlock()
		return errV2AgentLink
	}
	chunks := (transfer.totalLength + uint64(transfer.chunkSize) - 1) / uint64(transfer.chunkSize)
	index := transfer.nextIndex
	keyID := link.keys.ActiveKeyID()
	if index < chunks {
		// Mark before the send so an extremely fast ACK cannot race the sender's
		// bookkeeping and be mistaken for an acknowledgement of unsent data.
		transfer.sentIndex = index
		transfer.sent = true
	}
	link.mu.Unlock()
	if index == chunks {
		return nil
	}
	payload, err := link.readV2DownloadChunk(ctx, transfer, index)
	if err != nil {
		return errV2AgentLink
	}
	digest := sha256.Sum256(payload)
	err = link.sendEncrypted(ctx, keyID, remotev2.FrameType_FRAME_TYPE_FILE_CHUNK, transfer.channelID, transfer.streamID, &remotev2.FileChunk{
		TransferId: transfer.transferID, Index: index, ChunkHash: digest[:], Payload: payload,
	})
	zeroV2Bytes(payload)
	if errors.Is(err, errV2AgentCarrier) {
		return nil
	}
	if err != nil && !errors.Is(err, errV2AgentCarrier) {
		link.mu.Lock()
		if link.downloads[transfer.transferID] == transfer && transfer.sent && transfer.sentIndex == index {
			transfer.sent = false
		}
		link.mu.Unlock()
	}
	return err
}

func (link *v2AgentLink) readV2DownloadChunk(ctx context.Context, transfer *v2AgentDownloadTransfer, index uint64) ([]byte, error) {
	if link == nil || link.state == nil || transfer == nil || transfer.chunkSize == 0 {
		return nil, errV2AgentLink
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	manager.cleanup(time.Now().UTC())
	source := manager.downloads[transfer.transferID]
	if source == nil || source.SourceKind != transfer.sourceKind || source.PeerSessionID != transfer.peerSessionID ||
		source.ProjectID.String() != transfer.projectID || source.RelativePath != transfer.relativePath || source.Path != transfer.path ||
		source.Size < 0 || uint64(source.Size) != transfer.totalLength || source.Revision != transfer.revision || source.ProjectRevision != transfer.projectRevision ||
		source.TaskID != transfer.taskID || source.RunID != transfer.runID || source.Generation != transfer.generation ||
		source.SHA256 != base64.RawURLEncoding.EncodeToString(transfer.sha256[:]) {
		manager.mu.Unlock()
		return nil, errV2AgentLink
	}
	manager.mu.Unlock()
	if transfer.sourceKind == downloadSourceTaskLog {
		return link.readV2TaskLogDownloadChunk(ctx, transfer, index)
	}
	if transfer.sourceKind != downloadSourceProjectFile {
		return nil, errV2AgentLink
	}
	projectID, err := uuid.Parse(transfer.projectID)
	if err != nil || projectID == uuid.Nil {
		return nil, errV2AgentLink
	}
	project, err := link.state.business.projectByID(ctx, projectID)
	if err != nil || project.Revision != transfer.projectRevision {
		return nil, errV2AgentLink
	}
	resolved, normalized, err := secureExistingProjectPath(project, transfer.relativePath)
	if err != nil || normalized != transfer.relativePath || !sameFilesystemPath(resolved, transfer.path) {
		return nil, errV2AgentLink
	}
	info, err := os.Stat(resolved)
	if err != nil || workspaceFileRevision(transfer.relativePath, info) != transfer.revision {
		return nil, errV2AgentLink
	}
	offset := index * uint64(transfer.chunkSize)
	if offset > uint64(^uint64(0)>>1) || offset >= transfer.totalLength {
		return nil, errV2AgentLink
	}
	length := min(uint64(transfer.chunkSize), transfer.totalLength-offset)
	file, err := os.Open(resolved)
	if err != nil {
		return nil, errV2AgentLink
	}
	defer file.Close()
	payload := make([]byte, int(length))
	if _, err := file.ReadAt(payload, int64(offset)); err != nil && !errors.Is(err, io.EOF) {
		zeroV2Bytes(payload)
		return nil, errV2AgentLink
	}
	manager.mu.Lock()
	if current := manager.downloads[transfer.transferID]; current != nil && current.ProjectID.String() == transfer.projectID && current.Path == transfer.path {
		current.ExpiresAt = time.Now().UTC().Add(fileTransferTTL)
	}
	manager.mu.Unlock()
	return payload, nil
}

func (link *v2AgentLink) readV2TaskLogDownloadChunk(ctx context.Context, transfer *v2AgentDownloadTransfer, index uint64) ([]byte, error) {
	if link == nil || link.state == nil || link.state.tasksV2 == nil || transfer == nil ||
		transfer.peerSessionID != link.id || transfer.taskID == uuid.Nil || transfer.runID == uuid.Nil || transfer.generation == 0 {
		return nil, errV2AgentLink
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	manager.cleanup(time.Now().UTC())
	current := manager.downloads[transfer.transferID]
	if current == nil || current.SourceKind != downloadSourceTaskLog || current.PeerSessionID != link.id ||
		current.ProjectID.String() != transfer.projectID || current.TaskID != transfer.taskID || current.RunID != transfer.runID ||
		current.Generation != transfer.generation || current.Size < 0 || uint64(current.Size) != transfer.totalLength {
		manager.mu.Unlock()
		return nil, errV2AgentLink
	}
	current.ExpiresAt = time.Now().UTC().Add(fileTransferTTL)
	manager.mu.Unlock()
	task, err := link.state.tasksV2.Get(ctx, transfer.taskID)
	if err != nil || task.Definition.ProjectID.String() != transfer.projectID {
		return nil, errV2AgentLink
	}
	run, err := link.state.tasksV2.GetRun(ctx, transfer.taskID, transfer.runID)
	if err != nil || run.LogGeneration != transfer.generation ||
		(run.LogState != taskLogStateActive && run.LogState != taskLogStateSealed) {
		return nil, errV2AgentLink
	}
	offset := index * uint64(transfer.chunkSize)
	if offset > uint64(^uint64(0)>>1) || offset >= transfer.totalLength {
		return nil, errV2AgentLink
	}
	length := min(uint64(transfer.chunkSize), transfer.totalLength-offset)
	file, info, err := openPrivateTaskLogFile(link.state.tasksV2.logRoot, transfer.taskID, transfer.runID)
	if err != nil || info.Size() < 0 || uint64(info.Size()) < transfer.totalLength ||
		run.LogState == taskLogStateSealed && uint64(info.Size()) != transfer.totalLength {
		if file != nil {
			_ = file.Close()
		}
		return nil, errV2AgentLink
	}
	if run.LogState == taskLogStateActive {
		writer := link.state.tasksV2.activeRunLogWriter(run.ID)
		if writer == nil || !writer.matchesFile(info) {
			_ = file.Close()
			return nil, errV2AgentLink
		}
	}
	defer file.Close()
	payload := make([]byte, int(length))
	if _, err := file.ReadAt(payload, int64(offset)); err != nil && !errors.Is(err, io.EOF) {
		zeroV2Bytes(payload)
		return nil, errV2AgentLink
	}
	return payload, nil
}

func (link *v2AgentLink) handleV2DownloadCommit(ctx context.Context, record *remotev2.EncryptedRecord, commit *remotev2.FileCommit, transfer *v2AgentDownloadTransfer) error {
	if link == nil || transfer == nil || string(commit.GetSha256()) != string(transfer.sha256[:]) || commit.GetExpectedRevision() != transfer.expectedRevision {
		return errV2AgentLink
	}
	link.mu.Lock()
	committed := transfer.committed
	nextIndex := transfer.nextIndex
	chunks := (transfer.totalLength + uint64(transfer.chunkSize) - 1) / uint64(transfer.chunkSize)
	link.mu.Unlock()
	if committed {
		return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{TransferId: transfer.transferID})
	}
	if nextIndex != chunks {
		// A lost final FILE_ACK is common around a Carrier handoff. A commit can
		// only be produced after the Client verified the complete digest, so keep
		// the Stream open and resend the next outstanding chunk instead of
		// turning that transport race into a protocol failure.
		return link.sendNextV2DownloadChunk(ctx, transfer)
	}
	if link.verifyAndCompleteV2Download(ctx, transfer) != nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	transfer.committed = true
	link.mu.Unlock()
	return link.sendV2FileAck(ctx, record.GetChannelId(), record.GetStreamId(), &remotev2.FileAck{TransferId: transfer.transferID})
}

func (link *v2AgentLink) verifyAndCompleteV2Download(ctx context.Context, transfer *v2AgentDownloadTransfer) error {
	if link == nil || link.state == nil || transfer == nil {
		return errV2AgentLink
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	manager.cleanup(time.Now().UTC())
	source := manager.downloads[transfer.transferID]
	if source == nil || source.SourceKind != transfer.sourceKind || source.PeerSessionID != transfer.peerSessionID ||
		source.ProjectID.String() != transfer.projectID || source.RelativePath != transfer.relativePath || source.Path != transfer.path ||
		source.Size < 0 || uint64(source.Size) != transfer.totalLength || source.Revision != transfer.revision || source.ProjectRevision != transfer.projectRevision ||
		source.TaskID != transfer.taskID || source.RunID != transfer.runID || source.Generation != transfer.generation || source.Sealed != transfer.sealed ||
		source.SHA256 != base64.RawURLEncoding.EncodeToString(transfer.sha256[:]) {
		manager.mu.Unlock()
		return errV2AgentLink
	}
	manager.mu.Unlock()
	if transfer.sourceKind == downloadSourceTaskLog {
		if err := link.verifyV2TaskLogDownload(ctx, transfer); err != nil {
			return err
		}
		return link.completeManagedV2Download(transfer)
	}
	if transfer.sourceKind != downloadSourceProjectFile {
		return errV2AgentLink
	}
	projectID, err := uuid.Parse(transfer.projectID)
	if err != nil || projectID == uuid.Nil {
		return errV2AgentLink
	}
	project, err := link.state.business.projectByID(ctx, projectID)
	if err != nil || project.Revision != transfer.projectRevision {
		return errV2AgentLink
	}
	resolved, normalized, err := secureExistingProjectPath(project, transfer.relativePath)
	if err != nil || normalized != transfer.relativePath || !sameFilesystemPath(resolved, transfer.path) {
		return errV2AgentLink
	}
	digest, size, err := hashWorkspaceFile(ctx, resolved)
	if err != nil || size < 0 || uint64(size) != transfer.totalLength || digest != base64.RawURLEncoding.EncodeToString(transfer.sha256[:]) {
		return errV2AgentLink
	}
	info, err := os.Stat(resolved)
	if err != nil || workspaceFileRevision(transfer.relativePath, info) != transfer.revision {
		return errV2AgentLink
	}
	return link.completeManagedV2Download(transfer)
}

func (link *v2AgentLink) verifyV2TaskLogDownload(ctx context.Context, transfer *v2AgentDownloadTransfer) error {
	if link == nil || link.state == nil || link.state.tasksV2 == nil || transfer == nil || transfer.peerSessionID != link.id {
		return errV2AgentLink
	}
	task, err := link.state.tasksV2.Get(ctx, transfer.taskID)
	if err != nil || task.Definition.ProjectID.String() != transfer.projectID {
		return errV2AgentLink
	}
	run, err := link.state.tasksV2.GetRun(ctx, transfer.taskID, transfer.runID)
	if err != nil || run.LogGeneration != transfer.generation ||
		(run.LogState != taskLogStateActive && run.LogState != taskLogStateSealed) {
		return errV2AgentLink
	}
	file, info, err := openPrivateTaskLogFile(link.state.tasksV2.logRoot, transfer.taskID, transfer.runID)
	if err != nil || info.Size() < 0 || uint64(info.Size()) < transfer.totalLength || transfer.sealed && uint64(info.Size()) != transfer.totalLength {
		if file != nil {
			_ = file.Close()
		}
		return errV2AgentLink
	}
	if run.LogState == taskLogStateActive {
		writer := link.state.tasksV2.activeRunLogWriter(run.ID)
		if writer == nil || !writer.matchesFile(info) {
			_ = file.Close()
			return errV2AgentLink
		}
	}
	defer file.Close()
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, int64(transfer.totalLength)))
	if err != nil || written != int64(transfer.totalLength) || string(digest.Sum(nil)) != string(transfer.sha256[:]) {
		return errV2AgentLink
	}
	return nil
}

func (link *v2AgentLink) completeManagedV2Download(transfer *v2AgentDownloadTransfer) error {
	if link == nil || link.state == nil || transfer == nil {
		return errV2AgentLink
	}
	manager := fileRPCManagerFor(link.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.cleanup(time.Now().UTC())
	current := manager.downloads[transfer.transferID]
	if current == nil || current.SourceKind != transfer.sourceKind || current.PeerSessionID != transfer.peerSessionID ||
		current.ProjectID.String() != transfer.projectID || current.Path != transfer.path {
		return errV2AgentLink
	}
	if current.releaseLease != nil {
		current.releaseLease()
		current.releaseLease = nil
	}
	delete(manager.downloads, transfer.transferID)
	return nil
}

func (link *v2AgentLink) handleEventSubscribe(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	request := new(remotev2.EventSubscribe)
	if link == nil || !unmarshalV2AgentMessage(plaintext, request) || uuid.Validate(request.GetSubscriptionId()) != nil {
		return errV2AgentLink
	}
	stream, channel := link.streamAndChannel(record.GetChannelId(), record.GetStreamId())
	if stream == nil || channel == nil || stream.kind != remotev2.StreamKind_STREAM_KIND_EVENT || channel.kind != remotev2.ChannelKind_CHANNEL_KIND_PROJECT || link.state == nil || link.state.business == nil || link.state.eventHub == nil {
		return errV2AgentLink
	}
	if _, allowed := channel.scopes["remote.peer.events"]; !allowed {
		return errV2AgentLink
	}
	projectID, err := uuid.Parse(channel.projectID)
	if err != nil || projectID == uuid.Nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	if existing := link.events[request.GetSubscriptionId()]; existing != nil {
		same := existing.channelID == record.GetChannelId() && existing.streamID == record.GetStreamId()
		link.mu.Unlock()
		if !same {
			return errV2AgentLink
		}
		return link.replayEventSubscription(ctx, existing, request.GetAfterSequence())
	}
	link.mu.Unlock()

	subscriber, err := link.state.eventHub.subscribe(projectID)
	if err != nil {
		return errV2AgentLink
	}
	info, err := link.state.business.agentEventStreamInfo(ctx, projectID)
	if err != nil {
		subscriber.close()
		return errV2AgentLink
	}
	subscriber.beginLiveAt(info.HighWatermark)
	info, err = link.state.business.agentEventStreamInfo(ctx, projectID)
	if err != nil {
		subscriber.close()
		return errV2AgentLink
	}
	streamContext := stream.context
	if streamContext == nil {
		streamContext = link.context
	}
	subscriptionContext, cancel := context.WithCancel(streamContext)
	subscription := &v2AgentEventSubscription{
		subscriptionID: request.GetSubscriptionId(), channelID: record.GetChannelId(), streamID: record.GetStreamId(), projectID: projectID,
		highWatermark: info.HighWatermark, subscriber: subscriber, cancel: cancel,
	}
	link.mu.Lock()
	if link.closed || link.events[subscription.subscriptionID] != nil {
		link.mu.Unlock()
		cancel()
		subscriber.close()
		return errV2AgentLink
	}
	link.events[subscription.subscriptionID] = subscription
	stream.cleanup = func() { link.removeEventSubscription(subscription.subscriptionID) }
	link.mu.Unlock()
	afterSequence := request.GetAfterSequence()
	resetRequired, resetReason := false, ""
	// EventSubscribe has a scalar cursor for generated-code portability. A zero
	// cursor deliberately requests the authoritative bootstrap snapshot rather
	// than assuming an old browser has a valid event journal.
	if afterSequence == 0 {
		resetRequired, resetReason = true, "bootstrap"
	} else if afterSequence > info.HighWatermark {
		resetRequired, resetReason = true, "sequenceGap"
	} else if afterSequence+1 < info.MinimumAvailableSequence {
		resetRequired, resetReason = true, "retention"
	}
	if err := link.sendEventControl(ctx, subscription, "subscription.ready", info.HighWatermark, map[string]any{
		"schemaVersion": 1, "type": "subscription.ready", "projectId": projectID.String(),
		"minimumAvailableSequence": info.MinimumAvailableSequence, "highWatermark": info.HighWatermark,
		"resetRequired": resetRequired, "resetReason": resetReason,
		"heartbeatSeconds":     uint64(v2EventHeartbeatSeconds),
		"supportedTopics":      []string{"agent", "capabilities", "conversation", "message", "task", "taskLog", "workflow"},
		"eventContractVersion": 1, "acceptedEventKinds": []string{"agent.state.changed", "event.subscription.control"},
	}); err != nil {
		link.removeEventSubscription(subscription.subscriptionID)
		return err
	}
	if resetRequired {
		_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_EVENT_RESET_REQUIRED, subscription.channelID, subscription.streamID, &remotev2.EventResetRequired{
			SubscriptionId: subscription.subscriptionID, CurrentHighWatermark: info.HighWatermark,
		})
		link.removeEventSubscription(subscription.subscriptionID)
		return nil
	}
	if err := link.replayEventSubscription(ctx, subscription, afterSequence); err != nil {
		link.removeEventSubscription(subscription.subscriptionID)
		return err
	}
	go link.streamLiveEvents(subscriptionContext, subscription)
	go link.streamEventHeartbeats(subscriptionContext, subscription)
	return nil
}

func (link *v2AgentLink) handleEventAck(record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	ack := new(remotev2.EventAck)
	if link == nil || !unmarshalV2AgentMessage(plaintext, ack) || uuid.Validate(ack.GetSubscriptionId()) != nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	subscription := link.events[ack.GetSubscriptionId()]
	if subscription != nil && subscription.channelID == record.GetChannelId() && subscription.streamID == record.GetStreamId() {
		if ack.GetHighWatermark() > subscription.highWatermark {
			link.mu.Unlock()
			return errV2AgentLink
		}
		if ack.GetHighWatermark() <= subscription.acknowledged {
			link.mu.Unlock()
			return nil
		}
		// Acknowledgements are monotonic and are bounded by output that this
		// subscription actually emitted.  They are intentionally advisory: the
		// durable event journal remains the recovery source after a reconnect.
		subscription.acknowledged = ack.GetHighWatermark()
		link.mu.Unlock()
		return nil
	}
	link.mu.Unlock()
	return errV2AgentLink
}

// applyCarrierResume imports only advisory Stream acknowledgements that are
// already bound to a live Event Stream.  Carrier-visible metadata never
// identifies a project or exposes event content; the encrypted EVENT_RESUME
// that follows still decides exactly which durable records are replayed.
func (link *v2AgentLink) applyCarrierResume(acks []*remotev2.StreamAck) {
	if link == nil || len(acks) == 0 {
		return
	}
	// Serialize the transition with each live sender. Once this method returns,
	// no acknowledged Event Stream can emit a newer record until its encrypted
	// EVENT_RESUME has replayed the client's missing durable range.
	link.eventResumeMu.Lock()
	defer link.eventResumeMu.Unlock()
	matches := make(map[*v2AgentEventSubscription]uint64)
	link.mu.Lock()
	for _, ack := range acks {
		if ack == nil || ack.GetChannelId() == "" || ack.GetStreamId() == "" {
			continue
		}
		for _, subscription := range link.events {
			if subscription == nil || subscription.channelID != ack.GetChannelId() || subscription.streamID != ack.GetStreamId() ||
				ack.GetAcknowledgedSequence() < subscription.acknowledged || ack.GetAcknowledgedSequence() > subscription.highWatermark {
				continue
			}
			subscription.acknowledged = ack.GetAcknowledgedSequence()
			matches[subscription] = ack.GetAcknowledgedSequence()
			break
		}
	}
	link.mu.Unlock()
	for subscription := range matches {
		subscription.sendMu.Lock()
		if !subscription.resumePending {
			subscription.resumePending = true
			subscription.resumeReady = make(chan struct{})
		}
		subscription.sendMu.Unlock()
	}
}

// lockLiveEventSend returns with subscription.sendMu held. It prevents a live
// event or heartbeat from overtaking the durable replay requested after a
// Carrier handoff.
func (link *v2AgentLink) lockLiveEventSend(ctx context.Context, subscription *v2AgentEventSubscription) bool {
	if link == nil || subscription == nil {
		return false
	}
	for {
		link.eventResumeMu.Lock()
		subscription.sendMu.Lock()
		link.eventResumeMu.Unlock()
		if !subscription.resumePending {
			return true
		}
		ready := subscription.resumeReady
		subscription.sendMu.Unlock()
		if ready == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return false
		case <-ready:
		}
	}
}

func finishEventResumeLocked(subscription *v2AgentEventSubscription) {
	if subscription == nil || !subscription.resumePending {
		return
	}
	ready := subscription.resumeReady
	subscription.resumePending = false
	subscription.resumeReady = nil
	if ready != nil {
		close(ready)
	}
}

func (link *v2AgentLink) handleEventResume(ctx context.Context, record *remotev2.EncryptedRecord, plaintext []byte) error {
	defer zeroV2Bytes(plaintext)
	resume := new(remotev2.EventResume)
	if link == nil || !unmarshalV2AgentMessage(plaintext, resume) || uuid.Validate(resume.GetSubscriptionId()) != nil {
		return errV2AgentLink
	}
	link.mu.Lock()
	subscription := link.events[resume.GetSubscriptionId()]
	valid := subscription != nil && subscription.channelID == record.GetChannelId() && subscription.streamID == record.GetStreamId()
	link.mu.Unlock()
	if !valid {
		return errV2AgentLink
	}
	return link.replayEventSubscription(ctx, subscription, resume.GetAfterSequence())
}

func (link *v2AgentLink) replayEventSubscription(ctx context.Context, subscription *v2AgentEventSubscription, afterSequence uint64) error {
	if link == nil || subscription == nil || link.state == nil || link.state.business == nil {
		return errV2AgentLink
	}
	subscription.sendMu.Lock()
	succeeded := false
	defer func() {
		if succeeded {
			finishEventResumeLocked(subscription)
		}
		subscription.sendMu.Unlock()
	}()
	info, err := link.state.business.agentEventStreamInfo(ctx, subscription.projectID)
	if err != nil {
		return errV2AgentLink
	}
	if afterSequence > info.HighWatermark || afterSequence+1 < info.MinimumAvailableSequence {
		if err := link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_EVENT_RESET_REQUIRED, subscription.channelID, subscription.streamID, &remotev2.EventResetRequired{
			SubscriptionId: subscription.subscriptionID, CurrentHighWatermark: info.HighWatermark,
		}); err != nil {
			return err
		}
		succeeded = true
		link.removeEventSubscription(subscription.subscriptionID)
		return nil
	}
	if afterSequence == info.HighWatermark {
		succeeded = true
		return nil
	}
	events, err := link.state.business.listAgentEvents(ctx, subscription.projectID, afterSequence, info.HighWatermark)
	if err != nil || !agentEventReplayIsContiguous(events, afterSequence, info.HighWatermark) {
		if err := link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_EVENT_RESET_REQUIRED, subscription.channelID, subscription.streamID, &remotev2.EventResetRequired{
			SubscriptionId: subscription.subscriptionID, CurrentHighWatermark: info.HighWatermark,
		}); err != nil {
			return err
		}
		succeeded = true
		link.removeEventSubscription(subscription.subscriptionID)
		return nil
	}
	for _, event := range events {
		if err := link.sendEventRecordLocked(ctx, subscription, event); err != nil {
			return err
		}
	}
	succeeded = true
	return nil
}

func (link *v2AgentLink) streamLiveEvents(ctx context.Context, subscription *v2AgentEventSubscription) {
	if link == nil || subscription == nil || subscription.subscriber == nil {
		return
	}
	defer link.removeEventSubscription(subscription.subscriptionID)
	var pending *agentEventRecord
	for {
		if subscription.subscriber.resetReasonValue() != "" {
			if !link.lockLiveEventSend(ctx, subscription) {
				return
			}
			_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_EVENT_RESET_REQUIRED, subscription.channelID, subscription.streamID, &remotev2.EventResetRequired{
				SubscriptionId: subscription.subscriptionID, CurrentHighWatermark: link.state.eventHub.publishedSequence(subscription.projectID),
			})
			subscription.sendMu.Unlock()
			return
		}
		if pending == nil {
			event, ok := subscription.subscriber.next(ctx)
			if !ok {
				return
			}
			pending = &event
		}
		if !link.lockLiveEventSend(ctx, subscription) {
			return
		}
		link.mu.Lock()
		active := link.events[subscription.subscriptionID]
		highWatermark := subscription.highWatermark
		link.mu.Unlock()
		if active != subscription {
			subscription.sendMu.Unlock()
			return
		}
		if pending.Sequence <= highWatermark {
			// The snapshot/replay boundary can overlap entries already queued by
			// the live Hub. Suppress those duplicates before resuming delivery.
			subscription.sendMu.Unlock()
			pending = nil
			continue
		}
		if pending.Sequence != highWatermark+1 {
			_ = link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_EVENT_RESET_REQUIRED, subscription.channelID, subscription.streamID, &remotev2.EventResetRequired{
				SubscriptionId: subscription.subscriptionID, CurrentHighWatermark: link.state.eventHub.publishedSequence(subscription.projectID),
			})
			subscription.sendMu.Unlock()
			return
		}
		err := link.sendEventRecordLocked(ctx, subscription, *pending)
		subscription.sendMu.Unlock()
		if err != nil {
			if errors.Is(err, errV2AgentCarrier) {
				// Carrier suspension is transient. Do not drop a durable event;
				// retry after a small bounded delay and preserve its sequence.
				select {
				case <-ctx.Done():
					return
				case <-time.After(250 * time.Millisecond):
				}
				continue
			}
			return
		}
		pending = nil
	}
}

// streamEventHeartbeats keep long-lived Event Streams observable even when a
// project is quiet.  They are encrypted control records on the Event Stream,
// never Carrier-visible business metadata, and the client continues to ACK
// its own durable high-watermark independently.
func (link *v2AgentLink) streamEventHeartbeats(ctx context.Context, subscription *v2AgentEventSubscription) {
	if link == nil || subscription == nil {
		return
	}
	ticker := time.NewTicker(v2EventHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !link.lockLiveEventSend(ctx, subscription) {
				return
			}
			link.mu.Lock()
			active := link.events[subscription.subscriptionID]
			highWatermark := subscription.highWatermark
			link.mu.Unlock()
			if active != subscription {
				subscription.sendMu.Unlock()
				return
			}
			err := link.sendEventControl(ctx, subscription, "subscription.heartbeat", highWatermark, map[string]any{
				"schemaVersion": 1, "type": "subscription.heartbeat", "projectId": subscription.projectID.String(), "highWatermark": highWatermark,
			})
			subscription.sendMu.Unlock()
			if err != nil && !errors.Is(err, errV2AgentCarrier) {
				return
			}
		}
	}
}

// sendEventRecordLocked requires subscription.sendMu. Replay and live senders
// share this critical section so a post-resume event cannot overtake history.
func (link *v2AgentLink) sendEventRecordLocked(ctx context.Context, subscription *v2AgentEventSubscription, event agentEventRecord) error {
	if link == nil || subscription == nil || event.Sequence == 0 || uuid.Validate(event.EventID) != nil || len(event.SafePayloadJSON) == 0 {
		return errV2AgentLink
	}
	if err := link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_EVENT, subscription.channelID, subscription.streamID, &remotev2.RpcEvent{
		OperationId: subscription.subscriptionID, EventSequence: event.Sequence, Payload: append([]byte(nil), event.SafePayloadJSON...),
		EventId: event.EventID, HighWatermark: event.Sequence,
	}); err != nil {
		return err
	}
	link.mu.Lock()
	if active := link.events[subscription.subscriptionID]; active == subscription && event.Sequence > subscription.highWatermark {
		subscription.highWatermark = event.Sequence
	}
	link.mu.Unlock()
	return nil
}

func (link *v2AgentLink) sendEventControl(ctx context.Context, subscription *v2AgentEventSubscription, eventType string, highWatermark uint64, payload map[string]any) error {
	if link == nil || subscription == nil || eventType == "" || payload == nil {
		return errV2AgentLink
	}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumRPCPayload {
		return errV2AgentLink
	}
	defer zeroV2Bytes(encoded)
	return link.sendEncrypted(ctx, link.keys.ActiveKeyID(), remotev2.FrameType_FRAME_TYPE_RPC_EVENT, subscription.channelID, subscription.streamID, &remotev2.RpcEvent{
		OperationId: subscription.subscriptionID, EventSequence: 0, Payload: encoded, EventId: uuid.NewString(), HighWatermark: highWatermark,
	})
}

func (link *v2AgentLink) removeEventSubscription(subscriptionID string) {
	if link == nil || subscriptionID == "" {
		return
	}
	link.mu.Lock()
	subscription := link.events[subscriptionID]
	delete(link.events, subscriptionID)
	link.mu.Unlock()
	if subscription != nil {
		if subscription.cancel != nil {
			subscription.cancel()
		}
		if subscription.subscriber != nil {
			subscription.subscriber.close()
		}
	}
}

func (link *v2AgentLink) streamAndChannel(channelID, streamID string) (*v2AgentStream, *v2AgentChannel) {
	if link == nil {
		return nil, nil
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	channel := link.channels[channelID]
	if channel == nil {
		return nil, nil
	}
	return channel.streams[streamID], channel
}
