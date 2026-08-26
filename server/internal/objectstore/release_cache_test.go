package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLocalReleaseAssetCacheDownloadsGitHubSourceOnce(t *testing.T) {
	payload := []byte("private GitHub release asset")
	digest := sha256.Sum256(payload)
	var downloads atomic.Int32
	cache, err := NewLocalReleaseAssetCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalReleaseAssetCache() error = %v", err)
	}
	input := ReleaseAssetCacheInput{
		ObjectKey: "github/acme/wenzwork/assets/42/WenzWork.exe", FileName: "WenzWork.exe",
		FileSizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]),
	}
	opener := func(context.Context) (io.ReadCloser, error) {
		downloads.Add(1)
		return io.NopCloser(strings.NewReader(string(payload))), nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		cached, err := cache.OpenFrom(t.Context(), input, opener)
		if err != nil {
			t.Fatalf("OpenFrom() attempt %d error = %v", attempt, err)
		}
		content, readErr := io.ReadAll(cached.File)
		cached.File.Close()
		if readErr != nil || string(content) != string(payload) {
			t.Fatalf("cached content = %q, error = %v", content, readErr)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("GitHub source downloads = %d, want 1", downloads.Load())
	}
}

func TestReleaseAssetCacheDownloadsValidatesAndReusesFile(t *testing.T) {
	payload := []byte("cached WenzWork installer")
	digest := sha256.Sum256(payload)
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/wenzwork-releases/releases/1.2.3/windows/x64/id/WenzWork.exe" {
			t.Fatalf("unexpected storage request: %s %s", request.Method, request.URL.String())
		}
		downloads.Add(1)
		response.Header().Set("Content-Length", "25")
		_, _ = response.Write(payload)
	}))
	defer server.Close()

	cache, err := NewReleaseAssetCache(S3Config{
		Endpoint: server.URL, Region: "test-1", Bucket: "wenzwork-releases",
		AccessKeyID: "access", SecretAccessKey: "secret", AddressingStyle: S3AddressingStylePath,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("NewReleaseAssetCache() error = %v", err)
	}
	input := ReleaseAssetCacheInput{
		ObjectKey: "releases/1.2.3/windows/x64/id/WenzWork.exe", FileName: "WenzWork.exe",
		FileSizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]),
	}
	for attempt := 0; attempt < 2; attempt++ {
		cached, err := cache.Open(t.Context(), input)
		if err != nil {
			t.Fatalf("Open() attempt %d error = %v", attempt, err)
		}
		content, err := io.ReadAll(cached.File)
		cached.File.Close()
		if err != nil || string(content) != string(payload) {
			t.Fatalf("cached content = %q, error = %v", content, err)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("storage downloads = %d, want 1", downloads.Load())
	}
}

func TestReleaseAssetCacheRejectsFileAsCacheRoot(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "cache-file")
	if err := os.WriteFile(cachePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write cache root fixture: %v", err)
	}
	_, err := NewReleaseAssetCache(S3Config{
		Endpoint: "http://localhost:9000", Region: "test-1", Bucket: "wenzwork-releases",
		AccessKeyID: "access", SecretAccessKey: "secret", AddressingStyle: S3AddressingStylePath,
	}, cachePath)
	if err == nil {
		t.Fatal("NewReleaseAssetCache() accepted a regular file as cache root")
	}
}
