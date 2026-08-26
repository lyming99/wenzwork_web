-- +goose Up
-- Phase 4 keeps only authorization, command delivery and bounded projection
-- data in the cloud. AI secrets/prompts/replies and file paths/content are
-- carried only inside the end-to-end encrypted Peer RPC channel.
CREATE TABLE remote_access_grants (
    device_id uuid PRIMARY KEY REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scopes jsonb NOT NULL CHECK (jsonb_typeof(scopes) = 'array'),
    status varchar(16) NOT NULL CHECK (status IN ('enabled', 'revoked')),
    grant_version bigint NOT NULL CHECK (grant_version > 0),
    enabled_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'enabled' AND enabled_at IS NOT NULL AND revoked_at IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL))
);

CREATE INDEX remote_access_grants_user_status_idx
    ON remote_access_grants (user_id, status, updated_at DESC, device_id DESC);

-- A controller is a browser-held Ed25519 identity. Only the public half is
-- persisted. Rotation/revocation bumps both versions so already issued Peer
-- tickets can be fenced by Relay admission.
CREATE TABLE remote_controller_identities (
    controller_id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    registered_session_id uuid NOT NULL REFERENCES app_sessions(id) ON DELETE RESTRICT,
    identity_public_key bytea NOT NULL CHECK (octet_length(identity_public_key) = 32),
    public_key_thumbprint char(43) NOT NULL
        CHECK (public_key_thumbprint ~ '^[A-Za-z0-9_-]{43}$'),
    key_version bigint NOT NULL DEFAULT 1 CHECK (key_version > 0),
    grant_version bigint NOT NULL DEFAULT 1 CHECK (grant_version > 0),
    scopes jsonb NOT NULL CHECK (jsonb_typeof(scopes) = 'array'),
    status varchar(16) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'revoked')),
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'active' AND revoked_at IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL))
);

CREATE INDEX remote_controller_identities_user_status_idx
    ON remote_controller_identities (user_id, status, updated_at DESC, controller_id DESC);

CREATE TABLE remote_control_request_keys (
    request_key_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resource_id uuid NOT NULL,
    operation varchar(40) NOT NULL CHECK (operation IN (
        'access.enable', 'access.revoke', 'controller.register',
        'controller.rotate', 'controller.revoke'
    )),
    idempotency_key varchar(128) NOT NULL
        CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{8,128}$'),
    request_hash char(64) NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    result_version bigint NOT NULL DEFAULT 0 CHECK (result_version >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at),
    UNIQUE (user_id, resource_id, operation, idempotency_key)
);

CREATE INDEX remote_control_request_keys_expiry_idx
    ON remote_control_request_keys (expires_at);

CREATE TABLE remote_projects (
    device_id uuid NOT NULL REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    project_id uuid NOT NULL,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    display_name varchar(200) NOT NULL,
    revision bigint NOT NULL CHECK (revision >= 0),
    capabilities jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(capabilities) = 'array'),
    state varchar(16) NOT NULL CHECK (state IN ('available', 'removed', 'unavailable')),
    observed_at timestamptz NOT NULL,
    device_sequence bigint NOT NULL CHECK (device_sequence >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (device_id, project_id)
);

CREATE INDEX remote_projects_user_device_page_idx
    ON remote_projects (user_id, device_id, updated_at DESC, project_id DESC);

CREATE TABLE remote_tasks (
    task_id uuid PRIMARY KEY,
    device_id uuid NOT NULL REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id uuid,
    task_type varchar(80) NOT NULL,
    title varchar(200) NOT NULL,
    input_metadata jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(input_metadata) = 'object'),
    status varchar(24) NOT NULL CHECK (status IN (
        'queued', 'dispatched', 'accepted', 'running', 'cancel_requested',
        'cancelled', 'succeeded', 'failed', 'rejected', 'expired', 'timed_out'
    )),
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    result_code varchar(80),
    started_at timestamptz,
    finished_at timestamptz,
    device_sequence bigint NOT NULL DEFAULT 0 CHECK (device_sequence >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX remote_tasks_user_device_page_idx
    ON remote_tasks (user_id, device_id, updated_at DESC, task_id DESC);

CREATE TABLE remote_commands (
    command_id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id uuid NOT NULL REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    task_id uuid REFERENCES remote_tasks(task_id) ON DELETE CASCADE,
    kind varchar(32) NOT NULL CHECK (kind IN ('project.sync', 'task.create', 'task.cancel')),
    body jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(body) = 'object'),
    status varchar(16) NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'leased', 'accepted', 'completed', 'failed', 'cancelled', 'expired')),
    idempotency_key varchar(128) NOT NULL
        CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{8,128}$'),
    request_hash char(64) NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    grant_version bigint NOT NULL CHECK (grant_version > 0),
    expected_revision bigint CHECK (expected_revision >= 0),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    lease_token uuid,
    lease_expires_at timestamptz,
    expires_at timestamptz NOT NULL,
    acknowledged_at timestamptz,
    completed_at timestamptz,
    failure_code varchar(80),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, device_id, kind, idempotency_key),
    CHECK ((status = 'leased' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR status <> 'leased')
);

CREATE INDEX remote_commands_device_poll_idx
    ON remote_commands (device_id, created_at, command_id)
    WHERE status IN ('queued', 'leased');

CREATE TABLE remote_task_logs (
    task_id uuid NOT NULL REFERENCES remote_tasks(task_id) ON DELETE CASCADE,
    sequence bigint NOT NULL CHECK (sequence > 0),
    stream varchar(12) NOT NULL CHECK (stream IN ('stdout', 'stderr', 'system', 'tool')),
    occurred_at timestamptz NOT NULL,
    content text NOT NULL CHECK (octet_length(content) <= 262144),
    PRIMARY KEY (task_id, sequence)
);

CREATE TABLE remote_device_sync_state (
    device_id uuid PRIMARY KEY REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    high_watermark bigint NOT NULL DEFAULT 0 CHECK (high_watermark >= 0),
    minimum_available_sequence bigint NOT NULL DEFAULT 0 CHECK (minimum_available_sequence >= 0),
    last_sync_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (minimum_available_sequence <= high_watermark)
);

CREATE TABLE remote_changes (
    device_id uuid NOT NULL REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    sequence bigint NOT NULL CHECK (sequence > 0),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resource_kind varchar(16) NOT NULL CHECK (resource_kind IN ('project', 'task')),
    resource_id uuid NOT NULL,
    operation varchar(12) NOT NULL CHECK (operation IN ('upsert', 'tombstone')),
    revision bigint NOT NULL CHECK (revision >= 0),
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (device_id, sequence)
);

CREATE INDEX remote_changes_user_device_sequence_idx
    ON remote_changes (user_id, device_id, sequence);

CREATE TABLE remote_device_events (
    event_row_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id uuid NOT NULL,
    device_id uuid NOT NULL REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    task_id uuid REFERENCES remote_tasks(task_id) ON DELETE CASCADE,
    device_sequence bigint NOT NULL CHECK (device_sequence > 0),
    event_type varchar(48) NOT NULL,
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    payload jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    occurred_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (device_id, event_id),
    UNIQUE (device_id, device_sequence)
);

CREATE INDEX remote_device_events_task_resume_idx
    ON remote_device_events (user_id, task_id, event_row_id);

-- +goose Down
DROP TABLE IF EXISTS remote_device_events;
DROP TABLE IF EXISTS remote_changes;
DROP TABLE IF EXISTS remote_device_sync_state;
DROP TABLE IF EXISTS remote_task_logs;
DROP TABLE IF EXISTS remote_commands;
DROP TABLE IF EXISTS remote_tasks;
DROP TABLE IF EXISTS remote_projects;
DROP TABLE IF EXISTS remote_control_request_keys;
DROP TABLE IF EXISTS remote_controller_identities;
DROP TABLE IF EXISTS remote_access_grants;
