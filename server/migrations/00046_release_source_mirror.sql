-- +goose Up
ALTER TABLE release_source_settings
    ADD COLUMN mirror_base_url varchar(2048) NOT NULL DEFAULT '',
    ADD CONSTRAINT release_source_mirror_base_url_length
        CHECK (length(mirror_base_url) <= 2048);

-- +goose Down
ALTER TABLE release_source_settings
    DROP CONSTRAINT IF EXISTS release_source_mirror_base_url_length,
    DROP COLUMN IF EXISTS mirror_base_url;
