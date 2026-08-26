//go:build darwin

package main

import (
	"os"
	"os/exec"
)

func probeAICommandSandboxRuntime() aiCommandSandboxRuntime {
	result := aiCommandSandboxRuntime{TemporaryRoot: os.TempDir()}
	if seatbelt, err := exec.LookPath("sandbox-exec"); err == nil {
		result.Seatbelt = seatbelt
	}
	return result
}
