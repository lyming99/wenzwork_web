package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAICompatibleProviderTestsAndCompletesAgainstBoundEndpoint(t *testing.T) {
	var sawCompletion bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(response, `{"data":[{"id":"model-a"}]}`)
		case "/v1/chat/completions":
			sawCompletion = true
			var input struct {
				Model    string          `json:"model"`
				Messages []openAIMessage `json:"messages"`
				Stream   bool            `json:"stream"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input.Model != "model-a" || input.Stream || len(input.Messages) != 3 || input.Messages[2].Content != "next" {
				t.Errorf("completion input = %#v", input)
			}
			_, _ = io.WriteString(response, `{"choices":[{"message":{"role":"assistant","content":"real response"}}]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)

	config := aiConfig{Provider: "openai-compatible", BaseURL: server.URL + "/v1", Model: "model-a", Credential: "test-secret", Enabled: true}
	provider := openAICompatibleProvider{}
	latency, err := provider.Test(context.Background(), config)
	if err != nil || latency < 0 {
		t.Fatalf("Test() latency=%v err=%v", latency, err)
	}
	answer, err := provider.Complete(context.Background(), config, []chatMessage{
		{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"},
	}, "next")
	if err != nil || answer != "real response" || !sawCompletion {
		t.Fatalf("Complete() answer=%q saw=%v err=%v", answer, sawCompletion, err)
	}
}

func TestAIProviderEndpointValidationFailsClosed(t *testing.T) {
	for _, value := range []string{
		"http://example.com/v1",
		"https://user:pass@example.com/v1",
		"https://example.com/v1?token=secret",
		"file:///tmp/provider",
		"https://localhost/%2e%2e/admin",
	} {
		t.Run(strings.ReplaceAll(value, "/", "_"), func(t *testing.T) {
			if _, _, err := parseAIBaseURL(value); err == nil {
				t.Fatalf("parseAIBaseURL(%q) succeeded", value)
			}
		})
	}
	if safeAIProviderIP(net.ParseIP("127.0.0.1")) || safeAIProviderIP(net.ParseIP("10.0.0.1")) {
		t.Fatal("private or loopback address passed the provider dial policy")
	}
}

func TestAIProviderEndpointAllowsExplicitPrivateHTTPAddress(t *testing.T) {
	const value = "http://192.168.10.7:60632/v1"
	parsed, allowConfiguredLocal, err := parseAIBaseURL(value)
	if err != nil || parsed.String() != value || !allowConfiguredLocal {
		t.Fatalf("parseAIBaseURL(%q) = %v, allowConfiguredLocal=%v, error=%v", value, parsed, allowConfiguredLocal, err)
	}
	if !localAIProviderIP(net.ParseIP("192.168.10.7")) || localAIProviderIP(net.ParseIP("169.254.10.7")) {
		t.Fatal("configured local-address classification is incorrect")
	}
	if allowedAIProviderIP(net.ParseIP("8.8.8.8"), true) || allowedAIProviderIP(net.ParseIP("192.168.10.7"), false) {
		t.Fatal("configured local and public dial policies were mixed")
	}
	if _, allowLocal, err := parseAIBaseURL("https://provider.example.test/v1"); err != nil || allowLocal {
		t.Fatalf("public hostname gained local dial permission: allowLocal=%v error=%v", allowLocal, err)
	}
}

func TestAIProviderRejectsOversizedOrMalformedResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, `{"choices":[]}`)
	}))
	t.Cleanup(server.Close)
	config := aiConfig{Provider: "openai-compatible", BaseURL: server.URL, Model: "model-a", Enabled: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (openAICompatibleProvider{}).Complete(ctx, config, nil, "hello"); err == nil {
		t.Fatal("malformed completion response was accepted")
	}
}

func TestOpenAICompatibleProviderStreamsSSEChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(response, request)
			return
		}
		var input struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil || !input.Stream {
			t.Errorf("stream input = %#v, %v", input, err)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n")
		_, _ = io.WriteString(response, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n")
		_, _ = io.WriteString(response, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	config := aiConfig{Provider: "openai-compatible", BaseURL: server.URL + "/v1", Model: "model-a", Enabled: true}
	var chunks []string
	err := (openAICompatibleProvider{}).CompleteStream(context.Background(), config, nil, "next", func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil || strings.Join(chunks, "") != "hello world" {
		t.Fatalf("CompleteStream() chunks=%q err=%v", chunks, err)
	}
}
