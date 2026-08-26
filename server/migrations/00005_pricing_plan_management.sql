-- +goose Up
ALTER TABLE pricing_plans
    ADD COLUMN published_version bigint CHECK (published_version IS NULL OR published_version > 0);

-- Every mutation stores a complete immutable snapshot. The public catalog reads
-- only the snapshot selected by pricing_plans.published_version, so editing a
-- live plan cannot change the website until an explicit publish action occurs.
CREATE TABLE pricing_plan_versions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pricing_plan_id uuid NOT NULL REFERENCES pricing_plans(id) ON DELETE RESTRICT,
    version bigint NOT NULL CHECK (version > 0),
    code varchar(40) NOT NULL,
    name varchar(80) NOT NULL,
    description varchar(500) NOT NULL DEFAULT '',
    price_minor bigint CHECK (price_minor IS NULL OR price_minor >= 0),
    currency char(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    billing_period varchar(20) NOT NULL CHECK (billing_period IN ('free', 'month', 'year', 'one_time', 'redemption')),
    features_json jsonb NOT NULL CHECK (jsonb_typeof(features_json) = 'array'),
    status varchar(20) NOT NULL CHECK (status IN ('draft', 'published', 'archived')),
    sort_order integer NOT NULL CHECK (sort_order BETWEEN -100000 AND 100000),
    published_at timestamptz,
    change_type varchar(20) NOT NULL CHECK (change_type IN ('migrated', 'create', 'update', 'publish', 'archive')),
    changed_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (pricing_plan_id, version)
);

CREATE INDEX pricing_plan_versions_plan_created_idx
    ON pricing_plan_versions (pricing_plan_id, version DESC);

INSERT INTO pricing_plan_versions (
    pricing_plan_id, version, code, name, description, price_minor, currency,
    billing_period, features_json, status, sort_order, published_at, change_type, created_at
)
SELECT
    id, version, code, name, description, price_minor, currency,
    billing_period, features_json, status, sort_order, published_at, 'migrated', updated_at
FROM pricing_plans;

UPDATE pricing_plans
SET published_version = version
WHERE status IN ('published', 'archived');

-- +goose Down
DROP TABLE IF EXISTS pricing_plan_versions;
ALTER TABLE pricing_plans DROP COLUMN IF EXISTS published_version;
