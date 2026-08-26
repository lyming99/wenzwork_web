package fileprotocol

import (
	"errors"
	"sync"
)

var (
	ErrWindowBlocked = errors.New("file receiver window is exhausted")
	ErrWindowInvalid = errors.New("file receiver window is invalid")
)

// SendWindow enforces end-to-end backpressure before a Sender reads another
// source chunk. Relays may have their own smaller bounded queues, but cannot
// expand this receiver-advertised limit.
type SendWindow struct {
	mu       sync.Mutex
	limit    uint64
	inFlight uint64
}

func NewSendWindow(limit uint64) (*SendWindow, error) {
	if limit == 0 || limit > 4<<20 {
		return nil, ErrWindowInvalid
	}
	return &SendWindow{limit: limit}, nil
}

func (w *SendWindow) Reserve(bytes uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes == 0 || bytes > w.limit || w.inFlight > w.limit-bytes {
		return ErrWindowBlocked
	}
	w.inFlight += bytes
	return nil
}

func (w *SendWindow) Acknowledge(bytes uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if bytes == 0 || bytes > w.inFlight {
		return ErrWindowInvalid
	}
	w.inFlight -= bytes
	return nil
}

func (w *SendWindow) Update(limit uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if limit > 4<<20 {
		return ErrWindowInvalid
	}
	w.limit = limit
	return nil
}

func (w *SendWindow) Available() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight >= w.limit {
		return 0
	}
	return w.limit - w.inFlight
}
