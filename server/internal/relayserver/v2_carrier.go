package relayserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"google.golang.org/protobuf/proto"
)

const (
	V2Subprotocol              = "wenzwork-relay.v2"
	v2MaximumCarrierFrame      = 4 << 20
	v2DefaultControlBytes      = 512 << 10
	v2DefaultInteractiveBytes  = 2 << 20
	v2DefaultBulkBytes         = 8 << 20
	v2DefaultQueueFrames       = 256
	v2DefaultHeartbeatSeconds  = 25
	v2DefaultGlobalQueueBytes  = 256 << 20
	v2DefaultGlobalQueueFrames = 65_536
)

var v2MonotonicOrigin = time.Now()

func v2MonotonicNanos() int64 { return time.Since(v2MonotonicOrigin).Nanoseconds() }

func v2HeartbeatTimeout(seconds uint32) time.Duration {
	if seconds == 0 {
		seconds = v2DefaultHeartbeatSeconds
	}
	timeout := time.Duration(seconds)*2*time.Second + 3*time.Second
	if timeout < 15*time.Second {
		return 15 * time.Second
	}
	return timeout
}

var (
	ErrV2QueueFull   = errors.New("remote/v2 carrier queue is full")
	ErrV2QueueClosed = errors.New("remote/v2 carrier queue is closed")
	ErrV2Route       = errors.New("remote/v2 link route is unavailable")
)

// V2QueueBudget bounds the aggregate queued payload retained by every Carrier
// on one Relay process. Per-Carrier limits alone multiply into an unsafe host
// memory commitment when many peers become slow at the same time.
type V2QueueBudget struct {
	mu          sync.Mutex
	maxBytes    int64
	maxFrames   int64
	bytes       int64
	frames      int64
	classBytes  [4]int64
	classFrames [4]int64
	rejected    uint64
}

type V2QueueBudgetSnapshot struct {
	Bytes             int64
	Frames            int64
	MaxBytes          int64
	MaxFrames         int64
	Rejected          uint64
	ControlBytes      int64
	InteractiveBytes  int64
	BulkBytes         int64
	ControlFrames     int64
	InteractiveFrames int64
	BulkFrames        int64
}

func NewV2QueueBudget(maxBytes, maxFrames int64) (*V2QueueBudget, error) {
	if maxBytes < 1<<20 || maxBytes > 4<<30 || maxFrames < 1 || maxFrames > 1_000_000 {
		return nil, errors.New("remote/v2 global queue budget is invalid")
	}
	return &V2QueueBudget{maxBytes: maxBytes, maxFrames: maxFrames}, nil
}

func newDefaultV2QueueBudget() *V2QueueBudget {
	budget, _ := NewV2QueueBudget(v2DefaultGlobalQueueBytes, v2DefaultGlobalQueueFrames)
	return budget
}

func (budget *V2QueueBudget) reserve(class v2Priority, bytes int) bool {
	if budget == nil {
		return true
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	valid := bytes > 0 && class >= v2PriorityControl && class <= v2PriorityBulk
	projectedBytes, projectedFrames := budget.bytes+int64(bytes), budget.frames+1
	if valid && class == v2PriorityBulk {
		valid = budget.classBytes[class]+int64(bytes) <= budget.maxBytes*3/4 && budget.classFrames[class]+1 <= budget.maxFrames*3/4
	}
	if valid && class == v2PriorityInteractive {
		nonControlBytes := budget.classBytes[v2PriorityInteractive] + budget.classBytes[v2PriorityBulk] + int64(bytes)
		nonControlFrames := budget.classFrames[v2PriorityInteractive] + budget.classFrames[v2PriorityBulk] + 1
		valid = nonControlBytes <= budget.maxBytes*7/8 && nonControlFrames <= budget.maxFrames*7/8
	}
	if !valid || projectedBytes > budget.maxBytes || projectedFrames > budget.maxFrames {
		budget.rejected++
		return false
	}
	budget.bytes = projectedBytes
	budget.frames = projectedFrames
	budget.classBytes[class] += int64(bytes)
	budget.classFrames[class]++
	return true
}

func (budget *V2QueueBudget) release(class v2Priority, bytes, frames int) {
	if budget == nil || class < v2PriorityControl || class > v2PriorityBulk || bytes < 0 || frames < 0 {
		return
	}
	budget.mu.Lock()
	budget.bytes = max(0, budget.bytes-int64(bytes))
	budget.frames = max(0, budget.frames-int64(frames))
	budget.classBytes[class] = max(0, budget.classBytes[class]-int64(bytes))
	budget.classFrames[class] = max(0, budget.classFrames[class]-int64(frames))
	budget.mu.Unlock()
}

func (budget *V2QueueBudget) Snapshot() V2QueueBudgetSnapshot {
	if budget == nil {
		return V2QueueBudgetSnapshot{}
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return V2QueueBudgetSnapshot{
		Bytes: budget.bytes, Frames: budget.frames, MaxBytes: budget.maxBytes, MaxFrames: budget.maxFrames, Rejected: budget.rejected,
		ControlBytes: budget.classBytes[v2PriorityControl], InteractiveBytes: budget.classBytes[v2PriorityInteractive], BulkBytes: budget.classBytes[v2PriorityBulk],
		ControlFrames: budget.classFrames[v2PriorityControl], InteractiveFrames: budget.classFrames[v2PriorityInteractive], BulkFrames: budget.classFrames[v2PriorityBulk],
	}
}

type v2Priority uint8

const (
	v2PriorityControl v2Priority = iota + 1
	v2PriorityInteractive
	v2PriorityBulk
)

type V2QueueLimits struct {
	ControlBytes     int
	InteractiveBytes int
	BulkBytes        int
	// Frames is retained as a configuration compatibility default. New
	// deployments set the three per-priority limits explicitly so a saturated
	// bulk queue cannot consume every enqueue slot needed by Link control.
	Frames            int
	ControlFrames     int
	InteractiveFrames int
	BulkFrames        int
}

func (limits V2QueueLimits) normalized() V2QueueLimits {
	if limits.ControlBytes <= 0 {
		limits.ControlBytes = v2DefaultControlBytes
	}
	if limits.InteractiveBytes <= 0 {
		limits.InteractiveBytes = v2DefaultInteractiveBytes
	}
	if limits.BulkBytes <= 0 {
		limits.BulkBytes = v2DefaultBulkBytes
	}
	frameDefault := limits.Frames
	if frameDefault <= 0 {
		frameDefault = v2DefaultQueueFrames
	}
	if limits.ControlFrames <= 0 {
		limits.ControlFrames = frameDefault
	}
	if limits.InteractiveFrames <= 0 {
		limits.InteractiveFrames = frameDefault
	}
	if limits.BulkFrames <= 0 {
		limits.BulkFrames = frameDefault
	}
	return limits
}

func (limits V2QueueLimits) valid() bool {
	return limits.ControlBytes >= 4<<10 && limits.ControlBytes <= 16<<20 &&
		limits.InteractiveBytes >= 4<<10 && limits.InteractiveBytes <= 32<<20 &&
		limits.BulkBytes >= 4<<10 && limits.BulkBytes <= 64<<20 &&
		limits.ControlFrames >= 1 && limits.ControlFrames <= 4096 &&
		limits.InteractiveFrames >= 1 && limits.InteractiveFrames <= 4096 &&
		limits.BulkFrames >= 1 && limits.BulkFrames <= 4096
}

type v2QueuedFrame struct {
	envelope *remotev2.CarrierEnvelope
	class    v2Priority
	bytes    int
}

// v2PriorityQueue is deliberately byte and frame bounded per priority. A full
// bulk queue cannot consume the control or interactive budget.
type v2PriorityQueue struct {
	mu            sync.Mutex
	queues        map[v2Priority][]v2QueuedFrame
	bytes         map[v2Priority]int
	frames        map[v2Priority]int
	limits        V2QueueLimits
	notify        chan struct{}
	closed        bool
	scheduleIndex int
	rejected      atomic.Uint64
	budget        *V2QueueBudget
}

var v2PrioritySchedule = [...]v2Priority{
	v2PriorityControl, v2PriorityControl, v2PriorityControl, v2PriorityControl,
	v2PriorityInteractive, v2PriorityInteractive,
	v2PriorityBulk,
}

func newV2PriorityQueue(limits V2QueueLimits, budgets ...*V2QueueBudget) (*v2PriorityQueue, error) {
	limits = limits.normalized()
	if !limits.valid() {
		return nil, errors.New("remote/v2 queue limits are invalid")
	}
	var budget *V2QueueBudget
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	return &v2PriorityQueue{queues: make(map[v2Priority][]v2QueuedFrame), bytes: make(map[v2Priority]int), frames: make(map[v2Priority]int), limits: limits, notify: make(chan struct{}, 1), budget: budget}, nil
}

func (queue *v2PriorityQueue) enqueue(envelope *remotev2.CarrierEnvelope, class v2Priority) error {
	if queue == nil || envelope == nil || envelope.Body == nil || class < v2PriorityControl || class > v2PriorityBulk {
		return ErrV2QueueFull
	}
	// Packet fields are assigned only by the single writer after priority
	// selection. Reserve enough room for their varints while accounting the
	// queue, then enforce the exact Carrier limit after marshal in writeLoop.
	bytes := proto.Size(envelope) + 96
	if bytes <= 0 || bytes > v2MaximumCarrierFrame {
		queue.rejected.Add(1)
		return ErrV2QueueFull
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed {
		queue.rejected.Add(1)
		return ErrV2QueueClosed
	}
	limit := queue.limitFor(class)
	if queue.frames[class]+1 > queue.frameLimitFor(class) || queue.bytes[class]+bytes > limit {
		queue.rejected.Add(1)
		return ErrV2QueueFull
	}
	if !queue.budget.reserve(class, bytes) {
		queue.rejected.Add(1)
		return ErrV2QueueFull
	}
	clone, ok := proto.Clone(envelope).(*remotev2.CarrierEnvelope)
	if !ok {
		queue.budget.release(class, bytes, 1)
		queue.rejected.Add(1)
		return ErrV2QueueFull
	}
	queue.queues[class] = append(queue.queues[class], v2QueuedFrame{envelope: clone, class: class, bytes: bytes})
	queue.bytes[class] += bytes
	queue.frames[class]++
	queue.signal()
	return nil
}

func (queue *v2PriorityQueue) dequeue(ctx context.Context) (v2QueuedFrame, error) {
	for {
		queue.mu.Lock()
		for offset := 0; offset < len(v2PrioritySchedule); offset++ {
			index := (queue.scheduleIndex + offset) % len(v2PrioritySchedule)
			class := v2PrioritySchedule[index]
			frames := queue.queues[class]
			if len(frames) == 0 {
				continue
			}
			frame := frames[0]
			if len(frames) == 1 {
				delete(queue.queues, class)
			} else {
				queue.queues[class] = frames[1:]
			}
			queue.bytes[class] -= frame.bytes
			queue.frames[class]--
			queue.budget.release(class, frame.bytes, 1)
			queue.scheduleIndex = (index + 1) % len(v2PrioritySchedule)
			if queue.queuedFramesLocked() > 0 {
				queue.signal()
			}
			queue.mu.Unlock()
			return frame, nil
		}
		closed := queue.closed
		queue.mu.Unlock()
		if closed {
			return v2QueuedFrame{}, ErrV2QueueClosed
		}
		select {
		case <-ctx.Done():
			return v2QueuedFrame{}, ctx.Err()
		case <-queue.notify:
		}
	}
}

// V2QueueSnapshot contains only bounded transport counters; it never includes
// Link ciphertext or business metadata.
type V2QueueSnapshot struct {
	ControlFrames     int
	InteractiveFrames int
	BulkFrames        int
	ControlBytes      int
	InteractiveBytes  int
	BulkBytes         int
	Rejected          uint64
}

func (queue *v2PriorityQueue) snapshot() V2QueueSnapshot {
	if queue == nil {
		return V2QueueSnapshot{}
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return V2QueueSnapshot{
		ControlFrames: queue.frames[v2PriorityControl], InteractiveFrames: queue.frames[v2PriorityInteractive], BulkFrames: queue.frames[v2PriorityBulk],
		ControlBytes: queue.bytes[v2PriorityControl], InteractiveBytes: queue.bytes[v2PriorityInteractive], BulkBytes: queue.bytes[v2PriorityBulk], Rejected: queue.rejected.Load(),
	}
}

func (queue *v2PriorityQueue) close() {
	if queue == nil {
		return
	}
	queue.mu.Lock()
	if queue.closed {
		queue.mu.Unlock()
		return
	}
	queue.closed = true
	for class := v2PriorityControl; class <= v2PriorityBulk; class++ {
		queue.budget.release(class, queue.bytes[class], queue.frames[class])
	}
	queue.queues = make(map[v2Priority][]v2QueuedFrame)
	queue.bytes = make(map[v2Priority]int)
	queue.frames = make(map[v2Priority]int)
	queue.mu.Unlock()
	queue.signal()
}

func (queue *v2PriorityQueue) limitFor(class v2Priority) int {
	switch class {
	case v2PriorityControl:
		return queue.limits.ControlBytes
	case v2PriorityInteractive:
		return queue.limits.InteractiveBytes
	default:
		return queue.limits.BulkBytes
	}
}

func (queue *v2PriorityQueue) frameLimitFor(class v2Priority) int {
	switch class {
	case v2PriorityControl:
		return queue.limits.ControlFrames
	case v2PriorityInteractive:
		return queue.limits.InteractiveFrames
	default:
		return queue.limits.BulkFrames
	}
}

func (queue *v2PriorityQueue) queuedFramesLocked() int {
	if queue == nil {
		return 0
	}
	return queue.frames[v2PriorityControl] + queue.frames[v2PriorityInteractive] + queue.frames[v2PriorityBulk]
}

func (queue *v2PriorityQueue) signal() {
	select {
	case queue.notify <- struct{}{}:
	default:
	}
}

type v2CarrierRole uint8

const (
	v2CarrierClient v2CarrierRole = iota + 1
	v2CarrierDevice
)

type v2Carrier struct {
	id       string
	epoch    uint64
	role     v2CarrierRole
	clientID string
	deviceID string
	conn     *websocket.Conn
	queue    *v2PriorityQueue
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}

	packetMu             sync.Mutex
	nextPacket           uint64
	lastReceived         uint64
	lastPeerAcknowledged uint64
	closeOnce            sync.Once
	lastInbound          atomic.Int64
	lastOutbound         atomic.Int64
	lastPong             atomic.Int64
	lastPing             atomic.Int64
	lastAckProgress      atomic.Int64
	rttMillis            atomic.Int64
	heartbeat            atomic.Uint32
	watchdogOnce         sync.Once
	closeReason          atomic.Value
}

func newV2Carrier(parent context.Context, id string, epoch uint64, role v2CarrierRole, clientID, deviceID string, connection *websocket.Conn, limits V2QueueLimits, budget *V2QueueBudget) (*v2Carrier, error) {
	if parent == nil || id == "" || epoch == 0 || connection == nil || (role != v2CarrierClient && role != v2CarrierDevice) {
		return nil, ErrV2Route
	}
	queue, err := newV2PriorityQueue(limits, budget)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	carrier := &v2Carrier{id: id, epoch: epoch, role: role, clientID: clientID, deviceID: deviceID, conn: connection, queue: queue, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	now := v2MonotonicNanos()
	carrier.lastInbound.Store(now)
	carrier.lastOutbound.Store(now)
	carrier.lastPong.Store(now)
	carrier.lastAckProgress.Store(now)
	go carrier.writeLoop()
	return carrier, nil
}

// startWatchdog owns only liveness bookkeeping and socket shutdown. The
// handler remains the sole reader; this goroutine never calls Read.
func (carrier *v2Carrier) startWatchdog(seconds uint32, onTimeout func(string)) {
	if carrier == nil {
		return
	}
	if seconds < 1 || seconds > 300 {
		seconds = v2DefaultHeartbeatSeconds
	}
	carrier.watchdogOnce.Do(func() {
		if carrier.ctx == nil {
			return
		}
		carrier.heartbeat.Store(seconds)
		now := v2MonotonicNanos()
		carrier.lastInbound.Store(now)
		carrier.lastOutbound.Store(now)
		carrier.lastPong.Store(now)
		carrier.lastAckProgress.Store(now)
		interval := max(time.Second, time.Duration(seconds)*time.Second)
		carrier.lastPing.Store(now / int64(time.Millisecond))
		go func() {
			timer := time.NewTimer(interval)
			defer timer.Stop()
			for {
				select {
				case <-carrier.ctx.Done():
					return
				case <-timer.C:
					now := v2MonotonicNanos()
					last := carrier.lastInbound.Load()
					if pong := carrier.lastPong.Load(); pong > last {
						last = pong
					}
					timeout := v2HeartbeatTimeout(seconds)
					activityAge := time.Duration(max(int64(0), now-last))
					if activityAge > timeout {
						carrier.closeWithReason("heartbeat_timeout")
						if onTimeout != nil {
							onTimeout("heartbeat_timeout")
						}
						return
					}
					carrier.packetMu.Lock()
					outstanding := carrier.nextPacket > carrier.lastPeerAcknowledged
					carrier.packetMu.Unlock()
					ackAge := time.Duration(max(int64(0), now-carrier.lastAckProgress.Load()))
					if outstanding && ackAge > timeout {
						carrier.closeWithReason("ack_timeout")
						if onTimeout != nil {
							onTimeout("ack_timeout")
						}
						return
					}
					lastProbe := carrier.lastPing.Load() * int64(time.Millisecond)
					// A fully acknowledged business write is peer-visible activity and
					// can postpone a redundant probe. An unacknowledged write cannot:
					// receive-only streams such as AI generation have no Client business
					// frame on which to carry the Carrier ACK. Probe them once per H so
					// the Pong advances both inbound liveness and acknowledgement state.
					probeBase := lastProbe
					if !outstanding {
						probeBase = max(carrier.lastOutbound.Load(), lastProbe)
					}
					probeAge := time.Duration(max(int64(0), now-probeBase))
					if probeAge >= interval {
						probe := uint64(now / int64(time.Millisecond))
						carrier.lastPing.Store(int64(probe))
						if err := carrier.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{MonotonicMillis: probe}}}, v2PriorityControl); err != nil {
							carrier.closeWithReason("heartbeat_send_failed")
							if onTimeout != nil {
								onTimeout("heartbeat_send_failed")
							}
							return
						}
						probeAge = 0
					}
					delay := max(time.Millisecond, min(interval-probeAge, timeout-activityAge))
					if outstanding {
						delay = max(time.Millisecond, min(delay, timeout-ackAge))
					}
					timer.Reset(delay)
				}
			}
		}()
	})
}

func (carrier *v2Carrier) close() {
	carrier.closeWithReason("closed")
}

func (carrier *v2Carrier) closeWithReason(reason string) {
	if carrier == nil {
		return
	}
	carrier.closeOnce.Do(func() {
		if reason != "" {
			carrier.closeReason.Store(reason)
		}
		if carrier.cancel != nil {
			carrier.cancel()
		}
		if carrier.queue != nil {
			carrier.queue.close()
		}
		if carrier.conn != nil {
			carrier.conn.CloseNow()
		}
	})
}

func (carrier *v2Carrier) writeLoop() {
	defer close(carrier.done)
	for {
		frame, err := carrier.queue.dequeue(carrier.ctx)
		if err != nil {
			return
		}
		envelope := frame.envelope
		if envelope == nil {
			carrier.closeWithReason("write_error")
			return
		}
		// Allocate packet_sequence here, after the priority dequeue. This is the
		// only point allowed to advance outbound sequencing; allocating during
		// enqueue would make an overtaken bulk packet arrive after a control
		// packet with a larger sequence and force the peer to reject the Carrier.
		carrier.packetMu.Lock()
		if carrier.nextPacket == ^uint64(0) {
			carrier.packetMu.Unlock()
			carrier.closeWithReason("write_error")
			return
		}
		hadNoOutstandingPackets := carrier.lastPeerAcknowledged == carrier.nextPacket
		carrier.nextPacket++
		if hadNoOutstandingPackets {
			carrier.lastAckProgress.Store(v2MonotonicNanos())
		}
		envelope.ProtocolMajor = 2
		envelope.CarrierId = carrier.id
		envelope.CarrierEpoch = carrier.epoch
		envelope.PacketSequence = carrier.nextPacket
		envelope.AcknowledgedSequence = carrier.lastReceived
		carrier.packetMu.Unlock()
		payload, err := proto.Marshal(envelope)
		if err != nil || len(payload) == 0 || len(payload) > v2MaximumCarrierFrame {
			carrier.close()
			return
		}
		if err := carrier.conn.Write(carrier.ctx, websocket.MessageBinary, payload); err != nil {
			carrier.close()
			return
		}
		carrier.lastOutbound.Store(v2MonotonicNanos())
	}
}

func (carrier *v2Carrier) acceptIncoming(envelope *remotev2.CarrierEnvelope) error {
	if carrier == nil || envelope == nil || envelope.GetProtocolMajor() != 2 || envelope.GetCarrierId() != carrier.id || envelope.GetCarrierEpoch() != carrier.epoch || envelope.GetPacketSequence() == 0 || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return ErrV2Route
	}
	carrier.packetMu.Lock()
	defer carrier.packetMu.Unlock()
	if envelope.GetPacketSequence() != carrier.lastReceived+1 || envelope.GetAcknowledgedSequence() > carrier.nextPacket {
		return ErrV2Route
	}
	carrier.lastReceived = envelope.GetPacketSequence()
	now := v2MonotonicNanos()
	carrier.lastInbound.Store(now)
	if acknowledged := envelope.GetAcknowledgedSequence(); acknowledged > carrier.lastPeerAcknowledged {
		carrier.lastPeerAcknowledged = acknowledged
		carrier.lastAckProgress.Store(now)
	}
	if pong := envelope.GetPong(); pong != nil {
		probe := int64(pong.GetMonotonicMillis())
		if probe != 0 && probe == carrier.lastPing.Load() {
			carrier.lastPong.Store(now)
			carrier.rttMillis.Store((now - probe*int64(time.Millisecond)) / int64(time.Millisecond))
		}
	}
	return nil
}

func (carrier *v2Carrier) enqueueEnvelope(envelope *remotev2.CarrierEnvelope, class v2Priority) error {
	if carrier == nil || envelope == nil || envelope.Body == nil {
		return ErrV2QueueClosed
	}
	return carrier.queue.enqueue(envelope, class)
}

func (carrier *v2Carrier) enqueueLink(link *remotev2.LinkEnvelope) error {
	if link == nil {
		return ErrV2QueueFull
	}
	return carrier.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: link}}, v2LinkPriority(link))
}

func (carrier *v2Carrier) rejectStream(linkID, channelID, streamID string) error {
	return carrier.rejectStreamWithReason(linkID, channelID, streamID, remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_BACKPRESSURE)
}

func (carrier *v2Carrier) rejectStreamWithReason(linkID, channelID, streamID string, reason remotev2.ProtocolErrorCode) error {
	if carrier == nil || linkID == "" || reason == remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_UNSPECIFIED {
		return ErrV2Route
	}
	return carrier.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_StreamRejected{StreamRejected: &remotev2.CarrierStreamRejected{
		LinkId: linkID, ChannelId: channelID, StreamId: streamID, Reason: reason,
	}}}, v2PriorityControl)
}

func v2LinkPriority(link *remotev2.LinkEnvelope) v2Priority {
	if link == nil || link.GetEncrypted() == nil {
		return v2PriorityControl
	}
	switch remotev2.FrameType(link.GetEncrypted().GetFrameType()) {
	case remotev2.FrameType_FRAME_TYPE_FILE_CHUNK:
		return v2PriorityBulk
	case remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE, remotev2.FrameType_FRAME_TYPE_RPC_EVENT, remotev2.FrameType_FRAME_TYPE_STREAM_DATA:
		return v2PriorityInteractive
	default:
		// RPC_REQUEST deliberately shares the control FIFO with CHANNEL_OPEN and
		// STREAM_OPEN. Splitting a request from its opening frames lets weighted
		// priority scheduling deliver it first, at which point the receiver can
		// only reject an otherwise valid operation as STREAM_NOT_FOUND.
		return v2PriorityControl
	}
}

func v2LinkMetadata(link *remotev2.LinkEnvelope) (linkID, channelID, streamID string) {
	if link == nil {
		return "", "", ""
	}
	if record := link.GetEncrypted(); record != nil {
		return record.GetLinkId(), record.GetChannelId(), record.GetStreamId()
	}
	return link.GetLinkId(), "", ""
}
