package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

const stateSchemaVersion = 1

type clientState struct {
	SchemaVersion   int       `json:"schemaVersion"`
	DeviceID        uuid.UUID `json:"deviceId"`
	SessionID       uuid.UUID `json:"sessionId,omitempty"`
	PrivateKey      string    `json:"identityPrivateKey"`
	ConnectionEpoch uint64    `json:"connectionEpoch"`
	RefreshToken    string    `json:"refreshToken,omitempty"`

	identity ed25519.PrivateKey
}

func loadOrCreateState(path string) (clientState, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return clientState{}, errors.New("--state-file is required")
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, privateKey, keyErr := ed25519.GenerateKey(rand.Reader)
		if keyErr != nil {
			return clientState{}, fmt.Errorf("generate device identity: %w", keyErr)
		}
		state := clientState{
			SchemaVersion: stateSchemaVersion, DeviceID: uuid.New(),
			PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey), identity: privateKey,
		}
		if err := writeState(path, state); err != nil {
			return clientState{}, err
		}
		return state, nil
	}
	if err != nil {
		return clientState{}, fmt.Errorf("read state file: %w", err)
	}
	if len(contents) == 0 || len(contents) > 64<<10 {
		return clientState{}, errors.New("state file size is invalid")
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return clientState{}, fmt.Errorf("inspect state file: %w", statErr)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return clientState{}, errors.New("state file permissions must be 0600")
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var state clientState
	if err := decoder.Decode(&state); err != nil {
		return clientState{}, errors.New("state file JSON is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return clientState{}, errors.New("state file contains trailing data")
	}
	privateKey, err := base64.RawURLEncoding.DecodeString(state.PrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize || base64.RawURLEncoding.EncodeToString(privateKey) != state.PrivateKey ||
		state.SchemaVersion != stateSchemaVersion || state.DeviceID == uuid.Nil {
		return clientState{}, errors.New("state file identity is invalid")
	}
	state.identity = ed25519.PrivateKey(privateKey)
	return state, nil
}

func writeState(path string, state clientState) error {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	state.SchemaVersion = stateSchemaVersion
	if len(state.identity) == ed25519.PrivateKeySize {
		state.PrivateKey = base64.RawURLEncoding.EncodeToString(state.identity)
	}
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state file: %w", err)
	}
	contents = append(contents, '\n')
	temporary, err := os.CreateTemp(parent, ".relay-client-state-*")
	if err != nil {
		return fmt.Errorf("create state temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error {
		_ = temporary.Close()
		return cause
	}
	if err := temporary.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("secure state temporary file: %w", err))
	}
	if _, err := temporary.Write(contents); err != nil {
		return fail(fmt.Errorf("write state temporary file: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		return fail(fmt.Errorf("sync state temporary file: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace state file atomically: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure state file: %w", err)
	}
	if directory, err := os.Open(parent); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}
