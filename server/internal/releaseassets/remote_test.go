package releaseassets

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRemoteInspectorDownloadsAndDetectsMetadata(t *testing.T) {
	payload := []byte("WenzWork remote release asset\n")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("Accept-Encoding = %q", request.Header.Get("Accept-Encoding"))
		}
		response.Header().Set("Content-Disposition", `attachment; filename="WenzWork-windows-x64.exe"`)
		response.Header().Set("Content-Type", "application/vnd.microsoft.portable-executable")
		_, _ = response.Write(payload)
	}))
	defer server.Close()

	result, err := newRemoteInspectorForTest(server.Client()).Probe(t.Context(), server.URL+"/download?id=1")
	if err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	digest := sha256.Sum256(payload)
	if result.FileName != "WenzWork-windows-x64.exe" || result.FileSizeBytes != int64(len(payload)) ||
		result.SHA256 != hex.EncodeToString(digest[:]) || result.Platform != "windows" || result.Architecture != "x64" {
		t.Fatalf("Probe() = %+v", result)
	}
	if result.DownloadURL != server.URL+"/download?id=1" {
		t.Fatalf("DownloadURL = %q", result.DownloadURL)
	}
}

func TestRemoteInspectorRejectsPrivateAndInvalidURLs(t *testing.T) {
	inspector := NewRemoteInspector()
	for _, candidate := range []string{
		"file:///tmp/release", "https://user:password@example.com/file", "http://localhost/file",
		"http://127.0.0.1/file", "http://[::1]/file", "https://example.com/file#fragment",
	} {
		if _, err := inspector.Probe(t.Context(), candidate); err == nil {
			t.Fatalf("Probe(%q) error = nil", candidate)
		}
	}
}

func TestInferTarget(t *testing.T) {
	tests := []struct {
		name, platform, architecture string
	}{
		{"WenzWork-Setup-windows-amd64.exe", "windows", "x64"},
		{"WenzWork-darwin-arm64.dmg", "macos", "arm64"},
		{"wenzwork-linux.AppImage", "linux", "universal"},
		{"README.txt", "", ""},
	}
	for _, test := range tests {
		platform, architecture := InferTarget(test.name)
		if platform != test.platform || architecture != test.architecture {
			t.Errorf("InferTarget(%q) = %s/%s, want %s/%s", test.name, platform, architecture, test.platform, test.architecture)
		}
	}
	if !validSHA256(strings.Repeat("a", 64)) {
		t.Fatal("validSHA256 rejected a valid digest")
	}
}
