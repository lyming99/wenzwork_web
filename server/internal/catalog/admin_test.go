package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidateReleaseInputRequiresDeploymentAssetVersionAndTargetMatch(t *testing.T) {
	asset := SaveReleaseAssetInput{
		Platform: "linux", Architecture: "x64",
		FileName:      "wenzwork-relay-deployment-v0.2.9-linux-amd64.tar.gz",
		FileSizeBytes: 4096, SHA256: strings.Repeat("a", 64), SignatureStatus: "valid",
		Source:      "s3",
		ObjectKey:   "releases/v0.2.9/linux/x64/upload/wenzwork-relay-deployment-v0.2.9-linux-amd64.tar.gz",
		DownloadURL: "https://downloads.example.test/wenzwork-relay-deployment-v0.2.9-linux-amd64.tar.gz",
	}
	input := SaveReleaseInput{
		Project: ReleaseProjectWeb, Version: "0.2.9", Channel: "stable", Title: "Release", Status: "draft",
		ActorUserID: uuid.New(), Assets: []SaveReleaseAssetInput{asset},
	}

	if _, err := validateReleaseInput(input); err != nil {
		t.Fatalf("validateReleaseInput(matching deployment) error = %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*SaveReleaseInput)
	}{
		{name: "old package version", mutate: func(value *SaveReleaseInput) {
			value.Assets[0].FileName = "wenzwork-relay-deployment-v0.2.8-linux-amd64.tar.gz"
		}},
		{name: "file target mismatch", mutate: func(value *SaveReleaseInput) {
			value.Assets[0].FileName = "wenzwork-relay-deployment-v0.2.9-windows-amd64.tar.gz"
		}},
		{name: "metadata target mismatch", mutate: func(value *SaveReleaseInput) {
			value.Assets[0].Architecture = "arm64"
		}},
		{name: "wrong project", mutate: func(value *SaveReleaseInput) {
			value.Project = ReleaseProjectDesktop
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := input
			candidate.Assets = append([]SaveReleaseAssetInput(nil), input.Assets...)
			test.mutate(&candidate)
			if _, err := validateReleaseInput(candidate); !errors.Is(err, ErrReleaseAssetMismatch) {
				t.Fatalf("validateReleaseInput() error = %v, want ErrReleaseAssetMismatch", err)
			}
		})
	}
}

func TestValidateReleaseInputAcceptsBoundLocalPushAsset(t *testing.T) {
	digest := strings.Repeat("d", 64)
	fileName := "wenzwork-desktop-v1.2.3-windows-x64.zip"
	input := SaveReleaseInput{
		Project: ReleaseProjectDesktop, Version: "v1.2.3", Channel: "stable", Title: "Release", Status: "published",
		ActorUserID: uuid.New(), Assets: []SaveReleaseAssetInput{{
			Platform: "windows", Architecture: "x64", FileName: fileName, FileSizeBytes: 42,
			SHA256: digest, SignatureStatus: "unknown", Source: "local",
			ObjectKey: "local/desktop/v1.2.3/windows/x64/" + digest + "/" + fileName,
		}},
	}
	if _, err := validateReleaseInput(input); err != nil {
		t.Fatalf("validateReleaseInput(local push) error = %v", err)
	}
	input.Assets[0].ObjectKey = "local/mobile/v1.2.3/windows/x64/" + digest + "/" + fileName
	if _, err := validateReleaseInput(input); !errors.Is(err, ErrReleaseInvalid) {
		t.Fatalf("tampered local object key error = %v, want ErrReleaseInvalid", err)
	}
}

func TestValidateReleaseInputRequiresManagedAssets(t *testing.T) {
	base := SaveReleaseInput{
		Version: "1.2.3", Channel: "stable", Title: "Release", Status: "draft",
		ActorUserID: uuid.New(),
	}

	custom := base
	custom.Assets = []SaveReleaseAssetInput{{
		Platform: "windows", Architecture: "x64", FileName: "WenzWork.exe",
		FileSizeBytes: 1024, SHA256: strings.Repeat("a", 64), SignatureStatus: "valid",
		Source:      "custom",
		DownloadURL: "https://downloads.example.test/WenzWork.exe",
	}}
	if _, err := validateReleaseInput(custom); err == nil {
		t.Fatal("validateReleaseInput(custom) error = nil, want managed-source validation")
	}

	s3Input := base
	s3Input.Assets = []SaveReleaseAssetInput{{
		Platform: "linux", Architecture: "arm64", FileName: "WenzWork.AppImage",
		FileSizeBytes: 2048, SHA256: strings.Repeat("b", 64), SignatureStatus: "unsigned",
		Source: "s3", ObjectKey: "releases/1.2.3/linux/arm64/upload-id/WenzWork.AppImage",
		DownloadURL: "https://downloads.example.test/releases/1.2.3/linux/arm64/upload-id/WenzWork.AppImage",
	}}
	validated, err := validateReleaseInput(s3Input)
	if err != nil {
		t.Fatalf("validateReleaseInput(s3) error = %v", err)
	}
	if validated.Assets[0].Source != "s3" || validated.Assets[0].ObjectKey == "" {
		t.Fatalf("S3 asset = %+v", validated.Assets[0])
	}

	githubInput := base
	githubInput.Assets = []SaveReleaseAssetInput{{
		Platform: "windows", Architecture: "x64", FileName: "WenzWork.exe",
		FileSizeBytes: 4096, SHA256: strings.Repeat("c", 64), SignatureStatus: "valid",
		Source: "github", ObjectKey: "github/acme/wenzwork/assets/42/WenzWork.exe",
		DownloadURL: "https://github.com/acme/wenzwork/releases/download/v1.2.3/WenzWork.exe",
	}}
	validated, err = validateReleaseInput(githubInput)
	if err != nil || validated.Assets[0].Source != "github" {
		t.Fatalf("validateReleaseInput(github) = %+v, %v", validated, err)
	}

	githubInput.Assets[0].DownloadURL = "https://evil.example.test/WenzWork.exe"
	if _, err := validateReleaseInput(githubInput); err == nil {
		t.Fatal("validateReleaseInput(forged GitHub URL) error = nil")
	}

	mirrorURL := "https://mirror.example.test/api/v1/release-assets/asset-id/download"
	mirrorDigest := sha256.Sum256([]byte(mirrorURL))
	mirrorInput := base
	mirrorInput.Assets = []SaveReleaseAssetInput{{
		Platform: "windows", Architecture: "x64", FileName: "WenzWork.exe",
		FileSizeBytes: 8192, SHA256: strings.Repeat("d", 64), SignatureStatus: "valid",
		Source: "mirror", ObjectKey: "mirror/" + hex.EncodeToString(mirrorDigest[:]) + "/WenzWork.exe",
		DownloadURL: mirrorURL,
	}}
	validated, err = validateReleaseInput(mirrorInput)
	if err != nil || validated.Assets[0].Source != "mirror" {
		t.Fatalf("validateReleaseInput(mirror) = %+v, %v", validated, err)
	}
	mirrorInput.Assets[0].DownloadURL = "https://other.example.test/api/v1/release-assets/asset-id/download"
	if _, err := validateReleaseInput(mirrorInput); err == nil {
		t.Fatal("validateReleaseInput(forged mirror URL) error = nil")
	}
}

func TestBuildAdminAssetRowsPreservesS3ObjectKeyAndSource(t *testing.T) {
	releaseID := uuid.New()
	objectKey := "releases/1.2.3/windows/x64/upload-id/WenzWork.exe"
	rows := buildAdminAssetRows(releaseID, []SaveReleaseAssetInput{{
		Platform: "windows", Architecture: "x64", FileName: "WenzWork.exe",
		FileSizeBytes: 1024, SHA256: strings.Repeat("d", 64), SignatureStatus: "valid",
		Source: "s3", ObjectKey: objectKey,
		DownloadURL: "https://downloads.example.test/" + objectKey,
	}}, "draft", time.Now().UTC(), nil)
	if len(rows) != 1 || rows[0].ObjectKey != objectKey {
		t.Fatalf("asset rows = %+v", rows)
	}
	asset := adminReleaseFromRow(adminReleaseRow{ID: releaseID, Assets: rows}).Assets[0]
	if asset.Source != "s3" || asset.ObjectKey != objectKey {
		t.Fatalf("admin asset = %+v", asset)
	}
}

func TestValidateReleaseInputRejectsUntrustedS3ObjectKey(t *testing.T) {
	input := SaveReleaseInput{
		Version: "1.2.3", Channel: "stable", Title: "Release", Status: "draft",
		ActorUserID: uuid.New(),
		Assets: []SaveReleaseAssetInput{{
			Platform: "windows", Architecture: "x64", FileName: "WenzWork.exe",
			FileSizeBytes: 1024, SHA256: strings.Repeat("c", 64), SignatureStatus: "valid",
			Source: "s3", ObjectKey: "../private/WenzWork.exe",
			DownloadURL: "https://downloads.example.test/WenzWork.exe",
		}},
	}
	if _, err := validateReleaseInput(input); err == nil {
		t.Fatal("validateReleaseInput() error = nil, want invalid S3 object key")
	}
}
