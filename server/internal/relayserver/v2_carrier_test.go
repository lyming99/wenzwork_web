package relayserver

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestV2PriorityQueueBoundsFramesPerPriority(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{
		ControlBytes: 4 << 10, InteractiveBytes: 4 << 10, BulkBytes: 4 << 10,
		ControlFrames: 1, InteractiveFrames: 1, BulkFrames: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	bulk := &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: &remotev2.LinkEnvelope{LinkId: "link", Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{LinkId: "link", ChannelId: "channel", StreamId: "stream", FrameType: remotev2.FrameType_FRAME_TYPE_FILE_CHUNK}}}}}
	if err := queue.enqueue(bulk, v2PriorityBulk); err != nil {
		t.Fatalf("enqueue bulk = %v", err)
	}
	if err := queue.enqueue(bulk, v2PriorityBulk); !errors.Is(err, ErrV2QueueFull) {
		t.Fatalf("second bulk enqueue = %v, want ErrV2QueueFull", err)
	}
	control := &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{}}}
	if err := queue.enqueue(control, v2PriorityControl); err != nil {
		t.Fatalf("enqueue control behind saturated bulk = %v", err)
	}
	frame, err := queue.dequeue(context.Background())
	if err != nil || frame.class != v2PriorityControl {
		t.Fatalf("first dequeue = %#v, %v; want control", frame, err)
	}
	frame, err = queue.dequeue(context.Background())
	if err != nil || frame.class != v2PriorityBulk {
		t.Fatalf("second dequeue = %#v, %v; want bulk", frame, err)
	}
}

func TestV2PriorityQueuesShareAndReleaseGlobalBudget(t *testing.T) {
	budget, err := NewV2QueueBudget(1<<20, 2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := newV2PriorityQueue(V2QueueLimits{Frames: 8}, budget)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newV2PriorityQueue(V2QueueLimits{Frames: 8}, budget)
	if err != nil {
		t.Fatal(err)
	}
	frame := &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{MonotonicMillis: 1}}}
	if err := first.enqueue(frame, v2PriorityControl); err != nil {
		t.Fatal(err)
	}
	if err := second.enqueue(frame, v2PriorityControl); err != nil {
		t.Fatal(err)
	}
	if err := first.enqueue(frame, v2PriorityControl); !errors.Is(err, ErrV2QueueFull) {
		t.Fatalf("aggregate queue overflow = %v, want ErrV2QueueFull", err)
	}
	if snapshot := budget.Snapshot(); snapshot.Frames != 2 || snapshot.Rejected != 1 {
		t.Fatalf("aggregate budget snapshot = %+v", snapshot)
	}
	if _, err := first.dequeue(context.Background()); err != nil {
		t.Fatal(err)
	}
	second.close()
	if snapshot := budget.Snapshot(); snapshot.Frames != 0 || snapshot.Bytes != 0 {
		t.Fatalf("released aggregate budget snapshot = %+v", snapshot)
	}
}

func TestV2GlobalBudgetReservesControlCapacityDuringBulkSaturation(t *testing.T) {
	budget, err := NewV2QueueBudget(1<<20, 8)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 16}, budget)
	if err != nil {
		t.Fatal(err)
	}
	bulk := &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: &remotev2.LinkEnvelope{
		LinkId: "link", Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{FrameType: remotev2.FrameType_FRAME_TYPE_FILE_CHUNK, Ciphertext: []byte{1}}},
	}}}
	for range 6 {
		if err := queue.enqueue(bulk, v2PriorityBulk); err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.enqueue(bulk, v2PriorityBulk); !errors.Is(err, ErrV2QueueFull) {
		t.Fatalf("bulk reservation overflow = %v", err)
	}
	control := &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{MonotonicMillis: 1}}}
	for range 2 {
		if err := queue.enqueue(control, v2PriorityControl); err != nil {
			t.Fatalf("reserved control enqueue = %v", err)
		}
	}
	if snapshot := budget.Snapshot(); snapshot.BulkFrames != 6 || snapshot.ControlFrames != 2 || snapshot.Frames != 8 {
		t.Fatalf("priority-reserved budget = %+v", snapshot)
	}
}

func TestV2QueueChurnReturnsAllGlobalBudget(t *testing.T) {
	budget, err := NewV2QueueBudget(1<<20, 64)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 64}, budget)
	if err != nil {
		t.Fatal(err)
	}
	frame := &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{MonotonicMillis: 1}}}
	for range 10_000 {
		if err := queue.enqueue(frame, v2PriorityControl); err != nil {
			t.Fatal(err)
		}
		if _, err := queue.dequeue(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if snapshot := budget.Snapshot(); snapshot.Bytes != 0 || snapshot.Frames != 0 || snapshot.Rejected != 0 {
		t.Fatalf("queue churn leaked aggregate budget: %+v", snapshot)
	}
}

func TestV2CarrierWatchdogClosesHalfOpenConnection(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	carrier := &v2Carrier{ctx: ctx, cancel: cancel, queue: queue}
	timedOut := make(chan string, 1)
	carrier.startWatchdog(1, func(reason string) { timedOut <- reason })
	stale := v2MonotonicNanos() - int64(16*time.Second)
	carrier.lastInbound.Store(stale)
	carrier.lastPong.Store(stale)
	carrier.lastAckProgress.Store(stale)
	select {
	case reason := <-timedOut:
		if reason != "heartbeat_timeout" {
			t.Fatalf("watchdog reason = %q", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("half-open Carrier watchdog did not converge")
	}
}

func TestV2CarrierWatchdogClosesOneWayAcknowledgementBlackhole(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	carrier := &v2Carrier{ctx: ctx, cancel: cancel, queue: queue, nextPacket: 1}
	timedOut := make(chan string, 1)
	carrier.startWatchdog(1, func(reason string) { timedOut <- reason })
	carrier.lastInbound.Store(v2MonotonicNanos())
	carrier.lastPong.Store(v2MonotonicNanos())
	carrier.lastAckProgress.Store(v2MonotonicNanos() - int64(16*time.Second))
	select {
	case reason := <-timedOut:
		if reason != "ack_timeout" {
			t.Fatalf("watchdog reason = %q", reason)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("one-way Carrier blackhole did not converge")
	}
}

func TestV2CarrierWatchdogUsesOneFullIdleIntervalPerProbe(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	carrier := &v2Carrier{ctx: ctx, cancel: cancel, queue: queue}
	carrier.startWatchdog(1, nil)
	defer carrier.close()

	select {
	case <-queue.notify:
		t.Fatal("Relay probed at the retired half-interval cadence")
	case <-time.After(700 * time.Millisecond):
	}
	select {
	case <-queue.notify:
		frame, err := queue.dequeue(context.Background())
		if err != nil || frame.envelope.GetPing() == nil {
			t.Fatalf("idle watchdog frame = %+v, %v", frame, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Relay did not probe after one idle heartbeat interval")
	}
}

func TestV2CarrierWatchdogProbesDuringContinuousInboundTraffic(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	carrier := &v2Carrier{ctx: ctx, cancel: cancel, queue: queue}
	carrier.startWatchdog(1, nil)
	defer carrier.close()

	inbound := time.NewTicker(20 * time.Millisecond)
	defer inbound.Stop()
	deadline := time.NewTimer(1800 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case <-inbound.C:
			// Model a healthy one-way Device -> Relay event stream. It keeps
			// Relay-side liveness fresh but cannot satisfy the Device's need
			// to periodically receive a frame from Relay.
			carrier.lastInbound.Store(v2MonotonicNanos())
		case <-queue.notify:
			frame, err := queue.dequeue(context.Background())
			if err != nil || frame.envelope.GetPing() == nil {
				t.Fatalf("one-way watchdog frame = %+v, %v", frame, err)
			}
			return
		case <-deadline.C:
			t.Fatal("continuous inbound traffic suppressed the Relay heartbeat probe")
		}
	}
}

func TestV2CarrierWatchdogDefersProbeDuringAcknowledgedContinuousOutboundTraffic(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	carrier := &v2Carrier{ctx: ctx, cancel: cancel, queue: queue}
	carrier.startWatchdog(1, nil)
	defer carrier.close()

	outbound := time.NewTicker(20 * time.Millisecond)
	defer outbound.Stop()
	deadline := time.NewTimer(1500 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case <-outbound.C:
			// Model successful Relay -> Device business writes for which no
			// application acknowledgement is outstanding. Those frames can
			// postpone a redundant Ping.
			carrier.lastOutbound.Store(v2MonotonicNanos())
		case <-queue.notify:
			t.Fatal("continuous outbound traffic did not postpone the Relay heartbeat probe")
		case <-deadline.C:
			return
		}
	}
}

func TestV2CarrierWatchdogProbesDuringUnacknowledgedContinuousOutboundTraffic(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	carrier := &v2Carrier{
		ctx:          ctx,
		cancel:       cancel,
		queue:        queue,
		nextPacket:   1,
		lastReceived: 1,
	}
	carrier.startWatchdog(1, nil)
	defer carrier.close()

	outbound := time.NewTicker(20 * time.Millisecond)
	defer outbound.Stop()
	deadline := time.NewTimer(1800 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case <-outbound.C:
			// Model a receive-only AI conversation: the Relay keeps writing
			// deltas, but the Client has no business frame on which to carry
			// its Carrier acknowledgement. A Ping must solicit a Pong before
			// the otherwise healthy stream reaches the ACK timeout.
			carrier.lastOutbound.Store(v2MonotonicNanos())
		case <-queue.notify:
			frame, err := queue.dequeue(context.Background())
			if err != nil || frame.envelope.GetPing() == nil {
				t.Fatalf("unacknowledged outbound watchdog frame = %+v, %v", frame, err)
			}
			return
		case <-deadline.C:
			t.Fatal("unacknowledged continuous outbound traffic suppressed the Relay heartbeat probe")
		}
	}
}

func TestV2CarrierKeepsRPCRequestBehindItsStreamOpen(t *testing.T) {
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	carrier := &v2Carrier{queue: queue}
	linkID, channelID, streamID := "link", "channel", "stream"
	open := &remotev2.LinkEnvelope{LinkId: linkID, Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
		LinkId: linkID, ChannelId: channelID, StreamId: "v2-channel-control", FrameType: remotev2.FrameType_FRAME_TYPE_STREAM_OPEN,
	}}}
	request := &remotev2.LinkEnvelope{LinkId: linkID, Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
		LinkId: linkID, ChannelId: channelID, StreamId: streamID, FrameType: remotev2.FrameType_FRAME_TYPE_RPC_REQUEST,
	}}}
	if err := carrier.enqueueLink(open); err != nil {
		t.Fatal(err)
	}
	if err := carrier.enqueueLink(request); err != nil {
		t.Fatal(err)
	}
	first, err := queue.dequeue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := queue.dequeue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.envelope.GetLink().GetEncrypted().GetFrameType() != remotev2.FrameType_FRAME_TYPE_STREAM_OPEN ||
		second.envelope.GetLink().GetEncrypted().GetFrameType() != remotev2.FrameType_FRAME_TYPE_RPC_REQUEST ||
		first.class != v2PriorityControl || second.class != v2PriorityControl {
		t.Fatalf("causal Link order = (%#v, %#v)", first, second)
	}
}

func TestV2CarrierRejectsAcknowledgementBeyondSentSequence(t *testing.T) {
	carrier := &v2Carrier{id: "carrier", epoch: 1, nextPacket: 2}
	valid := &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: "carrier", CarrierEpoch: 1,
		PacketSequence: 1, AcknowledgedSequence: 2,
		Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{}},
	}
	if err := carrier.acceptIncoming(valid); err != nil {
		t.Fatalf("valid acknowledgement rejected: %v", err)
	}
	invalid := &remotev2.CarrierEnvelope{
		ProtocolMajor: 2, CarrierId: "carrier", CarrierEpoch: 1,
		PacketSequence: 2, AcknowledgedSequence: 3,
		Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{}},
	}
	if err := carrier.acceptIncoming(invalid); !errors.Is(err, ErrV2Route) {
		t.Fatalf("future acknowledgement error = %v, want ErrV2Route", err)
	}
}

func TestV2HubForwardsExpiredLinkFeedbackWithoutClosingSiblingCarriers(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 4})
	if err != nil {
		t.Fatal(err)
	}
	deviceQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 4})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client-new", epoch: 2, role: v2CarrierClient, clientID: "client", deviceID: "device", queue: clientQueue}
	device := &v2Carrier{id: "device-carrier", epoch: 7, role: v2CarrierDevice, deviceID: "device", queue: deviceQueue}
	hub := NewV2Hub()
	hub.carriers[client.id] = client
	hub.carriers[device.id] = device
	hub.deviceCarriers[device.deviceID] = device.id
	hub.links["link"] = v2LinkRoute{linkID: "link", clientID: client.clientID, deviceID: device.deviceID, clientCarrierID: client.id}

	if err := hub.routeStreamRejected(device, &remotev2.CarrierStreamRejected{
		LinkId: "link", Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED,
	}); err != nil {
		t.Fatalf("routeStreamRejected() = %v", err)
	}
	frame, err := clientQueue.dequeue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	feedback := frame.envelope.GetStreamRejected()
	if feedback == nil || feedback.GetLinkId() != "link" || feedback.GetReason() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED {
		t.Fatalf("forwarded feedback = %#v", frame.envelope)
	}
	if hub.carriers[device.id] != device || hub.links["link"].clientCarrierID != client.id {
		t.Fatal("transport feedback altered an otherwise healthy Carrier or Link route")
	}

	// An expired-link feedback is intentionally receiver-scoped. A stale
	// Carrier cannot forge it into a different controller's Link route.
	stale := &v2Carrier{id: "stale", role: v2CarrierClient, clientID: "other", deviceID: "device", queue: clientQueue}
	if err := hub.routeStreamRejected(stale, &remotev2.CarrierStreamRejected{
		LinkId: "link", Reason: remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED,
	}); !errors.Is(err, ErrV2Route) {
		t.Fatalf("stale routeStreamRejected() = %v, want ErrV2Route", err)
	}
}

func TestV2HubBackpressureNotifiesBothAIStreamEndpoints(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{ControlFrames: 4, InteractiveFrames: 1, BulkFrames: 1})
	if err != nil {
		t.Fatal(err)
	}
	deviceQueue, err := newV2PriorityQueue(V2QueueLimits{ControlFrames: 4, InteractiveFrames: 1, BulkFrames: 1})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client", epoch: 2, role: v2CarrierClient, clientID: "controller", deviceID: "device", queue: clientQueue}
	device := &v2Carrier{id: "device", epoch: 7, role: v2CarrierDevice, deviceID: "device", queue: deviceQueue}
	hub := NewV2Hub()
	hub.carriers[client.id], hub.carriers[device.id] = client, device
	hub.deviceCarriers[device.deviceID] = device.id
	hub.links["link"] = v2LinkRoute{linkID: "link", clientID: client.clientID, deviceID: device.deviceID, clientCarrierID: client.id, targetEpoch: device.epoch}

	// Saturate only the Client's interactive class. The independent Control
	// reservation must still carry BACKPRESSURE to both the Device sender and
	// the Client observer instead of leaving the latter on a long AI timeout.
	if err := client.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Link{Link: &remotev2.LinkEnvelope{
		LinkId: "other", Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{LinkId: "other", ChannelId: "other-channel", StreamId: "other-stream", FrameType: remotev2.FrameType_FRAME_TYPE_RPC_EVENT}},
	}}}, v2PriorityInteractive); err != nil {
		t.Fatal(err)
	}
	link := &remotev2.LinkEnvelope{LinkId: "link", Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
		LinkId: "link", ChannelId: "ai-channel", StreamId: "ai-stream", FrameType: remotev2.FrameType_FRAME_TYPE_RPC_EVENT, StreamSequence: 1,
	}}}
	if err := hub.routeDeviceLink(device, link); err != nil {
		t.Fatalf("routeDeviceLink() = %v", err)
	}
	for endpoint, queue := range map[string]*v2PriorityQueue{"device": deviceQueue, "client": clientQueue} {
		frame, err := queue.dequeue(context.Background())
		if err != nil {
			t.Fatalf("%s feedback: %v", endpoint, err)
		}
		rejected := frame.envelope.GetStreamRejected()
		if rejected == nil || rejected.GetLinkId() != "link" || rejected.GetChannelId() != "ai-channel" || rejected.GetStreamId() != "ai-stream" || rejected.GetReason() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_BACKPRESSURE {
			t.Fatalf("%s feedback = %#v", endpoint, frame.envelope)
		}
	}
	if hub.carriers[client.id] != client || hub.carriers[device.id] != device || hub.links["link"].linkID != "link" {
		t.Fatal("Stream backpressure altered its Carrier or Link")
	}
}

func TestV2HubRejectsUnknownClientLinkAsLinkScoped(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 4})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{
		id: "client", epoch: 2, role: v2CarrierClient,
		clientID: "client", deviceID: "device", queue: clientQueue,
	}
	hub := NewV2Hub()
	hub.carriers[client.id] = client
	claims := remoteauth.DeviceLinkGrantClaims{ClientID: client.clientID, DeviceID: client.deviceID}
	linkID := "orphan-link"
	link := &remotev2.LinkEnvelope{
		LinkId: linkID,
		Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
			LinkId: linkID, ChannelId: "channel", StreamId: "stream",
			FrameType: remotev2.FrameType_FRAME_TYPE_RPC_REQUEST, StreamSequence: 1,
		}},
	}
	if err := hub.routeClientLink(client, claims, link); err != nil {
		t.Fatalf("routeClientLink() = %v", err)
	}
	frame, err := clientQueue.dequeue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rejected := frame.envelope.GetStreamRejected()
	if rejected == nil || rejected.GetLinkId() != linkID || rejected.GetChannelId() != "" || rejected.GetStreamId() != "" ||
		rejected.GetReason() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_STREAM_NOT_FOUND {
		t.Fatalf("unknown Link rejection = %#v; want Link-scoped STREAM_NOT_FOUND", frame.envelope)
	}
}

func TestV2HubKeepsLinkQuietlyWhileDeviceCarrierIsAbsent(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client", epoch: 2, role: v2CarrierClient, clientID: "client", deviceID: "device", queue: clientQueue}
	hub := NewV2Hub()
	hub.carriers[client.id] = client
	hub.links["link"] = v2LinkRoute{linkID: "link", clientID: "client", deviceID: "device", clientCarrierID: client.id, targetEpoch: 7}
	claims := remoteauth.DeviceLinkGrantClaims{ClientID: "client", DeviceID: "device", TargetConnectionEpoch: 7}
	frame := &remotev2.LinkEnvelope{LinkId: "link", Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
		LinkId: "link", ChannelId: "channel", StreamId: "stream", FrameType: remotev2.FrameType_FRAME_TYPE_RPC_REQUEST,
	}}}
	if err := hub.routeClientLink(client, claims, frame); !errors.Is(err, ErrV2TransientRoute) {
		t.Fatalf("routeClientLink() = %v, want transient route", err)
	}
	if _, ok := hub.links["link"]; !ok {
		t.Fatal("transient target loss deleted the Link route")
	}
	if snapshot := clientQueue.snapshot(); snapshot.ControlFrames != 0 || snapshot.ControlBytes != 0 {
		t.Fatalf("transient target loss queued Carrier-wide feedback: %+v", snapshot)
	}
}

func TestV2HubDeviceHandoffPreservesClientCarrierAndMonotonicRoute(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	deviceQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client", epoch: 2, role: v2CarrierClient, clientID: "controller", deviceID: "device", queue: clientQueue}
	oldDevice := &v2Carrier{id: "device-7", epoch: 7, role: v2CarrierDevice, deviceID: "device"}
	newDevice := &v2Carrier{id: "device-8", epoch: 8, role: v2CarrierDevice, deviceID: "device", queue: deviceQueue}
	hub := NewV2Hub()
	hub.carriers[client.id], hub.carriers[oldDevice.id] = client, oldDevice
	hub.deviceCarriers[oldDevice.deviceID] = oldDevice.id
	hub.deviceRoutes[oldDevice.deviceID] = relayrouter.Route{DeviceID: "device", ConnectionID: oldDevice.id, ConnectionEpoch: oldDevice.epoch, ProtocolVersion: 2}
	hub.links["link"] = v2LinkRoute{linkID: "link", clientID: client.clientID, deviceID: oldDevice.deviceID, clientCarrierID: client.id, targetEpoch: oldDevice.epoch}

	route := relayrouter.Route{DeviceID: "device", ConnectionID: newDevice.id, ConnectionEpoch: newDevice.epoch, ProtocolVersion: 2}
	if err := hub.attachDeviceBeforePublish(newDevice, route, func() error {
		return newDevice.enqueueEnvelope(&remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ready{Ready: &remotev2.CarrierReady{CarrierId: newDevice.id, CarrierEpoch: newDevice.epoch}}}, v2PriorityControl)
	}); err != nil {
		t.Fatal(err)
	}
	frame, err := clientQueue.dequeue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rejected := frame.envelope.GetStreamRejected()
	if rejected == nil || rejected.GetLinkId() != "link" || rejected.GetReason() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_ROUTE_STALE {
		t.Fatalf("handoff feedback = %#v; want Link-scoped ROUTE_STALE", frame.envelope)
	}
	if current := hub.carriers[client.id]; current != client || hub.links["link"].targetEpoch != newDevice.epoch {
		t.Fatalf("client/link route changed unexpectedly: carrier=%p route=%+v", current, hub.links["link"])
	}
	staleDeviceFrame := &remotev2.LinkEnvelope{LinkId: "link", Body: &remotev2.LinkEnvelope_Encrypted{Encrypted: &remotev2.EncryptedRecord{
		LinkId: "link", ChannelId: "channel", StreamId: "stream", FrameType: remotev2.FrameType_FRAME_TYPE_RPC_RESPONSE,
	}}}
	if err := hub.routeDeviceLink(oldDevice, staleDeviceFrame); !errors.Is(err, ErrV2Route) {
		t.Fatalf("superseded Device frame = %v, want ErrV2Route", err)
	}
	if err := hub.resumeDevice(oldDevice, &remotev2.CarrierResume{LinkId: "link"}); !errors.Is(err, ErrV2Route) {
		t.Fatalf("superseded Device resume = %v, want ErrV2Route", err)
	}
	if snapshot := clientQueue.snapshot(); snapshot.ControlFrames != 0 {
		t.Fatalf("superseded Device reached Client queue: %+v", snapshot)
	}
	claims := remoteauth.DeviceLinkGrantClaims{ClientID: client.clientID, DeviceID: newDevice.deviceID}
	if err := hub.resumeClient(client, claims, &remotev2.CarrierResume{LinkId: "link"}); err != nil {
		t.Fatalf("resumeClient() after handoff = %v", err)
	}
	firstDeviceFrame, err := deviceQueue.dequeue(context.Background())
	if err != nil || firstDeviceFrame.envelope.GetReady() == nil {
		t.Fatalf("first Device frame after publication = %#v, %v; want CARRIER_READY", firstDeviceFrame.envelope, err)
	}
	secondDeviceFrame, err := deviceQueue.dequeue(context.Background())
	if err != nil || secondDeviceFrame.envelope.GetResume() == nil {
		t.Fatalf("second Device frame after publication = %#v, %v; want CARRIER_RESUME", secondDeviceFrame.envelope, err)
	}

	stale := &v2Carrier{id: "device-stale", epoch: 7, role: v2CarrierDevice, deviceID: "device"}
	staleRoute := relayrouter.Route{DeviceID: "device", ConnectionID: stale.id, ConnectionEpoch: stale.epoch, ProtocolVersion: 2}
	if err := hub.attachDevice(stale, staleRoute); !errors.Is(err, ErrV2Route) {
		t.Fatalf("lower epoch attach = %v, want ErrV2Route", err)
	}
	if hub.deviceCarriers["device"] != newDevice.id || hub.deviceRoutes["device"].ConnectionEpoch != newDevice.epoch {
		t.Fatalf("stale attach rolled route back: carrier=%q route=%+v", hub.deviceCarriers["device"], hub.deviceRoutes["device"])
	}
}

func TestV2HubParallelCarrierCannotReplayLinkInitToStealBinding(t *testing.T) {
	hub := NewV2Hub()
	current := &v2Carrier{id: "current", epoch: 2, role: v2CarrierClient, clientID: "controller", deviceID: "device"}
	parallel := &v2Carrier{id: "parallel", epoch: 3, role: v2CarrierClient, clientID: "controller", deviceID: "device"}
	hub.carriers[current.id], hub.carriers[parallel.id] = current, parallel
	hub.links["link"] = v2LinkRoute{linkID: "link", clientID: current.clientID, deviceID: current.deviceID, clientCarrierID: current.id, targetEpoch: 7}
	claims := remoteauth.DeviceLinkGrantClaims{
		GrantID: "grant", ClientID: current.clientID, DeviceID: current.deviceID,
		RelayNodeID: "node", RelayCellID: "cell", TargetConnectionEpoch: 7, ClientIdentityKeyVersion: 1,
	}
	init := &remotev2.LinkEnvelope{LinkId: "link", Body: &remotev2.LinkEnvelope_LinkInit{LinkInit: &remotev2.LinkInit{
		GrantId: claims.GrantID, LinkId: "link", ClientId: claims.ClientID, DeviceId: claims.DeviceID,
		RelayNodeId: claims.RelayNodeID, RelayCellId: claims.RelayCellID, TargetConnectionEpoch: claims.TargetConnectionEpoch,
		ClientIdentityKeyVersion: claims.ClientIdentityKeyVersion, DeviceConnectionGrant: "grant",
	}}}
	if err := hub.routeClientLink(parallel, claims, init); !errors.Is(err, ErrV2Route) {
		t.Fatalf("parallel LinkInit replay = %v, want ErrV2Route", err)
	}
	if got := hub.links["link"].clientCarrierID; got != current.id {
		t.Fatalf("parallel LinkInit stole binding: got %q, want %q", got, current.id)
	}
	hub.dropLinkFromCarrier(parallel, "link")
	if _, exists := hub.links["link"]; !exists {
		t.Fatal("parallel Carrier failure deleted the healthy Link route")
	}
}

func TestV2HubHostRouteRejectionStartsFiveMinuteLinkGrace(t *testing.T) {
	hub := NewV2Hub()
	device := &v2Carrier{id: "device-carrier", epoch: 7, role: v2CarrierDevice, deviceID: "device"}
	hub.carriers[device.id] = device
	hub.deviceCarriers[device.deviceID] = device.id
	hub.deviceRoutes[device.deviceID] = relayrouter.Route{DeviceID: device.deviceID, ConnectionID: device.id, ConnectionEpoch: device.epoch, ProtocolVersion: 2}
	hub.links["link"] = v2LinkRoute{linkID: "link", clientID: "controller", deviceID: device.deviceID, clientCarrierID: "client", targetEpoch: device.epoch}
	started := time.Now()
	if !hub.RejectResident(device.deviceID, device.id, device.epoch) {
		t.Fatal("RejectResident() = false")
	}
	expires := hub.links["link"].deviceSuspendedUntil
	if expires.Before(started.Add(v2LinkRouteGrace-time.Second)) || expires.After(time.Now().Add(v2LinkRouteGrace+time.Second)) {
		t.Fatalf("route grace expiry = %v, want approximately five minutes", expires)
	}
	route := hub.links["link"]
	route.deviceSuspendedUntil = time.Now().Add(-time.Second)
	hub.links["link"] = route
	if got := hub.ActiveLinkRoutes(); got != 0 {
		t.Fatalf("expired rejected route count = %d, want 0", got)
	}
}

func TestV2HubFreshControllerLinkReplacesAbandonedRoute(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 16})
	if err != nil {
		t.Fatal(err)
	}
	deviceQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 16})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client-carrier", epoch: 2, role: v2CarrierClient, clientID: "controller", deviceID: "device", queue: clientQueue}
	device := &v2Carrier{id: "device-carrier", epoch: 7, role: v2CarrierDevice, deviceID: "device", queue: deviceQueue}
	hub := NewV2Hub()
	hub.carriers[client.id], hub.carriers[device.id] = client, device
	hub.deviceCarriers[device.deviceID] = device.id
	claims := remoteauth.DeviceLinkGrantClaims{
		GrantID: "grant", ClientID: client.clientID, DeviceID: device.deviceID,
		RelayNodeID: "node", RelayCellID: "cell", TargetConnectionEpoch: device.epoch,
		ClientIdentityKeyVersion: 1,
	}
	linkInit := func(linkID string) *remotev2.LinkEnvelope {
		return &remotev2.LinkEnvelope{LinkId: linkID, Body: &remotev2.LinkEnvelope_LinkInit{LinkInit: &remotev2.LinkInit{
			GrantId: claims.GrantID, LinkId: linkID, ClientId: claims.ClientID, DeviceId: claims.DeviceID,
			RelayNodeId: claims.RelayNodeID, RelayCellId: claims.RelayCellID, TargetConnectionEpoch: claims.TargetConnectionEpoch,
			ClientIdentityKeyVersion: claims.ClientIdentityKeyVersion, DeviceConnectionGrant: "transient",
		}}}
	}
	firstID, secondID := "first-link", "second-link"
	if err := hub.routeClientLink(client, claims, linkInit(firstID)); err != nil {
		t.Fatal(err)
	}
	if err := hub.routeClientLink(client, claims, linkInit(secondID)); err != nil {
		t.Fatal(err)
	}
	if hub.ActiveLinkRoutes() != 1 || hub.links[secondID].linkID != secondID {
		t.Fatalf("fresh Link did not replace abandoned route: %#v", hub.links)
	}
	if _, exists := hub.links[firstID]; exists {
		t.Fatal("superseded Link route remained resident")
	}
}

func TestV2HubPersistentGrantCanOpenOnNewerDeviceEpoch(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	deviceQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client-carrier", epoch: 2, role: v2CarrierClient, clientID: "controller", deviceID: "device", queue: clientQueue}
	device := &v2Carrier{id: "device-carrier", epoch: 8, role: v2CarrierDevice, deviceID: "device", queue: deviceQueue}
	hub := NewV2Hub()
	hub.carriers[client.id], hub.carriers[device.id] = client, device
	hub.deviceCarriers[device.deviceID] = device.id
	hub.deviceRoutes[device.deviceID] = relayrouter.Route{DeviceID: device.deviceID, ConnectionID: device.id, ConnectionEpoch: device.epoch, GrantVersion: 3, ProtocolVersion: 2}
	claims := remoteauth.DeviceLinkGrantClaims{
		GrantID: "grant", ClientID: client.clientID, DeviceID: device.deviceID,
		RelayNodeID: "node", RelayCellID: "cell", TargetConnectionEpoch: 7,
		ClientIdentityKeyVersion: 1, DeviceGrantVersion: 3,
		MaximumLifetimeSeconds: 0, ExpiresAt: remoteauth.PersistentDeviceLinkGrantExpiresAtUnix,
	}
	linkID := "persistent-link"
	link := &remotev2.LinkEnvelope{LinkId: linkID, Body: &remotev2.LinkEnvelope_LinkInit{LinkInit: &remotev2.LinkInit{
		GrantId: claims.GrantID, LinkId: linkID, ClientId: claims.ClientID, DeviceId: claims.DeviceID,
		RelayNodeId: claims.RelayNodeID, RelayCellId: claims.RelayCellID, TargetConnectionEpoch: claims.TargetConnectionEpoch,
		ClientIdentityKeyVersion: claims.ClientIdentityKeyVersion, DeviceConnectionGrant: "persistent",
	}}}
	if err := hub.routeClientLink(client, claims, link); err != nil {
		t.Fatal(err)
	}
	if route := hub.links[linkID]; route.targetEpoch != device.epoch {
		t.Fatalf("persistent Link target epoch = %d, want current %d", route.targetEpoch, device.epoch)
	}
	frame, err := deviceQueue.dequeue(context.Background())
	if err != nil || frame.envelope.GetLink().GetLinkInit() == nil {
		t.Fatalf("persistent LinkInit forwarding = %#v, %v", frame.envelope, err)
	}
}

func TestV2HubReconnectChurnKeepsRouteMemoryBounded(t *testing.T) {
	clientQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 16})
	if err != nil {
		t.Fatal(err)
	}
	deviceQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 16})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client-carrier", epoch: 2, role: v2CarrierClient, clientID: "controller", deviceID: "device", queue: clientQueue}
	device := &v2Carrier{id: "device-carrier", epoch: 7, role: v2CarrierDevice, deviceID: "device", queue: deviceQueue}
	hub := NewV2Hub()
	hub.carriers[client.id], hub.carriers[device.id] = client, device
	hub.deviceCarriers[device.deviceID] = device.id
	claims := remoteauth.DeviceLinkGrantClaims{
		GrantID: "grant", ClientID: client.clientID, DeviceID: device.deviceID,
		RelayNodeID: "node", RelayCellID: "cell", TargetConnectionEpoch: device.epoch,
		ClientIdentityKeyVersion: 1,
	}
	for index := range 2_000 {
		linkID := "link-" + strconv.Itoa(index)
		link := &remotev2.LinkEnvelope{LinkId: linkID, Body: &remotev2.LinkEnvelope_LinkInit{LinkInit: &remotev2.LinkInit{
			GrantId: claims.GrantID, LinkId: linkID, ClientId: claims.ClientID, DeviceId: claims.DeviceID,
			RelayNodeId: claims.RelayNodeID, RelayCellId: claims.RelayCellID, TargetConnectionEpoch: claims.TargetConnectionEpoch,
			ClientIdentityKeyVersion: claims.ClientIdentityKeyVersion, DeviceConnectionGrant: "transient",
		}}}
		if err := hub.routeClientLink(client, claims, link); err != nil {
			t.Fatalf("reconnect %d = %v", index, err)
		}
		frames := 1
		if index > 0 {
			frames = 2 // expired previous Link feedback, then the fresh LinkInit
		}
		for range frames {
			if _, err := deviceQueue.dequeue(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if got := hub.ActiveLinkRoutes(); got != 1 {
			t.Fatalf("reconnect %d retained %d Link routes", index, got)
		}
	}
	if snapshot := deviceQueue.snapshot(); snapshot.ControlFrames != 0 || snapshot.ControlBytes != 0 {
		t.Fatalf("reconnect churn retained queued data: %+v", snapshot)
	}
}

func TestV2HubPrunesDisconnectedClientAndNotifiesDevice(t *testing.T) {
	deviceQueue, err := newV2PriorityQueue(V2QueueLimits{Frames: 8})
	if err != nil {
		t.Fatal(err)
	}
	client := &v2Carrier{id: "client", role: v2CarrierClient, clientID: "controller", deviceID: "device"}
	device := &v2Carrier{id: "device", role: v2CarrierDevice, deviceID: "device", queue: deviceQueue}
	hub := NewV2Hub()
	hub.carriers[client.id], hub.carriers[device.id] = client, device
	hub.deviceCarriers[device.deviceID] = device.id
	hub.links["link"] = v2LinkRoute{linkID: "link", clientID: client.clientID, deviceID: device.deviceID, clientCarrierID: client.id}

	hub.detach(client)
	route := hub.links["link"]
	if route.clientSuspendedUntil.IsZero() {
		t.Fatal("client disconnect did not start bounded Link recovery grace")
	}
	route.clientSuspendedUntil = time.Now().Add(-time.Second)
	hub.links["link"] = route
	if got := hub.ActiveLinkRoutes(); got != 0 {
		t.Fatalf("expired Link routes = %d, want 0", got)
	}
	frame, err := deviceQueue.dequeue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rejected := frame.envelope.GetStreamRejected()
	if rejected == nil || rejected.GetLinkId() != "link" || rejected.GetChannelId() != "" || rejected.GetStreamId() != "" ||
		rejected.GetReason() != remotev2.ProtocolErrorCode_PROTOCOL_ERROR_CODE_RESUME_EXPIRED {
		t.Fatalf("expired Link feedback = %#v", frame.envelope)
	}
}

func TestV2HubCloseAllReleasesLinkRoutes(t *testing.T) {
	hub := NewV2Hub()
	hub.links["link"] = v2LinkRoute{linkID: "link"}
	hub.CloseAll()
	if got := hub.ActiveLinkRoutes(); got != 0 {
		t.Fatalf("Link routes after CloseAll = %d", got)
	}
}

func BenchmarkV2PriorityQueueRoundTrip(b *testing.B) {
	budget, err := NewV2QueueBudget(256<<20, 1_000_000)
	if err != nil {
		b.Fatal(err)
	}
	queue, err := newV2PriorityQueue(V2QueueLimits{Frames: 256}, budget)
	if err != nil {
		b.Fatal(err)
	}
	frame := &remotev2.CarrierEnvelope{Body: &remotev2.CarrierEnvelope_Ping{Ping: &remotev2.CarrierPing{MonotonicMillis: 1}}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := queue.enqueue(frame, v2PriorityControl); err != nil {
			b.Fatal(err)
		}
		if _, err := queue.dequeue(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
