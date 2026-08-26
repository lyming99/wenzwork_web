package releaseassets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type RemoteInspector struct {
	client       httpDoer
	allowPrivate bool
}

type OpenedAsset struct {
	Metadata AssetMetadata
	Body     io.ReadCloser
}

func NewRemoteInspector() *RemoteInspector {
	return &RemoteInspector{client: newSafeRemoteHTTPClient()}
}

func newRemoteInspectorForTest(client httpDoer) *RemoteInspector {
	return &RemoteInspector{client: client, allowPrivate: true}
}

func (i *RemoteInspector) Probe(ctx context.Context, rawURL string) (AssetMetadata, error) {
	opened, err := i.Open(ctx, rawURL)
	if err != nil {
		return AssetMetadata{}, err
	}
	defer opened.Body.Close()

	hasher := sha256.New()
	limited := &io.LimitedReader{R: opened.Body, N: MaxAssetBytes + 1}
	written, err := io.CopyBuffer(hasher, limited, make([]byte, 128*1024))
	if err != nil {
		return AssetMetadata{}, fmt.Errorf("%w: %v", ErrRemoteDownloadFailed, err)
	}
	if written > MaxAssetBytes {
		return AssetMetadata{}, ErrRemoteAssetTooLarge
	}
	if written == 0 {
		return AssetMetadata{}, ErrRemoteAssetEmpty
	}
	if opened.Metadata.FileSizeBytes > 0 && opened.Metadata.FileSizeBytes != written {
		return AssetMetadata{}, fmt.Errorf("%w: expected %d bytes, received %d", ErrRemoteDownloadFailed, opened.Metadata.FileSizeBytes, written)
	}
	opened.Metadata.FileSizeBytes = written
	opened.Metadata.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return opened.Metadata, nil
}

func (i *RemoteInspector) Open(ctx context.Context, rawURL string) (OpenedAsset, error) {
	parsed, err := validateRemoteURL(rawURL, i.allowPrivate)
	if err != nil {
		return OpenedAsset{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return OpenedAsset{}, ErrRemoteURLInvalid
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "WenzWork-Release-Inspector/1.0")

	response, err := i.client.Do(request)
	if err != nil {
		return OpenedAsset{}, fmt.Errorf("%w: %v", ErrRemoteDownloadFailed, err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return OpenedAsset{}, fmt.Errorf("%w: remote server returned HTTP %d", ErrRemoteDownloadFailed, response.StatusCode)
	}
	if response.ContentLength > MaxAssetBytes {
		response.Body.Close()
		return OpenedAsset{}, ErrRemoteAssetTooLarge
	}

	fileName := responseFileName(response, parsed)
	contentType := normalizedContentType(response.Header.Get("Content-Type"))
	platform, architecture := InferTarget(fileName)
	fileSize := response.ContentLength
	if fileSize < 0 {
		fileSize = 0
	}
	return OpenedAsset{Metadata: AssetMetadata{
		FileName: fileName, FileSizeBytes: fileSize, ContentType: contentType,
		DownloadURL: parsed.String(), Platform: platform, Architecture: architecture,
	}, Body: response.Body}, nil
}

func validateRemoteURL(rawURL string, allowPrivate bool) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.Fragment != "" {
		return nil, ErrRemoteURLInvalid
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return nil, ErrRemoteAddressForbidden
	}
	if ip, err := netip.ParseAddr(host); err == nil && !allowPrivate && !isPublicRemoteIP(ip) {
		return nil, ErrRemoteAddressForbidden
	}
	return parsed, nil
}

func newSafeRemoteHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, address := range addresses {
			address = address.Unmap()
			if !isPublicRemoteIP(address) {
				lastErr = ErrRemoteAddressForbidden
				continue
			}
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = errors.New("host did not resolve to an address")
		}
		return nil, lastErr
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			_, err := validateRemoteURL(request.URL.String(), false)
			return err
		},
	}
}

func isPublicRemoteIP(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() ||
		address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsUnspecified() || address.IsMulticast() {
		return false
	}
	if address.Is4() {
		carrierGradeNAT := netip.MustParsePrefix("100.64.0.0/10")
		if carrierGradeNAT.Contains(address) {
			return false
		}
	}
	return true
}

func responseFileName(response *http.Response, original *url.URL) string {
	if _, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Disposition")); err == nil {
		if candidate := safeRemoteFileName(parameters["filename"]); candidate != "" {
			return candidate
		}
	}
	for _, candidateURL := range []*url.URL{response.Request.URL, original} {
		if candidateURL != nil {
			if candidate := safeRemoteFileName(path.Base(candidateURL.Path)); candidate != "" {
				return candidate
			}
		}
	}
	return "download.bin"
}

func safeRemoteFileName(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	value = path.Base(value)
	if value == "" || value == "." || value == ".." || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 255 {
		return ""
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return value
}

func normalizedContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil || mediaType == "" || len(mediaType) > 255 {
		return "application/octet-stream"
	}
	return mediaType
}
