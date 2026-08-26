package remotecontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type grantRow struct {
	GrantVersion int64  `gorm:"column:grant_version"`
	GrantStatus  string `gorm:"column:grant_status"`
	DeviceStatus string `gorm:"column:device_status"`
}

func (service *Service) requireGrant(ctx context.Context, database *gorm.DB, userID, deviceID uuid.UUID, _ string, lock bool) (grantRow, error) {
	if database == nil {
		database = service.store.db
	}
	query := database.WithContext(ctx).Table("remote_access_grants access_grant").
		Select("access_grant.grant_version, access_grant.status AS grant_status, credential.status AS device_status").
		Joins("JOIN remote_device_credentials credential ON credential.device_id = access_grant.device_id AND credential.user_id = access_grant.user_id").
		Where("access_grant.user_id = ? AND access_grant.device_id = ?", userID, deviceID)
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "access_grant"}})
	}
	var row grantRow
	if err := query.Take(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return grantRow{}, ErrNotFound
	} else if err != nil {
		return grantRow{}, fmt.Errorf("load remote access grant: %w", err)
	}
	if row.GrantVersion < 1 {
		return grantRow{}, ErrUnavailable
	}
	// A device grant controls whether its owner can reach it. Relay operation
	// labels are protocol metadata and never restrict project or task access.
	if row.GrantStatus != "enabled" || row.DeviceStatus != "active" {
		return grantRow{}, ErrForbidden
	}
	return row, nil
}

type projectRow struct {
	ProjectID    uuid.UUID       `gorm:"column:project_id"`
	DisplayName  string          `gorm:"column:display_name"`
	Revision     int64           `gorm:"column:revision"`
	Capabilities json.RawMessage `gorm:"column:capabilities"`
	State        string          `gorm:"column:state"`
	ObservedAt   time.Time       `gorm:"column:observed_at"`
	UpdatedAt    time.Time       `gorm:"column:updated_at"`
}

func (service *Service) ListProjects(ctx context.Context, userID, deviceID uuid.UUID, page PageRequest) (ProjectPage, error) {
	if userID == uuid.Nil || deviceID == uuid.Nil {
		return ProjectPage{}, ErrInvalidInput
	}
	if _, err := service.requireGrant(ctx, nil, userID, deviceID, "remote.project.read", false); err != nil {
		return ProjectPage{}, err
	}
	limit, err := normalizeLimit(page.Limit)
	if err != nil {
		return ProjectPage{}, err
	}
	cursor, err := service.cursors.decode(page.Cursor, "projects:"+deviceID.String())
	if err != nil {
		return ProjectPage{}, err
	}
	var rows []projectRow
	if err := service.store.db.WithContext(ctx).Raw(`
		SELECT project_id, display_name, revision, capabilities, state, observed_at, updated_at
		FROM remote_projects
		WHERE user_id = ? AND device_id = ?
		  AND state <> 'removed'
		  AND (?::timestamptz IS NULL OR (updated_at, project_id) < (?, ?))
		ORDER BY updated_at DESC, project_id DESC LIMIT ?`,
		userID, deviceID, nullableCursorTime(cursor), nullableCursorTime(cursor), nullableCursorID(cursor), limit+1).Scan(&rows).Error; err != nil {
		return ProjectPage{}, fmt.Errorf("%w: list projects: %v", ErrUnavailable, err)
	}
	now := service.now().UTC()
	result := ProjectPage{Items: make([]Project, 0, min(len(rows), limit)), ObservedAt: now, Stale: true}
	for index, row := range rows {
		if index == limit {
			encoded, encodeErr := service.cursors.encode("projects:"+deviceID.String(), rows[index-1].UpdatedAt, rows[index-1].ProjectID)
			if encodeErr != nil {
				return ProjectPage{}, ErrUnavailable
			}
			result.NextCursor = &encoded
			break
		}
		var capabilities []string
		if row.Revision < 0 || json.Unmarshal(row.Capabilities, &capabilities) != nil {
			return ProjectPage{}, ErrUnavailable
		}
		result.Items = append(result.Items, Project{
			ID: row.ProjectID, DisplayName: row.DisplayName, Revision: uint64(row.Revision), Capabilities: capabilities,
			ObservedAt: row.ObservedAt.UTC(), State: row.State, UpdatedAt: row.UpdatedAt.UTC(),
		})
		if row.ObservedAt.After(result.ObservedAt) || len(result.Items) == 1 {
			result.ObservedAt = row.ObservedAt.UTC()
		}
	}
	var state struct {
		HighWatermark int64      `gorm:"column:high_watermark"`
		LastSyncAt    *time.Time `gorm:"column:last_sync_at"`
		LastSeenAt    *time.Time `gorm:"column:last_allocation_at"`
	}
	if err := service.store.db.WithContext(ctx).Raw(`
		SELECT COALESCE(sync.high_watermark, 0) AS high_watermark, sync.last_sync_at, credential.last_allocation_at
		FROM remote_device_credentials credential
		LEFT JOIN remote_device_sync_state sync ON sync.device_id = credential.device_id
		WHERE credential.user_id = ? AND credential.device_id = ?`, userID, deviceID).Take(&state).Error; err != nil {
		return ProjectPage{}, ErrUnavailable
	}
	result.HighWatermark = uint64(state.HighWatermark)
	result.DeviceOnline = state.LastSeenAt != nil && state.LastSeenAt.After(now.Add(-2*time.Minute))
	result.Stale = state.LastSyncAt == nil || state.LastSyncAt.Before(now.Add(-5*time.Minute))
	if state.LastSyncAt != nil {
		result.ObservedAt = state.LastSyncAt.UTC()
	}
	return result, nil
}

type taskRow struct {
	TaskID     uuid.UUID  `gorm:"column:task_id"`
	DeviceID   uuid.UUID  `gorm:"column:device_id"`
	ProjectID  *uuid.UUID `gorm:"column:project_id"`
	TaskType   string     `gorm:"column:task_type"`
	Title      string     `gorm:"column:title"`
	Status     string     `gorm:"column:status"`
	Revision   int64      `gorm:"column:revision"`
	ResultCode *string    `gorm:"column:result_code"`
	StartedAt  *time.Time `gorm:"column:started_at"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
}

func taskFromRow(row taskRow) (Task, error) {
	if row.TaskID == uuid.Nil || row.DeviceID == uuid.Nil || row.Revision < 0 {
		return Task{}, ErrUnavailable
	}
	return Task{
		ID: row.TaskID, DeviceID: row.DeviceID, ProjectID: row.ProjectID, TaskType: row.TaskType, Title: row.Title,
		Status: row.Status, Revision: uint64(row.Revision),
		CreatedAt: row.CreatedAt.UTC(), StartedAt: utcPointer(row.StartedAt), FinishedAt: utcPointer(row.FinishedAt),
		ResultCode: row.ResultCode, UpdatedAt: row.UpdatedAt.UTC(),
	}, nil
}

func (service *Service) ListTasks(ctx context.Context, userID, deviceID uuid.UUID, page PageRequest) (TaskPage, error) {
	if userID == uuid.Nil || deviceID == uuid.Nil || (page.AfterRevision != nil && strings.TrimSpace(page.Cursor) != "") {
		return TaskPage{}, ErrInvalidInput
	}
	if _, err := service.requireGrant(ctx, nil, userID, deviceID, "remote.task.read", false); err != nil {
		return TaskPage{}, err
	}
	limit, err := normalizeLimit(page.Limit)
	if err != nil {
		return TaskPage{}, err
	}
	cursor, err := service.cursors.decode(page.Cursor, "tasks:"+deviceID.String())
	if err != nil {
		return TaskPage{}, err
	}
	var highWatermark int64
	if err := service.store.db.WithContext(ctx).Table("remote_tasks").
		Where("user_id = ? AND device_id = ?", userID, deviceID).
		Select("COALESCE(MAX(change_sequence), 0)").Scan(&highWatermark).Error; err != nil || highWatermark < 0 {
		return TaskPage{}, ErrUnavailable
	}
	result := TaskPage{Items: []Task{}, HighWatermark: uint64(highWatermark)}
	if page.AfterRevision != nil && *page.AfterRevision >= result.HighWatermark {
		return result, nil
	}
	if page.AfterRevision != nil && *page.AfterRevision > 0 {
		result.ResetRequired = true
	}
	var rows []taskRow
	if err := service.store.db.WithContext(ctx).Raw(`
		SELECT task_id, device_id, project_id, task_type, title, status, revision,
		       result_code, started_at, finished_at, created_at, updated_at
		FROM remote_tasks WHERE user_id = ? AND device_id = ?
		  AND (?::timestamptz IS NULL OR (updated_at, task_id) < (?, ?))
		ORDER BY updated_at DESC, task_id DESC LIMIT ?`, userID, deviceID, nullableCursorTime(cursor),
		nullableCursorTime(cursor), nullableCursorID(cursor), limit+1).Scan(&rows).Error; err != nil {
		return TaskPage{}, fmt.Errorf("%w: list tasks: %v", ErrUnavailable, err)
	}
	result.Items = make([]Task, 0, min(len(rows), limit))
	for index, row := range rows {
		if index == limit {
			encoded, encodeErr := service.cursors.encode("tasks:"+deviceID.String(), rows[index-1].UpdatedAt, rows[index-1].TaskID)
			if encodeErr != nil {
				return TaskPage{}, ErrUnavailable
			}
			result.NextCursor = &encoded
			break
		}
		task, convertErr := taskFromRow(row)
		if convertErr != nil {
			return TaskPage{}, convertErr
		}
		result.Items = append(result.Items, task)
	}
	return result, nil
}

func (service *Service) GetTask(ctx context.Context, userID, taskID uuid.UUID) (Task, error) {
	if userID == uuid.Nil || taskID == uuid.Nil {
		return Task{}, ErrInvalidInput
	}
	var row taskRow
	err := service.store.db.WithContext(ctx).Table("remote_tasks").Where("task_id = ? AND user_id = ?", taskID, userID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, ErrUnavailable
	}
	if _, err := service.requireGrant(ctx, nil, userID, row.DeviceID, "remote.task.read", false); err != nil {
		return Task{}, err
	}
	return taskFromRow(row)
}

func (service *Service) RequestProjectSync(ctx context.Context, input SyncProjectInput) (Operation, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.UserID == uuid.Nil || input.DeviceID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) ||
		(input.ProjectID != nil && *input.ProjectID == uuid.Nil) {
		return Operation{}, ErrInvalidInput
	}
	body := map[string]any{"afterSequence": input.AfterSequence, "knownHighWatermark": input.HighWatermark}
	if input.ProjectID != nil {
		body["projectId"] = *input.ProjectID
	}
	return service.enqueueCommand(ctx, input.UserID, input.DeviceID, nil, "project.sync", "remote.project.sync", body, nil, input.IdempotencyKey)
}

func (service *Service) CreateTask(ctx context.Context, input CreateTaskInput) (Task, Operation, error) {
	_ = service
	_ = ctx
	_ = input
	// Full task definitions are device-local and may only be submitted over an
	// E2EE Peer RPC session. The cloud control plane deliberately has no task
	// creation fallback because remote_commands.body is a reliable, plaintext
	// metadata lane rather than a business-content transport.
	return Task{}, Operation{}, ErrPeerRequired
}

func (service *Service) CancelTask(ctx context.Context, input CancelTaskInput) (Operation, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.UserID == uuid.Nil || input.TaskID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) {
		return Operation{}, ErrInvalidInput
	}
	var row taskRow
	if err := service.store.db.WithContext(ctx).Table("remote_tasks").Where("task_id = ? AND user_id = ?", input.TaskID, input.UserID).Take(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Operation{}, ErrNotFound
	} else if err != nil {
		return Operation{}, ErrUnavailable
	}
	body := map[string]any{"taskId": input.TaskID}
	operation, err := service.enqueueCommand(ctx, input.UserID, row.DeviceID, &input.TaskID, "task.cancel", "remote.task.write", body, nil, input.IdempotencyKey)
	if err != nil {
		return Operation{}, err
	}
	if !operation.Replayed {
		now := service.now().UTC()
		if updateErr := service.store.db.WithContext(ctx).Table("remote_tasks").Where("task_id = ? AND user_id = ? AND status NOT IN ?", input.TaskID, input.UserID,
			[]string{"cancelled", "succeeded", "failed", "rejected", "expired", "timed_out"}).
			Updates(map[string]any{"status": "cancel_requested", "revision": gorm.Expr("revision + 1"), "updated_at": now}).Error; updateErr != nil {
			return Operation{}, ErrUnavailable
		}
	}
	return operation, nil
}

// RetryTask re-enqueues the exact, already validated task specification under
// a fresh task id.  It deliberately accepts only terminal tasks: retry is not
// a second execution channel for work that may still be running on a device.
func (service *Service) RetryTask(ctx context.Context, input RetryTaskInput) (Task, Operation, error) {
	_ = service
	_ = ctx
	_ = input
	return Task{}, Operation{}, ErrPeerRequired
}

func (service *Service) enqueueCommand(ctx context.Context, userID, deviceID uuid.UUID, taskID *uuid.UUID, kind, requiredScope string, body map[string]any, expectedRevision *uint64, idempotencyKey string) (Operation, error) {
	if kind != "project.sync" && kind != "task.cancel" {
		return Operation{}, ErrInvalidInput
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil || len(bodyJSON) > MaximumTaskInputBytes+4096 || containsSensitiveJSON(bodyJSON) {
		return Operation{}, ErrInvalidInput
	}
	hash := requestHash(struct {
		DeviceID         uuid.UUID       `json:"deviceId"`
		Kind             string          `json:"kind"`
		Body             json.RawMessage `json:"body"`
		ExpectedRevision *uint64         `json:"expectedRevision"`
	}{deviceID, kind, bodyJSON, expectedRevision})
	now := service.now().UTC()
	var result Operation
	err = service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		grant, grantErr := service.requireGrant(ctx, tx, userID, deviceID, requiredScope, true)
		if grantErr != nil {
			return grantErr
		}
		var existing struct {
			CommandID   uuid.UUID `gorm:"column:command_id"`
			RequestHash string    `gorm:"column:request_hash"`
			Status      string    `gorm:"column:status"`
			CreatedAt   time.Time `gorm:"column:created_at"`
		}
		priorErr := tx.Table("remote_commands").Select("command_id, request_hash, status, created_at").
			Where("user_id = ? AND device_id = ? AND kind = ? AND idempotency_key = ?", userID, deviceID, kind, idempotencyKey).Take(&existing).Error
		if priorErr == nil {
			if existing.RequestHash != hash {
				return ErrIdempotencyConflict
			}
			result = Operation{ID: existing.CommandID, Kind: kind, Status: existing.Status, CreatedAt: existing.CreatedAt.UTC(), Replayed: true}
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return priorErr
		}
		commandID := uuid.New()
		if err := tx.Table("remote_commands").Create(map[string]any{
			"command_id": commandID, "user_id": userID, "device_id": deviceID, "task_id": taskID,
			"kind": kind, "body": string(bodyJSON), "status": "queued", "idempotency_key": idempotencyKey,
			"request_hash": hash, "grant_version": grant.GrantVersion, "expected_revision": expectedRevision,
			"expires_at": now.Add(DefaultCommandTTL), "created_at": now, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		result = Operation{ID: commandID, Kind: kind, Status: "queued", CreatedAt: now}
		return nil
	})
	if err != nil {
		return Operation{}, mapStoreError(err)
	}
	return result, nil
}

func containsSensitiveJSON(raw []byte) bool {
	var value any
	return json.Unmarshal(raw, &value) != nil || containsSensitiveField(value)
}

func (service *Service) ListTaskLogs(ctx context.Context, userID, taskID uuid.UUID, stream string, afterSequence uint64, limitBytes int) (TaskLogPage, error) {
	_ = service
	_ = ctx
	_ = userID
	_ = taskID
	_ = stream
	_ = afterSequence
	_ = limitBytes
	return TaskLogPage{}, ErrPeerRequired
}

func (service *Service) ListTaskEvents(ctx context.Context, userID, taskID uuid.UUID, afterEventID uint64, limit int) (TaskEventPage, error) {
	if userID == uuid.Nil || taskID == uuid.Nil {
		return TaskEventPage{}, ErrInvalidInput
	}
	if _, err := service.GetTask(ctx, userID, taskID); err != nil {
		return TaskEventPage{}, err
	}
	if limit == 0 {
		limit = DefaultEventPageLimit
	}
	if limit < 1 || limit > 500 {
		return TaskEventPage{}, ErrInvalidInput
	}
	var rows []struct {
		RowID      int64           `gorm:"column:event_row_id"`
		EventID    uuid.UUID       `gorm:"column:event_id"`
		EventType  string          `gorm:"column:event_type"`
		Revision   int64           `gorm:"column:revision"`
		Payload    json.RawMessage `gorm:"column:payload"`
		OccurredAt time.Time       `gorm:"column:occurred_at"`
	}
	if err := service.store.db.WithContext(ctx).Table("remote_device_events").
		Where("user_id = ? AND task_id = ? AND event_row_id > ?", userID, taskID, afterEventID).
		Order("event_row_id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return TaskEventPage{}, ErrUnavailable
	}
	result := TaskEventPage{Items: make([]TaskEvent, 0, min(len(rows), limit)), NextEventID: afterEventID}
	for index, row := range rows {
		if index == limit {
			result.HasMore = true
			break
		}
		if row.RowID < 1 || row.Revision < 0 || !json.Valid(row.Payload) {
			return TaskEventPage{}, ErrUnavailable
		}
		result.Items = append(result.Items, TaskEvent{ID: uint64(row.RowID), EventID: row.EventID, Type: row.EventType,
			Revision: uint64(row.Revision), OccurredAt: row.OccurredAt.UTC(), Payload: row.Payload})
		result.NextEventID = uint64(row.RowID)
	}
	return result, nil
}
