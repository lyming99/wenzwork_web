package releaseassets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
)

type DeliveryAsset struct {
	Source        string
	ObjectKey     string
	DownloadURL   string
	FileName      string
	FileSizeBytes int64
	SHA256        string
}

type GitHubTokenProvider func(context.Context) (string, error)
type GitHubRepositoryTokenProvider func(context.Context, string) (string, error)

type releaseAssetCache interface {
	Open(context.Context, objectstore.ReleaseAssetCacheInput) (objectstore.CachedReleaseAsset, error)
	OpenFrom(context.Context, objectstore.ReleaseAssetCacheInput, objectstore.ReleaseAssetOpener) (objectstore.CachedReleaseAsset, error)
}

type DeliveryService struct {
	cache                   releaseAssetCache
	localStore              *objectstore.LocalReleaseAssetStore
	tokenProvider           GitHubTokenProvider
	repositoryTokenProvider GitHubRepositoryTokenProvider
	apiBaseURL              string
	assetClient             *http.Client
	redirectClient          *http.Client
	mirrorInspector         *RemoteInspector
}

func (s *DeliveryService) WithLocalStore(store *objectstore.LocalReleaseAssetStore) *DeliveryService {
	if s != nil {
		s.localStore = store
	}
	return s
}

func NewDeliveryService(cache *objectstore.ReleaseAssetCache, tokenProvider GitHubTokenProvider) *DeliveryService {
	return &DeliveryService{
		cache:           cache,
		tokenProvider:   tokenProvider,
		apiBaseURL:      "https://api.github.com",
		assetClient:     newGitHubAssetHTTPClient(true),
		redirectClient:  newGitHubAssetHTTPClient(false),
		mirrorInspector: NewRemoteInspector(),
	}
}

func NewRepositoryDeliveryService(cache *objectstore.ReleaseAssetCache, tokenProvider GitHubRepositoryTokenProvider) *DeliveryService {
	service := NewDeliveryService(cache, nil)
	service.repositoryTokenProvider = tokenProvider
	return service
}

func (s *DeliveryService) Open(ctx context.Context, asset DeliveryAsset) (objectstore.CachedReleaseAsset, error) {
	if s == nil || s.cache == nil {
		return objectstore.CachedReleaseAsset{}, objectstore.ErrReleaseCacheNoSource
	}
	cacheInput := objectstore.ReleaseAssetCacheInput{
		ObjectKey: asset.ObjectKey, FileName: asset.FileName,
		FileSizeBytes: asset.FileSizeBytes, SHA256: asset.SHA256,
	}
	switch strings.TrimSpace(asset.Source) {
	case "s3":
		return s.cache.Open(ctx, cacheInput)
	case "local":
		if s.localStore == nil {
			return objectstore.CachedReleaseAsset{}, objectstore.ErrReleaseCacheNoSource
		}
		return s.localStore.Open(ctx, cacheInput)
	case "github":
		reference, err := parseGitHubAssetObjectKey(asset.ObjectKey, asset.FileName)
		if err != nil {
			return objectstore.CachedReleaseAsset{}, err
		}
		return s.cache.OpenFrom(ctx, cacheInput, func(ctx context.Context) (io.ReadCloser, error) {
			token, err := s.githubToken(ctx, reference.repository)
			if err != nil {
				return nil, err
			}
			client, err := newGitHubClient(reference.repository, token, s.apiBaseURL, s.assetClient)
			if err != nil {
				return nil, err
			}
			return client.OpenAsset(ctx, reference.assetID)
		})
	case "mirror":
		inspector := s.mirrorInspector
		if inspector == nil {
			inspector = NewRemoteInspector()
		}
		downloadURL, err := parseMirrorAssetReference(asset.ObjectKey, asset.FileName, asset.DownloadURL, inspector.allowPrivate)
		if err != nil {
			return objectstore.CachedReleaseAsset{}, err
		}
		return s.cache.OpenFrom(ctx, cacheInput, func(ctx context.Context) (io.ReadCloser, error) {
			opened, err := inspector.Open(ctx, downloadURL)
			if err != nil {
				return nil, err
			}
			if opened.Metadata.FileName != asset.FileName ||
				opened.Metadata.FileSizeBytes > 0 && opened.Metadata.FileSizeBytes != asset.FileSizeBytes {
				_ = opened.Body.Close()
				return nil, fmt.Errorf("%w: %s", ErrMirrorAssetMismatch, asset.FileName)
			}
			return opened.Body, nil
		})
	default:
		return objectstore.CachedReleaseAsset{}, objectstore.ErrReleaseCacheInputInvalid
	}
}

func parseMirrorAssetReference(objectKey, fileName, downloadURL string, allowPrivate bool) (string, error) {
	objectKey = strings.TrimSpace(objectKey)
	fileName = strings.TrimSpace(fileName)
	downloadURL = strings.TrimSpace(downloadURL)
	parsed, err := validateRemoteURL(downloadURL, allowPrivate)
	if err != nil {
		return "", err
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || safeRemoteFileName(fileName) != fileName ||
		path.Clean(objectKey) != objectKey || mirrorAssetObjectKey(parsed.String(), fileName) != objectKey {
		return "", ErrMirrorAssetMismatch
	}
	return parsed.String(), nil
}

func (s *DeliveryService) GitHubRedirect(ctx context.Context, asset DeliveryAsset) (string, error) {
	if s == nil || strings.TrimSpace(asset.Source) != "github" {
		return "", ErrGitHubAssetInvalid
	}
	reference, err := parseGitHubAssetObjectKey(asset.ObjectKey, asset.FileName)
	if err != nil {
		return "", err
	}
	token, err := s.githubToken(ctx, reference.repository)
	if err != nil {
		return "", err
	}
	client, err := newGitHubClient(reference.repository, token, s.apiBaseURL, s.redirectClient)
	if err != nil {
		return "", err
	}
	return client.AssetRedirect(ctx, reference.assetID)
}

func (s *DeliveryService) githubToken(ctx context.Context, repository string) (string, error) {
	if s.repositoryTokenProvider != nil {
		token, err := s.repositoryTokenProvider(ctx, repository)
		if err != nil {
			return "", fmt.Errorf("%w: load GitHub credentials: %v", ErrGitHubUnavailable, err)
		}
		return strings.TrimSpace(token), nil
	}
	if s.tokenProvider == nil {
		return "", nil
	}
	token, err := s.tokenProvider(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: load GitHub credentials: %v", ErrGitHubUnavailable, err)
	}
	return strings.TrimSpace(token), nil
}

type githubAssetReference struct {
	repository string
	assetID    int64
}

func parseGitHubAssetObjectKey(objectKey, fileName string) (githubAssetReference, error) {
	objectKey = strings.TrimSpace(objectKey)
	fileName = strings.TrimSpace(fileName)
	segments := strings.Split(objectKey, "/")
	if len(segments) != 6 || segments[0] != "github" || segments[3] != "assets" ||
		path.Clean(objectKey) != objectKey || segments[5] != fileName || safeRemoteFileName(fileName) != fileName {
		return githubAssetReference{}, ErrGitHubAssetInvalid
	}
	repository := segments[1] + "/" + segments[2]
	assetID, err := strconv.ParseInt(segments[4], 10, 64)
	if err != nil || assetID < 1 || !githubRepositoryPattern.MatchString(repository) || strings.Contains(repository, "..") {
		return githubAssetReference{}, ErrGitHubAssetInvalid
	}
	return githubAssetReference{repository: repository, assetID: assetID}, nil
}

func newGitHubAssetHTTPClient(followRedirects bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client := &http.Client{Transport: transport}
	if followRedirects {
		client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many GitHub asset redirects")
			}
			if !validGitHubAssetRedirect(request.URL) {
				return errors.New("GitHub asset redirected to an untrusted host")
			}
			return nil
		}
	} else {
		client.Timeout = 30 * time.Second
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	return client
}
