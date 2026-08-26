//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"
)

func TestAgentStateRealDiskFullFailsAtomicallyAndRecovers(t *testing.T) {
	if os.Getenv("WENZWORK_AGENT_DISK_FULL_EPHEMERAL_ROOT") != "I_UNDERSTAND_THIS_FILLS_A_PRIVATE_TEST_FILESYSTEM" {
		t.Skip("explicit private-filesystem confirmation is required")
	}
	root := filepath.Clean(os.Getenv("WENZWORK_AGENT_DISK_FULL_TEST_ROOT"))
	if !filepath.IsAbs(root) || !strings.HasPrefix(root, "/tmp/wenzwork-device-agent-disk-full.") {
		t.Fatal("disk-full test root is outside the expected ephemeral prefix")
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		t.Fatalf("disk-full test root is unavailable: %v", err)
	}
	parentInfo, err := os.Stat(filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	rootStat, rootOK := rootInfo.Sys().(*syscall.Stat_t)
	parentStat, parentOK := parentInfo.Sys().(*syscall.Stat_t)
	if !rootOK || !parentOK || rootStat.Dev == parentStat.Dev {
		t.Fatal("disk-full test root is not a distinct mounted filesystem")
	}

	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	statePath := filepath.Join(root, "state", "agent-state.json")
	workspace := filepath.Join(root, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	originalContents, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	originalRevision := state.Revision
	originalSessionID := state.SessionID

	fillerPath := filepath.Join(root, "disk-full.fixture")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte{0xa5}, 64<<10)
	full := false
	for !full {
		n, writeErr := filler.Write(block)
		if errors.Is(writeErr, syscall.ENOSPC) {
			full = true
			break
		}
		if writeErr != nil {
			_ = filler.Close()
			t.Fatalf("filling private filesystem failed with %v", writeErr)
		}
		if n <= 0 {
			_ = filler.Close()
			t.Fatal("filling private filesystem made no progress")
		}
	}
	if closeErr := filler.Close(); closeErr != nil && !errors.Is(closeErr, syscall.ENOSPC) {
		t.Fatal(closeErr)
	}
	if !full {
		t.Fatal("private filesystem did not report ENOSPC")
	}

	if err := state.setSessionID(uuid.New()); err == nil || state.Revision != originalRevision || state.SessionID != originalSessionID {
		t.Fatalf("disk-full mutation error=%v revision=%d session=%s", err, state.Revision, state.SessionID)
	}
	afterFailure, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(afterFailure, originalContents) {
		t.Fatalf("durable identity changed after ENOSPC: equal=%v error=%v", bytes.Equal(afterFailure, originalContents), err)
	}

	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := state.persistMutation(); err != nil || state.Revision != originalRevision+1 {
		t.Fatalf("state did not recover after space was released: revision=%d error=%v", state.Revision, err)
	}
	projects, err := state.business.listProjects(t.Context(), false)
	if err != nil || len(projects) == 0 {
		t.Fatalf("BusinessStore did not remain readable after ENOSPC: projects=%d error=%v", len(projects), err)
	}
}
