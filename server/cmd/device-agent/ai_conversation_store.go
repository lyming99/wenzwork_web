package main

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumAIConversationTitleBytes        = 200
	maximumAIConversationPreviewCodePoints = 240
	maximumAIConversationPreviewBytes      = 768
	// Conversation changes are a compact catalog-reconciliation log. They are
	// deliberately separate from the per-conversation replay journal below:
	// retaining a bounded change log must never become a product limit on the
	// number of messages a user can keep in a conversation.
	maximumAIConversationChanges      = 4096
	maximumAIConversationPage         = 200
	maximumAIEventPage                = 500
	maximumAIPersistentEventBytes     = 4 << 20
	maximumAIPersistentEventPayload   = 8 << 10
	maximumAIPersistentDeltaBytes     = 8 << 10
	maximumAIPendingApprovalBytes     = 32 << 10
	maximumAIMessageContentChunkBytes = 16 << 10
	maximumAIMessageToolRunsBytes     = 1 << 20
	maximumAICompactedToolRunText     = 256
	maximumAIEventReplayAge           = 7 * 24 * time.Hour
)

type aiConversationEvent struct {
	EventID        string         `json:"eventId"`
	ConversationID string         `json:"conversationId"`
	GenerationID   string         `json:"generationId"`
	MessageID      string         `json:"messageId"`
	Kind           string         `json:"kind"`
	Sequence       uint64         `json:"sequence"`
	Payload        map[string]any `json:"payload"`
	OccurredAt     time.Time      `json:"occurredAt"`
}

// aiConversationEventPage is the durable replay contract.  HighWatermark is
// captured before a page is read and therefore always means the server-side
// waterline, while NextSequence is the sole continuation cursor.
//
// PageBytes and StopReason are intentionally not serialized. They are safe
// operational values used by the RPC layer for metrics without retaining
// conversational content.
type aiConversationEventPage struct {
	Items                     []aiConversationEvent `json:"items"`
	NextSequence              uint64                `json:"nextSequence"`
	HighWatermark             uint64                `json:"highWatermark"`
	HasMore                   bool                  `json:"hasMore"`
	ResetRequired             bool                  `json:"resetRequired"`
	EarliestAvailableSequence uint64                `json:"earliestAvailableSequence"`
	PageBytes                 int                   `json:"-"`
	StopReason                string                `json:"-"`
}

type aiConversationGenerationState struct {
	ConversationID    string
	GenerationID      string
	Status            string
	StartedAt         time.Time
	UpdatedAt         time.Time
	LastEventSequence uint64
	CanAcceptNewTurn  bool
	ErrorCode         string
}

type aiConversationMessageContentChunk struct {
	Content    string
	Offset     uint64
	NextOffset uint64
	TotalBytes uint64
	HasMore    bool
	Revision   uint64
}

func (page aiConversationEventPage) responsePayload() map[string]any {
	return map[string]any{
		"items":                     page.Items,
		"nextSequence":              page.NextSequence,
		"highWatermark":             page.HighWatermark,
		"hasMore":                   page.HasMore,
		"resetRequired":             page.ResetRequired,
		"earliestAvailableSequence": page.EarliestAvailableSequence,
	}
}

func (page *aiConversationEventPage) measureResponseBytes() (int, error) {
	encoded, err := json.Marshal(page.responsePayload())
	if err != nil {
		return 0, err
	}
	page.PageBytes = len(encoded)
	return len(encoded), nil
}

type aiContextSummary struct {
	ConversationID  string    `json:"conversationId"`
	ThroughSequence uint64    `json:"throughSequence"`
	Content         string    `json:"content"`
	EstimatedTokens uint64    `json:"estimatedTokens"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type aiConversationChange struct {
	Sequence uint64           `json:"sequence"`
	Deleted  bool             `json:"deleted"`
	Value    conversationView `json:"value"`
}

// aiConversationLatestMessage is intentionally a compact, user-visible
// projection. It never contains reasoning, tool values, attachments, or an
// authoritative message body.
type aiConversationLatestMessage struct {
	Sequence  uint64    `json:"sequence"`
	Role      string    `json:"role"`
	Status    string    `json:"status"`
	Preview   string    `json:"preview"`
	CreatedAt time.Time `json:"createdAt"`
}

// aiConversationListItem is the catalog wire model. Keep it separate from
// conversationView so a normal list response cannot accidentally grow to
// include model configuration or other detail-only fields.
type aiConversationListItem struct {
	ID                             string                       `json:"id"`
	Revision                       uint64                       `json:"revision"`
	Title                          string                       `json:"title"`
	MessageCount                   int                          `json:"messageCount"`
	Status                         string                       `json:"status"`
	GenerationID                   string                       `json:"generationId,omitempty"`
	LastMessageSequence            uint64                       `json:"lastMessageSequence"`
	LastCompletedAssistantSequence uint64                       `json:"lastCompletedAssistantSequence"`
	LatestMessage                  *aiConversationLatestMessage `json:"latestMessage,omitempty"`
	UpdatedAt                      time.Time                    `json:"updatedAt"`
	Subagent                       *aiSubagentDescriptor        `json:"subagent,omitempty"`
}

type aiConversationCatalogChange struct {
	Sequence       uint64                  `json:"sequence"`
	Operation      string                  `json:"operation"`
	ConversationID string                  `json:"conversationId"`
	Item           *aiConversationListItem `json:"item,omitempty"`
}

type aiConversationCatalogPage struct {
	Items                    []aiConversationListItem      `json:"items"`
	Changes                  []aiConversationCatalogChange `json:"changes"`
	NextCursor               *aiConversationCatalogCursor  `json:"-"`
	AckedThroughSequence     uint64                        `json:"ackedThroughSequence"`
	HighWatermark            uint64                        `json:"highWatermark"`
	MinimumAvailableSequence uint64                        `json:"minimumAvailableSequence"`
	HasMoreChanges           bool                          `json:"hasMoreChanges"`
	ResetRequired            bool                          `json:"resetRequired"`
}

// aiConversationCatalogCursor is opaque on the wire. Its snapshot revision
// pins deep keyset pages while newer updates arrive through the change feed.
type aiConversationCatalogCursor struct {
	Version          uint8  `json:"v"`
	SnapshotRevision uint64 `json:"r"`
	UpdatedAtMS      int64  `json:"u"`
	ID               string `json:"i"`
}

type aiConversationCatalogListOptions struct {
	ProjectID     uuid.UUID
	Cursor        *aiConversationCatalogCursor
	Limit         int
	AfterRevision *uint64
}

// aiConversationCatalogProjection stays on the internal full conversation
// model only so transactional writers can create a compact journal payload.
// It is never serialized as part of legacy conversationView responses.
type aiConversationCatalogProjection struct {
	LatestMessageSequence          uint64
	LatestMessageRole              string
	LatestMessageStatus            string
	LatestMessagePreview           string
	LatestMessageCreatedAt         time.Time
	LastCompletedAssistantSequence uint64
	LastErrorCode                  string
}

type aiConversationListResult struct {
	Items          []conversationView     `json:"items"`
	Changes        []aiConversationChange `json:"changes"`
	NextOffset     int                    `json:"-"`
	HighWatermark  uint64                 `json:"highWatermark"`
	LatestSequence uint64                 `json:"latestSequence"`
	ResetRequired  bool                   `json:"resetRequired"`
	HasMoreChanges bool                   `json:"hasMoreChanges"`
}

type aiConversationListOptions struct {
	ProjectID     uuid.UUID
	Offset        int
	Limit         int
	AfterRevision *uint64
}

type aiMessagePage struct {
	Items         []chatMessage `json:"items"`
	NextBefore    uint64        `json:"nextBeforeSequence,omitempty"`
	HighWatermark uint64        `json:"highWatermark"`
	ResetRequired bool          `json:"resetRequired"`
}

// aiConversationSnapshot is read from one SQLite transaction. Its event
// watermark describes exactly the message projection returned alongside it;
// clients can therefore attach from the watermark without skipping a delta
// committed between separate read calls.
type aiConversationSnapshot struct {
	Conversation                   conversationView
	Messages                       aiMessagePage
	ContextSummary                 *aiContextSummary
	EventHighWatermark             uint64
	EarliestAvailableEventSequence uint64
}

type aiConversationSearchResult struct {
	Conversation conversationView `json:"conversation"`
	Snippet      string           `json:"snippet"`
}

type aiConversationTurn struct {
	Conversation conversationView
	History      []chatMessage
	User         chatMessage
	Assistant    chatMessage
	GenerationID string
	Replayed     bool
	GoalRound    *aiGoalRoundSource
	// nil preserves compatibility for trusted local callers and old tests,
	// which derive intent from the dispatcher's Channel scope. Non-nil values
	// are admitted from the explicit request or durable inbox record.
	WorkspaceToolsEnabled *bool
}

func migrateAIConversationSchemaV7(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ai_conversations (
            id TEXT PRIMARY KEY,
            device_id TEXT NOT NULL,
            project_id TEXT NOT NULL REFERENCES projects(id),
            revision INTEGER NOT NULL,
            title TEXT NOT NULL,
            config_id TEXT NOT NULL,
            model_binding_json TEXT NOT NULL,
            workspace_mode TEXT NOT NULL DEFAULT 'readOnly',
            state TEXT NOT NULL DEFAULT 'idle',
            generation_id TEXT NOT NULL DEFAULT '',
            active_assistant_id TEXT NOT NULL DEFAULT '',
            collaboration_json TEXT NOT NULL DEFAULT '{}',
            last_message_sequence INTEGER NOT NULL DEFAULT 0,
            message_count INTEGER NOT NULL DEFAULT 0,
            created_at_ms INTEGER NOT NULL,
            updated_at_ms INTEGER NOT NULL,
            CHECK(length(id) BETWEEN 1 AND 80),
            CHECK(revision > 0),
            CHECK(length(title) BETWEEN 1 AND 200),
            CHECK(length(config_id) BETWEEN 1 AND 80),
            CHECK(json_valid(model_binding_json) AND json_type(model_binding_json) = 'object'),
            CHECK(workspace_mode IN ('readOnly','edit','fullAccess')),
            CHECK(state IN ('idle','generating','failed')),
            CHECK(last_message_sequence >= 0),
            CHECK(message_count >= 0)
        )`,
		`CREATE INDEX IF NOT EXISTS ai_conversations_project_updated_idx ON ai_conversations(device_id, project_id, updated_at_ms DESC, id)`,
		`CREATE TABLE IF NOT EXISTS ai_messages (
            id TEXT PRIMARY KEY,
            conversation_id TEXT NOT NULL REFERENCES ai_conversations(id) ON DELETE CASCADE,
            revision INTEGER NOT NULL,
            sequence INTEGER NOT NULL,
            role TEXT NOT NULL,
            content TEXT NOT NULL DEFAULT '',
            status TEXT NOT NULL,
            error_code TEXT NOT NULL DEFAULT '',
            attachments_json TEXT NOT NULL DEFAULT '[]',
            reasoning TEXT NOT NULL DEFAULT '',
            tool_runs_json TEXT NOT NULL DEFAULT '[]',
            usage_json TEXT NOT NULL DEFAULT '{}',
            provider_run_json TEXT NOT NULL DEFAULT '{}',
            generation_id TEXT NOT NULL DEFAULT '',
            created_at_ms INTEGER NOT NULL,
            updated_at_ms INTEGER NOT NULL,
            UNIQUE(conversation_id, sequence),
            CHECK(revision > 0),
            CHECK(sequence > 0),
            CHECK(role IN ('system','user','assistant','tool')),
            CHECK(status IN ('complete','streaming','stopped','failed')),
            CHECK(json_valid(attachments_json) AND json_type(attachments_json) = 'array'),
            CHECK(json_valid(tool_runs_json) AND json_type(tool_runs_json) = 'array'),
            CHECK(json_valid(usage_json) AND json_type(usage_json) = 'object'),
            CHECK(json_valid(provider_run_json) AND json_type(provider_run_json) = 'object')
        )`,
		`CREATE INDEX IF NOT EXISTS ai_messages_conversation_sequence_idx ON ai_messages(conversation_id, sequence DESC)`,
		`CREATE TABLE IF NOT EXISTS ai_conversation_events (
            conversation_id TEXT NOT NULL REFERENCES ai_conversations(id) ON DELETE CASCADE,
            sequence INTEGER NOT NULL,
            event_id TEXT NOT NULL UNIQUE,
            generation_id TEXT NOT NULL DEFAULT '',
            message_id TEXT NOT NULL DEFAULT '',
            kind TEXT NOT NULL,
            payload_json TEXT NOT NULL DEFAULT '{}',
            occurred_at_ms INTEGER NOT NULL,
            PRIMARY KEY(conversation_id, sequence),
            CHECK(sequence > 0),
            CHECK(kind IN ('chat.text.delta','chat.reasoning.delta','chat.tool.status','chat.approval.requested','chat.usage','chat.completed','chat.failed','chat.cancelled')),
            CHECK(json_valid(payload_json) AND json_type(payload_json) = 'object')
        )`,
		`CREATE INDEX IF NOT EXISTS ai_conversation_events_generation_idx ON ai_conversation_events(conversation_id, generation_id, sequence)`,
		`CREATE TABLE IF NOT EXISTS ai_conversation_changes (
            sequence INTEGER PRIMARY KEY AUTOINCREMENT,
            project_id TEXT NOT NULL,
            conversation_id TEXT NOT NULL,
            deleted INTEGER NOT NULL DEFAULT 0,
            payload_json TEXT NOT NULL DEFAULT '{}',
            occurred_at_ms INTEGER NOT NULL,
            CHECK(deleted IN (0,1)),
            CHECK(json_valid(payload_json) AND json_type(payload_json) = 'object')
        )`,
		`CREATE INDEX IF NOT EXISTS ai_conversation_changes_project_sequence_idx ON ai_conversation_changes(project_id, sequence)`,
		`CREATE TABLE IF NOT EXISTS ai_context_summaries (
            conversation_id TEXT PRIMARY KEY REFERENCES ai_conversations(id) ON DELETE CASCADE,
            through_sequence INTEGER NOT NULL,
            content TEXT NOT NULL,
            estimated_tokens INTEGER NOT NULL,
            updated_at_ms INTEGER NOT NULL,
            CHECK(through_sequence > 0),
            CHECK(length(content) BETWEEN 1 AND 32768),
            CHECK(estimated_tokens > 0)
        )`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create AI conversation schema: %w", err)
		}
	}
	return nil
}

func ensureAIConversationCatalogColumn(ctx context.Context, tx *sql.Tx, name, statement string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(ai_conversations)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var column, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if column == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return err
	}
	return nil
}

// migrateAIConversationSchemaV13 materializes the small catalog projection on
// the conversation row. The historical message read occurs only during this
// one-time migration; normal catalog reads never touch ai_messages.
func migrateAIConversationSchemaV13(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errRPCInvalid
	}
	columns := []struct {
		name      string
		statement string
	}{
		{"latest_message_sequence", `ALTER TABLE ai_conversations ADD COLUMN latest_message_sequence INTEGER NOT NULL DEFAULT 0`},
		{"latest_message_role", `ALTER TABLE ai_conversations ADD COLUMN latest_message_role TEXT NOT NULL DEFAULT ''`},
		{"latest_message_status", `ALTER TABLE ai_conversations ADD COLUMN latest_message_status TEXT NOT NULL DEFAULT ''`},
		{"latest_message_preview", `ALTER TABLE ai_conversations ADD COLUMN latest_message_preview TEXT NOT NULL DEFAULT ''`},
		{"latest_message_created_at_ms", `ALTER TABLE ai_conversations ADD COLUMN latest_message_created_at_ms INTEGER NOT NULL DEFAULT 0`},
		{"last_completed_assistant_sequence", `ALTER TABLE ai_conversations ADD COLUMN last_completed_assistant_sequence INTEGER NOT NULL DEFAULT 0`},
		{"last_error_code", `ALTER TABLE ai_conversations ADD COLUMN last_error_code TEXT NOT NULL DEFAULT ''`},
	}
	for _, column := range columns {
		if err := ensureAIConversationCatalogColumn(ctx, tx, column.name, column.statement); err != nil {
			return fmt.Errorf("add AI conversation catalog column %s: %w", column.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS ai_conversations_project_updated_idx`); err != nil {
		return fmt.Errorf("replace AI conversation catalog index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS ai_conversations_project_updated_idx
		ON ai_conversations(device_id, project_id, updated_at_ms DESC, id DESC)`); err != nil {
		return fmt.Errorf("create AI conversation catalog index: %w", err)
	}
	if err := backfillAIConversationCatalogProjection(ctx, tx); err != nil {
		return fmt.Errorf("backfill AI conversation catalog projection: %w", err)
	}
	return nil
}

func backfillAIConversationCatalogProjection(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM ai_conversations`)
	if err != nil {
		return err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		projection := aiConversationCatalogProjection{}
		var content string
		var createdAtMilliseconds int64
		err := tx.QueryRowContext(ctx, `SELECT sequence,role,status,content,created_at_ms,error_code
			FROM ai_messages
			WHERE conversation_id=? AND role IN ('user','assistant')
			ORDER BY sequence DESC LIMIT 1`, id).Scan(
			&projection.LatestMessageSequence, &projection.LatestMessageRole, &projection.LatestMessageStatus,
			&content, &createdAtMilliseconds, &projection.LastErrorCode,
		)
		if errors.Is(err, sql.ErrNoRows) {
			projection = aiConversationCatalogProjection{}
		} else if err != nil {
			return err
		} else {
			projection.LatestMessagePreview = normalizeAIConversationPreview(content)
			projection.LatestMessageCreatedAt = time.UnixMilli(createdAtMilliseconds).UTC()
			terminalStatus, terminalErrorCode := projection.LatestMessageStatus, projection.LastErrorCode
			if projection.LatestMessageRole != "assistant" || projection.LatestMessageStatus != "failed" {
				projection.LastErrorCode = ""
			}
			if projection.LatestMessageRole == "assistant" && projection.LatestMessagePreview == "" &&
				(projection.LatestMessageStatus == "failed" || projection.LatestMessageStatus == "stopped") {
				var userSequence uint64
				var userRole, userStatus, userContent string
				var userCreatedAtMilliseconds int64
				userErr := tx.QueryRowContext(ctx, `SELECT sequence,role,status,content,created_at_ms
					FROM ai_messages WHERE conversation_id=? AND role='user' AND sequence<?
					ORDER BY sequence DESC LIMIT 1`, id, projection.LatestMessageSequence).Scan(
					&userSequence, &userRole, &userStatus, &userContent, &userCreatedAtMilliseconds,
				)
				if errors.Is(userErr, sql.ErrNoRows) {
					// Keep the empty terminal assistant projection when there is no
					// prior user-visible message to preserve.
				} else if userErr != nil {
					return userErr
				} else {
					projection.LatestMessageSequence = userSequence
					projection.LatestMessageRole = userRole
					projection.LatestMessageStatus = userStatus
					projection.LatestMessagePreview = normalizeAIConversationPreview(userContent)
					projection.LatestMessageCreatedAt = time.UnixMilli(userCreatedAtMilliseconds).UTC()
					if terminalStatus == "failed" && terminalErrorCode != "" {
						projection.LastErrorCode = terminalErrorCode
					}
				}
			}
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM ai_messages
				WHERE conversation_id=? AND role='assistant' AND status='complete'`, id).Scan(&projection.LastCompletedAssistantSequence); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET
			latest_message_sequence=?, latest_message_role=?, latest_message_status=?, latest_message_preview=?,
			latest_message_created_at_ms=?, last_completed_assistant_sequence=?, last_error_code=? WHERE id=?`,
			projection.LatestMessageSequence, projection.LatestMessageRole, projection.LatestMessageStatus,
			projection.LatestMessagePreview, catalogProjectionCreatedAtMilliseconds(projection),
			projection.LastCompletedAssistantSequence, projection.LastErrorCode, id); err != nil {
			return err
		}
	}
	return nil
}

func normalizeAIConversationPreview(value string) string {
	normalized := strings.Join(strings.Fields(value), " ")
	if normalized == "" {
		return ""
	}
	if utf8.RuneCountInString(normalized) <= maximumAIConversationPreviewCodePoints && len(normalized) <= maximumAIConversationPreviewBytes {
		return normalized
	}
	const ellipsis = "…"
	var builder strings.Builder
	builder.Grow(min(len(normalized), maximumAIConversationPreviewBytes))
	codePoints, bytes := 0, 0
	for _, r := range normalized {
		runeBytes := utf8.RuneLen(r)
		if codePoints >= maximumAIConversationPreviewCodePoints-1 || bytes+runeBytes+len(ellipsis) > maximumAIConversationPreviewBytes {
			break
		}
		builder.WriteRune(r)
		codePoints++
		bytes += runeBytes
	}
	return builder.String() + ellipsis
}

const (
	aiWorkspaceModeReadOnly       = "readOnly"
	aiWorkspaceModeWorkspaceWrite = "workspaceWrite"
	aiWorkspaceModeFullAccess     = "fullAccess"
)

func validAIWorkspaceMode(value string) bool {
	return value == aiWorkspaceModeReadOnly || value == aiWorkspaceModeWorkspaceWrite || value == aiWorkspaceModeFullAccess
}

// The v7 SQLite constraint predates the public Workspace Write name and only
// accepts `edit`. Keep that physical representation until a table-rebuild
// migration is warranted, while exposing only the canonical v4 wire value.
func aiWorkspaceModeForStorage(value string) string {
	if value == aiWorkspaceModeWorkspaceWrite {
		return "edit"
	}
	return value
}

func aiWorkspaceModeFromStorage(value string) string {
	if value == "edit" {
		return aiWorkspaceModeWorkspaceWrite
	}
	return value
}

func validAIMessageStatus(value string) bool {
	return value == "complete" || value == "streaming" || value == "stopped" || value == "failed"
}

func validAIConversationEventKind(value string) bool {
	return slices.Contains([]string{
		"chat.text.delta", "chat.reasoning.delta", "chat.tool.status", "chat.approval.requested",
		"chat.usage", "chat.completed", "chat.failed", "chat.cancelled", "chat.plan_mode.changed",
		"chat.todo.updated", "chat.subagent.started", "chat.subagent.status", "chat.subagent.message",
		"chat.goal.changed",
	}, value)
}

func validateAIConversationView(value conversationView) error {
	if uuid.Validate(value.ID) != nil || uuid.Validate(value.ProjectID) != nil || value.Revision == 0 ||
		strings.TrimSpace(value.Title) == "" || len(value.Title) > maximumAIConversationTitleBytes ||
		!validAIConfigID(value.ConfigID) || !validAIWorkspaceMode(value.WorkspaceMode) ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) ||
		value.ModelBinding.ConfigID != value.ConfigID || value.ModelBinding.ConfigRevision == 0 ||
		!slices.Contains(supportedAIProviders, value.ModelBinding.Provider) || strings.TrimSpace(value.ModelBinding.Model) == "" || len(value.ModelBinding.Model) > 120 ||
		!validAIConversationCollaboration(collaborationFromConversation(value)) {
		return errRPCInvalid
	}
	return nil
}

func validAIConversationCatalogProjection(value aiConversationCatalogProjection, lastMessageSequence uint64) bool {
	if value.LatestMessageSequence == 0 {
		return value.LatestMessageRole == "" && value.LatestMessageStatus == "" &&
			value.LatestMessagePreview == "" && value.LatestMessageCreatedAt.IsZero() &&
			value.LastCompletedAssistantSequence == 0 && value.LastErrorCode == ""
	}
	if value.LatestMessageSequence > lastMessageSequence || value.LastCompletedAssistantSequence > lastMessageSequence ||
		!slices.Contains([]string{"user", "assistant"}, value.LatestMessageRole) ||
		!validAIMessageStatus(value.LatestMessageStatus) || value.LatestMessageCreatedAt.IsZero() ||
		!utf8.ValidString(value.LatestMessagePreview) || len(value.LatestMessagePreview) > maximumAIConversationPreviewBytes ||
		len(value.LastErrorCode) > 80 || !utf8.ValidString(value.LastErrorCode) {
		return false
	}
	return true
}

func aiConversationCatalogStatus(value conversationView) string {
	return aiConversationCatalogStatusFrom(value.State, value.Catalog.LatestMessageRole, value.Catalog.LatestMessageStatus)
}

func aiConversationCatalogStatusFrom(state, latestRole, latestStatus string) string {
	if state == "generating" {
		return "generating"
	}
	if state == "failed" || latestStatus == "failed" {
		return "error"
	}
	if latestRole == "assistant" && latestStatus == "complete" {
		return "completed"
	}
	return "normal"
}

func aiConversationListItemFromView(value conversationView) aiConversationListItem {
	result := aiConversationListItem{
		ID: value.ID, Revision: value.Revision, Title: value.Title, MessageCount: value.MessageCount,
		Status: aiConversationCatalogStatus(value), GenerationID: value.GenerationID,
		LastMessageSequence:            value.LastMessageSequence,
		LastCompletedAssistantSequence: value.Catalog.LastCompletedAssistantSequence,
		UpdatedAt:                      value.UpdatedAt,
		Subagent:                       value.Subagent,
	}
	if value.Catalog.LatestMessageSequence > 0 {
		result.LatestMessage = &aiConversationLatestMessage{
			Sequence: value.Catalog.LatestMessageSequence, Role: value.Catalog.LatestMessageRole,
			Status: value.Catalog.LatestMessageStatus, Preview: value.Catalog.LatestMessagePreview,
			CreatedAt: value.Catalog.LatestMessageCreatedAt,
		}
	}
	return result
}

func validAIConversationListItem(value aiConversationListItem) bool {
	if uuid.Validate(value.ID) != nil || value.Revision == 0 || strings.TrimSpace(value.Title) == "" ||
		value.MessageCount < 0 || value.UpdatedAt.IsZero() || value.LastCompletedAssistantSequence > value.LastMessageSequence ||
		!slices.Contains([]string{"normal", "generating", "error", "completed"}, value.Status) {
		return false
	}
	if value.GenerationID != "" && uuid.Validate(value.GenerationID) != nil {
		return false
	}
	if value.Subagent != nil && !validAIConversationCollaboration(aiConversationCollaboration{Subagent: value.Subagent}) {
		return false
	}
	if value.LatestMessage == nil {
		return value.LastMessageSequence == 0 && value.LastCompletedAssistantSequence == 0
	}
	message := value.LatestMessage
	return message.Sequence > 0 && message.Sequence <= value.LastMessageSequence &&
		slices.Contains([]string{"user", "assistant"}, message.Role) && validAIMessageStatus(message.Status) &&
		!message.CreatedAt.IsZero() && utf8.ValidString(message.Preview) && len(message.Preview) <= maximumAIConversationPreviewBytes
}

func catalogProjectionCreatedAtMilliseconds(value aiConversationCatalogProjection) int64 {
	if value.LatestMessageCreatedAt.IsZero() {
		return 0
	}
	return value.LatestMessageCreatedAt.UTC().UnixMilli()
}

func withAIConversationCatalogMessage(
	projection aiConversationCatalogProjection,
	message chatMessage,
) aiConversationCatalogProjection {
	preview := normalizeAIConversationPreview(message.Content)
	// A failed/stopped placeholder can have no user-visible body. Keep the
	// prompt preview in that case instead of replacing useful list context with
	// an empty assistant row; conversation status and last_error_code still
	// describe the terminal outcome.
	if message.Role == "assistant" && preview == "" &&
		(message.Status == "failed" || message.Status == "stopped") && projection.LatestMessageRole == "user" {
		if message.Status == "failed" {
			projection.LastErrorCode = message.ErrorCode
		} else {
			projection.LastErrorCode = ""
		}
		return projection
	}
	projection.LatestMessageSequence = message.Sequence
	projection.LatestMessageRole = message.Role
	projection.LatestMessageStatus = message.Status
	projection.LatestMessagePreview = preview
	projection.LatestMessageCreatedAt = message.CreatedAt.UTC()
	if message.Role == "assistant" && message.Status == "complete" {
		projection.LastCompletedAssistantSequence = message.Sequence
	}
	if message.Role == "assistant" && message.Status == "failed" {
		projection.LastErrorCode = message.ErrorCode
	} else {
		projection.LastErrorCode = ""
	}
	return projection
}

func validateStoredAIMessage(value chatMessage) error {
	if uuid.Validate(value.ID) != nil || value.Revision == 0 || value.Sequence == 0 || !validMessageRole(value.Role) ||
		!validAIMessageStatus(value.Status) || !utf8.ValidString(value.Content) || len(value.Content) > maximumAssistantBytes ||
		!utf8.ValidString(value.Reasoning) || len(value.Reasoning) > maximumAssistantBytes || len(value.ErrorCode) > 80 ||
		len(value.Attachments) > 8 || len(value.ToolRuns) > 200 || value.CreatedAt.IsZero() {
		return errRPCInvalid
	}
	return nil
}

func marshalAIJSON(value any, maximum int) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maximum {
		return "", errRPCInvalid
	}
	return string(encoded), nil
}

func aiConversationBinding(config aiConfig) aiModelBinding {
	return aiModelBinding{ConfigID: config.ID, ConfigRevision: config.Revision, Provider: config.Provider, Model: config.Model}
}

const aiConversationSelect = `SELECT id, project_id, revision, title, config_id, model_binding_json,
    workspace_mode, last_message_sequence, message_count, created_at_ms, updated_at_ms, state, generation_id,
    latest_message_sequence, latest_message_role, latest_message_status, latest_message_preview,
    latest_message_created_at_ms, last_completed_assistant_sequence, last_error_code, collaboration_json
    FROM ai_conversations`

type aiSQLScanner interface {
	Scan(...any) error
}

func scanAIConversation(scanner aiSQLScanner) (conversationView, error) {
	var value conversationView
	var bindingJSON, collaborationJSON, state, generationID string
	var createdAtMilliseconds, updatedAtMilliseconds, latestMessageCreatedAtMilliseconds int64
	err := scanner.Scan(
		&value.ID, &value.ProjectID, &value.Revision, &value.Title, &value.ConfigID, &bindingJSON,
		&value.WorkspaceMode, &value.LastMessageSequence, &value.MessageCount, &createdAtMilliseconds,
		&updatedAtMilliseconds, &state, &generationID, &value.Catalog.LatestMessageSequence,
		&value.Catalog.LatestMessageRole, &value.Catalog.LatestMessageStatus, &value.Catalog.LatestMessagePreview,
		&latestMessageCreatedAtMilliseconds, &value.Catalog.LastCompletedAssistantSequence, &value.Catalog.LastErrorCode,
		&collaborationJSON,
	)
	if err != nil {
		return conversationView{}, err
	}
	var collaboration aiConversationCollaboration
	if json.Unmarshal([]byte(bindingJSON), &value.ModelBinding) != nil ||
		json.Unmarshal([]byte(collaborationJSON), &collaboration) != nil ||
		!validAIConversationCollaboration(collaboration) || createdAtMilliseconds <= 0 || updatedAtMilliseconds <= 0 {
		return conversationView{}, errors.New("stored AI conversation is invalid")
	}
	if collaboration.Todos == nil {
		collaboration.Todos = []aiTodoItem{}
	}
	value.PlanModeActive, value.Todos, value.Subagent, value.Goal = collaboration.PlanModeActive, collaboration.Todos, collaboration.Subagent, collaboration.Goal
	value.WorkspaceMode = aiWorkspaceModeFromStorage(value.WorkspaceMode)
	value.CreatedAt = time.UnixMilli(createdAtMilliseconds).UTC()
	value.UpdatedAt = time.UnixMilli(updatedAtMilliseconds).UTC()
	if latestMessageCreatedAtMilliseconds > 0 {
		value.Catalog.LatestMessageCreatedAt = time.UnixMilli(latestMessageCreatedAtMilliseconds).UTC()
	}
	value.State, value.GenerationID, value.Model = state, generationID, value.ModelBinding.Model
	if validateAIConversationView(value) != nil || !slices.Contains([]string{"idle", "generating", "failed"}, state) ||
		generationID != "" && uuid.Validate(generationID) != nil || !validAIConversationCatalogProjection(value.Catalog, value.LastMessageSequence) {
		return conversationView{}, errors.New("stored AI conversation is invalid")
	}
	return value, nil
}

const aiConversationCatalogSelect = `SELECT id, revision, title, message_count, state, generation_id,
    last_message_sequence, last_completed_assistant_sequence, latest_message_sequence,
    latest_message_role, latest_message_status, latest_message_preview, latest_message_created_at_ms,
    updated_at_ms, collaboration_json FROM ai_conversations`

func scanAIConversationListItem(scanner aiSQLScanner) (aiConversationListItem, error) {
	var value aiConversationListItem
	var state, latestRole, latestStatus string
	var latestSequence uint64
	var latestPreview, collaborationJSON string
	var latestCreatedAtMilliseconds, updatedAtMilliseconds int64
	if err := scanner.Scan(
		&value.ID, &value.Revision, &value.Title, &value.MessageCount, &state, &value.GenerationID,
		&value.LastMessageSequence, &value.LastCompletedAssistantSequence, &latestSequence,
		&latestRole, &latestStatus, &latestPreview, &latestCreatedAtMilliseconds, &updatedAtMilliseconds, &collaborationJSON,
	); err != nil {
		return aiConversationListItem{}, err
	}
	if updatedAtMilliseconds <= 0 {
		return aiConversationListItem{}, errors.New("stored AI conversation catalog item is invalid")
	}
	value.UpdatedAt = time.UnixMilli(updatedAtMilliseconds).UTC()
	var collaboration aiConversationCollaboration
	if json.Unmarshal([]byte(collaborationJSON), &collaboration) != nil || !validAIConversationCollaboration(collaboration) {
		return aiConversationListItem{}, errors.New("stored AI conversation catalog item is invalid")
	}
	value.Subagent = collaboration.Subagent
	value.Status = aiConversationCatalogStatusFrom(state, latestRole, latestStatus)
	if latestSequence > 0 {
		if latestCreatedAtMilliseconds <= 0 {
			return aiConversationListItem{}, errors.New("stored AI conversation catalog item is invalid")
		}
		value.LatestMessage = &aiConversationLatestMessage{
			Sequence: latestSequence, Role: latestRole, Status: latestStatus, Preview: latestPreview,
			CreatedAt: time.UnixMilli(latestCreatedAtMilliseconds).UTC(),
		}
	}
	if !validAIConversationListItem(value) {
		return aiConversationListItem{}, errors.New("stored AI conversation catalog item is invalid")
	}
	return value, nil
}

func scanAIMessage(scanner aiSQLScanner) (chatMessage, error) {
	var value chatMessage
	var attachmentsJSON, toolRunsJSON, usageJSON, providerRunJSON string
	var createdAtMilliseconds int64
	err := scanner.Scan(
		&value.ID, &value.Revision, &value.Sequence, &value.Role, &value.Content, &value.Status,
		&value.ErrorCode, &attachmentsJSON, &value.Reasoning, &toolRunsJSON, &usageJSON,
		&providerRunJSON, &value.GenerationID, &createdAtMilliseconds,
	)
	if err != nil {
		return chatMessage{}, err
	}
	if json.Unmarshal([]byte(attachmentsJSON), &value.Attachments) != nil ||
		json.Unmarshal([]byte(toolRunsJSON), &value.ToolRuns) != nil ||
		json.Unmarshal([]byte(usageJSON), &value.Usage) != nil ||
		json.Unmarshal([]byte(providerRunJSON), &value.ProviderRun) != nil || createdAtMilliseconds <= 0 {
		return chatMessage{}, errors.New("stored AI message is invalid")
	}
	if value.Attachments == nil {
		value.Attachments = []chatAttachmentReference{}
	}
	if value.ToolRuns == nil {
		value.ToolRuns = []chatToolRun{}
	}
	for index := range value.ToolRuns {
		if value.ToolRuns[index].Name == "" {
			value.ToolRuns[index].Name = value.ToolRuns[index].Tool
		}
		if value.ToolRuns[index].Tool == "" {
			value.ToolRuns[index].Tool = value.ToolRuns[index].Name
		}
	}
	value.CreatedAt = time.UnixMilli(createdAtMilliseconds).UTC()
	if validateStoredAIMessage(value) != nil || value.GenerationID != "" && uuid.Validate(value.GenerationID) != nil {
		return chatMessage{}, errors.New("stored AI message is invalid")
	}
	return value, nil
}

func queryAIConversation(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, deviceID, projectID, conversationID string) (conversationView, bool, error) {
	value, err := scanAIConversation(queryer.QueryRowContext(ctx, aiConversationSelect+
		` WHERE device_id = ? AND project_id = ? AND id = ?`, deviceID, projectID, conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return conversationView{}, false, nil
	}
	if err != nil {
		return conversationView{}, false, err
	}
	return value, true, nil
}

func appendAIConversationChange(ctx context.Context, store *businessStore, tx *sql.Tx, value conversationView, deleted bool, now time.Time) (conversationView, error) {
	// Reserve the journal sequence first, then persist the compact list item with
	// that sequence as its revision. Keeping the journal projection small avoids
	// retaining model bindings or other detail-only data for every catalog event.
	payload, err := marshalAIJSON(map[string]any{}, 4<<10)
	if err != nil {
		return conversationView{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO ai_conversation_changes(project_id, conversation_id, deleted, payload_json, occurred_at_ms)
        VALUES(?,?,?,?,?)`, value.ProjectID, value.ID, aiBoolInt(deleted), payload, now.UTC().UnixMilli())
	if err != nil {
		return conversationView{}, fmt.Errorf("append AI conversation change: %w", err)
	}
	sequence, err := result.LastInsertId()
	if err != nil || sequence <= 0 || uint64(sequence) > maxSafeJSONInteger {
		return conversationView{}, errors.New("AI conversation revision is exhausted")
	}
	value.Revision = uint64(sequence)
	if deleted {
		payload, err = marshalAIJSON(struct {
			ID string `json:"id"`
		}{ID: value.ID}, 4<<10)
	} else {
		payload, err = marshalAIJSON(aiConversationListItemFromView(value), 4<<10)
	}
	if err != nil {
		return conversationView{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_conversation_changes SET payload_json=? WHERE sequence=?`, payload, sequence); err != nil {
		return conversationView{}, fmt.Errorf("finalize AI conversation change: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_conversation_changes WHERE project_id=? AND sequence < COALESCE((
        SELECT sequence FROM ai_conversation_changes WHERE project_id=? ORDER BY sequence DESC LIMIT 1 OFFSET ?
	    ),0)`, value.ProjectID, value.ProjectID, maximumAIConversationChanges-1); err != nil {
		return conversationView{}, fmt.Errorf("prune AI conversation changes: %w", err)
	}
	if _, err := store.appendAgentEvent(ctx, tx, newConversationChangedAgentEvent(value, deleted, uint64(sequence), now)); err != nil {
		return conversationView{}, err
	}
	return value, nil
}

func appendAIConversationEvent(ctx context.Context, store *businessStore, tx *sql.Tx, event aiConversationEvent) (aiConversationEvent, error) {
	if uuid.Validate(event.ConversationID) != nil || uuid.Validate(event.EventID) != nil || !validAIConversationEventKind(event.Kind) ||
		event.GenerationID != "" && uuid.Validate(event.GenerationID) != nil || event.MessageID != "" && uuid.Validate(event.MessageID) != nil || event.OccurredAt.IsZero() {
		return aiConversationEvent{}, errRPCInvalid
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	payload, err := marshalAIJSON(event.Payload, 64<<10)
	if err != nil {
		return aiConversationEvent{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM ai_conversation_events WHERE conversation_id=?`, event.ConversationID).Scan(&event.Sequence); err != nil || event.Sequence == 0 || event.Sequence > maxSafeJSONInteger {
		return aiConversationEvent{}, firstError(err, errors.New("AI event sequence is exhausted"))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_conversation_events(
        conversation_id,sequence,event_id,generation_id,message_id,kind,payload_json,occurred_at_ms
    ) VALUES(?,?,?,?,?,?,?,?)`, event.ConversationID, event.Sequence, event.EventID, event.GenerationID,
		event.MessageID, event.Kind, payload, event.OccurredAt.UTC().UnixMilli()); err != nil {
		return aiConversationEvent{}, fmt.Errorf("append AI conversation event: %w", err)
	}
	if err := pruneAIConversationEvents(ctx, tx, event.ConversationID, event.OccurredAt); err != nil {
		return aiConversationEvent{}, fmt.Errorf("prune AI conversation events: %w", err)
	}
	// Conversation deltas stay in their own authorised journal. Emit only a
	// compact availability hint at a bounded cadence, while terminal events are
	// immediate so another client promptly reconciles the completed state.
	var projectRaw string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM ai_conversations WHERE id = ?`, event.ConversationID).Scan(&projectRaw); err != nil {
		return aiConversationEvent{}, err
	}
	projectID, err := uuid.Parse(projectRaw)
	if err != nil || projectID == uuid.Nil {
		return aiConversationEvent{}, errRPCInvalid
	}
	emitHint, err := shouldAppendAIConversationEventHint(ctx, tx, projectID, event)
	if err != nil {
		return aiConversationEvent{}, err
	}
	if emitHint {
		if _, err := store.appendAgentEvent(ctx, tx, newConversationEventsAvailableAgentEvent(projectID, event)); err != nil {
			return aiConversationEvent{}, err
		}
	}
	return event, nil
}

// pruneAIConversationEvents bounds only the replay cache. Messages, terminal
// state and tool records remain authoritative in their own tables, so a
// trimmed replay cursor always has a complete snapshot recovery path instead
// of silently losing chat history.  Keeping the window by both age and bytes
// prevents a high-frequency token stream from turning the count of events into
// an accidental product limit.
func pruneAIConversationEvents(ctx context.Context, tx *sql.Tx, conversationID string, now time.Time) error {
	if tx == nil || uuid.Validate(conversationID) != nil || now.IsZero() {
		return errRPCInvalid
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_conversation_events
		WHERE conversation_id=? AND occurred_at_ms<?`, conversationID, now.UTC().Add(-maximumAIEventReplayAge).UnixMilli()); err != nil {
		return err
	}
	for {
		var total int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(payload_json)+192),0)
			FROM ai_conversation_events WHERE conversation_id=?`, conversationID).Scan(&total); err != nil {
			return err
		}
		if total <= maximumAIPersistentEventBytes {
			return nil
		}
		var sequence uint64
		err := tx.QueryRowContext(ctx, `SELECT sequence FROM ai_conversation_events
			WHERE conversation_id=? ORDER BY sequence LIMIT 1`, conversationID).Scan(&sequence)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ai_conversation_events
			WHERE conversation_id=? AND sequence=?`, conversationID, sequence); err != nil {
			return err
		}
	}
}

func shouldAppendAIConversationEventHint(ctx context.Context, tx *sql.Tx, projectID uuid.UUID, event aiConversationEvent) (bool, error) {
	if tx == nil || projectID == uuid.Nil {
		return false, errRPCInvalid
	}
	if event.Kind == "chat.completed" || event.Kind == "chat.failed" || event.Kind == "chat.cancelled" {
		return true, nil
	}
	var lastHintAt int64
	err := tx.QueryRowContext(ctx, `SELECT occurred_at_ms FROM agent_event_journal
		WHERE project_id = ? AND event_type = 'conversation.events.available' AND aggregate_id = ?
		ORDER BY sequence DESC LIMIT 1`, projectID.String(), event.ConversationID).Scan(&lastHintAt)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil || lastHintAt < 1 {
		return false, firstError(err, errRPCInvalid)
	}
	return event.OccurredAt.UTC().UnixMilli()-lastHintAt >= agentEventConversationHintInterval.Milliseconds(), nil
}

func insertAIMessage(ctx context.Context, tx *sql.Tx, conversationID string, value chatMessage, now time.Time) error {
	if uuid.Validate(conversationID) != nil || validateStoredAIMessage(value) != nil {
		return errRPCInvalid
	}
	if value.Attachments == nil {
		value.Attachments = []chatAttachmentReference{}
	}
	if value.ToolRuns == nil {
		value.ToolRuns = []chatToolRun{}
	}
	attachments, err := marshalAIJSON(value.Attachments, 64<<10)
	if err != nil {
		return err
	}
	toolRuns, err := marshalAIJSON(value.ToolRuns, 256<<10)
	if err != nil {
		return err
	}
	usage, err := marshalAIJSON(value.Usage, 16<<10)
	if err != nil {
		return err
	}
	providerRun, err := marshalAIJSON(value.ProviderRun, 16<<10)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ai_messages(
        id,conversation_id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms,updated_at_ms
    ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, conversationID, value.Revision, value.Sequence,
		value.Role, value.Content, value.Status, value.ErrorCode, attachments, value.Reasoning, toolRuns, usage,
		providerRun, value.GenerationID, value.CreatedAt.UTC().UnixMilli(), now.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("insert AI message: %w", err)
	}
	return nil
}

func (store *businessStore) createAIConversation(ctx context.Context, projectID uuid.UUID, id, title, workspaceMode string, config aiConfig, now time.Time) (conversationView, error) {
	if store == nil || projectID == uuid.Nil || !validAIConfigID(config.ID) || config.Revision == 0 ||
		!validAIWorkspaceMode(workspaceMode) || now.IsZero() {
		return conversationView{}, errRPCInvalid
	}
	if id == "" {
		id = uuid.NewString()
	}
	if uuid.Validate(id) != nil || strings.TrimSpace(title) == "" || len(title) > maximumAIConversationTitleBytes || !utf8.ValidString(title) {
		return conversationView{}, errRPCInvalid
	}
	binding := aiConversationBinding(config)
	bindingJSON, err := marshalAIJSON(binding, 16<<10)
	if err != nil {
		return conversationView{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return conversationView{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return conversationView{}, err
	}
	defer tx.Rollback()
	var projectExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=? AND device_id=? AND state='available'`, projectID.String(), store.deviceID.String()).Scan(&projectExists); err != nil || projectExists != 1 {
		return conversationView{}, firstError(err, errRPCProject)
	}
	var idExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_conversations WHERE id=?`, id).Scan(&idExists); err != nil {
		return conversationView{}, err
	} else if idExists != 0 {
		return conversationView{}, errRPCRevision
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_conversations(
        id,device_id,project_id,revision,title,config_id,model_binding_json,workspace_mode,state,
		generation_id,active_assistant_id,collaboration_json,last_message_sequence,message_count,created_at_ms,updated_at_ms
    ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, store.deviceID.String(), projectID.String(), 1,
		strings.TrimSpace(title), config.ID, bindingJSON, aiWorkspaceModeForStorage(workspaceMode), "idle", "", "", "{}", 0, 0,
		now.UTC().UnixMilli(), now.UTC().UnixMilli()); err != nil {
		return conversationView{}, fmt.Errorf("create AI conversation: %w", err)
	}
	value := conversationView{
		ID: id, ProjectID: projectID.String(), Revision: 1, Title: strings.TrimSpace(title), ConfigID: config.ID,
		ModelBinding: binding, Model: binding.Model, WorkspaceMode: workspaceMode, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), State: "idle", Todos: []aiTodoItem{},
	}
	value, err = appendAIConversationChange(ctx, store, tx, value, false, now)
	if err != nil {
		return conversationView{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET revision=? WHERE id=?`, value.Revision, id); err != nil {
		return conversationView{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return conversationView{}, fmt.Errorf("commit AI conversation create: %w", err)
	}
	return value, nil
}

// forkAIConversation copies a settled transcript boundary into a child. The
// legacy branch mode excludes one user message so clients can edit it; the
// inclusive mode retains the selected message and is used by conversation
// templates that resume immediately after a saved position.
func (store *businessStore) forkAIConversation(
	ctx context.Context,
	projectID uuid.UUID,
	sourceConversationID, childConversationID, messageID string,
	messageSequence, expectedRevision uint64,
	includeBoundary bool,
	now time.Time,
) (conversationView, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(sourceConversationID) != nil ||
		uuid.Validate(childConversationID) != nil || uuid.Validate(messageID) != nil ||
		messageSequence == 0 || expectedRevision == 0 || now.IsZero() {
		return conversationView{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return conversationView{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return conversationView{}, err
	}
	defer tx.Rollback()

	source, found, err := queryAIConversation(
		ctx,
		tx,
		store.deviceID.String(),
		projectID.String(),
		sourceConversationID,
	)
	if err != nil {
		return conversationView{}, err
	}
	if !found {
		return conversationView{}, errRPCNotFound
	}
	if source.Revision != expectedRevision {
		return conversationView{}, errRPCRevision
	}
	selected, err := scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms
        FROM ai_messages WHERE conversation_id=? AND id=? AND sequence=?`, sourceConversationID, messageID, messageSequence))
	if errors.Is(err, sql.ErrNoRows) {
		return conversationView{}, errRPCNotFound
	}
	if err != nil {
		return conversationView{}, err
	}
	if selected.Status != "complete" || !includeBoundary && selected.Role != "user" {
		return conversationView{}, errRPCInvalid
	}
	var childExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_conversations WHERE id=?`, childConversationID).Scan(&childExists); err != nil {
		return conversationView{}, err
	}
	if childExists != 0 {
		return conversationView{}, errRPCRevision
	}

	comparison := "<"
	if includeBoundary {
		comparison = "<="
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms
		FROM ai_messages WHERE conversation_id=? AND sequence`+comparison+`? ORDER BY sequence`, sourceConversationID, messageSequence)
	if err != nil {
		return conversationView{}, err
	}
	prefix := make([]chatMessage, 0)
	for rows.Next() {
		message, scanErr := scanAIMessage(rows)
		if scanErr != nil {
			_ = rows.Close()
			return conversationView{}, scanErr
		}
		if message.Status == "streaming" {
			_ = rows.Close()
			return conversationView{}, errRPCRevision
		}
		prefix = append(prefix, message)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return conversationView{}, err
	}
	if err := rows.Close(); err != nil {
		return conversationView{}, err
	}

	projection := aiConversationCatalogProjection{}
	lastSequence := uint64(0)
	for _, message := range prefix {
		projection = withAIConversationCatalogMessage(projection, message)
		lastSequence = message.Sequence
	}
	bindingJSON, err := marshalAIJSON(source.ModelBinding, 16<<10)
	if err != nil {
		return conversationView{}, err
	}
	title := aiConversationForkTitle(source.Title)
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_conversations(
        id,device_id,project_id,revision,title,config_id,model_binding_json,workspace_mode,state,
        generation_id,active_assistant_id,collaboration_json,last_message_sequence,message_count,
        created_at_ms,updated_at_ms,latest_message_sequence,latest_message_role,latest_message_status,
        latest_message_preview,latest_message_created_at_ms,last_completed_assistant_sequence,last_error_code
    ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		childConversationID, store.deviceID.String(), projectID.String(), 1, title, source.ConfigID,
		bindingJSON, aiWorkspaceModeForStorage(source.WorkspaceMode), "idle", "", "", "{}",
		lastSequence, len(prefix), now.UTC().UnixMilli(), now.UTC().UnixMilli(),
		projection.LatestMessageSequence, projection.LatestMessageRole, projection.LatestMessageStatus,
		projection.LatestMessagePreview, catalogProjectionCreatedAtMilliseconds(projection),
		projection.LastCompletedAssistantSequence, projection.LastErrorCode,
	); err != nil {
		return conversationView{}, fmt.Errorf("create AI conversation fork: %w", err)
	}
	for _, original := range prefix {
		copy := original
		copy.ID = uuid.NewString()
		copy.Revision = 1
		copy.GenerationID = ""
		if err := insertAIMessage(ctx, tx, childConversationID, copy, now); err != nil {
			return conversationView{}, err
		}
	}
	summaryComparison := "<"
	if includeBoundary {
		summaryComparison = "<="
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_context_summaries(
        conversation_id,through_sequence,content,estimated_tokens,updated_at_ms
    ) SELECT ?,through_sequence,content,estimated_tokens,? FROM ai_context_summaries
		WHERE conversation_id=? AND through_sequence`+summaryComparison+`?`, childConversationID, now.UTC().UnixMilli(), sourceConversationID, messageSequence); err != nil {
		return conversationView{}, err
	}
	value := conversationView{
		ID: childConversationID, ProjectID: projectID.String(), Revision: 1, Title: title,
		ConfigID: source.ConfigID, ModelBinding: source.ModelBinding, Model: source.ModelBinding.Model,
		WorkspaceMode: source.WorkspaceMode, LastMessageSequence: lastSequence, MessageCount: len(prefix),
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(), State: "idle", Todos: []aiTodoItem{}, Catalog: projection,
	}
	value, err = appendAIConversationChange(ctx, store, tx, value, false, now)
	if err != nil {
		return conversationView{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET revision=? WHERE id=?`, value.Revision, childConversationID); err != nil {
		return conversationView{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return conversationView{}, fmt.Errorf("commit AI conversation fork: %w", err)
	}
	return value, nil
}

func aiConversationForkTitle(source string) string {
	const suffix = "（分支）"
	base := strings.TrimSpace(source)
	if base == "" {
		base = "新对话"
	}
	base = strings.TrimSpace(truncateAIUTF8(base, maximumAIConversationTitleBytes-len(suffix)))
	return base + suffix
}

func (store *businessStore) getAIConversation(ctx context.Context, projectID uuid.UUID, conversationID string) (conversationView, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil {
		return conversationView{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return conversationView{}, err
	}
	defer db.Close()
	value, found, err := queryAIConversation(ctx, db, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return conversationView{}, err
	}
	if !found {
		return conversationView{}, errRPCNotFound
	}
	return value, nil
}

func (store *businessStore) listAIConversations(ctx context.Context, options aiConversationListOptions) (aiConversationListResult, error) {
	if store == nil || options.ProjectID == uuid.Nil || options.Offset < 0 || options.Limit < 1 || options.Limit > maximumAIConversationPage ||
		options.AfterRevision != nil && options.Offset != 0 {
		return aiConversationListResult{}, errRPCInvalid
	}
	// This is a snapshot read. Do not queue the conversation list behind a
	// streaming-generation write lock; a short read-only SQLite handle gives
	// the mobile list a bounded latency while preserving transactional writes.
	db, err := store.openReadDB()
	if err != nil {
		return aiConversationListResult{}, err
	}
	defer db.Close()
	projectID := options.ProjectID.String()
	resetRequired := false
	var minimum, latest uint64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MIN(sequence),0),COALESCE(MAX(sequence),0) FROM ai_conversation_changes WHERE project_id=?`, projectID).Scan(&minimum, &latest); err != nil {
		return aiConversationListResult{}, err
	}
	if options.AfterRevision != nil {
		after := *options.AfterRevision
		if after > latest {
			return aiConversationListResult{}, errRPCInvalid
		}
		if minimum > 0 && after+1 < minimum {
			options.AfterRevision = nil
			options.Offset = 0
			resetRequired = true
		} else {
			rows, err := db.QueryContext(ctx, `SELECT sequence,deleted,payload_json FROM ai_conversation_changes
                WHERE project_id=? AND sequence>? ORDER BY sequence LIMIT ?`, projectID, after, options.Limit+1)
			if err != nil {
				return aiConversationListResult{}, err
			}
			changes := make([]aiConversationChange, 0, options.Limit)
			for rows.Next() {
				var change aiConversationChange
				var deleted int
				var payload string
				var compact struct {
					ID string `json:"id"`
				}
				if err := rows.Scan(&change.Sequence, &deleted, &payload); err != nil || json.Unmarshal([]byte(payload), &compact) != nil || uuid.Validate(compact.ID) != nil {
					return aiConversationListResult{}, errors.New("stored AI conversation change is invalid")
				}
				change.Deleted = deleted == 1
				change.Value = conversationView{ID: compact.ID, ProjectID: projectID, Revision: change.Sequence}
				changes = append(changes, change)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return aiConversationListResult{}, err
			}
			if err := rows.Close(); err != nil {
				return aiConversationListResult{}, err
			}
			hasMore := len(changes) > options.Limit
			if hasMore {
				changes = changes[:options.Limit]
			}
			// The persisted journal is compact from V13 onward. Preserve the
			// legacy response shape by hydrating the current full view for a
			// surviving upsert; V2 callers below remain on the compact path.
			for index := range changes {
				if changes[index].Deleted {
					continue
				}
				loaded, found, loadErr := queryAIConversation(ctx, db, store.deviceID.String(), projectID, changes[index].Value.ID)
				if loadErr != nil {
					return aiConversationListResult{}, loadErr
				}
				if found {
					changes[index].Value = loaded
				}
			}
			watermark := latest
			if hasMore {
				watermark = changes[len(changes)-1].Sequence
			}
			return aiConversationListResult{Items: []conversationView{}, Changes: changes, HighWatermark: watermark, LatestSequence: latest, HasMoreChanges: hasMore}, nil
		}
	}
	rows, err := db.QueryContext(ctx, aiConversationSelect+` WHERE device_id=? AND project_id=?
        ORDER BY updated_at_ms DESC,id LIMIT ? OFFSET ?`, store.deviceID.String(), projectID, options.Limit+1, options.Offset)
	if err != nil {
		return aiConversationListResult{}, err
	}
	defer rows.Close()
	items := make([]conversationView, 0, options.Limit)
	for rows.Next() {
		value, err := scanAIConversation(rows)
		if err != nil {
			return aiConversationListResult{}, err
		}
		items = append(items, value)
	}
	if err := rows.Err(); err != nil {
		return aiConversationListResult{}, err
	}
	nextOffset := 0
	if len(items) > options.Limit {
		items = items[:options.Limit]
		nextOffset = options.Offset + options.Limit
	}
	return aiConversationListResult{Items: items, Changes: []aiConversationChange{}, NextOffset: nextOffset, HighWatermark: latest, LatestSequence: latest, ResetRequired: resetRequired}, nil
}

// listAIConversationCatalog is the V2 catalog path. It reads only materialized
// summary columns, uses a snapshot-bound keyset cursor, and never joins or
// scans ai_messages during a normal list request.
func (store *businessStore) listAIConversationCatalog(ctx context.Context, options aiConversationCatalogListOptions) (aiConversationCatalogPage, error) {
	if store == nil || options.ProjectID == uuid.Nil || options.Limit < 1 || options.Limit > maximumAIConversationPage ||
		options.Cursor != nil && options.AfterRevision != nil {
		return aiConversationCatalogPage{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return aiConversationCatalogPage{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return aiConversationCatalogPage{}, err
	}
	defer tx.Rollback()
	projectID := options.ProjectID.String()
	var minimum, latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(sequence),0),COALESCE(MAX(sequence),0)
		FROM ai_conversation_changes WHERE project_id=?`, projectID).Scan(&minimum, &latest); err != nil {
		return aiConversationCatalogPage{}, err
	}

	if options.AfterRevision != nil {
		after := *options.AfterRevision
		if after > latest {
			return aiConversationCatalogPage{}, errRPCInvalid
		}
		if minimum > 0 && after+1 < minimum {
			return aiConversationCatalogPage{
				Items: []aiConversationListItem{}, Changes: []aiConversationCatalogChange{},
				AckedThroughSequence: after, HighWatermark: latest, MinimumAvailableSequence: minimum,
				ResetRequired: true,
			}, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT sequence,deleted,payload_json,conversation_id
			FROM ai_conversation_changes WHERE project_id=? AND sequence>? ORDER BY sequence LIMIT ?`,
			projectID, after, options.Limit+1)
		if err != nil {
			return aiConversationCatalogPage{}, err
		}
		type storedChange struct {
			sequence       uint64
			deleted        int
			payload        string
			conversationID string
		}
		stored := make([]storedChange, 0, options.Limit+1)
		for rows.Next() {
			var change storedChange
			if err := rows.Scan(&change.sequence, &change.deleted, &change.payload, &change.conversationID); err != nil {
				_ = rows.Close()
				return aiConversationCatalogPage{}, err
			}
			stored = append(stored, change)
		}
		if err := rows.Close(); err != nil {
			return aiConversationCatalogPage{}, err
		}
		hasMore := len(stored) > options.Limit
		if hasMore {
			stored = stored[:options.Limit]
		}
		changes := make([]aiConversationCatalogChange, 0, len(stored))
		for _, storedChange := range stored {
			if uuid.Validate(storedChange.conversationID) != nil || (storedChange.deleted != 0 && storedChange.deleted != 1) {
				return aiConversationCatalogPage{}, errors.New("stored AI conversation change is invalid")
			}
			change := aiConversationCatalogChange{
				Sequence: storedChange.sequence, ConversationID: storedChange.conversationID,
			}
			if storedChange.deleted == 1 {
				change.Operation = "delete"
			} else {
				var item aiConversationListItem
				if json.Unmarshal([]byte(storedChange.payload), &item) != nil || !validAIConversationListItem(item) {
					// V12 journal entries used conversationView. They are safe to
					// adapt from the current materialized projection during upgrade;
					// all new records already carry the compact item directly.
					loaded, scanErr := scanAIConversationListItem(tx.QueryRowContext(ctx, aiConversationCatalogSelect+
						` WHERE device_id=? AND project_id=? AND id=?`, store.deviceID.String(), projectID, storedChange.conversationID))
					if scanErr != nil {
						return aiConversationCatalogPage{}, firstError(scanErr, errors.New("stored AI conversation change is invalid"))
					}
					item = loaded
				}
				change.Operation, change.Item = "upsert", &item
			}
			changes = append(changes, change)
		}
		acked := after
		if len(changes) > 0 {
			acked = changes[len(changes)-1].Sequence
		}
		return aiConversationCatalogPage{
			Items: []aiConversationListItem{}, Changes: changes, AckedThroughSequence: acked,
			HighWatermark: latest, MinimumAvailableSequence: minimum, HasMoreChanges: hasMore,
		}, nil
	}

	snapshotRevision := latest
	var cursorUpdatedAt int64
	var cursorID string
	if cursor := options.Cursor; cursor != nil {
		if cursor.Version != 2 || cursor.SnapshotRevision == 0 || cursor.UpdatedAtMS <= 0 || uuid.Validate(cursor.ID) != nil {
			return aiConversationCatalogPage{}, errRPCInvalid
		}
		snapshotRevision, cursorUpdatedAt, cursorID = cursor.SnapshotRevision, cursor.UpdatedAtMS, cursor.ID
	}
	query := aiConversationCatalogSelect + ` WHERE device_id=? AND project_id=? AND revision<=?`
	args := []any{store.deviceID.String(), projectID, snapshotRevision}
	if options.Cursor != nil {
		query += ` AND (updated_at_ms<? OR (updated_at_ms=? AND id<?))`
		args = append(args, cursorUpdatedAt, cursorUpdatedAt, cursorID)
	}
	query += ` ORDER BY updated_at_ms DESC,id DESC LIMIT ?`
	args = append(args, options.Limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return aiConversationCatalogPage{}, err
	}
	defer rows.Close()
	items := make([]aiConversationListItem, 0, options.Limit)
	for rows.Next() {
		item, err := scanAIConversationListItem(rows)
		if err != nil {
			return aiConversationCatalogPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return aiConversationCatalogPage{}, err
	}
	var next *aiConversationCatalogCursor
	if len(items) > options.Limit {
		items = items[:options.Limit]
		last := items[len(items)-1]
		next = &aiConversationCatalogCursor{
			Version: 2, SnapshotRevision: snapshotRevision, UpdatedAtMS: last.UpdatedAt.UTC().UnixMilli(), ID: last.ID,
		}
	}
	return aiConversationCatalogPage{
		Items: items, Changes: []aiConversationCatalogChange{}, NextCursor: next,
		AckedThroughSequence: snapshotRevision, HighWatermark: snapshotRevision,
		MinimumAvailableSequence: minimum,
	}, nil
}

// getAIConversationCatalogItem is the narrow detail-header fallback for a
// conversation that is outside the loaded catalog window. It intentionally
// uses the same materialized projection as the list and never fetches a page
// of neighboring conversations or reads ai_messages.
func (store *businessStore) getAIConversationCatalogItem(ctx context.Context, projectID uuid.UUID, conversationID string) (aiConversationListItem, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil {
		return aiConversationListItem{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return aiConversationListItem{}, err
	}
	defer db.Close()
	item, err := scanAIConversationListItem(db.QueryRowContext(ctx, aiConversationCatalogSelect+
		` WHERE device_id=? AND project_id=? AND id=?`, store.deviceID.String(), projectID.String(), conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return aiConversationListItem{}, errRPCNotFound
	}
	if err != nil {
		return aiConversationListItem{}, err
	}
	return item, nil
}

func (store *businessStore) renameAIConversation(ctx context.Context, projectID uuid.UUID, conversationID, title string, expectedRevision uint64, now time.Time) (conversationView, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || expectedRevision == 0 ||
		strings.TrimSpace(title) == "" || len(title) > maximumAIConversationTitleBytes || !utf8.ValidString(title) || now.IsZero() {
		return conversationView{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return conversationView{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return conversationView{}, err
	}
	defer tx.Rollback()
	value, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return conversationView{}, err
	}
	if !found {
		return conversationView{}, errRPCNotFound
	}
	if value.Revision != expectedRevision || value.State == "generating" {
		return conversationView{}, errRPCRevision
	}
	previousRevision := value.Revision
	value.Title, value.UpdatedAt = strings.TrimSpace(title), now.UTC()
	value, err = appendAIConversationChange(ctx, store, tx, value, false, now)
	if err != nil {
		return conversationView{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET title=?,revision=?,updated_at_ms=?
        WHERE id=? AND device_id=? AND project_id=? AND revision=?`, value.Title, value.Revision, value.UpdatedAt.UnixMilli(),
		conversationID, store.deviceID.String(), projectID.String(), previousRevision)
	if err != nil {
		return conversationView{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return conversationView{}, errRPCRevision
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return conversationView{}, err
	}
	return value, nil
}

func (store *businessStore) deleteAIConversation(ctx context.Context, projectID uuid.UUID, conversationID string, expectedRevision uint64, now time.Time) (conversationView, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || expectedRevision == 0 || now.IsZero() {
		return conversationView{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return conversationView{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return conversationView{}, err
	}
	defer tx.Rollback()
	value, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return conversationView{}, err
	}
	if !found {
		return conversationView{}, errRPCNotFound
	}
	if value.Revision != expectedRevision || value.State == "generating" {
		return conversationView{}, errRPCRevision
	}
	rows, err := tx.QueryContext(ctx, aiConversationSelect+` WHERE device_id=? AND project_id=?`, store.deviceID.String(), projectID.String())
	if err != nil {
		return conversationView{}, err
	}
	all := make([]conversationView, 0)
	for rows.Next() {
		candidate, scanErr := scanAIConversation(rows)
		if scanErr != nil {
			rows.Close()
			return conversationView{}, scanErr
		}
		all = append(all, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return conversationView{}, err
	}
	if err := rows.Close(); err != nil {
		return conversationView{}, err
	}
	deletedIDs := map[string]struct{}{conversationID: {}}
	deleted := []conversationView{value}
	for changed := true; changed; {
		changed = false
		for _, candidate := range all {
			if candidate.Subagent == nil {
				continue
			}
			if _, parentDeleted := deletedIDs[candidate.Subagent.ParentConversationID]; !parentDeleted {
				continue
			}
			if _, alreadyDeleted := deletedIDs[candidate.ID]; alreadyDeleted {
				continue
			}
			deletedIDs[candidate.ID] = struct{}{}
			deleted = append(deleted, candidate)
			changed = true
		}
	}
	for index := range deleted {
		deleted[index].UpdatedAt = now.UTC()
		changed, changeErr := appendAIConversationChange(ctx, store, tx, deleted[index], true, now)
		if changeErr != nil {
			return conversationView{}, changeErr
		}
		deleted[index] = changed
		if deleted[index].ID == conversationID {
			value = changed
		}
	}
	for _, child := range deleted[1:] {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ai_conversations WHERE id=? AND device_id=? AND project_id=?`,
			child.ID, store.deviceID.String(), projectID.String()); err != nil {
			return conversationView{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM ai_conversations WHERE id=? AND device_id=? AND project_id=? AND revision=?`,
		conversationID, store.deviceID.String(), projectID.String(), expectedRevision)
	if err != nil {
		return conversationView{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return conversationView{}, errRPCRevision
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return conversationView{}, err
	}
	return value, nil
}

func (store *businessStore) listAIConversationMessages(ctx context.Context, projectID uuid.UUID, conversationID string, beforeSequence uint64, limit int) (aiMessagePage, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || limit < 1 || limit > maximumAIConversationPage {
		return aiMessagePage{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return aiMessagePage{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return aiMessagePage{}, err
	}
	defer tx.Rollback()
	conversationValue, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return aiMessagePage{}, err
	}
	if !found {
		return aiMessagePage{}, errRPCNotFound
	}
	if beforeSequence == 0 || beforeSequence > conversationValue.LastMessageSequence+1 {
		beforeSequence = conversationValue.LastMessageSequence + 1
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms
        FROM ai_messages WHERE conversation_id=? AND sequence<? ORDER BY sequence DESC LIMIT ?`, conversationID, beforeSequence, limit+1)
	if err != nil {
		return aiMessagePage{}, err
	}
	defer rows.Close()
	newestFirst := make([]chatMessage, 0, limit)
	for rows.Next() {
		message, err := scanAIMessage(rows)
		if err != nil {
			return aiMessagePage{}, err
		}
		newestFirst = append(newestFirst, message)
	}
	if err := rows.Err(); err != nil {
		return aiMessagePage{}, err
	}
	nextBefore := uint64(0)
	if len(newestFirst) > limit {
		newestFirst = newestFirst[:limit]
		nextBefore = newestFirst[len(newestFirst)-1].Sequence
	}
	items := make([]chatMessage, len(newestFirst))
	for index := range newestFirst {
		items[len(newestFirst)-1-index] = newestFirst[index]
	}
	return aiMessagePage{Items: items, NextBefore: nextBefore, HighWatermark: conversationValue.LastMessageSequence}, nil
}

// getAIConversationSnapshot creates the only supported conversation-open
// snapshot. Keep the conversation, message page, context summary and durable
// event bounds inside one read transaction; independently reading the event
// watermark after an older message page could permanently hide a delta from a
// later attach replay.
func (store *businessStore) getAIConversationSnapshot(ctx context.Context, projectID uuid.UUID, conversationID string, beforeSequence uint64, limit int) (aiConversationSnapshot, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || limit < 1 || limit > maximumAIConversationPage {
		return aiConversationSnapshot{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return aiConversationSnapshot{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return aiConversationSnapshot{}, err
	}
	defer tx.Rollback()
	conversationValue, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return aiConversationSnapshot{}, err
	}
	if !found {
		return aiConversationSnapshot{}, errRPCNotFound
	}
	if beforeSequence == 0 || beforeSequence > conversationValue.LastMessageSequence+1 {
		beforeSequence = conversationValue.LastMessageSequence + 1
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms
        FROM ai_messages WHERE conversation_id=? AND sequence<? ORDER BY sequence DESC LIMIT ?`, conversationID, beforeSequence, limit+1)
	if err != nil {
		return aiConversationSnapshot{}, err
	}
	defer rows.Close()
	newestFirst := make([]chatMessage, 0, limit)
	for rows.Next() {
		message, err := scanAIMessage(rows)
		if err != nil {
			return aiConversationSnapshot{}, err
		}
		newestFirst = append(newestFirst, message)
	}
	if err := rows.Err(); err != nil {
		return aiConversationSnapshot{}, err
	}
	nextBefore := uint64(0)
	if len(newestFirst) > limit {
		newestFirst = newestFirst[:limit]
		nextBefore = newestFirst[len(newestFirst)-1].Sequence
	}
	items := make([]chatMessage, len(newestFirst))
	for index := range newestFirst {
		items[len(newestFirst)-1-index] = newestFirst[index]
	}
	var earliest, latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(sequence),0),COALESCE(MAX(sequence),0)
        FROM ai_conversation_events WHERE conversation_id=?`, conversationID).Scan(&earliest, &latest); err != nil {
		return aiConversationSnapshot{}, err
	}
	var summary aiContextSummary
	var updatedAtMilliseconds int64
	err = tx.QueryRowContext(ctx, `SELECT s.conversation_id,s.through_sequence,s.content,s.estimated_tokens,s.updated_at_ms
        FROM ai_context_summaries s JOIN ai_conversations c ON c.id=s.conversation_id
        WHERE s.conversation_id=? AND c.device_id=? AND c.project_id=?`, conversationID, store.deviceID.String(), projectID.String()).
		Scan(&summary.ConversationID, &summary.ThroughSequence, &summary.Content, &summary.EstimatedTokens, &updatedAtMilliseconds)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return aiConversationSnapshot{}, err
	}
	var summaryValue *aiContextSummary
	if err == nil {
		summary.UpdatedAt = time.UnixMilli(updatedAtMilliseconds).UTC()
		summaryValue = &summary
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiConversationSnapshot{}, err
	}
	return aiConversationSnapshot{
		Conversation:                   conversationValue,
		Messages:                       aiMessagePage{Items: items, NextBefore: nextBefore, HighWatermark: conversationValue.LastMessageSequence},
		ContextSummary:                 summaryValue,
		EventHighWatermark:             latest,
		EarliestAvailableEventSequence: earliest,
	}, nil
}

func (store *businessStore) aiMessagesForCompletion(ctx context.Context, tx *sql.Tx, conversationID string, beforeSequence uint64) ([]chatMessage, error) {
	condition, arguments := "", []any{conversationID}
	if beforeSequence > 0 {
		condition = " AND sequence < ?"
		arguments = append(arguments, beforeSequence)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms
        FROM ai_messages WHERE conversation_id=? AND status='complete' AND role IN ('user','assistant')`+condition+
		` ORDER BY sequence DESC LIMIT 200`, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	newest := make([]chatMessage, 0, 200)
	for rows.Next() {
		message, err := scanAIMessage(rows)
		if err != nil {
			return nil, err
		}
		newest = append(newest, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]chatMessage, len(newest))
	for index := range newest {
		result[len(newest)-1-index] = newest[index]
	}
	return result, nil
}

func (store *businessStore) beginAIConversationTurn(ctx context.Context, projectID uuid.UUID, conversationID, userMessageID, prompt, workspaceMode string, attachments []chatAttachmentReference, config aiConfig, now time.Time) (aiConversationTurn, error) {
	return store.beginAIConversationTurnWithGeneration(ctx, projectID, conversationID, userMessageID, uuid.NewString(), prompt, workspaceMode, attachments, config, now)
}

func (store *businessStore) beginAIConversationTurnWithGeneration(ctx context.Context, projectID uuid.UUID, conversationID, userMessageID, generationID, prompt, workspaceMode string, attachments []chatAttachmentReference, config aiConfig, now time.Time) (aiConversationTurn, error) {
	prompt = strings.TrimSpace(prompt)
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(userMessageID) != nil || uuid.Validate(generationID) != nil ||
		prompt == "" && len(attachments) == 0 || len(prompt) > 32<<10 || !utf8.ValidString(prompt) || !validAIWorkspaceMode(workspaceMode) ||
		len(attachments) > 8 || config.ID == "" || config.Revision == 0 || now.IsZero() {
		return aiConversationTurn{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return aiConversationTurn{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return aiConversationTurn{}, err
	}
	defer tx.Rollback()
	value, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return aiConversationTurn{}, err
	}
	if !found {
		return aiConversationTurn{}, errRPCNotFound
	}
	if value.ConfigID != config.ID {
		return aiConversationTurn{}, errRPCRevision
	}
	if existing, err := scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
        WHERE conversation_id=? AND id=?`, conversationID, userMessageID)); err == nil {
		if existing.Role != "user" || existing.Content != prompt || !slices.Equal(existing.Attachments, attachments) {
			return aiConversationTurn{}, errRPCRevision
		}
		var assistant chatMessage
		assistant, err = scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
            reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
            WHERE conversation_id=? AND generation_id=? AND role='assistant' ORDER BY sequence DESC LIMIT 1`, conversationID, existing.GenerationID))
		if err != nil {
			return aiConversationTurn{}, err
		}
		return aiConversationTurn{Conversation: value, User: existing, Assistant: assistant, GenerationID: existing.GenerationID, Replayed: true}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return aiConversationTurn{}, err
	}
	if value.State == "generating" {
		return aiConversationTurn{}, errRPCConversationGenerationActive
	}
	history, err := store.aiMessagesForCompletion(ctx, tx, conversationID, 0)
	if err != nil {
		return aiConversationTurn{}, err
	}
	assistantID := uuid.NewString()
	binding := aiConversationBinding(config)
	value.ModelBinding, value.Model, value.WorkspaceMode = binding, binding.Model, workspaceMode
	value.Todos = []aiTodoItem{}
	value.State, value.GenerationID = "generating", generationID
	value.LastMessageSequence += 2
	value.MessageCount += 2
	value.UpdatedAt = now.UTC()
	user := chatMessage{
		ID: userMessageID, Revision: 1, Sequence: value.LastMessageSequence - 1, Role: "user", Content: prompt,
		Status: "complete", Attachments: append([]chatAttachmentReference(nil), attachments...), ToolRuns: []chatToolRun{},
		CreatedAt: now.UTC(), GenerationID: generationID,
	}
	assistant := chatMessage{
		ID: assistantID, Revision: 1, Sequence: value.LastMessageSequence, Role: "assistant", Status: "streaming",
		Attachments: []chatAttachmentReference{}, ToolRuns: []chatToolRun{}, CreatedAt: now.UTC(), GenerationID: generationID,
		ProviderRun: chatProviderRun{Provider: config.Provider, Model: config.Model, AttemptCount: 1},
	}
	value.Catalog = withAIConversationCatalogMessage(value.Catalog, user)
	previousRevision := value.Revision
	value, err = appendAIConversationChange(ctx, store, tx, value, false, now)
	if err != nil {
		return aiConversationTurn{}, err
	}
	if err := insertAIMessage(ctx, tx, conversationID, user, now); err != nil {
		return aiConversationTurn{}, err
	}
	if err := insertAIMessage(ctx, tx, conversationID, assistant, now); err != nil {
		return aiConversationTurn{}, err
	}
	bindingJSON, err := marshalAIJSON(binding, 16<<10)
	if err != nil {
		return aiConversationTurn{}, err
	}
	collaborationJSON, err := marshalAIJSON(collaborationFromConversation(value), 128<<10)
	if err != nil {
		return aiConversationTurn{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET revision=?,model_binding_json=?,workspace_mode=?,state='generating',
	        generation_id=?,active_assistant_id=?,last_message_sequence=?,message_count=?,updated_at_ms=?,collaboration_json=?,
	        latest_message_sequence=?,latest_message_role=?,latest_message_status=?,latest_message_preview=?,
	        latest_message_created_at_ms=?,last_completed_assistant_sequence=?,last_error_code=?
	        WHERE id=? AND device_id=? AND project_id=? AND revision=? AND state<>'generating'`, value.Revision, bindingJSON,
		aiWorkspaceModeForStorage(workspaceMode), generationID, assistantID, value.LastMessageSequence, value.MessageCount, value.UpdatedAt.UnixMilli(), collaborationJSON,
		value.Catalog.LatestMessageSequence, value.Catalog.LatestMessageRole, value.Catalog.LatestMessageStatus,
		value.Catalog.LatestMessagePreview, catalogProjectionCreatedAtMilliseconds(value.Catalog),
		value.Catalog.LastCompletedAssistantSequence, value.Catalog.LastErrorCode,
		conversationID, store.deviceID.String(), projectID.String(), previousRevision)
	if err != nil {
		return aiConversationTurn{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return aiConversationTurn{}, errRPCRevision
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiConversationTurn{}, err
	}
	return aiConversationTurn{Conversation: value, History: history, User: user, Assistant: assistant, GenerationID: generationID}, nil
}

func (store *businessStore) beginAIConversationRegeneration(ctx context.Context, projectID uuid.UUID, conversationID, assistantMessageID, workspaceMode string, config aiConfig, now time.Time) (aiConversationTurn, error) {
	return store.beginAIConversationRegenerationWithGeneration(ctx, projectID, conversationID, assistantMessageID, uuid.NewString(), workspaceMode, config, now)
}

func (store *businessStore) beginAIConversationRegenerationWithGeneration(ctx context.Context, projectID uuid.UUID, conversationID, assistantMessageID, generationID, workspaceMode string, config aiConfig, now time.Time) (aiConversationTurn, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(assistantMessageID) != nil || uuid.Validate(generationID) != nil ||
		!validAIWorkspaceMode(workspaceMode) || config.ID == "" || config.Revision == 0 || now.IsZero() {
		return aiConversationTurn{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return aiConversationTurn{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return aiConversationTurn{}, err
	}
	defer tx.Rollback()
	value, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return aiConversationTurn{}, err
	}
	if !found {
		return aiConversationTurn{}, errRPCNotFound
	}
	if value.ConfigID != config.ID {
		return aiConversationTurn{}, errRPCRevision
	}
	if value.State == "generating" {
		return aiConversationTurn{}, errRPCConversationGenerationActive
	}
	target, err := scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
        WHERE conversation_id=? AND id=?`, conversationID, assistantMessageID))
	if errors.Is(err, sql.ErrNoRows) {
		return aiConversationTurn{}, errRPCNotFound
	}
	if err != nil {
		return aiConversationTurn{}, err
	}
	if target.Role != "assistant" || target.Status == "streaming" {
		return aiConversationTurn{}, errRPCInvalid
	}
	user, err := scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
        WHERE conversation_id=? AND role='user' AND sequence<? ORDER BY sequence DESC LIMIT 1`, conversationID, target.Sequence))
	if errors.Is(err, sql.ErrNoRows) {
		return aiConversationTurn{}, errRPCInvalid
	}
	if err != nil {
		return aiConversationTurn{}, err
	}
	history, err := store.aiMessagesForCompletion(ctx, tx, conversationID, user.Sequence)
	if err != nil {
		return aiConversationTurn{}, err
	}
	newAssistantID := uuid.NewString()
	binding := aiConversationBinding(config)
	value.ModelBinding, value.Model, value.WorkspaceMode = binding, binding.Model, workspaceMode
	value.Todos = []aiTodoItem{}
	value.State, value.GenerationID = "generating", generationID
	value.LastMessageSequence++
	value.MessageCount++
	value.UpdatedAt = now.UTC()
	value.Catalog = withAIConversationCatalogMessage(value.Catalog, user)
	previousRevision := value.Revision
	value, err = appendAIConversationChange(ctx, store, tx, value, false, now)
	if err != nil {
		return aiConversationTurn{}, err
	}
	assistant := chatMessage{
		ID: newAssistantID, Revision: 1, Sequence: value.LastMessageSequence, Role: "assistant", Status: "streaming",
		Attachments: []chatAttachmentReference{}, ToolRuns: []chatToolRun{}, CreatedAt: now.UTC(), GenerationID: generationID,
		ProviderRun: chatProviderRun{Provider: config.Provider, Model: config.Model, AttemptCount: 1},
	}
	if err := insertAIMessage(ctx, tx, conversationID, assistant, now); err != nil {
		return aiConversationTurn{}, err
	}
	bindingJSON, err := marshalAIJSON(binding, 16<<10)
	if err != nil {
		return aiConversationTurn{}, err
	}
	collaborationJSON, err := marshalAIJSON(collaborationFromConversation(value), 128<<10)
	if err != nil {
		return aiConversationTurn{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET revision=?,model_binding_json=?,workspace_mode=?,state='generating',
	        generation_id=?,active_assistant_id=?,last_message_sequence=?,message_count=?,updated_at_ms=?,collaboration_json=?,
	        latest_message_sequence=?,latest_message_role=?,latest_message_status=?,latest_message_preview=?,
	        latest_message_created_at_ms=?,last_completed_assistant_sequence=?,last_error_code=?
	        WHERE id=? AND device_id=? AND project_id=? AND revision=? AND state<>'generating'`, value.Revision, bindingJSON,
		aiWorkspaceModeForStorage(workspaceMode), generationID, newAssistantID, value.LastMessageSequence, value.MessageCount, value.UpdatedAt.UnixMilli(), collaborationJSON,
		value.Catalog.LatestMessageSequence, value.Catalog.LatestMessageRole, value.Catalog.LatestMessageStatus,
		value.Catalog.LatestMessagePreview, catalogProjectionCreatedAtMilliseconds(value.Catalog),
		value.Catalog.LastCompletedAssistantSequence, value.Catalog.LastErrorCode,
		conversationID, store.deviceID.String(), projectID.String(), previousRevision)
	if err != nil {
		return aiConversationTurn{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return aiConversationTurn{}, errRPCRevision
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiConversationTurn{}, err
	}
	return aiConversationTurn{Conversation: value, History: history, User: user, Assistant: assistant, GenerationID: generationID}, nil
}

func (store *businessStore) appendAIConversationTextDelta(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID, delta string, now time.Time) (chatMessage, aiConversationEvent, error) {
	return store.appendAIConversationDelta(ctx, projectID, conversationID, generationID, assistantID, "text", delta, now)
}

func compactAIConversationToolRunForEvent(run chatToolRun) chatToolRun {
	encoded, err := json.Marshal(run)
	if err == nil && len(encoded) <= maximumAIPersistentEventPayload {
		return run
	}
	// The authoritative run remains in ai_messages. Replay only needs enough
	// state to update the timeline; clients that need the full arguments/result
	// obtain it from the message snapshot instead of forcing it into every event
	// page.
	result := run
	result.Arguments = map[string]any{"truncated": true, "source": "messageSnapshot"}
	result.Result = map[string]any{"truncated": true, "source": "messageSnapshot"}
	result.Output = truncateAIConversationUTF8(run.Output, 1024)
	return result
}

func truncateAIConversationUTF8(value string, maximumBytes int) string {
	if maximumBytes < 1 || len(value) <= maximumBytes {
		return value
	}
	end := maximumBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	if end == 0 {
		return ""
	}
	return value[:end] + "…"
}

func compactAIConversationToolRunStorageValue(value any) any {
	marker := map[string]any{"truncated": true, "reason": "message_storage_limit"}
	encoded, err := json.Marshal(value)
	markerEncoded, markerErr := json.Marshal(marker)
	if err != nil || markerErr == nil && len(encoded) > len(markerEncoded) {
		return marker
	}
	return value
}

func compactAIConversationToolRunForStorage(run chatToolRun) chatToolRun {
	result := run
	result.Description = truncateAIConversationUTF8(result.Description, maximumAICompactedToolRunText)
	result.Arguments = compactAIConversationToolRunStorageValue(result.Arguments)
	result.Result = compactAIConversationToolRunStorageValue(result.Result)
	result.Output = truncateAIConversationUTF8(result.Output, maximumAICompactedToolRunText)
	// Views and content offsets are derived presentation data. Dropping them is
	// preferable to losing the run identity, status, timing, or the containing
	// assistant message when an older near-limit record must be terminalized.
	result.View = nil
	result.ContentOffset = nil
	return result
}

type aiConversationToolRunCompactionCandidate struct {
	index        int
	savings      int
	terminalized bool
	value        chatToolRun
}

// marshalTerminalAIConversationToolRuns preserves the complete tool timeline
// whenever it fits. Older Agents could persist an active array with only a few
// bytes left below the limit; adding finishedAt and the cancellation code on a
// restart then made that same record impossible to save. In that compatibility
// case, compact payload-heavy fields in the smallest useful order while keeping
// every run's identity and terminal state.
func marshalTerminalAIConversationToolRuns(runs []chatToolRun, terminalizedIndexes []int) ([]chatToolRun, string, error) {
	encoded, err := marshalAIJSON(runs, maximumAIMessageToolRunsBytes)
	if err == nil {
		return runs, encoded, nil
	}
	terminalized := make(map[int]struct{}, len(terminalizedIndexes))
	for _, index := range terminalizedIndexes {
		terminalized[index] = struct{}{}
	}
	candidates := make([]aiConversationToolRunCompactionCandidate, 0, len(runs))
	for index, run := range runs {
		compacted := compactAIConversationToolRunForStorage(run)
		before, beforeErr := json.Marshal(run)
		after, afterErr := json.Marshal(compacted)
		if beforeErr != nil || afterErr != nil || len(after) >= len(before) {
			continue
		}
		_, preferred := terminalized[index]
		candidates = append(candidates, aiConversationToolRunCompactionCandidate{
			index: index, savings: len(before) - len(after), terminalized: preferred, value: compacted,
		})
	}
	slices.SortFunc(candidates, func(left, right aiConversationToolRunCompactionCandidate) int {
		if left.terminalized != right.terminalized {
			if left.terminalized {
				return -1
			}
			return 1
		}
		if order := cmp.Compare(right.savings, left.savings); order != 0 {
			return order
		}
		return cmp.Compare(left.index, right.index)
	})
	result := append([]chatToolRun(nil), runs...)
	for _, candidate := range candidates {
		result[candidate.index] = candidate.value
		encoded, err = marshalAIJSON(result, maximumAIMessageToolRunsBytes)
		if err == nil {
			return result, encoded, nil
		}
	}
	return nil, "", err
}

// marshalActiveAIConversationToolRuns rejects a new active snapshot unless its
// running entries can still be terminalized at their maximum valid metadata
// sizes. This keeps newly written records out of the legacy recovery trap.
func marshalActiveAIConversationToolRuns(runs []chatToolRun) (string, error) {
	encoded, err := marshalAIJSON(runs, maximumAIMessageToolRunsBytes)
	if err != nil {
		return "", err
	}
	recoverable := append([]chatToolRun(nil), runs...)
	maximumTimestamp := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	maximumErrorCode := strings.Repeat("x", 80)
	for index := range recoverable {
		if recoverable[index].Status != "running" {
			continue
		}
		recoverable[index].Status = "cancelled"
		recoverable[index].ErrorCode = maximumErrorCode
		recoverable[index].FinishedAt = &maximumTimestamp
	}
	if _, err := marshalAIJSON(recoverable, maximumAIMessageToolRunsBytes); err != nil {
		return "", err
	}
	return encoded, nil
}

func (store *businessStore) appendAIConversationReasoningDelta(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID, delta string, now time.Time) (chatMessage, aiConversationEvent, error) {
	return store.appendAIConversationDelta(ctx, projectID, conversationID, generationID, assistantID, "reasoning", delta, now)
}

func (store *businessStore) upsertAIConversationToolRun(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID string, run chatToolRun, now time.Time) (chatMessage, aiConversationEvent, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil ||
		uuid.Validate(assistantID) != nil || !validChatToolRun(run) || now.IsZero() {
		return chatMessage{}, aiConversationEvent{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	defer tx.Rollback()
	var activeGeneration, activeAssistant, state string
	if err := tx.QueryRowContext(ctx, `SELECT generation_id,active_assistant_id,state FROM ai_conversations
        WHERE id=? AND device_id=? AND project_id=?`, conversationID, store.deviceID.String(), projectID.String()).Scan(&activeGeneration, &activeAssistant, &state); errors.Is(err, sql.ErrNoRows) {
		return chatMessage{}, aiConversationEvent{}, errRPCNotFound
	} else if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	if state != "generating" || activeGeneration != generationID || activeAssistant != assistantID {
		return chatMessage{}, aiConversationEvent{}, errRPCRevision
	}
	message, err := scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
        WHERE conversation_id=? AND id=?`, conversationID, assistantID))
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	if message.Status != "streaming" || message.Revision == maxSafeJSONInteger {
		return chatMessage{}, aiConversationEvent{}, errRPCRevision
	}
	found := -1
	for index := range message.ToolRuns {
		if message.ToolRuns[index].ID == run.ID {
			found = index
			break
		}
	}
	if found < 0 {
		if len(message.ToolRuns) >= 200 || run.Status != "running" {
			return chatMessage{}, aiConversationEvent{}, errRPCInvalid
		}
		message.ToolRuns = append(message.ToolRuns, run)
	} else {
		previous := message.ToolRuns[found]
		previousArguments, _ := json.Marshal(previous.Arguments)
		nextArguments, _ := json.Marshal(run.Arguments)
		if previous.Tool != run.Tool || !bytes.Equal(previousArguments, nextArguments) || !previous.StartedAt.Equal(run.StartedAt) ||
			previous.Status != "running" || run.Status == "running" && len(run.Output) <= len(previous.Output) {
			return chatMessage{}, aiConversationEvent{}, errRPCRevision
		}
		message.ToolRuns[found] = run
	}
	toolRunsJSON, err := marshalActiveAIConversationToolRuns(message.ToolRuns)
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	previousRevision := message.Revision
	message.Revision++
	result, err := tx.ExecContext(ctx, `UPDATE ai_messages SET revision=?,tool_runs_json=?,updated_at_ms=?
        WHERE id=? AND conversation_id=? AND revision=? AND status='streaming'`, message.Revision, toolRunsJSON,
		now.UTC().UnixMilli(), assistantID, conversationID, previousRevision)
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return chatMessage{}, aiConversationEvent{}, errRPCRevision
	}
	event, err := appendAIConversationEvent(ctx, store, tx, aiConversationEvent{
		EventID: uuid.NewString(), ConversationID: conversationID, GenerationID: generationID, MessageID: assistantID,
		Kind: "chat.tool.status", Payload: map[string]any{"toolRun": compactAIConversationToolRunForEvent(run)}, OccurredAt: now.UTC(),
	})
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	return message, event, nil
}

func compactAIConversationApprovalForEvent(approval aiApprovalRequest) map[string]any {
	payload := map[string]any{"approval": approval}
	if encoded, err := json.Marshal(payload); err == nil && len(encoded) <= maximumAIPersistentEventPayload {
		return payload
	}
	// The complete preview stays with the in-memory pending approval and is
	// returned by conversation.generation.get. A replay page carries only the
	// stable identity needed to trigger that safe, bounded lookup.
	return map[string]any{"approvalRef": map[string]any{
		"id": approval.ID, "conversationId": approval.ConversationID, "generationId": approval.GenerationID,
		"messageId": approval.MessageID, "toolCallId": approval.ToolCallID, "toolName": approval.ToolName,
		"source": "generationState",
	}}
}

func (store *businessStore) appendAIConversationApprovalRequested(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID string, approval aiApprovalRequest, now time.Time) (aiConversationEvent, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil ||
		uuid.Validate(assistantID) != nil || !validAIApprovalRequest(approval) || now.IsZero() {
		return aiConversationEvent{}, errRPCInvalid
	}
	if _, err := marshalAIJSON(approval, maximumAIPendingApprovalBytes); err != nil {
		return aiConversationEvent{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return aiConversationEvent{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return aiConversationEvent{}, err
	}
	defer tx.Rollback()
	var activeGeneration, activeAssistant, state string
	if err := tx.QueryRowContext(ctx, `SELECT generation_id,active_assistant_id,state FROM ai_conversations
        WHERE id=? AND device_id=? AND project_id=?`, conversationID, store.deviceID.String(), projectID.String()).Scan(&activeGeneration, &activeAssistant, &state); errors.Is(err, sql.ErrNoRows) {
		return aiConversationEvent{}, errRPCNotFound
	} else if err != nil {
		return aiConversationEvent{}, err
	}
	if state != "generating" || activeGeneration != generationID || activeAssistant != assistantID {
		return aiConversationEvent{}, errRPCRevision
	}
	event, err := appendAIConversationEvent(ctx, store, tx, aiConversationEvent{
		EventID: uuid.NewString(), ConversationID: conversationID, GenerationID: generationID, MessageID: assistantID,
		Kind: "chat.approval.requested", Payload: compactAIConversationApprovalForEvent(approval), OccurredAt: now.UTC(),
	})
	if err != nil {
		return aiConversationEvent{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiConversationEvent{}, err
	}
	return event, nil
}

func validChatToolRun(run chatToolRun) bool {
	if run.Name == "" {
		run.Name = run.Tool
	}
	if run.Tool == "" {
		run.Tool = run.Name
	}
	if run.ID == "" || len(run.ID) > 256 || !utf8.ValidString(run.ID) || run.Name != run.Tool || !validAIProviderToolName(run.Tool) ||
		!utf8.ValidString(run.Description) || len(run.Description) > 2048 || !utf8.ValidString(run.Output) || len(run.Output) > maximumAIWorkspaceToolResult ||
		!slices.Contains([]string{"running", "succeeded", "failed", "cancelled"}, run.Status) || len(run.ErrorCode) > 80 || run.StartedAt.IsZero() {
		return false
	}
	terminal := run.Status != "running"
	if terminal != (run.FinishedAt != nil) || run.FinishedAt != nil && run.FinishedAt.Before(run.StartedAt) {
		return false
	}
	arguments, argumentsErr := json.Marshal(run.Arguments)
	result, resultErr := json.Marshal(run.Result)
	if run.View != nil {
		view, viewErr := json.Marshal(run.View)
		if viewErr != nil || len(view) > maximumAIToolViewBytes {
			return false
		}
	}
	return argumentsErr == nil && resultErr == nil && len(arguments) <= maximumRPCPayload && len(result) <= 48<<10
}

func (store *businessStore) appendAIConversationDelta(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID, field, delta string, now time.Time) (chatMessage, aiConversationEvent, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil ||
		uuid.Validate(assistantID) != nil || field != "text" && field != "reasoning" ||
		delta == "" || len(delta) > maximumAIPersistentDeltaBytes || !utf8.ValidString(delta) || now.IsZero() {
		return chatMessage{}, aiConversationEvent{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	defer tx.Rollback()
	var activeGeneration, activeAssistant, state string
	if err := tx.QueryRowContext(ctx, `SELECT generation_id,active_assistant_id,state FROM ai_conversations
        WHERE id=? AND device_id=? AND project_id=?`, conversationID, store.deviceID.String(), projectID.String()).Scan(&activeGeneration, &activeAssistant, &state); errors.Is(err, sql.ErrNoRows) {
		return chatMessage{}, aiConversationEvent{}, errRPCNotFound
	} else if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	if state != "generating" || activeGeneration != generationID || activeAssistant != assistantID {
		return chatMessage{}, aiConversationEvent{}, errRPCRevision
	}
	message, err := scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
        WHERE conversation_id=? AND id=?`, conversationID, assistantID))
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	current := message.Content
	if field == "reasoning" {
		current = message.Reasoning
	}
	if message.Status != "streaming" || len(current)+len(delta) > maximumAssistantBytes || message.Revision == maxSafeJSONInteger {
		return chatMessage{}, aiConversationEvent{}, errRPCRevision
	}
	previousRevision := message.Revision
	current += delta
	message.Revision++
	var result sql.Result
	if field == "text" {
		message.Content = current
		result, err = tx.ExecContext(ctx, `UPDATE ai_messages SET revision=?,content=?,updated_at_ms=?
            WHERE id=? AND conversation_id=? AND revision=? AND status='streaming'`, message.Revision, message.Content,
			now.UTC().UnixMilli(), assistantID, conversationID, previousRevision)
	} else {
		message.Reasoning = current
		result, err = tx.ExecContext(ctx, `UPDATE ai_messages SET revision=?,reasoning=?,updated_at_ms=?
            WHERE id=? AND conversation_id=? AND revision=? AND status='streaming'`, message.Revision, message.Reasoning,
			now.UTC().UnixMilli(), assistantID, conversationID, previousRevision)
	}
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return chatMessage{}, aiConversationEvent{}, errRPCRevision
	}
	eventKind := "chat." + field + ".delta"
	event, err := appendAIConversationEvent(ctx, store, tx, aiConversationEvent{
		EventID: uuid.NewString(), ConversationID: conversationID, GenerationID: generationID, MessageID: assistantID,
		Kind: eventKind, Payload: map[string]any{"delta": delta}, OccurredAt: now.UTC(),
	})
	if err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return chatMessage{}, aiConversationEvent{}, err
	}
	return message, event, nil
}

func (store *businessStore) finishAIConversationTurn(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID string, usage chatUsage, providerRun chatProviderRun, now time.Time) (chatMessage, conversationView, []aiConversationEvent, error) {
	return store.completeAIConversationTurn(ctx, projectID, conversationID, generationID, assistantID, "complete", "", usage, providerRun, now)
}

func (store *businessStore) abortAIConversationTurn(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID, status, errorCode string, providerRun chatProviderRun, now time.Time) (chatMessage, conversationView, []aiConversationEvent, error) {
	if status != "stopped" && status != "failed" {
		return chatMessage{}, conversationView{}, nil, errRPCInvalid
	}
	return store.completeAIConversationTurn(ctx, projectID, conversationID, generationID, assistantID, status, errorCode, chatUsage{}, providerRun, now)
}

func compactAIConversationTerminalPayload(message chatMessage, errorCode string) map[string]any {
	payload := map[string]any{
		"messageRef": map[string]any{
			"id": message.ID, "revision": message.Revision, "sequence": message.Sequence,
			"status": message.Status, "generationId": message.GenerationID,
		},
		"errorCode": errorCode,
	}
	if encoded, err := json.Marshal(message); err == nil && len(encoded) <= maximumAIPersistentEventPayload {
		payload["message"] = message
	}
	return payload
}

func (store *businessStore) completeAIConversationTurn(ctx context.Context, projectID uuid.UUID, conversationID, generationID, assistantID, status, errorCode string, usage chatUsage, providerRun chatProviderRun, now time.Time) (chatMessage, conversationView, []aiConversationEvent, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil ||
		uuid.Validate(assistantID) != nil || !validAIMessageStatus(status) || status == "streaming" || len(errorCode) > 80 || now.IsZero() {
		return chatMessage{}, conversationView{}, nil, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	defer tx.Rollback()
	value, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	if !found {
		return chatMessage{}, conversationView{}, nil, errRPCNotFound
	}
	if value.State != "generating" || value.GenerationID != generationID {
		return chatMessage{}, conversationView{}, nil, errRPCRevision
	}
	var activeAssistant string
	if err := tx.QueryRowContext(ctx, `SELECT active_assistant_id FROM ai_conversations WHERE id=?`, conversationID).Scan(&activeAssistant); err != nil || activeAssistant != assistantID {
		return chatMessage{}, conversationView{}, nil, firstError(err, errRPCRevision)
	}
	message, err := scanAIMessage(tx.QueryRowContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
        WHERE conversation_id=? AND id=?`, conversationID, assistantID))
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	if message.Status != "streaming" || status == "complete" && strings.TrimSpace(message.Content) == "" && len(message.ToolRuns) == 0 || message.Revision == maxSafeJSONInteger {
		return chatMessage{}, conversationView{}, nil, errRPCRevision
	}
	terminalizedIndexes := make([]int, 0)
	for index := range message.ToolRuns {
		if message.ToolRuns[index].Status != "running" {
			continue
		}
		if status == "complete" {
			return chatMessage{}, conversationView{}, nil, errRPCRevision
		}
		finishedAt := now.UTC()
		message.ToolRuns[index].FinishedAt = &finishedAt
		message.ToolRuns[index].ErrorCode = firstNonEmpty(errorCode, "generation_failed")
		message.ToolRuns[index].Status = "failed"
		if status == "stopped" {
			message.ToolRuns[index].Status = "cancelled"
			message.ToolRuns[index].ErrorCode = "cancelled"
		}
		terminalizedIndexes = append(terminalizedIndexes, index)
	}
	message.Status, message.ErrorCode, message.Usage, message.ProviderRun = status, errorCode, usage, providerRun
	message.Revision++
	var toolRunsJSON string
	message.ToolRuns, toolRunsJSON, err = marshalTerminalAIConversationToolRuns(message.ToolRuns, terminalizedIndexes)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	terminalizedToolRuns := make([]chatToolRun, 0, len(terminalizedIndexes))
	for _, index := range terminalizedIndexes {
		terminalizedToolRuns = append(terminalizedToolRuns, message.ToolRuns[index])
	}
	usageJSON, err := marshalAIJSON(message.Usage, 16<<10)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	providerRunJSON, err := marshalAIJSON(message.ProviderRun, 16<<10)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_messages SET revision=?,status=?,error_code=?,tool_runs_json=?,usage_json=?,provider_run_json=?,updated_at_ms=?
		WHERE id=? AND conversation_id=? AND status='streaming'`, message.Revision, status, errorCode, toolRunsJSON, usageJSON,
		providerRunJSON, now.UTC().UnixMilli(), assistantID, conversationID)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return chatMessage{}, conversationView{}, nil, errRPCRevision
	}
	events := make([]aiConversationEvent, 0, len(terminalizedToolRuns)+2)
	for _, run := range terminalizedToolRuns {
		toolEvent, err := appendAIConversationEvent(ctx, store, tx, aiConversationEvent{
			EventID: uuid.NewString(), ConversationID: conversationID, GenerationID: generationID, MessageID: assistantID,
			Kind: "chat.tool.status", Payload: map[string]any{"toolRun": compactAIConversationToolRunForEvent(run)}, OccurredAt: now.UTC(),
		})
		if err != nil {
			return chatMessage{}, conversationView{}, nil, err
		}
		events = append(events, toolEvent)
	}
	if status == "complete" {
		usageEvent, err := appendAIConversationEvent(ctx, store, tx, aiConversationEvent{
			EventID: uuid.NewString(), ConversationID: conversationID, GenerationID: generationID, MessageID: assistantID,
			Kind: "chat.usage", Payload: map[string]any{"usage": usage}, OccurredAt: now.UTC(),
		})
		if err != nil {
			return chatMessage{}, conversationView{}, nil, err
		}
		events = append(events, usageEvent)
	}
	terminalKind := "chat.completed"
	if status == "stopped" {
		terminalKind = "chat.cancelled"
	} else if status == "failed" {
		terminalKind = "chat.failed"
	}
	terminalEvent, err := appendAIConversationEvent(ctx, store, tx, aiConversationEvent{
		EventID: uuid.NewString(), ConversationID: conversationID, GenerationID: generationID, MessageID: assistantID,
		Kind: terminalKind, Payload: compactAIConversationTerminalPayload(message, errorCode), OccurredAt: now.UTC(),
	})
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	events = append(events, terminalEvent)
	previousRevision := value.Revision
	value.UpdatedAt, value.GenerationID = now.UTC(), ""
	value.State = "idle"
	if status == "failed" {
		value.State = "failed"
	}
	value.Catalog = withAIConversationCatalogMessage(value.Catalog, message)
	value, err = appendAIConversationChange(ctx, store, tx, value, false, now)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE ai_conversations SET revision=?,state=?,generation_id='',active_assistant_id='',updated_at_ms=?,
		latest_message_sequence=?,latest_message_role=?,latest_message_status=?,latest_message_preview=?,
		latest_message_created_at_ms=?,last_completed_assistant_sequence=?,last_error_code=?
		WHERE id=? AND device_id=? AND project_id=? AND revision=? AND generation_id=? AND active_assistant_id=?`, value.Revision,
		value.State, value.UpdatedAt.UnixMilli(), value.Catalog.LatestMessageSequence,
		value.Catalog.LatestMessageRole, value.Catalog.LatestMessageStatus, value.Catalog.LatestMessagePreview,
		catalogProjectionCreatedAtMilliseconds(value.Catalog), value.Catalog.LastCompletedAssistantSequence,
		value.Catalog.LastErrorCode, conversationID, store.deviceID.String(), projectID.String(), previousRevision, generationID, assistantID)
	if err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return chatMessage{}, conversationView{}, nil, errRPCRevision
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return chatMessage{}, conversationView{}, nil, err
	}
	return message, value, events, nil
}

// aiConversationEventWatermarks exposes replay retention boundaries alongside
// the authoritative message snapshot. A caller must never infer them from a
// message sequence: event and message journals intentionally have independent
// sequence spaces.
func (store *businessStore) aiConversationEventWatermarks(ctx context.Context, projectID uuid.UUID, conversationID string) (uint64, uint64, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil {
		return 0, 0, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	if _, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID); err != nil {
		return 0, 0, err
	} else if !found {
		return 0, 0, errRPCNotFound
	}
	var earliest, latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(sequence),0),COALESCE(MAX(sequence),0)
		FROM ai_conversation_events WHERE conversation_id=?`, conversationID).Scan(&earliest, &latest); err != nil {
		return 0, 0, err
	}
	return latest, earliest, nil
}

// getAIConversationGenerationState answers both current-generation checks and
// reconnect checks for a terminal generation. Completed messages are the
// authority once ai_conversations has cleared its active generation id.
func (store *businessStore) getAIConversationGenerationState(ctx context.Context, projectID uuid.UUID, conversationID, requestedGenerationID string) (aiConversationGenerationState, uint64, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil ||
		requestedGenerationID != "" && uuid.Validate(requestedGenerationID) != nil {
		return aiConversationGenerationState{}, 0, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return aiConversationGenerationState{}, 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return aiConversationGenerationState{}, 0, err
	}
	defer tx.Rollback()
	conversation, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return aiConversationGenerationState{}, 0, err
	}
	if !found {
		return aiConversationGenerationState{}, 0, errRPCNotFound
	}
	result := aiConversationGenerationState{
		ConversationID:   conversationID,
		Status:           "idle",
		CanAcceptNewTurn: conversation.State != "generating",
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)
		FROM ai_conversation_events WHERE conversation_id=?`, conversationID).Scan(&result.LastEventSequence); err != nil {
		return aiConversationGenerationState{}, 0, err
	}
	generationID := strings.TrimSpace(requestedGenerationID)
	if generationID == "" {
		generationID = strings.TrimSpace(conversation.GenerationID)
	}
	if generationID == "" {
		// A previous failed generation never blocks a new turn. Retain its state
		// only as a diagnostic when no active generation is being checked.
		if conversation.State == "failed" {
			result.Status = "failed"
		}
		return result, conversation.Revision, nil
	}
	var status, errorCode string
	var startedAtMilliseconds, updatedAtMilliseconds int64
	err = tx.QueryRowContext(ctx, `SELECT status,error_code,created_at_ms,updated_at_ms
		FROM ai_messages WHERE conversation_id=? AND generation_id=? AND role='assistant'
		ORDER BY sequence DESC LIMIT 1`, conversationID, generationID).Scan(&status, &errorCode, &startedAtMilliseconds, &updatedAtMilliseconds)
	if errors.Is(err, sql.ErrNoRows) {
		return aiConversationGenerationState{}, 0, errRPCRevision
	}
	if err != nil || startedAtMilliseconds <= 0 || updatedAtMilliseconds <= 0 {
		return aiConversationGenerationState{}, 0, firstError(err, errRPCInvalid)
	}
	result.GenerationID = generationID
	result.StartedAt = time.UnixMilli(startedAtMilliseconds).UTC()
	result.UpdatedAt = time.UnixMilli(updatedAtMilliseconds).UTC()
	result.ErrorCode = errorCode
	switch status {
	case "streaming":
		result.Status, result.CanAcceptNewTurn = "running", false
		var latestKind string
		if err := tx.QueryRowContext(ctx, `SELECT kind FROM ai_conversation_events
			WHERE conversation_id=? AND generation_id=? ORDER BY sequence DESC LIMIT 1`, conversationID, generationID).Scan(&latestKind); err == nil && latestKind == "chat.approval.requested" {
			result.Status = "awaitingApproval"
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return aiConversationGenerationState{}, 0, err
		}
	case "complete":
		result.Status, result.CanAcceptNewTurn = "completed", true
	case "stopped":
		result.Status, result.CanAcceptNewTurn = "cancelled", true
	case "failed":
		result.Status, result.CanAcceptNewTurn = "failed", true
	default:
		return aiConversationGenerationState{}, 0, errRPCInvalid
	}
	return result, conversation.Revision, nil
}

func (store *businessStore) readAIConversationMessageContent(ctx context.Context, projectID uuid.UUID, conversationID, messageID, field string, offset uint64, maximumBytes int) (aiConversationMessageContentChunk, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(messageID) != nil ||
		field != "content" && field != "reasoning" || maximumBytes < 1 || maximumBytes > maximumAIMessageContentChunkBytes {
		return aiConversationMessageContentChunk{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return aiConversationMessageContentChunk{}, err
	}
	defer db.Close()
	var content []byte
	var totalBytes int64
	var revision uint64
	column := "content"
	if field == "reasoning" {
		column = "reasoning"
	}
	// The previous implementation selected the complete TEXT value for every
	// 16 KiB chunk. A 1 MiB terminal answer therefore moved and validated about
	// 64 MiB inside the Agent before it reached the client. SQLite BLOB substr
	// is byte-addressed, matching the wire cursor, and the join verifies the
	// project binding in the same read without materializing an unrelated row.
	err = db.QueryRowContext(ctx, `SELECT
		substr(CAST(m.`+column+` AS BLOB),?,?),
		length(CAST(m.`+column+` AS BLOB)),m.revision
		FROM ai_messages m JOIN ai_conversations c ON c.id=m.conversation_id
		WHERE m.conversation_id=? AND m.id=? AND c.device_id=? AND c.project_id=?`,
		offset+1, maximumBytes, conversationID, messageID, store.deviceID.String(), projectID.String()).
		Scan(&content, &totalBytes, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return aiConversationMessageContentChunk{}, errRPCNotFound
	} else if err != nil {
		return aiConversationMessageContentChunk{}, err
	}
	if totalBytes < 0 || offset > uint64(totalBytes) {
		return aiConversationMessageContentChunk{}, errRPCInvalid
	}
	end := len(content)
	for end > 0 && !utf8.Valid(content[:end]) {
		end--
	}
	if end == 0 && offset < uint64(totalBytes) {
		return aiConversationMessageContentChunk{}, errRPCInvalid
	}
	content = content[:end]
	next := offset + uint64(end)
	return aiConversationMessageContentChunk{
		Content: string(content), Offset: offset, NextOffset: next, TotalBytes: uint64(totalBytes), HasMore: next < uint64(totalBytes), Revision: revision,
	}, nil
}

// listAIConversationEvents retains the legacy store signature for callers
// compiled against the v2 implementation. New RPC code must use
// listAIConversationEventsPage so it receives an unambiguous nextSequence and
// the byte-budget guarantee.
func (store *businessStore) listAIConversationEvents(ctx context.Context, projectID uuid.UUID, conversationID string, afterSequence uint64, limit int) ([]aiConversationEvent, uint64, bool, bool, error) {
	page, err := store.listAIConversationEventsPage(ctx, projectID, conversationID, afterSequence, limit, maximumRPCPayload)
	if err != nil {
		return nil, 0, false, false, err
	}
	return page.Items, page.HighWatermark, page.ResetRequired, page.HasMore, nil
}

// listAIConversationEventsPage builds the final JSON response incrementally.
// A page may contain fewer than limit events when the next event would exceed
// responseBytes; the cursor still advances for every successful page.
func (store *businessStore) listAIConversationEventsPage(ctx context.Context, projectID uuid.UUID, conversationID string, afterSequence uint64, limit, responseBytes int) (aiConversationEventPage, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || limit < 1 || limit > maximumAIEventPage || responseBytes < 1 || responseBytes > maximumRPCPayload {
		return aiConversationEventPage{}, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return aiConversationEventPage{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return aiConversationEventPage{}, err
	}
	defer tx.Rollback()
	if _, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID); err != nil {
		return aiConversationEventPage{}, err
	} else if !found {
		return aiConversationEventPage{}, errRPCNotFound
	}
	var minimum, latest uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(sequence),0),COALESCE(MAX(sequence),0)
        FROM ai_conversation_events WHERE conversation_id=?`, conversationID).Scan(&minimum, &latest); err != nil {
		return aiConversationEventPage{}, err
	}
	page := aiConversationEventPage{
		Items:                     make([]aiConversationEvent, 0, limit),
		NextSequence:              afterSequence,
		HighWatermark:             latest,
		EarliestAvailableSequence: minimum,
		StopReason:                "watermark",
	}
	if afterSequence > latest || minimum > 1 && afterSequence < minimum-1 {
		page.ResetRequired = true
		page.StopReason = "cursor_expired"
		if _, err := page.measureResponseBytes(); err != nil {
			return aiConversationEventPage{}, err
		}
		if page.PageBytes > responseBytes {
			return aiConversationEventPage{}, errRPCResponsePageTooLarge
		}
		return page, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,event_id,generation_id,message_id,kind,payload_json,occurred_at_ms
        FROM ai_conversation_events WHERE conversation_id=? AND sequence>? ORDER BY sequence LIMIT ?`, conversationID, afterSequence, limit+1)
	if err != nil {
		return aiConversationEventPage{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var event aiConversationEvent
		var payload string
		var occurredAtMilliseconds int64
		if err := rows.Scan(&event.Sequence, &event.EventID, &event.GenerationID, &event.MessageID, &event.Kind, &payload, &occurredAtMilliseconds); err != nil ||
			json.Unmarshal([]byte(payload), &event.Payload) != nil || occurredAtMilliseconds <= 0 || !validAIConversationEventKind(event.Kind) {
			return aiConversationEventPage{}, errors.New("stored AI conversation event is invalid")
		}
		event.ConversationID = conversationID
		event.OccurredAt = time.UnixMilli(occurredAtMilliseconds).UTC()
		if len(page.Items) == limit {
			page.HasMore = true
			page.StopReason = "item_limit"
			break
		}
		candidate := append(page.Items, event)
		candidatePage := page
		candidatePage.Items = candidate
		candidatePage.NextSequence = event.Sequence
		// Test the larger boolean representation. If it fits with hasMore=true,
		// the final no-more page also fits.
		candidatePage.HasMore = true
		if bytes, err := candidatePage.measureResponseBytes(); err != nil {
			return aiConversationEventPage{}, err
		} else if bytes > responseBytes {
			if len(page.Items) == 0 {
				return aiConversationEventPage{}, errRPCEventItemTooLarge
			}
			page.HasMore = true
			page.StopReason = "byte_budget"
			break
		}
		page.Items = candidate
		page.NextSequence = event.Sequence
	}
	if err := rows.Err(); err != nil {
		return aiConversationEventPage{}, err
	}
	if !page.HasMore && page.NextSequence < latest {
		// The SQL look-ahead may end exactly at the requested limit; a fixed
		// query-start watermark makes this a safe, content-free continuation.
		page.HasMore = true
		page.StopReason = "watermark"
	}
	if len(page.Items) > 0 && page.NextSequence <= afterSequence {
		return aiConversationEventPage{}, errRPCInvalid
	}
	if _, err := page.measureResponseBytes(); err != nil {
		return aiConversationEventPage{}, err
	}
	if page.PageBytes > responseBytes {
		return aiConversationEventPage{}, errRPCResponsePageTooLarge
	}
	return page, nil
}

func (store *businessStore) activeAIConversationGeneration(ctx context.Context, projectID uuid.UUID, conversationID string) (conversationView, string, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil {
		return conversationView{}, "", errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return conversationView{}, "", err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return conversationView{}, "", err
	}
	defer tx.Rollback()
	value, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return conversationView{}, "", err
	}
	if !found {
		return conversationView{}, "", errRPCNotFound
	}
	var assistantID string
	if err := tx.QueryRowContext(ctx, `SELECT active_assistant_id FROM ai_conversations WHERE id=?`, conversationID).Scan(&assistantID); err != nil {
		return conversationView{}, "", err
	}
	return value, assistantID, nil
}

func (store *businessStore) searchAIConversations(ctx context.Context, projectID uuid.UUID, query string, offset, limit int) ([]aiConversationSearchResult, int, error) {
	query = strings.TrimSpace(query)
	if store == nil || projectID == uuid.Nil || query == "" || len(query) > 200 || !utf8.ValidString(query) || offset < 0 || limit < 1 || limit > maximumAIConversationPage {
		return nil, 0, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	needle := strings.ToLower(query)
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_conversations c WHERE c.device_id=? AND c.project_id=? AND (
        instr(lower(c.title),?)>0 OR EXISTS(SELECT 1 FROM ai_messages m WHERE m.conversation_id=c.id AND instr(lower(m.content),?)>0)
    )`, store.deviceID.String(), projectID.String(), needle, needle).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.project_id,c.revision,c.title,c.config_id,c.model_binding_json,
        c.workspace_mode,c.last_message_sequence,c.message_count,c.created_at_ms,c.updated_at_ms,c.state,c.generation_id,
        COALESCE((SELECT substr(m.content,1,240) FROM ai_messages m WHERE m.conversation_id=c.id AND instr(lower(m.content),?)>0 ORDER BY m.sequence DESC LIMIT 1),'')
        FROM ai_conversations c WHERE c.device_id=? AND c.project_id=? AND (
            instr(lower(c.title),?)>0 OR EXISTS(SELECT 1 FROM ai_messages m WHERE m.conversation_id=c.id AND instr(lower(m.content),?)>0)
        ) ORDER BY c.updated_at_ms DESC,c.id LIMIT ? OFFSET ?`, needle, store.deviceID.String(), projectID.String(), needle, needle, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	results := make([]aiConversationSearchResult, 0, limit)
	for rows.Next() {
		var value conversationView
		var bindingJSON, state, generationID, snippet string
		var createdAtMilliseconds, updatedAtMilliseconds int64
		if err := rows.Scan(&value.ID, &value.ProjectID, &value.Revision, &value.Title, &value.ConfigID, &bindingJSON,
			&value.WorkspaceMode, &value.LastMessageSequence, &value.MessageCount, &createdAtMilliseconds, &updatedAtMilliseconds,
			&state, &generationID, &snippet); err != nil || json.Unmarshal([]byte(bindingJSON), &value.ModelBinding) != nil {
			return nil, 0, errors.New("stored AI conversation search result is invalid")
		}
		value.CreatedAt, value.UpdatedAt = time.UnixMilli(createdAtMilliseconds).UTC(), time.UnixMilli(updatedAtMilliseconds).UTC()
		value.State, value.GenerationID, value.Model = state, generationID, value.ModelBinding.Model
		value.WorkspaceMode = aiWorkspaceModeFromStorage(value.WorkspaceMode)
		if validateAIConversationView(value) != nil {
			return nil, 0, errors.New("stored AI conversation search result is invalid")
		}
		if snippet == "" {
			snippet = value.Title
		}
		results = append(results, aiConversationSearchResult{Conversation: value, Snippet: snippet})
	}
	return results, total, rows.Err()
}

func (store *businessStore) saveAIContextSummary(ctx context.Context, projectID uuid.UUID, summary aiContextSummary) error {
	if store == nil || projectID == uuid.Nil || uuid.Validate(summary.ConversationID) != nil || summary.ThroughSequence == 0 ||
		strings.TrimSpace(summary.Content) == "" || len(summary.Content) > 32768 || summary.EstimatedTokens == 0 || summary.UpdatedAt.IsZero() {
		return errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.ExecContext(ctx, `INSERT INTO ai_context_summaries(conversation_id,through_sequence,content,estimated_tokens,updated_at_ms)
        SELECT id,?,?,?,? FROM ai_conversations WHERE id=? AND device_id=? AND project_id=?
        ON CONFLICT(conversation_id) DO UPDATE SET through_sequence=excluded.through_sequence,content=excluded.content,
            estimated_tokens=excluded.estimated_tokens,updated_at_ms=excluded.updated_at_ms
        WHERE excluded.through_sequence>=ai_context_summaries.through_sequence`, summary.ThroughSequence, summary.Content,
		summary.EstimatedTokens, summary.UpdatedAt.UTC().UnixMilli(), summary.ConversationID, store.deviceID.String(), projectID.String())
	if err != nil {
		return fmt.Errorf("save AI context summary: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return errRPCNotFound
	}
	return nil
}

func (store *businessStore) loadAIContextSummary(ctx context.Context, projectID uuid.UUID, conversationID string) (*aiContextSummary, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil {
		return nil, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var value aiContextSummary
	var updatedAtMilliseconds int64
	err = db.QueryRowContext(ctx, `SELECT s.conversation_id,s.through_sequence,s.content,s.estimated_tokens,s.updated_at_ms
        FROM ai_context_summaries s JOIN ai_conversations c ON c.id=s.conversation_id
        WHERE s.conversation_id=? AND c.device_id=? AND c.project_id=?`, conversationID, store.deviceID.String(), projectID.String()).
		Scan(&value.ConversationID, &value.ThroughSequence, &value.Content, &value.EstimatedTokens, &updatedAtMilliseconds)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	value.UpdatedAt = time.UnixMilli(updatedAtMilliseconds).UTC()
	return &value, nil
}

func legacyAIConversationConfig(configs map[string]aiConfig, model string) aiConfig {
	model = strings.TrimSpace(model)
	var selected *aiConfig
	for _, candidate := range configs {
		if model != "" && candidate.Model != model {
			continue
		}
		copy := candidate
		if selected == nil || copy.ID == "default" || selected.ID != "default" && copy.ID < selected.ID {
			selected = &copy
		}
	}
	if selected != nil {
		return *selected
	}
	if model == "" {
		model = "legacy-unbound"
	}
	return aiConfig{ID: "legacy-unbound", Revision: 1, Provider: "openai-compatible", Model: model}
}

func (store *businessStore) migrateLegacyAIConversations(ctx context.Context, projectID uuid.UUID, legacy map[string]conversation, configs map[string]aiConfig) error {
	if store == nil || projectID == uuid.Nil || len(legacy) == 0 {
		return nil
	}
	ids := make([]string, 0, len(legacy))
	for id := range legacy {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		old := legacy[id]
		if old.ID == "" {
			old.ID = id
		}
		if old.ID != id || validateLegacyConversation(old) != nil {
			return errors.New("legacy AI conversation is invalid")
		}
		config := legacyAIConversationConfig(configs, old.Model)
		if err := store.importLegacyAIConversation(ctx, projectID, old, config); err != nil {
			return err
		}
	}
	return nil
}

func normalizedLegacyAIMessages(old conversation) ([]chatMessage, uint64, time.Time, time.Time, error) {
	createdAt, updatedAt := old.UpdatedAt.UTC(), old.UpdatedAt.UTC()
	messages := make([]chatMessage, 0, len(old.Messages))
	lastSequence := uint64(0)
	for index, source := range old.Messages {
		message := source
		if message.Sequence == 0 {
			message.Sequence = uint64(index + 1)
		}
		message.Revision = 1
		message.Attachments = []chatAttachmentReference{}
		message.ToolRuns = []chatToolRun{}
		message.GenerationID = ""
		message.CreatedAt = message.CreatedAt.UTC().Truncate(time.Millisecond)
		switch message.Status {
		case "failed":
		case "streaming", "stopped", "cancelled":
			message.Status, message.ErrorCode = "stopped", "legacy_interrupted"
		default:
			message.Status = "complete"
		}
		if validateStoredAIMessage(message) != nil {
			return nil, 0, time.Time{}, time.Time{}, errors.New("legacy AI message is invalid")
		}
		createdAt = minTime(createdAt, message.CreatedAt)
		updatedAt = maxTime(updatedAt, message.CreatedAt)
		lastSequence = max(lastSequence, message.Sequence)
		messages = append(messages, message)
	}
	slices.SortFunc(messages, func(left, right chatMessage) int {
		return cmp.Compare(left.Sequence, right.Sequence)
	})
	return messages, lastSequence, createdAt.Truncate(time.Millisecond), updatedAt.Truncate(time.Millisecond), nil
}

func minTime(left, right time.Time) time.Time {
	if right.Before(left) {
		return right
	}
	return left
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func sameImportedLegacyAIMessage(stored, expected chatMessage) bool {
	return stored.ID == expected.ID && stored.Revision == expected.Revision && stored.Sequence == expected.Sequence &&
		stored.Role == expected.Role && stored.Content == expected.Content && stored.Status == expected.Status &&
		stored.ErrorCode == expected.ErrorCode && stored.Reasoning == expected.Reasoning &&
		len(stored.Attachments) == 0 && len(stored.ToolRuns) == 0 && stored.Usage == expected.Usage &&
		stored.ProviderRun == expected.ProviderRun && stored.GenerationID == "" && stored.CreatedAt.Equal(expected.CreatedAt)
}

func storedLegacyAIMessagesMatch(ctx context.Context, tx *sql.Tx, conversationID string, expected []chatMessage) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,revision,sequence,role,content,status,error_code,attachments_json,
        reasoning,tool_runs_json,usage_json,provider_run_json,generation_id,created_at_ms FROM ai_messages
        WHERE conversation_id=? ORDER BY sequence`, conversationID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		message, err := scanAIMessage(rows)
		if err != nil {
			return false, err
		}
		if index >= len(expected) || !sameImportedLegacyAIMessage(message, expected[index]) {
			return false, nil
		}
		index++
	}
	return index == len(expected), rows.Err()
}

func (store *businessStore) importLegacyAIConversation(ctx context.Context, projectID uuid.UUID, old conversation, config aiConfig) error {
	messages, lastSequence, createdAt, updatedAt, err := normalizedLegacyAIMessages(old)
	if err != nil {
		return err
	}
	binding := aiConversationBinding(config)
	bindingJSON, err := marshalAIJSON(binding, 16<<10)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var projectExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=? AND device_id=? AND state='available'`, projectID.String(), store.deviceID.String()).Scan(&projectExists); err != nil || projectExists != 1 {
		return firstError(err, errRPCProject)
	}
	value, found, err := queryAIConversation(ctx, tx, store.deviceID.String(), projectID.String(), old.ID)
	if err != nil {
		return err
	}
	if found {
		matches, err := storedLegacyAIMessagesMatch(ctx, tx, old.ID, messages)
		if err != nil {
			return err
		}
		metadataMatches := value.Title == strings.TrimSpace(old.Title) && value.ConfigID == config.ID &&
			value.ModelBinding == binding && value.WorkspaceMode == "readOnly" && value.State == "idle" && value.GenerationID == ""
		if metadataMatches && value.MessageCount == len(messages) && value.LastMessageSequence == lastSequence && matches {
			return nil
		}
		if !metadataMatches || value.MessageCount != 0 || value.LastMessageSequence != 0 {
			return errors.New("legacy AI conversation conflicts with stored conversation")
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ai_conversations(
            id,device_id,project_id,revision,title,config_id,model_binding_json,workspace_mode,state,
            generation_id,active_assistant_id,last_message_sequence,message_count,created_at_ms,updated_at_ms
        ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, old.ID, store.deviceID.String(), projectID.String(), 1,
			strings.TrimSpace(old.Title), config.ID, bindingJSON, "readOnly", "idle", "", "", 0, 0,
			createdAt.UnixMilli(), createdAt.UnixMilli()); err != nil {
			return fmt.Errorf("create legacy AI conversation: %w", err)
		}
		value = conversationView{
			ID: old.ID, ProjectID: projectID.String(), Revision: 1, Title: strings.TrimSpace(old.Title), ConfigID: config.ID,
			ModelBinding: binding, Model: binding.Model, WorkspaceMode: "readOnly", CreatedAt: createdAt, UpdatedAt: createdAt, State: "idle",
		}
		value, err = appendAIConversationChange(ctx, store, tx, value, false, createdAt)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET revision=? WHERE id=? AND revision=1`, value.Revision, old.ID); err != nil {
			return err
		}
	}
	for _, message := range messages {
		if err := insertAIMessage(ctx, tx, old.ID, message, updatedAt); err != nil {
			return err
		}
		if message.Role == "user" || message.Role == "assistant" {
			value.Catalog = withAIConversationCatalogMessage(value.Catalog, message)
		}
	}
	previousRevision := value.Revision
	value.LastMessageSequence, value.MessageCount, value.UpdatedAt = lastSequence, len(messages), updatedAt
	value, err = appendAIConversationChange(ctx, store, tx, value, false, updatedAt)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_conversations SET revision=?,last_message_sequence=?,message_count=?,updated_at_ms=?,
		latest_message_sequence=?,latest_message_role=?,latest_message_status=?,latest_message_preview=?,
		latest_message_created_at_ms=?,last_completed_assistant_sequence=?,last_error_code=?
		WHERE id=? AND device_id=? AND project_id=? AND revision=?`, value.Revision, lastSequence, len(messages),
		updatedAt.UnixMilli(), value.Catalog.LatestMessageSequence, value.Catalog.LatestMessageRole,
		value.Catalog.LatestMessageStatus, value.Catalog.LatestMessagePreview,
		catalogProjectionCreatedAtMilliseconds(value.Catalog), value.Catalog.LastCompletedAssistantSequence,
		value.Catalog.LastErrorCode, old.ID, store.deviceID.String(), projectID.String(), previousRevision)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return errRPCRevision
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return err
	}
	return nil
}

func (store *businessStore) recoverInterruptedAIConversations(ctx context.Context, now time.Time) (int, error) {
	if store == nil || now.IsZero() {
		return 0, errRPCInvalid
	}
	store.mu.Lock()
	db, err := store.openDB()
	if err != nil {
		store.mu.Unlock()
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT project_id,id,generation_id,active_assistant_id FROM ai_conversations
        WHERE device_id=? AND state='generating'`, store.deviceID.String())
	if err != nil {
		_ = db.Close()
		store.mu.Unlock()
		return 0, err
	}
	type interrupted struct{ projectID, conversationID, generationID, assistantID string }
	items := make([]interrupted, 0)
	for rows.Next() {
		var item interrupted
		if err := rows.Scan(&item.projectID, &item.conversationID, &item.generationID, &item.assistantID); err != nil {
			_ = rows.Close()
			_ = db.Close()
			store.mu.Unlock()
			return 0, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	_ = rows.Close()
	_ = db.Close()
	store.mu.Unlock()
	if err != nil {
		return 0, err
	}
	recovered := 0
	for _, item := range items {
		projectID, projectErr := uuid.Parse(item.projectID)
		if projectErr != nil {
			return recovered, errors.New("stored AI generation project is invalid")
		}
		_, _, _, abortErr := store.abortAIConversationTurn(ctx, projectID, item.conversationID, item.generationID,
			item.assistantID, "stopped", "agent_restarted", chatProviderRun{FinishReason: "cancelled"}, now)
		if abortErr != nil && !errors.Is(abortErr, errRPCRevision) {
			return recovered, abortErr
		}
		if abortErr == nil {
			recovered++
		}
	}
	if _, err := store.recoverInterruptedAISubagents(ctx, now); err != nil {
		return recovered, err
	}
	return recovered, nil
}

// aiToolResultArtifactRetention bounds how long spilled tool-result artifacts
// stay on disk. The sweep runs opportunistically on each save, so artifact
// growth stays bounded even when conversations are never deleted.
const aiToolResultArtifactRetention = 24 * time.Hour

// saveAIToolResultArtifact stores an oversized tool result outside the model
// context and returns its artifact id. The caller keeps the full plaintext out
// of the conversation history; the artifact stays conversation-scoped.
func (store *businessStore) saveAIToolResultArtifact(ctx context.Context, conversationID, toolCallID string, content []byte) (string, error) {
	if store == nil || uuid.Validate(conversationID) != nil || toolCallID == "" || len(toolCallID) > 256 || len(content) == 0 {
		return "", errRPCInvalid
	}
	artifactID := uuid.NewString()
	now := time.Now().UTC()
	stored := content
	if len(stored) > maximumAIToolResultArtifactBytes {
		stored = stored[:maximumAIToolResultArtifactBytes]
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_tool_result_artifact WHERE created_at_ms < ?`,
		now.Add(-aiToolResultArtifactRetention).UnixMilli()); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_tool_result_artifact(
            artifact_id,conversation_id,tool_call_id,total_bytes,content,created_at_ms
        ) VALUES(?,?,?,?,?,?)`, artifactID, conversationID, toolCallID, len(content), stored, now.UnixMilli()); err != nil {
		return "", err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return "", err
	}
	return artifactID, nil
}

// readAIToolResultArtifact loads a spilled artifact's stored content and its
// original total size. The artifact is only reachable inside the owning
// conversation, so cross-conversation reads fail closed.
func (store *businessStore) readAIToolResultArtifact(ctx context.Context, conversationID, artifactID string) ([]byte, int64, error) {
	if store == nil || uuid.Validate(conversationID) != nil || uuid.Validate(artifactID) != nil {
		return nil, 0, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()
	var content []byte
	var totalBytes int64
	err = db.QueryRowContext(ctx, `SELECT content,total_bytes FROM ai_tool_result_artifact WHERE artifact_id=? AND conversation_id=?`,
		artifactID, conversationID).Scan(&content, &totalBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, errRPCNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return content, totalBytes, nil
}
