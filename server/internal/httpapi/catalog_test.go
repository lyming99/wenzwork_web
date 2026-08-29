package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/catalog"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
	"github.com/wenzwork/wenzwork-web/server/internal/releaseassets"
)

type fakeCatalogReader struct {
	plans       []catalog.PricingPlan
	latest      catalog.Release
	latestErr   error
	releases    []catalog.Release
	releasesErr error
	asset       catalog.ReleaseAssetDownload
	assetErr    error
	settings    catalog.ReleaseDeliverySettings
	settingsErr error
	settingsHit int
}

func (f *fakeCatalogReader) ListPricingPlans(context.Context) ([]catalog.PricingPlan, error) {
	return f.plans, nil
}

func (f *fakeCatalogReader) LatestRelease(context.Context, catalog.ReleaseFilter) (catalog.Release, error) {
	return f.latest, f.latestErr
}

func (f *fakeCatalogReader) ListReleases(context.Context, catalog.ReleaseFilter) ([]catalog.Release, error) {
	return f.releases, f.releasesErr
}

func (f *fakeCatalogReader) ReleaseAssetDownload(context.Context, uuid.UUID) (catalog.ReleaseAssetDownload, error) {
	return f.asset, f.assetErr
}

func (f *fakeCatalogReader) GetReleaseDeliverySettings(context.Context) (catalog.ReleaseDeliverySettings, error) {
	f.settingsHit++
	return f.settings, f.settingsErr
}

func newCatalogTestRouter(reader CatalogReader) http.Handler {
	return NewRouter(Dependencies{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Catalog: reader,
	})
}

type fakeReleaseAssetDownloadService struct {
	filePath    string
	err         error
	redirectURL string
	input       releaseassets.DeliveryAsset
}

func (f *fakeReleaseAssetDownloadService) Open(_ context.Context, input releaseassets.DeliveryAsset) (objectstore.CachedReleaseAsset, error) {
	f.input = input
	if f.err != nil {
		return objectstore.CachedReleaseAsset{}, f.err
	}
	file, err := os.Open(f.filePath)
	return objectstore.CachedReleaseAsset{File: file, ModTime: time.Unix(1, 0)}, err
}

func (f *fakeReleaseAssetDownloadService) GitHubRedirect(_ context.Context, input releaseassets.DeliveryAsset) (string, error) {
	f.input = input
	return f.redirectURL, f.err
}

func TestPricingPlansReturnsPublishedCatalogShape(t *testing.T) {
	zero := int64(0)
	original := int64(5900)
	trafficLimit := int64(10)
	router := newCatalogTestRouter(&fakeCatalogReader{plans: []catalog.PricingPlan{{
		Code:                  "free",
		Name:                  "Free",
		Description:           "Start writing",
		PriceMinor:            &zero,
		OriginalPriceMinor:    &original,
		Currency:              "CNY",
		BillingPeriod:         "free",
		Features:              []string{"Markdown"},
		RemoteAccessEnabled:   true,
		DeviceLimit:           3,
		MonthlyTrafficLimitGB: &trafficLimit,
	}}})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/pricing-plans", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if response.Header().Get("Cache-Control") != publicCacheControl {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	for _, expected := range []string{`"items"`, `"code":"free"`, `"priceMinor":0`, `"originalPriceMinor":5900`, `"features":["Markdown"]`, `"remoteAccessEnabled":true`, `"deviceLimit":3`, `"monthlyTrafficLimitGb":10`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("body = %s, want %s", response.Body.String(), expected)
		}
	}
}

func TestLatestReleaseMapsAbsentAndValidatesFilters(t *testing.T) {
	router := newCatalogTestRouter(&fakeCatalogReader{latestErr: catalog.ErrReleaseNotFound})

	notFound := httptest.NewRecorder()
	router.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/api/v1/releases/latest", nil))
	if notFound.Code != http.StatusNotFound || !strings.Contains(notFound.Body.String(), `"code":"release_not_found"`) {
		t.Fatalf("not-found response = %d %s", notFound.Code, notFound.Body.String())
	}

	invalid := httptest.NewRecorder()
	router.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/api/v1/releases?platform=plan9", nil))
	if invalid.Code != http.StatusBadRequest || !strings.Contains(invalid.Body.String(), `"code":"invalid_platform"`) {
		t.Fatalf("invalid response = %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestLatestReleaseReturnsAssetsAndCacheHeader(t *testing.T) {
	assetID := uuid.New()
	router := newCatalogTestRouter(&fakeCatalogReader{latest: catalog.Release{
		ID:           uuid.New(),
		Version:      "1.2.3",
		Channel:      "stable",
		Title:        "WenzWork 1.2.3",
		PublishedAt:  time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
		Assets:       []catalog.ReleaseAsset{{ID: assetID, Platform: "windows", Architecture: "x64", FileSizeBytes: 42, SHA256: strings.Repeat("a", 64), DownloadURL: "/download"}},
		ReleaseNotes: "Safe release",
	}})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/releases/latest?platform=windows&architecture=x64", nil))

	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != releaseCatalogCacheControl {
		t.Fatalf("response = %d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"version":"1.2.3"`) || !strings.Contains(response.Body.String(), `"platform":"windows"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestReleaseListRequiresCacheRevalidation(t *testing.T) {
	router := newCatalogTestRouter(&fakeCatalogReader{releases: []catalog.Release{{
		ID: uuid.New(), Version: "0.2.9", Channel: "stable", Title: "WenzWork v0.2.9",
		PublishedAt: time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC),
	}}})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/releases?project=web", nil))

	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != releaseCatalogCacheControl {
		t.Fatalf("response = %d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"version":"0.2.9"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestAssetDownloadRequiresPublishedSafeTarget(t *testing.T) {
	assetID := uuid.New()
	asset := catalog.ReleaseAssetDownload{
		ObjectKey: "releases/1.2.3/windows/x64/id/wenzwork.exe", FileName: "wenzwork.exe",
		FileSizeBytes: 42, SHA256: strings.Repeat("a", 64),
	}
	router := newCatalogTestRouter(&fakeCatalogReader{
		asset: asset, settings: catalog.ReleaseDeliverySettings{
			DownloadMode: catalog.ReleaseDownloadS3Redirect, S3URLPrefix: "https://downloads.example.test",
		},
	})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != "https://downloads.example.test/releases/1.2.3/windows/x64/id/wenzwork.exe" {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}

	jsonRequest := httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil)
	jsonRequest.Header.Set("Accept", "application/json")
	jsonResponse := httptest.NewRecorder()
	router.ServeHTTP(jsonResponse, jsonRequest)
	if jsonResponse.Code != http.StatusOK || !strings.Contains(jsonResponse.Body.String(), `"url":"https://downloads.example.test/releases/1.2.3/windows/x64/id/wenzwork.exe"`) {
		t.Fatalf("JSON response = %d %s", jsonResponse.Code, jsonResponse.Body.String())
	}

	unsafeRouter := newCatalogTestRouter(&fakeCatalogReader{asset: asset, settings: catalog.ReleaseDeliverySettings{DownloadMode: catalog.ReleaseDownloadS3Redirect, S3URLPrefix: "javascript:alert(1)"}})
	unsafe := httptest.NewRecorder()
	unsafeRouter.ServeHTTP(unsafe, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if unsafe.Code != http.StatusServiceUnavailable || !strings.Contains(unsafe.Body.String(), `"code":"asset_url_invalid"`) {
		t.Fatalf("unsafe response = %d %s", unsafe.Code, unsafe.Body.String())
	}

	missingRouter := newCatalogTestRouter(&fakeCatalogReader{assetErr: errors.New("database down")})
	missing := httptest.NewRecorder()
	missingRouter.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if missing.Code != http.StatusServiceUnavailable {
		t.Fatalf("dependency failure status = %d", missing.Code)
	}

	notFoundReader := &fakeCatalogReader{assetErr: catalog.ErrAssetNotFound}
	notFoundRouter := newCatalogTestRouter(notFoundReader)
	notFound := httptest.NewRecorder()
	notFoundRouter.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if notFound.Code != http.StatusNotFound || !strings.Contains(notFound.Body.String(), `"code":"asset_not_found"`) {
		t.Fatalf("not-found response = %d %s", notFound.Code, notFound.Body.String())
	}
	if notFoundReader.settingsHit != 0 {
		t.Fatalf("delivery settings queried after asset lookup failed")
	}

	legacyReader := &fakeCatalogReader{asset: catalog.ReleaseAssetDownload{
		ObjectKey: "external/legacy/WenzWork.exe", DownloadURL: "https://legacy.example.test/WenzWork.exe",
	}}
	legacyRouter := newCatalogTestRouter(legacyReader)
	legacy := httptest.NewRecorder()
	legacyRouter.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if legacy.Code != http.StatusFound || legacy.Header().Get("Location") != "https://legacy.example.test/WenzWork.exe" {
		t.Fatalf("legacy response = %d location=%q body=%s", legacy.Code, legacy.Header().Get("Location"), legacy.Body.String())
	}
	if legacyReader.settingsHit != 0 {
		t.Fatal("delivery settings queried for a legacy external asset")
	}
}

func TestSuccessfulAssetDownloadRecordsClientIPButHEADDoesNot(t *testing.T) {
	assetID := uuid.New()
	statistics := &fakeAnalyticsService{}
	reader := &fakeCatalogReader{
		asset: catalog.ReleaseAssetDownload{
			ObjectKey: "releases/1.2.3/windows/x64/id/wenzwork.exe", FileName: "wenzwork.exe",
			FileSizeBytes: 42, SHA256: strings.Repeat("a", 64),
		},
		settings: catalog.ReleaseDeliverySettings{
			DownloadMode: catalog.ReleaseDownloadS3Redirect, S3URLPrefix: "https://downloads.example.test",
		},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Catalog: reader, Analytics: statistics,
		TrustedProxies: []string{"127.0.0.1/32"},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil)
	request.Header.Set("User-Agent", "Download Browser")
	request.Header.Set("X-Forwarded-For", "203.0.113.90")
	request.RemoteAddr = "127.0.0.1:45678"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusFound || statistics.downloadCalls != 1 || statistics.download.AssetID != assetID || statistics.download.ClientIP != "203.0.113.90" || statistics.download.UserAgent != "Download Browser" {
		t.Fatalf("response=%d download event=%+v calls=%d", response.Code, statistics.download, statistics.downloadCalls)
	}

	head := httptest.NewRecorder()
	router.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if head.Code != http.StatusFound || statistics.downloadCalls != 1 {
		t.Fatalf("HEAD response=%d download calls=%d", head.Code, statistics.downloadCalls)
	}
}

func TestAssetDownloadCanUseHostCachedProxy(t *testing.T) {
	payload := "cached installer"
	filePath := t.TempDir() + "/asset.bin"
	if err := os.WriteFile(filePath, []byte(payload), 0o600); err != nil {
		t.Fatalf("write cached fixture: %v", err)
	}
	assetID := uuid.New()
	reader := &fakeCatalogReader{
		asset: catalog.ReleaseAssetDownload{
			ObjectKey: "releases/1.2.3/linux/x64/id/WenzWork.AppImage", FileName: "WenzWork.AppImage",
			FileSizeBytes: int64(len(payload)), SHA256: strings.Repeat("b", 64),
		},
		settings: catalog.ReleaseDeliverySettings{DownloadMode: catalog.ReleaseDownloadProxyCached},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Catalog: reader,
		ReleaseDownloads: &fakeReleaseAssetDownloadService{filePath: filePath},
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if response.Code != http.StatusOK || response.Body.String() != payload {
		t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), "WenzWork.AppImage") || response.Header().Get("ETag") != `"`+strings.Repeat("b", 64)+`"` {
		t.Fatalf("proxy headers = %v", response.Header())
	}
}

func TestMirrorAssetAlwaysDownloadsThroughHostCache(t *testing.T) {
	payload := "mirror cached installer"
	filePath := t.TempDir() + "/asset.bin"
	if err := os.WriteFile(filePath, []byte(payload), 0o600); err != nil {
		t.Fatalf("write cached fixture: %v", err)
	}
	assetID := uuid.New()
	downloads := &fakeReleaseAssetDownloadService{filePath: filePath}
	reader := &fakeCatalogReader{
		asset: catalog.ReleaseAssetDownload{
			Source: "mirror", ObjectKey: "mirror/" + strings.Repeat("a", 64) + "/WenzWork.exe",
			DownloadURL: "https://mirror.example.test/api/v1/release-assets/id/download",
			FileName:    "WenzWork.exe", FileSizeBytes: int64(len(payload)), SHA256: strings.Repeat("b", 64),
		},
		settings: catalog.ReleaseDeliverySettings{DownloadMode: catalog.ReleaseDownloadS3Redirect},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Catalog: reader, ReleaseDownloads: downloads,
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))

	if response.Code != http.StatusOK || response.Body.String() != payload {
		t.Fatalf("mirror proxy response = %d %q", response.Code, response.Body.String())
	}
	if downloads.input.Source != "mirror" || downloads.input.DownloadURL != reader.asset.DownloadURL {
		t.Fatalf("mirror delivery input = %+v", downloads.input)
	}

	downloads.err = releaseassets.ErrMirrorAssetMismatch
	mismatch := httptest.NewRecorder()
	router.ServeHTTP(mismatch, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if mismatch.Code != http.StatusBadGateway || !strings.Contains(mismatch.Body.String(), `"code":"mirror_asset_integrity_failed"`) {
		t.Fatalf("mirror mismatch response = %d %s", mismatch.Code, mismatch.Body.String())
	}
}

func TestLocalPushedAssetAlwaysDownloadsFromHostStorage(t *testing.T) {
	payload := "locally pushed installer"
	filePath := t.TempDir() + "/asset.bin"
	if err := os.WriteFile(filePath, []byte(payload), 0o600); err != nil {
		t.Fatalf("write local fixture: %v", err)
	}
	assetID := uuid.New()
	downloads := &fakeReleaseAssetDownloadService{filePath: filePath}
	reader := &fakeCatalogReader{
		asset: catalog.ReleaseAssetDownload{
			Source: "local", ObjectKey: "local/desktop/1.2.3/windows/x64/" + strings.Repeat("b", 64) + "/WenzWork.zip",
			FileName: "WenzWork.zip", FileSizeBytes: int64(len(payload)), SHA256: strings.Repeat("b", 64),
		},
		settings: catalog.ReleaseDeliverySettings{DownloadMode: catalog.ReleaseDownloadGitHubRedirect},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Catalog: reader, ReleaseDownloads: downloads,
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if response.Code != http.StatusOK || response.Body.String() != payload || downloads.input.Source != "local" {
		t.Fatalf("local proxy response = %d %q input=%+v", response.Code, response.Body.String(), downloads.input)
	}
}

func TestAssetDownloadCanUseAuthenticatedGitHubRedirect(t *testing.T) {
	assetID := uuid.New()
	downloads := &fakeReleaseAssetDownloadService{
		redirectURL: "https://release-assets.githubusercontent.com/signed?token=temporary",
	}
	reader := &fakeCatalogReader{
		asset: catalog.ReleaseAssetDownload{
			Source: "github", ObjectKey: "github/acme/wenzwork/assets/42/WenzWork.exe", FileName: "WenzWork.exe",
			FileSizeBytes: 42, SHA256: strings.Repeat("c", 64),
		},
		settings: catalog.ReleaseDeliverySettings{DownloadMode: catalog.ReleaseDownloadGitHubRedirect},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Catalog: reader, ReleaseDownloads: downloads,
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))

	if response.Code != http.StatusFound || response.Header().Get("Location") != downloads.redirectURL {
		t.Fatalf("GitHub redirect = %d location=%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if downloads.input.Source != "github" || downloads.input.ObjectKey != reader.asset.ObjectKey {
		t.Fatalf("delivery input = %+v", downloads.input)
	}

	reader.settings.DownloadMode = catalog.ReleaseDownloadS3Redirect
	incompatible := httptest.NewRecorder()
	router.ServeHTTP(incompatible, httptest.NewRequest(http.MethodGet, "/api/v1/release-assets/"+assetID.String()+"/download", nil))
	if incompatible.Code != http.StatusServiceUnavailable || !strings.Contains(incompatible.Body.String(), `"code":"release_source_incompatible"`) {
		t.Fatalf("incompatible response = %d %s", incompatible.Code, incompatible.Body.String())
	}
}
