-- +goose Up
ALTER TABLE release_source_settings
    ADD COLUMN github_token_ciphertext bytea,
    ADD COLUMN github_token_initialized boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT release_source_token_ciphertext_length
        CHECK (
            github_token_ciphertext IS NULL
            OR octet_length(github_token_ciphertext) BETWEEN 30 AND 4096
        );

-- +goose Down
ALTER TABLE release_source_settings
    DROP CONSTRAINT IF EXISTS release_source_token_ciphertext_length,
    DROP COLUMN IF EXISTS github_token_initialized,
    DROP COLUMN IF EXISTS github_token_ciphertext;
