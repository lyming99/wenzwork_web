package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayhost"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

func TestConfigCheckVerifiesCertificateScope(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	authority, err := relayidentity.LoadOrCreateDevelopmentCA(filepath.Join(root, "ca-source"), now)
	if err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(root, "identity", "identity.key")
	privateKey, _, err := relayidentity.LoadOrCreatePrivateKey(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	installationID, cellID := uuid.New(), uuid.New()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	issued, err := authority.IssueNode(
		publicKey, installationID.String(), cellID.String(), relayidentity.Thumbprint(publicKey), now, time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath := filepath.Join(root, "tls", "node.crt")
	caPath := filepath.Join(root, "tls", "ca.crt")
	if err := relayhost.WriteCredential(certificatePath, issued.CertificatePEM, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := relayhost.WriteCredential(caPath, issued.CAPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	config := relayhost.Config{
		InstallationID: installationID, CellID: cellID, Version: "v1.2.3", ProtocolVersion: 2,
		DirectoryURL: "https://directory.example.test:9443", ListenAddress: "127.0.0.1:18443", HealthAddress: "127.0.0.1:19090",
		IdentityPrivateKeyFile: identityPath, CertificateFile: certificatePath, CACertificateFile: caPath,
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := relayhost.Save(configPath, config); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runConfigCheck([]string{"--config-file", configPath}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "valid") {
		t.Fatalf("config check output = %q", output.String())
	}

	config.CellID = uuid.New()
	wrongConfigPath := filepath.Join(root, "wrong-config.yaml")
	if err := relayhost.Save(wrongConfigPath, config); err != nil {
		t.Fatal(err)
	}
	if err := runConfigCheck([]string{"--config-file", wrongConfigPath}, io.Discard); err == nil {
		t.Fatal("config check accepted a certificate scoped to another Cell")
	}
}

func TestValidateControlURL(t *testing.T) {
	for _, valid := range []string{"https://control.example.test", "https://control.example.test/base", "http://control.example.test:8080", "http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		if _, err := validateControlURL(valid); err != nil {
			t.Errorf("validateControlURL(%q) error = %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "control.example.test", "https://user:pass@control.example.test", "https://control.example.test?token=secret", "ftp://control.example.test"} {
		if _, err := validateControlURL(invalid); err == nil {
			t.Errorf("validateControlURL(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestReadEnrollmentTokenFromStdin(t *testing.T) {
	token := strings.Repeat("t", 43)
	got, err := readEnrollmentToken(strings.NewReader(token+"\n"), io.Discard, true, "")
	if err != nil || got != token {
		t.Fatalf("readEnrollmentToken() = %q, %v", got, err)
	}
	for _, invalid := range []string{"short", strings.Repeat("x", 129), strings.Repeat("x", 42) + " bad"} {
		if _, err := readEnrollmentToken(strings.NewReader(invalid), io.Discard, true, ""); err == nil {
			t.Errorf("readEnrollmentToken accepted %q", invalid)
		}
	}
}

func TestEnrollDoesNotFollowRedirectWithCredential(t *testing.T) {
	var redirectedRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		redirectedRequests.Add(1)
		response.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(destination.Close)

	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)
	baseURL, _ := url.Parse(source.URL)
	_, err := enroll(context.Background(), baseURL, strings.Repeat("s", 43), relaymanagement.EnrollmentRequest{})
	if err == nil {
		t.Fatal("enroll() followed or accepted an HTTP redirect")
	}
	if redirectedRequests.Load() != 0 {
		t.Fatalf("redirect destination received %d requests; Enrollment credential could leak", redirectedRequests.Load())
	}
}

func TestEnrollUsesAuthorizationHeaderWithoutQueryOrCookie(t *testing.T) {
	token := strings.Repeat("q", 43)
	installationID := uuid.New()
	cellID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.RawQuery != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "Enrollment "+token {
			t.Errorf("enrollment transport query=%q cookie=%q authorization=%q", request.URL.RawQuery, request.Header.Get("Cookie"), request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(response).Encode(relaymanagement.EnrollmentResult{InstallationID: installationID, CellID: cellID})
	}))
	t.Cleanup(server.Close)
	baseURL, _ := url.Parse(server.URL)
	result, err := enroll(context.Background(), baseURL, token, relaymanagement.EnrollmentRequest{})
	if err != nil || result.InstallationID != installationID || result.CellID != cellID {
		t.Fatalf("enroll() = %+v, %v", result, err)
	}
}
