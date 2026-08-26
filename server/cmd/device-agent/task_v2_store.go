package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const taskV2SelectColumns = `id, project_id, kind, title, cwd, scope,
    owner_workflow_task_id, parent_task_id, root_task_id, definition_json, definition_revision,
    status, revision, current_run_id, scheduled_at_ms, created_at_ms, updated_at_ms,
    started_at_ms, finished_at_ms, exit_code, result_code,
    COALESCE((SELECT MAX(task_change.sequence) FROM task_changes AS task_change WHERE task_change.task_id = tasks.id), 0),
    COALESCE((SELECT task_run.log_state FROM task_runs AS task_run WHERE task_run.id = tasks.current_run_id), 'none'),
    COALESCE((SELECT task_run.log_generation FROM task_runs AS task_run WHERE task_run.id = tasks.current_run_id), 0),
    COALESCE((SELECT task_run.log_format_version FROM task_runs AS task_run WHERE task_run.id = tasks.current_run_id), 0),
    COALESCE((SELECT task_run.log_size_bytes FROM task_runs AS task_run WHERE task_run.id = tasks.current_run_id), 0),
    COALESCE((SELECT task_run.log_sha256 FROM task_runs AS task_run WHERE task_run.id = tasks.current_run_id), ''),
    (SELECT task_run.log_updated_at_ms FROM task_runs AS task_run WHERE task_run.id = tasks.current_run_id),
    COALESCE((SELECT task_run.log_path FROM task_runs AS task_run WHERE task_run.id = tasks.current_run_id), '')`

const taskV2RunSelectColumns = `id, task_id, workflow_revision_id, parent_workflow_task_run_id,
    workflow_node_id, status, attempt, created_at_ms, started_at_ms, finished_at_ms,
    exit_code, result_code, cli_session_id, log_state, log_generation, log_format_version,
    log_size_bytes, log_sha256, log_updated_at_ms, log_path`

// Task output can be substantially more frequent than state changes. A hint
// every 250ms is enough to wake connected clients without turning raw output
// into an event stream (clients still fetch logs through the authorised RPC).
const agentEventTaskLogHintInterval = 250 * time.Millisecond

type taskV2Store struct {
	business               *businessStore
	logRoot                string
	maximumLogBytesPerTask uint64
	maximumLogBytesGlobal  uint64
	maximumChanges         int
	logHintMu              sync.Mutex
	lastLogHintAt          map[string]time.Time
	logHintProjects        map[uuid.UUID]uuid.UUID
	trailingLogHints       map[string]*taskLogTrailingHint
	logHintsClosed         bool
	fileLogMu              sync.Mutex
	fileLogPublishes       map[uuid.UUID]*taskFileLogPublishState
	fileLogClosed          bool
	fileLogWG              sync.WaitGroup
	writersMu              sync.RWMutex
	writers                map[uuid.UUID]*taskRunLogWriter
	logLeaseMu             sync.Mutex
	logLeases              map[uuid.UUID]int
	logCapacityMu          sync.Mutex
	logReservations        map[uuid.UUID]uint64
	maximumLogDiskBytes    uint64
	minimumLogDiskFree     uint64
	diskFreeBytes          func(string) (uint64, error)
	diskPressureMu         sync.Mutex
	diskPressureCount      uint64
	diskPressureReason     string
	maintenanceMu          sync.Mutex
	maintenanceCancel      context.CancelFunc
	maintenanceDone        chan struct{}
	maintenanceInterval    time.Duration
	taskLogGC              chan uuid.UUID
	taskLogMaintenanceWake chan struct{}
}

type taskFileLogPublishState struct {
	projectID          uuid.UUID
	taskID             uuid.UUID
	runID              uuid.UUID
	generation         uint64
	size               uint64
	lastCheckpointSize uint64
	lastCheckpointAt   time.Time
	lastEventAt        time.Time
	checkpointRunning  bool
	eventRunning       bool
	checkpointPending  bool
	eventPending       bool
	eventOperation     string
	trailingEventTimer *time.Timer
}

type taskLogTrailingHint struct {
	projectID uuid.UUID
	taskID    uuid.UUID
	runID     *uuid.UUID
	sequence  uint64
	occurred  time.Time
	timer     *time.Timer
}

type taskV2LogProjection struct {
	DisplayText     string
	SourceEncoding  string
	IsBinary        bool
	HadDecodeErrors bool
	RawAvailable    bool
}

type taskV2Change struct {
	Sequence   uint64    `json:"sequence"`
	TaskID     uuid.UUID `json:"taskId"`
	ProjectID  uuid.UUID `json:"projectId"`
	Revision   uint64    `json:"revision"`
	Operation  string    `json:"operation"`
	OccurredAt time.Time `json:"occurredAt"`
}

type taskV2LogPage struct {
	Items                    []taskV2Log `json:"items"`
	AckedThroughSequence     uint64      `json:"ackedThroughSequence"`
	HighWatermark            uint64      `json:"highWatermark"`
	MinimumAvailableSequence uint64      `json:"minimumAvailableSequence"`
	NextBeforeSequence       uint64      `json:"nextBeforeSequence,omitempty"`
	LineCount                uint64      `json:"lineCount,omitempty"`
	HasMore                  bool        `json:"hasMore"`
	ResetRequired            bool        `json:"resetRequired"`
}

type taskV2ChangePage struct {
	Items                    []taskV2Change `json:"items"`
	AckedThroughSequence     uint64         `json:"ackedThroughSequence"`
	HighWatermark            uint64         `json:"highWatermark"`
	MinimumAvailableSequence uint64         `json:"minimumAvailableSequence"`
	HasMore                  bool           `json:"hasMore"`
	ResetRequired            bool           `json:"resetRequired"`
}

type taskV2BatchResult struct {
	Items         []taskV2Record `json:"items"`
	AffectedCount int            `json:"affectedCount"`
	HighWatermark uint64         `json:"highWatermark"`
}

type taskV2FollowUpResult struct {
	Source        taskV2Record `json:"source"`
	FollowUp      taskV2Record `json:"followUp"`
	HighWatermark uint64       `json:"highWatermark"`
}

func newTaskV2Store(business *businessStore) *taskV2Store {
	if business == nil {
		return nil
	}
	return &taskV2Store{
		business:               business,
		logRoot:                filepath.Join(filepath.Dir(business.path), "logs", "tasks"),
		maximumLogBytesPerTask: maximumTaskLogBytesPerTask,
		maximumLogBytesGlobal:  maximumTaskLogBytesGlobal,
		maximumChanges:         maximumTaskChanges,
		lastLogHintAt:          make(map[string]time.Time),
		logHintProjects:        make(map[uuid.UUID]uuid.UUID),
		trailingLogHints:       make(map[string]*taskLogTrailingHint),
		fileLogPublishes:       make(map[uuid.UUID]*taskFileLogPublishState),
		writers:                make(map[uuid.UUID]*taskRunLogWriter),
		logLeases:              make(map[uuid.UUID]int),
		logReservations:        make(map[uuid.UUID]uint64),
		maximumLogDiskBytes:    maximumTaskLogDiskBytes,
		minimumLogDiskFree:     minimumTaskLogDiskFreeBytes,
		diskFreeBytes:          taskLogDiskFreeBytes,
		maintenanceInterval:    defaultTaskLogMaintenanceInterval,
		taskLogGC:              make(chan uuid.UUID, 256),
		taskLogMaintenanceWake: make(chan struct{}, 1),
	}
}

func (store *taskV2Store) Create(ctx context.Context, definition taskV2Definition, now time.Time) (taskV2Record, error) {
	if store == nil || store.business == nil || definition.ID == uuid.Nil || definition.ProjectID == uuid.Nil ||
		definition.Kind == "workflow" || definition.Scope != "topLevel" || now.IsZero() {
		return taskV2Record{}, errRPCInvalid
	}
	encoded, err := json.Marshal(definition)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return taskV2Record{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, err
	}
	defer tx.Rollback()
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, definition.ProjectID); err != nil {
		return taskV2Record{}, err
	}
	if err := validateTaskRelationshipsTx(ctx, tx, definition); err != nil {
		return taskV2Record{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(
        id, project_id, kind, title, cwd, scope, owner_workflow_task_id, parent_task_id, root_task_id,
        definition_json, definition_revision, status, revision, current_run_id, next_log_sequence,
        scheduled_at_ms, created_at_ms, updated_at_ms, result_code
    ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'queued', 1, NULL, 1, ?, ?, ?, '')`,
		definition.ID.String(), definition.ProjectID.String(), definition.Kind, definition.Title, definition.CWD, definition.Scope,
		nullableUUIDString(definition.OwnerWorkflowTaskID), nullableUUIDString(definition.ParentTaskID), nullableUUIDString(definition.RootTaskID),
		encoded, nullableTimeMillis(definition.Execution.ScheduledAt), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		if isSQLiteConstraint(err) {
			return taskV2Record{}, errRPCRevision
		}
		return taskV2Record{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, definition.ID, definition.ProjectID, 1, "upsert", now)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, err
	}
	return taskV2Record{
		Definition: definition, DefinitionRevision: 1, Status: "queued", Revision: 1,
		ChangeSequence: changeSequence, CreatedAt: now, UpdatedAt: now, LogState: taskLogStateNone,
	}, nil
}

func (store *taskV2Store) Get(ctx context.Context, taskID uuid.UUID) (taskV2Record, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil {
		return taskV2Record{}, errRPCInvalid
	}
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, err
	}
	defer db.Close()
	result, err := scanTaskV2(db.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, errRPCNotFound
	}
	return result, err
}

func (store *taskV2Store) List(ctx context.Context, projectID uuid.UUID) ([]taskV2Record, error) {
	if store == nil || store.business == nil || projectID == uuid.Nil {
		return nil, errRPCInvalid
	}
	db, err := store.business.openReadDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE project_id = ? ORDER BY created_at_ms DESC, id`, projectID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]taskV2Record, 0)
	for rows.Next() {
		task, err := scanTaskV2(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

type taskV2VersionedPage struct {
	Items                    []taskV2Record
	NextCursor               *string
	HighWatermark            uint64
	MinimumAvailableSequence uint64
	Start                    int
	Total                    int
}

// ListVersionedPage reads the watermark, count and requested task page from
// one read-only SQLite snapshot. The previous first-screen path decoded every
// task in a project and paginated in Go, which made a 20-item mobile list
// scale with the entire task history and could race its cursor watermark.
func (store *taskV2Store) ListVersionedPage(ctx context.Context, projectID uuid.UUID, input rpcInput) (taskV2VersionedPage, error) {
	if store == nil || store.business == nil || projectID == uuid.Nil {
		return taskV2VersionedPage{}, errRPCInvalid
	}
	db, err := store.business.openReadDB()
	if err != nil {
		return taskV2VersionedPage{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return taskV2VersionedPage{}, err
	}
	defer tx.Rollback()
	highWatermark, minimumAvailable, err := taskV2ProjectWatermarks(ctx, tx, projectID)
	if err != nil {
		return taskV2VersionedPage{}, err
	}
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE project_id = ?`, projectID.String()).Scan(&total); err != nil {
		return taskV2VersionedPage{}, err
	}
	start, end, next, err := versionedPageWindow(input, total, highWatermark)
	if err != nil {
		return taskV2VersionedPage{}, err
	}
	page := taskV2VersionedPage{
		Items: make([]taskV2Record, 0, end-start), NextCursor: next,
		HighWatermark: highWatermark, MinimumAvailableSequence: minimumAvailable,
		Start: start, Total: total,
	}
	if start == end {
		return page, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks
		WHERE project_id = ? ORDER BY created_at_ms DESC, id LIMIT ? OFFSET ?`, projectID.String(), end-start, start)
	if err != nil {
		return taskV2VersionedPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		task, scanErr := scanTaskV2(rows)
		if scanErr != nil {
			return taskV2VersionedPage{}, scanErr
		}
		page.Items = append(page.Items, task)
	}
	if err := rows.Err(); err != nil {
		return taskV2VersionedPage{}, err
	}
	return page, nil
}

// ListTopLevel returns the device-local task records that may have a bounded
// cloud projection. Definitions remain in SQLite and are never returned by a
// control-plane endpoint; the caller must explicitly construct the safe
// projection fields.
func (store *taskV2Store) ListTopLevel(ctx context.Context) ([]taskV2Record, error) {
	if store == nil || store.business == nil {
		return nil, errRPCInvalid
	}
	db, err := store.business.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE scope = 'topLevel' ORDER BY created_at_ms, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]taskV2Record, 0)
	for rows.Next() {
		task, scanErr := scanTaskV2(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

func (store *taskV2Store) ListRuns(ctx context.Context, taskID uuid.UUID) ([]taskV2Run, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil {
		return nil, errRPCInvalid
	}
	db, err := store.business.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, taskID.String()).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, errRPCNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+taskV2RunSelectColumns+`
        FROM task_runs WHERE task_id = ? ORDER BY attempt DESC, created_at_ms DESC, id`, taskID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]taskV2Run, 0)
	for rows.Next() {
		run, err := scanTaskV2Run(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func (store *taskV2Store) ListChanges(ctx context.Context, projectID uuid.UUID, afterSequence uint64, limit int) (taskV2ChangePage, error) {
	if store == nil || store.business == nil || projectID == uuid.Nil || limit < 1 || limit > 256 {
		return taskV2ChangePage{}, errRPCInvalid
	}
	db, err := store.business.openReadDB()
	if err != nil {
		return taskV2ChangePage{}, err
	}
	defer db.Close()
	highWatermark, minimumAvailable, err := taskV2ProjectWatermarks(ctx, db, projectID)
	if err != nil {
		return taskV2ChangePage{}, err
	}
	page := taskV2ChangePage{
		Items: make([]taskV2Change, 0), AckedThroughSequence: afterSequence,
		HighWatermark: highWatermark, MinimumAvailableSequence: minimumAvailable,
		ResetRequired: minimumAvailable > 0 && afterSequence+1 < minimumAvailable,
	}
	if page.ResetRequired || afterSequence >= highWatermark {
		return page, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT sequence, task_id, project_id, revision, operation, occurred_at_ms
        FROM task_changes WHERE project_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, projectID.String(), afterSequence, limit+1)
	if err != nil {
		return taskV2ChangePage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var sequence, revision uint64
		var taskIDText, projectIDText, operation string
		var occurredAt int64
		if err := rows.Scan(&sequence, &taskIDText, &projectIDText, &revision, &operation, &occurredAt); err != nil {
			return taskV2ChangePage{}, err
		}
		taskID, taskErr := uuid.Parse(taskIDText)
		storedProjectID, projectErr := uuid.Parse(projectIDText)
		if taskErr != nil || projectErr != nil || taskID == uuid.Nil || storedProjectID != projectID || sequence == 0 || revision == 0 ||
			(operation != "upsert" && operation != "delete") || occurredAt <= 0 {
			return taskV2ChangePage{}, errors.New("task change row is invalid")
		}
		if len(page.Items) == limit {
			page.HasMore = true
			continue
		}
		page.Items = append(page.Items, taskV2Change{
			Sequence: sequence, TaskID: taskID, ProjectID: storedProjectID, Revision: revision,
			Operation: operation, OccurredAt: time.UnixMilli(occurredAt).UTC(),
		})
		page.AckedThroughSequence = sequence
	}
	if err := rows.Err(); err != nil {
		return taskV2ChangePage{}, err
	}
	return page, nil
}

func (store *taskV2Store) ActivateQueue(
	ctx context.Context,
	projectID uuid.UUID,
	expectedHighWatermark *uint64,
	now time.Time,
) (taskV2BatchResult, error) {
	if store == nil || store.business == nil || projectID == uuid.Nil || now.IsZero() {
		return taskV2BatchResult{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2BatchResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2BatchResult{}, err
	}
	defer tx.Rollback()
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, projectID); err != nil {
		return taskV2BatchResult{}, err
	}
	highWatermark, _, err := taskV2ProjectWatermarks(ctx, tx, projectID)
	if err != nil {
		return taskV2BatchResult{}, err
	}
	if expectedHighWatermark != nil && *expectedHighWatermark != highWatermark {
		return taskV2BatchResult{}, errRPCRevision
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks
        WHERE project_id = ? AND scope = 'topLevel' AND status = 'queued' AND (scheduled_at_ms IS NULL OR scheduled_at_ms <= ?)
        ORDER BY created_at_ms, id`, projectID.String(), now.UnixMilli())
	if err != nil {
		return taskV2BatchResult{}, err
	}
	ready := make([]taskV2Record, 0)
	for rows.Next() {
		task, scanErr := scanTaskV2(rows)
		if scanErr != nil {
			_ = rows.Close()
			return taskV2BatchResult{}, scanErr
		}
		ready = append(ready, task)
	}
	if err := rows.Close(); err != nil {
		return taskV2BatchResult{}, err
	}
	for index := range ready {
		task := &ready[index]
		nextRevision := task.Revision + 1
		nextDefinitionRevision := task.DefinitionRevision
		if task.Definition.Execution.ScheduledAt != nil {
			task.Definition.Execution.ScheduledAt = nil
			nextDefinitionRevision++
		}
		encodedDefinition, err := json.Marshal(task.Definition)
		if err != nil || len(encodedDefinition) == 0 || len(encodedDefinition) > maximumTaskDefinitionBytes {
			return taskV2BatchResult{}, errRPCInvalid
		}
		mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'waiting', revision = ?, current_run_id = NULL,
            started_at_ms = NULL, finished_at_ms = NULL, exit_code = NULL, result_code = '', updated_at_ms = ?
			, definition_json = ?, definition_revision = ?, scheduled_at_ms = NULL
			WHERE id = ? AND revision = ? AND status = 'queued'`, nextRevision, now.UnixMilli(), encodedDefinition,
			nextDefinitionRevision, task.Definition.ID.String(), task.Revision)
		if err != nil {
			return taskV2BatchResult{}, err
		}
		if err := requireSingleTaskMutation(mutation); err != nil {
			return taskV2BatchResult{}, err
		}
		sequence, err := appendTaskV2Change(ctx, store.business, tx, task.Definition.ID, projectID, nextRevision, "upsert", now)
		if err != nil {
			return taskV2BatchResult{}, err
		}
		task.Status, task.Revision, task.DefinitionRevision, task.ChangeSequence, task.CurrentRunID =
			"waiting", nextRevision, nextDefinitionRevision, sequence, nil
		task.StartedAt, task.FinishedAt, task.ExitCode, task.ResultCode, task.UpdatedAt = nil, nil, nil, "", now
		highWatermark = sequence
	}
	if err := pruneTaskV2Changes(ctx, tx, projectID, store.maximumChanges); err != nil {
		return taskV2BatchResult{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2BatchResult{}, err
	}
	return taskV2BatchResult{Items: ready, AffectedCount: len(ready), HighWatermark: highWatermark}, nil
}

// StopAll cancels every active top-level task in one project-scoped CAS
// transaction. It is the remote equivalent of WenzMark's stop-all action: a
// stale project watermark rejects the whole batch, so a reconnect cannot stop
// a task that was created after the user pressed the button.
func (store *taskV2Store) StopAll(
	ctx context.Context,
	projectID uuid.UUID,
	expectedHighWatermark *uint64,
	now time.Time,
) (taskV2BatchResult, error) {
	if store == nil || store.business == nil || projectID == uuid.Nil || now.IsZero() {
		return taskV2BatchResult{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2BatchResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2BatchResult{}, err
	}
	defer tx.Rollback()
	highWatermark, _, err := taskV2ProjectWatermarks(ctx, tx, projectID)
	if err != nil {
		return taskV2BatchResult{}, err
	}
	if expectedHighWatermark != nil && *expectedHighWatermark != highWatermark {
		return taskV2BatchResult{}, errRPCRevision
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks
        WHERE project_id = ? AND scope = 'topLevel' AND status IN ('queued','waiting','running')
        ORDER BY created_at_ms, id`, projectID.String())
	if err != nil {
		return taskV2BatchResult{}, err
	}
	active := make([]taskV2Record, 0)
	for rows.Next() {
		task, scanErr := scanTaskV2(rows)
		if scanErr != nil {
			_ = rows.Close()
			return taskV2BatchResult{}, scanErr
		}
		active = append(active, task)
	}
	if err := rows.Close(); err != nil {
		return taskV2BatchResult{}, err
	}
	for index := range active {
		task := &active[index]
		nextRevision := task.Revision + 1
		nextDefinitionRevision := task.DefinitionRevision
		if task.Definition.Execution.ScheduledAt != nil {
			task.Definition.Execution.ScheduledAt = nil
			nextDefinitionRevision++
		}
		encodedDefinition, marshalErr := json.Marshal(task.Definition)
		if marshalErr != nil || len(encodedDefinition) == 0 || len(encodedDefinition) > maximumTaskDefinitionBytes {
			return taskV2BatchResult{}, errRPCInvalid
		}
		mutation, execErr := tx.ExecContext(ctx, `UPDATE tasks SET status = 'cancelled', revision = ?, current_run_id = ?,
            started_at_ms = started_at_ms, finished_at_ms = ?, exit_code = NULL, result_code = 'cancelled', updated_at_ms = ?,
            definition_json = ?, definition_revision = ?, scheduled_at_ms = NULL
            WHERE id = ? AND revision = ? AND status IN ('queued','waiting','running')`,
			nextRevision, nullableUUIDString(task.CurrentRunID), now.UnixMilli(), now.UnixMilli(), encodedDefinition,
			nextDefinitionRevision, task.Definition.ID.String(), task.Revision)
		if execErr != nil {
			return taskV2BatchResult{}, execErr
		}
		if err := requireSingleTaskMutation(mutation); err != nil {
			return taskV2BatchResult{}, err
		}
		if task.CurrentRunID != nil {
			if _, execErr := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'cancelled', finished_at_ms = ?, result_code = 'cancelled'
                WHERE id = ? AND task_id = ? AND status IN ('waiting','running')`, now.UnixMilli(), task.CurrentRunID.String(), task.Definition.ID.String()); execErr != nil {
				return taskV2BatchResult{}, execErr
			}
		}
		changeSequence, changeErr := appendTaskV2Change(ctx, store.business, tx, task.Definition.ID, projectID, nextRevision, "upsert", now)
		if changeErr != nil {
			return taskV2BatchResult{}, changeErr
		}
		task.Status, task.Revision, task.ChangeSequence = "cancelled", nextRevision, changeSequence
		task.FinishedAt, task.ResultCode, task.UpdatedAt = timePointer(now), "cancelled", now
	}
	if err := pruneTaskV2Changes(ctx, tx, projectID, store.maximumChanges); err != nil {
		return taskV2BatchResult{}, err
	}
	highWatermark, _, err = taskV2ProjectWatermarks(ctx, tx, projectID)
	if err != nil {
		return taskV2BatchResult{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2BatchResult{}, err
	}
	return taskV2BatchResult{Items: active, AffectedCount: len(active), HighWatermark: highWatermark}, nil
}

func (store *taskV2Store) ClearFinished(
	ctx context.Context,
	projectID uuid.UUID,
	expectedHighWatermark *uint64,
	now time.Time,
) (taskV2BatchResult, error) {
	if store == nil || store.business == nil || projectID == uuid.Nil || now.IsZero() {
		return taskV2BatchResult{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2BatchResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2BatchResult{}, err
	}
	defer tx.Rollback()
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, projectID); err != nil {
		return taskV2BatchResult{}, err
	}
	highWatermark, _, err := taskV2ProjectWatermarks(ctx, tx, projectID)
	if err != nil {
		return taskV2BatchResult{}, err
	}
	if expectedHighWatermark != nil && *expectedHighWatermark != highWatermark {
		return taskV2BatchResult{}, errRPCRevision
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks
        WHERE project_id = ? AND scope = 'topLevel' AND status IN ('failed','blocked','cancelled','completed','succeeded')
		ORDER BY created_at_ms, id`, projectID.String())
	if err != nil {
		return taskV2BatchResult{}, err
	}
	finished := make([]taskV2Record, 0)
	for rows.Next() {
		task, scanErr := scanTaskV2(rows)
		if scanErr != nil {
			_ = rows.Close()
			return taskV2BatchResult{}, scanErr
		}
		finished = append(finished, task)
	}
	if err := rows.Close(); err != nil {
		return taskV2BatchResult{}, err
	}
	for _, task := range finished {
		sequence, err := deleteTaskV2RecordTx(ctx, store.business, tx, task, now)
		if err != nil {
			return taskV2BatchResult{}, err
		}
		highWatermark = sequence
	}
	if err := pruneTaskV2Changes(ctx, tx, projectID, store.maximumChanges); err != nil {
		return taskV2BatchResult{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2BatchResult{}, err
	}
	for _, task := range finished {
		taskID := task.Definition.ID
		store.queueTaskLogDirectoryGC(taskID)
	}
	return taskV2BatchResult{Items: []taskV2Record{}, AffectedCount: len(finished), HighWatermark: highWatermark}, nil
}

func (store *taskV2Store) UpdateDefinition(
	ctx context.Context,
	definition taskV2Definition,
	expectedRevision uint64,
	now time.Time,
) (taskV2Record, error) {
	if store == nil || store.business == nil || expectedRevision == 0 || definition.Kind == "workflow" ||
		definition.Scope != "topLevel" || now.IsZero() {
		return taskV2Record{}, errRPCInvalid
	}
	encoded, err := json.Marshal(definition)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return taskV2Record{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, definition.ID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, err
	}
	if current.Revision != expectedRevision || current.Definition.ProjectID != definition.ProjectID || current.Definition.Kind == "workflow" ||
		current.Definition.Scope != "topLevel" || current.Status == "running" || current.Status == "waiting" || current.Status == "awaitingAcceptance" {
		return taskV2Record{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, definition.ProjectID); err != nil {
		return taskV2Record{}, err
	}
	if err := validateTaskRelationshipsTx(ctx, tx, definition); err != nil {
		return taskV2Record{}, err
	}
	nextDefinitionRevision, nextRevision := current.DefinitionRevision+1, current.Revision+1
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET kind = ?, title = ?, cwd = ?, scope = ?, owner_workflow_task_id = ?,
        parent_task_id = ?, root_task_id = ?, definition_json = ?, definition_revision = ?, revision = ?, scheduled_at_ms = ?, updated_at_ms = ?
        WHERE id = ? AND revision = ? AND status NOT IN ('running','waiting','awaitingAcceptance')`,
		definition.Kind, definition.Title, definition.CWD, definition.Scope, nullableUUIDString(definition.OwnerWorkflowTaskID),
		nullableUUIDString(definition.ParentTaskID), nullableUUIDString(definition.RootTaskID), encoded, nextDefinitionRevision,
		nextRevision, nullableTimeMillis(definition.Execution.ScheduledAt), now.UnixMilli(), definition.ID.String(), expectedRevision)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, definition.ID, definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, err
	}
	current.Definition, current.DefinitionRevision, current.Revision = definition, nextDefinitionRevision, nextRevision
	current.ChangeSequence, current.UpdatedAt = changeSequence, now
	return current, nil
}

// StartNow applies the task-card "run now" semantics used by WenzMark.
// Queue-wide activation continues to preserve the configured serial lane,
// while an explicit per-task start promotes standalone work to the parallel
// lane so it can overlap an already running serial task.
func (store *taskV2Store) StartNow(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	now time.Time,
) (taskV2Record, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 || now.IsZero() {
		return taskV2Record{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, err
	}
	if current.Revision != expectedRevision || (current.Status != "queued" && current.Status != "waiting") {
		return taskV2Record{}, errRPCRevision
	}
	if current.Definition.Scope != "topLevel" || current.Definition.Execution.WorkflowID != nil {
		return taskV2Record{}, errRPCInvalid
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, current.Definition.ProjectID); err != nil {
		return taskV2Record{}, err
	}

	// A waiting workflow is already owned by the workflow scheduler. Waking the
	// engine is sufficient and its definition must not be rewritten as parallel.
	if current.Status == "waiting" && current.Definition.Kind == "workflow" {
		return current, nil
	}
	if current.Definition.Kind == "workflow" && current.Status != "queued" {
		return taskV2Record{}, errRPCInvalid
	}

	definitionChanged := false
	if current.Definition.Kind != "workflow" {
		if current.Definition.Execution.Mode != "parallel" {
			current.Definition.Execution.Mode = "parallel"
			definitionChanged = true
		}
		if current.Status == "queued" && !current.Definition.Execution.RunImmediately {
			current.Definition.Execution.RunImmediately = true
			definitionChanged = true
		}
	}
	if current.Definition.Execution.ScheduledAt != nil {
		current.Definition.Execution.ScheduledAt = nil
		definitionChanged = true
	}
	if current.Status == "waiting" && !definitionChanged {
		return current, nil
	}

	encodedDefinition, err := json.Marshal(current.Definition)
	if err != nil || len(encodedDefinition) == 0 || len(encodedDefinition) > maximumTaskDefinitionBytes {
		return taskV2Record{}, errRPCInvalid
	}
	nextRevision := current.Revision + 1
	nextDefinitionRevision := current.DefinitionRevision
	if definitionChanged {
		nextDefinitionRevision++
	}
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'waiting', revision = ?, current_run_id = NULL,
		started_at_ms = NULL, finished_at_ms = NULL, exit_code = NULL, result_code = '', updated_at_ms = ?,
		definition_json = ?, definition_revision = ?, scheduled_at_ms = NULL
		WHERE id = ? AND revision = ? AND status = ?`,
		nextRevision, now.UnixMilli(), encodedDefinition, nextDefinitionRevision,
		taskID.String(), expectedRevision, current.Status)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, current.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, current.Definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, err
	}
	current.Status, current.Revision, current.DefinitionRevision = "waiting", nextRevision, nextDefinitionRevision
	current.ChangeSequence, current.UpdatedAt = changeSequence, now
	current.CurrentRunID, current.StartedAt, current.FinishedAt, current.ExitCode = nil, nil, nil, nil
	current.ResultCode = ""
	return current, nil
}

func (store *taskV2Store) CreateFollowUp(
	ctx context.Context,
	sourceTaskID uuid.UUID,
	expectedSourceRevision uint64,
	followUp taskV2Definition,
	now time.Time,
) (taskV2FollowUpResult, error) {
	if store == nil || store.business == nil || sourceTaskID == uuid.Nil || expectedSourceRevision == 0 ||
		followUp.ID == uuid.Nil || followUp.ID == sourceTaskID || now.IsZero() {
		return taskV2FollowUpResult{}, errRPCInvalid
	}
	encoded, err := json.Marshal(followUp)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return taskV2FollowUpResult{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2FollowUpResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2FollowUpResult{}, err
	}
	defer tx.Rollback()
	source, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, sourceTaskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2FollowUpResult{}, errRPCNotFound
	}
	if err != nil {
		return taskV2FollowUpResult{}, err
	}
	if source.Revision != expectedSourceRevision ||
		(source.Status != "awaitingAcceptance" && source.Status != "failed" && source.Status != "blocked" && source.Status != "cancelled") {
		return taskV2FollowUpResult{}, errRPCRevision
	}
	rootID := source.Definition.ID
	if source.Definition.RootTaskID != nil {
		rootID = *source.Definition.RootTaskID
	}
	if source.Definition.ProjectID != followUp.ProjectID || source.Definition.Scope != "topLevel" || followUp.Scope != "topLevel" ||
		source.Definition.Kind != followUp.Kind || followUp.Kind == "script" || followUp.Kind == "workflow" ||
		followUp.ParentTaskID == nil || *followUp.ParentTaskID != source.Definition.ID ||
		followUp.RootTaskID == nil || *followUp.RootTaskID != rootID || followUp.OwnerWorkflowTaskID != nil ||
		followUp.Execution.WorkflowID != nil || followUp.Execution.Relation != "dependency" || followUp.Execution.Mode != "serial" ||
		!followUp.Execution.RunImmediately || followUp.Execution.ScheduledAt != nil || len(followUp.Execution.RelatedTaskIDs) != 1 ||
		followUp.Execution.RelatedTaskIDs[0] != source.Definition.ID || followUp.AcceptanceFeedback == "" {
		return taskV2FollowUpResult{}, errRPCInvalid
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, followUp.ProjectID); err != nil {
		return taskV2FollowUpResult{}, err
	}
	if err := validateTaskRelationshipsTx(ctx, tx, followUp); err != nil {
		return taskV2FollowUpResult{}, err
	}
	highWatermark := source.ChangeSequence
	if source.Status == "awaitingAcceptance" {
		nextRevision := source.Revision + 1
		mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'changesRequested', revision = ?, result_code = 'follow_up_created', updated_at_ms = ?
            WHERE id = ? AND revision = ? AND status = 'awaitingAcceptance'`, nextRevision, now.UnixMilli(), sourceTaskID.String(), expectedSourceRevision)
		if err != nil {
			return taskV2FollowUpResult{}, err
		}
		if err := requireSingleTaskMutation(mutation); err != nil {
			return taskV2FollowUpResult{}, err
		}
		sequence, err := appendTaskV2Change(ctx, store.business, tx, sourceTaskID, followUp.ProjectID, nextRevision, "upsert", now)
		if err != nil {
			return taskV2FollowUpResult{}, err
		}
		source.Status, source.Revision, source.ResultCode, source.UpdatedAt, source.ChangeSequence =
			"changesRequested", nextRevision, "follow_up_created", now, sequence
		highWatermark = sequence
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(
        id, project_id, kind, title, cwd, scope, owner_workflow_task_id, parent_task_id, root_task_id,
        definition_json, definition_revision, status, revision, current_run_id, next_log_sequence,
        scheduled_at_ms, created_at_ms, updated_at_ms, result_code
    ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'waiting', 1, NULL, 1, NULL, ?, ?, '')`,
		followUp.ID.String(), followUp.ProjectID.String(), followUp.Kind, followUp.Title, followUp.CWD, followUp.Scope,
		nullableUUIDString(followUp.OwnerWorkflowTaskID), nullableUUIDString(followUp.ParentTaskID), nullableUUIDString(followUp.RootTaskID),
		encoded, now.UnixMilli(), now.UnixMilli()); err != nil {
		if isSQLiteConstraint(err) {
			return taskV2FollowUpResult{}, errRPCRevision
		}
		return taskV2FollowUpResult{}, err
	}
	followUpSequence, err := appendTaskV2Change(ctx, store.business, tx, followUp.ID, followUp.ProjectID, 1, "upsert", now)
	if err != nil {
		return taskV2FollowUpResult{}, err
	}
	highWatermark = followUpSequence
	if err := pruneTaskV2Changes(ctx, tx, followUp.ProjectID, store.maximumChanges); err != nil {
		return taskV2FollowUpResult{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2FollowUpResult{}, err
	}
	created := taskV2Record{
		Definition: followUp, DefinitionRevision: 1, Status: "waiting", Revision: 1, ChangeSequence: followUpSequence,
		CreatedAt: now, UpdatedAt: now, LogState: taskLogStateNone,
	}
	return taskV2FollowUpResult{Source: source, FollowUp: created, HighWatermark: highWatermark}, nil
}

func (store *taskV2Store) Transition(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	nextStatus string,
	resultCode string,
	now time.Time,
) (taskV2Record, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 || !validTaskV2Status(nextStatus) || now.IsZero() || !validTaskResultCode(resultCode) {
		return taskV2Record{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, err
	}
	if current.Revision != expectedRevision || !validTaskV2Transition(current.Status, nextStatus) {
		return taskV2Record{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, current.Definition.ProjectID); err != nil {
		return taskV2Record{}, err
	}
	nextRevision := current.Revision + 1
	nextDefinitionRevision := current.DefinitionRevision
	startedAt, finishedAt := current.StartedAt, current.FinishedAt
	currentRun := current.CurrentRunID
	var exitCode any = nullableInt(current.ExitCode)
	if (nextStatus == "waiting" || nextStatus == "cancelled") && current.Definition.Execution.ScheduledAt != nil {
		current.Definition.Execution.ScheduledAt = nil
		nextDefinitionRevision++
	}
	encodedDefinition, err := json.Marshal(current.Definition)
	if err != nil || len(encodedDefinition) == 0 || len(encodedDefinition) > maximumTaskDefinitionBytes {
		return taskV2Record{}, errRPCInvalid
	}
	if nextStatus == "waiting" {
		startedAt, finishedAt, currentRun, exitCode, resultCode = nil, nil, nil, nil, ""
	}
	if nextStatus == "cancelled" || nextStatus == "completed" || nextStatus == "changesRequested" || nextStatus == "failed" || nextStatus == "blocked" || nextStatus == "succeeded" {
		finishedAt = timePointer(now)
	}
	if nextStatus == "awaitingAcceptance" && current.CurrentRunID != nil &&
		(current.Status == "completed" || current.Status == "succeeded") {
		runMutation, runErr := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'awaitingAcceptance'
			WHERE id = ? AND task_id = ? AND status = ?`, current.CurrentRunID.String(), taskID.String(), current.Status)
		if runErr != nil {
			return taskV2Record{}, runErr
		}
		if err := requireSingleTaskMutation(runMutation); err != nil {
			return taskV2Record{}, err
		}
	}
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, revision = ?, current_run_id = ?, started_at_ms = ?, finished_at_ms = ?,
		exit_code = ?, result_code = ?, updated_at_ms = ?, definition_json = ?, definition_revision = ?, scheduled_at_ms = ?
		WHERE id = ? AND revision = ? AND status = ?`,
		nextStatus, nextRevision, nullableUUIDString(currentRun), nullableTimeMillis(startedAt), nullableTimeMillis(finishedAt), exitCode,
		resultCode, now.UnixMilli(), encodedDefinition, nextDefinitionRevision, nullableTimeMillis(current.Definition.Execution.ScheduledAt),
		taskID.String(), expectedRevision, current.Status)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, current.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, current.Definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, err
	}
	current.Status, current.Revision, current.DefinitionRevision, current.ChangeSequence, current.UpdatedAt =
		nextStatus, nextRevision, nextDefinitionRevision, changeSequence, now
	current.StartedAt, current.FinishedAt, current.CurrentRunID, current.ResultCode = startedAt, finishedAt, currentRun, resultCode
	if currentRun == nil {
		current.LogAvailable, current.LogState, current.LogGeneration, current.LogSizeBytes = false, taskLogStateNone, 0, 0
		current.LogFormatVersion, current.LogSHA256, current.LogUpdatedAt, current.LegacyLogPath = 0, "", nil, ""
	}
	if exitCode == nil {
		current.ExitCode = nil
	}
	return current, nil
}

func (store *taskV2Store) StartRun(ctx context.Context, taskID uuid.UUID, expectedRevision uint64, now time.Time) (taskV2Record, taskV2Run, error) {
	return store.startRun(ctx, taskID, expectedRevision, "", now)
}

func (store *taskV2Store) StartRunWithLogRoot(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	logRoot string,
	now time.Time,
) (taskV2Record, taskV2Run, error) {
	return store.startRun(ctx, taskID, expectedRevision, logRoot, now)
}

func (store *taskV2Store) startRun(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	logRoot string,
	now time.Time,
) (taskV2Record, taskV2Run, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 || now.IsZero() {
		return taskV2Record{}, taskV2Run{}, errRPCInvalid
	}
	// logRoot is accepted only for source compatibility with older local
	// callers. New runs always derive their private path from the state-file
	// directory and never persist an absolute path.
	if logRoot != "" {
		if _, err := normalizeTaskRunLogRoot(logRoot); err != nil {
			return taskV2Record{}, taskV2Run{}, err
		}
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, taskV2Run{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if current.Revision != expectedRevision || current.Status != "waiting" {
		return taskV2Record{}, taskV2Run{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, current.Definition.ProjectID); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	var run taskV2Run
	if current.Definition.Scope == "workflowNode" && current.CurrentRunID != nil {
		run, err = scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
			FROM task_runs WHERE id = ?`, current.CurrentRunID.String()))
		if err != nil || run.TaskID != taskID || run.Status != "waiting" || run.WorkflowRevisionID == nil ||
			run.ParentWorkflowTaskRunID == nil || run.WorkflowNodeID == "" {
			return taskV2Record{}, taskV2Run{}, errRPCRevision
		}
		parentRun, err := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
			FROM task_runs WHERE id = ?`, run.ParentWorkflowTaskRunID.String()))
		if err != nil || parentRun.Status != "running" || parentRun.WorkflowRevisionID == nil || *parentRun.WorkflowRevisionID != *run.WorkflowRevisionID {
			return taskV2Record{}, taskV2Run{}, errRPCRevision
		}
		parent, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, parentRun.TaskID.String()))
		if err != nil || parent.Definition.Kind != "workflow" || parent.Status != "running" || parent.CurrentRunID == nil ||
			*parent.CurrentRunID != parentRun.ID || current.Definition.OwnerWorkflowTaskID == nil ||
			*current.Definition.OwnerWorkflowTaskID != parent.Definition.ID {
			return taskV2Record{}, taskV2Run{}, errRPCRevision
		}
		runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'running', started_at_ms = ?, finished_at_ms = NULL,
			exit_code = NULL, result_code = '', log_path = '', log_state = 'creating', log_generation = 1,
			log_format_version = 1, log_size_bytes = 0, log_sha256 = '', log_updated_at_ms = ?
			WHERE id = ? AND task_id = ? AND status = 'waiting'`,
			now.UnixMilli(), now.UnixMilli(), run.ID.String(), taskID.String())
		if err != nil {
			return taskV2Record{}, taskV2Run{}, err
		}
		if err := requireSingleTaskMutation(runMutation); err != nil {
			return taskV2Record{}, taskV2Run{}, err
		}
		run.Status, run.StartedAt, run.FinishedAt, run.ExitCode, run.ResultCode = "running", timePointer(now), nil, nil, ""
		run.LogState, run.LogGeneration, run.LogFormatVersion, run.LogUpdatedAt = taskLogStateCreating, 1, 1, timePointer(now)
	} else {
		var nextAttempt int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), -1) + 1 FROM task_runs WHERE task_id = ?`, taskID.String()).Scan(&nextAttempt); err != nil {
			return taskV2Record{}, taskV2Run{}, err
		}
		if nextAttempt < 0 || uint64(nextAttempt) > uint64(^uint32(0)) {
			return taskV2Record{}, taskV2Run{}, errRPCBusy
		}
		run = taskV2Run{
			ID: uuid.New(), TaskID: taskID, Status: "running", Attempt: uint32(nextAttempt), CreatedAt: now, StartedAt: timePointer(now),
			LogState: taskLogStateCreating, LogGeneration: 1, LogFormatVersion: 1, LogUpdatedAt: timePointer(now),
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_runs(
			id, task_id, status, attempt, created_at_ms, started_at_ms, log_path,
			log_state, log_generation, log_format_version, log_size_bytes, log_sha256, log_updated_at_ms)
			VALUES(?, ?, 'running', ?, ?, ?, '', 'creating', 1, 1, 0, '', ?)`,
			run.ID.String(), taskID.String(), run.Attempt, now.UnixMilli(), now.UnixMilli(), now.UnixMilli()); err != nil {
			return taskV2Record{}, taskV2Run{}, err
		}
	}
	nextRevision := current.Revision + 1
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'running', revision = ?, current_run_id = ?, started_at_ms = ?,
        finished_at_ms = NULL, exit_code = NULL, result_code = '', updated_at_ms = ? WHERE id = ? AND revision = ? AND status = 'waiting'`,
		nextRevision, run.ID.String(), now.UnixMilli(), now.UnixMilli(), taskID.String(), expectedRevision)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, current.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, current.Definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	current.Status, current.Revision, current.ChangeSequence, current.CurrentRunID = "running", nextRevision, changeSequence, &run.ID
	current.StartedAt, current.FinishedAt, current.ExitCode, current.ResultCode, current.UpdatedAt = timePointer(now), nil, nil, "", now
	current.LogAvailable, current.LogState, current.LogGeneration = false, taskLogStateCreating, 1
	current.LogFormatVersion, current.LogSizeBytes, current.LogSHA256, current.LogUpdatedAt, current.LegacyLogPath = 1, 0, "", timePointer(now), ""
	return current, run, nil
}

func (store *taskV2Store) FinishRun(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	status string,
	exitCode int,
	resultCode string,
	cliSessionID string,
	now time.Time,
) (taskV2Record, taskV2Run, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 ||
		!validTaskV2Transition("running", status) || len(cliSessionID) > 512 || cliSessionID != "" && !validTaskCliSessionID(cliSessionID) ||
		!validTaskResultCode(resultCode) || now.IsZero() {
		return taskV2Record{}, taskV2Run{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return taskV2Record{}, taskV2Run{}, errRPCNotFound
		}
		return taskV2Record{}, taskV2Run{}, err
	}
	if current.Revision != expectedRevision || current.Status != "running" || current.CurrentRunID == nil {
		return taskV2Record{}, taskV2Run{}, errRPCRevision
	}
	run, err := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
        FROM task_runs WHERE id = ?`, current.CurrentRunID.String()))
	if err != nil || run.Status != "running" {
		return taskV2Record{}, taskV2Run{}, errRPCRevision
	}
	runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = ?, finished_at_ms = ?, exit_code = ?, result_code = ?, cli_session_id = ?
	        WHERE id = ? AND status = 'running'`, status, now.UnixMilli(), exitCode, resultCode, cliSessionID, run.ID.String())
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if err := requireSingleTaskMutation(runMutation); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	nextRevision := current.Revision + 1
	nextDefinitionRevision := current.DefinitionRevision
	if cliSessionID != "" && current.Definition.Execution.CliSessionID != cliSessionID {
		current.Definition.Execution.CliSessionID = cliSessionID
		nextDefinitionRevision++
	}
	encodedDefinition, err := json.Marshal(current.Definition)
	if err != nil || len(encodedDefinition) == 0 || len(encodedDefinition) > maximumTaskDefinitionBytes {
		return taskV2Record{}, taskV2Run{}, errRPCInvalid
	}
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, revision = ?, finished_at_ms = ?, exit_code = ?, result_code = ?,
		updated_at_ms = ?, definition_json = ?, definition_revision = ?
		WHERE id = ? AND revision = ? AND status = 'running' AND current_run_id = ?`, status, nextRevision,
		now.UnixMilli(), exitCode, resultCode, now.UnixMilli(), encodedDefinition, nextDefinitionRevision,
		taskID.String(), expectedRevision, run.ID.String())
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, current.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, current.Definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, taskV2Run{}, err
	}
	run.Status, run.FinishedAt, run.ExitCode, run.ResultCode, run.CliSessionID = status, timePointer(now), &exitCode, resultCode, cliSessionID
	current.Status, current.Revision, current.DefinitionRevision, current.ChangeSequence, current.FinishedAt, current.ExitCode =
		status, nextRevision, nextDefinitionRevision, changeSequence, timePointer(now), &exitCode
	current.ResultCode, current.UpdatedAt = resultCode, now
	return current, run, nil
}

// Accept commits user-visible acceptance state. Post-run status changes do not
// append to an already sealed execution log and never create unscoped log rows.
func (store *taskV2Store) Accept(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	content []byte,
	now time.Time,
) (taskV2Record, taskV2Log, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 ||
		len(content) == 0 || len(content) > maximumTaskLogEntryBytes || now.IsZero() {
		return taskV2Record{}, taskV2Log{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, taskV2Log{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	if current.Revision != expectedRevision || !validTaskV2Transition(current.Status, "completed") {
		return taskV2Record{}, taskV2Log{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, current.Definition.ProjectID); err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	nextRevision := current.Revision + 1
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'completed', revision = ?, finished_at_ms = ?,
		result_code = 'accepted', updated_at_ms = ? WHERE id = ? AND revision = ? AND status = ?`,
		nextRevision, now.UnixMilli(), now.UnixMilli(), taskID.String(), expectedRevision, current.Status)
	if err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	if current.CurrentRunID != nil {
		runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'completed'
			WHERE id = ? AND task_id = ? AND status IN ('awaitingAcceptance','changesRequested')`,
			current.CurrentRunID.String(), taskID.String())
		if err != nil {
			return taskV2Record{}, taskV2Log{}, err
		}
		if err := requireSingleTaskMutation(runMutation); err != nil {
			return taskV2Record{}, taskV2Log{}, err
		}
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, current.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, current.Definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, taskV2Log{}, err
	}
	current.Status, current.Revision, current.ChangeSequence = "completed", nextRevision, changeSequence
	current.FinishedAt, current.ResultCode, current.UpdatedAt = timePointer(now), "accepted", now
	return current, taskV2Log{}, nil
}

func (store *taskV2Store) AppendLog(ctx context.Context, taskID uuid.UUID, runID *uuid.UUID, stream string, content []byte, now time.Time) (taskV2Log, error) {
	return store.appendLogWithProjection(ctx, taskID, runID, stream, content, taskV2LogProjection{RawAvailable: true}, now)
}

// AppendDecodedLog records raw process bytes and the deterministic display
// projection separately. Content remains the retained BLOB used for forensic
// troubleshooting and byte limits; display metadata is safe for task RPCs.
func (store *taskV2Store) AppendDecodedLog(
	ctx context.Context,
	taskID uuid.UUID,
	runID *uuid.UUID,
	stream string,
	raw []byte,
	displayText, sourceEncoding string,
	isBinary, hadDecodeErrors, rawAvailable bool,
	now time.Time,
) (taskV2Log, error) {
	if !utf8.ValidString(displayText) || !utf8.ValidString(sourceEncoding) || len(displayText) > maximumTaskLogEntryBytes || len(sourceEncoding) > 64 {
		return taskV2Log{}, errRPCInvalid
	}
	if isBinary && sourceEncoding != "binary" {
		return taskV2Log{}, errRPCInvalid
	}
	return store.appendLogWithProjection(ctx, taskID, runID, stream, raw, taskV2LogProjection{
		DisplayText: displayText, SourceEncoding: sourceEncoding, IsBinary: isBinary,
		HadDecodeErrors: hadDecodeErrors, RawAvailable: rawAvailable,
	}, now)
}

func (store *taskV2Store) appendLogWithProjection(
	ctx context.Context,
	taskID uuid.UUID,
	runID *uuid.UUID,
	stream string,
	content []byte,
	projection taskV2LogProjection,
	now time.Time,
) (taskV2Log, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || !validTaskV2LogStream(stream) || len(content) == 0 || len(content) > maximumTaskLogEntryBytes || now.IsZero() {
		return taskV2Log{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Log{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Log{}, err
	}
	defer tx.Rollback()
	entry, err := appendTaskV2LogProjectionTx(ctx, tx, taskID, runID, stream, content, projection, now, store.maximumLogBytesPerTask, store.maximumLogBytesGlobal)
	if err != nil {
		return taskV2Log{}, err
	}
	hintKey := taskLogHintKey(taskID, runID)
	projectID, err := store.taskLogHintProject(ctx, tx, taskID)
	if err != nil {
		return taskV2Log{}, err
	}
	emittedLogHint := false
	if store.shouldEmitTaskLogHint(hintKey, now) {
		if _, err := store.business.appendAgentEvent(ctx, tx, newTaskLogsAvailableAgentEvent(projectID, taskID, runID, entry.Sequence, now)); err != nil {
			return taskV2Log{}, err
		}
		emittedLogHint = true
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Log{}, err
	}
	if emittedLogHint {
		store.recordTaskLogHint(hintKey, now)
	}
	store.scheduleTrailingTaskLogHint(hintKey, projectID, taskID, runID, entry.Sequence, now)
	return entry, nil
}

func taskLogHintKey(taskID uuid.UUID, runID *uuid.UUID) string {
	run := ""
	if runID != nil && *runID != uuid.Nil {
		run = runID.String()
	}
	return taskID.String() + "\x00" + run
}

func (store *taskV2Store) shouldEmitTaskLogHint(hintKey string, now time.Time) bool {
	if store == nil || store.lastLogHintAt == nil {
		return true
	}
	store.logHintMu.Lock()
	defer store.logHintMu.Unlock()
	last := store.lastLogHintAt[hintKey]
	return last.IsZero() || now.Sub(last) >= agentEventTaskLogHintInterval
}

func (store *taskV2Store) taskLogHintProject(ctx context.Context, tx *sql.Tx, taskID uuid.UUID) (uuid.UUID, error) {
	if store == nil || tx == nil || taskID == uuid.Nil {
		return uuid.Nil, errRPCInvalid
	}
	store.logHintMu.Lock()
	projectID := store.logHintProjects[taskID]
	store.logHintMu.Unlock()
	if projectID != uuid.Nil {
		return projectID, nil
	}
	var projectText string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, taskID.String()).Scan(&projectText); err != nil {
		return uuid.Nil, err
	}
	parsed, err := uuid.Parse(projectText)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, errRPCInvalid
	}
	store.logHintMu.Lock()
	if len(store.logHintProjects) >= 8192 {
		store.logHintProjects = make(map[uuid.UUID]uuid.UUID)
	}
	store.logHintProjects[taskID] = parsed
	store.logHintMu.Unlock()
	return parsed, nil
}

func (store *taskV2Store) recordTaskLogHint(hintKey string, occurred time.Time) {
	if store == nil || hintKey == "" || occurred.IsZero() {
		return
	}
	store.logHintMu.Lock()
	if !store.logHintsClosed {
		store.lastLogHintAt[hintKey] = occurred.UTC()
	}
	store.logHintMu.Unlock()
}

func (store *taskV2Store) scheduleTrailingTaskLogHint(hintKey string, projectID, taskID uuid.UUID, runID *uuid.UUID, sequence uint64, occurred time.Time) {
	if store == nil || hintKey == "" || projectID == uuid.Nil || taskID == uuid.Nil || sequence == 0 || occurred.IsZero() {
		return
	}
	var runCopy *uuid.UUID
	if runID != nil && *runID != uuid.Nil {
		value := *runID
		runCopy = &value
	}
	hint := &taskLogTrailingHint{
		projectID: projectID, taskID: taskID, runID: runCopy, sequence: sequence, occurred: occurred.UTC(),
	}
	store.logHintMu.Lock()
	if store.logHintsClosed {
		store.logHintMu.Unlock()
		return
	}
	if previous := store.trailingLogHints[hintKey]; previous != nil && previous.timer != nil {
		previous.timer.Stop()
	}
	store.trailingLogHints[hintKey] = hint
	hint.timer = time.AfterFunc(agentEventTaskLogHintInterval, func() {
		store.flushTrailingTaskLogHint(hintKey, hint)
	})
	store.logHintMu.Unlock()
}

func (store *taskV2Store) flushTrailingTaskLogHint(hintKey string, hint *taskLogTrailingHint) {
	if store == nil || hint == nil {
		return
	}
	store.logHintMu.Lock()
	if store.logHintsClosed || store.trailingLogHints[hintKey] != hint {
		store.logHintMu.Unlock()
		return
	}
	delete(store.trailingLogHints, hintKey)
	store.logHintMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := store.business.appendAgentEvent(ctx, tx, newTaskLogsAvailableAgentEvent(hint.projectID, hint.taskID, hint.runID, hint.sequence, time.Now().UTC())); err != nil {
		return
	}
	if commitBusinessTransaction(ctx, tx) != nil {
		return
	}
	// Keep logical output time as the debounce watermark. A new burst after the
	// quiet interval therefore gets an immediate leading hint even though the
	// trailing event itself was committed slightly later.
	store.recordTaskLogHint(hintKey, hint.occurred)
}

func (store *taskV2Store) closeTaskLogHints() {
	if store == nil {
		return
	}
	store.logHintMu.Lock()
	store.logHintsClosed = true
	for _, hint := range store.trailingLogHints {
		if hint != nil && hint.timer != nil {
			hint.timer.Stop()
		}
	}
	store.trailingLogHints = make(map[string]*taskLogTrailingHint)
	store.logHintMu.Unlock()
}

func appendTaskV2LogTx(
	ctx context.Context,
	tx *sql.Tx,
	taskID uuid.UUID,
	runID *uuid.UUID,
	stream string,
	content []byte,
	now time.Time,
	maximumPerTask uint64,
	maximumGlobal uint64,
) (taskV2Log, error) {
	return appendTaskV2LogProjectionTx(ctx, tx, taskID, runID, stream, content, taskV2LogProjection{RawAvailable: true}, now, maximumPerTask, maximumGlobal)
}

func appendTaskV2LogProjectionTx(
	ctx context.Context,
	tx *sql.Tx,
	taskID uuid.UUID,
	runID *uuid.UUID,
	stream string,
	content []byte,
	projection taskV2LogProjection,
	now time.Time,
	maximumPerTask uint64,
	maximumGlobal uint64,
) (taskV2Log, error) {
	if len(content) == 0 {
		return taskV2Log{}, errRPCInvalid
	}
	// A task log page is line-oriented. Split at newlines where possible and
	// hard-wrap long lines so a single RPC item can never exceed 1 KiB. This is
	// done at the Agent boundary, keeping the message layer independent from
	// the Carrier/Link framing and making reverse (tail) pagination cheap.
	rawParts := splitTaskLogBytes(content)
	displayParts := splitTaskLogDisplay(projection.DisplayText)
	partCount := len(rawParts)
	if len(displayParts) > partCount {
		partCount = len(displayParts)
	}
	if partCount == 0 {
		partCount = 1
	}
	var last taskV2Log
	for index := 0; index < partCount; index++ {
		raw := []byte(nil)
		rawAvailable := projection.RawAvailable && index < len(rawParts)
		if index < len(rawParts) {
			raw = rawParts[index]
		}
		display := ""
		if index < len(displayParts) {
			display = displayParts[index]
		}
		if len(raw) == 0 {
			raw = []byte(display)
		}
		if len(raw) == 0 {
			continue
		}
		entry, err := appendTaskV2LogPartTx(ctx, tx, taskID, runID, stream, raw, taskV2LogProjection{
			DisplayText: display, SourceEncoding: projection.SourceEncoding, IsBinary: projection.IsBinary,
			HadDecodeErrors: projection.HadDecodeErrors, RawAvailable: rawAvailable,
		}, now, maximumPerTask, maximumGlobal)
		if err != nil {
			return taskV2Log{}, err
		}
		last = entry
	}
	if last.Sequence == 0 {
		return taskV2Log{}, errRPCInvalid
	}
	return last, nil
}

func appendTaskV2LogPartTx(
	ctx context.Context,
	tx *sql.Tx,
	taskID uuid.UUID,
	runID *uuid.UUID,
	stream string,
	content []byte,
	projection taskV2LogProjection,
	now time.Time,
	maximumPerTask uint64,
	maximumGlobal uint64,
) (taskV2Log, error) {
	var nextSequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT next_log_sequence FROM tasks WHERE id = ?`, taskID.String()).Scan(&nextSequence); errors.Is(err, sql.ErrNoRows) {
		return taskV2Log{}, errRPCNotFound
	} else if err != nil || nextSequence == 0 {
		return taskV2Log{}, err
	}
	if runID != nil {
		var linkedTask string
		if err := tx.QueryRowContext(ctx, `SELECT task_id FROM task_runs WHERE id = ?`, runID.String()).Scan(&linkedTask); err != nil || linkedTask != taskID.String() {
			return taskV2Log{}, errRPCRevision
		}
	}
	if err := evictTaskLogBytes(ctx, tx, taskID, uint64(len(content)), maximumPerTask); err != nil {
		return taskV2Log{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_logs(
            task_id, run_id, sequence, stream, content, display_content, source_encoding,
            is_binary, had_decode_errors, raw_available, byte_count, occurred_at_ms
        ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID.String(), nullableUUIDString(runID), nextSequence, stream, content, projection.DisplayText, projection.SourceEncoding,
		boolToSQLite(projection.IsBinary), boolToSQLite(projection.HadDecodeErrors), boolToSQLite(projection.RawAvailable), len(content), now.UnixMilli()); err != nil {
		return taskV2Log{}, err
	}
	sequenceMutation, err := tx.ExecContext(ctx, `UPDATE tasks SET next_log_sequence = ? WHERE id = ? AND next_log_sequence = ?`, nextSequence+1, taskID.String(), nextSequence)
	if err != nil {
		return taskV2Log{}, err
	}
	if err := requireSingleTaskMutation(sequenceMutation); err != nil {
		return taskV2Log{}, err
	}
	if err := evictGlobalTaskLogBytes(ctx, tx, maximumGlobal); err != nil {
		return taskV2Log{}, err
	}
	return taskV2Log{
		TaskID: taskID, RunID: runID, Sequence: nextSequence, Stream: stream, Content: append([]byte(nil), content...),
		DisplayText: projection.DisplayText, SourceEncoding: projection.SourceEncoding, IsBinary: projection.IsBinary,
		HadDecodeErrors: projection.HadDecodeErrors, RawAvailable: projection.RawAvailable, OccurredAt: now,
	}, nil
}

const maximumTaskLogLineBytes = 1024

func splitTaskLogBytes(content []byte) [][]byte {
	if len(content) == 0 {
		return nil
	}
	parts := make([][]byte, 0, (len(content)+maximumTaskLogLineBytes-1)/maximumTaskLogLineBytes)
	for len(content) > 0 {
		cut := len(content)
		if cut > maximumTaskLogLineBytes {
			cut = maximumTaskLogLineBytes
			for index := cut - 1; index >= 0; index-- {
				if content[index] == '\n' {
					cut = index + 1
					break
				}
			}
		}
		parts = append(parts, append([]byte(nil), content[:cut]...))
		content = content[cut:]
	}
	return parts
}

func splitTaskLogDisplay(display string) []string {
	if display == "" {
		return nil
	}
	bytes := []byte(display)
	result := make([]string, 0, (len(bytes)+maximumTaskLogLineBytes-1)/maximumTaskLogLineBytes)
	start := 0
	for start < len(bytes) {
		end := start
		for end < len(bytes) {
			runeValue, size := utf8.DecodeRune(bytes[end:])
			if runeValue == utf8.RuneError && size == 1 {
				// The caller validates UTF-8; retain a defensive replacement if
				// malformed data ever reaches this helper.
				size = 1
			}
			if end-start+size > maximumTaskLogLineBytes && end > start {
				break
			}
			end += size
			if bytes[end-1] == '\n' || end-start >= maximumTaskLogLineBytes {
				break
			}
		}
		if end == start {
			_, size := utf8.DecodeRune(bytes[start:])
			end += max(size, 1)
		}
		result = append(result, string(bytes[start:end]))
		start = end
	}
	return result
}

func boolToSQLite(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (store *taskV2Store) ListLogs(ctx context.Context, taskID uuid.UUID, stream string, afterSequence, limitBytes uint64) (taskV2LogPage, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || stream != "" && !validTaskV2LogStream(stream) || limitBytes == 0 || limitBytes > 1<<20 {
		return taskV2LogPage{}, errRPCInvalid
	}
	// This is a hot read path while a task terminal is visible. Use the bounded
	// read-only handle and seek from the acknowledged sequence through the
	// (task_id, sequence) primary key. The previous implementation decoded every
	// retained log row on every refresh and discarded the acknowledged prefix in
	// Go, so transfer latency grew linearly with the lifetime of the task.
	db, err := store.business.openReadDB()
	if err != nil {
		return taskV2LogPage{}, err
	}
	defer db.Close()
	var nextSequence uint64
	if err := db.QueryRowContext(ctx, `SELECT next_log_sequence FROM tasks WHERE id = ?`, taskID.String()).Scan(&nextSequence); errors.Is(err, sql.ErrNoRows) {
		return taskV2LogPage{}, errRPCNotFound
	} else if err != nil || nextSequence == 0 {
		return taskV2LogPage{}, err
	}
	minimumQuery := `SELECT COALESCE(MIN(sequence), 0) FROM task_logs WHERE task_id = ?`
	minimumArguments := []any{taskID.String()}
	if stream != "" {
		minimumQuery += ` AND stream = ?`
		minimumArguments = append(minimumArguments, stream)
	}
	var minimum int64
	if err := db.QueryRowContext(ctx, minimumQuery, minimumArguments...).Scan(&minimum); err != nil || minimum < 0 {
		return taskV2LogPage{}, firstError(err, errors.New("task log retention is invalid"))
	}
	page := taskV2LogPage{
		Items:                    make([]taskV2Log, 0),
		AckedThroughSequence:     afterSequence,
		HighWatermark:            nextSequence - 1,
		MinimumAvailableSequence: uint64(minimum),
	}
	page.ResetRequired = page.MinimumAvailableSequence > 0 && afterSequence+1 < page.MinimumAvailableSequence
	if page.ResetRequired || afterSequence >= page.HighWatermark {
		return page, nil
	}

	query := `SELECT task_id, run_id, sequence, stream, content, display_content, source_encoding,
        is_binary, had_decode_errors, raw_available, occurred_at_ms FROM task_logs
        WHERE task_id = ? AND sequence > ? AND sequence <= ?`
	arguments := []any{taskID.String(), afterSequence, page.HighWatermark}
	if stream != "" {
		query += ` AND stream = ?`
		arguments = append(arguments, stream)
	}
	query += ` ORDER BY sequence`
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return taskV2LogPage{}, err
	}
	defer rows.Close()
	used := uint64(0)
	for rows.Next() {
		entry, err := scanTaskV2Log(rows)
		if err != nil {
			return taskV2LogPage{}, err
		}
		if used+uint64(len(entry.Content)) > limitBytes && len(page.Items) > 0 {
			page.HasMore = true
			break
		}
		page.Items = append(page.Items, entry)
		used += uint64(len(entry.Content))
		page.AckedThroughSequence = entry.Sequence
	}
	if err := rows.Err(); err != nil {
		return taskV2LogPage{}, err
	}
	return page, nil
}

// ListLogsBefore returns the newest bounded set of log lines before the
// supplied sequence. beforeSequence=0 means the current high-water mark. The
// response is chronological so callers can prepend it without reordering the
// visible tail.
func (store *taskV2Store) ListLogsBefore(ctx context.Context, taskID uuid.UUID, stream string, beforeSequence, limitLines uint64) (taskV2LogPage, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || stream != "" && !validTaskV2LogStream(stream) || limitLines == 0 || limitLines > 100 {
		return taskV2LogPage{}, errRPCInvalid
	}
	db, err := store.business.openReadDB()
	if err != nil {
		return taskV2LogPage{}, err
	}
	defer db.Close()
	var nextSequence uint64
	if err := db.QueryRowContext(ctx, `SELECT next_log_sequence FROM tasks WHERE id = ?`, taskID.String()).Scan(&nextSequence); errors.Is(err, sql.ErrNoRows) {
		return taskV2LogPage{}, errRPCNotFound
	} else if err != nil || nextSequence == 0 {
		return taskV2LogPage{}, err
	}
	watermark := nextSequence - 1
	if beforeSequence > 0 && beforeSequence < watermark {
		watermark = beforeSequence
	}
	minimumQuery := `SELECT COALESCE(MIN(sequence), 0) FROM task_logs WHERE task_id = ?`
	minimumArguments := []any{taskID.String()}
	if stream != "" {
		minimumQuery += ` AND stream = ?`
		minimumArguments = append(minimumArguments, stream)
	}
	var minimum int64
	if err := db.QueryRowContext(ctx, minimumQuery, minimumArguments...).Scan(&minimum); err != nil || minimum < 0 {
		return taskV2LogPage{}, err
	}
	query := `SELECT task_id, run_id, sequence, stream, content, display_content, source_encoding,
        is_binary, had_decode_errors, raw_available, occurred_at_ms FROM task_logs WHERE task_id = ? AND sequence <= ?`
	arguments := []any{taskID.String(), watermark}
	if stream != "" {
		query += ` AND stream = ?`
		arguments = append(arguments, stream)
	}
	query += ` ORDER BY sequence DESC LIMIT ?`
	arguments = append(arguments, limitLines+1)
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return taskV2LogPage{}, err
	}
	defer rows.Close()
	newestFirst := make([]taskV2Log, 0, limitLines+1)
	for rows.Next() {
		entry, scanErr := scanTaskV2Log(rows)
		if scanErr != nil {
			return taskV2LogPage{}, scanErr
		}
		newestFirst = append(newestFirst, entry)
	}
	if err := rows.Err(); err != nil {
		return taskV2LogPage{}, err
	}
	page := taskV2LogPage{
		Items:                    make([]taskV2Log, 0, min(len(newestFirst), int(limitLines))),
		AckedThroughSequence:     watermark,
		HighWatermark:            nextSequence - 1,
		MinimumAvailableSequence: uint64(minimum),
		LineCount:                uint64(min(len(newestFirst), int(limitLines))),
	}
	// A cursor older than the retained window cannot be satisfied by reverse
	// pagination. Tell the caller to rebuild from the current tail instead of
	// silently presenting a discontinuous history.
	if beforeSequence > 0 && minimum > 0 && beforeSequence < uint64(minimum) {
		page.Items = nil
		page.AckedThroughSequence = beforeSequence
		page.LineCount = 0
		page.HasMore = false
		page.ResetRequired = true
		return page, nil
	}
	if len(newestFirst) > int(limitLines) {
		page.HasMore = true
		newestFirst = newestFirst[:limitLines]
	}
	for index := len(newestFirst) - 1; index >= 0; index-- {
		page.Items = append(page.Items, newestFirst[index])
	}
	if len(page.Items) > 0 && page.HasMore {
		page.NextBeforeSequence = page.Items[0].Sequence - 1
	} else {
		page.NextBeforeSequence = 0
	}
	return page, nil
}

func (store *taskV2Store) RecoverInterrupted(ctx context.Context, now time.Time) (int, error) {
	if store == nil || store.business == nil || now.IsZero() {
		return 0, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var interrupted []taskV2Record
	for rows.Next() {
		task, err := scanTaskV2(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		interrupted = append(interrupted, task)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	recovered := 0
	projects := make(map[uuid.UUID]struct{}, len(interrupted))
	for _, task := range interrupted {
		if task.Definition.Kind == "workflow" && task.Definition.Scope == "topLevel" && task.CurrentRunID != nil {
			var runTaskID, runStatus string
			var runRevisionID, pinnedRevisionID sql.NullString
			runErr := tx.QueryRowContext(ctx, `SELECT task_id, workflow_revision_id, status
				FROM task_runs WHERE id = ?`, task.CurrentRunID.String()).Scan(&runTaskID, &runRevisionID, &runStatus)
			pinnedErr := tx.QueryRowContext(ctx, `SELECT revision_id FROM workflow_runs WHERE task_run_id = ?`,
				task.CurrentRunID.String()).Scan(&pinnedRevisionID)
			if runErr == nil && pinnedErr == nil && runTaskID == task.Definition.ID.String() && runStatus == "running" &&
				runRevisionID.Valid && pinnedRevisionID.Valid && runRevisionID.String == pinnedRevisionID.String {
				continue
			}
		}
		nextRevision := task.Revision + 1
		mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'failed', revision = ?, finished_at_ms = ?, result_code = 'agent_restarted', updated_at_ms = ?
	            WHERE id = ? AND revision = ? AND status = 'running'`, nextRevision, now.UnixMilli(), now.UnixMilli(), task.Definition.ID.String(), task.Revision)
		if err != nil {
			return 0, err
		}
		if err := requireSingleTaskMutation(mutation); err != nil {
			return 0, err
		}
		if task.CurrentRunID != nil {
			runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'failed', finished_at_ms = ?, result_code = 'agent_restarted'
	                WHERE id = ? AND status = 'running'`, now.UnixMilli(), task.CurrentRunID.String())
			if err != nil {
				return 0, err
			}
			if err := requireSingleTaskMutation(runMutation); err != nil {
				return 0, err
			}
		}
		if _, err := appendTaskV2Change(ctx, store.business, tx, task.Definition.ID, task.Definition.ProjectID, nextRevision, "upsert", now); err != nil {
			return 0, err
		}
		recovered++
		projects[task.Definition.ProjectID] = struct{}{}
	}
	for projectID := range projects {
		if err := pruneTaskV2Changes(ctx, tx, projectID, store.maximumChanges); err != nil {
			return 0, err
		}
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return 0, err
	}
	return recovered, nil
}

func (store *taskV2Store) Delete(ctx context.Context, taskID uuid.UUID, expectedRevision uint64, now time.Time) error {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 || now.IsZero() {
		return errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return errRPCNotFound
	}
	if err != nil {
		return err
	}
	if current.Revision != expectedRevision || current.Definition.Scope != "topLevel" || !taskV2TerminalStatus(current.Status) {
		return errRPCRevision
	}
	if _, err := deleteTaskV2RecordTx(ctx, store.business, tx, current, now); err != nil {
		return err
	}
	if err := pruneTaskV2Changes(ctx, tx, current.Definition.ProjectID, store.maximumChanges); err != nil {
		return err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return err
	}
	store.queueTaskLogDirectoryGC(taskID)
	return nil
}

func deleteTaskV2RecordTx(ctx context.Context, business *businessStore, tx *sql.Tx, current taskV2Record, now time.Time) (uint64, error) {
	if business == nil || tx == nil || current.Definition.ID == uuid.Nil || current.Definition.ProjectID == uuid.Nil ||
		current.Definition.Scope != "topLevel" || !taskV2TerminalStatus(current.Status) || now.IsZero() {
		return 0, errRPCInvalid
	}
	children := make([]taskV2Record, 0)
	if current.Definition.Kind == "workflow" {
		rows, err := tx.QueryContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks
			WHERE owner_workflow_task_id = ? ORDER BY created_at_ms, id`, current.Definition.ID.String())
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			child, scanErr := scanTaskV2(rows)
			if scanErr != nil {
				_ = rows.Close()
				return 0, scanErr
			}
			if child.Definition.Scope != "workflowNode" || child.Definition.ProjectID != current.Definition.ProjectID ||
				child.Definition.OwnerWorkflowTaskID == nil || *child.Definition.OwnerWorkflowTaskID != current.Definition.ID ||
				child.Status == "waiting" || child.Status == "running" || child.Status == "awaitingAcceptance" {
				_ = rows.Close()
				return 0, errRPCBusy
			}
			children = append(children, child)
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
	}
	for _, child := range children {
		if _, err := appendTaskV2Change(ctx, business, tx, child.Definition.ID, child.Definition.ProjectID, child.Revision+1, "delete", now); err != nil {
			return 0, err
		}
	}
	sequence, err := appendTaskV2Change(ctx, business, tx, current.Definition.ID, current.Definition.ProjectID, current.Revision+1, "delete", now)
	if err != nil {
		return 0, err
	}
	mutation, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ? AND project_id = ? AND revision = ? AND status = ?`,
		current.Definition.ID.String(), current.Definition.ProjectID.String(), current.Revision, current.Status)
	if err != nil {
		return 0, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return 0, err
	}
	for _, child := range children {
		mutation, err := tx.ExecContext(ctx, `DELETE FROM tasks WHERE id = ? AND project_id = ? AND revision = ? AND owner_workflow_task_id = ?`,
			child.Definition.ID.String(), child.Definition.ProjectID.String(), child.Revision, current.Definition.ID.String())
		if err != nil {
			return 0, err
		}
		if err := requireSingleTaskMutation(mutation); err != nil {
			return 0, err
		}
	}
	return sequence, nil
}

func requireTaskProjectPolicy(ctx context.Context, tx *sql.Tx, deviceID, projectID uuid.UUID) error {
	var state string
	var allowed int
	err := tx.QueryRowContext(ctx, `SELECT state, allow_task_execution FROM projects WHERE id = ? AND device_id = ?`, projectID.String(), deviceID.String()).Scan(&state, &allowed)
	if errors.Is(err, sql.ErrNoRows) || state != "available" {
		return errRPCProject
	}
	if err != nil {
		return err
	}
	if allowed == 0 {
		return errRPCCapability
	}
	return nil
}

func validateTaskRelationshipsTx(ctx context.Context, tx *sql.Tx, definition taskV2Definition) error {
	identifiers := append([]uuid.UUID(nil), definition.Execution.RelatedTaskIDs...)
	for _, pointer := range []*uuid.UUID{definition.OwnerWorkflowTaskID, definition.ParentTaskID, definition.RootTaskID} {
		if pointer != nil {
			identifiers = append(identifiers, *pointer)
		}
	}
	seen := make(map[uuid.UUID]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		if identifier == uuid.Nil || identifier == definition.ID {
			return errRPCInvalid
		}
		if _, found := seen[identifier]; found {
			continue
		}
		seen[identifier] = struct{}{}
		var projectID, scope string
		if err := tx.QueryRowContext(ctx, `SELECT project_id, scope FROM tasks WHERE id = ?`, identifier.String()).Scan(&projectID, &scope); errors.Is(err, sql.ErrNoRows) {
			return errRPCNotFound
		} else if err != nil {
			return err
		}
		if projectID != definition.ProjectID.String() {
			return errRPCProject
		}
		if scope != "topLevel" {
			return errRPCInvalid
		}
	}
	return nil
}

type taskV2QueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func taskV2ProjectWatermarks(ctx context.Context, query taskV2QueryRower, projectID uuid.UUID) (uint64, uint64, error) {
	if query == nil || projectID == uuid.Nil {
		return 0, 0, errRPCInvalid
	}
	var highWatermark, minimumAvailable uint64
	if err := query.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0), COALESCE(MIN(sequence), 0)
        FROM task_changes WHERE project_id = ?`, projectID.String()).Scan(&highWatermark, &minimumAvailable); err != nil {
		return 0, 0, err
	}
	return highWatermark, minimumAvailable, nil
}

func appendTaskV2Change(ctx context.Context, store *businessStore, tx *sql.Tx, taskID, projectID uuid.UUID, revision uint64, operation string, now time.Time) (uint64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO task_changes(task_id, project_id, revision, operation, occurred_at_ms) VALUES(?, ?, ?, ?, ?)`,
		taskID.String(), projectID.String(), revision, operation, now.UTC().UnixMilli())
	if err != nil {
		return 0, err
	}
	sequence, err := result.LastInsertId()
	if err != nil || sequence < 1 {
		return 0, errors.New("task change sequence is invalid")
	}
	if _, err := store.appendAgentEvent(ctx, tx, newTaskChangedAgentEvent(projectID, taskID, revision, uint64(sequence), operation, now)); err != nil {
		return 0, err
	}
	return uint64(sequence), nil
}

func pruneTaskV2Changes(ctx context.Context, tx *sql.Tx, projectID uuid.UUID, maximum int) error {
	if maximum < 1 {
		return errRPCInvalid
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM task_changes WHERE project_id = ? AND sequence NOT IN (
        SELECT sequence FROM task_changes WHERE project_id = ? ORDER BY sequence DESC LIMIT ?
    )`, projectID.String(), projectID.String(), maximum)
	return err
}

func evictTaskLogBytes(ctx context.Context, tx *sql.Tx, taskID uuid.UUID, incoming, maximum uint64) error {
	var total uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(byte_count), 0) FROM task_logs WHERE task_id = ?`, taskID.String()).Scan(&total); err != nil {
		return err
	}
	if maximum == 0 || incoming > maximum {
		return errRPCBusy
	}
	if total+incoming <= maximum {
		return nil
	}
	return deleteOldestTaskLogBytes(ctx, tx, `WHERE task_id = ?`, []any{taskID.String()}, total+incoming-maximum)
}

func evictGlobalTaskLogBytes(ctx context.Context, tx *sql.Tx, maximum uint64) error {
	if maximum == 0 {
		return errRPCBusy
	}
	var total uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(byte_count), 0) FROM task_logs`).Scan(&total); err != nil {
		return err
	}
	if total <= maximum {
		return nil
	}
	return deleteOldestTaskLogBytes(ctx, tx, ``, nil, total-maximum)
}

func deleteOldestTaskLogBytes(ctx context.Context, tx *sql.Tx, where string, arguments []any, required uint64) error {
	if required == 0 || required > 1<<63 {
		return nil
	}
	query := `SELECT task_id, sequence, byte_count FROM task_logs ` + where + ` ORDER BY occurred_at_ms, task_id, sequence`
	rows, err := tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return err
	}
	type key struct {
		taskID   string
		sequence uint64
	}
	var keys []key
	var removed uint64
	for rows.Next() && removed < required {
		var item key
		var bytes uint64
		if err := rows.Scan(&item.taskID, &item.sequence, &bytes); err != nil {
			_ = rows.Close()
			return err
		}
		keys, removed = append(keys, item), removed+bytes
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range keys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM task_logs WHERE task_id = ? AND sequence = ?`, item.taskID, item.sequence); err != nil {
			return err
		}
	}
	return nil
}

func scanTaskV2(scanner interface{ Scan(...any) error }) (taskV2Record, error) {
	var id, projectID, kind, title, cwd, scope, status, resultCode, logState, logSHA256, logPath string
	var owner, parent, root, currentRun sql.NullString
	var definitionJSON []byte
	var definitionRevision, revision, createdAt, updatedAt, changeSequence, logGeneration, logFormatVersion, logSizeBytes uint64
	var scheduledAt, startedAt, finishedAt, exitCode, logUpdatedAt sql.NullInt64
	if err := scanner.Scan(
		&id, &projectID, &kind, &title, &cwd, &scope, &owner, &parent, &root, &definitionJSON, &definitionRevision,
		&status, &revision, &currentRun, &scheduledAt, &createdAt, &updatedAt, &startedAt, &finishedAt, &exitCode, &resultCode, &changeSequence,
		&logState, &logGeneration, &logFormatVersion, &logSizeBytes, &logSHA256, &logUpdatedAt, &logPath,
	); err != nil {
		return taskV2Record{}, err
	}
	definition, err := decodeTaskV2Definition(definitionJSON)
	parsedID, idErr := uuid.Parse(id)
	parsedProjectID, projectErr := uuid.Parse(projectID)
	if err != nil || idErr != nil || projectErr != nil || parsedID == uuid.Nil || parsedProjectID == uuid.Nil ||
		definition.ID != parsedID || definition.ProjectID != parsedProjectID || definition.Kind != kind || definition.Title != title ||
		definition.CWD != cwd || definition.Scope != scope || definitionRevision == 0 || revision == 0 || !validTaskV2Status(status) ||
		createdAt == 0 || updatedAt == 0 || !validTaskRunLogPath(logPath) ||
		!validTaskLogMetadata(logState, logGeneration, logFormatVersion, logSizeBytes, logSHA256, logUpdatedAt) {
		return taskV2Record{}, errors.New("task store row is invalid")
	}
	ownerID, err := parseNullableUUID(owner)
	if err != nil || !sameUUIDPointer(ownerID, definition.OwnerWorkflowTaskID) {
		return taskV2Record{}, errors.New("task owner row is invalid")
	}
	parentID, err := parseNullableUUID(parent)
	if err != nil || !sameUUIDPointer(parentID, definition.ParentTaskID) {
		return taskV2Record{}, errors.New("task parent row is invalid")
	}
	rootID, err := parseNullableUUID(root)
	if err != nil || !sameUUIDPointer(rootID, definition.RootTaskID) {
		return taskV2Record{}, errors.New("task root row is invalid")
	}
	runID, err := parseNullableUUID(currentRun)
	if err != nil {
		return taskV2Record{}, errors.New("task run row is invalid")
	}
	storedScheduledAt := nullableMillisTime(scheduledAt)
	if !sameTimePointer(storedScheduledAt, definition.Execution.ScheduledAt) {
		return taskV2Record{}, errors.New("task schedule row is invalid")
	}
	record := taskV2Record{
		Definition: definition, DefinitionRevision: definitionRevision, Status: status, Revision: revision,
		ChangeSequence: changeSequence, CurrentRunID: runID, CreatedAt: time.UnixMilli(int64(createdAt)).UTC(), UpdatedAt: time.UnixMilli(int64(updatedAt)).UTC(),
		StartedAt: nullableMillisTime(startedAt), FinishedAt: nullableMillisTime(finishedAt), ResultCode: resultCode,
		LogAvailable: taskLogAvailable(logState), LogState: logState, LogGeneration: logGeneration,
		LogFormatVersion: uint32(logFormatVersion), LogSizeBytes: logSizeBytes, LogSHA256: logSHA256,
		LogUpdatedAt: nullableMillisTime(logUpdatedAt), LegacyLogPath: logPath,
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		record.ExitCode = &value
	}
	return record, nil
}

func scanTaskV2Run(scanner interface{ Scan(...any) error }) (taskV2Run, error) {
	var id, taskID, status, resultCode, sessionID, logState, logSHA256, logPath string
	var workflowRevision, parentWorkflow, nodeID sql.NullString
	var attempt uint32
	var createdAt, logGeneration, logFormatVersion, logSizeBytes uint64
	var startedAt, finishedAt, exitCode, logUpdatedAt sql.NullInt64
	if err := scanner.Scan(&id, &taskID, &workflowRevision, &parentWorkflow, &nodeID, &status, &attempt, &createdAt,
		&startedAt, &finishedAt, &exitCode, &resultCode, &sessionID, &logState, &logGeneration, &logFormatVersion,
		&logSizeBytes, &logSHA256, &logUpdatedAt, &logPath); err != nil {
		return taskV2Run{}, err
	}
	parsedID, idErr := uuid.Parse(id)
	parsedTaskID, taskErr := uuid.Parse(taskID)
	workflowID, workflowErr := parseNullableUUID(workflowRevision)
	parentID, parentErr := parseNullableUUID(parentWorkflow)
	if idErr != nil || taskErr != nil || workflowErr != nil || parentErr != nil || parsedID == uuid.Nil || parsedTaskID == uuid.Nil ||
		!validTaskV2Status(status) || createdAt == 0 || !validTaskRunLogPath(logPath) ||
		!validTaskLogMetadata(logState, logGeneration, logFormatVersion, logSizeBytes, logSHA256, logUpdatedAt) {
		return taskV2Run{}, errors.New("task run row is invalid")
	}
	run := taskV2Run{
		ID: parsedID, TaskID: parsedTaskID, WorkflowRevisionID: workflowID, ParentWorkflowTaskRunID: parentID,
		WorkflowNodeID: nodeID.String, Status: status, Attempt: attempt, CreatedAt: time.UnixMilli(int64(createdAt)).UTC(),
		StartedAt: nullableMillisTime(startedAt), FinishedAt: nullableMillisTime(finishedAt), ResultCode: resultCode, CliSessionID: sessionID,
		LogAvailable: taskLogAvailable(logState), LogState: logState, LogGeneration: logGeneration,
		LogFormatVersion: uint32(logFormatVersion), LogSizeBytes: logSizeBytes, LogSHA256: logSHA256,
		LogUpdatedAt: nullableMillisTime(logUpdatedAt), LegacyLogPath: logPath,
	}
	if exitCode.Valid {
		value := int(exitCode.Int64)
		run.ExitCode = &value
	}
	return run, nil
}

func validTaskLogMetadata(state string, generation, formatVersion, size uint64, digest string, updatedAt sql.NullInt64) bool {
	if !validTaskLogState(state) || formatVersion > 1 || size > maximumTaskRunLogFileBytes ||
		(state == taskLogStateNone && generation != 0) || (state != taskLogStateNone && generation == 0) ||
		updatedAt.Valid && updatedAt.Int64 <= 0 {
		return false
	}
	if digest == "" {
		return true
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == digest
}

func scanTaskV2Log(scanner interface{ Scan(...any) error }) (taskV2Log, error) {
	var taskID string
	var runID sql.NullString
	var sequence uint64
	var stream string
	var content []byte
	var displayText []byte
	var sourceEncoding string
	var isBinary, hadDecodeErrors, rawAvailable int
	var occurredAt int64
	if err := scanner.Scan(&taskID, &runID, &sequence, &stream, &content, &displayText, &sourceEncoding, &isBinary, &hadDecodeErrors, &rawAvailable, &occurredAt); err != nil {
		return taskV2Log{}, err
	}
	parsedTaskID, taskErr := uuid.Parse(taskID)
	parsedRunID, runErr := parseNullableUUID(runID)
	if taskErr != nil || runErr != nil || parsedTaskID == uuid.Nil || sequence == 0 || !validTaskV2LogStream(stream) ||
		len(content) == 0 || len(content) > maximumTaskLogEntryBytes || occurredAt <= 0 || !utf8.Valid(displayText) ||
		!utf8.ValidString(sourceEncoding) || (isBinary != 0 && isBinary != 1) || (hadDecodeErrors != 0 && hadDecodeErrors != 1) || (rawAvailable != 0 && rawAvailable != 1) {
		return taskV2Log{}, errors.New("task log row is invalid")
	}
	return taskV2Log{
		TaskID: parsedTaskID, RunID: parsedRunID, Sequence: sequence, Stream: stream,
		Content: append([]byte(nil), content...), DisplayText: string(displayText), SourceEncoding: sourceEncoding,
		IsBinary: isBinary == 1, HadDecodeErrors: hadDecodeErrors == 1, RawAvailable: rawAvailable == 1,
		OccurredAt: time.UnixMilli(occurredAt).UTC(),
	}, nil
}

func nullableUUIDString(value *uuid.UUID) any {
	if value == nil {
		return nil
	}
	return value.String()
}

func parseNullableUUID(value sql.NullString) (*uuid.UUID, error) {
	if !value.Valid || value.String == "" {
		return nil, nil
	}
	parsed, err := uuid.Parse(value.String)
	if err != nil || parsed == uuid.Nil {
		return nil, errors.New("UUID is invalid")
	}
	return &parsed, nil
}

func sameUUIDPointer(left, right *uuid.UUID) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func sameTimePointer(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}

func nullableTimeMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().UnixMilli()
}

func nullableMillisTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.UnixMilli(value.Int64).UTC()
	return &result
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func requireSingleTaskMutation(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errRPCRevision
	}
	return nil
}

func validTaskV2LogStream(value string) bool {
	return value == "stdout" || value == "stderr" || value == "system" || value == "tool"
}

func validTaskResultCode(value string) bool {
	if len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character != '_' && character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func isSQLiteConstraint(err error) bool {
	return err != nil && (errors.Is(err, sql.ErrNoRows) || containsFold(err.Error(), "constraint"))
}

func containsFold(value, target string) bool {
	for index := 0; index+len(target) <= len(value); index++ {
		if equalFoldASCII(value[index:index+len(target)], target) {
			return true
		}
	}
	return false
}

func equalFoldASCII(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		l, r := left[index], right[index]
		if l >= 'A' && l <= 'Z' {
			l += 'a' - 'A'
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if l != r {
			return false
		}
	}
	return true
}
