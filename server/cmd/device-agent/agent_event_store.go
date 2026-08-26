package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The event journal is intentionally small. It is a durable hint/outbox, not
// a second copy of messages, task definitions, terminal output, or logs.
const (
	maximumAgentEventRetention               = 4096
	maximumAgentEventPayloadBytes            = 4 << 10
	maximumAgentEventQueueCount              = 256
	maximumAgentEventQueueBytes              = 512 << 10
	maximumAgentEventSubscriptions           = 8
	maximumAgentEventSubscriptionsPerProject = 4
	minimumAgentEventHeartbeatSeconds        = 15
	maximumAgentEventHeartbeatSeconds        = 60
	agentEventRetentionAge                   = 24 * time.Hour
	agentEventConversationHintInterval       = 250 * time.Millisecond
)

type agentEventCursor struct {
	Kind  string `json:"kind"`
	Value uint64 `json:"value"`
}

type agentEventRecord struct {
	ProjectID          uuid.UUID
	Sequence           uint64
	EventID            string
	Topic              string
	EventType          string
	AggregateType      string
	AggregateID        string
	Operation          string
	Revision           uint64
	Cursor             agentEventCursor
	Data               map[string]any
	CausationRequestID string
	OccurredAt         time.Time
	SafePayloadJSON    []byte
}

type agentEventStreamInfo struct {
	MinimumAvailableSequence uint64
	HighWatermark            uint64
}

func migrateAgentEventSchemaV12(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agent_event_streams (
        project_id TEXT PRIMARY KEY,
        high_watermark INTEGER NOT NULL
    )`); err != nil {
		return fmt.Errorf("create Agent event streams: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS agent_event_journal (
        project_id TEXT NOT NULL,
        sequence INTEGER NOT NULL,
        event_id TEXT NOT NULL UNIQUE,
        topic TEXT NOT NULL,
        event_type TEXT NOT NULL,
        aggregate_type TEXT NOT NULL,
        aggregate_id TEXT NOT NULL,
        operation TEXT NOT NULL,
        revision INTEGER NOT NULL DEFAULT 0,
        domain_cursor_kind TEXT NOT NULL DEFAULT '',
        domain_cursor INTEGER NOT NULL DEFAULT 0,
        safe_payload_json TEXT NOT NULL DEFAULT '{}',
        causation_request_id TEXT NOT NULL DEFAULT '',
        occurred_at_ms INTEGER NOT NULL,
        PRIMARY KEY (project_id, sequence),
        CHECK(sequence > 0),
        CHECK(revision >= 0),
        CHECK(domain_cursor >= 0),
        CHECK(operation IN ('upsert','delete','status','invalidate'))
    )`); err != nil {
		return fmt.Errorf("create Agent event journal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS agent_event_journal_project_sequence_idx
        ON agent_event_journal(project_id, sequence)`); err != nil {
		return fmt.Errorf("create Agent event journal index: %w", err)
	}
	return nil
}

func newTaskChangedAgentEvent(projectID, taskID uuid.UUID, revision, changeSequence uint64, operation string, occurredAt time.Time) agentEventRecord {
	return agentEventRecord{
		ProjectID: projectID, Topic: "task", EventType: "task.changed", AggregateType: "task",
		AggregateID: taskID.String(), Operation: operation, Revision: revision,
		Cursor: agentEventCursor{Kind: "task_changes", Value: changeSequence}, OccurredAt: occurredAt,
	}
}

func newTaskLogsAvailableAgentEvent(projectID, taskID uuid.UUID, runID *uuid.UUID, logSequence uint64, occurredAt time.Time) agentEventRecord {
	data := map[string]any{"highWatermark": logSequence}
	if runID != nil && *runID != uuid.Nil {
		data["runId"] = runID.String()
	}
	return agentEventRecord{
		ProjectID: projectID, Topic: "taskLog", EventType: "task.logs.available", AggregateType: "task",
		AggregateID: taskID.String(), Operation: "status", Revision: logSequence,
		Cursor: agentEventCursor{Kind: "task_logs", Value: logSequence}, Data: data, OccurredAt: occurredAt,
	}
}

func newTaskLogBytesAvailableAgentEvent(projectID, taskID, runID uuid.UUID, generation, size uint64, operation string, occurredAt time.Time) agentEventRecord {
	if operation == "" {
		operation = "status"
	}
	return agentEventRecord{
		ProjectID: projectID, Topic: "taskLog", EventType: "task.logs.available", AggregateType: "task",
		AggregateID: taskID.String(), Operation: operation, Revision: size,
		Cursor:     agentEventCursor{Kind: "task_log_bytes", Value: size},
		Data:       map[string]any{"runId": runID.String(), "generation": generation, "highWatermark": size},
		OccurredAt: occurredAt,
	}
}

func newConversationChangedAgentEvent(value conversationView, deleted bool, changeSequence uint64, occurredAt time.Time) agentEventRecord {
	operation := "upsert"
	if deleted {
		operation = "delete"
	}
	return agentEventRecord{
		ProjectID: mustAgentEventUUID(value.ProjectID), Topic: "conversation", EventType: "conversation.changed",
		AggregateType: "conversation", AggregateID: value.ID, Operation: operation, Revision: value.Revision,
		Cursor: agentEventCursor{Kind: "ai_conversation_changes", Value: changeSequence}, OccurredAt: occurredAt,
		Data: map[string]any{"state": value.State, "lastMessageSequence": value.LastMessageSequence},
	}
}

func newConversationEventsAvailableAgentEvent(projectID uuid.UUID, event aiConversationEvent) agentEventRecord {
	data := map[string]any{}
	if event.GenerationID != "" {
		data["generationId"] = event.GenerationID
	}
	return agentEventRecord{
		ProjectID: projectID, Topic: "message", EventType: "conversation.events.available",
		AggregateType: "conversation", AggregateID: event.ConversationID, Operation: "status", Revision: event.Sequence,
		Cursor: agentEventCursor{Kind: "ai_conversation_events", Value: event.Sequence}, OccurredAt: event.OccurredAt, Data: data,
	}
}

func mustAgentEventUUID(value string) uuid.UUID {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

func appendAgentEvent(ctx context.Context, tx *sql.Tx, event agentEventRecord) (agentEventRecord, error) {
	if tx == nil || !validAgentEvent(event) {
		return agentEventRecord{}, errRPCInvalid
	}
	payload, err := event.safePayload()
	if err != nil {
		return agentEventRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_event_streams(project_id, high_watermark)
        VALUES(?, 0) ON CONFLICT(project_id) DO NOTHING`, event.ProjectID.String()); err != nil {
		return agentEventRecord{}, fmt.Errorf("create Agent event stream: %w", err)
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `UPDATE agent_event_streams SET high_watermark = high_watermark + 1
		WHERE project_id = ? RETURNING high_watermark`, event.ProjectID.String()).Scan(&sequence); err != nil || sequence < 1 || sequence > int64(maxSafeJSONInteger) {
		return agentEventRecord{}, firstError(err, errors.New("Agent event sequence is exhausted"))
	}
	event.Sequence = uint64(sequence)
	event.EventID = uuid.NewString()
	event.SafePayloadJSON = payload
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_event_journal(
        project_id, sequence, event_id, topic, event_type, aggregate_type, aggregate_id, operation,
        revision, domain_cursor_kind, domain_cursor, safe_payload_json, causation_request_id, occurred_at_ms
    ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		event.ProjectID.String(), event.Sequence, event.EventID, event.Topic, event.EventType, event.AggregateType,
		event.AggregateID, event.Operation, event.Revision, event.Cursor.Kind, event.Cursor.Value, string(payload),
		event.CausationRequestID, event.OccurredAt.UTC().UnixMilli()); err != nil {
		return agentEventRecord{}, fmt.Errorf("append Agent event: %w", err)
	}
	retentionFloor := uint64(0)
	if event.Sequence > maximumAgentEventRetention {
		retentionFloor = event.Sequence - maximumAgentEventRetention
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_event_journal
        WHERE project_id = ? AND (sequence <= ? OR occurred_at_ms < ?)`, event.ProjectID.String(),
		retentionFloor, event.OccurredAt.UTC().Add(-agentEventRetentionAge).UnixMilli()); err != nil {
		return agentEventRecord{}, fmt.Errorf("prune Agent event journal: %w", err)
	}
	return event, nil
}

// appendAgentEvent records the durable outbox entry and wakes the process-local
// publisher. Production writers hold store.mu until their transaction commits,
// so the pump's matching lock is a commit barrier rather than a polling loop.
func (store *businessStore) appendAgentEvent(ctx context.Context, tx *sql.Tx, event agentEventRecord) (agentEventRecord, error) {
	stored, err := appendAgentEvent(ctx, tx, event)
	if err == nil {
		store.wakeAgentEvents()
	}
	return stored, err
}

func validAgentEvent(event agentEventRecord) bool {
	if event.ProjectID == uuid.Nil || uuid.Validate(event.AggregateID) != nil || !validAgentEventTopic(event.Topic, event.EventType) ||
		!validAgentEventOperation(event.Operation) || event.Revision > maxSafeJSONInteger || event.Cursor.Value > maxSafeJSONInteger ||
		event.Cursor.Kind == "" || event.OccurredAt.IsZero() {
		return false
	}
	if event.CausationRequestID != "" && uuid.Validate(event.CausationRequestID) != nil {
		return false
	}
	return validAgentEventData(event.EventType, event.Data)
}

func validAgentEventTopic(topic, eventType string) bool {
	switch eventType {
	case "task.changed":
		return topic == "task"
	case "conversation.changed":
		return topic == "conversation"
	case "conversation.events.available":
		return topic == "message"
	case "task.logs.available":
		return topic == "taskLog"
	case "capabilities.changed":
		return topic == "capabilities"
	case "agent.status.changed":
		return topic == "agent"
	case "workflow.changed":
		return topic == "workflow"
	default:
		return false
	}
}

func validAgentEventOperation(value string) bool {
	switch value {
	case "upsert", "delete", "status", "invalidate":
		return true
	default:
		return false
	}
}

func validAgentEventData(eventType string, data map[string]any) bool {
	if len(data) == 0 {
		return true
	}
	allowed := map[string]struct{}{}
	switch eventType {
	case "task.changed":
		allowed["status"] = struct{}{}
	case "conversation.changed":
		allowed["state"] = struct{}{}
		allowed["lastMessageSequence"] = struct{}{}
	case "conversation.events.available":
		allowed["generationId"] = struct{}{}
	case "task.logs.available":
		allowed["runId"] = struct{}{}
		allowed["generation"] = struct{}{}
		allowed["highWatermark"] = struct{}{}
	case "capabilities.changed":
		allowed["capabilitiesRevision"] = struct{}{}
	case "agent.status.changed":
		allowed["status"] = struct{}{}
		allowed["activeTaskCount"] = struct{}{}
		allowed["activeGenerationCount"] = struct{}{}
	}
	for key, value := range data {
		if _, ok := allowed[key]; !ok || !validAgentEventScalar(value) {
			return false
		}
	}
	return true
}

func validAgentEventScalar(value any) bool {
	switch typed := value.(type) {
	case string:
		return len(typed) <= 160
	case bool:
		return true
	case uint64:
		return typed <= maxSafeJSONInteger
	case int:
		return typed >= 0 && uint64(typed) <= maxSafeJSONInteger
	case int64:
		return typed >= 0 && uint64(typed) <= maxSafeJSONInteger
	case float64:
		return typed >= 0 && typed == float64(uint64(typed)) && typed <= float64(maxSafeJSONInteger)
	default:
		return false
	}
}

func (event agentEventRecord) safePayload() ([]byte, error) {
	if !validAgentEvent(event) {
		return nil, errRPCInvalid
	}
	payload := map[string]any{
		"schemaVersion": 1,
		"projectId":     event.ProjectID.String(),
		"topic":         event.Topic,
		"type":          event.EventType,
		"aggregateType": event.AggregateType,
		"aggregateId":   event.AggregateID,
		"operation":     event.Operation,
		"revision":      event.Revision,
		"cursor": map[string]any{
			"kind":  event.Cursor.Kind,
			"value": event.Cursor.Value,
		},
	}
	if len(event.Data) > 0 {
		payload["data"] = event.Data
	}
	if event.CausationRequestID != "" {
		payload["causationRequestId"] = event.CausationRequestID
	}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumAgentEventPayloadBytes {
		return nil, firstError(err, errRPCInvalid)
	}
	return encoded, nil
}

func (store *businessStore) agentEventStreamInfo(ctx context.Context, projectID uuid.UUID) (agentEventStreamInfo, error) {
	if store == nil || projectID == uuid.Nil {
		return agentEventStreamInfo{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return agentEventStreamInfo{}, err
	}
	defer db.Close()
	var high int64
	err = db.QueryRowContext(ctx, `SELECT high_watermark FROM agent_event_streams WHERE project_id = ?`, projectID.String()).Scan(&high)
	if errors.Is(err, sql.ErrNoRows) {
		return agentEventStreamInfo{MinimumAvailableSequence: 1}, nil
	}
	if err != nil || high < 0 || high > int64(maxSafeJSONInteger) {
		return agentEventStreamInfo{}, firstError(err, errors.New("Agent event stream is invalid"))
	}
	var minimum sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(sequence) FROM agent_event_journal WHERE project_id = ?`, projectID.String()).Scan(&minimum); err != nil {
		return agentEventStreamInfo{}, err
	}
	result := agentEventStreamInfo{HighWatermark: uint64(high)}
	if minimum.Valid {
		if minimum.Int64 < 1 || minimum.Int64 > int64(maxSafeJSONInteger) {
			return agentEventStreamInfo{}, errors.New("Agent event retention is invalid")
		}
		result.MinimumAvailableSequence = uint64(minimum.Int64)
	} else {
		result.MinimumAvailableSequence = result.HighWatermark + 1
	}
	return result, nil
}

func (store *businessStore) listAgentEvents(ctx context.Context, projectID uuid.UUID, afterSequence, throughSequence uint64) ([]agentEventRecord, error) {
	if store == nil || projectID == uuid.Nil || afterSequence > maxSafeJSONInteger || throughSequence > maxSafeJSONInteger || afterSequence > throughSequence {
		return nil, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT project_id, sequence, event_id, topic, event_type, aggregate_type, aggregate_id, operation,
        revision, domain_cursor_kind, domain_cursor, safe_payload_json, causation_request_id, occurred_at_ms
        FROM agent_event_journal WHERE project_id = ? AND sequence > ? AND sequence <= ? ORDER BY sequence`,
		projectID.String(), afterSequence, throughSequence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]agentEventRecord, 0)
	for rows.Next() {
		value, err := scanAgentEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

type agentEventScanner interface {
	Scan(...any) error
}

func scanAgentEvent(scanner agentEventScanner) (agentEventRecord, error) {
	var projectID, eventID, safePayload, causation string
	var sequence, revision, cursorValue, occurredAt int64
	var event agentEventRecord
	if err := scanner.Scan(&projectID, &sequence, &eventID, &event.Topic, &event.EventType, &event.AggregateType, &event.AggregateID,
		&event.Operation, &revision, &event.Cursor.Kind, &cursorValue, &safePayload, &causation, &occurredAt); err != nil {
		return agentEventRecord{}, err
	}
	parsedProject, err := uuid.Parse(projectID)
	if err != nil || sequence < 1 || sequence > int64(maxSafeJSONInteger) || revision < 0 || revision > int64(maxSafeJSONInteger) ||
		cursorValue < 0 || cursorValue > int64(maxSafeJSONInteger) || uuid.Validate(eventID) != nil || uuid.Validate(event.AggregateID) != nil ||
		(causation != "" && uuid.Validate(causation) != nil) || occurredAt <= 0 || len(safePayload) == 0 || len(safePayload) > maximumAgentEventPayloadBytes || !json.Valid([]byte(safePayload)) {
		return agentEventRecord{}, errors.New("stored Agent event is invalid")
	}
	event.ProjectID = parsedProject
	event.Sequence = uint64(sequence)
	event.EventID = eventID
	event.Revision = uint64(revision)
	event.Cursor.Value = uint64(cursorValue)
	event.CausationRequestID = causation
	event.OccurredAt = time.UnixMilli(occurredAt).UTC()
	event.SafePayloadJSON = []byte(safePayload)
	if !validAgentEvent(event) {
		return agentEventRecord{}, errors.New("stored Agent event is invalid")
	}
	return event, nil
}
