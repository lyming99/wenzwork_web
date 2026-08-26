-- +goose Up
CREATE TABLE release_source_settings (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    github_repository varchar(200) NOT NULL DEFAULT ''
        CHECK (length(github_repository) <= 200),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO release_source_settings (singleton, github_repository)
VALUES (true, '');

-- +goose Down
DROP TABLE IF EXISTS release_source_settings;
