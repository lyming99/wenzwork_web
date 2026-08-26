//go:build linux || darwin

package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRawProcessSupervisorProvidesSafeRuntimeEnvironmentToUnixRunner(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	t.Setenv("HOME", profile)
	t.Setenv("OPENAI_API_KEY", "must-not-leak")
	script := filepath.Join(root, "runner")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env sh\nprintf '%s|%s' \"$HOME\" \"${OPENAI_API_KEY-unset}\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	supervisor := newRawProcessSupervisor()
	t.Cleanup(func() { _ = supervisor.Close() })
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: []string{script},
		Limits: processResourceLimits{
			MaximumLifetime: time.Minute, MaximumMemoryBytes: 256 << 20, MaximumOutputBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stdout, readErr := io.ReadAll(process.Stdout())
	stderr, stderrErr := io.ReadAll(process.Stderr())
	exitCode := process.Wait()
	process.release()
	if readErr != nil || stderrErr != nil || exitCode != 0 {
		t.Fatalf("Unix runner exit=%d stdout=%q stderr=%q errors=%v/%v", exitCode, stdout, stderr, readErr, stderrErr)
	}
	if got, want := string(stdout), profile+"|unset"; got != want {
		t.Fatalf("Unix runner environment = %q, want %q", got, want)
	}
}
