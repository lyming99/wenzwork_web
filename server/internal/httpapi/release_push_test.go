package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/catalog"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
)

type fakeReleasePushAssetStore struct {
	called    bool
	project   string
	input     objectstore.ReleaseAssetUploadInput
	verifyErr error
}

func (f *fakeReleasePushAssetStore) Verify(_ context.Context, _ objectstore.ReleaseAssetCacheInput) error {
	return f.verifyErr
}

func (f *fakeReleasePushAssetStore) Upload(_ context.Context, project string, input objectstore.ReleaseAssetUploadInput, body io.Reader) (objectstore.ReleaseAssetUpload, error) {
	f.called, f.project, f.input = true, project, input
	_, _ = io.Copy(io.Discard, body)
	return objectstore.ReleaseAssetUpload{
		ObjectKey:     "local/desktop/1.2.3/windows/x64/" + input.SHA256 + "/" + input.FileName,
		FileSizeBytes: input.FileSizeBytes, SHA256: input.SHA256,
	}, nil
}

type fakeReleasePushService struct {
	input catalog.PushReleaseInput
}

type fakeReleaseAccessKeyService struct {
	key      string
	settings catalog.ReleaseAccessKeySettings
	input    catalog.UpdateReleaseAccessKeySettingsInput
	err      error
}

func (f *fakeReleaseAccessKeyService) VerifyReleaseAccessKey(_ context.Context, accessKey string) (bool, error) {
	return accessKey == f.key, f.err
}

func (f *fakeReleaseAccessKeyService) GetReleaseAccessKeySettings(context.Context) (catalog.ReleaseAccessKeySettings, error) {
	return f.settings, f.err
}

func (f *fakeReleaseAccessKeyService) UpdateReleaseAccessKeySettings(_ context.Context, input catalog.UpdateReleaseAccessKeySettingsInput) (catalog.ReleaseAccessKeySettings, error) {
	f.input = input
	f.key = input.AccessKey
	f.settings = catalog.ReleaseAccessKeySettings{
		AccessKeyConfigured: true,
		KeyPrefix:           input.AccessKey[:16],
		Version:             input.ExpectedVersion + 1,
		UpdatedAt:           time.Now().UTC(),
	}
	return f.settings, f.err
}

func (f *fakeReleasePushService) PushRelease(_ context.Context, input catalog.PushReleaseInput) (catalog.AdminRelease, bool, error) {
	f.input = input
	return catalog.AdminRelease{ID: uuid.New(), Project: input.Project, Version: input.Version, Status: "published"}, true, nil
}

func TestReleasePushRequiresAccessKeyBeforeReadingAsset(t *testing.T) {
	assets := &fakeReleasePushAssetStore{}
	router := NewRouter(Dependencies{ReleasePushAssets: assets, ReleaseAccessKey: "release_" + strings.Repeat("a", 43)})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/release-push/assets", strings.NewReader("payload"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || assets.called {
		t.Fatalf("response = %d %s, storage called = %v", response.Code, response.Body.String(), assets.called)
	}
}

func TestReleasePushUploadsAssetAndFinalizesPublishedRelease(t *testing.T) {
	key := "release_" + strings.Repeat("b", 43)
	assets := &fakeReleasePushAssetStore{}
	releases := &fakeReleasePushService{}
	router := NewRouter(Dependencies{ReleasePush: releases, ReleasePushAssets: assets, ReleaseAccessKey: key})
	digest := strings.Repeat("c", 64)
	query := url.Values{
		"project": {"desktop"}, "version": {"1.2.3"}, "platform": {"windows"}, "architecture": {"x64"},
		"fileName": {"wenzwork-desktop-1.2.3-windows-x64.zip"}, "fileSizeBytes": {"7"}, "sha256": {digest},
	}
	upload := httptest.NewRequest(http.MethodPost, "/api/v1/release-push/assets?"+query.Encode(), strings.NewReader("payload"))
	upload.Header.Set("Authorization", "Bearer "+key)
	upload.Header.Set("Content-Type", "application/zip")
	uploadResponse := httptest.NewRecorder()
	router.ServeHTTP(uploadResponse, upload)
	if uploadResponse.Code != http.StatusCreated || !assets.called || assets.project != "desktop" || assets.input.SHA256 != digest {
		t.Fatalf("upload response = %d %s, storage = %+v", uploadResponse.Code, uploadResponse.Body.String(), assets)
	}

	body := `{"project":"desktop","version":"1.2.3","softwareName":"WenzWork 桌面端","assets":[{"platform":"windows","architecture":"x64","fileName":"wenzwork-desktop-1.2.3-windows-x64.zip","fileSizeBytes":7,"sha256":"` + digest + `","signatureStatus":"unknown","source":"local","objectKey":"local/desktop/1.2.3/windows/x64/` + digest + `/wenzwork-desktop-1.2.3-windows-x64.zip","downloadUrl":""}]}`
	finalize := httptest.NewRequest(http.MethodPost, "/api/v1/release-push", strings.NewReader(body))
	finalize.Header.Set("Authorization", "Bearer "+key)
	finalize.Header.Set("Content-Type", "application/json")
	finalizeResponse := httptest.NewRecorder()
	router.ServeHTTP(finalizeResponse, finalize)
	if finalizeResponse.Code != http.StatusCreated || releases.input.Project != "desktop" || !releases.input.Publish || len(releases.input.Assets) != 1 {
		t.Fatalf("finalize response = %d %s, input = %+v", finalizeResponse.Code, finalizeResponse.Body.String(), releases.input)
	}
}

func TestReleasePushRejectsCatalogFinalizeWhenLocalAssetIsMissing(t *testing.T) {
	key := "release_" + strings.Repeat("e", 43)
	assets := &fakeReleasePushAssetStore{verifyErr: errors.New("missing")}
	releases := &fakeReleasePushService{}
	router := NewRouter(Dependencies{ReleasePush: releases, ReleasePushAssets: assets, ReleaseAccessKey: key})
	body := `{"project":"mobile","version":"1.0.0","assets":[{"platform":"android","architecture":"universal","fileName":"app.apk","fileSizeBytes":7,"sha256":"` + strings.Repeat("f", 64) + `","signatureStatus":"unknown","source":"local","objectKey":"local/mobile/1.0.0/android/universal/` + strings.Repeat("f", 64) + `/app.apk","downloadUrl":""}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/release-push", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), `"code":"release_push_asset_missing"`) || releases.input.Project != "" {
		t.Fatalf("response = %d %s, release input = %+v", response.Code, response.Body.String(), releases.input)
	}
}

func TestReleasePushReadsDatabaseBackedAccessKeyForEveryRequest(t *testing.T) {
	oldKey := "release_" + strings.Repeat("g", 43)
	newKey := "release_" + strings.Repeat("h", 43)
	accessKeys := &fakeReleaseAccessKeyService{key: oldKey}
	assets := &fakeReleasePushAssetStore{}
	releases := &fakeReleasePushService{}
	router := NewRouter(Dependencies{
		ReleasePush: releases, ReleasePushAssets: assets, ReleaseAccessKeys: accessKeys,
	})
	body := `{"project":"desktop","version":"1.2.3","assets":[]}`

	request := httptest.NewRequest(http.MethodPost, "/api/v1/release-push", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+oldKey)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("old key response before rotation = %d %s", response.Code, response.Body.String())
	}

	accessKeys.key = newKey
	staleRequest := httptest.NewRequest(http.MethodPost, "/api/v1/release-push", strings.NewReader(body))
	staleRequest.Header.Set("Authorization", "Bearer "+oldKey)
	staleRequest.Header.Set("Content-Type", "application/json")
	staleResponse := httptest.NewRecorder()
	router.ServeHTTP(staleResponse, staleRequest)
	if staleResponse.Code != http.StatusUnauthorized || !strings.Contains(staleResponse.Body.String(), `"code":"release_access_key_invalid"`) {
		t.Fatalf("old key response after rotation = %d %s", staleResponse.Code, staleResponse.Body.String())
	}

	newRequest := httptest.NewRequest(http.MethodPost, "/api/v1/release-push", strings.NewReader(body))
	newRequest.Header.Set("Authorization", "Bearer "+newKey)
	newRequest.Header.Set("Content-Type", "application/json")
	newResponse := httptest.NewRecorder()
	router.ServeHTTP(newResponse, newRequest)
	if newResponse.Code != http.StatusCreated {
		t.Fatalf("new key response after rotation = %d %s", newResponse.Code, newResponse.Body.String())
	}
}
