-- +goose Up
ALTER TABLE release_delivery_settings
    DROP CONSTRAINT IF EXISTS release_delivery_settings_download_mode_check;

ALTER TABLE release_delivery_settings
    ADD CONSTRAINT release_delivery_settings_download_mode_check
        CHECK (download_mode IN ('proxy_cached', 's3_redirect', 'github_redirect'));

-- +goose Down
UPDATE release_delivery_settings
SET download_mode = 'proxy_cached'
WHERE download_mode = 'github_redirect';

ALTER TABLE release_delivery_settings
    DROP CONSTRAINT IF EXISTS release_delivery_settings_download_mode_check;

ALTER TABLE release_delivery_settings
    ADD CONSTRAINT release_delivery_settings_download_mode_check
        CHECK (download_mode IN ('proxy_cached', 's3_redirect'));
