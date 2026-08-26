-- +goose Up
ALTER TABLE users
    ADD COLUMN password_changed_at timestamptz NOT NULL DEFAULT now();

CREATE TABLE sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    csrf_token_hash char(64) NOT NULL CHECK (csrf_token_hash ~ '^[0-9a-f]{64}$'),
    user_agent_summary varchar(255) NOT NULL DEFAULT '',
    remember_me boolean NOT NULL DEFAULT false,
    assurance_level smallint NOT NULL DEFAULT 1 CHECK (assurance_level IN (1, 2)),
    last_seen_at timestamptz NOT NULL,
    idle_expires_at timestamptz NOT NULL,
    absolute_expires_at timestamptz NOT NULL,
    mfa_verified_at timestamptz,
    revoked_at timestamptz,
    revoked_reason varchar(120),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (idle_expires_at > created_at),
    CHECK (absolute_expires_at > created_at),
    CHECK (idle_expires_at <= absolute_expires_at),
    CHECK ((revoked_at IS NULL AND revoked_reason IS NULL) OR (revoked_at IS NOT NULL AND revoked_reason IS NOT NULL)),
    CHECK ((assurance_level = 1 AND mfa_verified_at IS NULL) OR assurance_level = 2)
);

CREATE INDEX sessions_user_active_idx
    ON sessions (user_id, last_seen_at DESC)
    WHERE revoked_at IS NULL;
CREATE INDEX sessions_expiry_idx
    ON sessions (idle_expires_at, absolute_expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE email_verification_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (used_at IS NULL OR used_at >= created_at)
);

CREATE INDEX email_verification_tokens_user_active_idx
    ON email_verification_tokens (user_id, expires_at DESC)
    WHERE used_at IS NULL;

CREATE TABLE password_reset_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash char(64) NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK (used_at IS NULL OR used_at >= created_at)
);

CREATE INDEX password_reset_tokens_user_active_idx
    ON password_reset_tokens (user_id, expires_at DESC)
    WHERE used_at IS NULL;

CREATE TABLE mfa_totp_credentials (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    secret_ciphertext bytea NOT NULL,
    verified_at timestamptz,
    last_used_step bigint NOT NULL DEFAULT -1 CHECK (last_used_step >= -1),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE mfa_recovery_codes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash text NOT NULL,
    used_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX mfa_recovery_codes_user_unused_idx
    ON mfa_recovery_codes (user_id, created_at DESC)
    WHERE used_at IS NULL;

CREATE TABLE auth_rate_limits (
    scope varchar(40) NOT NULL,
    key_digest char(64) NOT NULL CHECK (key_digest ~ '^[0-9a-f]{64}$'),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    window_started_at timestamptz NOT NULL,
    blocked_until timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, key_digest)
);

CREATE INDEX auth_rate_limits_cleanup_idx ON auth_rate_limits (updated_at);

-- +goose Down
DROP TABLE IF EXISTS auth_rate_limits;
DROP TABLE IF EXISTS mfa_recovery_codes;
DROP TABLE IF EXISTS mfa_totp_credentials;
DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS email_verification_tokens;
DROP TABLE IF EXISTS sessions;
ALTER TABLE users DROP COLUMN IF EXISTS password_changed_at;
