package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultIPLookupTimeout  = 2 * time.Second
	providerFailureCooldown = 30 * time.Second
	maxIPAPIResponseBytes   = 64 << 10
)

type APILocationResolver struct {
	client    *http.Client
	providers []*apiLocationProvider
	now       func() time.Time
}

type apiLocationProvider struct {
	name    string
	baseURL string

	mu               sync.Mutex
	unavailableUntil time.Time
}

type ipAPIResponse struct {
	Status      string `json:"status"`
	Message     string `json:"message"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	RegionName  string `json:"regionName"`
	City        string `json:"city"`
	Query       string `json:"query"`
}

func NewAPILocationResolver() *APILocationResolver {
	return newAPILocationResolver(
		&http.Client{Timeout: defaultIPLookupTimeout},
		[]apiLocationProvider{
			{name: "toolshu", baseURL: "https://api.toolshu.com/api/ip/"},
			{name: "ip-api", baseURL: "http://ip-api.com/json/"},
		},
	)
}

func newAPILocationResolver(client *http.Client, providers []apiLocationProvider) *APILocationResolver {
	if client == nil {
		client = &http.Client{Timeout: defaultIPLookupTimeout}
	}
	providerPointers := make([]*apiLocationProvider, 0, len(providers))
	for index := range providers {
		providers[index].baseURL = strings.TrimRight(strings.TrimSpace(providers[index].baseURL), "/") + "/"
		providerPointers = append(providerPointers, &providers[index])
	}
	return &APILocationResolver{client: client, providers: providerPointers, now: time.Now}
}

func (r *APILocationResolver) Lookup(ip netip.Addr) (Location, error) {
	return r.LookupContext(context.Background(), ip)
}

func (r *APILocationResolver) LookupContext(ctx context.Context, ip netip.Addr) (Location, error) {
	if !ip.IsValid() {
		return Location{}, errors.New("IP address is invalid")
	}
	ip = ip.WithZone("").Unmap()
	if localAddress(ip) {
		return localNetworkLocation(), nil
	}
	if r == nil || len(r.providers) == 0 {
		return Location{}, errors.New("IP geolocation providers are not configured")
	}

	var lookupErrors []error
	for _, provider := range r.providers {
		if provider == nil || !provider.available(r.now()) {
			continue
		}
		location, err := r.lookupProvider(ctx, ip, provider)
		if err == nil && hasLocation(location) {
			return location, nil
		}
		if err == nil {
			err = errors.New("provider returned an empty location")
		}
		lookupErrors = append(lookupErrors, fmt.Errorf("%s: %w", provider.name, err))
	}
	if len(lookupErrors) == 0 {
		return Location{}, errors.New("all IP geolocation providers are temporarily rate limited")
	}
	return Location{}, errors.Join(lookupErrors...)
}

func (r *APILocationResolver) lookupProvider(parentCtx context.Context, ip netip.Addr, provider *apiLocationProvider) (Location, error) {
	lookupURL, err := url.Parse(provider.baseURL + url.PathEscape(ip.String()))
	if err != nil {
		return Location{}, fmt.Errorf("build lookup URL: %w", err)
	}
	query := lookupURL.Query()
	query.Set("lang", "zh-CN")
	lookupURL.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(parentCtx, defaultIPLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupURL.String(), nil)
	if err != nil {
		return Location{}, fmt.Errorf("create lookup request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "WenzWork-IP-Geolocation/1.0")

	response, err := r.client.Do(req)
	if err != nil {
		if parentCtx.Err() == nil {
			provider.markUnavailable(r.now(), providerFailureCooldown)
		}
		return Location{}, fmt.Errorf("request provider: %w", err)
	}
	defer response.Body.Close()
	provider.observeRateLimit(response.StatusCode, response.Header, r.now())
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if response.StatusCode != http.StatusTooManyRequests {
			provider.markUnavailable(r.now(), providerFailureCooldown)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return Location{}, fmt.Errorf("provider returned HTTP %d", response.StatusCode)
	}

	limited := &io.LimitedReader{R: response.Body, N: maxIPAPIResponseBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		provider.markUnavailable(r.now(), providerFailureCooldown)
		return Location{}, fmt.Errorf("read provider response: %w", err)
	}
	if len(body) > maxIPAPIResponseBytes {
		provider.markUnavailable(r.now(), providerFailureCooldown)
		return Location{}, errors.New("provider response is too large")
	}
	var payload ipAPIResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		provider.markUnavailable(r.now(), providerFailureCooldown)
		return Location{}, fmt.Errorf("decode provider response: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(payload.Status), "success") {
		message := strings.TrimSpace(payload.Message)
		if message == "" {
			message = "lookup failed"
		}
		return Location{}, errors.New(message)
	}
	if payload.Query != "" {
		responseIP, err := netip.ParseAddr(strings.TrimSpace(payload.Query))
		if err != nil || responseIP.WithZone("").Unmap() != ip {
			provider.markUnavailable(r.now(), providerFailureCooldown)
			return Location{}, errors.New("provider response IP does not match the request")
		}
	}
	return Location{
		CountryCode: payload.CountryCode,
		CountryName: payload.Country,
		RegionName:  payload.RegionName,
		CityName:    payload.City,
		Source:      provider.name,
	}, nil
}

func (p *apiLocationProvider) available(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !now.Before(p.unavailableUntil)
}

func (p *apiLocationProvider) observeRateLimit(status int, headers http.Header, now time.Time) {
	remaining, remainingErr := strconv.Atoi(strings.TrimSpace(headers.Get("X-Rl")))
	if status != http.StatusTooManyRequests && (remainingErr != nil || remaining > 0) {
		return
	}
	retryAfter := parseRetryAfter(headers, now)
	if retryAfter <= 0 {
		retryAfter = time.Minute
	}
	p.markUnavailable(now, retryAfter)
}

func (p *apiLocationProvider) markUnavailable(now time.Time, duration time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	blockedUntil := now.Add(duration)
	if blockedUntil.After(p.unavailableUntil) {
		p.unavailableUntil = blockedUntil
	}
}

func parseRetryAfter(headers http.Header, now time.Time) time.Duration {
	for _, name := range []string{"X-Ttl", "Retry-After"} {
		raw := strings.TrimSpace(headers.Get(name))
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		if name == "Retry-After" {
			if value, err := http.ParseTime(raw); err == nil && value.After(now) {
				return value.Sub(now)
			}
		}
	}
	return 0
}
