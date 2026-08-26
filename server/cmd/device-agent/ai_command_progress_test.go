package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAICommandProgressHeartbeats(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	starter := new(fakeRawStarter)
	fixture.executor.supervisor = newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	fixture.executor.supervisor.memoryPollInterval = time.Hour
	plan := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{"command": "echo heartbeat", "timeout_seconds": float64(30)})
	plan.progress = make(chan string, 8)
	resultChannel := make(chan aiWorkspaceToolResult, 1)
	go func() { resultChannel <- fixture.executor.Execute(t.Context(), fixture.context, plan, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for starter.latest() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	process := starter.latest()
	if process == nil {
		t.Fatal("supervised process did not start")
	}
	if err := process.emitStdout([]byte("heartbeat partial output\r\n")); err != nil {
		t.Fatal(err)
	}
	var heartbeat string
	select {
	case heartbeat = <-plan.progress:
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat received")
	}
	if !strings.Contains(heartbeat, "heartbeat partial output") {
		t.Fatalf("heartbeat = %q", heartbeat)
	}
	process.finish(0)
	result := <-resultChannel
	if result.IsError {
		t.Fatalf("command result = %+v", result)
	}
	// The channel must close once the command settles.
	for {
		if _, open := <-plan.progress; !open {
			break
		}
	}
}

func TestAIConversationToolRunAllowsRunningHeartbeatGrowth(t *testing.T) {
	state, projectID, config, conversation, now := newLongReplayFixture(t)
	turn, err := state.business.beginAIConversationTurnWithGeneration(t.Context(), projectID, conversation.ID,
		uuid.NewString(), uuid.NewString(), "heartbeat turn", conversation.WorkspaceMode, nil, config, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	started := now.Add(time.Second)
	run := chatToolRun{
		ID: "heartbeat-run", Tool: "run_command", Name: "run_command", Description: "心跳命令",
		Status: "running", Arguments: map[string]any{"command": "echo hi"}, Result: map[string]any{},
		Output: "", StartedAt: started,
	}
	store := state.business
	if _, _, err := store.upsertAIConversationToolRun(t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, run, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	run.Output = "partial one\n"
	if _, _, err := store.upsertAIConversationToolRun(t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, run, now.Add(2*time.Second)); err != nil {
		t.Fatalf("heartbeat growth rejected: %v", err)
	}
	// Shrinking output must still be rejected.
	run.Output = "p"
	if _, _, err := store.upsertAIConversationToolRun(t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, run, now.Add(3*time.Second)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("shrinking heartbeat error = %v", err)
	}
	// Terminal update after heartbeats succeeds.
	run.Output = "partial one\nfinal"
	finished := now.Add(4 * time.Second)
	run.Status, run.FinishedAt = "succeeded", &finished
	if _, _, err := store.upsertAIConversationToolRun(t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, run, now.Add(4*time.Second)); err != nil {
		t.Fatalf("terminal update rejected: %v", err)
	}
	// Late heartbeats after the terminal state must be rejected.
	run.Status, run.FinishedAt = "running", nil
	run.Output = "partial one\nfinal\nlate"
	if _, _, err := store.upsertAIConversationToolRun(t.Context(), projectID, conversation.ID, turn.GenerationID, turn.Assistant.ID, run, now.Add(5*time.Second)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("late heartbeat error = %v", err)
	}
}

func TestArgumentBackgroundParsing(t *testing.T) {
	if argumentBackground(json.RawMessage(`{"command":"x","background":true}`)) != true {
		t.Fatal("background=true must parse")
	}
	if argumentBackground(json.RawMessage(`{"command":"x"}`)) {
		t.Fatal("missing background must default false")
	}
	if argumentBackground(json.RawMessage(`{"command":"x","background":"yes"}`)) {
		t.Fatal("non-bool background must default false")
	}
	if argumentBackground(json.RawMessage(`not json`)) {
		t.Fatal("invalid json must default false")
	}
}
