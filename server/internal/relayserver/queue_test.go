package relayserver

import (
	"context"
	"errors"
	"testing"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/proto"
)

func TestBoundedQueueEnforcesFrameAndByteLimits(t *testing.T) {
	queue, err := NewBoundedQueue(128<<10, 2)
	if err != nil {
		t.Fatal(err)
	}
	ping := func(sequence uint64) *remotev1.Envelope {
		return &remotev1.Envelope{ProtocolVersion: 1, Sequence: sequence, Frame: &remotev1.Envelope_Pong{Pong: &remotev1.Pong{}}}
	}
	if err := queue.Enqueue(ping(1)); err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ping(2)); err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ping(3)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third Enqueue() error = %v", err)
	}
	for expected := uint64(1); expected <= 2; expected++ {
		payload, err := queue.Dequeue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var envelope remotev1.Envelope
		if err := proto.Unmarshal(payload, &envelope); err != nil || envelope.GetSequence() != expected {
			t.Fatalf("dequeued sequence = %d, error = %v", envelope.GetSequence(), err)
		}
	}
	queue.Close()
	if _, err := queue.Dequeue(context.Background()); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("closed Dequeue() error = %v", err)
	}
}

func TestBoundedQueueRejectsOversizedControlFrame(t *testing.T) {
	queue, _ := NewBoundedQueue(DefaultQueueBytes, DefaultQueueFrames)
	envelope := &remotev1.Envelope{
		ProtocolVersion: 1,
		Frame:           &remotev1.Envelope_DeviceEvent{DeviceEvent: &remotev1.DeviceEvent{TypedPayload: make([]byte, 65<<10)}},
	}
	if err := queue.Enqueue(envelope); err == nil {
		t.Fatal("Enqueue accepted an oversized control frame")
	}
}

func TestDecodeNodeDeliveryEnforcesChannelAndEpoch(t *testing.T) {
	envelope := &remotev1.Envelope{
		ProtocolVersion: 1, ConnectionEpoch: 7,
		Frame: &remotev1.Envelope_CommandReceived{CommandReceived: &remotev1.CommandReceived{CommandId: "command-1"}},
	}
	payload, _ := proto.MarshalOptions{Deterministic: true}.Marshal(envelope)
	if _, err := DecodeNodeDelivery("downlink", payload, 7); err != nil {
		t.Fatalf("DecodeNodeDelivery() error = %v", err)
	}
	if _, err := DecodeNodeDelivery("peer", payload, 7); err == nil {
		t.Fatal("DecodeNodeDelivery accepted reliable control on peer channel")
	}
	if _, err := DecodeNodeDelivery("downlink", payload, 8); err == nil {
		t.Fatal("DecodeNodeDelivery accepted a stale connection epoch")
	}
}
