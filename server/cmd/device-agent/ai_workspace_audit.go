package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type aiToolAuditRecord struct {
	ID               string
	ProjectID        uuid.UUID
	ConversationID   string
	GenerationID     string
	ToolCallIDSHA256 string
	ToolName         string
	ArgumentsSHA256  string
	PreviewSHA256    string
	ResultSHA256     string
	Outcome          string
	ErrorCode        string
	ApprovalDecision string
	AllowForSession  bool
	StartedAt        time.Time
	FinishedAt       *time.Time
}

func (store *businessStore) recordAIToolAudit(ctx context.Context, record aiToolAuditRecord) error {
	if store == nil || !validAIToolAuditRecord(record) {
		return errors.New("AI tool audit is invalid")
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
	var existing aiToolAuditRecord
	var existingProjectID string
	var allowForSession int64
	var startedAtMilliseconds int64
	var finished sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,
        arguments_sha256,preview_sha256,result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
        FROM ai_tool_audit WHERE id=?`, record.ID).Scan(
		&existingProjectID, &existing.ConversationID, &existing.GenerationID, &existing.ToolCallIDSHA256, &existing.ToolName,
		&existing.ArgumentsSHA256, &existing.PreviewSHA256, &existing.ResultSHA256, &existing.Outcome, &existing.ErrorCode,
		&existing.ApprovalDecision, &allowForSession, &startedAtMilliseconds, &finished,
	)
	if errors.Is(err, sql.ErrNoRows) {
		var finishedAt any
		if record.FinishedAt != nil {
			finishedAt = record.FinishedAt.UTC().UnixMilli()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ai_tool_audit(
            id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
        ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.ID, record.ProjectID.String(), record.ConversationID, record.GenerationID,
			record.ToolCallIDSHA256, record.ToolName, record.ArgumentsSHA256, record.PreviewSHA256, record.ResultSHA256,
			record.Outcome, record.ErrorCode, record.ApprovalDecision, boolInteger(record.AllowForSession), record.StartedAt.UTC().UnixMilli(), finishedAt)
		if err != nil {
			return fmt.Errorf("insert AI tool audit: %w", err)
		}
		return commitBusinessTransaction(ctx, tx)
	}
	if err != nil {
		return fmt.Errorf("read AI tool audit: %w", err)
	}
	// The immutable identity and digest columns prevent an approval response or
	// replayed provider tool-call ID from being rebound to different plaintext.
	if existingProjectID != record.ProjectID.String() || existing.ConversationID != record.ConversationID ||
		existing.GenerationID != record.GenerationID || existing.ToolCallIDSHA256 != record.ToolCallIDSHA256 ||
		existing.ToolName != record.ToolName || existing.ArgumentsSHA256 != record.ArgumentsSHA256 || existing.PreviewSHA256 != record.PreviewSHA256 {
		return errRPCRevision
	}
	if startedAtMilliseconds != record.StartedAt.UTC().UnixMilli() {
		return errRPCRevision
	}
	if finished.Valid {
		// A byte-for-byte terminal replay is idempotent; terminal outcomes cannot
		// be rewritten to make an already denied tool appear successful.
		if existing.ResultSHA256 == record.ResultSHA256 && existing.Outcome == record.Outcome &&
			existing.ErrorCode == record.ErrorCode && existing.ApprovalDecision == record.ApprovalDecision &&
			(allowForSession != 0) == record.AllowForSession && record.FinishedAt != nil && finished.Int64 == record.FinishedAt.UTC().UnixMilli() {
			return commitBusinessTransaction(ctx, tx)
		}
		return errRPCRevision
	}
	var finishedAt any
	if record.FinishedAt != nil {
		finishedAt = record.FinishedAt.UTC().UnixMilli()
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_tool_audit SET result_sha256=?,outcome=?,error_code=?,approval_decision=?,
        allow_for_session=?,finished_at_ms=? WHERE id=? AND finished_at_ms IS NULL`, record.ResultSHA256, record.Outcome,
		record.ErrorCode, record.ApprovalDecision, boolInteger(record.AllowForSession), finishedAt, record.ID)
	if err != nil {
		return fmt.Errorf("update AI tool audit: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return errRPCRevision
	}
	return commitBusinessTransaction(ctx, tx)
}

func validAIToolAuditRecord(record aiToolAuditRecord) bool {
	if uuid.Validate(record.ID) != nil || record.ProjectID == uuid.Nil || uuid.Validate(record.ConversationID) != nil ||
		uuid.Validate(record.GenerationID) != nil || !validAIWorkspaceToolName(record.ToolName) ||
		!validAIWorkspaceDigest(record.ToolCallIDSHA256) || !validAIWorkspaceDigest(record.ArgumentsSHA256) ||
		!validAIWorkspaceDigest(record.PreviewSHA256) || record.StartedAt.IsZero() || len(record.ErrorCode) > 80 {
		return false
	}
	if record.ResultSHA256 != "" && !validAIWorkspaceDigest(record.ResultSHA256) {
		return false
	}
	if !strings.Contains("|awaiting_approval|running|succeeded|failed|denied|cancelled|", "|"+record.Outcome+"|") ||
		!strings.Contains("||allow_once|allow_for_session|deny|timeout|cancelled|", "|"+record.ApprovalDecision+"|") {
		return false
	}
	terminal := record.Outcome == "succeeded" || record.Outcome == "failed" || record.Outcome == "denied" || record.Outcome == "cancelled"
	return terminal == (record.FinishedAt != nil) && (record.FinishedAt == nil || !record.FinishedAt.Before(record.StartedAt))
}

func validAIWorkspaceToolName(value string) bool {
	for _, name := range []string{
		"list_files", "search_files", "read_file", "read_tool_result", "read_image", "web_search", "web_fetch", "write_file", "replace_in_file", "rollback_file_change", "run_command",
		"terminal_open", "terminal_send", "terminal_read", "terminal_signal", "terminal_close", "terminal_list",
	} {
		if value == name {
			return true
		}
	}
	return strings.HasPrefix(value, "mcp__") && validAIProviderToolName(value)
}

func validAIWorkspaceDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}
