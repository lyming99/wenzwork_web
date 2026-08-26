package releaseassets

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

const MaxAssetBytes = int64(5 * 1024 * 1024 * 1024)

var (
	ErrRemoteURLInvalid       = errors.New("remote release asset URL is invalid")
	ErrRemoteAddressForbidden = errors.New("remote release asset address is forbidden")
	ErrRemoteDownloadFailed   = errors.New("remote release asset download failed")
	ErrRemoteAssetTooLarge    = errors.New("remote release asset is too large")
	ErrRemoteAssetEmpty       = errors.New("remote release asset is empty")
	ErrRemoteTargetInvalid    = errors.New("remote release asset target is invalid")
	ErrStorageUnavailable     = errors.New("release asset storage is unavailable")
	ErrGitHubUnconfigured     = errors.New("GitHub release repository is not configured")
	ErrGitHubReleaseNotFound  = errors.New("GitHub release was not found")
	ErrGitHubAssetInvalid     = errors.New("GitHub release asset reference is invalid")
	ErrGitHubAssetNotFound    = errors.New("GitHub release asset was not found")
	ErrGitHubAuthentication   = errors.New("GitHub authentication failed")
	ErrGitHubRateLimited      = errors.New("GitHub API rate limit exceeded")
	ErrGitHubUnavailable      = errors.New("GitHub release API is unavailable")
	ErrMirrorUnconfigured     = errors.New("release mirror is not configured")
	ErrMirrorURLInvalid       = errors.New("release mirror URL is invalid")
	ErrMirrorReleaseNotFound  = errors.New("release mirror does not have a published release")
	ErrMirrorCatalogInvalid   = errors.New("release mirror catalog is invalid")
	ErrMirrorAssetMismatch    = errors.New("release mirror asset does not match its catalog metadata")
	ErrMirrorUnavailable      = errors.New("release mirror is unavailable")

	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type AssetMetadata struct {
	FileName      string `json:"fileName"`
	FileSizeBytes int64  `json:"fileSizeBytes"`
	SHA256        string `json:"sha256"`
	ContentType   string `json:"contentType"`
	DownloadURL   string `json:"downloadUrl"`
	Source        string `json:"source,omitempty"`
	ObjectKey     string `json:"objectKey,omitempty"`
	Platform      string `json:"platform,omitempty"`
	Architecture  string `json:"architecture,omitempty"`
}

type RemoteImportInput struct {
	Version      string
	Platform     string
	Architecture string
	DownloadURL  string
}

type StoredAsset struct {
	AssetMetadata
	ObjectKey string `json:"objectKey"`
}

type GitHubRelease struct {
	Repository  string          `json:"repository"`
	TagName     string          `json:"tagName"`
	Version     string          `json:"version"`
	Name        string          `json:"name"`
	Summary     string          `json:"summary"`
	Body        string          `json:"body"`
	HTMLURL     string          `json:"htmlUrl"`
	Prerelease  bool            `json:"prerelease"`
	PublishedAt time.Time       `json:"publishedAt"`
	Assets      []AssetMetadata `json:"assets"`
}

type MirrorReleaseAsset struct {
	FileName        string `json:"fileName"`
	FileSizeBytes   int64  `json:"fileSizeBytes"`
	SHA256          string `json:"sha256"`
	ContentType     string `json:"contentType"`
	DownloadURL     string `json:"downloadUrl"`
	Source          string `json:"source"`
	ObjectKey       string `json:"objectKey"`
	Platform        string `json:"platform"`
	Architecture    string `json:"architecture"`
	SignatureStatus string `json:"signatureStatus"`
}

type MirrorReleaseImport struct {
	MirrorBaseURL string               `json:"mirrorBaseUrl"`
	Project       string               `json:"project"`
	Version       string               `json:"version"`
	Channel       string               `json:"channel"`
	Title         string               `json:"title"`
	Summary       string               `json:"summary"`
	ReleaseNotes  string               `json:"releaseNotes"`
	PublishedAt   time.Time            `json:"publishedAt"`
	Assets        []MirrorReleaseAsset `json:"assets"`
}

func InferTarget(fileName string) (string, string) {
	name := strings.ToLower(fileName)
	platform := ""
	switch {
	case containsAny(name, ".apk", ".aab", "android"):
		platform = "android"
	case containsAny(name, ".ipa", "ios"):
		platform = "ios"
	case containsAny(name, "wenzwork-web-") && !containsAny(name, "server", "host", "relay", "device-agent"):
		platform = "web"
	case containsAny(name, "windows", "win32", "win64", ".exe", ".msi"):
		platform = "windows"
	case containsAny(name, "macos", "darwin", "osx", ".dmg", ".pkg"):
		platform = "macos"
	case containsAny(name, "linux", "appimage", ".deb", ".rpm"):
		platform = "linux"
	}

	architecture := ""
	switch {
	case containsAny(name, "arm64", "aarch64"):
		architecture = "arm64"
	case containsAny(name, "x86_64", "amd64", "x64", "win64"):
		architecture = "x64"
	case containsAny(name, "universal", "-all.", "_all."):
		architecture = "universal"
	case platform != "":
		architecture = "universal"
	}
	return platform, architecture
}

func validSHA256(value string) bool {
	return sha256Pattern.MatchString(strings.ToLower(strings.TrimSpace(value)))
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
