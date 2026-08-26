package releaseassets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const githubAPIVersion = "2022-11-28"

var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type GitHubClient struct {
	repository string
	token      string
	apiBaseURL string
	client     *http.Client
}

type githubReleaseResponse struct {
	TagName     string                `json:"tag_name"`
	Name        string                `json:"name"`
	Body        string                `json:"body"`
	HTMLURL     string                `json:"html_url"`
	Prerelease  bool                  `json:"prerelease"`
	PublishedAt time.Time             `json:"published_at"`
	Assets      []githubAssetResponse `json:"assets"`
}

type githubAssetResponse struct {
	ID                 int64  `json:"id"`
	APIURL             string `json:"url"`
	Name               string `json:"name"`
	State              string `json:"state"`
	ContentType        string `json:"content_type"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func NewGitHubClient(repository, token string) (*GitHubClient, error) {
	return newGitHubClient(repository, token, "https://api.github.com", &http.Client{Timeout: 30 * time.Second})
}

func newGitHubClient(repository, token, apiBaseURL string, client *http.Client) (*GitHubClient, error) {
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return nil, ErrGitHubUnconfigured
	}
	if !githubRepositoryPattern.MatchString(repository) || strings.Contains(repository, "..") {
		return nil, fmt.Errorf("invalid GitHub release repository %q", repository)
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(apiBaseURL), "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid GitHub API base URL")
	}
	if client == nil {
		return nil, fmt.Errorf("GitHub HTTP client is required")
	}
	return &GitHubClient{repository: repository, token: strings.TrimSpace(token), apiBaseURL: parsed.String(), client: client}, nil
}

func (g *GitHubClient) Latest(ctx context.Context) (GitHubRelease, error) {
	endpoint := g.apiBaseURL + "/repos/" + g.repository + "/releases/latest"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	g.setHeaders(request, "application/vnd.github+json")
	response, err := g.client.Do(request)
	if err != nil {
		return GitHubRelease{}, fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	defer response.Body.Close()
	if err := githubResponseError(response); err != nil {
		return GitHubRelease{}, err
	}

	var payload githubReleaseResponse
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4*1024*1024))
	if err := decoder.Decode(&payload); err != nil {
		return GitHubRelease{}, fmt.Errorf("%w: invalid response: %v", ErrGitHubUnavailable, err)
	}

	checksums := g.loadChecksums(ctx, payload.Assets)
	assets := make([]AssetMetadata, 0, len(payload.Assets))
	for _, asset := range payload.Assets {
		objectKey, validReference := githubAssetObjectKey(g.repository, asset.ID, asset.Name)
		if asset.State != "uploaded" || asset.Size < 1 || asset.Size > MaxAssetBytes || !validReference ||
			strings.TrimSpace(asset.BrowserDownloadURL) == "" || isChecksumFile(asset.Name) || !isInstallableAssetName(asset.Name) {
			continue
		}
		sha256 := digestSHA256(asset.Digest)
		if sha256 == "" {
			sha256 = checksums[asset.Name]
		}
		platform, architecture := InferTarget(asset.Name)
		assets = append(assets, AssetMetadata{
			FileName: asset.Name, FileSizeBytes: asset.Size, SHA256: sha256,
			ContentType: normalizedContentType(asset.ContentType), DownloadURL: asset.BrowserDownloadURL,
			Source: "github", ObjectKey: objectKey,
			Platform: platform, Architecture: architecture,
		})
	}
	sort.SliceStable(assets, func(i, j int) bool { return assets[i].FileName < assets[j].FileName })

	version := strings.TrimSpace(payload.TagName)
	if len(version) > 1 && (version[0] == 'v' || version[0] == 'V') {
		version = version[1:]
	}
	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = strings.TrimSpace(payload.TagName)
	}
	return GitHubRelease{
		Repository: g.repository, TagName: payload.TagName, Version: version, Name: name,
		Summary: releaseSummary(payload.Body), Body: payload.Body, HTMLURL: payload.HTMLURL,
		Prerelease: payload.Prerelease, PublishedAt: payload.PublishedAt, Assets: assets,
	}, nil
}

func (g *GitHubClient) loadChecksums(ctx context.Context, assets []githubAssetResponse) map[string]string {
	result := make(map[string]string)
	for _, asset := range assets {
		if asset.State != "uploaded" || !isChecksumFile(asset.Name) || asset.Size < 1 || asset.Size > 1024*1024 || asset.ID < 1 {
			continue
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.assetAPIURL(asset.ID), nil)
		if err != nil {
			continue
		}
		g.setHeaders(request, "application/octet-stream")
		response, err := g.client.Do(request)
		if err != nil {
			continue
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			content, readErr := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
			if readErr == nil && len(content) <= 1024*1024 {
				parseChecksumFile(string(content), result)
			}
		}
		response.Body.Close()
		if len(result) > 0 {
			break
		}
	}
	return result
}

func (g *GitHubClient) OpenAsset(ctx context.Context, assetID int64) (io.ReadCloser, error) {
	if assetID < 1 {
		return nil, ErrGitHubAssetInvalid
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.assetAPIURL(assetID), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	g.setHeaders(request, "application/octet-stream")
	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, githubAssetResponseError(response)
	}
	return response.Body, nil
}

func (g *GitHubClient) AssetRedirect(ctx context.Context, assetID int64) (string, error) {
	if assetID < 1 {
		return "", ErrGitHubAssetInvalid
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, g.assetAPIURL(assetID), nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	g.setHeaders(request, "application/octet-stream")
	response, err := g.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrGitHubUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return "", fmt.Errorf("%w: GitHub did not return an asset redirect", ErrGitHubUnavailable)
	}
	if response.StatusCode < http.StatusMultipleChoices || response.StatusCode >= http.StatusBadRequest {
		return "", githubAssetResponseError(response)
	}
	target, err := response.Location()
	if err != nil || !validGitHubAssetRedirect(target) {
		return "", fmt.Errorf("%w: GitHub returned an invalid asset redirect", ErrGitHubUnavailable)
	}
	return target.String(), nil
}

func (g *GitHubClient) assetAPIURL(assetID int64) string {
	return g.apiBaseURL + "/repos/" + g.repository + "/releases/assets/" + strconv.FormatInt(assetID, 10)
}

func (g *GitHubClient) setHeaders(request *http.Request, accept string) {
	request.Header.Set("Accept", accept)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	request.Header.Set("User-Agent", "WenzWork-Release-Importer/1.0")
	if g.token != "" {
		request.Header.Set("Authorization", "Bearer "+g.token)
	}
}

func githubResponseError(response *http.Response) error {
	return githubAPIResponseError(response, ErrGitHubReleaseNotFound)
}

func githubAssetResponseError(response *http.Response) error {
	return githubAPIResponseError(response, ErrGitHubAssetNotFound)
}

func githubAPIResponseError(response *http.Response, notFound error) error {
	switch response.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return notFound
	case http.StatusUnauthorized:
		return ErrGitHubAuthentication
	case http.StatusTooManyRequests:
		return ErrGitHubRateLimited
	case http.StatusForbidden:
		if response.Header.Get("X-RateLimit-Remaining") == "0" {
			return ErrGitHubRateLimited
		}
		return ErrGitHubAuthentication
	}
	return fmt.Errorf("%w: GitHub returned HTTP %d", ErrGitHubUnavailable, response.StatusCode)
}

func githubAssetObjectKey(repository string, assetID int64, fileName string) (string, bool) {
	fileName = strings.TrimSpace(fileName)
	if assetID < 1 || !githubRepositoryPattern.MatchString(repository) || strings.Contains(repository, "..") ||
		safeRemoteFileName(fileName) != fileName {
		return "", false
	}
	return "github/" + repository + "/assets/" + strconv.FormatInt(assetID, 10) + "/" + fileName, true
}

func validGitHubAssetRedirect(target *url.URL) bool {
	if target == nil || target.Scheme != "https" || target.User != nil || target.Hostname() == "" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	return host == "github.com" || strings.HasSuffix(host, ".github.com") ||
		host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

func digestSHA256(value string) string {
	algorithm, digest, found := strings.Cut(strings.ToLower(strings.TrimSpace(value)), ":")
	if !found || algorithm != "sha256" || !validSHA256(digest) {
		return ""
	}
	return digest
}

func isChecksumFile(fileName string) bool {
	name := strings.ToLower(strings.TrimSpace(fileName))
	return strings.Contains(name, "sha256sum") || strings.HasSuffix(name, ".sha256") || strings.HasSuffix(name, ".sha256sum")
}

func isInstallableAssetName(fileName string) bool {
	name := strings.ToLower(strings.TrimSpace(fileName))
	for _, suffix := range []string{
		".tar.gz", ".appimage", ".exe", ".msi", ".dmg", ".pkg", ".deb", ".rpm",
		".apk", ".aab", ".ipa", ".zip",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func parseChecksumFile(content string, destination map[string]string) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 || !validSHA256(fields[0]) {
			continue
		}
		fileName := strings.TrimPrefix(strings.Join(fields[1:], " "), "*")
		fileName = strings.TrimSpace(fileName)
		if fileName != "" {
			destination[fileName] = strings.ToLower(fields[0])
		}
	}
}

func releaseSummary(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#*- ")
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		if len([]rune(line)) > 1000 {
			return string([]rune(line)[:1000])
		}
		return line
	}
	return ""
}
