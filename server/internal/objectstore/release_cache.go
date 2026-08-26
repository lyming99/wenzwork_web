package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var (
	ErrReleaseCacheInputInvalid = errors.New("release cache input is invalid")
	ErrReleaseCacheCorrupt      = errors.New("release cache object failed integrity validation")
	ErrReleaseCacheNoSource     = errors.New("release cache source is unavailable")
)

type ReleaseAssetCacheInput struct {
	ObjectKey     string
	FileName      string
	FileSizeBytes int64
	SHA256        string
}

type CachedReleaseAsset struct {
	File    *os.File
	ModTime time.Time
}

type ReleaseAssetOpener func(context.Context) (io.ReadCloser, error)

type ReleaseAssetCache struct {
	client  *s3.Client
	bucket  string
	root    string
	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewReleaseAssetCache(cfg S3Config, cacheDirectory string) (*ReleaseAssetCache, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 30 * time.Second
	client, validated, err := newS3Client(cfg, &http.Client{Transport: transport})
	if err != nil {
		return nil, fmt.Errorf("configure release asset cache storage: %w", err)
	}
	cache, err := NewLocalReleaseAssetCache(cacheDirectory)
	if err != nil {
		return nil, err
	}
	cache.client = client
	cache.bucket = validated.Bucket
	return cache, nil
}

func NewLocalReleaseAssetCache(cacheDirectory string) (*ReleaseAssetCache, error) {
	cacheDirectory = strings.TrimSpace(cacheDirectory)
	if cacheDirectory == "" || strings.ContainsRune(cacheDirectory, '\x00') {
		return nil, errors.New("release asset cache directory is required")
	}
	root, err := filepath.Abs(cacheDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve release asset cache directory: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create release asset cache root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect release asset cache root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("release asset cache root is not a directory")
	}
	return &ReleaseAssetCache{root: root, locks: make(map[string]*sync.Mutex)}, nil
}

func (c *ReleaseAssetCache) Open(ctx context.Context, input ReleaseAssetCacheInput) (CachedReleaseAsset, error) {
	if c == nil || c.client == nil || c.bucket == "" {
		return CachedReleaseAsset{}, ErrReleaseCacheNoSource
	}
	return c.OpenFrom(ctx, input, func(ctx context.Context) (io.ReadCloser, error) {
		response, err := c.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(c.bucket), Key: aws.String(input.ObjectKey),
		})
		if err != nil {
			return nil, fmt.Errorf("download release asset from object storage: %w", err)
		}
		return response.Body, nil
	})
}

func (c *ReleaseAssetCache) OpenFrom(ctx context.Context, input ReleaseAssetCacheInput, opener ReleaseAssetOpener) (CachedReleaseAsset, error) {
	input.ObjectKey = strings.TrimSpace(input.ObjectKey)
	input.FileName = strings.TrimSpace(input.FileName)
	input.SHA256 = strings.ToLower(strings.TrimSpace(input.SHA256))
	if c == nil || opener == nil || !validReleaseCacheInput(input) {
		return CachedReleaseAsset{}, ErrReleaseCacheInputInvalid
	}

	lock := c.lockFor(input.SHA256)
	lock.Lock()
	defer lock.Unlock()

	directory := filepath.Join(c.root, input.SHA256[:2])
	finalPath := filepath.Join(directory, input.SHA256)
	if cached, ok := openValidCachedFile(finalPath, input.FileSizeBytes); ok {
		return cached, nil
	}
	if err := os.Remove(finalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return CachedReleaseAsset{}, fmt.Errorf("remove invalid release cache file: %w", err)
	}
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return CachedReleaseAsset{}, fmt.Errorf("create release cache directory: %w", err)
	}

	body, err := opener(ctx)
	if err != nil {
		return CachedReleaseAsset{}, err
	}
	defer body.Close()

	temporary, err := os.CreateTemp(directory, ".release-partial-*")
	if err != nil {
		return CachedReleaseAsset{}, fmt.Errorf("create release cache file: %w", err)
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
		return CachedReleaseAsset{}, fmt.Errorf("secure release cache file: %w", err)
	}

	hasher := sha256.New()
	limited := &io.LimitedReader{R: body, N: input.FileSizeBytes + 1}
	written, err := io.CopyBuffer(io.MultiWriter(temporary, hasher), limited, make([]byte, 128*1024))
	if err != nil {
		return CachedReleaseAsset{}, fmt.Errorf("write release cache file: %w", err)
	}
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if written != input.FileSizeBytes || actualSHA256 != input.SHA256 {
		return CachedReleaseAsset{}, fmt.Errorf("%w: expected %d/%s, received %d/%s", ErrReleaseCacheCorrupt, input.FileSizeBytes, input.SHA256, written, actualSHA256)
	}
	if err := temporary.Sync(); err != nil {
		return CachedReleaseAsset{}, fmt.Errorf("flush release cache file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return CachedReleaseAsset{}, fmt.Errorf("close release cache file: %w", err)
	}
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		if cached, ok := openValidCachedFile(finalPath, input.FileSizeBytes); ok {
			complete = true
			_ = os.Remove(temporaryPath)
			return cached, nil
		}
		return CachedReleaseAsset{}, fmt.Errorf("publish release cache file: %w", err)
	}
	complete = true
	cached, ok := openValidCachedFile(finalPath, input.FileSizeBytes)
	if !ok {
		return CachedReleaseAsset{}, ErrReleaseCacheCorrupt
	}
	return cached, nil
}

func validReleaseCacheInput(input ReleaseAssetCacheInput) bool {
	validSourceKey := strings.HasPrefix(input.ObjectKey, "releases/") ||
		strings.HasPrefix(input.ObjectKey, "github/") || strings.HasPrefix(input.ObjectKey, "mirror/")
	return validSourceKey && path.Clean(input.ObjectKey) == input.ObjectKey &&
		path.Base(input.ObjectKey) == input.FileName && validUploadFileName(input.FileName) &&
		input.FileSizeBytes > 0 && input.FileSizeBytes <= MaxReleaseAssetBytes && releaseSHA256Pattern.MatchString(input.SHA256)
}

func openValidCachedFile(filePath string, expectedSize int64) (CachedReleaseAsset, bool) {
	file, err := os.Open(filePath)
	if err != nil {
		return CachedReleaseAsset{}, false
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		file.Close()
		return CachedReleaseAsset{}, false
	}
	return CachedReleaseAsset{File: file, ModTime: info.ModTime()}, true
}

func (c *ReleaseAssetCache) lockFor(key string) *sync.Mutex {
	c.locksMu.Lock()
	defer c.locksMu.Unlock()
	if lock := c.locks[key]; lock != nil {
		return lock
	}
	lock := &sync.Mutex{}
	c.locks[key] = lock
	return lock
}
