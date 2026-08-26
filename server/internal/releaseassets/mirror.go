package releaseassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	mirrorBaseURLMaximumBytes = 2048
	mirrorCatalogMaximumBytes = 4 * 1024 * 1024
)

type MirrorClient struct {
	baseURL      *url.URL
	client       httpDoer
	allowPrivate bool
}

type mirrorCatalogRelease struct {
	Project      string               `json:"project"`
	Version      string               `json:"version"`
	Channel      string               `json:"channel"`
	Title        string               `json:"title"`
	Summary      string               `json:"summary"`
	ReleaseNotes string               `json:"releaseNotes"`
	PublishedAt  time.Time            `json:"publishedAt"`
	Assets       []mirrorCatalogAsset `json:"assets"`
}

type mirrorCatalogAsset struct {
	FileName        string `json:"fileName"`
	FileSizeBytes   int64  `json:"fileSizeBytes"`
	SHA256          string `json:"sha256"`
	SignatureStatus string `json:"signatureStatus"`
	DownloadURL     string `json:"downloadUrl"`
	Platform        string `json:"platform"`
	Architecture    string `json:"architecture"`
}

func NewMirrorClient(baseURL string) (*MirrorClient, error) {
	return newMirrorClient(baseURL, newSafeRemoteHTTPClient(), false)
}

func newMirrorClient(baseURL string, client httpDoer, allowPrivate bool) (*MirrorClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, ErrMirrorUnconfigured
	}
	parsed, err := url.ParseRequestURI(baseURL)
	if len(baseURL) > mirrorBaseURLMaximumBytes || err != nil || parsed.Host == "" || parsed.Hostname() == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" ||
		(parsed.Path != "" && path.Clean(parsed.Path) != parsed.Path) || client == nil {
		return nil, ErrMirrorURLInvalid
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !allowPrivate {
		if _, err := validateRemoteURL(parsed.String(), false); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMirrorURLInvalid, err)
		}
	}
	return &MirrorClient{baseURL: parsed, client: client, allowPrivate: allowPrivate}, nil
}

func (m *MirrorClient) BaseURL() string {
	return m.baseURL.String()
}

func (m *MirrorClient) Latest(ctx context.Context, project string) (mirrorCatalogRelease, error) {
	project = strings.ToLower(strings.TrimSpace(project))
	if project != "web" && project != "desktop" && project != "mobile" {
		return mirrorCatalogRelease{}, ErrMirrorCatalogInvalid
	}
	endpoint := *m.baseURL
	endpoint.Path = path.Join(endpoint.Path, "/api/v1/releases/latest")
	query := endpoint.Query()
	query.Set("project", project)
	query.Set("channel", "stable")
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return mirrorCatalogRelease{}, fmt.Errorf("%w: %v", ErrMirrorUnavailable, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("User-Agent", "WenzWork-Release-Mirror/1.0")
	response, err := m.client.Do(request)
	if err != nil {
		if errors.Is(err, ErrRemoteAddressForbidden) {
			return mirrorCatalogRelease{}, err
		}
		return mirrorCatalogRelease{}, fmt.Errorf("%w: %v", ErrMirrorUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return mirrorCatalogRelease{}, ErrMirrorReleaseNotFound
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return mirrorCatalogRelease{}, fmt.Errorf("%w: mirror returned HTTP %d", ErrMirrorUnavailable, response.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, mirrorCatalogMaximumBytes+1))
	if err != nil {
		return mirrorCatalogRelease{}, fmt.Errorf("%w: %v", ErrMirrorUnavailable, err)
	}
	if len(payload) > mirrorCatalogMaximumBytes {
		return mirrorCatalogRelease{}, fmt.Errorf("%w: response is too large", ErrMirrorCatalogInvalid)
	}
	var release mirrorCatalogRelease
	if err := json.Unmarshal(payload, &release); err != nil {
		return mirrorCatalogRelease{}, fmt.Errorf("%w: invalid JSON", ErrMirrorCatalogInvalid)
	}
	if err := m.validateRelease(&release, project); err != nil {
		return mirrorCatalogRelease{}, err
	}
	return release, nil
}

func (m *MirrorClient) validateRelease(release *mirrorCatalogRelease, project string) error {
	if release.Project != project || release.Channel != "stable" || release.PublishedAt.IsZero() ||
		!validMirrorText(release.Version, 50, false) || !validMirrorText(release.Title, 120, false) ||
		!validMirrorText(release.Summary, 1000, true) || !validMirrorText(release.ReleaseNotes, 50000, true) ||
		len(release.Assets) == 0 || len(release.Assets) > 100 {
		return ErrMirrorCatalogInvalid
	}
	seen := make(map[string]struct{}, len(release.Assets))
	for index := range release.Assets {
		asset := &release.Assets[index]
		asset.FileName = strings.TrimSpace(asset.FileName)
		asset.SHA256 = strings.ToLower(strings.TrimSpace(asset.SHA256))
		asset.Platform = strings.ToLower(strings.TrimSpace(asset.Platform))
		asset.Architecture = strings.ToLower(strings.TrimSpace(asset.Architecture))
		asset.SignatureStatus = strings.ToLower(strings.TrimSpace(asset.SignatureStatus))
		if safeRemoteFileName(asset.FileName) != asset.FileName || asset.FileSizeBytes < 1 ||
			asset.FileSizeBytes > MaxAssetBytes || !validSHA256(asset.SHA256) ||
			!validTarget(asset.Platform, asset.Architecture) ||
			(asset.SignatureStatus != "unknown" && asset.SignatureStatus != "unsigned" && asset.SignatureStatus != "valid") {
			return fmt.Errorf("%w: asset %d", ErrMirrorCatalogInvalid, index+1)
		}
		downloadURL, err := m.resolveAssetURL(asset.DownloadURL)
		if err != nil {
			return fmt.Errorf("%w: asset %d download URL", ErrMirrorCatalogInvalid, index+1)
		}
		asset.DownloadURL = downloadURL
		key := asset.Platform + "\x00" + asset.Architecture + "\x00" + asset.FileName
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: duplicate asset %q", ErrMirrorCatalogInvalid, asset.FileName)
		}
		seen[key] = struct{}{}
	}
	release.PublishedAt = release.PublishedAt.UTC()
	return nil
}

func (m *MirrorClient) resolveAssetURL(rawURL string) (string, error) {
	reference, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || reference.String() == "" {
		return "", ErrMirrorCatalogInvalid
	}
	baseDirectory := *m.baseURL
	baseDirectory.Path = strings.TrimRight(baseDirectory.Path, "/") + "/"
	target := baseDirectory.ResolveReference(reference)
	if !strings.EqualFold(target.Scheme, m.baseURL.Scheme) || !strings.EqualFold(target.Host, m.baseURL.Host) ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return "", ErrMirrorCatalogInvalid
	}
	if _, err := validateRemoteURL(target.String(), m.allowPrivate); err != nil {
		return "", err
	}
	return target.String(), nil
}

func mirrorAssetObjectKey(downloadURL, fileName string) string {
	digest := sha256.Sum256([]byte(downloadURL))
	return "mirror/" + hex.EncodeToString(digest[:]) + "/" + fileName
}

func validMirrorText(value string, maximumRunes int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximumRunes || !allowEmpty && strings.TrimSpace(value) == "" {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t' {
			return false
		}
	}
	return true
}
