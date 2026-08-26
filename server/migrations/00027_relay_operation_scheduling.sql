-- +goose Up
ALTER TABLE relay_operations
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now();

DROP INDEX relay_operations_claim_idx;
CREATE INDEX relay_operations_claim_idx
    ON relay_operations (status, next_attempt_at, worker_heartbeat_at, request_sequence)
    WHERE status IN ('pending', 'running');

-- +goose Down
DROP INDEX IF EXISTS relay_operations_claim_idx;
ALTER TABLE relay_operations DROP COLUMN IF EXISTS next_attempt_at;
CREATE INDEX relay_operations_claim_idx
    ON relay_operations (status, worker_heartbeat_at, created_at)
    WHERE status IN ('pending', 'running');
