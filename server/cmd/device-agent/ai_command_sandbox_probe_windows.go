//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func probeAICommandSandboxRuntime() aiCommandSandboxRuntime {
	result := aiCommandSandboxRuntime{TemporaryRoot: os.TempDir()}
	self, err := os.Executable()
	if err != nil {
		return result
	}
	if strings.HasSuffix(strings.ToLower(filepath.Base(self)), ".test.exe") {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, self, aiCommandSandboxWindowsACLInternal, "--probe")
	configureBackgroundProcess(probe)
	if err := probe.Run(); err == nil && ctx.Err() == nil {
		result.WindowsACLRunner = self
	}
	return result
}
