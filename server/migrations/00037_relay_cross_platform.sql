-- +goose Up
-- Relay releases remain target-specific: each supported OS/architecture pair
-- has its own native service scripts, binaries, signed manifest and artifact.
ALTER TABLE relay_server_releases
    DROP CONSTRAINT IF EXISTS relay_server_releases_platform_check,
    ADD CONSTRAINT relay_server_releases_platform_check
        CHECK (platform IN ('linux', 'windows', 'darwin'));

ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_platform_check,
    ADD CONSTRAINT relay_node_installations_platform_check
        CHECK (platform IN ('linux', 'windows', 'darwin'));

ALTER TABLE relay_node_enrollment_tokens
    DROP CONSTRAINT IF EXISTS relay_node_enrollment_tokens_platform_check,
    ADD CONSTRAINT relay_node_enrollment_tokens_platform_check
        CHECK (platform IN ('linux', 'windows', 'darwin'));

-- +goose Down
-- Preserve cross-platform history while preventing new non-Linux writes after
-- a rollback. PostgreSQL still enforces NOT VALID checks for new rows.
ALTER TABLE relay_node_enrollment_tokens
    DROP CONSTRAINT IF EXISTS relay_node_enrollment_tokens_platform_check,
    ADD CONSTRAINT relay_node_enrollment_tokens_platform_check
        CHECK (platform IN ('linux')) NOT VALID;

ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_platform_check,
    ADD CONSTRAINT relay_node_installations_platform_check
        CHECK (platform IN ('linux')) NOT VALID;

ALTER TABLE relay_server_releases
    DROP CONSTRAINT IF EXISTS relay_server_releases_platform_check,
    ADD CONSTRAINT relay_server_releases_platform_check
        CHECK (platform IN ('linux')) NOT VALID;
