package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maximumAIStreamRecordBytes = 128 << 10

// splitAIProviderSSERecords preserves SSE event boundaries instead of treating
// each data line as a complete JSON document. Providers may split one event
// across several data fields and may use either LF, CRLF, or CR line endings.
func splitAIProviderSSERecords(data []byte, atEOF bool) (advance int, token []byte, err error) {
	boundary, width := -1, 0
	for _, candidate := range []struct {
		separator []byte
		width     int
	}{
		{separator: []byte("\r\n\r\n"), width: 4},
		{separator: []byte("\n\n"), width: 2},
		{separator: []byte("\r\r"), width: 2},
	} {
		if index := bytes.Index(data, candidate.separator); index >= 0 && (boundary < 0 || index < boundary) {
			boundary, width = index, candidate.width
		}
	}
	if boundary >= 0 {
		return boundary + width, data[:boundary], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func aiProviderSSEData(record string) (string, bool) {
	record = strings.ReplaceAll(record, "\r\n", "\n")
	record = strings.ReplaceAll(record, "\r", "\n")
	dataLines := make([]string, 0, 1)
	for _, line := range strings.Split(record, "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			value = ""
		}
		if field != "data" {
			continue
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		dataLines = append(dataLines, value)
	}
	if len(dataLines) == 0 {
		return "", false
	}
	return strings.Join(dataLines, "\n"), true
}

func aiProviderEventContainsError(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	if value, present := raw["error"]; present && value != nil {
		switch typed := value.(type) {
		case string:
			return strings.TrimSpace(typed) != ""
		case map[string]any:
			return len(typed) > 0
		default:
			return true
		}
	}
	typeName := strings.ToLower(strings.TrimSpace(stringValue(raw["type"])))
	return typeName == "error" || strings.HasSuffix(typeName, "_error")
}

type aiModelDescriptor struct {
	ID              string          `json:"id"`
	DisplayName     string          `json:"displayName"`
	Capabilities    map[string]bool `json:"capabilities"`
	MaxInputTokens  *uint64         `json:"maxInputTokens,omitempty"`
	MaxOutputTokens *uint64         `json:"maxOutputTokens,omitempty"`
}

type aiModelDiscoverer interface {
	DiscoverModels(context.Context, aiConfig) ([]aiModelDescriptor, error)
}

type aiProviderRouter struct {
	mu    sync.Mutex
	gates map[string]*aiProviderGate
}

type aiProviderGate struct {
	slots chan struct{}
	mu    sync.Mutex
	next  time.Time
}

var defaultAIProviderRouter = &aiProviderRouter{gates: map[string]*aiProviderGate{}}

func (router *aiProviderRouter) Test(ctx context.Context, config aiConfig) (time.Duration, error) {
	config = effectiveAIConfig(config)
	release, err := router.acquire(ctx, config)
	if err != nil {
		return 0, err
	}
	defer release()
	provider, err := providerAdapter(config)
	if err != nil {
		return 0, err
	}
	return withAIRetry(ctx, config, func() (time.Duration, error) { return provider.Test(ctx, config) })
}

func (router *aiProviderRouter) Complete(ctx context.Context, config aiConfig, history []chatMessage, prompt string) (string, error) {
	config = effectiveAIConfig(config)
	release, err := router.acquire(ctx, config)
	if err != nil {
		return "", err
	}
	defer release()
	provider, err := providerAdapter(config)
	if err != nil {
		return "", err
	}
	return withAIRetry(ctx, config, func() (string, error) { return provider.Complete(ctx, config, history, prompt) })
}

func (router *aiProviderRouter) CompleteStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onChunk func(string) error) error {
	if onChunk == nil {
		return errAIProvider
	}
	return router.CompleteEventStream(ctx, config, history, prompt, func(event aiProviderStreamEvent) error {
		if event.Kind == "text" && event.Delta != "" {
			return onChunk(event.Delta)
		}
		return nil
	})
}

func (router *aiProviderRouter) CompleteEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onEvent func(aiProviderStreamEvent) error) error {
	return router.CompletePromptEventStream(ctx, config, history, aiProviderPrompt{Text: prompt}, onEvent)
}

func (router *aiProviderRouter) CompletePromptEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil {
		return errAIProvider
	}
	if err := validateAIProviderPrompt(prompt); err != nil {
		return err
	}
	config = effectiveAIConfig(config)
	release, err := router.acquire(ctx, config)
	if err != nil {
		return err
	}
	defer release()
	provider, err := providerAdapter(config)
	if err != nil {
		return err
	}
	promptStreamer, promptCapable := provider.(promptEventStreamingAIProvider)
	eventStreamer, eventCapable := provider.(eventStreamingAIProvider)
	streamer, streamCapable := provider.(streamingAIProvider)
	if len(prompt.Images) > 0 && !promptCapable {
		return errAIProvider
	}
	if !promptCapable && !eventCapable && !streamCapable {
		answer, err := provider.Complete(ctx, config, history, prompt.Text)
		if err != nil {
			return err
		}
		return onEvent(aiProviderStreamEvent{Kind: "text", Delta: answer})
	}
	for attempt := uint32(0); ; attempt++ {
		emitted := false
		deferred := make([]aiProviderStreamEvent, 0, 4)
		emit := func(event aiProviderStreamEvent) error {
			meaningful := (event.Kind == "text" || event.Kind == "reasoning") && event.Delta != "" || event.Kind == "tool_calls" && len(event.ToolCalls) > 0
			if !emitted && !meaningful {
				if len(deferred) >= 64 {
					return errAIProvider
				}
				deferred = append(deferred, event)
				return nil
			}
			if meaningful {
				emitted = true
			}
			if err := onEvent(event); err != nil {
				return err
			}
			if emitted && len(deferred) > 0 {
				for _, pending := range deferred {
					if err := onEvent(pending); err != nil {
						return err
					}
				}
				deferred = deferred[:0]
			}
			return nil
		}
		var err error
		if promptCapable {
			err = promptStreamer.CompletePromptEventStream(ctx, config, history, prompt, emit)
		} else if eventCapable {
			err = eventStreamer.CompleteEventStream(ctx, config, history, prompt.Text, emit)
		} else {
			err = streamer.CompleteStream(ctx, config, history, prompt.Text, func(chunk string) error {
				return emit(aiProviderStreamEvent{Kind: "text", Delta: chunk})
			})
		}
		if err == nil && !emitted {
			err = errAIProvider
		}
		if err == nil || emitted || attempt >= config.MaxRetries || !retryableAIError(err) {
			return err
		}
		if err := waitAIRetry(ctx, config, attempt+1); err != nil {
			return err
		}
	}
}

func (router *aiProviderRouter) DiscoverModels(ctx context.Context, config aiConfig) ([]aiModelDescriptor, error) {
	config = effectiveAIConfig(config)
	release, err := router.acquire(ctx, config)
	if err != nil {
		return nil, err
	}
	defer release()
	provider, err := providerAdapter(config)
	if err != nil {
		return nil, err
	}
	discoverer, ok := provider.(aiModelDiscoverer)
	if !ok {
		return nil, errAIProvider
	}
	models, err := withAIRetry(ctx, config, func() ([]aiModelDescriptor, error) {
		return discoverer.DiscoverModels(ctx, config)
	})
	if err != nil {
		return nil, err
	}
	if len(models) == 0 || len(models) > 1000 {
		return nil, errAIProvider
	}
	slices.SortFunc(models, func(left, right aiModelDescriptor) int { return strings.Compare(left.ID, right.ID) })
	return models, nil
}

func (router *aiProviderRouter) acquire(ctx context.Context, config aiConfig) (func(), error) {
	key := config.ID
	if key == "" {
		key = config.Provider + "\x00" + config.BaseURL
	}
	router.mu.Lock()
	gate := router.gates[key]
	if gate == nil {
		gate = &aiProviderGate{slots: make(chan struct{}, 2)}
		router.gates[key] = gate
	}
	router.mu.Unlock()
	select {
	case gate.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	gate.mu.Lock()
	now := time.Now()
	wait := time.Duration(0)
	if gate.next.After(now) {
		wait = gate.next.Sub(now)
	}
	start := now.Add(wait)
	gate.next = start.Add(100 * time.Millisecond)
	gate.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			<-gate.slots
			return nil, ctx.Err()
		}
	}
	return func() { <-gate.slots }, nil
}

func providerAdapter(config aiConfig) (aiProvider, error) {
	switch config.Provider {
	case "openai", "deepseek", "openai-compatible":
		return openAICompatibleProvider{}, nil
	case "anthropic":
		return anthropicProvider{}, nil
	case "google":
		return googleProvider{}, nil
	case "ollama":
		return ollamaProvider{}, nil
	default:
		return nil, errRPCInvalid
	}
}

func effectiveAIConfig(config aiConfig) aiConfig {
	legacyShape := config.ReasoningEffort == ""
	defaults := defaultAIConfigSettings(config.ID)
	config.Provider = canonicalAIProvider(config.Provider)
	config.NonSecretHeaders = cloneStringMap(config.NonSecretHeaders)
	if config.SystemPrompt == "" && legacyShape {
		config.SystemPrompt = defaults.SystemPrompt
	}
	if config.ReasoningEffort == "" {
		config.ReasoningEffort = defaults.ReasoningEffort
	}
	if config.Temperature == 0 && legacyShape {
		config.Temperature = defaults.Temperature
	}
	if config.MaxTurnOutputTokens == 0 {
		config.MaxTurnOutputTokens = defaults.MaxTurnOutputTokens
	}
	if config.MaxActiveContextTokens == 0 {
		config.MaxActiveContextTokens = defaults.MaxActiveContextTokens
	}
	if config.MaxAgentRounds == 0 {
		config.MaxAgentRounds = defaults.MaxAgentRounds
	}
	if config.MaxAgentToolCalls == 0 {
		config.MaxAgentToolCalls = defaults.MaxAgentToolCalls
	}
	if config.MaxAgentNoProgressRounds == 0 {
		config.MaxAgentNoProgressRounds = defaults.MaxAgentNoProgressRounds
	}
	if config.RequestTimeoutSeconds == 0 {
		config.RequestTimeoutSeconds = defaults.RequestTimeoutSeconds
	}
	if config.MaxRetries == 0 && legacyShape {
		config.MaxRetries = defaults.MaxRetries
	}
	if config.RetryBaseDelayMilliseconds == 0 && legacyShape {
		config.RetryBaseDelayMilliseconds = defaults.RetryBaseDelayMilliseconds
	}
	return config
}

func aiOpenAIReasoningFields(config aiConfig) map[string]any {
	effort := config.ReasoningEffort
	if effort == "" || effort == "automatic" {
		return nil
	}
	if config.Provider == "deepseek" {
		if effort == "none" {
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}
		}
		return map[string]any{
			"thinking":         map[string]any{"type": "enabled"},
			"reasoning_effort": effort,
		}
	}
	return map[string]any{"reasoning_effort": effort}
}

func aiAnthropicReasoningFields(config aiConfig) map[string]any {
	effort := config.ReasoningEffort
	if effort == "" || effort == "automatic" {
		return nil
	}
	if effort == "none" {
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}
	}
	if effort == "minimal" {
		effort = "low"
	}
	return map[string]any{"output_config": map[string]any{"effort": effort}}
}

func aiGoogleThinkingConfig(config aiConfig) map[string]any {
	effort := config.ReasoningEffort
	if effort == "" || effort == "automatic" {
		return nil
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(config.Model)), "gemini-2.5") {
		budget := -1
		switch effort {
		case "none":
			budget = 0
		case "minimal":
			budget = 512
		case "low":
			budget = 1024
		case "medium":
			budget = 8192
		}
		return map[string]any{"thinkingBudget": budget}
	}
	level := effort
	switch effort {
	case "none", "minimal":
		level = "minimal"
	case "xhigh", "max", "ultra":
		level = "high"
	}
	return map[string]any{"thinkingLevel": level}
}

func aiOllamaThinkValue(config aiConfig) any {
	effort := config.ReasoningEffort
	if effort == "" || effort == "automatic" {
		return nil
	}
	if effort == "none" {
		return false
	}
	if !strings.Contains(strings.ToLower(strings.TrimSpace(config.Model)), "gpt-oss") {
		return true
	}
	switch effort {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	default:
		return "high"
	}
}

func withAIRetry[T any](ctx context.Context, config aiConfig, operation func() (T, error)) (T, error) {
	var zero T
	for attempt := uint32(0); ; attempt++ {
		value, err := operation()
		if err == nil {
			return value, nil
		}
		if attempt >= config.MaxRetries || !retryableAIError(err) {
			return zero, err
		}
		if err := waitAIRetry(ctx, config, attempt+1); err != nil {
			return zero, err
		}
	}
}

func waitAIRetry(ctx context.Context, config aiConfig, attempt uint32) error {
	delay := time.Duration(config.RetryBaseDelayMilliseconds) * time.Millisecond * time.Duration(attempt)
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type aiHTTPError struct {
	retryable       bool
	contextOverflow bool
	statusCode      int
}

func (err aiHTTPError) Error() string { return errAIProvider.Error() }
func (err aiHTTPError) Unwrap() error { return errAIProvider }

func retryableAIError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errAIProviderStreamTruncated) {
		return true
	}
	var httpError aiHTTPError
	return errors.As(err, &httpError) && httpError.retryable
}

// aiTimedResponseBody keeps a streaming request's timeout alive until the
// response body is consumed. http.Client.Do returns as soon as headers arrive,
// so cancelling a request context at that point would truncate every stream.
type aiTimedResponseBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelFunc
}

func (body *aiTimedResponseBody) Read(buffer []byte) (int, error) {
	read, err := body.ReadCloser.Read(buffer)
	if err == nil || body.ctx == nil {
		return read, err
	}
	switch body.ctx.Err() {
	case context.DeadlineExceeded:
		return read, errAIProviderRequestTimeout
	case context.Canceled:
		return read, context.Canceled
	default:
		return read, err
	}
}

func (body *aiTimedResponseBody) Close() error {
	err := body.ReadCloser.Close()
	if body.cancel != nil {
		body.cancel()
	}
	return err
}

func aiProviderStreamScanError(ctx context.Context, scanErr error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(scanErr, errAIProviderRequestTimeout) || errors.Is(scanErr, context.DeadlineExceeded) {
		return errAIProviderRequestTimeout
	}
	if errors.Is(scanErr, context.Canceled) {
		return context.Canceled
	}
	return aiHTTPError{retryable: true}
}

func doAIProviderJSON(ctx context.Context, config aiConfig, method, suffix string, payload []byte) ([]byte, error) {
	response, err := openAIProviderResponse(ctx, config, method, suffix, payload, "application/json", false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximumAIResponseBytes+1))
	if err != nil || len(contents) == 0 || len(contents) > maximumAIResponseBytes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, aiHTTPError{retryable: true}
	}
	return contents, nil
}

func openAIProviderResponse(ctx context.Context, config aiConfig, method, suffix string, payload []byte, accept string, streaming bool) (*http.Response, error) {
	endpoint, client, err := newAIRequestTarget(config.BaseURL, suffix)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(config.RequestTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Duration(defaultAIRequestTimeoutSeconds) * time.Second
	}
	requestContext := ctx
	releaseRequest := func() {}
	if streaming {
		client.Timeout = 0
		requestContext, releaseRequest = context.WithTimeout(ctx, timeout)
	} else {
		client.Timeout = timeout
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(requestContext, method, endpoint.String(), body)
	if err != nil {
		releaseRequest()
		return nil, errAIProvider
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "wenzwork-device-agent/1")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range config.NonSecretHeaders {
		request.Header.Set(name, value)
	}
	switch config.Provider {
	case "anthropic":
		request.Header.Set("x-api-key", config.Credential)
		request.Header.Set("anthropic-version", "2023-06-01")
	case "google":
		request.Header.Set("x-goog-api-key", config.Credential)
	case "ollama":
		if config.Credential != "" {
			request.Header.Set("Authorization", "Bearer "+config.Credential)
		}
	default:
		if config.Credential != "" {
			request.Header.Set("Authorization", "Bearer "+config.Credential)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		requestErr := requestContext.Err()
		releaseRequest()
		if errors.Is(requestErr, context.DeadlineExceeded) {
			return nil, errAIProviderRequestTimeout
		}
		if requestErr != nil {
			return nil, requestErr
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, aiHTTPError{retryable: true}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		contents, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		releaseRequest()
		retryable := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return nil, aiHTTPError{
			retryable:       retryable,
			contextOverflow: isAIContextOverflowResponse(response.StatusCode, contents),
			statusCode:      response.StatusCode,
		}
	}
	if streaming {
		response.Body = &aiTimedResponseBody{ReadCloser: response.Body, ctx: requestContext, cancel: releaseRequest}
	} else {
		releaseRequest()
	}
	return response, nil
}

type anthropicProvider struct{}

func (provider anthropicProvider) Test(ctx context.Context, config aiConfig) (time.Duration, error) {
	started := time.Now()
	_, err := provider.DiscoverModels(ctx, config)
	return time.Since(started), err
}

func (provider anthropicProvider) DiscoverModels(ctx context.Context, config aiConfig) ([]aiModelDescriptor, error) {
	contents, err := doAIProviderJSON(ctx, config, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	return parseModelList(contents, config.Provider, "data")
}

func (provider anthropicProvider) Complete(ctx context.Context, config aiConfig, history []chatMessage, prompt string) (string, error) {
	return collectAIStream(func(onChunk func(string) error) error {
		return provider.CompleteStream(ctx, config, history, prompt, onChunk)
	})
}

func (anthropicProvider) CompleteStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onChunk func(string) error) error {
	if onChunk == nil {
		return errAIProvider
	}
	return (anthropicProvider{}).CompleteEventStream(ctx, config, history, prompt, func(event aiProviderStreamEvent) error {
		if event.Kind == "text" && event.Delta != "" {
			return onChunk(event.Delta)
		}
		return nil
	})
}

func (anthropicProvider) CompleteEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onEvent func(aiProviderStreamEvent) error) error {
	return (anthropicProvider{}).CompletePromptEventStream(ctx, config, history, aiProviderPrompt{Text: prompt}, onEvent)
}

func (anthropicProvider) CompletePromptEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil || validateAIProviderPrompt(prompt) != nil {
		return errAIProvider
	}
	body := map[string]any{
		"model": config.Model, "system": config.SystemPrompt, "messages": anthropicMessagesForPrompt(history, prompt),
		"stream": true, "temperature": config.Temperature, "max_tokens": config.MaxTurnOutputTokens,
	}
	if len(prompt.Tools) > 0 {
		body["tools"] = anthropicToolDefinitions(prompt.Tools)
	}
	for name, value := range aiAnthropicReasoningFields(config) {
		body[name] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return errAIProvider
	}
	response, err := openAIProviderResponse(ctx, config, http.MethodPost, "/messages", payload, "text/event-stream", true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return consumeAnthropicProviderEvents(ctx, response.Body, onEvent)
}

type googleProvider struct{}

func (provider googleProvider) Test(ctx context.Context, config aiConfig) (time.Duration, error) {
	started := time.Now()
	_, err := provider.DiscoverModels(ctx, config)
	return time.Since(started), err
}

func (provider googleProvider) DiscoverModels(ctx context.Context, config aiConfig) ([]aiModelDescriptor, error) {
	contents, err := doAIProviderJSON(ctx, config, http.MethodGet, "/models?pageSize=1000", nil)
	if err != nil {
		return nil, err
	}
	return parseModelList(contents, config.Provider, "models")
}

func (provider googleProvider) Complete(ctx context.Context, config aiConfig, history []chatMessage, prompt string) (string, error) {
	return collectAIStream(func(onChunk func(string) error) error {
		return provider.CompleteStream(ctx, config, history, prompt, onChunk)
	})
}

func (googleProvider) CompleteStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onChunk func(string) error) error {
	if onChunk == nil {
		return errAIProvider
	}
	return (googleProvider{}).CompleteEventStream(ctx, config, history, prompt, func(event aiProviderStreamEvent) error {
		if event.Kind == "text" && event.Delta != "" {
			return onChunk(event.Delta)
		}
		return nil
	})
}

func (googleProvider) CompleteEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onEvent func(aiProviderStreamEvent) error) error {
	return (googleProvider{}).CompletePromptEventStream(ctx, config, history, aiProviderPrompt{Text: prompt}, onEvent)
}

func (googleProvider) CompletePromptEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil || validateAIProviderPrompt(prompt) != nil {
		return errAIProvider
	}
	generationConfig := map[string]any{"temperature": config.Temperature, "maxOutputTokens": config.MaxTurnOutputTokens}
	if thinkingConfig := aiGoogleThinkingConfig(config); thinkingConfig != nil {
		generationConfig["thinkingConfig"] = thinkingConfig
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]string{{"text": config.SystemPrompt}}},
		"contents":          googleMessagesForPrompt(history, prompt),
		"generationConfig":  generationConfig,
	}
	if len(prompt.Tools) > 0 {
		body["tools"] = []map[string]any{{"functionDeclarations": googleToolDefinitions(prompt.Tools)}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return errAIProvider
	}
	suffix := "/models/" + url.PathEscape(config.Model) + ":streamGenerateContent?alt=sse"
	response, err := openAIProviderResponse(ctx, config, http.MethodPost, suffix, payload, "text/event-stream", true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return consumeSSEProviderEvents(ctx, response.Body, googleStreamEvents, onEvent)
}

type ollamaProvider struct{}

func (provider ollamaProvider) Test(ctx context.Context, config aiConfig) (time.Duration, error) {
	started := time.Now()
	_, err := provider.DiscoverModels(ctx, config)
	return time.Since(started), err
}

func (provider ollamaProvider) DiscoverModels(ctx context.Context, config aiConfig) ([]aiModelDescriptor, error) {
	contents, err := doAIProviderJSON(ctx, config, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return nil, err
	}
	return parseModelList(contents, config.Provider, "models")
}

func (provider ollamaProvider) Complete(ctx context.Context, config aiConfig, history []chatMessage, prompt string) (string, error) {
	return collectAIStream(func(onChunk func(string) error) error {
		return provider.CompleteStream(ctx, config, history, prompt, onChunk)
	})
}

func (ollamaProvider) CompleteStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onChunk func(string) error) error {
	if onChunk == nil {
		return errAIProvider
	}
	return (ollamaProvider{}).CompleteEventStream(ctx, config, history, prompt, func(event aiProviderStreamEvent) error {
		if event.Kind == "text" && event.Delta != "" {
			return onChunk(event.Delta)
		}
		return nil
	})
}

func (ollamaProvider) CompleteEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onEvent func(aiProviderStreamEvent) error) error {
	return (ollamaProvider{}).CompletePromptEventStream(ctx, config, history, aiProviderPrompt{Text: prompt}, onEvent)
}

func (ollamaProvider) CompletePromptEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil || validateAIProviderPrompt(prompt) != nil {
		return errAIProvider
	}
	body := map[string]any{
		"model": config.Model, "messages": ollamaMessagesForPrompt(config, history, prompt), "stream": true,
		"options": map[string]any{"temperature": config.Temperature, "num_predict": config.MaxTurnOutputTokens},
	}
	if len(prompt.Tools) > 0 {
		body["tools"] = openAIToolDefinitions(prompt.Tools)
	}
	if think := aiOllamaThinkValue(config); think != nil {
		body["think"] = think
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return errAIProvider
	}
	response, err := openAIProviderResponse(ctx, config, http.MethodPost, "/api/chat", payload, "application/x-ndjson", true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return consumeNDJSONProviderEvents(ctx, response.Body, onEvent)
}

func (openAICompatibleProvider) DiscoverModels(ctx context.Context, config aiConfig) ([]aiModelDescriptor, error) {
	contents, err := doAIProviderJSON(ctx, config, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	return parseModelList(contents, config.Provider, "data")
}

func parseModelList(contents []byte, provider, field string) ([]aiModelDescriptor, error) {
	var root map[string]any
	if json.Unmarshal(contents, &root) != nil {
		return nil, errAIProvider
	}
	values, ok := root[field].([]any)
	if !ok || len(values) == 0 || len(values) > 1000 {
		return nil, errAIProvider
	}
	models := make([]aiModelDescriptor, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errAIProvider
		}
		id := stringValue(item["id"])
		if id == "" {
			id = stringValue(item["name"])
		}
		if provider == "google" {
			id = strings.TrimPrefix(id, "models/")
		}
		if provider == "ollama" && id == "" {
			id = stringValue(item["model"])
		}
		if id == "" || len(id) > 240 || !utf8.ValidString(id) {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		displayName := firstString(item["displayName"], item["display_name"], item["name"], id)
		models = append(models, aiModelDescriptor{
			ID: id, DisplayName: displayName, Capabilities: defaultAIModelCapabilities(provider, id),
			MaxInputTokens:  firstUint(item["inputTokenLimit"], item["input_token_limit"], item["contextWindowTokenLimit"], item["context_window"], item["context_length"]),
			MaxOutputTokens: firstUint(item["outputTokenLimit"], item["output_token_limit"], item["max_output_tokens"], item["max_tokens"]),
		})
	}
	if len(models) == 0 {
		return nil, errAIProvider
	}
	return models, nil
}

func defaultAIModelCapabilities(provider, model string) map[string]bool {
	capabilities := map[string]bool{"streaming": true, "tokenCount": true}
	switch provider {
	case "openai", "anthropic", "google":
		capabilities["nativeTools"] = true
		capabilities["imageInput"] = true
		capabilities["documentInput"] = true
	case "deepseek", "ollama":
		capabilities["nativeTools"] = true
	}
	lower := strings.ToLower(model)
	if strings.Contains(lower, "reason") || strings.Contains(lower, "thinking") || strings.Contains(lower, "o1") || strings.Contains(lower, "o3") || strings.Contains(lower, "o4") {
		capabilities["reasoning"] = true
	}
	return capabilities
}

func anthropicMessages(history []chatMessage, prompt string) []map[string]any {
	return anthropicMessagesForPrompt(history, aiProviderPrompt{Text: prompt})
}

func anthropicMessagesForPrompt(history []chatMessage, prompt aiProviderPrompt) []map[string]any {
	messages := boundedAIMessages(history, prompt.Text)
	result := make([]map[string]any, 0, len(messages))
	for index, message := range messages {
		content := any(message.Content)
		if index == len(messages)-1 && len(prompt.Images) > 0 {
			blocks := make([]map[string]any, 0, len(prompt.Images)+1)
			for _, image := range prompt.Images {
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": image.MimeType, "data": image.Base64Data,
				}})
			}
			if message.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": message.Content})
			}
			content = blocks
		}
		result = append(result, map[string]any{"role": message.Role, "content": content})
	}
	for _, exchange := range prompt.ToolExchanges {
		assistantBlocks := make([]map[string]any, 0, len(exchange.Calls)+1)
		if exchange.AssistantText != "" {
			assistantBlocks = append(assistantBlocks, map[string]any{"type": "text", "text": exchange.AssistantText})
		}
		for _, call := range exchange.Calls {
			var arguments map[string]any
			_ = json.Unmarshal(call.Arguments, &arguments)
			assistantBlocks = append(assistantBlocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": arguments})
		}
		result = append(result, map[string]any{"role": "assistant", "content": assistantBlocks})
		resultBlocks := make([]map[string]any, 0, len(exchange.Results))
		for _, toolResult := range exchange.Results {
			content := any(toolResult.Content)
			if toolResult.Untrusted {
				content = []map[string]any{{"type": "text", "text": aiProviderUntrustedContent(toolResult.Content)}}
			} else if toolResult.Image != nil {
				blocks := []map[string]any{{"type": "text", "text": toolResult.Content}}
				blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{
					"type": "base64", "media_type": toolResult.Image.MimeType, "data": toolResult.Image.Base64Data,
				}})
				content = blocks
			}
			resultBlocks = append(resultBlocks, map[string]any{
				"type": "tool_result", "tool_use_id": toolResult.ToolCallID, "content": content, "is_error": toolResult.IsError,
			})
		}
		result = append(result, map[string]any{"role": "user", "content": resultBlocks})
	}
	return result
}

func anthropicToolDefinitions(definitions []aiWorkspaceToolDefinition) []map[string]any {
	result := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, map[string]any{"name": definition.Name, "description": definition.Description, "input_schema": definition.InputSchema})
	}
	return result
}

func googleMessages(history []chatMessage, prompt string) []map[string]any {
	return googleMessagesForPrompt(history, aiProviderPrompt{Text: prompt})
}

func googleMessagesForPrompt(history []chatMessage, prompt aiProviderPrompt) []map[string]any {
	messages := boundedAIMessages(history, prompt.Text)
	result := make([]map[string]any, 0, len(messages))
	for index, message := range messages {
		role := "user"
		if message.Role == "assistant" {
			role = "model"
		}
		parts := make([]map[string]any, 0, len(prompt.Images)+1)
		if message.Content != "" {
			parts = append(parts, map[string]any{"text": message.Content})
		}
		if index == len(messages)-1 {
			for _, image := range prompt.Images {
				parts = append(parts, map[string]any{"inlineData": map[string]any{
					"mimeType": image.MimeType, "data": image.Base64Data,
				}})
			}
		}
		result = append(result, map[string]any{"role": role, "parts": parts})
	}
	for _, exchange := range prompt.ToolExchanges {
		modelParts := make([]map[string]any, 0, len(exchange.Calls)+1)
		if exchange.AssistantText != "" {
			modelParts = append(modelParts, map[string]any{"text": exchange.AssistantText})
		}
		for _, call := range exchange.Calls {
			var arguments map[string]any
			_ = json.Unmarshal(call.Arguments, &arguments)
			modelParts = append(modelParts, map[string]any{"functionCall": map[string]any{"id": call.ID, "name": call.Name, "args": arguments}})
		}
		result = append(result, map[string]any{"role": "model", "parts": modelParts})
		responseParts := make([]map[string]any, 0, len(exchange.Results))
		for _, toolResult := range exchange.Results {
			responseContent := toolResult.Content
			if toolResult.Untrusted {
				responseContent = aiProviderUntrustedContent(toolResult.Content)
			}
			responseParts = append(responseParts, map[string]any{"functionResponse": map[string]any{
				"id": toolResult.ToolCallID, "name": toolResult.Name,
				"response": map[string]any{"content": responseContent, "isError": toolResult.IsError},
			}})
			if toolResult.Image != nil {
				responseParts = append(responseParts, map[string]any{"inlineData": map[string]any{
					"mimeType": toolResult.Image.MimeType, "data": toolResult.Image.Base64Data,
				}})
			}
		}
		result = append(result, map[string]any{"role": "user", "parts": responseParts})
	}
	return result
}

func googleToolDefinitions(definitions []aiWorkspaceToolDefinition) []map[string]any {
	result := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, map[string]any{"name": definition.Name, "description": definition.Description, "parameters": definition.InputSchema})
	}
	return result
}

func ollamaMessages(config aiConfig, history []chatMessage, prompt string) []map[string]any {
	return ollamaMessagesForPrompt(config, history, aiProviderPrompt{Text: prompt})
}

func ollamaMessagesForPrompt(config aiConfig, history []chatMessage, prompt aiProviderPrompt) []map[string]any {
	messages := boundedAIMessages(history, prompt.Text)
	result := make([]map[string]any, 0, len(messages)+1)
	if config.SystemPrompt != "" {
		result = append(result, map[string]any{"role": "system", "content": config.SystemPrompt})
	}
	for index, message := range messages {
		value := map[string]any{"role": message.Role, "content": message.Content}
		if index == len(messages)-1 && len(prompt.Images) > 0 {
			images := make([]string, 0, len(prompt.Images))
			for _, image := range prompt.Images {
				images = append(images, image.Base64Data)
			}
			value["images"] = images
		}
		result = append(result, value)
	}
	for _, exchange := range prompt.ToolExchanges {
		toolCalls := make([]map[string]any, 0, len(exchange.Calls))
		for _, call := range exchange.Calls {
			var arguments map[string]any
			_ = json.Unmarshal(call.Arguments, &arguments)
			toolCalls = append(toolCalls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": arguments}})
		}
		result = append(result, map[string]any{"role": "assistant", "content": exchange.AssistantText, "tool_calls": toolCalls})
		for _, toolResult := range exchange.Results {
			result = append(result, map[string]any{"role": "tool", "content": toolResult.Content, "tool_name": toolResult.Name, "tool_call_id": toolResult.ToolCallID})
		}
	}
	return result
}

func consumeSSEText(ctx context.Context, source io.Reader, extract func(map[string]any) string, onChunk func(string) error) error {
	scanner := bufio.NewScanner(io.LimitReader(source, maximumAIResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), maximumAIStreamRecordBytes)
	scanner.Split(splitAIProviderSSERecords)
	seen, total := false, 0
	protocolTerminal := false
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, present := aiProviderSSEData(scanner.Text())
		if !present {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			protocolTerminal = true
			return streamCompletionResult(seen, total, protocolTerminal)
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			return errAIProvider
		}
		if aiProviderEventContainsError(event) {
			return errAIProvider
		}
		chunk := extract(event)
		if chunk == "" {
			continue
		}
		if !utf8.ValidString(chunk) || total+len(chunk) > maximumAssistantBytes {
			return errAIProvider
		}
		total += len(chunk)
		seen = true
		if err := onChunk(chunk); err != nil {
			return err
		}
	}
	if scanner.Err() != nil {
		return aiProviderStreamScanError(ctx, scanner.Err())
	}
	return streamCompletionResult(seen, total, protocolTerminal)
}

func consumeSSEProviderEvents(ctx context.Context, source io.Reader, extract func(map[string]any) []aiProviderStreamEvent, onEvent func(aiProviderStreamEvent) error) error {
	if extract == nil || onEvent == nil {
		return errAIProvider
	}
	scanner := bufio.NewScanner(io.LimitReader(source, maximumAIResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), maximumAIStreamRecordBytes)
	scanner.Split(splitAIProviderSSERecords)
	seenOutput, seenText, textBytes, reasoningBytes := false, false, 0, 0
	protocolTerminal := false
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, present := aiProviderSSEData(scanner.Text())
		if !present {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			protocolTerminal = true
			return streamCompletionResult(seenOutput, textBytes, protocolTerminal)
		}
		var raw map[string]any
		if json.Unmarshal([]byte(data), &raw) != nil {
			return errAIProvider
		}
		if aiProviderEventContainsError(raw) {
			return errAIProvider
		}
		for _, event := range extract(raw) {
			if event.Kind == "completed" {
				protocolTerminal = true
			}
			if event.Kind == "tool_calls" {
				if len(event.ToolCalls) == 0 {
					return errAIProvider
				}
				for _, call := range event.ToolCalls {
					if !validAIProviderToolCall(call) {
						return errAIProvider
					}
				}
				seenOutput = true
			}
			if event.Kind == "text" || event.Kind == "reasoning" {
				if event.Delta == "" {
					continue
				}
				if !utf8.ValidString(event.Delta) {
					return errAIProvider
				}
				if event.Kind == "text" {
					textBytes += len(event.Delta)
					seenText, seenOutput = true, true
					if textBytes > maximumAssistantBytes {
						return errAIProvider
					}
				} else {
					reasoningBytes += len(event.Delta)
					if reasoningBytes > maximumAssistantBytes {
						return errAIProvider
					}
				}
			}
			if err := onEvent(event); err != nil {
				return err
			}
		}
	}
	if scanner.Err() != nil {
		return aiProviderStreamScanError(ctx, scanner.Err())
	}
	_ = seenText
	return streamCompletionResult(seenOutput, textBytes, protocolTerminal)
}

type anthropicToolBlock struct {
	Index       int
	ID          string
	Name        string
	Input       map[string]any
	PartialJSON strings.Builder
	Emitted     bool
}

func consumeAnthropicProviderEvents(ctx context.Context, source io.Reader, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil {
		return errAIProvider
	}
	scanner := bufio.NewScanner(io.LimitReader(source, maximumAIResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), maximumAIStreamRecordBytes)
	scanner.Split(splitAIProviderSSERecords)
	blocks := make(map[int]*anthropicToolBlock)
	seenOutput, textBytes, reasoningBytes := false, 0, 0
	protocolTerminal := false
	requestID := ""
	emitBlock := func(block *anthropicToolBlock) error {
		if block == nil || block.Emitted {
			return nil
		}
		arguments := strings.TrimSpace(block.PartialJSON.String())
		if arguments == "" {
			encoded, err := json.Marshal(block.Input)
			if err != nil {
				return errAIProvider
			}
			arguments = string(encoded)
		}
		id := block.ID
		if id == "" {
			id = "toolu_" + aiWorkspaceBytesHash([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", requestID, block.Index, block.Name, arguments)))[:24]
		}
		call := aiProviderToolCall{ID: id, Name: block.Name, Arguments: json.RawMessage(arguments)}
		if !validAIProviderToolCall(call) {
			return errAIProvider
		}
		block.Emitted, seenOutput = true, true
		return onEvent(aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{call}, ProviderRequestID: requestID})
	}
	emitRemaining := func() error {
		indexes := make([]int, 0, len(blocks))
		for index := range blocks {
			indexes = append(indexes, index)
		}
		slices.Sort(indexes)
		for _, index := range indexes {
			if err := emitBlock(blocks[index]); err != nil {
				return err
			}
		}
		return nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, present := aiProviderSSEData(scanner.Text())
		if !present {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			protocolTerminal = true
			if err := emitRemaining(); err != nil {
				return err
			}
			return streamCompletionResult(seenOutput, textBytes, protocolTerminal)
		}
		var raw map[string]any
		if json.Unmarshal([]byte(data), &raw) != nil {
			return errAIProvider
		}
		if aiProviderEventContainsError(raw) {
			return errAIProvider
		}
		message, _ := raw["message"].(map[string]any)
		if value := firstString(raw["id"], message["id"]); value != "" {
			requestID = value
		}
		index := int(uintFromAny(raw["index"]))
		typeName := stringValue(raw["type"])
		if typeName == "message_stop" {
			protocolTerminal = true
		}
		if typeName == "content_block_start" {
			contentBlock, _ := raw["content_block"].(map[string]any)
			if stringValue(contentBlock["type"]) == "tool_use" {
				input, _ := contentBlock["input"].(map[string]any)
				blocks[index] = &anthropicToolBlock{Index: index, ID: stringValue(contentBlock["id"]), Name: stringValue(contentBlock["name"]), Input: input}
			}
		}
		if typeName == "content_block_delta" {
			delta, _ := raw["delta"].(map[string]any)
			if stringValue(delta["type"]) == "input_json_delta" {
				block := blocks[index]
				partial := rawStringValue(delta["partial_json"])
				if block == nil || !utf8.ValidString(partial) || block.PartialJSON.Len()+len(partial) > maximumRPCPayload {
					return errAIProvider
				}
				block.PartialJSON.WriteString(partial)
			}
		}
		if typeName == "content_block_stop" {
			if err := emitBlock(blocks[index]); err != nil {
				return err
			}
		}
		for _, event := range anthropicStreamEvents(raw) {
			if event.ProviderRequestID == "" {
				event.ProviderRequestID = requestID
			}
			if event.Kind == "text" || event.Kind == "reasoning" {
				if event.Delta == "" || !utf8.ValidString(event.Delta) {
					continue
				}
				if event.Kind == "text" {
					textBytes += len(event.Delta)
					seenOutput = true
					if textBytes > maximumAssistantBytes {
						return errAIProvider
					}
				} else {
					reasoningBytes += len(event.Delta)
					if reasoningBytes > maximumAssistantBytes {
						return errAIProvider
					}
				}
			}
			if err := onEvent(event); err != nil {
				return err
			}
		}
		if protocolTerminal {
			if err := emitRemaining(); err != nil {
				return err
			}
			return streamCompletionResult(seenOutput, textBytes, protocolTerminal)
		}
	}
	if scanner.Err() != nil {
		return aiProviderStreamScanError(ctx, scanner.Err())
	}
	return streamCompletionResult(seenOutput, textBytes, protocolTerminal)
}

func anthropicStreamEvents(raw map[string]any) []aiProviderStreamEvent {
	result := make([]aiProviderStreamEvent, 0, 4)
	requestID := stringValue(raw["id"])
	message, _ := raw["message"].(map[string]any)
	if requestID == "" {
		requestID = stringValue(message["id"])
	}
	delta, _ := raw["delta"].(map[string]any)
	text := rawStringValue(delta["text"])
	if text == "" {
		if block, ok := raw["content_block"].(map[string]any); ok {
			text = rawStringValue(block["text"])
		}
	}
	reasoning := firstNonEmptyRaw(rawStringValue(delta["thinking"]), rawStringValue(delta["reasoning"]))
	if reasoning != "" {
		result = append(result, aiProviderStreamEvent{Kind: "reasoning", Delta: reasoning, ProviderRequestID: requestID})
	}
	if text != "" {
		result = append(result, aiProviderStreamEvent{Kind: "text", Delta: text, ProviderRequestID: requestID})
	}
	usageSource, _ := raw["usage"].(map[string]any)
	if len(usageSource) == 0 {
		usageSource, _ = message["usage"].(map[string]any)
	}
	usage := chatUsage{
		InputTokens:       uintFromAny(usageSource["input_tokens"]),
		OutputTokens:      uintFromAny(usageSource["output_tokens"]),
		CachedInputTokens: uintFromAny(usageSource["cache_read_input_tokens"]),
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	if usage.TotalTokens > 0 {
		result = append(result, aiProviderStreamEvent{Kind: "usage", Usage: usage, ProviderRequestID: requestID})
	}
	finishReason := firstString(delta["stop_reason"], raw["stop_reason"])
	if finishReason != "" {
		result = append(result, aiProviderStreamEvent{Kind: "completed", FinishReason: finishReason, ProviderRequestID: requestID})
	}
	return result
}

func googleStreamEvents(raw map[string]any) []aiProviderStreamEvent {
	result := make([]aiProviderStreamEvent, 0, 6)
	requestID := stringValue(raw["responseId"])
	candidates, _ := raw["candidates"].([]any)
	finishReason := ""
	if len(candidates) > 0 {
		candidate, _ := candidates[0].(map[string]any)
		finishReason = stringValue(candidate["finishReason"])
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		toolCalls := make([]aiProviderToolCall, 0)
		for index, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if functionCall, ok := part["functionCall"].(map[string]any); ok {
				arguments, _ := functionCall["args"].(map[string]any)
				encoded, err := json.Marshal(arguments)
				if err == nil {
					id := stringValue(functionCall["id"])
					name := stringValue(functionCall["name"])
					if id == "" {
						id = "call_" + aiWorkspaceBytesHash([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", requestID, index, name, encoded)))[:24]
					}
					call := aiProviderToolCall{ID: id, Name: name, Arguments: encoded}
					if validAIProviderToolCall(call) {
						toolCalls = append(toolCalls, call)
					}
				}
			}
			text := rawStringValue(part["text"])
			if text == "" {
				continue
			}
			kind := "text"
			if thought, _ := part["thought"].(bool); thought {
				kind = "reasoning"
			}
			result = append(result, aiProviderStreamEvent{Kind: kind, Delta: text, ProviderRequestID: requestID})
		}
		if len(toolCalls) > 0 {
			result = append(result, aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: toolCalls, ProviderRequestID: requestID})
		}
	}
	usageSource, _ := raw["usageMetadata"].(map[string]any)
	usage := chatUsage{
		InputTokens:       uintFromAny(usageSource["promptTokenCount"]),
		OutputTokens:      uintFromAny(usageSource["candidatesTokenCount"]),
		ReasoningTokens:   uintFromAny(usageSource["thoughtsTokenCount"]),
		CachedInputTokens: uintFromAny(usageSource["cachedContentTokenCount"]),
		TotalTokens:       uintFromAny(usageSource["totalTokenCount"]),
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	if usage.TotalTokens > 0 {
		result = append(result, aiProviderStreamEvent{Kind: "usage", Usage: usage, ProviderRequestID: requestID})
	}
	if finishReason != "" {
		result = append(result, aiProviderStreamEvent{Kind: "completed", FinishReason: finishReason, ProviderRequestID: requestID})
	}
	return result
}

func consumeNDJSONProviderEvents(ctx context.Context, source io.Reader, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil {
		return errAIProvider
	}
	scanner := bufio.NewScanner(io.LimitReader(source, maximumAIResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), maximumAIStreamRecordBytes)
	seenOutput, seenText, textBytes, reasoningBytes := false, false, 0, 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if json.Unmarshal([]byte(line), &raw) != nil {
			return errAIProvider
		}
		if aiProviderEventContainsError(raw) {
			return errAIProvider
		}
		message, _ := raw["message"].(map[string]any)
		if calls, err := ollamaToolCalls(message["tool_calls"]); err != nil {
			return err
		} else if len(calls) > 0 {
			seenOutput = true
			if err := onEvent(aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: calls}); err != nil {
				return err
			}
		}
		for _, event := range []aiProviderStreamEvent{
			{Kind: "reasoning", Delta: firstNonEmptyRaw(rawStringValue(message["thinking"]), rawStringValue(raw["thinking"]))},
			{Kind: "text", Delta: firstNonEmptyRaw(rawStringValue(message["content"]), rawStringValue(raw["response"]))},
		} {
			if event.Delta == "" {
				continue
			}
			if !utf8.ValidString(event.Delta) {
				return errAIProvider
			}
			if event.Kind == "text" {
				textBytes += len(event.Delta)
				seenText, seenOutput = true, true
				if textBytes > maximumAssistantBytes {
					return errAIProvider
				}
			} else {
				reasoningBytes += len(event.Delta)
				if reasoningBytes > maximumAssistantBytes {
					return errAIProvider
				}
			}
			if err := onEvent(event); err != nil {
				return err
			}
		}
		if done, _ := raw["done"].(bool); done {
			usage := chatUsage{InputTokens: uintFromAny(raw["prompt_eval_count"]), OutputTokens: uintFromAny(raw["eval_count"])}
			usage.TotalTokens = usage.InputTokens + usage.OutputTokens
			if usage.TotalTokens > 0 {
				if err := onEvent(aiProviderStreamEvent{Kind: "usage", Usage: usage}); err != nil {
					return err
				}
			}
			if err := onEvent(aiProviderStreamEvent{Kind: "completed", FinishReason: firstString(raw["done_reason"], "stop")}); err != nil {
				return err
			}
			_ = seenText
			return streamCompletionResult(seenOutput, textBytes, true)
		}
	}
	if scanner.Err() != nil {
		return aiProviderStreamScanError(ctx, scanner.Err())
	}
	_ = seenText
	return streamCompletionResult(seenOutput, textBytes, false)
}

func ollamaToolCalls(raw any) ([]aiProviderToolCall, error) {
	values, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return nil, nil
		}
		return nil, errAIProvider
	}
	result := make([]aiProviderToolCall, 0, len(values))
	for index, value := range values {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, errAIProvider
		}
		function, _ := item["function"].(map[string]any)
		name := stringValue(function["name"])
		var arguments []byte
		switch value := function["arguments"].(type) {
		case map[string]any:
			arguments, _ = json.Marshal(value)
		case string:
			arguments = []byte(value)
		case nil:
			arguments = []byte("{}")
		}
		id := stringValue(item["id"])
		if id == "" {
			id = "call_" + aiWorkspaceBytesHash([]byte(fmt.Sprintf("%d\x00%s\x00%s", index, name, arguments)))[:24]
		}
		call := aiProviderToolCall{ID: id, Name: name, Arguments: arguments}
		if !validAIProviderToolCall(call) {
			return nil, errAIProvider
		}
		result = append(result, call)
	}
	return result, nil
}

func uintFromAny(raw any) uint64 {
	switch value := raw.(type) {
	case float64:
		if value >= 0 && value == float64(uint64(value)) {
			return uint64(value)
		}
	case string:
		parsed, _ := strconv.ParseUint(value, 10, 64)
		return parsed
	}
	return 0
}

func consumeNDJSONText(ctx context.Context, source io.Reader, onChunk func(string) error) error {
	scanner := bufio.NewScanner(io.LimitReader(source, maximumAIResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), maximumAIStreamRecordBytes)
	seen, total := false, 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			return errAIProvider
		}
		chunk := rawStringValue(event["response"])
		if message, ok := event["message"].(map[string]any); ok {
			chunk = rawStringValue(message["content"])
		}
		if chunk != "" {
			if !utf8.ValidString(chunk) || total+len(chunk) > maximumAssistantBytes {
				return errAIProvider
			}
			total += len(chunk)
			seen = true
			if err := onChunk(chunk); err != nil {
				return err
			}
		}
		if done, _ := event["done"].(bool); done {
			return streamCompletionResult(seen, total, true)
		}
	}
	if scanner.Err() != nil {
		return aiProviderStreamScanError(ctx, scanner.Err())
	}
	return streamCompletionResult(seen, total, false)
}

func googleText(event map[string]any) string {
	candidates, ok := event["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return ""
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var builder strings.Builder
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		builder.WriteString(rawStringValue(part["text"]))
	}
	return builder.String()
}

func collectAIStream(stream func(func(string) error) error) (string, error) {
	var builder strings.Builder
	err := stream(func(chunk string) error {
		if builder.Len()+len(chunk) > maximumAssistantBytes {
			return errAIProvider
		}
		builder.WriteString(chunk)
		return nil
	})
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(builder.String())
	if value == "" {
		return "", errAIProvider
	}
	return value, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func rawStringValue(value any) string {
	text, _ := value.(string)
	return text
}

func firstString(values ...any) string {
	for _, value := range values {
		if text := stringValue(value); text != "" {
			return text
		}
	}
	return ""
}

func firstUint(values ...any) *uint64 {
	for _, raw := range values {
		var value uint64
		switch number := raw.(type) {
		case float64:
			if number > 0 && number == float64(uint64(number)) {
				value = uint64(number)
			}
		case string:
			value, _ = strconv.ParseUint(number, 10, 64)
		}
		if value > 0 {
			return &value
		}
	}
	return nil
}

func redactedAIProviderError(provider string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: %w", provider, errAIProvider)
}
