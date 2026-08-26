package releaseassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
)

func TestDeliveryServiceCachesAuthenticatedGitHubAsset(t *testing.T) {
	payload := []byte("private GitHub installer")
	digest := sha256.Sum256(payload)
	var downloads atomic.Int32
	var tokenLoads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/repos/acme/wenzwork/releases/assets/42":
			if request.Header.Get("Authorization") != "Bearer github_pat_test" || request.Header.Get("Accept") != "application/octet-stream" {
				t.Fatalf("GitHub asset headers = %v", request.Header)
			}
			downloads.Add(1)
			http.Redirect(response, request, server.URL+"/signed-download", http.StatusFound)
		case "/signed-download":
			_, _ = response.Write(payload)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	cache, err := objectstore.NewLocalReleaseAssetCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalReleaseAssetCache() error = %v", err)
	}
	service := &DeliveryService{
		cache: cache, tokenProvider: func(context.Context) (string, error) {
			tokenLoads.Add(1)
			return "github_pat_test", nil
		},
		apiBaseURL: server.URL, assetClient: server.Client(), redirectClient: server.Client(),
	}
	asset := DeliveryAsset{
		Source: "github", ObjectKey: "github/acme/wenzwork/assets/42/WenzWork.exe", FileName: "WenzWork.exe",
		FileSizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]),
	}
	for attempt := 0; attempt < 2; attempt++ {
		cached, err := service.Open(t.Context(), asset)
		if err != nil {
			t.Fatalf("Open() attempt %d error = %v", attempt, err)
		}
		content, readErr := io.ReadAll(cached.File)
		cached.File.Close()
		if readErr != nil || string(content) != string(payload) {
			t.Fatalf("cached content = %q, error = %v", content, readErr)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("GitHub downloads = %d, want 1", downloads.Load())
	}
	if tokenLoads.Load() != 1 {
		t.Fatalf("GitHub token loads = %d, want 1", tokenLoads.Load())
	}
}

func TestDeliveryServiceCachesMirrorLinkWithoutS3(t *testing.T) {
	payload := []byte("mirror installer")
	digest := sha256.Sum256(payload)
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/release-assets/id/download" || request.Header.Get("Accept") != "application/octet-stream" {
			t.Fatalf("mirror request = %s headers=%v", request.URL.String(), request.Header)
		}
		downloads.Add(1)
		response.Header().Set("Content-Disposition", `attachment; filename="WenzWork.exe"`)
		response.Header().Set("Content-Length", "16")
		_, _ = response.Write(payload)
	}))
	defer server.Close()

	cache, err := objectstore.NewLocalReleaseAssetCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalReleaseAssetCache() error = %v", err)
	}
	downloadURL := server.URL + "/api/v1/release-assets/id/download"
	service := &DeliveryService{cache: cache, mirrorInspector: newRemoteInspectorForTest(server.Client())}
	asset := DeliveryAsset{
		Source: "mirror", ObjectKey: mirrorAssetObjectKey(downloadURL, "WenzWork.exe"),
		DownloadURL: downloadURL, FileName: "WenzWork.exe", FileSizeBytes: int64(len(payload)),
		SHA256: hex.EncodeToString(digest[:]),
	}
	for attempt := 0; attempt < 2; attempt++ {
		cached, err := service.Open(t.Context(), asset)
		if err != nil {
			t.Fatalf("Open() attempt %d error = %v", attempt, err)
		}
		content, readErr := io.ReadAll(cached.File)
		_ = cached.File.Close()
		if readErr != nil || string(content) != string(payload) {
			t.Fatalf("cached content = %q, error = %v", content, readErr)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("mirror downloads = %d, want 1", downloads.Load())
	}
}

func TestDeliveryServiceOpensDurableLocalPushAssetWithoutS3(t *testing.T) {
	payload := []byte("local push installer")
	digest := sha256.Sum256(payload)
	localStore, err := objectstore.NewLocalReleaseAssetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := localStore.Upload(t.Context(), "desktop", objectstore.ReleaseAssetUploadInput{
		Version: "1.2.3", Platform: "windows", Architecture: "x64", FileName: "WenzWork.zip",
		FileSizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]), ContentType: "application/zip",
	}, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	cache, err := objectstore.NewLocalReleaseAssetCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDeliveryService(cache, nil).WithLocalStore(localStore)
	opened, err := service.Open(t.Context(), DeliveryAsset{
		Source: "local", ObjectKey: stored.ObjectKey, FileName: "WenzWork.zip",
		FileSizeBytes: int64(len(payload)), SHA256: hex.EncodeToString(digest[:]),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer opened.File.Close()
	contents, err := io.ReadAll(opened.File)
	if err != nil || string(contents) != string(payload) {
		t.Fatalf("local contents = %q, error = %v", contents, err)
	}
}

func TestDeliveryServiceRejectsForgedMirrorReference(t *testing.T) {
	cache, err := objectstore.NewLocalReleaseAssetCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalReleaseAssetCache() error = %v", err)
	}
	service := &DeliveryService{cache: cache}
	_, err = service.Open(t.Context(), DeliveryAsset{
		Source: "mirror", ObjectKey: "mirror/" + strings.Repeat("a", 64) + "/WenzWork.exe",
		DownloadURL: "https://mirror.example.test/api/v1/release-assets/id/download",
		FileName:    "WenzWork.exe", FileSizeBytes: 42, SHA256: strings.Repeat("b", 64),
	})
	if !errors.Is(err, ErrMirrorAssetMismatch) {
		t.Fatalf("Open() error = %v, want ErrMirrorAssetMismatch", err)
	}
}

func TestDeliveryServiceResolvesPrivateGitHubAssetWithoutExposingToken(t *testing.T) {
	const signedURL = "https://release-assets.githubusercontent.com/github-production-release-asset/signed?token=temporary"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/acme/wenzwork/releases/assets/42" {
			http.NotFound(response, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer github_pat_private" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Location", signedURL)
		response.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	redirectClient := server.Client()
	redirectClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	service := &DeliveryService{
		tokenProvider: func(context.Context) (string, error) { return "github_pat_private", nil },
		apiBaseURL:    server.URL, redirectClient: redirectClient,
	}
	target, err := service.GitHubRedirect(t.Context(), DeliveryAsset{
		Source: "github", ObjectKey: "github/acme/wenzwork/assets/42/WenzWork.exe", FileName: "WenzWork.exe",
	})
	if err != nil {
		t.Fatalf("GitHubRedirect() error = %v", err)
	}
	if target != signedURL || strings.Contains(target, "github_pat_private") {
		t.Fatalf("redirect target = %q", target)
	}
}

func TestDeliveryServiceSelectsTokenByAssetRepository(t *testing.T) {
	const signedURL = "https://release-assets.githubusercontent.com/github-production-release-asset/mobile-signed"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/acme/mobile/releases/assets/77" {
			http.NotFound(response, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer mobile_token" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Location", signedURL)
		response.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	redirectClient := server.Client()
	redirectClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var selectedRepository string
	service := &DeliveryService{
		repositoryTokenProvider: func(_ context.Context, repository string) (string, error) {
			selectedRepository = repository
			if repository == "acme/mobile" {
				return "mobile_token", nil
			}
			return "wrong_token", nil
		},
		apiBaseURL: server.URL, redirectClient: redirectClient,
	}
	target, err := service.GitHubRedirect(t.Context(), DeliveryAsset{
		Source: "github", ObjectKey: "github/acme/mobile/assets/77/WenzWork.apk", FileName: "WenzWork.apk",
	})
	if err != nil {
		t.Fatalf("GitHubRedirect() error = %v", err)
	}
	if selectedRepository != "acme/mobile" {
		t.Fatalf("selected repository = %q, want acme/mobile", selectedRepository)
	}
	if target != signedURL {
		t.Fatalf("redirect target = %q, want %q", target, signedURL)
	}
}

func TestDeliveryServiceRejectsForgedGitHubAssetReference(t *testing.T) {
	service := &DeliveryService{}
	_, err := service.GitHubRedirect(t.Context(), DeliveryAsset{
		Source: "github", ObjectKey: "github/acme/wenzwork/assets/not-a-number/WenzWork.exe", FileName: "WenzWork.exe",
	})
	if err != ErrGitHubAssetInvalid {
		t.Fatalf("GitHubRedirect() error = %v, want ErrGitHubAssetInvalid", err)
	}
}
