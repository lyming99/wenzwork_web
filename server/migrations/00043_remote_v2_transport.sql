-- +goose Up
-- remote/v2 is a coordinated, non-compatible transport cutover.  Remove the
-- v1-only checks inside the migration transaction, rewrite stored endpoints,
-- and then install the v2 checks before the transaction becomes visible.

ALTER TABLE relay_cell_endpoints
    DROP CONSTRAINT IF EXISTS relay_cell_endpoints_public_endpoint_check;

UPDATE relay_cell_endpoints
SET public_endpoint = regexp_replace(public_endpoint, '/v1/connect$', '/v2/connect')
WHERE public_endpoint ~ '/v1/connect$';

ALTER TABLE relay_cell_endpoints
    ADD CONSTRAINT relay_cell_endpoints_public_endpoint_check
    CHECK (public_endpoint ~ '^wss?://[^?#]+/v2/connect$');

ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_public_endpoint_check;

UPDATE relay_node_installations
SET public_endpoint = regexp_replace(public_endpoint, '/v1/connect$', '/v2/connect')
WHERE public_endpoint ~ '/v1/connect$';

ALTER TABLE relay_node_installations
    ADD CONSTRAINT relay_node_installations_public_endpoint_check
    CHECK (
        public_endpoint = '' OR
        (length(public_endpoint) <= 255 AND public_endpoint ~ '^wss?://[^?#]+/v2/connect$')
    );

UPDATE relay_node_instances
SET addresses = COALESCE((
    SELECT jsonb_agg(
        CASE WHEN value ~ '/v1/connect$' THEN regexp_replace(value, '/v1/connect$', '/v2/connect') ELSE value END
    )
    FROM jsonb_array_elements_text(addresses) AS address(value)
), '[]'::jsonb)
WHERE addresses::text LIKE '%/v1/connect%';

UPDATE relay_cells SET protocol_min = 2, protocol_max = 2;
ALTER TABLE relay_cells ALTER COLUMN protocol_min SET DEFAULT 2;
ALTER TABLE relay_cells ALTER COLUMN protocol_max SET DEFAULT 2;

UPDATE remote_device_credentials SET protocol_min = 2, protocol_max = 2;

-- +goose Down
ALTER TABLE relay_cell_endpoints
    DROP CONSTRAINT IF EXISTS relay_cell_endpoints_public_endpoint_check;

UPDATE relay_cell_endpoints
SET public_endpoint = regexp_replace(public_endpoint, '/v2/connect$', '/v1/connect')
WHERE public_endpoint ~ '/v2/connect$';

ALTER TABLE relay_cell_endpoints
    ADD CONSTRAINT relay_cell_endpoints_public_endpoint_check
    CHECK (public_endpoint ~ '^wss?://[^?#]+/v1/connect$');

ALTER TABLE relay_node_installations
    DROP CONSTRAINT IF EXISTS relay_node_installations_public_endpoint_check;

UPDATE relay_node_installations
SET public_endpoint = regexp_replace(public_endpoint, '/v2/connect$', '/v1/connect')
WHERE public_endpoint ~ '/v2/connect$';

ALTER TABLE relay_node_installations
    ADD CONSTRAINT relay_node_installations_public_endpoint_check
    CHECK (
        public_endpoint = '' OR
        (length(public_endpoint) <= 255 AND public_endpoint ~ '^wss?://[^?#]+/v1/connect$')
    );

UPDATE relay_node_instances
SET addresses = COALESCE((
    SELECT jsonb_agg(
        CASE WHEN value ~ '/v2/connect$' THEN regexp_replace(value, '/v2/connect$', '/v1/connect') ELSE value END
    )
    FROM jsonb_array_elements_text(addresses) AS address(value)
), '[]'::jsonb)
WHERE addresses::text LIKE '%/v2/connect%';

UPDATE relay_cells SET protocol_min = 1, protocol_max = 1;
ALTER TABLE relay_cells ALTER COLUMN protocol_min SET DEFAULT 1;
ALTER TABLE relay_cells ALTER COLUMN protocol_max SET DEFAULT 1;

UPDATE remote_device_credentials SET protocol_min = 1, protocol_max = 1;
