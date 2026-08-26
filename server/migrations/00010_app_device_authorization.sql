-- +goose Up
CREATE TABLE app_authorization_requests (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id varchar(80) NOT NULL,
    device_code_hash char(64) NOT NULL UNIQUE CHECK (device_code_hash ~ '^[0-9a-f]{64}$'),
    user_code_hash char(64) NOT NULL UNIQUE CHECK (user_code_hash ~ '^[0-9a-f]{64}$'),
    device_id uuid NOT NULL,
    device_name varchar(120) NOT NULL,
    scope varchar(255) NOT NULL,
    status varchar(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'denied', 'consumed')),
    user_id uuid REFERENCES users(id) ON DELETE CASCADE,
    poll_interval_seconds smallint NOT NULL DEFAULT 5
        CHECK (poll_interval_seconds BETWEEN 1 AND 60),
    last_polled_at timestamptz,
    approved_at timestamptz,
    consumed_at timestamptz,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (
        (status = 'pending' AND user_id IS NULL AND approved_at IS NULL AND consumed_at IS NULL)
        OR (status = 'approved' AND user_id IS NOT NULL AND approved_at IS NOT NULL AND consumed_at IS NULL)
        OR (status = 'denied' AND user_id IS NOT NULL AND approved_at IS NULL AND consumed_at IS NULL)
        OR (status = 'consumed' AND user_id IS NOT NULL AND approved_at IS NOT NULL AND consumed_at IS NOT NULL)
    )
);

CREATE INDEX app_authorization_requests_expiry_idx
    ON app_authorization_requests (expires_at)
    WHERE status IN ('pending', 'approved');

CREATE TABLE app_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id varchar(80) NOT NULL,
    device_id uuid NOT NULL,
    device_name varchar(120) NOT NULL,
    scope varchar(255) NOT NULL,
    last_seen_at timestamptz NOT NULL,
    idle_expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    revoked_reason varchar(120),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (idle_expires_at > created_at),
    CHECK ((revoked_at IS NULL AND revoked_reason IS NULL) OR (revoked_at IS NOT NULL AND revoked_reason IS NOT NULL))
);

CREATE UNIQUE INDEX app_sessions_active_device_idx
    ON app_sessions (user_id, client_id, device_id)
    WHERE revoked_at IS NULL;
CREATE INDEX app_sessions_user_active_idx
    ON app_sessions (user_id, last_seen_at DESC)
    WHERE revoked_at IS NULL;
CREATE INDEX app_sessions_expiry_idx
    ON app_sessions (idle_expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE app_access_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES app_sessions(id) ON DELETE CASCADE,
    token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

CREATE INDEX app_access_tokens_expiry_idx ON app_access_tokens (expires_at);

CREATE TABLE app_refresh_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL REFERENCES app_sessions(id) ON DELETE CASCADE,
    token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'rotated', 'revoked')),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    replaced_by uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (
        (status = 'active' AND used_at IS NULL AND replaced_by IS NULL)
        OR (status = 'rotated' AND used_at IS NOT NULL AND replaced_by IS NOT NULL)
        OR status = 'revoked'
    )
);

CREATE INDEX app_refresh_tokens_session_status_idx
    ON app_refresh_tokens (session_id, status);
CREATE INDEX app_refresh_tokens_expiry_idx
    ON app_refresh_tokens (expires_at)
    WHERE status = 'active';

-- +goose Down
DROP TABLE IF EXISTS app_refresh_tokens;
DROP TABLE IF EXISTS app_access_tokens;
DROP TABLE IF EXISTS app_sessions;
DROP TABLE IF EXISTS app_authorization_requests;
