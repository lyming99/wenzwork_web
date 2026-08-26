package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coder/websocket"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

const (
	maximumRelayWriterQueue       = 256
	maximumRelayControlQueue      = 32
	maximumRelayTerminalQueue     = 64
	maximumRelayBulkQueue         = maximumRelayWriterQueue - maximumRelayControlQueue - maximumRelayTerminalQueue
	maximumControlWriteBurst      = 8
	maximumRelayWriterIngestBurst = 8
)

var (
	errRelayWriterBackpressure = errors.New("Relay writer queue is full")
	errRelayWriterClosed       = errors.New("Relay writer is closed")
)

// relaySocketWriter is deliberately small so the priority scheduler can be
// exercised without a live WebSocket. *websocket.Conn satisfies it directly.
type relaySocketWriter interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

type relayWritePriority uint8

const (
	relayWriteBulk relayWritePriority = iota
	relayWriteTerminal
	relayWriteControl
)

type relayWriteRequest struct {
	ctx      context.Context
	payload  []byte
	priority relayWritePriority
	result   chan error
}

// relayWriteScheduler is the only production owner of websocket.Write after a
// target Relay connection has completed its handshake. Producers prepare and
// validate frames before enqueueing them, then wait only for their own bounded
// write request. This keeps encryption sequencing session-local while giving
// control/terminal frames a path around high-volume deltas.
type relayWriteScheduler struct {
	socket        relaySocketWriter
	controlInbox  chan relayWriteRequest
	terminalInbox chan relayWriteRequest
	bulkInbox     chan relayWriteRequest
	done          chan struct{}

	gate   sync.RWMutex
	closed bool
	once   sync.Once
}

func newRelayWriteScheduler(socket relaySocketWriter) *relayWriteScheduler {
	writer := &relayWriteScheduler{
		socket:        socket,
		controlInbox:  make(chan relayWriteRequest, maximumRelayControlQueue),
		terminalInbox: make(chan relayWriteRequest, maximumRelayTerminalQueue),
		bulkInbox:     make(chan relayWriteRequest, maximumRelayBulkQueue),
		done:          make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (writer *relayWriteScheduler) enqueue(ctx context.Context, payload []byte, priority relayWritePriority) error {
	if writer == nil || writer.socket == nil {
		return errRelayWriterClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	request := relayWriteRequest{
		ctx: ctx, payload: append([]byte(nil), payload...), priority: priority, result: make(chan error, 1),
	}

	// Do not allow a slow consumer to hold an arbitrary number of producers.
	// The connection writer intentionally has a hard frame-count boundary; a
	// caller can then retire only its affected query/session instead of adding
	// unbounded memory pressure to the physical connection.
	writer.gate.RLock()
	if writer.closed {
		writer.gate.RUnlock()
		return errRelayWriterClosed
	}
	inbox := writer.bulkInbox
	switch priority {
	case relayWriteControl:
		inbox = writer.controlInbox
	case relayWriteTerminal:
		inbox = writer.terminalInbox
	}
	select {
	case inbox <- request:
		writer.gate.RUnlock()
	case <-ctx.Done():
		writer.gate.RUnlock()
		return ctx.Err()
	default:
		writer.gate.RUnlock()
		return errRelayWriterBackpressure
	}

	select {
	case err := <-request.result:
		return err
	case <-writer.done:
		return errRelayWriterClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (writer *relayWriteScheduler) close() {
	if writer == nil {
		return
	}
	writer.once.Do(func() {
		writer.gate.Lock()
		writer.closed = true
		close(writer.done)
		writer.gate.Unlock()
	})
}

func (writer *relayWriteScheduler) run() {
	var control, terminal, bulk []relayWriteRequest
	controlBurst := 0
	ingestBurst := 0
	preferBulk := false

	for {
		if len(control) == 0 && len(terminal) == 0 && len(bulk) == 0 {
			if request, ok := writer.takeQueuedInput(); ok {
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst = 1
				continue
			}
			select {
			case <-writer.done:
				return
			case request := <-writer.controlInbox:
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst = 1
			case request := <-writer.terminalInbox:
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst = 1
			case request := <-writer.bulkInbox:
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst = 1
			}
			continue
		}

		// Pull already-ready producers in before selecting the next frame, so a
		// newly enqueued Pong or completion is not hidden behind bulk work.
		if ingestBurst < maximumRelayWriterIngestBurst {
			if request, ok := writer.takeQueuedInput(); ok {
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst++
				continue
			}
			select {
			case <-writer.done:
				return
			case request := <-writer.controlInbox:
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst++
				continue
			case request := <-writer.terminalInbox:
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst++
				continue
			case request := <-writer.bulkInbox:
				control, terminal, bulk = appendRelayWriteRequest(request, control, terminal, bulk)
				ingestBurst++
				continue
			default:
			}
		}

		var request relayWriteRequest
		request, control, terminal, bulk, controlBurst, preferBulk = nextRelayWriteRequest(
			control, terminal, bulk, controlBurst, preferBulk,
		)
		ingestBurst = 0
		if err := request.ctx.Err(); err != nil {
			deliverRelayWriteResult(request, err)
			continue
		}
		if err := writer.socket.Write(request.ctx, websocket.MessageBinary, request.payload); err != nil {
			deliverRelayWriteResult(request, fmt.Errorf("%w: %v", errRelaySocketWrite, err))
			writer.close()
			return
		}
		deliverRelayWriteResult(request, nil)
	}
}

func (writer *relayWriteScheduler) takeQueuedInput() (relayWriteRequest, bool) {
	if writer == nil {
		return relayWriteRequest{}, false
	}
	select {
	case request := <-writer.controlInbox:
		return request, true
	default:
	}
	select {
	case request := <-writer.terminalInbox:
		return request, true
	default:
	}
	select {
	case request := <-writer.bulkInbox:
		return request, true
	default:
	}
	return relayWriteRequest{}, false
}

func appendRelayWriteRequest(request relayWriteRequest, control, terminal, bulk []relayWriteRequest) ([]relayWriteRequest, []relayWriteRequest, []relayWriteRequest) {
	switch request.priority {
	case relayWriteControl:
		return append(control, request), terminal, bulk
	case relayWriteTerminal:
		return control, append(terminal, request), bulk
	default:
		return control, terminal, append(bulk, request)
	}
}

func nextRelayWriteRequest(control, terminal, bulk []relayWriteRequest, controlBurst int, preferBulk bool) (relayWriteRequest, []relayWriteRequest, []relayWriteRequest, []relayWriteRequest, int, bool) {
	if len(control) > 0 && (controlBurst < maximumControlWriteBurst || (len(terminal) == 0 && len(bulk) == 0)) {
		return control[0], control[1:], terminal, bulk, controlBurst + 1, preferBulk
	}
	controlBurst = 0
	if len(terminal) > 0 && len(bulk) > 0 {
		if preferBulk {
			return bulk[0], control, terminal, bulk[1:], controlBurst, false
		}
		return terminal[0], control, terminal[1:], bulk, controlBurst, true
	}
	if len(terminal) > 0 {
		return terminal[0], control, terminal[1:], bulk, controlBurst, true
	}
	return bulk[0], control, terminal, bulk[1:], controlBurst, false
}

func deliverRelayWriteResult(request relayWriteRequest, err error) {
	select {
	case request.result <- err:
	default:
	}
}

func relayWritePriorityForEnvelope(envelope *remotev1.Envelope) relayWritePriority {
	if envelope == nil {
		return relayWriteTerminal
	}
	switch {
	case envelope.GetPing() != nil,
		envelope.GetPong() != nil,
		envelope.GetGoAway() != nil,
		envelope.GetPeerCancel() != nil,
		envelope.GetPeerError() != nil,
		envelope.GetAuthProof() != nil:
		return relayWriteControl
	case envelope.GetPeerDelta() != nil:
		return relayWriteBulk
	default:
		// Ready, open acknowledgement and RPC completion are all terminal
		// frames. They should pass deltas but still share the fairness budget.
		return relayWriteTerminal
	}
}
