//go:build integration

package analytics

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"gorm.io/gorm"
)

type fixedLocationResolver struct {
	location Location
	calls    atomic.Int64
}

type failingLocationResolver struct {
	calls atomic.Int64
}

func (r *failingLocationResolver) Lookup(_ netip.Addr) (Location, error) {
	r.calls.Add(1)
	return Location{}, errors.New("providers unavailable")
}

func (r *fixedLocationResolver) Lookup(_ netip.Addr) (Location, error) {
	r.calls.Add(1)
	return r.location, nil
}

func TestStoreRecordsAndReportsPageViewsRegionsIPsAndLogins(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	resolver := &fixedLocationResolver{location: Location{
		CountryCode: "CN", CountryName: "中国", RegionName: "北京市", CityName: "北京", Source: "fixture",
	}}
	store, err := NewStore(db, resolver)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	userID := uuid.New()
	sessionID := uuid.New()
	appSessionID := uuid.New()
	releaseID := uuid.New()
	assetID := uuid.New()
	email := "analytics-" + uuid.NewString() + "@example.test"
	firstIP := integrationIP()
	secondIP := integrationIP()
	baseTime := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	if err := createAnalyticsFixture(db, userID, sessionID, appSessionID, email, baseTime); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	if err := createAnalyticsDownloadFixture(db, releaseID, assetID, baseTime); err != nil {
		t.Fatalf("create download fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM release_download_events WHERE client_ip IN (?, ?)", firstIP, secondIP).Error
		_ = db.Exec("DELETE FROM website_page_views WHERE client_ip IN (?, ?)", firstIP, secondIP).Error
		_ = db.Exec("DELETE FROM website_visitors WHERE client_ip IN (?, ?)", firstIP, secondIP).Error
		_ = db.Exec("DELETE FROM ip_geolocation_cache WHERE client_ip IN (?, ?)", firstIP, secondIP).Error
		_ = db.Exec("DELETE FROM release_assets WHERE id = ?", assetID).Error
		_ = db.Exec("DELETE FROM releases WHERE id = ?", releaseID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", userID).Error
	})

	for _, event := range []struct {
		at       time.Time
		path     string
		ip       string
		referrer string
	}{
		{at: baseTime, path: "/", ip: firstIP, referrer: "https://Search.Example.test/results?q=secret"},
		{at: baseTime.Add(time.Hour), path: "/pricing", ip: firstIP, referrer: "https://search.example.test/another"},
		{at: baseTime.Add(24 * time.Hour), path: "/pricing", ip: secondIP},
	} {
		store.now = func() time.Time { return event.at }
		if err := store.RecordPageView(ctx, PageViewInput{Path: event.path, ClientIP: event.ip, Referrer: event.referrer, UserAgent: "Integration Browser"}); err != nil {
			t.Fatalf("RecordPageView() error = %v", err)
		}
	}
	store.now = func() time.Time { return baseTime.Add(2 * time.Hour) }
	login := LoginEventInput{UserID: userID, WebSessionID: sessionID, LoginMethod: LoginMethodPassword, ClientIP: firstIP, UserAgent: "Login Browser"}
	if err := store.RecordLogin(ctx, login); err != nil {
		t.Fatalf("RecordLogin() error = %v", err)
	}
	if err := store.RecordLogin(ctx, login); err != nil {
		t.Fatalf("RecordLogin(idempotent) error = %v", err)
	}
	store.now = func() time.Time { return baseTime.Add(3 * time.Hour) }
	appLogin := LoginEventInput{UserID: userID, AppSessionID: appSessionID, LoginMethod: LoginMethodAppDevice, ClientIP: secondIP, UserAgent: "WenzWork Desktop"}
	if err := store.RecordLogin(ctx, appLogin); err != nil {
		t.Fatalf("RecordLogin(app) error = %v", err)
	}
	if err := store.RecordLogin(ctx, appLogin); err != nil {
		t.Fatalf("RecordLogin(app idempotent) error = %v", err)
	}
	for _, offset := range []time.Duration{90 * time.Minute, 210 * time.Minute} {
		store.now = func() time.Time { return baseTime.Add(offset) }
		if err := store.RecordDownload(ctx, DownloadEventInput{AssetID: assetID, ClientIP: firstIP, UserAgent: "Download Browser"}); err != nil {
			t.Fatalf("RecordDownload() error = %v", err)
		}
	}
	store.now = func() time.Time { return baseTime.Add(90 * time.Minute) }
	if err := store.RecordRegistration(ctx, RegistrationEventInput{UserID: userID, ClientIP: firstIP, UserAgent: "Registration Browser"}); err != nil {
		t.Fatalf("RecordRegistration() error = %v", err)
	}
	if resolver.calls.Load() != 0 {
		t.Fatalf("resolver calls while recording events = %d, want 0", resolver.calls.Load())
	}
	var storedLocations int64
	if err := db.Table("website_page_views").Where("client_ip IN ?", []string{firstIP, secondIP}).
		Where("country_name <> ''").Count(&storedLocations).Error; err != nil {
		t.Fatalf("count stored page-view locations: %v", err)
	}
	if storedLocations != 0 {
		t.Fatalf("stored page-view locations = %d, want 0 before an admin query", storedLocations)
	}

	reportRange := ReportRange{From: baseTime.Add(-time.Hour), To: baseTime.Add(48 * time.Hour), Granularity: GranularityDay}
	overview, err := store.Overview(ctx, reportRange)
	if err != nil {
		t.Fatalf("Overview() error = %v", err)
	}
	if overview.Summary.PageViews != 3 || overview.Summary.UniqueIPs != 2 || overview.Summary.DownloadEvents != 2 || overview.Summary.LoginEvents != 2 || overview.Summary.UniqueLoginIPs != 2 {
		t.Fatalf("summary = %+v", overview.Summary)
	}
	if overview.Summary.DownloadedVisitorIPs != 1 || overview.Summary.RegisteredVisitorIPs != 1 || overview.Summary.VisitorDownloadRate != 0.5 || overview.Summary.VisitorRegistrationRate != 0.5 {
		t.Fatalf("conversion summary = %+v", overview.Summary)
	}
	if len(overview.Regions) != 1 || overview.Regions[0].CountryName != "中国" || overview.Regions[0].PageViews != 3 {
		t.Fatalf("regions = %+v", overview.Regions)
	}
	if resolver.calls.Load() != 2 {
		t.Fatalf("resolver calls after Overview() = %d, want 2 unique IPs", resolver.calls.Load())
	}
	if len(overview.IPs) != 2 || len(overview.Sources) != 2 || len(overview.Paths) != 2 || len(overview.Daily) != 3 || len(overview.Timeline) != 3 || len(overview.RecentNewIPs) != 2 {
		t.Fatalf("overview sizes = IPs %d sources %d paths %d daily %d timeline %d recent %d", len(overview.IPs), len(overview.Sources), len(overview.Paths), len(overview.Daily), len(overview.Timeline), len(overview.RecentNewIPs))
	}
	if overview.Sources[0].ReferrerHost != "search.example.test" || overview.Sources[0].PageViews != 2 || overview.Sources[0].UniqueIPs != 1 || overview.Sources[1].ReferrerHost != "" || overview.Sources[1].PageViews != 1 {
		t.Fatalf("sources = %+v", overview.Sources)
	}
	if overview.Daily[0].DownloadEvents != 2 || overview.Timeline[0].UniqueIPs != 1 || overview.Timeline[0].DownloadEvents != 2 || overview.Timeline[0].DownloadedVisitorIPs != 1 || overview.Timeline[0].RegisteredVisitorIPs != 1 || overview.Timeline[0].VisitorDownloadRate != 1 || overview.Timeline[1].UniqueIPs != 1 || overview.Timeline[1].DownloadedVisitorIPs != 0 {
		t.Fatalf("timeline = %+v", overview.Timeline)
	}
	canonicalIP, _ := normalizeIP(firstIP)
	canonicalAppIP, _ := normalizeIP(secondIP)
	if overview.IPs[0].IP != canonicalIP.String() || !overview.IPs[0].LastSeenAt.Equal(baseTime.Add(time.Hour)) {
		t.Fatalf("first IP stat = %+v", overview.IPs[0])
	}
	if overview.IPs[1].IP != canonicalAppIP.String() || !overview.IPs[1].LastSeenAt.Equal(baseTime.Add(24*time.Hour)) {
		t.Fatalf("second IP stat = %+v", overview.IPs[1])
	}
	if overview.RecentNewIPs[0].IP != canonicalAppIP.String() || overview.RecentNewIPs[0].DownloadedSameDay || overview.RecentNewIPs[0].RegisteredSameDay || overview.RecentNewIPs[1].IP != canonicalIP.String() || !overview.RecentNewIPs[1].DownloadedSameDay || !overview.RecentNewIPs[1].RegisteredSameDay || overview.RecentNewIPs[1].PageViews != 2 {
		t.Fatalf("recent new IPs = %+v", overview.RecentNewIPs)
	}
	var visitors []struct {
		ClientIP  string `gorm:"column:client_ip"`
		PageViews int64  `gorm:"column:page_views"`
	}
	if err := db.Table("website_visitors").Select("host(client_ip) AS client_ip, page_views").Where("client_ip IN ?", []string{firstIP, secondIP}).Order("page_views DESC").Scan(&visitors).Error; err != nil {
		t.Fatalf("read website visitors: %v", err)
	}
	if len(visitors) != 2 || visitors[0].PageViews != 2 || visitors[1].PageViews != 1 {
		t.Fatalf("website visitors = %+v", visitors)
	}

	logins, err := store.ListLoginEvents(ctx, LoginEventFilter{
		ReportRange: reportRange, Query: strings.ToUpper(email), Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListLoginEvents() error = %v", err)
	}
	if logins.Total != 2 || len(logins.Items) != 2 || logins.Items[0].IP != canonicalAppIP.String() || logins.Items[0].LoginMethod != LoginMethodAppDevice || logins.Items[1].IP != canonicalIP.String() || logins.Items[1].LoginMethod != LoginMethodPassword || logins.Items[0].Email != email || logins.Items[0].RegionName != "北京市" {
		t.Fatalf("login events = %+v", logins)
	}
	if !logins.Items[0].LoggedInAt.Equal(baseTime.Add(3*time.Hour)) || !logins.Items[1].LoggedInAt.Equal(baseTime.Add(2*time.Hour)) {
		t.Fatalf("login event times = %+v", logins.Items)
	}
	if resolver.calls.Load() != 2 {
		t.Fatalf("resolver calls after cached login query = %d, want 2", resolver.calls.Load())
	}
	var cacheRows []locationCacheRow
	if err := db.Where("client_ip IN ?", []string{firstIP, secondIP}).Find(&cacheRows).Error; err != nil {
		t.Fatalf("read location cache: %v", err)
	}
	if len(cacheRows) != 2 {
		t.Fatalf("location cache rows = %d, want 2", len(cacheRows))
	}
	for _, row := range cacheRows {
		if row.LookupStatus != locationCacheStatusResolved || row.Source != "fixture" || row.CountryName != "中国" || !row.ExpiresAt.Equal(row.ResolvedAt.Add(locationCacheSuccessTTL)) {
			t.Fatalf("location cache row = %+v", row)
		}
	}
	if _, err := store.Overview(ctx, reportRange); err != nil {
		t.Fatalf("second Overview() error = %v", err)
	}
	if resolver.calls.Load() != 2 {
		t.Fatalf("resolver calls after second cached Overview() = %d, want 2", resolver.calls.Load())
	}
	hourly, err := store.Overview(ctx, ReportRange{
		From: baseTime, To: baseTime.Add(4 * time.Hour), Granularity: GranularityHour,
	})
	if err != nil {
		t.Fatalf("hourly Overview() error = %v", err)
	}
	if hourly.Range.Granularity != GranularityHour || len(hourly.Timeline) != 4 || hourly.Timeline[0].UniqueIPs != 1 || hourly.Timeline[0].VisitorDownloadRate != 1 || hourly.Timeline[0].DownloadEvents != 0 || hourly.Timeline[1].DownloadEvents != 1 || hourly.Timeline[3].DownloadEvents != 1 || hourly.Timeline[3].UniqueIPs != 0 {
		t.Fatalf("hourly timeline = range %+v items %+v", hourly.Range, hourly.Timeline)
	}
}

func TestStoreCachesLookupFailuresWithoutFailingAdminQueries(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	resolver := &failingLocationResolver{}
	store, err := NewStore(db, resolver)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	ip := integrationIP()
	eventTime := time.Date(2040, 1, 2, 3, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return eventTime }
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM website_page_views WHERE client_ip = ?", ip).Error
		_ = db.Exec("DELETE FROM website_visitors WHERE client_ip = ?", ip).Error
		_ = db.Exec("DELETE FROM ip_geolocation_cache WHERE client_ip = ?", ip).Error
	})
	if err := store.RecordPageView(ctx, PageViewInput{Path: "/failure-cache", ClientIP: ip}); err != nil {
		t.Fatalf("RecordPageView() error = %v", err)
	}
	reportRange := ReportRange{From: eventTime.Add(-time.Hour), To: eventTime.Add(time.Hour)}
	overview, err := store.Overview(ctx, reportRange)
	if err != nil {
		t.Fatalf("Overview() error = %v", err)
	}
	if len(overview.IPs) != 1 || overview.IPs[0].CountryName != "未知" {
		t.Fatalf("IP stats = %+v", overview.IPs)
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("resolver calls after first Overview() = %d, want 1", resolver.calls.Load())
	}
	if _, err := store.Overview(ctx, reportRange); err != nil {
		t.Fatalf("second Overview() error = %v", err)
	}
	if resolver.calls.Load() != 1 {
		t.Fatalf("resolver calls after cached failure = %d, want 1", resolver.calls.Load())
	}
	var cacheRow locationCacheRow
	if err := db.Where("client_ip = ?", ip).First(&cacheRow).Error; err != nil {
		t.Fatalf("read failed location cache: %v", err)
	}
	if cacheRow.LookupStatus != locationCacheStatusFailed || !cacheRow.ExpiresAt.Equal(cacheRow.ResolvedAt.Add(locationCacheFailureTTL)) {
		t.Fatalf("failed location cache row = %+v", cacheRow)
	}
}

func createAnalyticsDownloadFixture(db *gorm.DB, releaseID, assetID uuid.UUID, now time.Time) error {
	version := "analytics-" + uuid.NewString()
	if err := db.Exec(`INSERT INTO releases (id, version, channel, title, status, published_at, created_at, updated_at)
		VALUES (?, ?, 'stable', 'Analytics Release', 'published', ?, ?, ?)`,
		releaseID, version, now, now, now).Error; err != nil {
		return err
	}
	return db.Exec(`INSERT INTO release_assets (
		id, release_id, platform, architecture, file_name, file_size_bytes, sha256,
		signature_status, object_key, download_url, status, created_at, updated_at
	) VALUES (?, ?, 'windows', 'x64', 'WenzWork.exe', 42, ?, 'valid', ?, ?, 'published', ?, ?)`,
		assetID, releaseID, strings.Repeat("a", 64),
		"releases/"+version+"/windows/x64/"+assetID.String()+"/WenzWork.exe",
		"https://downloads.example.test/"+assetID.String(), now, now).Error
}

func createAnalyticsFixture(db *gorm.DB, userID, sessionID, appSessionID uuid.UUID, email string, now time.Time) error {
	if err := db.Exec(`INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at, password_changed_at, created_at, updated_at)
		VALUES (?, ?, 'not-used', 'Analytics User', 'active', ?, ?, ?, ?)`, userID, email, now, now, now, now).Error; err != nil {
		return err
	}
	if err := db.Exec(`INSERT INTO sessions (
		id, user_id, token_hash, csrf_token_hash, user_agent_summary, remember_me, assurance_level,
		last_seen_at, idle_expires_at, absolute_expires_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, 'Integration Browser', false, 1, ?, ?, ?, ?, ?)`,
		sessionID, userID, fmt.Sprintf("%x", sha256.Sum256([]byte("token-"+sessionID.String()))), fmt.Sprintf("%x", sha256.Sum256([]byte("csrf-"+sessionID.String()))),
		now, now.Add(time.Hour), now.Add(24*time.Hour), now, now).Error; err != nil {
		return err
	}
	return db.Exec(`INSERT INTO app_sessions (
		id, user_id, client_id, device_id, device_name, scope,
		last_seen_at, idle_expires_at, created_at, updated_at
	) VALUES (?, ?, 'wenzwork-desktop', ?, 'Integration Desktop', 'profile.read membership.read', ?, ?, ?, ?)`,
		appSessionID, userID, uuid.New(), now, now.Add(30*24*time.Hour), now, now).Error
}

func integrationIP() string {
	value := strings.ReplaceAll(uuid.NewString(), "-", "")
	return strings.Join([]string{"2001", "db8", value[0:4], value[4:8], value[8:12], value[12:16], value[16:20], value[20:24]}, ":")
}
