package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/deviceaccesskey"
	"github.com/wenzwork/wenzwork-web/server/internal/relayallocation"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
	"github.com/wenzwork/wenzwork-web/server/internal/remotedevice"
)

type remoteDeviceServiceStub struct {
	input           remotedevice.RegisterInput
	result          remotedevice.Registration
	err             error
	bootstrapInput  deviceaccesskey.BootstrapInput
	bootstrapResult deviceaccesskey.BootstrapResult
	bootstrapError  error
}

func (stub *remoteDeviceServiceStub) BootstrapAccessKey(_ context.Context, input deviceaccesskey.BootstrapInput) (deviceaccesskey.BootstrapResult, error) {
	stub.bootstrapInput = input
	return stub.bootstrapResult, stub.bootstrapError
}

func (stub *remoteDeviceServiceStub) Register(_ context.Context, input remotedevice.RegisterInput) (remotedevice.Registration, error) {
	stub.input = input
	return stub.result, stub.err
}

type remoteAllocationServiceStub struct {
	createInput  relayallocation.CreateInput
	refreshInput relayallocation.RefreshInput
	result       relayallocation.Result
	err          error
}

func (stub *remoteAllocationServiceStub) Create(_ context.Context, input relayallocation.CreateInput) (relayallocation.Result, error) {
	stub.createInput = input
	return stub.result, stub.err
}

func (stub *remoteAllocationServiceStub) Refresh(_ context.Context, input relayallocation.RefreshInput) (relayallocation.Result, error) {
	stub.refreshInput = input
	return stub.result, stub.err
}

func remoteDeviceTestRouter(appAuth AppAuthService, devices RemoteDeviceService, allocations RemoteAllocationService) http.Handler {
	return NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AppAuth: appAuth,
		RemoteDevice: devices, RemoteAllocation: allocations,
	})
}

func authenticatedRemoteSession(scope string) auth.AuthenticatedAppSession {
	return auth.AuthenticatedAppSession{
		SessionID: uuid.New(), DeviceID: uuid.New(), Scope: scope,
		User:            auth.User{ID: uuid.New(), Status: "active"},
		AccessExpiresAt: time.Now().Add(time.Minute), SessionExpiresAt: time.Now().Add(time.Hour),
	}
}

func TestRemoteDeviceRoutesRequireBearerScopeAndNeverCache(t *testing.T) {
	session := authenticatedRemoteSession("profile.read membership.read")
	authService := &fakeAppAuthService{authenticated: session}
	router := remoteDeviceTestRouter(authService, &remoteDeviceServiceStub{}, &remoteAllocationServiceStub{})

	for name, testCase := range map[string]struct {
		authorization string
		wantStatus    int
		wantCode      string
	}{
		"missing bearer": {"", http.StatusUnauthorized, "app_token_invalid"},
		"missing scope":  {"Bearer access-token", http.StatusForbidden, "insufficient_scope"},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/device/registrations", strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", testCase.authorization)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+testCase.wantCode+`"`) {
				t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("headers = %#v", response.Header())
			}
		})
	}
}

func TestDeviceAccessKeyBootstrapIssuesShortLivedAppCredentials(t *testing.T) {
	deviceID, userID, sessionID := uuid.New(), uuid.New(), uuid.New()
	key := "device_" + strings.Repeat("A", 43)
	devices := &remoteDeviceServiceStub{bootstrapResult: deviceaccesskey.BootstrapResult{
		UserID: userID, SessionID: sessionID, AccessToken: "short-app-access", AccessExpiresIn: 900,
		RefreshToken: "rotatable-refresh", RefreshExpiresIn: 86400, Scope: "remote.connect remote.peer.query",
	}}
	router := remoteDeviceTestRouter(nil, devices, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/device/access-key-bootstrap", strings.NewReader(
		`{"deviceId":"`+deviceID.String()+`","deviceName":"worker-1"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "DeviceKey "+key)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK || devices.bootstrapInput.Key != key || devices.bootstrapInput.DeviceID != deviceID ||
		devices.bootstrapInput.DeviceName != "worker-1" {
		t.Fatalf("status/input/body = %d %+v %s", response.Code, devices.bootstrapInput, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Vary") != "Authorization" ||
		strings.Contains(response.Body.String(), key) || !strings.Contains(response.Body.String(), "short-app-access") {
		t.Fatalf("headers/body = %#v %s", response.Header(), response.Body.String())
	}
}

func TestDeviceAccessKeyBootstrapRejectsMalformedOrRevokedKeyWithoutDisclosure(t *testing.T) {
	validKey := "device_" + strings.Repeat("B", 43)
	for name, testCase := range map[string]struct {
		authorization string
		serviceError  error
	}{
		"bearer is not accepted":       {authorization: "Bearer " + validKey},
		"extra whitespace is rejected": {authorization: "DeviceKey  " + validKey},
		"revoked key":                  {authorization: "DeviceKey " + validKey, serviceError: deviceaccesskey.ErrUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			devices := &remoteDeviceServiceStub{bootstrapError: testCase.serviceError}
			router := remoteDeviceTestRouter(nil, devices, nil)
			request := httptest.NewRequest(http.MethodPost, "/v1/device/access-key-bootstrap", strings.NewReader(
				`{"deviceId":"`+uuid.NewString()+`","deviceName":"worker"}`,
			))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", testCase.authorization)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"code":"device_key_invalid"`) ||
				strings.Contains(response.Body.String(), validKey) {
				t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDeviceAccessKeyBootstrapExplainsMembershipGate(t *testing.T) {
	key := "device_" + strings.Repeat("C", 43)
	devices := &remoteDeviceServiceStub{bootstrapError: remoteaccesspolicy.ErrMembershipRequired}
	router := remoteDeviceTestRouter(nil, devices, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/device/access-key-bootstrap", strings.NewReader(
		`{"deviceId":"`+uuid.NewString()+`","deviceName":"worker"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "DeviceKey "+key)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"membership_required"`) {
		t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
	}
}

func TestRemoteDeviceRegistrationMapsMembershipAndDeviceLimit(t *testing.T) {
	session := authenticatedRemoteSession(auth.DeviceAuthorizationScope)
	authService := &fakeAppAuthService{authenticated: session}
	for name, testCase := range map[string]struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		"membership": {remoteaccesspolicy.ErrMembershipRequired, http.StatusForbidden, "membership_required"},
		"limit":      {remoteaccesspolicy.ErrDeviceLimitReached, http.StatusConflict, "device_limit_reached"},
	} {
		t.Run(name, func(t *testing.T) {
			devices := &remoteDeviceServiceStub{err: testCase.err}
			router := remoteDeviceTestRouter(authService, devices, nil)
			request := httptest.NewRequest(http.MethodPost, "/v1/device/registrations", strings.NewReader(`{
				"deviceName":"worker","platform":"linux","agentVersion":"test","protocolMin":2,"protocolMax":2,
				"capabilities":["relay.ping"],"identityAlgorithm":"ed25519","identityPublicKey":"key","proof":"proof"
			}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer access-token")
			request.Header.Set("Idempotency-Key", "registration-gated")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+testCase.wantCode+`"`) {
				t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRegisterRemoteDeviceUsesAuthenticatedPrincipal(t *testing.T) {
	session := authenticatedRemoteSession(auth.DeviceAuthorizationScope)
	authService := &fakeAppAuthService{authenticated: session}
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	devices := &remoteDeviceServiceStub{result: remotedevice.Registration{Created: true, Credential: remotedevice.Credential{
		DeviceID: session.DeviceID, UserID: session.User.ID, RegisteredSessionID: session.SessionID,
		DeviceName: "test-device", Platform: "windows", AgentVersion: "0.1.0", ProtocolMin: 1, ProtocolMax: 1,
		Capabilities: []string{"relay.ping"}, PublicKeyThumbprint: strings.Repeat("a", 43), GrantVersion: 1,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}}}
	router := remoteDeviceTestRouter(authService, devices, &remoteAllocationServiceStub{})
	body := `{"deviceName":"test-device","platform":"windows","agentVersion":"0.1.0","protocolMin":1,"protocolMax":1,"capabilities":["relay.ping"],"identityAlgorithm":"ed25519","identityPublicKey":"public-key","proof":"proof"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/device/registrations", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer access-token")
	request.Header.Set("Idempotency-Key", "registration-123")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if devices.input.UserID != session.User.ID || devices.input.SessionID != session.SessionID || devices.input.DeviceID != session.DeviceID || devices.input.IdempotencyKey != "registration-123" {
		t.Fatalf("registration principal/input = %+v", devices.input)
	}
	if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), strings.Repeat("a", 43)) {
		t.Fatalf("response headers/body = %#v %s", response.Header(), response.Body.String())
	}
}

func TestRelayAllocationAndRefreshRoutesMapAuthenticatedInputs(t *testing.T) {
	session := authenticatedRemoteSession(auth.DeviceAuthorizationScope)
	authService := &fakeAppAuthService{authenticated: session}
	assignmentID, cellID := uuid.New(), uuid.New()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	allocations := &remoteAllocationServiceStub{result: relayallocation.Result{
		AssignmentID: assignmentID, AssignmentVersion: 3, Scope: "user",
		Primary:   relayallocation.Endpoint{CellID: cellID, EndpointRevision: 2, URL: "wss://relay.example.test/v2/connect"},
		Fallbacks: []relayallocation.Endpoint{}, ConnectionTicket: "ticket-in-response-only",
		TicketExpiresAt: now.Add(5 * time.Minute), AssignmentLeaseExpiresAt: now.Add(24 * time.Hour),
		RefreshAfter: now.Add(4 * time.Minute), RetryPolicy: relayallocation.RetryPolicy{InitialDelayMS: 1000, MaxDelayMS: 30000},
		DeviceLinkGrantTrust: relayallocation.DeviceLinkGrantTrustBundle{Issuer: "wenzwork-control", Keys: []relayallocation.DeviceLinkGrantVerificationKey{{
			KeyID: "device-link-key-1", Algorithm: "Ed25519", PublicKey: strings.Repeat("A", 43),
		}}},
	}}
	router := remoteDeviceTestRouter(authService, &remoteDeviceServiceStub{}, allocations)

	createBody := `{"remoteDeviceId":"` + session.DeviceID.String() + `","protocolMin":1,"protocolMax":1,"connectionEpoch":9}`
	createRequest := httptest.NewRequest(http.MethodPost, "/v1/device/relay-allocations", strings.NewReader(createBody))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Authorization", "Bearer access-token")
	createRequest.Header.Set("Idempotency-Key", "allocation-123")
	createResponse := httptest.NewRecorder()
	router.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusOK || allocations.createInput.UserID != session.User.ID || allocations.createInput.SessionID != session.SessionID || allocations.createInput.ConnectionEpoch != 9 {
		t.Fatalf("create status/input = %d %+v body=%s", createResponse.Code, allocations.createInput, createResponse.Body.String())
	}

	refreshRequest := httptest.NewRequest(http.MethodPost, "/v1/device/relay-allocations/"+assignmentID.String()+"/refresh", strings.NewReader(`{"reason":"goaway","lastEndpointRevision":2}`))
	refreshRequest.Header.Set("Content-Type", "application/json")
	refreshRequest.Header.Set("Authorization", "Bearer access-token")
	refreshRequest.Header.Set("Idempotency-Key", "refresh-1234")
	refreshResponse := httptest.NewRecorder()
	router.ServeHTTP(refreshResponse, refreshRequest)
	if refreshResponse.Code != http.StatusOK || allocations.refreshInput.AssignmentID != assignmentID || allocations.refreshInput.DeviceID != session.DeviceID || allocations.refreshInput.Reason != "goaway" {
		t.Fatalf("refresh status/input = %d %+v body=%s", refreshResponse.Code, allocations.refreshInput, refreshResponse.Body.String())
	}
	for _, response := range []*httptest.ResponseRecorder{createResponse, refreshResponse} {
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
		}
		var value map[string]any
		err := json.Unmarshal(response.Body.Bytes(), &value)
		trust, _ := value["deviceLinkGrantTrust"].(map[string]any)
		if err != nil || value["connectionTicket"] != "ticket-in-response-only" || trust["issuer"] != "wenzwork-control" {
			t.Fatalf("response body = %s error=%v", response.Body.String(), err)
		}
	}
}

func TestRelayAllocationMapsDeviceMismatchAndUnavailable(t *testing.T) {
	session := authenticatedRemoteSession(auth.DeviceAuthorizationScope)
	authService := &fakeAppAuthService{authenticated: session}
	for name, testCase := range map[string]struct {
		serviceError error
		wantStatus   int
		wantCode     string
	}{
		"device mismatch": {relayallocation.ErrDeviceForbidden, http.StatusForbidden, "remote_device_forbidden"},
		"membership":      {remoteaccesspolicy.ErrMembershipRequired, http.StatusForbidden, "membership_required"},
		"dependency down": {relayallocation.ErrAllocationUnavailable, http.StatusServiceUnavailable, "relay_unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			allocations := &remoteAllocationServiceStub{err: testCase.serviceError}
			router := remoteDeviceTestRouter(authService, &remoteDeviceServiceStub{}, allocations)
			body := `{"remoteDeviceId":"` + uuid.NewString() + `","protocolMin":1,"protocolMax":1,"connectionEpoch":1}`
			request := httptest.NewRequest(http.MethodPost, "/v1/device/relay-allocations", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer access-token")
			request.Header.Set("Idempotency-Key", "allocation-err")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+testCase.wantCode+`"`) {
				t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestV1PeerAndFileTicketRoutesAreRemoved(t *testing.T) {
	router := remoteDeviceTestRouter(&fakeAppAuthService{authenticated: authenticatedRemoteSession(auth.DeviceAuthorizationScope)}, &remoteDeviceServiceStub{}, &remoteAllocationServiceStub{})
	for _, path := range []string{
		"/v1/device/peer-session-tickets",
		"/v1/device/file-transfer-tickets",
		"/api/v1/remote/controllers/" + uuid.NewString() + "/rpc-sessions",
	} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
			t.Fatalf("legacy route %q = %d %s, want 404", path, response.Code, response.Body.String())
		}
	}
}
