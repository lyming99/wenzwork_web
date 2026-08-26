//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupWindowsExecutableInInteractivePathFindsCmdShim(t *testing.T) {
	root := t.TempDir()
	shim := filepath.Join(root, "codex.cmd")
	writeNativeCodexTestFile(t, shim, []byte("@echo off\r\n"))
	got, err := lookupWindowsExecutableInEnvironment("codex", []string{"Path=" + root, "PATHEXT=.CMD;.EXE"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(got, shim) {
		t.Fatalf("resolved command = %q, want %q", got, shim)
	}
}
