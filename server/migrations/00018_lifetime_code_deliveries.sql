-- +goose Up
CREATE TABLE lifetime_code_deliveries (
    id uuid PRIMARY KEY,
    email varchar(320) NOT NULL,
    code_id uuid NOT NULL UNIQUE REFERENCES redemption_codes(id) ON DELETE RESTRICT,
    code_ciphertext bytea,
    delivery_status varchar(20) NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending', 'sent', 'failed')),
    delivery_attempts integer NOT NULL DEFAULT 1 CHECK (delivery_attempts > 0),
    last_delivery_attempt_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz,
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (delivery_status = 'sent' AND sent_at IS NOT NULL AND code_ciphertext IS NULL)
        OR (delivery_status <> 'sent' AND sent_at IS NULL AND code_ciphertext IS NOT NULL)
    )
);

CREATE INDEX lifetime_code_deliveries_created_idx
    ON lifetime_code_deliveries (created_at DESC);
CREATE INDEX lifetime_code_deliveries_email_created_idx
    ON lifetime_code_deliveries (lower(email), created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS lifetime_code_deliveries;
