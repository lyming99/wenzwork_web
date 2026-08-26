package fileprotocol

import (
	"errors"
	"testing"
)

func TestSendWindowStopsAtZeroAndResumesAfterDurableAck(t *testing.T) {
	window, err := NewSendWindow(2 * DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := window.Reserve(DefaultChunkSize); err != nil {
		t.Fatal(err)
	}
	if err := window.Reserve(DefaultChunkSize); err != nil {
		t.Fatal(err)
	}
	if window.Available() != 0 {
		t.Fatalf("available = %d", window.Available())
	}
	if err := window.Reserve(1); !errors.Is(err, ErrWindowBlocked) {
		t.Fatalf("Reserve() error = %v", err)
	}
	if err := window.Acknowledge(DefaultChunkSize); err != nil {
		t.Fatal(err)
	}
	if window.Available() != DefaultChunkSize {
		t.Fatalf("available after ACK = %d", window.Available())
	}
	if err := window.Update(0); err != nil {
		t.Fatal(err)
	}
	if window.Available() != 0 || !errors.Is(window.Reserve(1), ErrWindowBlocked) {
		t.Fatal("zero window did not stop sender")
	}
}
