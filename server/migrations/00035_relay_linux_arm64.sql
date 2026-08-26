-- +goose Up
-- Relay Server remains a Linux/systemd service. This migration adds arm64 as
-- a second binary target while preserving the existing amd64 rows and all
-- release-artifact foreign-key/uniqueness guarantees.
ALTER TABLE relay_server_releases
    DROP CONSTRAINT IF EXISTS relay_server_releases_architecture_check,
    ADD CONSTRAINT relay_server_releases_architecture_check
        CHECK (architecture IN ('amd64', 'arm64'));

ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_architecture_check,
    ADD CONSTRAINT relay_node_installations_architecture_check
        CHECK (architecture IN ('amd64', 'arm64'));

ALTER TABLE relay_node_enrollment_tokens
    DROP CONSTRAINT IF EXISTS relay_node_enrollment_tokens_architecture_check,
    ADD CONSTRAINT relay_node_enrollment_tokens_architecture_check
        CHECK (architecture IN ('amd64', 'arm64'));

-- +goose Down
-- NOT VALID preserves any arm64 history during a rollback while restoring the
-- old amd64-only rule for new or updated rows. Operators must migrate arm64
-- records before explicitly validating these constraints.
ALTER TABLE relay_node_enrollment_tokens
    DROP CONSTRAINT IF EXISTS relay_node_enrollment_tokens_architecture_check,
    ADD CONSTRAINT relay_node_enrollment_tokens_architecture_check
        CHECK (architecture IN ('amd64')) NOT VALID;

ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_architecture_check,
    ADD CONSTRAINT relay_node_installations_architecture_check
        CHECK (architecture IN ('amd64')) NOT VALID;

ALTER TABLE relay_server_releases
    DROP CONSTRAINT IF EXISTS relay_server_releases_architecture_check,
    ADD CONSTRAINT relay_server_releases_architecture_check
        CHECK (architecture IN ('amd64')) NOT VALID;
