package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const (
	maximumSecretBytes     = 16 << 10
	maximumSecretStoreSize = 4 << 20
	fileSecretStoreVersion = 1
)

type secretStore interface {
	Get(context.Context, string) ([]byte, bool, error)
	Put(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

var openSecretStore = newPlatformSecretStore

func aiCredentialSecretKey(configID string) string {
	digest := sha256.Sum256([]byte("wenzwork-ai-config-secret:v1\x00" + configID))
	return "ai-config:" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func validSecretKey(key string) bool {
	if len(key) == 0 || len(key) > 128 {
		return false
	}
	for _, character := range key {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func validateSecretValue(value []byte) error {
	if len(value) == 0 || len(value) > maximumSecretBytes {
		return errors.New("secret value size is invalid")
	}
	return nil
}

func zeroSecret(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// unavailableSecretStore lets an Agent without a platform vault continue to
// run when it has no configured secrets. Secret writes and required reads fail
// closed with a fixed message; secret material is never included in the error.
type unavailableSecretStore struct {
	reason string
}

func (store unavailableSecretStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (store unavailableSecretStore) Put(context.Context, string, []byte) error {
	if store.reason == "" {
		return errors.New("platform secret store is unavailable")
	}
	return errors.New(store.reason)
}

func (unavailableSecretStore) Delete(context.Context, string) error { return nil }

type encryptedFileSecretStore struct {
	mu   sync.Mutex
	path string
	aead cipher.AEAD
}

type encryptedSecretFile struct {
	Version int               `json:"version"`
	Values  map[string]string `json:"values"`
}

// newEncryptedFileSecretStore is the explicit restricted-file fallback. Its
// encryption key is derived from the separately protected device identity;
// the file itself contains only random nonces and authenticated ciphertext.
func newEncryptedFileSecretStore(path string, deviceID uuid.UUID, identity ed25519.PrivateKey) (secretStore, error) {
	if strings.TrimSpace(path) == "" || deviceID == uuid.Nil || len(identity) != ed25519.PrivateKeySize {
		return nil, errors.New("file secret store identity is invalid")
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("wenzwork-file-secret-store:v1\x00"))
	_, _ = hash.Write(deviceID[:])
	_, _ = hash.Write(identity)
	key := hash.Sum(nil)
	defer zeroSecret(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("initialize file secret store encryption")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("initialize file secret store authentication")
	}
	return &encryptedFileSecretStore{path: path + ".secrets.enc", aead: aead}, nil
}

func (store *encryptedFileSecretStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
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
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(sealed) <= store.aead.NonceSize()+store.aead.Overhead() || len(sealed) > maximumSecretBytes+store.aead.NonceSize()+store.aead.Overhead() {
		return nil, false, errors.New("encrypted secret value is invalid")
	}
	nonce := sealed[:store.aead.NonceSize()]
	plaintext, err := store.aead.Open(nil, nonce, sealed[store.aead.NonceSize():], []byte(key))
	if err != nil || validateSecretValue(plaintext) != nil {
		zeroSecret(plaintext)
		return nil, false, errors.New("encrypted secret value cannot be opened")
	}
	return plaintext, true, nil
}

func (store *encryptedFileSecretStore) Put(ctx context.Context, key string, value []byte) error {
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
	nonce := make([]byte, store.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return errors.New("generate secret nonce")
	}
	sealed := store.aead.Seal(nonce, nonce, value, []byte(key))
	values[key] = base64.RawURLEncoding.EncodeToString(sealed)
	zeroSecret(sealed)
	return store.writeLocked(values)
}

func (store *encryptedFileSecretStore) Delete(ctx context.Context, key string) error {
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

func (store *encryptedFileSecretStore) readLocked() (map[string]string, error) {
	contents, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, errors.New("read encrypted secret store")
	}
	if len(contents) == 0 || len(contents) > maximumSecretStoreSize {
		return nil, errors.New("encrypted secret store size is invalid")
	}
	if err := verifyStateFileSecurity(store.path); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	var file encryptedSecretFile
	if err := decoder.Decode(&file); err != nil || file.Version != fileSecretStoreVersion || file.Values == nil {
		return nil, errors.New("encrypted secret store is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("encrypted secret store contains trailing data")
	}
	for key, value := range file.Values {
		if !validSecretKey(key) || len(value) == 0 || len(value) > (maximumSecretBytes+64)*2 {
			return nil, errors.New("encrypted secret store entry is invalid")
		}
	}
	return file.Values, nil
}

func (store *encryptedFileSecretStore) writeLocked(values map[string]string) error {
	contents, err := json.Marshal(encryptedSecretFile{Version: fileSecretStoreVersion, Values: values})
	if err != nil || len(contents) > maximumSecretStoreSize {
		return errors.New("encode encrypted secret store")
	}
	contents = append(contents, '\n')
	parent := filepath.Dir(store.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errors.New("create encrypted secret store directory")
	}
	temporary, err := os.CreateTemp(parent, ".device-agent-secrets-*")
	if err != nil {
		return errors.New("create encrypted secret store temporary file")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error {
		_ = temporary.Close()
		return cause
	}
	if err := temporary.Chmod(0o600); err != nil {
		return fail(errors.New("protect encrypted secret store temporary file"))
	}
	if _, err := temporary.Write(contents); err != nil {
		return fail(errors.New("write encrypted secret store temporary file"))
	}
	if err := temporary.Sync(); err != nil {
		return fail(errors.New("sync encrypted secret store temporary file"))
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close encrypted secret store temporary file")
	}
	if err := secureStateFile(temporaryPath); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return errors.New("install encrypted secret store")
	}
	if err := secureStateFile(store.path); err != nil {
		return fmt.Errorf("protect encrypted secret store: %w", err)
	}
	return nil
}
