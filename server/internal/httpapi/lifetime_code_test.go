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
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
)

type fakeAdminLifetimeCodeService struct {
	items  []membership.LifetimeCodeDelivery
	result membership.LifetimeCodeDeliveryResult
	err    error
	limit  int
	input  membership.LifetimeCodeDeliveryInput
}

func (f *fakeAdminLifetimeCodeService) ListLifetimeCodeDeliveries(_ context.Context, limit int) ([]membership.LifetimeCodeDelivery, error) {
	f.limit = limit
	return f.items, f.err
}

func (f *fakeAdminLifetimeCodeService) SendLifetimeCode(_ context.Context, input membership.LifetimeCodeDeliveryInput) (membership.LifetimeCodeDeliveryResult, error) {
	f.input = input
	return f.result, f.err
}

func TestAdminLifetimeCodeRoutesSendWithoutExposingPlaintext(t *testing.T) {
	actorID := uuid.New()
	requestID := uuid.New()
	now := time.Now().UTC()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{
			ID: actorID, Status: "active", Roles: []string{"membership_admin"},
		},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: now.Add(time.Hour),
	}}
	delivery := membership.LifetimeCodeDelivery{
		ID: requestID, Email: "buyer@example.com", CodeHint: "ABCD",
		DeliveryStatus: "sent", RedemptionStatus: "active", DeliveryAttempts: 1,
		LastDeliveryAttemptAt: now, SentAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	service := &fakeAdminLifetimeCodeService{
		items: []membership.LifetimeCodeDelivery{delivery},
		result: membership.LifetimeCodeDeliveryResult{
			Delivery: delivery, NewDelivery: true,
		},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		LifetimeCodeAdmin: service,
		AuthHTTP:          AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, adminRequest(http.MethodGet, "/api/v1/admin/lifetime-code-deliveries?limit=20", "", "", ""))
	if listResponse.Code != http.StatusOK || service.limit != 20 ||
		!strings.Contains(listResponse.Body.String(), `"codeHint":"ABCD"`) {
		t.Fatalf("list response = %d %s limit=%d", listResponse.Code, listResponse.Body.String(), service.limit)
	}

	missingCSRF := httptest.NewRecorder()
	body := `{"requestId":"` + requestID.String() + `","email":"buyer@example.com"}`
	router.ServeHTTP(missingCSRF, adminRequest(http.MethodPost, "/api/v1/admin/lifetime-code-deliveries", body, csrfToken, ""))
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF response = %d %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	sendResponse := httptest.NewRecorder()
	router.ServeHTTP(sendResponse, adminRequest(http.MethodPost, "/api/v1/admin/lifetime-code-deliveries", body, csrfToken, csrfToken))
	if sendResponse.Code != http.StatusCreated || service.input.RequestID != requestID ||
		service.input.ActorUserID != actorID || service.input.Email != "buyer@example.com" {
		t.Fatalf("send response = %d %s input=%+v", sendResponse.Code, sendResponse.Body.String(), service.input)
	}
	if strings.Contains(sendResponse.Body.String(), "WZM-") {
		t.Fatal("lifetime code response exposed plaintext")
	}
	if sendResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", sendResponse.Header().Get("Cache-Control"))
	}
}

func TestAdminLifetimeCodeRouteMapsDeliveryFailureToRetryableProblem(t *testing.T) {
	now := time.Now().UTC()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{
			ID: uuid.New(), Status: "active", Roles: []string{"membership_admin"},
		},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: now.Add(time.Hour),
	}}
	service := &fakeAdminLifetimeCodeService{err: membership.ErrLifetimeCodeEmailDeliveryFailed}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		LifetimeCodeAdmin: service,
		AuthHTTP:          AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
	body := `{"requestId":"` + uuid.NewString() + `","email":"buyer@example.com"}`
	response := httptest.NewRecorder()
	router.ServeHTTP(response, adminRequest(http.MethodPost, "/api/v1/admin/lifetime-code-deliveries", body, csrfToken, csrfToken))

	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"lifetime_code_email_delivery_failed"`) ||
		!strings.Contains(response.Body.String(), "同一个激活码") {
		t.Fatalf("delivery failure response = %d %s", response.Code, response.Body.String())
	}
}
