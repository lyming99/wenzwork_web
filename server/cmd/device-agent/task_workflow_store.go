package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
)

type workflowV2Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (store *taskV2Store) CreateWorkflow(
	ctx context.Context,
	definition taskV2Definition,
	revision workflowV2Revision,
	now time.Time,
) (taskV2Record, workflowV2Revision, error) {
	if store == nil || store.business == nil || definition.ID == uuid.Nil || definition.Kind != "workflow" ||
		revision.WorkflowTaskID != definition.ID || revision.Version != 1 || now.IsZero() {
		return taskV2Record{}, workflowV2Revision{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	defer tx.Rollback()
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, definition.ProjectID); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := ensureWorkflowTaskIDsAvailable(ctx, tx, definition.ID, revision); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	parent, err := insertWorkflowV2TaskTx(ctx, tx, definition, now)
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	parent.ChangeSequence, err = appendTaskV2Change(ctx, store.business, tx, definition.ID, definition.ProjectID, parent.Revision, "upsert", now)
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := validateWorkflowV2SourcesTx(ctx, tx, definition, revision); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	for _, node := range revision.Nodes {
		if node.Type != "task" || node.TaskDefinition == nil {
			continue
		}
		if err := validateTaskRelationshipsTx(ctx, tx, *node.TaskDefinition); err != nil {
			return taskV2Record{}, workflowV2Revision{}, err
		}
		child, err := insertWorkflowV2TaskTx(ctx, tx, *node.TaskDefinition, now)
		if err != nil {
			return taskV2Record{}, workflowV2Revision{}, err
		}
		if _, err := appendTaskV2Change(ctx, store.business, tx, child.Definition.ID, child.Definition.ProjectID, child.Revision, "upsert", now); err != nil {
			return taskV2Record{}, workflowV2Revision{}, err
		}
	}
	if err := insertWorkflowV2RevisionTx(ctx, tx, revision); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	return parent, revision, nil
}

func (store *taskV2Store) PublishWorkflowRevision(
	ctx context.Context,
	definition taskV2Definition,
	expectedTaskRevision uint64,
	revision workflowV2Revision,
	now time.Time,
) (taskV2Record, workflowV2Revision, error) {
	if store == nil || store.business == nil || definition.ID == uuid.Nil || definition.Kind != "workflow" || expectedTaskRevision == 0 ||
		revision.WorkflowTaskID != definition.ID || revision.Version < 2 || now.IsZero() {
		return taskV2Record{}, workflowV2Revision{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, definition.ID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, workflowV2Revision{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	currentRevisionID, currentVersion, configErr := workflowV2DefinitionRevision(current.Definition.Config)
	if current.Revision != expectedTaskRevision || current.Definition.Kind != "workflow" || current.Definition.ProjectID != definition.ProjectID ||
		configErr != nil || currentRevisionID == uuid.Nil || revision.Version != currentVersion+1 || definition.Config == nil {
		return taskV2Record{}, workflowV2Revision{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, definition.ProjectID); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := ensureWorkflowTaskIDsAvailable(ctx, tx, uuid.Nil, revision); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := validateWorkflowV2SourcesTx(ctx, tx, definition, revision); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	for _, node := range revision.Nodes {
		if node.Type != "task" || node.TaskDefinition == nil {
			continue
		}
		if err := validateTaskRelationshipsTx(ctx, tx, *node.TaskDefinition); err != nil {
			return taskV2Record{}, workflowV2Revision{}, err
		}
		child, err := insertWorkflowV2TaskTx(ctx, tx, *node.TaskDefinition, now)
		if err != nil {
			return taskV2Record{}, workflowV2Revision{}, err
		}
		if _, err := appendTaskV2Change(ctx, store.business, tx, child.Definition.ID, child.Definition.ProjectID, child.Revision, "upsert", now); err != nil {
			return taskV2Record{}, workflowV2Revision{}, err
		}
	}
	if err := insertWorkflowV2RevisionTx(ctx, tx, revision); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	encoded, err := json.Marshal(definition)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return taskV2Record{}, workflowV2Revision{}, errRPCInvalid
	}
	nextRevision, nextDefinitionRevision := current.Revision+1, current.DefinitionRevision+1
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET title = ?, cwd = ?, definition_json = ?, definition_revision = ?, revision = ?, updated_at_ms = ?
		WHERE id = ? AND revision = ? AND kind = 'workflow'`, definition.Title, definition.CWD, encoded, nextDefinitionRevision,
		nextRevision, now.UnixMilli(), definition.ID.String(), expectedTaskRevision)
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, definition.ID, definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, workflowV2Revision{}, err
	}
	current.Definition, current.DefinitionRevision, current.Revision = definition, nextDefinitionRevision, nextRevision
	current.ChangeSequence, current.UpdatedAt = changeSequence, now
	return current, revision, nil
}

func insertWorkflowV2TaskTx(ctx context.Context, tx *sql.Tx, definition taskV2Definition, now time.Time) (taskV2Record, error) {
	encoded, err := json.Marshal(definition)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return taskV2Record{}, errRPCInvalid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tasks(
		id, project_id, kind, title, cwd, scope, owner_workflow_task_id, parent_task_id, root_task_id,
		definition_json, definition_revision, status, revision, current_run_id, next_log_sequence,
		scheduled_at_ms, created_at_ms, updated_at_ms, result_code
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'queued', 1, NULL, 1, ?, ?, ?, '')`,
		definition.ID.String(), definition.ProjectID.String(), definition.Kind, definition.Title, definition.CWD, definition.Scope,
		nullableUUIDString(definition.OwnerWorkflowTaskID), nullableUUIDString(definition.ParentTaskID), nullableUUIDString(definition.RootTaskID), encoded,
		nullableTimeMillis(definition.Execution.ScheduledAt), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		if isSQLiteConstraint(err) {
			return taskV2Record{}, errRPCRevision
		}
		return taskV2Record{}, err
	}
	return taskV2Record{
		Definition: definition, DefinitionRevision: 1, Status: "queued", Revision: 1,
		CreatedAt: now, UpdatedAt: now, LogState: taskLogStateNone,
	}, nil
}

func ensureWorkflowTaskIDsAvailable(ctx context.Context, tx *sql.Tx, parentID uuid.UUID, revision workflowV2Revision) error {
	ids := make([]uuid.UUID, 0, len(revision.Nodes)+2)
	if parentID != uuid.Nil {
		ids = append(ids, parentID)
	}
	for _, node := range revision.Nodes {
		if node.TaskDefinitionID != nil {
			ids = append(ids, *node.TaskDefinitionID)
		}
	}
	for _, id := range ids {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, id.String()).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errRPCRevision
		}
	}
	var revisionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_revisions WHERE id = ?`, revision.ID.String()).Scan(&revisionCount); err != nil {
		return err
	}
	if revisionCount != 0 {
		return errRPCRevision
	}
	return nil
}

func validateWorkflowV2SourcesTx(ctx context.Context, tx *sql.Tx, parent taskV2Definition, revision workflowV2Revision) error {
	for _, node := range revision.Nodes {
		if node.SourceTaskID == nil {
			continue
		}
		if *node.SourceTaskID == parent.ID || node.SourceTaskRevision == nil {
			return errRPCInvalid
		}
		source, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, node.SourceTaskID.String()))
		if errors.Is(err, sql.ErrNoRows) {
			return errRPCNotFound
		}
		if err != nil {
			return err
		}
		if source.Definition.ProjectID != parent.ProjectID {
			return errRPCProject
		}
		if source.Definition.Scope != "topLevel" {
			return errRPCInvalid
		}
		if source.DefinitionRevision != *node.SourceTaskRevision {
			return errRPCRevision
		}
	}
	return nil
}

func insertWorkflowV2RevisionTx(ctx context.Context, tx *sql.Tx, revision workflowV2Revision) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_revisions(
		id, workflow_task_id, version, description, failure_policy, graph_digest, created_at_ms, maximum_parallelism
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, revision.ID.String(), revision.WorkflowTaskID.String(), revision.Version, revision.Description,
		revision.FailurePolicy, revision.GraphDigest, revision.CreatedAt.UnixMilli(), revision.MaximumParallelism)
	if err != nil {
		if isSQLiteConstraint(err) {
			return errRPCRevision
		}
		return err
	}
	for _, node := range revision.Nodes {
		var snapshot any
		if node.TaskDefinition != nil {
			encoded, err := json.Marshal(node.TaskDefinition)
			if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
				return errRPCInvalid
			}
			snapshot = encoded
		}
		var sourceRevision any
		if node.SourceTaskRevision != nil {
			sourceRevision = *node.SourceTaskRevision
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_nodes(
			revision_id, node_id, node_type, task_definition_id, task_snapshot_json, source_task_id, source_task_revision, position_x, position_y
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, revision.ID.String(), node.ID, node.Type, nullableUUIDString(node.TaskDefinitionID), snapshot,
			nullableUUIDString(node.SourceTaskID), sourceRevision, node.Position.X, node.Position.Y); err != nil {
			return err
		}
	}
	for _, edge := range revision.Edges {
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_edges(revision_id, edge_id, source_id, target_id, edge_type, label)
			VALUES(?, ?, ?, ?, ?, ?)`, revision.ID.String(), edge.ID, edge.SourceID, edge.TargetID, edge.Type, nullableString(edge.Label)); err != nil {
			return err
		}
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (store *taskV2Store) GetWorkflowRevision(ctx context.Context, revisionID uuid.UUID) (workflowV2Revision, error) {
	if store == nil || store.business == nil || revisionID == uuid.Nil {
		return workflowV2Revision{}, errRPCInvalid
	}
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return workflowV2Revision{}, err
	}
	defer db.Close()
	return hydrateWorkflowV2Revision(ctx, db, revisionID)
}

func (store *taskV2Store) ListWorkflowRevisions(ctx context.Context, taskID uuid.UUID) ([]workflowV2Revision, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil {
		return nil, errRPCInvalid
	}
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id FROM workflow_revisions WHERE workflow_task_id = ? ORDER BY version DESC`, taskID.String())
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			_ = rows.Close()
			return nil, err
		}
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil {
			_ = rows.Close()
			return nil, errors.New("stored workflow revision identity is invalid")
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]workflowV2Revision, 0, len(ids))
	for _, id := range ids {
		revision, err := hydrateWorkflowV2Revision(ctx, db, id)
		if err != nil {
			return nil, err
		}
		result = append(result, revision)
	}
	return result, nil
}

func hydrateWorkflowV2Revision(ctx context.Context, query workflowV2Queryer, revisionID uuid.UUID) (workflowV2Revision, error) {
	var revision workflowV2Revision
	var rawID, rawTaskID string
	var version int64
	var createdAt int64
	var parallelism int64
	err := query.QueryRowContext(ctx, `SELECT id, workflow_task_id, version, description, failure_policy, graph_digest, created_at_ms, maximum_parallelism
		FROM workflow_revisions WHERE id = ?`, revisionID.String()).Scan(&rawID, &rawTaskID, &version, &revision.Description,
		&revision.FailurePolicy, &revision.GraphDigest, &createdAt, &parallelism)
	if errors.Is(err, sql.ErrNoRows) {
		return workflowV2Revision{}, errRPCNotFound
	}
	if err != nil {
		return workflowV2Revision{}, err
	}
	parsedID, idErr := uuid.Parse(rawID)
	parsedTaskID, taskErr := uuid.Parse(rawTaskID)
	if idErr != nil || taskErr != nil || parsedID != revisionID || parsedTaskID == uuid.Nil || version < 1 || parallelism < 0 || parallelism > maximumWorkflowV2Parallelism ||
		!slices.Contains(workflowV2FailurePolicies, revision.FailurePolicy) || len(revision.GraphDigest) != sha256.Size*2 || createdAt <= 0 {
		return workflowV2Revision{}, errors.New("stored workflow revision is invalid")
	}
	revision.ID, revision.WorkflowTaskID, revision.Version = parsedID, parsedTaskID, uint64(version)
	revision.MaximumParallelism, revision.CreatedAt = uint32(parallelism), time.UnixMilli(createdAt).UTC()
	nodeRows, err := query.QueryContext(ctx, `SELECT node_id, node_type, task_definition_id, task_snapshot_json, source_task_id,
		source_task_revision, position_x, position_y FROM workflow_nodes WHERE revision_id = ? ORDER BY node_id`, revisionID.String())
	if err != nil {
		return workflowV2Revision{}, err
	}
	for nodeRows.Next() {
		var node workflowV2Node
		var taskID, sourceID sql.NullString
		var snapshot []byte
		var sourceRevision sql.NullInt64
		if err := nodeRows.Scan(&node.ID, &node.Type, &taskID, &snapshot, &sourceID, &sourceRevision, &node.Position.X, &node.Position.Y); err != nil {
			_ = nodeRows.Close()
			return workflowV2Revision{}, err
		}
		parsedTaskID, taskErr := parseNullableUUID(taskID)
		parsedSourceID, sourceErr := parseNullableUUID(sourceID)
		if taskErr != nil || sourceErr != nil || !validWorkflowV2Identifier(node.ID) || !slices.Contains(workflowV2NodeTypes, node.Type) {
			_ = nodeRows.Close()
			return workflowV2Revision{}, errors.New("stored workflow node is invalid")
		}
		node.TaskDefinitionID, node.SourceTaskID = parsedTaskID, parsedSourceID
		if sourceRevision.Valid {
			if sourceRevision.Int64 < 1 {
				_ = nodeRows.Close()
				return workflowV2Revision{}, errors.New("stored workflow source revision is invalid")
			}
			value := uint64(sourceRevision.Int64)
			node.SourceTaskRevision = &value
		}
		if node.Type == "task" {
			if node.TaskDefinitionID == nil || len(snapshot) == 0 {
				_ = nodeRows.Close()
				return workflowV2Revision{}, errors.New("stored workflow task snapshot is missing")
			}
			definition, err := decodeTaskV2Definition(snapshot)
			if err != nil || definition.ID != *node.TaskDefinitionID || definition.OwnerWorkflowTaskID == nil || *definition.OwnerWorkflowTaskID != revision.WorkflowTaskID {
				_ = nodeRows.Close()
				return workflowV2Revision{}, errors.New("stored workflow task snapshot is invalid")
			}
			node.TaskDefinition = &definition
		} else if node.TaskDefinitionID != nil || len(snapshot) != 0 || node.SourceTaskID != nil || node.SourceTaskRevision != nil {
			_ = nodeRows.Close()
			return workflowV2Revision{}, errors.New("stored workflow control node is invalid")
		}
		revision.Nodes = append(revision.Nodes, node)
	}
	if err := nodeRows.Close(); err != nil {
		return workflowV2Revision{}, err
	}
	edgeRows, err := query.QueryContext(ctx, `SELECT edge_id, source_id, target_id, edge_type, label
		FROM workflow_edges WHERE revision_id = ? ORDER BY edge_id`, revisionID.String())
	if err != nil {
		return workflowV2Revision{}, err
	}
	for edgeRows.Next() {
		var edge workflowV2Edge
		var label sql.NullString
		if err := edgeRows.Scan(&edge.ID, &edge.SourceID, &edge.TargetID, &edge.Type, &label); err != nil {
			_ = edgeRows.Close()
			return workflowV2Revision{}, err
		}
		if label.Valid {
			edge.Label = label.String
		}
		revision.Edges = append(revision.Edges, edge)
	}
	if err := edgeRows.Close(); err != nil {
		return workflowV2Revision{}, err
	}
	if len(revision.Nodes) < 3 || len(revision.Nodes) > maximumWorkflowV2Nodes || len(revision.Edges) < 2 || len(revision.Edges) > maximumWorkflowV2Edges {
		return workflowV2Revision{}, errors.New("stored workflow graph size is invalid")
	}
	if _, ok := workflowV2TopologicalOrder(revision.Nodes, revision.Edges); !ok {
		return workflowV2Revision{}, errors.New("stored workflow graph is cyclic")
	}
	digest, err := workflowV2GraphDigest(revision)
	if err != nil || digest != revision.GraphDigest {
		return workflowV2Revision{}, errors.New("stored workflow graph digest mismatch")
	}
	return revision, nil
}

func (store *taskV2Store) StartWorkflowRun(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	now time.Time,
) (taskV2Record, taskV2Run, workflowV2Revision, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 || now.IsZero() {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	defer tx.Rollback()
	current, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	if current.Revision != expectedRevision || current.Status != "waiting" || current.Definition.Kind != "workflow" || current.CurrentRunID != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, current.Definition.ProjectID); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	revisionID, revisionVersion, err := workflowV2DefinitionRevision(current.Definition.Config)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	workflowRevision, err := hydrateWorkflowV2Revision(ctx, tx, revisionID)
	if err != nil || workflowRevision.WorkflowTaskID != taskID || workflowRevision.Version != revisionVersion {
		if err != nil {
			return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
		}
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, errRPCRevision
	}
	var nextAttempt int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), -1) + 1 FROM task_runs WHERE task_id = ?`, taskID.String()).Scan(&nextAttempt); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	if nextAttempt < 0 || uint64(nextAttempt) > uint64(^uint32(0)) {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, errRPCBusy
	}
	runID := uuid.New()
	run := taskV2Run{
		ID: runID, TaskID: taskID, WorkflowRevisionID: &revisionID, Status: "running", Attempt: uint32(nextAttempt),
		CreatedAt: now, StartedAt: timePointer(now),
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_runs(
		id, task_id, workflow_revision_id, status, attempt, created_at_ms, started_at_ms
	) VALUES(?, ?, ?, 'running', ?, ?, ?)`, runID.String(), taskID.String(), revisionID.String(), run.Attempt, now.UnixMilli(), now.UnixMilli()); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_runs(task_run_id, revision_id, created_at_ms) VALUES(?, ?, ?)`,
		runID.String(), revisionID.String(), now.UnixMilli()); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	for _, node := range workflowRevision.Nodes {
		status := "pending"
		if node.Type == "start" {
			status = "succeeded"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_node_runs(
			workflow_task_run_id, revision_id, node_id, child_task_run_id, status, attempt
		) VALUES(?, ?, ?, NULL, ?, 0)`, runID.String(), revisionID.String(), node.ID, status); err != nil {
			return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
		}
	}
	nextRevision := current.Revision + 1
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'running', revision = ?, current_run_id = ?, started_at_ms = ?,
		finished_at_ms = NULL, exit_code = NULL, result_code = '', updated_at_ms = ?
		WHERE id = ? AND revision = ? AND status = 'waiting' AND current_run_id IS NULL`,
		nextRevision, runID.String(), now.UnixMilli(), now.UnixMilli(), taskID.String(), expectedRevision)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, current.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	message := []byte("[WenzWork] Workflow execution started with immutable revision " + revisionID.String() + ".\n")
	if _, err := appendTaskV2LogTx(ctx, tx, taskID, &runID, "system", message, now, store.maximumLogBytesPerTask, store.maximumLogBytesGlobal); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, current.Definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, taskV2Run{}, workflowV2Revision{}, err
	}
	current.Status, current.Revision, current.ChangeSequence, current.CurrentRunID = "running", nextRevision, changeSequence, &runID
	current.StartedAt, current.FinishedAt, current.ExitCode, current.ResultCode, current.UpdatedAt = timePointer(now), nil, nil, "", now
	return current, run, workflowRevision, nil
}

func (store *taskV2Store) GetWorkflowRunSnapshot(
	ctx context.Context,
	taskID uuid.UUID,
	runID *uuid.UUID,
) (workflowV2RunSnapshot, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil {
		return workflowV2RunSnapshot{}, errRPCInvalid
	}
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return workflowV2RunSnapshot{}, err
	}
	defer db.Close()
	task, err := scanTaskV2(db.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return workflowV2RunSnapshot{}, errRPCNotFound
	}
	if err != nil || task.Definition.Kind != "workflow" {
		if err != nil {
			return workflowV2RunSnapshot{}, err
		}
		return workflowV2RunSnapshot{}, errRPCInvalid
	}
	selectedRunID := runID
	if selectedRunID == nil {
		selectedRunID = task.CurrentRunID
	}
	if selectedRunID == nil || *selectedRunID == uuid.Nil {
		return workflowV2RunSnapshot{}, errRPCNotFound
	}
	run, err := scanTaskV2Run(db.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
		FROM task_runs WHERE id = ?`, selectedRunID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return workflowV2RunSnapshot{}, errRPCNotFound
	}
	if err != nil || run.TaskID != taskID || run.WorkflowRevisionID == nil {
		if err != nil {
			return workflowV2RunSnapshot{}, err
		}
		return workflowV2RunSnapshot{}, errRPCRevision
	}
	revision, err := hydrateWorkflowV2Revision(ctx, db, *run.WorkflowRevisionID)
	if err != nil {
		return workflowV2RunSnapshot{}, err
	}
	nodeRuns, err := listWorkflowV2NodeRuns(ctx, db, run.ID)
	if err != nil {
		return workflowV2RunSnapshot{}, err
	}
	children := make([]taskV2Record, 0)
	seen := make(map[uuid.UUID]struct{})
	for _, nodeRun := range nodeRuns {
		if nodeRun.ChildTaskRunID == nil {
			continue
		}
		var rawTaskID string
		if err := db.QueryRowContext(ctx, `SELECT task_id FROM task_runs WHERE id = ?`, nodeRun.ChildTaskRunID.String()).Scan(&rawTaskID); err != nil {
			return workflowV2RunSnapshot{}, err
		}
		childTaskID, err := uuid.Parse(rawTaskID)
		if err != nil || childTaskID == uuid.Nil {
			return workflowV2RunSnapshot{}, errors.New("stored workflow child task identity is invalid")
		}
		if _, exists := seen[childTaskID]; exists {
			continue
		}
		child, err := scanTaskV2(db.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, childTaskID.String()))
		if err != nil {
			return workflowV2RunSnapshot{}, err
		}
		seen[childTaskID] = struct{}{}
		children = append(children, child)
	}
	sort.Slice(children, func(left, right int) bool {
		return children[left].Definition.ID.String() < children[right].Definition.ID.String()
	})
	return workflowV2RunSnapshot{Task: task, TaskRun: run, Revision: revision, NodeRuns: nodeRuns, ChildTasks: children}, nil
}

func listWorkflowV2NodeRuns(ctx context.Context, query workflowV2Queryer, workflowRunID uuid.UUID) ([]workflowV2NodeRun, error) {
	rows, err := query.QueryContext(ctx, `SELECT workflow_task_run_id, revision_id, node_id, child_task_run_id, status, attempt
		FROM workflow_node_runs WHERE workflow_task_run_id = ? ORDER BY node_id, attempt DESC`, workflowRunID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]workflowV2NodeRun, 0)
	seen := make(map[string]struct{})
	for rows.Next() {
		var rawRunID, rawRevisionID string
		var childID sql.NullString
		var attempt int64
		var nodeRun workflowV2NodeRun
		if err := rows.Scan(&rawRunID, &rawRevisionID, &nodeRun.NodeID, &childID, &nodeRun.Status, &attempt); err != nil {
			return nil, err
		}
		if _, found := seen[nodeRun.NodeID]; found {
			continue
		}
		parsedRunID, runErr := uuid.Parse(rawRunID)
		parsedRevisionID, revisionErr := uuid.Parse(rawRevisionID)
		parsedChildID, childErr := parseNullableUUID(childID)
		if runErr != nil || revisionErr != nil || childErr != nil || parsedRunID != workflowRunID || parsedRevisionID == uuid.Nil ||
			!validWorkflowV2Identifier(nodeRun.NodeID) || !validWorkflowV2NodeStatus(nodeRun.Status) || attempt < 0 || uint64(attempt) > uint64(^uint32(0)) {
			return nil, errors.New("stored workflow node run is invalid")
		}
		nodeRun.WorkflowTaskRunID, nodeRun.RevisionID, nodeRun.ChildTaskRunID, nodeRun.Attempt = parsedRunID, parsedRevisionID, parsedChildID, uint32(attempt)
		seen[nodeRun.NodeID] = struct{}{}
		result = append(result, nodeRun)
	}
	return result, rows.Err()
}

type workflowV2TickResult struct {
	Task           taskV2Record
	ScheduledCount int
	Finished       bool
}

func (store *taskV2Store) TickWorkflow(
	ctx context.Context,
	taskID uuid.UUID,
	parallelLimit int,
	now time.Time,
) (workflowV2TickResult, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || parallelLimit < 1 || now.IsZero() {
		return workflowV2TickResult{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return workflowV2TickResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return workflowV2TickResult{}, err
	}
	defer tx.Rollback()
	parent, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return workflowV2TickResult{}, errRPCNotFound
	}
	if err != nil {
		return workflowV2TickResult{}, err
	}
	if parent.Definition.Kind != "workflow" || parent.Status != "running" || parent.CurrentRunID == nil {
		return workflowV2TickResult{Task: parent}, nil
	}
	parentRun, err := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
		FROM task_runs WHERE id = ?`, parent.CurrentRunID.String()))
	if err != nil || parentRun.TaskID != taskID || parentRun.Status != "running" || parentRun.WorkflowRevisionID == nil {
		return workflowV2TickResult{}, errors.New("active workflow parent run is invalid")
	}
	var pinnedRevisionRaw string
	if err := tx.QueryRowContext(ctx, `SELECT revision_id FROM workflow_runs WHERE task_run_id = ?`, parentRun.ID.String()).Scan(&pinnedRevisionRaw); err != nil {
		return workflowV2TickResult{}, errors.New("active workflow snapshot is unavailable")
	}
	pinnedRevisionID, err := uuid.Parse(pinnedRevisionRaw)
	if err != nil || pinnedRevisionID == uuid.Nil || pinnedRevisionID != *parentRun.WorkflowRevisionID {
		return workflowV2TickResult{}, errors.New("active workflow snapshot identity is invalid")
	}
	revision, err := hydrateWorkflowV2Revision(ctx, tx, pinnedRevisionID)
	if err != nil || revision.WorkflowTaskID != taskID {
		return workflowV2TickResult{}, errors.New("active workflow revision is invalid")
	}
	nodeRuns, err := listWorkflowV2NodeRuns(ctx, tx, parentRun.ID)
	if err != nil || len(nodeRuns) != len(revision.Nodes) {
		return workflowV2TickResult{}, errors.New("active workflow node snapshot is invalid")
	}
	runsByNode := make(map[string]*workflowV2NodeRun, len(nodeRuns))
	for index := range nodeRuns {
		run := &nodeRuns[index]
		if run.RevisionID != pinnedRevisionID {
			return workflowV2TickResult{}, errors.New("active workflow node revision is invalid")
		}
		runsByNode[run.NodeID] = run
	}
	workflowChanged := false
	for _, node := range revision.Nodes {
		run := runsByNode[node.ID]
		if run == nil || node.Type != "task" || run.ChildTaskRunID == nil || workflowV2NodeTerminal(run.Status) {
			continue
		}
		childRun, err := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
			FROM task_runs WHERE id = ?`, run.ChildTaskRunID.String()))
		if err != nil || childRun.ParentWorkflowTaskRunID == nil || *childRun.ParentWorkflowTaskRunID != parentRun.ID || childRun.WorkflowNodeID != node.ID {
			return workflowV2TickResult{}, errors.New("workflow child run is invalid")
		}
		nextStatus := workflowV2NodeStatusForTaskStatus(childRun.Status)
		if nextStatus != run.Status {
			if err := updateWorkflowV2NodeRunStatusTx(ctx, tx, run, nextStatus); err != nil {
				return workflowV2TickResult{}, err
			}
			run.Status = nextStatus
			workflowChanged = true
		}
	}
	if revision.FailurePolicy == "stopOnFailure" {
		hasFailure := false
		for _, run := range runsByNode {
			if run.Status == "failed" || run.Status == "blocked" || run.Status == "cancelled" {
				hasFailure = true
				break
			}
		}
		if hasFailure {
			for _, run := range runsByNode {
				if run.ChildTaskRunID != nil || run.Status != "pending" && run.Status != "ready" {
					continue
				}
				if err := updateWorkflowV2NodeRunStatusTx(ctx, tx, run, "blocked"); err != nil {
					return workflowV2TickResult{}, err
				}
				run.Status = "blocked"
				workflowChanged = true
			}
		}
	}

	topology, ok := workflowV2TopologicalOrder(revision.Nodes, revision.Edges)
	if !ok {
		return workflowV2TickResult{}, errors.New("active workflow graph is cyclic")
	}
	nodesByID := make(map[string]workflowV2Node, len(revision.Nodes))
	incoming := make(map[string][]workflowV2Edge, len(revision.Nodes))
	for _, node := range revision.Nodes {
		nodesByID[node.ID] = node
	}
	for _, edge := range revision.Edges {
		incoming[edge.TargetID] = append(incoming[edge.TargetID], edge)
	}
	for _, nodeID := range topology {
		node := nodesByID[nodeID]
		run := runsByNode[nodeID]
		if run == nil || node.Type == "start" || run.ChildTaskRunID != nil || run.Status != "pending" && run.Status != "ready" {
			continue
		}
		allSettled, matched := true, false
		for _, edge := range incoming[nodeID] {
			source := runsByNode[edge.SourceID]
			if source == nil || !workflowV2NodeTerminal(source.Status) {
				allSettled = false
				break
			}
			matched = matched || workflowV2EdgeMatches(edge.Type, source.Status)
		}
		if !allSettled {
			continue
		}
		nextStatus := "skipped"
		if matched && node.Type == "finish" {
			nextStatus = "succeeded"
		} else if matched {
			nextStatus = "ready"
		}
		if nextStatus != run.Status {
			if err := updateWorkflowV2NodeRunStatusTx(ctx, tx, run, nextStatus); err != nil {
				return workflowV2TickResult{}, err
			}
			run.Status = nextStatus
			workflowChanged = true
		}
	}

	effectiveParallelism := parallelLimit
	if revision.MaximumParallelism > 0 && int(revision.MaximumParallelism) < effectiveParallelism {
		effectiveParallelism = int(revision.MaximumParallelism)
	}
	activeNodes := 0
	for _, run := range runsByNode {
		if run.Status == "running" {
			activeNodes++
		}
	}
	scheduledCount := 0
	for _, nodeID := range topology {
		if activeNodes >= effectiveParallelism {
			break
		}
		node := nodesByID[nodeID]
		run := runsByNode[nodeID]
		if node.Type != "task" || run == nil || run.Status != "ready" || run.ChildTaskRunID != nil || node.TaskDefinitionID == nil || node.TaskDefinition == nil {
			continue
		}
		child, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, node.TaskDefinitionID.String()))
		if err != nil || child.Definition.Scope != "workflowNode" || child.Definition.OwnerWorkflowTaskID == nil || *child.Definition.OwnerWorkflowTaskID != taskID ||
			child.Definition.ProjectID != parent.Definition.ProjectID || !sameTaskV2Definition(child.Definition, *node.TaskDefinition) ||
			child.Status == "waiting" || child.Status == "running" || child.Status == "awaitingAcceptance" {
			return workflowV2TickResult{}, errors.New("workflow child definition is unavailable")
		}
		var nextAttempt int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt), -1) + 1 FROM task_runs WHERE task_id = ?`, child.Definition.ID.String()).Scan(&nextAttempt); err != nil {
			return workflowV2TickResult{}, err
		}
		if nextAttempt < 0 || uint64(nextAttempt) > uint64(^uint32(0)) {
			return workflowV2TickResult{}, errRPCBusy
		}
		childRunID := uuid.New()
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_runs(
			id, task_id, workflow_revision_id, parent_workflow_task_run_id, workflow_node_id, status, attempt, created_at_ms
		) VALUES(?, ?, ?, ?, ?, 'waiting', ?, ?)`, childRunID.String(), child.Definition.ID.String(), pinnedRevisionID.String(),
			parentRun.ID.String(), node.ID, nextAttempt, now.UnixMilli()); err != nil {
			return workflowV2TickResult{}, err
		}
		nextChildRevision := child.Revision + 1
		childMutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'waiting', revision = ?, current_run_id = ?, started_at_ms = NULL,
			finished_at_ms = NULL, exit_code = NULL, result_code = '', updated_at_ms = ?
			WHERE id = ? AND revision = ? AND status NOT IN ('waiting','running','awaitingAcceptance')`,
			nextChildRevision, childRunID.String(), now.UnixMilli(), child.Definition.ID.String(), child.Revision)
		if err != nil {
			return workflowV2TickResult{}, err
		}
		if err := requireSingleTaskMutation(childMutation); err != nil {
			return workflowV2TickResult{}, err
		}
		nodeMutation, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET status = 'running', child_task_run_id = ?
			WHERE workflow_task_run_id = ? AND node_id = ? AND attempt = ? AND status = 'ready' AND child_task_run_id IS NULL`,
			childRunID.String(), parentRun.ID.String(), node.ID, run.Attempt)
		if err != nil {
			return workflowV2TickResult{}, err
		}
		if err := requireSingleTaskMutation(nodeMutation); err != nil {
			return workflowV2TickResult{}, err
		}
		if _, err := appendTaskV2Change(ctx, store.business, tx, child.Definition.ID, child.Definition.ProjectID, nextChildRevision, "upsert", now); err != nil {
			return workflowV2TickResult{}, err
		}
		run.Status, run.ChildTaskRunID = "running", &childRunID
		activeNodes++
		scheduledCount++
		workflowChanged = true
	}

	allTerminal, hasFailure := true, false
	finishSucceeded := false
	for _, node := range revision.Nodes {
		run := runsByNode[node.ID]
		if run == nil || !workflowV2NodeTerminal(run.Status) {
			allTerminal = false
			continue
		}
		if node.Type == "finish" {
			finishSucceeded = run.Status == "succeeded"
		}
		if run.Status == "failed" || run.Status == "blocked" || run.Status == "cancelled" {
			hasFailure = true
		}
	}
	finished := false
	if allTerminal {
		status, resultCode, exitCode := "awaitingAcceptance", "workflow_succeeded", 0
		if hasFailure && revision.FailurePolicy == "stopOnFailure" {
			status, resultCode, exitCode = "failed", "workflow_node_failed", 1
		} else if !finishSucceeded {
			status, resultCode, exitCode = "failed", "workflow_path_incomplete", 1
		} else if hasFailure {
			resultCode = "workflow_completed_with_failures"
		}
		nextRevision := parent.Revision + 1
		taskMutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, revision = ?, finished_at_ms = ?, exit_code = ?, result_code = ?, updated_at_ms = ?
			WHERE id = ? AND revision = ? AND status = 'running' AND current_run_id = ?`, status, nextRevision, now.UnixMilli(),
			exitCode, resultCode, now.UnixMilli(), taskID.String(), parent.Revision, parentRun.ID.String())
		if err != nil {
			return workflowV2TickResult{}, err
		}
		if err := requireSingleTaskMutation(taskMutation); err != nil {
			return workflowV2TickResult{}, err
		}
		runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = ?, finished_at_ms = ?, exit_code = ?, result_code = ?
			WHERE id = ? AND status = 'running'`, status, now.UnixMilli(), exitCode, resultCode, parentRun.ID.String())
		if err != nil {
			return workflowV2TickResult{}, err
		}
		if err := requireSingleTaskMutation(runMutation); err != nil {
			return workflowV2TickResult{}, err
		}
		changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, parent.Definition.ProjectID, nextRevision, "upsert", now)
		if err != nil {
			return workflowV2TickResult{}, err
		}
		message := "[WenzWork] Workflow execution completed and is awaiting acceptance.\n"
		if status == "failed" {
			message = "[WenzWork] Workflow execution failed.\n"
		}
		if _, err := appendTaskV2LogTx(ctx, tx, taskID, &parentRun.ID, "system", []byte(message), now, store.maximumLogBytesPerTask, store.maximumLogBytesGlobal); err != nil {
			return workflowV2TickResult{}, err
		}
		parent.Status, parent.Revision, parent.ChangeSequence = status, nextRevision, changeSequence
		parent.FinishedAt, parent.ExitCode, parent.ResultCode, parent.UpdatedAt = timePointer(now), &exitCode, resultCode, now
		finished = true
	} else if workflowChanged {
		nextRevision := parent.Revision + 1
		mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET revision = ?, updated_at_ms = ? WHERE id = ? AND revision = ? AND status = 'running'`,
			nextRevision, now.UnixMilli(), taskID.String(), parent.Revision)
		if err != nil {
			return workflowV2TickResult{}, err
		}
		if err := requireSingleTaskMutation(mutation); err != nil {
			return workflowV2TickResult{}, err
		}
		changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, parent.Definition.ProjectID, nextRevision, "upsert", now)
		if err != nil {
			return workflowV2TickResult{}, err
		}
		parent.Revision, parent.ChangeSequence, parent.UpdatedAt = nextRevision, changeSequence, now
	}
	if err := pruneTaskV2Changes(ctx, tx, parent.Definition.ProjectID, store.maximumChanges); err != nil {
		return workflowV2TickResult{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return workflowV2TickResult{}, err
	}
	return workflowV2TickResult{Task: parent, ScheduledCount: scheduledCount, Finished: finished}, nil
}

type workflowV2NodeRetryResult struct {
	Task    taskV2Record      `json:"task"`
	TaskRun taskV2Run         `json:"taskRun"`
	NodeRun workflowV2NodeRun `json:"nodeRun"`
	Resumed bool              `json:"resumed"`
}

func (store *taskV2Store) RetryWorkflowNode(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	nodeID string,
	now time.Time,
) (workflowV2NodeRetryResult, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 ||
		!validWorkflowV2Identifier(nodeID) || now.IsZero() {
		return workflowV2NodeRetryResult{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	defer tx.Rollback()
	parent, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return workflowV2NodeRetryResult{}, errRPCNotFound
	}
	if err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	if parent.Revision != expectedRevision || parent.Definition.Kind != "workflow" || parent.Status != "failed" || parent.CurrentRunID == nil {
		return workflowV2NodeRetryResult{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, parent.Definition.ProjectID); err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	parentRun, err := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
		FROM task_runs WHERE id = ?`, parent.CurrentRunID.String()))
	if err != nil || parentRun.TaskID != taskID || parentRun.Status != "failed" || parentRun.WorkflowRevisionID == nil {
		return workflowV2NodeRetryResult{}, errRPCRevision
	}
	revision, err := hydrateWorkflowV2Revision(ctx, tx, *parentRun.WorkflowRevisionID)
	if err != nil || revision.WorkflowTaskID != taskID {
		if err != nil {
			return workflowV2NodeRetryResult{}, err
		}
		return workflowV2NodeRetryResult{}, errRPCRevision
	}
	nodeIndex := slices.IndexFunc(revision.Nodes, func(node workflowV2Node) bool { return node.ID == nodeID })
	if nodeIndex < 0 || revision.Nodes[nodeIndex].Type != "task" {
		return workflowV2NodeRetryResult{}, errRPCInvalid
	}
	nodeRuns, err := listWorkflowV2NodeRuns(ctx, tx, parentRun.ID)
	if err != nil || len(nodeRuns) != len(revision.Nodes) {
		return workflowV2NodeRetryResult{}, errRPCRevision
	}
	targetIndex := slices.IndexFunc(nodeRuns, func(run workflowV2NodeRun) bool { return run.NodeID == nodeID })
	if targetIndex < 0 {
		return workflowV2NodeRetryResult{}, errRPCRevision
	}
	target := nodeRuns[targetIndex]
	if target.ChildTaskRunID == nil || target.Status != "failed" && target.Status != "blocked" || target.Attempt == ^uint32(0) {
		return workflowV2NodeRetryResult{}, errRPCRevision
	}
	otherFailures := 0
	for index := range nodeRuns {
		run := nodeRuns[index]
		if index != targetIndex && run.ChildTaskRunID != nil && (run.Status == "failed" || run.Status == "blocked") {
			otherFailures++
		}
	}
	retried := workflowV2NodeRun{
		WorkflowTaskRunID: parentRun.ID,
		RevisionID:        *parentRun.WorkflowRevisionID,
		NodeID:            nodeID,
		Status:            "pending",
		Attempt:           target.Attempt + 1,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_node_runs(
		workflow_task_run_id, revision_id, node_id, child_task_run_id, status, attempt
	) VALUES(?, ?, ?, NULL, 'pending', ?)`, parentRun.ID.String(), parentRun.WorkflowRevisionID.String(), nodeID, retried.Attempt); err != nil {
		if isSQLiteConstraint(err) {
			return workflowV2NodeRetryResult{}, errRPCRevision
		}
		return workflowV2NodeRetryResult{}, err
	}
	resumed := otherFailures == 0
	if resumed {
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET status = 'pending'
			WHERE workflow_task_run_id = ? AND child_task_run_id IS NULL AND status = 'blocked'`, parentRun.ID.String()); err != nil {
			return workflowV2NodeRetryResult{}, err
		}
	}
	nextRevision := parent.Revision + 1
	if resumed {
		taskMutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'running', revision = ?, finished_at_ms = NULL,
			exit_code = NULL, result_code = '', updated_at_ms = ?
			WHERE id = ? AND revision = ? AND status = 'failed' AND current_run_id = ?`, nextRevision, now.UnixMilli(),
			taskID.String(), expectedRevision, parentRun.ID.String())
		if err != nil {
			return workflowV2NodeRetryResult{}, err
		}
		if err := requireSingleTaskMutation(taskMutation); err != nil {
			return workflowV2NodeRetryResult{}, err
		}
		runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'running', finished_at_ms = NULL,
			exit_code = NULL, result_code = '' WHERE id = ? AND status = 'failed'`, parentRun.ID.String())
		if err != nil {
			return workflowV2NodeRetryResult{}, err
		}
		if err := requireSingleTaskMutation(runMutation); err != nil {
			return workflowV2NodeRetryResult{}, err
		}
		parent.Status, parent.FinishedAt, parent.ExitCode, parent.ResultCode = "running", nil, nil, ""
		parentRun.Status, parentRun.FinishedAt, parentRun.ExitCode, parentRun.ResultCode = "running", nil, nil, ""
	} else {
		taskMutation, err := tx.ExecContext(ctx, `UPDATE tasks SET revision = ?, updated_at_ms = ?
			WHERE id = ? AND revision = ? AND status = 'failed' AND current_run_id = ?`, nextRevision, now.UnixMilli(),
			taskID.String(), expectedRevision, parentRun.ID.String())
		if err != nil {
			return workflowV2NodeRetryResult{}, err
		}
		if err := requireSingleTaskMutation(taskMutation); err != nil {
			return workflowV2NodeRetryResult{}, err
		}
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, parent.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	message := []byte("[WenzWork] Workflow node retry registered: " + nodeID + ".\n")
	if _, err := appendTaskV2LogTx(ctx, tx, taskID, &parentRun.ID, "system", message, now,
		store.maximumLogBytesPerTask, store.maximumLogBytesGlobal); err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, parent.Definition.ProjectID, store.maximumChanges); err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return workflowV2NodeRetryResult{}, err
	}
	parent.Revision, parent.ChangeSequence, parent.UpdatedAt = nextRevision, changeSequence, now
	return workflowV2NodeRetryResult{Task: parent, TaskRun: parentRun, NodeRun: retried, Resumed: resumed}, nil
}

type workflowV2CancelResult struct {
	Task            taskV2Record
	RunningChildren []taskV2Record
}

func (store *taskV2Store) CancelWorkflow(
	ctx context.Context,
	taskID uuid.UUID,
	expectedRevision uint64,
	now time.Time,
) (workflowV2CancelResult, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || expectedRevision == 0 || now.IsZero() {
		return workflowV2CancelResult{}, errRPCInvalid
	}
	now = now.UTC()
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return workflowV2CancelResult{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return workflowV2CancelResult{}, err
	}
	defer tx.Rollback()
	parent, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return workflowV2CancelResult{}, errRPCNotFound
	}
	if err != nil {
		return workflowV2CancelResult{}, err
	}
	if parent.Revision != expectedRevision || parent.Definition.Kind != "workflow" || parent.Status != "running" || parent.CurrentRunID == nil {
		return workflowV2CancelResult{}, errRPCRevision
	}
	if err := requireTaskProjectPolicy(ctx, tx, store.business.deviceID, parent.Definition.ProjectID); err != nil {
		return workflowV2CancelResult{}, err
	}
	parentRun, err := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
		FROM task_runs WHERE id = ?`, parent.CurrentRunID.String()))
	if err != nil || parentRun.TaskID != taskID || parentRun.Status != "running" || parentRun.WorkflowRevisionID == nil {
		return workflowV2CancelResult{}, errRPCRevision
	}
	nodeRuns, err := listWorkflowV2NodeRuns(ctx, tx, parentRun.ID)
	if err != nil {
		return workflowV2CancelResult{}, err
	}
	runningChildren := make([]taskV2Record, 0)
	for index := range nodeRuns {
		nodeRun := &nodeRuns[index]
		if workflowV2NodeTerminal(nodeRun.Status) {
			continue
		}
		if nodeRun.ChildTaskRunID == nil {
			if err := updateWorkflowV2NodeRunStatusTx(ctx, tx, nodeRun, "cancelled"); err != nil {
				return workflowV2CancelResult{}, err
			}
			nodeRun.Status = "cancelled"
			continue
		}
		childRun, err := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
			FROM task_runs WHERE id = ?`, nodeRun.ChildTaskRunID.String()))
		if err != nil || childRun.ParentWorkflowTaskRunID == nil || *childRun.ParentWorkflowTaskRunID != parentRun.ID {
			return workflowV2CancelResult{}, errRPCRevision
		}
		child, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, childRun.TaskID.String()))
		if err != nil || child.CurrentRunID == nil || *child.CurrentRunID != childRun.ID {
			return workflowV2CancelResult{}, errRPCRevision
		}
		if childRun.Status == "running" && child.Status == "running" {
			runningChildren = append(runningChildren, child)
			continue
		}
		if childRun.Status == "queued" || childRun.Status == "waiting" || childRun.Status == "awaitingAcceptance" ||
			child.Status == "queued" || child.Status == "waiting" || child.Status == "awaitingAcceptance" {
			nextChildRevision := child.Revision + 1
			childMutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'cancelled', revision = ?, finished_at_ms = ?,
				exit_code = NULL, result_code = 'cancelled', updated_at_ms = ?
				WHERE id = ? AND revision = ? AND current_run_id = ? AND status IN ('queued','waiting','awaitingAcceptance')`,
				nextChildRevision, now.UnixMilli(), now.UnixMilli(), child.Definition.ID.String(), child.Revision, childRun.ID.String())
			if err != nil {
				return workflowV2CancelResult{}, err
			}
			if err := requireSingleTaskMutation(childMutation); err != nil {
				return workflowV2CancelResult{}, err
			}
			runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'cancelled', finished_at_ms = ?,
				exit_code = NULL, result_code = 'cancelled' WHERE id = ? AND status IN ('queued','waiting','awaitingAcceptance')`,
				now.UnixMilli(), childRun.ID.String())
			if err != nil {
				return workflowV2CancelResult{}, err
			}
			if err := requireSingleTaskMutation(runMutation); err != nil {
				return workflowV2CancelResult{}, err
			}
			if _, err := appendTaskV2Change(ctx, store.business, tx, child.Definition.ID, child.Definition.ProjectID, nextChildRevision, "upsert", now); err != nil {
				return workflowV2CancelResult{}, err
			}
			if err := updateWorkflowV2NodeRunStatusTx(ctx, tx, nodeRun, "cancelled"); err != nil {
				return workflowV2CancelResult{}, err
			}
			nodeRun.Status = "cancelled"
			continue
		}
		nextStatus := workflowV2NodeStatusForTaskStatus(childRun.Status)
		if !workflowV2NodeTerminal(nextStatus) {
			nextStatus = "cancelled"
		}
		if err := updateWorkflowV2NodeRunStatusTx(ctx, tx, nodeRun, nextStatus); err != nil {
			return workflowV2CancelResult{}, err
		}
		nodeRun.Status = nextStatus
	}
	nextRevision := parent.Revision + 1
	parentMutation, err := tx.ExecContext(ctx, `UPDATE tasks SET status = 'cancelled', revision = ?, finished_at_ms = ?,
		exit_code = NULL, result_code = 'cancelled', updated_at_ms = ?
		WHERE id = ? AND revision = ? AND status = 'running' AND current_run_id = ?`, nextRevision, now.UnixMilli(),
		now.UnixMilli(), taskID.String(), expectedRevision, parentRun.ID.String())
	if err != nil {
		return workflowV2CancelResult{}, err
	}
	if err := requireSingleTaskMutation(parentMutation); err != nil {
		return workflowV2CancelResult{}, err
	}
	runMutation, err := tx.ExecContext(ctx, `UPDATE task_runs SET status = 'cancelled', finished_at_ms = ?,
		exit_code = NULL, result_code = 'cancelled' WHERE id = ? AND status = 'running'`, now.UnixMilli(), parentRun.ID.String())
	if err != nil {
		return workflowV2CancelResult{}, err
	}
	if err := requireSingleTaskMutation(runMutation); err != nil {
		return workflowV2CancelResult{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, parent.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return workflowV2CancelResult{}, err
	}
	if _, err := appendTaskV2LogTx(ctx, tx, taskID, &parentRun.ID, "system", []byte("[WenzWork] Workflow execution cancelled.\n"),
		now, store.maximumLogBytesPerTask, store.maximumLogBytesGlobal); err != nil {
		return workflowV2CancelResult{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, parent.Definition.ProjectID, store.maximumChanges); err != nil {
		return workflowV2CancelResult{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return workflowV2CancelResult{}, err
	}
	parent.Status, parent.Revision, parent.ChangeSequence, parent.FinishedAt, parent.ExitCode, parent.ResultCode, parent.UpdatedAt =
		"cancelled", nextRevision, changeSequence, timePointer(now), nil, "cancelled", now
	return workflowV2CancelResult{Task: parent, RunningChildren: runningChildren}, nil
}

func (store *taskV2Store) FinalizeCancelledWorkflow(
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
	parent, err := scanTaskV2(tx.QueryRowContext(ctx, `SELECT `+taskV2SelectColumns+` FROM tasks WHERE id = ?`, taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Record{}, errRPCNotFound
	}
	if err != nil {
		return taskV2Record{}, err
	}
	if parent.Revision != expectedRevision || parent.Definition.Kind != "workflow" || parent.Status != "cancelled" || parent.CurrentRunID == nil {
		return taskV2Record{}, errRPCRevision
	}
	nodeRuns, err := listWorkflowV2NodeRuns(ctx, tx, *parent.CurrentRunID)
	if err != nil {
		return taskV2Record{}, err
	}
	changed := false
	for index := range nodeRuns {
		nodeRun := &nodeRuns[index]
		if workflowV2NodeTerminal(nodeRun.Status) {
			continue
		}
		nextStatus := "cancelled"
		if nodeRun.ChildTaskRunID != nil {
			childRun, childErr := scanTaskV2Run(tx.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
				FROM task_runs WHERE id = ?`, nodeRun.ChildTaskRunID.String()))
			if childErr == nil {
				projected := workflowV2NodeStatusForTaskStatus(childRun.Status)
				if workflowV2NodeTerminal(projected) {
					nextStatus = projected
				} else {
					continue
				}
			} else if !errors.Is(childErr, sql.ErrNoRows) {
				return taskV2Record{}, childErr
			}
		}
		if err := updateWorkflowV2NodeRunStatusTx(ctx, tx, nodeRun, nextStatus); err != nil {
			return taskV2Record{}, err
		}
		nodeRun.Status = nextStatus
		changed = true
	}
	if !changed {
		if err := commitBusinessTransaction(ctx, tx); err != nil {
			return taskV2Record{}, err
		}
		return parent, nil
	}
	nextRevision := parent.Revision + 1
	mutation, err := tx.ExecContext(ctx, `UPDATE tasks SET revision = ?, updated_at_ms = ?
		WHERE id = ? AND revision = ? AND status = 'cancelled'`, nextRevision, now.UnixMilli(), taskID.String(), expectedRevision)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := requireSingleTaskMutation(mutation); err != nil {
		return taskV2Record{}, err
	}
	changeSequence, err := appendTaskV2Change(ctx, store.business, tx, taskID, parent.Definition.ProjectID, nextRevision, "upsert", now)
	if err != nil {
		return taskV2Record{}, err
	}
	if err := pruneTaskV2Changes(ctx, tx, parent.Definition.ProjectID, store.maximumChanges); err != nil {
		return taskV2Record{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return taskV2Record{}, err
	}
	parent.Revision, parent.ChangeSequence, parent.UpdatedAt = nextRevision, changeSequence, now
	return parent, nil
}

func updateWorkflowV2NodeRunStatusTx(ctx context.Context, tx *sql.Tx, run *workflowV2NodeRun, nextStatus string) error {
	if run == nil || !validWorkflowV2NodeStatus(nextStatus) {
		return errRPCInvalid
	}
	mutation, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs SET status = ?
		WHERE workflow_task_run_id = ? AND node_id = ? AND attempt = ? AND status = ?`, nextStatus,
		run.WorkflowTaskRunID.String(), run.NodeID, run.Attempt, run.Status)
	if err != nil {
		return err
	}
	return requireSingleTaskMutation(mutation)
}

func workflowV2NodeStatusForTaskStatus(status string) string {
	switch status {
	case "queued", "waiting", "running", "awaitingAcceptance":
		return "running"
	case "changesRequested", "completed", "succeeded":
		return "succeeded"
	case "failed":
		return "failed"
	case "blocked":
		return "blocked"
	case "cancelled":
		return "cancelled"
	default:
		return "failed"
	}
}

func workflowV2NodeTerminal(status string) bool {
	return status == "succeeded" || status == "failed" || status == "blocked" || status == "cancelled" || status == "skipped"
}

func workflowV2EdgeMatches(edgeType, sourceStatus string) bool {
	if sourceStatus == "skipped" {
		return false
	}
	if edgeType == "always" {
		return workflowV2NodeTerminal(sourceStatus)
	}
	if edgeType == "onSuccess" {
		return sourceStatus == "succeeded"
	}
	return edgeType == "onFailure" && (sourceStatus == "failed" || sourceStatus == "blocked" || sourceStatus == "cancelled")
}

func sameTaskV2Definition(left, right taskV2Definition) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}
