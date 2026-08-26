//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAIWindowsACLRunnerEnforcesReadOnlyAndWorkspaceWrite(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	insideFile := filepath.Join(workspace, "inside.txt")
	outsideFile := filepath.Join(outside, "outside.txt")

	readOnlyScript := "try { Set-Content -LiteralPath " + powershellAIWindowsLiteral(insideFile) + " -Value denied -ErrorAction Stop; exit 91 } catch { exit 0 }"
	exitCode, err := runAIWindowsSandbox(aiWindowsSandboxOptions{
		workspace: workspace, workingDirectory: workspace, mode: aiWorkspaceModeReadOnly,
		cpuSeconds: 30, memoryBytes: 256 << 20, maxProcesses: 8,
		command: []string{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", readOnlyScript},
	})
	if err != nil || exitCode != 0 {
		t.Fatalf("read-only exit=%d error=%v", exitCode, err)
	}
	if _, err := os.Stat(insideFile); !os.IsNotExist(err) {
		t.Fatalf("read-only unexpectedly created workspace file: %v", err)
	}

	workspaceWriteScript := "$ErrorActionPreference='Stop'; Set-Content -LiteralPath " + powershellAIWindowsLiteral(insideFile) +
		" -Value allowed; try { Set-Content -LiteralPath " + powershellAIWindowsLiteral(outsideFile) +
		" -Value denied -ErrorAction Stop; exit 91 } catch { exit 0 }"
	exitCode, err = runAIWindowsSandbox(aiWindowsSandboxOptions{
		workspace: workspace, workingDirectory: workspace, mode: aiWorkspaceModeWorkspaceWrite,
		cpuSeconds: 30, memoryBytes: 256 << 20, maxProcesses: 8,
		command: []string{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", workspaceWriteScript},
	})
	if err != nil || exitCode != 0 {
		t.Fatalf("workspace-write exit=%d error=%v", exitCode, err)
	}
	if _, err := os.Stat(insideFile); err != nil {
		t.Fatalf("workspace-write did not create workspace file: %v", err)
	}
	if _, err := os.Stat(outsideFile); !os.IsNotExist(err) {
		t.Fatalf("workspace-write unexpectedly created external file: %v", err)
	}
}

func powershellAIWindowsLiteral(value string) string {
	result := "'"
	for _, character := range value {
		if character == '\'' {
			result += "''"
		} else {
			result += string(character)
		}
	}
	return result + "'"
}
