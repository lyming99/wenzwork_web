package relayrelease

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maximumChecksumsBytes = 1 << 20

type BundleVerifyOptions struct {
	ArchivePath   string
	ChecksumsPath string
	SignaturePath string
	PublicKeyPath string
}

// VerifyBundle authenticates SHA256SUMS with the pinned Ed25519 Release key,
// then verifies the selected archive digest. It is implemented in Go so the
// same bootstrap verifier works on Linux, Windows and macOS without relying on
// platform-specific OpenSSL behavior.
func VerifyBundle(options BundleVerifyOptions) error {
	archivePath := filepath.Clean(strings.TrimSpace(options.ArchivePath))
	checksumsPath := filepath.Clean(strings.TrimSpace(options.ChecksumsPath))
	signaturePath := filepath.Clean(strings.TrimSpace(options.SignaturePath))
	publicKeyPath := filepath.Clean(strings.TrimSpace(options.PublicKeyPath))
	if archivePath == "." || checksumsPath == "." || signaturePath == "." || publicKeyPath == "." {
		return errors.New("archive, checksums, signature, and public key are required")
	}
	for _, path := range []string{archivePath, checksumsPath, signaturePath, publicKeyPath} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Release verification input is missing or unsafe: %s", filepath.Base(path))
		}
	}

	checksums, err := readLimitedFile(checksumsPath, maximumChecksumsBytes)
	if err != nil {
		return fmt.Errorf("read SHA256SUMS: %w", err)
	}
	signature, err := readLimitedFile(signaturePath, ed25519.SignatureSize)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("SHA256SUMS signature is invalid")
	}
	publicKeyPEM, err := readLimitedFile(publicKeyPath, 1<<16)
	if err != nil {
		return fmt.Errorf("read Release public key: %w", err)
	}
	publicKey, err := parseReleasePublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	if !ed25519.Verify(publicKey, checksums, signature) {
		return errors.New("SHA256SUMS signature verification failed")
	}

	archiveName := filepath.Base(archivePath)
	if archiveName == "." || archiveName == "" || strings.ContainsAny(archiveName, "\r\n") {
		return errors.New("Relay archive name is invalid")
	}
	expected, err := checksumForArchive(checksums, archiveName)
	if err != nil {
		return err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open Relay archive: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, archive)
	closeErr := archive.Close()
	if copyErr != nil || closeErr != nil {
		return errors.New("read Relay archive for SHA-256 verification")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return errors.New("Relay archive SHA-256 verification failed")
	}
	return nil
}

func readLimitedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("file exceeds the verification size limit")
	}
	return contents, nil
}

func parseReleasePublicKey(contents []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("Release signing public key must contain one PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	publicKey, ok := parsed.(ed25519.PublicKey)
	if err != nil || !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("Release signing public key must be Ed25519")
	}
	return publicKey, nil
}

func checksumForArchive(contents []byte, archiveName string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(contents)))
	found := ""
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if len(line) < 67 || line[0] == '\\' {
			continue
		}
		digest := line[:64]
		separator := line[64:66]
		name := line[66:]
		if separator != "  " && separator != " *" {
			continue
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || name != archiveName {
			continue
		}
		if found != "" {
			return "", errors.New("SHA256SUMS contains duplicate Relay archive entries")
		}
		found = strings.ToLower(digest)
	}
	if err := scanner.Err(); err != nil {
		return "", errors.New("parse SHA256SUMS")
	}
	if found == "" {
		return "", errors.New("SHA256SUMS does not contain the selected Relay archive")
	}
	return found, nil
}
