package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newAIAgentLoopFixture(t *testing.T, provider aiProvider) (*agentState, dispatcher, uuid.UUID, conversationView) {
	t.Helper()
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.business.close() })
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	conversation, err := state.business.createAIConversation(t.Context(), projectID, "", "Agent loop", "readOnly", config, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.ai.chat",
		ai: provider, requestProjectID: projectID.String(),
	}
	return state, dispatch, projectID, conversation
}

func TestAIAgentInboxPersistsAndClaimsStepBeforeOneTurn(t *testing.T) {
	state, _, projectID, conversation := newAIAgentLoopFixture(t, staticAIProvider{})
	now := time.Now().UTC()
	items := []aiAgentInboxItem{
		{ID: uuid.NewString(), ConversationID: conversation.ID, Destination: aiInboxNextTurn, Prompt: "turn one", WorkspaceMode: "readOnly", CreatedAt: now},
		{ID: uuid.NewString(), ConversationID: conversation.ID, Destination: aiInboxNextStep, Prompt: "step one", WorkspaceMode: "readOnly", CreatedAt: now.Add(time.Millisecond)},
		{ID: uuid.NewString(), ConversationID: conversation.ID, Destination: aiInboxNextStep, Prompt: "step two", WorkspaceMode: "readOnly", CreatedAt: now.Add(2 * time.Millisecond)},
		{ID: uuid.NewString(), ConversationID: conversation.ID, Destination: aiInboxNextTurn, Prompt: "turn two", WorkspaceMode: "readOnly", WorkspaceToolsEnabled: true, CreatedAt: now.Add(3 * time.Millisecond)},
	}
	for _, item := range items {
		if _, err := state.business.enqueueAIAgentInboxItem(t.Context(), projectID, item); err != nil {
			t.Fatal(err)
		}
	}
	generationID := uuid.NewString()
	claimed, err := state.business.claimAIAgentInbox(t.Context(), projectID, conversation.ID, aiInboxNextTurn, generationID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 || claimed[0].Prompt != "step one" || claimed[1].Prompt != "step two" || claimed[2].Prompt != "turn one" {
		t.Fatalf("claimed = %#v", claimed)
	}
	pending, err := state.business.listAIAgentInbox(t.Context(), projectID, conversation.ID)
	if err != nil || len(pending) != 1 || pending[0].Prompt != "turn two" {
		t.Fatalf("pending = %#v, error = %v", pending, err)
	}

	reloaded, err := loadOrCreateAgentState(state.path, state.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.business.close() })
	pending, err = reloaded.business.listAIAgentInbox(t.Context(), projectID, conversation.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != items[3].ID || !pending[0].WorkspaceToolsEnabled {
		t.Fatalf("reloaded pending = %#v, error = %v", pending, err)
	}
}

func TestAIAgentIdleDeliveryJoinsDurableInboxFIFO(t *testing.T) {
	state, dispatch, projectID, conversation := newAIAgentLoopFixture(t, staticAIProvider{})
	oldID := uuid.NewString()
	if _, err := state.business.enqueueAIAgentInboxItem(t.Context(), projectID, aiAgentInboxItem{
		ID: oldID, ConversationID: conversation.ID, Destination: aiInboxNextTurn,
		Prompt: "parked first", WorkspaceMode: "readOnly", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	newID := uuid.NewString()
	response, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": conversation.ID, "messageId": newID, "prompt": "new second", "destination": aiInboxNextStep,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response.(map[string]any)
	if result["queued"] != true || result["messageId"] != newID || result["destination"] != aiInboxNextTurn {
		t.Fatalf("response = %#v", result)
	}
	page, err := state.business.listAIConversationMessages(t.Context(), projectID, conversation.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 4 || page.Items[0].ID != oldID || page.Items[0].Content != "parked first" ||
		page.Items[1].Content != "answer: parked first" || page.Items[2].ID != newID ||
		page.Items[2].Content != "new second" || page.Items[3].Content != "answer: new second" {
		t.Fatalf("messages = %#v", page.Items)
	}
	pending, err := state.business.listAIAgentInbox(t.Context(), projectID, conversation.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %#v, error = %v", pending, err)
	}
}

type terminalSteeringProvider struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	prompts []string
	history [][]chatMessage
}

func (*terminalSteeringProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (*terminalSteeringProvider) Complete(context.Context, aiConfig, []chatMessage, string) (string, error) {
	return "", errAIProvider
}

func (provider *terminalSteeringProvider) CompleteEventStream(ctx context.Context, _ aiConfig, history []chatMessage, prompt string, onEvent func(aiProviderStreamEvent) error) error {
	provider.mu.Lock()
	index := len(provider.prompts)
	provider.prompts = append(provider.prompts, prompt)
	provider.history = append(provider.history, append([]chatMessage(nil), history...))
	provider.mu.Unlock()
	if index == 0 {
		close(provider.started)
		select {
		case <-provider.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := onEvent(aiProviderStreamEvent{Kind: "text", Delta: "draft"}); err != nil {
			return err
		}
	} else if err := onEvent(aiProviderStreamEvent{Kind: "text", Delta: "revised"}); err != nil {
		return err
	}
	return onEvent(aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"})
}

func TestAIAgentLoopClaimsTerminalSteeringWithoutStartingAnotherTurn(t *testing.T) {
	provider := &terminalSteeringProvider{started: make(chan struct{}), release: make(chan struct{})}
	state, dispatch, projectID, conversation := newAIAgentLoopFixture(t, provider)
	result := make(chan error, 1)
	go func() {
		_, _, err := dispatch.callConversationSend(context.Background(), rpcInput{
			"conversationId": conversation.ID, "messageId": uuid.NewString(), "prompt": "initial",
		})
		result <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	queued, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": conversation.ID, "messageId": uuid.NewString(), "prompt": "steer now", "destination": aiInboxNextStep,
	})
	if err != nil || queued.(map[string]any)["queued"] != true || queued.(map[string]any)["destination"] != aiInboxNextStep {
		t.Fatalf("queued = %#v, error = %v", queued, err)
	}
	close(provider.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	if len(provider.prompts) != 2 || provider.prompts[1] != "[User steering]\nsteer now" ||
		len(provider.history[1]) == 0 || provider.history[1][len(provider.history[1])-1].Content != "draft" {
		t.Fatalf("prompts=%#v history=%#v", provider.prompts, provider.history)
	}
	provider.mu.Unlock()
	page, err := state.business.listAIConversationMessages(t.Context(), projectID, conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || page.Items[1].Content != "draft\n\nrevised" || page.Items[1].Status != "complete" {
		t.Fatalf("messages=%#v error=%v", page.Items, err)
	}
	pending, err := state.business.listAIAgentInbox(t.Context(), projectID, conversation.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v error=%v", pending, err)
	}
}

type cancellationWakeProvider struct {
	started       chan struct{}
	cancelled     chan struct{}
	finishStopped chan struct{}
	mu            sync.Mutex
	calls         int
}

func (*cancellationWakeProvider) Test(context.Context, aiConfig) (time.Duration, error) {
	return time.Millisecond, nil
}

func (provider *cancellationWakeProvider) Complete(ctx context.Context, _ aiConfig, _ []chatMessage, prompt string) (string, error) {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.mu.Unlock()
	if call > 1 {
		return "replacement: " + prompt, nil
	}
	close(provider.started)
	<-ctx.Done()
	close(provider.cancelled)
	<-provider.finishStopped
	return "", ctx.Err()
}

func TestAIAgentCancellationRetargetsAndWakesAfterConvergence(t *testing.T) {
	provider := &cancellationWakeProvider{
		started: make(chan struct{}), cancelled: make(chan struct{}), finishStopped: make(chan struct{}),
	}
	state, dispatch, projectID, conversation := newAIAgentLoopFixture(t, provider)
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := dispatch.callConversationSend(context.Background(), rpcInput{
			"conversationId": conversation.ID, "messageId": uuid.NewString(), "prompt": "first",
		})
		firstDone <- err
	}()
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	parkedID := uuid.NewString()
	parked, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": conversation.ID, "messageId": parkedID, "prompt": "drop on cancel", "destination": aiInboxNextTurn,
	})
	if err != nil || parked.(map[string]any)["queued"] != true {
		t.Fatalf("pre-cancel input = %#v, error = %v", parked, err)
	}
	if _, _, err := dispatch.cancelAIConversationRPC(t.Context(), projectID, rpcInput{"conversationId": conversation.ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not observe cancellation")
	}
	if pending, listErr := state.business.listAIAgentInbox(t.Context(), projectID, conversation.ID); listErr != nil || len(pending) != 0 {
		t.Fatalf("default cancel retained inbox=%#v error=%v", pending, listErr)
	}
	replacementID := uuid.NewString()
	queued, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": conversation.ID, "messageId": replacementID, "prompt": "after cancel", "destination": aiInboxNextStep,
	})
	if err != nil || queued.(map[string]any)["queued"] != true || queued.(map[string]any)["destination"] != aiInboxNextTurn {
		t.Fatalf("post-cancel input = %#v, error = %v", queued, err)
	}
	close(provider.finishStopped)
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first run error = %v", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		page, pageErr := state.business.listAIConversationMessages(t.Context(), projectID, conversation.ID, 0, 10)
		if pageErr == nil && len(page.Items) == 4 && page.Items[3].Status == "complete" {
			if page.Items[2].ID != replacementID || page.Items[3].Content != "replacement: after cancel" {
				t.Fatalf("replacement messages = %#v", page.Items)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement did not converge: messages=%#v error=%v", page.Items, pageErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
