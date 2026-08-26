-- +goose Up
CREATE TABLE release_delivery_settings (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    download_mode varchar(20) NOT NULL DEFAULT 'proxy_cached'
        CHECK (download_mode IN ('proxy_cached', 's3_redirect')),
    s3_url_prefix text NOT NULL DEFAULT '',
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_by uuid REFERENCES users(id) ON DELETE SET NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (download_mode <> 's3_redirect' OR length(s3_url_prefix) > 0)
);

INSERT INTO release_delivery_settings (singleton, download_mode, s3_url_prefix)
VALUES (true, 'proxy_cached', '');

-- +goose Down
DROP TABLE IF EXISTS release_delivery_settings;
