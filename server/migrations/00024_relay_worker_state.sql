-- +goose Up
ALTER TABLE relay_cell_endpoints
    ADD COLUMN version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    ADD COLUMN drain_until timestamptz,
    ADD COLUMN superseded_at timestamptz;

CREATE UNIQUE INDEX relay_cell_endpoints_public_live_idx
    ON relay_cell_endpoints (public_endpoint)
    WHERE status NOT IN ('retired', 'failed');

ALTER TABLE relay_assignments
    ADD COLUMN fallback_cell_ids jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN effective_at timestamptz,
    ADD COLUMN superseded_at timestamptz,
    ADD CONSTRAINT relay_assignments_fallback_limit_check
        CHECK (jsonb_typeof(fallback_cell_ids) = 'array' AND jsonb_array_length(fallback_cell_ids) <= 8);

ALTER TABLE relay_operations
    DROP CONSTRAINT relay_operations_operation_type_check,
    ADD CONSTRAINT relay_operations_operation_type_check CHECK (operation_type IN (
        'node_drain', 'cell_drain', 'cell_update', 'migrate_user', 'user_unpin',
        'bulk_migrate', 'endpoint_validate', 'endpoint_activate',
        'installation_revoke', 'certificate_rotate', 'rebuild_projection'
    )),
    ADD COLUMN claim_token uuid,
    ADD COLUMN worker_heartbeat_at timestamptz;

CREATE INDEX relay_operations_claim_idx
    ON relay_operations (status, worker_heartbeat_at, created_at)
    WHERE status IN ('pending', 'running');

ALTER TABLE relay_outbox
    ADD COLUMN claim_token uuid;

CREATE INDEX relay_outbox_claim_idx
    ON relay_outbox (claimed_at, available_at, created_at)
    WHERE published_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS relay_outbox_claim_idx;
ALTER TABLE relay_outbox DROP COLUMN IF EXISTS claim_token;

DROP INDEX IF EXISTS relay_operations_claim_idx;
ALTER TABLE relay_operations
    DROP COLUMN IF EXISTS worker_heartbeat_at,
    DROP COLUMN IF EXISTS claim_token,
    DROP CONSTRAINT relay_operations_operation_type_check,
    ADD CONSTRAINT relay_operations_operation_type_check CHECK (operation_type IN (
        'node_drain', 'cell_drain', 'migrate_user', 'bulk_migrate', 'endpoint_validate',
        'endpoint_activate', 'installation_revoke', 'certificate_rotate', 'rebuild_projection'
    ));

ALTER TABLE relay_assignments
    DROP CONSTRAINT IF EXISTS relay_assignments_fallback_limit_check,
    DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS effective_at,
    DROP COLUMN IF EXISTS fallback_cell_ids;

DROP INDEX IF EXISTS relay_cell_endpoints_public_live_idx;
ALTER TABLE relay_cell_endpoints
    DROP COLUMN IF EXISTS superseded_at,
    DROP COLUMN IF EXISTS drain_until,
    DROP COLUMN IF EXISTS version;
