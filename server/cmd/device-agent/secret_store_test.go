package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type recordingSecretStore struct {
	values  map[string][]byte
	putCall int
	failPut int
}

func (store *recordingSecretStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	value, found := store.values[key]
	return append([]byte(nil), value...), found, nil
}

func (store *recordingSecretStore) Put(_ context.Context, key string, value []byte) error {
	store.putCall++
	if store.failPut != 0 && store.putCall == store.failPut {
		return errors.New("injected SecretStore failure")
	}
	store.values[key] = append([]byte(nil), value...)
	return nil
}

func (store *recordingSecretStore) Delete(_ context.Context, key string) error {
	if value, found := store.values[key]; found {
		zeroSecret(value)
	}
	delete(store.values, key)
	return nil
}

func TestEncryptedFileSecretStoreRoundTripContainsNoPlaintext(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := newEncryptedFileSecretStore(state.path, state.DeviceID, state.identity)
	if err != nil {
		t.Fatal(err)
	}
	key := aiCredentialSecretKey("primary")
	secret := []byte("private-file-store-marker")
	if err := store.Put(t.Context(), key, secret); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.Get(t.Context(), key)
	if err != nil || !found || !bytes.Equal(loaded, secret) {
		t.Fatalf("Get() = %q, %v, %v", loaded, found, err)
	}
	zeroSecret(loaded)
	contents, err := os.ReadFile(state.path + ".secrets.enc")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(contents, secret) {
		t.Fatal("encrypted fallback contains plaintext")
	}
	if err := store.Delete(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Get(t.Context(), key); err != nil || found {
		t.Fatalf("Get() after delete found=%v err=%v", found, err)
	}
}

func TestLegacyAISecretsMigrateIdempotentlyOutOfIdentityState(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "legacy-ai-private-marker"
	state.AIConfigs["legacy"] = aiConfig{
		ID: "legacy", Name: "Legacy", Provider: "openai-compatible", Model: "model",
		Enabled: true, Revision: state.Revision, LegacyCredential: marker,
	}
	state.LegacyAIConfigs = state.AIConfigs
	if err := state.write(); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(statePath); err != nil || !bytes.Contains(contents, []byte(marker)) {
		t.Fatalf("legacy fixture was not persisted: %v", err)
	}

	migrated, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	config := migrated.AIConfigs["legacy"]
	if config.Credential != marker || !config.CredentialConfigured || config.LegacyCredential != "" {
		t.Fatalf("migrated config = %+v", config.view())
	}
	for _, path := range []string{statePath, statePath + ".secrets.enc"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, []byte(marker)) {
			t.Fatalf("%s contains migrated plaintext", path)
		}
	}
	identity, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(identity, []byte(`"aiConfigs"`)) || bytes.Contains(identity, []byte(`"Legacy"`)) {
		t.Fatal("identity file retained migrated AI configuration")
	}
	business, err := os.ReadFile(statePath + ".business.sqlite")
	if err != nil || !bytes.Contains(business, []byte("Legacy")) || bytes.Contains(business, []byte(marker)) {
		t.Fatalf("BusinessStore migration contents are invalid: %v", err)
	}
	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil || reloaded.AIConfigs["legacy"].Credential != marker {
		t.Fatalf("idempotent reload credential=%q error=%v", reloaded.AIConfigs["legacy"].Credential, err)
	}
}

func TestLegacyAISecretMigrationRollsBackNewItemsOnFailure(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		state.AIConfigs[id] = aiConfig{
			ID: id, Name: id, Provider: "openai-compatible", Model: "model", Enabled: true,
			Revision: state.Revision, LegacyCredential: "private-" + id + "-marker",
		}
	}
	state.LegacyAIConfigs = state.AIConfigs
	if err := state.write(); err != nil {
		t.Fatal(err)
	}
	fake := &recordingSecretStore{values: map[string][]byte{}, failPut: 2}
	previousFactory := openSecretStore
	openSecretStore = func(string, uuid.UUID, ed25519.PrivateKey) (secretStore, error) { return fake, nil }
	t.Cleanup(func() { openSecretStore = previousFactory })
	_, err = loadOrCreateAgentState(statePath, workspace)
	if err == nil || strings.Contains(err.Error(), "private-first-marker") || strings.Contains(err.Error(), "private-second-marker") {
		t.Fatalf("migration error = %v", err)
	}
	if len(fake.values) != 0 {
		t.Fatalf("migration left SecretStore items: %#v", fake.values)
	}
	contents, readErr := os.ReadFile(statePath)
	if readErr != nil || !bytes.Contains(contents, []byte("private-first-marker")) || !bytes.Contains(contents, []byte("private-second-marker")) {
		t.Fatalf("legacy source was modified after failed migration: %v", readErr)
	}
}

func TestAIConfigSecretStorePreservesReplacesClearsAndDeletes(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}
	update := func(expected uint64, name, secretFragment string) map[string]any {
		input := fmt.Sprintf(`{"id":"primary","expectedRevision":%d,"name":%q,"provider":"openai-compatible","baseUrl":"https://api.example.test/v1","model":"model","enabled":true%s}`,
			expected, name, secretFragment)
		return dispatchJSON(t, dispatch, "ai.config.update", input)
	}
	first := update(0, "First", `,"secret":"first-private-marker"`)
	firstRevision := uint64(first["revision"].(float64))
	if first["secretConfigured"] != true {
		t.Fatalf("first config = %#v", first)
	}
	preserved := update(firstRevision, "Preserved", "")
	preservedRevision := uint64(preserved["revision"].(float64))
	if state.AIConfigs["primary"].Credential != "first-private-marker" || preserved["secretConfigured"] != true {
		t.Fatalf("preserved config = %#v", preserved)
	}
	replaced := update(preservedRevision, "Replaced", `,"secret":"second-private-marker"`)
	replacedRevision := uint64(replaced["revision"].(float64))
	if state.AIConfigs["primary"].Credential != "second-private-marker" {
		t.Fatalf("replacement credential = %q", state.AIConfigs["primary"].Credential)
	}
	cleared := update(replacedRevision, "Cleared", `,"secret":""`)
	if cleared["secretConfigured"] != false || state.AIConfigs["primary"].Credential != "" {
		t.Fatalf("cleared config = %#v", cleared)
	}
	if _, found, err := state.secrets.Get(t.Context(), aiCredentialSecretKey("primary")); err != nil || found {
		t.Fatalf("cleared SecretStore item found=%v error=%v", found, err)
	}
	clearedRevision := uint64(cleared["revision"].(float64))
	configured := update(clearedRevision, "Again", `,"secret":"third-private-marker"`)
	deleteResponse := dispatchEnvelope(t, dispatch, "ai.config.delete", `{"configId":"primary"}`)
	if deleteResponse.GetError() != nil || configured["secretConfigured"] != true {
		t.Fatalf("delete response = %+v", deleteResponse.GetError())
	}
	if _, found, err := state.secrets.Get(t.Context(), aiCredentialSecretKey("primary")); err != nil || found {
		t.Fatalf("deleted SecretStore item found=%v error=%v", found, err)
	}
	for _, path := range []string{state.path, state.path + ".secrets.enc"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range []string{"first-private-marker", "second-private-marker", "third-private-marker"} {
			if bytes.Contains(contents, []byte(marker)) {
				t.Fatalf("%s contains plaintext marker", path)
			}
		}
	}
}
