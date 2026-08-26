package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

type recordingAIModelProvider struct {
	discoveries atomic.Int32
}

func TestDefaultAIConfigUsesFiveMinuteRequestTimeout(t *testing.T) {
	if got := defaultAIConfigSettings("default").RequestTimeoutSeconds; got != 300 {
		t.Fatalf("default request timeout = %d, want 300", got)
	}
}

func TestAIConfigStorageAllowsUltraReasoningEffort(t *testing.T) {
	config := defaultAIConfigSettings("ultra")
	config.Name = "Ultra"
	config.Provider = "openai-compatible"
	config.BaseURL = "https://api.example.test/v1"
	config.Model = "gpt-test"
	config.ReasoningEffort = "ultra"
	if err := validateAIConfigForStorage(config, false); err != nil {
		t.Fatalf("ultra reasoning effort rejected: %v", err)
	}
}

func TestAIConfigUpdatePersistsExplicitPrivateHTTPBaseURL(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}
	created := dispatchJSON(t, dispatch, "ai.config.update", `{
		"id":"lan","expectedRevision":0,"name":"LAN Provider","provider":"openai-compatible",
		"baseUrl":"http://192.168.10.7:60632/v1","model":"gpt-5.6-sol","enabled":true
	}`)
	if created["baseUrl"] != "http://192.168.10.7:60632/v1" || created["revision"] != float64(1) {
		t.Fatalf("created LAN config = %#v", created)
	}
	if err := state.close(); err != nil {
		t.Fatal(err)
	}

	state, err = loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	stored, found := state.AIConfigs["lan"]
	if !found || stored.BaseURL != "http://192.168.10.7:60632/v1" || stored.Model != "gpt-5.6-sol" {
		t.Fatalf("reloaded LAN config = %+v, found=%v", stored.view(), found)
	}
}

func TestAIConfigUpdateReportsRejectedBaseURLAsInvalidArgument(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}
	response := dispatchEnvelope(t, dispatch, "ai.config.update", `{
		"id":"invalid-url","expectedRevision":0,"name":"Invalid URL","provider":"openai-compatible",
		"baseUrl":"http://provider.example.test/v1","model":"model","enabled":true
	}`)
	rpcError := response.GetError()
	if rpcError == nil || rpcError.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT || rpcError.GetRetryable() {
		t.Fatalf("rejected Base URL RPC error = %+v", rpcError)
	}
}

func TestAIConfigReasoningEffortCatalogIsDeviceOwnedAndModelBound(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	config := defaultAIConfigSettings("reasoning")
	config.Name, config.Provider, config.BaseURL, config.Model = "Reasoning", "openai", "https://api.example.test/v1", "gpt-5.6"
	config.Enabled, config.Credential, config.CredentialConfigured, config.Revision = true, "device-only", true, 7
	state.AIConfigs[config.ID] = config
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}

	result := dispatchJSON(t, dispatch, "ai.config.reasoning-efforts", `{"id":"reasoning","model":"gpt-5.6"}`)
	if result["configId"] != "reasoning" || result["model"] != "gpt-5.6" {
		t.Fatalf("reasoning catalog binding = %#v", result)
	}
	efforts, ok := result["items"].([]any)
	if !ok || len(efforts) != 9 || efforts[0] != "automatic" || efforts[len(efforts)-1] != "ultra" {
		t.Fatalf("reasoning catalog = %#v", result["items"])
	}
}

func (*recordingAIModelProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (*recordingAIModelProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "unused", nil
}

func (provider *recordingAIModelProvider) DiscoverModels(_ context.Context, config aiConfig) ([]aiModelDescriptor, error) {
	call := provider.discoveries.Add(1)
	input, output := uint64(120000), uint64(16000)
	return []aiModelDescriptor{{
		ID: fmt.Sprintf("%s-r%d-call%d", config.Model, config.Revision, call), DisplayName: "Mock model",
		Capabilities: map[string]bool{"streaming": true, "reasoning": true}, MaxInputTokens: &input, MaxOutputTokens: &output,
	}}, nil
}

func TestAIConfigRichSettingsPersistOutsideIdentityAndSecretStoreIsExplicit(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}
	const secret = "phase5a-private-credential-marker"
	const systemPrompt = "phase5a-system-prompt-marker"
	created := dispatchJSON(t, dispatch, "ai.config.update", `{
		"id":"rich","expectedRevision":0,"name":"Phase5A Rich Config","provider":"anthropic",
		"baseUrl":"https://api.example.test/v1","nonSecretHeaders":{"X-Region":"cn-test"},"model":"claude-test",
		"systemPrompt":"phase5a-system-prompt-marker","temperature":0.35,"reasoningEffort":"max",
		"maxTurnOutputTokens":32000,"maxActiveContextTokens":240000,"maxAgentRounds":72,
		"maxAgentToolCalls":88,"maxAgentNoProgressRounds":9,"requestTimeoutSeconds":180,
		"maxRetries":4,"retryBaseDelayMilliseconds":725,"showUsage":false,"enabled":true,
		"secretAction":"replace","secret":"phase5a-private-credential-marker"
	}`)
	if created["secretConfigured"] != true || created["reasoningEffort"] != "max" || created["showUsage"] != false {
		t.Fatalf("created config = %#v", created)
	}
	if _, exposed := created["secret"]; exposed {
		t.Fatal("config response exposed secret")
	}

	identity, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"aiConfigs"`, "Phase5A Rich Config", systemPrompt, secret, "api.example.test"} {
		if bytes.Contains(identity, []byte(forbidden)) {
			t.Fatalf("identity file contains AI configuration marker %q", forbidden)
		}
	}
	business, err := os.ReadFile(statePath + ".business.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Phase5A Rich Config", systemPrompt, "api.example.test", "claude-test"} {
		if !bytes.Contains(business, []byte(expected)) {
			t.Fatalf("BusinessStore does not contain non-secret marker %q", expected)
		}
	}
	if bytes.Contains(business, []byte(secret)) {
		t.Fatal("BusinessStore contains AI credential")
	}
	storedSecret, found, err := state.secrets.Get(t.Context(), aiCredentialSecretKey("rich"))
	if err != nil || !found || string(storedSecret) != secret {
		t.Fatalf("SecretStore credential found=%v value=%q error=%v", found, storedSecret, err)
	}
	zeroSecret(storedSecret)

	revision := uint64(created["revision"].(float64))
	kept := dispatchJSON(t, dispatch, "ai.config.update", fmt.Sprintf(`{
		"id":"rich","expectedRevision":%d,"name":"Kept","provider":"anthropic","baseUrl":"https://api.example.test/v1",
		"model":"claude-test","reasoningEffort":"high","enabled":true,"secretAction":"keep"
	}`, revision))
	if state.AIConfigs["rich"].Credential != secret || kept["secretConfigured"] != true {
		t.Fatalf("explicit keep failed: %#v", kept)
	}
	revision = uint64(kept["revision"].(float64))
	replaced := dispatchJSON(t, dispatch, "ai.config.update", fmt.Sprintf(`{
		"id":"rich","expectedRevision":%d,"name":"Replaced","provider":"anthropic","baseUrl":"https://api.example.test/v1",
		"model":"claude-test","enabled":true,"secretAction":"replace","secret":"replacement-private-marker"
	}`, revision))
	if state.AIConfigs["rich"].Credential != "replacement-private-marker" {
		t.Fatalf("explicit replace failed: %#v", replaced)
	}
	revision = uint64(replaced["revision"].(float64))
	cleared := dispatchJSON(t, dispatch, "ai.config.update", fmt.Sprintf(`{
		"id":"rich","expectedRevision":%d,"name":"Cleared","provider":"anthropic","baseUrl":"https://api.example.test/v1",
		"model":"claude-test","enabled":true,"secretAction":"clear"
	}`, revision))
	if cleared["secretConfigured"] != false || state.AIConfigs["rich"].Credential != "" {
		t.Fatalf("explicit clear failed: %#v", cleared)
	}

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	config := reloaded.AIConfigs["rich"]
	if config.Name != "Cleared" || config.Provider != "anthropic" || config.Model != "claude-test" ||
		config.CredentialConfigured || config.Credential != "" || config.ReasoningEffort != "automatic" {
		t.Fatalf("reloaded config = %+v", config.view())
	}
}

func TestAIModelDiscoveryCacheIsRevisionBound(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingAIModelProvider{}
	now := time.Now().UTC()
	dispatch := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.ai.config", ai: provider}
	const createPayload = `{
		"id":"cache","expectedRevision":0,"name":"Cache","provider":"openai-compatible",
		"baseUrl":"https://api.example.test/v1","model":"model-a","enabled":true
	}`
	var createInput rpcInput
	if err := json.Unmarshal([]byte(createPayload), &createInput); err != nil {
		t.Fatal(err)
	}
	if config, err := configFromInput(createInput); err != nil {
		t.Fatalf("configFromInput() = %+v, %v", config, err)
	}
	created := dispatchJSON(t, dispatch, "ai.config.update", createPayload)
	first := dispatchJSON(t, dispatch, "ai.config.models", `{"id":"cache"}`)
	second := dispatchJSON(t, dispatch, "ai.config.models", `{"id":"cache"}`)
	if provider.discoveries.Load() != 1 || first["cached"] != false || second["cached"] != true {
		t.Fatalf("cache calls=%d first=%#v second=%#v", provider.discoveries.Load(), first, second)
	}
	firstItems := first["items"].([]any)
	if len(firstItems) != 1 || !strings.Contains(firstItems[0].(map[string]any)["id"].(string), "-r1-call1") {
		t.Fatalf("first model payload = %#v", firstItems)
	}

	revision := uint64(created["revision"].(float64))
	updated := dispatchJSON(t, dispatch, "ai.config.update", fmt.Sprintf(`{
		"id":"cache","expectedRevision":%d,"name":"Cache v2","provider":"openai-compatible",
		"baseUrl":"https://api.example.test/v1","model":"model-b","enabled":true,"secretAction":"keep"
	}`, revision))
	third := dispatchJSON(t, dispatch, "ai.config.models", `{"id":"cache"}`)
	if provider.discoveries.Load() != 2 || third["cached"] != false ||
		third["configRevision"] != updated["revision"] {
		t.Fatalf("revision cache calls=%d updated=%#v third=%#v", provider.discoveries.Load(), updated, third)
	}
	thirdItems := third["items"].([]any)
	if len(thirdItems) != 1 || !strings.Contains(thirdItems[0].(map[string]any)["id"].(string), "model-b-r2-call2") {
		t.Fatalf("revision-bound models = %#v", thirdItems)
	}
}

func TestAIConfigAndModelListsPageByEncodedResponseBudget(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	discoveredAt := time.Now().UTC().Truncate(time.Millisecond)
	for index := 0; index < 8; index++ {
		config := defaultAIConfigSettings(fmt.Sprintf("page-%02d", index))
		config.Name = fmt.Sprintf("Paged config %02d", index)
		config.Provider = "openai-compatible"
		config.BaseURL = "https://api.example.test/v1"
		config.Model = "model"
		config.Enabled = true
		config.SystemPrompt = strings.Repeat("x", 14<<10)
		stored, putErr := state.business.putAIConfig(t.Context(), config, 0)
		if putErr != nil {
			t.Fatal(putErr)
		}
		state.AIConfigs[stored.ID] = stored
	}
	modelConfig := state.AIConfigs["page-00"]
	models := make([]aiModelDescriptor, 0, 500)
	for index := 0; index < 500; index++ {
		models = append(models, aiModelDescriptor{
			ID: fmt.Sprintf("model-%04d", index), DisplayName: fmt.Sprintf("Model %04d %s", index, strings.Repeat("<", 180)),
			Capabilities: map[string]bool{"streaming": true, "tools": index%2 == 0},
		})
	}
	if err := state.business.replaceAIModelDiscovery(t.Context(), modelConfig, models, discoveredAt); err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: func() time.Time { return discoveredAt }, scope: "remote.peer.ai.config"}

	configIDs := collectPagedRPCIDs(t, dispatch, "ai.config.list", "", "id")
	if len(configIDs) != 8 {
		t.Fatalf("config ids=%d, want 8", len(configIDs))
	}
	modelIDs := collectPagedRPCIDs(t, dispatch, "ai.config.models", modelConfig.ID, "id")
	if len(modelIDs) != len(models) {
		t.Fatalf("model ids=%d, want %d", len(modelIDs), len(models))
	}
}

func collectPagedRPCIDs(t *testing.T, dispatch dispatcher, method, configID, idField string) map[string]struct{} {
	t.Helper()
	result := map[string]struct{}{}
	seenCursors := map[string]struct{}{}
	cursor := ""
	for {
		input := map[string]any{"limit": 200}
		if configID != "" {
			input["id"] = configID
		}
		if cursor != "" {
			input["cursor"] = cursor
		}
		encodedInput, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		response := dispatchEnvelope(t, dispatch, method, string(encodedInput))
		if response.GetError() != nil {
			t.Fatalf("%s error = %+v", method, response.GetError())
		}
		if len(response.GetJsonPayload()) > preferredRPCPagePayload {
			t.Fatalf("%s page bytes=%d, preferred=%d", method, len(response.GetJsonPayload()), preferredRPCPagePayload)
		}
		var page map[string]any
		if err := json.Unmarshal(response.GetJsonPayload(), &page); err != nil {
			t.Fatal(err)
		}
		for _, raw := range page["items"].([]any) {
			id := raw.(map[string]any)[idField].(string)
			if _, duplicate := result[id]; duplicate {
				t.Fatalf("%s duplicated %s %q", method, idField, id)
			}
			result[id] = struct{}{}
		}
		next, _ := page["nextCursor"].(string)
		if next == "" {
			return result
		}
		if _, duplicate := seenCursors[next]; duplicate {
			t.Fatalf("%s repeated cursor %q", method, next)
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
}

func TestAIConversationBusinessStoreTurnRoundTrip(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	config := defaultAIConfigSettings("default")
	config.Name, config.Provider, config.BaseURL, config.Model = "Test", "openai-compatible", "https://api.example.test/v1", "model"
	config.Enabled, config.Revision = true, 1
	state.AIConfigs[config.ID] = config
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.chat", ai: staticAIProvider{}}
	createdValue, _, err := dispatch.createAIConversationRPC(t.Context(), stableProjectID(state.DeviceID, ""), rpcInput{"title": "Round trip"})
	if err != nil {
		t.Fatal(err)
	}
	created := createdValue.(conversationView)
	result, revision, err := dispatch.callConversationSend(t.Context(), rpcInput{"conversationId": created.ID, "prompt": "hello"})
	if err != nil {
		t.Fatalf("callConversationSend() = %#v, revision=%d, error=%T %v", result, revision, err, err)
	}
	if result.(map[string]any)["accepted"] != true || revision <= created.Revision {
		t.Fatalf("send result=%#v revision=%d", result, revision)
	}
}
