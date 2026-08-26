package relayprotocol

import (
	"errors"
	"fmt"
)

const (
	ControlFrameLimit  = 64 << 10
	AbsoluteFrameLimit = 128 << 10
	FileChunkPlaintext = 64 << 10
)

var (
	ErrUnknownFrame  = errors.New("unknown relay frame")
	ErrFrameTooLarge = errors.New("relay frame exceeds limit")
)

type FrameClass uint8

const (
	FrameClassUnknown FrameClass = iota
	FrameClassHeartbeat
	FrameClassReliableControl
	FrameClassPeerPayload
	FrameClassFileControl
	FrameClassFilePayload
)

func ClassifyFrame(frameType string) FrameClass {
	switch frameType {
	case "AUTH_CHALLENGE", "AUTH_PROOF", "READY", "PING", "PONG", "GOAWAY":
		return FrameClassHeartbeat
	case "COMMAND", "COMMAND_RECEIVED", "COMMAND_ACCEPTED", "DEVICE_EVENT", "EVENT_TRANSPORT_ACK":
		return FrameClassReliableControl
	case "PEER_OPEN", "PEER_READY", "PEER_QUERY", "PEER_DELTA", "PEER_COMPLETE", "PEER_CANCEL", "PEER_ERROR":
		return FrameClassPeerPayload
	case "FILE_OPEN", "FILE_ACCEPT", "FILE_REJECT":
		return FrameClassFileControl
	case "FILE_MANIFEST", "FILE_CHUNK", "FILE_ACK", "FILE_WINDOW_UPDATE", "FILE_RESUME", "FILE_COMPLETE", "FILE_VERIFIED", "FILE_CANCEL", "FILE_ERROR":
		return FrameClassFilePayload
	default:
		return FrameClassUnknown
	}
}

func ValidateFrameSize(frameType string, encodedBytes int) error {
	class := ClassifyFrame(frameType)
	if class == FrameClassUnknown {
		return ErrUnknownFrame
	}
	if encodedBytes < 0 || encodedBytes > AbsoluteFrameLimit {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, encodedBytes, AbsoluteFrameLimit)
	}
	if class != FrameClassFilePayload && encodedBytes > ControlFrameLimit {
		return fmt.Errorf("%w: control frame %d > %d", ErrFrameTooLarge, encodedBytes, ControlFrameLimit)
	}
	return nil
}

// MayUsePersistentBus is intentionally false for all Peer and File frames.
func MayUsePersistentBus(frameType string) bool {
	return ClassifyFrame(frameType) == FrameClassReliableControl
}
