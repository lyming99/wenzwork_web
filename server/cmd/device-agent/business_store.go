package main

// The Agent identity file intentionally remains a small, independently
// protected JSON document.  Business data belongs in this SQLite store so
// projects, tasks and conversations can evolve through migrations without
// mixing credentials or device identity into application records.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const businessSchemaVersion = 26
const localListReadBudget = 750 * time.Millisecond

type projectPolicy struct {
	AllowInteractiveTerminal bool `json:"allowInteractiveTerminal"`
	AllowTaskExecution       bool `json:"allowTaskExecution"`
	AllowAIWorkspaceTools    bool `json:"allowAIWorkspaceTools"`
	AllowRemoteCreate        bool `json:"allowRemoteCreate"`
	AllowRecursiveDelete     bool `json:"allowRecursiveDelete"`
}

// The device is explicitly enrolled by its owner. Its workspace is therefore
// usable as a full local project out of the box; callers that need a narrower
// policy can still set one through the local project CLI.
var defaultProjectPolicy = projectPolicy{
	AllowInteractiveTerminal: true,
	AllowTaskExecution:       true,
	AllowAIWorkspaceTools:    true,
	AllowRemoteCreate:        true,
	AllowRecursiveDelete:     true,
}

// registeredProject contains the device-only local path.  Never marshal this
// type directly into a Peer RPC response or a control-plane projection.
type registeredProject struct {
	ID                 uuid.UUID
	DisplayName        string
	LocalPath          string
	GitURL             string
	State              string
	Revision           uint64
	Policy             projectPolicy
	LegacyRelativePath string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type businessStore struct {
	mu                 sync.Mutex
	taskLogMigrationMu sync.Mutex
	path               string
	deviceID           uuid.UUID
	agentEventWakeMu   sync.RWMutex
	agentEventWake     func()

	compatibilityMetricsMu       sync.Mutex
	pendingCompatibilityMetrics  map[compatibilityMetricKey]compatibilityMetricWrite
	compatibilityMetricsFlushing bool
	compatibilityMetricsDone     chan struct{}
	operationMaintenanceStop     chan struct{}
	operationMaintenanceDone     chan struct{}
	operationMaintenanceOnce     sync.Once
	operationJournalSaveHook     func() error
}

func (store *businessStore) setAgentEventWake(wake func()) {
	if store == nil {
		return
	}
	store.agentEventWakeMu.Lock()
	store.agentEventWake = wake
	store.agentEventWakeMu.Unlock()
}

func (store *businessStore) wakeAgentEvents() {
	if store == nil {
		return
	}
	store.agentEventWakeMu.RLock()
	wake := store.agentEventWake
	store.agentEventWakeMu.RUnlock()
	if wake != nil {
		wake()
	}
}

type terminalSessionAudit struct {
	SessionID   uuid.UUID
	ProjectID   uuid.UUID
	OpenedAt    time.Time
	ClosedAt    time.Time
	InputBytes  uint64
	OutputBytes uint64
	ExitCode    int
	CloseReason string
}

func openBusinessStore(state *agentState) (*businessStore, error) {
	if state == nil || state.DeviceID == uuid.Nil || strings.TrimSpace(state.path) == "" {
		return nil, errors.New("business store identity is invalid")
	}
	path := state.path + ".business.sqlite"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create business store directory: %w", err)
	}
	// DELETE journaling avoids leaving a second plaintext WAL file with a
	// different ACL behind on platforms where service identities change.
	store := &businessStore{path: path, deviceID: state.DeviceID}
	if err := store.migrate(context.Background()); err != nil {
		return nil, err
	}
	if err := secureStateFile(path); err != nil {
		return nil, fmt.Errorf("protect business store: %w", err)
	}
	if err := store.ensureLegacyWorkspace(context.Background(), state); err != nil {
		return nil, err
	}
	store.startV2OperationMaintenance()
	return store, nil
}

func (store *businessStore) close() error {
	if store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store.stopV2OperationMaintenance(ctx)
	_ = store.waitForCompatibilityMetrics(ctx)
	return nil
}

// SQLite handles are intentionally short-lived. The Agent holds a durable
// store descriptor, not an open Windows file handle, so abrupt process exits
// and callers that embed the Agent (including CLI/test use) cannot strand a
// locked database file.
func (store *businessStore) openDB() (*sql.DB, error) {
	return store.openDBWithPragmas("busy_timeout(5000)&_pragma=journal_mode(DELETE)&_pragma=foreign_keys(ON)")
}

// openReadDB is intentionally bounded well below the UI loading budget. A
// snapshot/list/replay RPC must not inherit the five-second write retry budget:
// if a local mutation holds SQLite briefly, the caller can retain its last
// good projection and retry instead of showing an apparently stuck spinner.
// `query_only` also prevents an accidental read-path write from this handle.
func (store *businessStore) openReadDB() (*sql.DB, error) {
	return store.openDBWithPragmas("busy_timeout(750)&_pragma=foreign_keys(ON)&_pragma=query_only(ON)")
}

func (store *businessStore) openDBWithPragmas(pragmas string) (*sql.DB, error) {
	if store == nil || store.path == "" || store.deviceID == uuid.Nil {
		return nil, errors.New("business store is unavailable")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(store.path)+"?_pragma="+pragmas)
	if err != nil {
		return nil, fmt.Errorf("open business store: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (store *businessStore) migrate(ctx context.Context) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin business store migration: %w", err)
	}
	fail := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
            version INTEGER PRIMARY KEY,
            applied_at_ms INTEGER NOT NULL
        )`); err != nil {
		return fail(fmt.Errorf("create business migrations: %w", err))
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fail(fmt.Errorf("read business schema version: %w", err))
	}
	if current > businessSchemaVersion {
		return fail(errors.New("business store schema is newer than this Agent"))
	}
	if current < 1 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE projects (
                id TEXT PRIMARY KEY,
                device_id TEXT NOT NULL,
                display_name TEXT NOT NULL,
                local_path TEXT NOT NULL UNIQUE,
                git_url TEXT NOT NULL DEFAULT '',
                state TEXT NOT NULL,
                revision INTEGER NOT NULL,
                allow_interactive_terminal INTEGER NOT NULL DEFAULT 0,
                allow_remote_create INTEGER NOT NULL DEFAULT 0,
                legacy_relative_path TEXT NOT NULL DEFAULT '',
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL,
                CHECK(state IN ('available', 'unavailable', 'removed')),
                CHECK(revision > 0)
            )`); err != nil {
			return fail(fmt.Errorf("create project registry: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX projects_device_state_idx ON projects(device_id, state, updated_at_ms)`); err != nil {
			return fail(fmt.Errorf("create project registry index: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE project_changes (
                sequence INTEGER PRIMARY KEY AUTOINCREMENT,
                project_id TEXT NOT NULL,
                revision INTEGER NOT NULL,
                operation TEXT NOT NULL,
                occurred_at_ms INTEGER NOT NULL,
                CHECK(operation IN ('upsert', 'tombstone'))
            )`); err != nil {
			return fail(fmt.Errorf("create project change log: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(1, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 1
	}
	if current < 2 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE projects ADD COLUMN allow_recursive_delete INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fail(fmt.Errorf("add recursive-delete policy: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(2, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 2
	}
	if current < 3 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE terminal_session_audit (
                session_id TEXT PRIMARY KEY,
                project_id TEXT NOT NULL,
                opened_at_ms INTEGER NOT NULL,
                closed_at_ms INTEGER,
                input_bytes INTEGER NOT NULL DEFAULT 0,
                output_bytes INTEGER NOT NULL DEFAULT 0,
                exit_code INTEGER,
                close_reason TEXT NOT NULL DEFAULT '',
                CHECK(input_bytes >= 0),
                CHECK(output_bytes >= 0)
            )`); err != nil {
			return fail(fmt.Errorf("create terminal session audit: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX terminal_session_audit_project_time_idx ON terminal_session_audit(project_id, opened_at_ms)`); err != nil {
			return fail(fmt.Errorf("create terminal session audit index: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(3, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 3
	}
	if current < 4 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE projects ADD COLUMN allow_task_execution INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fail(fmt.Errorf("add task-execution policy: %w", err))
		}
		if err := migrateTaskSchemaV4(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(4, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 4
	}
	if current < 5 {
		if err := migrateTaskSchemaV5(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(5, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 5
	}
	if current < 6 {
		if err := migrateAIConfigSchemaV6(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(6, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 6
	}
	if current < 7 {
		if err := migrateAIConversationSchemaV7(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(7, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 7
	}
	if current < 8 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE projects ADD COLUMN allow_ai_workspace_tools INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fail(fmt.Errorf("add AI workspace-tools policy: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE ai_tool_audit (
                id TEXT PRIMARY KEY,
                project_id TEXT NOT NULL,
                conversation_id TEXT NOT NULL,
                generation_id TEXT NOT NULL,
                tool_call_id_sha256 TEXT NOT NULL,
                tool_name TEXT NOT NULL,
                arguments_sha256 TEXT NOT NULL,
                preview_sha256 TEXT NOT NULL,
                result_sha256 TEXT NOT NULL DEFAULT '',
                outcome TEXT NOT NULL,
                error_code TEXT NOT NULL DEFAULT '',
                approval_decision TEXT NOT NULL DEFAULT '',
                allow_for_session INTEGER NOT NULL DEFAULT 0,
                started_at_ms INTEGER NOT NULL,
                finished_at_ms INTEGER,
                CHECK(tool_name IN ('list_files','search_files','read_file','write_file','replace_in_file','rollback_file_change','run_command','terminal_open','terminal_send','terminal_read','terminal_signal','terminal_close','terminal_list')),
                CHECK(outcome IN ('awaiting_approval','running','succeeded','failed','denied','cancelled')),
                CHECK(approval_decision IN ('','allow_once','allow_for_session','deny','timeout','cancelled'))
            )`); err != nil {
			return fail(fmt.Errorf("create AI tool audit: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX ai_tool_audit_project_time_idx ON ai_tool_audit(project_id, started_at_ms)`); err != nil {
			return fail(fmt.Errorf("create AI tool audit index: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(8, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 8
	}
	if current < 9 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE rpc_compatibility_metrics (
                capability_version TEXT NOT NULL,
                error_code TEXT NOT NULL,
                call_count INTEGER NOT NULL,
                total_duration_ms INTEGER NOT NULL,
                first_observed_at_ms INTEGER NOT NULL,
                last_observed_at_ms INTEGER NOT NULL,
                PRIMARY KEY(capability_version, error_code),
                CHECK(capability_version IN ('files.v1','files.v2','terminal.v1','terminal.v2','tasks.v1','tasks.v2','ai.v1','ai.v2')),
                CHECK(length(error_code) BETWEEN 1 AND 80),
                CHECK(call_count > 0),
                CHECK(total_duration_ms >= 0),
                CHECK(last_observed_at_ms >= first_observed_at_ms)
            )`); err != nil {
			return fail(fmt.Errorf("create RPC compatibility metrics: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(9, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
	}
	if current < 10 {
		// First-screen task lists order by project and creation time, while each
		// projected row reads its latest change sequence. These indexes make that
		// bounded page query independent of the total task/change history.
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS tasks_project_created_idx ON tasks(project_id, created_at_ms DESC, id)`); err != nil {
			return fail(fmt.Errorf("create task list index: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS task_changes_task_sequence_idx ON task_changes(task_id, sequence DESC)`); err != nil {
			return fail(fmt.Errorf("create task change lookup index: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(10, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 10
	}
	if current < 11 {
		// Versions through v10 registered the default workspace with every
		// remote capability disabled. The owner has already explicitly enrolled
		// this device, so migrate those historical defaults to the current
		// full-access baseline once. Future local policy edits remain intact.
		if _, err := tx.ExecContext(ctx, `UPDATE projects
			SET allow_interactive_terminal = 1, allow_task_execution = 1, allow_ai_workspace_tools = 1,
				allow_remote_create = 1, allow_recursive_delete = 1
			WHERE allow_interactive_terminal = 0 OR allow_task_execution = 0 OR allow_ai_workspace_tools = 0 OR
				allow_remote_create = 0 OR allow_recursive_delete = 0`); err != nil {
			return fail(fmt.Errorf("enable default project capabilities: %w", err))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(11, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record business migration: %w", err))
		}
		current = 11
	}
	if current < 12 {
		if err := migrateAgentEventSchemaV12(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(12, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record Agent event migration: %w", err))
		}
		current = 12
	}
	if current < 13 {
		if err := migrateAIConversationSchemaV13(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(13, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record AI conversation catalog migration: %w", err))
		}
		current = 13
	}
	if current < 14 {
		if err := migrateTaskSchemaV14(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(14, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record task log decoding migration: %w", err))
		}
		current = 14
	}
	if current < 15 {
		if err := migrateAIToolAuditSchemaV15(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(15, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record persistent AI terminal audit migration: %w", err))
		}
		current = 15
	}
	if current < 16 {
		if err := migrateAIAgentInboxSchemaV16(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(16, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record AI Agent inbox migration: %w", err))
		}
		current = 16
	}
	if current < 17 {
		if err := migrateAICollaborationSchemaV17(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(17, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record AI collaboration migration: %w", err))
		}
		current = 17
	}
	if current < 18 {
		if err := migrateAIToolAuditSchemaV18(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(18, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record AI web tool audit migration: %w", err))
		}
		current = 18
	}
	if current < 19 {
		if err := migrateAIToolResultArtifactSchemaV19(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(19, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record AI tool result artifact migration: %w", err))
		}
		current = 19
	}
	if current < 20 {
		if err := migrateAIMCPAuditSchemaV20(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(20, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record MCP tool audit migration: %w", err))
		}
		current = 20
	}
	if current < 21 {
		if err := migrateRemoteV2OperationSchemaV21(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(21, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record remote/v2 operation migration: %w", err))
		}
		current = 21
	}
	if current < 22 {
		if err := migrateTaskSchemaV22(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(22, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record task run log path migration: %w", err))
		}
		current = 22
	}
	if current < 23 {
		if err := migrateTaskSchemaV23(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(23, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record task log file migration: %w", err))
		}
		current = 23
	}
	if current < 24 {
		if err := migrateAIAgentInboxWorkspaceToolsSchemaV24(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(24, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record AI Agent inbox workspace-tool intent migration: %w", err))
		}
		current = 24
	}
	if current < 25 {
		if err := migrateRemoteV2OperationClaimSchemaV25(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(25, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record remote/v2 operation claim migration: %w", err))
		}
		current = 25
	}
	if current < 26 {
		if err := migrateRemoteV2SideEffectSchemaV26(ctx, tx); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at_ms) VALUES(26, ?)`, time.Now().UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("record remote/v2 side-effect migration: %w", err))
		}
		current = 26
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return fmt.Errorf("commit business store migration: %w", err)
	}
	return nil
}

func migrateAIAgentInboxWorkspaceToolsSchemaV24(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("AI Agent inbox workspace-tool intent migration transaction is required")
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(ai_agent_inbox)`)
	if err != nil {
		return fmt.Errorf("inspect AI Agent inbox workspace-tool intent: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var column, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if column == "workspace_tools_enabled" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE ai_agent_inbox ADD COLUMN workspace_tools_enabled INTEGER NOT NULL DEFAULT 0
		CHECK(workspace_tools_enabled IN (0,1))`); err != nil {
		return fmt.Errorf("migrate AI Agent inbox workspace-tool intent: %w", err)
	}
	return nil
}

// migrateAIMCPAuditSchemaV20 widens the audit tool-name contract so dynamic
// mcp__<server>__<tool> names can be recorded.
func migrateAIMCPAuditSchemaV20(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("MCP tool audit migration transaction is required")
	}
	statements := []string{
		`ALTER TABLE ai_tool_audit RENAME TO ai_tool_audit_v19`,
		`DROP INDEX IF EXISTS ai_tool_audit_project_time_idx`,
		`CREATE TABLE ai_tool_audit (
            id TEXT PRIMARY KEY,
            project_id TEXT NOT NULL,
            conversation_id TEXT NOT NULL,
            generation_id TEXT NOT NULL,
            tool_call_id_sha256 TEXT NOT NULL,
            tool_name TEXT NOT NULL,
            arguments_sha256 TEXT NOT NULL,
            preview_sha256 TEXT NOT NULL,
            result_sha256 TEXT NOT NULL DEFAULT '',
            outcome TEXT NOT NULL,
            error_code TEXT NOT NULL DEFAULT '',
            approval_decision TEXT NOT NULL DEFAULT '',
            allow_for_session INTEGER NOT NULL DEFAULT 0,
            started_at_ms INTEGER NOT NULL,
            finished_at_ms INTEGER,
            CHECK(tool_name IN ('list_files','search_files','read_file','read_tool_result','read_image','web_search','web_fetch','write_file','replace_in_file','rollback_file_change','run_command','terminal_open','terminal_send','terminal_read','terminal_signal','terminal_close','terminal_list') OR tool_name LIKE 'mcp__%'),
            CHECK(outcome IN ('awaiting_approval','running','succeeded','failed','denied','cancelled')),
            CHECK(approval_decision IN ('','allow_once','allow_for_session','deny','timeout','cancelled'))
        )`,
		`INSERT INTO ai_tool_audit(
            id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
        ) SELECT id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
          FROM ai_tool_audit_v19`,
		`DROP TABLE ai_tool_audit_v19`,
		`CREATE INDEX ai_tool_audit_project_time_idx ON ai_tool_audit(project_id, started_at_ms)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate AI tool audit for MCP tools: %w", err)
		}
	}
	return nil
}

// migrateAIToolResultArtifactSchemaV19 extends the AI tool audit name contract
// with read_tool_result and adds the session-scoped artifact table that stores
// oversized tool results spilled out of the model context.
func migrateAIToolResultArtifactSchemaV19(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("AI tool result artifact migration transaction is required")
	}
	statements := []string{
		`ALTER TABLE ai_tool_audit RENAME TO ai_tool_audit_v18`,
		`DROP INDEX IF EXISTS ai_tool_audit_project_time_idx`,
		`CREATE TABLE ai_tool_audit (
            id TEXT PRIMARY KEY,
            project_id TEXT NOT NULL,
            conversation_id TEXT NOT NULL,
            generation_id TEXT NOT NULL,
            tool_call_id_sha256 TEXT NOT NULL,
            tool_name TEXT NOT NULL,
            arguments_sha256 TEXT NOT NULL,
            preview_sha256 TEXT NOT NULL,
            result_sha256 TEXT NOT NULL DEFAULT '',
            outcome TEXT NOT NULL,
            error_code TEXT NOT NULL DEFAULT '',
            approval_decision TEXT NOT NULL DEFAULT '',
            allow_for_session INTEGER NOT NULL DEFAULT 0,
            started_at_ms INTEGER NOT NULL,
            finished_at_ms INTEGER,
            CHECK(tool_name IN ('list_files','search_files','read_file','read_tool_result','read_image','web_search','web_fetch','write_file','replace_in_file','rollback_file_change','run_command','terminal_open','terminal_send','terminal_read','terminal_signal','terminal_close','terminal_list')),
            CHECK(outcome IN ('awaiting_approval','running','succeeded','failed','denied','cancelled')),
            CHECK(approval_decision IN ('','allow_once','allow_for_session','deny','timeout','cancelled'))
        )`,
		`INSERT INTO ai_tool_audit(
            id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
        ) SELECT id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
          FROM ai_tool_audit_v18`,
		`DROP TABLE ai_tool_audit_v18`,
		`CREATE INDEX ai_tool_audit_project_time_idx ON ai_tool_audit(project_id, started_at_ms)`,
		`CREATE TABLE IF NOT EXISTS ai_tool_result_artifact (
            artifact_id TEXT PRIMARY KEY,
            conversation_id TEXT NOT NULL REFERENCES ai_conversations(id) ON DELETE CASCADE,
            tool_call_id TEXT NOT NULL,
            total_bytes INTEGER NOT NULL,
            content BLOB NOT NULL,
            created_at_ms INTEGER NOT NULL,
            CHECK(total_bytes > 0),
            CHECK(length(tool_call_id) BETWEEN 1 AND 256)
        )`,
		`CREATE INDEX IF NOT EXISTS ai_tool_result_artifact_conversation_idx ON ai_tool_result_artifact(conversation_id, created_at_ms)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate AI tool result artifacts: %w", err)
		}
	}
	return nil
}

func migrateAIToolAuditSchemaV18(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("AI web tool audit migration transaction is required")
	}
	statements := []string{
		`ALTER TABLE ai_tool_audit RENAME TO ai_tool_audit_v17`,
		`DROP INDEX IF EXISTS ai_tool_audit_project_time_idx`,
		`CREATE TABLE ai_tool_audit (
            id TEXT PRIMARY KEY,
            project_id TEXT NOT NULL,
            conversation_id TEXT NOT NULL,
            generation_id TEXT NOT NULL,
            tool_call_id_sha256 TEXT NOT NULL,
            tool_name TEXT NOT NULL,
            arguments_sha256 TEXT NOT NULL,
            preview_sha256 TEXT NOT NULL,
            result_sha256 TEXT NOT NULL DEFAULT '',
            outcome TEXT NOT NULL,
            error_code TEXT NOT NULL DEFAULT '',
            approval_decision TEXT NOT NULL DEFAULT '',
            allow_for_session INTEGER NOT NULL DEFAULT 0,
            started_at_ms INTEGER NOT NULL,
            finished_at_ms INTEGER,
            CHECK(tool_name IN ('list_files','search_files','read_file','web_search','web_fetch','write_file','replace_in_file','rollback_file_change','run_command','terminal_open','terminal_send','terminal_read','terminal_signal','terminal_close','terminal_list')),
            CHECK(outcome IN ('awaiting_approval','running','succeeded','failed','denied','cancelled')),
            CHECK(approval_decision IN ('','allow_once','allow_for_session','deny','timeout','cancelled'))
        )`,
		`INSERT INTO ai_tool_audit(
            id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
        ) SELECT id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
          FROM ai_tool_audit_v17`,
		`DROP TABLE ai_tool_audit_v17`,
		`CREATE INDEX ai_tool_audit_project_time_idx ON ai_tool_audit(project_id, started_at_ms)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate AI tool audit for web tools: %w", err)
		}
	}
	return nil
}

func migrateAICollaborationSchemaV17(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("AI collaboration migration transaction is required")
	}
	if err := ensureAIConversationCatalogColumn(ctx, tx, "collaboration_json",
		`ALTER TABLE ai_conversations ADD COLUMN collaboration_json TEXT NOT NULL DEFAULT '{}'`); err != nil {
		return fmt.Errorf("add AI collaboration state: %w", err)
	}
	statements := []string{
		`ALTER TABLE ai_conversation_events RENAME TO ai_conversation_events_v16`,
		`DROP INDEX IF EXISTS ai_conversation_events_generation_idx`,
		`CREATE TABLE ai_conversation_events (
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
            CHECK(json_valid(payload_json) AND json_type(payload_json) = 'object')
        )`,
		`INSERT INTO ai_conversation_events(
            conversation_id,sequence,event_id,generation_id,message_id,kind,payload_json,occurred_at_ms
        ) SELECT conversation_id,sequence,event_id,generation_id,message_id,kind,payload_json,occurred_at_ms
          FROM ai_conversation_events_v16`,
		`DROP TABLE ai_conversation_events_v16`,
		`CREATE INDEX ai_conversation_events_generation_idx
            ON ai_conversation_events(conversation_id, generation_id, sequence)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate AI collaboration events: %w", err)
		}
	}
	return nil
}

func migrateAIAgentInboxSchemaV16(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("AI Agent inbox migration transaction is required")
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ai_agent_inbox (
            sequence INTEGER PRIMARY KEY AUTOINCREMENT,
            item_id TEXT NOT NULL UNIQUE,
            device_id TEXT NOT NULL,
            project_id TEXT NOT NULL,
            conversation_id TEXT NOT NULL REFERENCES ai_conversations(id) ON DELETE CASCADE,
            destination TEXT NOT NULL,
            prompt TEXT NOT NULL,
            attachments_json TEXT NOT NULL DEFAULT '[]',
            workspace_mode TEXT NOT NULL,
            state TEXT NOT NULL DEFAULT 'pending',
            claimed_generation_id TEXT NOT NULL DEFAULT '',
            created_at_ms INTEGER NOT NULL,
            updated_at_ms INTEGER NOT NULL,
            CHECK(length(item_id) BETWEEN 1 AND 80),
            CHECK(destination IN ('nextTurn','nextStep')),
            CHECK(state IN ('pending','claimed','cancelled')),
            CHECK(workspace_mode IN ('readOnly','edit','fullAccess'))
        )`,
		`CREATE INDEX IF NOT EXISTS ai_agent_inbox_pending_idx
            ON ai_agent_inbox(device_id, project_id, conversation_id, state, destination, sequence)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate AI Agent inbox: %w", err)
		}
	}
	return nil
}

func migrateAIToolAuditSchemaV15(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("AI tool audit migration transaction is required")
	}
	statements := []string{
		`ALTER TABLE ai_tool_audit RENAME TO ai_tool_audit_v14`,
		`DROP INDEX IF EXISTS ai_tool_audit_project_time_idx`,
		`CREATE TABLE ai_tool_audit (
            id TEXT PRIMARY KEY,
            project_id TEXT NOT NULL,
            conversation_id TEXT NOT NULL,
            generation_id TEXT NOT NULL,
            tool_call_id_sha256 TEXT NOT NULL,
            tool_name TEXT NOT NULL,
            arguments_sha256 TEXT NOT NULL,
            preview_sha256 TEXT NOT NULL,
            result_sha256 TEXT NOT NULL DEFAULT '',
            outcome TEXT NOT NULL,
            error_code TEXT NOT NULL DEFAULT '',
            approval_decision TEXT NOT NULL DEFAULT '',
            allow_for_session INTEGER NOT NULL DEFAULT 0,
            started_at_ms INTEGER NOT NULL,
            finished_at_ms INTEGER,
            CHECK(tool_name IN ('list_files','search_files','read_file','write_file','replace_in_file','rollback_file_change','run_command','terminal_open','terminal_send','terminal_read','terminal_signal','terminal_close','terminal_list')),
            CHECK(outcome IN ('awaiting_approval','running','succeeded','failed','denied','cancelled')),
            CHECK(approval_decision IN ('','allow_once','allow_for_session','deny','timeout','cancelled'))
        )`,
		`INSERT INTO ai_tool_audit(
            id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
        ) SELECT id,project_id,conversation_id,generation_id,tool_call_id_sha256,tool_name,arguments_sha256,preview_sha256,
            result_sha256,outcome,error_code,approval_decision,allow_for_session,started_at_ms,finished_at_ms
          FROM ai_tool_audit_v14`,
		`DROP TABLE ai_tool_audit_v14`,
		`CREATE INDEX ai_tool_audit_project_time_idx ON ai_tool_audit(project_id, started_at_ms)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate AI tool audit for persistent terminals: %w", err)
		}
	}
	return nil
}

func (store *businessStore) recordTerminalSessionOpened(ctx context.Context, audit terminalSessionAudit) error {
	if store == nil || audit.SessionID == uuid.Nil || audit.ProjectID == uuid.Nil || audit.OpenedAt.IsZero() {
		return errors.New("terminal session audit is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `INSERT INTO terminal_session_audit(session_id, project_id, opened_at_ms) VALUES(?, ?, ?)`,
		audit.SessionID.String(), audit.ProjectID.String(), audit.OpenedAt.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("record terminal session open audit: %w", err)
	}
	return nil
}

func (store *businessStore) recordTerminalSessionFinished(ctx context.Context, audit terminalSessionAudit) error {
	if store == nil || audit.SessionID == uuid.Nil || audit.ProjectID == uuid.Nil || audit.OpenedAt.IsZero() || audit.ClosedAt.IsZero() ||
		audit.ClosedAt.Before(audit.OpenedAt) || !validTerminalCloseReason(audit.CloseReason) {
		return errors.New("terminal session completion audit is invalid")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.ExecContext(ctx, `UPDATE terminal_session_audit
        SET closed_at_ms = ?, input_bytes = ?, output_bytes = ?, exit_code = ?, close_reason = ?
        WHERE session_id = ? AND project_id = ? AND closed_at_ms IS NULL`,
		audit.ClosedAt.UTC().UnixMilli(), audit.InputBytes, audit.OutputBytes, audit.ExitCode, audit.CloseReason,
		audit.SessionID.String(), audit.ProjectID.String())
	if err != nil {
		return fmt.Errorf("record terminal session completion audit: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return errors.New("terminal session completion audit was not applied")
	}
	return nil
}

func validTerminalCloseReason(value string) bool {
	switch value {
	case "process_exit", "client_close", "client_terminate", "agent_exit", "policy_revoked", "idle_timeout",
		"disconnect_timeout", "lifetime_limit", "memory_limit", "output_limit", "output_rate_limit",
		"input_rate_limit", "pty_read_error", "pty_write_error", "audit_unavailable":
		return true
	default:
		return false
	}
}

// ensureLegacyWorkspace is deliberately idempotent.  Existing --workspace
// installations retain their deterministic v1 IDs, including direct child
// projects that older clients already selected.  The original identity file
// is never rewritten or removed as part of this migration.
func (store *businessStore) ensureLegacyWorkspace(ctx context.Context, state *agentState) error {
	if store == nil || state == nil || strings.TrimSpace(state.Workspace) == "" {
		return errors.New("legacy workspace migration is invalid")
	}
	root, err := canonicalProjectPath(state.Workspace)
	if err != nil {
		return fmt.Errorf("resolve legacy workspace: %w", err)
	}
	rootName := safeProjectDisplayName(filepath.Base(root))
	if rootName == "" {
		rootName = "Workspace"
	}
	if err := store.upsertLegacyProject(ctx, registeredProject{
		ID: stableProjectID(state.DeviceID, ""), DisplayName: rootName, LocalPath: root,
		State: "available", LegacyRelativePath: "",
	}); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("scan legacy workspace: %w", err)
	}
	count := 1
	for _, entry := range entries {
		if count >= maximumLocalProjects || strings.HasPrefix(entry.Name(), ".") || !validFileName(entry.Name()) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			continue
		}
		path, pathErr := canonicalProjectPath(filepath.Join(root, entry.Name()))
		if pathErr != nil {
			continue
		}
		relative := filepath.ToSlash(entry.Name())
		if err := store.upsertLegacyProject(ctx, registeredProject{
			ID: stableProjectID(state.DeviceID, relative), DisplayName: safeProjectDisplayName(entry.Name()), LocalPath: path,
			State: "available", LegacyRelativePath: relative,
		}); err != nil {
			return err
		}
		count++
	}
	return nil
}

func (store *businessStore) upsertLegacyProject(ctx context.Context, project registeredProject) error {
	if project.ID == uuid.Nil || project.DisplayName == "" || project.LocalPath == "" {
		return errors.New("legacy project is invalid")
	}
	now := time.Now().UTC()
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
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM projects WHERE id = ? AND device_id = ?`, project.ID.String(), store.deviceID.String()).Scan(&revision)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A newer, explicitly registered project can overlap a legacy workspace
		// child while using a non-deterministic ID. The path is unique, so retain
		// that user-created project rather than trying to add a second legacy
		// record for the same directory on every startup.
		var pathOwner string
		pathErr := tx.QueryRowContext(ctx, `SELECT id FROM projects WHERE local_path = ?`, project.LocalPath).Scan(&pathOwner)
		if pathErr == nil && pathOwner != project.ID.String() {
			return commitBusinessTransaction(ctx, tx)
		}
		if pathErr != nil && !errors.Is(pathErr, sql.ErrNoRows) {
			return pathErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO projects(
				id, device_id, display_name, local_path, git_url, state, revision,
				allow_interactive_terminal, allow_task_execution, allow_ai_workspace_tools, allow_remote_create, allow_recursive_delete, legacy_relative_path, created_at_ms, updated_at_ms
			) VALUES(?, ?, ?, ?, '', 'available', 1, 1, 1, 1, 1, 1, ?, ?, ?)`,
			project.ID.String(), store.deviceID.String(), project.DisplayName, project.LocalPath, project.LegacyRelativePath, now.UnixMilli(), now.UnixMilli())
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO project_changes(project_id, revision, operation, occurred_at_ms) VALUES(?, 1, 'upsert', ?)`, project.ID.String(), now.UnixMilli())
		}
		if err != nil {
			return err
		}
		return commitBusinessTransaction(ctx, tx)
	case err != nil:
		return err
	}
	if revision < 1 {
		return errors.New("legacy project revision is invalid")
	}
	// Local moves of the legacy workspace should not change its stable project
	// ID. Update the device-only path and increment revision only when a view
	// visible to a peer changes.
	var displayName, localPath, state, legacyRelative string
	if err := tx.QueryRowContext(ctx, `SELECT display_name, local_path, state, legacy_relative_path FROM projects WHERE id = ? AND device_id = ?`, project.ID.String(), store.deviceID.String()).Scan(&displayName, &localPath, &state, &legacyRelative); err != nil {
		return err
	}
	// A user removal is a durable registry tombstone. Legacy workspace discovery
	// must not silently reactivate it on the next scan or Agent restart; an
	// explicit addProject call remains the only path that restores the record.
	if state == "removed" {
		return commitBusinessTransaction(ctx, tx)
	}
	if displayName == project.DisplayName && sameFilesystemPath(localPath, project.LocalPath) && state == "available" && legacyRelative == project.LegacyRelativePath {
		return commitBusinessTransaction(ctx, tx)
	}
	revision++
	result, err := tx.ExecContext(ctx, `UPDATE projects SET display_name = ?, local_path = ?, state = 'available', legacy_relative_path = ?, revision = ?, updated_at_ms = ?
		WHERE id = ? AND device_id = ? AND revision = ?`, project.DisplayName, project.LocalPath, project.LegacyRelativePath,
		revision, now.UnixMilli(), project.ID.String(), store.deviceID.String(), revision-1)
	if err != nil {
		return err
	}
	if err := requireSingleProjectMutation(result); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO project_changes(project_id, revision, operation, occurred_at_ms) VALUES(?, ?, 'upsert', ?)`, project.ID.String(), revision, now.UnixMilli()); err != nil {
		return err
	}
	return commitBusinessTransaction(ctx, tx)
}

func (store *businessStore) listProjects(ctx context.Context, includeRemoved bool) ([]registeredProject, error) {
	if store == nil {
		return nil, errors.New("business store is unavailable")
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := `SELECT id, display_name, local_path, git_url, state, revision,
		allow_interactive_terminal, allow_task_execution, allow_ai_workspace_tools, allow_remote_create, allow_recursive_delete, legacy_relative_path, created_at_ms, updated_at_ms
        FROM projects WHERE device_id = ?`
	if !includeRemoved {
		query += ` AND state != 'removed'`
	}
	query += ` ORDER BY display_name COLLATE NOCASE, id`
	rows, err := db.QueryContext(ctx, query, store.deviceID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []registeredProject{}
	for rows.Next() {
		project, err := scanRegisteredProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return projects, nil
}

func (store *businessStore) projectByID(ctx context.Context, id uuid.UUID) (registeredProject, error) {
	if store == nil || id == uuid.Nil {
		return registeredProject{}, errRPCNotFound
	}
	db, err := store.openReadDB()
	if err != nil {
		return registeredProject{}, err
	}
	defer db.Close()
	row := db.QueryRowContext(ctx, `SELECT id, display_name, local_path, git_url, state, revision,
		allow_interactive_terminal, allow_task_execution, allow_ai_workspace_tools, allow_remote_create, allow_recursive_delete, legacy_relative_path, created_at_ms, updated_at_ms
        FROM projects WHERE device_id = ? AND id = ?`, store.deviceID.String(), id.String())
	project, err := scanRegisteredProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return registeredProject{}, errRPCNotFound
	}
	if err != nil {
		return registeredProject{}, err
	}
	return project, nil
}

func (store *businessStore) addProject(ctx context.Context, path, displayName, gitURL string, policy projectPolicy) (registeredProject, error) {
	if store == nil {
		return registeredProject{}, errors.New("business store is unavailable")
	}
	canonical, err := canonicalProjectPath(path)
	if err != nil {
		return registeredProject{}, err
	}
	displayName = safeProjectDisplayName(displayName)
	if displayName == "" {
		displayName = safeProjectDisplayName(filepath.Base(canonical))
	}
	if displayName == "" || !validProjectGitURL(gitURL) {
		return registeredProject{}, errRPCInvalid
	}
	now := time.Now().UTC()
	project := registeredProject{
		ID: uuid.New(), DisplayName: displayName, LocalPath: canonical, GitURL: strings.TrimSpace(gitURL),
		State: "available", Revision: 1, Policy: policy, CreatedAt: now, UpdatedAt: now,
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return registeredProject{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return registeredProject{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, local_path, state, revision, legacy_relative_path, created_at_ms
        FROM projects WHERE device_id = ?`, store.deviceID.String())
	if err != nil {
		return registeredProject{}, err
	}
	activeCount := 0
	var existingID, existingState, existingLegacy string
	var existingRevision uint64
	var existingCreatedAt time.Time
	for rows.Next() {
		var id, localPath, state, legacy string
		var revision, createdAt int64
		if err := rows.Scan(&id, &localPath, &state, &revision, &legacy, &createdAt); err != nil {
			_ = rows.Close()
			return registeredProject{}, err
		}
		if state != "removed" {
			activeCount++
		}
		if sameFilesystemPath(localPath, canonical) {
			existingID, existingState, existingLegacy = id, state, legacy
			if revision < 1 || createdAt <= 0 {
				_ = rows.Close()
				return registeredProject{}, errors.New("business project row is invalid")
			}
			existingRevision, existingCreatedAt = uint64(revision), time.UnixMilli(createdAt).UTC()
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return registeredProject{}, err
	}
	if err := rows.Close(); err != nil {
		return registeredProject{}, err
	}
	if existingID != "" {
		if existingState != "removed" || existingRevision == ^uint64(0) {
			return registeredProject{}, errRPCRevision
		}
		if activeCount >= maximumLocalProjects {
			return registeredProject{}, errRPCBusy
		}
		parsed, err := uuid.Parse(existingID)
		if err != nil || parsed == uuid.Nil {
			return registeredProject{}, errors.New("business project row is invalid")
		}
		project.ID, project.Revision, project.CreatedAt = parsed, existingRevision+1, existingCreatedAt
		project.LegacyRelativePath = existingLegacy
		result, err := tx.ExecContext(ctx, `UPDATE projects SET display_name = ?, git_url = ?, state = 'available', revision = ?,
				allow_interactive_terminal = ?, allow_task_execution = ?, allow_ai_workspace_tools = ?, allow_remote_create = ?, allow_recursive_delete = ?, updated_at_ms = ?
				WHERE id = ? AND device_id = ? AND state = 'removed' AND revision = ?`,
			project.DisplayName, project.GitURL, project.Revision, boolInteger(policy.AllowInteractiveTerminal),
			boolInteger(policy.AllowTaskExecution), boolInteger(policy.AllowAIWorkspaceTools), boolInteger(policy.AllowRemoteCreate), boolInteger(policy.AllowRecursiveDelete),
			now.UnixMilli(), project.ID.String(), store.deviceID.String(), existingRevision)
		if err != nil {
			return registeredProject{}, err
		}
		if err := requireSingleProjectMutation(result); err != nil {
			return registeredProject{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_changes(project_id, revision, operation, occurred_at_ms)
                VALUES(?, ?, 'upsert', ?)`, project.ID.String(), project.Revision, now.UnixMilli()); err != nil {
			return registeredProject{}, err
		}
		if err := commitBusinessTransaction(ctx, tx); err != nil {
			return registeredProject{}, err
		}
		return project, nil
	}
	if activeCount >= maximumLocalProjects {
		return registeredProject{}, errRPCBusy
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects(
			id, device_id, display_name, local_path, git_url, state, revision,
			allow_interactive_terminal, allow_task_execution, allow_ai_workspace_tools, allow_remote_create, allow_recursive_delete, legacy_relative_path, created_at_ms, updated_at_ms
		) VALUES(?, ?, ?, ?, ?, 'available', 1, ?, ?, ?, ?, ?, '', ?, ?)`, project.ID.String(), store.deviceID.String(),
		project.DisplayName, project.LocalPath, project.GitURL, boolInteger(policy.AllowInteractiveTerminal), boolInteger(policy.AllowTaskExecution),
		boolInteger(policy.AllowAIWorkspaceTools), boolInteger(policy.AllowRemoteCreate), boolInteger(policy.AllowRecursiveDelete), now.UnixMilli(), now.UnixMilli()); err != nil {
		return registeredProject{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_changes(project_id, revision, operation, occurred_at_ms) VALUES(?, 1, 'upsert', ?)`, project.ID.String(), now.UnixMilli()); err != nil {
		return registeredProject{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return registeredProject{}, err
	}
	return project, nil
}

func (store *businessStore) updateProject(ctx context.Context, id uuid.UUID, displayName, gitURL *string, policy *projectPolicy, expectedRevision *uint64) (registeredProject, error) {
	if store == nil || id == uuid.Nil {
		return registeredProject{}, errRPCNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return registeredProject{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return registeredProject{}, err
	}
	defer tx.Rollback()
	project, err := scanRegisteredProject(tx.QueryRowContext(ctx, `SELECT id, display_name, local_path, git_url, state, revision,
		allow_interactive_terminal, allow_task_execution, allow_ai_workspace_tools, allow_remote_create, allow_recursive_delete, legacy_relative_path, created_at_ms, updated_at_ms
        FROM projects WHERE id = ? AND device_id = ?`, id.String(), store.deviceID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return registeredProject{}, errRPCNotFound
	}
	if err != nil {
		return registeredProject{}, err
	}
	if project.State == "removed" || expectedRevision != nil && project.Revision != *expectedRevision || project.Revision == ^uint64(0) {
		return registeredProject{}, errRPCRevision
	}
	prior := project
	if displayName != nil {
		project.DisplayName = safeProjectDisplayName(*displayName)
		if project.DisplayName == "" {
			return registeredProject{}, errRPCInvalid
		}
	}
	if gitURL != nil {
		if !validProjectGitURL(*gitURL) {
			return registeredProject{}, errRPCInvalid
		}
		project.GitURL = strings.TrimSpace(*gitURL)
	}
	if policy != nil {
		project.Policy = *policy
	}
	if project.DisplayName == prior.DisplayName && project.GitURL == prior.GitURL && project.Policy == prior.Policy {
		return prior, nil
	}
	now := time.Now().UTC()
	project.Revision++
	project.UpdatedAt = now
	result, err := tx.ExecContext(ctx, `UPDATE projects SET display_name = ?, git_url = ?, revision = ?, allow_interactive_terminal = ?, allow_task_execution = ?, allow_ai_workspace_tools = ?, allow_remote_create = ?, allow_recursive_delete = ?, updated_at_ms = ?
		WHERE id = ? AND device_id = ? AND state != 'removed' AND revision = ?`,
		project.DisplayName, project.GitURL, project.Revision, boolInteger(project.Policy.AllowInteractiveTerminal),
		boolInteger(project.Policy.AllowTaskExecution), boolInteger(project.Policy.AllowAIWorkspaceTools), boolInteger(project.Policy.AllowRemoteCreate),
		boolInteger(project.Policy.AllowRecursiveDelete), now.UnixMilli(), id.String(), store.deviceID.String(), prior.Revision)
	if err != nil {
		return registeredProject{}, err
	}
	if err := requireSingleProjectMutation(result); err != nil {
		return registeredProject{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_changes(project_id, revision, operation, occurred_at_ms) VALUES(?, ?, 'upsert', ?)`, id.String(), project.Revision, now.UnixMilli()); err != nil {
		return registeredProject{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return registeredProject{}, err
	}
	return project, nil
}

// removeProject only removes the registry entry.  It never touches the
// directory, which makes accidental remote deletion impossible at this layer.
func (store *businessStore) removeProject(ctx context.Context, id uuid.UUID, expectedRevision *uint64) (registeredProject, error) {
	if store == nil || id == uuid.Nil {
		return registeredProject{}, errRPCNotFound
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return registeredProject{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return registeredProject{}, err
	}
	defer tx.Rollback()
	project, err := scanRegisteredProject(tx.QueryRowContext(ctx, `SELECT id, display_name, local_path, git_url, state, revision,
		allow_interactive_terminal, allow_task_execution, allow_ai_workspace_tools, allow_remote_create, allow_recursive_delete, legacy_relative_path, created_at_ms, updated_at_ms
        FROM projects WHERE id = ? AND device_id = ?`, id.String(), store.deviceID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return registeredProject{}, errRPCNotFound
	}
	if err != nil {
		return registeredProject{}, err
	}
	if project.State == "removed" || expectedRevision != nil && project.Revision != *expectedRevision || project.Revision == ^uint64(0) {
		return registeredProject{}, errRPCRevision
	}
	var activeAIConversationExists, activeTaskExists int
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM ai_conversations WHERE device_id = ? AND project_id = ? AND state = 'generating'),
		EXISTS(SELECT 1 FROM tasks WHERE project_id = ?
			AND status NOT IN ('completed','failed','blocked','cancelled','succeeded','changesRequested'))`,
		store.deviceID.String(), id.String(), id.String()).Scan(&activeAIConversationExists, &activeTaskExists); err != nil {
		return registeredProject{}, err
	}
	if activeAIConversationExists != 0 {
		return registeredProject{}, errRPCProjectHasActiveAIConversations
	}
	if activeTaskExists != 0 {
		return registeredProject{}, errRPCProjectHasActiveTasks
	}
	priorRevision := project.Revision
	now := time.Now().UTC()
	project.State = "removed"
	project.Revision++
	project.UpdatedAt = now
	result, err := tx.ExecContext(ctx, `UPDATE projects SET state = 'removed', revision = ?, updated_at_ms = ?
        WHERE id = ? AND device_id = ? AND state != 'removed' AND revision = ?`,
		project.Revision, now.UnixMilli(), id.String(), store.deviceID.String(), priorRevision)
	if err != nil {
		return registeredProject{}, err
	}
	if err := requireSingleProjectMutation(result); err != nil {
		return registeredProject{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO project_changes(project_id, revision, operation, occurred_at_ms) VALUES(?, ?, 'tombstone', ?)`, id.String(), project.Revision, now.UnixMilli()); err != nil {
		return registeredProject{}, err
	}
	// Event cursors are meaningful only while the project grant exists. Do not
	// let a restored project inherit durable hints from its prior registration.
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_event_journal WHERE project_id = ?`, id.String()); err != nil {
		return registeredProject{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_event_streams WHERE project_id = ?`, id.String()); err != nil {
		return registeredProject{}, err
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return registeredProject{}, err
	}
	return project, nil
}

func scanRegisteredProject(scanner interface{ Scan(...any) error }) (registeredProject, error) {
	var id string
	var revision, interactive, taskExecution, aiWorkspaceTools, remoteCreate, recursiveDelete, createdAt, updatedAt int64
	var project registeredProject
	if err := scanner.Scan(&id, &project.DisplayName, &project.LocalPath, &project.GitURL, &project.State, &revision,
		&interactive, &taskExecution, &aiWorkspaceTools, &remoteCreate, &recursiveDelete, &project.LegacyRelativePath, &createdAt, &updatedAt); err != nil {
		return registeredProject{}, err
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || revision < 1 || createdAt <= 0 || updatedAt <= 0 || (project.State != "available" && project.State != "unavailable" && project.State != "removed") {
		return registeredProject{}, errors.New("business project row is invalid")
	}
	project.ID, project.Revision = parsed, uint64(revision)
	project.Policy = projectPolicy{
		AllowInteractiveTerminal: interactive != 0,
		AllowTaskExecution:       taskExecution != 0,
		AllowAIWorkspaceTools:    aiWorkspaceTools != 0,
		AllowRemoteCreate:        remoteCreate != 0,
		AllowRecursiveDelete:     recursiveDelete != 0,
	}
	project.CreatedAt, project.UpdatedAt = time.UnixMilli(createdAt).UTC(), time.UnixMilli(updatedAt).UTC()
	return project, nil
}

func canonicalProjectPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errRPCInvalid
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return "", errRPCForbidden
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || !sameFilesystemPath(resolved, abs) {
		return "", errRPCForbidden
	}
	return filepath.Clean(abs), nil
}

func validProjectGitURL(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if len(value) > 2048 || !strings.Contains(value, "://") {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "ssh") && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func boolInteger(value bool) int {
	if value {
		return 1
	}
	return 0
}

func requireSingleProjectMutation(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errRPCRevision
	}
	return nil
}

func sortedProjectIDs(projects []registeredProject) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	slices.SortFunc(ids, func(left, right uuid.UUID) int { return strings.Compare(left.String(), right.String()) })
	return ids
}
