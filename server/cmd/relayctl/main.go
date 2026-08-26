package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayhost"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrelease"
	"golang.org/x/term"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "relayctl:", err)
		os.Exit(1)
	}
}

func run(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return usageError()
	}
	switch arguments[0] {
	case "enroll":
		return runEnroll(arguments[1:], stdin, stdout, stderr)
	case "identity":
		if len(arguments) < 2 || arguments[1] != "show" {
			return usageError()
		}
		return runIdentityShow(arguments[2:], stdout)
	case "config":
		if len(arguments) < 2 || arguments[1] != "check" {
			return usageError()
		}
		return runConfigCheck(arguments[2:], stdout)
	case "release":
		if len(arguments) < 2 {
			return usageError()
		}
		switch arguments[1] {
		case "verify":
			return runReleaseVerify(arguments[2:], stdout, stderr)
		case "verify-bundle":
			return runReleaseVerifyBundle(arguments[2:], stdout, stderr)
		default:
			return usageError()
		}
	case "status", "health":
		return runHealth(arguments[0], arguments[1:], stdout)
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return nil
	default:
		return usageError()
	}
}

func runReleaseVerifyBundle(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("release verify-bundle", flag.ContinueOnError)
	flags.SetOutput(stderr)
	archive := flags.String("archive", "", "Relay archive path")
	checksums := flags.String("checksums", "", "signed SHA256SUMS path")
	signature := flags.String("signature", "", "SHA256SUMS Ed25519 signature path")
	publicKey := flags.String("public-key", "", "trusted Release public key path")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return usageError()
	}
	if err := relayrelease.VerifyBundle(relayrelease.BundleVerifyOptions{
		ArchivePath: *archive, ChecksumsPath: *checksums, SignaturePath: *signature, PublicKeyPath: *publicKey,
	}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Relay archive signature and SHA-256 verified.")
	return nil
}

func runReleaseVerify(arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("release verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "extracted Relay release root")
	manifest := flags.String("manifest", "release-manifest.json", "manifest path relative to the release root")
	expectedVersion := flags.String("expected-version", "", "expected Relay release version")
	expectedPlatform := flags.String("expected-platform", runtime.GOOS, "expected target platform")
	expectedArchitecture := flags.String("expected-architecture", runtime.GOARCH, "expected target architecture")
	protocolVersion := flags.Int("protocol-version", 1, "required Relay protocol version")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return usageError()
	}
	verified, err := relayrelease.Verify(relayrelease.VerifyOptions{
		Root: *root, ManifestPath: *manifest, Version: strings.TrimSpace(*expectedVersion),
		Platform: strings.TrimSpace(*expectedPlatform), Architecture: strings.TrimSpace(*expectedArchitecture),
		ProtocolVersion: *protocolVersion,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Relay release %s verified (key %s, protocol %d-%d).\n", verified.Version, verified.SigningKeyID, verified.ProtocolMin, verified.ProtocolMax)
	return nil
}

func runEnroll(arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	flags.SetOutput(stderr)
	controlURL := flags.String("control-url", "", "Control Plane base URL")
	installationRaw := flags.String("installation-id", "", "Installation UUID")
	tokenStdin := flags.Bool("token-stdin", false, "read the one-time token from standard input")
	tokenFile := flags.String("token-file", "", "read the one-time token from a 0600 file")
	identityFile := flags.String("identity-file", relayhost.DefaultIdentityFile, "Ed25519 private key path")
	certificateFile := flags.String("certificate-file", relayhost.DefaultCertificateFile, "node certificate path")
	caFile := flags.String("ca-file", relayhost.DefaultCAFile, "node CA certificate path")
	configFile := flags.String("config-file", relayhost.DefaultConfigFile, "Relay configuration path")
	releaseVersion := flags.String("release-version", "", "installed Relay version")
	protocolVersion := flags.Int("protocol-version", 1, "Relay protocol version")
	listenAddress := flags.String("listen-address", ":8443", "Relay WSS listener")
	healthAddress := flags.String("health-address", "127.0.0.1:19090", "local health listener")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return usageError()
	}
	if *tokenStdin == (*tokenFile != "") {
		return errors.New("select exactly one of --token-stdin or --token-file")
	}
	baseURL, err := validateControlURL(*controlURL)
	if err != nil {
		return err
	}
	installationID, err := uuid.Parse(strings.TrimSpace(*installationRaw))
	if err != nil {
		return errors.New("--installation-id must be a UUID")
	}
	if !relayrelease.SupportsTarget(runtime.GOOS, runtime.GOARCH) {
		return fmt.Errorf("enrollment is not supported on this host target: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	metadata, err := fetchBootstrapInstallation(context.Background(), baseURL, installationID)
	if err != nil {
		return err
	}
	if metadata.Platform != runtime.GOOS || metadata.Architecture != runtime.GOARCH ||
		!relayrelease.SupportsTarget(metadata.Platform, metadata.Architecture) {
		return fmt.Errorf("the Installation target %s/%s does not match this host %s/%s",
			metadata.Platform, metadata.Architecture, runtime.GOOS, runtime.GOARCH)
	}
	selectedVersion := strings.TrimSpace(*releaseVersion)
	if selectedVersion == "" {
		selectedVersion = metadata.ReleaseVersion
	}
	if selectedVersion == "" || (metadata.ReleaseVersion != "" && selectedVersion != metadata.ReleaseVersion) {
		return errors.New("the installed release does not match the Installation release")
	}
	if *protocolVersion < metadata.ProtocolMin || *protocolVersion > metadata.ProtocolMax {
		return errors.New("the installed protocol version is not allowed by the Installation")
	}
	for _, path := range []string{*identityFile, *certificateFile, *caFile, *configFile} {
		if err := prepareParent(path); err != nil {
			return err
		}
	}
	privateKey, _, err := relayidentity.LoadOrCreatePrivateKey(*identityFile)
	if err != nil {
		return err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	publicKeyEncoded, _ := relayidentity.EncodePublicKey(publicKey)
	token, err := readEnrollmentToken(stdin, stderr, *tokenStdin, *tokenFile)
	if err != nil {
		return err
	}
	defer zeroString(&token)
	nonceBytes := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return errors.New("could not generate enrollment nonce")
	}
	timestamp := time.Now().UTC().Truncate(time.Second)
	request := relaymanagement.EnrollmentRequest{
		InstallationID: installationID.String(), CellID: metadata.CellID.String(), PublicKey: publicKeyEncoded,
		Nonce: base64.RawURLEncoding.EncodeToString(nonceBytes), Timestamp: timestamp,
		Version: selectedVersion, ProtocolVersion: *protocolVersion,
		Addresses: localAddresses(), Capabilities: map[string]any{"os": runtime.GOOS, "architecture": runtime.GOARCH},
	}
	request.Signature, err = relayidentity.SignEnrollment(privateKey, relayidentity.EnrollmentProof{
		InstallationID: request.InstallationID, CellID: request.CellID, PublicKey: request.PublicKey,
		TokenDigest: relayidentity.TokenDigest(token), Nonce: request.Nonce, Timestamp: request.Timestamp,
	})
	if err != nil {
		return err
	}
	result, err := enroll(context.Background(), baseURL, token, request)
	if err != nil {
		return err
	}
	if result.InstallationID != installationID || result.CellID != metadata.CellID || result.IdentityThumbprint != relayidentity.Thumbprint(publicKey) {
		return errors.New("the enrollment response identity does not match this host")
	}
	if err := verifyIssuedCertificate([]byte(result.CertificatePEM), []byte(result.CertificateAuthorityPEM), publicKey, result); err != nil {
		return err
	}
	if err := relayhost.WriteCredential(*certificateFile, []byte(result.CertificatePEM), 0o640); err != nil {
		return err
	}
	if err := relayhost.WriteCredential(*caFile, []byte(result.CertificateAuthorityPEM), 0o644); err != nil {
		return err
	}
	config := relayhost.Config{
		InstallationID: installationID, CellID: metadata.CellID, Version: selectedVersion,
		ProtocolVersion: *protocolVersion, DirectoryURL: result.DirectoryURL,
		ListenAddress: *listenAddress, HealthAddress: *healthAddress,
		IdentityPrivateKeyFile: *identityFile, CertificateFile: *certificateFile, CACertificateFile: *caFile,
	}
	if err := relayhost.Save(*configFile, config); err != nil {
		return err
	}
	if *tokenFile != "" {
		if err := os.Remove(filepath.Clean(*tokenFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("enrollment succeeded, but the token file could not be removed: %w", err)
		}
	}
	fmt.Fprintf(stdout, "Enrollment succeeded.\nIdentity SHA-256: %s\nCertificate expires: %s\nNext: start wenzwork-relay and verify the fingerprint in the management console before activation.\n", result.IdentityThumbprint, result.CertificateExpiresAt.Format(time.RFC3339))
	return nil
}

func runIdentityShow(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("identity show", flag.ContinueOnError)
	identityFile := flags.String("identity-file", relayhost.DefaultIdentityFile, "Ed25519 private key path")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return usageError()
	}
	contents, err := os.ReadFile(filepath.Clean(*identityFile))
	if err != nil {
		return fmt.Errorf("read relay identity: %w", err)
	}
	privateKey, err := relayidentity.ParsePrivateKeyPEM(contents)
	if err != nil {
		return err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	encoded, _ := relayidentity.EncodePublicKey(publicKey)
	fmt.Fprintf(stdout, "Public key: %s\nSHA-256 fingerprint: %s\n", encoded, relayidentity.Thumbprint(publicKey))
	return nil
}

func runConfigCheck(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("config check", flag.ContinueOnError)
	configFile := flags.String("config-file", relayhost.DefaultConfigFile, "Relay configuration path")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return usageError()
	}
	config, err := relayhost.Load(*configFile)
	if err != nil {
		return err
	}
	privateKeyPEM, err := os.ReadFile(filepath.Clean(config.IdentityPrivateKeyFile))
	if err != nil {
		return fmt.Errorf("read identity private key: %w", err)
	}
	privateKey, err := relayidentity.ParsePrivateKeyPEM(privateKeyPEM)
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Clean(config.IdentityPrivateKeyFile))
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			return errors.New("identity private key permissions must be 0600")
		}
	}
	certificatePEM, err := os.ReadFile(filepath.Clean(config.CertificateFile))
	if err != nil {
		return fmt.Errorf("read node certificate: %w", err)
	}
	if _, err := tls.X509KeyPair(certificatePEM, privateKeyPEM); err != nil {
		return fmt.Errorf("node certificate does not match the identity key: %w", err)
	}
	caPEM, err := os.ReadFile(filepath.Clean(config.CACertificateFile))
	if err != nil {
		return fmt.Errorf("read node CA certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("node CA certificate is invalid")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	if err := verifyIssuedCertificate(certificatePEM, caPEM, publicKey, relaymanagement.EnrollmentResult{
		InstallationID: config.InstallationID, CellID: config.CellID,
		IdentityThumbprint: relayidentity.Thumbprint(publicKey),
	}); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Relay configuration and identity files are valid.")
	return nil
}

func runHealth(command string, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configFile := flags.String("config-file", relayhost.DefaultConfigFile, "Relay configuration path")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return usageError()
	}
	config, err := relayhost.Load(*configFile)
	if err != nil {
		return err
	}
	path := "/health/ready"
	if command == "status" {
		path = "/status"
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + config.HealthAddress + path)
	if err != nil {
		return fmt.Errorf("local Relay health request failed: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("local Relay is not ready (HTTP %d)", response.StatusCode)
	}
	_, _ = stdout.Write(body)
	return nil
}

func fetchBootstrapInstallation(ctx context.Context, baseURL *url.URL, installationID uuid.UUID) (relaymanagement.BootstrapInstallation, error) {
	target := *baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/api/v1/relay/bootstrap/node-installations/" + installationID.String()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	request.Header.Set("Accept", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return relaymanagement.BootstrapInstallation{}, fmt.Errorf("read bootstrap metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return relaymanagement.BootstrapInstallation{}, fmt.Errorf("bootstrap metadata was rejected (HTTP %d)", response.StatusCode)
	}
	var result relaymanagement.BootstrapInstallation
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return relaymanagement.BootstrapInstallation{}, errors.New("bootstrap metadata response is invalid")
	}
	return result, nil
}

func enroll(ctx context.Context, baseURL *url.URL, token string, input relaymanagement.EnrollmentRequest) (relaymanagement.EnrollmentResult, error) {
	body, _ := json.Marshal(input)
	target := *baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/api/v1/relay/bootstrap/enrollments"
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Enrollment "+token)
	response, err := (&http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Do(request)
	if err != nil {
		return relaymanagement.EnrollmentResult{}, fmt.Errorf("send enrollment request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return relaymanagement.EnrollmentResult{}, fmt.Errorf("enrollment was rejected (HTTP %d); generate a new one-time token", response.StatusCode)
	}
	var result relaymanagement.EnrollmentResult
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return relaymanagement.EnrollmentResult{}, errors.New("enrollment response is invalid")
	}
	return result, nil
}

func readEnrollmentToken(stdin io.Reader, stderr io.Writer, fromStdin bool, tokenFile string) (string, error) {
	var contents []byte
	var err error
	if fromStdin {
		if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			fmt.Fprint(stderr, "Enrollment Token: ")
			contents, err = term.ReadPassword(int(file.Fd()))
			fmt.Fprintln(stderr)
		} else {
			contents, err = io.ReadAll(io.LimitReader(stdin, 1024))
		}
	} else {
		path := filepath.Clean(tokenFile)
		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", fmt.Errorf("read enrollment token file metadata: %w", statErr)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("enrollment token file permissions must be 0600")
		}
		contents, err = os.ReadFile(path)
	}
	if err != nil {
		return "", errors.New("could not read Enrollment Token")
	}
	if len(contents) > 512 {
		return "", errors.New("Enrollment Token is invalid")
	}
	token := strings.TrimSpace(string(contents))
	for index := range contents {
		contents[index] = 0
	}
	if len(token) < 43 || len(token) > 128 || strings.ContainsAny(token, "\r\n \t") {
		return "", errors.New("Enrollment Token is invalid")
	}
	return token, nil
}

func validateControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("--control-url must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return parsed, nil
}

func verifyIssuedCertificate(certificatePEM, caPEM []byte, publicKey ed25519.PublicKey, result relaymanagement.EnrollmentResult) error {
	block, rest := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return errors.New("enrollment returned an invalid node certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("enrollment returned an invalid node certificate")
	}
	certificatePublicKey, ok := certificate.PublicKey.(ed25519.PublicKey)
	if !ok || !certificatePublicKey.Equal(publicKey) || len(certificate.URIs) != 1 {
		return errors.New("enrollment returned a node certificate for another identity")
	}
	installationRaw, cellRaw, err := relayidentity.ParseIdentityURI(certificate.URIs[0])
	if err != nil || installationRaw != result.InstallationID.String() || cellRaw != result.CellID.String() {
		return errors.New("enrollment returned a node certificate with incorrect scope")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("enrollment returned an invalid CA certificate")
	}
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("verify enrolled node certificate: %w", err)
	}
	return nil
}

func prepareParent(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return errors.New("output path is invalid")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	return nil
}

func localAddresses() []string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return []string{}
	}
	result := make([]string, 0, len(addresses))
	for _, address := range addresses {
		value := address.String()
		if value != "" && len(result) < 16 {
			result = append(result, value)
		}
	}
	return result
}

func zeroString(value *string) {
	if value != nil {
		*value = ""
	}
}

func usageError() error {
	return errors.New("usage: relayctl <enroll|identity show|config check|release verify|release verify-bundle|status|health|version> [options]")
}
