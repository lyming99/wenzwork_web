//go:build !windows && !linux && !darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"sync"
)

func configureRawProcessCommand(command *exec.Cmd) (func(), error) {
	if command == nil {
		return nil, errors.New("raw command is nil")
	}
	command.Env = mergeInteractiveCommandEnvironment(filterCommandRuntimeEnvironment(os.Environ()), command.Env)
	return func() {}, nil
}

func attachRawProcessTree(command *exec.Cmd) (rawProcessTree, error) {
	if command == nil || command.Process == nil {
		return nil, errors.New("raw process is not running")
	}
	return &directRawProcessTree{process: command.Process}, nil
}

type directRawProcessTree struct {
	process *os.Process
	once    sync.Once
	err     error
}

func (tree *directRawProcessTree) Close() error {
	if tree == nil || tree.process == nil {
		return nil
	}
	tree.once.Do(func() {
		if err := tree.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			tree.err = err
		}
	})
	return tree.err
}
