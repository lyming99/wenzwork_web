package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maximumAIResponseBytes = 1 << 20
	// The full-access workspace surface occupies 32 definitions before
	// project task tools are added. Keep a bounded margin for those built-ins
	// and small MCP catalogs while retaining the aggregate schema byte limit.
	maximumAIProviderTools = 64
	// Assistant text is durable data, not a Peer-frame payload. Keep its
	// single-record safety ceiling aligned with the provider response reader;
	// conversation.message.content serves it in small UTF-8-safe chunks.
	// In particular, do not reuse the 48 KiB event/RPC page budget here.
	maximumAssistantBytes = maximumAIResponseBytes
	maximumAIHistoryBytes = 128 << 10
)

var (
	errAIProvider                = errors.New("AI provider is unavailable")
	errAIProviderStreamTruncated = fmt.Errorf("AI provider stream ended before its terminal marker: %w", errAIProvider)
	errAIProviderRequestTimeout  = fmt.Errorf("AI provider request timed out: %w", errAIProvider)
)

type aiProvider interface {
	Test(context.Context, aiConfig) (time.Duration, error)
	Complete(context.Context, aiConfig, []chatMessage, string) (string, error)
}

// streamingAIProvider is optional so tested/custom providers can retain the
// small completion interface while OpenAI-compatible providers stream tokens.
type streamingAIProvider interface {
	CompleteStream(context.Context, aiConfig, []chatMessage, string, func(string) error) error
}

type aiProviderStreamEvent struct {
	Kind              string
	Delta             string
	Usage             chatUsage
	ToolCalls         []aiProviderToolCall
	ProviderRequestID string
	FinishReason      string
}

type eventStreamingAIProvider interface {
	CompleteEventStream(context.Context, aiConfig, []chatMessage, string, func(aiProviderStreamEvent) error) error
}

type aiPromptImage struct {
	Name       string
	MimeType   string
	Base64Data string
}

type aiProviderPrompt struct {
	Text          string
	Images        []aiPromptImage
	Tools         []aiWorkspaceToolDefinition
	ToolExchanges []aiProviderToolExchange
}

type aiProviderToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type aiProviderToolResult struct {
	ToolCallID string         `json:"toolCallId"`
	Name       string         `json:"name"`
	Content    string         `json:"content"`
	IsError    bool           `json:"isError"`
	Untrusted  bool           `json:"untrusted,omitempty"`
	Image      *aiPromptImage `json:"image,omitempty"`
}

type aiProviderToolExchange struct {
	AssistantText      string                 `json:"assistantText,omitempty"`
	AssistantReasoning string                 `json:"assistantReasoning,omitempty"`
	Calls              []aiProviderToolCall   `json:"calls"`
	Results            []aiProviderToolResult `json:"results"`
}

type promptEventStreamingAIProvider interface {
	CompletePromptEventStream(context.Context, aiConfig, []chatMessage, aiProviderPrompt, func(aiProviderStreamEvent) error) error
}

type openAICompatibleProvider struct{}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func validateAIConfig(config aiConfig) error {
	config = effectiveAIConfig(config)
	if config.Provider == "" || strings.TrimSpace(config.Model) == "" || len(config.Model) > 120 ||
		validateNonSecretAIHeaders(config.NonSecretHeaders) != nil ||
		(config.Provider != "ollama" && config.Provider != "openai-compatible" && strings.TrimSpace(config.Credential) == "") {
		return errRPCInvalid
	}
	_, _, err := parseAIBaseURL(config.BaseURL)
	if err != nil {
		return errRPCInvalid
	}
	return nil
}

func (openAICompatibleProvider) Test(ctx context.Context, config aiConfig) (time.Duration, error) {
	started := time.Now()
	_, err := (openAICompatibleProvider{}).DiscoverModels(ctx, config)
	return time.Since(started), err
}

func (openAICompatibleProvider) Complete(ctx context.Context, config aiConfig, history []chatMessage, prompt string) (string, error) {
	messages := boundedOpenAIMessages(config, history, prompt)
	config = effectiveAIConfig(config)
	body := map[string]any{
		"model": config.Model, "messages": messages, "stream": false,
		"temperature": config.Temperature, "max_tokens": config.MaxTurnOutputTokens,
	}
	for name, value := range aiOpenAIReasoningFields(config) {
		body[name] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", errAIProvider
	}
	contents, err := doAIProviderJSON(ctx, effectiveAIConfig(config), http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return "", err
	}
	var response struct {
		Choices []struct {
			Message openAIMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(contents, &response) != nil || len(response.Choices) == 0 {
		return "", errAIProvider
	}
	content := strings.TrimSpace(response.Choices[0].Message.Content)
	if content == "" || !utf8.ValidString(content) || len(content) > maximumAssistantBytes {
		return "", errAIProvider
	}
	return content, nil
}

func (openAICompatibleProvider) CompleteStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onChunk func(string) error) error {
	if onChunk == nil {
		return errAIProvider
	}
	return (openAICompatibleProvider{}).CompleteEventStream(ctx, config, history, prompt, func(event aiProviderStreamEvent) error {
		if event.Kind == "text" && event.Delta != "" {
			return onChunk(event.Delta)
		}
		return nil
	})
}

func (openAICompatibleProvider) CompleteEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt string, onEvent func(aiProviderStreamEvent) error) error {
	return (openAICompatibleProvider{}).CompletePromptEventStream(ctx, config, history, aiProviderPrompt{Text: prompt}, onEvent)
}

func (openAICompatibleProvider) CompletePromptEventStream(ctx context.Context, config aiConfig, history []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil {
		return errAIProvider
	}
	if err := validateAIProviderPrompt(prompt); err != nil {
		return err
	}
	config = effectiveAIConfig(config)
	body := map[string]any{
		"model": config.Model, "messages": openAIMessagesForPrompt(config, history, prompt), "stream": true,
		"temperature": config.Temperature, "max_tokens": config.MaxTurnOutputTokens,
	}
	if len(prompt.Tools) > 0 {
		body["tools"] = openAIToolDefinitions(prompt.Tools)
		body["tool_choice"] = "auto"
	}
	for name, value := range aiOpenAIReasoningFields(config) {
		body[name] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return errAIProvider
	}
	response, err := openAIProviderResponse(ctx, config, http.MethodPost, "/chat/completions", payload, "text/event-stream", true)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return consumeOpenAIEventStream(ctx, response.Body, onEvent)
}

func validateAIProviderPrompt(prompt aiProviderPrompt) error {
	if strings.TrimSpace(prompt.Text) == "" && len(prompt.Images) == 0 || len(prompt.Text) > maximumAIHistoryBytes || len(prompt.Images) > 8 ||
		len(prompt.Tools) > maximumAIProviderTools || len(prompt.ToolExchanges) > 200 {
		return errRPCInvalid
	}
	total := 0
	for _, image := range prompt.Images {
		if strings.TrimSpace(image.Name) == "" || len(image.Name) > 255 ||
			!slices.Contains([]string{"image/png", "image/jpeg", "image/webp", "image/gif"}, image.MimeType) || image.Base64Data == "" {
			return errRPCInvalid
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(image.Base64Data)
		if err != nil || len(decoded) == 0 || len(decoded) > 10<<20 {
			return errRPCInvalid
		}
		total += len(decoded)
		if total > 32<<20 {
			return errRPCInvalid
		}
	}
	toolBytes := 0
	seenNames := make(map[string]struct{}, len(prompt.Tools))
	for _, tool := range prompt.Tools {
		if !validAIProviderToolName(tool.Name) || strings.TrimSpace(tool.Description) == "" || len(tool.Description) > 2048 || tool.InputSchema == nil {
			return errRPCInvalid
		}
		if _, duplicate := seenNames[tool.Name]; duplicate {
			return errRPCInvalid
		}
		seenNames[tool.Name] = struct{}{}
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil || len(encoded) > 16<<10 {
			return errRPCInvalid
		}
		toolBytes += len(encoded) + len(tool.Description)
	}
	for _, exchange := range prompt.ToolExchanges {
		if len(exchange.Calls) == 0 || len(exchange.Calls) != len(exchange.Results) ||
			len(exchange.AssistantText) > maximumAssistantBytes || len(exchange.AssistantReasoning) > maximumAssistantBytes {
			return errRPCInvalid
		}
		for index, call := range exchange.Calls {
			result := exchange.Results[index]
			if !validAIProviderToolCall(call) || result.ToolCallID != call.ID || result.Name != call.Name ||
				!utf8.ValidString(result.Content) || len(result.Content) > maximumAssistantBytes {
				return errRPCInvalid
			}
			if result.Image != nil {
				if err := validateAIPromptImage(*result.Image); err != nil {
					return err
				}
			}
			toolBytes += len(call.Arguments) + len(result.Content)
			if toolBytes > 512<<10 {
				return errRPCInvalid
			}
		}
	}
	return nil
}

func validateAIPromptImage(image aiPromptImage) error {
	if strings.TrimSpace(image.Name) == "" || len(image.Name) > 255 ||
		!slices.Contains([]string{"image/png", "image/jpeg", "image/webp", "image/gif"}, image.MimeType) || image.Base64Data == "" {
		return errRPCInvalid
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(image.Base64Data)
	if err != nil || len(decoded) == 0 || len(decoded) > 10<<20 {
		return errRPCInvalid
	}
	return nil
}

func openAIMessagesForPrompt(config aiConfig, history []chatMessage, prompt aiProviderPrompt) any {
	messages := boundedOpenAIMessages(config, history, prompt.Text)
	result := make([]map[string]any, 0, len(messages))
	for index, message := range messages {
		content := any(message.Content)
		if index == len(messages)-1 && len(prompt.Images) > 0 {
			parts := make([]map[string]any, 0, len(prompt.Images)+1)
			if message.Content != "" {
				parts = append(parts, map[string]any{"type": "text", "text": message.Content})
			}
			for _, image := range prompt.Images {
				parts = append(parts, map[string]any{
					"type": "image_url", "image_url": map[string]any{"url": "data:" + image.MimeType + ";base64," + image.Base64Data},
				})
			}
			content = parts
		}
		result = append(result, map[string]any{"role": message.Role, "content": content})
	}
	for _, exchange := range prompt.ToolExchanges {
		toolCalls := make([]map[string]any, 0, len(exchange.Calls))
		for _, call := range exchange.Calls {
			toolCalls = append(toolCalls, map[string]any{
				"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": string(call.Arguments)},
			})
		}
		result = append(result, map[string]any{"role": "assistant", "content": exchange.AssistantText, "tool_calls": toolCalls})
		for _, toolResult := range exchange.Results {
			content := any(toolResult.Content)
			if toolResult.Untrusted {
				content = []map[string]any{{"type": "text", "text": aiProviderUntrustedContent(toolResult.Content)}}
			} else if toolResult.Image != nil {
				parts := []map[string]any{{"type": "text", "text": toolResult.Content}}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:" + toolResult.Image.MimeType + ";base64," + toolResult.Image.Base64Data,
				}})
				content = parts
			}
			result = append(result, map[string]any{"role": "tool", "tool_call_id": toolResult.ToolCallID, "content": content})
		}
	}
	return result
}

func openAIToolDefinitions(definitions []aiWorkspaceToolDefinition) []map[string]any {
	result := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, map[string]any{"type": "function", "function": map[string]any{
			"name": definition.Name, "description": definition.Description, "parameters": definition.InputSchema,
		}})
	}
	return result
}

func validAIProviderToolCall(call aiProviderToolCall) bool {
	if call.ID == "" || len(call.ID) > 256 || !utf8.ValidString(call.ID) || !validAIProviderToolName(call.Name) ||
		len(call.Arguments) == 0 || len(call.Arguments) > maximumRPCPayload || !json.Valid(call.Arguments) {
		return false
	}
	var arguments map[string]any
	return json.Unmarshal(call.Arguments, &arguments) == nil && arguments != nil
}

func validAIProviderToolName(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character != '_' && character != '-' && (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func boundedOpenAIMessages(config aiConfig, history []chatMessage, prompt string) []openAIMessage {
	messages := boundedAIMessages(history, prompt)
	if strings.TrimSpace(config.SystemPrompt) == "" {
		return messages
	}
	result := make([]openAIMessage, 0, len(messages)+1)
	result = append(result, openAIMessage{Role: "system", Content: config.SystemPrompt})
	return append(result, messages...)
}

func consumeOpenAIStream(ctx context.Context, source io.Reader, onChunk func(string) error) error {
	return consumeOpenAIEventStream(ctx, source, func(event aiProviderStreamEvent) error {
		if event.Kind == "text" && event.Delta != "" {
			return onChunk(event.Delta)
		}
		return nil
	})
}

func consumeOpenAIEventStream(ctx context.Context, source io.Reader, onEvent func(aiProviderStreamEvent) error) error {
	if onEvent == nil {
		return errAIProvider
	}
	scanner := bufio.NewScanner(io.LimitReader(source, maximumAIResponseBytes+1))
	scanner.Buffer(make([]byte, 4096), maximumAIStreamRecordBytes)
	scanner.Split(splitAIProviderSSERecords)
	seenOutput, seenText, textBytes, reasoningBytes := false, false, 0, 0
	protocolTerminal := false
	toolCalls := make(map[int]*aiOpenAIToolCallAccumulator)
	toolsEmitted := false
	emitTools := func(requestID string) error {
		if toolsEmitted || len(toolCalls) == 0 {
			return nil
		}
		completed, err := completeOpenAIToolCalls(toolCalls, requestID)
		if err != nil {
			return err
		}
		toolsEmitted, seenOutput = true, true
		return onEvent(aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: completed, ProviderRequestID: requestID})
	}
	requestID := ""
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, present := aiProviderSSEData(scanner.Text())
		if !present {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			protocolTerminal = true
			if err := emitTools(requestID); err != nil {
				return err
			}
			return streamCompletionResult(seenOutput, textBytes, protocolTerminal)
		}
		var raw struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				PromptTokens        uint64 `json:"prompt_tokens"`
				CompletionTokens    uint64 `json:"completion_tokens"`
				TotalTokens         uint64 `json:"total_tokens"`
				PromptTokensDetails struct {
					CachedTokens uint64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CompletionTokensDetails struct {
					ReasoningTokens uint64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &raw) != nil {
			return errAIProvider
		}
		if len(raw.Error) > 0 && string(raw.Error) != "null" || strings.EqualFold(strings.TrimSpace(raw.Type), "error") {
			return errAIProvider
		}
		if raw.ID != "" {
			requestID = raw.ID
		}
		if raw.Usage.PromptTokens > 0 || raw.Usage.CompletionTokens > 0 || raw.Usage.TotalTokens > 0 {
			usage := chatUsage{
				InputTokens: raw.Usage.PromptTokens, OutputTokens: raw.Usage.CompletionTokens,
				ReasoningTokens:   raw.Usage.CompletionTokensDetails.ReasoningTokens,
				CachedInputTokens: raw.Usage.PromptTokensDetails.CachedTokens, TotalTokens: raw.Usage.TotalTokens,
			}
			if usage.TotalTokens == 0 {
				usage.TotalTokens = usage.InputTokens + usage.OutputTokens
			}
			if err := onEvent(aiProviderStreamEvent{Kind: "usage", Usage: usage, ProviderRequestID: raw.ID}); err != nil {
				return err
			}
		}
		if len(raw.Choices) == 0 {
			continue
		}
		choice := raw.Choices[0]
		for _, delta := range choice.Delta.ToolCalls {
			accumulator := toolCalls[delta.Index]
			if accumulator == nil {
				accumulator = &aiOpenAIToolCallAccumulator{Index: delta.Index}
				toolCalls[delta.Index] = accumulator
			}
			if delta.ID != "" {
				if accumulator.ID != "" && accumulator.ID != delta.ID {
					return errAIProvider
				}
				accumulator.ID = delta.ID
			}
			accumulator.Name += delta.Function.Name
			accumulator.Arguments += delta.Function.Arguments
			if len(accumulator.ID) > 256 || len(accumulator.Name) > 64 || len(accumulator.Arguments) > maximumRPCPayload ||
				!utf8.ValidString(accumulator.ID) || !utf8.ValidString(accumulator.Name) || !utf8.ValidString(accumulator.Arguments) {
				return errAIProvider
			}
		}
		for _, event := range []aiProviderStreamEvent{
			{Kind: "reasoning", Delta: firstNonEmptyRaw(choice.Delta.ReasoningContent, choice.Delta.Reasoning), ProviderRequestID: raw.ID},
			{Kind: "text", Delta: choice.Delta.Content, ProviderRequestID: raw.ID},
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
		if choice.FinishReason != "" {
			protocolTerminal = true
			if err := emitTools(raw.ID); err != nil {
				return err
			}
			if err := onEvent(aiProviderStreamEvent{Kind: "completed", ProviderRequestID: raw.ID, FinishReason: choice.FinishReason}); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return aiProviderStreamScanError(ctx, err)
	}
	_ = seenText
	return streamCompletionResult(seenOutput, textBytes, protocolTerminal)
}

type aiOpenAIToolCallAccumulator struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

func completeOpenAIToolCalls(pending map[int]*aiOpenAIToolCallAccumulator, requestID string) ([]aiProviderToolCall, error) {
	indexes := make([]int, 0, len(pending))
	for index := range pending {
		indexes = append(indexes, index)
	}
	slices.Sort(indexes)
	result := make([]aiProviderToolCall, 0, len(indexes))
	for _, index := range indexes {
		value := pending[index]
		arguments := strings.TrimSpace(value.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		id := value.ID
		if id == "" {
			id = "call_" + aiWorkspaceBytesHash([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", requestID, index, value.Name, arguments)))[:24]
		}
		call := aiProviderToolCall{ID: id, Name: value.Name, Arguments: json.RawMessage(arguments)}
		if !validAIProviderToolCall(call) {
			return nil, errAIProvider
		}
		result = append(result, call)
	}
	if len(result) == 0 {
		return nil, errAIProvider
	}
	return result, nil
}

func firstNonEmptyRaw(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func streamCompletionResult(seen bool, total int, protocolTerminal bool) error {
	_ = total
	if !protocolTerminal {
		return errAIProviderStreamTruncated
	}
	if !seen {
		return errAIProvider
	}
	return nil
}

func boundedAIMessages(history []chatMessage, prompt string) []openAIMessage {
	remaining := maximumAIHistoryBytes - len(prompt)
	if remaining < 0 {
		remaining = 0
	}
	start := len(history)
	for start > 0 && len(history)-start < 50 {
		candidate := history[start-1]
		if candidate.Role != "user" && candidate.Role != "assistant" {
			start--
			continue
		}
		if len(candidate.Content) > remaining {
			break
		}
		remaining -= len(candidate.Content)
		start--
	}
	messages := make([]openAIMessage, 0, len(history)-start+1)
	for _, message := range history[start:] {
		if (message.Role == "user" || message.Role == "assistant") && message.Content != "" {
			messages = append(messages, openAIMessage{Role: message.Role, Content: message.Content})
		}
	}
	return append(messages, openAIMessage{Role: "user", Content: prompt})
}

func newAIRequestTarget(baseURL, suffix string) (*url.URL, *http.Client, error) {
	parsed, allowConfiguredLocal, err := parseAIBaseURL(baseURL)
	if err != nil {
		return nil, nil, err
	}
	relative, err := url.Parse(suffix)
	if err != nil || relative.IsAbs() || relative.Host != "" || !strings.HasPrefix(relative.Path, "/") || relative.Fragment != "" {
		return nil, nil, errAIProvider
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + relative.Path
	parsed.RawQuery = relative.RawQuery
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
	}
	transport.DialContext = restrictedAIDialer(parsed.Hostname(), allowConfiguredLocal)
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return parsed, client, nil
}

func newAIStreamingRequestTarget(baseURL, suffix string) (*url.URL, *http.Client, error) {
	endpoint, client, err := newAIRequestTarget(baseURL, suffix)
	if err != nil {
		return nil, nil, err
	}
	// The encrypted peer request carries the authoritative deadline. A fixed
	// client timeout would truncate otherwise healthy token streams.
	client.Timeout = 0
	return endpoint, client, nil
}

func parseAIBaseURL(value string) (*url.URL, bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed == nil || parsed.IsAbs() == false || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Opaque != "" {
		return nil, false, errAIProvider
	}
	hostname := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(hostname)
	// Local AI gateways commonly expose OpenAI-compatible HTTP endpoints on a
	// LAN. Permit that only when the configuration names the private address
	// literally (or uses localhost). A public hostname never gains this flag,
	// so DNS rebinding to a private address remains blocked by the dialer.
	allowConfiguredLocal := hostname == "localhost" || (ip != nil && localAIProviderIP(ip))
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && allowConfiguredLocal) {
		return nil, false, errAIProvider
	}
	if strings.Contains(hostname, "%") {
		return nil, false, errAIProvider
	}
	return parsed, allowConfiguredLocal, nil
}

func restrictedAIDialer(expectedHost string, allowConfiguredLocal bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(expectedHost, ".")) {
			return nil, errAIProvider
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, errAIProvider
		}
		for _, address := range addresses {
			ip := address.IP
			if allowedAIProviderIP(ip, allowConfiguredLocal) {
				connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return connection, nil
				}
			}
		}
		return nil, errAIProvider
	}
}

func allowedAIProviderIP(ip net.IP, allowConfiguredLocal bool) bool {
	if allowConfiguredLocal {
		return localAIProviderIP(ip)
	}
	return safeAIProviderIP(ip)
}

func localAIProviderIP(ip net.IP) bool {
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback()) && !ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

func safeAIProviderIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

func doAIRequest(ctx context.Context, client *http.Client, method string, endpoint *url.URL, credential string, payload []byte) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, errAIProvider
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "wenzwork-device-agent/1")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := client.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, errAIProvider
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		contents, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, aiHTTPError{
			contextOverflow: isAIContextOverflowResponse(response.StatusCode, contents),
			statusCode:      response.StatusCode,
		}
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximumAIResponseBytes+1))
	if err != nil || len(contents) == 0 || len(contents) > maximumAIResponseBytes {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, errAIProvider
	}
	return contents, nil
}

func selectAIConfig(state *agentState, requestedModel string) (aiConfig, error) {
	if state == nil {
		return aiConfig{}, errRPCNotFound
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return selectAIConfigLocked(state, requestedModel)
}

func selectAIConfigLocked(state *agentState, requestedModel string) (aiConfig, error) {
	requestedModel = strings.TrimSpace(requestedModel)
	var selected *aiConfig
	for _, candidate := range state.AIConfigs {
		if !candidate.Enabled || validateAIConfig(candidate) != nil {
			continue
		}
		if requestedModel != "" && candidate.Model != requestedModel && candidate.ID != requestedModel {
			continue
		}
		copy := candidate
		if selected == nil || copy.ID == "default" || (selected.ID != "default" && copy.ID < selected.ID) {
			selected = &copy
		}
	}
	if selected == nil {
		return aiConfig{}, errRPCNotFound
	}
	return *selected, nil
}

func milliseconds(duration time.Duration) int64 {
	value := duration.Milliseconds()
	if value < 0 {
		return 0
	}
	return value
}

func providerFor(dispatch dispatcher) aiProvider {
	if dispatch.ai != nil {
		return dispatch.ai
	}
	return defaultAIProviderRouter
}

func wrapAIError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errAIProviderStreamTruncated) || errors.Is(err, errAIProviderRequestTimeout) {
		return err
	}
	return fmt.Errorf("%w", errAIProvider)
}
