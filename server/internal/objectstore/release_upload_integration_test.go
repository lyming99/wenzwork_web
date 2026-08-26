package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/joho/godotenv"
)

func TestReleaseAssetServerUploadAndCacheIntegration(t *testing.T) {
	if os.Getenv("TEST_S3_UPLOAD") != "1" {
		t.Skip("TEST_S3_UPLOAD is not enabled")
	}
	if envFile := os.Getenv("TEST_S3_ENV_FILE"); envFile != "" {
		if err := godotenv.Overload(envFile); err != nil {
			t.Fatalf("load S3 integration environment: %v", err)
		}
	}
	cfg := S3Config{
		Endpoint: os.Getenv("S3_ENDPOINT"), Region: os.Getenv("S3_REGION"), Bucket: os.Getenv("S3_BUCKET"),
		AccessKeyID: os.Getenv("S3_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("S3_SESSION_TOKEN"), AddressingStyle: os.Getenv("S3_ADDRESSING_STYLE"),
	}
	uploader, err := NewReleaseAssetUploader(cfg, os.Getenv("DOWNLOAD_CDN_BASE_URL"))
	if err != nil {
		t.Fatalf("NewReleaseAssetUploader() error = %v", err)
	}
	client, _, err := newS3Client(cfg, http.DefaultClient)
	if err != nil {
		t.Fatalf("newS3Client() error = %v", err)
	}
	deleteReleaseUploadIntegrationObjects(t, client, cfg.Bucket)
	t.Cleanup(func() { deleteReleaseUploadIntegrationObjects(t, client, cfg.Bucket) })
	// Exceed the 16 MiB threshold so this test covers the multipart streaming path
	// used by real installers rather than only the small-object PutObject path.
	payload := bytes.Repeat([]byte{0x57}, 17*1024*1024+123)
	digest := sha256.Sum256(payload)
	upload, err := uploader.Upload(t.Context(), ReleaseAssetUploadInput{
		Version: "integration-test", Platform: "linux", Architecture: "x64",
		FileName: "wenzwork-upload-test.txt", FileSizeBytes: int64(len(payload)),
		SHA256: hex.EncodeToString(digest[:]), ContentType: "text/plain",
	}, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	head, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String(cfg.Bucket), Key: aws.String(upload.ObjectKey),
	})
	if err != nil {
		t.Fatalf("HeadObject() error = %v", err)
	}
	if head.ContentLength == nil || *head.ContentLength != int64(len(payload)) {
		t.Fatalf("uploaded ContentLength = %v", head.ContentLength)
	}

	cache, err := NewReleaseAssetCache(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("NewReleaseAssetCache() error = %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		cached, err := cache.Open(t.Context(), ReleaseAssetCacheInput{
			ObjectKey: upload.ObjectKey, FileName: "wenzwork-upload-test.txt",
			FileSizeBytes: upload.FileSizeBytes, SHA256: upload.SHA256,
		})
		if err != nil {
			t.Fatalf("cache.Open() attempt %d error = %v", attempt+1, err)
		}
		content, readErr := io.ReadAll(cached.File)
		_ = cached.File.Close()
		if readErr != nil || !bytes.Equal(content, payload) {
			t.Fatalf("cached content attempt %d = %q, %v", attempt+1, content, readErr)
		}
		if attempt == 0 {
			if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
				Bucket: aws.String(cfg.Bucket), Key: aws.String(upload.ObjectKey),
			}); err != nil {
				t.Fatalf("delete S3 object after cache fill: %v", err)
			}
		}
	}
}

func deleteReleaseUploadIntegrationObjects(t *testing.T, client *s3.Client, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String("releases/integration-test/"),
	})
	if err != nil {
		t.Errorf("list integration upload objects for cleanup: %v", err)
		return
	}
	for _, object := range result.Contents {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(bucket), Key: object.Key,
		}); err != nil {
			t.Errorf("delete integration upload object %q: %v", aws.ToString(object.Key), err)
		}
	}
}
