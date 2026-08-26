//go:build !windows

package main

import "errors"

type agentInstanceLock struct{}

func acquireAgentInstanceLock(statePath string) (*agentInstanceLock, error) {
	if statePath == "" {
		return nil, errors.New("device state path is required")
	}
	return &agentInstanceLock{}, nil
}

func (lock *agentInstanceLock) Close() error { return nil }
