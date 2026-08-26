package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestAgentStateStorageFailureRollsBackInMemoryMutations(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	originalContents, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	originalRevision := state.Revision
	originalSessionID := state.SessionID
	originalEpoch := state.ConnectionEpoch

	blocker := filepath.Join(directory, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("storage unavailable"), 0o600); err != nil {
		t.Fatal(err)
	}
	state.path = filepath.Join(blocker, "state.json")
	if err := state.persistMutation(); err == nil || state.Revision != originalRevision {
		t.Fatalf("failed revision mutation error=%v revision=%d", err, state.Revision)
	}
	if err := state.setSessionID(uuid.New()); err == nil || state.SessionID != originalSessionID {
		t.Fatalf("failed session mutation error=%v session=%s", err, state.SessionID)
	}
	if epoch, err := state.advanceConnectionEpoch(); err == nil || epoch != 0 || state.ConnectionEpoch != originalEpoch {
		t.Fatalf("failed epoch mutation epoch=%d error=%v state=%d", epoch, err, state.ConnectionEpoch)
	}

	state.path = statePath
	afterFailure, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(afterFailure, originalContents) {
		t.Fatalf("durable identity changed after storage failure: equal=%v error=%v", bytes.Equal(afterFailure, originalContents), err)
	}
	if err := state.persistMutation(); err != nil || state.Revision != originalRevision+1 {
		t.Fatalf("state did not recover after storage returned: revision=%d error=%v", state.Revision, err)
	}
}

func TestBusinessStoreCorruptionFailsClosedAndBackupRestores(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	identityBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	businessPath := state.business.path
	businessBackup, err := os.ReadFile(businessPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("not a SQLite database; private payload marker must remain local")
	if err := os.WriteFile(businessPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadOrCreateAgentState(statePath, workspace); err == nil {
		t.Fatal("corrupt BusinessStore unexpectedly started")
	}
	identityAfter, identityErr := os.ReadFile(statePath)
	corruptAfter, corruptErr := os.ReadFile(businessPath)
	if identityErr != nil || corruptErr != nil || !bytes.Equal(identityAfter, identityBefore) || !bytes.Equal(corruptAfter, corrupt) {
		t.Fatalf(
			"corruption handling modified durable inputs: identity=%v database=%v errors=%v/%v",
			bytes.Equal(identityAfter, identityBefore),
			bytes.Equal(corruptAfter, corrupt),
			identityErr,
			corruptErr,
		)
	}

	if err := os.WriteFile(businessPath, businessBackup, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatalf("restored BusinessStore did not start: %v", err)
	}
	projects, err := restored.business.listProjects(t.Context(), false)
	if err != nil || len(projects) == 0 {
		t.Fatalf("restored BusinessStore projects=%d error=%v", len(projects), err)
	}
}

func TestAIGenerationRegistryEnforcesGlobalConcurrency(t *testing.T) {
	state := &agentState{aiGenerations: map[string]activeAIGeneration{}}
	conversationIDs := make([]string, 0, maximumActiveAIGenerations)
	generationIDs := make([]string, 0, maximumActiveAIGenerations)
	for index := 0; index < maximumActiveAIGenerations; index++ {
		conversationID, generationID := uuid.NewString(), uuid.NewString()
		if !state.registerAIGeneration(conversationID, generationID, func() {}) {
			t.Fatalf("generation %d was rejected before the global limit", index)
		}
		conversationIDs = append(conversationIDs, conversationID)
		generationIDs = append(generationIDs, generationID)
	}
	if state.registerAIGeneration(uuid.NewString(), uuid.NewString(), func() {}) {
		t.Fatal("generation above the global concurrency limit was accepted")
	}
	if state.registerAIGeneration(conversationIDs[0], uuid.NewString(), func() {}) {
		t.Fatal("second generation for one conversation was accepted")
	}

	state.unregisterAIGeneration(conversationIDs[0], generationIDs[0])
	if !state.registerAIGeneration(uuid.NewString(), uuid.NewString(), func() {}) {
		t.Fatal("capacity was not released after a generation finished")
	}
}

func TestRPCPayloadLimitsRejectLargeAndUncompressedBombInputs(t *testing.T) {
	query := dispatcher{now: time.Now, scope: "remote.peer.query"}
	oversized := append([]byte(`{"padding":"`), bytes.Repeat([]byte("x"), maximumRPCPayload)...)
	oversized = append(oversized, []byte(`"}`)...)
	envelope, err := newCallEnvelope(uuid.NewString(), "agent.capabilities.get", []byte(`{}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	envelope.GetRequest().JsonPayload = oversized
	response := query.dispatch(t.Context(), envelope).GetResponse()
	if response.GetError() == nil || response.GetError().GetSafeMessage() != "RPC_PAYLOAD_TOO_LARGE" ||
		response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED {
		t.Fatalf("oversized RPC response = %+v", response)
	}

	root := filepath.Clean(filepath.Join("..", "..", ".."))
	for _, path := range []string{
		filepath.Join(root, "server", "cmd", "device-agent", "network.go"),
		filepath.Join(root, "server", "internal", "relayserver", "handler.go"),
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(contents, []byte("CompressionMode: websocket.CompressionDisabled")) {
			t.Fatalf("Peer WebSocket compression is not explicitly disabled in %s", path)
		}
	}
}
