package releaseassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
)

func TestServiceValidatesRepositoryForEveryGitHubQuery(t *testing.T) {
	service := NewService(nil)
	if _, err := service.LatestGitHubRelease(t.Context(), "", ""); !errors.Is(err, ErrGitHubUnconfigured) {
		t.Fatalf("LatestGitHubRelease(empty) error = %v, want ErrGitHubUnconfigured", err)
	}
	if _, err := service.LatestGitHubRelease(t.Context(), "invalid repository", ""); err == nil {
		t.Fatal("LatestGitHubRelease(invalid) error = nil")
	}
}

type fakeReleaseStorage struct {
	input   objectstore.ReleaseAssetUploadInput
	payload []byte
}

func (f *fakeReleaseStorage) Upload(_ context.Context, input objectstore.ReleaseAssetUploadInput, body io.Reader) (objectstore.ReleaseAssetUpload, error) {
	f.input = input
	f.payload, _ = io.ReadAll(body)
	digest := sha256.Sum256(f.payload)
	return objectstore.ReleaseAssetUpload{
		ObjectKey:     "releases/1.2.3/windows/x64/id/" + input.FileName,
		DownloadURL:   "https://downloads.example.test/releases/1.2.3/windows/x64/id/" + input.FileName,
		FileSizeBytes: int64(len(f.payload)), SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func TestServiceImportsRemoteAssetIntoStorageWithDetectedMetadata(t *testing.T) {
	payload := []byte("remote installer")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Disposition", `attachment; filename="WenzWork-windows-x64.exe"`)
		response.Header().Set("Content-Type", "application/octet-stream")
		_, _ = response.Write(payload)
	}))
	defer server.Close()

	storage := &fakeReleaseStorage{}
	service := &Service{
		inspector: newRemoteInspectorForTest(server.Client()),
		storage:   storage,
	}
	stored, err := service.ImportRemote(t.Context(), RemoteImportInput{
		Version: "1.2.3", Platform: "windows", Architecture: "x64", DownloadURL: server.URL + "/installer",
	})
	if err != nil {
		t.Fatalf("ImportRemote() error = %v", err)
	}
	if string(storage.payload) != string(payload) || storage.input.FileName != "WenzWork-windows-x64.exe" {
		t.Fatalf("storage input = %+v payload=%q", storage.input, storage.payload)
	}
	if stored.ObjectKey == "" || stored.FileSizeBytes != int64(len(payload)) || stored.Platform != "windows" || stored.Architecture != "x64" {
		t.Fatalf("stored asset = %+v", stored)
	}
}
