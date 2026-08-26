-- +goose Up
CREATE SEQUENCE remote_task_change_sequence AS bigint;

ALTER TABLE remote_tasks
    ADD COLUMN change_sequence bigint;

UPDATE remote_tasks
SET change_sequence = nextval('remote_task_change_sequence');

ALTER TABLE remote_tasks
    ALTER COLUMN change_sequence SET NOT NULL,
    ALTER COLUMN change_sequence SET DEFAULT nextval('remote_task_change_sequence');

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION assign_remote_task_change_sequence()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.change_sequence := nextval('remote_task_change_sequence');
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER remote_tasks_change_sequence_trigger
BEFORE INSERT OR UPDATE ON remote_tasks
FOR EACH ROW
EXECUTE FUNCTION assign_remote_task_change_sequence();

CREATE INDEX remote_tasks_user_device_change_idx
    ON remote_tasks (user_id, device_id, change_sequence DESC);

-- +goose Down
DROP INDEX IF EXISTS remote_tasks_user_device_change_idx;
DROP TRIGGER IF EXISTS remote_tasks_change_sequence_trigger ON remote_tasks;
DROP FUNCTION IF EXISTS assign_remote_task_change_sequence();
ALTER TABLE remote_tasks DROP COLUMN IF EXISTS change_sequence;
DROP SEQUENCE IF EXISTS remote_task_change_sequence;
