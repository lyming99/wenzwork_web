package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
)

type remoteAccessPolicyStub struct {
	settings remoteaccesspolicy.Settings
	input    remoteaccesspolicy.UpdateSettingsInput
	err      error
}

func (stub *remoteAccessPolicyStub) GetSettings(context.Context) (remoteaccesspolicy.Settings, error) {
	return stub.settings, stub.err
}

func (stub *remoteAccessPolicyStub) UpdateSettings(_ context.Context, input remoteaccesspolicy.UpdateSettingsInput) (remoteaccesspolicy.Settings, error) {
	stub.input = input
	return stub.settings, stub.err
}

func TestRemoteAccessPolicyAdminRoutesAreVersionedAndCSRFProtected(t *testing.T) {
	userID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active", Roles: []string{"membership_admin"}},
		CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	stub := &remoteAccessPolicyStub{settings: remoteaccesspolicy.Settings{
		DeviceLimit: 24, Version: 7, UpdatedAt: time.Date(2026, 8, 23, 5, 0, 0, 0, time.UTC),
	}}
	router := NewRouter(Dependencies{
		Auth: authService, RemoteAccessPolicy: stub,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})

	get := httptest.NewRequest(http.MethodGet, "/api/v1/admin/remote-access-policy", nil)
	get.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, get)
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"deviceLimit":24`) {
		t.Fatalf("GET status/body = %d %s", getResponse.Code, getResponse.Body.String())
	}

	missingCSRF := httptest.NewRequest(http.MethodPut, "/api/v1/admin/remote-access-policy", strings.NewReader(`{"deviceLimit":12,"expectedVersion":7}`))
	missingCSRF.Header.Set("Content-Type", "application/json")
	missingCSRF.Header.Set("Origin", "http://localhost:5173")
	missingCSRF.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden || stub.input.ActorUserID != uuid.Nil {
		t.Fatalf("PUT without CSRF status/input = %d %+v", missingResponse.Code, stub.input)
	}

	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/remote-access-policy", strings.NewReader(`{"deviceLimit":12,"expectedVersion":7}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || stub.input.DeviceLimit != 12 || stub.input.ExpectedVersion != 7 || stub.input.ActorUserID != userID {
		t.Fatalf("PUT status/input/body = %d %+v %s", response.Code, stub.input, response.Body.String())
	}
}

func TestRemoteAccessPolicyAdminRouteMapsOptimisticConflict(t *testing.T) {
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: []string{"membership_admin"}},
		CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	stub := &remoteAccessPolicyStub{err: remoteaccesspolicy.ErrSettingsConflict}
	router := NewRouter(Dependencies{
		Auth: authService, RemoteAccessPolicy: stub,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/remote-access-policy", strings.NewReader(`{"deviceLimit":10,"expectedVersion":1}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"remote_access_policy_conflict"`) {
		t.Fatalf("conflict status/body = %d %s", response.Code, response.Body.String())
	}
}
