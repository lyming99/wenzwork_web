-- +goose Up
CREATE TABLE website_visitors (
    client_ip inet PRIMARY KEY,
    first_seen_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    page_views bigint NOT NULL CHECK (page_views > 0),
    CHECK (last_seen_at >= first_seen_at)
);

INSERT INTO website_visitors (client_ip, first_seen_at, last_seen_at, page_views)
SELECT client_ip, MIN(occurred_at), MAX(occurred_at), COUNT(*)::bigint
FROM website_page_views
GROUP BY client_ip;

CREATE INDEX website_visitors_first_seen_idx
    ON website_visitors (first_seen_at DESC);
CREATE INDEX website_visitors_last_seen_idx
    ON website_visitors (last_seen_at DESC);

CREATE TABLE release_download_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    asset_id uuid REFERENCES release_assets(id) ON DELETE SET NULL,
    client_ip inet NOT NULL,
    user_agent_summary varchar(255) NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX release_download_events_occurred_idx
    ON release_download_events (occurred_at DESC);
CREATE INDEX release_download_events_ip_occurred_idx
    ON release_download_events (client_ip, occurred_at DESC);
CREATE INDEX release_download_events_asset_occurred_idx
    ON release_download_events (asset_id, occurred_at DESC)
    WHERE asset_id IS NOT NULL;

CREATE TABLE account_registration_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id uuid NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    client_ip inet NOT NULL,
    user_agent_summary varchar(255) NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX account_registration_events_occurred_idx
    ON account_registration_events (occurred_at DESC);
CREATE INDEX account_registration_events_ip_occurred_idx
    ON account_registration_events (client_ip, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS account_registration_events;
DROP TABLE IF EXISTS release_download_events;
DROP TABLE IF EXISTS website_visitors;
