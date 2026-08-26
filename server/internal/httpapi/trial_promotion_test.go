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

type fakeTrialPromotionService struct {
	status membership.TrialPromotionStatus
	result membership.TrialPromotionClaimResult
	err    error
	email  string
	ip     string
}

func (f *fakeTrialPromotionService) Status(context.Context) (membership.TrialPromotionStatus, error) {
	return f.status, f.err
}

func (f *fakeTrialPromotionService) Claim(
	_ context.Context,
	email string,
	ip string,
) (membership.TrialPromotionClaimResult, error) {
	f.email = email
	f.ip = ip
	return f.result, f.err
}

type fakeAdminTrialPromotionService struct {
	overview   membership.TrialPromotionAdminOverview
	claims     membership.TrialPromotionAdminClaimList
	filter     membership.TrialPromotionClaimFilter
	actorID    uuid.UUID
	enabled    bool
	dailyQuota int
}

func (f *fakeAdminTrialPromotionService) AdminOverview(
	context.Context,
) (membership.TrialPromotionAdminOverview, error) {
	return f.overview, nil
}

func (f *fakeAdminTrialPromotionService) ListAdminClaims(
	_ context.Context,
	filter membership.TrialPromotionClaimFilter,
) (membership.TrialPromotionAdminClaimList, error) {
	f.filter = filter
	return f.claims, nil
}

func (f *fakeAdminTrialPromotionService) UpdateAdminSettings(
	_ context.Context,
	actorID uuid.UUID,
	enabled bool,
	dailyQuota int,
) (membership.TrialPromotionAdminOverview, error) {
	f.actorID = actorID
	f.enabled = enabled
	f.dailyQuota = dailyQuota
	f.overview.Enabled = enabled
	f.overview.DailyQuota = dailyQuota
	return f.overview, nil
}

func trialPromotionTestRouter(service TrialPromotionService) http.Handler {
	return NewRouter(Dependencies{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		TrialPromotion: service,
		AuthHTTP:       AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
}

func TestTrialPromotionStatusAndClaimDoNotExposeCode(t *testing.T) {
	status := membership.TrialPromotionStatus{
		Enabled: true, Available: true, DailyLimit: 100, ClaimedToday: 1,
		RemainingToday: 99, GrantDays: 30,
		RefreshesAt: time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC),
	}
	service := &fakeTrialPromotionService{
		status: status,
		result: membership.TrialPromotionClaimResult{
			Promotion: status, DeliveryStatus: "sent", NewClaim: true,
		},
	}
	router := trialPromotionTestRouter(service)

	statusResponse := httptest.NewRecorder()
	router.ServeHTTP(
		statusResponse,
		httptest.NewRequest(http.MethodGet, "/api/v1/promotions/trial-pro", nil),
	)
	if statusResponse.Code != http.StatusOK ||
		!strings.Contains(statusResponse.Body.String(), `"remainingToday":99`) ||
		!strings.Contains(statusResponse.Body.String(), `"grantDays":30`) {
		t.Fatalf("status response = %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/promotions/trial-pro/claims",
		strings.NewReader(`{"email":"trial@example.com"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.RemoteAddr = "192.0.2.20:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated ||
		!strings.Contains(response.Body.String(), `"deliveryStatus":"sent"`) {
		t.Fatalf("claim response = %d %s", response.Code, response.Body.String())
	}
	if service.email != "trial@example.com" || service.ip != "192.0.2.20" {
		t.Fatalf("claim input = email %q ip %q", service.email, service.ip)
	}
	if strings.Contains(response.Body.String(), "WZM-") ||
		strings.Contains(strings.ToLower(response.Body.String()), "codehint") {
		t.Fatal("claim response exposed trial redemption code information")
	}
}

func TestTrialPromotionClaimMapsAvailabilityAndRateLimitErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		statusCode int
		code       string
	}{
		{
			name: "unavailable", err: membership.ErrTrialPromotionUnavailable,
			statusCode: http.StatusConflict, code: "trial_promotion_unavailable",
		},
		{
			name: "rate limited", err: membership.ErrTrialPromotionRateLimit,
			statusCode: http.StatusTooManyRequests, code: "trial_promotion_rate_limited",
		},
		{
			name: "delivery", err: membership.ErrTrialPromotionDelivery,
			statusCode: http.StatusServiceUnavailable,
			code:       "trial_promotion_email_delivery_failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"/api/v1/promotions/trial-pro/claims",
				strings.NewReader(`{"email":"trial@example.com"}`),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://localhost:5173")
			response := httptest.NewRecorder()

			trialPromotionTestRouter(&fakeTrialPromotionService{err: test.err}).
				ServeHTTP(response, request)

			if response.Code != test.statusCode ||
				!strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("claim response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAdminTrialPromotionRoutesRequireMembershipAdminMFAAndCSRF(t *testing.T) {
	actorID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(),
		User: auth.User{
			ID: actorID, Status: "active", Roles: []string{"membership_admin"},
		},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2,
		AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	claimID := uuid.New()
	service := &fakeAdminTrialPromotionService{
		overview: membership.TrialPromotionAdminOverview{
			Enabled: true, DailyQuota: 100, Today: "2026-07-25", TodayLimit: 100,
			ClaimedToday: 6, RemainingToday: 94, Available: true, GrantDays: 30,
			RefreshesAt:     time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC),
			TotalClaimCount: 6, UpdatedAt: time.Now(),
		},
		claims: membership.TrialPromotionAdminClaimList{
			Items: []membership.TrialPromotionAdminClaim{{
				ID: claimID, Email: "trial@example.test", ClaimDate: "2026-07-25",
				CodeHint: "EFGH", DeliveryStatus: "sent", RedemptionStatus: "active",
				DeliveryAttempts: 1, LastDeliveryAttemptAt: time.Now(), CreatedAt: time.Now(),
			}},
			Total: 1, Limit: 50,
		},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		TrialAdmin: service,
		AuthHTTP:   AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	overviewResponse := httptest.NewRecorder()
	router.ServeHTTP(
		overviewResponse,
		adminRequest(http.MethodGet, "/api/v1/admin/trial-promotion", "", "", ""),
	)
	if overviewResponse.Code != http.StatusOK ||
		!strings.Contains(overviewResponse.Body.String(), `"remainingToday":94`) {
		t.Fatalf("overview response = %d %s", overviewResponse.Code, overviewResponse.Body.String())
	}

	claimsResponse := httptest.NewRecorder()
	router.ServeHTTP(
		claimsResponse,
		adminRequest(
			http.MethodGet,
			"/api/v1/admin/trial-promotion/claims?q=trial&deliveryStatus=sent&redemptionStatus=active",
			"",
			"",
			"",
		),
	)
	if claimsResponse.Code != http.StatusOK ||
		!strings.Contains(claimsResponse.Body.String(), `"codeHint":"EFGH"`) ||
		strings.Contains(claimsResponse.Body.String(), "WZM-") {
		t.Fatalf("claims response = %d %s", claimsResponse.Code, claimsResponse.Body.String())
	}
	if service.filter.Query != "trial" || service.filter.DeliveryStatus != "sent" ||
		service.filter.RedemptionStatus != "active" {
		t.Fatalf("claims filter = %+v", service.filter)
	}

	missingCSRF := httptest.NewRecorder()
	router.ServeHTTP(
		missingCSRF,
		adminRequest(
			http.MethodPut,
			"/api/v1/admin/trial-promotion",
			`{"enabled":false,"dailyQuota":120}`,
			csrfToken,
			"",
		),
	)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF response = %d %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	updateResponse := httptest.NewRecorder()
	router.ServeHTTP(
		updateResponse,
		adminRequest(
			http.MethodPut,
			"/api/v1/admin/trial-promotion",
			`{"enabled":false,"dailyQuota":120}`,
			csrfToken,
			csrfToken,
		),
	)
	if updateResponse.Code != http.StatusOK || service.actorID != actorID ||
		service.enabled || service.dailyQuota != 120 {
		t.Fatalf(
			"update response = %d %s actor=%s enabled=%t dailyQuota=%d",
			updateResponse.Code,
			updateResponse.Body.String(),
			service.actorID,
			service.enabled,
			service.dailyQuota,
		)
	}
}
