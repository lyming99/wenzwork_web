-- +goose Up
-- Remote access is a versioned pricing-plan entitlement. Publishing a pricing
-- plan therefore changes both the public catalogue and the limits enforced for
-- subsequent device enrolments.
ALTER TABLE pricing_plans
    ADD COLUMN remote_access_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN device_limit integer NOT NULL DEFAULT 10
        CHECK (device_limit BETWEEN 1 AND 100000),
    ADD COLUMN monthly_traffic_limit_gb bigint
        CHECK (monthly_traffic_limit_gb BETWEEN 1 AND 1000000);

ALTER TABLE pricing_plan_versions
    ADD COLUMN remote_access_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN device_limit integer NOT NULL DEFAULT 10
        CHECK (device_limit BETWEEN 1 AND 100000),
    ADD COLUMN monthly_traffic_limit_gb bigint
        CHECK (monthly_traffic_limit_gb BETWEEN 1 AND 1000000);

-- Preserve the former global device limit for every existing plan. Existing
-- paid plans stay usable, while Free remains closed until an administrator
-- explicitly enables and publishes it. Pro's stored traffic quota mirrors the
-- currently advertised 10 GB/month; traffic enforcement is intentionally not
-- part of this migration.
UPDATE pricing_plans
SET remote_access_enabled = billing_period <> 'free',
    device_limit = COALESCE(
        (SELECT device_limit FROM remote_access_policy_settings WHERE singleton = true),
        10
    ),
    monthly_traffic_limit_gb = CASE WHEN code = 'pro' THEN 10 ELSE NULL END;

UPDATE pricing_plan_versions
SET remote_access_enabled = billing_period <> 'free',
    device_limit = COALESCE(
        (SELECT device_limit FROM remote_access_policy_settings WHERE singleton = true),
        10
    ),
    monthly_traffic_limit_gb = CASE WHEN code = 'pro' THEN 10 ELSE NULL END;

-- +goose Down
ALTER TABLE pricing_plan_versions
    DROP COLUMN IF EXISTS monthly_traffic_limit_gb,
    DROP COLUMN IF EXISTS device_limit,
    DROP COLUMN IF EXISTS remote_access_enabled;

ALTER TABLE pricing_plans
    DROP COLUMN IF EXISTS monthly_traffic_limit_gb,
    DROP COLUMN IF EXISTS device_limit,
    DROP COLUMN IF EXISTS remote_access_enabled;
