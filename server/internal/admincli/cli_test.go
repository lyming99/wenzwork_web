package admincli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAuthenticatesWithMFAAndQueriesDocuments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "wenzwork_session", Value: "level-one", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "wenzwork_csrf", Value: "csrf-one", Path: "/"})
			_, _ = w.Write([]byte(`{"mfaRequired":true,"mfaEnforced":true,"assuranceLevel":1}`))
		case "/api/v1/auth/mfa/totp/verify":
			if r.Header.Get("X-CSRF-Token") != "csrf-one" {
				t.Errorf("MFA CSRF header = %q", r.Header.Get("X-CSRF-Token"))
			}
			http.SetCookie(w, &http.Cookie{Name: "wenzwork_session", Value: "level-two", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "wenzwork_csrf", Value: "csrf-two", Path: "/"})
			_, _ = w.Write([]byte(`{"assuranceLevel":2}`))
		case "/api/v1/admin/help-documents":
			cookie, _ := r.Cookie("wenzwork_session")
			if cookie == nil || cookie.Value != "level-two" {
				t.Errorf("admin session cookie = %#v", cookie)
			}
			_, _ = w.Write([]byte(`{"items":[],"total":0,"limit":100,"offset":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	env := map[string]string{
		"WENZWORK_ADMIN_API_URL":  server.URL + "/api/v1",
		"WENZWORK_ADMIN_EMAIL":    "admin@example.test",
		"WENZWORK_ADMIN_PASSWORD": "secret-not-printed",
		"WENZWORK_ADMIN_MFA_CODE": "123456",
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"docs", "list"}, &output, func(key string) string { return env[key] }); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(output.String(), `"total": 0`) || strings.Contains(output.String(), env["WENZWORK_ADMIN_PASSWORD"]) {
		t.Fatalf("Run() output = %q", output.String())
	}
}

func TestRunSkipsMFAChallengeWhenDevelopmentServerDisablesEnforcement(t *testing.T) {
	mfaVerificationCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "wenzwork_session", Value: "development-session", Path: "/", HttpOnly: true})
			http.SetCookie(w, &http.Cookie{Name: "wenzwork_csrf", Value: "development-csrf", Path: "/"})
			_, _ = w.Write([]byte(`{"mfaRequired":true,"mfaEnforced":false,"assuranceLevel":1}`))
		case "/api/v1/auth/mfa/totp/verify":
			mfaVerificationCalled = true
			http.Error(w, "MFA must not be requested", http.StatusInternalServerError)
		case "/api/v1/admin/help-documents":
			_, _ = w.Write([]byte(`{"items":[],"total":0,"limit":100,"offset":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	env := map[string]string{
		"WENZWORK_ADMIN_API_URL":  server.URL + "/api/v1",
		"WENZWORK_ADMIN_EMAIL":    "admin@example.test",
		"WENZWORK_ADMIN_PASSWORD": "development-password",
	}
	if err := Run(context.Background(), []string{"docs", "list"}, &bytes.Buffer{}, func(key string) string { return env[key] }); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if mfaVerificationCalled {
		t.Fatal("development login unexpectedly requested MFA verification")
	}
}

func TestRunCreatesReleaseDraftAndRequiresPublishConfirmation(t *testing.T) {
	var requestStatus string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/admin/releases" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-CSRF-Token") != "csrf-token" {
			t.Errorf("CSRF header = %q", r.Header.Get("X-CSRF-Token"))
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		requestStatus, _ = payload["status"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"release":{"id":"00000000-0000-0000-0000-000000000001"}}`))
	}))
	defer server.Close()
	directory := t.TempDir()
	file := filepath.Join(directory, "release.json")
	if err := os.WriteFile(file, []byte(`{"version":"1.2.3","status":"published","title":"Release","channel":"stable","summary":"","releaseNotes":"","assets":[]}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	env := map[string]string{
		"WENZWORK_ADMIN_API_URL": server.URL + "/api/v1",
		"WENZWORK_ADMIN_SESSION": "session-token",
		"WENZWORK_ADMIN_CSRF":    "csrf-token",
	}
	if err := Run(context.Background(), []string{"releases", "draft", "--file", file}, &bytes.Buffer{}, func(key string) string { return env[key] }); err != nil {
		t.Fatalf("Run(draft) error = %v", err)
	}
	if requestStatus != "draft" {
		t.Fatalf("request status = %q, want draft", requestStatus)
	}
	if err := Run(context.Background(), []string{"releases", "publish", "release-id"}, &bytes.Buffer{}, func(key string) string { return env[key] }); !IsUsageError(err) {
		t.Fatalf("Run(publish without confirm) error = %v", err)
	}
}
