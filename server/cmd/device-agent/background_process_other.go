//go:build !windows

package main

import "os/exec"

func configureBackgroundProcess(_ *exec.Cmd) {}
