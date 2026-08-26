//go:build windows

package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAgentInstanceLockRejectsSecondServeForSameState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "agent-state.json")
	first, err := acquireAgentInstanceLock(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := acquireAgentInstanceLock(statePath)
	if second != nil || !errors.Is(err, errAgentAlreadyRunning) {
		t.Fatalf("second lock/error = %#v / %v", second, err)
	}
}
