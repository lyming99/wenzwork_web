-- +goose Up
CREATE TABLE pricing_plans (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code varchar(40) NOT NULL UNIQUE,
    name varchar(80) NOT NULL,
    description varchar(500) NOT NULL DEFAULT '',
    price_minor bigint CHECK (price_minor IS NULL OR price_minor >= 0),
    currency char(3) NOT NULL DEFAULT 'CNY' CHECK (currency ~ '^[A-Z]{3}$'),
    billing_period varchar(20) NOT NULL CHECK (billing_period IN ('free', 'month', 'year', 'one_time', 'redemption')),
    features_json jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(features_json) = 'array'),
    status varchar(20) NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'archived')),
    sort_order integer NOT NULL DEFAULT 0,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (status = 'draft' OR published_at IS NOT NULL)
);

CREATE INDEX pricing_plans_public_order_idx
    ON pricing_plans (sort_order, code)
    WHERE status = 'published';

INSERT INTO pricing_plans (
    code, name, description, price_minor, currency, billing_period,
    features_json, status, sort_order, published_at
) VALUES
    (
        'free', 'Free', '适合体验 WenzWork 核心写作流程。', 0, 'CNY', 'free',
        '["本地 Markdown 文件", "基础编辑与预览", "公开帮助与版本更新"]'::jsonb,
        'published', 10, now()
    ),
    (
        'pro', 'Pro', '为需要进阶能力与持续服务的创作者准备。', NULL, 'CNY', 'redemption',
        '["包含 Free 全部能力", "Pro 会员权益（发布前公布）", "兑换有效期安全顺延"]'::jsonb,
        'published', 20, now()
    );

CREATE TABLE releases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version varchar(50) NOT NULL UNIQUE,
    channel varchar(20) NOT NULL DEFAULT 'stable' CHECK (channel IN ('stable', 'beta')),
    title varchar(120) NOT NULL,
    summary varchar(1000) NOT NULL DEFAULT '',
    release_notes text NOT NULL DEFAULT '',
    status varchar(20) NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'withdrawn')),
    published_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (status = 'draft' OR published_at IS NOT NULL)
);

CREATE INDEX releases_public_latest_idx
    ON releases (channel, published_at DESC, id DESC)
    WHERE status = 'published';

CREATE TABLE release_assets (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id uuid NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
    platform varchar(20) NOT NULL CHECK (platform IN ('windows', 'macos', 'linux')),
    architecture varchar(20) NOT NULL CHECK (architecture IN ('x64', 'arm64', 'universal')),
    file_name varchar(255) NOT NULL,
    file_size_bytes bigint NOT NULL CHECK (file_size_bytes > 0),
    sha256 char(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    signature_status varchar(20) NOT NULL DEFAULT 'unknown' CHECK (signature_status IN ('unknown', 'unsigned', 'valid')),
    object_key varchar(1024) NOT NULL UNIQUE,
    download_url text NOT NULL,
    status varchar(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'verified', 'published', 'withdrawn')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (release_id, platform, architecture)
);

CREATE INDEX release_assets_public_filter_idx
    ON release_assets (release_id, platform, architecture)
    WHERE status = 'published';

-- +goose Down
DROP TABLE IF EXISTS release_assets;
DROP TABLE IF EXISTS releases;
DROP TABLE IF EXISTS pricing_plans;
