package releaseassets

import (
	"context"
	"io"
	"strings"

	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
)

type releaseStorage interface {
	Upload(context.Context, objectstore.ReleaseAssetUploadInput, io.Reader) (objectstore.ReleaseAssetUpload, error)
}

type Service struct {
	inspector       *RemoteInspector
	storage         releaseStorage
	newMirrorClient func(string) (*MirrorClient, error)
}

func NewService(storage releaseStorage) *Service {
	return &Service{
		inspector: NewRemoteInspector(), storage: storage,
		newMirrorClient: NewMirrorClient,
	}
}

func (s *Service) Probe(ctx context.Context, downloadURL string) (AssetMetadata, error) {
	return s.inspector.Probe(ctx, downloadURL)
}

func (s *Service) LatestGitHubRelease(ctx context.Context, repository, token string) (GitHubRelease, error) {
	github, err := NewGitHubClient(repository, token)
	if err != nil {
		return GitHubRelease{}, err
	}
	return github.Latest(ctx)
}

func (s *Service) ImportLatestMirrorRelease(ctx context.Context, mirrorBaseURL, project string) (MirrorReleaseImport, error) {
	newClient := s.newMirrorClient
	if newClient == nil {
		newClient = NewMirrorClient
	}
	mirror, err := newClient(mirrorBaseURL)
	if err != nil {
		return MirrorReleaseImport{}, err
	}
	catalogRelease, err := mirror.Latest(ctx, project)
	if err != nil {
		return MirrorReleaseImport{}, err
	}

	result := MirrorReleaseImport{
		MirrorBaseURL: mirror.BaseURL(), Project: catalogRelease.Project,
		Version: catalogRelease.Version, Channel: catalogRelease.Channel,
		Title: catalogRelease.Title, Summary: catalogRelease.Summary,
		ReleaseNotes: catalogRelease.ReleaseNotes, PublishedAt: catalogRelease.PublishedAt,
		Assets: make([]MirrorReleaseAsset, 0, len(catalogRelease.Assets)),
	}
	for _, asset := range catalogRelease.Assets {
		result.Assets = append(result.Assets, MirrorReleaseAsset{
			FileName: asset.FileName, FileSizeBytes: asset.FileSizeBytes,
			SHA256: strings.ToLower(asset.SHA256), ContentType: "application/octet-stream",
			DownloadURL: asset.DownloadURL, Source: "mirror",
			ObjectKey: mirrorAssetObjectKey(asset.DownloadURL, asset.FileName),
			Platform:  asset.Platform, Architecture: asset.Architecture,
			SignatureStatus: asset.SignatureStatus,
		})
	}
	return result, nil
}

func (s *Service) ImportRemote(ctx context.Context, input RemoteImportInput) (StoredAsset, error) {
	if s.storage == nil {
		return StoredAsset{}, ErrStorageUnavailable
	}
	opened, err := s.inspector.Open(ctx, input.DownloadURL)
	if err != nil {
		return StoredAsset{}, err
	}
	defer opened.Body.Close()

	platform := strings.TrimSpace(input.Platform)
	if platform == "" {
		platform = opened.Metadata.Platform
	}
	architecture := strings.TrimSpace(input.Architecture)
	if architecture == "" {
		architecture = opened.Metadata.Architecture
	}
	if !validTarget(platform, architecture) {
		return StoredAsset{}, ErrRemoteTargetInvalid
	}

	stored, err := s.storage.Upload(ctx, objectstore.ReleaseAssetUploadInput{
		Version: input.Version, Platform: platform, Architecture: architecture,
		FileName: opened.Metadata.FileName, FileSizeBytes: opened.Metadata.FileSizeBytes,
		ContentType: opened.Metadata.ContentType,
	}, opened.Body)
	if err != nil {
		return StoredAsset{}, err
	}
	opened.Metadata.Platform = platform
	opened.Metadata.Architecture = architecture
	opened.Metadata.FileSizeBytes = stored.FileSizeBytes
	opened.Metadata.SHA256 = stored.SHA256
	opened.Metadata.DownloadURL = stored.DownloadURL
	return StoredAsset{AssetMetadata: opened.Metadata, ObjectKey: stored.ObjectKey}, nil
}

func validTarget(platform, architecture string) bool {
	return (platform == "web" || platform == "windows" || platform == "macos" || platform == "linux" ||
		platform == "android" || platform == "ios") &&
		(architecture == "x64" || architecture == "arm64" || architecture == "universal")
}
