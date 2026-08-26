//go:build linux

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
	if bubblewrap, err := exec.LookPath("bwrap"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		probe := exec.CommandContext(ctx, bubblewrap,
			"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent", "--unshare-net", "--", "/bin/true")
		probe.Stdin, probe.Stdout, probe.Stderr = nil, nil, nil
		err = probe.Run()
		cancel()
		if err == nil && ctx.Err() == nil {
			result.Bubblewrap = bubblewrap
			return result
		}
	}

	self, err := os.Executable()
	if err != nil {
		return result
	}
	if strings.HasSuffix(filepath.Base(self), ".test") {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probe := exec.CommandContext(ctx, self, aiCommandSandboxLandlockInternal, "--probe")
	output, err := probe.Output()
	if err != nil || ctx.Err() != nil {
		return result
	}
	normalized := strings.ToLower(strings.TrimSpace(string(output)))
	switch {
	case strings.Contains(normalized, "fully enforced"):
		result.LandlockRunner = self
		result.LandlockEnforcement = aiCommandSandboxEnforcementFull
	case strings.Contains(normalized, "partially enforced"):
		result.LandlockRunner = self
		result.LandlockEnforcement = aiCommandSandboxEnforcementPartial
	}
	return result
}
