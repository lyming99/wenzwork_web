-- +goose Up
-- Relay always exposes a plaintext WebSocket listener. TLS termination belongs
-- to an operator-managed reverse proxy such as Nginx, so the internal listener
-- port is independent from the client-facing ws:// or wss:// URL.
ALTER TABLE relay_node_installations
    ADD COLUMN listener_port integer NOT NULL DEFAULT 8443
        CHECK (listener_port BETWEEN 1 AND 65535);

ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_tls_material_pair,
    DROP COLUMN IF EXISTS tls_private_key_nonce,
    DROP COLUMN IF EXISTS tls_private_key_ciphertext,
    DROP COLUMN IF EXISTS tls_certificate_pem;

-- +goose Down
ALTER TABLE relay_node_installations
    ADD COLUMN tls_certificate_pem text NOT NULL DEFAULT '',
    ADD COLUMN tls_private_key_ciphertext bytea,
    ADD COLUMN tls_private_key_nonce bytea,
    ADD CONSTRAINT relay_node_installations_tls_material_pair
        CHECK (
            (tls_private_key_ciphertext IS NULL AND tls_private_key_nonce IS NULL) OR
            (octet_length(tls_private_key_ciphertext) > 0 AND octet_length(tls_private_key_nonce) = 12)
        );

ALTER TABLE relay_node_installations
    DROP COLUMN IF EXISTS listener_port;
