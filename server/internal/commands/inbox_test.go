package commands

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInboxExecutesOneSideEffectForOneHundredDeliveries(t *testing.T) {
	inbox := NewInbox()
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	var sideEffects atomic.Int64
	operation := func(context.Context) ([]byte, error) {
		sideEffects.Add(1)
		time.Sleep(time.Millisecond)
		return []byte("task-created"), nil
	}
	var waitGroup sync.WaitGroup
	errorsChannel := make(chan error, 100)
	duplicateCount := atomic.Int64{}
	for range 100 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			result, duplicate, err := inbox.Execute(context.Background(), "command-1", now.Add(time.Minute), now, operation)
			if err != nil {
				errorsChannel <- err
				return
			}
			if duplicate {
				duplicateCount.Add(1)
			}
			if string(result.Payload) != "task-created" {
				errorsChannel <- errors.New("unexpected stored payload")
			}
		}()
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
	if sideEffects.Load() != 1 || duplicateCount.Load() != 99 {
		t.Fatalf("side effects=%d duplicates=%d", sideEffects.Load(), duplicateCount.Load())
	}
}

func TestInboxRejectsExpiredCommandBeforePersistence(t *testing.T) {
	inbox := NewInbox()
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	_, _, err := inbox.Execute(context.Background(), "command-1", now, now, func(context.Context) ([]byte, error) {
		t.Fatal("expired operation executed")
		return nil, nil
	})
	if !errors.Is(err, ErrCommandExpired) {
		t.Fatalf("Execute() error = %v", err)
	}
}
