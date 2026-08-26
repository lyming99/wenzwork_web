package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLiveHealthIncludesRequestIDAndSecurityHeaders(t *testing.T) {
	router := NewRouter(Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health/live", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("X-Request-ID is empty")
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestReadyHealthReturnsProblemWhenDependencyFails(t *testing.T) {
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Readiness: func(context.Context) error {
			return errors.New("database unavailable")
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health/ready", nil)
	response := httptest.NewRecorder()

	router.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "application/problem+json") {
		t.Fatalf("Content-Type = %q, want application/problem+json", contentType)
	}
	if !strings.Contains(response.Body.String(), `"code":"not_ready"`) {
		t.Fatalf("body = %s, want stable error code", response.Body.String())
	}
}
