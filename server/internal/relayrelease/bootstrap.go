package relayrelease

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const testSigningKeyMarker = "TEST_ONLY_SIGNING_KEY"

// ValidateBootstrapAssets prevents missing or placeholder supply-chain assets
// from being exposed. Production Release packaging replaces the repository
// test public key and deliberately omits the marker file.
func ValidateBootstrapAssets(directory string, production bool) error {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if directory == "." || directory == "" {
		return errors.New("Relay bootstrap asset directory is required")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Relay bootstrap asset directory is missing or unsafe")
	}
	if production {
		if _, err := os.Lstat(filepath.Join(directory, testSigningKeyMarker)); err == nil {
			return errors.New("production cannot use the repository Relay test signing key")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect Relay signing key marker: %w", err)
		}
	}
	required := []string{
		"install.sh", "upgrade.sh", filepath.Join("lib", "common.sh"),
		filepath.Join("windows", "Install.ps1"), filepath.Join("windows", "Upgrade.ps1"), filepath.Join("windows", "lib", "RelayCommon.psm1"),
		filepath.Join("darwin", "install.sh"), filepath.Join("darwin", "upgrade.sh"), filepath.Join("darwin", "lib", "common.sh"),
		"release-signing-public-key.pem",
	}
	if production {
		required = append(required,
			filepath.Join("windows", "relayctl-amd64.exe"), filepath.Join("windows", "relayctl-arm64.exe"),
			filepath.Join("darwin", "relayctl-amd64"), filepath.Join("darwin", "relayctl-arm64"),
		)
	}
	for _, relative := range required {
		path := filepath.Join(directory, relative)
		info, err := os.Lstat(path)
		maximumBytes := int64(1 << 20)
		if strings.Contains(filepath.Base(relative), "relayctl-") {
			maximumBytes = 64 << 20
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumBytes {
			return fmt.Errorf("Relay bootstrap asset is missing or unsafe: %s", relative)
		}
	}
	contents, err := os.ReadFile(filepath.Join(directory, "release-signing-public-key.pem"))
	if err != nil {
		return fmt.Errorf("read Relay release signing public key: %w", err)
	}
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("Relay release signing public key must contain one PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	publicKey, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("Relay release signing public key must be Ed25519")
	}
	return nil
}
