package relayserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
)

var (
	ErrNodeDraining       = errors.New("Relay node is draining")
	ErrConnectionCapacity = errors.New("Relay connection capacity is exhausted")
	ErrSessionNotFound    = errors.New("Relay device session was not found")
)

type frameConnection interface {
	Write(context.Context, websocket.MessageType, []byte) error
	Close(websocket.StatusCode, string) error
}

type Session struct {
	endpointID   string
	deviceID     string
	connectionID string
	epoch        uint64
	route        *relayrouter.Route
	connection   frameConnection
	queue        *BoundedQueue
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	once         sync.Once
	metrics      *ConnectionManager
}

func newSession(endpointID, deviceID, connectionID string, epoch uint64, connection frameConnection, metrics *ConnectionManager) (*Session, error) {
	if endpointID == "" || deviceID == "" || connectionID == "" || epoch == 0 || connection == nil {
		return nil, ErrSessionNotFound
	}
	queue, err := NewBoundedQueue(DefaultQueueBytes, DefaultQueueFrames)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{
		endpointID: endpointID, deviceID: deviceID, connectionID: connectionID, epoch: epoch, connection: connection,
		queue: queue, ctx: ctx, cancel: cancel, done: make(chan struct{}), metrics: metrics,
	}, nil
}

func (session *Session) start() {
	go func() {
		defer close(session.done)
		for {
			payload, err := session.queue.Dequeue(session.ctx)
			if err != nil {
				return
			}
			writeContext, cancel := context.WithTimeout(session.ctx, 10*time.Second)
			started := time.Now()
			err = session.connection.Write(writeContext, websocket.MessageBinary, payload)
			cancel()
			if session.metrics != nil {
				session.metrics.RecordEgress(len(payload), time.Since(started))
			}
			if err != nil {
				session.cancel()
				return
			}
		}
	}()
}

func (session *Session) Enqueue(envelope *remotev1.Envelope) error {
	if envelope == nil || envelope.GetConnectionEpoch() != session.epoch {
		return ErrSessionNotFound
	}
	return session.queue.Enqueue(envelope)
}

func (session *Session) Context() context.Context { return session.ctx }

func (session *Session) stop(status websocket.StatusCode, reason string) {
	session.once.Do(func() {
		session.cancel()
		session.queue.Close()
		_ = session.connection.Close(status, reason)
	})
}

type ConnectionMetrics struct {
	Active             int64
	Peak               int64
	ActiveHandshakes   int64
	HandshakeSucceeded int64
	HandshakeFailed    int64
	QueueRejected      int64
	QueueFrames        int64
	QueueBytes         int64
	HandshakeRejected  int64
	SupersededSessions int64
	DrainStarted       int64
	RouteRejected      int64
	RateLimited        int64
	IngressFrames      int64
	IngressBytes       int64
	EgressFrames       int64
	EgressBytes        int64
	WriteLagMicros     int64
}

type ConnectionManager struct {
	mu              sync.Mutex
	sessions        map[string]*Session
	maxConnections  int
	handshakes      chan struct{}
	draining        atomic.Bool
	revoked         atomic.Bool
	drainGeneration atomic.Uint64
	active          atomic.Int64
	peak            atomic.Int64
	queueRejected   atomic.Int64
	handshakeReject atomic.Int64
	superseded      atomic.Int64
	drains          atomic.Int64
	handshakeOK     atomic.Int64
	handshakeFailed atomic.Int64
	routeRejected   atomic.Int64
	rateLimited     atomic.Int64
	ingressFrames   atomic.Int64
	ingressBytes    atomic.Int64
	egressFrames    atomic.Int64
	egressBytes     atomic.Int64
	writeLagMicros  atomic.Int64
	routeChanges    chan struct{}
}

func NewConnectionManager(maxConnections, maxConcurrentHandshakes int) (*ConnectionManager, error) {
	if maxConnections < 1 || maxConnections > 10_000_000 || maxConcurrentHandshakes < 1 || maxConcurrentHandshakes > 100_000 {
		return nil, errors.New("Relay connection limits are invalid")
	}
	return &ConnectionManager{
		sessions: make(map[string]*Session), maxConnections: maxConnections,
		handshakes: make(chan struct{}, maxConcurrentHandshakes), routeChanges: make(chan struct{}, 1),
	}, nil
}

func (manager *ConnectionManager) AcquireHandshake(ctx context.Context) error {
	if manager == nil || manager.draining.Load() || manager.revoked.Load() {
		return ErrNodeDraining
	}
	select {
	case manager.handshakes <- struct{}{}:
		if manager.draining.Load() || manager.revoked.Load() {
			<-manager.handshakes
			return ErrNodeDraining
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		manager.handshakeReject.Add(1)
		return ErrConnectionCapacity
	}
}

func (manager *ConnectionManager) ReleaseHandshake() {
	if manager == nil {
		return
	}
	select {
	case <-manager.handshakes:
	default:
	}
}

func (manager *ConnectionManager) Attach(deviceID, connectionID string, epoch uint64, connection frameConnection) (*Session, error) {
	return manager.AttachEndpoint(deviceID, deviceID, connectionID, epoch, connection)
}

// AttachEndpoint attaches a connection under a Relay-local endpoint ID. A
// resident device uses its device ID as the endpoint ID; a direct controller
// uses a ticket-scoped endpoint ID so it does not replace the device's resident
// connection on this or another Relay.
func (manager *ConnectionManager) AttachEndpoint(endpointID, deviceID, connectionID string, epoch uint64, connection frameConnection) (*Session, error) {
	if manager == nil || manager.draining.Load() || manager.revoked.Load() {
		return nil, ErrNodeDraining
	}
	session, err := newSession(endpointID, deviceID, connectionID, epoch, connection, manager)
	if err != nil {
		return nil, err
	}
	manager.mu.Lock()
	if manager.draining.Load() || manager.revoked.Load() {
		manager.mu.Unlock()
		return nil, ErrNodeDraining
	}
	current := manager.sessions[endpointID]
	if current != nil && epoch <= current.epoch {
		manager.mu.Unlock()
		return nil, ErrSessionNotFound
	}
	if current == nil && len(manager.sessions) >= manager.maxConnections {
		manager.mu.Unlock()
		return nil, ErrConnectionCapacity
	}
	manager.sessions[endpointID] = session
	if current == nil {
		active := manager.active.Add(1)
		for {
			peak := manager.peak.Load()
			if active <= peak || manager.peak.CompareAndSwap(peak, active) {
				break
			}
		}
	} else {
		manager.superseded.Add(1)
	}
	manager.mu.Unlock()
	session.start()
	if current != nil {
		_ = current.Enqueue(goAwayEnvelope(current.epoch, remotev1.GoAwayReason_GO_AWAY_REASON_GRANT_REVOKED, 0, true))
		go func() {
			timer := time.NewTimer(250 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-current.done:
			case <-timer.C:
				current.stop(websocket.StatusPolicyViolation, "superseded connection")
			}
		}()
	}
	return session, nil
}

func (manager *ConnectionManager) Detach(deviceID, connectionID string, epoch uint64) {
	manager.DetachEndpoint(deviceID, connectionID, epoch)
}

func (manager *ConnectionManager) DetachEndpoint(endpointID, connectionID string, epoch uint64) {
	if manager == nil {
		return
	}
	manager.mu.Lock()
	session := manager.sessions[endpointID]
	routeChanged := false
	if session != nil && session.connectionID == connectionID && session.epoch == epoch {
		delete(manager.sessions, endpointID)
		manager.active.Add(-1)
		routeChanged = session.route != nil
	} else {
		session = nil
	}
	manager.mu.Unlock()
	if session != nil {
		session.cancel()
		session.queue.Close()
	}
	if routeChanged {
		manager.signalRouteChange()
	}
}

// BindResidentRoute attaches the Host-authorized route metadata to an already
// authenticated resident socket. Runtime heartbeats publish only these bound
// routes; direct controller sockets are intentionally excluded.
func (manager *ConnectionManager) BindResidentRoute(endpointID, connectionID string, epoch uint64, route relayrouter.Route) error {
	if manager == nil || endpointID == "" || connectionID == "" || epoch == 0 || route.DeviceID == "" || route.UserID == "" ||
		route.CellID == "" || route.NodeID == "" || route.ConnectionID != connectionID || route.ConnectionEpoch != epoch ||
		route.AssignmentVersion == 0 || route.GrantVersion == 0 || route.ProtocolVersion == 0 {
		return ErrSessionNotFound
	}
	manager.mu.Lock()
	session := manager.sessions[endpointID]
	if session == nil || session.endpointID != session.deviceID || session.deviceID != route.DeviceID ||
		session.connectionID != connectionID || session.epoch != epoch {
		manager.mu.Unlock()
		return ErrSessionNotFound
	}
	copy := route
	copy.ConnectedAt = time.Now().UTC()
	session.route = &copy
	manager.mu.Unlock()
	manager.signalRouteChange()
	return nil
}

// ResidentRoutes returns a point-in-time copy for the next authenticated
// Relay-to-Host heartbeat.
func (manager *ConnectionManager) ResidentRoutes() []relayrouter.Route {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	routes := make([]relayrouter.Route, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		if session.route != nil && session.endpointID == session.deviceID {
			routes = append(routes, *session.route)
		}
	}
	manager.mu.Unlock()
	return routes
}

// RouteChanges wakes the runtime so connect/disconnect state does not wait for
// the normal heartbeat interval before reaching Host.
func (manager *ConnectionManager) RouteChanges() <-chan struct{} {
	if manager == nil {
		return nil
	}
	return manager.routeChanges
}

func (manager *ConnectionManager) signalRouteChange() {
	if manager == nil {
		return
	}
	select {
	case manager.routeChanges <- struct{}{}:
	default:
	}
}

// RejectResident applies a Host heartbeat decision to one exact connection.
// Epoch and connection ID prevent a delayed response from closing a newer
// socket that has already replaced it.
func (manager *ConnectionManager) RejectResident(deviceID, connectionID string, epoch uint64, reason remotev1.GoAwayReason) bool {
	if manager == nil || deviceID == "" || connectionID == "" || epoch == 0 {
		return false
	}
	manager.mu.Lock()
	session := manager.sessions[deviceID]
	if session == nil || session.route == nil || session.connectionID != connectionID || session.epoch != epoch {
		manager.mu.Unlock()
		return false
	}
	delete(manager.sessions, deviceID)
	manager.active.Add(-1)
	manager.mu.Unlock()
	manager.signalRouteChange()
	_ = session.Enqueue(goAwayEnvelope(epoch, reason, 0, true))
	go func() {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-session.done:
		case <-timer.C:
			session.stop(websocket.StatusPolicyViolation, "Host rejected resident route")
		}
	}()
	return true
}

func (manager *ConnectionManager) Enqueue(deviceID string, epoch uint64, envelope *remotev1.Envelope) error {
	return manager.EnqueueEndpoint(deviceID, epoch, envelope)
}

func (manager *ConnectionManager) EnqueueEndpoint(endpointID string, epoch uint64, envelope *remotev1.Envelope) error {
	if manager == nil {
		return ErrSessionNotFound
	}
	manager.mu.Lock()
	session := manager.sessions[endpointID]
	manager.mu.Unlock()
	if session == nil || session.epoch != epoch {
		return ErrSessionNotFound
	}
	if err := session.Enqueue(envelope); err != nil {
		if errors.Is(err, ErrQueueFull) {
			manager.queueRejected.Add(1)
		}
		return err
	}
	return nil
}

// HasResident reports whether the exact target connection selected by the
// management service is still attached to this Relay process.
func (manager *ConnectionManager) HasResident(deviceID string, epoch uint64) bool {
	if manager == nil || deviceID == "" || epoch == 0 {
		return false
	}
	manager.mu.Lock()
	session := manager.sessions[deviceID]
	manager.mu.Unlock()
	return session != nil && session.deviceID == deviceID && session.epoch == epoch
}

func (manager *ConnectionManager) BeginDrain(reason remotev1.GoAwayReason, reconnectAfter time.Duration, refreshAssignment bool, deadline time.Duration) {
	if manager == nil || manager.revoked.Load() || !manager.draining.CompareAndSwap(false, true) {
		return
	}
	manager.drains.Add(1)
	generation := manager.drainGeneration.Add(1)
	if deadline < time.Second {
		deadline = time.Second
	}
	if deadline > 30*time.Minute {
		deadline = 30 * time.Minute
	}
	manager.NotifyAll(reason, reconnectAfter, refreshAssignment)
	go func() {
		timer := time.NewTimer(deadline)
		defer timer.Stop()
		<-timer.C
		if manager.draining.Load() && manager.drainGeneration.Load() == generation {
			manager.CloseAll(websocket.StatusGoingAway, "relay draining")
		}
	}()
}

// Revoke permanently closes admission for this process. Unlike a normal drain,
// a delayed resume event or heartbeat cannot reopen a revoked node identity.
func (manager *ConnectionManager) Revoke() {
	if manager == nil || !manager.revoked.CompareAndSwap(false, true) {
		return
	}
	manager.draining.Store(true)
	manager.drains.Add(1)
	generation := manager.drainGeneration.Add(1)
	manager.NotifyAll(remotev1.GoAwayReason_GO_AWAY_REASON_GRANT_REVOKED, 0, true)
	go func() {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		<-timer.C
		if manager.revoked.Load() && manager.drainGeneration.Load() == generation {
			manager.CloseAll(websocket.StatusPolicyViolation, "relay identity revoked")
		}
	}()
}

func (manager *ConnectionManager) Resume() {
	if manager == nil || manager.revoked.Load() || !manager.draining.CompareAndSwap(true, false) {
		return
	}
	// Invalidate the pending drain deadline without killing already established
	// sessions. Clients that acted on GOAWAY may reconnect normally.
	manager.drainGeneration.Add(1)
}

func (manager *ConnectionManager) NotifyAll(reason remotev1.GoAwayReason, reconnectAfter time.Duration, refreshAssignment bool) {
	if manager == nil {
		return
	}
	manager.mu.Lock()
	sessions := make([]*Session, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	manager.mu.Unlock()
	for _, session := range sessions {
		_ = session.Enqueue(goAwayEnvelope(session.epoch, reason, reconnectAfter, refreshAssignment))
	}
}

func (manager *ConnectionManager) CloseAll(status websocket.StatusCode, reason string) {
	if manager == nil {
		return
	}
	manager.mu.Lock()
	sessions := make([]*Session, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	routeChanged := false
	for _, session := range sessions {
		routeChanged = routeChanged || session.route != nil
	}
	manager.sessions = make(map[string]*Session)
	manager.active.Store(0)
	manager.mu.Unlock()
	for _, session := range sessions {
		session.stop(status, reason)
	}
	if routeChanged {
		manager.signalRouteChange()
	}
}

func (manager *ConnectionManager) Accepting() bool {
	return manager != nil && !manager.draining.Load() && !manager.revoked.Load()
}

func (manager *ConnectionManager) Metrics() ConnectionMetrics {
	if manager == nil {
		return ConnectionMetrics{}
	}
	manager.mu.Lock()
	sessions := make([]*Session, 0, len(manager.sessions))
	for _, session := range manager.sessions {
		sessions = append(sessions, session)
	}
	manager.mu.Unlock()
	var queueFrames, queueBytes int64
	for _, session := range sessions {
		frames, bytes := session.queue.Usage()
		queueFrames += int64(frames)
		queueBytes += int64(bytes)
	}
	return ConnectionMetrics{
		Active: manager.active.Load(), Peak: manager.peak.Load(), QueueRejected: manager.queueRejected.Load(),
		ActiveHandshakes: int64(len(manager.handshakes)), HandshakeSucceeded: manager.handshakeOK.Load(),
		HandshakeFailed: manager.handshakeFailed.Load(), HandshakeRejected: manager.handshakeReject.Load(),
		QueueFrames: queueFrames, QueueBytes: queueBytes, SupersededSessions: manager.superseded.Load(),
		DrainStarted: manager.drains.Load(), RouteRejected: manager.routeRejected.Load(),
		RateLimited:   manager.rateLimited.Load(),
		IngressFrames: manager.ingressFrames.Load(), IngressBytes: manager.ingressBytes.Load(),
		EgressFrames: manager.egressFrames.Load(), EgressBytes: manager.egressBytes.Load(),
		WriteLagMicros: manager.writeLagMicros.Load(),
	}
}

func (manager *ConnectionManager) RecordHandshake(success bool) {
	if manager == nil {
		return
	}
	if success {
		manager.handshakeOK.Add(1)
	} else {
		manager.handshakeFailed.Add(1)
	}
}

func (manager *ConnectionManager) RecordRouteRejection() {
	if manager == nil {
		return
	}
	manager.routeRejected.Add(1)
}

func (manager *ConnectionManager) RecordRateLimit() {
	if manager != nil {
		manager.rateLimited.Add(1)
	}
}

func (manager *ConnectionManager) RecordIngress(bytes int) {
	if manager == nil || bytes < 0 {
		return
	}
	manager.ingressFrames.Add(1)
	manager.ingressBytes.Add(int64(bytes))
}

func (manager *ConnectionManager) RecordEgress(bytes int, latency time.Duration) {
	if manager == nil || bytes < 0 {
		return
	}
	manager.egressFrames.Add(1)
	manager.egressBytes.Add(int64(bytes))
	micros := latency.Microseconds()
	if micros < 0 {
		micros = 0
	}
	manager.writeLagMicros.Store(micros)
}

func goAwayEnvelope(epoch uint64, reason remotev1.GoAwayReason, reconnectAfter time.Duration, refreshAssignment bool) *remotev1.Envelope {
	reconnectMillis := reconnectAfter.Milliseconds()
	if reconnectMillis < 0 {
		reconnectMillis = 0
	}
	if reconnectMillis > int64(^uint32(0)) {
		reconnectMillis = int64(^uint32(0))
	}
	return &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: epoch,
		Frame: &remotev1.Envelope_GoAway{GoAway: &remotev1.GoAway{
			Reason: reason, ReconnectAfterMillis: uint32(reconnectMillis), RefreshAssignment: refreshAssignment,
		}},
	}
}
