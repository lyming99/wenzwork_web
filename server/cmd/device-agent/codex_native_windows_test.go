//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeNativeCodexTestFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindWindowsNativeCodexExecutableFindsBundledBinary(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "codex.cmd")
	writeNativeCodexTestFile(t, shim, []byte("@echo off\r\n"))
	native := filepath.Join(root, "node_modules", "@openai", "codex", "vendor", "win", "codex.exe")
	writeNativeCodexTestFile(t, native, []byte("native"))

	got, err := findWindowsNativeCodexExecutable(shim)
	if err != nil {
		t.Fatal(err)
	}
	if got != native {
		t.Fatalf("native executable = %q, want %q", got, native)
	}
}

func TestCodexExecutableForNativeModeUsesDirectExecutable(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "codex.exe")
	writeNativeCodexTestFile(t, native, []byte("native"))
	got, err := codexExecutableForLaunchMode(map[string]any{"launchMode": "windowsNativeExe"}, native)
	if err != nil {
		t.Fatal(err)
	}
	if got != native {
		t.Fatalf("native launch executable = %q, want %q", got, native)
	}
}
