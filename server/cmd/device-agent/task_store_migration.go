package main

import (
	"context"
	"database/sql"
	"fmt"
)

func migrateTaskSchemaV4(ctx context.Context, tx *sql.Tx) error {
	statements := []struct {
		name string
		sql  string
	}{
		{name: "task records", sql: `CREATE TABLE tasks (
            id TEXT PRIMARY KEY,
            project_id TEXT NOT NULL,
            kind TEXT NOT NULL,
            title TEXT NOT NULL,
            cwd TEXT NOT NULL DEFAULT '',
            scope TEXT NOT NULL DEFAULT 'topLevel',
            owner_workflow_task_id TEXT,
            parent_task_id TEXT,
            root_task_id TEXT,
            definition_json BLOB NOT NULL,
            definition_revision INTEGER NOT NULL,
            status TEXT NOT NULL,
            revision INTEGER NOT NULL,
            current_run_id TEXT,
            next_log_sequence INTEGER NOT NULL DEFAULT 1,
            scheduled_at_ms INTEGER,
            created_at_ms INTEGER NOT NULL,
            updated_at_ms INTEGER NOT NULL,
            started_at_ms INTEGER,
            finished_at_ms INTEGER,
            exit_code INTEGER,
            result_code TEXT NOT NULL DEFAULT '',
            CHECK(kind IN ('codex','cursor','hermes','jcode','opencode','claude','kimi','pi','script','workflow')),
            CHECK(scope IN ('topLevel','workflowNode')),
            CHECK(status IN ('queued','waiting','running','awaitingAcceptance','changesRequested','completed','failed','blocked','cancelled','succeeded')),
            CHECK(definition_revision > 0),
            CHECK(revision > 0),
            CHECK(next_log_sequence > 0),
            FOREIGN KEY(project_id) REFERENCES projects(id)
        )`},
		{name: "task project index", sql: `CREATE INDEX tasks_project_status_time_idx ON tasks(project_id, status, created_at_ms, id)`},
		{name: "task schedule index", sql: `CREATE INDEX tasks_schedule_idx ON tasks(status, scheduled_at_ms) WHERE scheduled_at_ms IS NOT NULL`},
		{name: "task relationship index", sql: `CREATE INDEX tasks_parent_idx ON tasks(parent_task_id, root_task_id)`},
		{name: "task runs", sql: `CREATE TABLE task_runs (
            id TEXT PRIMARY KEY,
            task_id TEXT NOT NULL,
            workflow_revision_id TEXT,
            parent_workflow_task_run_id TEXT,
            workflow_node_id TEXT,
            status TEXT NOT NULL,
            attempt INTEGER NOT NULL,
            created_at_ms INTEGER NOT NULL,
            started_at_ms INTEGER,
            finished_at_ms INTEGER,
            exit_code INTEGER,
            result_code TEXT NOT NULL DEFAULT '',
            cli_session_id TEXT NOT NULL DEFAULT '',
            CHECK(status IN ('queued','waiting','running','awaitingAcceptance','changesRequested','completed','failed','blocked','cancelled','succeeded')),
            CHECK(attempt >= 0),
            FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
        )`},
		{name: "task run index", sql: `CREATE INDEX task_runs_task_attempt_idx ON task_runs(task_id, attempt DESC, created_at_ms DESC)`},
		{name: "task logs", sql: `CREATE TABLE task_logs (
            task_id TEXT NOT NULL,
            run_id TEXT,
            sequence INTEGER NOT NULL,
            stream TEXT NOT NULL,
            content BLOB NOT NULL,
            byte_count INTEGER NOT NULL,
            occurred_at_ms INTEGER NOT NULL,
            PRIMARY KEY(task_id, sequence),
            CHECK(stream IN ('stdout','stderr','system','tool')),
            CHECK(sequence > 0),
            CHECK(byte_count >= 0),
            FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE,
            FOREIGN KEY(run_id) REFERENCES task_runs(id) ON DELETE CASCADE
        )`},
		{name: "task log retention index", sql: `CREATE INDEX task_logs_time_idx ON task_logs(occurred_at_ms, task_id, sequence)`},
		{name: "task changes", sql: `CREATE TABLE task_changes (
            sequence INTEGER PRIMARY KEY AUTOINCREMENT,
            task_id TEXT NOT NULL,
            project_id TEXT NOT NULL,
            revision INTEGER NOT NULL,
            operation TEXT NOT NULL,
            occurred_at_ms INTEGER NOT NULL,
            CHECK(operation IN ('upsert','delete')),
            CHECK(revision > 0)
        )`},
		{name: "task change project index", sql: `CREATE INDEX task_changes_project_sequence_idx ON task_changes(project_id, sequence)`},
		{name: "workflow revisions", sql: `CREATE TABLE workflow_revisions (
            id TEXT PRIMARY KEY,
            workflow_task_id TEXT NOT NULL,
            version INTEGER NOT NULL,
            description TEXT NOT NULL DEFAULT '',
            failure_policy TEXT NOT NULL,
            graph_digest TEXT NOT NULL,
            created_at_ms INTEGER NOT NULL,
            UNIQUE(workflow_task_id, version),
            CHECK(version > 0),
            CHECK(failure_policy IN ('stopOnFailure','continueOnFailure')),
            FOREIGN KEY(workflow_task_id) REFERENCES tasks(id) ON DELETE CASCADE
        )`},
		{name: "workflow nodes", sql: `CREATE TABLE workflow_nodes (
            revision_id TEXT NOT NULL,
            node_id TEXT NOT NULL,
            node_type TEXT NOT NULL,
            task_definition_id TEXT,
            task_snapshot_json BLOB,
            source_task_id TEXT,
            source_task_revision INTEGER,
            position_x REAL NOT NULL,
            position_y REAL NOT NULL,
            PRIMARY KEY(revision_id, node_id),
            CHECK(node_type IN ('start','task','finish')),
            FOREIGN KEY(revision_id) REFERENCES workflow_revisions(id) ON DELETE CASCADE
        )`},
		{name: "workflow edges", sql: `CREATE TABLE workflow_edges (
            revision_id TEXT NOT NULL,
            edge_id TEXT NOT NULL,
            source_id TEXT NOT NULL,
            target_id TEXT NOT NULL,
            edge_type TEXT NOT NULL,
            label TEXT,
            PRIMARY KEY(revision_id, edge_id),
            CHECK(edge_type IN ('onSuccess','onFailure','always')),
            FOREIGN KEY(revision_id) REFERENCES workflow_revisions(id) ON DELETE CASCADE
        )`},
		{name: "workflow runs", sql: `CREATE TABLE workflow_runs (
            task_run_id TEXT PRIMARY KEY,
            revision_id TEXT NOT NULL,
            created_at_ms INTEGER NOT NULL,
            FOREIGN KEY(task_run_id) REFERENCES task_runs(id) ON DELETE CASCADE,
            FOREIGN KEY(revision_id) REFERENCES workflow_revisions(id)
        )`},
		{name: "workflow node runs", sql: `CREATE TABLE workflow_node_runs (
            workflow_task_run_id TEXT NOT NULL,
            revision_id TEXT NOT NULL,
            node_id TEXT NOT NULL,
            child_task_run_id TEXT,
            status TEXT NOT NULL,
            attempt INTEGER NOT NULL,
            PRIMARY KEY(workflow_task_run_id, node_id, attempt),
            CHECK(status IN ('pending','ready','running','succeeded','failed','blocked','cancelled','skipped')),
            CHECK(attempt >= 0),
            FOREIGN KEY(workflow_task_run_id) REFERENCES workflow_runs(task_run_id) ON DELETE CASCADE,
            FOREIGN KEY(revision_id, node_id) REFERENCES workflow_nodes(revision_id, node_id),
            FOREIGN KEY(child_task_run_id) REFERENCES task_runs(id)
        )`},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf("create %s: %w", statement.name, err)
		}
	}
	return nil
}

func migrateTaskSchemaV5(ctx context.Context, tx *sql.Tx) error {
	statements := []struct {
		name string
		sql  string
	}{
		{name: "workflow revision parallelism", sql: `ALTER TABLE workflow_revisions
            ADD COLUMN maximum_parallelism INTEGER NOT NULL DEFAULT 0
            CHECK(maximum_parallelism >= 0 AND maximum_parallelism <= 64)`},
		{name: "workflow child run index", sql: `CREATE INDEX task_runs_workflow_parent_node_idx
            ON task_runs(parent_workflow_task_run_id, workflow_node_id, attempt DESC)
            WHERE parent_workflow_task_run_id IS NOT NULL`},
		{name: "workflow node child index", sql: `CREATE INDEX workflow_node_runs_child_idx
            ON workflow_node_runs(child_task_run_id)
            WHERE child_task_run_id IS NOT NULL`},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf("create %s: %w", statement.name, err)
		}
	}
	return nil
}

func migrateTaskSchemaV14(ctx context.Context, tx *sql.Tx) error {
	statements := []struct {
		name string
		sql  string
	}{
		{name: "task log display projection", sql: `ALTER TABLE task_logs
            ADD COLUMN display_content BLOB NOT NULL DEFAULT X''`},
		{name: "task log source encoding", sql: `ALTER TABLE task_logs
            ADD COLUMN source_encoding TEXT NOT NULL DEFAULT ''`},
		{name: "task log binary marker", sql: `ALTER TABLE task_logs
            ADD COLUMN is_binary INTEGER NOT NULL DEFAULT 0 CHECK(is_binary IN (0, 1))`},
		{name: "task log decode error marker", sql: `ALTER TABLE task_logs
            ADD COLUMN had_decode_errors INTEGER NOT NULL DEFAULT 0 CHECK(had_decode_errors IN (0, 1))`},
		{name: "task log raw availability marker", sql: `ALTER TABLE task_logs
            ADD COLUMN raw_available INTEGER NOT NULL DEFAULT 1 CHECK(raw_available IN (0, 1))`},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf("create %s: %w", statement.name, err)
		}
	}
	return nil
}

func migrateTaskSchemaV22(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE task_runs
        ADD COLUMN log_path TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add task run log path: %w", err)
	}
	return nil
}

// v23 moves the task-log source of truth out of SQLite. The legacy log_path
// and task_logs table deliberately remain migration-only inputs for one
// release cycle; new runs use only the metadata added here.
func migrateTaskSchemaV23(ctx context.Context, tx *sql.Tx) error {
	statements := []struct {
		name string
		sql  string
	}{
		{name: "task run log state", sql: `ALTER TABLE task_runs
            ADD COLUMN log_state TEXT NOT NULL DEFAULT 'none'
            CHECK(log_state IN ('none','creating','active','sealed','expired','missing','migrating'))`},
		{name: "task run log generation", sql: `ALTER TABLE task_runs
            ADD COLUMN log_generation INTEGER NOT NULL DEFAULT 0 CHECK(log_generation >= 0)`},
		{name: "task run log format", sql: `ALTER TABLE task_runs
            ADD COLUMN log_format_version INTEGER NOT NULL DEFAULT 0 CHECK(log_format_version >= 0)`},
		{name: "task run log size", sql: `ALTER TABLE task_runs
            ADD COLUMN log_size_bytes INTEGER NOT NULL DEFAULT 0 CHECK(log_size_bytes >= 0)`},
		{name: "task run log digest", sql: `ALTER TABLE task_runs
            ADD COLUMN log_sha256 TEXT NOT NULL DEFAULT ''`},
		{name: "task run log update time", sql: `ALTER TABLE task_runs
            ADD COLUMN log_updated_at_ms INTEGER`},
		{name: "task run log retention index", sql: `CREATE INDEX task_runs_log_retention_idx
            ON task_runs(log_state, finished_at_ms, task_id, log_size_bytes)`},
		{name: "task log migration reports", sql: `CREATE TABLE task_log_migration_reports (
            task_id TEXT PRIMARY KEY,
            migrated_run_count INTEGER NOT NULL DEFAULT 0,
            migrated_byte_count INTEGER NOT NULL DEFAULT 0,
            legacy_unscoped_rows INTEGER NOT NULL DEFAULT 0,
            legacy_unscoped_sha256 TEXT NOT NULL DEFAULT '',
            updated_at_ms INTEGER NOT NULL,
            CHECK(migrated_run_count >= 0),
            CHECK(migrated_byte_count >= 0),
            CHECK(legacy_unscoped_rows >= 0),
            FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
        )`},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf("create %s: %w", statement.name, err)
		}
	}
	return nil
}
