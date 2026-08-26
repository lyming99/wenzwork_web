package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"golang.org/x/text/transform"
)

const (
	aiWebSearchBaseURL          = "https://api.deepseek.com/anthropic/v1"
	aiWebSearchModel            = "deepseek-v4-flash"
	aiWebSearchMaximumResults   = 8
	aiWebFetchMaximumURLLength  = 2048
	aiWebFetchMaximumBytes      = 5_000_000
	aiWebFetchMaximumCharacters = 100_000
	aiWebSearchResponseBytes    = 1 << 20
	aiWebMaximumRedirects       = 5
	aiWebUserAgent              = "wenzwork-device-agent/1"
)

var (
	aiWebWhitespacePattern = regexp.MustCompile(`\s+`)
	aiWebLineTailPattern   = regexp.MustCompile(`[ \t]+\n`)
	aiWebLineHeadPattern   = regexp.MustCompile(`\n[ \t]+`)
	aiWebBlankLinesPattern = regexp.MustCompile(`\n{3,}`)
	aiWebIPv4Denied        = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.88.99.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
	aiWebIPv6Global        = netip.MustParsePrefix("2000::/3")
	aiWebIPv6Documentation = netip.MustParsePrefix("2001:db8::/32")
)

type aiWebError struct {
	Message string
	Code    string
}

func (err *aiWebError) Error() string {
	if err == nil {
		return ""
	}
	return err.Message
}

func newAIWebError(message, code string) error {
	return &aiWebError{Message: message, Code: code}
}

type aiWebSearchSource struct {
	URL         string `json:"url"`
	Title       string `json:"title,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
}

type aiWebSearchResult struct {
	Sources   []aiWebSearchSource
	Answer    string
	Truncated bool
}

type aiWebFetchResult struct {
	URL        string
	StatusCode int
	Body       string
	Kind       string
	Truncated  bool
}

type aiWebToolService interface {
	searchEndpoint(aiConfig) (*url.URL, error)
	validateFetchURL(string) (*url.URL, error)
	search(context.Context, aiConfig, string) (aiWebSearchResult, error)
	fetch(context.Context, string) (aiWebFetchResult, error)
}

type defaultAIWebToolService struct {
	getenv              func(string) string
	lookupIP            func(context.Context, string) ([]net.IPAddr, error)
	searchClientFactory func(*url.URL) *http.Client
	fetchClientFactory  func(*url.URL) *http.Client
}

func newDefaultAIWebToolService() *defaultAIWebToolService {
	return &defaultAIWebToolService{
		getenv:   os.Getenv,
		lookupIP: net.DefaultResolver.LookupIPAddr,
	}
}

func (service *defaultAIWebToolService) environment(name string) string {
	if service != nil && service.getenv != nil {
		return strings.TrimSpace(service.getenv(name))
	}
	return strings.TrimSpace(os.Getenv(name))
}

func (service *defaultAIWebToolService) searchEndpoint(config aiConfig) (*url.URL, error) {
	provider := canonicalAIProvider(config.Provider)
	if provider == "openai" || provider == "openai-compatible" {
		return aiOpenAIWebSearchEndpoint(config)
	}
	if provider != "deepseek" {
		return nil, newAIWebError("当前 AI 提供方不支持内置网页搜索。", "web_search_unsupported_provider")
	}
	raw := service.environment("DEEPSEEK_SEARCH_BASE_URL")
	if raw == "" {
		raw = aiWebSearchBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || !parsed.IsAbs() || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" {
		return nil, newAIWebError("DeepSeek 网页搜索地址无效。", "web_search_invalid_endpoint")
	}
	host := normalizeAIWebHost(parsed.Hostname())
	address := net.ParseIP(host)
	loopback := host == "localhost" || strings.HasSuffix(host, ".localhost") || address != nil && address.IsLoopback()
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, newAIWebError("DeepSeek 网页搜索仅允许 HTTPS，或用于本机开发的 HTTP loopback 地址。", "web_search_invalid_endpoint")
	}
	if !validAIWebPort(parsed) {
		return nil, newAIWebError("DeepSeek 网页搜索地址端口无效。", "web_search_invalid_endpoint")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/messages") {
		path += "/messages"
	}
	parsed.Path = path
	return parsed, nil
}

func aiOpenAIWebSearchEndpoint(config aiConfig) (*url.URL, error) {
	provider := canonicalAIProvider(config.Provider)
	if provider != "openai" && provider != "openai-compatible" {
		return nil, newAIWebError("当前 AI 提供方不支持 OpenAI 网页搜索。", "web_search_unsupported_provider")
	}
	baseURL := strings.TrimSpace(config.BaseURL)
	if baseURL == "" && provider == "openai" {
		baseURL = defaultAIProviderBaseURL("openai")
	}
	parsed, _, err := parseAIBaseURL(baseURL)
	if err != nil || !strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "api.openai.com") {
		return nil, newAIWebError("OpenAI 原生网页搜索仅对 api.openai.com 官方端点启用。", "web_search_unsupported_provider")
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	if basePath == "" {
		basePath = "/v1"
	}
	if basePath != "/v1" {
		return nil, newAIWebError("OpenAI 原生网页搜索地址必须使用官方 /v1 API 路径。", "web_search_unsupported_provider")
	}
	parsed.Path = basePath + "/responses"
	return parsed, nil
}

func aiProviderWebSearchAvailable(config aiConfig) bool {
	provider := canonicalAIProvider(config.Provider)
	if provider == "deepseek" {
		return true
	}
	if provider != "openai" && provider != "openai-compatible" {
		return false
	}
	_, err := aiOpenAIWebSearchEndpoint(config)
	return err == nil
}

func (service *defaultAIWebToolService) search(ctx context.Context, config aiConfig, query string) (aiWebSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 4096 {
		return aiWebSearchResult{}, newAIWebError("网页搜索关键词必须包含 1 至 4096 个字符。", "web_search_invalid_query")
	}
	provider := canonicalAIProvider(config.Provider)
	if provider == "openai" || provider == "openai-compatible" {
		if !aiProviderWebSearchAvailable(config) {
			return aiWebSearchResult{}, newAIWebError("该 OpenAI 兼容端点未声明 OpenAI 原生网页搜索能力。", "web_search_unsupported_provider")
		}
		return service.searchOpenAI(ctx, config, query)
	}
	if provider != "deepseek" {
		return aiWebSearchResult{}, newAIWebError("当前 AI 提供方不支持内置网页搜索。", "web_search_unsupported_provider")
	}
	credential := ""
	if provider == "deepseek" {
		credential = strings.TrimSpace(config.Credential)
	}
	if credential == "" {
		credential = service.environment("DEEPSEEK_API_KEY")
	}
	if credential == "" {
		return aiWebSearchResult{}, newAIWebError("网页搜索需要 DeepSeek API Key；请使用 DeepSeek 模型配置，或在 device-agent 启动环境中设置 DEEPSEEK_API_KEY。", "web_search_credential_missing")
	}
	endpoint, err := service.searchEndpoint(config)
	if err != nil {
		return aiWebSearchResult{}, err
	}
	model := service.environment("DEEPSEEK_SEARCH_MODEL")
	if model == "" {
		model = aiWebSearchModel
	}
	payload, err := json.Marshal(map[string]any{
		"model": model, "max_tokens": 4096,
		"messages": []any{map[string]any{
			"role": "user", "content": []any{map[string]any{
				"type": "text", "text": "Perform a web search for the query: " + query,
			}},
		}},
		"tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}},
	})
	if err != nil {
		return aiWebSearchResult{}, newAIWebError("无法构造 DeepSeek 网页搜索请求。", "web_search_provider_error")
	}
	requestContext, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return aiWebSearchResult{}, newAIWebError("无法构造 DeepSeek 网页搜索请求。", "web_search_provider_error")
	}
	request.Header.Set("x-api-key", credential)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", aiWebUserAgent)
	client := service.newSearchClient(endpoint)
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return aiWebSearchResult{}, ctx.Err()
		}
		if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return aiWebSearchResult{}, newAIWebError("DeepSeek 网页搜索请求超时。", "web_search_timeout")
		}
		return aiWebSearchResult{}, newAIWebError("DeepSeek 网页搜索请求失败。", "web_search_provider_error")
	}
	defer response.Body.Close()
	if isAIWebRedirect(response.StatusCode) {
		return aiWebSearchResult{}, newAIWebError("DeepSeek 网页搜索服务返回了重定向；为避免 API Key 被转发，请检查搜索服务地址。", "web_search_redirect_blocked")
	}
	body, tooLarge, readErr := readAIWebCapped(response.Body, aiWebSearchResponseBytes)
	if readErr != nil {
		return aiWebSearchResult{}, newAIWebError("读取 DeepSeek 网页搜索响应失败。", "web_search_provider_error")
	}
	if tooLarge || !utf8.Valid(body) {
		return aiWebSearchResult{}, newAIWebError("DeepSeek 网页搜索响应无效或超过大小限制。", "web_response_too_large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return aiWebSearchResult{}, newAIWebError(aiDeepSeekWebErrorMessage(response.StatusCode, body), "web_search_provider_error")
	}
	return parseAIWebSearchResponse(body)
}

func (service *defaultAIWebToolService) searchOpenAI(ctx context.Context, config aiConfig, query string) (aiWebSearchResult, error) {
	credential := strings.TrimSpace(config.Credential)
	if credential == "" {
		credential = service.environment("OPENAI_API_KEY")
	}
	if credential == "" {
		return aiWebSearchResult{}, newAIWebError("OpenAI 网页搜索需要 API Key。", "web_search_credential_missing")
	}
	endpoint, err := service.searchEndpoint(config)
	if err != nil {
		return aiWebSearchResult{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"model":       config.Model,
		"input":       "Search the web for the following query and return a concise, factual answer with citations:\n\n" + query,
		"tools":       []any{map[string]any{"type": "web_search"}},
		"tool_choice": "required",
		"include":     []string{"web_search_call.action.sources"},
		"store":       false,
	})
	if err != nil {
		return aiWebSearchResult{}, newAIWebError("无法构造 OpenAI 网页搜索请求。", "web_search_provider_error")
	}
	requestContext, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return aiWebSearchResult{}, newAIWebError("无法构造 OpenAI 网页搜索请求。", "web_search_provider_error")
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", aiWebUserAgent)
	for name, value := range config.NonSecretHeaders {
		request.Header.Set(name, value)
	}
	client := service.newSearchClient(endpoint)
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return aiWebSearchResult{}, ctx.Err()
		}
		if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
			return aiWebSearchResult{}, newAIWebError("OpenAI 网页搜索请求超时。", "web_search_timeout")
		}
		return aiWebSearchResult{}, newAIWebError("OpenAI 网页搜索请求失败。", "web_search_provider_error")
	}
	defer response.Body.Close()
	if isAIWebRedirect(response.StatusCode) {
		return aiWebSearchResult{}, newAIWebError("OpenAI 网页搜索服务返回了重定向；API Key 未被转发。", "web_search_redirect_blocked")
	}
	body, tooLarge, readErr := readAIWebCapped(response.Body, aiWebSearchResponseBytes)
	if readErr != nil || tooLarge || !utf8.Valid(body) {
		return aiWebSearchResult{}, newAIWebError("OpenAI 网页搜索响应无效或超过大小限制。", "web_response_too_large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return aiWebSearchResult{}, newAIWebError(aiOpenAIWebErrorMessage(response.StatusCode, body), "web_search_provider_error")
	}
	return parseAIOpenAIWebSearchResponse(body)
}

func (service *defaultAIWebToolService) newSearchClient(endpoint *url.URL) *http.Client {
	if service != nil && service.searchClientFactory != nil {
		client := service.searchClientFactory(endpoint)
		client.CheckRedirect = rejectAIWebRedirect
		return client
	}
	host := normalizeAIWebHost(endpoint.Hostname())
	address := net.ParseIP(host)
	allowLoopback := host == "localhost" || strings.HasSuffix(host, ".localhost") || address != nil && address.IsLoopback()
	transport := newAIWebTransport()
	transport.DialContext = restrictedAIDialer(endpoint.Hostname(), allowLoopback)
	return &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: rejectAIWebRedirect}
}

func (service *defaultAIWebToolService) validateFetchURL(value string) (*url.URL, error) {
	return validateAIWebFetchURL(value)
}

func validateAIWebFetchURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > aiWebFetchMaximumURLLength || !utf8.ValidString(value) {
		return nil, newAIWebError("网页抓取 URL 无效或超过 2048 个字符。", "web_fetch_invalid_url")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || !parsed.IsAbs() || parsed.Hostname() == "" || parsed.Opaque != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || !validAIWebPort(parsed) {
		return nil, newAIWebError("网页抓取仅支持完整的 HTTP(S) URL。", "web_fetch_invalid_url")
	}
	if parsed.User != nil {
		return nil, newAIWebError("网页地址不能内嵌用户名或密码。", "web_fetch_blocked_url")
	}
	host := normalizeAIWebHost(parsed.Hostname())
	if !allowedAIWebHost(host) {
		return nil, newAIWebError("网页抓取不能访问本机、内网、链路本地或保留地址。", "web_fetch_blocked_url")
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	return parsed, nil
}

func (service *defaultAIWebToolService) fetch(ctx context.Context, rawURL string) (aiWebFetchResult, error) {
	current, err := service.validateFetchURL(rawURL)
	if err != nil {
		return aiWebFetchResult{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	redirects := 0
	for {
		request, err := http.NewRequestWithContext(requestContext, http.MethodGet, current.String(), nil)
		if err != nil {
			return aiWebFetchResult{}, newAIWebError("无法构造网页抓取请求。", "web_fetch_provider_error")
		}
		request.Header.Set("Accept", "text/html,application/xhtml+xml,text/*;q=0.9,application/json;q=0.8,application/xml;q=0.8")
		request.Header.Set("User-Agent", aiWebUserAgent)
		client := service.newFetchClient(current)
		response, requestErr := client.Do(request)
		client.CloseIdleConnections()
		if requestErr != nil {
			if ctx.Err() != nil {
				return aiWebFetchResult{}, ctx.Err()
			}
			if errors.Is(requestContext.Err(), context.DeadlineExceeded) {
				return aiWebFetchResult{}, newAIWebError("网页抓取请求超时。", "web_fetch_timeout")
			}
			var webError *aiWebError
			if errors.As(requestErr, &webError) {
				return aiWebFetchResult{}, webError
			}
			return aiWebFetchResult{}, newAIWebError("网页抓取请求失败。", "web_fetch_provider_error")
		}
		if isAIWebRedirect(response.StatusCode) {
			location := response.Header.Get("Location")
			_ = response.Body.Close()
			if redirects >= aiWebMaximumRedirects {
				return aiWebFetchResult{}, newAIWebError("网页抓取超过 5 次重定向。", "web_fetch_redirect_blocked")
			}
			if strings.TrimSpace(location) == "" {
				return aiWebFetchResult{}, newAIWebError(fmt.Sprintf("网页抓取收到 HTTP %d，但响应没有 Location。", response.StatusCode), "web_fetch_provider_error")
			}
			reference, parseErr := url.Parse(location)
			if parseErr != nil {
				return aiWebFetchResult{}, newAIWebError("网页抓取收到无效的重定向地址。", "web_fetch_provider_error")
			}
			target, validateErr := service.validateFetchURL(current.ResolveReference(reference).String())
			if validateErr != nil {
				return aiWebFetchResult{}, validateErr
			}
			if !sameAIWebOrigin(current, target) {
				return aiWebFetchResult{}, newAIWebError("网页抓取不会自动跟随跨源重定向到 "+aiWebOrigin(target)+"；请对该地址发起新的 web_fetch。", "web_fetch_redirect_blocked")
			}
			current, redirects = target, redirects+1
			continue
		}
		result, readErr := readAIWebFetchResponse(response, current)
		_ = response.Body.Close()
		if readErr != nil {
			return aiWebFetchResult{}, readErr
		}
		return result, nil
	}
}

func (service *defaultAIWebToolService) newFetchClient(target *url.URL) *http.Client {
	if service != nil && service.fetchClientFactory != nil {
		client := service.fetchClientFactory(target)
		client.CheckRedirect = rejectAIWebRedirect
		return client
	}
	transport := newAIWebTransport()
	lookup := net.DefaultResolver.LookupIPAddr
	if service != nil && service.lookupIP != nil {
		lookup = service.lookupIP
	}
	transport.DialContext = restrictedAIWebDialer(target.Hostname(), lookup)
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: rejectAIWebRedirect}
}

func newAIWebTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, ForceAttemptHTTP2: true, MaxIdleConns: 4, MaxIdleConnsPerHost: 2,
		IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 20 * time.Second,
	}
}

func rejectAIWebRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func restrictedAIWebDialer(expectedHost string, lookup func(context.Context, string) ([]net.IPAddr, error)) func(context.Context, string, string) (net.Conn, error) {
	expectedHost = normalizeAIWebHost(expectedHost)
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || normalizeAIWebHost(host) != expectedHost {
			return nil, newAIWebError("网页抓取连接目标与已审批地址不一致。", "web_fetch_blocked_url")
		}
		addresses, err := lookup(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, newAIWebError("网页抓取无法解析目标主机。", "web_fetch_provider_error")
		}
		for _, address := range addresses {
			if !safeAIWebIP(address.IP) {
				return nil, newAIWebError("网页抓取目标解析到了本机、内网或保留地址。", "web_fetch_blocked_url")
			}
		}
		var lastErr error
		for _, candidate := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, newAIWebError("网页抓取无法连接目标主机。", "web_fetch_provider_error")
	}
}

func safeAIWebIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if address.Is4() {
		for _, denied := range aiWebIPv4Denied {
			if denied.Contains(address) {
				return false
			}
		}
		return true
	}
	return aiWebIPv6Global.Contains(address) && !aiWebIPv6Documentation.Contains(address)
}

func allowedAIWebHost(host string) bool {
	host = normalizeAIWebHost(host)
	if host == "" || strings.Contains(host, "%") {
		return false
	}
	if address := net.ParseIP(host); address != nil {
		return safeAIWebIP(address)
	}
	if !strings.Contains(host, ".") || !aiWorkspaceHostPattern.MatchString(host) {
		return false
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".test", ".invalid", ".example", ".onion"} {
		if strings.HasSuffix("."+host, suffix) {
			return false
		}
	}
	return host != "metadata.google.internal" && host != "metadata.goog"
}

func normalizeAIWebHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func validAIWebPort(target *url.URL) bool {
	port := target.Port()
	if port == "" {
		return true
	}
	value, err := strconv.Atoi(port)
	return err == nil && value > 0 && value <= 65535
}

func sameAIWebOrigin(left, right *url.URL) bool {
	return left.Scheme == right.Scheme && normalizeAIWebHost(left.Hostname()) == normalizeAIWebHost(right.Hostname()) && effectiveAIWebPort(left) == effectiveAIWebPort(right)
}

func effectiveAIWebPort(target *url.URL) string {
	if target.Port() != "" {
		return target.Port()
	}
	if target.Scheme == "https" {
		return "443"
	}
	return "80"
}

func aiWebOrigin(target *url.URL) string {
	return target.Scheme + "://" + target.Host
}

func isAIWebRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther ||
		status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
}

func readAIWebFetchResponse(response *http.Response, finalURL *url.URL) (aiWebFetchResult, error) {
	contentType := response.Header.Get("Content-Type")
	kind := classifyAIWebContentType(contentType)
	if kind == "" {
		return aiWebFetchResult{}, newAIWebError("网页抓取不支持 Content-Type："+firstNonEmpty(contentType, "unknown")+"。", "web_fetch_unsupported_content_type")
	}
	if declared := response.Header.Get("Content-Length"); declared != "" {
		if length, err := strconv.ParseInt(declared, 10, 64); err == nil && length > aiWebFetchMaximumBytes {
			return aiWebFetchResult{}, newAIWebError("网页响应超过 5 MB 大小限制。", "web_fetch_too_large")
		}
	}
	body, byteTruncated, err := readAIWebCapped(response.Body, aiWebFetchMaximumBytes)
	if err != nil {
		return aiWebFetchResult{}, newAIWebError("读取网页响应失败。", "web_fetch_provider_error")
	}
	decoded, err := decodeAIWebBody(body, contentType)
	if err != nil {
		return aiWebFetchResult{}, err
	}
	runes := []rune(decoded)
	characterTruncated := len(runes) > aiWebFetchMaximumCharacters
	if characterTruncated {
		decoded = string(runes[:aiWebFetchMaximumCharacters])
	}
	return aiWebFetchResult{
		URL: finalURL.String(), StatusCode: response.StatusCode, Body: decoded, Kind: kind,
		Truncated: byteTruncated || characterTruncated,
	}, nil
}

func readAIWebCapped(reader io.Reader, maximum int) ([]byte, bool, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, int64(maximum)+1))
	if err != nil {
		return nil, false, err
	}
	if len(contents) > maximum {
		return contents[:maximum], true, nil
	}
	return contents, false, nil
}

func classifyAIWebContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		return "html"
	}
	if strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || mediaType == "application/xml" ||
		strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml") {
		return "text"
	}
	return ""
}

func decodeAIWebBody(contents []byte, contentType string) (string, error) {
	_, parameters, _ := mime.ParseMediaType(contentType)
	label := strings.TrimSpace(parameters["charset"])
	if label == "" || strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "utf8") {
		return string(bytes.ToValidUTF8(contents, []byte("�"))), nil
	}
	encoding, _ := charset.Lookup(label)
	if encoding == nil {
		return "", newAIWebError("网页响应使用了不支持的字符编码。", "web_fetch_unsupported_content_type")
	}
	reader := transform.NewReader(bytes.NewReader(contents), encoding.NewDecoder())
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", newAIWebError("网页响应字符解码失败。", "web_fetch_unsupported_content_type")
	}
	return string(bytes.ToValidUTF8(decoded, []byte("�"))), nil
}

func parseAIWebSearchResponse(body []byte) (aiWebSearchResult, error) {
	var response struct {
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &response) != nil || response.Content == nil {
		return aiWebSearchResult{}, newAIWebError("DeepSeek 网页搜索返回了无法解析的 JSON。", "web_search_provider_error")
	}
	snippets := make(map[string]string)
	resultBlocks := 0
	for _, raw := range response.Content {
		var block struct {
			Type      string `json:"type"`
			Citations []struct {
				URL       string `json:"url"`
				CitedText string `json:"cited_text"`
			} `json:"citations"`
		}
		if json.Unmarshal(raw, &block) != nil {
			continue
		}
		if block.Type == "web_search_tool_result" {
			resultBlocks++
		}
		if block.Type != "text" {
			continue
		}
		for _, citation := range block.Citations {
			normalized := safeAIWebSearchSourceURL(citation.URL)
			text := strings.TrimSpace(citation.CitedText)
			if normalized != "" && text != "" {
				if _, exists := snippets[normalized]; !exists {
					snippets[normalized] = truncateAIWebRunes(text, 2000)
				}
			}
		}
	}
	if resultBlocks == 0 {
		return aiWebSearchResult{}, newAIWebError("DeepSeek 没有返回 web_search_tool_result；原生网页搜索可能未触发。", "web_search_provider_error")
	}
	sources := make([]aiWebSearchSource, 0, aiWebSearchMaximumResults+1)
	seen := make(map[string]struct{})
	for _, raw := range response.Content {
		var block struct {
			Type    string            `json:"type"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &block) != nil || block.Type != "web_search_tool_result" {
			continue
		}
		for _, itemRaw := range block.Content {
			var item struct {
				Type    string `json:"type"`
				URL     string `json:"url"`
				Title   string `json:"title"`
				PageAge string `json:"page_age"`
			}
			if json.Unmarshal(itemRaw, &item) != nil || item.Type != "web_search_result" {
				continue
			}
			normalized := safeAIWebSearchSourceURL(item.URL)
			if normalized == "" {
				continue
			}
			if _, exists := seen[normalized]; exists {
				continue
			}
			seen[normalized] = struct{}{}
			sources = append(sources, aiWebSearchSource{
				URL: normalized, Title: truncateAIWebRunes(strings.TrimSpace(item.Title), 500),
				Snippet: snippets[normalized], PublishedAt: truncateAIWebRunes(strings.TrimSpace(item.PageAge), 200),
			})
		}
	}
	truncated := len(sources) > aiWebSearchMaximumResults
	if truncated {
		sources = sources[:aiWebSearchMaximumResults]
	}
	return aiWebSearchResult{Sources: sources, Truncated: truncated}, nil
}

func parseAIOpenAIWebSearchResponse(body []byte) (aiWebSearchResult, error) {
	var response struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
		Output []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Action struct {
				Sources []struct {
					URL   string `json:"url"`
					Title string `json:"title"`
				} `json:"sources"`
			} `json:"action"`
			Content []struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Annotations []struct {
					Type  string `json:"type"`
					URL   string `json:"url"`
					Title string `json:"title"`
				} `json:"annotations"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(body, &response) != nil || response.Output == nil {
		return aiWebSearchResult{}, newAIWebError("OpenAI 网页搜索返回了无法解析的 JSON。", "web_search_provider_error")
	}
	if response.Error != nil || response.Status != "" && response.Status != "completed" {
		return aiWebSearchResult{}, newAIWebError("OpenAI 网页搜索未完成。", "web_search_provider_error")
	}
	searchCompleted := false
	sources := make([]aiWebSearchSource, 0, aiWebSearchMaximumResults+1)
	seen := make(map[string]struct{})
	addSource := func(rawURL, rawTitle string) {
		normalized := safeAIWebSearchSourceURL(rawURL)
		if normalized == "" {
			return
		}
		if _, duplicate := seen[normalized]; duplicate {
			return
		}
		seen[normalized] = struct{}{}
		sources = append(sources, aiWebSearchSource{
			URL: normalized, Title: truncateAIWebRunes(strings.TrimSpace(rawTitle), 500),
		})
	}
	var answer strings.Builder
	for _, item := range response.Output {
		switch item.Type {
		case "web_search_call":
			if item.Status == "" || item.Status == "completed" {
				searchCompleted = true
			}
			for _, source := range item.Action.Sources {
				addSource(source.URL, source.Title)
			}
		case "message":
			for _, content := range item.Content {
				if content.Type == "output_text" && strings.TrimSpace(content.Text) != "" {
					if answer.Len() > 0 {
						answer.WriteString("\n\n")
					}
					answer.WriteString(strings.TrimSpace(content.Text))
				}
				for _, annotation := range content.Annotations {
					if annotation.Type == "url_citation" {
						addSource(annotation.URL, annotation.Title)
					}
				}
			}
		}
	}
	if !searchCompleted {
		return aiWebSearchResult{}, newAIWebError("OpenAI 没有返回已完成的 web_search_call。", "web_search_provider_error")
	}
	truncated := len(sources) > aiWebSearchMaximumResults
	if truncated {
		sources = sources[:aiWebSearchMaximumResults]
	}
	return aiWebSearchResult{
		Sources: sources, Answer: truncateAIWebRunes(strings.TrimSpace(answer.String()), 16000), Truncated: truncated,
	}, nil
}

func safeAIWebSearchSourceURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > aiWebFetchMaximumURLLength {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || !parsed.IsAbs() || parsed.Hostname() == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	return parsed.String()
}

func aiDeepSeekWebErrorMessage(status int, body []byte) string {
	var response struct {
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(body, &response) == nil {
		if strings.TrimSpace(response.Message) != "" {
			return truncateAIWebRunes(strings.TrimSpace(response.Message), 2000)
		}
		switch value := response.Error.(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				return truncateAIWebRunes(strings.TrimSpace(value), 2000)
			}
		case map[string]any:
			if message, ok := value["message"].(string); ok && strings.TrimSpace(message) != "" {
				return truncateAIWebRunes(strings.TrimSpace(message), 2000)
			}
		}
	}
	return fmt.Sprintf("DeepSeek 网页搜索服务返回 HTTP %d。", status)
}

func aiOpenAIWebErrorMessage(status int, body []byte) string {
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &response) == nil && strings.TrimSpace(response.Error.Message) != "" {
		return truncateAIWebRunes(strings.TrimSpace(response.Error.Message), 2000)
	}
	return fmt.Sprintf("OpenAI 网页搜索服务返回 HTTP %d。", status)
}

func formatAIWebSearchResult(result aiWebSearchResult) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(result.Answer) != "" {
		parts = append(parts, "Search answer:\n"+strings.TrimSpace(result.Answer))
	}
	if len(result.Sources) == 0 {
		if strings.TrimSpace(result.Answer) == "" {
			parts = append(parts, "No results found.")
		} else {
			parts = append(parts, "No cited sources were returned.")
		}
	} else {
		lines := make([]string, 0, len(result.Sources))
		for _, source := range result.Sources {
			label := strings.TrimSpace(source.Title)
			if label == "" {
				if parsed, err := url.Parse(source.URL); err == nil {
					label = parsed.Hostname()
				}
			}
			metadata := make([]string, 0, 2)
			if strings.TrimSpace(source.Snippet) != "" {
				metadata = append(metadata, strings.TrimSpace(source.Snippet))
			}
			if strings.TrimSpace(source.PublishedAt) != "" {
				metadata = append(metadata, "("+strings.TrimSpace(source.PublishedAt)+")")
			}
			line := "- [" + escapeAIWebMarkdownLabel(label) + "](" + source.URL + ")"
			if len(metadata) > 0 {
				line += " — " + strings.Join(metadata, " ")
			}
			lines = append(lines, line)
		}
		parts = append(parts, "Sources:\n"+strings.Join(lines, "\n"))
	}
	if result.Truncated {
		parts = append(parts, fmt.Sprintf("(Showing the first %d sources. Refine the query for more.)", len(result.Sources)))
	}
	parts = append(parts, "Cite the relevant URLs above as markdown links in your answer.")
	return strings.Join(parts, "\n\n")
}

func formatAIWebFetchResult(result aiWebFetchResult) (string, bool) {
	body := result.Body
	if result.Kind == "html" {
		if parsed, err := url.Parse(result.URL); err == nil {
			body = aiWebHTMLToMarkdown(body, parsed)
		}
	}
	prefix := fmt.Sprintf("Fetched %s (HTTP %d)\n\n%s", result.URL, result.StatusCode, body)
	footer := "\n\n(Content truncated. Fetch a more specific URL or section for the full text.)"
	truncated := result.Truncated || len(prefix) > maximumAIWorkspaceToolResult
	if !truncated {
		return prefix, false
	}
	maximumPrefix := maximumAIWorkspaceToolResult - len(footer)
	if maximumPrefix < 0 {
		maximumPrefix = 0
	}
	return truncateAIUTF8(prefix, maximumPrefix) + footer, true
}

func aiWebHTMLToMarkdown(source string, baseURL *url.URL) string {
	root, err := nethtml.Parse(strings.NewReader(source))
	if err != nil {
		return source
	}
	if body := findAIWebHTMLElement(root, "body"); body != nil {
		root = body
	}
	renderer := aiWebMarkdownRenderer{baseURL: baseURL}
	value := renderer.node(root, 0, false)
	value = strings.ReplaceAll(value, "\u00a0", " ")
	value = aiWebLineTailPattern.ReplaceAllString(value, "\n")
	value = aiWebLineHeadPattern.ReplaceAllString(value, "\n")
	value = aiWebBlankLinesPattern.ReplaceAllString(value, "\n\n")
	return strings.TrimSpace(value)
}

type aiWebMarkdownRenderer struct {
	baseURL *url.URL
}

func (renderer aiWebMarkdownRenderer) node(node *nethtml.Node, depth int, preformatted bool) string {
	if node == nil || depth > 512 {
		return ""
	}
	if node.Type == nethtml.TextNode {
		if preformatted {
			return node.Data
		}
		return aiWebWhitespacePattern.ReplaceAllString(node.Data, " ")
	}
	if node.Type != nethtml.ElementNode && node.Type != nethtml.DocumentNode {
		return ""
	}
	name := strings.ToLower(node.Data)
	if name == "script" || name == "style" || name == "noscript" || name == "template" || name == "svg" {
		return ""
	}
	children := func(pre bool) string {
		var output strings.Builder
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			output.WriteString(renderer.node(child, depth+1, preformatted || pre))
		}
		return output.String()
	}
	switch name {
	case "br":
		return "\n"
	case "hr":
		return "\n\n---\n\n"
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level, _ := strconv.Atoi(name[1:])
		return "\n\n" + strings.Repeat("#", level) + " " + strings.TrimSpace(children(false)) + "\n\n"
	case "p", "div", "section", "article", "main", "header", "footer", "nav", "aside", "figure", "figcaption":
		return "\n\n" + strings.TrimSpace(children(false)) + "\n\n"
	case "strong", "b":
		return "**" + strings.TrimSpace(children(false)) + "**"
	case "em", "i":
		return "*" + strings.TrimSpace(children(false)) + "*"
	case "del", "s", "strike":
		return "~~" + strings.TrimSpace(children(false)) + "~~"
	case "code":
		if node.Parent != nil && strings.EqualFold(node.Parent.Data, "pre") {
			return children(true)
		}
		value := strings.TrimSpace(children(true))
		fence := "`"
		if strings.Contains(value, "`") {
			fence = "``"
		}
		return fence + value + fence
	case "pre":
		return "\n\n```\n" + strings.TrimRight(children(true), " \t\r\n") + "\n```\n\n"
	case "a":
		label := strings.TrimSpace(children(false))
		href := strings.TrimSpace(aiWebHTMLAttribute(node, "href"))
		resolved := renderer.resolveLink(href)
		if resolved == "" {
			return label
		}
		if label == "" {
			if parsed, err := url.Parse(resolved); err == nil {
				label = parsed.Hostname()
			}
		}
		return "[" + escapeAIWebMarkdownLabel(label) + "](" + resolved + ")"
	case "img":
		resolved := renderer.resolveLink(strings.TrimSpace(aiWebHTMLAttribute(node, "src")))
		if resolved == "" {
			return ""
		}
		return "![" + escapeAIWebMarkdownLabel(aiWebHTMLAttribute(node, "alt")) + "](" + resolved + ")"
	case "ul", "ol":
		ordered := name == "ol"
		index := 1
		if start, err := strconv.Atoi(aiWebHTMLAttribute(node, "start")); err == nil {
			index = start
		}
		lines := make([]string, 0)
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != nethtml.ElementNode || !strings.EqualFold(child.Data, "li") {
				continue
			}
			prefix := "- "
			if ordered {
				prefix = strconv.Itoa(index) + ". "
				index++
			}
			value := strings.ReplaceAll(strings.TrimSpace(renderer.node(child, depth+1, false)), "\n", "\n  ")
			lines = append(lines, prefix+value)
		}
		return "\n\n" + strings.Join(lines, "\n") + "\n\n"
	case "li":
		return strings.TrimSpace(children(false))
	case "blockquote":
		lines := strings.Split(strings.TrimSpace(children(false)), "\n")
		for index := range lines {
			lines[index] = "> " + lines[index]
		}
		return "\n\n" + strings.Join(lines, "\n") + "\n\n"
	case "table":
		return renderer.table(node, depth)
	default:
		return children(false)
	}
}

func (renderer aiWebMarkdownRenderer) table(table *nethtml.Node, depth int) string {
	rows := collectAIWebHTMLElements(table, "tr", true)
	values := make([][]string, 0, len(rows))
	columns := 0
	for _, row := range rows {
		cells := make([]string, 0)
		for cell := row.FirstChild; cell != nil; cell = cell.NextSibling {
			if cell.Type != nethtml.ElementNode || !strings.EqualFold(cell.Data, "th") && !strings.EqualFold(cell.Data, "td") {
				continue
			}
			value := strings.TrimSpace(renderer.node(cell, depth+1, false))
			value = strings.ReplaceAll(value, "|", `\|`)
			value = strings.Join(strings.Fields(value), " ")
			cells = append(cells, value)
		}
		if len(cells) > 0 {
			values = append(values, cells)
			columns = max(columns, len(cells))
		}
	}
	if len(values) == 0 {
		return ""
	}
	markdownRow := func(cells []string) string {
		padded := append([]string(nil), cells...)
		for len(padded) < columns {
			padded = append(padded, "")
		}
		return "| " + strings.Join(padded, " | ") + " |"
	}
	separators := make([]string, columns)
	for index := range separators {
		separators[index] = "---"
	}
	lines := []string{markdownRow(values[0]), markdownRow(separators)}
	for _, row := range values[1:] {
		lines = append(lines, markdownRow(row))
	}
	return "\n\n" + strings.Join(lines, "\n") + "\n\n"
}

func collectAIWebHTMLElements(node *nethtml.Node, name string, skipNestedTables bool) []*nethtml.Node {
	result := make([]*nethtml.Node, 0)
	var visit func(*nethtml.Node)
	visit = func(current *nethtml.Node) {
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == nethtml.ElementNode && strings.EqualFold(child.Data, name) {
				result = append(result, child)
				continue
			}
			if skipNestedTables && child != node && child.Type == nethtml.ElementNode && strings.EqualFold(child.Data, "table") {
				continue
			}
			visit(child)
		}
	}
	visit(node)
	return result
}

func (renderer aiWebMarkdownRenderer) resolveLink(value string) string {
	if value == "" || renderer.baseURL == nil {
		return ""
	}
	reference, err := url.Parse(value)
	if err != nil {
		return ""
	}
	resolved := renderer.baseURL.ResolveReference(reference)
	if (resolved.Scheme != "http" && resolved.Scheme != "https") || resolved.Hostname() == "" || resolved.User != nil {
		return ""
	}
	return resolved.String()
}

func aiWebHTMLAttribute(node *nethtml.Node, name string) string {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return attribute.Val
		}
	}
	return ""
}

func findAIWebHTMLElement(node *nethtml.Node, name string) *nethtml.Node {
	if node == nil {
		return nil
	}
	if node.Type == nethtml.ElementNode && strings.EqualFold(node.Data, name) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findAIWebHTMLElement(child, name); found != nil {
			return found
		}
	}
	return nil
}

func escapeAIWebMarkdownLabel(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "[", "\\[")
	value = strings.ReplaceAll(value, "]", "\\]")
	return strings.Join(strings.Fields(value), " ")
}

func truncateAIWebRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}

func (executor *aiWorkspaceToolExecutor) webSearch(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	query, err := aiWorkspaceString(plan.Call.Arguments, "query", false, 16<<10)
	if err != nil || executor == nil || executor.web == nil {
		return aiWorkspaceToolResult{}, firstError(err, errRPCCapability)
	}
	result, err := executor.web.search(ctx, workspace.aiConfig, query)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	sources := make([]map[string]any, 0, len(result.Sources))
	for _, source := range result.Sources {
		sources = append(sources, map[string]any{
			"url": source.URL, "title": source.Title, "snippet": source.Snippet, "publishedAt": source.PublishedAt,
		})
	}
	toolResult := aiWorkspaceToolSuccess(formatAIWebSearchResult(result), fmt.Sprintf("找到 %d 个网页来源", len(result.Sources)), map[string]any{
		"source_kind": "web", "untrusted": true, "sources": sources, "truncated": result.Truncated,
	})
	aiWorkspaceAttachView(&toolResult, aiWorkspaceWebView(sources, result.Truncated))
	return toolResult, nil
}

func (executor *aiWorkspaceToolExecutor) webFetch(ctx context.Context, _ aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	rawURL, err := aiWorkspaceString(plan.Call.Arguments, "url", false, aiWebFetchMaximumURLLength)
	if err != nil || executor == nil || executor.web == nil {
		return aiWorkspaceToolResult{}, firstError(err, errRPCCapability)
	}
	result, err := executor.web.fetch(ctx, rawURL)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	content, truncated := formatAIWebFetchResult(result)
	return aiWorkspaceToolSuccess(content, fmt.Sprintf("已抓取 %s（HTTP %d）", result.URL, result.StatusCode), map[string]any{
		"source_kind": "web", "untrusted": true, "url": result.URL, "status_code": result.StatusCode, "truncated": truncated,
	}), nil
}
