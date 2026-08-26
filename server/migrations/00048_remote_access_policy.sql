-- +goose Up
CREATE TABLE remote_access_policy_settings (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    device_limit integer NOT NULL DEFAULT 10 CHECK (device_limit BETWEEN 1 AND 100000),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO remote_access_policy_settings (singleton, device_limit)
VALUES (true, 10);

-- +goose Down
DROP TABLE IF EXISTS remote_access_policy_settings;
