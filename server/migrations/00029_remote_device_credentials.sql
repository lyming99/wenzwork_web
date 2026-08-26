-- +goose Up
CREATE TABLE remote_device_credentials (
    device_id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    registered_session_id uuid NOT NULL REFERENCES app_sessions(id) ON DELETE RESTRICT,
    device_name varchar(120) NOT NULL,
    platform varchar(20) NOT NULL CHECK (platform IN ('windows', 'macos', 'linux')),
    agent_version varchar(64) NOT NULL,
    protocol_min integer NOT NULL CHECK (protocol_min > 0),
    protocol_max integer NOT NULL CHECK (protocol_max >= protocol_min),
    capabilities jsonb NOT NULL DEFAULT '["relay.ping"]'::jsonb
        CHECK (jsonb_typeof(capabilities) = 'array'),
    identity_public_key bytea NOT NULL CHECK (octet_length(identity_public_key) = 32),
    public_key_thumbprint char(43) NOT NULL
        CHECK (public_key_thumbprint ~ '^[A-Za-z0-9_-]{43}$'),
    grant_version bigint NOT NULL DEFAULT 1 CHECK (grant_version > 0),
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'revoked', 'quarantined')),
    last_connection_epoch bigint NOT NULL DEFAULT 0 CHECK (last_connection_epoch >= 0),
    last_allocation_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX remote_device_credentials_user_status_idx
    ON remote_device_credentials (user_id, status, updated_at DESC);

-- Idempotency records bind a key to the authenticated principal, operation and
-- request digest without persisting short-lived Relay tickets.
CREATE TABLE remote_device_request_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id uuid NOT NULL,
    operation varchar(32) NOT NULL
        CHECK (operation IN ('registration', 'allocation', 'allocation_refresh')),
    idempotency_key varchar(128) NOT NULL
        CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{8,128}$'),
    request_hash char(64) NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    assignment_id uuid REFERENCES relay_assignments(id) ON DELETE SET NULL,
    outbox_id uuid REFERENCES relay_outbox(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    CHECK (expires_at > created_at),
    UNIQUE (user_id, device_id, operation, idempotency_key)
);

CREATE INDEX remote_device_request_keys_expiry_idx
    ON remote_device_request_keys (expires_at);

-- +goose Down
DROP TABLE IF EXISTS remote_device_request_keys;
DROP TABLE IF EXISTS remote_device_credentials;
