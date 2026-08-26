package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

func TestLocalReleaseAssetStoreUploadsAndOpensVerifiedAsset(t *testing.T) {
	store, err := NewLocalReleaseAssetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("verified local release")
	digestBytes := sha256.Sum256(payload)
	digest := hex.EncodeToString(digestBytes[:])
	result, err := store.Upload(context.Background(), "desktop", ReleaseAssetUploadInput{
		Version: "v1.2.3", Platform: "windows", Architecture: "x64", FileName: "wenzwork-desktop-v1.2.3-windows-x64.zip",
		FileSizeBytes: int64(len(payload)), SHA256: digest, ContentType: "application/zip",
	}, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if !strings.HasPrefix(result.ObjectKey, "local/desktop/v1.2.3/windows/x64/"+digest+"/") || result.DownloadURL != "" {
		t.Fatalf("Upload() result = %+v", result)
	}
	opened, err := store.Open(context.Background(), ReleaseAssetCacheInput{
		ObjectKey: result.ObjectKey, FileName: "wenzwork-desktop-v1.2.3-windows-x64.zip",
		FileSizeBytes: int64(len(payload)), SHA256: digest,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer opened.File.Close()
	actual, err := io.ReadAll(opened.File)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("opened payload = %q, error = %v", actual, err)
	}
}

func TestLocalReleaseAssetStoreRejectsChecksumMismatch(t *testing.T) {
	store, err := NewLocalReleaseAssetStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Upload(context.Background(), "mobile", ReleaseAssetUploadInput{
		Version: "1.0.0", Platform: "android", Architecture: "universal", FileName: "wenzwork-mobile-1.0.0-android-universal.apk",
		FileSizeBytes: 3, SHA256: strings.Repeat("a", 64), ContentType: "application/octet-stream",
	}, bytes.NewBufferString("bad"))
	if err == nil || !strings.Contains(err.Error(), ErrReleaseUploadChecksumMismatch.Error()) {
		t.Fatalf("Upload() error = %v, want checksum mismatch", err)
	}
}
