-- +goose Up
UPDATE pricing_plan_versions AS published
SET description = '自部署服务免费'
FROM pricing_plans AS plan
WHERE plan.code = 'free'
  AND published.pricing_plan_id = plan.id
  AND published.version = plan.published_version;

-- Preserve any independently staged administrator edit while keeping the
-- current row aligned when it still represents the published Free plan.
UPDATE pricing_plans
SET description = '自部署服务免费',
    updated_at = now()
WHERE code = 'free'
  AND (published_version IS NULL OR version = published_version);

-- +goose Down
UPDATE pricing_plan_versions AS published
SET description = '适合体验 WenzWork 核心写作流程。'
FROM pricing_plans AS plan
WHERE plan.code = 'free'
  AND published.pricing_plan_id = plan.id
  AND published.version = plan.published_version
  AND published.description = '自部署服务免费';

UPDATE pricing_plans
SET description = '适合体验 WenzWork 核心写作流程。',
    updated_at = now()
WHERE code = 'free'
  AND description = '自部署服务免费'
  AND (published_version IS NULL OR version = published_version);
