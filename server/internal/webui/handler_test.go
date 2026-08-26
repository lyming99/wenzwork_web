package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandlerRoutesAPIStaticPagesAndSPAFallback(t *testing.T) {
	root := createWebFixture(t)
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, `{"source":"api"}`)
	})
	handler, err := NewHandler(api, root)
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}

	tests := []struct {
		name        string
		method      string
		path        string
		status      int
		body        string
		cacheHeader string
	}{
		{name: "API route", method: http.MethodGet, path: "/api/v1/health/live", status: http.StatusTeapot, body: `{"source":"api"}`},
		{name: "API root", method: http.MethodGet, path: "/api", status: http.StatusTeapot, body: `{"source":"api"}`},
		{name: "non-GET request", method: http.MethodPost, path: "/account", status: http.StatusTeapot, body: `{"source":"api"}`},
		{name: "root index", method: http.MethodGet, path: "/", status: http.StatusOK, body: "spa-index", cacheHeader: "no-cache"},
		{name: "prerendered page", method: http.MethodGet, path: "/pricing", status: http.StatusOK, body: "pricing-page", cacheHeader: "no-cache"},
		{name: "nested prerendered page", method: http.MethodGet, path: "/help/getting-started", status: http.StatusOK, body: "help-page", cacheHeader: "no-cache"},
		{name: "SPA fallback", method: http.MethodGet, path: "/account/security", status: http.StatusOK, body: "spa-index", cacheHeader: "no-cache"},
		{name: "versioned asset", method: http.MethodGet, path: "/assets/app-abc123.js", status: http.StatusOK, body: "asset-content", cacheHeader: "public, max-age=31536000, immutable"},
		{name: "missing asset", method: http.MethodGet, path: "/assets/missing.js", status: http.StatusNotFound, body: "404 page not found"},
		{name: "missing extensionless asset", method: http.MethodGet, path: "/assets/missing", status: http.StatusNotFound, body: "404 page not found"},
		{name: "hidden build metadata", method: http.MethodGet, path: "/.vite/manifest.json", status: http.StatusNotFound, body: "404 page not found"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if !strings.Contains(response.Body.String(), test.body) {
				t.Fatalf("body = %q, want it to contain %q", response.Body.String(), test.body)
			}
			if test.cacheHeader != "" && response.Header().Get("Cache-Control") != test.cacheHeader {
				t.Fatalf("Cache-Control = %q, want %q", response.Header().Get("Cache-Control"), test.cacheHeader)
			}
			if !isAPIPath(test.path) && test.method == http.MethodGet && response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("web response is missing security headers")
			}
		})
	}
}

func TestNewHandlerRejectsInvalidWebRoot(t *testing.T) {
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if _, err := NewHandler(api, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("NewHandler() error = nil, want missing directory error")
	}

	root := t.TempDir()
	if _, err := NewHandler(api, root); err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("NewHandler() error = %v, want missing index error", err)
	}
}

func createWebFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatalf("create assets directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".vite"), 0o755); err != nil {
		t.Fatalf("create hidden metadata directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "help"), 0o755); err != nil {
		t.Fatalf("create nested page directory: %v", err)
	}
	files := map[string]string{
		"index.html":                "spa-index",
		"pricing.html":              "pricing-page",
		"help/getting-started.html": "help-page",
		"assets/app-abc123.js":      "asset-content",
		".vite/manifest.json":       "hidden-metadata",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}
