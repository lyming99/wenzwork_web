-- +goose Up
-- A direct endpoint is Agent-owned runtime metadata. direct_mode_enabled is
-- account-owned preference and deliberately survives an Agent restart or a
-- temporary direct-listener outage.
ALTER TABLE remote_device_credentials
    ADD COLUMN direct_mode_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN direct_endpoint_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN direct_tls_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN direct_ip varchar(45) NOT NULL DEFAULT '',
    ADD COLUMN direct_port integer NOT NULL DEFAULT 0,
    ADD COLUMN direct_connection_epoch bigint NOT NULL DEFAULT 0,
    ADD COLUMN direct_last_seen_at timestamptz,
    ADD CONSTRAINT remote_device_direct_endpoint_check CHECK (
        (
            direct_endpoint_enabled
            AND direct_ip <> ''
            AND direct_port BETWEEN 1 AND 65535
            AND direct_connection_epoch > 0
            AND direct_last_seen_at IS NOT NULL
        ) OR (
            NOT direct_endpoint_enabled
            AND NOT direct_tls_enabled
            AND direct_ip = ''
            AND direct_port = 0
            AND direct_connection_epoch = 0
            AND direct_last_seen_at IS NULL
        )
    );

CREATE INDEX remote_device_direct_presence_idx
    ON remote_device_credentials (direct_last_seen_at DESC, device_id)
    WHERE direct_endpoint_enabled;

-- +goose Down
DROP INDEX IF EXISTS remote_device_direct_presence_idx;
ALTER TABLE remote_device_credentials
    DROP CONSTRAINT IF EXISTS remote_device_direct_endpoint_check,
    DROP COLUMN IF EXISTS direct_last_seen_at,
    DROP COLUMN IF EXISTS direct_connection_epoch,
    DROP COLUMN IF EXISTS direct_port,
    DROP COLUMN IF EXISTS direct_ip,
    DROP COLUMN IF EXISTS direct_tls_enabled,
    DROP COLUMN IF EXISTS direct_endpoint_enabled,
    DROP COLUMN IF EXISTS direct_mode_enabled;
