//go:build !windows && !linux && !darwin

package main

import "os/exec"

func lookupCommandExecutable(name string) (string, error) { return exec.LookPath(name) }
