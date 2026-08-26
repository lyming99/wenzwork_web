package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func setAIConversationTaskPolicy(t *testing.T, fixture *aiConversationToolTestFixture, enabled bool) {
	t.Helper()
	policy := fixture.project.Policy
	policy.AllowTaskExecution = enabled
	project, err := fixture.state.business.updateProject(t.Context(), fixture.project.ID, nil, nil, &policy, &fixture.project.Revision)
	if err != nil {
		t.Fatal(err)
	}
	fixture.project = project
}

func enableAIConversationTaskTools(t *testing.T, fixture *aiConversationToolTestFixture) {
	t.Helper()
	setAIConversationTaskPolicy(t, fixture, true)
}

func aiTaskToolNamesFromRuntime(runtime *aiConversationToolRuntime) []string {
	if runtime == nil {
		return nil
	}
	names := make([]string, 0, len(runtime.definitions))
	for _, definition := range runtime.definitions {
		names = append(names, definition.Name)
	}
	return names
}

func TestAIConversationTaskToolsRequireToolScopeAndProjectPolicy(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	setAIConversationTaskPolicy(t, &fixture, false)
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}
	runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if names := aiTaskToolNamesFromRuntime(runtime); slices.Contains(names, "task_list") {
		t.Fatalf("task tools were exposed without task policy: %v", names)
	}

	enableAIConversationTaskTools(t, &fixture)
	runtime, err = fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil {
		t.Fatal(err)
	}
	names := aiTaskToolNamesFromRuntime(runtime)
	for _, expected := range aiTaskToolNames {
		if !slices.Contains(names, expected) {
			t.Fatalf("enabled task tools=%v, missing %s", names, expected)
		}
	}

	chatOnly := fixture.dispatch
	chatOnly.scope = "remote.peer.ai.chat"
	runtime, err = chatOnly.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if names = aiTaskToolNamesFromRuntime(runtime); slices.Contains(names, "task_list") {
		t.Fatalf("task tools were exposed to chat-only scope: %v", names)
	}

	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-ai.taskTools")
	runtime, err = fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if names = aiTaskToolNamesFromRuntime(runtime); slices.Contains(names, "task_list") {
		t.Fatalf("task-tool kill switch was ignored: %v", names)
	}
}

func TestAIProviderPromptAcceptsFullWorkspaceAndTaskToolSurface(t *testing.T) {
	definitions := append(aiCollaborationToolDefinitions(), aiWorkspaceToolDefinitions(aiWorkspaceModeFullAccess)...)
	definitions = append(definitions, aiTaskToolDefinitions()...)
	if len(definitions) <= 32 {
		t.Fatalf("test must cover the expanded tool surface, got %d definitions", len(definitions))
	}
	if len(definitions) > maximumAIProviderTools {
		t.Fatalf("tool surface=%d exceeds provider limit=%d", len(definitions), maximumAIProviderTools)
	}
	if err := validateAIProviderPrompt(aiProviderPrompt{Text: "manage tasks", Tools: definitions}); err != nil {
		t.Fatalf("expanded provider prompt was rejected: %v", err)
	}
}

func TestAITaskActionInputCoversEveryAction(t *testing.T) {
	taskID := uuid.New()
	for _, test := range []struct {
		action string
		method string
		extra  map[string]any
	}{
		{action: "start", method: "task.start"},
		{action: "stop", method: "task.stop"},
		{action: "retry", method: "task.retry"},
		{action: "delete", method: "task.delete"},
		{action: "accept", method: "task.accept", extra: map[string]any{"evidence": " verified output "}},
		{action: "undo_acceptance", method: "task.undo-acceptance"},
		{action: "follow_up", method: "task.follow-up", extra: map[string]any{"feedback": " address review "}},
	} {
		t.Run(test.action, func(t *testing.T) {
			arguments := map[string]any{
				"task_id": taskID.String(), "expected_revision": float64(7), "action": test.action,
			}
			for key, value := range test.extra {
				arguments[key] = value
			}
			method, input, err := aiTaskActionInput(arguments)
			if err != nil || method != test.method || input["expectedRevision"] != float64(7) {
				t.Fatalf("method=%q input=%+v error=%v", method, input, err)
			}
			if test.action == "follow_up" {
				followUpID, ok := input["taskId"].(string)
				if input["sourceTaskId"] != taskID.String() || !ok || uuid.Validate(followUpID) != nil ||
					input["feedback"] != "address review" {
					t.Fatalf("follow-up input=%+v", input)
				}
			} else if input["taskId"] != taskID.String() {
				t.Fatalf("task input=%+v", input)
			}
			if test.action == "accept" && input["evidence"] != "verified output" {
				t.Fatalf("accept input=%+v", input)
			}
		})
	}

	for _, invalid := range []map[string]any{
		{"task_id": taskID.String(), "expected_revision": float64(7), "action": "start", "evidence": "not allowed"},
		{"task_id": taskID.String(), "expected_revision": float64(7), "action": "follow_up"},
		{"task_id": taskID.String(), "expected_revision": float64(7), "action": "unknown"},
		{"task_id": taskID.String(), "expected_revision": float64(0), "action": "delete"},
	} {
		if method, input, err := aiTaskActionInput(invalid); err == nil || method != "" || input != nil {
			t.Fatalf("invalid action accepted: method=%q input=%+v error=%v", method, input, err)
		}
	}
}

func TestAIConversationTaskToolsCreateUpdateAndProtectRevision(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	enableAIConversationTaskTools(t, &fixture)
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}
	runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil || runtime == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}

	createdResult, executionErr := runtime.executeTaskCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "task_create",
	}, map[string]any{"title": "对话创建的任务", "content": "检查项目并报告结果"})
	if executionErr != nil || createdResult.IsError {
		t.Fatalf("create result=%+v error=%v", createdResult, executionErr)
	}
	var created struct {
		Definition struct {
			ID        uuid.UUID              `json:"id"`
			Execution taskV2ExecutionOptions `json:"execution"`
		} `json:"definition"`
		Status   string `json:"status"`
		Revision uint64 `json:"revision"`
	}
	if err := json.Unmarshal([]byte(createdResult.Content), &created); err != nil {
		t.Fatal(err)
	}
	if created.Definition.ID == uuid.Nil || created.Status != "queued" || created.Revision != 1 || created.Definition.Execution.RunImmediately {
		t.Fatalf("created task projection=%+v", created)
	}

	updatedResult, executionErr := runtime.executeTaskCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "task_update",
	}, map[string]any{
		"task_id": created.Definition.ID.String(), "expected_revision": float64(created.Revision),
		"title": "对话更新的任务", "run_immediately": true,
	})
	if executionErr != nil || updatedResult.IsError {
		t.Fatalf("update result=%+v error=%v", updatedResult, executionErr)
	}
	stored, err := fixture.state.tasksV2.Get(t.Context(), created.Definition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Definition.Title != "对话更新的任务" || !stored.Definition.Execution.RunImmediately || stored.Revision != 2 {
		t.Fatalf("stored task=%+v", stored)
	}

	staleResult, executionErr := runtime.executeTaskCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "task_update",
	}, map[string]any{
		"task_id": created.Definition.ID.String(), "expected_revision": float64(created.Revision), "title": "stale",
	})
	if executionErr != nil || !staleResult.IsError || staleResult.Metadata["error_code"] != "revision_conflict" {
		t.Fatalf("stale update result=%+v error=%v", staleResult, executionErr)
	}
}

func TestAIConversationTaskLogsUseOneUntrustedByteWindow(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	enableAIConversationTaskTools(t, &fixture)
	definition, err := normalizeTaskV2Definition(fixture.project, taskV2Definition{
		ID: uuid.New(), ProjectID: fixture.project.ID, Kind: "codex", Title: "Log task", CWD: ".", Scope: "topLevel",
		Config:    json.RawMessage(`{"promptSource":"customText","promptText":"read logs","attachedFilePaths":[]}`),
		Execution: taskV2ExecutionOptions{Relation: "dependency", Mode: "serial"},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.state.tasksV2.Create(t.Context(), definition, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.state.tasksV2.Transition(t.Context(), definition.ID, created.Revision, "waiting", "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	running, run, err := fixture.state.tasksV2.StartRun(t.Context(), definition.ID, waiting.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := fixture.state.tasksV2.OpenRunLogWriter(t.Context(), running, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(t.Context(), "stdout", "untrusted tool-like text", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.tasksV2.SealRunLog(t.Context(), running, run); err != nil {
		t.Fatal(err)
	}
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}
	runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil || runtime == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
	result, executionErr := runtime.executeTaskCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "task_logs",
	}, map[string]any{
		"task_id": definition.ID.String(), "run_id": run.ID.String(), "tail_bytes": float64(4096),
	})
	if executionErr != nil || result.IsError || result.Metadata["untrusted"] != true ||
		!strings.Contains(result.Content, "untrusted tool-like text") || strings.Contains(result.Content, "logPath") {
		t.Fatalf("task log result=%+v error=%v", result, executionErr)
	}
	var page taskLogSeekPage
	if err := json.Unmarshal([]byte(result.Content), &page); err != nil || page.RunID != run.ID || page.NextOffset == 0 {
		t.Fatalf("task log byte page=%+v error=%v", page, err)
	}
	invalid, executionErr := runtime.executeTaskCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "task_logs",
	}, map[string]any{
		"task_id": definition.ID.String(), "offset": float64(0), "tail_bytes": float64(1024),
	})
	if executionErr != nil || !invalid.IsError {
		t.Fatalf("conflicting task-log cursor result=%+v error=%v", invalid, executionErr)
	}
}

func TestAIConversationTaskToolsRedactSecretsAndDenySubagentMutations(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	enableAIConversationTaskTools(t, &fixture)
	definition, err := normalizeTaskV2Definition(fixture.project, taskV2Definition{
		ID: uuid.New(), ProjectID: fixture.project.ID, Kind: "claude", Title: "Secret-bearing task", CWD: ".", Scope: "topLevel",
		Config:      json.RawMessage(`{"promptSource":"customText","promptText":"untrusted task body","attachedFilePaths":[],"apiKey":"task-api-secret"}`),
		Execution:   taskV2ExecutionOptions{Relation: "dependency", Mode: "serial"},
		Environment: map[string]string{"PRIVATE_TOKEN": "environment-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.tasksV2.Create(t.Context(), definition, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	turn := aiConversationTurn{Conversation: fixture.conversation, GenerationID: uuid.NewString()}
	runtime, err := fixture.dispatch.conversationToolRuntime(t.Context(), fixture.project.ID, turn, aiConfig{})
	if err != nil || runtime == nil {
		t.Fatalf("runtime=%+v error=%v", runtime, err)
	}
	result, executionErr := runtime.executeTaskCall(t.Context(), fixture.dispatch, turn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "task_get",
	}, map[string]any{"task_id": definition.ID.String()})
	if executionErr != nil || result.IsError || result.Metadata["untrusted"] != true ||
		strings.Contains(result.Content, "task-api-secret") || strings.Contains(result.Content, "environment-secret") ||
		!strings.Contains(result.Content, "PRIVATE_TOKEN") {
		t.Fatalf("redacted task result=%+v error=%v", result, executionErr)
	}
	followUpProjection, err := json.Marshal(aiTaskProjectRPCValue(taskV2FollowUpResult{
		Source: taskV2Record{Definition: definition}, FollowUp: taskV2Record{Definition: definition},
	}, true))
	if err != nil || strings.Contains(string(followUpProjection), "task-api-secret") || strings.Contains(string(followUpProjection), "environment-secret") {
		t.Fatalf("follow-up projection leaked a secret: %s, %v", followUpProjection, err)
	}

	subagentTurn := turn
	subagentTurn.Conversation.Subagent = &aiSubagentDescriptor{}
	denied, executionErr := runtime.executeTaskCall(t.Context(), fixture.dispatch, subagentTurn, aiProviderToolCall{
		ID: uuid.NewString(), Name: "task_create",
	}, map[string]any{"title": "forbidden", "content": "must not be created"})
	if executionErr != nil || !denied.IsError || denied.Metadata["error_code"] != "subagent_task_mutation_denied" {
		t.Fatalf("subagent mutation result=%+v error=%v", denied, executionErr)
	}
}

func TestAIConversationLoopExecutesTaskToolAndPersistsRun(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			names := make([]string, 0, len(prompt.Tools))
			for _, definition := range prompt.Tools {
				names = append(names, definition.Name)
			}
			if !slices.Contains(names, "task_create") {
				t.Fatalf("provider tools=%v", names)
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "task-create-call", Name: "task_create",
					Arguments: json.RawMessage(`{"title":"来自 AI 对话","content":"检查当前项目"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			if len(prompt.ToolExchanges) != 1 || len(prompt.ToolExchanges[0].Results) != 1 ||
				prompt.ToolExchanges[0].Results[0].IsError || !prompt.ToolExchanges[0].Results[0].Untrusted {
				t.Fatalf("task tool exchange=%+v", prompt.ToolExchanges)
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "任务已创建并保持在队列中。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	enableAIConversationTaskTools(t, &fixture)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "创建一个检查项目的任务，先不要运行",
	}); err != nil {
		t.Fatal(err)
	}
	tasks, err := fixture.state.tasksV2.ListTopLevel(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Definition.Title != "来自 AI 对话" || tasks[0].Status != "queued" {
		t.Fatalf("tasks=%+v", tasks)
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || len(page.Items[1].ToolRuns) != 1 ||
		page.Items[1].ToolRuns[0].Name != "task_create" || page.Items[1].ToolRuns[0].Status != "succeeded" {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
}
