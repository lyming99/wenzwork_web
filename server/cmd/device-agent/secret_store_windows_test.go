//go:build windows

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestWindowsDPAPISecretStoreSupportsFirstAIConfigCredential(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "native")
	_, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "agent-state.json")
	store, err := newPlatformSecretStore(statePath, uuid.New(), identity)
	if err != nil {
		t.Fatal(err)
	}

	key := aiCredentialSecretKey("default")
	first := []byte("first-private-marker")
	replacement := []byte("replacement-private-marker")
	if err := store.Put(t.Context(), key, first); err != nil {
		t.Fatalf("first Put() error = %v", err)
	}
	if err := store.Put(t.Context(), key, replacement); err != nil {
		t.Fatalf("replacement Put() error = %v", err)
	}
	got, found, err := store.Get(t.Context(), key)
	if err != nil || !found || !bytes.Equal(got, replacement) {
		t.Fatalf("Get() = %q, %v, %v", got, found, err)
	}
	zeroSecret(got)

	contents, err := os.ReadFile(statePath + ".secrets.dpapi")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(contents, first) || bytes.Contains(contents, replacement) {
		t.Fatal("Windows DPAPI secret store contains plaintext")
	}
	if err := store.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, found, err := store.Get(t.Context(), key); err != nil || found {
		t.Fatalf("Get() after Delete() found=%v error=%v", found, err)
	}
}
