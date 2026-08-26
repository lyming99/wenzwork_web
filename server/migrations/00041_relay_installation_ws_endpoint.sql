-- +goose Up
-- Relay hosts may listen directly over plaintext WS in private or local
-- deployments. Keep the same strict path/query shape as WSS while allowing
-- either supported WebSocket scheme.
ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_public_endpoint_check;

ALTER TABLE relay_node_installations
    ADD CONSTRAINT relay_node_installations_public_endpoint_check
    CHECK (
        public_endpoint = '' OR
        (length(public_endpoint) <= 255 AND public_endpoint ~ '^wss?://[^?#]+/v1/connect$')
    );

-- +goose Down
ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_public_endpoint_check;

ALTER TABLE relay_node_installations
    ADD CONSTRAINT relay_node_installations_public_endpoint_check
    CHECK (
        public_endpoint = '' OR
        (length(public_endpoint) <= 255 AND public_endpoint ~ '^wss://[^?#]+/v1/connect$')
    );
