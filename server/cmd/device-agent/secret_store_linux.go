//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/google/uuid"
)

type secretServiceStore struct {
	executable string
	service    string
}

func newPlatformSecretStore(path string, deviceID uuid.UUID, identity ed25519.PrivateKey) (secretStore, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("WENZWORK_AGENT_SECRET_STORE")))
	if mode == "file" {
		return newEncryptedFileSecretStore(path, deviceID, identity)
	}
	if mode != "" && mode != "native" {
		return nil, errors.New("WENZWORK_AGENT_SECRET_STORE must be native or file")
	}
	executable, err := exec.LookPath("secret-tool")
	if err != nil {
		return unavailableSecretStore{reason: "Linux Secret Service is unavailable; install secret-tool or explicitly select the encrypted file secret store"}, nil
	}
	return &secretServiceStore{executable: executable, service: "wenzwork-device-agent-" + deviceID.String()}, nil
}

func (store *secretServiceStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if !validSecretKey(key) {
		return nil, false, errors.New("secret key is invalid")
	}
	command := exec.CommandContext(ctx, store.executable, "lookup", "service", store.service, "account", key)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = new(bytes.Buffer)
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, false, nil
		}
		return nil, false, errors.New("read Linux Secret Service item")
	}
	raw := stdout.Bytes()
	value := bytes.TrimSuffix(raw, []byte{'\n'})
	value = bytes.TrimSuffix(value, []byte{'\r'})
	if err := validateSecretValue(value); err != nil {
		zeroSecret(raw)
		return nil, false, errors.New("Linux Secret Service returned an invalid item")
	}
	result := append([]byte(nil), value...)
	zeroSecret(raw)
	return result, true, nil
}

func (store *secretServiceStore) Put(ctx context.Context, key string, value []byte) error {
	if !validSecretKey(key) {
		return errors.New("secret key is invalid")
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, store.executable, "store", "--label=Wenzwork Device Agent", "service", store.service, "account", key)
	command.Stdin = bytes.NewReader(value)
	command.Stdout = new(bytes.Buffer)
	command.Stderr = new(bytes.Buffer)
	if err := command.Run(); err != nil {
		return errors.New("write Linux Secret Service item")
	}
	return nil
}

func (store *secretServiceStore) Delete(ctx context.Context, key string) error {
	if !validSecretKey(key) {
		return errors.New("secret key is invalid")
	}
	command := exec.CommandContext(ctx, store.executable, "clear", "service", store.service, "account", key)
	command.Stdout = new(bytes.Buffer)
	command.Stderr = new(bytes.Buffer)
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil
		}
		return errors.New("delete Linux Secret Service item")
	}
	return nil
}
