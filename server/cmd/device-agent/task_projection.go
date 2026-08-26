package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

// reconcileTaskV2Projections copies only reviewed metadata from the local
// business database into the reliable control-plane outbox. In particular it
// never reads or serializes definition_json, task logs, user titles, paths,
// prompts, attachments, environment values, or runner arguments.
func (loop *deviceControlLoop) reconcileTaskV2Projections(ctx context.Context) error {
	if loop == nil || loop.state == nil || loop.state.tasksV2 == nil || loop.store == nil {
		return nil
	}
	tasks, err := loop.state.tasksV2.ListTopLevel(ctx)
	if err != nil {
		return err
	}
	sort.Slice(tasks, func(left, right int) bool {
		return tasks[left].Definition.ID.String() < tasks[right].Definition.ID.String()
	})
	snapshot, err := loop.store.snapshot()
	if err != nil {
		return err
	}
	presentSnapshot := make(map[string]struct{}, len(tasks))
	needsUpdate := false
	for _, task := range tasks {
		key := task.Definition.ID.String()
		presentSnapshot[key] = struct{}{}
		cursor, exists := snapshot.Sync.TaskProjections[key]
		if !exists || !cursor.Present || cursor.Revision < task.Revision {
			needsUpdate = true
		}
	}
	if !needsUpdate {
		for key, cursor := range snapshot.Sync.TaskProjections {
			if _, exists := presentSnapshot[key]; cursor.Present && !exists {
				needsUpdate = true
				break
			}
		}
	}
	if !needsUpdate {
		return nil
	}
	now := loop.now().UTC()
	return loop.store.update(func(state *controlPersistentState) error {
		if state.Sync.TaskProjections == nil {
			state.Sync.TaskProjections = map[string]taskProjectionCursor{}
		}
		present := make(map[string]struct{}, len(tasks))
		next := state.Sync.HighWatermark + uint64(len(state.Sync.Pending)) + 1
		for _, task := range tasks {
			key := task.Definition.ID.String()
			present[key] = struct{}{}
			cursor, projected := state.Sync.TaskProjections[key]
			if projected && cursor.Present && cursor.Revision >= task.Revision {
				continue
			}
			revision := task.Revision
			if revision == 0 {
				return errors.New("Task v2 projection has an invalid revision")
			}
			if cursor.Revision >= revision {
				revision = cursor.Revision + 1
			}
			occurredAt := task.CreatedAt.UTC()
			if occurredAt.IsZero() || occurredAt.After(now.Add(5*time.Minute)) {
				occurredAt = now
			}
			var resultCode *string
			if task.ResultCode != "" {
				value := task.ResultCode
				resultCode = &value
			}
			state.Sync.Pending = append(state.Sync.Pending, remotecontrol.DeviceChange{
				Sequence: next, Kind: "task", Operation: "upsert", ResourceID: task.Definition.ID,
				Revision: revision, OccurredAt: occurredAt, ProjectID: &task.Definition.ProjectID,
				TaskType: task.Definition.Kind, Title: remotecontrol.TaskProjectionDisplayName(task.Definition.Kind),
				Status: taskV2ProjectionStatus(task.Status), ResultCode: resultCode,
				StartedAt: task.StartedAt, FinishedAt: task.FinishedAt,
			})
			state.Sync.TaskProjections[key] = taskProjectionCursor{Revision: revision, Present: true}
			next++
		}

		keys := make([]string, 0, len(state.Sync.TaskProjections))
		for key, cursor := range state.Sync.TaskProjections {
			if cursor.Present {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, exists := present[key]; exists {
				continue
			}
			cursor := state.Sync.TaskProjections[key]
			taskID, parseErr := uuid.Parse(key)
			if parseErr != nil || taskID == uuid.Nil || cursor.Revision == ^uint64(0) {
				return errors.New("Task v2 projection cursor is invalid")
			}
			cursor.Revision++
			cursor.Present = false
			state.Sync.Pending = append(state.Sync.Pending, remotecontrol.DeviceChange{
				Sequence: next, Kind: "task", Operation: "tombstone", ResourceID: taskID,
				Revision: cursor.Revision, OccurredAt: now, Status: "expired",
			})
			state.Sync.TaskProjections[key] = cursor
			next++
		}
		return nil
	})
}

func taskV2ProjectionStatus(status string) string {
	switch status {
	case "queued", "waiting":
		return "queued"
	case "running":
		return "running"
	case "awaitingAcceptance", "changesRequested":
		return "accepted"
	case "completed", "succeeded":
		return "succeeded"
	case "failed", "blocked":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return "failed"
	}
}

func terminalTaskV2ProjectionStatus(status string) bool {
	switch status {
	case "completed", "failed", "blocked", "cancelled", "succeeded":
		return true
	default:
		return false
	}
}

// cancelTaskV2 resolves the current local revision on-device. The cloud
// command intentionally carries only taskId, so no definition or user text is
// needed for reliable cancellation after an offline period.
func (loop *deviceControlLoop) cancelTaskV2(ctx context.Context, taskID uuid.UUID) (bool, error) {
	if loop == nil || loop.state == nil || loop.state.tasksV2 == nil {
		return false, nil
	}
	for attempt := 0; attempt < 4; attempt++ {
		task, err := loop.state.tasksV2.Get(ctx, taskID)
		if errors.Is(err, errRPCNotFound) {
			return false, nil
		}
		if err != nil {
			return true, err
		}
		if terminalTaskV2ProjectionStatus(task.Status) {
			return true, nil
		}
		if engine := loop.state.currentTaskEngine(); engine != nil {
			_, err = engine.Stop(ctx, task.Definition.ProjectID, taskID, task.Revision)
		} else {
			_, err = loop.state.tasksV2.Transition(ctx, taskID, task.Revision, "cancelled", "cancelled", loop.now().UTC())
		}
		if err == nil {
			loop.state.wakeTaskEngine()
			return true, nil
		}
		if !errors.Is(err, errRPCRevision) && !errors.Is(err, errRPCBusy) {
			return true, err
		}
	}
	return true, errRPCBusy
}
