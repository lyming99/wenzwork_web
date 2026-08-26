package relayrelease

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateBootstrapAssetsRejectsTestKeyInProduction(t *testing.T) {
	directory := t.TempDir()
	for _, relative := range []string{
		"install.sh", "upgrade.sh", filepath.Join("lib", "common.sh"),
		filepath.Join("windows", "Install.ps1"), filepath.Join("windows", "Upgrade.ps1"), filepath.Join("windows", "lib", "RelayCommon.psm1"),
		filepath.Join("windows", "relayctl-amd64.exe"), filepath.Join("windows", "relayctl-arm64.exe"),
		filepath.Join("darwin", "install.sh"), filepath.Join("darwin", "upgrade.sh"), filepath.Join("darwin", "lib", "common.sh"),
		filepath.Join("darwin", "relayctl-amd64"), filepath.Join("darwin", "relayctl-arm64"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, relative)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeBootstrapTestFile(t, filepath.Join(directory, relative), []byte("bootstrap test asset\n"))
	}
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	writeBootstrapTestFile(t, filepath.Join(directory, "release-signing-public-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	if err := ValidateBootstrapAssets(directory, true); err != nil {
		t.Fatalf("production assets without marker: %v", err)
	}
	writeBootstrapTestFile(t, filepath.Join(directory, testSigningKeyMarker), []byte("test only\n"))
	if err := ValidateBootstrapAssets(directory, false); err != nil {
		t.Fatalf("development assets with marker: %v", err)
	}
	if err := ValidateBootstrapAssets(directory, true); err == nil {
		t.Fatal("production accepted the repository test signing key marker")
	}
}

func TestValidateRepositoryBootstrapAssetsAcrossPlatforms(t *testing.T) {
	directory := filepath.Join("..", "..", "..", "deploy", "relay")
	if err := ValidateBootstrapAssets(directory, false); err != nil {
		t.Fatalf("repository bootstrap assets: %v", err)
	}
}

func TestValidateBootstrapAssetsReportsTestKeyBeforeIncompleteProductionAssets(t *testing.T) {
	directory := t.TempDir()
	writeBootstrapTestFile(t, filepath.Join(directory, testSigningKeyMarker), []byte("test only\n"))
	err := ValidateBootstrapAssets(directory, true)
	if err == nil || !strings.Contains(err.Error(), "test signing key") {
		t.Fatalf("production validation error = %v, want test signing key rejection", err)
	}
}

func writeBootstrapTestFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
}
