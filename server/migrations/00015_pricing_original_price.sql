-- +goose Up
ALTER TABLE pricing_plans
    ADD COLUMN original_price_minor bigint;

ALTER TABLE pricing_plan_versions
    ADD COLUMN original_price_minor bigint;

ALTER TABLE pricing_plans
    ADD CONSTRAINT pricing_plans_original_price_check
    CHECK (
        original_price_minor IS NULL OR
        (price_minor IS NOT NULL AND original_price_minor > price_minor)
    );

ALTER TABLE pricing_plan_versions
    ADD CONSTRAINT pricing_plan_versions_original_price_check
    CHECK (
        original_price_minor IS NULL OR
        (price_minor IS NOT NULL AND original_price_minor > price_minor)
    );

-- Preserve the current WenzWork beta promotion when its discounted price has
-- already been configured. Other plans remain unset until an administrator
-- explicitly saves and publishes an original price.
UPDATE pricing_plans
SET original_price_minor = 5900
WHERE code = 'pro' AND price_minor = 3900 AND original_price_minor IS NULL;

UPDATE pricing_plan_versions AS version
SET original_price_minor = 5900
FROM pricing_plans AS plan
WHERE version.pricing_plan_id = plan.id
  AND version.version = plan.published_version
  AND plan.code = 'pro'
  AND version.price_minor = 3900
  AND version.original_price_minor IS NULL;

-- +goose Down
ALTER TABLE pricing_plan_versions
    DROP COLUMN IF EXISTS original_price_minor;

ALTER TABLE pricing_plans
    DROP COLUMN IF EXISTS original_price_minor;
