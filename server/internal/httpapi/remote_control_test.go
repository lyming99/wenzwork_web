package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

type fakeRemoteControlService struct {
	RemoteControlService
	listDevicesFn        func(context.Context, uuid.UUID, remotecontrol.PageRequest) (remotecontrol.DevicePage, error)
	updateDeviceFn       func(context.Context, remotecontrol.DeviceUpdateInput) (remotecontrol.Device, error)
	deleteDeviceFn       func(context.Context, remotecontrol.DeviceDeletionInput) error
	listTasksFn          func(context.Context, uuid.UUID, uuid.UUID, remotecontrol.PageRequest) (remotecontrol.TaskPage, error)
	registerControllerFn func(context.Context, remotecontrol.RegisterControllerInput) (remotecontrol.ControllerIdentity, error)
	issueDeviceLinkFn    func(context.Context, remotecontrol.DeviceLinkInput) (remotecontrol.DeviceLink, error)
	revokeDeviceLinkFn   func(context.Context, remotecontrol.DeviceLinkRevocationInput) error
	pollCommandsFn       func(context.Context, remotecontrol.DevicePrincipal, int) (remotecontrol.CommandPage, error)
	retryTaskFn          func(context.Context, remotecontrol.RetryTaskInput) (remotecontrol.Task, remotecontrol.Operation, error)
}

func (fake *fakeRemoteControlService) ListDevices(ctx context.Context, userID uuid.UUID, page remotecontrol.PageRequest) (remotecontrol.DevicePage, error) {
	return fake.listDevicesFn(ctx, userID, page)
}

func (fake *fakeRemoteControlService) UpdateDevice(ctx context.Context, input remotecontrol.DeviceUpdateInput) (remotecontrol.Device, error) {
	return fake.updateDeviceFn(ctx, input)
}

func (fake *fakeRemoteControlService) DeleteDevice(ctx context.Context, input remotecontrol.DeviceDeletionInput) error {
	return fake.deleteDeviceFn(ctx, input)
}

func (fake *fakeRemoteControlService) RegisterController(ctx context.Context, input remotecontrol.RegisterControllerInput) (remotecontrol.ControllerIdentity, error) {
	return fake.registerControllerFn(ctx, input)
}

func (fake *fakeRemoteControlService) ListTasks(ctx context.Context, userID, deviceID uuid.UUID, page remotecontrol.PageRequest) (remotecontrol.TaskPage, error) {
	return fake.listTasksFn(ctx, userID, deviceID, page)
}

func (fake *fakeRemoteControlService) PollCommands(ctx context.Context, principal remotecontrol.DevicePrincipal, limit int) (remotecontrol.CommandPage, error) {
	return fake.pollCommandsFn(ctx, principal, limit)
}

func (fake *fakeRemoteControlService) RetryTask(ctx context.Context, input remotecontrol.RetryTaskInput) (remotecontrol.Task, remotecontrol.Operation, error) {
	return fake.retryTaskFn(ctx, input)
}

func (fake *fakeRemoteControlService) IssueDeviceLink(ctx context.Context, input remotecontrol.DeviceLinkInput) (remotecontrol.DeviceLink, error) {
	if fake.issueDeviceLinkFn == nil {
		return remotecontrol.DeviceLink{}, remotecontrol.ErrUnavailable
	}
	return fake.issueDeviceLinkFn(ctx, input)
}

func (fake *fakeRemoteControlService) RevokeDeviceLink(ctx context.Context, input remotecontrol.DeviceLinkRevocationInput) error {
	if fake.revokeDeviceLinkFn == nil {
		return remotecontrol.ErrUnavailable
	}
	return fake.revokeDeviceLinkFn(ctx, input)
}

func newRemoteControlTestRouter(browserAuth AuthService, appAuth AppAuthService, remote RemoteControlService) http.Handler {
	return NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: browserAuth, AppAuth: appAuth, RemoteControl: remote,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
}

func TestRemoteControlDeviceListUsesAuthenticatedUserAndKeysetQuery(t *testing.T) {
	userID, deviceID := uuid.New(), uuid.New()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	remote := &fakeRemoteControlService{}
	remote.listDevicesFn = func(_ context.Context, gotUserID uuid.UUID, page remotecontrol.PageRequest) (remotecontrol.DevicePage, error) {
		if gotUserID != userID || page.Cursor != "opaque-cursor" || page.Limit != 25 {
			t.Fatalf("ListDevices(%s, %+v)", gotUserID, page)
		}
		return remotecontrol.DevicePage{Items: []remotecontrol.Device{{ID: deviceID, InstallationDeviceID: deviceID, DeviceName: "desktop"}}, ObservedAt: time.Now().UTC()}, nil
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/remote/devices?cursor=opaque-cursor&limit=25", nil)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), deviceID.String()) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestRemoteControlDeviceListAcceptsDesktopBearerWithRemoteConnectScope(t *testing.T) {
	userID, deviceID := uuid.New(), uuid.New()
	appAuth := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID: uuid.New(), User: auth.User{ID: userID, Status: "active"},
		Scope: "profile.read remote.connect", AccessExpiresAt: time.Now().Add(time.Hour), SessionExpiresAt: time.Now().Add(time.Hour),
	}}
	remote := &fakeRemoteControlService{}
	remote.listDevicesFn = func(_ context.Context, gotUserID uuid.UUID, _ remotecontrol.PageRequest) (remotecontrol.DevicePage, error) {
		if gotUserID != userID {
			t.Fatalf("ListDevices user = %s, want %s", gotUserID, userID)
		}
		return remotecontrol.DevicePage{Items: []remotecontrol.Device{{ID: deviceID, InstallationDeviceID: deviceID, DeviceName: "desktop"}}}, nil
	}
	router := newRemoteControlTestRouter(nil, appAuth, remote)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/remote/devices", nil)
	request.Header.Set("Authorization", "Bearer desktop-access-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), deviceID.String()) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestRemoteDeviceDeletionRequiresCSRFAndUsesAuthenticatedOwner(t *testing.T) {
	userID, deviceID := uuid.New(), uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.deleteDeviceFn = func(_ context.Context, input remotecontrol.DeviceDeletionInput) error {
		called++
		if input.UserID != userID || input.DeviceID != deviceID {
			t.Fatalf("DeleteDevice input = %+v", input)
		}
		return nil
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	path := "/api/v1/remote/devices/" + deviceID.String()

	missingCSRF := httptest.NewRequest(http.MethodDelete, path, nil)
	missingCSRF.Header.Set("Origin", "http://localhost:5173")
	missingCSRF.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden || called != 0 {
		t.Fatalf("missing CSRF response = %d %s called=%d", missingResponse.Code, missingResponse.Body.String(), called)
	}

	valid := httptest.NewRequest(http.MethodDelete, path, nil)
	valid.Header.Set("Origin", "http://localhost:5173")
	valid.Header.Set("X-CSRF-Token", csrfToken)
	valid.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	valid.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	validResponse := httptest.NewRecorder()
	router.ServeHTTP(validResponse, valid)
	if validResponse.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("valid response = %d %s called=%d", validResponse.Code, validResponse.Body.String(), called)
	}
}

func TestRemoteDeviceUpdateRequiresCSRFAndOnlyForwardsDisplayName(t *testing.T) {
	userID, deviceID := uuid.New(), uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.updateDeviceFn = func(_ context.Context, input remotecontrol.DeviceUpdateInput) (remotecontrol.Device, error) {
		called++
		if input.UserID != userID || input.DeviceID != deviceID || input.DeviceName != "设计工作站" {
			t.Fatalf("UpdateDevice input = %+v", input)
		}
		return remotecontrol.Device{ID: deviceID, InstallationDeviceID: deviceID, DeviceName: input.DeviceName}, nil
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	path := "/api/v1/remote/devices/" + deviceID.String()
	body := `{"deviceName":"设计工作站"}`

	missingCSRF := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	missingCSRF.Header.Set("Content-Type", "application/json")
	missingCSRF.Header.Set("Origin", "http://localhost:5173")
	missingCSRF.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden || called != 0 {
		t.Fatalf("missing CSRF response = %d %s called=%d", missingResponse.Code, missingResponse.Body.String(), called)
	}

	valid := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	valid.Header.Set("Content-Type", "application/json")
	valid.Header.Set("Origin", "http://localhost:5173")
	valid.Header.Set("X-CSRF-Token", csrfToken)
	valid.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	valid.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	validResponse := httptest.NewRecorder()
	router.ServeHTTP(validResponse, valid)
	if validResponse.Code != http.StatusOK || called != 1 || !strings.Contains(validResponse.Body.String(), "设计工作站") {
		t.Fatalf("valid response = %d %s called=%d", validResponse.Code, validResponse.Body.String(), called)
	}
}

func TestRemoteDeviceUpdateAcceptsNativeBearerWithoutCSRF(t *testing.T) {
	userID, deviceID := uuid.New(), uuid.New()
	appAuth := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID: uuid.New(), User: auth.User{ID: userID, Status: "active"},
		Scope: "profile.read remote.connect", AccessExpiresAt: time.Now().Add(time.Hour), SessionExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.updateDeviceFn = func(_ context.Context, input remotecontrol.DeviceUpdateInput) (remotecontrol.Device, error) {
		called++
		if input.UserID != userID || input.DeviceID != deviceID || input.DeviceName != "Build workstation" {
			t.Fatalf("UpdateDevice input = %+v", input)
		}
		return remotecontrol.Device{ID: deviceID, InstallationDeviceID: deviceID, DeviceName: input.DeviceName}, nil
	}
	router := newRemoteControlTestRouter(nil, appAuth, remote)
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/remote/devices/"+deviceID.String(), strings.NewReader(`{"deviceName":"Build workstation"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer desktop-access-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || called != 1 {
		t.Fatalf("response = %d %s called=%d", response.Code, response.Body.String(), called)
	}
}

func TestRemoteControllerRegistrationRequiresCSRFAndForwardsOnlyPublicIdentity(t *testing.T) {
	userID, sessionID, controllerID := uuid.New(), uuid.New(), uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: sessionID, User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.registerControllerFn = func(_ context.Context, input remotecontrol.RegisterControllerInput) (remotecontrol.ControllerIdentity, error) {
		called++
		if input.UserID != userID || input.SessionID != sessionID || input.ControllerID != controllerID || input.IdentityPublicKey != "public-key" ||
			input.Proof != "proof" || input.IdempotencyKey != "controller-register-123" || len(input.Scopes) != 1 {
			t.Fatalf("RegisterController input = %+v", input)
		}
		return remotecontrol.ControllerIdentity{ID: controllerID, IdentityAlgorithm: "Ed25519", IdentityPublicKey: input.IdentityPublicKey, Status: "active"}, nil
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	body := `{"controllerId":"` + controllerID.String() + `","identityAlgorithm":"Ed25519","identityPublicKey":"public-key","proof":"proof","scopes":["remote.peer.query"]}`

	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/remote/controllers", strings.NewReader(body))
	missingCSRF.Header.Set("Content-Type", "application/json")
	missingCSRF.Header.Set("Origin", "http://localhost:5173")
	missingCSRF.Header.Set("Idempotency-Key", "controller-register-123")
	missingCSRF.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	missingResponse := httptest.NewRecorder()
	router.ServeHTTP(missingResponse, missingCSRF)
	if missingResponse.Code != http.StatusForbidden || called != 0 {
		t.Fatalf("missing CSRF response = %d %s called=%d", missingResponse.Code, missingResponse.Body.String(), called)
	}

	valid := httptest.NewRequest(http.MethodPost, "/api/v1/remote/controllers", strings.NewReader(body))
	valid.Header.Set("Content-Type", "application/json")
	valid.Header.Set("Origin", "http://localhost:5173")
	valid.Header.Set("Idempotency-Key", "controller-register-123")
	valid.Header.Set("X-CSRF-Token", csrfToken)
	valid.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	valid.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	validResponse := httptest.NewRecorder()
	router.ServeHTTP(validResponse, valid)
	if validResponse.Code != http.StatusCreated || called != 1 {
		t.Fatalf("valid response = %d %s called=%d", validResponse.Code, validResponse.Body.String(), called)
	}
	var decoded map[string]any
	if json.Unmarshal(validResponse.Body.Bytes(), &decoded) != nil || strings.Contains(validResponse.Body.String(), "private") {
		t.Fatalf("controller response = %s", validResponse.Body.String())
	}
}

func TestRemoteControllerRegistrationAcceptsNativeBearerWithoutCSRF(t *testing.T) {
	userID, sessionID, controllerID := uuid.New(), uuid.New(), uuid.New()
	appAuth := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID: sessionID, User: auth.User{ID: userID, Status: "active"},
		Scope: "profile.read remote.connect", AccessExpiresAt: time.Now().Add(time.Hour), SessionExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.registerControllerFn = func(_ context.Context, input remotecontrol.RegisterControllerInput) (remotecontrol.ControllerIdentity, error) {
		called++
		if input.UserID != userID || input.SessionID != uuid.Nil || input.ControllerID != controllerID {
			t.Fatalf("RegisterController input = %+v", input)
		}
		return remotecontrol.ControllerIdentity{ID: controllerID, IdentityAlgorithm: "Ed25519", IdentityPublicKey: input.IdentityPublicKey, Status: "active", Scopes: input.Scopes}, nil
	}
	router := newRemoteControlTestRouter(nil, appAuth, remote)
	body := `{"controllerId":"` + controllerID.String() + `","identityAlgorithm":"Ed25519","identityPublicKey":"public-key","proof":"proof","scopes":["remote.peer.query"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/remote/controllers", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer mobile-access-token")
	request.Header.Set("Idempotency-Key", "mobile-controller-register-123")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || called != 1 {
		t.Fatalf("response = %d %s called=%d", response.Code, response.Body.String(), called)
	}
}

func TestRemoteV2DeviceLinkUsesDeviceScopedRequestAndNoStoreResponse(t *testing.T) {
	userID, sessionID, controllerID, deviceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: sessionID, User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.issueDeviceLinkFn = func(_ context.Context, input remotecontrol.DeviceLinkInput) (remotecontrol.DeviceLink, error) {
		called++
		if input.UserID != userID || input.SessionID != sessionID || input.ControllerID != controllerID || input.TargetDeviceID != deviceID || input.ClientIdentityKeyVersion != 3 || input.IdempotencyKey != "device-link-123" {
			t.Fatalf("IssueDeviceLink input = %+v", input)
		}
		if input.RequestedMaximumLifetimeSec == nil || *input.RequestedMaximumLifetimeSec != 60 {
			t.Fatalf("requested lifetime = %v", input.RequestedMaximumLifetimeSec)
		}
		return remotecontrol.DeviceLink{
			GrantID: uuid.New(), Grant: "opaque-grant", ExpiresAt: time.Now().Add(time.Minute), MaximumLifetimeSeconds: 60,
			RelayURL: "wss://relay.example.test/v2/connect", RelayNodeID: uuid.New(), RelayCellID: uuid.New(), TargetConnectionEpoch: 8,
			DeviceIdentityAlgorithm: "Ed25519", DeviceIdentityPublicKey: "public", DeviceKeyThumbprint: "thumbprint", DeviceIdentityKeyVersion: 2,
		}, nil
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	body := `{"targetDeviceId":"` + deviceID.String() + `","clientIdentityKeyVersion":3,"requestedMaximumLifetimeSeconds":60}`
	request := httptest.NewRequest(http.MethodPost, "/api/v2/remote/controllers/"+controllerID.String()+"/device-links", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.Header.Set("Idempotency-Key", "device-link-123")
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || called != 1 {
		t.Fatalf("response = %d %s called=%d", response.Code, response.Body.String(), called)
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), "opaque-grant") {
		t.Fatalf("v2 response headers/body = %#v %s", response.Header(), response.Body.String())
	}
}

func TestRemoteV2DeviceLinkRevocationUsesNonBearerGrantID(t *testing.T) {
	userID, controllerID, grantID := uuid.New(), uuid.New(), uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.revokeDeviceLinkFn = func(_ context.Context, input remotecontrol.DeviceLinkRevocationInput) error {
		called++
		if input.UserID != userID || input.ControllerID != controllerID || input.GrantID != grantID {
			t.Fatalf("RevokeDeviceLink input = %+v", input)
		}
		return nil
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	request := httptest.NewRequest(http.MethodDelete, "/api/v2/remote/controllers/"+controllerID.String()+"/device-links/"+grantID.String(), nil)
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || called != 1 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response = %d %s called=%d headers=%#v", response.Code, response.Body.String(), called, response.Header())
	}
}

func TestRemoteDeviceCommandPollUsesBearerPrincipal(t *testing.T) {
	userID, deviceID, commandID := uuid.New(), uuid.New(), uuid.New()
	appAuth := &fakeAppAuthService{authenticated: auth.AuthenticatedAppSession{
		SessionID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, DeviceID: deviceID,
		Scope: "profile.read membership.read remote.connect", AccessExpiresAt: time.Now().Add(time.Hour), SessionExpiresAt: time.Now().Add(time.Hour),
	}}
	remote := &fakeRemoteControlService{}
	remote.pollCommandsFn = func(_ context.Context, principal remotecontrol.DevicePrincipal, limit int) (remotecontrol.CommandPage, error) {
		if principal.UserID != userID || principal.DeviceID != deviceID || limit != 7 {
			t.Fatalf("PollCommands(%+v, %d)", principal, limit)
		}
		return remotecontrol.CommandPage{Items: []remotecontrol.Command{{ID: commandID, Kind: "project.sync"}}, PollAfterMs: 1500}, nil
	}
	router := newRemoteControlTestRouter(nil, appAuth, remote)
	request := httptest.NewRequest(http.MethodGet, "/v1/device/remote-control/commands?limit=7", nil)
	request.Header.Set("Authorization", "Bearer app-access-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), commandID.String()) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestRemoteTaskListForwardsAfterRevisionCacheValidator(t *testing.T) {
	userID, deviceID := uuid.New(), uuid.New()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	remote := &fakeRemoteControlService{}
	remote.listTasksFn = func(_ context.Context, gotUserID, gotDeviceID uuid.UUID, page remotecontrol.PageRequest) (remotecontrol.TaskPage, error) {
		if gotUserID != userID || gotDeviceID != deviceID || page.AfterRevision == nil || *page.AfterRevision != 42 || page.Cursor != "" || page.Limit != 30 {
			t.Fatalf("ListTasks(%s, %s, %+v)", gotUserID, gotDeviceID, page)
		}
		return remotecontrol.TaskPage{Items: []remotecontrol.Task{}, HighWatermark: 42}, nil
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/remote/devices/"+deviceID.String()+"/tasks?limit=30&afterRevision=42", nil)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"highWatermark":42`) || !strings.Contains(response.Body.String(), `"resetRequired":false`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestRemoteTaskRetryRequiresPeerAndNeverCallsCloudTaskService(t *testing.T) {
	userID, sessionID, sourceTaskID := uuid.New(), uuid.New(), uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: sessionID, User: auth.User{ID: userID, Status: "active"}, CSRFTokenHash: csrfHash, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	called := 0
	remote := &fakeRemoteControlService{}
	remote.retryTaskFn = func(_ context.Context, input remotecontrol.RetryTaskInput) (remotecontrol.Task, remotecontrol.Operation, error) {
		called++
		return remotecontrol.Task{}, remotecontrol.Operation{}, errors.New("must not be called")
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	path := "/api/v1/remote/tasks/" + sourceTaskID.String() + "/retries"
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"prompt":"must-not-be-decoded"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("Idempotency-Key", "retry-task-123")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || called != 0 || !strings.Contains(response.Body.String(), `"code":"remote_peer_required"`) {
		t.Fatalf("response = %d %s called=%d", response.Code, response.Body.String(), called)
	}
}

func TestRemoteControlErrorsUseStableProblemCodes(t *testing.T) {
	userID := uuid.New()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Status: "active"}, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	remote := &fakeRemoteControlService{}
	remote.listDevicesFn = func(context.Context, uuid.UUID, remotecontrol.PageRequest) (remotecontrol.DevicePage, error) {
		return remotecontrol.DevicePage{}, remotecontrol.ErrInvalidInput
	}
	router := newRemoteControlTestRouter(authService, nil, remote)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/remote/devices", nil)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"remote_control_invalid"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}
