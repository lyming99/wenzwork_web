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
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

type fakeAppAuthService struct {
	created       auth.DeviceAuthorization
	createInput   auth.CreateDeviceAuthorizationInput
	browser       auth.BrowserDeviceAuthorization
	decisionCode  string
	decisionUser  uuid.UUID
	token         auth.AppTokenResult
	tokenErr      error
	passwordInput auth.PasswordAppLoginInput
	passwordErr   error
	refreshErr    error
	authenticated auth.AuthenticatedAppSession
	authErr       error
}

func (f *fakeAppAuthService) CreateDeviceAuthorization(_ context.Context, input auth.CreateDeviceAuthorizationInput) (auth.DeviceAuthorization, error) {
	f.createInput = input
	return f.created, nil
}
func (f *fakeAppAuthService) GetDeviceAuthorization(context.Context, string, uuid.UUID) (auth.BrowserDeviceAuthorization, error) {
	return f.browser, nil
}
func (f *fakeAppAuthService) ApproveDeviceAuthorization(_ context.Context, code string, userID uuid.UUID) (auth.BrowserDeviceAuthorization, error) {
	f.decisionCode, f.decisionUser = code, userID
	return f.browser, nil
}
func (f *fakeAppAuthService) DenyDeviceAuthorization(_ context.Context, code string, userID uuid.UUID) error {
	f.decisionCode, f.decisionUser = code, userID
	return nil
}
func (f *fakeAppAuthService) ExchangeDeviceCode(context.Context, string, string) (auth.AppTokenResult, error) {
	return f.token, f.tokenErr
}
func (f *fakeAppAuthService) LoginAppWithPassword(_ context.Context, input auth.PasswordAppLoginInput) (auth.AppTokenResult, error) {
	f.passwordInput = input
	return f.token, f.passwordErr
}
func (f *fakeAppAuthService) RefreshAppToken(context.Context, string, string) (auth.AppTokenResult, error) {
	return f.token, f.refreshErr
}
func (f *fakeAppAuthService) AuthenticateAppAccessToken(context.Context, string) (auth.AuthenticatedAppSession, error) {
	return f.authenticated, f.authErr
}
func (f *fakeAppAuthService) RevokeAppToken(context.Context, string, string) error { return nil }

func newAppAuthTestRouter(browserAuth AuthService, appAuth AppAuthService) http.Handler {
	return NewRouter(Dependencies{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth:    browserAuth,
		AppAuth: appAuth,
		AuthHTTP: AuthHTTPConfig{
			AllowedOrigins: []string{"http://localhost:5173"},
		},
	})
}

func TestDeviceAuthorizationCreationReturnsSeparatePollingSecret(t *testing.T) {
	requestID := uuid.New()
	service := &fakeAppAuthService{created: auth.DeviceAuthorization{
		RequestID: requestID, DeviceCode: "device-secret", UserCode: "ABCD-EFGH",
		VerificationURI:         "https://example.test/app-login",
		VerificationURIComplete: "https://example.test/app-login?code=ABCD-EFGH",
		ExpiresIn:               600, Interval: 5,
	}}
	router := newAppAuthTestRouter(nil, service)
	deviceID := uuid.NewString()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/device-authorization", strings.NewReader(`{
		"client_id":"wenzwork-desktop","device_id":"`+deviceID+`","device_name":"DESKTOP-TEST"
	}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "192.0.2.10:12345"
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if service.createInput.DeviceID != deviceID || service.createInput.ClientIP != "192.0.2.10" {
		t.Fatalf("create input = %+v", service.createInput)
	}
	if !strings.Contains(response.Body.String(), `"device_code":"device-secret"`) ||
		strings.Contains(service.created.VerificationURIComplete, service.created.DeviceCode) {
		t.Fatalf("device response/link leaked or omitted polling secret: %s", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestBrowserDeviceApprovalRequiresSessionBoundCSRF(t *testing.T) {
	user := auth.User{ID: uuid.New(), Email: "user@example.test", Status: "active", Roles: []string{"user"}}
	csrf, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	browserAuth := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: user, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	appAuth := &fakeAppAuthService{browser: auth.BrowserDeviceAuthorization{
		RequestID: uuid.New(), ClientID: auth.DesktopClientID, DeviceID: uuid.New(),
		DeviceName: "DESKTOP-TEST", UserCode: "ABCD-EFGH", Scope: auth.DeviceAuthorizationScope,
		Status: "approved", ExpiresAt: time.Now().Add(time.Minute),
	}}
	router := newAppAuthTestRouter(browserAuth, appAuth)

	missing := deviceDecisionRequest("", csrf)
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d %s", missingResponse.Code, missingResponse.Body.String())
	}

	request := deviceDecisionRequest(csrf, csrf)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || appAuth.decisionCode != "ABCD-EFGH" || appAuth.decisionUser != user.ID {
		t.Fatalf("approval = %d %s code=%q user=%s", response.Code, response.Body.String(), appAuth.decisionCode, appAuth.decisionUser)
	}
}

func deviceDecisionRequest(csrfHeader, csrfCookie string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/device-authorization/approve", strings.NewReader(`{"userCode":"ABCD-EFGH"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	if csrfHeader != "" {
		request.Header.Set("X-CSRF-Token", csrfHeader)
	}
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "browser-session"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfCookie})
	return request
}

func TestDeviceTokenPollingUsesStandardErrorsAndNoStore(t *testing.T) {
	service := &fakeAppAuthService{tokenErr: auth.ErrDeviceAuthorizationPending}
	router := newAppAuthTestRouter(nil, service)
	form := url.Values{
		"grant_type": {auth.DeviceGrantType}, "client_id": {auth.DesktopClientID}, "device_code": {"pending-code"},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"error":"authorization_pending"`) {
		t.Fatalf("pending token response = %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}

	service.tokenErr = nil
	service.token = auth.AppTokenResult{
		SessionID: uuid.New(), AccessToken: "access-secret", AccessExpiresIn: 900,
		RefreshToken: "refresh-secret", RefreshExpiresIn: 2592000, Scope: auth.DeviceAuthorizationScope,
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"token_type":"Bearer"`) ||
		!strings.Contains(response.Body.String(), `"refresh_token":"refresh-secret"`) {
		t.Fatalf("successful token response = %d %s", response.Code, response.Body.String())
	}
}

func TestPasswordTokenGrantIssuesMobileAppTokens(t *testing.T) {
	service := &fakeAppAuthService{token: auth.AppTokenResult{
		UserID: uuid.New(), SessionID: uuid.New(), AccessToken: "access-secret", AccessExpiresIn: 900,
		RefreshToken: "refresh-secret", RefreshExpiresIn: 2592000, Scope: auth.DeviceAuthorizationScope,
	}}
	router := newAppAuthTestRouter(nil, service)
	deviceID := uuid.NewString()
	form := url.Values{
		"grant_type": {auth.PasswordGrantType}, "client_id": {auth.MobileClientID},
		"device_id": {deviceID}, "device_name": {"WenzWork Android"},
		"email": {"user@example.test"}, "password": {"correct horse battery staple"},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "192.0.2.91:4444"
	request.Header.Set("User-Agent", "WenzWork Mobile")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"access_token":"access-secret"`) {
		t.Fatalf("password token response = %d %s", response.Code, response.Body.String())
	}
	if service.passwordInput.ClientID != auth.MobileClientID || service.passwordInput.DeviceID != deviceID ||
		service.passwordInput.Email != "user@example.test" || service.passwordInput.ClientIP != "192.0.2.91" {
		t.Fatalf("password login input = %+v", service.passwordInput)
	}
}

func TestAccountBearerAuthenticationRejectsMixedCredentials(t *testing.T) {
	user := auth.User{ID: uuid.New(), Email: "app@example.test", Status: "active", Roles: []string{"user"}}
	service := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID: uuid.New(), User: user, ClientID: auth.DesktopClientID, DeviceID: uuid.New(),
		Scope: auth.DeviceAuthorizationScope, AccessExpiresAt: time.Now().Add(time.Minute),
		SessionExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}}
	router := newAppAuthTestRouter(nil, service)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.Header.Set("Authorization", "Bearer access-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), user.Email) {
		t.Fatalf("bearer account = %d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.Header.Set("Authorization", "Bearer access-secret")
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "browser-session"})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"ambiguous_authentication"`) {
		t.Fatalf("mixed authentication = %d %s", response.Code, response.Body.String())
	}
}
