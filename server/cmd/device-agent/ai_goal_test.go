package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAIGoalBlankObjectiveDoesNotCreateOrArmGoal(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)

	_, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.goal.create", rpcInput{
		"conversationId": fixture.conversation.ID,
		"objective":      " \t\r\n ",
		"maxGoalRounds":  float64(8),
	})
	if !errors.Is(err, errRPCInvalid) {
		t.Fatalf("blank Goal error = %v", err)
	}
	conversation, loadErr := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, fixture.conversation.ID)
	fixture.state.aiGoalMu.Lock()
	_, armed := fixture.state.aiGoalArmed[fixture.conversation.ID]
	fixture.state.aiGoalMu.Unlock()
	if loadErr != nil || conversation.Goal != nil || armed {
		t.Fatalf("conversation=%+v load=%v", conversation, loadErr)
	}
	calls, _ := provider.snapshot()
	if calls != 0 {
		t.Fatalf("blank Goal invoked provider %d times", calls)
	}
}

func TestAIGoalRunsOnDeviceAndCompletesThroughCASTool(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			if !strings.Contains(prompt.Text, "<goal_round>") || !strings.Contains(prompt.Text, "Ship verified Goal Mode") {
				return fmt.Errorf("unexpected Goal prompt: %q", prompt.Text)
			}
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "get-goal-1", Name: "get_goal", Arguments: []byte(`{}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			if len(prompt.ToolExchanges) != 1 || len(prompt.ToolExchanges[0].Results) != 1 {
				return fmt.Errorf("missing get_goal exchange: %+v", prompt.ToolExchanges)
			}
			var envelope struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(prompt.ToolExchanges[0].Results[0].Content), &envelope); err != nil {
				return fmt.Errorf("invalid get_goal envelope: %q, %v", prompt.ToolExchanges[0].Results[0].Content, err)
			}
			var result struct {
				Activation string `json:"activation"`
				Goal       struct {
					ID       string `json:"id"`
					Revision uint64 `json:"revision"`
				} `json:"goal"`
			}
			if err := json.Unmarshal([]byte(envelope.Content), &result); err != nil || result.Goal.ID == "" || result.Goal.Revision == 0 || result.Activation != "armed" {
				return fmt.Errorf("invalid get_goal result: %q, %v", envelope.Content, err)
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(envelope.Content), &raw); err != nil {
				return err
			}
			goal, _ := raw["goal"].(map[string]any)
			if _, found := goal["createdAt"]; found {
				return fmt.Errorf("get_goal leaked persistence timestamps: %q", envelope.Content)
			}
			arguments, _ := json.Marshal(map[string]any{
				"goal_id": result.Goal.ID, "revision": result.Goal.Revision, "action": "complete",
			})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "complete-goal-1", Name: "update_goal", Arguments: arguments,
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 2:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "Goal completed with evidence."},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return fmt.Errorf("unexpected provider call %d", index)
		}
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)

	created, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.goal.create", rpcInput{
		"conversationId": fixture.conversation.ID,
		"objective":      "Ship verified Goal Mode",
		"maxGoalRounds":  float64(8),
	})
	if err != nil {
		t.Fatal(err)
	}
	createdConversation := created.(conversationView)
	if createdConversation.Goal == nil || !createdConversation.GoalArmed || createdConversation.Goal.Phase != "active" {
		t.Fatalf("created Goal = %+v", createdConversation)
	}

	deadline := time.Now().Add(5 * time.Second)
	var settled conversationView
	for time.Now().Before(deadline) {
		settled, err = fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, fixture.conversation.ID)
		if err == nil && settled.Goal != nil && settled.Goal.Phase == "complete" && settled.State == "idle" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || settled.Goal == nil || settled.Goal.Phase != "complete" || settled.Goal.RoundsStarted != 1 {
		t.Fatalf("settled Goal = %+v, error=%v", settled.Goal, err)
	}
	if fixture.state.isAIGoalArmed(fixture.conversation.ID, settled.Goal.ID) {
		t.Fatal("terminal Goal remained armed")
	}
	calls, prompts := provider.snapshot()
	if calls != 3 || len(prompts) != 3 {
		t.Fatalf("provider calls=%d prompts=%+v", calls, prompts)
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || page.Items[0].Role != "user" || !strings.Contains(page.Items[0].Content, "<goal_round>") || page.Items[1].Role != "assistant" {
		t.Fatalf("Goal messages = %+v, error=%v", page.Items, err)
	}
	events, _, _, _, err := fixture.state.business.listAIConversationEvents(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	goalEvents := 0
	for _, event := range events {
		if event.Kind == "chat.goal.changed" {
			goalEvents++
		}
	}
	if goalEvents < 3 {
		t.Fatalf("Goal events = %+v", events)
	}
}

func TestAIGoalToolValueUsesCanonicalCompactShape(t *testing.T) {
	now := time.Now().UTC()
	goal := &aiGoalSnapshot{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Revision: 7,
		Objective: "Verify the compact value", Phase: "blocked", RoundsStarted: 4, MaxGoalRounds: 9,
		BlockedReason: &aiGoalBlockReason{Code: "model-reported", Message: "Concrete blocker."},
		CreatedAt:     now.Add(-time.Minute), UpdatedAt: now,
	}
	want := map[string]any{
		"goal": map[string]any{
			"id": goal.ID, "revision": uint64(7), "objective": goal.Objective, "phase": "blocked",
			"roundsStarted": uint64(4), "maxGoalRounds": uint64(9),
			"blockedReason": map[string]any{"code": "model-reported", "message": "Concrete blocker."},
		},
		"activation": "disarmed",
	}
	if got := aiGoalToolValue(goal, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("Goal tool value = %#v, want %#v", got, want)
	}
	if got := aiGoalToolValue(nil, false); !reflect.DeepEqual(got, map[string]any{"goal": nil}) {
		t.Fatalf("empty Goal tool value = %#v", got)
	}
}

func TestAIGoalLoweredCapBlocksAtNextDeviceBoundary(t *testing.T) {
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, &scriptedConversationToolProvider{})
	now := time.Now().UTC()
	seed := &aiGoalSnapshot{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Revision: 2,
		Objective: "Respect a lowered cap", Phase: "active", RoundsStarted: 3, MaxGoalRounds: 8,
		CreatedAt: now, UpdatedAt: now,
	}
	_, _, err := fixture.state.business.updateAIConversationCollaboration(t.Context(), fixture.project.ID, fixture.conversation.ID,
		"", "", "chat.goal.changed", map[string]any{"operation": "seed"},
		func(collaboration *aiConversationCollaboration) error {
			collaboration.Goal = cloneAIGoalSnapshot(seed)
			return nil
		}, now)
	if err != nil {
		t.Fatal(err)
	}
	fixture.state.armAIGoal(fixture.conversation.ID, seed.ID)
	const heldGenerationID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	fixture.state.aiGenerationMu.Lock()
	fixture.state.aiGenerations[fixture.conversation.ID] = activeAIGeneration{
		GenerationID: heldGenerationID, Cancel: func() {}, Phase: aiAgentPhaseRunning,
	}
	fixture.state.aiGenerationMu.Unlock()
	maximum := uint64(2)
	edited, err := fixture.dispatch.editAIGoal(t.Context(), fixture.project.ID, fixture.conversation.ID,
		seed.ID, seed.Revision, nil, &maximum, "", "", "user")
	if err != nil || edited.Goal == nil || edited.Goal.RoundsStarted != 3 || edited.Goal.MaxGoalRounds != 2 || !edited.GoalArmed {
		t.Fatalf("edited Goal = %+v, error=%v", edited.Goal, err)
	}
	blocked, source, prompt, err := fixture.dispatch.admitNextAIGoalRound(t.Context(), fixture.project.ID, fixture.conversation.ID)
	if err != nil || source != nil || prompt != "" || blocked.Goal == nil || blocked.Goal.Phase != "blocked" ||
		blocked.Goal.BlockedReason == nil || blocked.Goal.BlockedReason.Code != "round-limit" || blocked.Goal.RoundsStarted != 3 {
		t.Fatalf("capped Goal = %+v, source=%+v, prompt=%q, error=%v", blocked.Goal, source, prompt, err)
	}
	fixture.state.unregisterAIGeneration(fixture.conversation.ID, heldGenerationID)
}

func TestAIGoalCASAndRestartActivationSemantics(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(_ int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		return emitProviderEvents(onEvent,
			aiProviderStreamEvent{Kind: "text", Delta: "progress"},
			aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
		)
	}
	fixture := newAIConversationToolTestFixture(t, aiWorkspaceModeReadOnly, provider)
	// Seed a durable active Goal without arming it, which is exactly the state
	// observed after a device-agent restart.
	now := time.Now().UTC()
	seed := &aiGoalSnapshot{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Revision: 4,
		Objective: "Resume explicitly", Phase: "active", MaxGoalRounds: 8,
		CreatedAt: now, UpdatedAt: now,
	}
	_, _, err := fixture.state.business.updateAIConversationCollaboration(t.Context(), fixture.project.ID, fixture.conversation.ID,
		"", "", "chat.goal.changed", map[string]any{"operation": "seed"},
		func(collaboration *aiConversationCollaboration) error {
			collaboration.Goal = cloneAIGoalSnapshot(seed)
			return nil
		}, now)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.state.isAIGoalArmed(fixture.conversation.ID, seed.ID) {
		t.Fatal("persisted active Goal was implicitly armed")
	}
	if _, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.goal.edit", rpcInput{
		"conversationId": fixture.conversation.ID,
		"goalId":         seed.ID,
		"revision":       float64(seed.Revision - 1),
		"objective":      "stale edit",
	}); err != errRPCRevision {
		t.Fatalf("stale Goal edit error = %v", err)
	}
	// Hold a synthetic active slot so resume records a wake request instead of
	// launching a background driver; this test is about restart activation and
	// leaves no goroutine racing TempDir cleanup.
	const heldGenerationID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	fixture.state.aiGenerationMu.Lock()
	fixture.state.aiGenerations[fixture.conversation.ID] = activeAIGeneration{
		GenerationID: heldGenerationID,
		Cancel:       func() {},
		Phase:        aiAgentPhaseRunning,
	}
	fixture.state.aiGenerationMu.Unlock()
	resumed, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.goal.resume", rpcInput{
		"conversationId": fixture.conversation.ID,
		"goalId":         seed.ID,
		"revision":       float64(seed.Revision),
	})
	if err != nil || !resumed.(conversationView).GoalArmed {
		t.Fatalf("resumed Goal = %+v, error=%v", resumed, err)
	}
	resumedGoal := resumed.(conversationView).Goal
	listed, _, err := fixture.dispatch.listAIConversationRPC(t.Context(), fixture.project.ID, rpcInput{"limit": float64(10)})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := listed.(map[string]any)["items"].([]conversationView)
	listedArmed := false
	for _, item := range items {
		if item.ID == fixture.conversation.ID {
			listedArmed = item.GoalArmed
		}
	}
	if !listedArmed {
		t.Fatalf("legacy conversation list did not project process-local Goal activation: %+v", items)
	}
	if _, _, err := fixture.dispatch.callAIConversationRPC(t.Context(), "conversation.goal.pause", rpcInput{
		"conversationId": fixture.conversation.ID,
		"goalId":         resumedGoal.ID,
		"revision":       float64(resumedGoal.Revision),
	}); err != nil {
		t.Fatal(err)
	}
	fixture.state.unregisterAIGeneration(fixture.conversation.ID, heldGenerationID)
}
