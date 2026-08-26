-- +goose Up
CREATE TABLE release_access_key_settings (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    access_key_digest varchar(64),
    access_key_prefix varchar(16) NOT NULL DEFAULT '',
    initialized boolean NOT NULL DEFAULT false,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT release_access_key_digest_shape CHECK (
        (initialized = false AND access_key_digest IS NULL AND access_key_prefix = '')
        OR (
            initialized = true
            AND access_key_digest IS NOT NULL
            AND access_key_digest ~ '^[0-9a-f]{64}$'
            AND access_key_prefix ~ '^release_[A-Za-z0-9_-]{8}$'
        )
    )
);

INSERT INTO release_access_key_settings (singleton, initialized)
VALUES (true, false);

-- +goose Down
DROP TABLE IF EXISTS release_access_key_settings;
