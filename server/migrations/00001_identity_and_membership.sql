-- +goose Up
CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email varchar(320) NOT NULL,
    password_hash text NOT NULL,
    display_name varchar(120) NOT NULL DEFAULT '',
    status varchar(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'pending')),
    email_verified_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX users_email_normalized_key ON users (lower(email));

CREATE TABLE roles (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code varchar(40) NOT NULL UNIQUE,
    name varchar(80) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO roles (code, name) VALUES
    ('user', '普通用户'),
    ('support_admin', '用户支持管理员'),
    ('content_admin', '内容管理员'),
    ('membership_admin', '会员管理员'),
    ('release_admin', '版本发布管理员'),
    ('super_admin', '超级管理员');

CREATE TABLE user_roles (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id uuid NOT NULL REFERENCES roles(id) ON DELETE RESTRICT,
    granted_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role_id)
);

CREATE TABLE membership_plans (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code varchar(40) NOT NULL UNIQUE,
    name varchar(80) NOT NULL,
    rank integer NOT NULL CHECK (rank >= 0),
    features_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    status varchar(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO membership_plans (code, name, rank, features_json) VALUES
    ('free', 'Free', 0, '["基础功能"]'::jsonb),
    ('pro', 'Pro', 10, '["完整专业功能"]'::jsonb);

CREATE TABLE memberships (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    plan_id uuid NOT NULL REFERENCES membership_plans(id) ON DELETE RESTRICT,
    starts_at timestamptz NOT NULL,
    expires_at timestamptz,
    source varchar(30) NOT NULL CHECK (source IN ('redemption_code', 'admin_adjustment', 'system')),
    status varchar(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'revoked')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at IS NULL OR expires_at > starts_at)
);

CREATE INDEX memberships_plan_status_idx ON memberships (plan_id, status);
CREATE INDEX memberships_expires_at_idx ON memberships (expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE redemption_code_batches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name varchar(120) NOT NULL,
    plan_id uuid NOT NULL REFERENCES membership_plans(id) ON DELETE RESTRICT,
    grant_type varchar(20) NOT NULL CHECK (grant_type IN ('duration', 'lifetime')),
    grant_days integer,
    quantity integer NOT NULL CHECK (quantity BETWEEN 1 AND 5000),
    redeem_before timestamptz,
    status varchar(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked', 'exhausted')),
    note text NOT NULL DEFAULT '',
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (grant_type = 'duration' AND grant_days > 0)
        OR (grant_type = 'lifetime' AND grant_days IS NULL)
    )
);

CREATE INDEX redemption_code_batches_status_idx ON redemption_code_batches (status, created_at DESC);

CREATE TABLE redemption_codes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id uuid NOT NULL REFERENCES redemption_code_batches(id) ON DELETE RESTRICT,
    code_digest char(64) NOT NULL UNIQUE,
    code_hint char(4) NOT NULL,
    status varchar(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'redeemed', 'revoked')),
    redeemed_by uuid REFERENCES users(id) ON DELETE RESTRICT,
    redeemed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (status = 'redeemed' AND redeemed_by IS NOT NULL AND redeemed_at IS NOT NULL)
        OR (status <> 'redeemed' AND redeemed_by IS NULL AND redeemed_at IS NULL)
    )
);

CREATE INDEX redemption_codes_batch_status_idx ON redemption_codes (batch_id, status);
CREATE INDEX redemption_codes_redeemed_by_idx ON redemption_codes (redeemed_by, redeemed_at DESC) WHERE redeemed_by IS NOT NULL;

CREATE TABLE membership_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    event_type varchar(40) NOT NULL CHECK (event_type IN ('redemption', 'admin_adjustment', 'expiration', 'revocation')),
    source_type varchar(40) NOT NULL,
    source_id uuid,
    before_json jsonb,
    after_json jsonb NOT NULL,
    reason text NOT NULL DEFAULT '',
    actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX membership_events_user_created_idx ON membership_events (user_id, created_at DESC);

CREATE TABLE audit_logs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    action varchar(100) NOT NULL,
    resource_type varchar(80) NOT NULL,
    resource_id uuid,
    before_json jsonb,
    after_json jsonb,
    request_id uuid,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_actor_created_idx ON audit_logs (actor_user_id, created_at DESC);
CREATE INDEX audit_logs_resource_idx ON audit_logs (resource_type, resource_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS membership_events;
DROP TABLE IF EXISTS redemption_codes;
DROP TABLE IF EXISTS redemption_code_batches;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS membership_plans;
DROP TABLE IF EXISTS user_roles;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS users;

