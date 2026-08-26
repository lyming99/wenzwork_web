package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleaseAssetUploaderStreamsThroughServerAndBuildsCDNURL(t *testing.T) {
	payload := []byte("WenzWork release upload")
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPut || !strings.HasPrefix(request.URL.Path, "/wenzwork-publish/releases/v1.2.3/windows/x64/") ||
			!strings.HasSuffix(request.URL.Path, "/WenzWork Setup.exe") {
			t.Fatalf("unexpected storage request: %s %s", request.Method, request.URL.String())
		}
		var err error
		uploaded, err = io.ReadAll(request.Body)
		if err != nil {
			t.Fatalf("read upload body: %v", err)
		}
		response.Header().Set("ETag", `"test-etag"`)
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	uploader, err := NewReleaseAssetUploader(S3Config{
		Endpoint: server.URL, Region: "test-1", Bucket: "wenzwork-publish",
		AccessKeyID: "access-key", SecretAccessKey: "secret-key", AddressingStyle: S3AddressingStylePath,
	}, "https://downloads.example.test/files")
	if err != nil {
		t.Fatalf("NewReleaseAssetUploader() error = %v", err)
	}
	digest := sha256.Sum256(payload)
	result, err := uploader.Upload(t.Context(), ReleaseAssetUploadInput{
		Version: "v1.2.3", Platform: "windows", Architecture: "x64",
		FileName: "WenzWork Setup.exe", FileSizeBytes: int64(len(payload)),
		SHA256: hex.EncodeToString(digest[:]), ContentType: "application/octet-stream",
	}, bytes.NewBuffer(payload))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if !bytes.Equal(uploaded, payload) {
		t.Fatalf("uploaded payload = %q", uploaded)
	}
	if !strings.HasPrefix(result.ObjectKey, "releases/v1.2.3/windows/x64/") ||
		!strings.HasSuffix(result.ObjectKey, "/WenzWork Setup.exe") {
		t.Fatalf("ObjectKey = %q", result.ObjectKey)
	}
	if !strings.HasPrefix(result.DownloadURL, "https://downloads.example.test/files/releases/") ||
		!strings.HasSuffix(result.DownloadURL, "/WenzWork%20Setup.exe") {
		t.Fatalf("DownloadURL = %q", result.DownloadURL)
	}
}

func TestReleaseAssetUploaderDerivesVirtualHostedDownloadURL(t *testing.T) {
	baseURL, err := resolveDownloadBaseURL(S3Config{
		Endpoint: "https://s3.oss-cn-hangzhou.aliyuncs.com", Region: "cn-hangzhou", Bucket: "wenzwork-publish",
		AccessKeyID: "access-key", SecretAccessKey: "secret-key", AddressingStyle: S3AddressingStyleVirtual,
	}, "")
	if err != nil {
		t.Fatalf("resolveDownloadBaseURL() error = %v", err)
	}
	if baseURL != "https://wenzwork-publish.oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("baseURL = %q", baseURL)
	}
}

func TestReleaseAssetUploaderRejectsInvalidInput(t *testing.T) {
	uploader, err := NewReleaseAssetUploader(S3Config{
		Endpoint: "http://localhost:9000", Region: "us-east-1", Bucket: "wenzwork-releases",
		AccessKeyID: "access-key", SecretAccessKey: "secret-key", AddressingStyle: S3AddressingStylePath,
	}, "http://localhost:9000/wenzwork-releases")
	if err != nil {
		t.Fatalf("NewReleaseAssetUploader() error = %v", err)
	}
	_, err = uploader.Upload(t.Context(), ReleaseAssetUploadInput{
		Version: "1.0.0", Platform: "windows", Architecture: "x64", FileName: "../WenzWork.exe",
		FileSizeBytes: 10, SHA256: strings.Repeat("c", 64),
	}, strings.NewReader("0123456789"))
	if !errors.Is(err, ErrReleaseUploadInvalid) {
		t.Fatalf("Upload() error = %v, want ErrReleaseUploadInvalid", err)
	}
}
