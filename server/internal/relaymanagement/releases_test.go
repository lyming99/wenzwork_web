package relaymanagement

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validSaveReleaseInput(now time.Time) SaveReleaseInput {
	return SaveReleaseInput{
		Version: "1.2.3", Platform: "linux", Architecture: "amd64",
		ProtocolMin: 2, ProtocolMax: 2, BuildCommit: strings.Repeat("a", 40), BuildTime: now.Add(-time.Minute),
		SigningKeyID: "release-2026", ManifestSHA256: strings.Repeat("b", 64), ManifestSignature: strings.Repeat("c", 64),
		Artifacts: []SaveReleaseArtifactInput{{
			FileName: "wenzwork-relay-1.2.3-linux-amd64.tar.gz", FileSizeBytes: 4096,
			SHA256: strings.Repeat("d", 64), Signature: strings.Repeat("e", 64),
			ObjectKey: "relay/1.2.3/wenzwork-relay-1.2.3-linux-amd64.tar.gz",
		}},
		ActorUserID: uuid.New(),
	}
}

func TestValidateSaveReleaseAcceptsMetadataAndObjectKey(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	input := validSaveReleaseInput(now)
	input.Platform, input.Architecture = " LINUX ", " AMD64 "

	got, err := validateSaveRelease(input, now)
	if err != nil {
		t.Fatalf("validateSaveRelease() error = %v", err)
	}
	if got.Platform != "linux" || got.Architecture != "amd64" || got.Artifacts[0].ObjectKey != input.Artifacts[0].ObjectKey {
		t.Fatalf("normalized release = %#v", got)
	}
}

func TestValidateSaveReleaseAcceptsDeploymentPackageWithOptionalVersionPrefix(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	input := validSaveReleaseInput(now)
	input.Version = "0.2.9"
	input.Artifacts[0].FileName = "wenzwork-relay-deployment-v0.2.9-linux-amd64.tar.gz"
	input.Artifacts[0].ObjectKey = "relay/v0.2.9/" + input.Artifacts[0].FileName
	if _, err := validateSaveRelease(input, now); err != nil {
		t.Fatalf("validateSaveRelease(deployment package) error = %v", err)
	}
}

func TestValidateSaveReleaseAcceptsCrossPlatformMatrix(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	for _, platform := range []string{"linux", "windows", "darwin"} {
		for _, architecture := range []string{"amd64", "arm64"} {
			t.Run(platform+"/"+architecture, func(t *testing.T) {
				input := validSaveReleaseInput(now)
				input.Platform = " " + strings.ToUpper(platform) + " "
				input.Architecture = " " + strings.ToUpper(architecture) + " "
				fileName := fmt.Sprintf("wenzwork-relay-1.2.3-%s-%s.tar.gz", platform, architecture)
				input.Artifacts[0].FileName = fileName
				input.Artifacts[0].ObjectKey = "relay/1.2.3/" + fileName

				got, err := validateSaveRelease(input, now)
				if err != nil {
					t.Fatalf("validateSaveRelease() error = %v", err)
				}
				if got.Platform != platform || got.Architecture != architecture {
					t.Fatalf("normalized release target = %s/%s", got.Platform, got.Architecture)
				}
			})
		}
	}
}

func TestValidateSaveReleaseAcceptsSixteenArtifactsAndMaximumObjectKey(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	input := validSaveReleaseInput(now)
	fileName := input.Artifacts[0].FileName
	input.Artifacts[0].ObjectKey = strings.Repeat("a", 1024-len(fileName)-1) + "/" + fileName
	for index := 1; index < 16; index++ {
		name := fmt.Sprintf("release-note-%02d.txt", index)
		input.Artifacts = append(input.Artifacts, SaveReleaseArtifactInput{
			FileName: name, FileSizeBytes: int64(index), SHA256: strings.Repeat("d", 64),
			Signature: strings.Repeat("e", 16), ObjectKey: "relay/1.2.3/" + name,
		})
	}
	if _, err := validateSaveRelease(input, now); err != nil {
		t.Fatalf("validateSaveRelease() error = %v", err)
	}

	input.Artifacts[0].ObjectKey = "a" + input.Artifacts[0].ObjectKey
	if _, err := validateSaveRelease(input, now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("validateSaveRelease() with 1025-byte object key error = %v, want ErrInvalidInput", err)
	}
}

func TestValidateSaveReleaseRejectsUnsafeOrAmbiguousArtifacts(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*SaveReleaseInput)
	}{
		{name: "path traversal", mutate: func(input *SaveReleaseInput) {
			input.Artifacts[0].ObjectKey = "relay/../" + input.Artifacts[0].FileName
		}},
		{name: "absolute object key", mutate: func(input *SaveReleaseInput) {
			input.Artifacts[0].ObjectKey = "/relay/1.2.3/" + input.Artifacts[0].FileName
		}},
		{name: "object name mismatch", mutate: func(input *SaveReleaseInput) { input.Artifacts[0].ObjectKey = "relay/1.2.3/other.tar.gz" }},
		{name: "package version mismatch", mutate: func(input *SaveReleaseInput) {
			input.Artifacts[0].FileName = "wenzwork-relay-1.2.2-linux-amd64.tar.gz"
			input.Artifacts[0].ObjectKey = "relay/1.2.3/" + input.Artifacts[0].FileName
		}},
		{name: "package platform mismatch", mutate: func(input *SaveReleaseInput) {
			input.Artifacts[0].FileName = "wenzwork-relay-1.2.3-windows-amd64.tar.gz"
			input.Artifacts[0].ObjectKey = "relay/1.2.3/" + input.Artifacts[0].FileName
		}},
		{name: "ambiguous package name", mutate: func(input *SaveReleaseInput) {
			input.Artifacts[0].FileName = "wenzwork-relay.tar.gz"
			input.Artifacts[0].ObjectKey = "relay/1.2.3/" + input.Artifacts[0].FileName
		}},
		{name: "two packages", mutate: func(input *SaveReleaseInput) {
			second := input.Artifacts[0]
			second.FileName = "second.tar.gz"
			second.ObjectKey = "relay/1.2.3/second.tar.gz"
			input.Artifacts = append(input.Artifacts, second)
		}},
		{name: "too many artifacts", mutate: func(input *SaveReleaseInput) {
			for index := 1; index <= 16; index++ {
				name := fmt.Sprintf("release-note-%02d.txt", index)
				input.Artifacts = append(input.Artifacts, SaveReleaseArtifactInput{
					FileName: name, FileSizeBytes: int64(index), SHA256: strings.Repeat("d", 64),
					Signature: strings.Repeat("e", 16), ObjectKey: "relay/1.2.3/" + name,
				})
			}
		}},
		{name: "future build", mutate: func(input *SaveReleaseInput) { input.BuildTime = now.Add(6 * time.Minute) }},
		{name: "protocol inversion", mutate: func(input *SaveReleaseInput) { input.ProtocolMin, input.ProtocolMax = 3, 2 }},
		{name: "legacy protocol", mutate: func(input *SaveReleaseInput) { input.ProtocolMin, input.ProtocolMax = 1, 1 }},
		{name: "unsupported platform", mutate: func(input *SaveReleaseInput) { input.Platform = "freebsd" }},
		{name: "unsupported architecture", mutate: func(input *SaveReleaseInput) { input.Architecture = "386" }},
		{name: "short build commit", mutate: func(input *SaveReleaseInput) { input.BuildCommit = strings.Repeat("a", 39) }},
		{name: "short manifest signature", mutate: func(input *SaveReleaseInput) { input.ManifestSignature = strings.Repeat("c", 15) }},
		{name: "short artifact signature", mutate: func(input *SaveReleaseInput) { input.Artifacts[0].Signature = strings.Repeat("e", 15) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validSaveReleaseInput(now)
			test.mutate(&input)
			if _, err := validateSaveRelease(input, now); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("validateSaveRelease() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}
