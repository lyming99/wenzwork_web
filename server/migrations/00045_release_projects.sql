-- +goose Up
ALTER TABLE release_source_settings
    DROP CONSTRAINT release_source_settings_pkey,
    ADD COLUMN project varchar(20);

UPDATE release_source_settings SET project = 'web';

ALTER TABLE release_source_settings
    ALTER COLUMN project SET NOT NULL,
    ADD CONSTRAINT release_source_settings_project_check
        CHECK (project IN ('web', 'desktop', 'mobile')),
    DROP COLUMN singleton,
    ADD PRIMARY KEY (project);

INSERT INTO release_source_settings (project, github_repository, github_token_initialized)
VALUES
    ('desktop', '', false),
    ('mobile', '', false)
ON CONFLICT (project) DO NOTHING;

ALTER TABLE releases
    ADD COLUMN project varchar(20) NOT NULL DEFAULT 'desktop'
        CONSTRAINT releases_project_check CHECK (project IN ('web', 'desktop', 'mobile')),
    DROP CONSTRAINT releases_version_key,
    ADD CONSTRAINT releases_project_version_key UNIQUE (project, version);

DROP INDEX releases_public_latest_idx;
CREATE INDEX releases_public_latest_idx
    ON releases (project, channel, published_at DESC, id DESC)
    WHERE status = 'published';

ALTER TABLE release_assets
    DROP CONSTRAINT release_assets_platform_check,
    DROP CONSTRAINT release_assets_architecture_check,
    DROP CONSTRAINT release_assets_release_id_platform_architecture_key,
    ADD CONSTRAINT release_assets_platform_check
        CHECK (platform IN ('web', 'windows', 'macos', 'linux', 'android', 'ios')),
    ADD CONSTRAINT release_assets_architecture_check
        CHECK (architecture IN ('x64', 'arm64', 'universal'));

-- +goose Down
ALTER TABLE release_assets
    DROP CONSTRAINT release_assets_platform_check,
    DROP CONSTRAINT release_assets_architecture_check,
    ADD CONSTRAINT release_assets_platform_check
        CHECK (platform IN ('windows', 'macos', 'linux')),
    ADD CONSTRAINT release_assets_architecture_check
        CHECK (architecture IN ('x64', 'arm64', 'universal')),
    ADD CONSTRAINT release_assets_release_id_platform_architecture_key
        UNIQUE (release_id, platform, architecture);

DROP INDEX releases_public_latest_idx;
ALTER TABLE releases
    DROP CONSTRAINT releases_project_version_key,
    DROP CONSTRAINT releases_project_check,
    DROP COLUMN project,
    ADD CONSTRAINT releases_version_key UNIQUE (version);
CREATE INDEX releases_public_latest_idx
    ON releases (channel, published_at DESC, id DESC)
    WHERE status = 'published';

DELETE FROM release_source_settings WHERE project <> 'web';
ALTER TABLE release_source_settings
    ADD COLUMN singleton boolean NOT NULL DEFAULT true
        CONSTRAINT release_source_settings_singleton_check CHECK (singleton),
    DROP CONSTRAINT release_source_settings_pkey,
    DROP CONSTRAINT release_source_settings_project_check,
    DROP COLUMN project,
    ADD PRIMARY KEY (singleton);
