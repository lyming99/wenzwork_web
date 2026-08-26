package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func installTestAIConfig(state *agentState) aiConfig {
	config := defaultAIConfigSettings("default")
	config.Name, config.Provider, config.BaseURL, config.Model = "Test", "openai-compatible", "https://api.example.test/v1", "model-test"
	config.Enabled, config.Revision = true, 1
	state.mu.Lock()
	state.AIConfigs[config.ID] = config
	state.mu.Unlock()
	return config
}

type richEventAIProvider struct{}

type interleavedAssistantUpdateProvider struct{}

type truncatedEventAIProvider struct{}

func (richEventAIProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (interleavedAssistantUpdateProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (interleavedAssistantUpdateProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProvider
}

func (interleavedAssistantUpdateProvider) CompleteEventStream(_ context.Context, _ aiConfig, _ []chatMessage, _ string, onEvent func(aiProviderStreamEvent) error) error {
	events := []aiProviderStreamEvent{
		{Kind: "reasoning", Delta: "**Inspecting**\n\n"},
		{Kind: "text", Delta: "第一段进度。"},
		{Kind: "reasoning", Delta: "**Continuing**\n\n"},
		{Kind: "text", Delta: "第二段进度。"},
		{Kind: "reasoning", Delta: "**Finishing**\n\n"},
		// A provider-supplied boundary must not be duplicated.
		{Kind: "text", Delta: "\n\n第三段结论。"},
		{Kind: "completed", FinishReason: "stop"},
	}
	for _, event := range events {
		if err := onEvent(event); err != nil {
			return err
		}
	}
	return nil
}

type capturingPromptAIProvider struct {
	prompt aiProviderPrompt
}

func (*capturingPromptAIProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (*capturingPromptAIProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProvider
}

func (provider *capturingPromptAIProvider) CompletePromptEventStream(_ context.Context, _ aiConfig, _ []chatMessage, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
	provider.prompt = prompt
	if err := onEvent(aiProviderStreamEvent{Kind: "text", Delta: "image received"}); err != nil {
		return err
	}
	return onEvent(aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"})
}

func TestAIConversationV2SupportsProjectImageOnlyPrompt(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	created, err := state.business.createAIConversation(t.Context(), projectID, "", "Image only", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	imageBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(workspace, "pixel.png")
	if err := os.WriteFile(imagePath, imageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(imageBytes)
	provider := &capturingPromptAIProvider{}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now.Add(time.Second) }, scope: "remote.peer.ai.chat",
		ai: provider, requestProjectID: projectID.String(),
	}
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": created.ID, "messageId": uuid.NewString(), "prompt": "",
		"attachments": []any{map[string]any{
			"id": uuid.NewString(), "relativePath": "pixel.png", "name": "pixel.png", "mimeType": "image/png",
			"size": float64(len(imageBytes)), "sha256": base64.RawURLEncoding.EncodeToString(digest[:]),
			"revision": float64(workspaceFileRevision("pixel.png", info)),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(provider.prompt.Images) != 1 || provider.prompt.Images[0].Name != "pixel.png" ||
		provider.prompt.Images[0].Base64Data != base64.StdEncoding.EncodeToString(imageBytes) ||
		!strings.Contains(provider.prompt.Text, `content="supplied-separately"`) {
		t.Fatalf("provider prompt = %+v", provider.prompt)
	}
	page, err := state.business.listAIConversationMessages(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || page.Items[0].Content != "" || len(page.Items[0].Attachments) != 1 ||
		page.Items[1].Content != "image received" || page.Items[1].Status != "complete" {
		t.Fatalf("messages=%#v error=%v", page.Items, err)
	}
}

func (richEventAIProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "rich answer", nil
}

func (truncatedEventAIProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (truncatedEventAIProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProviderStreamTruncated
}

func (truncatedEventAIProvider) CompleteEventStream(_ context.Context, _ aiConfig, _ []chatMessage, _ string, onEvent func(aiProviderStreamEvent) error) error {
	if err := onEvent(aiProviderStreamEvent{Kind: "text", Delta: "durable partial"}); err != nil {
		return err
	}
	return errAIProviderStreamTruncated
}

func (richEventAIProvider) CompleteEventStream(_ context.Context, _ aiConfig, _ []chatMessage, _ string, onEvent func(aiProviderStreamEvent) error) error {
	events := []aiProviderStreamEvent{
		{Kind: "reasoning", Delta: "reasoning trace", ProviderRequestID: "provider-request-42"},
		{Kind: "text", Delta: "rich answer", ProviderRequestID: "provider-request-42"},
		{Kind: "usage", Usage: chatUsage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3, CachedInputTokens: 2, TotalTokens: 18}, ProviderRequestID: "provider-request-42"},
		{Kind: "completed", ProviderRequestID: "provider-request-42", FinishReason: "end_turn"},
	}
	for _, event := range events {
		if err := onEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func TestAIConversationV2PersistsRichProviderStream(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	created, err := state.business.createAIConversation(t.Context(), projectID, "", "Rich stream", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now.Add(time.Second) }, scope: "remote.peer.ai.chat",
		ai: richEventAIProvider{}, requestProjectID: projectID.String(),
	}
	var live []aiConversationEvent
	dispatch.chatEvent = func(event aiConversationEvent) error {
		live = append(live, event)
		return nil
	}
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": created.ID, "messageId": uuid.NewString(), "prompt": "explain it",
	}); err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"chat.reasoning.delta", "chat.text.delta", "chat.usage", "chat.completed"}
	if len(live) != len(wantKinds) {
		t.Fatalf("live events = %#v", live)
	}
	for index, kind := range wantKinds {
		if live[index].Kind != kind || index > 0 && live[index-1].Sequence >= live[index].Sequence {
			t.Fatalf("live events = %#v", live)
		}
	}
	page, err := state.business.listAIConversationMessages(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("messages=%#v error=%v", page.Items, err)
	}
	assistant := page.Items[1]
	if assistant.Content != "rich answer" || assistant.Reasoning != "reasoning trace" ||
		assistant.Usage != (chatUsage{InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3, CachedInputTokens: 2, TotalTokens: 18}) ||
		assistant.ProviderRun.ProviderRequestID != "provider-request-42" || assistant.ProviderRun.FinishReason != "end_turn" {
		t.Fatalf("assistant = %+v", assistant)
	}
	replayed, _, reset, more, err := state.business.listAIConversationEvents(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || reset || more || len(replayed) != len(live) {
		t.Fatalf("replayed=%#v reset=%v more=%v error=%v", replayed, reset, more, err)
	}
	for index := range live {
		if replayed[index].EventID != live[index].EventID || replayed[index].Kind != live[index].Kind {
			t.Fatalf("live/replay mismatch live=%#v replayed=%#v", live, replayed)
		}
	}
}

func TestAIConversationV2PersistsPrematureProviderEOFAsFailure(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	created, err := state.business.createAIConversation(t.Context(), projectID, "", "Truncated stream", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now.Add(time.Second) }, scope: "remote.peer.ai.chat",
		ai: truncatedEventAIProvider{}, requestProjectID: projectID.String(),
	}
	var live []aiConversationEvent
	dispatch.chatEvent = func(event aiConversationEvent) error {
		live = append(live, event)
		return nil
	}
	_, _, sendErr := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": created.ID, "messageId": uuid.NewString(), "prompt": "start",
	})
	if !errors.Is(sendErr, errAIProviderStreamTruncated) {
		t.Fatalf("send error = %v", sendErr)
	}
	if len(live) != 2 || live[0].Kind != "chat.text.delta" || live[1].Kind != "chat.failed" ||
		live[1].Payload["errorCode"] != "provider_stream_truncated" {
		t.Fatalf("live events = %#v", live)
	}
	page, err := state.business.listAIConversationMessages(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("messages=%#v error=%v", page.Items, err)
	}
	assistant := page.Items[1]
	if assistant.Content != "durable partial" || assistant.Status != "failed" ||
		assistant.ErrorCode != "provider_stream_truncated" || assistant.ProviderRun.FinishReason != "error" {
		t.Fatalf("assistant = %+v", assistant)
	}
	conversation, err := state.business.getAIConversation(t.Context(), projectID, created.ID)
	if err != nil || conversation.State != "failed" || conversation.GenerationID != "" {
		t.Fatalf("conversation=%+v error=%v", conversation, err)
	}
}

func TestAIConversationV2PreservesInterleavedAssistantUpdateBoundaries(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	created, err := state.business.createAIConversation(t.Context(), projectID, "", "Interleaved updates", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now.Add(time.Second) }, scope: "remote.peer.ai.chat",
		ai: interleavedAssistantUpdateProvider{}, requestProjectID: projectID.String(),
	}
	var liveText strings.Builder
	dispatch.chatEvent = func(event aiConversationEvent) error {
		if event.Kind == "chat.text.delta" {
			liveText.WriteString(event.Payload["delta"].(string))
		}
		return nil
	}
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": created.ID, "messageId": uuid.NewString(), "prompt": "continue",
	}); err != nil {
		t.Fatal(err)
	}

	const want = "第一段进度。\n\n第二段进度。\n\n第三段结论。"
	page, err := state.business.listAIConversationMessages(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("messages=%#v error=%v", page.Items, err)
	}
	if page.Items[1].Content != want || liveText.String() != want {
		t.Fatalf("assistant content=%q live=%q want=%q", page.Items[1].Content, liveText.String(), want)
	}
}

func TestAIConversationV2ProjectIsolationRichLifecycleAndReplay(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	rootProjectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	dispatch := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.chat", ai: staticAIProvider{}, requestProjectID: rootProjectID.String()}
	createdValue, _, err := dispatch.createAIConversationRPC(t.Context(), rootProjectID, rpcInput{
		"title": "Project-bound chat", "configId": config.ID, "workspaceMode": "edit",
	})
	if err != nil {
		t.Fatal(err)
	}
	created := createdValue.(conversationView)
	if created.ProjectID != rootProjectID.String() || created.ConfigID != config.ID || created.WorkspaceMode != "workspaceWrite" || created.ModelBinding.ConfigRevision != 1 {
		t.Fatalf("created conversation = %+v", created)
	}
	stored, err := state.business.getAIConversation(t.Context(), rootProjectID, created.ID)
	if err != nil || stored.WorkspaceMode != aiWorkspaceModeWorkspaceWrite {
		t.Fatalf("legacy edit storage compatibility = %+v, %v", stored, err)
	}

	otherPath := filepath.Join(directory, "other-project")
	if err := os.MkdirAll(otherPath, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := state.business.addProject(t.Context(), otherPath, "Other", "", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.business.getAIConversation(t.Context(), other.ID, created.ID); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("cross-project conversation read = %v", err)
	}

	attachmentPath := filepath.Join(workspace, "context.md")
	attachmentBody := []byte("phase5b attachment body\n")
	if err := os.WriteFile(attachmentPath, attachmentBody, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(attachmentPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(attachmentBody)
	messageID := uuid.NewString()
	input := rpcInput{
		"conversationId": created.ID, "messageId": messageID, "prompt": "Use the attached context.", "workspaceMode": "workspaceWrite",
		"attachments": []any{map[string]any{
			"id": uuid.NewString(), "relativePath": "context.md", "name": "context.md", "mimeType": "text/markdown",
			"size": float64(len(attachmentBody)), "sha256": base64.RawURLEncoding.EncodeToString(digest[:]),
			"revision": float64(workspaceFileRevision("context.md", info)),
		}},
	}
	streamed := make([]aiConversationEvent, 0)
	dispatch.chatEvent = func(event aiConversationEvent) error {
		streamed = append(streamed, event)
		return nil
	}
	result, revision, err := dispatch.callConversationSend(t.Context(), input)
	if err != nil || result.(map[string]any)["accepted"] != true || revision <= created.Revision {
		t.Fatalf("send result=%#v revision=%d error=%v", result, revision, err)
	}
	if len(streamed) != 3 || streamed[0].Kind != "chat.text.delta" || streamed[1].Kind != "chat.usage" || streamed[2].Kind != "chat.completed" ||
		streamed[0].Sequence >= streamed[1].Sequence || streamed[1].Sequence >= streamed[2].Sequence {
		t.Fatalf("stream events = %#v", streamed)
	}

	page, err := state.business.listAIConversationMessages(t.Context(), rootProjectID, created.ID, 0, 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("message page=%#v error=%v", page, err)
	}
	if page.Items[0].ID != messageID || len(page.Items[0].Attachments) != 1 || page.Items[0].Attachments[0].RelativePath != "context.md" {
		t.Fatalf("user message = %+v", page.Items[0])
	}
	assistant := page.Items[1]
	if assistant.Status != "complete" || assistant.ProviderRun.Provider != config.Provider || assistant.ProviderRun.Model != config.Model ||
		assistant.Usage.TotalTokens == 0 || !bytes.Contains([]byte(assistant.Content), attachmentBody) {
		t.Fatalf("assistant message = %+v", assistant)
	}

	replayed, watermark, reset, hasMore, err := state.business.listAIConversationEvents(t.Context(), rootProjectID, created.ID, 0, 20)
	if err != nil || reset || hasMore || len(replayed) != 3 || watermark != replayed[2].Sequence {
		t.Fatalf("event replay=%#v watermark=%d reset=%v more=%v error=%v", replayed, watermark, reset, hasMore, err)
	}
	search, total, err := state.business.searchAIConversations(t.Context(), rootProjectID, "attachment body", 0, 10)
	if err != nil || total != 1 || len(search) != 1 || search[0].Conversation.ID != created.ID {
		t.Fatalf("search=%#v total=%d error=%v", search, total, err)
	}

	current, err := state.business.getAIConversation(t.Context(), rootProjectID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := state.business.renameAIConversation(t.Context(), rootProjectID, created.ID, "Renamed", current.Revision, now.Add(time.Second))
	if err != nil || renamed.Title != "Renamed" {
		t.Fatalf("rename=%+v error=%v", renamed, err)
	}
	if _, err := state.business.renameAIConversation(t.Context(), rootProjectID, created.ID, "Stale", current.Revision, now.Add(2*time.Second)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale rename = %v", err)
	}

	dispatch.now = func() time.Time { return now.Add(3 * time.Second) }
	regenerationRequestID := uuid.NewString()
	regenerated, _, err := dispatch.regenerateAIConversationRPC(t.Context(), rootProjectID, rpcInput{
		"conversationId": created.ID, "messageId": assistant.ID, "regenerationRequestId": regenerationRequestID, "workspaceMode": "readOnly",
	})
	if err != nil || regenerated.(map[string]any)["accepted"] != true {
		t.Fatalf("regenerate=%#v error=%v", regenerated, err)
	}
	page, err = state.business.listAIConversationMessages(t.Context(), rootProjectID, created.ID, 0, 10)
	if err != nil || len(page.Items) != 3 || page.Items[2].Status != "complete" || page.Items[2].Sequence != 3 {
		t.Fatalf("regenerated messages=%#v error=%v", page.Items, err)
	}
	replayedRegeneration, _, err := dispatch.regenerateAIConversationRPC(t.Context(), rootProjectID, rpcInput{
		"conversationId": created.ID, "messageId": assistant.ID, "regenerationRequestId": regenerationRequestID, "workspaceMode": "readOnly",
	})
	if err != nil || replayedRegeneration.(map[string]any)["replayed"] != true || replayedRegeneration.(map[string]any)["generationId"] != regenerationRequestID {
		t.Fatalf("replayed regenerate=%#v error=%v", replayedRegeneration, err)
	}
	page, err = state.business.listAIConversationMessages(t.Context(), rootProjectID, created.ID, 0, 10)
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("replayed regeneration duplicated messages=%#v error=%v", page.Items, err)
	}

	identity, err := os.ReadFile(state.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("Use the attached context."), attachmentBody, []byte(`"conversations"`)} {
		if bytes.Contains(identity, forbidden) {
			t.Fatalf("identity contains conversation marker %q", forbidden)
		}
	}
}

func TestAIConversationV2RestartRecoversStreamingTurn(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	created, err := state.business.createAIConversation(t.Context(), projectID, "", "Recover", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := state.business.beginAIConversationTurn(t.Context(), projectID, created.ID, uuid.NewString(), "persist before provider", "readOnly", nil, config, now.Add(time.Second))
	if err != nil || turn.Conversation.State != "generating" {
		t.Fatalf("begin turn=%+v error=%v", turn, err)
	}
	if _, _, err := state.business.appendAIConversationTextDelta(t.Context(), projectID, created.ID, turn.GenerationID, turn.Assistant.ID, "partial", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reloaded.business.getAIConversation(t.Context(), projectID, created.ID)
	if err != nil || value.State != "idle" || value.GenerationID != "" {
		t.Fatalf("recovered conversation=%+v error=%v", value, err)
	}
	messages, err := reloaded.business.listAIConversationMessages(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || len(messages.Items) != 2 || messages.Items[0].Content != "persist before provider" ||
		messages.Items[1].Content != "partial" || messages.Items[1].Status != "stopped" || messages.Items[1].ErrorCode != "agent_restarted" {
		t.Fatalf("recovered messages=%#v error=%v", messages.Items, err)
	}
	events, _, _, _, err := reloaded.business.listAIConversationEvents(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || len(events) != 2 || events[1].Kind != "chat.cancelled" {
		t.Fatalf("recovered events=%#v error=%v", events, err)
	}
	identity, err := os.ReadFile(statePath)
	if err != nil || bytes.Contains(identity, []byte("persist before provider")) || bytes.Contains(identity, []byte("partial")) {
		t.Fatalf("identity contains recovered prompt: %v", err)
	}
}

func nearLimitInterruptedAIToolRuns(t *testing.T, now time.Time) ([]chatToolRun, []byte) {
	t.Helper()
	const runningStart = 12
	runs := make([]chatToolRun, 20)
	for index := range runs {
		finishedAt := now.Add(time.Duration(index+1) * time.Millisecond)
		runs[index] = chatToolRun{
			ID: uuid.NewString(), Tool: "read_file", Name: "read_file", Status: "succeeded",
			Arguments: map[string]any{"padding": strings.Repeat("a", 48<<10)}, Result: nil,
			StartedAt: now, FinishedAt: &finishedAt,
		}
		if index >= runningStart {
			runs[index].Status = "running"
			runs[index].FinishedAt = nil
		}
		if !validChatToolRun(runs[index]) {
			t.Fatalf("initial tool run %d is invalid", index)
		}
	}

	target := maximumAIMessageToolRunsBytes - 16
	for index := range runs {
		low, high, best := 0, maximumAIWorkspaceToolResult, 0
		for low <= high {
			middle := low + (high-low)/2
			runs[index].Output = strings.Repeat("o", middle)
			encoded, err := json.Marshal(runs)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) <= target {
				best = middle
				low = middle + 1
			} else {
				high = middle - 1
			}
		}
		runs[index].Output = strings.Repeat("o", best)
		if best < maximumAIWorkspaceToolResult {
			break
		}
	}
	encoded, err := json.Marshal(runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > target || target-len(encoded) > 1 {
		t.Fatalf("near-limit tool runs bytes=%d target=%d", len(encoded), target)
	}
	for index, run := range runs {
		if !validChatToolRun(run) {
			t.Fatalf("padded tool run %d is invalid", index)
		}
	}
	return runs, encoded
}

func TestActiveAIConversationToolRunsReserveRestartTerminalization(t *testing.T) {
	now := time.Now().UTC()
	runs, encoded := nearLimitInterruptedAIToolRuns(t, now)
	if len(encoded) > maximumAIMessageToolRunsBytes {
		t.Fatalf("active snapshot bytes=%d", len(encoded))
	}
	if _, err := marshalActiveAIConversationToolRuns(runs); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("active snapshot without recovery headroom error=%v", err)
	}

	terminalized := make([]int, 0)
	for index := range runs {
		if runs[index].Status != "running" {
			continue
		}
		finishedAt := now.Add(time.Second)
		runs[index].Status = "cancelled"
		runs[index].ErrorCode = "cancelled"
		runs[index].FinishedAt = &finishedAt
		terminalized = append(terminalized, index)
	}
	compacted, terminalJSON, err := marshalTerminalAIConversationToolRuns(runs, terminalized)
	if err != nil || len(terminalJSON) > maximumAIMessageToolRunsBytes || len(compacted) != len(runs) {
		t.Fatalf("terminalized bytes=%d runs=%d error=%v", len(terminalJSON), len(compacted), err)
	}
	for index, run := range compacted {
		if !validChatToolRun(run) {
			t.Fatalf("compacted tool run %d is invalid", index)
		}
	}
}

func TestAIConversationV2RestartRecoversNearLimitToolRuns(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC().Add(-time.Minute)
	created, err := state.business.createAIConversation(t.Context(), projectID, "", "Recover near limit", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := state.business.beginAIConversationTurn(
		t.Context(), projectID, created.ID, uuid.NewString(), "persist before restart", "readOnly", nil, config, now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, encoded := nearLimitInterruptedAIToolRuns(t, now.Add(2*time.Second))
	db, err := state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE ai_messages SET tool_runs_json=? WHERE conversation_id=? AND id=?`,
		string(encoded), created.ID, turn.Assistant.ID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.close()
	value, err := reloaded.business.getAIConversation(t.Context(), projectID, created.ID)
	if err != nil || value.State != "idle" || value.GenerationID != "" {
		t.Fatalf("recovered conversation=%+v error=%v", value, err)
	}
	messages, err := reloaded.business.listAIConversationMessages(t.Context(), projectID, created.ID, 0, 10)
	if err != nil || len(messages.Items) != 2 || messages.Items[1].Status != "stopped" ||
		messages.Items[1].ErrorCode != "agent_restarted" || len(messages.Items[1].ToolRuns) != len(runs) {
		t.Fatalf("recovered messages=%#v error=%v", messages.Items, err)
	}
	truncated := false
	for index, run := range messages.Items[1].ToolRuns {
		marker, argumentsTruncated := run.Arguments.(map[string]any)
		argumentsTruncated = argumentsTruncated && marker["truncated"] == true
		if index < 12 {
			if run.Status != "succeeded" {
				t.Fatalf("completed tool run %d status=%q", index, run.Status)
			}
			if argumentsTruncated {
				t.Fatalf("completed tool run %d was compacted before interrupted runs", index)
			}
		} else if run.Status != "cancelled" || run.FinishedAt == nil {
			t.Fatalf("interrupted tool run %d=%+v", index, run)
		}
		if argumentsTruncated {
			truncated = true
		}
		if !validChatToolRun(run) {
			t.Fatalf("recovered tool run %d is invalid", index)
		}
	}
	if !truncated {
		t.Fatal("near-limit recovery did not compact any interrupted tool payload")
	}
	readDB, err := reloaded.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	var storedToolRuns string
	if err := readDB.QueryRowContext(t.Context(), `SELECT tool_runs_json FROM ai_messages WHERE id=?`, turn.Assistant.ID).Scan(&storedToolRuns); err != nil {
		_ = readDB.Close()
		t.Fatal(err)
	}
	_ = readDB.Close()
	if len(storedToolRuns) > maximumAIMessageToolRunsBytes {
		t.Fatalf("stored recovered tool runs bytes=%d", len(storedToolRuns))
	}
	if recovered, err := reloaded.business.recoverInterruptedAIConversations(t.Context(), now.Add(3*time.Second)); err != nil || recovered != 0 {
		t.Fatalf("second recovery=%d error=%v", recovered, err)
	}
}

func TestLegacyAIConversationsMigrateToCompatibilityProject(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	conversationID := uuid.NewString()
	legacy := conversation{
		ID: conversationID, Title: "Legacy conversation", Model: "legacy-model", UpdatedAt: now, State: "idle",
		Messages: []chatMessage{{ID: uuid.NewString(), Sequence: 1, Role: "user", Content: "legacy-private-prompt", Status: "complete", CreatedAt: now}},
	}
	state.LegacyConversations = map[string]conversation{conversationID: legacy}
	state.Conversations = state.LegacyConversations
	if err := state.write(); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(statePath)
	if err != nil || !bytes.Contains(fixture, []byte("legacy-private-prompt")) {
		t.Fatalf("legacy fixture is missing: %v", err)
	}

	migrated, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	projectID := stableProjectID(migrated.DeviceID, "")
	value, err := migrated.business.getAIConversation(t.Context(), projectID, conversationID)
	if err != nil || value.ProjectID != projectID.String() || value.ConfigID != "legacy-unbound" {
		t.Fatalf("migrated conversation=%+v error=%v", value, err)
	}
	messages, err := migrated.business.listAIConversationMessages(t.Context(), projectID, conversationID, 0, 10)
	if err != nil || len(messages.Items) != 1 || messages.Items[0].Content != "legacy-private-prompt" {
		t.Fatalf("migrated messages=%#v error=%v", messages.Items, err)
	}
	identity, err := os.ReadFile(statePath)
	if err != nil || bytes.Contains(identity, []byte("legacy-private-prompt")) || bytes.Contains(identity, []byte(`"conversations"`)) {
		t.Fatalf("identity retained legacy conversation: %v", err)
	}
	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	messages, err = reloaded.business.listAIConversationMessages(t.Context(), projectID, conversationID, 0, 10)
	if err != nil || len(messages.Items) != 1 {
		t.Fatalf("idempotent migration messages=%#v error=%v", messages.Items, err)
	}
}

func TestLegacyAIConversationImportIsAtomicAndRepairsEmptyPredecessor(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC().Truncate(time.Millisecond)
	conversationID := uuid.NewString()
	if _, err := state.business.createAIConversation(t.Context(), projectID, conversationID, "Repair import", "readOnly", config, now); err != nil {
		t.Fatal(err)
	}
	legacy := conversation{
		ID: conversationID, Title: "Repair import", Model: config.Model, UpdatedAt: now.Add(time.Second), State: "idle",
		Messages: []chatMessage{{ID: uuid.NewString(), Sequence: 1, Role: "user", Content: "atomic legacy body", Status: "complete", CreatedAt: now.Add(time.Second)}},
	}
	if err := state.business.migrateLegacyAIConversations(t.Context(), projectID, map[string]conversation{conversationID: legacy}, state.AIConfigs); err != nil {
		t.Fatal(err)
	}
	if err := state.business.migrateLegacyAIConversations(t.Context(), projectID, map[string]conversation{conversationID: legacy}, state.AIConfigs); err != nil {
		t.Fatalf("idempotent import = %v", err)
	}
	page, err := state.business.listAIConversationMessages(t.Context(), projectID, conversationID, 0, 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Content != "atomic legacy body" {
		t.Fatalf("messages=%#v error=%v", page.Items, err)
	}

	invalidID := uuid.NewString()
	invalid := conversation{
		ID: invalidID, Title: "Invalid import", Model: config.Model, UpdatedAt: now, State: "idle",
		Messages: []chatMessage{{ID: uuid.NewString(), Sequence: 1, Role: "user", Content: strings.Repeat("x", maximumAssistantBytes+1), Status: "complete", CreatedAt: now}},
	}
	if err := state.business.migrateLegacyAIConversations(t.Context(), projectID, map[string]conversation{invalidID: invalid}, state.AIConfigs); err == nil {
		t.Fatal("oversized legacy import succeeded")
	}
	if _, err := state.business.getAIConversation(t.Context(), projectID, invalidID); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("failed import left a conversation: %v", err)
	}
}

func TestLegacyAIConversationImportDoesNotOverwriteCollision(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC().Truncate(time.Millisecond)
	id := uuid.NewString()
	if _, err := state.business.createAIConversation(t.Context(), projectID, id, "User conversation", "readOnly", config, now); err != nil {
		t.Fatal(err)
	}
	legacy := conversation{
		ID: id, Title: "Legacy collision", Model: config.Model, UpdatedAt: now, State: "idle",
		Messages: []chatMessage{{ID: uuid.NewString(), Sequence: 1, Role: "user", Content: "must not overwrite", Status: "complete", CreatedAt: now}},
	}
	if err := state.business.migrateLegacyAIConversations(t.Context(), projectID, map[string]conversation{id: legacy}, state.AIConfigs); err == nil {
		t.Fatal("legacy collision was accepted")
	}
	value, err := state.business.getAIConversation(t.Context(), projectID, id)
	if err != nil || value.Title != "User conversation" || value.MessageCount != 0 {
		t.Fatalf("collision target=%+v error=%v", value, err)
	}
}

func TestAIContextSummaryRoundTrip(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	created, err := state.business.createAIConversation(context.Background(), projectID, "", "Summary", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	summary := aiContextSummary{ConversationID: created.ID, ThroughSequence: 3, Content: "bounded context summary", EstimatedTokens: 5, UpdatedAt: now}
	if err := state.business.saveAIContextSummary(t.Context(), projectID, summary); err != nil {
		t.Fatal(err)
	}
	loaded, err := state.business.loadAIContextSummary(t.Context(), projectID, created.ID)
	if err != nil || loaded == nil || loaded.Content != summary.Content || loaded.ThroughSequence != 3 {
		t.Fatalf("loaded summary=%+v error=%v", loaded, err)
	}
}
