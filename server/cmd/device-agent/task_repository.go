package main

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

// TaskRepository mirrors the durable boundaries used by WenzMark's task and
// task_run stores: immutable IDs, per-record revision CAS, a monotonic change
// journal, and persisted log tails. It intentionally exposes no command string
// or generic runner entry point.
type TaskRepository interface {
	Snapshot(context.Context) (taskRepositorySnapshot, error)
	Get(context.Context, uuid.UUID) (localTask, error)
	Create(context.Context, localTask, []taskLogDraft) (localTask, error)
	CompareAndSwap(context.Context, uuid.UUID, uint64, localTask, []taskLogDraft) (localTask, error)
	ListLogs(context.Context, uuid.UUID) ([]remotecontrol.TaskLog, error)
}

type taskRepositorySnapshot struct {
	Tasks                    map[string]localTask
	Changes                  []localTaskChange
	HighWatermark            uint64
	MinimumAvailableSequence uint64
}

type taskLogDraft struct {
	Stream  string
	Content string
	At      time.Time
}

type encryptedControlTaskRepository struct {
	store *controlStateStore
}

func newEncryptedControlTaskRepository(store *controlStateStore) TaskRepository {
	if store == nil {
		return nil
	}
	return encryptedControlTaskRepository{store: store}
}

func (repository encryptedControlTaskRepository) Snapshot(ctx context.Context) (taskRepositorySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return taskRepositorySnapshot{}, err
	}
	snapshot, err := repository.store.snapshot()
	if err != nil {
		return taskRepositorySnapshot{}, err
	}
	return taskRepositorySnapshot{
		Tasks: snapshot.Tasks, Changes: snapshot.TaskChanges, HighWatermark: snapshot.TaskHighWatermark,
		MinimumAvailableSequence: snapshot.TaskMinimumAvailableSequence,
	}, nil
}

func (repository encryptedControlTaskRepository) Get(ctx context.Context, taskID uuid.UUID) (localTask, error) {
	if taskID == uuid.Nil {
		return localTask{}, errRPCInvalid
	}
	snapshot, err := repository.Snapshot(ctx)
	if err != nil {
		return localTask{}, err
	}
	task, exists := snapshot.Tasks[taskID.String()]
	if !exists {
		return localTask{}, errRPCNotFound
	}
	return task, nil
}

func (repository encryptedControlTaskRepository) Create(ctx context.Context, task localTask, logs []taskLogDraft) (localTask, error) {
	if err := ctx.Err(); err != nil {
		return localTask{}, err
	}
	if task.Spec.TaskID == uuid.Nil || task.Revision == 0 || task.CreatedAt.IsZero() || !supportedTypedTask(task.Spec.TaskType) {
		return localTask{}, errRPCInvalid
	}
	result := task
	err := repository.store.update(func(state *controlPersistentState) error {
		if existing, exists := state.Tasks[task.Spec.TaskID.String()]; exists {
			if !existing.PeerManaged || !sameLocalTaskSpec(existing.Spec, task.Spec) {
				return errRPCRevision
			}
			result = existing
			return nil
		}
		for _, draft := range logs {
			if !validTaskLogDraft(draft) {
				return errRPCInvalid
			}
			appendTaskLogEvent(state, &result, draft.Stream, draft.Content, draft.At)
		}
		putLocalTask(state, &result)
		return nil
	})
	return result, err
}

func (repository encryptedControlTaskRepository) CompareAndSwap(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	next localTask,
	logs []taskLogDraft,
) (localTask, error) {
	if err := ctx.Err(); err != nil {
		return localTask{}, err
	}
	if taskID == uuid.Nil || next.Spec.TaskID != taskID || next.Revision <= expectedRevision || !supportedTypedTask(next.Spec.TaskType) {
		return localTask{}, errRPCInvalid
	}
	result := next
	err := repository.store.update(func(state *controlPersistentState) error {
		current, exists := state.Tasks[taskID.String()]
		if !exists {
			return errRPCNotFound
		}
		if current.Revision != expectedRevision || !sameLocalTaskSpec(current.Spec, next.Spec) || current.PeerManaged != next.PeerManaged {
			return errRPCRevision
		}
		for _, draft := range logs {
			if !validTaskLogDraft(draft) {
				return errRPCInvalid
			}
			appendTaskLogEvent(state, &result, draft.Stream, draft.Content, draft.At)
		}
		putLocalTask(state, &result)
		return nil
	})
	return result, err
}

func (repository encryptedControlTaskRepository) ListLogs(ctx context.Context, taskID uuid.UUID) ([]remotecontrol.TaskLog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if taskID == uuid.Nil {
		return nil, errRPCInvalid
	}
	snapshot, err := repository.store.snapshot()
	if err != nil {
		return nil, err
	}
	if _, exists := snapshot.Tasks[taskID.String()]; !exists {
		return nil, errRPCNotFound
	}
	return append([]remotecontrol.TaskLog(nil), snapshot.TaskLogs[taskID.String()]...), nil
}

func validTaskLogDraft(value taskLogDraft) bool {
	return validTaskLogStream(value.Stream) && value.Content != "" && !value.At.IsZero() && len([]byte(value.Content)) <= remotecontrol.MaximumLogContentBytes
}

var _ TaskRepository = encryptedControlTaskRepository{}
