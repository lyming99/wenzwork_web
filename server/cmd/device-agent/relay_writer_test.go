package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type recordingRelaySocketWriter struct {
	mu      sync.Mutex
	writes  []string
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (writer *recordingRelaySocketWriter) Write(ctx context.Context, _ websocket.MessageType, payload []byte) error {
	writer.once.Do(func() { close(writer.started) })
	if writer.release != nil {
		select {
		case <-writer.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		writer.release = nil
	}
	writer.mu.Lock()
	writer.writes = append(writer.writes, string(payload))
	writer.mu.Unlock()
	return nil
}

func (writer *recordingRelaySocketWriter) recordedWrites() []string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return append([]string(nil), writer.writes...)
}

func TestRelayWriterPrioritizesControlOverQueuedBulk(t *testing.T) {
	release := make(chan struct{})
	socket := &recordingRelaySocketWriter{started: make(chan struct{}), release: release}
	writer := newRelayWriteScheduler(socket)
	defer writer.close()

	first := enqueueRelayWrite(writer, "bulk-1", relayWriteBulk)
	select {
	case <-socket.started:
	case <-time.After(time.Second):
		t.Fatal("first bulk frame did not reach writer")
	}
	secondBulk := enqueueRelayWrite(writer, "bulk-2", relayWriteBulk)
	terminal := enqueueRelayWrite(writer, "complete", relayWriteTerminal)
	control := enqueueRelayWrite(writer, "pong", relayWriteControl)
	waitForRelayWriterDepth(t, writer, 1, 1, 1)

	close(release)
	for _, result := range []<-chan error{first, secondBulk, terminal, control} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("writer result = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("queued writer request did not complete")
		}
	}

	got := socket.recordedWrites()
	want := []string{"bulk-1", "pong", "complete", "bulk-2"}
	if len(got) != len(want) {
		t.Fatalf("writes = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("writes = %#v, want %#v", got, want)
		}
	}
}

func TestRelayWriterReservesControlCapacityWhenBulkIsFull(t *testing.T) {
	release := make(chan struct{})
	socket := &recordingRelaySocketWriter{started: make(chan struct{}), release: release}
	writer := newRelayWriteScheduler(socket)
	defer writer.close()

	first := enqueueRelayWrite(writer, "first", relayWriteBulk)
	select {
	case <-socket.started:
	case <-time.After(time.Second):
		t.Fatal("first bulk frame did not reach writer")
	}
	queuedBulk := make([]<-chan error, 0, maximumRelayBulkQueue)
	for index := 0; index < maximumRelayBulkQueue; index++ {
		queuedBulk = append(queuedBulk, enqueueRelayWrite(writer, "bulk", relayWriteBulk))
	}
	waitForRelayWriterDepth(t, writer, 0, 0, maximumRelayBulkQueue)
	if err := writer.enqueue(context.Background(), []byte("overflow"), relayWriteBulk); err != errRelayWriterBackpressure {
		t.Fatalf("bulk overflow = %v, want %v", err, errRelayWriterBackpressure)
	}
	control := enqueueRelayWrite(writer, "pong", relayWriteControl)
	waitForRelayWriterDepth(t, writer, 1, 0, maximumRelayBulkQueue)

	close(release)
	for _, result := range append([]<-chan error{first, control}, queuedBulk...) {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("writer result = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("queued writer request did not complete")
		}
	}
	if got := socket.recordedWrites(); len(got) != maximumRelayBulkQueue+2 || got[1] != "pong" {
		t.Fatalf("writes = %d/%#v, control was not admitted ahead of bulk", len(got), got[:min(len(got), 3)])
	}
}

func enqueueRelayWrite(writer *relayWriteScheduler, payload string, priority relayWritePriority) <-chan error {
	result := make(chan error, 1)
	go func() { result <- writer.enqueue(context.Background(), []byte(payload), priority) }()
	return result
}

func waitForRelayWriterDepth(t *testing.T, writer *relayWriteScheduler, control, terminal, bulk int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(writer.controlInbox) == control && len(writer.terminalInbox) == terminal && len(writer.bulkInbox) == bulk {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("writer queue depth = %d/%d/%d, want %d/%d/%d", len(writer.controlInbox), len(writer.terminalInbox), len(writer.bulkInbox), control, terminal, bulk)
}
