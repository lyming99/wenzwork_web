package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

type fakeAuthService struct {
	registerResult auth.RegisterResult
	registerErr    error
	loginResult    auth.LoginResult
	loginErr       error
	loginCalls     int
	authenticated  auth.AuthenticatedSession
	profile        auth.User
	mfaSession     auth.Session
	mfaErr         error
}

func (f *fakeAuthService) Register(context.Context, auth.RegisterInput) (auth.RegisterResult, error) {
	if f.registerResult.User.ID == uuid.Nil && !f.registerResult.VerificationSent &&
		!f.registerResult.AlreadyRegistered && f.registerResult.VerificationExpires.IsZero() && f.registerErr == nil {
		return auth.RegisterResult{VerificationSent: true}, nil
	}
	return f.registerResult, f.registerErr
}
func (f *fakeAuthService) ResendVerification(context.Context, string) error { return nil }
func (f *fakeAuthService) VerifyEmail(context.Context, string) (auth.User, error) {
	return f.profile, nil
}
func (f *fakeAuthService) Login(context.Context, auth.LoginInput) (auth.LoginResult, error) {
	f.loginCalls++
	return f.loginResult, f.loginErr
}
func (f *fakeAuthService) AuthenticateSession(context.Context, string) (auth.AuthenticatedSession, error) {
	return f.authenticated, nil
}
func (f *fakeAuthService) Logout(context.Context, string) error { return nil }
func (f *fakeAuthService) ListSessions(context.Context, uuid.UUID, uuid.UUID) ([]auth.SessionSummary, error) {
	return []auth.SessionSummary{}, nil
}
func (f *fakeAuthService) RevokeSession(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (f *fakeAuthService) UpdateProfile(_ context.Context, _ uuid.UUID, displayName string) (auth.User, error) {
	user := f.profile
	user.DisplayName = displayName
	return user, nil
}
func (f *fakeAuthService) RequestPasswordReset(context.Context, string) error  { return nil }
func (f *fakeAuthService) ResetPassword(context.Context, string, string) error { return nil }
func (f *fakeAuthService) ChangePassword(context.Context, uuid.UUID, uuid.UUID, string, string, bool) error {
	return nil
}
func (f *fakeAuthService) GetMFAStatus(context.Context, uuid.UUID) (auth.MFAStatus, error) {
	return auth.MFAStatus{}, nil
}
func (f *fakeAuthService) BeginTOTPEnrollment(context.Context, uuid.UUID, string) (auth.MFAEnrollment, error) {
	return auth.MFAEnrollment{Secret: "SECRET", OTPAuthURI: "otpauth://totp/WenzWork"}, nil
}
func (f *fakeAuthService) ConfirmTOTPEnrollment(context.Context, auth.AuthenticatedSession, string) (auth.MFAConfirmation, error) {
	return auth.MFAConfirmation{}, nil
}
func (f *fakeAuthService) VerifyMFA(context.Context, auth.AuthenticatedSession, string) (auth.Session, error) {
	return f.mfaSession, f.mfaErr
}
func (f *fakeAuthService) RegenerateRecoveryCodes(context.Context, auth.AuthenticatedSession, string) ([]string, error) {
	return []string{}, nil
}
func (f *fakeAuthService) DisableTOTP(context.Context, auth.AuthenticatedSession, string, string) error {
	return nil
}

func newAuthTestRouter(service AuthService) http.Handler {
	return newAuthTestRouterWithConfig(service, AuthHTTPConfig{
		AllowedOrigins: []string{"http://localhost:5173"},
	})
}

func newAuthTestRouterWithConfig(service AuthService, config AuthHTTPConfig) http.Handler {
	if len(config.AllowedOrigins) == 0 {
		config.AllowedOrigins = []string{"http://localhost:5173"}
	}
	return NewRouter(Dependencies{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth:     service,
		AuthHTTP: config,
	})
}

func TestAuthLoginSetsServerSessionAndCSRFCookies(t *testing.T) {
	userID := uuid.New()
	sessionID := uuid.New()
	service := &fakeAuthService{loginResult: auth.LoginResult{Session: auth.Session{
		ID: sessionID,
		User: auth.User{
			ID: userID, Email: "user@example.test", DisplayName: "User", Status: "active", Roles: []string{"user"},
		},
		Token: "session-plaintext", CSRFToken: "csrf-plaintext", RememberMe: true,
		AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(24 * time.Hour),
	}}}
	router := newAuthTestRouter(service)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"user@example.test","password":"correct password","rememberMe":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("cookie count = %d, want 2", len(cookies))
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range cookies {
		switch cookie.Name {
		case "wenzwork_session":
			sessionCookie = cookie
		case "wenzwork_csrf":
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || sessionCookie.Value != "session-plaintext" || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v", sessionCookie)
	}
	if csrfCookie == nil || csrfCookie.Value != "csrf-plaintext" || csrfCookie.HttpOnly {
		t.Fatalf("CSRF cookie = %+v", csrfCookie)
	}
	if strings.Contains(response.Body.String(), "session-plaintext") || strings.Contains(response.Body.String(), "csrf-plaintext") {
		t.Fatal("login JSON leaked session or CSRF token")
	}
	if !strings.Contains(response.Body.String(), `"mfaEnforced":true`) {
		t.Fatalf("login response did not report explicitly enabled MFA enforcement: %s", response.Body.String())
	}
}

func TestAuthLoginReportsConfiguredMFABypass(t *testing.T) {
	service := &fakeAuthService{loginResult: auth.LoginResult{
		Session: auth.Session{
			ID: uuid.New(),
			User: auth.User{
				ID: uuid.New(), Email: "admin@example.test", DisplayName: "Admin", Status: "active",
				Roles: []string{"super_admin"},
			},
			Token: "session-plaintext", CSRFToken: "csrf-plaintext", AssuranceLevel: 1,
			AbsoluteExpiresAt: time.Now().Add(time.Hour),
		},
		MFARequired: true,
	}}
	router := newAuthTestRouterWithConfig(service, AuthHTTPConfig{DisableAdminMFA: true})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"email":"admin@example.test","password":"development password","rememberMe":false}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"mfaRequired":true`) ||
		!strings.Contains(response.Body.String(), `"mfaEnforced":false`) {
		t.Fatalf("MFA-optional login = %d %s", response.Code, response.Body.String())
	}
}

func TestAuthLoginRejectsHTTPOriginWhenSecureCookiesAreConfigured(t *testing.T) {
	service := &fakeAuthService{loginResult: auth.LoginResult{Session: auth.Session{
		ID: uuid.New(), User: auth.User{
			ID: uuid.New(), Email: "admin@example.test", DisplayName: "Admin", Status: "active",
			Roles: []string{"super_admin"},
		},
		Token: "session-plaintext", CSRFToken: "csrf-plaintext", AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}}
	router := newAuthTestRouterWithConfig(service, AuthHTTPConfig{
		CookieSecure: true, AllowedOrigins: []string{"http://host.example.test", "https://host.example.test"},
	})
	request := httptest.NewRequest(http.MethodPost, "http://host.example.test/api/v1/auth/login", strings.NewReader(`{"email":"admin@example.test","password":"correct password","rememberMe":false}`))
	request.Host = "host.example.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://host.example.test")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"secure_transport_required"`) {
		t.Fatalf("insecure secure-cookie login = %d %s", response.Code, response.Body.String())
	}
	if service.loginCalls != 0 {
		t.Fatalf("Login() calls = %d, want 0", service.loginCalls)
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatalf("cookies = %+v, want none", response.Result().Cookies())
	}
}

func TestAuthLoginAllowsHTTPSOriginWithSecureCookies(t *testing.T) {
	service := &fakeAuthService{loginResult: auth.LoginResult{Session: auth.Session{
		ID: uuid.New(), User: auth.User{
			ID: uuid.New(), Email: "admin@example.test", DisplayName: "Admin", Status: "active",
			Roles: []string{"super_admin"},
		},
		Token: "session-plaintext", CSRFToken: "csrf-plaintext", AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}}
	router := newAuthTestRouterWithConfig(service, AuthHTTPConfig{
		CookieSecure: true, AllowedOrigins: []string{"https://host.example.test"},
	})
	request := httptest.NewRequest(http.MethodPost, "https://host.example.test/api/v1/auth/login", strings.NewReader(`{"email":"admin@example.test","password":"correct password","rememberMe":false}`))
	request.Host = "host.example.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://host.example.test")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("secure login = %d %s", response.Code, response.Body.String())
	}
	if service.loginCalls != 1 {
		t.Fatalf("Login() calls = %d, want 1", service.loginCalls)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Name != "__Host-wenzwork_session" || !cookies[0].Secure ||
		cookies[1].Name != "__Host-wenzwork_csrf" || !cookies[1].Secure {
		t.Fatalf("secure cookies = %+v", cookies)
	}
}

func TestAuthRejectsMissingOriginAndUnknownJSON(t *testing.T) {
	router := newAuthTestRouter(&fakeAuthService{})

	missingOrigin := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(missingOrigin, request)
	if missingOrigin.Code != http.StatusForbidden || !strings.Contains(missingOrigin.Body.String(), `"code":"origin_rejected"`) {
		t.Fatalf("missing origin = %d %s", missingOrigin.Code, missingOrigin.Body.String())
	}

	unknownField := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", strings.NewReader(`{"email":"x@example.test","password":"long enough password","displayName":"X","admin":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	router.ServeHTTP(unknownField, request)
	if unknownField.Code != http.StatusBadRequest || !strings.Contains(unknownField.Body.String(), `"code":"invalid_json"`) {
		t.Fatalf("unknown field = %d %s", unknownField.Code, unknownField.Body.String())
	}
}

func TestAuthAllowsDirectSameOriginBeforePublicURLSetup(t *testing.T) {
	router := newAuthTestRouter(&fakeAuthService{})
	request := httptest.NewRequest(http.MethodPost, "http://192.0.2.20:8080/api/v1/auth/register", strings.NewReader(`{"email":"x@example.test","password":"long enough password","displayName":"X"}`))
	request.Host = "192.0.2.20:8080"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://192.0.2.20:8080")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("direct same-origin request = %d %s", response.Code, response.Body.String())
	}
}

func TestAuthRejectsDifferentUnconfiguredOrigin(t *testing.T) {
	router := newAuthTestRouter(&fakeAuthService{})
	request := httptest.NewRequest(http.MethodPost, "http://192.0.2.20:8080/api/v1/auth/register", strings.NewReader(`{}`))
	request.Host = "192.0.2.20:8080"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://attacker.example")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"origin_rejected"`) {
		t.Fatalf("different origin = %d %s", response.Code, response.Body.String())
	}
}

func TestProtectedProfileRequiresBoundCSRFToken(t *testing.T) {
	user := auth.User{ID: uuid.New(), Email: "user@example.test", DisplayName: "Before", Status: "active", Roles: []string{"user"}}
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	service := &fakeAuthService{
		profile: user,
		authenticated: auth.AuthenticatedSession{
			ID: uuid.New(), User: user, CSRFTokenHash: csrfHash, AssuranceLevel: 1,
			AbsoluteExpiresAt: time.Now().Add(time.Hour),
		},
	}
	router := newAuthTestRouter(service)

	missing := httptest.NewRecorder()
	request := profileRequest(t, csrfToken, "")
	router.ServeHTTP(missing, request)
	if missing.Code != http.StatusForbidden || !strings.Contains(missing.Body.String(), `"code":"csrf_rejected"`) {
		t.Fatalf("missing CSRF = %d %s", missing.Code, missing.Body.String())
	}

	valid := httptest.NewRecorder()
	request = profileRequest(t, csrfToken, csrfToken)
	router.ServeHTTP(valid, request)
	if valid.Code != http.StatusOK || !strings.Contains(valid.Body.String(), `"displayName":"After"`) {
		t.Fatalf("valid CSRF = %d %s", valid.Code, valid.Body.String())
	}
}

func profileRequest(t *testing.T, csrfCookie, csrfHeader string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/me", strings.NewReader(`{"displayName":"After"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfCookie})
	if csrfHeader != "" {
		request.Header.Set("X-CSRF-Token", csrfHeader)
	}
	return request
}

func TestPermissionMiddlewareRequiresRoleAndMFA(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/admin", func(c *gin.Context) {
		c.Set(authSessionContextKey, auth.AuthenticatedSession{
			User: auth.User{Roles: []string{"release_admin"}}, AssuranceLevel: 1,
		})
		c.Next()
	}, RequirePermission(auth.PermissionAdminReleases, true), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"mfa_required"`) {
		t.Fatalf("MFA middleware = %d %s", response.Code, response.Body.String())
	}
}

func TestPermissionMiddlewareAllowsLevelOneSessionWhenMFAIsNotRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/admin", func(c *gin.Context) {
		c.Set(authSessionContextKey, auth.AuthenticatedSession{
			User: auth.User{Roles: []string{"release_admin"}}, AssuranceLevel: 1,
		})
		c.Next()
	}, RequirePermission(auth.PermissionAdminReleases, false), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("debug MFA bypass middleware = %d %s", response.Code, response.Body.String())
	}
}

func TestMFAVerificationRotatesCookiesWithoutLeakingTokens(t *testing.T) {
	user := auth.User{ID: uuid.New(), Email: "admin@example.test", DisplayName: "Admin", Status: "active", Roles: []string{"super_admin"}}
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	service := &fakeAuthService{
		authenticated: auth.AuthenticatedSession{
			ID: uuid.New(), User: user, CSRFTokenHash: csrfHash, AssuranceLevel: 1,
			AbsoluteExpiresAt: time.Now().Add(time.Hour),
		},
		mfaSession: auth.Session{
			ID: uuid.New(), User: user, Token: "rotated-session-token", CSRFToken: "rotated-csrf-token",
			AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
		},
	}
	router := newAuthTestRouter(service)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/mfa/totp/verify", strings.NewReader(`{"code":"123456"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "old-session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"assuranceLevel":2`) {
		t.Fatalf("MFA response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "rotated-session-token") || strings.Contains(response.Body.String(), "rotated-csrf-token") {
		t.Fatal("MFA response body leaked rotated credentials")
	}
	if len(response.Result().Cookies()) != 2 {
		t.Fatalf("rotated cookie count = %d, want 2", len(response.Result().Cookies()))
	}
}
