-- +goose Up
ALTER TABLE relay_assignments
    ADD COLUMN source_operation_id uuid REFERENCES relay_operations(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX relay_assignments_source_operation_idx
    ON relay_assignments (source_operation_id)
    WHERE source_operation_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS relay_assignments_source_operation_idx;
ALTER TABLE relay_assignments DROP COLUMN IF EXISTS source_operation_id;
