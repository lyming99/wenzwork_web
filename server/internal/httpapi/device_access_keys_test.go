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
	"github.com/wenzwork/wenzwork-web/server/internal/deviceaccesskey"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
)

type accessKeyDeviceServiceStub struct {
	remoteDeviceServiceStub
	createInput deviceaccesskey.CreateInput
	created     deviceaccesskey.AccessKey
	listed      []deviceaccesskey.AccessKey
	rotated     deviceaccesskey.AccessKey
	rotatedID   uuid.UUID
	revokedID   uuid.UUID
	deletedID   uuid.UUID
	userID      uuid.UUID
	createErr   error
	deleteErr   error
	rotateInput deviceaccesskey.RotateInput
}

func (stub *accessKeyDeviceServiceStub) CreateAccessKey(_ context.Context, input deviceaccesskey.CreateInput) (deviceaccesskey.AccessKey, error) {
	stub.createInput = input
	return stub.created, stub.createErr
}

func (stub *accessKeyDeviceServiceStub) RotateAccessKey(_ context.Context, input deviceaccesskey.RotateInput) (deviceaccesskey.AccessKey, error) {
	stub.rotateInput = input
	stub.rotatedID, stub.userID = input.KeyID, input.UserID
	return stub.rotated, nil
}

func (stub *accessKeyDeviceServiceStub) RevokeAccessKey(_ context.Context, keyID, userID uuid.UUID) error {
	stub.revokedID, stub.userID = keyID, userID
	return nil
}

func (stub *accessKeyDeviceServiceStub) DeleteAccessKey(_ context.Context, keyID, userID uuid.UUID) error {
	stub.deletedID, stub.userID = keyID, userID
	return stub.deleteErr
}

func (stub *accessKeyDeviceServiceStub) ListAccessKeys(_ context.Context, userID uuid.UUID) ([]deviceaccesskey.AccessKey, error) {
	stub.userID = userID
	return stub.listed, nil
}

func TestDeviceAccessKeyManagementUsesSessionCSRFAndShowsSecretOnlyOnCreate(t *testing.T) {
	userID, keyID := uuid.New(), uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash,
		AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	stub := &accessKeyDeviceServiceStub{
		created: deviceaccesskey.AccessKey{
			ID: keyID, Label: "desktop", Key: "device_secret-returned-once", KeyPrefix: "device_secret", Scopes: []string{"remote.connect"}, Status: "active",
		},
		listed: []deviceaccesskey.AccessKey{{ID: keyID, Label: "desktop", KeyPrefix: "device_secret", Scopes: []string{"remote.connect"}, Status: "active"}},
	}
	router := NewRouter(Dependencies{Auth: authService, RemoteDevice: stub, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}}})

	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/remote/device-access-keys", strings.NewReader(`{"label":"desktop","scopes":["remote.connect"]}`))
	missingCSRF.Header.Set("Content-Type", "application/json")
	missingCSRF.Header.Set("Origin", "http://localhost:5173")
	missingCSRF.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status/body = %d %s", missingResponse.Code, missingResponse.Body.String())
	}

	create := httptest.NewRequest(http.MethodPost, "/api/v1/remote/device-access-keys", strings.NewReader(`{"label":"desktop"}`))
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Origin", "http://localhost:5173")
	create.Header.Set("X-CSRF-Token", csrfToken)
	create.Header.Set("Idempotency-Key", "device-key-create-1")
	create.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	create.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	createResponse := httptest.NewRecorder()
	router.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated || stub.createInput.UserID != userID || stub.createInput.Label != "desktop" || len(stub.createInput.Scopes) != 0 ||
		stub.createInput.IdempotencyKey != "device-key-create-1" ||
		!strings.Contains(createResponse.Body.String(), "device_secret-returned-once") {
		t.Fatalf("create status/input/body = %d %+v %s", createResponse.Code, stub.createInput, createResponse.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/api/v1/remote/device-access-keys", nil)
	list.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK || stub.userID != userID || strings.Contains(listResponse.Body.String(), "returned-once") ||
		listResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list status/body = %d %s", listResponse.Code, listResponse.Body.String())
	}
}

func TestDeviceAccessKeyManagementAcceptsDesktopBearerWithoutCSRF(t *testing.T) {
	userID, keyID, replacementID := uuid.New(), uuid.New(), uuid.New()
	appAuth := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID:        uuid.New(),
		User:             auth.User{ID: userID, Status: "active"},
		Scope:            "profile.read remote.connect",
		AccessExpiresAt:  time.Now().Add(time.Hour),
		SessionExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}}
	stub := &accessKeyDeviceServiceStub{
		created: deviceaccesskey.AccessKey{
			ID: keyID, Label: "desktop", Key: "device_secret-returned-once", KeyPrefix: "device_secret", Scopes: []string{"remote.connect"}, Status: "active",
		},
		listed: []deviceaccesskey.AccessKey{{
			ID: keyID, Label: "desktop", KeyPrefix: "device_secret", Scopes: []string{"remote.connect"}, Status: "active",
		}},
		rotated: deviceaccesskey.AccessKey{
			ID: replacementID, Label: "desktop", Key: "device_rotated", KeyPrefix: "device_rotated", Scopes: []string{"remote.connect"}, Status: "active",
		},
	}
	router := NewRouter(Dependencies{
		AppAuth: appAuth, RemoteDevice: stub, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	list := httptest.NewRequest(http.MethodGet, "/api/v1/remote/device-access-keys", nil)
	list.Header.Set("Authorization", "Bearer desktop-access-token")
	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK || stub.userID != userID || listResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("list status/body/user/cache = %d %s %s %q", listResponse.Code, listResponse.Body.String(), stub.userID, listResponse.Header().Get("Cache-Control"))
	}

	create := httptest.NewRequest(http.MethodPost, "/api/v1/remote/device-access-keys", strings.NewReader(`{"label":"desktop"}`))
	create.Header.Set("Authorization", "Bearer desktop-access-token")
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("Idempotency-Key", "desktop-key-create-1")
	createResponse := httptest.NewRecorder()
	router.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusCreated || stub.createInput.UserID != userID || stub.createInput.IdempotencyKey != "desktop-key-create-1" || !strings.Contains(createResponse.Body.String(), "device_secret-returned-once") {
		t.Fatalf("create status/input/body = %d %+v %s", createResponse.Code, stub.createInput, createResponse.Body.String())
	}

	rotation := httptest.NewRequest(http.MethodPost, "/api/v1/remote/device-access-keys/"+keyID.String()+"/rotation", nil)
	rotation.Header.Set("Authorization", "Bearer desktop-access-token")
	rotation.Header.Set("Idempotency-Key", "desktop-key-rotate-1")
	rotationResponse := httptest.NewRecorder()
	router.ServeHTTP(rotationResponse, rotation)
	if rotationResponse.Code != http.StatusOK || stub.rotatedID != keyID || stub.rotateInput.UserID != userID || stub.rotateInput.IdempotencyKey != "desktop-key-rotate-1" {
		t.Fatalf("rotation status/input = %d %+v", rotationResponse.Code, stub.rotateInput)
	}

	revoke := httptest.NewRequest(http.MethodDelete, "/api/v1/remote/device-access-keys/"+keyID.String(), nil)
	revoke.Header.Set("Authorization", "Bearer desktop-access-token")
	revokeResponse := httptest.NewRecorder()
	router.ServeHTTP(revokeResponse, revoke)
	if revokeResponse.Code != http.StatusNoContent || stub.revokedID != keyID || stub.userID != userID {
		t.Fatalf("revoke status/id/user = %d %s %s", revokeResponse.Code, stub.revokedID, stub.userID)
	}

	deleteKey := httptest.NewRequest(http.MethodDelete, "/api/v1/remote/device-access-keys/"+keyID.String()+"/permanent", nil)
	deleteKey.Header.Set("Authorization", "Bearer desktop-access-token")
	deleteResponse := httptest.NewRecorder()
	router.ServeHTTP(deleteResponse, deleteKey)
	if deleteResponse.Code != http.StatusNoContent || stub.deletedID != keyID || stub.userID != userID {
		t.Fatalf("delete status/id/user = %d %s %s", deleteResponse.Code, stub.deletedID, stub.userID)
	}
}

func TestDeviceAccessKeyManagementBearerRequiresScopeAndRejectsMixedCredentials(t *testing.T) {
	appAuth := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID:        uuid.New(),
		User:             auth.User{ID: uuid.New(), Status: "active"},
		Scope:            "profile.read",
		AccessExpiresAt:  time.Now().Add(time.Hour),
		SessionExpiresAt: time.Now().Add(30 * 24 * time.Hour),
	}}
	router := NewRouter(Dependencies{
		AppAuth: appAuth, RemoteDevice: &accessKeyDeviceServiceStub{}, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	missingScope := httptest.NewRequest(http.MethodGet, "/api/v1/remote/device-access-keys", nil)
	missingScope.Header.Set("Authorization", "Bearer desktop-access-token")
	missingScopeResponse := httptest.NewRecorder()
	router.ServeHTTP(missingScopeResponse, missingScope)
	if missingScopeResponse.Code != http.StatusForbidden || !strings.Contains(missingScopeResponse.Body.String(), `"code":"insufficient_scope"`) {
		t.Fatalf("missing scope response = %d %s", missingScopeResponse.Code, missingScopeResponse.Body.String())
	}

	mixed := httptest.NewRequest(http.MethodGet, "/api/v1/remote/device-access-keys", nil)
	mixed.Header.Set("Authorization", "Bearer desktop-access-token")
	mixed.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "browser-session"})
	mixedResponse := httptest.NewRecorder()
	router.ServeHTTP(mixedResponse, mixed)
	if mixedResponse.Code != http.StatusBadRequest || !strings.Contains(mixedResponse.Body.String(), `"code":"ambiguous_authentication"`) {
		t.Fatalf("mixed credentials response = %d %s", mixedResponse.Code, mixedResponse.Body.String())
	}
}

func TestDeviceAccessKeyRotateAndRevokeAreUserScoped(t *testing.T) {
	userID, keyID := uuid.New(), uuid.New()
	csrfToken, csrfHash, _ := auth.NewOpaqueToken()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	stub := &accessKeyDeviceServiceStub{rotated: deviceaccesskey.AccessKey{ID: uuid.New(), Key: "device_rotated", Status: "active"}}
	router := NewRouter(Dependencies{Auth: authService, RemoteDevice: stub, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}}})
	for method, path := range map[string]string{
		http.MethodPost:   "/api/v1/remote/device-access-keys/" + keyID.String() + "/rotation",
		http.MethodDelete: "/api/v1/remote/device-access-keys/" + keyID.String(),
	} {
		request := httptest.NewRequest(method, path, nil)
		request.Header.Set("Origin", "http://localhost:5173")
		request.Header.Set("X-CSRF-Token", csrfToken)
		if method == http.MethodPost {
			request.Header.Set("Idempotency-Key", "device-key-rotate-1")
		}
		request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
		request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK && response.Code != http.StatusNoContent {
			t.Fatalf("%s status/body = %d %s", method, response.Code, response.Body.String())
		}
	}
	if stub.rotatedID != keyID || stub.revokedID != keyID || stub.userID != userID || stub.rotateInput.IdempotencyKey != "device-key-rotate-1" {
		t.Fatalf("rotation/revocation IDs = %s %s user=%s", stub.rotatedID, stub.revokedID, stub.userID)
	}
}

func TestDeviceAccessKeyPermanentDeletionRequiresCSRFAndMapsActiveConflict(t *testing.T) {
	userID, keyID := uuid.New(), uuid.New()
	csrfToken, csrfHash, _ := auth.NewOpaqueToken()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	stub := &accessKeyDeviceServiceStub{deleteErr: deviceaccesskey.ErrConflict}
	router := NewRouter(Dependencies{Auth: authService, RemoteDevice: stub, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}}})
	path := "/api/v1/remote/device-access-keys/" + keyID.String() + "/permanent"

	withoutCSRF := httptest.NewRequest(http.MethodDelete, path, nil)
	withoutCSRF.Header.Set("Origin", "http://localhost:5173")
	withoutCSRF.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	withoutCSRFResponse := httptest.NewRecorder()
	router.ServeHTTP(withoutCSRFResponse, withoutCSRF)
	if withoutCSRFResponse.Code != http.StatusForbidden || stub.deletedID != uuid.Nil {
		t.Fatalf("delete without CSRF status/id = %d %s", withoutCSRFResponse.Code, stub.deletedID)
	}

	request := httptest.NewRequest(http.MethodDelete, path, nil)
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || stub.deletedID != keyID || stub.userID != userID ||
		!strings.Contains(response.Body.String(), "device_access_key_conflict") {
		t.Fatalf("active key deletion status/id/user/body = %d %s %s %s", response.Code, stub.deletedID, stub.userID, response.Body.String())
	}
}

func TestDeviceAccessKeyMutationsRequireCanonicalIdempotencyKeyAndMapDigestConflict(t *testing.T) {
	userID := uuid.New()
	csrfToken, csrfHash, _ := auth.NewOpaqueToken()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	stub := &accessKeyDeviceServiceStub{created: deviceaccesskey.AccessKey{ID: uuid.New(), Key: "device_secret", Status: "active"}}
	router := NewRouter(Dependencies{Auth: authService, RemoteDevice: stub, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}}})

	request := func(idempotencyKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/remote/device-access-keys", strings.NewReader(`{"label":"desktop","scopes":["remote.connect"]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:5173")
		req.Header.Set("X-CSRF-Token", csrfToken)
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		req.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
		req.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}

	for _, invalid := range []string{"", "short", "contains space"} {
		response := request(invalid)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "idempotency_key_invalid") {
			t.Fatalf("invalid key %q status/body = %d %s", invalid, response.Code, response.Body.String())
		}
	}
	stub.createErr = deviceaccesskey.ErrIdempotencyConflict
	response := request("device-key-conflict-1")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "idempotency_conflict") {
		t.Fatalf("conflict status/body = %d %s", response.Code, response.Body.String())
	}
	stub.createErr = remoteaccesspolicy.ErrMembershipRequired
	response = request("device-key-membership-1")
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "membership_required") {
		t.Fatalf("membership status/body = %d %s", response.Code, response.Body.String())
	}
}
