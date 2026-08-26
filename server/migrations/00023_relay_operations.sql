-- +goose Up
CREATE TABLE relay_assignments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    cell_id uuid NOT NULL REFERENCES relay_cells(id) ON DELETE RESTRICT,
    assignment_version bigint NOT NULL CHECK (assignment_version > 0),
    mode varchar(20) NOT NULL DEFAULT 'auto' CHECK (mode IN ('auto', 'pinned')),
    status varchar(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'current', 'historical', 'expired')),
    lease_expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, assignment_version)
);

CREATE UNIQUE INDEX relay_assignments_one_current_idx
    ON relay_assignments (user_id) WHERE status = 'current';
CREATE INDEX relay_assignments_cell_status_idx ON relay_assignments (cell_id, status);

CREATE TABLE relay_assignment_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    assignment_id uuid NOT NULL REFERENCES relay_assignments(id) ON DELETE CASCADE,
    event_type varchar(40) NOT NULL,
    assignment_version bigint NOT NULL CHECK (assignment_version > 0),
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX relay_assignment_events_assignment_idx
    ON relay_assignment_events (assignment_id, created_at DESC);

CREATE TABLE relay_assignment_pins (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    cell_id uuid NOT NULL REFERENCES relay_cells(id) ON DELETE RESTRICT,
    reason varchar(500) NOT NULL,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE relay_operations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_type varchar(40) NOT NULL CHECK (operation_type IN (
        'node_drain', 'cell_drain', 'migrate_user', 'bulk_migrate', 'endpoint_validate',
        'endpoint_activate', 'installation_revoke', 'certificate_rotate', 'rebuild_projection'
    )),
    status varchar(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled', 'timed_out')),
    target_type varchar(40) NOT NULL,
    target_id uuid,
    request_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    progress_completed integer NOT NULL DEFAULT 0 CHECK (progress_completed >= 0),
    progress_total integer NOT NULL DEFAULT 0 CHECK (progress_total >= progress_completed),
    idempotency_key varchar(200),
    error_code varchar(100),
    error_message varchar(500),
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX relay_operations_actor_idempotency_idx
    ON relay_operations (created_by, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX relay_operations_status_created_idx ON relay_operations (status, created_at);

CREATE TABLE relay_operation_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    operation_id uuid NOT NULL REFERENCES relay_operations(id) ON DELETE CASCADE,
    target_type varchar(40) NOT NULL,
    target_id uuid,
    status varchar(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'skipped')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    error_code varchar(100),
    error_message varchar(500),
    started_at timestamptz,
    finished_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX relay_operation_items_operation_idx
    ON relay_operation_items (operation_id, status, created_at);

CREATE TABLE relay_connection_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid REFERENCES relay_node_installations(id) ON DELETE SET NULL,
    instance_id uuid REFERENCES relay_node_instances(id) ON DELETE SET NULL,
    event_type varchar(40) NOT NULL,
    result varchar(20) NOT NULL CHECK (result IN ('accepted', 'rejected', 'closed')),
    reason_code varchar(100) NOT NULL DEFAULT '',
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX relay_connection_audit_instance_created_idx
    ON relay_connection_audit (instance_id, created_at DESC);

CREATE TABLE relay_outbox (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type varchar(60) NOT NULL,
    aggregate_id uuid NOT NULL,
    event_type varchar(100) NOT NULL,
    payload jsonb NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at timestamptz NOT NULL DEFAULT now(),
    claimed_at timestamptz,
    published_at timestamptz,
    last_error varchar(500),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX relay_outbox_pending_idx
    ON relay_outbox (available_at, created_at) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS relay_outbox;
DROP TABLE IF EXISTS relay_connection_audit;
DROP TABLE IF EXISTS relay_operation_items;
DROP TABLE IF EXISTS relay_operations;
DROP TABLE IF EXISTS relay_assignment_pins;
DROP TABLE IF EXISTS relay_assignment_events;
DROP TABLE IF EXISTS relay_assignments;
