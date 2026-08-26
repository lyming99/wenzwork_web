-- +goose Up
CREATE TABLE relay_node_access_keys (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid NOT NULL REFERENCES relay_node_installations(id) ON DELETE CASCADE,
    key_prefix varchar(20) NOT NULL,
    key_digest char(64) NOT NULL UNIQUE CHECK (key_digest ~ '^[0-9a-f]{64}$'),
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'revoked')),
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);

CREATE UNIQUE INDEX relay_node_access_keys_one_active_idx
    ON relay_node_access_keys (installation_id) WHERE status = 'active';
CREATE INDEX relay_node_access_keys_installation_created_idx
    ON relay_node_access_keys (installation_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS relay_node_access_keys;
