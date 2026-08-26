package commands

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrCommandExpired = errors.New("command is expired")
	ErrCommandInvalid = errors.New("command ID is required")
)

type Result struct {
	Payload []byte
	Digest  [sha256.Size]byte
	Error   string
}

type inboxRecord struct {
	done   chan struct{}
	result Result
}

// Inbox is the executable POC contract for the Agent's persistent SQLite
// command_inbox. A production Store must persist the record before returning
// COMMAND_ACCEPTED; this implementation models the same concurrency behavior.
type Inbox struct {
	mu      sync.Mutex
	records map[string]*inboxRecord
}

func NewInbox() *Inbox {
	return &Inbox{records: make(map[string]*inboxRecord)}
}

func (i *Inbox) Execute(ctx context.Context, commandID string, expiresAt, now time.Time, operation func(context.Context) ([]byte, error)) (Result, bool, error) {
	if commandID == "" || operation == nil {
		return Result{}, false, ErrCommandInvalid
	}
	if !now.Before(expiresAt) {
		return Result{}, false, ErrCommandExpired
	}

	i.mu.Lock()
	if existing, ok := i.records[commandID]; ok {
		done := existing.done
		i.mu.Unlock()
		select {
		case <-done:
			return cloneResult(existing.result), true, nil
		case <-ctx.Done():
			return Result{}, true, ctx.Err()
		}
	}
	record := &inboxRecord{done: make(chan struct{})}
	i.records[commandID] = record
	i.mu.Unlock()

	payload, operationErr := operation(ctx)
	result := Result{Payload: append([]byte(nil), payload...), Digest: sha256.Sum256(payload)}
	if operationErr != nil {
		result.Error = operationErr.Error()
	}

	i.mu.Lock()
	record.result = result
	close(record.done)
	i.mu.Unlock()
	return cloneResult(result), false, nil
}

func (i *Inbox) Result(commandID string) (Result, bool) {
	i.mu.Lock()
	record, ok := i.records[commandID]
	if !ok {
		i.mu.Unlock()
		return Result{}, false
	}
	done := record.done
	i.mu.Unlock()
	select {
	case <-done:
		return cloneResult(record.result), true
	default:
		return Result{}, false
	}
}

func cloneResult(result Result) Result {
	result.Payload = append([]byte(nil), result.Payload...)
	return result
}

func (r Result) Err() error {
	if r.Error == "" {
		return nil
	}
	return fmt.Errorf("stored command result: %s", r.Error)
}
