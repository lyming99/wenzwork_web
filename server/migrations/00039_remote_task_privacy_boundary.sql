-- +goose Up
-- Task definitions and logs are device-local business data. Remove historical
-- cloud copies before enforcing the projection-only schema.
DELETE FROM remote_commands WHERE kind = 'task.create';
DELETE FROM remote_device_events WHERE event_type = 'task.log';
DROP TABLE IF EXISTS remote_task_logs;

UPDATE remote_tasks
SET task_type = CASE WHEN task_type IN (
        'codex', 'cursor', 'hermes', 'jcode', 'opencode', 'claude', 'kimi', 'pi',
        'script', 'workflow', 'workspace.inspect', 'markdown.render', 'ai.summarize', 'remote'
    ) THEN task_type ELSE 'remote' END,
title = CASE task_type
    WHEN 'codex' THEN 'Codex task'
    WHEN 'cursor' THEN 'Cursor task'
    WHEN 'hermes' THEN 'Hermes task'
    WHEN 'jcode' THEN 'JCode task'
    WHEN 'opencode' THEN 'OpenCode task'
    WHEN 'claude' THEN 'Claude task'
    WHEN 'kimi' THEN 'Kimi task'
    WHEN 'pi' THEN 'Pi task'
    WHEN 'script' THEN 'Script task'
    WHEN 'workflow' THEN 'Workflow task'
    WHEN 'workspace.inspect' THEN 'Workspace inspection'
    WHEN 'markdown.render' THEN 'Markdown rendering'
    WHEN 'ai.summarize' THEN 'AI summary'
    ELSE 'Remote task'
END,
input_metadata = '{}'::jsonb;

ALTER TABLE remote_tasks DROP COLUMN input_metadata;

ALTER TABLE remote_tasks
    ADD CONSTRAINT remote_tasks_projection_type_check
    CHECK (task_type IN (
        'codex', 'cursor', 'hermes', 'jcode', 'opencode', 'claude', 'kimi', 'pi',
        'script', 'workflow', 'workspace.inspect', 'markdown.render', 'ai.summarize', 'remote'
    )),
    ADD CONSTRAINT remote_tasks_projection_title_check
    CHECK (title = CASE task_type
        WHEN 'codex' THEN 'Codex task'
        WHEN 'cursor' THEN 'Cursor task'
        WHEN 'hermes' THEN 'Hermes task'
        WHEN 'jcode' THEN 'JCode task'
        WHEN 'opencode' THEN 'OpenCode task'
        WHEN 'claude' THEN 'Claude task'
        WHEN 'kimi' THEN 'Kimi task'
        WHEN 'pi' THEN 'Pi task'
        WHEN 'script' THEN 'Script task'
        WHEN 'workflow' THEN 'Workflow task'
        WHEN 'workspace.inspect' THEN 'Workspace inspection'
        WHEN 'markdown.render' THEN 'Markdown rendering'
        WHEN 'ai.summarize' THEN 'AI summary'
        ELSE 'Remote task'
    END);

ALTER TABLE remote_commands DROP CONSTRAINT IF EXISTS remote_commands_kind_check;
ALTER TABLE remote_commands
    ADD CONSTRAINT remote_commands_kind_check
    CHECK (kind IN ('project.sync', 'task.cancel'));

ALTER TABLE remote_commands
    ADD CONSTRAINT remote_commands_projection_body_check
    CHECK (
        (
            kind = 'task.cancel'
            AND task_id IS NOT NULL
            AND expected_revision IS NULL
            AND body = jsonb_build_object('taskId', task_id)
        )
        OR
        (
            kind = 'project.sync'
            AND task_id IS NULL
            AND body ? 'afterSequence'
            AND body ? 'knownHighWatermark'
            AND jsonb_typeof(body -> 'afterSequence') = 'number'
            AND jsonb_typeof(body -> 'knownHighWatermark') = 'number'
            AND (NOT (body ? 'projectId') OR jsonb_typeof(body -> 'projectId') = 'string')
            AND body - ARRAY['afterSequence', 'knownHighWatermark', 'projectId']::text[] = '{}'::jsonb
        )
    );

-- +goose Down
ALTER TABLE remote_commands DROP CONSTRAINT IF EXISTS remote_commands_projection_body_check;
ALTER TABLE remote_commands DROP CONSTRAINT IF EXISTS remote_commands_kind_check;
ALTER TABLE remote_commands
    ADD CONSTRAINT remote_commands_kind_check
    CHECK (kind IN ('project.sync', 'task.create', 'task.cancel'));

ALTER TABLE remote_tasks DROP CONSTRAINT IF EXISTS remote_tasks_projection_title_check;
ALTER TABLE remote_tasks DROP CONSTRAINT IF EXISTS remote_tasks_projection_type_check;

ALTER TABLE remote_tasks
    ADD COLUMN input_metadata jsonb NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(input_metadata) = 'object');

CREATE TABLE remote_task_logs (
    task_id uuid NOT NULL REFERENCES remote_tasks(task_id) ON DELETE CASCADE,
    sequence bigint NOT NULL CHECK (sequence > 0),
    stream varchar(12) NOT NULL CHECK (stream IN ('stdout', 'stderr', 'system', 'tool')),
    occurred_at timestamptz NOT NULL,
    content text NOT NULL CHECK (octet_length(content) <= 262144),
    PRIMARY KEY (task_id, sequence)
);
