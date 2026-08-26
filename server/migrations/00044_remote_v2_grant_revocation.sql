-- +goose Up
-- Store only the non-bearer DeviceConnectionGrant ID so an authenticated
-- controller can revoke a proof-bound Grant. The signed Grant, handshake
-- secret, ciphertext and project metadata are deliberately absent.
CREATE TABLE remote_device_link_grants (
    grant_id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    controller_id uuid NOT NULL REFERENCES remote_controller_identities(controller_id) ON DELETE CASCADE,
    device_id uuid NOT NULL REFERENCES remote_device_credentials(device_id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

CREATE INDEX remote_device_link_grants_owner_expiry_idx
    ON remote_device_link_grants (user_id, controller_id, expires_at);

-- +goose Down
DROP TABLE IF EXISTS remote_device_link_grants;
