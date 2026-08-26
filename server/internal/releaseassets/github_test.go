package releaseassets

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubClientReadsLatestReleaseAndChecksumFallback(t *testing.T) {
	checksum := strings.Repeat("b", 64)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-GitHub-Api-Version") != githubAPIVersion {
			t.Fatalf("API version header = %q", request.Header.Get("X-GitHub-Api-Version"))
		}
		switch request.URL.Path {
		case "/repos/acme/wenzwork/releases/latest":
			response.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(response, `{
                  "tag_name":"v1.2.3","name":"Fast release","body":"## Changes\nFaster startup",
                  "html_url":"https://github.com/acme/wenzwork/releases/tag/v1.2.3",
                  "prerelease":false,"published_at":"2026-07-22T00:00:00Z",
				  "assets":[
				    {"id":101,"url":%q,"name":"WenzWork-windows-x64.exe","state":"uploaded","content_type":"application/octet-stream","size":42,"digest":"sha256:%s","browser_download_url":"https://github.com/acme/wenzwork/releases/download/v1.2.3/WenzWork-windows-x64.exe"},
				    {"id":102,"url":%q,"name":"WenzWork-linux-arm64.AppImage","state":"uploaded","content_type":"application/octet-stream","size":43,"digest":"","browser_download_url":"https://github.com/acme/wenzwork/releases/download/v1.2.3/WenzWork-linux-arm64.AppImage"},
				    {"id":103,"url":%q,"name":"wenzwork-v1.2.3-SHA256SUMS.txt","state":"uploaded","content_type":"text/plain","size":205,"digest":"","browser_download_url":"https://github.com/acme/wenzwork/releases/download/v1.2.3/checksums"}
				  ]}`, server.URL+"/assets/windows", strings.Repeat("a", 64), server.URL+"/assets/linux", server.URL+"/assets/checksums")
		case "/repos/acme/wenzwork/releases/assets/103":
			if request.Header.Get("Accept") != "application/octet-stream" {
				t.Fatalf("checksum Accept = %q", request.Header.Get("Accept"))
			}
			_, _ = fmt.Fprintf(response, "%s  WenzWork-linux-arm64.AppImage\n", checksum)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client, err := newGitHubClient("acme/wenzwork", "token", server.URL, server.Client())
	if err != nil {
		t.Fatalf("newGitHubClient() error = %v", err)
	}
	release, err := client.Latest(t.Context())
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if release.Version != "1.2.3" || release.Name != "Fast release" || release.Summary != "Changes" || len(release.Assets) != 2 {
		t.Fatalf("Latest() = %+v", release)
	}
	if release.Assets[0].SHA256 != checksum || release.Assets[0].Platform != "linux" || release.Assets[0].Architecture != "arm64" {
		t.Fatalf("linux asset = %+v", release.Assets[0])
	}
	if release.Assets[0].Source != "github" || release.Assets[0].ObjectKey != "github/acme/wenzwork/assets/102/WenzWork-linux-arm64.AppImage" {
		t.Fatalf("linux GitHub reference = %+v", release.Assets[0])
	}
	if release.Assets[1].SHA256 != strings.Repeat("a", 64) || release.Assets[1].Platform != "windows" {
		t.Fatalf("windows asset = %+v", release.Assets[1])
	}
}

func TestGitHubClientMapsNotFoundAuthenticationAndRateLimit(t *testing.T) {
	for _, test := range []struct {
		status int
		header string
		want   error
	}{
		{http.StatusNotFound, "", ErrGitHubReleaseNotFound},
		{http.StatusUnauthorized, "", ErrGitHubAuthentication},
		{http.StatusForbidden, "1", ErrGitHubAuthentication},
		{http.StatusForbidden, "0", ErrGitHubRateLimited},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			if test.header != "" {
				response.Header().Set("X-RateLimit-Remaining", test.header)
			}
			response.WriteHeader(test.status)
		}))
		client, err := newGitHubClient("acme/wenzwork", "", server.URL, server.Client())
		if err != nil {
			t.Fatalf("newGitHubClient() error = %v", err)
		}
		_, got := client.Latest(t.Context())
		server.Close()
		if got != test.want {
			t.Errorf("Latest() error = %v, want %v", got, test.want)
		}
	}
}
