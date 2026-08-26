package objectstore

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckS3WritesReadsAndDeletesProbe(t *testing.T) {
	var mu sync.Mutex
	var stored []byte
	var methods []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-access-key/") {
			http.Error(w, "request is not signed with the configured access key", http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/wenzwork-test/.wenzwork-init/probe-") {
			http.NotFound(w, r)
			return
		}

		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			stored = append([]byte(nil), body...)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if stored == nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(stored)
		case http.MethodDelete:
			stored = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	err := CheckS3(t.Context(), S3Config{
		Endpoint:        server.URL + "/",
		Region:          "us-east-1",
		Bucket:          "wenzwork-test",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
	})
	if err != nil {
		t.Fatalf("CheckS3() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(methods, ","), "PUT,GET,DELETE"; got != want {
		t.Fatalf("request methods = %q, want %q", got, want)
	}
	if stored != nil {
		t.Fatal("probe object was not deleted")
	}
}

func TestCheckS3CleansUpProbeAfterContentMismatch(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			_, _ = io.WriteString(w, "corrupted probe")
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	err := CheckS3(t.Context(), S3Config{
		Endpoint:        server.URL,
		Region:          "us-east-1",
		Bucket:          "wenzwork-test",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("CheckS3() error = %v, want content mismatch", err)
	}
	if got, want := strings.Join(methods, ","), "PUT,GET,DELETE"; got != want {
		t.Fatalf("request methods = %q, want cleanup sequence %q", got, want)
	}
}

func TestCheckS3UsesVirtualHostedStyleForAliyunOSS(t *testing.T) {
	var mu sync.Mutex
	var stored []byte
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if !strings.HasPrefix(r.Host, "wenzwork-test.oss-cn-hangzhou.aliyuncs.com:") {
			http.Error(w, "bucket is not in the request hostname", http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/.wenzwork-init/probe-") {
			http.Error(w, "bucket unexpectedly appears in the request path", http.StatusForbidden)
			return
		}

		methods = append(methods, r.Method)
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			stored = append([]byte(nil), body...)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			_, _ = w.Write(stored)
		case http.MethodDelete:
			stored = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	err = checkS3(t.Context(), S3Config{
		Endpoint:        "http://oss-cn-hangzhou.aliyuncs.com:" + serverURL.Port(),
		Region:          "cn-hangzhou",
		Bucket:          "wenzwork-test",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		AddressingStyle: S3AddressingStyleAuto,
	}, httpClient)
	if err != nil {
		t.Fatalf("checkS3() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := strings.Join(methods, ","), "PUT,GET,DELETE"; got != want {
		t.Fatalf("request methods = %q, want %q", got, want)
	}
	if stored != nil {
		t.Fatal("probe object was not deleted")
	}
}

func TestResolveS3AddressingStyle(t *testing.T) {
	base := S3Config{
		Region:          "us-east-1",
		Bucket:          "wenzwork-test",
		AccessKeyID:     "access-key",
		SecretAccessKey: "secret-key",
	}
	tests := []struct {
		name     string
		endpoint string
		style    string
		want     string
	}{
		{name: "Alibaba OSS endpoint", endpoint: "https://oss-cn-hangzhou.aliyuncs.com", want: S3AddressingStyleVirtual},
		{name: "Alibaba S3 compatibility endpoint", endpoint: "https://s3.oss-cn-hangzhou.aliyuncs.com", want: S3AddressingStyleVirtual},
		{name: "AWS endpoint", endpoint: "https://s3.us-east-1.amazonaws.com", want: S3AddressingStyleVirtual},
		{name: "local MinIO", endpoint: "http://localhost:9000", want: S3AddressingStylePath},
		{name: "custom endpoint", endpoint: "https://objects.example.test", want: S3AddressingStylePath},
		{name: "explicit virtual", endpoint: "https://objects.example.test", style: S3AddressingStyleVirtual, want: S3AddressingStyleVirtual},
		{name: "explicit path", endpoint: "https://oss-cn-hangzhou.aliyuncs.com", style: S3AddressingStylePath, want: S3AddressingStylePath},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.Endpoint = test.endpoint
			cfg.AddressingStyle = test.style
			got, err := ResolveS3AddressingStyle(cfg)
			if err != nil {
				t.Fatalf("ResolveS3AddressingStyle() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ResolveS3AddressingStyle() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestValidateS3Config(t *testing.T) {
	valid := S3Config{
		Endpoint:        "https://s3.example.test",
		Region:          "us-east-1",
		Bucket:          "wenzwork-test",
		AccessKeyID:     "access-key",
		SecretAccessKey: "secret-key",
	}
	tests := []struct {
		name   string
		mutate func(*S3Config)
	}{
		{name: "relative endpoint", mutate: func(cfg *S3Config) { cfg.Endpoint = "s3.example.test" }},
		{name: "endpoint credentials", mutate: func(cfg *S3Config) { cfg.Endpoint = "https://user:pass@s3.example.test" }},
		{name: "invalid region", mutate: func(cfg *S3Config) { cfg.Region = "us east 1" }},
		{name: "invalid bucket", mutate: func(cfg *S3Config) { cfg.Bucket = "ab" }},
		{name: "missing access key", mutate: func(cfg *S3Config) { cfg.AccessKeyID = "" }},
		{name: "missing secret key", mutate: func(cfg *S3Config) { cfg.SecretAccessKey = "" }},
		{name: "invalid addressing style", mutate: func(cfg *S3Config) { cfg.AddressingStyle = "bucket" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			if _, err := validateS3Config(cfg); err == nil {
				t.Fatal("validateS3Config() error = nil, want error")
			}
		})
	}
}
