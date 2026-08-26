package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

const (
	controlCommandLimit = 100
	controlBatchLimit   = 200
	projectScanInterval = 30 * time.Second
)

type deviceControlLoop struct {
	state          *agentState
	store          *controlStateStore
	tokens         *deviceTokenManager
	taskHTTPClient *http.Client
	tasks          TaskRepository
	now            func() time.Time
	aiComplete     taskAICompleter

	runContext context.Context
	taskMu     sync.Mutex
	taskCancel map[string]context.CancelFunc
	taskWG     sync.WaitGroup
}

func newDeviceControlLoop(state *agentState, store *controlStateStore, tokens *deviceTokenManager, taskHTTPClient *http.Client) (*deviceControlLoop, error) {
	if state == nil || store == nil || tokens == nil || taskHTTPClient == nil {
		return nil, errors.New("device control loop dependencies are required")
	}
	return &deviceControlLoop{
		state: state, store: store, tokens: tokens, taskHTTPClient: taskHTTPClient,
		tasks: newEncryptedControlTaskRepository(store), now: func() time.Time { return time.Now().UTC() }, taskCancel: map[string]context.CancelFunc{},
	}, nil
}

func (loop *deviceControlLoop) run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("device control context is required")
	}
	loop.runContext = ctx
	defer func() {
		loop.taskMu.Lock()
		for _, cancel := range loop.taskCancel {
			cancel()
		}
		loop.taskMu.Unlock()
		loop.taskWG.Wait()
	}()
	if _, err := reconcileWorkspaceProjects(ctx, loop.state, loop.store, loop.now(), false, nil); err != nil {
		return fmt.Errorf("scan workspace projects: %w", err)
	}
	if err := loop.reconcileTaskV2Projections(ctx); err != nil {
		return fmt.Errorf("prepare Task v2 projections: %w", err)
	}
	loop.resumeRunningTasks()
	nextScan := loop.now().Add(projectScanInterval)
	backoff := 500 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !loop.now().Before(nextScan) {
			_, _ = reconcileWorkspaceProjects(ctx, loop.state, loop.store, loop.now(), false, nil)
			nextScan = loop.now().Add(projectScanInterval)
		}
		retryableFailure := false
		if err := loop.reconcileTaskV2Projections(ctx); err != nil {
			retryableFailure = true
		}
		for _, operation := range []func(context.Context) error{loop.flushChanges, loop.flushEvents, loop.flushAcks} {
			if err := operation(ctx); err != nil {
				if errors.Is(err, errDeviceAuthentication) {
					return err
				}
				retryableFailure = true
			}
		}
		page, err := loop.pollCommands(ctx)
		if err != nil {
			if errors.Is(err, errDeviceAuthentication) {
				return err
			}
			retryableFailure = true
		} else {
			for _, command := range page.Items {
				if err := loop.receiveCommand(command); err != nil {
					return err
				}
			}
			if err := loop.flushAcks(ctx); err != nil {
				if errors.Is(err, errDeviceAuthentication) {
					return err
				}
				retryableFailure = true
			}
			loop.activateAcceptedCommands(ctx)
		}
		wait := 1500 * time.Millisecond
		if err == nil && page.PollAfterMs >= 100 && page.PollAfterMs <= 60000 {
			wait = time.Duration(page.PollAfterMs) * time.Millisecond
		}
		if err == nil && len(page.Items) > 0 {
			wait = 50 * time.Millisecond
		}
		if retryableFailure {
			wait = backoff
			backoff = min(backoff*2, 30*time.Second)
		} else {
			backoff = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (loop *deviceControlLoop) flushChanges(ctx context.Context) error {
	snapshot, err := loop.store.snapshot()
	if err != nil || len(snapshot.Sync.Pending) == 0 {
		return err
	}
	count := min(controlBatchLimit, len(snapshot.Sync.Pending))
	batch := append([]remotecontrol.DeviceChange(nil), snapshot.Sync.Pending[:count]...)
	request := remotecontrol.PushChangesInput{BaseHighWatermark: snapshot.Sync.HighWatermark, Reset: snapshot.Sync.Reset, Changes: batch}
	var result remotecontrol.PushChangesResult
	if err := loop.tokens.doJSON(ctx, http.MethodPost, "/v1/device/remote-control/changes", "", request, &result); err != nil {
		return err
	}
	if result.ResetRequired {
		return prepareWorkspaceProjectReset(ctx, loop.state, loop.store, loop.now())
	}
	wantHighWatermark := batch[len(batch)-1].Sequence
	if result.HighWatermark != wantHighWatermark || result.Applied+result.Replayed != len(batch) {
		return errors.New("project change acknowledgement is invalid")
	}
	return loop.store.update(func(state *controlPersistentState) error {
		if len(state.Sync.Pending) < len(batch) || state.Sync.HighWatermark != snapshot.Sync.HighWatermark {
			return nil
		}
		for index := range batch {
			if state.Sync.Pending[index].Sequence != batch[index].Sequence || state.Sync.Pending[index].ResourceID != batch[index].ResourceID {
				return nil
			}
		}
		state.Sync.Pending = append([]remotecontrol.DeviceChange(nil), state.Sync.Pending[len(batch):]...)
		state.Sync.HighWatermark = result.HighWatermark
		state.Sync.Reset = false
		return nil
	})
}

func (loop *deviceControlLoop) flushEvents(ctx context.Context) error {
	snapshot, err := loop.store.snapshot()
	if err != nil || len(snapshot.PendingEvents) == 0 {
		return err
	}
	count := min(controlBatchLimit, len(snapshot.PendingEvents))
	batch := append([]remotecontrol.DeviceEventInput(nil), snapshot.PendingEvents[:count]...)
	var result remotecontrol.PushEventsResult
	if err := loop.tokens.doJSON(ctx, http.MethodPost, "/v1/device/remote-control/events", "", remotecontrol.PushEventsInput{Events: batch}, &result); err != nil {
		return err
	}
	if result.Accepted+result.Replayed != len(batch) {
		return errors.New("task event acknowledgement is invalid")
	}
	last := batch[len(batch)-1].DeviceSequence
	return loop.store.update(func(state *controlPersistentState) error {
		if len(state.PendingEvents) < len(batch) {
			return nil
		}
		for index := range batch {
			if state.PendingEvents[index].EventID != batch[index].EventID || state.PendingEvents[index].DeviceSequence != batch[index].DeviceSequence {
				return nil
			}
		}
		state.PendingEvents = append([]remotecontrol.DeviceEventInput(nil), state.PendingEvents[len(batch):]...)
		state.EventAckedSequence = max(state.EventAckedSequence, last)
		return nil
	})
}

func (loop *deviceControlLoop) pollCommands(ctx context.Context) (remotecontrol.CommandPage, error) {
	var page remotecontrol.CommandPage
	path := "/v1/device/remote-control/commands?limit=" + url.QueryEscape(fmt.Sprint(controlCommandLimit))
	if err := loop.tokens.doJSON(ctx, http.MethodGet, path, "", nil, &page); err != nil {
		return remotecontrol.CommandPage{}, err
	}
	if len(page.Items) > controlCommandLimit || page.PollAfterMs < 0 || page.PollAfterMs > 60000 {
		return remotecontrol.CommandPage{}, errors.New("command page is invalid")
	}
	return page, nil
}

func (loop *deviceControlLoop) receiveCommand(command remotecontrol.Command) error {
	now := loop.now().UTC()
	if command.ID == uuid.Nil || command.LeaseToken == uuid.Nil || command.GrantVersion == 0 || command.Kind == "" || len(command.Kind) > 80 ||
		len(command.Body) == 0 || len(command.Body) > 24<<10 || !json.Valid(command.Body) || command.ExpiresAt.IsZero() || !command.ExpiresAt.After(now) {
		return errors.New("leased command is invalid")
	}
	return loop.store.update(func(state *controlPersistentState) error {
		key := command.ID.String()
		if existing, found := state.Commands[key]; found {
			if existing.Command.Kind != command.Kind || existing.Command.TaskID == nil != (command.TaskID == nil) ||
				existing.Command.TaskID != nil && *existing.Command.TaskID != *command.TaskID || !bytes.Equal(existing.Command.Body, command.Body) {
				return errors.New("command replay changed immutable fields")
			}
			existing.Command = command
			existing.AckSent = false
			state.Commands[key] = existing
			return nil
		}
		record := localCommand{Command: command, ExecutionState: "accepted", DesiredAck: "accepted"}
		switch command.Kind {
		case "project.sync":
			body, err := decodeProjectSyncCommand(command.Body)
			if err != nil {
				record.ExecutionState, record.DesiredAck, record.FailureCode = "terminal", "failed", "invalid_command"
			} else {
				record.ProjectSync = &body
			}
		case "task.create":
			spec, err := decodeTaskCreateCommand(command.Body, command.TaskID)
			if err != nil {
				failure := "invalid_task_input"
				if errors.Is(err, errTaskUnsupported) {
					failure = "unsupported_task_type"
				}
				record.ExecutionState, record.DesiredAck, record.FailureCode = "terminal", "failed", failure
				var raw taskCreateCommandBody
				if decodeClosedCommandJSON(command.Body, &raw) == nil && raw.TaskID != uuid.Nil && command.TaskID != nil && raw.TaskID == *command.TaskID {
					rejected := localTask{Spec: localTaskSpec{TaskID: raw.TaskID, ProjectID: raw.ProjectID, TaskType: raw.TaskType, Title: raw.Title, Input: raw.Input}, CommandID: command.ID, Status: "rejected", Revision: taskRevisionAfter(0, now), CreatedAt: now, NextLogSequence: 1, TerminalResult: failure}
					record.RequiredEvent = appendTaskStatusEvent(state, &rejected, "rejected", failure, now)
					putLocalTask(state, &rejected)
				}
			} else {
				record.DecodedTask = &spec
				task, exists := state.Tasks[spec.TaskID.String()]
				if exists && !sameLocalTaskSpec(task.Spec, spec) {
					record.ExecutionState, record.DesiredAck, record.FailureCode = "terminal", "failed", "task_conflict"
				} else if !exists {
					task = localTask{Spec: spec, CommandID: command.ID, Status: "accepted", Revision: taskRevisionAfter(0, now), CreatedAt: now, NextLogSequence: 1, CancelRequested: state.CancelledTasks[spec.TaskID.String()]}
					putLocalTask(state, &task)
				}
			}
		case "task.cancel":
			taskID, err := decodeTaskCancelCommand(command.Body, command.TaskID)
			if err != nil {
				record.ExecutionState, record.DesiredAck, record.FailureCode = "terminal", "failed", "invalid_command"
			} else {
				record.CancellationTaskID = &taskID
			}
		default:
			record.ExecutionState, record.DesiredAck, record.FailureCode = "terminal", "failed", "unsupported_command"
		}
		state.Commands[key] = record
		return nil
	})
}

func sameLocalTaskSpec(left, right localTaskSpec) bool {
	return left.TaskID == right.TaskID && left.TaskType == right.TaskType && left.Title == right.Title &&
		(left.ProjectID == nil) == (right.ProjectID == nil) && (left.ProjectID == nil || *left.ProjectID == *right.ProjectID) &&
		(left.ExpectedRevision == nil) == (right.ExpectedRevision == nil) && (left.ExpectedRevision == nil || *left.ExpectedRevision == *right.ExpectedRevision) &&
		bytes.Equal(left.Input, right.Input)
}

func (loop *deviceControlLoop) flushAcks(ctx context.Context) error {
	snapshot, err := loop.store.snapshot()
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(snapshot.Commands))
	for key := range snapshot.Commands {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		record := snapshot.Commands[key]
		if record.AckSent || record.DesiredAck == "" || record.RequiredChange > snapshot.Sync.HighWatermark || record.RequiredEvent > snapshot.EventAckedSequence {
			continue
		}
		input := remotecontrol.AckCommandInput{LeaseToken: record.Command.LeaseToken, Status: record.DesiredAck, FailureCode: record.FailureCode}
		path := "/v1/device/remote-control/commands/" + record.Command.ID.String() + "/ack"
		if err := loop.tokens.doJSON(ctx, http.MethodPost, path, "", input, nil); err != nil {
			return err
		}
		if err := loop.store.update(func(state *controlPersistentState) error {
			current, exists := state.Commands[key]
			if exists && current.Command.LeaseToken == record.Command.LeaseToken && current.DesiredAck == record.DesiredAck {
				current.AckSent = true
				state.Commands[key] = current
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (loop *deviceControlLoop) activateAcceptedCommands(ctx context.Context) {
	snapshot, err := loop.store.snapshot()
	if err != nil {
		return
	}
	keys := make([]string, 0, len(snapshot.Commands))
	for key := range snapshot.Commands {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		record := snapshot.Commands[key]
		if !record.AckSent || record.DesiredAck != "accepted" || record.ExecutionState != "accepted" {
			continue
		}
		switch record.Command.Kind {
		case "project.sync":
			if record.ProjectSync == nil {
				continue
			}
			required, scanErr := reconcileWorkspaceProjects(ctx, loop.state, loop.store, loop.now(), true, record.ProjectSync.ProjectID)
			_ = loop.store.update(func(state *controlPersistentState) error {
				current := state.Commands[key]
				current.ExecutionState = "terminal"
				current.AckSent = false
				if scanErr != nil || record.ProjectSync.ProjectID != nil && required == 0 {
					current.DesiredAck, current.FailureCode = "failed", "project_unavailable"
				} else {
					current.DesiredAck, current.RequiredChange = "completed", required
				}
				state.Commands[key] = current
				return nil
			})
		case "task.create":
			if record.DecodedTask != nil {
				loop.activateTask(key, *record.DecodedTask)
			}
		case "task.cancel":
			if record.CancellationTaskID != nil {
				loop.activateCancellation(ctx, key, *record.CancellationTaskID)
			}
		}
	}
}

func (loop *deviceControlLoop) activateTask(commandKey string, spec localTaskSpec) {
	now := loop.now().UTC()
	shouldStart := false
	_ = loop.store.update(func(state *controlPersistentState) error {
		record := state.Commands[commandKey]
		task := state.Tasks[spec.TaskID.String()]
		if task.Status != "accepted" {
			return nil
		}
		record.ExecutionState = "running"
		state.Commands[commandKey] = record
		if task.CancelRequested || state.CancelledTasks[spec.TaskID.String()] {
			task.Status, task.FinishedAt, task.TerminalResult = "cancelled", timePointer(now), "cancelled"
			appendTaskLogEvent(state, &task, "system", "Task was cancelled before execution.", now)
			required := appendTaskStatusEvent(state, &task, "cancelled", "cancelled", now)
			record = state.Commands[commandKey]
			record.ExecutionState, record.DesiredAck, record.AckSent, record.RequiredEvent = "terminal", "completed", false, required
			state.Commands[commandKey] = record
			putLocalTask(state, &task)
			return nil
		}
		task.Status, task.StartedAt = "running", timePointer(now)
		appendTaskStatusEvent(state, &task, "accepted", "", now)
		appendTaskLogEvent(state, &task, "system", "Task accepted.", now)
		appendTaskStatusEvent(state, &task, "running", "", now)
		appendTaskLogEvent(state, &task, "system", "Task started.", now)
		putLocalTask(state, &task)
		shouldStart = true
		return nil
	})
	if shouldStart {
		loop.startTaskWorker(spec.TaskID)
	}
}

func (loop *deviceControlLoop) activateCancellation(ctx context.Context, commandKey string, taskID uuid.UUID) {
	handledV2, v2Err := loop.cancelTaskV2(ctx, taskID)
	if handledV2 {
		_ = loop.store.update(func(state *controlPersistentState) error {
			record := state.Commands[commandKey]
			record.ExecutionState, record.AckSent = "terminal", false
			if v2Err != nil {
				record.DesiredAck, record.FailureCode = "failed", "task_cancel_failed"
			} else {
				record.DesiredAck, record.FailureCode = "completed", ""
			}
			state.Commands[commandKey] = record
			return nil
		})
		return
	}
	now := loop.now().UTC()
	_ = loop.store.update(func(state *controlPersistentState) error {
		state.CancelledTasks[taskID.String()] = true
		if task, exists := state.Tasks[taskID.String()]; exists && task.Status != "succeeded" && task.Status != "failed" && task.Status != "cancelled" && task.Status != "rejected" {
			task.CancelRequested = true
			if task.PeerManaged {
				task.Status, task.Revision = "cancel_requested", taskRevisionAfter(task.Revision, now)
			}
			appendTaskLogEvent(state, &task, "system", "Cancellation requested.", now)
			putLocalTask(state, &task)
		}
		record := state.Commands[commandKey]
		record.ExecutionState, record.DesiredAck, record.AckSent = "terminal", "completed", false
		state.Commands[commandKey] = record
		return nil
	})
	loop.taskMu.Lock()
	cancel := loop.taskCancel[taskID.String()]
	loop.taskMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (loop *deviceControlLoop) resumeRunningTasks() {
	snapshot, err := loop.store.snapshot()
	if err != nil {
		return
	}
	for _, task := range snapshot.Tasks {
		if task.Status == "running" {
			loop.startTaskWorker(task.Spec.TaskID)
		}
	}
}

func (loop *deviceControlLoop) startTaskWorker(taskID uuid.UUID) {
	loop.taskMu.Lock()
	if _, running := loop.taskCancel[taskID.String()]; running {
		loop.taskMu.Unlock()
		return
	}
	base := loop.runContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	loop.taskCancel[taskID.String()] = cancel
	loop.taskWG.Add(1)
	loop.taskMu.Unlock()
	go func() {
		defer loop.taskWG.Done()
		snapshot, err := loop.store.snapshot()
		if err == nil {
			task := snapshot.Tasks[taskID.String()]
			if task.CancelRequested {
				cancel()
			}
			result := loop.executeTask(ctx, task.Spec)
			loop.finishTask(taskID, result)
		}
		cancel()
		loop.taskMu.Lock()
		delete(loop.taskCancel, taskID.String())
		loop.taskMu.Unlock()
	}()
}

func (loop *deviceControlLoop) finishTask(taskID uuid.UUID, result taskExecutionResult) {
	now := loop.now().UTC()
	_ = loop.store.update(func(state *controlPersistentState) error {
		task, exists := state.Tasks[taskID.String()]
		if !exists || task.Status == "succeeded" || task.Status == "failed" || task.Status == "cancelled" || task.Status == "rejected" {
			return nil
		}
		if task.CancelRequested && result.Status != "cancelled" {
			result = taskExecutionResult{Status: "cancelled", ResultCode: "cancelled", Log: "Task was cancelled."}
		}
		if result.Status != "succeeded" && result.Status != "failed" && result.Status != "cancelled" {
			result = taskExecutionResult{Status: "failed", ResultCode: "task_failed", Log: "Task failed."}
		}
		appendTaskLogEvent(state, &task, "stdout", result.Log, now)
		appendTaskLogEvent(state, &task, "system", "Task finished with status "+result.Status+".", now)
		task.Status, task.FinishedAt, task.TerminalResult = result.Status, timePointer(now), result.ResultCode
		required := appendTaskStatusEvent(state, &task, result.Status, result.ResultCode, now)
		putLocalTask(state, &task)
		if !task.PeerManaged && task.CommandID != uuid.Nil {
			if command, exists := state.Commands[task.CommandID.String()]; exists {
				command.ExecutionState, command.DesiredAck, command.AckSent, command.RequiredEvent = "terminal", "completed", false, required
				state.Commands[task.CommandID.String()] = command
			}
		}
		return nil
	})
}

func appendTaskLogEvent(state *controlPersistentState, task *localTask, stream, content string, now time.Time) uint64 {
	if task.NextLogSequence == 0 {
		task.NextLogSequence = 1
	}
	sequence := task.NextLogSequence
	task.NextLogSequence++
	entry := remotecontrol.TaskLog{Stream: stream, Sequence: sequence, OccurredAt: now.UTC(), Content: content}
	persistTaskLog(state, task.Spec.TaskID, entry)
	// Logs are retained in the Agent's encrypted local store and read through
	// E2EE Peer RPC only. The control-plane event outbox carries status metadata,
	// never CLI output or tool text.
	return 0
}

func appendTaskStatusEvent(state *controlPersistentState, task *localTask, status, resultCode string, now time.Time) uint64 {
	task.Revision = taskRevisionAfter(task.Revision, now)
	if task.PeerManaged {
		return 0
	}
	deviceSequence := state.NextEventSequence
	state.NextEventSequence++
	event := remotecontrol.DeviceEventInput{
		EventID: uuid.New(), TaskID: task.Spec.TaskID, DeviceSequence: deviceSequence, Type: "task." + status,
		Revision: task.Revision, OccurredAt: now.UTC(), Status: status, StartedAt: task.StartedAt, FinishedAt: task.FinishedAt,
	}
	if resultCode != "" {
		event.ResultCode = &resultCode
	}
	state.PendingEvents = append(state.PendingEvents, event)
	return deviceSequence
}

func putLocalTask(state *controlPersistentState, task *localTask) {
	if state.TaskHighWatermark < ^uint64(0) {
		state.TaskHighWatermark++
	}
	task.ChangeSequence = state.TaskHighWatermark
	state.Tasks[task.Spec.TaskID.String()] = *task
	state.TaskChanges = append(state.TaskChanges, localTaskChange{Sequence: state.TaskHighWatermark, Task: *task})
	if len(state.TaskChanges) > maximumPersistedTaskChanges {
		state.TaskChanges = append([]localTaskChange(nil), state.TaskChanges[len(state.TaskChanges)-maximumPersistedTaskChanges:]...)
	}
	if len(state.TaskChanges) > 0 {
		state.TaskMinimumAvailableSequence = state.TaskChanges[0].Sequence
	} else if state.TaskHighWatermark < ^uint64(0) {
		state.TaskMinimumAvailableSequence = state.TaskHighWatermark + 1
	}
}

func persistTaskLog(state *controlPersistentState, taskID uuid.UUID, entry remotecontrol.TaskLog) {
	key := taskID.String()
	logs := append(state.TaskLogs[key], entry)
	if len(logs) > maximumPersistedTaskLogsPerTask {
		logs = append([]remotecontrol.TaskLog(nil), logs[len(logs)-maximumPersistedTaskLogsPerTask:]...)
	}
	state.TaskLogs[key] = logs
	for persistedTaskLogCount(state.TaskLogs) > maximumPersistedTaskLogsTotal {
		oldestKey := ""
		var oldest time.Time
		for candidate, entries := range state.TaskLogs {
			if len(entries) == 0 {
				continue
			}
			if oldestKey == "" || entries[0].OccurredAt.Before(oldest) {
				oldestKey, oldest = candidate, entries[0].OccurredAt
			}
		}
		if oldestKey == "" {
			break
		}
		state.TaskLogs[oldestKey] = append([]remotecontrol.TaskLog(nil), state.TaskLogs[oldestKey][1:]...)
	}
}

func persistedTaskLogCount(logs map[string][]remotecontrol.TaskLog) int {
	total := 0
	for _, entries := range logs {
		total += len(entries)
	}
	return total
}

func taskRevisionAfter(current uint64, now time.Time) uint64 {
	candidate := uint64(max(now.UTC().UnixMilli(), int64(1)))
	if candidate <= current {
		return current + 1
	}
	return candidate
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}
