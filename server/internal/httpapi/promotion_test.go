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

type fakePromotionService struct {
	status membership.BetaPromotionStatus
	result membership.BetaPromotionClaimResult
	qr     membership.BetaPromotionGroupQRCode
	qrErr  error
	err    error
	email  string
	ip     string
}

type fakeAdminPromotionService struct {
	overview      membership.BetaPromotionAdminOverview
	claims        membership.BetaPromotionAdminClaimList
	filter        membership.BetaPromotionClaimFilter
	actorID       uuid.UUID
	remaining     int
	qrContentType string
	qrContent     []byte
	qrRemoved     bool
}

func (f *fakeAdminPromotionService) AdminOverview(context.Context) (membership.BetaPromotionAdminOverview, error) {
	return f.overview, nil
}

func (f *fakeAdminPromotionService) ListAdminClaims(_ context.Context, filter membership.BetaPromotionClaimFilter) (membership.BetaPromotionAdminClaimList, error) {
	f.filter = filter
	return f.claims, nil
}

func (f *fakeAdminPromotionService) UpdateAdminRemaining(_ context.Context, actorID uuid.UUID, remaining int) (membership.BetaPromotionAdminOverview, error) {
	f.actorID = actorID
	f.remaining = remaining
	f.overview.Remaining = remaining
	f.overview.Available = remaining > 0
	return f.overview, nil
}

func (f *fakeAdminPromotionService) UpdateAdminGroupQRCode(_ context.Context, actorID uuid.UUID, contentType string, content []byte) (membership.BetaPromotionAdminOverview, error) {
	f.actorID = actorID
	f.qrContentType = contentType
	f.qrContent = append([]byte(nil), content...)
	f.overview.GroupQRCodeConfigured = true
	return f.overview, nil
}

func (f *fakeAdminPromotionService) RemoveAdminGroupQRCode(_ context.Context, actorID uuid.UUID) (membership.BetaPromotionAdminOverview, error) {
	f.actorID = actorID
	f.qrRemoved = true
	f.overview.GroupQRCodeConfigured = false
	f.overview.GroupQRCodeURL = nil
	f.overview.GroupQRCodeUpdatedAt = nil
	return f.overview, nil
}

func (f *fakePromotionService) Status(context.Context) (membership.BetaPromotionStatus, error) {
	return f.status, f.err
}

func (f *fakePromotionService) GroupQRCode(context.Context) (membership.BetaPromotionGroupQRCode, error) {
	return f.qr, f.qrErr
}

func (f *fakePromotionService) Claim(_ context.Context, email, ip string) (membership.BetaPromotionClaimResult, error) {
	f.email = email
	f.ip = ip
	return f.result, f.err
}

func promotionTestRouter(service PromotionService) http.Handler {
	return NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Promotion: service,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
}

func TestBetaPromotionStatusAndClaimDoNotExposeCode(t *testing.T) {
	promotion := membership.BetaPromotionStatus{Limit: 100, Claimed: 1, Remaining: 99, Available: true}
	groupQRCodeURL := "/api/v1/promotions/beta-pro/group-qr?v=123"
	service := &fakePromotionService{
		status: promotion,
		result: membership.BetaPromotionClaimResult{
			Promotion: promotion, DeliveryStatus: "sent", GroupQRCodeURL: &groupQRCodeURL, NewClaim: true,
		},
	}
	router := promotionTestRouter(service)

	statusResponse := httptest.NewRecorder()
	router.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/api/v1/promotions/beta-pro", nil))
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"remaining":99`) {
		t.Fatalf("status response = %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/promotions/beta-pro/claims", strings.NewReader(`{"email":"member@example.com"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.RemoteAddr = "192.0.2.10:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"deliveryStatus":"sent"`) {
		t.Fatalf("claim response = %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"groupQRCodeUrl":"`+groupQRCodeURL+`"`) {
		t.Fatalf("claim response does not include configured group QR code URL: %s", response.Body.String())
	}
	if service.email != "member@example.com" || service.ip != "192.0.2.10" {
		t.Fatalf("claim input = email %q ip %q", service.email, service.ip)
	}
	if strings.Contains(response.Body.String(), "WZM-") || strings.Contains(strings.ToLower(response.Body.String()), "codeHint") {
		t.Fatal("claim response exposed redemption code information")
	}
}

func TestBetaPromotionGroupQRCodeServesConfiguredImageWithCacheValidation(t *testing.T) {
	updatedAt := time.Date(2026, 7, 24, 8, 0, 0, 0, time.UTC)
	service := &fakePromotionService{qr: membership.BetaPromotionGroupQRCode{
		Content: []byte("png-image"), ContentType: "image/png", UpdatedAt: updatedAt,
	}}
	router := promotionTestRouter(service)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/promotions/beta-pro/group-qr", nil))
	if response.Code != http.StatusOK || response.Body.String() != "png-image" {
		t.Fatalf("group QR response = %d %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "image/png" ||
		response.Header().Get("Cache-Control") != "public, max-age=300" ||
		response.Header().Get("ETag") == "" {
		t.Fatalf("group QR headers = %+v", response.Header())
	}

	cachedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/promotions/beta-pro/group-qr", nil)
	cachedRequest.Header.Set("If-None-Match", response.Header().Get("ETag"))
	cachedResponse := httptest.NewRecorder()
	router.ServeHTTP(cachedResponse, cachedRequest)
	if cachedResponse.Code != http.StatusNotModified {
		t.Fatalf("cached group QR response = %d %s", cachedResponse.Code, cachedResponse.Body.String())
	}
}

func TestBetaPromotionGroupQRCodeReturnsNotFoundWhenUnconfigured(t *testing.T) {
	response := httptest.NewRecorder()
	promotionTestRouter(&fakePromotionService{
		qrErr: membership.ErrBetaPromotionGroupQRCodeNotConfigured,
	}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/promotions/beta-pro/group-qr", nil))

	if response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), `"code":"promotion_group_qr_not_configured"`) {
		t.Fatalf("unconfigured group QR response = %d %s", response.Code, response.Body.String())
	}
}

func TestBetaPromotionClaimRequiresAllowedOrigin(t *testing.T) {
	service := &fakePromotionService{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/promotions/beta-pro/claims", strings.NewReader(`{"email":"member@example.com"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	promotionTestRouter(service).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"origin_rejected"`) {
		t.Fatalf("claim response = %d %s", response.Code, response.Body.String())
	}
}

func TestBetaPromotionClaimMapsQuotaAndRateLimitErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		statusCode int
		code       string
	}{
		{name: "exhausted", err: membership.ErrBetaPromotionExhausted, statusCode: http.StatusConflict, code: "promotion_exhausted"},
		{name: "rate limited", err: membership.ErrBetaPromotionRateLimit, statusCode: http.StatusTooManyRequests, code: "promotion_rate_limited"},
		{name: "delivery", err: membership.ErrBetaPromotionDelivery, statusCode: http.StatusServiceUnavailable, code: "promotion_email_delivery_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/promotions/beta-pro/claims", strings.NewReader(`{"email":"member@example.com"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "http://localhost:5173")
			response := httptest.NewRecorder()

			promotionTestRouter(&fakePromotionService{err: test.err}).ServeHTTP(response, request)

			if response.Code != test.statusCode || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("claim response = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAdminBetaPromotionRoutesRequireMembershipAdminMFAAndCSRF(t *testing.T) {
	actorID := uuid.New()
	claimID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"membership_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	service := &fakeAdminPromotionService{
		overview: membership.BetaPromotionAdminOverview{
			Code: "beta-pro-launch", Status: "active", Limit: 100, Claimed: 20,
			Remaining: 80, Available: true, SentDeliveryCount: 19,
			GroupQRCodeConfigured: false, UpdatedAt: time.Now(),
		},
		claims: membership.BetaPromotionAdminClaimList{
			Items: []membership.BetaPromotionAdminClaim{{
				ID: claimID, Email: "member@example.test", CodeHint: "ABCD",
				DeliveryStatus: "sent", RedemptionStatus: "active", DeliveryAttempts: 1,
				LastDeliveryAttemptAt: time.Now(), CreatedAt: time.Now(),
			}},
			Total: 1, Limit: 50,
		},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		PromotionAdmin: service, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	overviewResponse := httptest.NewRecorder()
	router.ServeHTTP(overviewResponse, adminRequest(http.MethodGet, "/api/v1/admin/beta-promotion", "", "", ""))
	if overviewResponse.Code != http.StatusOK || !strings.Contains(overviewResponse.Body.String(), `"remaining":80`) {
		t.Fatalf("overview response = %d %s", overviewResponse.Code, overviewResponse.Body.String())
	}

	claimsResponse := httptest.NewRecorder()
	router.ServeHTTP(claimsResponse, adminRequest(http.MethodGet, "/api/v1/admin/beta-promotion/claims?q=member&deliveryStatus=sent&redemptionStatus=active", "", "", ""))
	if claimsResponse.Code != http.StatusOK || !strings.Contains(claimsResponse.Body.String(), `"codeHint":"ABCD"`) || strings.Contains(claimsResponse.Body.String(), "WZM-") {
		t.Fatalf("claims response = %d %s", claimsResponse.Code, claimsResponse.Body.String())
	}
	if service.filter.Query != "member" || service.filter.DeliveryStatus != "sent" || service.filter.RedemptionStatus != "active" {
		t.Fatalf("claims filter = %+v", service.filter)
	}

	missingCSRF := httptest.NewRecorder()
	router.ServeHTTP(missingCSRF, adminRequest(http.MethodPut, "/api/v1/admin/beta-promotion", `{"remaining":0}`, csrfToken, ""))
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF response = %d %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	updateResponse := httptest.NewRecorder()
	router.ServeHTTP(updateResponse, adminRequest(http.MethodPut, "/api/v1/admin/beta-promotion", `{"remaining":0}`, csrfToken, csrfToken))
	if updateResponse.Code != http.StatusOK || service.actorID != actorID || service.remaining != 0 {
		t.Fatalf("update response = %d %s actor=%s remaining=%d", updateResponse.Code, updateResponse.Body.String(), service.actorID, service.remaining)
	}

	uploadRequest := adminRequest(
		http.MethodPut,
		"/api/v1/admin/beta-promotion/group-qr",
		"configured-qr-image",
		csrfToken,
		csrfToken,
	)
	uploadRequest.Header.Set("Content-Type", "image/png")
	uploadResponse := httptest.NewRecorder()
	router.ServeHTTP(uploadResponse, uploadRequest)
	if uploadResponse.Code != http.StatusOK ||
		service.qrContentType != "image/png" ||
		string(service.qrContent) != "configured-qr-image" ||
		!strings.Contains(uploadResponse.Body.String(), `"groupQRCodeConfigured":true`) {
		t.Fatalf(
			"upload response = %d %s contentType=%q content=%q",
			uploadResponse.Code,
			uploadResponse.Body.String(),
			service.qrContentType,
			service.qrContent,
		)
	}

	removeResponse := httptest.NewRecorder()
	router.ServeHTTP(
		removeResponse,
		adminRequest(
			http.MethodDelete,
			"/api/v1/admin/beta-promotion/group-qr",
			"",
			csrfToken,
			csrfToken,
		),
	)
	if removeResponse.Code != http.StatusOK || !service.qrRemoved ||
		!strings.Contains(removeResponse.Body.String(), `"groupQRCodeConfigured":false`) {
		t.Fatalf("remove response = %d %s removed=%t", removeResponse.Code, removeResponse.Body.String(), service.qrRemoved)
	}
}

func TestAdminBetaPromotionGroupQRCodeRejectsMissingCSRFAndOversizedImages(t *testing.T) {
	actorID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"membership_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	service := &fakeAdminPromotionService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		PromotionAdmin: service, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	missingCSRF := adminRequest(
		http.MethodPut,
		"/api/v1/admin/beta-promotion/group-qr",
		"image",
		csrfToken,
		"",
	)
	missingCSRF.Header.Set("Content-Type", "image/png")
	missingCSRFResponse := httptest.NewRecorder()
	router.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF response = %d %s", missingCSRFResponse.Code, missingCSRFResponse.Body.String())
	}

	oversized := strings.Repeat("x", membership.BetaPromotionGroupQRCodeMaxBytes+1)
	oversizedRequest := adminRequest(
		http.MethodPut,
		"/api/v1/admin/beta-promotion/group-qr",
		oversized,
		csrfToken,
		csrfToken,
	)
	oversizedRequest.Header.Set("Content-Type", "image/png")
	oversizedResponse := httptest.NewRecorder()
	router.ServeHTTP(oversizedResponse, oversizedRequest)
	if oversizedResponse.Code != http.StatusRequestEntityTooLarge ||
		!strings.Contains(oversizedResponse.Body.String(), `"code":"beta_promotion_group_qr_too_large"`) {
		t.Fatalf("oversized response = %d %s", oversizedResponse.Code, oversizedResponse.Body.String())
	}
}
