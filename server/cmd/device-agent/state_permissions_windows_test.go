//go:build windows

package main

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestAgentStateUsesProtectedWindowsACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if _, err := loadOrCreateAgentState(path, filepath.Join(t.TempDir(), "workspace")); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("agent state DACL inherits permissions")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 3 {
		t.Fatalf("agent state DACL = %#v, err=%v", dacl, err)
	}
}
