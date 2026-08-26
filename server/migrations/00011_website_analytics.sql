-- +goose Up
CREATE TABLE website_page_views (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    path varchar(512) NOT NULL,
    client_ip inet NOT NULL,
    country_code varchar(2) NOT NULL DEFAULT '',
    country_name varchar(120) NOT NULL DEFAULT '',
    region_name varchar(160) NOT NULL DEFAULT '',
    city_name varchar(160) NOT NULL DEFAULT '',
    referrer_host varchar(255) NOT NULL DEFAULT '',
    user_agent_summary varchar(255) NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX website_page_views_occurred_idx
    ON website_page_views (occurred_at DESC);
CREATE INDEX website_page_views_ip_occurred_idx
    ON website_page_views (client_ip, occurred_at DESC);
CREATE INDEX website_page_views_region_occurred_idx
    ON website_page_views (country_code, region_name, city_name, occurred_at DESC);
CREATE INDEX website_page_views_path_occurred_idx
    ON website_page_views (path, occurred_at DESC);

CREATE TABLE account_login_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    session_id uuid UNIQUE REFERENCES sessions(id) ON DELETE SET NULL,
    client_ip inet NOT NULL,
    country_code varchar(2) NOT NULL DEFAULT '',
    country_name varchar(120) NOT NULL DEFAULT '',
    region_name varchar(160) NOT NULL DEFAULT '',
    city_name varchar(160) NOT NULL DEFAULT '',
    user_agent_summary varchar(255) NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX account_login_events_occurred_idx
    ON account_login_events (occurred_at DESC);
CREATE INDEX account_login_events_user_occurred_idx
    ON account_login_events (user_id, occurred_at DESC);
CREATE INDEX account_login_events_ip_occurred_idx
    ON account_login_events (client_ip, occurred_at DESC);

-- +goose Down
DROP TABLE IF EXISTS account_login_events;
DROP TABLE IF EXISTS website_page_views;
