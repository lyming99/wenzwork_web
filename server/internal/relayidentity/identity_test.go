package relayidentity

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestEnrollmentProofIsBoundToTokenAndIdentity(t *testing.T) {
	publicKey, privateKey, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := EncodePublicKey(publicKey)
	proof := EnrollmentProof{
		InstallationID: "installation-1", CellID: "cell-1", PublicKey: encoded,
		TokenDigest: TokenDigest("token-1"), Nonce: "nonce-1", Timestamp: time.Unix(1_800_000_000, 0).UTC(),
	}
	signature, err := SignEnrollment(privateKey, proof)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnrollment(publicKey, proof, signature); err != nil {
		t.Fatalf("VerifyEnrollment() error = %v", err)
	}
	proof.TokenDigest = TokenDigest("token-2")
	if err := VerifyEnrollment(publicKey, proof, signature); err == nil {
		t.Fatal("VerifyEnrollment() accepted a proof replayed with another token")
	}
}

func TestIdentityFileIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "identity.key")
	first, created, err := LoadOrCreatePrivateKey(path)
	if err != nil || !created {
		t.Fatalf("first LoadOrCreatePrivateKey() = created %v, error %v", created, err)
	}
	second, created, err := LoadOrCreatePrivateKey(path)
	if err != nil || created {
		t.Fatalf("second LoadOrCreatePrivateKey() = created %v, error %v", created, err)
	}
	if !first.Equal(second) || len(first) != ed25519.PrivateKeySize {
		t.Fatal("identity changed after reload")
	}
}

func TestIdentityFileRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink requires optional Windows privileges")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target.key")
	if _, _, err := LoadOrCreatePrivateKey(target); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreatePrivateKey(link); err == nil {
		t.Fatal("LoadOrCreatePrivateKey() accepted a symlink")
	}
}

func TestDevelopmentCAIssuesBoundClientCertificate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	authority, err := LoadOrCreateDevelopmentCA(t.TempDir(), now)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, _, _ := Generate()
	issued, err := authority.IssueNode(publicKey, "installation-1", "cell-1", Thumbprint(publicKey), now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(issued.CertificatePEM)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	installationID, cellID, err := ParseIdentityURI(certificate.URIs[0])
	if err != nil || installationID != "installation-1" || cellID != "cell-1" {
		t.Fatalf("certificate identity = %q/%q, error %v", installationID, cellID, err)
	}
}
