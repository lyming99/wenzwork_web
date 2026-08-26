//go:build !windows

package main

import "os/exec"

func taskExecCommand(executable string, arguments []string) (*exec.Cmd, error) {
	return exec.Command(executable, arguments...), nil
}
