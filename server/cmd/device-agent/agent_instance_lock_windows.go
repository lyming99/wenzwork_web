//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var errAgentAlreadyRunning = errors.New("device agent is already running for this state file")

type agentInstanceLock struct {
	file *os.File
}

func acquireAgentInstanceLock(statePath string) (*agentInstanceLock, error) {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return nil, errors.New("device state path is required")
	}
	lockPath := filepath.Clean(statePath) + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open device agent lock: %w", err)
	}
	lock := &agentInstanceLock{file: file}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errAgentAlreadyRunning
		}
		return nil, fmt.Errorf("lock device agent state: %w", err)
	}
	return lock, nil
}

func (lock *agentInstanceLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	var overlapped windows.Overlapped
	unlockErr := windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &overlapped)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
