package relayrelease

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyBundleAuthenticatesChecksumsAndArchive(t *testing.T) {
	directory := t.TempDir()
	archiveName := "wenzwork-relay-1.2.3-windows-arm64.tar.gz"
	archive := []byte("signed release archive")
	digest := sha256.Sum256(archive)
	checksums := []byte(hex.EncodeToString(digest[:]) + "  " + archiveName + "\n")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, directory, archiveName, archive)
	writeTestFile(t, directory, "SHA256SUMS", checksums)
	writeTestFile(t, directory, "SHA256SUMS.sig", ed25519.Sign(privateKey, checksums))
	writeTestFile(t, directory, "release-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))

	options := BundleVerifyOptions{
		ArchivePath: filepath.Join(directory, archiveName), ChecksumsPath: filepath.Join(directory, "SHA256SUMS"),
		SignaturePath: filepath.Join(directory, "SHA256SUMS.sig"), PublicKeyPath: filepath.Join(directory, "release-key.pem"),
	}
	if err := VerifyBundle(options); err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if err := os.WriteFile(options.ArchivePath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundle(options); err == nil {
		t.Fatal("VerifyBundle() accepted a tampered archive")
	}
}

func TestVerifyBundleRejectsTamperedSignatureAndDuplicateEntry(t *testing.T) {
	directory := t.TempDir()
	archiveName := "relay.tar.gz"
	archive := []byte("archive")
	digest := sha256.Sum256(archive)
	line := hex.EncodeToString(digest[:]) + "  " + archiveName + "\n"
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, directory, archiveName, archive)
	writeTestFile(t, directory, "SHA256SUMS", []byte(line))
	writeTestFile(t, directory, "SHA256SUMS.sig", ed25519.Sign(privateKey, []byte(line)))
	writeTestFile(t, directory, "release-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	options := BundleVerifyOptions{
		ArchivePath: filepath.Join(directory, archiveName), ChecksumsPath: filepath.Join(directory, "SHA256SUMS"),
		SignaturePath: filepath.Join(directory, "SHA256SUMS.sig"), PublicKeyPath: filepath.Join(directory, "release-key.pem"),
	}

	tamperedSignature := ed25519.Sign(privateKey, []byte("other"))
	writeTestFile(t, directory, "SHA256SUMS.sig", tamperedSignature)
	if err := VerifyBundle(options); err == nil {
		t.Fatal("VerifyBundle() accepted a signature for different checksums")
	}
	duplicate := []byte(line + line)
	writeTestFile(t, directory, "SHA256SUMS", duplicate)
	writeTestFile(t, directory, "SHA256SUMS.sig", ed25519.Sign(privateKey, duplicate))
	if err := VerifyBundle(options); err == nil {
		t.Fatal("VerifyBundle() accepted duplicate archive checksum entries")
	}
}
