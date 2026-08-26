package analytics

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestNormalizePagePathDropsQueryAndNormalizesSegments(t *testing.T) {
	got, err := normalizePagePath("/help/../pricing?campaign=private#ignored")
	if err != nil || got != "/pricing" {
		t.Fatalf("normalizePagePath() = %q, %v", got, err)
	}
	for _, value := range []string{"", "https://example.test/pricing", "relative", "/bad\npath"} {
		if _, err := normalizePagePath(value); err == nil {
			t.Fatalf("normalizePagePath(%q) error = nil", value)
		}
	}
}

func TestNoopLocationResolverLabelsPrivateAddressesWithoutExternalLookup(t *testing.T) {
	location, err := (NoopLocationResolver{}).Lookup(netip.MustParseAddr("192.168.1.5"))
	if err != nil || location.CountryCode != "ZZ" || location.CountryName != "本地网络" {
		t.Fatalf("Lookup(private) = %+v, %v", location, err)
	}
	location, err = (NoopLocationResolver{}).Lookup(netip.MustParseAddr("203.0.113.8"))
	if err != nil || location != (Location{}) {
		t.Fatalf("Lookup(public) = %+v, %v", location, err)
	}
}

func TestInputSanitizersKeepOnlyCoarseSafeMetadata(t *testing.T) {
	if got := referrerHost("https://Search.Example.test/results?q=secret"); got != "search.example.test" {
		t.Fatalf("referrerHost() = %q", got)
	}
	if got := summarizeUserAgent(" Browser\r\nInjected "); got != "BrowserInjected" {
		t.Fatalf("summarizeUserAgent() = %q", got)
	}
	if got := truncate(strings.Repeat("界", 300), 160); len([]rune(got)) != 160 {
		t.Fatalf("truncate() rune length = %d", len([]rune(got)))
	}
}

func TestValidateReportRangeLimitsReportsToOneYear(t *testing.T) {
	now := time.Now()
	if err := validateReportRange(ReportRange{From: now.Add(-30 * 24 * time.Hour), To: now}); err != nil {
		t.Fatalf("validateReportRange(valid) = %v", err)
	}
	if err := validateReportRange(ReportRange{From: now.Add(-367 * 24 * time.Hour), To: now}); err == nil {
		t.Fatal("validateReportRange(long) error = nil")
	}
	if err := validateReportRange(ReportRange{From: now.Add(-32 * 24 * time.Hour), To: now, Granularity: GranularityHour}); err == nil {
		t.Fatal("validateReportRange(long hourly) error = nil")
	}
	if err := validateReportRange(ReportRange{From: now.Add(-7 * 24 * time.Hour), To: now, Granularity: "minute"}); err == nil {
		t.Fatal("validateReportRange(invalid granularity) error = nil")
	}
}
