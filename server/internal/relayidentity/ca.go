package relayidentity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type IssuedCertificate struct {
	CertificatePEM []byte
	CAPEM          []byte
	SerialNumber   string
	SHA256         string
	NotBefore      time.Time
	NotAfter       time.Time
}

type IssuedServerCertificate struct {
	CertificatePEM []byte
	PrivateKeyPEM  []byte
	NotAfter       time.Time
}

type CertificateAuthority struct {
	certificate    *x509.Certificate
	privateKey     crypto.Signer
	certificatePEM []byte
}

func LoadCertificateAuthority(certificatePath, privateKeyPath string) (*CertificateAuthority, error) {
	certificatePEM, err := os.ReadFile(filepath.Clean(certificatePath))
	if err != nil {
		return nil, fmt.Errorf("read Relay CA certificate: %w", err)
	}
	privateKeyPEM, err := os.ReadFile(filepath.Clean(privateKeyPath))
	if err != nil {
		return nil, fmt.Errorf("read Relay CA private key: %w", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Clean(privateKeyPath))
		if err != nil || info.Mode().Perm()&0o027 != 0 {
			return nil, errors.New("Relay CA private key must not be group-writable or world-accessible")
		}
	}
	return ParseCertificateAuthority(certificatePEM, privateKeyPEM)
}

func ParseCertificateAuthority(certificatePEM, privateKeyPEM []byte) (*CertificateAuthority, error) {
	certificateBlock, rest := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("Relay CA certificate must contain one CERTIFICATE block")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil || !certificate.IsCA {
		return nil, errors.New("Relay CA certificate is invalid or not a CA")
	}
	keyBlock, keyRest := pem.Decode(privateKeyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(keyRest))) != 0 {
		return nil, errors.New("Relay CA key must contain one PKCS#8 PRIVATE KEY block")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse Relay CA private key: %w", err)
	}
	privateKey, ok := parsedKey.(crypto.Signer)
	publicKey, publicKeyOK := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || !publicKeyOK || !publicKey.Equal(privateKey.Public()) {
		return nil, errors.New("Relay CA certificate and private key do not match")
	}
	return &CertificateAuthority{certificate: certificate, privateKey: privateKey, certificatePEM: certificatePEM}, nil
}

// LoadOrCreateCertificateAuthority persists a Host-local CA so a standalone
// deployment does not require separately provisioned PKI. Operators can still
// provide an external certificate/key pair through LoadCertificateAuthority.
func LoadOrCreateCertificateAuthority(directory string, now time.Time) (*CertificateAuthority, error) {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if directory == "." || directory == "" {
		return nil, errors.New("Relay development CA directory is required")
	}
	certificatePath := filepath.Join(directory, "ca.crt")
	privateKeyPath := filepath.Join(directory, "ca.key")
	if _, err := os.Stat(certificatePath); err == nil {
		return LoadCertificateAuthority(certificatePath, privateKeyPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat Relay development CA: %w", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create Relay development CA directory: %w", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate Relay development CA: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "WenzWork Relay CA", Organization: []string{"WenzWork"}},
		NotBefore:    now.UTC().Add(-time.Minute), NotAfter: now.UTC().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create Relay development CA: %w", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal Relay development CA key: %w", err)
	}
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeExclusive(privateKeyPath, privateKeyPEM, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadCertificateAuthority(certificatePath, privateKeyPath)
		}
		return nil, err
	}
	if err := writeExclusive(certificatePath, certificatePEM, 0o644); err != nil {
		_ = os.Remove(privateKeyPath)
		return nil, err
	}
	return ParseCertificateAuthority(certificatePEM, privateKeyPEM)
}

// LoadOrCreateDevelopmentCA is retained for callers using the previous name.
func LoadOrCreateDevelopmentCA(directory string, now time.Time) (*CertificateAuthority, error) {
	return LoadOrCreateCertificateAuthority(directory, now)
}

func (authority *CertificateAuthority) IssueNode(publicKey ed25519.PublicKey, installationID, cellID, thumbprint string, now time.Time, lifetime time.Duration) (IssuedCertificate, error) {
	if authority == nil || authority.certificate == nil || authority.privateKey == nil {
		return IssuedCertificate{}, errors.New("Relay certificate authority is unavailable")
	}
	if len(publicKey) != ed25519.PublicKeySize || Thumbprint(publicKey) != thumbprint {
		return IssuedCertificate{}, errors.New("Relay certificate public key does not match thumbprint")
	}
	if lifetime < time.Minute || lifetime > 7*24*time.Hour {
		return IssuedCertificate{}, errors.New("Relay certificate lifetime is outside the allowed range")
	}
	identityURI, err := IdentityURI(installationID, cellID)
	if err != nil {
		return IssuedCertificate{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return IssuedCertificate{}, err
	}
	notBefore := now.UTC().Add(-time.Minute)
	notAfter := now.UTC().Add(lifetime)
	if notAfter.After(authority.certificate.NotAfter) {
		notAfter = authority.certificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "relay-installation:" + installationID, OrganizationalUnit: []string{"cell:" + cellID}},
		URIs:         []*url.URL{identityURI},
		NotBefore:    notBefore, NotAfter: notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, publicKey, authority.privateKey)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("issue Relay node certificate: %w", err)
	}
	digest := sha256.Sum256(der)
	return IssuedCertificate{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		CAPEM:          append([]byte(nil), authority.certificatePEM...),
		SerialNumber:   serial.Text(16), SHA256: hex.EncodeToString(digest[:]),
		NotBefore: notBefore, NotAfter: notAfter,
	}, nil
}

func (authority *CertificateAuthority) CAPEM() []byte {
	if authority == nil {
		return nil
	}
	return append([]byte(nil), authority.certificatePEM...)
}

// IssueServer creates a short-lived TLS server certificate for a Directory
// listener. Production normally supplies its own managed listener certificate;
// this method keeps local development fully mTLS without a committed key.
func (authority *CertificateAuthority) IssueServer(hosts []string, now time.Time, lifetime time.Duration) (IssuedServerCertificate, error) {
	if authority == nil || authority.certificate == nil || authority.privateKey == nil {
		return IssuedServerCertificate{}, errors.New("Relay certificate authority is unavailable")
	}
	if len(hosts) == 0 || len(hosts) > 16 || lifetime < time.Minute || lifetime > 30*24*time.Hour {
		return IssuedServerCertificate{}, errors.New("Relay Directory server certificate request is invalid")
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return IssuedServerCertificate{}, fmt.Errorf("generate Relay Directory key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return IssuedServerCertificate{}, err
	}
	notAfter := now.UTC().Add(lifetime)
	if notAfter.After(authority.certificate.NotAfter) {
		notAfter = authority.certificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "WenzWork Relay Directory"},
		NotBefore:    now.UTC().Add(-time.Minute), NotAfter: notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" || strings.ContainsAny(host, "/?#") {
			return IssuedServerCertificate{}, errors.New("invalid Relay Directory server name")
		}
		if address := net.ParseIP(host); address != nil {
			template.IPAddresses = append(template.IPAddresses, address)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, publicKey, authority.privateKey)
	if err != nil {
		return IssuedServerCertificate{}, fmt.Errorf("issue Relay Directory certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return IssuedServerCertificate{}, fmt.Errorf("marshal Relay Directory private key: %w", err)
	}
	return IssuedServerCertificate{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		NotAfter:       notAfter,
	}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func writeExclusive(path string, contents []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
