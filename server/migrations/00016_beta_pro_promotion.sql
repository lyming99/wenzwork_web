-- +goose Up
ALTER TABLE redemption_code_batches
    ALTER COLUMN created_by DROP NOT NULL;

CREATE TABLE promotion_campaigns (
    code varchar(60) PRIMARY KEY,
    batch_id uuid NOT NULL UNIQUE REFERENCES redemption_code_batches(id) ON DELETE CASCADE,
    quota integer NOT NULL CHECK (quota > 0),
    claimed_count integer NOT NULL DEFAULT 0 CHECK (claimed_count >= 0 AND claimed_count <= quota),
    status varchar(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'exhausted', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE promotion_claims (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_code varchar(60) NOT NULL REFERENCES promotion_campaigns(code) ON DELETE RESTRICT,
    email varchar(320) NOT NULL,
    code_id uuid NOT NULL UNIQUE REFERENCES redemption_codes(id) ON DELETE RESTRICT,
    code_ciphertext bytea,
    client_ip_digest char(64) NOT NULL,
    delivery_status varchar(20) NOT NULL DEFAULT 'pending' CHECK (delivery_status IN ('pending', 'sent', 'failed')),
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

CREATE UNIQUE INDEX promotion_claims_campaign_email_key
    ON promotion_claims (campaign_code, lower(email));
CREATE INDEX promotion_claims_campaign_created_idx
    ON promotion_claims (campaign_code, created_at DESC);
CREATE INDEX promotion_claims_ip_created_idx
    ON promotion_claims (campaign_code, client_ip_digest, created_at DESC);

WITH campaign_batch AS (
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
        'WenzWork 官网内测 1 年 Pro 赠送',
        id,
        'duration',
        365,
        100,
        'active',
        'system:beta-pro-launch',
        NULL
    FROM membership_plans
    WHERE code = 'pro' AND status = 'active'
    RETURNING id
)
INSERT INTO promotion_campaigns (code, batch_id, quota)
SELECT 'beta-pro-launch', id, 100
FROM campaign_batch;

-- +goose Down
DROP TABLE IF EXISTS promotion_claims;

DELETE FROM redemption_codes
WHERE batch_id IN (
    SELECT batch_id
    FROM promotion_campaigns
    WHERE code = 'beta-pro-launch'
);

DELETE FROM redemption_code_batches
WHERE id IN (
    SELECT batch_id
    FROM promotion_campaigns
    WHERE code = 'beta-pro-launch'
);

DROP TABLE IF EXISTS promotion_campaigns;

ALTER TABLE redemption_code_batches
    ALTER COLUMN created_by SET NOT NULL;
