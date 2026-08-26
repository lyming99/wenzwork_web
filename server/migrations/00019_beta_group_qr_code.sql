-- +goose Up
ALTER TABLE promotion_campaigns
    ADD COLUMN group_qr_code bytea,
    ADD COLUMN group_qr_content_type varchar(20),
    ADD COLUMN group_qr_updated_at timestamptz,
    ADD CONSTRAINT promotion_campaigns_group_qr_code_check CHECK (
        (
            group_qr_code IS NULL
            AND group_qr_content_type IS NULL
            AND group_qr_updated_at IS NULL
        )
        OR (
            group_qr_code IS NOT NULL
            AND group_qr_content_type IS NOT NULL
            AND group_qr_updated_at IS NOT NULL
            AND octet_length(group_qr_code) BETWEEN 1 AND 2097152
            AND group_qr_content_type IN ('image/png', 'image/jpeg')
        )
    );

-- +goose Down
ALTER TABLE promotion_campaigns
    DROP CONSTRAINT promotion_campaigns_group_qr_code_check,
    DROP COLUMN group_qr_updated_at,
    DROP COLUMN group_qr_content_type,
    DROP COLUMN group_qr_code;
