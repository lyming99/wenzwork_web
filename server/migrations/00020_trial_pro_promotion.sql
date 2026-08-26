-- +goose Up
CREATE TABLE trial_promotion_settings (
    singleton smallint PRIMARY KEY DEFAULT 1 CHECK (singleton = 1),
    batch_id uuid NOT NULL UNIQUE REFERENCES redemption_code_batches(id) ON DELETE RESTRICT,
    enabled boolean NOT NULL DEFAULT true,
    daily_quota integer NOT NULL DEFAULT 100 CHECK (daily_quota BETWEEN 1 AND 5000),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE trial_promotion_days (
    claim_date date PRIMARY KEY,
    quota integer NOT NULL CHECK (quota BETWEEN 1 AND 5000),
    claimed_count integer NOT NULL DEFAULT 0 CHECK (
        claimed_count >= 0 AND claimed_count <= quota
    ),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE trial_promotion_claims (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email varchar(320) NOT NULL,
    claim_date date NOT NULL REFERENCES trial_promotion_days(claim_date) ON DELETE RESTRICT,
    code_id uuid NOT NULL UNIQUE REFERENCES redemption_codes(id) ON DELETE RESTRICT,
    code_ciphertext bytea,
    client_ip_digest char(64) NOT NULL,
    delivery_status varchar(20) NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending', 'sent', 'failed')),
    delivery_attempts integer NOT NULL DEFAULT 1 CHECK (delivery_attempts > 0),
    last_delivery_attempt_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (delivery_status = 'sent' AND sent_at IS NOT NULL AND code_ciphertext IS NULL)
        OR (delivery_status <> 'sent' AND sent_at IS NULL AND code_ciphertext IS NOT NULL)
    )
);

CREATE UNIQUE INDEX trial_promotion_claims_email_key
    ON trial_promotion_claims (lower(email));
CREATE INDEX trial_promotion_claims_date_created_idx
    ON trial_promotion_claims (claim_date, created_at DESC);
CREATE INDEX trial_promotion_claims_ip_created_idx
    ON trial_promotion_claims (client_ip_digest, created_at DESC);

WITH trial_batch AS (
    INSERT INTO redemption_code_batches (
        name,
        plan_id,
        grant_type,
        grant_days,
        quantity,
        status,
        note,
        created_by
    )
    SELECT
        'WenzWork 官网每日 30 天 Pro 试用',
        id,
        'duration',
        30,
        100,
        'active',
        'system:trial-pro-daily',
        NULL
    FROM membership_plans
    WHERE code = 'pro' AND status = 'active'
    RETURNING id
)
INSERT INTO trial_promotion_settings (batch_id, enabled, daily_quota)
SELECT id, true, 100
FROM trial_batch;

-- +goose Down
DROP TABLE IF EXISTS trial_promotion_claims;
DROP TABLE IF EXISTS trial_promotion_days;
DROP TABLE IF EXISTS trial_promotion_settings;

DELETE FROM redemption_codes
WHERE batch_id IN (
    SELECT id
    FROM redemption_code_batches
    WHERE note = 'system:trial-pro-daily'
);

DELETE FROM redemption_code_batches
WHERE note = 'system:trial-pro-daily';
