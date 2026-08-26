package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAIToolExecutionBudget(t *testing.T) {
	if budget := aiToolExecutionBudget("run_command"); budget != 120*time.Second {
		t.Errorf("run_command budget = %v", budget)
	}
	if budget := aiToolExecutionBudget("terminal_send"); budget != 120*time.Second {
		t.Errorf("terminal_send budget = %v", budget)
	}
	if budget := aiToolExecutionBudget("web_search"); budget != 60*time.Second {
		t.Errorf("web_search budget = %v", budget)
	}
	if budget := aiToolExecutionBudget("web_fetch"); budget != 30*time.Second {
		t.Errorf("web_fetch budget = %v", budget)
	}
	if budget := aiToolExecutionBudget("read_file"); budget != 30*time.Second {
		t.Errorf("read_file budget = %v", budget)
	}
	if budget := aiToolExecutionBudget("todo_write"); budget != 30*time.Second {
		t.Errorf("collaboration fallback budget = %v", budget)
	}
}

func TestAIWorkspaceCommandTimeoutPreservesPartialOutput(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	starter := new(fakeRawStarter)
	fixture.executor.supervisor = newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	fixture.executor.supervisor.memoryPollInterval = time.Hour
	plan := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{"command": "echo partial", "timeout_seconds": float64(30)})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resultChannel := make(chan aiWorkspaceToolResult, 1)
	go func() { resultChannel <- fixture.executor.Execute(ctx, fixture.context, plan, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for starter.latest() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	process := starter.latest()
	if process == nil {
		t.Fatal("supervised process did not start")
	}
	if err := process.emitStdout([]byte("partial output line\r\n")); err != nil {
		t.Fatal(err)
	}
	result := <-resultChannel
	if !result.IsError || result.Metadata["error_code"] != "timeout" {
		t.Fatalf("command timeout result = %+v", result)
	}
	if !strings.Contains(result.Content, "partial output line") || !strings.Contains(result.Summary, "超时") {
		t.Fatalf("partial output lost: %+v", result)
	}
}

func TestAIConversationToolLoopBudgetTimesOutWebSearch(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(func() { close(release); server.Close() })
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("DEEPSEEK_SEARCH_BASE_URL", server.URL)
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "search-call-1", Name: "web_search", Arguments: json.RawMessage(`{"query":"timeout test"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "搜索超时已处理。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixtureWithConfig(t, aiWorkspaceModeFullAccess, provider, func(config *aiConfig) {
		config.Provider, config.Model = "deepseek", "deepseek-chat"
		config.BaseURL = "https://api.deepseek.com/v1"
		config.Credential, config.CredentialConfigured = "test-key", true
	})
	fixture.dispatch.aiToolTimeout = func(string) time.Duration { return 200 * time.Millisecond }
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "搜索超时测试",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	if len(prompts) != 2 || len(prompts[1].ToolExchanges) != 1 {
		t.Fatalf("prompts = %+v", prompts)
	}
	result := prompts[1].ToolExchanges[0].Results[0]
	if !result.IsError || !strings.Contains(result.Content, `"error_code":"timeout"`) || !strings.Contains(result.Content, "工具执行超时并已终止") {
		t.Fatalf("search timeout result = %+v", result)
	}
}

func TestAIToolCallRunsInParallel(t *testing.T) {
	parallel := []string{"list_files", "search_files", "read_file", "web_search", "web_fetch", "terminal_read", "terminal_list", "get_goal"}
	for _, name := range parallel {
		if !aiToolCallRunsInParallel(name) {
			t.Errorf("expected %s to run in parallel", name)
		}
	}
	exclusive := []string{"write_file", "replace_in_file", "rollback_file_change", "run_command",
		"terminal_open", "terminal_send", "terminal_signal", "terminal_close",
		"todo_write", "exit_plan_mode", "spawn_agent", "list_agents", "send_message", "interrupt_agent",
		"create_goal", "update_goal", "unknown_tool"}
	for _, name := range exclusive {
		if aiToolCallRunsInParallel(name) {
			t.Errorf("expected %s to be exclusive", name)
		}
	}
}

func TestAIToolCallRoundGroups(t *testing.T) {
	call := func(name string) aiProviderToolCall {
		return aiProviderToolCall{ID: uuid.NewString(), Name: name}
	}
	cases := []struct {
		name  string
		calls []aiProviderToolCall
		want  [][]string
	}{
		{
			name:  "parallel run then exclusive barrier",
			calls: []aiProviderToolCall{call("read_file"), call("read_file"), call("write_file"), call("read_file")},
			want:  [][]string{{"read_file", "read_file"}, {"write_file"}, {"read_file"}},
		},
		{
			name:  "exclusive calls stand alone",
			calls: []aiProviderToolCall{call("write_file"), call("replace_in_file")},
			want:  [][]string{{"write_file"}, {"replace_in_file"}},
		},
		{
			name:  "single parallel call is its own group",
			calls: []aiProviderToolCall{call("read_file")},
			want:  [][]string{{"read_file"}},
		},
		{
			name:  "mixed collaboration tools stay exclusive",
			calls: []aiProviderToolCall{call("get_goal"), call("todo_write"), call("get_goal")},
			want:  [][]string{{"get_goal"}, {"todo_write"}, {"get_goal"}},
		},
		{
			name:  "empty round",
			calls: nil,
			want:  [][]string{},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			groups := aiToolCallRoundGroups(test.calls)
			var got [][]string
			for _, group := range groups {
				names := make([]string, 0, len(group))
				for _, call := range group {
					names = append(names, call.Name)
				}
				got = append(got, names)
			}
			if len(got) != len(test.want) {
				t.Fatalf("groups=%v want=%v", got, test.want)
			}
			for index := range got {
				if !slices.Equal(got[index], test.want[index]) {
					t.Fatalf("groups=%v want=%v", got, test.want)
				}
			}
		})
	}
}

func TestAIToolRepeatReminderThresholds(t *testing.T) {
	calls := []aiProviderToolCall{{ID: "a", Name: "read_file"}, {ID: "b", Name: "read_file"}}
	if reminder := aiToolRepeatReminder(calls, 2); reminder != "" {
		t.Errorf("round 2 must stay silent, got %q", reminder)
	}
	if reminder := aiToolRepeatReminder(calls, 3); !strings.Contains(reminder, "重复相同的工具调用") {
		t.Errorf("round 3 must be gentle, got %q", reminder)
	}
	if reminder := aiToolRepeatReminder(calls, 5); !strings.Contains(reminder, "tool: read_file") {
		t.Errorf("round 5 must be detailed, got %q", reminder)
	}
	if reminder := aiToolRepeatReminder(calls, 7); reminder != "" {
		t.Errorf("round 7 must stay silent before the hard limit, got %q", reminder)
	}
}

func TestAIConversationToolLoopExecutesParallelReadsInModelOrder(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{
					{ID: "read-a", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)},
					{ID: "read-b", Name: "read_file", Arguments: json.RawMessage(`{"path":"b.txt"}`)},
					{ID: "read-missing", Name: "read_file", Arguments: json.RawMessage(`{"path":"missing.txt"}`)},
				}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "并行读取完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeFullAccess, provider)
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "a.txt"), []byte("alpha body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "b.txt"), []byte("beta body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := make([]aiConversationEvent, 0)
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		events = append(events, event)
		return nil
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "并行读取三个文件",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	if len(prompts) != 2 || len(prompts[1].ToolExchanges) != 1 {
		t.Fatalf("prompts = %+v", prompts)
	}
	exchange := prompts[1].ToolExchanges[0]
	if len(exchange.Calls) != 3 || len(exchange.Results) != 3 {
		t.Fatalf("exchange = %+v", exchange)
	}
	if !strings.Contains(exchange.Results[0].Content, "alpha body") ||
		!strings.Contains(exchange.Results[1].Content, "beta body") || exchange.Results[0].IsError || exchange.Results[1].IsError {
		t.Fatalf("results = %+v", exchange.Results)
	}
	if !exchange.Results[2].IsError || exchange.Results[2].ToolCallID != "read-missing" {
		t.Fatalf("missing result = %+v", exchange.Results[2])
	}
	// The persisted tool_runs array must keep model order: every running record
	// is written before the parallel execution starts.
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || len(page.Items[1].ToolRuns) != 3 {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
	gotIDs := []string{page.Items[1].ToolRuns[0].ID, page.Items[1].ToolRuns[1].ID, page.Items[1].ToolRuns[2].ID}
	if !slices.Equal(gotIDs, []string{"read-a", "read-b", "read-missing"}) {
		t.Fatalf("tool run order = %v", gotIDs)
	}
	if page.Items[1].ToolRuns[2].Status != "failed" || page.Items[1].ToolRuns[0].Status != "succeeded" || page.Items[1].ToolRuns[1].Status != "succeeded" {
		t.Fatalf("tool runs = %+v", page.Items[1].ToolRuns)
	}
	// Six status events: three running first in model order, then three
	// finished. Finish order may vary; sequences must stay strictly increasing.
	var statusRuns []chatToolRun
	statusSequences := make([]uint64, 0, 6)
	for _, event := range events {
		if event.Kind != "chat.tool.status" {
			continue
		}
		encoded, err := json.Marshal(event.Payload["toolRun"])
		if err != nil {
			t.Fatal(err)
		}
		var run chatToolRun
		if json.Unmarshal(encoded, &run) != nil {
			t.Fatal("decode tool run")
		}
		statusRuns = append(statusRuns, run)
		statusSequences = append(statusSequences, event.Sequence)
	}
	if len(statusRuns) != 6 {
		t.Fatalf("status events = %d, events = %+v", len(statusRuns), events)
	}
	var runningIDs []string
	for index, run := range statusRuns {
		if index <= 2 {
			if run.Status != "running" {
				t.Fatalf("expected running start first, status runs = %+v", statusRuns)
			}
			runningIDs = append(runningIDs, run.ID)
		} else if run.Status == "running" {
			t.Fatalf("finished event carried running status, status runs = %+v", statusRuns)
		}
		if index > 0 && statusSequences[index-1] >= statusSequences[index] {
			t.Fatalf("status events out of order = %+v", events)
		}
	}
	if !slices.Equal(runningIDs, []string{"read-a", "read-b", "read-missing"}) {
		t.Fatalf("running order=%v events=%+v", runningIDs, events)
	}
}

func TestAIConversationToolLoopBarrierBetweenWriteAndRead(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			writeArguments, _ := json.Marshal(map[string]any{"path": "c.txt", "content": "barrier body\n"})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{
					{ID: "read-c-first", Name: "read_file", Arguments: json.RawMessage(`{"path":"c.txt"}`)},
					{ID: "write-c", Name: "write_file", Arguments: writeArguments},
					{ID: "read-c-last", Name: "read_file", Arguments: json.RawMessage(`{"path":"c.txt"}`)},
				}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "屏障验证完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeFullAccess, provider)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "先读后写再读",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	results := prompts[1].ToolExchanges[0].Results
	if len(results) != 3 {
		t.Fatalf("results = %+v", results)
	}
	// The exclusive write_file forms a barrier: the read before it must see the
	// file missing, the read after it must see the written content. Running all
	// three concurrently would make this flaky; barriers make it deterministic.
	if !results[0].IsError || results[1].IsError || !strings.Contains(results[2].Content, "barrier body") {
		t.Fatalf("barrier results = %+v", results)
	}
}

func TestAIConversationToolLoopInjectsRepeatReminder(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if index < 6 {
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "repeat-read-" + string(rune('a'+index)), Name: "read_file", Arguments: json.RawMessage(`{"path":"stable.txt"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		}
		if index == 6 {
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "收到提醒，更换策略。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		}
		return errAIProvider
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "stable.txt"), []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "重复读取并观察提醒",
	}); err != nil {
		t.Fatal(err)
	}
	calls, prompts := provider.snapshot()
	if calls != 7 {
		t.Fatalf("provider calls = %d", calls)
	}
	// The exchange fingerprint goes identical from round 2 onward, so the
	// gentle reminder lands in the 5th provider prompt and the detailed one in
	// the 7th (the injection targets the NEXT round's prompt).
	if !strings.Contains(prompts[4].Text, "重复相同的工具调用") {
		t.Fatalf("gentle reminder missing in prompt 5: %q", prompts[4].Text)
	}
	if !strings.Contains(prompts[6].Text, "检测到重复的工具调用") || !strings.Contains(prompts[6].Text, "tool: read_file") {
		t.Fatalf("detailed reminder missing in prompt 7: %q", prompts[6].Text)
	}
	if strings.Contains(prompts[0].Text, "重复相同的工具调用") {
		t.Fatalf("first prompt must not carry a reminder: %q", prompts[0].Text)
	}
	if strings.Contains(prompts[1].Text, "重复相同的工具调用") {
		t.Fatalf("second prompt must not carry a reminder: %q", prompts[1].Text)
	}
}

func TestStartCallRejectsInvalidArguments(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeFullAccess, &scriptedConversationToolProvider{})
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}
	runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil || runtime == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
	if _, _, err := runtime.startCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{ID: "bad", Name: "read_file", Arguments: json.RawMessage(`not json`)}, 0); err == nil {
		t.Fatal("invalid arguments must be rejected")
	}
	if _, _, err := runtime.startCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{ID: "no-id", Name: "read_file", Arguments: json.RawMessage(`{}`)}, 0); err == nil {
		t.Fatal("missing call id must be rejected")
	}
}
