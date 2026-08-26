//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func TestTaskRunnerProbeSupportsWindowsBatchShim(t *testing.T) {
	script := filepath.Join(t.TempDir(), "probe.cmd")
	if err := os.WriteFile(script, []byte("@echo off\r\necho probe:%~1\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := runTaskRunnerProbe(context.Background(), script, []string{"--version"})
	if err != nil || !strings.Contains(string(output), "probe:--version") {
		t.Fatalf("batch probe = %q, %v", output, err)
	}
}

func TestWindowsBatchTaskExecPreservesReviewedArguments(t *testing.T) {
	script := filepath.Join(t.TempDir(), "runner.cmd")
	contents := []byte("@echo off\r\nsetlocal DisableDelayedExpansion\r\n<nul set /p \"=[%~1][%~2][%~3]\"\r\nexit /b 0\r\n")
	if err := os.WriteFile(script, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := taskExecCommand(script, []string{"alpha & beta", "100%done", "bang!value"})
	if err != nil {
		t.Fatal(err)
	}
	if command.SysProcAttr == nil || !strings.Contains(command.SysProcAttr.CmdLine, windowsCmdUTF8Bootstrap+" &") {
		t.Fatalf("batch command did not initialize UTF-8: %#v", command.SysProcAttr)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("batch command failed: %v; stderr=%q", err, stderr.String())
	}
	if got, want := stdout.String(), "[alpha & beta][100%done][bang!value]"; got != want {
		t.Fatalf("batch arguments = %q, want %q", got, want)
	}
}

func TestWindowsBatchTaskExecPreservesPipedStdinForGrandchild(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "runner.cmd")
	contents := []byte("@echo off\r\n%SystemRoot%\\System32\\findstr.exe /r \".*\"\r\n")
	if err := os.WriteFile(script, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := taskExecCommand(script, nil)
	if err != nil {
		t.Fatal(err)
	}
	command.Stdin = strings.NewReader("piped-stdin-sentinel\n")
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("piped-stdin-sentinel")) {
		t.Fatalf("batch piped stdin: output=%q err=%v", output, err)
	}
}

func TestWindowsBatchTaskExecPreservesPrivateStdinForGrandchild(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "runner.cmd")
	contents := []byte("@echo off\r\n%SystemRoot%\\System32\\findstr.exe /r \".*\"\r\n")
	if err := os.WriteFile(script, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, "state", "agent-state.json")
	prompt, err := createManagedTaskPrompt(statePath, uuid.New(), uuid.New(), []byte("private-stdin-sentinel\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer prompt.Cleanup()

	supervisor := newRawProcessSupervisor()
	defer supervisor.Close()
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: []string{script}, PrivateStdinPath: prompt.Path,
		Limits: processResourceLimits{
			MaximumLifetime: time.Minute, MaximumMemoryBytes: 256 << 20, MaximumOutputBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer process.release()
	stdoutReady, stderrReady := make(chan []byte, 1), make(chan []byte, 1)
	go func() { output, _ := io.ReadAll(process.Stdout()); stdoutReady <- output }()
	go func() { output, _ := io.ReadAll(process.Stderr()); stderrReady <- output }()
	if exitCode := process.Wait(); exitCode != 0 {
		t.Fatalf("batch stdin runner exit = %d, stderr=%q", exitCode, <-stderrReady)
	}
	stdout, stderr := <-stdoutReady, <-stderrReady
	if !bytes.Contains(stdout, []byte("private-stdin-sentinel")) || len(stderr) != 0 {
		t.Fatalf("batch grandchild streams: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestInternalTaskExecPreservesPrivateStdinForBatchGrandchild(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "runner.cmd")
	contents := []byte("@echo off\r\n%SystemRoot%\\System32\\findstr.exe /r \".*\"\r\n")
	if err := os.WriteFile(script, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	prompt, err := createManagedTaskPrompt(
		filepath.Join(root, "state", "agent-state.json"), uuid.New(), uuid.New(), []byte("helper-stdin-sentinel\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer prompt.Cleanup()

	var stdout, stderr bytes.Buffer
	err = runInternalTaskExec([]string{"--stdin-file", prompt.Path, "--", script}, &stdout, &stderr)
	if err != nil || !bytes.Contains(stdout.Bytes(), []byte("helper-stdin-sentinel")) || stderr.Len() != 0 {
		t.Fatalf("task helper stdin: stdout=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
}

func TestRealCodexRunnerConsumesPrivateStdin(t *testing.T) {
	if os.Getenv("WENZWORK_REAL_CODEX_TEST") != "1" {
		t.Skip("set WENZWORK_REAL_CODEX_TEST=1 to run the installed Codex CLI")
	}
	registry := newTaskRunnerRegistry()
	capability, err := registry.probe(t.Context(), "codex", true)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	arguments, err := prepareCodexArguments(
		map[string]any{"reasoningEffort": "low"}, capability, "", root,
	)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := createManagedTaskPrompt(
		filepath.Join(root, "state", "agent-state.json"), uuid.New(), uuid.New(),
		[]byte("Reply with exactly WENZWORK_STDIN_OK. Do not use tools or modify files.\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer prompt.Cleanup()

	supervisor := newRawProcessSupervisor()
	defer supervisor.Close()
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: append([]string{capability.executable}, arguments...), PrivateStdinPath: prompt.Path,
		Limits: processResourceLimits{
			MaximumLifetime: 5 * time.Minute, MaximumMemoryBytes: 2 << 30, MaximumOutputBytes: 8 << 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer process.release()
	stdoutReady, stderrReady := make(chan []byte, 1), make(chan []byte, 1)
	go func() { output, _ := io.ReadAll(process.Stdout()); stdoutReady <- output }()
	go func() { output, _ := io.ReadAll(process.Stderr()); stderrReady <- output }()
	exitCode := process.Wait()
	stdout, stderr := <-stdoutReady, <-stderrReady
	combined := append(append([]byte(nil), stdout...), stderr...)
	if exitCode != 0 || bytes.Contains(combined, []byte("No prompt provided via stdin")) ||
		!bytes.Contains(combined, []byte("WENZWORK_STDIN_OK")) {
		t.Fatalf("real Codex stdin: exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
}

func TestWindowsBatchTaskExecRejectsQuoteAndControlInjection(t *testing.T) {
	for _, value := range []string{"quote\"value", "line\rbreak", "line\nbreak", "nul\x00value"} {
		if _, _, err := windowsBatchCommandLine(`C:\\runner.cmd`, []string{value}); err == nil {
			t.Fatalf("windowsBatchCommandLine accepted %q", value)
		}
	}
	line, environment, err := windowsBatchCommandLine(`C:\\runner.cmd`, []string{"value^with%tokens"})
	if err != nil || strings.Contains(line, "value^with%tokens") || !strings.Contains(line, "%WENZWORK_TASK_EXEC_ARG_001%") ||
		!slices.Contains(environment, "WENZWORK_TASK_EXEC_ARG_001=value^with%tokens") {
		t.Fatalf("indirect batch line/environment = %q, %#v, %v", line, environment, err)
	}
}

func TestWindowsBatchTaskExecAcceptsPreparedCodexArguments(t *testing.T) {
	arguments, err := prepareCodexArguments(
		map[string]any{"reasoningEffort": "medium"},
		taskRunnerCapability{Features: map[string]bool{"json": true}},
		"",
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := windowsBatchCommandLine(`C:\runner.cmd`, arguments); err != nil {
		t.Fatalf("prepared Codex arguments were rejected by the batch launcher: %v", err)
	}
}

func TestWindowsPowerShellTaskExecUsesNoBOMBootstrapAndIndirectArguments(t *testing.T) {
	command, environment, err := windowsPowerShellTaskCommand(`C:\workspace\runner.ps1`, []string{"alpha & beta"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[System.Text.UTF8Encoding]::new($false)",
		"$global:LASTEXITCODE = $null",
		"if (-not $__wenzworkSuccess)",
		"$env:WENZWORK_TASK_EXEC_ARG_000",
		"$env:WENZWORK_TASK_EXEC_ARG_001",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("PowerShell task command missing %q: %s", want, command)
		}
	}
	if strings.Contains(command, `C:\workspace\runner.ps1`) || strings.Contains(command, "-File") ||
		!slices.Contains(environment, `WENZWORK_TASK_EXEC_ARG_000=C:\workspace\runner.ps1`) ||
		!slices.Contains(environment, "WENZWORK_TASK_EXEC_ARG_001=alpha & beta") {
		t.Fatalf("PowerShell task command/environment = %q %#v", command, environment)
	}
}

func TestWindowsPowerShellTaskExecWritesUTF8WithoutBOMAndPreservesExitCode(t *testing.T) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		t.Skip("Windows PowerShell is unavailable")
	}
	script := filepath.Join(t.TempDir(), "runner.ps1")
	// Keep the script itself ASCII so PowerShell 5.1's script-source default
	// cannot influence this test. The process output must still be UTF-8.
	if err := os.WriteFile(script, []byte("Write-Output ([string]([char]0x4e2d) + [char]0x6587)\r\nexit 7\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := taskExecCommand(script, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("PowerShell exit = %v, stderr=%q", err, stderr.String())
	}
	if bytes.HasPrefix(stdout.Bytes(), []byte{0xef, 0xbb, 0xbf}) || !utf8.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), "中文") {
		t.Fatalf("PowerShell UTF-8 output = %x (%q)", stdout.Bytes(), stdout.String())
	}
}
