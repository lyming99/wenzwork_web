//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

func configureRawProcessCommand(command *exec.Cmd) (func(), error) {
	if command == nil {
		return nil, errors.New("raw command is nil")
	}
	command.Env = mergeInteractiveCommandEnvironment(filterCommandRuntimeEnvironment(os.Environ()), command.Env)
	attributes := syscall.SysProcAttr{Setpgid: true}
	if command.SysProcAttr != nil {
		attributes = *command.SysProcAttr
		attributes.Setpgid = true
	}
	command.SysProcAttr = &attributes
	return func() {}, nil
}

func attachRawProcessTree(command *exec.Cmd) (rawProcessTree, error) {
	if command == nil || command.Process == nil || command.Process.Pid < 1 {
		return nil, errors.New("raw process is not running")
	}
	return &unixRawProcessTree{pid: command.Process.Pid}, nil
}

type unixRawProcessTree struct {
	pid  int
	once sync.Once
	err  error
}

func (tree *unixRawProcessTree) Close() error {
	if tree == nil || tree.pid < 1 {
		return nil
	}
	tree.once.Do(func() {
		// A negative PID targets the process group set before exec, so children
		// cannot outlive a timeout, cancellation, or Agent shutdown.
		if err := syscall.Kill(-tree.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			tree.err = err
		}
	})
	return tree.err
}
