-- +goose Up
CREATE TABLE remote_device_access_key_request_keys (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    operation varchar(24) NOT NULL
        CHECK (operation IN ('create', 'rotate')),
    resource_id uuid NOT NULL,
    idempotency_key varchar(128) NOT NULL
        CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{8,128}$'),
    request_digest char(64) NOT NULL
        CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    response_ciphertext bytea NOT NULL
        CHECK (octet_length(response_ciphertext) BETWEEN 32 AND 16384),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, operation, resource_id, idempotency_key)
);

CREATE INDEX remote_device_access_key_request_keys_created_idx
    ON remote_device_access_key_request_keys (created_at);

COMMENT ON COLUMN remote_device_access_key_request_keys.response_ciphertext IS
    'Versioned AEAD ciphertext of the one-time response; plaintext Device Access Keys are never stored.';

-- +goose Down
DROP TABLE IF EXISTS remote_device_access_key_request_keys;
