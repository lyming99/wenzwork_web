-- +goose Up
ALTER TABLE remote_device_credentials
    ADD COLUMN key_version bigint NOT NULL DEFAULT 1 CHECK (key_version > 0),
    ADD COLUMN scopes jsonb NOT NULL DEFAULT '["remote.connect"]'::jsonb
        CHECK (jsonb_typeof(scopes) = 'array');

ALTER TABLE remote_device_request_keys
    DROP CONSTRAINT remote_device_request_keys_operation_check,
    ADD CONSTRAINT remote_device_request_keys_operation_check
        CHECK (operation IN ('registration', 'allocation', 'allocation_refresh', 'key_rotation'));

CREATE TABLE remote_device_access_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label varchar(120) NOT NULL,
    key_prefix varchar(20) NOT NULL,
    key_digest char(64) NOT NULL UNIQUE
        CHECK (key_digest ~ '^[0-9a-f]{64}$'),
    scopes jsonb NOT NULL
        CHECK (jsonb_typeof(scopes) = 'array'),
    bound_device_id uuid,
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'revoked')),
    expires_at timestamptz,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at IS NULL OR expires_at > created_at),
    CHECK (
        (status = 'active' AND revoked_at IS NULL)
        OR (status = 'revoked' AND revoked_at IS NOT NULL)
    )
);

CREATE INDEX remote_device_access_keys_user_status_idx
    ON remote_device_access_keys (user_id, status, created_at DESC);
CREATE UNIQUE INDEX remote_device_access_keys_active_device_idx
    ON remote_device_access_keys (user_id, bound_device_id)
    WHERE status = 'active' AND bound_device_id IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS remote_device_access_keys;
ALTER TABLE remote_device_request_keys
    DROP CONSTRAINT remote_device_request_keys_operation_check,
    ADD CONSTRAINT remote_device_request_keys_operation_check
        CHECK (operation IN ('registration', 'allocation', 'allocation_refresh'));
ALTER TABLE remote_device_credentials
    DROP COLUMN IF EXISTS scopes,
    DROP COLUMN IF EXISTS key_version;
