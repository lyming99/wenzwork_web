-- +goose Up
ALTER TABLE account_login_events
    ADD COLUMN app_session_id uuid UNIQUE REFERENCES app_sessions(id) ON DELETE SET NULL,
    ADD COLUMN login_method varchar(20) NOT NULL DEFAULT 'password'
        CHECK (login_method IN ('password', 'app_device')),
    ADD CONSTRAINT account_login_events_session_kind_check CHECK (
        (login_method = 'password' AND session_id IS NOT NULL AND app_session_id IS NULL)
        OR (login_method = 'app_device' AND session_id IS NULL AND app_session_id IS NOT NULL)
    );

CREATE INDEX account_login_events_method_occurred_idx
    ON account_login_events (login_method, occurred_at DESC);

-- +goose Down
DROP INDEX IF EXISTS account_login_events_method_occurred_idx;
ALTER TABLE account_login_events
    DROP CONSTRAINT IF EXISTS account_login_events_session_kind_check,
    DROP COLUMN IF EXISTS login_method,
    DROP COLUMN IF EXISTS app_session_id;
