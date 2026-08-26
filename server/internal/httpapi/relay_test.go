package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

type relayServiceStub struct {
	enrollmentCalls int
	token           string
	request         relaymanagement.EnrollmentRequest
	accessKeyCalls  int
	accessKey       string
	accessBinding   relaymanagement.AccessKeyBinding
	topology        []relaymanagement.Region
	createCalls     int
	createInput     relaymanagement.CreateInstallationInput
	installCalls    int
	installInput    relaymanagement.CreateInstallSessionInput
	bootstrap       relaymanagement.BootstrapReleaseArtifact
	releaseInput    relaymanagement.SaveReleaseInput
	managedReleases []relaymanagement.Release
}

func (stub *relayServiceStub) ListTopology(context.Context) ([]relaymanagement.Region, error) {
	return stub.topology, nil
}
func (stub *relayServiceStub) ListInstallations(context.Context, *uuid.UUID) ([]relaymanagement.Installation, error) {
	return nil, nil
}
func (stub *relayServiceStub) GetInstallation(context.Context, uuid.UUID) (relaymanagement.Installation, error) {
	return relaymanagement.Installation{}, nil
}
func (stub *relayServiceStub) CreateInstallation(_ context.Context, input relaymanagement.CreateInstallationInput) (relaymanagement.Installation, error) {
	stub.createCalls++
	stub.createInput = input
	return relaymanagement.Installation{
		ID: uuid.New(), DisplayName: input.DisplayName, Region: input.Region, Group: input.Group,
	}, nil
}
func (stub *relayServiceStub) UpdateInstallation(context.Context, uuid.UUID, relaymanagement.UpdateInstallationInput) (relaymanagement.Installation, error) {
	return relaymanagement.Installation{}, nil
}
func (stub *relayServiceStub) DeleteInstallation(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (stub *relayServiceStub) CreateAccessKey(context.Context, uuid.UUID, uuid.UUID) (relaymanagement.AccessKey, error) {
	return relaymanagement.AccessKey{}, nil
}
func (stub *relayServiceStub) ResolveAccessKey(_ context.Context, key string) (relaymanagement.AccessKeyBinding, error) {
	stub.accessKeyCalls++
	stub.accessKey = key
	return stub.accessBinding, nil
}
func (stub *relayServiceStub) RegisterInstanceWithAccessKey(context.Context, string, relaymanagement.RegisterInstanceInput) (relaymanagement.NodeInstance, error) {
	return relaymanagement.NodeInstance{}, nil
}
func (stub *relayServiceStub) HeartbeatWithAccessKey(context.Context, string, relaymanagement.HeartbeatInput) (relaymanagement.HeartbeatResult, error) {
	return relaymanagement.HeartbeatResult{}, nil
}
func (stub *relayServiceStub) UnregisterInstanceWithAccessKey(context.Context, string, uuid.UUID) error {
	return nil
}
func (stub *relayServiceStub) CreateEnrollmentToken(context.Context, uuid.UUID, uuid.UUID) (relaymanagement.EnrollmentToken, error) {
	return relaymanagement.EnrollmentToken{}, nil
}
func (stub *relayServiceStub) GetBootstrapInstallation(context.Context, uuid.UUID) (relaymanagement.BootstrapInstallation, error) {
	return relaymanagement.BootstrapInstallation{}, nil
}
func (stub *relayServiceStub) Enroll(_ context.Context, token string, request relaymanagement.EnrollmentRequest) (relaymanagement.EnrollmentResult, error) {
	stub.enrollmentCalls++
	stub.token = token
	stub.request = request
	return relaymanagement.EnrollmentResult{InstallationID: uuid.New()}, nil
}
func (stub *relayServiceStub) CreateInstallSession(_ context.Context, input relaymanagement.CreateInstallSessionInput) (relaymanagement.InstallSession, error) {
	stub.installCalls++
	stub.installInput = input
	return relaymanagement.InstallSession{ID: uuid.New(), InstallationID: input.InstallationID, ReleaseID: input.ReleaseID, Mode: input.Mode, Status: "waiting"}, nil
}
func (stub *relayServiceStub) GetBootstrapReleaseArtifact(context.Context, uuid.UUID) (relaymanagement.BootstrapReleaseArtifact, error) {
	return stub.bootstrap, nil
}
func (stub *relayServiceStub) ActivateInstallation(context.Context, uuid.UUID, relaymanagement.ActivateInstallationInput) (relaymanagement.Installation, error) {
	return relaymanagement.Installation{}, nil
}
func (stub *relayServiceStub) RevokeInstallation(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}
func (stub *relayServiceStub) ListReleases(context.Context) ([]relaymanagement.Release, error) {
	return nil, nil
}
func (stub *relayServiceStub) ListManagedReleases(context.Context) ([]relaymanagement.Release, error) {
	return stub.managedReleases, nil
}
func (stub *relayServiceStub) CreateRelease(_ context.Context, input relaymanagement.SaveReleaseInput) (relaymanagement.Release, error) {
	stub.releaseInput = input
	return relaymanagement.Release{ID: uuid.New(), Version: input.Version, Status: "draft"}, nil
}
func (stub *relayServiceStub) UpdateRelease(context.Context, uuid.UUID, relaymanagement.SaveReleaseInput) (relaymanagement.Release, error) {
	return relaymanagement.Release{}, nil
}
func (stub *relayServiceStub) PublishRelease(context.Context, uuid.UUID, uuid.UUID) (relaymanagement.Release, error) {
	return relaymanagement.Release{}, nil
}
func (stub *relayServiceStub) RetireRelease(context.Context, uuid.UUID, uuid.UUID) (relaymanagement.Release, error) {
	return relaymanagement.Release{}, nil
}
func (stub *relayServiceStub) DeleteRelease(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func TestRelayAgentAcceptsAccessKeyOnlyInRelayKeyAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := "relay_" + strings.Repeat("a", 43)
	installationID, cellID := uuid.New(), uuid.New()

	tests := []struct {
		name          string
		target        string
		authorization string
		cookie        string
		wantStatus    int
		wantCalls     int
	}{
		{name: "authorization header", target: "/api/v1/relay/agent/configuration", authorization: "RelayKey " + key, wantStatus: http.StatusOK, wantCalls: 1},
		{name: "query rejected", target: "/api/v1/relay/agent/configuration?access_key=" + key, authorization: "RelayKey " + key, wantStatus: http.StatusUnauthorized},
		{name: "cookie rejected", target: "/api/v1/relay/agent/configuration", authorization: "RelayKey " + key, cookie: "relay_access_key=" + key, wantStatus: http.StatusUnauthorized},
		{name: "bearer rejected", target: "/api/v1/relay/agent/configuration", authorization: "Bearer " + key, wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &relayServiceStub{accessBinding: relaymanagement.AccessKeyBinding{
				InstallationID: installationID, CellID: cellID, Status: "active",
				Configuration: relaymanagement.AgentRuntimeConfiguration{
					ProtocolVersion: 2, PublicEndpoint: "wss://relay.example.test/v2/connect",
					ListenAddress: ":8443", HealthAddress: "127.0.0.1:19090",
					RedisURL: "redis://redis.example.test:6379/0", TicketIssuer: "wenzwork-control",
					TicketPublicKeys:          map[string]string{"connection": strings.Repeat("a", 43)},
					DeviceLinkGrantPublicKeys: map[string]string{"device-link": strings.Repeat("b", 43)},
					ConnectionHardLimit:       10_000, HandshakeConcurrency: 128,
				},
			}}
			router := gin.New()
			registerRelayRoutes(router.Group("/api/v1"), stub, nil, AuthHTTPConfig{}, "https://control.example.test", "https://directory.example.test", "https://downloads.example.test", t.TempDir(), "cn-dev", "standard", "r017")
			request := httptest.NewRequest(http.MethodGet, test.target, nil)
			request.Header.Set("Authorization", test.authorization)
			if test.cookie != "" {
				request.Header.Set("Cookie", test.cookie)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus || stub.accessKeyCalls != test.wantCalls {
				t.Fatalf("status=%d calls=%d, want status=%d calls=%d; body=%s", response.Code, stub.accessKeyCalls, test.wantStatus, test.wantCalls, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
			}
			if test.wantCalls == 1 && stub.accessKey != key {
				t.Fatal("ResolveAccessKey did not receive the header credential")
			}
			if test.wantCalls == 1 && (!strings.Contains(response.Body.String(), `"ticketPublicKeys"`) ||
				!strings.Contains(response.Body.String(), `"deviceLinkGrantPublicKeys"`)) {
				t.Fatalf("configuration response omitted downloaded verification keys: %s", response.Body.String())
			}
		})
	}
}

func TestRelayEnrollmentAcceptsTokenOnlyInEnrollmentAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	token := strings.Repeat("a", 43)

	tests := []struct {
		name          string
		target        string
		authorization string
		cookie        string
		wantStatus    int
		wantCalls     int
	}{
		{name: "authorization header", target: "/api/v1/relay/bootstrap/enrollments", authorization: "Enrollment " + token, wantStatus: http.StatusCreated, wantCalls: 1},
		{name: "query rejected", target: "/api/v1/relay/bootstrap/enrollments?token=" + token, authorization: "Enrollment " + token, wantStatus: http.StatusBadRequest},
		{name: "cookie rejected", target: "/api/v1/relay/bootstrap/enrollments", authorization: "Enrollment " + token, cookie: "relay_token=" + token, wantStatus: http.StatusBadRequest},
		{name: "bearer rejected", target: "/api/v1/relay/bootstrap/enrollments", authorization: "Bearer " + token, wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &relayServiceStub{}
			router := gin.New()
			registerRelayRoutes(router.Group("/api/v1"), stub, nil, AuthHTTPConfig{}, "https://control.example.test", "https://directory.example.test", "https://downloads.example.test", t.TempDir(), "cn-dev", "standard", "r017")
			request := httptest.NewRequest(http.MethodPost, test.target, bytes.NewBufferString(`{"installationId":"example"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", test.authorization)
			if test.cookie != "" {
				request.Header.Set("Cookie", test.cookie)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus || stub.enrollmentCalls != test.wantCalls {
				t.Fatalf("status=%d calls=%d, want status=%d calls=%d; body=%s", response.Code, stub.enrollmentCalls, test.wantStatus, test.wantCalls, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
			}
			if test.wantCalls == 1 && stub.token != token {
				t.Fatalf("Enroll token = %q, want header token", stub.token)
			}
		})
	}
}

func TestRelayAdminCreatesHostWithoutClientCellOrTopologyInitialization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	userID := uuid.New()
	authStub := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(),
		User: auth.User{
			ID: userID, Email: "admin@example.test", DisplayName: "Admin", Status: "active", Roles: []string{"super_admin"},
		},
		CSRFTokenHash: csrfHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	relayStub := &relayServiceStub{}
	router := NewRouter(Dependencies{
		Auth: authStub, Relay: relayStub,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})
	body := `{"releaseId":null,"displayName":"relay-east-01","region":"华东","group":"production","failureDomain":"","operationsNote":"","publicEndpoint":"","listenerPort":18443,"platform":"linux","architecture":"amd64"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/relay/node-installations", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("create Relay host status = %d, body=%s", response.Code, response.Body.String())
	}
	if relayStub.createCalls != 1 {
		t.Fatalf("CreateInstallation calls = %d, want 1", relayStub.createCalls)
	}
	if relayStub.createInput.CellID != uuid.Nil {
		t.Fatalf("client-free Relay creation unexpectedly required Cell %s", relayStub.createInput.CellID)
	}
	if relayStub.createInput.Region != "华东" || relayStub.createInput.Group != "production" || relayStub.createInput.ListenerPort != 18443 || relayStub.createInput.ActorUserID != userID {
		t.Fatalf("Relay create input = %#v", relayStub.createInput)
	}
}

func TestRelayAdminCreatesAccessKeyInstallAndUpgradeCommands(t *testing.T) {
	gin.SetMode(gin.TestMode)
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	userID, installationID, releaseID := uuid.New(), uuid.New(), uuid.New()
	authStub := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Email: "admin@example.test", DisplayName: "Admin", Status: "active", Roles: []string{"super_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	relayStub := &relayServiceStub{bootstrap: relaymanagement.BootstrapReleaseArtifact{
		ReleaseVersion: "1.2.3", Platform: "linux", Architecture: "amd64",
		FileName:  "wenzwork-relay-1.2.3-linux-amd64.tar.gz",
		ObjectKey: "relay/1.2.3/wenzwork-relay-1.2.3-linux-amd64.tar.gz",
	}}
	router := NewRouter(Dependencies{
		Auth: authStub, Relay: relayStub, PublicBaseURL: "https://control.example.test",
		RelayArtifactBaseURL: "https://downloads.example.test", RelayBootstrapAssetsDir: t.TempDir(),
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})
	send := func(action string) *httptest.ResponseRecorder {
		body := `{"releaseId":"` + releaseID.String() + `","mode":"script","action":"` + action + `"}`
		request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/relay/node-installations/"+installationID.String()+"/install-sessions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://localhost:5173")
		request.Header.Set("X-CSRF-Token", csrfToken)
		request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
		request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	installed := send("install")
	if installed.Code != http.StatusCreated || !strings.Contains(installed.Body.String(), "--access-key-stdin") || strings.Contains(installed.Body.String(), "enrollment-token") {
		t.Fatalf("install command status=%d body=%s", installed.Code, installed.Body.String())
	}
	if relayStub.installInput.Action != "install" || relayStub.installInput.ActorUserID != userID {
		t.Fatalf("install session input = %#v", relayStub.installInput)
	}

	upgraded := send("upgrade")
	if upgraded.Code != http.StatusCreated || !strings.Contains(upgraded.Body.String(), "upgrade.sh") || strings.Contains(upgraded.Body.String(), "--access-key-stdin") {
		t.Fatalf("upgrade command status=%d body=%s", upgraded.Code, upgraded.Body.String())
	}
	if relayStub.installInput.Action != "upgrade" {
		t.Fatalf("upgrade session action = %q", relayStub.installInput.Action)
	}
}

func TestRelayInstallSessionFailsClosedWhenBootstrapAssetsAreDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authStub := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Email: "admin@example.test", DisplayName: "Admin", Status: "active", Roles: []string{"super_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	relayStub := &relayServiceStub{}
	router := NewRouter(Dependencies{
		Auth: authStub, Relay: relayStub, PublicBaseURL: "https://control.example.test",
		RelayArtifactBaseURL: "https://downloads.example.test",
		AuthHTTP:             AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})
	installationID, releaseID := uuid.New(), uuid.New()
	body := `{"releaseId":"` + releaseID.String() + `","mode":"script","action":"install"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/relay/node-installations/"+installationID.String()+"/install-sessions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"relay_bootstrap_unavailable"`) {
		t.Fatalf("disabled Relay bootstrap status=%d body=%s", response.Code, response.Body.String())
	}
	if relayStub.installCalls != 0 {
		t.Fatalf("CreateInstallSession calls = %d, want 0", relayStub.installCalls)
	}
	assetResponse := httptest.NewRecorder()
	router.ServeHTTP(assetResponse, httptest.NewRequest(http.MethodGet, "/api/v1/relay/bootstrap/install.sh", nil))
	if assetResponse.Code != http.StatusNotFound {
		t.Fatalf("disabled Relay bootstrap asset status = %d, want 404", assetResponse.Code)
	}
}

func TestRelayInstallSessionCommandsCoverSupportedPlatforms(t *testing.T) {
	installationID := uuid.New()
	for _, target := range []struct {
		platform, architecture string
		installMarkers         []string
		upgradeMarkers         []string
		downloadMarkers        []string
	}{
		{platform: "linux", architecture: "amd64", installMarkers: []string{"install.sh", "--access-key-stdin", "sha256sum -c"}, upgradeMarkers: []string{"upgrade.sh"}, downloadMarkers: []string{"./scripts/install.sh", "--package-file"}},
		{platform: "darwin", architecture: "arm64", installMarkers: []string{"darwin/install.sh", "relayctl-arm64", "shasum -a 256", "--access-key-stdin"}, upgradeMarkers: []string{"darwin/upgrade.sh"}, downloadMarkers: []string{"./scripts/install.sh", "--verifier-url", "relayctl-arm64"}},
		{platform: "windows", architecture: "amd64", installMarkers: []string{"windows/Install.ps1", "relayctl-amd64.exe"}, upgradeMarkers: []string{"windows/Upgrade.ps1"}, downloadMarkers: []string{".\\scripts\\Install.ps1", "-VerifierFile", "relayctl-amd64.exe"}},
	} {
		t.Run(target.platform+"/"+target.architecture, func(t *testing.T) {
			fileName := fmt.Sprintf("wenzwork-relay-1.2.3-%s-%s.tar.gz", target.platform, target.architecture)
			artifact := relaymanagement.BootstrapReleaseArtifact{
				ReleaseVersion: "1.2.3", Platform: target.platform, Architecture: target.architecture,
				FileName: fileName, ObjectKey: "relay/1.2.3/" + fileName,
			}
			installed, err := relayInstallSessionCommand(artifact, installationID, "script", "install", "https://control.example.test", "https://downloads.example.test")
			if err != nil {
				t.Fatalf("install command error = %v", err)
			}
			for _, marker := range target.installMarkers {
				if !strings.Contains(installed, marker) {
					t.Fatalf("install command %q does not contain %q", installed, marker)
				}
			}
			if target.platform == "windows" && strings.Contains(installed, "-AccessKeyStdin") {
				t.Fatalf("interactive Windows install command bypasses the hidden Access Key prompt: %q", installed)
			}
			upgraded, err := relayInstallSessionCommand(artifact, installationID, "script", "upgrade", "https://control.example.test", "https://downloads.example.test")
			if err != nil {
				t.Fatalf("upgrade command error = %v", err)
			}
			for _, marker := range target.upgradeMarkers {
				if !strings.Contains(upgraded, marker) {
					t.Fatalf("upgrade command %q does not contain %q", upgraded, marker)
				}
			}
			if strings.Contains(upgraded, "AccessKey") || strings.Contains(upgraded, "access-key-stdin") {
				t.Fatalf("upgrade command unexpectedly requests an Access Key: %q", upgraded)
			}
			downloaded, err := relayInstallSessionCommand(artifact, installationID, "download", "install", "https://control.example.test", "https://downloads.example.test")
			if err != nil {
				t.Fatalf("download command error = %v", err)
			}
			for _, marker := range target.downloadMarkers {
				if !strings.Contains(downloaded, marker) {
					t.Fatalf("download command %q does not contain %q", downloaded, marker)
				}
			}
			if target.platform == "windows" && strings.Contains(downloaded, "-AccessKeyStdin") {
				t.Fatalf("interactive Windows download command bypasses the hidden Access Key prompt: %q", downloaded)
			}
		})
	}
}

func TestRelayInstallSessionCommandAllowsOperatorSelectedHTTP(t *testing.T) {
	artifact := relaymanagement.BootstrapReleaseArtifact{
		ReleaseVersion: "1.2.3", Platform: "linux", Architecture: "amd64",
		FileName:  "wenzwork-relay-1.2.3-linux-amd64.tar.gz",
		ObjectKey: "relay/1.2.3/wenzwork-relay-1.2.3-linux-amd64.tar.gz",
	}
	command, err := relayInstallSessionCommand(
		artifact,
		uuid.New(),
		"script",
		"install",
		"http://control.example.test:8080",
		"http://downloads.example.test:8080",
	)
	if err != nil {
		t.Fatalf("HTTP install command error = %v", err)
	}
	for _, marker := range []string{
		"--proto '=http,https'",
		"http://control.example.test:8080/api/v1/relay/bootstrap/install.sh",
		"http://downloads.example.test:8080/relay/1.2.3/wenzwork-relay-1.2.3-linux-amd64.tar.gz",
		"--management-url 'http://control.example.test:8080'",
	} {
		if !strings.Contains(command, marker) {
			t.Fatalf("HTTP install command %q does not contain %q", command, marker)
		}
	}
}

func TestRelayAdminCreatesReleaseMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	userID := uuid.New()
	authStub := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: userID, Email: "admin@example.test", DisplayName: "Admin", Status: "active", Roles: []string{"super_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	relayStub := &relayServiceStub{}
	router := NewRouter(Dependencies{
		Auth: authStub, Relay: relayStub,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}, DisableAdminMFA: true},
	})
	body := `{"version":"1.2.3","platform":"linux","architecture":"arm64","protocolMin":1,"protocolMax":2,"buildCommit":"` + strings.Repeat("a", 40) + `","buildTime":"2026-08-08T12:00:00Z","signingKeyId":"release-2026","manifestSha256":"` + strings.Repeat("b", 64) + `","manifestSignature":"` + strings.Repeat("c", 64) + `","artifacts":[{"fileName":"wenzwork-relay.tar.gz","fileSizeBytes":4096,"sha256":"` + strings.Repeat("d", 64) + `","signature":"` + strings.Repeat("e", 64) + `","objectKey":"relay/1.2.3/wenzwork-relay.tar.gz"}]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/relay/releases", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session-token"})
	request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfToken})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("create Relay release status=%d body=%s", response.Code, response.Body.String())
	}
	if relayStub.releaseInput.ActorUserID != userID || relayStub.releaseInput.Architecture != "arm64" || relayStub.releaseInput.Artifacts[0].ObjectKey != "relay/1.2.3/wenzwork-relay.tar.gz" {
		t.Fatalf("release input = %#v", relayStub.releaseInput)
	}
}

func TestPreferredRelayCellIDUsesConfiguredRouteAndFallsBack(t *testing.T) {
	wanted := uuid.New()
	other := uuid.New()
	topology := []relaymanagement.Region{{
		Code: "cn-dev",
		Pools: []relaymanagement.Pool{{
			Code: "standard",
			Cells: []relaymanagement.Cell{{ID: other, Code: "r018"}, {
				ID: wanted, Code: "r017", Status: "draft",
			}},
		}},
	}}
	if got, ok := preferredRelayCellID(topology, "cn-dev", "standard", "r017"); !ok || got != wanted {
		t.Fatalf("preferredRelayCellID() = %s, %v, want %s", got, ok, wanted)
	}
	if got, ok := preferredRelayCellID(topology, "missing", "missing", "missing"); !ok || got != other {
		t.Fatalf("preferredRelayCellID fallback = %s, %v, want %s", got, ok, other)
	}
	topology[0].Pools[0].Cells[0].Status = "disabled"
	topology[0].Pools[0].Cells[1].ID = uuid.Nil
	if _, ok := preferredRelayCellID(topology, "cn-dev", "standard", "r017"); ok {
		t.Fatal("preferredRelayCellID accepted an unavailable Cell")
	}
}

func TestRelayArtifactURLAndShellQuoteRejectInjection(t *testing.T) {
	url, err := relayArtifactURL("https://downloads.example.test/relay", "v1/wenzwork-relay.tar.gz")
	if err != nil || url != "https://downloads.example.test/relay/v1/wenzwork-relay.tar.gz" {
		t.Fatalf("relayArtifactURL() = %q, %v", url, err)
	}
	for _, unsafe := range []string{"../secret", "v1/file name.tar.gz", "v1/file?token=secret", "v1/file'bad"} {
		if _, err := relayArtifactURL("https://downloads.example.test", unsafe); err == nil {
			t.Fatalf("relayArtifactURL accepted unsafe object key %q", unsafe)
		}
	}
	if quoted := shellQuote("it's-safe"); quoted != `'it'"'"'s-safe'` {
		t.Fatalf("shellQuote() = %q", quoted)
	}
	for _, baseURL := range []string{"https://control.example.test", "http://control.example.test:8080"} {
		if command := relayBootstrapCurl(); !strings.Contains(command, "--proto '=http,https'") || !strings.Contains(command, "--proto-redir '=http,https'") {
			t.Fatalf("HTTP(S) bootstrap curl flags for %q = %q", baseURL, command)
		}
	}
}

func TestRelayBootstrapServesUpgradeScriptAndGeneratedChecksum(t *testing.T) {
	directory := t.TempDir()
	contents := []byte("#!/usr/bin/env bash\nset -euo pipefail\n")
	if err := os.WriteFile(directory+"/upgrade.sh", contents, 0o755); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/upgrade.sh", relayBootstrapAsset(directory, "upgrade.sh"))
	router.GET("/upgrade.sh.sha256", relayBootstrapAsset(directory, "upgrade.sh.sha256"))

	for _, target := range []string{"/upgrade.sh", "/upgrade.sh.sha256"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", target, response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s Cache-Control=%q", target, response.Header().Get("Cache-Control"))
		}
	}
}

func TestRelayBootstrapServesPlatformScriptsAndVerifierChecksums(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "windows", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "windows", "Install.ps1"), []byte("Set-StrictMode -Version Latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verifier := bytes.Repeat([]byte{0x5a}, 2<<20)
	if err := os.WriteFile(filepath.Join(directory, "windows", "relayctl-arm64.exe"), verifier, 0o755); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/windows/Install.ps1", relayBootstrapAsset(directory, "windows/Install.ps1"))
	router.GET("/windows/relayctl-arm64.exe", relayBootstrapAsset(directory, "windows/relayctl-arm64.exe"))
	router.GET("/windows/relayctl-arm64.exe.sha256", relayBootstrapAsset(directory, "windows/relayctl-arm64.exe.sha256"))

	for _, target := range []string{"/windows/Install.ps1", "/windows/relayctl-arm64.exe", "/windows/relayctl-arm64.exe.sha256"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}
