-- +goose Up
ALTER TABLE relay_node_installations
    ADD COLUMN region varchar(80) NOT NULL DEFAULT '',
    ADD COLUMN relay_group varchar(80) NOT NULL DEFAULT '';

-- Preserve useful labels for existing Relay records while keeping the
-- topology itself an internal implementation detail.
UPDATE relay_node_installations AS installation
SET region = topology_region.code,
    relay_group = pool.code
FROM relay_cells AS cell
JOIN relay_pools AS pool ON pool.id = cell.pool_id
JOIN relay_regions AS topology_region ON topology_region.id = pool.region_id
WHERE installation.cell_id = cell.id
  AND installation.region = ''
  AND installation.relay_group = '';

CREATE INDEX relay_node_installations_region_group_idx
    ON relay_node_installations (region, relay_group, updated_at DESC)
    WHERE status <> 'deleted';

-- +goose Down
DROP INDEX IF EXISTS relay_node_installations_region_group_idx;

ALTER TABLE relay_node_installations
    DROP COLUMN IF EXISTS relay_group,
    DROP COLUMN IF EXISTS region;
