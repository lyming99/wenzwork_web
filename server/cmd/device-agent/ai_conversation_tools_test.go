package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type scriptedConversationToolProvider struct {
	mu      sync.Mutex
	calls   int
	prompts []aiProviderPrompt
	step    func(int, aiProviderPrompt, func(aiProviderStreamEvent) error) error
}

func (*scriptedConversationToolProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (*scriptedConversationToolProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProvider
}

func (provider *scriptedConversationToolProvider) CompletePromptEventStream(
	_ context.Context,
	_ aiConfig,
	_ []chatMessage,
	prompt aiProviderPrompt,
	onEvent func(aiProviderStreamEvent) error,
) error {
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return err
	}
	var snapshot aiProviderPrompt
	if json.Unmarshal(encoded, &snapshot) != nil {
		return errAIProvider
	}
	provider.mu.Lock()
	index := provider.calls
	provider.calls++
	provider.prompts = append(provider.prompts, snapshot)
	step := provider.step
	provider.mu.Unlock()
	if step == nil {
		return errAIProvider
	}
	return step(index, snapshot, onEvent)
}

func (provider *scriptedConversationToolProvider) snapshot() (int, []aiProviderPrompt) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls, append([]aiProviderPrompt(nil), provider.prompts...)
}

type aiConversationToolTestFixture struct {
	state        *agentState
	project      registeredProject
	conversation conversationView
	dispatch     dispatcher
}

func newAIConversationToolTestFixture(t *testing.T, mode string, provider aiProvider) aiConversationToolTestFixture {
	return newAIConversationToolTestFixtureWithConfig(t, mode, provider, nil)
}

func newAIConversationToolTestFixtureWithConfig(
	t *testing.T,
	mode string,
	provider aiProvider,
	configure func(*aiConfig),
) aiConversationToolTestFixture {
	t.Helper()
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	config := installTestAIConfig(state)
	if configure != nil {
		configure(&config)
		state.mu.Lock()
		state.AIConfigs[config.ID] = config
		state.mu.Unlock()
	}
	projectID := stableProjectID(state.DeviceID, "")
	project, err := state.business.projectByID(t.Context(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	policy := project.Policy
	policy.AllowAIWorkspaceTools = true
	// Workspace-tool fixtures opt into task management explicitly in the
	// task-tool tests so existing tool-count assertions remain focused.
	policy.AllowTaskExecution = false
	project, err = state.business.updateProject(t.Context(), projectID, nil, nil, &policy, &project.Revision)
	if err != nil {
		t.Fatal(err)
	}
	created, err := state.business.createAIConversation(t.Context(), projectID, "", "Tool loop", mode, config, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return aiConversationToolTestFixture{
		state: state, project: project, conversation: created,
		dispatch: dispatcher{
			state: state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.ai.tools",
			ai: provider, requestProjectID: projectID.String(),
		},
	}
}

func emitProviderEvents(onEvent func(aiProviderStreamEvent) error, events ...aiProviderStreamEvent) error {
	for _, event := range events {
		if err := onEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func TestAIConversationToolRuntimeHidesPersistentToolsWithoutPTYRuntime(t *testing.T) {
	t.Cleanup(setInteractiveTerminalRuntimeProbe(func() bool { return false }))
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeFullAccess, &scriptedConversationToolProvider{})

	runtime, err := fixture.dispatch.conversationToolRuntime(
		t.Context(),
		fixture.project.ID,
		aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()},
		aiConfig{},
	)
	if err != nil || runtime == nil {
		t.Fatalf("tool runtime=%+v error=%v", runtime, err)
	}
	names := make([]string, 0, len(runtime.definitions))
	for _, definition := range runtime.definitions {
		names = append(names, definition.Name)
	}
	if slices.Contains(names, "terminal_open") || !slices.Contains(names, "run_command") {
		t.Fatalf("runtime tools without PTY=%v", names)
	}
}

func TestAIConversationToolRuntimeScopesWebSearchToVerifiedProviderBackend(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "must-not-enable-search-for-other-providers")
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}

	for _, test := range []struct {
		provider string
		baseURL  string
		model    string
		want     bool
	}{
		{provider: "deepseek", model: "deepseek-chat", want: true},
		{provider: "openai", model: "gpt-5", want: true},
		{provider: "openai-compatible", baseURL: "https://api.openai.com/v1", model: "gpt-5", want: true},
		{provider: "openai-compatible", model: "compatible-model", want: false},
		{provider: "openai-compatible", baseURL: "https://compatible.example.org/v1", model: "compatible-model", want: false},
		{provider: "anthropic", model: "claude-sonnet-4", want: false},
	} {
		t.Run(test.provider+"/"+test.baseURL, func(t *testing.T) {
			runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{
				Provider: test.provider, BaseURL: test.baseURL, Model: test.model,
			})
			if err != nil || runtime == nil {
				t.Fatalf("runtime=%+v error=%v", runtime, err)
			}
			names := make([]string, 0, len(runtime.definitions))
			for _, definition := range runtime.definitions {
				names = append(names, definition.Name)
			}
			if slices.Contains(names, "web_search") != test.want || !slices.Contains(names, "web_fetch") {
				t.Fatalf("provider %q tools = %v", test.provider, names)
			}
			if !test.want {
				_, _, callErr := runtime.startCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
					ID: uuid.NewString(), Name: "web_search", Arguments: json.RawMessage(`{"query":"must stay disabled"}`),
				}, 0)
				if !errors.Is(callErr, errRPCInvalid) {
					t.Fatalf("provider %q hidden web_search error = %v", test.provider, callErr)
				}
			}
		})
	}
}

func TestAIConversationWorkspaceToolIntentMatchesAuthorizedChannel(t *testing.T) {
	t.Run("tools-request-on-chat-scope-is-rejected", func(t *testing.T) {
		provider := &scriptedConversationToolProvider{step: func(_ int, _ aiProviderPrompt, _ func(aiProviderStreamEvent) error) error {
			t.Fatal("provider must not run after a scope mismatch")
			return nil
		}}
		fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
		fixture.dispatch.scope = "remote.peer.ai.chat"
		_, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
			"conversationId":       fixture.conversation.ID,
			"messageId":            uuid.NewString(),
			"prompt":               "inspect",
			"enableWorkspaceTools": true,
		})
		if !errors.Is(err, errRPCAIToolsScopeRequired) {
			t.Fatalf("scope mismatch error = %v", err)
		}
		if calls, _ := provider.snapshot(); calls != 0 {
			t.Fatalf("provider calls = %d", calls)
		}
	})

	for _, test := range []struct {
		name    string
		enabled bool
		want    bool
	}{
		{name: "explicitly-disabled", enabled: false, want: false},
		{name: "explicitly-enabled", enabled: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &scriptedConversationToolProvider{}
			provider.step = func(_ int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
				hasReadFile := false
				for _, definition := range prompt.Tools {
					if definition.Name == "read_file" {
						hasReadFile = true
					}
				}
				if hasReadFile != test.want {
					t.Fatalf("read_file exposed=%v want=%v tools=%+v", hasReadFile, test.want, prompt.Tools)
				}
				return emitProviderEvents(onEvent,
					aiProviderStreamEvent{Kind: "text", Delta: "done"},
					aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
				)
			}
			fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
			if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
				"conversationId":       fixture.conversation.ID,
				"messageId":            uuid.NewString(),
				"prompt":               "inspect",
				"enableWorkspaceTools": test.enabled,
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAIAgentInboxPreservesWorkspaceToolIntentAcrossDriverScopes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	queuedToolsSeen := false
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if index == 0 {
			close(started)
			<-release
		} else {
			hasReadFile := false
			for _, definition := range prompt.Tools {
				if definition.Name == "read_file" {
					hasReadFile = true
				}
			}
			queuedToolsSeen = hasReadFile
		}
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: fmt.Sprintf("round-%d", index+1)},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	chatDispatch := fixture.dispatch
	chatDispatch.scope = "remote.peer.ai.chat"
	result := make(chan error, 1)
	go func() {
		_, _, err := chatDispatch.callConversationSend(context.Background(), rpcInput{
			"conversationId":       fixture.conversation.ID,
			"messageId":            uuid.NewString(),
			"prompt":               "first without tools",
			"enableWorkspaceTools": false,
		})
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first provider round did not start")
	}
	queued, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId":       fixture.conversation.ID,
		"messageId":            uuid.NewString(),
		"prompt":               "next with tools",
		"destination":          aiInboxNextStep,
		"enableWorkspaceTools": true,
	})
	if err != nil || queued.(map[string]any)["destination"] != aiInboxNextTurn {
		t.Fatalf("queued=%#v error=%v", queued, err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if calls, prompts := provider.snapshot(); calls != 2 || len(prompts) != 2 || !queuedToolsSeen {
		t.Fatalf("provider calls=%d prompts=%+v", calls, prompts)
	}
}

func TestAIConversationToolRuntimeExposesGoalToolsOnlyToGoalRounds(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}

	runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil || runtime == nil {
		t.Fatalf("ordinary tool runtime=%+v error=%v", runtime, err)
	}
	for _, name := range []string{"get_goal", "create_goal", "update_goal"} {
		if runtime.exposes(name) {
			t.Fatalf("ordinary chat exposed %s", name)
		}
	}

	turn.GoalRound = &aiGoalRoundSource{GoalID: uuid.NewString(), Revision: 1, Round: 1}
	runtime, err = fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil || runtime == nil {
		t.Fatalf("Goal round tool runtime=%+v error=%v", runtime, err)
	}
	if !runtime.exposes("get_goal") || !runtime.exposes("update_goal") || runtime.exposes("create_goal") {
		t.Fatalf("Goal round tool exposure: get=%v create=%v update=%v",
			runtime.exposes("get_goal"), runtime.exposes("create_goal"), runtime.exposes("update_goal"))
	}
}

func TestAIConversationModelCannotCreateGoalWithoutExplicitClientAction(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}
	runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil || runtime == nil {
		t.Fatalf("tool runtime=%+v error=%v", runtime, err)
	}

	result, err := runtime.executeCollaborationCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "create_goal",
	}, map[string]any{"objective": "Audit the project for memory leaks", "max_goal_rounds": float64(7)})
	if err != nil || !result.IsError || result.Metadata["error_code"] != "goal_authority_denied" {
		t.Fatalf("unauthorized create_goal result=%+v error=%v", result, err)
	}
	conversation, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || conversation.Goal != nil || conversation.GoalArmed {
		t.Fatalf("ordinary chat created Goal=%+v armed=%v error=%v", conversation.Goal, conversation.GoalArmed, err)
	}
}

func TestAICollaborationGuidanceKeepsOrdinaryChatOutOfGoalMode(t *testing.T) {
	conversation := conversationView{Goal: &aiGoalSnapshot{
		ID: uuid.NewString(), Revision: 2, Objective: "Finished objective", Phase: "complete",
		RoundsStarted: 1, MaxGoalRounds: 7,
	}}
	ordinary := aiCollaborationSystemGuidance(conversation, nil)
	if !strings.Contains(ordinary, "THIS IS AN ORDINARY CHAT TURN, NOT A GOAL ROUND") ||
		strings.Contains(ordinary, "THIS IS AN ADMITTED GOAL ROUND") || strings.Contains(ordinary, "Stop further work") {
		t.Fatalf("ordinary guidance leaked Goal authority: %q", ordinary)
	}

	round := aiCollaborationSystemGuidance(conversation, &aiGoalRoundSource{
		GoalID: conversation.Goal.ID, Revision: conversation.Goal.Revision, Round: conversation.Goal.RoundsStarted,
	})
	if !strings.Contains(round, "THIS IS AN ADMITTED GOAL ROUND") || !strings.Contains(round, "Stop further work") {
		t.Fatalf("Goal round guidance missing terminal authority: %q", round)
	}
}

func TestAIConversationToolLoopExecutesReadToolAndPersistsEvents(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "read-call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "读取完成。"},
				aiProviderStreamEvent{Kind: "usage", Usage: chatUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "notes.txt"), []byte("tool-visible body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := make([]aiConversationEvent, 0)
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		events = append(events, event)
		return nil
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "读取 notes.txt",
	}); err != nil {
		t.Fatal(err)
	}
	calls, prompts := provider.snapshot()
	if calls != 2 || len(prompts) != 2 || len(prompts[0].Tools) != 20 || len(prompts[0].ToolExchanges) != 0 || len(prompts[1].ToolExchanges) != 1 {
		t.Fatalf("provider calls=%d prompts=%+v", calls, prompts)
	}
	toolNames := make([]string, 0, len(prompts[0].Tools))
	for _, tool := range prompts[0].Tools {
		toolNames = append(toolNames, tool.Name)
	}
	if !slices.Equal(toolNames, []string{"todo_write", "exit_plan_mode", "skill", "ask_user_question", "spawn_agent", "subagent_fork", "list_agents", "send_message", "interrupt_agent", "job_list", "job_output", "job_kill", "list_files", "search_files", "read_file", "read_tool_result", "web_fetch", "write_file", "replace_in_file", "run_command"}) ||
		!strings.Contains(prompts[1].ToolExchanges[0].Results[0].Content, "tool-visible body") {
		t.Fatalf("tool prompt = %+v", prompts)
	}
	wantKinds := []string{"chat.tool.status", "chat.tool.status", "chat.text.delta", "chat.usage", "chat.completed"}
	if len(events) != len(wantKinds) {
		t.Fatalf("events = %+v", events)
	}
	for index, kind := range wantKinds {
		if events[index].Kind != kind || index > 0 && events[index-1].Sequence >= events[index].Sequence {
			t.Fatalf("events = %+v", events)
		}
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
	assistant := page.Items[1]
	if assistant.Content != "读取完成。" || len(assistant.ToolRuns) != 1 || assistant.ToolRuns[0].Status != "succeeded" ||
		!strings.Contains(assistant.ToolRuns[0].Output, "tool-visible body") || assistant.ProviderRun.AttemptCount != 2 || assistant.Usage.TotalTokens != 10 {
		t.Fatalf("assistant = %+v", assistant)
	}
}

func TestAIConversationToolLoopTreatsLegacyRoundAndToolCountsAsAdvisory(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0, 1:
			arguments, _ := json.Marshal(map[string]any{"path": fmt.Sprintf("step-%d.txt", index+1)})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: fmt.Sprintf("advisory-call-%d", index+1), Name: "read_file", Arguments: arguments,
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 2:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "超过旧计数阈值后仍完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	for index := 1; index <= 2; index++ {
		if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, fmt.Sprintf("step-%d.txt", index)), []byte(fmt.Sprintf("evidence-%d\n", index)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.state.mu.Lock()
	config := fixture.state.AIConfigs["default"]
	config.MaxAgentRounds = 1
	config.MaxAgentToolCalls = 1
	fixture.state.AIConfigs["default"] = config
	fixture.state.mu.Unlock()

	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "完成两个读取步骤",
	}); err != nil {
		t.Fatal(err)
	}
	calls, _ := provider.snapshot()
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || calls != 3 || len(page.Items) != 2 || page.Items[1].Status != "complete" ||
		page.Items[1].Content != "超过旧计数阈值后仍完成。" || len(page.Items[1].ToolRuns) != 2 || page.Items[1].ProviderRun.AttemptCount != 3 {
		t.Fatalf("calls=%d messages=%+v error=%v", calls, page.Items, err)
	}
}

func TestAIConversationToolLoopReturnsTerminalOpenFailureToProvider(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			// Missing type makes planning fail before any terminal is opened.
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "terminal-open-invalid", Name: "terminal_open", Arguments: json.RawMessage(`{"name":"shell"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			if len(prompt.ToolExchanges) != 1 || len(prompt.ToolExchanges[0].Results) != 1 || !prompt.ToolExchanges[0].Results[0].IsError {
				t.Fatalf("tool failure was not returned to provider: %+v", prompt.ToolExchanges)
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "工具失败已收到，继续回答。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeFullAccess, provider)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "打开终端",
	}); err != nil {
		t.Fatal(err)
	}
	calls, _ := provider.snapshot()
	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || page.Items[1].Status != "complete" || page.Items[1].Content != "工具失败已收到，继续回答。" || len(page.Items[1].ToolRuns) != 1 || page.Items[1].ToolRuns[0].Status != "failed" {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
}

func TestAIConversationSearchFilesPreservesUTF8AndContinues(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "search-unicode-call", Name: "search_files",
					Arguments: json.RawMessage(`{"query":"任务","file_pattern":"*.md","max_results":20}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			if len(prompt.ToolExchanges) != 1 || len(prompt.ToolExchanges[0].Results) != 1 ||
				!utf8.ValidString(prompt.ToolExchanges[0].Results[0].Content) {
				t.Fatalf("search tool exchange = %+v", prompt.ToolExchanges)
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "搜索完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	// aiWorkspaceShorten historically cut at maximum-1 bytes. This line puts
	// the first byte of a three-byte Han character exactly on that boundary.
	longLine := strings.Repeat("a", 238) + "任务"
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "notes.md"), []byte(longLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "搜索任务",
	}); err != nil {
		t.Fatal(err)
	}
	calls, _ := provider.snapshot()
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || calls != 2 || len(page.Items) != 2 || page.Items[1].Status != "complete" || len(page.Items[1].ToolRuns) != 1 ||
		page.Items[1].ToolRuns[0].Status != "succeeded" || !utf8.ValidString(page.Items[1].ToolRuns[0].Output) {
		t.Fatalf("calls=%d messages=%+v error=%v", calls, page.Items, err)
	}
}

func TestAIConversationToolLoopApprovalAndSessionGrant(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	firstBody, secondBody := "first approved body\n", "second approved body\n"
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{"path": "approved.txt", "content": firstBody, "expected_hash": "absent"})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "write-call-1", Name: "write_file", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			arguments, _ := json.Marshal(map[string]any{
				"path": "approved.txt", "old_text": firstBody, "new_text": secondBody,
				"expected_hash": aiWorkspaceBytesHash([]byte(firstBody)),
			})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "write-call-2", Name: "replace_in_file", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 2:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "两次修改均已完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "workspaceWrite", provider)
	events := make([]aiConversationEvent, 0)
	approvals := make([]aiApprovalRequest, 0)
	var approvalErr error
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		events = append(events, event)
		if event.Kind != "chat.approval.requested" {
			return nil
		}
		encoded, err := json.Marshal(event.Payload["approval"])
		if err != nil {
			return err
		}
		var request aiApprovalRequest
		if err := json.Unmarshal(encoded, &request); err != nil {
			return err
		}
		approvals = append(approvals, request)
		_, _, approvalErr = fixture.dispatch.respondAIConversationApprovalRPC(t.Context(), fixture.project.ID, rpcInput{
			"approvalId": request.ID, "conversationId": request.ConversationID, "generationId": request.GenerationID,
			"toolCallId": request.ToolCallID, "decision": "allowForSession",
		})
		return approvalErr
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "写入并替换 approved.txt",
	}); err != nil || approvalErr != nil {
		t.Fatalf("send=%v approval=%v", err, approvalErr)
	}
	contents, err := os.ReadFile(filepath.Join(fixture.project.LocalPath, "approved.txt"))
	if err != nil || string(contents) != secondBody {
		t.Fatalf("contents=%q error=%v", contents, err)
	}
	if len(approvals) != 1 || !approvals[0].AllowForSession || approvals[0].ToolName != "write_file" {
		t.Fatalf("approvals = %+v", approvals)
	}
	approvalEvents := 0
	for _, event := range events {
		if event.Kind == "chat.approval.requested" {
			approvalEvents++
		}
	}
	if approvalEvents != 1 {
		t.Fatalf("events = %+v", events)
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || len(page.Items[1].ToolRuns) != 2 {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
	for _, run := range page.Items[1].ToolRuns {
		if run.Status != "succeeded" || run.ContentOffset == nil || *run.ContentOffset != 0 {
			t.Fatalf("tool run = %+v", run)
		}
	}
	if _, _, err := fixture.dispatch.respondAIConversationApprovalRPC(t.Context(), fixture.project.ID, rpcInput{
		"approvalId": approvals[0].ID, "conversationId": approvals[0].ConversationID, "generationId": approvals[0].GenerationID,
		"toolCallId": approvals[0].ToolCallID, "decision": "allowOnce",
	}); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("approval replay = %v", err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var auditCount, sessionGrantCount int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*),COALESCE(SUM(allow_for_session),0) FROM ai_tool_audit WHERE generation_id=?`, approvals[0].GenerationID).Scan(&auditCount, &sessionGrantCount); err != nil || auditCount != 2 || sessionGrantCount != 2 {
		t.Fatalf("audit count=%d grants=%d error=%v", auditCount, sessionGrantCount, err)
	}
}

func TestAIConversationToolLoopContinuesWhenToolExecutionReportsError(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{
				"path": "approval-event-failure.txt", "content": "body\n", "expected_hash": "absent",
			})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "write-call-event-failure", Name: "write_file", Arguments: arguments,
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "工具错误已返回，继续回答。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "workspaceWrite", provider)
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		if event.Kind == "chat.approval.requested" {
			return errors.New("approval event sink failed")
		}
		return nil
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "写入文件",
	}); err != nil {
		t.Fatal(err)
	}
	calls, _ := provider.snapshot()
	if calls != 2 {
		t.Fatalf("provider calls = %d, want 2", calls)
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || page.Items[1].Status != "complete" || page.Items[1].Content != "工具错误已返回，继续回答。" || len(page.Items[1].ToolRuns) != 1 || page.Items[1].ToolRuns[0].Status != "failed" {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
}

func TestAIConversationToolLoopStopsRepeatedNoProgress(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		arguments := json.RawMessage(`{"path":"stable.txt"}`)
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
				ID: "repeat-call-" + string(rune('a'+index)), Name: "read_file", Arguments: arguments,
			}}},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "stable.txt"), []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.state.mu.Lock()
	config := fixture.state.AIConfigs["default"]
	// Legacy clients may still send this field. It must not weaken the runtime
	// invariant or turn a normal second tool step into a terminal failure.
	config.MaxAgentNoProgressRounds = 1
	config.MaxAgentRounds = 5
	fixture.state.AIConfigs["default"] = config
	fixture.state.mu.Unlock()
	events := make([]aiConversationEvent, 0)
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		events = append(events, event)
		return nil
	}
	_, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "重复读取",
	})
	if !errors.Is(err, errRPCBusy) {
		t.Fatalf("send error = %v", err)
	}
	calls, _ := provider.snapshot()
	wantCalls := int(maximumAIAgentNoProgressRounds) + 1
	if calls != wantCalls || len(events) != wantCalls*2+1 || events[len(events)-1].Kind != "chat.failed" {
		t.Fatalf("calls=%d events=%+v", calls, events)
	}
	page, pageErr := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if pageErr != nil || len(page.Items) != 2 || page.Items[1].Status != "failed" || page.Items[1].ErrorCode != "agent_no_progress" || len(page.Items[1].ToolRuns) != wantCalls {
		t.Fatalf("messages=%+v error=%v", page.Items, pageErr)
	}
}

func TestAIConversationPendingApprovalCancellationDeniesAndConverges(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if index != 0 {
			return errAIProvider
		}
		arguments, _ := json.Marshal(map[string]any{
			"path": "must-not-exist.txt", "content": "cancelled private body\n", "expected_hash": "absent",
		})
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "cancel-write-call", Name: "write_file", Arguments: arguments}}},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, "workspaceWrite", provider)
	approvalSeen := make(chan aiApprovalRequest, 1)
	var eventsMu sync.Mutex
	events := make([]aiConversationEvent, 0)
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
		if event.Kind == "chat.approval.requested" {
			encoded, err := json.Marshal(event.Payload["approval"])
			if err != nil {
				return err
			}
			var request aiApprovalRequest
			if err := json.Unmarshal(encoded, &request); err != nil {
				return err
			}
			approvalSeen <- request
		}
		return nil
	}
	sendError := make(chan error, 1)
	go func() {
		_, _, err := fixture.dispatch.callConversationSend(context.Background(), rpcInput{
			"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "等待审批后写入",
		})
		sendError <- err
	}()
	var approval aiApprovalRequest
	select {
	case approval = <-approvalSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("approval event was not emitted")
	}
	result, _, err := fixture.dispatch.cancelAIConversationRPC(t.Context(), fixture.project.ID, rpcInput{
		"conversationId": fixture.conversation.ID, "generationId": approval.GenerationID,
	})
	if err != nil || result.(map[string]any)["cancelled"] != true {
		t.Fatalf("cancel=%#v error=%v", result, err)
	}
	select {
	case err := <-sendError:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("send error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled generation did not stop")
	}
	if _, err := os.Stat(filepath.Join(fixture.project.LocalPath, "must-not-exist.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled write exists: %v", err)
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || page.Items[1].Status != "stopped" || len(page.Items[1].ToolRuns) != 1 ||
		page.Items[1].ToolRuns[0].Status != "cancelled" || page.Items[1].ToolRuns[0].FinishedAt == nil {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
	fixture.state.aiApprovalMu.Lock()
	pending := len(fixture.state.aiApprovals)
	fixture.state.aiApprovalMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending approvals = %d", pending)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var outcome, decision string
	if err := db.QueryRowContext(t.Context(), `SELECT outcome,approval_decision FROM ai_tool_audit WHERE generation_id=?`, approval.GenerationID).Scan(&outcome, &decision); err != nil || outcome != "cancelled" || decision != "cancelled" {
		t.Fatalf("audit outcome=%q decision=%q error=%v", outcome, decision, err)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if !slices.ContainsFunc(events, func(event aiConversationEvent) bool { return event.Kind == "chat.cancelled" }) {
		t.Fatalf("events = %+v", events)
	}
}

func TestAIApprovalRegistryIsBoundOneTimeAndFailClosed(t *testing.T) {
	state := &agentState{
		aiApprovals: make(map[string]*pendingAIApproval), aiSessionGrants: make(map[string]map[string]struct{}),
	}
	projectID, conversationID, generationID, messageID := uuid.New(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	digest := aiWorkspaceBytesHash([]byte("approval scope"))
	request := aiApprovalRequest{
		ID: uuid.NewString(), ConversationID: conversationID, GenerationID: generationID, MessageID: messageID,
		ToolCallID: "tool-call", ToolName: "write_file", ExpiresAt: time.Now().Add(time.Minute),
		Preview: aiWorkspaceApprovalPreview{ArgumentsSHA256: digest},
	}
	pending, err := state.registerAIApproval(projectID, request, digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.resolveAIApproval(uuid.New(), request.ID, conversationID, generationID, request.ToolCallID, "allowOnce"); !errors.Is(err, errRPCRevision) {
		t.Fatalf("cross-project resolution = %v", err)
	}
	if _, err := state.resolveAIApproval(projectID, request.ID, conversationID, generationID, request.ToolCallID, "allowForSession"); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("disallowed session grant = %v", err)
	}
	if _, err := state.resolveAIApproval(projectID, request.ID, conversationID, generationID, request.ToolCallID, "allowOnce"); err != nil {
		t.Fatal(err)
	}
	resolution := state.waitAIApproval(t.Context(), pending)
	if !resolution.Approved || resolution.Decision != "allowOnce" {
		t.Fatalf("resolution = %+v", resolution)
	}
	if _, err := state.resolveAIApproval(projectID, request.ID, conversationID, generationID, request.ToolCallID, "deny"); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("replayed resolution = %v", err)
	}

	cancelRequest := request
	cancelRequest.ID, cancelRequest.ToolCallID = uuid.NewString(), "cancel-call"
	cancelPending, err := state.registerAIApproval(projectID, cancelRequest, digest)
	if err != nil {
		t.Fatal(err)
	}
	cancelContext, cancel := context.WithCancel(context.Background())
	cancel()
	if resolution := state.waitAIApproval(cancelContext, cancelPending); resolution.Decision != "cancelled" {
		t.Fatalf("cancel resolution = %+v", resolution)
	}

	timeoutRequest := request
	timeoutRequest.ID, timeoutRequest.ToolCallID, timeoutRequest.ExpiresAt = uuid.NewString(), "timeout-call", time.Now().Add(20*time.Millisecond)
	timeoutPending, err := state.registerAIApproval(projectID, timeoutRequest, digest)
	if err != nil {
		t.Fatal(err)
	}
	if resolution := state.waitAIApproval(context.Background(), timeoutPending); resolution.Decision != "timeout" {
		t.Fatalf("timeout resolution = %+v", resolution)
	}
	state.aiApprovalMu.Lock()
	remaining := len(state.aiApprovals)
	state.aiApprovalMu.Unlock()
	if remaining != 0 {
		t.Fatalf("pending approvals = %d", remaining)
	}
}
