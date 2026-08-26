-- +goose Up
CREATE TABLE ip_geolocation_cache (
    client_ip inet PRIMARY KEY,
    country_code varchar(2) NOT NULL DEFAULT '',
    country_name varchar(120) NOT NULL DEFAULT '',
    region_name varchar(160) NOT NULL DEFAULT '',
    city_name varchar(160) NOT NULL DEFAULT '',
    source varchar(32) NOT NULL DEFAULT '',
    lookup_status varchar(20) NOT NULL
        CHECK (lookup_status IN ('resolved', 'not_found', 'failed')),
    resolved_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > resolved_at)
);

CREATE INDEX ip_geolocation_cache_expires_idx
    ON ip_geolocation_cache (expires_at);

-- +goose Down
DROP TABLE IF EXISTS ip_geolocation_cache;
