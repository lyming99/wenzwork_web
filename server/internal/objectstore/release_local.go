package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// LocalReleaseAssetStore is the durable origin for assets received by the
// Release push API. It is intentionally separate from the disposable download
// cache: clearing RELEASE_ASSET_CACHE_DIR must not delete locally published
// releases.
type LocalReleaseAssetStore struct {
	root    string
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewLocalReleaseAssetStore(directory string) (*LocalReleaseAssetStore, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" || strings.ContainsRune(directory, '\x00') {
		return nil, errors.New("local release asset directory is required")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, fmt.Errorf("resolve local release asset directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create local release asset directory: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect local release asset directory: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("local release asset root is not a directory")
	}
	return &LocalReleaseAssetStore{root: root, locks: make(map[string]*sync.Mutex)}, nil
}

func (s *LocalReleaseAssetStore) Upload(ctx context.Context, project string, input ReleaseAssetUploadInput, body io.Reader) (ReleaseAssetUpload, error) {
	project = strings.ToLower(strings.TrimSpace(project))
	validated, err := validateReleaseUploadInput(input)
	if err != nil || !validLocalReleaseProject(project) || validated.FileSizeBytes < 1 || validated.SHA256 == "" || body == nil {
		return ReleaseAssetUpload{}, ErrReleaseUploadInvalid
	}
	if err := ctx.Err(); err != nil {
		return ReleaseAssetUpload{}, err
	}

	lock := s.lockFor(validated.SHA256)
	lock.Lock()
	defer lock.Unlock()

	directory := filepath.Join(s.root, validated.SHA256[:2])
	finalPath := filepath.Join(directory, validated.SHA256)
	existingValid := validLocalReleaseFile(finalPath, validated.FileSizeBytes, validated.SHA256)
	if !existingValid {
		if err := os.Remove(finalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ReleaseAssetUpload{}, fmt.Errorf("remove invalid local release asset: %w", err)
		}
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("create local release asset directory: %w", err)
	}

	temporary, err := os.CreateTemp(directory, ".release-push-*")
	if err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("create local release asset: %w", err)
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		_ = temporary.Close()
		if !complete {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o640); err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("secure local release asset: %w", err)
	}

	hasher := sha256.New()
	limited := &io.LimitedReader{R: body, N: MaxReleaseAssetBytes + 1}
	written, err := io.CopyBuffer(io.MultiWriter(temporary, hasher), limited, make([]byte, 128*1024))
	if err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("write local release asset: %w", err)
	}
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	switch {
	case written > MaxReleaseAssetBytes:
		return ReleaseAssetUpload{}, ErrReleaseUploadTooLarge
	case written != validated.FileSizeBytes:
		return ReleaseAssetUpload{}, fmt.Errorf("%w: expected %d bytes, received %d", ErrReleaseUploadSizeMismatch, validated.FileSizeBytes, written)
	case actualSHA256 != validated.SHA256:
		return ReleaseAssetUpload{}, fmt.Errorf("%w: expected %s, received %s", ErrReleaseUploadChecksumMismatch, validated.SHA256, actualSHA256)
	case ctx.Err() != nil:
		return ReleaseAssetUpload{}, ctx.Err()
	}
	if err := temporary.Sync(); err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("flush local release asset: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("close local release asset: %w", err)
	}
	if existingValid {
		complete = true
		_ = os.Remove(temporaryPath)
		return localReleaseAssetUpload(project, validated), nil
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return ReleaseAssetUpload{}, fmt.Errorf("publish local release asset: %w", err)
	}
	complete = true
	return localReleaseAssetUpload(project, validated), nil
}

func (s *LocalReleaseAssetStore) Open(ctx context.Context, input ReleaseAssetCacheInput) (CachedReleaseAsset, error) {
	input.ObjectKey = strings.TrimSpace(input.ObjectKey)
	input.FileName = strings.TrimSpace(input.FileName)
	input.SHA256 = strings.ToLower(strings.TrimSpace(input.SHA256))
	if s == nil || !validLocalReleaseAssetInput(input) {
		return CachedReleaseAsset{}, ErrReleaseCacheInputInvalid
	}
	if err := ctx.Err(); err != nil {
		return CachedReleaseAsset{}, err
	}
	filePath := filepath.Join(s.root, input.SHA256[:2], input.SHA256)
	cached, ok := openValidCachedFile(filePath, input.FileSizeBytes)
	if !ok {
		return CachedReleaseAsset{}, ErrReleaseCacheNoSource
	}
	return cached, nil
}

func (s *LocalReleaseAssetStore) Verify(ctx context.Context, input ReleaseAssetCacheInput) error {
	opened, err := s.Open(ctx, input)
	if err != nil {
		return err
	}
	return opened.File.Close()
}

func localReleaseAssetUpload(project string, input ReleaseAssetUploadInput) ReleaseAssetUpload {
	objectKey := strings.Join([]string{
		"local", project, releaseVersionSegment(input.Version), input.Platform,
		input.Architecture, input.SHA256, input.FileName,
	}, "/")
	return ReleaseAssetUpload{
		ObjectKey: objectKey, DownloadURL: "", FileSizeBytes: input.FileSizeBytes, SHA256: input.SHA256,
	}
}

func validLocalReleaseAssetInput(input ReleaseAssetCacheInput) bool {
	segments := strings.Split(input.ObjectKey, "/")
	return len(segments) == 7 && segments[0] == "local" && validLocalReleaseProject(segments[1]) &&
		segments[2] != "" && segments[3] != "" && segments[4] != "" && segments[5] == input.SHA256 &&
		segments[6] == input.FileName && path.Clean(input.ObjectKey) == input.ObjectKey &&
		validUploadFileName(input.FileName) && input.FileSizeBytes > 0 && input.FileSizeBytes <= MaxReleaseAssetBytes &&
		releaseSHA256Pattern.MatchString(input.SHA256)
}

func validLocalReleaseProject(project string) bool {
	return project == "web" || project == "desktop" || project == "mobile"
}

func validLocalReleaseFile(filePath string, expectedSize int64, expectedSHA256 string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return false
	}
	hasher := sha256.New()
	if _, err := io.CopyBuffer(hasher, file, make([]byte, 128*1024)); err != nil {
		return false
	}
	return hex.EncodeToString(hasher.Sum(nil)) == expectedSHA256
}

func (s *LocalReleaseAssetStore) lockFor(key string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	if lock := s.locks[key]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	s.locks[key] = lock
	return lock
}
