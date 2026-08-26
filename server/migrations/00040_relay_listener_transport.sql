-- +goose Up
ALTER TABLE relay_node_installations
    ADD COLUMN tls_certificate_pem text NOT NULL DEFAULT '',
    ADD COLUMN tls_private_key_ciphertext bytea,
    ADD COLUMN tls_private_key_nonce bytea,
    ADD CONSTRAINT relay_node_installations_tls_material_pair
        CHECK (
            (tls_private_key_ciphertext IS NULL AND tls_private_key_nonce IS NULL) OR
            (octet_length(tls_private_key_ciphertext) > 0 AND octet_length(tls_private_key_nonce) = 12)
        );

-- +goose Down
ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_tls_material_pair,
    DROP COLUMN IF EXISTS tls_private_key_nonce,
    DROP COLUMN IF EXISTS tls_private_key_ciphertext,
    DROP COLUMN IF EXISTS tls_certificate_pem;
