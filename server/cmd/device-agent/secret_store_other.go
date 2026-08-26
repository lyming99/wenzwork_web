//go:build !windows && !linux && !darwin

package main

import (
	"crypto/ed25519"
	"errors"
	"os"
	"strings"

	"github.com/google/uuid"
)

func newPlatformSecretStore(path string, deviceID uuid.UUID, identity ed25519.PrivateKey) (secretStore, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("WENZWORK_AGENT_SECRET_STORE")))
	if mode == "file" {
		return newEncryptedFileSecretStore(path, deviceID, identity)
	}
	if mode != "" && mode != "native" {
		return nil, errors.New("WENZWORK_AGENT_SECRET_STORE must be native or file")
	}
	return unavailableSecretStore{reason: "platform secret store is unavailable; explicitly select the encrypted file secret store"}, nil
}
