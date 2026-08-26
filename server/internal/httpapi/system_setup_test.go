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

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/systemsetup"
)

type systemSetupServiceStub struct {
	required bool
	settings systemsetup.Settings
	input    systemsetup.ApplyInput
	applied  int
	applyErr error
}

func (stub *systemSetupServiceStub) Required() bool { return stub.required }
func (stub *systemSetupServiceStub) Get(context.Context) (systemsetup.Settings, error) {
	return stub.settings, nil
}
func (stub *systemSetupServiceStub) Apply(_ context.Context, input systemsetup.ApplyInput) (systemsetup.ApplyResult, error) {
	stub.input = input
	stub.applied++
	return systemsetup.ApplyResult{Settings: stub.settings, RestartRequired: true}, stub.applyErr
}

func TestSystemSetupRouteRequiresSuperAdministrator(t *testing.T) {
	service := &systemSetupServiceStub{required: true, settings: systemsetup.Settings{Required: true}}
	for _, test := range []struct {
		name   string
		roles  []string
		status int
	}{
		{name: "super administrator", roles: []string{"super_admin"}, status: http.StatusOK},
		{name: "delegated administrator", roles: []string{"release_admin"}, status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
				ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: test.roles},
				AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
			}}
			router := newSystemSetupTestRouter(authService, service)
			request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/system-setup", nil)
			request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSystemSetupRouteMapsAuthenticatedRequest(t *testing.T) {
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	service := &systemSetupServiceStub{required: true, settings: systemsetup.Settings{Required: true}}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: []string{"super_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	router := newSystemSetupTestRouter(authService, service)
	body := `{"publicBaseUrl":"https://control.example.test","databaseUrl":"postgres://db","redisUrl":"redis://redis:6379/0","smtpHost":"smtp.example.test","smtpPort":587,"smtpUser":"mailer","smtpPassword":"secret","clearSmtpPassword":false,"mailFrom":"noreply@example.test","cookieSecure":true,"adminMfaRequired":true,"registrationEnabled":true,"allowedOrigins":["https://control.example.test"],"webGithubRepository":"acme/web","desktopGithubRepository":"acme/desktop","mobileGithubRepository":"acme/mobile"}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/system-setup", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || service.applied != 1 || service.input.PublicBaseURL != "https://control.example.test" ||
		service.input.SMTPPassword == nil || *service.input.SMTPPassword != "secret" || !service.input.CookieSecure ||
		!service.input.AdminMFARequired || service.input.MobileGitHubRepository != "acme/mobile" {
		t.Fatalf("setup request = status %d applied %d input %+v body=%s", response.Code, service.applied, service.input, response.Body.String())
	}
}

func TestSystemSetupRouteReportsAdministratorEmailTestFailure(t *testing.T) {
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	service := &systemSetupServiceStub{required: true, applyErr: systemsetup.ErrSMTPUnavailable}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: []string{"super_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/system-setup", strings.NewReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()

	newSystemSetupTestRouter(authService, service).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"system_setup_smtp_unavailable"`) {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func newSystemSetupTestRouter(authService AuthService, setup SystemSetupService) http.Handler {
	return NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, SystemSetup: setup,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})
}
