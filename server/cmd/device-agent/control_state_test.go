package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestControlStateEncryptsCredentialsAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "agent.json")
	workspace := filepath.Join(root, "workspace")
	agent, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadControlState(agent)
	if err != nil {
		t.Fatal(err)
	}
	refreshToken := "refresh-secret-that-must-never-be-plaintext"
	taskID := uuid.New()
	if err := store.update(func(state *controlPersistentState) error {
		state.Auth = controlAuthState{RefreshToken: refreshToken, RefreshExpiresAt: time.Now().Add(time.Hour), SessionID: uuid.New(), Scope: "remote.connect"}
		state.CancelledTasks[taskID.String()] = true
		state.NextEventSequence = 9
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := os.ReadFile(statePath + controlStateFileExtension)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(refreshToken)) || bytes.Contains(ciphertext, []byte(taskID.String())) {
		t.Fatalf("encrypted state disclosed plaintext: %s", ciphertext)
	}

	reloadedAgent, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadControlState(reloadedAgent)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reloaded.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Auth.RefreshToken != refreshToken || !snapshot.CancelledTasks[taskID.String()] || snapshot.NextEventSequence != 9 {
		t.Fatalf("reloaded state = %#v", snapshot)
	}
}

func TestControlStateRejectsCiphertextTampering(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "agent.json")
	agent, err := loadOrCreateAgentState(statePath, filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadControlState(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.update(func(state *controlPersistentState) error {
		state.Auth.RefreshToken = "refresh-secret"
		state.Auth.RefreshExpiresAt = time.Now().Add(time.Hour)
		state.Auth.SessionID = uuid.New()
		state.Auth.Scope = "remote.connect"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := statePath + controlStateFileExtension
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents[len(contents)/2] ^= 1
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadControlState(agent); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tampered state error = %v", err)
	}
}
