package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAIWorkspaceReadImageReturnsImageBlock(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, "readOnly")
	root := fixture.project.LocalPath
	pngBytes := []byte("\x89PNG\r\n\x1a\nfake-image-body")
	if err := os.WriteFile(filepath.Join(root, "diagram.png"), pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("not an image\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := planAIWorkspaceTool(t, fixture, "read_image", map[string]any{"path": "diagram.png"})
	if plan.RequiresApproval || plan.Preview.Risk != "readOnly" {
		t.Fatalf("image plan = %+v", plan)
	}
	result := fixture.executor.Execute(t.Context(), fixture.context, plan, false)
	if result.IsError || result.Image == nil || result.Image.MimeType != "image/png" || result.Image.Name != "diagram.png" {
		t.Fatalf("image result = %+v", result)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(result.Image.Base64Data)
	if err != nil || string(decoded) != string(pngBytes) {
		t.Fatalf("image base64 mismatch: %v", err)
	}
	hash, _ := result.Metadata["content_hash"].(string)
	if len(hash) != 64 || !strings.Contains(result.Content, hash) || strings.Contains(result.Content, result.Image.Base64Data) {
		t.Fatalf("image content = %q", result.Content)
	}
	// Wrong extension is rejected with a stable error code.
	badPlan := planAIWorkspaceTool(t, fixture, "read_image", map[string]any{"path": "notes.txt"})
	bad := fixture.executor.Execute(t.Context(), fixture.context, badPlan, false)
	if !bad.IsError || bad.Metadata["error_code"] != "image_unsupported" {
		t.Fatalf("unsupported image result = %+v", bad)
	}
	// Sensitive files stay unreadable.
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sensitive := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "read_image", map[string]any{"path": ".env"}), false)
	if !sensitive.IsError || sensitive.Metadata["error_code"] != "forbidden" {
		t.Fatalf("sensitive image result = %+v", sensitive)
	}
	// readOnly mode advertises the tool.
	names := make([]string, 0, 8)
	for _, definition := range aiWorkspaceToolDefinitions(aiWorkspaceModeReadOnly) {
		names = append(names, definition.Name)
	}
	if !slices.Contains(names, "read_image") {
		t.Fatalf("readOnly definitions missing read_image: %v", names)
	}
}

func TestAIWorkspaceReadImageGatedByModelCapabilities(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, "readOnly", &scriptedConversationToolProvider{})
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}
	withImages, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{Provider: "openai", Model: "gpt-4o"})
	if err != nil || withImages == nil {
		t.Fatalf("runtime=%+v error=%v", withImages, err)
	}
	names := make([]string, 0, len(withImages.definitions))
	for _, definition := range withImages.definitions {
		names = append(names, definition.Name)
	}
	if !slices.Contains(names, "read_image") {
		t.Fatalf("imageInput model must see read_image: %v", names)
	}
	withoutImages, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{Provider: "openai-compatible", Model: "model-test"})
	if err != nil || withoutImages == nil {
		t.Fatalf("runtime=%+v error=%v", withoutImages, err)
	}
	for _, definition := range withoutImages.definitions {
		if definition.Name == "read_image" {
			t.Fatal("text-only model must not see read_image")
		}
	}
}

func TestAIProviderToolResultImagesTranslatePerProvider(t *testing.T) {
	image := &aiPromptImage{Name: "diagram.png", MimeType: "image/png", Base64Data: base64.StdEncoding.EncodeToString([]byte("png-body"))}
	prompt := aiProviderPrompt{
		Text: "inspect",
		ToolExchanges: []aiProviderToolExchange{{
			Calls:   []aiProviderToolCall{{ID: "call-1", Name: "read_image", Arguments: json.RawMessage(`{"path":"diagram.png"}`)}},
			Results: []aiProviderToolResult{{ToolCallID: "call-1", Name: "read_image", Content: `{"image":"diagram.png","mime_type":"image/png"}`, Image: image}},
		}},
	}
	if err := validateAIProviderPrompt(prompt); err != nil {
		t.Fatalf("prompt validation: %v", err)
	}

	openAI := openAIMessagesForPrompt(aiConfig{}, nil, prompt)
	openAIList, _ := openAI.([]map[string]any)
	toolMessage := openAIList[len(openAIList)-1]
	parts, _ := toolMessage["content"].([]map[string]any)
	if len(parts) != 2 || parts[0]["type"] != "text" {
		t.Fatalf("openai tool message = %+v", toolMessage)
	}
	imageURL, _ := parts[1]["image_url"].(map[string]any)
	if url, _ := imageURL["url"].(string); !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("openai image part = %+v", parts[1])
	}

	anthropic := anthropicMessagesForPrompt(nil, prompt)
	userBlocks, _ := anthropic[len(anthropic)-1]["content"].([]map[string]any)
	if len(userBlocks) != 1 || userBlocks[0]["type"] != "tool_result" {
		t.Fatalf("anthropic result blocks = %+v", userBlocks)
	}
	toolBlocks, _ := userBlocks[0]["content"].([]map[string]any)
	if len(toolBlocks) != 2 || toolBlocks[1]["type"] != "image" {
		t.Fatalf("anthropic tool result content = %+v", toolBlocks)
	}

	google := googleMessagesForPrompt(nil, prompt)
	partsList, _ := google[len(google)-1]["parts"].([]map[string]any)
	if len(partsList) != 2 || partsList[0]["functionResponse"] == nil || partsList[1]["inlineData"] == nil {
		t.Fatalf("google response parts = %+v", partsList)
	}
}

func TestAIConversationToolLoopReadImageFlowsToPrompt(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "image-call-1", Name: "read_image", Arguments: json.RawMessage(`{"path":"diagram.png"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "图片已读取。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	fixture.state.mu.Lock()
	config := fixture.state.AIConfigs["default"]
	config.Provider, config.Credential = "openai", "test-credential"
	fixture.state.AIConfigs["default"] = config
	fixture.state.mu.Unlock()
	pngBytes := []byte("\x89PNG\r\n\x1a\nloop-image")
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "diagram.png"), pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "读取图片",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	result := prompts[1].ToolExchanges[0].Results[0]
	if result.IsError || result.Image == nil || result.Image.MimeType != "image/png" || result.Image.Name != "diagram.png" {
		t.Fatalf("loop image result = %+v", result)
	}
	if decoded, err := base64.StdEncoding.Strict().DecodeString(result.Image.Base64Data); err != nil || string(decoded) != string(pngBytes) {
		t.Fatalf("loop image payload mismatch: %v", err)
	}
	if strings.Contains(result.Content, result.Image.Base64Data) {
		t.Fatal("tool result JSON must not embed the base64 payload")
	}
	// The persisted tool run stays bounded (no base64 in the stored output).
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || len(page.Items[1].ToolRuns) != 1 {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
	if strings.Contains(page.Items[1].ToolRuns[0].Output, "base64") || len(page.Items[1].ToolRuns[0].Output) > 4096 {
		t.Fatalf("persisted tool run = %+v", page.Items[1].ToolRuns[0])
	}
}
