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

type fakeMembershipService struct {
	redeemErr membershipError
	batches   []membership.BatchSummary
	created   membership.CreatedBatch
}

type membershipError struct{ err error }

func (f *fakeMembershipService) GetMembership(context.Context, uuid.UUID) (membership.MembershipStatus, error) {
	return membership.MembershipStatus{
		PlanCode: "pro", PlanName: "Pro", StartsAt: time.Now().Add(-time.Hour),
		Lifetime: true, Source: "redemption_code",
	}, nil
}
func (f *fakeMembershipService) ListRedemptions(context.Context, uuid.UUID, int) ([]membership.RedemptionRecord, error) {
	return []membership.RedemptionRecord{}, nil
}
func (f *fakeMembershipService) RedeemFromIP(context.Context, uuid.UUID, string, string) (membership.RedemptionResult, error) {
	return membership.RedemptionResult{CodeHint: "MNPQ", RedeemedAt: time.Now()}, f.redeemErr.err
}
func (f *fakeMembershipService) CreateBatch(context.Context, membership.CreateBatchInput) (membership.CreatedBatch, error) {
	return f.created, nil
}
func (f *fakeMembershipService) ListBatches(context.Context, int) ([]membership.BatchSummary, error) {
	return f.batches, nil
}
func (f *fakeMembershipService) RevokeBatch(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func membershipTestRouter(authService AuthService, membershipService MembershipService) http.Handler {
	return NewRouter(Dependencies{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Auth:       authService,
		Membership: membershipService,
		AuthHTTP: AuthHTTPConfig{
			AllowedOrigins: []string{"http://localhost:5173"},
		},
	})
}

func TestMembershipRedemptionRequiresBoundCSRFAndDoesNotEchoCode(t *testing.T) {
	user := auth.User{ID: uuid.New(), Email: "user@example.test", Status: "active", Roles: []string{"user"}}
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: user, CSRFTokenHash: csrfHash, AssuranceLevel: 1,
		AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	membershipService := &fakeMembershipService{redeemErr: membershipError{err: membership.ErrCodeUnavailable}}
	router := membershipTestRouter(authService, membershipService)
	plaintext := "WZM-2345-6789-ABCD-EFGH-JKMN"

	missing := redemptionRequest(plaintext, csrfToken, "")
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF response = %d %s", missingResponse.Code, missingResponse.Body.String())
	}

	request := redemptionRequest(plaintext, csrfToken, csrfToken)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"redemption_unavailable"`) {
		t.Fatalf("redemption response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), plaintext) {
		t.Fatal("redemption error echoed the plaintext code")
	}
}

func TestMembershipAdminRouteRequiresPermissionAndMFA(t *testing.T) {
	user := auth.User{ID: uuid.New(), Email: "admin@example.test", Status: "active", Roles: []string{"membership_admin"}}
	membershipService := &fakeMembershipService{batches: []membership.BatchSummary{}}

	lowAssurance := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: user, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/redemption-code-batches", nil)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	response := httptest.NewRecorder()
	membershipTestRouter(lowAssurance, membershipService).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"mfa_required"`) {
		t.Fatalf("low assurance admin response = %d %s", response.Code, response.Body.String())
	}

	highAssurance := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: user, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/redemption-code-batches", nil)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	response = httptest.NewRecorder()
	membershipTestRouter(highAssurance, membershipService).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("high assurance admin response = %d %s", response.Code, response.Body.String())
	}
}

func TestMembershipStatusAcceptsScopedAppBearerToken(t *testing.T) {
	user := auth.User{ID: uuid.New(), Email: "desktop@example.test", Status: "active", Roles: []string{"user"}}
	appAuth := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID: uuid.New(), User: user, ClientID: auth.DesktopClientID, DeviceID: uuid.New(),
		Scope: auth.DeviceAuthorizationScope, AccessExpiresAt: time.Now().Add(time.Minute),
		SessionExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AppAuth: appAuth,
		Membership: &fakeMembershipService{},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/me/membership", nil)
	request.Header.Set("Authorization", "Bearer app-access-token")
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"planCode":"pro"`) {
		t.Fatalf("app membership response = %d %s", response.Code, response.Body.String())
	}
}

func redemptionRequest(code, csrfCookie, csrfHeader string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/v1/me/redemptions", strings.NewReader(`{"code":"`+code+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfHeader)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfCookie})
	request.RemoteAddr = "192.0.2.100:45678"
	return request
}
