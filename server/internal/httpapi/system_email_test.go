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
	"github.com/wenzwork/wenzwork-web/server/internal/emailsettings"
)

type systemEmailServiceStub struct {
	settings  emailsettings.Settings
	update    emailsettings.UpdateInput
	testInput emailsettings.TestInput
}

func (stub *systemEmailServiceStub) GetSettings(context.Context) (emailsettings.Settings, error) {
	return stub.settings, nil
}

func (stub *systemEmailServiceStub) Update(_ context.Context, input emailsettings.UpdateInput) (emailsettings.Settings, error) {
	stub.update = input
	return stub.settings, nil
}

func (stub *systemEmailServiceStub) ResetToLocal(context.Context, int64, uuid.UUID) (emailsettings.Settings, error) {
	return stub.settings, nil
}

func (stub *systemEmailServiceStub) Test(_ context.Context, input emailsettings.TestInput) error {
	stub.testInput = input
	return nil
}

func TestSystemEmailRoutesMapUpdateAndDraftTest(t *testing.T) {
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	actorID := uuid.New()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"super_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	service := &systemEmailServiceStub{settings: emailsettings.Settings{
		Configured: true, Source: emailsettings.SourceDatabase, SMTPHost: "smtp.example.test",
		SMTPPort: 587, MailFrom: "noreply@example.test", Version: 2,
	}}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, SystemEmail: service,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})

	update := authenticatedSystemEmailRequest(http.MethodPut, "/api/v1/admin/system-email",
		`{"smtpHost":"smtp.example.test","smtpPort":587,"smtpUser":"mailer","smtpPassword":"secret","clearSmtpPassword":false,"mailFrom":"noreply@example.test","expectedVersion":1}`,
		csrfToken)
	updateResponse := httptest.NewRecorder()
	router.ServeHTTP(updateResponse, update)
	if updateResponse.Code != http.StatusOK || service.update.ActorUserID != actorID || service.update.SMTPPassword == nil ||
		*service.update.SMTPPassword != "secret" || service.update.ExpectedVersion != 1 {
		t.Fatalf("update status=%d input=%+v body=%s", updateResponse.Code, service.update, updateResponse.Body.String())
	}

	testRequest := authenticatedSystemEmailRequest(http.MethodPost, "/api/v1/admin/system-email/test",
		`{"smtpHost":"smtp.draft.test","smtpPort":2525,"smtpUser":"","clearSmtpPassword":false,"mailFrom":"draft@example.test","recipient":"admin@example.test"}`,
		csrfToken)
	testResponse := httptest.NewRecorder()
	router.ServeHTTP(testResponse, testRequest)
	if testResponse.Code != http.StatusNoContent || service.testInput.SMTPHost != "smtp.draft.test" ||
		service.testInput.Recipient != "admin@example.test" {
		t.Fatalf("test status=%d input=%+v body=%s", testResponse.Code, service.testInput, testResponse.Body.String())
	}
}

func authenticatedSystemEmailRequest(method, path, body, csrfToken string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	return request
}
