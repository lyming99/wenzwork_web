package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAIExitPlanModeRequiresExplicitApproval(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if index == 0 {
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "exit-plan-call", Name: "exit_plan_mode", Arguments: []byte(`{"plan":"# Implementation plan\n\n1. Inspect the API.\n2. Implement and verify the change."}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		}
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "approved"},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	if _, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.plan.set", rpcInput{
		"conversationId": fixture.conversation.ID, "active": true,
	}); err != nil {
		t.Fatal(err)
	}
	approvalRequests := make(chan aiApprovalRequest, 1)
	fixture.dispatch.chatEvent = func(event aiConversationEvent) error {
		if event.Kind == "chat.approval.requested" {
			if request, ok := event.Payload["approval"].(aiApprovalRequest); ok {
				approvalRequests <- request
			}
		}
		return nil
	}
	sendDone := make(chan error, 1)
	go func() {
		_, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
			"conversationId": fixture.conversation.ID,
			"messageId":      uuid.NewString(),
			"prompt":         "Finish the plan",
		})
		sendDone <- err
	}()
	var request aiApprovalRequest
	select {
	case request = <-approvalRequests:
	case <-time.After(3 * time.Second):
		t.Fatal("Plan approval request was not emitted")
	}
	if _, _, err := fixture.dispatch.respondAIConversationApprovalRPC(t.Context(), fixture.project.ID, rpcInput{
		"approvalId":     request.ID,
		"conversationId": request.ConversationID,
		"generationId":   request.GenerationID,
		"toolCallId":     request.ToolCallID,
		"decision":       "allowOnce",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Plan approval did not resume generation")
	}
	conversation, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || conversation.PlanModeActive {
		t.Fatalf("conversation after approval = %+v, error=%v", conversation, err)
	}
}

func TestAIPlanModePersistsAndTodoSnapshotClearsOnNextTurn(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if index == 0 {
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "todo-call", Name: "todo_write", Arguments: []byte(`{"todos":[{"content":"Inspect API","status":"in_progress"},{"content":"Implement","status":"pending"}]}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		}
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "done"},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)

	value, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.plan.set", rpcInput{
		"conversationId": fixture.conversation.ID,
		"active":         true,
	})
	if err != nil || !value.(conversationView).PlanModeActive {
		t.Fatalf("enable Plan Mode = %#v, error=%v", value, err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID,
		"messageId":      uuid.NewString(),
		"prompt":         "Plan this change",
	}); err != nil {
		t.Fatal(err)
	}
	afterTodo, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || !afterTodo.PlanModeActive || len(afterTodo.Todos) != 2 || afterTodo.Todos[0].Status != "in_progress" {
		t.Fatalf("collaboration after todo = %+v, error=%v", afterTodo, err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID,
		"messageId":      uuid.NewString(),
		"prompt":         "Continue",
	}); err != nil {
		t.Fatal(err)
	}
	afterNextTurn, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || !afterNextTurn.PlanModeActive || len(afterNextTurn.Todos) != 0 {
		t.Fatalf("collaboration after next turn = %+v, error=%v", afterNextTurn, err)
	}
}

func TestAISubagentRunsInIndependentConversationAndSettlesParentNotice(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(_ int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		if !strings.Contains(prompt.Text, "Inspect the API") {
			return errAIProvider
		}
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "child result"},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)

	spawned, err := fixture.dispatch.spawnAISubagent(
		t.Context(), fixture.project.ID, fixture.conversation.ID,
		"Inspect the API", "API inspection", false,
	)
	if err != nil || spawned.Status != "completed" || spawned.Output != "child result" {
		t.Fatalf("spawn result = %+v, error=%v", spawned, err)
	}
	child, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, spawned.AgentID)
	if err != nil || child.Subagent == nil || child.Subagent.ParentConversationID != fixture.conversation.ID ||
		child.Subagent.Depth != 1 || child.Subagent.Status != "completed" || child.WorkspaceMode != fixture.conversation.WorkspaceMode {
		t.Fatalf("child = %+v, error=%v", child, err)
	}
	children, err := fixture.state.business.listAISubagents(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("children = %+v, error=%v", children, err)
	}
	inbox, err := fixture.state.business.listAIAgentInbox(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || len(inbox) != 1 || inbox[0].Destination != aiInboxNextStep || !strings.Contains(inbox[0].Prompt, child.ID) {
		t.Fatalf("parent inbox = %+v, error=%v", inbox, err)
	}
	result, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.subagents.list", rpcInput{
		"conversationId": fixture.conversation.ID,
	})
	root, rootOK := result.(map[string]any)
	items, itemsOK := root["items"].([]conversationView)
	if err != nil || !rootOK || !itemsOK || len(items) != 1 || items[0].ID != child.ID {
		t.Fatalf("subagent RPC = %#v, error=%v", result, err)
	}
	parent, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.delete", rpcInput{
		"conversationId":   fixture.conversation.ID,
		"expectedRevision": float64(parent.Revision),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, child.ID); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("child survived parent deletion: %v", err)
	}
}

type blockingAISubagentTreeProvider struct {
	started   chan string
	cancelled chan string
	release   <-chan struct{}
}

type delayedAISubagentSettlementProvider struct {
	childStarted  chan struct{}
	releaseChild  <-chan struct{}
	parentSettled chan struct{}
	parentResumed chan struct{}
	startOnce     sync.Once
	settledOnce   sync.Once
	resumedOnce   sync.Once
}

func (*delayedAISubagentSettlementProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (*delayedAISubagentSettlementProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProvider
}

func (provider *delayedAISubagentSettlementProvider) CompletePromptEventStream(
	ctx context.Context,
	_ aiConfig,
	_ []chatMessage,
	prompt aiProviderPrompt,
	onEvent func(aiProviderStreamEvent) error,
) error {
	switch {
	case strings.Contains(prompt.Text, `"type":"subagent_result"`):
		provider.resumedOnce.Do(func() { close(provider.parentResumed) })
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "parent integrated child result"},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	case strings.Contains(prompt.Text, "slow child task"):
		provider.startOnce.Do(func() { close(provider.childStarted) })
		select {
		case <-provider.releaseChild:
		case <-ctx.Done():
			return ctx.Err()
		}
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "delayed child result"},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	case strings.Contains(prompt.Text, "coordinate background child"):
		if len(prompt.ToolExchanges) == 0 {
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "spawn-delayed-child", Name: "spawn_agent",
					Arguments: []byte(`{"task":"slow child task","label":"slow child","background":true}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		}
		provider.settledOnce.Do(func() { close(provider.parentSettled) })
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "parent is idle while child runs"},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	default:
		return errAIProvider
	}
}

func TestBackgroundAISubagentSettlementWakesIdleParent(t *testing.T) {
	releaseChild := make(chan struct{})
	provider := &delayedAISubagentSettlementProvider{
		childStarted: make(chan struct{}), releaseChild: releaseChild,
		parentSettled: make(chan struct{}), parentResumed: make(chan struct{}),
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)

	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID,
		"messageId":      uuid.NewString(),
		"prompt":         "coordinate background child",
	}); err != nil {
		t.Fatal(err)
	}
	receiveSignal := func(channel <-chan struct{}, message string) {
		t.Helper()
		select {
		case <-channel:
		case <-time.After(3 * time.Second):
			t.Fatal(message)
		}
	}
	receiveSignal(provider.childStarted, "background child did not start")
	receiveSignal(provider.parentSettled, "parent did not become idle before the child settled")
	close(releaseChild)
	receiveSignal(provider.parentResumed, "idle parent was not woken by the child settlement notice")

	deadline := time.Now().Add(3 * time.Second)
	for {
		page, err := fixture.state.business.listAIConversationMessages(
			t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10,
		)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, message := range page.Items {
			if message.Role == "assistant" && message.Status == "complete" && message.Content == "parent integrated child result" {
				found = true
				break
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent messages after child settlement = %+v", page.Items)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBackgroundAISubagentEventsStayOutOfParentRequestStream(t *testing.T) {
	childStreamed := make(chan struct{})
	var childStreamedOnce sync.Once
	provider := &scriptedConversationToolProvider{}
	provider.step = func(_ int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch {
		case strings.Contains(prompt.Text, `"type":"subagent_result"`):
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "parent integrated background child"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case strings.Contains(prompt.Text, "stream-isolated child task"):
			err := emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "background child result"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
			childStreamedOnce.Do(func() { close(childStreamed) })
			return err
		case strings.Contains(prompt.Text, "coordinate streamed background child"):
			if len(prompt.ToolExchanges) == 0 {
				return emitProviderEvents(onEvent,
					aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
						ID: "spawn-streamed-child", Name: "spawn_agent",
						Arguments: []byte(`{"task":"stream-isolated child task","label":"streamed child","background":true}`),
					}}},
					aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
				)
			}
			select {
			case <-childStreamed:
			case <-time.After(3 * time.Second):
				return errors.New("background child did not stream before the parent settled")
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "parent completed independently"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	request, err := newCallEnvelope(uuid.NewString(), "conversation.send", []byte(`{"conversationId":"`+
		fixture.conversation.ID+`","messageId":"`+uuid.NewString()+`","content":"coordinate streamed background child"}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	response, envelopes := fixture.dispatch.dispatchStream(t.Context(), request)
	if response.GetResponse().GetError() != nil {
		t.Fatalf("conversation.send response = %+v", response.GetResponse().GetError())
	}
	if len(envelopes) == 0 {
		t.Fatal("conversation.send emitted no request-bound events")
	}
	for _, envelope := range envelopes {
		var event aiConversationEvent
		if err := json.Unmarshal(envelope.GetEvent().GetJsonPayload(), &event); err != nil {
			t.Fatal(err)
		}
		if event.ConversationID != fixture.conversation.ID {
			t.Fatalf("parent request stream leaked %s event from child conversation %s", event.Kind, event.ConversationID)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		children, err := fixture.state.business.listAISubagents(t.Context(), fixture.project.ID, fixture.conversation.ID)
		if err != nil {
			t.Fatal(err)
		}
		fixture.state.aiSubagentMu.Lock()
		activeChildren := len(fixture.state.aiSubagentActivities)
		fixture.state.aiSubagentMu.Unlock()
		fixture.state.aiGenerationMu.Lock()
		activeGenerations := len(fixture.state.aiGenerations)
		fixture.state.aiGenerationMu.Unlock()
		if len(children) == 1 && children[0].Subagent != nil && children[0].Subagent.Status == "completed" &&
			activeChildren == 0 && activeGenerations == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child did not quiesce: children=%+v activities=%d generations=%d", children, activeChildren, activeGenerations)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAISubagentFollowUpEventsStayOutOfParentRequestStream(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(_ int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch {
		case strings.Contains(prompt.Text, "initial isolated child task"):
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "initial child result"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		case strings.Contains(prompt.Text, "follow-up isolated child task"):
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "follow-up child result"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	spawned, err := fixture.dispatch.spawnAISubagent(
		t.Context(), fixture.project.ID, fixture.conversation.ID,
		"initial isolated child task", "isolated child", false,
	)
	if err != nil || spawned.Status != "completed" {
		t.Fatalf("initial child = %+v, error=%v", spawned, err)
	}

	streamed := make([]aiConversationEvent, 0)
	resumed := fixture.dispatch
	resumed.chatEvent = func(event aiConversationEvent) error {
		streamed = append(streamed, event)
		return nil
	}
	if _, err := fixture.state.business.enqueueAIAgentInboxItem(t.Context(), fixture.project.ID, aiAgentInboxItem{
		ID:             uuid.NewString(),
		ConversationID: spawned.AgentID,
		Destination:    aiInboxNextStep,
		Prompt:         "follow-up isolated child task",
		Attachments:    []chatAttachmentReference{},
		WorkspaceMode:  aiWorkspaceModeReadOnly,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// sendAISubagentMessage starts this same autonomous driver in a goroutine.
	// Run it synchronously here so the regression assertion cannot race fixture
	// cleanup while preserving the copied request-callback failure condition.
	resumed.resumeAIAgentInbox(fixture.project.ID, spawned.AgentID)

	if len(streamed) != 0 {
		for _, event := range streamed {
			if event.ConversationID != fixture.conversation.ID {
				t.Fatalf("parent request stream leaked %s event from resumed child conversation %s", event.Kind, event.ConversationID)
			}
		}
		t.Fatalf("autonomous child driver reused a request-bound callback: %+v", streamed)
	}
	child, err := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, spawned.AgentID)
	if err != nil || child.Subagent == nil || child.Subagent.Status != "completed" || child.State != "idle" {
		t.Fatalf("follow-up child did not settle: %+v, error=%v", child, err)
	}
	childEvents, _, _, _, err := fixture.state.business.listAIConversationEvents(
		t.Context(), fixture.project.ID, spawned.AgentID, 0, 100,
	)
	if err != nil {
		t.Fatal(err)
	}
	foundFollowUp := false
	for _, event := range childEvents {
		if event.Kind == "chat.text.delta" && event.Payload["delta"] == "follow-up child result" {
			foundFollowUp = true
			break
		}
	}
	if !foundFollowUp {
		t.Fatalf("resumed child follow-up was not durably published: %+v", childEvents)
	}
}

func (*blockingAISubagentTreeProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (*blockingAISubagentTreeProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProvider
}

func (provider *blockingAISubagentTreeProvider) CompletePromptEventStream(
	ctx context.Context,
	_ aiConfig,
	_ []chatMessage,
	prompt aiProviderPrompt,
	_ func(aiProviderStreamEvent) error,
) error {
	provider.started <- prompt.Text
	<-ctx.Done()
	provider.cancelled <- prompt.Text
	<-provider.release
	return ctx.Err()
}

func receiveAISubagentTreeSignal(t *testing.T, values <-chan string, expected string) {
	t.Helper()
	select {
	case value := <-values:
		if value != expected {
			t.Fatalf("subagent tree signal = %q, want %q", value, expected)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for subagent tree signal %q", expected)
	}
}

func TestConversationCancelStopsCompleteSubagentTreeWithoutParentWake(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	provider := &blockingAISubagentTreeProvider{
		started: make(chan string, 8), cancelled: make(chan string, 8), release: release,
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	rootDone := make(chan error, 1)
	go func() {
		_, _, err := fixture.dispatch.callConversationSend(context.Background(), rpcInput{
			"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "root task",
		})
		rootDone <- err
	}()
	receiveAISubagentTreeSignal(t, provider.started, "root task")

	child, err := fixture.dispatch.spawnAISubagent(
		context.Background(), fixture.project.ID, fixture.conversation.ID, "child task", "child", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiveAISubagentTreeSignal(t, provider.started, "child task")
	grandchild, err := fixture.dispatch.spawnAISubagent(
		context.Background(), fixture.project.ID, child.AgentID, "grandchild task", "grandchild", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiveAISubagentTreeSignal(t, provider.started, "grandchild task")
	sibling, err := fixture.dispatch.spawnAISubagent(
		context.Background(), fixture.project.ID, fixture.conversation.ID, "sibling task", "sibling", true,
	)
	if err != nil {
		t.Fatal(err)
	}
	receiveAISubagentTreeSignal(t, provider.started, "sibling task")

	result, _, err := fixture.dispatch.cancelAIConversationRPC(t.Context(), fixture.project.ID, rpcInput{
		"conversationId": fixture.conversation.ID,
	})
	if err != nil || result.(map[string]any)["cancelled"] != true {
		t.Fatalf("cancel result = %#v, error=%v", result, err)
	}
	cancelled := map[string]bool{}
	for len(cancelled) < 4 {
		select {
		case task := <-provider.cancelled:
			cancelled[task] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("cancelled tasks = %#v", cancelled)
		}
	}
	for _, task := range []string{"root task", "child task", "grandchild task", "sibling task"} {
		if !cancelled[task] {
			t.Fatalf("task %q did not observe cancellation: %#v", task, cancelled)
		}
	}
	if _, err := fixture.dispatch.spawnAISubagent(
		context.Background(), fixture.project.ID, fixture.conversation.ID, "late task", "late", true,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("spawn below cancellation cutoff error = %v", err)
	}

	close(release)
	released = true
	if err := <-rootDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("root completion error = %v", err)
	}
	childIDs := []string{child.AgentID, grandchild.AgentID, sibling.AgentID}
	deadline := time.Now().Add(4 * time.Second)
	for {
		settled := !fixture.state.isAISubagentClosing(fixture.conversation.ID)
		for _, childID := range childIDs {
			value, getErr := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, childID)
			if getErr != nil || value.Subagent == nil || value.Subagent.Status != "interrupted" {
				settled = false
				break
			}
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subagent tree did not settle after cancellation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, conversationID := range append([]string{fixture.conversation.ID}, childIDs...) {
		inbox, listErr := fixture.state.business.listAIAgentInbox(t.Context(), fixture.project.ID, conversationID)
		if listErr != nil || len(inbox) != 0 {
			t.Fatalf("conversation %s inbox after tree cancellation = %#v, error=%v", conversationID, inbox, listErr)
		}
	}
	directChildren, err := fixture.state.business.listAISubagents(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || len(directChildren) != 2 {
		t.Fatalf("direct children after rejected late spawn = %#v, error=%v", directChildren, err)
	}
}
