//go:build windows

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
)

const dpapiSecretStoreVersion = 1

type dpapiSecretStore struct {
	mu      sync.Mutex
	path    string
	entropy []byte
}

type dpapiSecretFile struct {
	Version int               `json:"version"`
	Values  map[string]string `json:"values"`
}

func newPlatformSecretStore(path string, deviceID uuid.UUID, identity ed25519.PrivateKey) (secretStore, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("WENZWORK_AGENT_SECRET_STORE")))
	if mode == "file" {
		return newEncryptedFileSecretStore(path, deviceID, identity)
	}
	if mode != "" && mode != "native" {
		return nil, errors.New("WENZWORK_AGENT_SECRET_STORE must be native or file")
	}
	if strings.TrimSpace(path) == "" || deviceID == uuid.Nil || len(identity) != ed25519.PrivateKeySize {
		return nil, errors.New("Windows secret store identity is invalid")
	}
	digest := sha256.Sum256(append([]byte("wenzwork-dpapi-secret-store:v1\x00"), deviceID[:]...))
	return &dpapiSecretStore{path: path + ".secrets.dpapi", entropy: append([]byte(nil), digest[:]...)}, nil
}

func (store *dpapiSecretStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !validSecretKey(key) {
		return nil, false, errors.New("secret key is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	values, err := store.readLocked()
	if err != nil {
		return nil, false, err
	}
	encoded, found := values[key]
	if !found {
		return nil, false, nil
	}
	ciphertext, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(ciphertext) == 0 || len(ciphertext) > maximumSecretBytes+4096 {
		return nil, false, errors.New("DPAPI secret value is invalid")
	}
	plaintext, err := store.unprotect(ciphertext)
	zeroSecret(ciphertext)
	if err != nil || validateSecretValue(plaintext) != nil {
		zeroSecret(plaintext)
		return nil, false, errors.New("DPAPI secret value cannot be opened")
	}
	return plaintext, true, nil
}

func (store *dpapiSecretStore) Put(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSecretKey(key) {
		return errors.New("secret key is invalid")
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	values, err := store.readLocked()
	if err != nil {
		return err
	}
	ciphertext, err := store.protect(value)
	if err != nil {
		return err
	}
	values[key] = base64.RawURLEncoding.EncodeToString(ciphertext)
	zeroSecret(ciphertext)
	return store.writeLocked(values)
}

func (store *dpapiSecretStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validSecretKey(key) {
		return errors.New("secret key is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	values, err := store.readLocked()
	if err != nil {
		return err
	}
	if _, found := values[key]; !found {
		return nil
	}
	delete(values, key)
	return store.writeLocked(values)
}

func (store *dpapiSecretStore) protect(value []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
	entropy := windows.DataBlob{Size: uint32(len(store.entropy)), Data: &store.entropy[0]}
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, errors.New("protect secret with Windows DPAPI")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	if output.Data == nil || output.Size == 0 || output.Size > maximumSecretBytes+4096 {
		return nil, errors.New("Windows DPAPI returned an invalid secret")
	}
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}

func (store *dpapiSecretStore) unprotect(value []byte) ([]byte, error) {
	input := windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
	entropy := windows.DataBlob{Size: uint32(len(store.entropy)), Data: &store.entropy[0]}
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, errors.New("open secret with Windows DPAPI")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	if output.Data == nil || output.Size == 0 || output.Size > maximumSecretBytes {
		return nil, errors.New("Windows DPAPI opened an invalid secret")
	}
	plaintext := append([]byte(nil), unsafe.Slice(output.Data, output.Size)...)
	zeroSecret(unsafe.Slice(output.Data, output.Size))
	return plaintext, nil
}

func (store *dpapiSecretStore) readLocked() (map[string]string, error) {
	contents, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, errors.New("read Windows secret store")
	}
	if len(contents) == 0 || len(contents) > maximumSecretStoreSize {
		return nil, errors.New("Windows secret store size is invalid")
	}
	if err := verifyStateFileSecurity(store.path); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var file dpapiSecretFile
	if err := decoder.Decode(&file); err != nil || file.Version != dpapiSecretStoreVersion || file.Values == nil {
		return nil, errors.New("Windows secret store is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("Windows secret store contains trailing data")
	}
	for key, value := range file.Values {
		if !validSecretKey(key) || len(value) == 0 || len(value) > (maximumSecretBytes+4096)*2 {
			return nil, errors.New("Windows secret store entry is invalid")
		}
	}
	return file.Values, nil
}

func (store *dpapiSecretStore) writeLocked(values map[string]string) error {
	contents, err := json.Marshal(dpapiSecretFile{Version: dpapiSecretStoreVersion, Values: values})
	if err != nil || len(contents) > maximumSecretStoreSize {
		return errors.New("encode Windows secret store")
	}
	contents = append(contents, '\n')
	parent := filepath.Dir(store.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errors.New("create Windows secret store directory")
	}
	temporary, err := os.CreateTemp(parent, ".device-agent-secrets-*")
	if err != nil {
		return errors.New("create Windows secret store temporary file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error {
		_ = temporary.Close()
		return cause
	}
	if err := temporary.Chmod(0o600); err != nil {
		return fail(errors.New("protect Windows secret store temporary file"))
	}
	if _, err := temporary.Write(contents); err != nil {
		return fail(errors.New("write Windows secret store temporary file"))
	}
	if err := temporary.Sync(); err != nil {
		return fail(errors.New("sync Windows secret store temporary file"))
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close Windows secret store temporary file")
	}
	if err := secureStateFile(temporaryPath); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return errors.New("install Windows secret store")
	}
	return secureStateFile(store.path)
}
