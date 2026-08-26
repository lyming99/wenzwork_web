package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const defaultHostSecretsFile = "cache/host-secrets/application.env"

const releaseAccessKeyPrefix = "release_"

type generatedHostSecrets struct {
	mfaEncryptionKey      string
	redemptionCodeHMACKey string
}

// withGeneratedHostSecrets keeps deployment-only cryptographic material out
// of the user-managed Host environment. Explicit valid values remain
// supported for existing installations; missing values are loaded from, or
// generated into, a stable owner-only file.
func withGeneratedHostSecrets(lookup lookupFunc) (lookupFunc, error) {
	mfa, hasMFA := lookup("MFA_ENCRYPTION_KEY")
	redemption, hasRedemption := lookup("REDEMPTION_CODE_HMAC_KEY")
	needsMFA := needsGeneratedHostSecret(mfa, hasMFA)
	needsRedemption := needsGeneratedHostSecret(redemption, hasRedemption)
	if !needsMFA && !needsRedemption {
		return lookup, nil
	}

	path := valueOrDefault(lookup, "HOST_SECRETS_FILE", defaultHostSecretsFile)
	generated, err := loadOrCreateGeneratedHostSecrets(path)
	if err != nil {
		return nil, err
	}
	overrides := map[string]string{}
	if needsMFA {
		overrides["MFA_ENCRYPTION_KEY"] = generated.mfaEncryptionKey
	}
	if needsRedemption {
		overrides["REDEMPTION_CODE_HMAC_KEY"] = generated.redemptionCodeHMACKey
	}
	return func(key string) (string, bool) {
		if value, ok := overrides[key]; ok {
			return value, true
		}
		return lookup(key)
	}, nil
}

// withGeneratedReleaseAccessKey preserves the legacy bootstrap credential for
// one-time import into the database. An explicit RELEASE_ACCESS_KEY remains
// supported; otherwise the bootstrap value is stored beside the other Host
// secrets in an owner-only file. Once imported, the database-backed digest is
// authoritative and administrator rotations are not overwritten on restart.
func withGeneratedReleaseAccessKey(lookup lookupFunc) (lookupFunc, error) {
	configured, present := lookup("RELEASE_ACCESS_KEY")
	if !needsGeneratedHostSecret(configured, present) {
		return lookup, nil
	}
	hostSecretsFile := valueOrDefault(lookup, "HOST_SECRETS_FILE", defaultHostSecretsFile)
	keyFile := valueOrDefault(
		lookup,
		"RELEASE_ACCESS_KEY_FILE",
		filepath.Join(filepath.Dir(hostSecretsFile), "release-access-key"),
	)
	key, err := loadOrCreateReleaseAccessKey(keyFile)
	if err != nil {
		return nil, err
	}
	return func(name string) (string, bool) {
		if name == "RELEASE_ACCESS_KEY" {
			return key, true
		}
		return lookup(name)
	}, nil
}

func loadOrCreateReleaseAccessKey(path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" || strings.ContainsRune(path, '\x00') {
		return "", errors.New("RELEASE_ACCESS_KEY_FILE is invalid")
	}
	if contents, err := readGeneratedHostSecretsFile(path); err == nil {
		key := strings.TrimSpace(string(contents))
		if !validReleaseAccessKey(key) {
			return "", errors.New("Release Access Key file is invalid")
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create Release Access Key directory: %w", err)
	}
	key, err := randomReleaseAccessKey()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		contents, readErr := readGeneratedHostSecretsFile(path)
		if readErr != nil {
			return "", readErr
		}
		existing := strings.TrimSpace(string(contents))
		if !validReleaseAccessKey(existing) {
			return "", errors.New("Release Access Key file is invalid")
		}
		return existing, nil
	}
	if err != nil {
		return "", fmt.Errorf("create Release Access Key file: %w", err)
	}
	contents := []byte(key + "\n")
	written, writeErr := file.Write(contents)
	closeErr := file.Close()
	if writeErr != nil || written != len(contents) || closeErr != nil {
		_ = os.Remove(path)
		if writeErr != nil {
			return "", fmt.Errorf("write Release Access Key file: %w", writeErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close Release Access Key file: %w", closeErr)
		}
		return "", errors.New("write Release Access Key file: short write")
	}
	return key, nil
}

func randomReleaseAccessKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Release Access Key: %w", err)
	}
	return releaseAccessKeyPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func validReleaseAccessKey(value string) bool {
	if len(value) != len(releaseAccessKeyPrefix)+43 || !strings.HasPrefix(value, releaseAccessKeyPrefix) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, releaseAccessKeyPrefix))
	return err == nil && len(decoded) == 32
}

func needsGeneratedHostSecret(value string, present bool) bool {
	value = strings.TrimSpace(value)
	return !present || value == "" || strings.HasPrefix(value, "<") ||
		strings.Contains(value, "generate-") || strings.Contains(value, "replace-")
}

func loadOrCreateGeneratedHostSecrets(path string) (generatedHostSecrets, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" || strings.ContainsRune(path, '\x00') {
		return generatedHostSecrets{}, errors.New("HOST_SECRETS_FILE is invalid")
	}
	if contents, err := readGeneratedHostSecretsFile(path); err == nil {
		return parseGeneratedHostSecrets(contents)
	} else if !errors.Is(err, os.ErrNotExist) {
		return generatedHostSecrets{}, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return generatedHostSecrets{}, fmt.Errorf("create Host secrets directory: %w", err)
	}
	secrets, err := newGeneratedHostSecrets()
	if err != nil {
		return generatedHostSecrets{}, err
	}
	contents := []byte("MFA_ENCRYPTION_KEY=" + secrets.mfaEncryptionKey + "\n" +
		"REDEMPTION_CODE_HMAC_KEY=" + secrets.redemptionCodeHMACKey + "\n")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		contents, readErr := readGeneratedHostSecretsFile(path)
		if readErr != nil {
			return generatedHostSecrets{}, readErr
		}
		return parseGeneratedHostSecrets(contents)
	}
	if err != nil {
		return generatedHostSecrets{}, fmt.Errorf("create Host secrets file: %w", err)
	}
	written, writeErr := file.Write(contents)
	closeErr := file.Close()
	if writeErr != nil || written != len(contents) || closeErr != nil {
		_ = os.Remove(path)
		if writeErr != nil {
			return generatedHostSecrets{}, fmt.Errorf("write Host secrets file: %w", writeErr)
		}
		if closeErr != nil {
			return generatedHostSecrets{}, fmt.Errorf("close Host secrets file: %w", closeErr)
		}
		return generatedHostSecrets{}, errors.New("write Host secrets file: short write")
	}
	return secrets, nil
}

func readGeneratedHostSecretsFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Host secrets file must be a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Host secrets file must be readable only by its owner")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Host secrets file: %w", err)
	}
	if len(contents) > 4096 {
		return nil, errors.New("Host secrets file is unexpectedly large")
	}
	return contents, nil
}

func parseGeneratedHostSecrets(contents []byte) (generatedHostSecrets, error) {
	values := map[string]string{}
	for _, rawLine := range strings.Split(string(contents), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || (key != "MFA_ENCRYPTION_KEY" && key != "REDEMPTION_CODE_HMAC_KEY") {
			return generatedHostSecrets{}, errors.New("Host secrets file contains an unsupported entry")
		}
		if _, exists := values[key]; exists {
			return generatedHostSecrets{}, fmt.Errorf("Host secrets file contains duplicate %s", key)
		}
		if !validGeneratedHostSecret(value) {
			return generatedHostSecrets{}, fmt.Errorf("Host secrets file contains invalid %s", key)
		}
		values[key] = value
	}
	secrets := generatedHostSecrets{
		mfaEncryptionKey:      values["MFA_ENCRYPTION_KEY"],
		redemptionCodeHMACKey: values["REDEMPTION_CODE_HMAC_KEY"],
	}
	if secrets.mfaEncryptionKey == "" || secrets.redemptionCodeHMACKey == "" {
		return generatedHostSecrets{}, errors.New("Host secrets file is incomplete")
	}
	if secrets.mfaEncryptionKey == secrets.redemptionCodeHMACKey {
		return generatedHostSecrets{}, errors.New("Host secrets file keys must be independent")
	}
	return secrets, nil
}

func newGeneratedHostSecrets() (generatedHostSecrets, error) {
	first, err := randomHostSecret()
	if err != nil {
		return generatedHostSecrets{}, err
	}
	second, err := randomHostSecret()
	if err != nil {
		return generatedHostSecrets{}, err
	}
	for second == first {
		second, err = randomHostSecret()
		if err != nil {
			return generatedHostSecrets{}, err
		}
	}
	return generatedHostSecrets{mfaEncryptionKey: first, redemptionCodeHMACKey: second}, nil
}

func randomHostSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Host secret: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func validGeneratedHostSecret(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
