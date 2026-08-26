package relayidentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const enrollmentContext = "wenzwork-relay-enrollment-v1"

// EnrollmentProof is the canonical, replay-resistant statement signed by a
// Relay installation during first enrollment.
type EnrollmentProof struct {
	InstallationID string
	CellID         string
	PublicKey      string
	TokenDigest    string
	Nonce          string
	Timestamp      time.Time
}

func Generate() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate relay identity: %w", err)
	}
	return publicKey, privateKey, nil
}

// LoadOrCreatePrivateKey keeps an existing installation identity stable. The
// caller must arrange ownership by the dedicated service account; this helper
// enforces owner-only file permissions on platforms that support them.
func LoadOrCreatePrivateKey(path string) (ed25519.PrivateKey, bool, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, false, errors.New("relay identity path is required")
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, false, errors.New("relay identity must be a regular file")
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, false, fmt.Errorf("read relay identity: %w", readErr)
		}
		privateKey, parseErr := ParsePrivateKeyPEM(contents)
		if parseErr != nil {
			return nil, false, fmt.Errorf("load relay identity: %w", parseErr)
		}
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return nil, false, fmt.Errorf("secure relay identity: %w", chmodErr)
		}
		return privateKey, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("read relay identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create relay identity directory: %w", err)
	}
	_, privateKey, err := Generate()
	if err != nil {
		return nil, false, err
	}
	encoded, err := MarshalPrivateKeyPEM(privateKey)
	if err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return LoadOrCreatePrivateKey(path)
	}
	if err != nil {
		return nil, false, fmt.Errorf("create relay identity: %w", err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("write relay identity: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("close relay identity: %w", err)
	}
	return privateKey, true, nil
}

func MarshalPrivateKeyPEM(privateKey ed25519.PrivateKey) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal relay identity: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func ParsePrivateKeyPEM(contents []byte) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("identity must contain one PKCS#8 PRIVATE KEY block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 identity: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("identity is not an Ed25519 private key")
	}
	return privateKey, nil
}

func ParsePublicKeyPEM(contents []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("identity must contain one PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX identity: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("identity is not an Ed25519 public key")
	}
	return publicKey, nil
}

func EncodePublicKey(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", errors.New("invalid Ed25519 public key")
	}
	return base64.RawURLEncoding.EncodeToString(publicKey), nil
}

func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

func Thumbprint(publicKey ed25519.PublicKey) string {
	digest := sha256.Sum256(publicKey)
	return hex.EncodeToString(digest[:])
}

func TokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func SignEnrollment(privateKey ed25519.PrivateKey, proof EnrollmentProof) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("invalid Ed25519 private key")
	}
	message, err := enrollmentMessage(proof)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, message)), nil
}

func VerifyEnrollment(publicKey ed25519.PublicKey, proof EnrollmentProof, signature string) error {
	message, err := enrollmentMessage(proof)
	if err != nil {
		return err
	}
	rawSignature, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(rawSignature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, message, rawSignature) {
		return errors.New("invalid enrollment proof")
	}
	return nil
}

func enrollmentMessage(proof EnrollmentProof) ([]byte, error) {
	fields := []string{
		strings.TrimSpace(proof.InstallationID), strings.TrimSpace(proof.CellID),
		strings.TrimSpace(proof.PublicKey), strings.TrimSpace(proof.TokenDigest),
		strings.TrimSpace(proof.Nonce), proof.Timestamp.UTC().Format(time.RFC3339),
	}
	for _, field := range fields {
		if field == "" || strings.ContainsAny(field, "\r\n") {
			return nil, errors.New("invalid enrollment proof fields")
		}
	}
	return []byte(enrollmentContext + "\n" + strings.Join(fields, "\n")), nil
}

func IdentityURI(installationID, cellID string) (*url.URL, error) {
	installationID = strings.TrimSpace(installationID)
	cellID = strings.TrimSpace(cellID)
	if installationID == "" || cellID == "" || strings.ContainsAny(installationID+cellID, "/?#") {
		return nil, errors.New("invalid relay certificate identity")
	}
	return url.Parse("spiffe://wenzwork/relay/cell/" + url.PathEscape(cellID) + "/installation/" + url.PathEscape(installationID))
}

func ParseIdentityURI(identity *url.URL) (installationID, cellID string, err error) {
	if identity == nil || identity.Scheme != "spiffe" || identity.Host != "wenzwork" || identity.RawQuery != "" || identity.Fragment != "" {
		return "", "", errors.New("invalid relay certificate URI")
	}
	parts := strings.Split(strings.Trim(identity.EscapedPath(), "/"), "/")
	if len(parts) != 5 || parts[0] != "relay" || parts[1] != "cell" || parts[3] != "installation" {
		return "", "", errors.New("invalid relay certificate URI")
	}
	cellID, err = url.PathUnescape(parts[2])
	if err != nil {
		return "", "", errors.New("invalid relay certificate Cell ID")
	}
	installationID, err = url.PathUnescape(parts[4])
	if err != nil || cellID == "" || installationID == "" {
		return "", "", errors.New("invalid relay certificate Installation ID")
	}
	return installationID, cellID, nil
}
