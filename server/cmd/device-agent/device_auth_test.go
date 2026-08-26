package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

func TestDeviceTokenManagerShortTTLRefreshRotationAndEncryptedResume(t *testing.T) {
	var mu sync.Mutex
	bootstrapCalls, refreshCalls := 0, 0
	sessionID := uuid.New()
	currentRefresh := "refresh-1"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/device/access-key-bootstrap":
			mu.Lock()
			bootstrapCalls++
			mu.Unlock()
			if request.Header.Get("Authorization") != "DeviceKey device_"+strings.Repeat("A", 43) {
				writer.WriteHeader(http.StatusUnauthorized)
				_, _ = writer.Write([]byte(`{"code":"device_key_invalid"}`))
				return
			}
			_ = json.NewEncoder(writer).Encode(deviceTokenSet{AccessToken: "access-1", ExpiresIn: 2, RefreshToken: currentRefresh, RefreshExpiresIn: 120, SessionID: sessionID, Scope: "remote.connect remote.task.read"})
		case "/api/v1/oauth/token":
			if err := request.ParseForm(); err != nil || request.Form.Get("grant_type") != auth.RefreshTokenGrantType || request.Form.Get("client_id") != auth.DesktopClientID || request.Form.Get("refresh_token") != currentRefresh {
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			mu.Lock()
			refreshCalls++
			currentRefresh = "refresh-" + string(rune('1'+refreshCalls))
			access := "access-" + string(rune('1'+refreshCalls))
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(deviceTokenSet{AccessToken: access, ExpiresIn: 2, RefreshToken: currentRefresh, RefreshExpiresIn: 120, SessionID: sessionID, Scope: "remote.connect remote.task.read"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manager, store, agent := newAuthTestManager(t, server.URL)
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	gotSession, err := manager.bootstrapOrResume(context.Background(), "device_"+strings.Repeat("A", 43), "test-device")
	if err != nil || gotSession != sessionID {
		t.Fatalf("bootstrap session/error = %s / %v", gotSession, err)
	}
	contents, err := os.ReadFile(agent.path + controlStateFileExtension)
	if err != nil || strings.Contains(string(contents), "refresh-1") {
		t.Fatalf("persisted refresh token is plaintext: %v %s", err, contents)
	}
	now = now.Add(1900 * time.Millisecond)
	authorization, err := manager.authorization(context.Background())
	if err != nil || authorization != "Bearer access-2" {
		t.Fatalf("refreshed authorization = %q / %v", authorization, err)
	}

	reloadedManager, err := newDeviceTokenManager(server.Client(), mustURL(t, server.URL), store)
	if err != nil {
		t.Fatal(err)
	}
	reloadedManager.now = func() time.Time { return now }
	if _, err := reloadedManager.bootstrapOrResume(context.Background(), "not-a-device-key", "test-device"); err != nil {
		t.Fatalf("encrypted refresh resume failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bootstrapCalls != 1 || refreshCalls != 2 {
		t.Fatalf("bootstrap/refresh calls = %d/%d", bootstrapCalls, refreshCalls)
	}
}

func TestDeviceTokenManagerRetriesOneUnauthorizedThenFailsClosed(t *testing.T) {
	var mu sync.Mutex
	refreshCalls, protectedCalls := 0, 0
	sessionID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/oauth/token":
			mu.Lock()
			refreshCalls++
			call := refreshCalls
			mu.Unlock()
			_ = request.ParseForm()
			if call > 1 || request.Form.Get("refresh_token") != "refresh-initial" {
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_ = json.NewEncoder(writer).Encode(deviceTokenSet{AccessToken: "access-refreshed", ExpiresIn: 60, RefreshToken: "refresh-rotated", RefreshExpiresIn: 120, SessionID: sessionID, Scope: "remote.connect"})
		case "/v1/device/remote-control/commands":
			mu.Lock()
			protectedCalls++
			mu.Unlock()
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"code":"app_access_token_invalid"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	manager, _, _ := newAuthTestManager(t, server.URL)
	manager.now = func() time.Time { return time.Now().UTC() }
	if err := manager.acceptInitial(deviceTokenSet{AccessToken: "access-initial", ExpiresIn: 60, RefreshToken: "refresh-initial", RefreshExpiresIn: 120, SessionID: sessionID, Scope: "remote.connect"}); err != nil {
		t.Fatal(err)
	}
	var page map[string]any
	err := manager.doJSON(context.Background(), http.MethodGet, "/v1/device/remote-control/commands", "", nil, &page)
	if !errors.Is(err, errDeviceAuthentication) {
		t.Fatalf("second 401 error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshCalls != 1 || protectedCalls != 2 {
		t.Fatalf("refresh/protected calls = %d/%d", refreshCalls, protectedCalls)
	}
}

func TestDeviceTokenManagerReauthorizesWithConfiguredAccessKeyAfterRefreshRevocation(t *testing.T) {
	var mu sync.Mutex
	bootstrapCalls, refreshCalls := 0, 0
	recoveredSession := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/oauth/token":
			mu.Lock()
			refreshCalls++
			mu.Unlock()
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
		case "/v1/device/access-key-bootstrap":
			mu.Lock()
			bootstrapCalls++
			mu.Unlock()
			if request.Header.Get("Authorization") != "DeviceKey device_"+strings.Repeat("C", 43) {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(writer).Encode(deviceTokenSet{
				AccessToken: "access-recovered", ExpiresIn: 60, RefreshToken: "refresh-recovered", RefreshExpiresIn: 120,
				SessionID: recoveredSession, Scope: "remote.connect",
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	store := newAuthControlStateStore(t)
	manager, err := newDeviceTokenManager(server.Client(), mustURL(t, server.URL), store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manager.now = func() time.Time { return now }
	if err := manager.acceptInitial(deviceTokenSet{
		AccessToken: "access-stale", ExpiresIn: 60, RefreshToken: "refresh-revoked", RefreshExpiresIn: 120,
		SessionID: uuid.New(), Scope: "remote.connect",
	}); err != nil {
		t.Fatal(err)
	}
	gotSession, err := manager.bootstrapOrResume(context.Background(), "device_"+strings.Repeat("C", 43), "test-device")
	if err != nil || gotSession != recoveredSession {
		t.Fatalf("recovered session/error = %s / %v", gotSession, err)
	}
	state, err := store.snapshot()
	if err != nil || state.Auth.SessionID != recoveredSession || state.Auth.RefreshToken != "refresh-recovered" {
		t.Fatalf("persisted recovered authentication = %+v / %v", state.Auth, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshCalls != 1 || bootstrapCalls != 1 {
		t.Fatalf("refresh/bootstrap calls = %d/%d", refreshCalls, bootstrapCalls)
	}
}

func newAuthControlStateStore(t *testing.T) *controlStateStore {
	t.Helper()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	deviceID := uuid.New()
	return &controlStateStore{
		path:     filepath.Join(t.TempDir(), "remote-control.enc"),
		deviceID: deviceID,
		aead:     aead,
		state:    newControlPersistentState(deviceID),
	}
}

func TestParseControlRetryAfterSupportsDeltaAndHTTPDate(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	if got := parseControlRetryAfter("45", now); got != 45*time.Second {
		t.Fatalf("delta Retry-After = %s", got)
	}
	if got := parseControlRetryAfter("Wed, 19 Aug 2026 12:01:00 GMT", now); got != time.Minute {
		t.Fatalf("date Retry-After = %s", got)
	}
	if got := parseControlRetryAfter("7200", now); got != 0 {
		t.Fatalf("unsafe Retry-After = %s", got)
	}
}

func newAuthTestManager(t *testing.T, rawURL string) (*deviceTokenManager, *controlStateStore, *agentState) {
	t.Helper()
	root := t.TempDir()
	agent, err := loadOrCreateAgentState(filepath.Join(root, "agent.json"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadControlState(agent)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newDeviceTokenManager(http.DefaultClient, mustURL(t, rawURL), store)
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, agent
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
