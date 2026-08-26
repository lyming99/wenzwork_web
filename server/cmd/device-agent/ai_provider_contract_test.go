package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAIProviderReasoningMappingsMatchLocalMode(t *testing.T) {
	assertJSON := func(label string, got any, want string) {
		t.Helper()
		encoded, err := json.Marshal(got)
		if err != nil || string(encoded) != want {
			t.Fatalf("%s = %s, %v; want %s", label, encoded, err, want)
		}
	}
	assertJSON("OpenAI", aiOpenAIReasoningFields(aiConfig{Provider: "openai", ReasoningEffort: "high"}), `{"reasoning_effort":"high"}`)
	assertJSON("OpenAI ultra", aiOpenAIReasoningFields(aiConfig{Provider: "openai", ReasoningEffort: "ultra"}), `{"reasoning_effort":"ultra"}`)
	assertJSON("DeepSeek disabled", aiOpenAIReasoningFields(aiConfig{Provider: "deepseek", ReasoningEffort: "none"}), `{"thinking":{"type":"disabled"}}`)
	assertJSON("DeepSeek enabled", aiOpenAIReasoningFields(aiConfig{Provider: "deepseek", ReasoningEffort: "medium"}), `{"reasoning_effort":"medium","thinking":{"type":"enabled"}}`)
	assertJSON("Anthropic minimal", aiAnthropicReasoningFields(aiConfig{ReasoningEffort: "minimal"}), `{"output_config":{"effort":"low"}}`)
	assertJSON("Anthropic disabled", aiAnthropicReasoningFields(aiConfig{ReasoningEffort: "none"}), `{"thinking":{"type":"disabled"}}`)
	assertJSON("Gemini 2.5", aiGoogleThinkingConfig(aiConfig{Model: "gemini-2.5-pro", ReasoningEffort: "medium"}), `{"thinkingBudget":8192}`)
	assertJSON("Gemini level", aiGoogleThinkingConfig(aiConfig{Model: "gemini-3-pro", ReasoningEffort: "xhigh"}), `{"thinkingLevel":"high"}`)
	assertJSON("Gemini ultra", aiGoogleThinkingConfig(aiConfig{Model: "gemini-3-pro", ReasoningEffort: "ultra"}), `{"thinkingLevel":"high"}`)
	assertJSON("Ollama gpt-oss", aiOllamaThinkValue(aiConfig{Model: "gpt-oss:20b", ReasoningEffort: "medium"}), `"medium"`)
	assertJSON("Ollama disabled", aiOllamaThinkValue(aiConfig{Model: "qwen", ReasoningEffort: "none"}), `false`)
	if aiOpenAIReasoningFields(aiConfig{ReasoningEffort: "automatic"}) != nil ||
		aiGoogleThinkingConfig(aiConfig{ReasoningEffort: "automatic"}) != nil ||
		aiOllamaThinkValue(aiConfig{ReasoningEffort: "automatic"}) != nil {
		t.Fatal("automatic reasoning must defer to the provider")
	}
}

func TestAIStreamParsersPreserveChunkWhitespace(t *testing.T) {
	var anthropic strings.Builder
	err := consumeSSEText(t.Context(), bytes.NewBufferString("data: {\"delta\":{\"text\":\"hello \"}}\n\ndata: {\"delta\":{\"text\":\" world\"}}\n\ndata: [DONE]\n\n"), func(event map[string]any) string {
		delta, _ := event["delta"].(map[string]any)
		return rawStringValue(delta["text"])
	}, func(chunk string) error {
		anthropic.WriteString(chunk)
		return nil
	})
	if err != nil || anthropic.String() != "hello  world" {
		t.Fatalf("Anthropic stream = %q, %v", anthropic.String(), err)
	}

	var ollama strings.Builder
	err = consumeNDJSONText(t.Context(), bytes.NewBufferString("{\"message\":{\"content\":\"hello \"},\"done\":false}\n{\"message\":{\"content\":\" world\"},\"done\":false}\n{\"done\":true}\n"), func(chunk string) error {
		ollama.WriteString(chunk)
		return nil
	})
	if err != nil || ollama.String() != "hello  world" {
		t.Fatalf("Ollama stream = %q, %v", ollama.String(), err)
	}
}

func TestAIProviderSSERecordsSupportMultilineCRLFAndLargePayloads(t *testing.T) {
	t.Run("multiline-crlf-and-comment", func(t *testing.T) {
		source := ": provider heartbeat\r\n\r\n" +
			"event: message\r\n" +
			"data: {\"choices\":[{\"delta\":\r\n" +
			"data: {\"content\":\"multi-line\"},\"finish_reason\":\"stop\"}]}\r\n\r\n"
		var answer strings.Builder
		err := consumeOpenAIEventStream(t.Context(), strings.NewReader(source), func(event aiProviderStreamEvent) error {
			if event.Kind == "text" {
				answer.WriteString(event.Delta)
			}
			return nil
		})
		if err != nil || answer.String() != "multi-line" {
			t.Fatalf("answer=%q error=%v", answer.String(), err)
		}
	})

	t.Run("record-larger-than-legacy-openai-limit", func(t *testing.T) {
		content := strings.Repeat("x", 70<<10)
		source := "data: {\"choices\":[{\"delta\":{\"content\":\"" + content + "\"},\"finish_reason\":\"stop\"}]}\n\n"
		var answer strings.Builder
		err := consumeOpenAIEventStream(t.Context(), strings.NewReader(source), func(event aiProviderStreamEvent) error {
			if event.Kind == "text" {
				answer.WriteString(event.Delta)
			}
			return nil
		})
		if err != nil || answer.String() != content {
			t.Fatalf("answer bytes=%d error=%v", answer.Len(), err)
		}
	})
}

func TestAIProviderEventParsersExposeReasoningUsageAndCompletion(t *testing.T) {
	tests := []struct {
		name, source, text, reasoning, requestID, finishReason string
		usage                                                  chatUsage
		consume                                                func(context.Context, io.Reader, func(aiProviderStreamEvent) error) error
	}{
		{
			name: "openai", text: "answer", reasoning: "think ", requestID: "req-openai", finishReason: "stop",
			usage: chatUsage{InputTokens: 5, OutputTokens: 2, ReasoningTokens: 1, CachedInputTokens: 3, TotalTokens: 7},
			source: "data: {\"id\":\"req-openai\",\"choices\":[{\"delta\":{\"reasoning_content\":\"think \"}}]}\n\n" +
				"data: {\"id\":\"req-openai\",\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\n" +
				"data: [DONE]\n\n",
			consume: consumeOpenAIEventStream,
		},
		{
			name: "anthropic", text: "reply", reasoning: "thought", requestID: "msg-anthropic", finishReason: "end_turn",
			usage: chatUsage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7},
			source: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-anthropic\",\"usage\":{\"input_tokens\":5}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thought\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"reply\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
			consume: consumeAnthropicProviderEvents,
		},
		{
			name: "google", text: "result", reasoning: "analysis", requestID: "resp-google", finishReason: "STOP",
			usage:  chatUsage{InputTokens: 6, OutputTokens: 3, ReasoningTokens: 2, CachedInputTokens: 1, TotalTokens: 9},
			source: "data: {\"responseId\":\"resp-google\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"analysis\",\"thought\":true},{\"text\":\"result\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":6,\"candidatesTokenCount\":3,\"thoughtsTokenCount\":2,\"cachedContentTokenCount\":1,\"totalTokenCount\":9}}\n\n",
			consume: func(ctx context.Context, source io.Reader, onEvent func(aiProviderStreamEvent) error) error {
				return consumeSSEProviderEvents(ctx, source, googleStreamEvents, onEvent)
			},
		},
		{
			name: "ollama", text: "done", reasoning: "think", finishReason: "stop",
			usage: chatUsage{InputTokens: 7, OutputTokens: 4, TotalTokens: 11},
			source: "{\"message\":{\"thinking\":\"think\",\"content\":\"done\"},\"done\":false}\n" +
				"{\"done\":true,\"done_reason\":\"stop\",\"prompt_eval_count\":7,\"eval_count\":4}\n",
			consume: consumeNDJSONProviderEvents,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var text, reasoning strings.Builder
			var usage chatUsage
			requestID, finishReason := "", ""
			err := test.consume(t.Context(), strings.NewReader(test.source), func(event aiProviderStreamEvent) error {
				switch event.Kind {
				case "text":
					text.WriteString(event.Delta)
				case "reasoning":
					reasoning.WriteString(event.Delta)
				case "usage":
					usage = mergeAIUsage(usage, event.Usage)
				case "completed":
					finishReason = event.FinishReason
				}
				if event.ProviderRequestID != "" {
					requestID = event.ProviderRequestID
				}
				return nil
			})
			if err != nil || text.String() != test.text || reasoning.String() != test.reasoning || usage != test.usage ||
				requestID != test.requestID || finishReason != test.finishReason {
				t.Fatalf("events text=%q reasoning=%q usage=%+v request=%q finish=%q error=%v", text.String(), reasoning.String(), usage, requestID, finishReason, err)
			}
		})
	}
}

func TestAIProviderStreamsRejectPrematureEOF(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		consume func(context.Context, io.Reader, func(aiProviderStreamEvent) error) error
	}{
		{
			name:    "openai-after-text",
			source:  "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
			consume: consumeOpenAIEventStream,
		},
		{
			name: "anthropic-before-message-stop",
			source: "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n",
			consume: consumeAnthropicProviderEvents,
		},
		{
			name:   "google-without-finish-reason",
			source: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n",
			consume: func(ctx context.Context, source io.Reader, onEvent func(aiProviderStreamEvent) error) error {
				return consumeSSEProviderEvents(ctx, source, googleStreamEvents, onEvent)
			},
		},
		{
			name:    "ollama-before-done",
			source:  "{\"message\":{\"content\":\"partial\"},\"done\":false}\n",
			consume: consumeNDJSONProviderEvents,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var partial strings.Builder
			err := test.consume(t.Context(), strings.NewReader(test.source), func(event aiProviderStreamEvent) error {
				if event.Kind == "text" {
					partial.WriteString(event.Delta)
				}
				return nil
			})
			if !errors.Is(err, errAIProviderStreamTruncated) || partial.String() != "partial" {
				t.Fatalf("partial=%q error=%v", partial.String(), err)
			}
		})
	}
}

func TestAIProviderStreamsRejectExplicitErrorEventsAfterOutput(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		consume func(context.Context, io.Reader, func(aiProviderStreamEvent) error) error
	}{
		{
			name: "openai",
			source: "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n" +
				"data: {\"error\":{\"message\":\"provider rejected the stream\"}}\n\n",
			consume: consumeOpenAIEventStream,
		},
		{
			name: "anthropic",
			source: "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
				"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n",
			consume: consumeAnthropicProviderEvents,
		},
		{
			name: "google",
			source: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n" +
				"data: {\"error\":{\"code\":503}}\n\n",
			consume: func(ctx context.Context, source io.Reader, onEvent func(aiProviderStreamEvent) error) error {
				return consumeSSEProviderEvents(ctx, source, googleStreamEvents, onEvent)
			},
		},
		{
			name: "ollama",
			source: "{\"message\":{\"content\":\"partial\"},\"done\":false}\n" +
				"{\"error\":\"model runner stopped\"}\n",
			consume: consumeNDJSONProviderEvents,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var partial strings.Builder
			err := test.consume(t.Context(), strings.NewReader(test.source), func(event aiProviderStreamEvent) error {
				if event.Kind == "text" {
					partial.WriteString(event.Delta)
				}
				return nil
			})
			if err == nil || errors.Is(err, errAIProviderStreamTruncated) || partial.String() != "partial" {
				t.Fatalf("partial=%q error=%v", partial.String(), err)
			}
		})
	}
}

func TestAIProviderRouterRetriesTruncationOnlyBeforeOutput(t *testing.T) {
	t.Run("before-output", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "text/event-stream")
			if requests.Add(1) == 1 {
				_, _ = io.WriteString(response, "data: {\"choices\":[]}\n\n")
				return
			}
			_, _ = io.WriteString(response, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		t.Cleanup(server.Close)
		config := defaultAIConfigSettings("truncated-before-output")
		config.Name, config.Provider, config.BaseURL, config.Model = "Truncated", "openai-compatible", server.URL, "model"
		config.Enabled, config.Revision, config.MaxRetries, config.RetryBaseDelayMilliseconds = true, 1, 1, 1
		var answer strings.Builder
		err := defaultAIProviderRouter.CompleteStream(t.Context(), config, nil, "hello", func(chunk string) error {
			answer.WriteString(chunk)
			return nil
		})
		if err != nil || answer.String() != "recovered" || requests.Load() != 2 {
			t.Fatalf("answer=%q requests=%d error=%v", answer.String(), requests.Load(), err)
		}
	})

	t.Run("after-output", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(response, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		}))
		t.Cleanup(server.Close)
		config := defaultAIConfigSettings("truncated-after-output")
		config.Name, config.Provider, config.BaseURL, config.Model = "Truncated", "openai-compatible", server.URL, "model"
		config.Enabled, config.Revision, config.MaxRetries, config.RetryBaseDelayMilliseconds = true, 1, 2, 1
		var answer strings.Builder
		err := defaultAIProviderRouter.CompleteStream(t.Context(), config, nil, "hello", func(chunk string) error {
			answer.WriteString(chunk)
			return nil
		})
		if !errors.Is(err, errAIProviderStreamTruncated) || answer.String() != "partial" || requests.Load() != 1 {
			t.Fatalf("answer=%q requests=%d error=%v", answer.String(), requests.Load(), err)
		}
	})
}

func TestAIProviderStreamingRequestTimeoutIsHonored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.WriteHeader(http.StatusOK)
		response.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	config := defaultAIConfigSettings("stream-timeout")
	config.Name, config.Provider, config.BaseURL, config.Model = "Timeout", "openai-compatible", server.URL, "model"
	config.Enabled, config.Revision, config.RequestTimeoutSeconds, config.MaxRetries = true, 1, 1, 0
	started := time.Now()
	err := defaultAIProviderRouter.CompleteStream(t.Context(), config, nil, "hello", func(string) error { return nil })
	duration := time.Since(started)
	if !errors.Is(err, errAIProviderRequestTimeout) || duration < 800*time.Millisecond || duration > 1500*time.Millisecond {
		t.Fatalf("duration=%v error=%v", duration, err)
	}
}

func TestAIProviderMultimodalPayloadContracts(t *testing.T) {
	prompt := aiProviderPrompt{Text: "inspect", Images: []aiPromptImage{{Name: "pixel.png", MimeType: "image/png", Base64Data: "AQID"}}}
	assertJSON := func(name string, value any, want string) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != want {
			t.Fatalf("%s payload=%s error=%v want=%s", name, encoded, err, want)
		}
	}
	assertJSON("openai", openAIMessagesForPrompt(aiConfig{}, nil, prompt),
		`[{"content":[{"text":"inspect","type":"text"},{"image_url":{"url":"data:image/png;base64,AQID"},"type":"image_url"}],"role":"user"}]`)
	assertJSON("anthropic", anthropicMessagesForPrompt(nil, prompt),
		`[{"content":[{"source":{"data":"AQID","media_type":"image/png","type":"base64"},"type":"image"},{"text":"inspect","type":"text"}],"role":"user"}]`)
	assertJSON("google", googleMessagesForPrompt(nil, prompt),
		`[{"parts":[{"text":"inspect"},{"inlineData":{"data":"AQID","mimeType":"image/png"}}],"role":"user"}]`)
	assertJSON("ollama", ollamaMessagesForPrompt(aiConfig{}, nil, prompt),
		`[{"content":"inspect","images":["AQID"],"role":"user"}]`)
}

func TestAIProviderNativeToolPayloadContracts(t *testing.T) {
	definition := aiWorkspaceToolDefinitions("readOnly")[2]
	call := aiProviderToolCall{ID: "call-read-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)}
	result := aiProviderToolResult{ToolCallID: call.ID, Name: call.Name, Content: `{"content_hash":"abc"}`}
	prompt := aiProviderPrompt{Text: "inspect", Tools: []aiWorkspaceToolDefinition{definition}, ToolExchanges: []aiProviderToolExchange{{
		AssistantText: "I will inspect it.", Calls: []aiProviderToolCall{call}, Results: []aiProviderToolResult{result},
	}}}

	openAIMessages, err := json.Marshal(openAIMessagesForPrompt(aiConfig{}, nil, prompt))
	if err != nil || !bytes.Contains(openAIMessages, []byte(`"tool_calls":[{"function":{"arguments":"{\"path\":\"README.md\"}","name":"read_file"}`)) ||
		!bytes.Contains(openAIMessages, []byte(`"role":"tool","tool_call_id":"call-read-1"`)) {
		t.Fatalf("OpenAI tool messages = %s, %v", openAIMessages, err)
	}
	openAITools, _ := json.Marshal(openAIToolDefinitions(prompt.Tools))
	if !bytes.Contains(openAITools, []byte(`"name":"read_file"`)) || !bytes.Contains(openAITools, []byte(`"parameters"`)) {
		t.Fatalf("OpenAI tools = %s", openAITools)
	}

	anthropicMessages, _ := json.Marshal(anthropicMessagesForPrompt(nil, prompt))
	if !bytes.Contains(anthropicMessages, []byte(`"type":"tool_use"`)) || !bytes.Contains(anthropicMessages, []byte(`"type":"tool_result"`)) ||
		!bytes.Contains(anthropicMessages, []byte(`"tool_use_id":"call-read-1"`)) {
		t.Fatalf("Anthropic tool messages = %s", anthropicMessages)
	}
	anthropicTools, _ := json.Marshal(anthropicToolDefinitions(prompt.Tools))
	if !bytes.Contains(anthropicTools, []byte(`"input_schema"`)) {
		t.Fatalf("Anthropic tools = %s", anthropicTools)
	}

	googleMessages, _ := json.Marshal(googleMessagesForPrompt(nil, prompt))
	if !bytes.Contains(googleMessages, []byte(`"functionCall"`)) || !bytes.Contains(googleMessages, []byte(`"functionResponse"`)) ||
		!bytes.Contains(googleMessages, []byte(`"id":"call-read-1"`)) {
		t.Fatalf("Google tool messages = %s", googleMessages)
	}
	googleTools, _ := json.Marshal(googleToolDefinitions(prompt.Tools))
	if !bytes.Contains(googleTools, []byte(`"parameters"`)) {
		t.Fatalf("Google tools = %s", googleTools)
	}

	ollamaMessages, _ := json.Marshal(ollamaMessagesForPrompt(aiConfig{}, nil, prompt))
	if !bytes.Contains(ollamaMessages, []byte(`"tool_calls"`)) || !bytes.Contains(ollamaMessages, []byte(`"role":"tool"`)) ||
		!bytes.Contains(ollamaMessages, []byte(`"tool_call_id":"call-read-1"`)) {
		t.Fatalf("Ollama tool messages = %s", ollamaMessages)
	}
	if err := validateAIProviderPrompt(prompt); err != nil {
		t.Fatalf("tool prompt validation = %v", err)
	}
}

func TestAIProviderToolOnlyStreamsDecodeAcrossNativeProtocols(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		consume func(context.Context, io.Reader, func(aiProviderStreamEvent) error) error
	}{
		{
			name: "openai-fragmented",
			source: "data: {\"id\":\"req-tools\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-openai\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
				"data: {\"id\":\"req-tools\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"README.md\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: [DONE]\n\n",
			consume: consumeOpenAIEventStream,
		},
		{
			name: "anthropic-fragmented",
			source: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-tools\"}}\n\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call-anthropic\",\"name\":\"read_file\",\"input\":{}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}\n\n" +
				"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
			consume: consumeAnthropicProviderEvents,
		},
		{
			name:   "google",
			source: "data: {\"responseId\":\"resp-tools\",\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"id\":\"call-google\",\"name\":\"read_file\",\"args\":{\"path\":\"README.md\"}}}]},\"finishReason\":\"STOP\"}]}\n\n",
			consume: func(ctx context.Context, source io.Reader, onEvent func(aiProviderStreamEvent) error) error {
				return consumeSSEProviderEvents(ctx, source, googleStreamEvents, onEvent)
			},
		},
		{
			name:    "ollama",
			source:  "{\"message\":{\"tool_calls\":[{\"id\":\"call-ollama\",\"function\":{\"name\":\"read_file\",\"arguments\":{\"path\":\"README.md\"}}}]},\"done\":true,\"done_reason\":\"stop\"}\n",
			consume: consumeNDJSONProviderEvents,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []aiProviderToolCall
			finishReason := ""
			err := test.consume(t.Context(), strings.NewReader(test.source), func(event aiProviderStreamEvent) error {
				if event.Kind == "tool_calls" {
					calls = append(calls, event.ToolCalls...)
				}
				if event.Kind == "completed" {
					finishReason = event.FinishReason
				}
				return nil
			})
			if err != nil || len(calls) != 1 || calls[0].Name != "read_file" || string(calls[0].Arguments) != `{"path":"README.md"}` || finishReason == "" {
				t.Fatalf("calls=%+v finish=%q error=%v", calls, finishReason, err)
			}
		})
	}
}

func TestSixAIProviderContractsDiscoverTestAndStream(t *testing.T) {
	tests := []struct {
		provider     string
		model        string
		basePath     string
		credential   string
		modelsPath   string
		streamPath   string
		modelsBody   string
		streamBody   string
		contentType  string
		verifyHeader func(*testing.T, *http.Request)
	}{
		{
			provider: "openai", model: "gpt-test", basePath: "/v1", credential: "openai-secret",
			modelsPath: "/v1/models", streamPath: "/v1/chat/completions",
			modelsBody:  `{"data":[{"id":"gpt-test","display_name":"GPT Test","input_token_limit":120000,"output_token_limit":16000}]}`,
			streamBody:  "data: {\"choices\":[{\"delta\":{\"content\":\"openai-ok\"}}]}\n\ndata: [DONE]\n\n",
			contentType: "text/event-stream",
			verifyHeader: func(t *testing.T, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer openai-secret" {
					t.Fatalf("OpenAI Authorization = %q", request.Header.Get("Authorization"))
				}
			},
		},
		{
			provider: "anthropic", model: "claude-test", basePath: "/v1", credential: "anthropic-secret",
			modelsPath: "/v1/models", streamPath: "/v1/messages",
			modelsBody: `{"data":[{"id":"claude-test","display_name":"Claude Test"}]}`,
			streamBody: "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"anthropic-ok\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
			contentType: "text/event-stream",
			verifyHeader: func(t *testing.T, request *http.Request) {
				if request.Header.Get("x-api-key") != "anthropic-secret" || request.Header.Get("anthropic-version") != "2023-06-01" {
					t.Fatalf("Anthropic headers = %#v", request.Header)
				}
			},
		},
		{
			provider: "google", model: "gemini-test", basePath: "/v1beta", credential: "google-secret",
			modelsPath: "/v1beta/models", streamPath: "/v1beta/models/gemini-test:streamGenerateContent",
			modelsBody:  `{"models":[{"name":"models/gemini-test","displayName":"Gemini Test","inputTokenLimit":120000,"outputTokenLimit":16000}]}`,
			streamBody:  "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"google-ok\"}]},\"finishReason\":\"STOP\"}]}\n\n",
			contentType: "text/event-stream",
			verifyHeader: func(t *testing.T, request *http.Request) {
				if request.Header.Get("x-goog-api-key") != "google-secret" {
					t.Fatalf("Google API key header = %q", request.Header.Get("x-goog-api-key"))
				}
			},
		},
		{
			provider: "deepseek", model: "deepseek-test", basePath: "/v1", credential: "deepseek-secret",
			modelsPath: "/v1/models", streamPath: "/v1/chat/completions",
			modelsBody:  `{"data":[{"id":"deepseek-test"}]}`,
			streamBody:  "data: {\"choices\":[{\"delta\":{\"content\":\"deepseek-ok\"}}]}\n\ndata: [DONE]\n\n",
			contentType: "text/event-stream",
			verifyHeader: func(t *testing.T, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer deepseek-secret" {
					t.Fatalf("DeepSeek Authorization = %q", request.Header.Get("Authorization"))
				}
			},
		},
		{
			provider: "ollama", model: "llama-test", credential: "",
			modelsPath: "/api/tags", streamPath: "/api/chat",
			modelsBody:  `{"models":[{"name":"llama-test","details":{"context_length":120000}}]}`,
			streamBody:  "{\"message\":{\"content\":\"ollama-ok\"},\"done\":false}\n{\"done\":true}\n",
			contentType: "application/x-ndjson",
			verifyHeader: func(t *testing.T, request *http.Request) {
				if request.Header.Get("Authorization") != "" {
					t.Fatalf("Ollama Authorization = %q", request.Header.Get("Authorization"))
				}
			},
		},
		{
			provider: "openai-compatible", model: "custom-test", basePath: "/api/v1", credential: "optional-secret",
			modelsPath: "/api/v1/models", streamPath: "/api/v1/chat/completions",
			modelsBody:  `{"data":[{"id":"custom-test"}]}`,
			streamBody:  "data: {\"choices\":[{\"delta\":{\"content\":\"openai-compatible-ok\"}}]}\n\ndata: [DONE]\n\n",
			contentType: "text/event-stream",
			verifyHeader: func(t *testing.T, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer optional-secret" {
					t.Fatalf("compatible Authorization = %q", request.Header.Get("Authorization"))
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			var modelRequests, streamRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				test.verifyHeader(t, request)
				if request.Header.Get("X-Wenzwork-Region") != "test-region" {
					t.Fatalf("non-secret header = %q", request.Header.Get("X-Wenzwork-Region"))
				}
				switch request.URL.Path {
				case test.modelsPath:
					modelRequests.Add(1)
					if test.provider == "google" && request.URL.Query().Get("pageSize") != "1000" {
						t.Fatalf("Google pageSize = %q", request.URL.Query().Get("pageSize"))
					}
					response.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(response, test.modelsBody)
				case test.streamPath:
					streamRequests.Add(1)
					if test.provider == "google" && request.URL.Query().Get("alt") != "sse" {
						t.Fatalf("Google alt = %q", request.URL.Query().Get("alt"))
					}
					var payload map[string]any
					if json.NewDecoder(request.Body).Decode(&payload) != nil {
						t.Fatal("stream request body is not JSON")
					}
					response.Header().Set("Content-Type", test.contentType)
					_, _ = io.WriteString(response, test.streamBody)
				default:
					http.NotFound(response, request)
				}
			}))
			t.Cleanup(server.Close)

			config := defaultAIConfigSettings("provider-" + test.provider)
			config.Name, config.Provider, config.BaseURL, config.Model = test.provider, test.provider, server.URL+test.basePath, test.model
			config.Credential, config.CredentialConfigured, config.Enabled, config.Revision = test.credential, test.credential != "", true, 1
			config.NonSecretHeaders = map[string]string{"X-Wenzwork-Region": "test-region"}
			if err := validateAIConfig(config); err != nil {
				t.Fatalf("validateAIConfig() = %v", err)
			}
			latency, err := defaultAIProviderRouter.Test(context.Background(), config)
			if err != nil || latency < 0 {
				t.Fatalf("Test() latency=%v error=%v", latency, err)
			}
			models, err := defaultAIProviderRouter.DiscoverModels(context.Background(), config)
			if err != nil || len(models) != 1 || models[0].ID != test.model || models[0].DisplayName == "" || !models[0].Capabilities["streaming"] {
				t.Fatalf("DiscoverModels() = %#v, %v", models, err)
			}
			var answer strings.Builder
			err = defaultAIProviderRouter.CompleteStream(context.Background(), config, nil, "hello", func(chunk string) error {
				answer.WriteString(chunk)
				return nil
			})
			if err != nil || answer.String() != test.provider+"-ok" || modelRequests.Load() != 2 || streamRequests.Load() != 1 {
				t.Fatalf("stream answer=%q models=%d streams=%d error=%v", answer.String(), modelRequests.Load(), streamRequests.Load(), err)
			}
		})
	}
}

func TestAIProviderRouterRetriesOnlyBeforeStreamingOutput(t *testing.T) {
	var modelRequests, streamRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			if modelRequests.Add(1) == 1 {
				response.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(response, `{"data":[{"id":"retry-model"}]}`)
		case "/v1/chat/completions":
			streamRequests.Add(1)
			response.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(response, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			_, _ = io.WriteString(response, "data: malformed\n\n")
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	config := defaultAIConfigSettings("retry-contract")
	config.Name, config.Provider, config.BaseURL, config.Model = "Retry", "openai-compatible", server.URL+"/v1", "retry-model"
	config.Enabled, config.Revision, config.MaxRetries, config.RetryBaseDelayMilliseconds = true, 1, 1, 1
	models, err := defaultAIProviderRouter.DiscoverModels(context.Background(), config)
	if err != nil || len(models) != 1 || modelRequests.Load() != 2 {
		t.Fatalf("retry discovery models=%#v requests=%d error=%v", models, modelRequests.Load(), err)
	}
	var chunks []string
	err = defaultAIProviderRouter.CompleteStream(context.Background(), config, nil, "hello", func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	})
	if err == nil || strings.Join(chunks, "") != "partial" || streamRequests.Load() != 1 {
		t.Fatalf("post-output retry chunks=%q requests=%d error=%v", chunks, streamRequests.Load(), err)
	}
}

func TestAIProviderConfigurationRejectsSecretHeaders(t *testing.T) {
	config := defaultAIConfigSettings("headers")
	config.Name, config.Provider, config.BaseURL, config.Model = "Headers", "openai-compatible", "https://example.test/v1", "model"
	for _, header := range []string{"Authorization", "X-API-Key", "x_access_token", "Cookie"} {
		t.Run(header, func(t *testing.T) {
			config.NonSecretHeaders = map[string]string{header: "must-not-persist"}
			if validateAIConfigForStorage(config, false) == nil {
				t.Fatalf("secret-like header %q was accepted", header)
			}
		})
	}
}

func Example_aiModelDescriptor() {
	model := aiModelDescriptor{ID: "model", DisplayName: "Model", Capabilities: map[string]bool{"streaming": true}}
	fmt.Println(model.ID, model.Capabilities["streaming"])
	// Output: model true
}

func TestAIProviderTimeoutIsHonored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(1500 * time.Millisecond)
	}))
	t.Cleanup(server.Close)
	config := defaultAIConfigSettings("timeout")
	config.Name, config.Provider, config.BaseURL, config.Model = "Timeout", "openai-compatible", server.URL+"/v1", "model"
	config.RequestTimeoutSeconds, config.MaxRetries = 1, 0
	started := time.Now()
	_, err := defaultAIProviderRouter.DiscoverModels(context.Background(), config)
	if err == nil || time.Since(started) > 1400*time.Millisecond {
		t.Fatalf("timeout duration=%v error=%v", time.Since(started), err)
	}
}
