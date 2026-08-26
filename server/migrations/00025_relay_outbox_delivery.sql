-- +goose Up
ALTER TABLE relay_outbox
    ADD COLUMN dead_lettered_at timestamptz,
    ADD COLUMN dead_letter_reason varchar(120),
    ADD CONSTRAINT relay_outbox_terminal_state_check CHECK (
        NOT (published_at IS NOT NULL AND dead_lettered_at IS NOT NULL)
        AND ((dead_lettered_at IS NULL) = (dead_letter_reason IS NULL))
    );

DROP INDEX relay_outbox_claim_idx;
DROP INDEX relay_outbox_pending_idx;
CREATE INDEX relay_outbox_pending_idx
    ON relay_outbox (available_at, created_at)
    WHERE published_at IS NULL AND dead_lettered_at IS NULL;
CREATE INDEX relay_outbox_claim_idx
    ON relay_outbox (claimed_at, available_at, created_at)
    WHERE published_at IS NULL AND dead_lettered_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS relay_outbox_claim_idx;
DROP INDEX IF EXISTS relay_outbox_pending_idx;
ALTER TABLE relay_outbox
    DROP CONSTRAINT IF EXISTS relay_outbox_terminal_state_check,
    DROP COLUMN IF EXISTS dead_letter_reason,
    DROP COLUMN IF EXISTS dead_lettered_at;
CREATE INDEX relay_outbox_pending_idx
    ON relay_outbox (available_at, created_at) WHERE published_at IS NULL;
CREATE INDEX relay_outbox_claim_idx
    ON relay_outbox (claimed_at, available_at, created_at)
    WHERE published_at IS NULL;
