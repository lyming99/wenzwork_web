package relayserver

import (
	"context"
	"errors"
	"sync"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/relayprotocol"
	"google.golang.org/protobuf/proto"
)

const (
	DefaultQueueBytes  = 4 << 20
	DefaultQueueFrames = 256
)

var (
	ErrQueueFull   = errors.New("Relay outbound queue is full")
	ErrQueueClosed = errors.New("Relay outbound queue is closed")
)

type queuedFrame struct {
	payload []byte
	class   relayprotocol.FrameClass
}

type BoundedQueue struct {
	mu        sync.Mutex
	queues    map[relayprotocol.FrameClass][]queuedFrame
	notify    chan struct{}
	closed    bool
	bytes     int
	frames    int
	maxBytes  int
	maxFrames int
	cursor    int
}

var fairClassOrder = []relayprotocol.FrameClass{
	relayprotocol.FrameClassHeartbeat,
	relayprotocol.FrameClassReliableControl,
	relayprotocol.FrameClassHeartbeat,
	relayprotocol.FrameClassPeerPayload,
	relayprotocol.FrameClassFileControl,
	relayprotocol.FrameClassFilePayload,
}

func NewBoundedQueue(maxBytes, maxFrames int) (*BoundedQueue, error) {
	if maxBytes < relayprotocol.AbsoluteFrameLimit || maxBytes > 64<<20 || maxFrames < 1 || maxFrames > 4096 {
		return nil, errors.New("Relay queue limits are invalid")
	}
	return &BoundedQueue{
		queues: make(map[relayprotocol.FrameClass][]queuedFrame), notify: make(chan struct{}, 1),
		maxBytes: maxBytes, maxFrames: maxFrames,
	}, nil
}

func (queue *BoundedQueue) Enqueue(envelope *remotev1.Envelope) error {
	if envelope == nil {
		return relayprotocol.ErrUnknownFrame
	}
	frameName, class := envelopeClass(envelope)
	if class == relayprotocol.FrameClassUnknown {
		return relayprotocol.ErrUnknownFrame
	}
	payload, err := proto.Marshal(envelope)
	if err != nil {
		return err
	}
	if err := relayprotocol.ValidateFrameSize(frameName, len(payload)); err != nil {
		return err
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed {
		return ErrQueueClosed
	}
	if queue.frames+1 > queue.maxFrames || queue.bytes+len(payload) > queue.maxBytes {
		return ErrQueueFull
	}
	queue.queues[class] = append(queue.queues[class], queuedFrame{payload: payload, class: class})
	queue.frames++
	queue.bytes += len(payload)
	select {
	case queue.notify <- struct{}{}:
	default:
	}
	return nil
}

func DecodeNodeDelivery(channel string, payload []byte, expectedEpoch uint64) (*remotev1.Envelope, error) {
	if len(payload) == 0 || len(payload) > relayprotocol.AbsoluteFrameLimit || expectedEpoch == 0 {
		return nil, relayprotocol.ErrFrameTooLarge
	}
	envelope := new(remotev1.Envelope)
	if err := proto.Unmarshal(payload, envelope); err != nil || len(envelope.ProtoReflect().GetUnknown()) != 0 {
		return nil, relayprotocol.ErrUnknownFrame
	}
	frameName, class := envelopeClass(envelope)
	if envelope.GetProtocolVersion() != 1 || envelope.GetConnectionEpoch() != expectedEpoch || relayprotocol.ValidateFrameSize(frameName, len(payload)) != nil {
		return nil, relayprotocol.ErrUnknownFrame
	}
	allowed := (channel == "downlink" && class == relayprotocol.FrameClassReliableControl) ||
		(channel == "peer" && class == relayprotocol.FrameClassPeerPayload) ||
		(channel == "file-control" && class == relayprotocol.FrameClassFileControl)
	if !allowed {
		return nil, relayprotocol.ErrUnknownFrame
	}
	return envelope, nil
}

func (queue *BoundedQueue) Dequeue(ctx context.Context) ([]byte, error) {
	for {
		queue.mu.Lock()
		if frame, ok := queue.nextLocked(); ok {
			queue.mu.Unlock()
			return frame.payload, nil
		}
		closed := queue.closed
		queue.mu.Unlock()
		if closed {
			return nil, ErrQueueClosed
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-queue.notify:
		}
	}
}

func (queue *BoundedQueue) Close() {
	queue.mu.Lock()
	queue.closed = true
	queue.mu.Unlock()
	select {
	case queue.notify <- struct{}{}:
	default:
	}
}

func (queue *BoundedQueue) Usage() (frames, bytes int) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.frames, queue.bytes
}

func (queue *BoundedQueue) nextLocked() (queuedFrame, bool) {
	for range len(fairClassOrder) {
		class := fairClassOrder[queue.cursor%len(fairClassOrder)]
		queue.cursor = (queue.cursor + 1) % len(fairClassOrder)
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
		queue.frames--
		queue.bytes -= len(frame.payload)
		if queue.frames > 0 {
			select {
			case queue.notify <- struct{}{}:
			default:
			}
		}
		return frame, true
	}
	return queuedFrame{}, false
}

func envelopeClass(envelope *remotev1.Envelope) (string, relayprotocol.FrameClass) {
	switch envelope.Frame.(type) {
	case *remotev1.Envelope_AuthChallenge:
		return "AUTH_CHALLENGE", relayprotocol.FrameClassHeartbeat
	case *remotev1.Envelope_AuthProof:
		return "AUTH_PROOF", relayprotocol.FrameClassHeartbeat
	case *remotev1.Envelope_Ready:
		return "READY", relayprotocol.FrameClassHeartbeat
	case *remotev1.Envelope_Ping:
		return "PING", relayprotocol.FrameClassHeartbeat
	case *remotev1.Envelope_Pong:
		return "PONG", relayprotocol.FrameClassHeartbeat
	case *remotev1.Envelope_GoAway:
		return "GOAWAY", relayprotocol.FrameClassHeartbeat
	case *remotev1.Envelope_Command:
		return "COMMAND", relayprotocol.FrameClassReliableControl
	case *remotev1.Envelope_CommandReceived:
		return "COMMAND_RECEIVED", relayprotocol.FrameClassReliableControl
	case *remotev1.Envelope_CommandAccepted:
		return "COMMAND_ACCEPTED", relayprotocol.FrameClassReliableControl
	case *remotev1.Envelope_DeviceEvent:
		return "DEVICE_EVENT", relayprotocol.FrameClassReliableControl
	case *remotev1.Envelope_EventTransportAck:
		return "EVENT_TRANSPORT_ACK", relayprotocol.FrameClassReliableControl
	case *remotev1.Envelope_PeerOpen:
		return "PEER_OPEN", relayprotocol.FrameClassPeerPayload
	case *remotev1.Envelope_PeerReady:
		return "PEER_READY", relayprotocol.FrameClassPeerPayload
	case *remotev1.Envelope_PeerQuery:
		return "PEER_QUERY", relayprotocol.FrameClassPeerPayload
	case *remotev1.Envelope_PeerDelta:
		return "PEER_DELTA", relayprotocol.FrameClassPeerPayload
	case *remotev1.Envelope_PeerComplete:
		return "PEER_COMPLETE", relayprotocol.FrameClassPeerPayload
	case *remotev1.Envelope_PeerCancel:
		return "PEER_CANCEL", relayprotocol.FrameClassPeerPayload
	case *remotev1.Envelope_PeerError:
		return "PEER_ERROR", relayprotocol.FrameClassPeerPayload
	case *remotev1.Envelope_FileOpen:
		return "FILE_OPEN", relayprotocol.FrameClassFileControl
	case *remotev1.Envelope_FileAccept:
		return "FILE_ACCEPT", relayprotocol.FrameClassFileControl
	case *remotev1.Envelope_FileReject:
		return "FILE_REJECT", relayprotocol.FrameClassFileControl
	case *remotev1.Envelope_FileManifest:
		return "FILE_MANIFEST", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileChunk:
		return "FILE_CHUNK", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileAck:
		return "FILE_ACK", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileWindowUpdate:
		return "FILE_WINDOW_UPDATE", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileResume:
		return "FILE_RESUME", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileComplete:
		return "FILE_COMPLETE", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileVerified:
		return "FILE_VERIFIED", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileCancel:
		return "FILE_CANCEL", relayprotocol.FrameClassFilePayload
	case *remotev1.Envelope_FileError:
		return "FILE_ERROR", relayprotocol.FrameClassFilePayload
	default:
		return "", relayprotocol.FrameClassUnknown
	}
}
