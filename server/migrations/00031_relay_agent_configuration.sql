-- +goose Up
ALTER TABLE relay_node_installations
    ADD COLUMN public_endpoint text NOT NULL DEFAULT ''
    CHECK (
        public_endpoint = '' OR
        (length(public_endpoint) <= 255 AND public_endpoint ~ '^wss://[^?#]+/v1/connect$')
    );

-- Preserve the last exact endpoint reported by already-connected Access Key
-- Relay instances so upgrading the management service does not strand them.
UPDATE relay_node_installations AS installation
SET public_endpoint = COALESCE((
    SELECT address.value
    FROM relay_node_instances AS instance
    CROSS JOIN LATERAL jsonb_array_elements_text(instance.addresses) AS address(value)
    WHERE instance.installation_id = installation.id
      AND address.value ~ '^wss://[^?#]+/v1/connect$'
    ORDER BY instance.started_at DESC
    LIMIT 1
), '')
WHERE installation.public_endpoint = '';

-- +goose Down
ALTER TABLE relay_node_installations DROP COLUMN IF EXISTS public_endpoint;
