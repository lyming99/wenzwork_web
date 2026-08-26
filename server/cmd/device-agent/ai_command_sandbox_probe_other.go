//go:build !linux && !darwin && !windows

package main

import "os"

func probeAICommandSandboxRuntime() aiCommandSandboxRuntime {
	return aiCommandSandboxRuntime{TemporaryRoot: os.TempDir()}
}
