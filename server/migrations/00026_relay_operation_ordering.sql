-- +goose Up
ALTER TABLE relay_operations
    ADD COLUMN request_sequence bigint GENERATED ALWAYS AS IDENTITY;

CREATE UNIQUE INDEX relay_operations_request_sequence_idx
    ON relay_operations (request_sequence);

CREATE INDEX relay_operations_target_order_idx
    ON relay_operations (target_type, target_id, request_sequence)
    WHERE status IN ('pending', 'running');

-- +goose Down
DROP INDEX IF EXISTS relay_operations_target_order_idx;
DROP INDEX IF EXISTS relay_operations_request_sequence_idx;
ALTER TABLE relay_operations DROP COLUMN IF EXISTS request_sequence;
