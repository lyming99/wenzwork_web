package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

func TestDeviceControlLoopProjectTaskCancelEventsAndAcks(t *testing.T) {
	root := t.TempDir()
	agent, err := loadOrCreateAgentState(filepath.Join(root, "agent.json"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(agent.Workspace, "README.md"), []byte("# Test project\n\nA useful project."))
	agent.AIConfigs["fast"] = aiConfig{ID: "fast", Name: "Fast", Provider: "openai-compatible", BaseURL: "https://example.test/v1", Model: "fast", Enabled: true, Credential: "secret", Revision: 1}
	agent.AIConfigs["slow"] = aiConfig{ID: "slow", Name: "Slow", Provider: "openai-compatible", BaseURL: "https://example.test/v1", Model: "slow", Enabled: true, Credential: "secret", Revision: 1}
	if err := agent.write(); err != nil {
		t.Fatal(err)
	}
	projectID := stableProjectID(agent.DeviceID, "")
	commands := controlTestCommands(projectID)

	type observedState struct {
		sync.Mutex
		delivered       bool
		highWatermark   uint64
		ackStatuses     map[string][]string
		events          []remotecontrol.DeviceEventInput
		changeBatches   int
		completedSignal chan struct{}
		completedOnce   sync.Once
	}
	observed := &observedState{ackStatuses: map[string][]string{}, completedSignal: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bearer loop-access" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"code":"app_access_token_invalid"}`))
			return
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/device/remote-control/changes":
			var input remotecontrol.PushChangesInput
			if json.NewDecoder(request.Body).Decode(&input) != nil || len(input.Changes) == 0 {
				http.Error(writer, "invalid", http.StatusBadRequest)
				return
			}
			observed.Lock()
			if input.BaseHighWatermark != observed.highWatermark || input.Changes[0].Sequence != observed.highWatermark+1 {
				observed.Unlock()
				writer.WriteHeader(http.StatusConflict)
				_, _ = writer.Write([]byte(`{"code":"remote_sequence_gap"}`))
				return
			}
			observed.highWatermark = input.Changes[len(input.Changes)-1].Sequence
			observed.changeBatches++
			high := observed.highWatermark
			observed.Unlock()
			_ = json.NewEncoder(writer).Encode(remotecontrol.PushChangesResult{HighWatermark: high, Applied: len(input.Changes)})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/device/remote-control/commands":
			observed.Lock()
			items := []remotecontrol.Command{}
			if !observed.delivered {
				observed.delivered = true
				items = commands
			}
			observed.Unlock()
			_ = json.NewEncoder(writer).Encode(remotecontrol.CommandPage{Items: items, PollAfterMs: 100})
		case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/v1/device/remote-control/commands/") && strings.HasSuffix(request.URL.Path, "/ack"):
			parts := strings.Split(request.URL.Path, "/")
			commandID := parts[len(parts)-2]
			var input remotecontrol.AckCommandInput
			if json.NewDecoder(request.Body).Decode(&input) != nil || input.LeaseToken == uuid.Nil {
				http.Error(writer, "invalid", http.StatusBadRequest)
				return
			}
			observed.Lock()
			observed.ackStatuses[commandID] = append(observed.ackStatuses[commandID], input.Status)
			terminal := 0
			for _, statuses := range observed.ackStatuses {
				if slices.Contains(statuses, "completed") || slices.Contains(statuses, "failed") {
					terminal++
				}
			}
			if terminal == len(commands) {
				observed.completedOnce.Do(func() { close(observed.completedSignal) })
			}
			observed.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && request.URL.Path == "/v1/device/remote-control/events":
			var input remotecontrol.PushEventsInput
			if json.NewDecoder(request.Body).Decode(&input) != nil || len(input.Events) == 0 {
				http.Error(writer, "invalid", http.StatusBadRequest)
				return
			}
			observed.Lock()
			observed.events = append(observed.events, input.Events...)
			observed.Unlock()
			_ = json.NewEncoder(writer).Encode(remotecontrol.PushEventsResult{Accepted: len(input.Events)})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	store, err := loadControlState(agent)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := newDeviceTokenManager(server.Client(), mustURL(t, server.URL), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.acceptInitial(deviceTokenSet{AccessToken: "loop-access", ExpiresIn: 600, RefreshToken: "loop-refresh", RefreshExpiresIn: 1200, SessionID: uuid.New(), Scope: "remote.connect"}); err != nil {
		t.Fatal(err)
	}
	loop, err := newDeviceControlLoop(agent, store, manager, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	loop.aiComplete = func(ctx context.Context, config aiConfig, source string) (string, error) {
		if config.Model == "slow" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		if !strings.Contains(source, "useful project") {
			return "", errors.New("source mismatch")
		}
		return "# Summary\n\nA concise summary.", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.run(ctx) }()
	select {
	case <-observed.completedSignal:
	case runErr := <-done:
		t.Fatalf("control loop exited before completion: %v", runErr)
	case <-time.After(10 * time.Second):
		cancel()
		snapshot, _ := store.snapshot()
		observed.Lock()
		acks := observed.ackStatuses
		eventCount := len(observed.events)
		observed.Unlock()
		t.Fatalf("control loop did not complete commands: ACKs=%#v events=%d pendingEvents=%d commands=%#v tasks=%#v", acks, eventCount, len(snapshot.PendingEvents), snapshot.Commands, snapshot.Tasks)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("control loop exit = %v", err)
	}

	observed.Lock()
	defer observed.Unlock()
	if observed.changeBatches == 0 || observed.highWatermark == 0 {
		t.Fatalf("project synchronization = batches %d watermark %d", observed.changeBatches, observed.highWatermark)
	}
	for _, command := range commands {
		statuses := observed.ackStatuses[command.ID.String()]
		if command.Kind == "task.cancel" || command.Kind == "project.sync" || command.Kind == "task.create" {
			if !slices.Contains(statuses, "accepted") || !slices.Contains(statuses, "completed") {
				t.Errorf("command %s (%s) ACKs = %v", command.ID, command.Kind, statuses)
			}
		}
	}
	if len(observed.events) == 0 {
		t.Fatal("no task events were delivered")
	}
	lastDeviceSequence := uint64(0)
	terminal := map[uuid.UUID]string{}
	for _, event := range observed.events {
		if event.DeviceSequence != lastDeviceSequence+1 {
			t.Fatalf("device event sequence = %d after %d", event.DeviceSequence, lastDeviceSequence)
		}
		lastDeviceSequence = event.DeviceSequence
		if event.Type == "task.log" {
			t.Fatalf("task log escaped the device-local store: %+v", event)
		} else if event.Type == "task.succeeded" || event.Type == "task.failed" || event.Type == "task.cancelled" {
			terminal[event.TaskID] = strings.TrimPrefix(event.Type, "task.")
		}
	}
	if len(terminal) != 4 {
		t.Fatalf("terminal task statuses = %#v", terminal)
	}
	cancelled := 0
	for _, status := range terminal {
		if status == "cancelled" {
			cancelled++
		}
	}
	if cancelled != 1 {
		t.Fatalf("cancelled task count = %d (%#v)", cancelled, terminal)
	}
	if _, err := os.Stat(filepath.Join(agent.Workspace, ".wenzwork-output", "readme.html")); err != nil {
		t.Fatalf("rendered markdown output: %v", err)
	}
	if summary, err := os.ReadFile(filepath.Join(agent.Workspace, ".wenzwork-output", "summary.md")); err != nil || !strings.Contains(string(summary), "concise summary") {
		t.Fatalf("AI summary output = %q / %v", summary, err)
	}
}

func TestDeviceControlInboxSurvivesRestartAndDoesNotReexecuteTerminalTask(t *testing.T) {
	root := t.TempDir()
	agent, err := loadOrCreateAgentState(filepath.Join(root, "agent.json"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(agent.Workspace, "README.md"), []byte("# Restart"))
	store, err := loadControlState(agent)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	ackCalls, eventCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/ack"):
			mu.Lock()
			ackCalls++
			mu.Unlock()
			writer.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(request.URL.Path, "/events"):
			var input remotecontrol.PushEventsInput
			_ = json.NewDecoder(request.Body).Decode(&input)
			mu.Lock()
			eventCalls += len(input.Events)
			mu.Unlock()
			_ = json.NewEncoder(writer).Encode(remotecontrol.PushEventsResult{Accepted: len(input.Events)})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	manager, _ := newDeviceTokenManager(server.Client(), mustURL(t, server.URL), store)
	_ = manager.acceptInitial(deviceTokenSet{AccessToken: "restart-access", ExpiresIn: 600, RefreshToken: "restart-refresh", RefreshExpiresIn: 1200, SessionID: uuid.New(), Scope: "remote.connect"})
	loop, _ := newDeviceControlLoop(agent, store, manager, server.Client())
	taskID := uuid.New()
	command := remotecontrol.Command{
		ID: uuid.New(), Kind: "task.create", TaskID: &taskID,
		Body:         mustJSON(t, map[string]any{"taskId": taskID, "taskType": "workspace.inspect", "title": "Restart", "input": map[string]any{}}),
		GrantVersion: 1, LeaseToken: uuid.New(), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(),
	}
	if err := loop.receiveCommand(command); err != nil || loop.flushAcks(context.Background()) != nil {
		t.Fatalf("persist/accepted ACK = %v", err)
	}

	reloadedAgent, err := loadOrCreateAgentState(agent.path, agent.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	reloadedStore, err := loadControlState(reloadedAgent)
	if err != nil {
		t.Fatal(err)
	}
	reloadedManager, _ := newDeviceTokenManager(server.Client(), mustURL(t, server.URL), reloadedStore)
	_ = reloadedManager.acceptInitial(deviceTokenSet{AccessToken: "restart-access", ExpiresIn: 600, RefreshToken: "restart-refresh-2", RefreshExpiresIn: 1200, SessionID: uuid.New(), Scope: "remote.connect"})
	reloadedLoop, _ := newDeviceControlLoop(reloadedAgent, reloadedStore, reloadedManager, server.Client())
	reloadedLoop.runContext = context.Background()
	reloadedLoop.activateAcceptedCommands(context.Background())
	reloadedLoop.taskWG.Wait()
	if err := reloadedLoop.flushEvents(context.Background()); err != nil || reloadedLoop.flushAcks(context.Background()) != nil {
		t.Fatalf("restart outbox flush = %v", err)
	}
	snapshot, err := reloadedStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Tasks[taskID.String()].Status != "succeeded" || !snapshot.Commands[command.ID.String()].AckSent {
		t.Fatalf("restart state = task %#v command %#v", snapshot.Tasks[taskID.String()], snapshot.Commands[command.ID.String()])
	}

	// A re-leased terminal command updates only its lease and replays the final
	// ACK; its task body is not executed again and no new event is emitted.
	command.LeaseToken = uuid.New()
	if err := reloadedLoop.receiveCommand(command); err != nil || reloadedLoop.flushAcks(context.Background()) != nil {
		t.Fatalf("terminal replay = %v", err)
	}
	before := eventCalls
	reloadedLoop.activateAcceptedCommands(context.Background())
	reloadedLoop.taskWG.Wait()
	if err := reloadedLoop.flushEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if ackCalls != 3 || eventCalls != before {
		t.Fatalf("restart/replay ACKs/events = %d/%d (before %d)", ackCalls, eventCalls, before)
	}
}

func controlTestCommands(projectID uuid.UUID) []remotecontrol.Command {
	now := time.Now().UTC()
	newCommand := func(kind string, taskID *uuid.UUID, body map[string]any) remotecontrol.Command {
		return remotecontrol.Command{ID: uuid.New(), Kind: kind, TaskID: taskID, Body: mustJSONNoTest(body), GrantVersion: 1, LeaseToken: uuid.New(), ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	}
	inspectID, renderID, summarizeID, cancelID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	commands := []remotecontrol.Command{
		newCommand("project.sync", nil, map[string]any{"afterSequence": 0, "knownHighWatermark": 0}),
		newCommand("task.create", &inspectID, map[string]any{"taskId": inspectID, "projectId": projectID, "taskType": "workspace.inspect", "title": "Inspect", "input": map[string]any{"maxDepth": 3}}),
		newCommand("task.create", &renderID, map[string]any{"taskId": renderID, "projectId": projectID, "taskType": "markdown.render", "title": "Render", "input": map[string]any{"theme": "dark"}}),
		newCommand("task.create", &summarizeID, map[string]any{"taskId": summarizeID, "projectId": projectID, "taskType": "ai.summarize", "title": "Summarize", "input": map[string]any{"configId": "fast"}}),
		newCommand("task.create", &cancelID, map[string]any{"taskId": cancelID, "projectId": projectID, "taskType": "ai.summarize", "title": "Cancel", "input": map[string]any{"configId": "slow"}}),
		newCommand("task.cancel", &cancelID, map[string]any{"taskId": cancelID}),
	}
	return commands
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	return mustJSONNoTest(value)
}

func mustJSONNoTest(value any) json.RawMessage {
	payload, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return payload
}
