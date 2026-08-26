-- +goose Up
ALTER TABLE promotion_campaigns
    ADD COLUMN redemption_policy varchar(30) NOT NULL DEFAULT 'standard'
    CHECK (redemption_policy IN ('standard', 'beta_email_once'));

UPDATE promotion_campaigns
SET redemption_policy = 'beta_email_once'
WHERE code = 'beta-pro-launch';

ALTER TABLE promotion_campaigns
    DROP CONSTRAINT promotion_campaigns_quota_check;

ALTER TABLE promotion_campaigns
    ADD CONSTRAINT promotion_campaigns_quota_check CHECK (quota >= 0);

UPDATE redemption_code_batches AS batch
SET status = 'active', updated_at = now()
FROM promotion_campaigns AS campaign
WHERE batch.id = campaign.batch_id
  AND batch.status = 'exhausted'
  AND (
      campaign.status = 'active'
      OR EXISTS (
          SELECT 1
          FROM redemption_codes AS code
          WHERE code.batch_id = batch.id AND code.status = 'active'
      )
  );

-- +goose Down
UPDATE promotion_campaigns
SET quota = 1
WHERE quota = 0;

ALTER TABLE promotion_campaigns
    DROP CONSTRAINT promotion_campaigns_quota_check;

ALTER TABLE promotion_campaigns
    ADD CONSTRAINT promotion_campaigns_quota_check CHECK (quota > 0);

ALTER TABLE promotion_campaigns
    DROP COLUMN redemption_policy;
