package analytics

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPILocationResolverUsesPrimaryProviderFirst(t *testing.T) {
	var primaryCalls atomic.Int32
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		primaryCalls.Add(1)
		if request.URL.Query().Get("lang") != "zh-CN" {
			t.Errorf("lang = %q, want zh-CN", request.URL.Query().Get("lang"))
		}
		writeIPAPISuccess(t, w, request, "中国", "CN", "江西", "南昌")
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		http.Error(w, "unexpected fallback", http.StatusInternalServerError)
	}))
	t.Cleanup(fallback.Close)

	resolver := testAPILocationResolver(primary.URL, fallback.URL)
	location, err := resolver.Lookup(netip.MustParseAddr("183.217.223.64"))
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if location.CountryCode != "CN" || location.CountryName != "中国" || location.RegionName != "江西" || location.CityName != "南昌" || location.Source != "primary" {
		t.Fatalf("Lookup() = %+v", location)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 0 {
		t.Fatalf("provider calls = primary %d fallback %d", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestAPILocationResolverFallsBackWhenPrimaryIsUnavailable(t *testing.T) {
	var primaryCalls atomic.Int32
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		fallbackCalls.Add(1)
		writeIPAPISuccess(t, w, request, "美国", "US", "加利福尼亚州", "山景城")
	}))
	t.Cleanup(fallback.Close)

	resolver := testAPILocationResolver(primary.URL, fallback.URL)
	location, err := resolver.Lookup(netip.MustParseAddr("8.8.8.8"))
	if err != nil || location.CountryCode != "US" || location.Source != "fallback" {
		t.Fatalf("Lookup() = %+v, %v", location, err)
	}
	if fallbackCalls.Load() != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls.Load())
	}
	if _, err := resolver.Lookup(netip.MustParseAddr("8.8.4.4")); err != nil {
		t.Fatalf("second Lookup() error = %v", err)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 2 {
		t.Fatalf("provider calls after cooldown = primary %d fallback %d", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestAPILocationResolverHonorsProviderRateLimitHeaders(t *testing.T) {
	var primaryCalls atomic.Int32
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("X-Rl", "0")
		w.Header().Set("X-Ttl", "60")
		writeIPAPISuccess(t, w, request, "中国", "CN", "北京", "北京")
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		fallbackCalls.Add(1)
		writeIPAPISuccess(t, w, request, "美国", "US", "", "")
	}))
	t.Cleanup(fallback.Close)

	resolver := testAPILocationResolver(primary.URL, fallback.URL)
	if _, err := resolver.Lookup(netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatalf("first Lookup() error = %v", err)
	}
	location, err := resolver.Lookup(netip.MustParseAddr("8.8.4.4"))
	if err != nil || location.Source != "fallback" {
		t.Fatalf("second Lookup() = %+v, %v", location, err)
	}
	if primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("provider calls = primary %d fallback %d", primaryCalls.Load(), fallbackCalls.Load())
	}
}

func TestAPILocationResolverReturnsErrorWhenBothProvidersFail(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"fail","message":"reserved range"}`))
	}))
	t.Cleanup(primary.Close)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusBadGateway)
	}))
	t.Cleanup(fallback.Close)

	resolver := testAPILocationResolver(primary.URL, fallback.URL)
	_, err := resolver.LookupContext(context.Background(), netip.MustParseAddr("203.0.113.8"))
	if err == nil || !strings.Contains(err.Error(), "primary") || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("LookupContext() error = %v, want both provider errors", err)
	}
}

func TestAPILocationResolverDoesNotSendPrivateAddresses(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(provider.Close)

	resolver := testAPILocationResolver(provider.URL, provider.URL)
	location, err := resolver.Lookup(netip.MustParseAddr("192.168.10.8"))
	if err != nil || location.CountryName != "本地网络" || location.Source != "local" {
		t.Fatalf("Lookup(private) = %+v, %v", location, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", calls.Load())
	}
}

func testAPILocationResolver(primaryURL, fallbackURL string) *APILocationResolver {
	return newAPILocationResolver(
		&http.Client{Timeout: time.Second},
		[]apiLocationProvider{
			{name: "primary", baseURL: primaryURL},
			{name: "fallback", baseURL: fallbackURL},
		},
	)
}

func writeIPAPISuccess(t *testing.T, w http.ResponseWriter, request *http.Request, country, countryCode, region, city string) {
	t.Helper()
	ip := strings.TrimPrefix(request.URL.Path, "/")
	w.Header().Set("Content-Type", "application/json")
	_, err := fmt.Fprintf(w, `{"status":"success","country":%q,"countryCode":%q,"regionName":%q,"city":%q,"query":%q}`,
		country, countryCode, region, city, ip)
	if err != nil {
		t.Errorf("write response: %v", err)
	}
}
