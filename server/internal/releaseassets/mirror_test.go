package releaseassets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMirrorClientReadsLatestReleaseAndResolvesSameOriginAsset(t *testing.T) {
	payload := []byte("mirror installer")
	digest := sha256.Sum256(payload)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/mirror/api/v1/releases/latest":
			if request.URL.Query().Get("project") != "desktop" || request.URL.Query().Get("channel") != "stable" {
				t.Fatalf("catalog query = %s", request.URL.RawQuery)
			}
			if request.Header.Get("Cache-Control") != "no-cache" {
				t.Fatalf("Cache-Control = %q", request.Header.Get("Cache-Control"))
			}
			_, _ = fmt.Fprintf(response, `{
                  "project":"desktop","version":"1.2.3","channel":"stable",
                  "title":"WenzWork 1.2.3","summary":"Fast release","releaseNotes":"Release notes",
                  "publishedAt":"2026-08-22T08:00:00Z","assets":[{
                    "fileName":"WenzWork-windows-x64.exe","fileSizeBytes":%d,"sha256":"%s",
                    "signatureStatus":"valid","downloadUrl":"/api/v1/release-assets/00000000-0000-0000-0000-000000000001/download",
                    "platform":"windows","architecture":"x64"
                  }]}`, len(payload), hex.EncodeToString(digest[:]))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client, err := newMirrorClient(server.URL+"/mirror/", server.Client(), true)
	if err != nil {
		t.Fatalf("newMirrorClient() error = %v", err)
	}
	release, err := client.Latest(t.Context(), "desktop")
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if client.BaseURL() != server.URL+"/mirror" || release.Version != "1.2.3" || len(release.Assets) != 1 {
		t.Fatalf("Latest() = %+v, base URL = %q", release, client.BaseURL())
	}
	if release.Assets[0].DownloadURL != server.URL+"/api/v1/release-assets/00000000-0000-0000-0000-000000000001/download" {
		t.Fatalf("asset URL = %q", release.Assets[0].DownloadURL)
	}
}

func TestMirrorClientRejectsCrossOriginAndInvalidCatalogAssets(t *testing.T) {
	for _, downloadURL := range []string{
		"https://other.example.test/api/v1/release-assets/id/download",
		"/api/v1/release-assets/id/download?token=secret",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(response, `{
                  "project":"web","version":"1.2.3","channel":"stable","title":"Release",
                  "summary":"","releaseNotes":"","publishedAt":"2026-08-22T08:00:00Z","assets":[{
                    "fileName":"wenzwork-host-deployment-v1.2.3-linux-amd64.tar.gz","fileSizeBytes":42,
                    "sha256":"%s","signatureStatus":"valid","downloadUrl":%q,
                    "platform":"linux","architecture":"x64"
                  }]}`, strings.Repeat("a", 64), downloadURL)
		}))
		client, err := newMirrorClient(server.URL, server.Client(), true)
		if err != nil {
			t.Fatalf("newMirrorClient() error = %v", err)
		}
		_, got := client.Latest(t.Context(), "web")
		server.Close()
		if !errors.Is(got, ErrMirrorCatalogInvalid) {
			t.Errorf("Latest() error = %v, want ErrMirrorCatalogInvalid", got)
		}
	}
}

func TestServiceImportsLatestMirrorReleaseAsCachedLinkWithoutStorage(t *testing.T) {
	payload := []byte("mirror installer payload")
	digest := sha256.Sum256(payload)
	digestText := hex.EncodeToString(digest[:])
	var assetDownloads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/releases/latest":
			_, _ = fmt.Fprintf(response, `{
                  "project":"desktop","version":"1.2.3","channel":"stable","title":"Release",
                  "summary":"Summary","releaseNotes":"Notes","publishedAt":"2026-08-22T08:00:00Z","assets":[{
                    "fileName":"WenzWork-windows-x64.exe","fileSizeBytes":%d,"sha256":"%s",
                    "signatureStatus":"valid","downloadUrl":"/api/v1/release-assets/00000000-0000-0000-0000-000000000001/download",
                    "platform":"windows","architecture":"x64"
                  }]}`, len(payload), digestText)
		case "/api/v1/release-assets/00000000-0000-0000-0000-000000000001/download":
			assetDownloads.Add(1)
			response.Header().Set("Content-Disposition", `attachment; filename="WenzWork-windows-x64.exe"`)
			response.Header().Set("Content-Type", "application/octet-stream")
			_, _ = response.Write(payload)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	mirror, err := newMirrorClient(server.URL, server.Client(), true)
	if err != nil {
		t.Fatalf("newMirrorClient() error = %v", err)
	}
	service := &Service{
		newMirrorClient: func(string) (*MirrorClient, error) { return mirror, nil },
	}
	result, err := service.ImportLatestMirrorRelease(t.Context(), server.URL, "desktop")
	if err != nil {
		t.Fatalf("ImportLatestMirrorRelease() error = %v", err)
	}
	if result.Version != "1.2.3" || result.Project != "desktop" || len(result.Assets) != 1 {
		t.Fatalf("ImportLatestMirrorRelease() = %+v", result)
	}
	if assetDownloads.Load() != 0 {
		t.Fatalf("mirror assets downloaded during catalog import = %d, want 0", assetDownloads.Load())
	}
	asset := result.Assets[0]
	expectedURL := server.URL + "/api/v1/release-assets/00000000-0000-0000-0000-000000000001/download"
	if asset.Source != "mirror" || asset.SignatureStatus != "valid" || asset.SHA256 != digestText ||
		asset.DownloadURL != expectedURL || asset.ObjectKey != mirrorAssetObjectKey(expectedURL, asset.FileName) {
		t.Fatalf("linked mirror asset = %+v", asset)
	}
}

func TestMirrorClientRejectsPrivateProductionAddress(t *testing.T) {
	if _, err := NewMirrorClient("http://127.0.0.1:8080"); !errors.Is(err, ErrMirrorURLInvalid) {
		t.Fatalf("NewMirrorClient(private) error = %v, want ErrMirrorURLInvalid", err)
	}
}
