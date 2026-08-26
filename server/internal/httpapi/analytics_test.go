package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/analytics"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

type fakeAnalyticsService struct {
	pageView          analytics.PageViewInput
	login             analytics.LoginEventInput
	download          analytics.DownloadEventInput
	registration      analytics.RegistrationEventInput
	overview          analytics.Overview
	reportRange       analytics.ReportRange
	loginEvents       analytics.LoginEventList
	overviewCalls     int
	loginCalls        int
	downloadCalls     int
	registrationCalls int
}

func (f *fakeAnalyticsService) RecordPageView(_ context.Context, input analytics.PageViewInput) error {
	f.pageView = input
	return nil
}

func (f *fakeAnalyticsService) RecordLogin(_ context.Context, input analytics.LoginEventInput) error {
	f.login = input
	f.loginCalls++
	return nil
}

func (f *fakeAnalyticsService) RecordDownload(_ context.Context, input analytics.DownloadEventInput) error {
	f.download = input
	f.downloadCalls++
	return nil
}

func (f *fakeAnalyticsService) RecordRegistration(_ context.Context, input analytics.RegistrationEventInput) error {
	f.registration = input
	f.registrationCalls++
	return nil
}

func (f *fakeAnalyticsService) Overview(_ context.Context, reportRange analytics.ReportRange) (analytics.Overview, error) {
	f.reportRange = reportRange
	f.overviewCalls++
	return f.overview, nil
}

func (f *fakeAnalyticsService) ListLoginEvents(context.Context, analytics.LoginEventFilter) (analytics.LoginEventList, error) {
	return f.loginEvents, nil
}

func TestPageViewCollectionUsesIPFromTrustedLoopbackProxy(t *testing.T) {
	service := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Analytics:      service,
		TrustedProxies: []string{"127.0.0.1/32"},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/analytics/page-view", strings.NewReader(`{"path":"/pricing","referrer":"https://search.example/result"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Analytics Browser")
	request.Header.Set("X-Forwarded-For", "203.0.113.8")
	request.RemoteAddr = "127.0.0.1:43210"
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if service.pageView.ClientIP != "203.0.113.8" || service.pageView.Path != "/pricing" || service.pageView.UserAgent != "Analytics Browser" {
		t.Fatalf("page view input = %+v", service.pageView)
	}
}

func TestPageViewCollectionIgnoresForwardedIPFromUntrustedPeer(t *testing.T) {
	service := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Analytics: service,
		TrustedProxies: []string{"127.0.0.1/32"},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/analytics/page-view", strings.NewReader(`{"path":"/"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Forwarded-For", "8.8.8.8")
	request.RemoteAddr = "192.0.2.55:43210"
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || service.pageView.ClientIP != "192.0.2.55" {
		t.Fatalf("status = %d client IP=%q body=%s", response.Code, service.pageView.ClientIP, response.Body.String())
	}
}

func TestSuccessfulRegistrationRecordsSourceIP(t *testing.T) {
	userID := uuid.New()
	authService := &fakeAuthService{registerResult: auth.RegisterResult{
		User: auth.User{ID: userID, Email: "new@example.test", Status: "pending"}, VerificationSent: true,
	}}
	statistics := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, Analytics: statistics,
		TrustedProxies: []string{"127.0.0.1/32"},
		AuthHTTP:       AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"new@example.test","password":"correct password","displayName":"New User"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("User-Agent", "Registration Browser")
	request.Header.Set("X-Forwarded-For", "198.51.100.30")
	request.RemoteAddr = "127.0.0.1:34567"
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if statistics.registrationCalls != 1 || statistics.registration.UserID != userID || statistics.registration.ClientIP != "198.51.100.30" || statistics.registration.UserAgent != "Registration Browser" {
		t.Fatalf("registration event = %+v calls=%d", statistics.registration, statistics.registrationCalls)
	}
}

func TestDuplicateRegistrationDoesNotRecordASecondRegistrationEvent(t *testing.T) {
	authService := &fakeAuthService{registerResult: auth.RegisterResult{AlreadyRegistered: true}}
	statistics := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, Analytics: statistics,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"existing@example.test","password":"correct password","displayName":"Existing"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || statistics.registrationCalls != 0 {
		t.Fatalf("status = %d registration calls=%d body=%s", response.Code, statistics.registrationCalls, response.Body.String())
	}
}

func TestSuccessfulPasswordLoginRecordsIPAndSession(t *testing.T) {
	userID := uuid.New()
	sessionID := uuid.New()
	authService := &fakeAuthService{loginResult: auth.LoginResult{Session: auth.Session{
		ID:    sessionID,
		User:  auth.User{ID: userID, Email: "user@example.test", Status: "active", Roles: []string{"user"}},
		Token: "session-token", CSRFToken: "csrf-token", AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}}
	statistics := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, Analytics: statistics,
		TrustedProxies: []string{"127.0.0.1/32"},
		AuthHTTP:       AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"user@example.test","password":"correct password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("User-Agent", "Login Browser")
	request.Header.Set("X-Forwarded-For", "198.51.100.22")
	request.RemoteAddr = "127.0.0.1:12345"
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if statistics.loginCalls != 1 || statistics.login.UserID != userID || statistics.login.WebSessionID != sessionID || statistics.login.LoginMethod != analytics.LoginMethodPassword || statistics.login.ClientIP != "198.51.100.22" || statistics.login.UserAgent != "Login Browser" {
		t.Fatalf("login event = %+v calls=%d", statistics.login, statistics.loginCalls)
	}
}

func TestSuccessfulDeviceExchangeRecordsAppLoginIP(t *testing.T) {
	userID := uuid.New()
	sessionID := uuid.New()
	appAuth := &fakeAppAuthService{token: auth.AppTokenResult{
		UserID: userID, SessionID: sessionID, AccessToken: "access-token", AccessExpiresIn: 900,
		RefreshToken: "refresh-token", RefreshExpiresIn: 2_592_000, Scope: auth.DeviceAuthorizationScope,
	}}
	statistics := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AppAuth: appAuth, Analytics: statistics,
		TrustedProxies: []string{"127.0.0.1/32"},
	})
	form := url.Values{
		"grant_type": {auth.DeviceGrantType}, "client_id": {auth.DesktopClientID}, "device_code": {"approved-device-code"},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "WenzWork Desktop")
	request.Header.Set("X-Forwarded-For", "203.0.113.44")
	request.RemoteAddr = "127.0.0.1:23456"
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if statistics.loginCalls != 1 || statistics.login.UserID != userID || statistics.login.AppSessionID != sessionID || statistics.login.LoginMethod != analytics.LoginMethodAppDevice || statistics.login.ClientIP != "203.0.113.44" {
		t.Fatalf("app login event = %+v calls=%d", statistics.login, statistics.loginCalls)
	}
}

func TestAnalyticsOverviewRequiresAuditPermissionAndMFA(t *testing.T) {
	for _, test := range []struct {
		name      string
		roles     []string
		assurance int16
		want      int
		calls     int
	}{
		{name: "ordinary user", roles: []string{"user"}, assurance: 2, want: http.StatusForbidden},
		{name: "administrator without MFA", roles: []string{"super_admin"}, assurance: 1, want: http.StatusForbidden},
		{name: "super administrator", roles: []string{"super_admin"}, assurance: 2, want: http.StatusOK, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
				ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: test.roles},
				AssuranceLevel: test.assurance, AbsoluteExpiresAt: time.Now().Add(time.Hour),
			}}
			statistics := &fakeAnalyticsService{overview: analytics.Overview{
				Range: analytics.OverviewRange{From: time.Now().Add(-time.Hour), To: time.Now(), Timezone: analytics.ReportTimezone},
				Daily: []analytics.DailyStat{}, Regions: []analytics.RegionStat{}, IPs: []analytics.IPStat{}, Sources: []analytics.SourceStat{}, Paths: []analytics.PathStat{},
			}}
			router := NewRouter(Dependencies{
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, Analytics: statistics,
			})
			request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/analytics/overview?from=2026-07-21T16%3A00%3A00Z&to=2026-07-22T16%3A00%3A00Z&granularity=hour", nil)
			request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
			response := httptest.NewRecorder()

			router.ServeHTTP(response, request)

			if response.Code != test.want || statistics.overviewCalls != test.calls {
				t.Fatalf("status = %d calls=%d body=%s", response.Code, statistics.overviewCalls, response.Body.String())
			}
			if test.calls == 1 && (statistics.reportRange.Granularity != analytics.GranularityHour || !statistics.reportRange.From.Equal(time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)) || !statistics.reportRange.To.Equal(time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC))) {
				t.Fatalf("report range = %+v", statistics.reportRange)
			}
		})
	}
}

func TestAnalyticsOverviewRejectsInvalidGranularity(t *testing.T) {
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: []string{"super_admin"}},
		AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	statistics := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, Analytics: statistics,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/analytics/overview?granularity=minute", nil)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || statistics.overviewCalls != 0 {
		t.Fatalf("status = %d calls=%d body=%s", response.Code, statistics.overviewCalls, response.Body.String())
	}
}

func TestAnalyticsOverviewDefaultsToSevenDays(t *testing.T) {
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: []string{"super_admin"}},
		AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	statistics := &fakeAnalyticsService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, Analytics: statistics,
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/analytics/overview", nil)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || statistics.overviewCalls != 1 {
		t.Fatalf("status = %d calls=%d body=%s", response.Code, statistics.overviewCalls, response.Body.String())
	}
	if statistics.reportRange.Granularity != analytics.GranularityDay || statistics.reportRange.To.Sub(statistics.reportRange.From) != 7*24*time.Hour {
		t.Fatalf("default report range = %+v", statistics.reportRange)
	}
}
