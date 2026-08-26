package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAIConversationForkCopiesPrefixBeforeEditableUserMessage(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(
		filepath.Join(directory, "state.json"),
		filepath.Join(directory, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	source, err := state.business.createAIConversation(
		t.Context(),
		projectID,
		"",
		"Architecture review",
		aiWorkspaceModeReadOnly,
		config,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: state,
		now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
		scope:                 "remote.peer.ai.chat",
		ai:                    staticAIProvider{},
		requestProjectID:      projectID.String(),
		enforceProjectBinding: true,
	}
	firstUserID := uuid.NewString()
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": source.ID,
		"messageId":      firstUserID,
		"prompt":         "Inspect the current architecture.",
	}); err != nil {
		t.Fatal(err)
	}
	secondUserID := uuid.NewString()
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": source.ID,
		"messageId":      secondUserID,
		"prompt":         "Propose the implementation.",
	}); err != nil {
		t.Fatal(err)
	}
	source, err = state.business.getAIConversation(t.Context(), projectID, source.ID)
	if err != nil {
		t.Fatal(err)
	}

	childID := uuid.NewString()
	value, _, err := dispatch.forkAIConversationRPC(t.Context(), projectID, rpcInput{
		"sourceConversationId": source.ID,
		"conversationId":       childID,
		"messageId":            secondUserID,
		"messageSequence":      float64(3),
		"expectedRevision":     float64(source.Revision),
	})
	if err != nil {
		t.Fatal(err)
	}
	child := value.(conversationView)
	if child.ID != childID || child.MessageCount != 2 || child.LastMessageSequence != 2 ||
		child.State != "idle" || !strings.HasSuffix(child.Title, "（分支）") {
		t.Fatalf("child conversation = %+v", child)
	}
	childMessages, err := state.business.listAIConversationMessages(
		t.Context(),
		projectID,
		child.ID,
		0,
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(childMessages.Items) != 2 ||
		childMessages.Items[0].Content != "Inspect the current architecture." ||
		childMessages.Items[1].Role != "assistant" {
		t.Fatalf("child messages = %#v", childMessages.Items)
	}
	if childMessages.Items[0].ID == firstUserID ||
		childMessages.Items[0].ID == secondUserID {
		t.Fatalf("fork reused source message ids: %#v", childMessages.Items)
	}
	sourceMessages, err := state.business.listAIConversationMessages(
		t.Context(),
		projectID,
		source.ID,
		0,
		10,
	)
	if err != nil || len(sourceMessages.Items) != 4 ||
		sourceMessages.Items[2].ID != secondUserID {
		t.Fatalf("source messages = %#v, error=%v", sourceMessages.Items, err)
	}
}

func TestAIConversationForkCopiesInclusiveTemplateBoundary(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(
		filepath.Join(directory, "state.json"),
		filepath.Join(directory, "workspace"),
	)
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	projectID := stableProjectID(state.DeviceID, "")
	now := time.Now().UTC()
	source, err := state.business.createAIConversation(
		t.Context(), projectID, "", "Template source", aiWorkspaceModeReadOnly, config, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state:                 state,
		now:                   func() time.Time { return now.Add(time.Minute) },
		scope:                 "remote.peer.ai.chat",
		ai:                    staticAIProvider{},
		requestProjectID:      projectID.String(),
		enforceProjectBinding: true,
	}
	userID := uuid.NewString()
	if _, _, err := dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": source.ID,
		"messageId":      userID,
		"prompt":         "Keep this complete turn.",
	}); err != nil {
		t.Fatal(err)
	}
	source, err = state.business.getAIConversation(t.Context(), projectID, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := state.business.listAIConversationMessages(t.Context(), projectID, source.ID, 0, 10)
	if err != nil || len(messages.Items) != 2 {
		t.Fatalf("source messages = %#v, error=%v", messages.Items, err)
	}
	assistant := messages.Items[1]
	childID := uuid.NewString()
	value, _, err := dispatch.forkAIConversationRPC(t.Context(), projectID, rpcInput{
		"sourceConversationId":   source.ID,
		"conversationId":         childID,
		"throughMessageId":       assistant.ID,
		"throughMessageSequence": float64(assistant.Sequence),
		"expectedRevision":       float64(source.Revision),
	})
	if err != nil {
		t.Fatal(err)
	}
	child := value.(conversationView)
	if child.MessageCount != 2 || child.LastMessageSequence != assistant.Sequence {
		t.Fatalf("child conversation = %+v", child)
	}
	childMessages, err := state.business.listAIConversationMessages(t.Context(), projectID, child.ID, 0, 10)
	if err != nil || len(childMessages.Items) != 2 || childMessages.Items[1].Role != "assistant" {
		t.Fatalf("child messages = %#v, error=%v", childMessages.Items, err)
	}
}
