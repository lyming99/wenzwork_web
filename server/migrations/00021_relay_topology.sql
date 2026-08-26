-- +goose Up
CREATE TABLE relay_regions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code varchar(80) NOT NULL UNIQUE,
    name varchar(120) NOT NULL,
    data_residency varchar(120) NOT NULL DEFAULT '',
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE relay_pools (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    region_id uuid NOT NULL REFERENCES relay_regions(id) ON DELETE RESTRICT,
    code varchar(80) NOT NULL,
    name varchar(120) NOT NULL,
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (region_id, code)
);

CREATE TABLE relay_cells (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    pool_id uuid NOT NULL REFERENCES relay_pools(id) ON DELETE RESTRICT,
    code varchar(80) NOT NULL UNIQUE,
    name varchar(120) NOT NULL,
    failure_domain varchar(120) NOT NULL DEFAULT '',
    status varchar(20) NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'draining', 'disabled')),
    weight numeric(7,3) NOT NULL DEFAULT 1 CHECK (weight > 0 AND weight <= 100),
    connection_soft_limit bigint NOT NULL DEFAULT 1000 CHECK (connection_soft_limit > 0),
    connection_hard_limit bigint NOT NULL DEFAULT 1200 CHECK (connection_hard_limit >= connection_soft_limit),
    file_bandwidth_soft_limit_mbps numeric(12,3) NOT NULL DEFAULT 1000 CHECK (file_bandwidth_soft_limit_mbps > 0),
    file_bandwidth_hard_limit_mbps numeric(12,3) NOT NULL DEFAULT 1200
        CHECK (file_bandwidth_hard_limit_mbps >= file_bandwidth_soft_limit_mbps),
    protocol_min integer NOT NULL DEFAULT 1 CHECK (protocol_min > 0),
    protocol_max integer NOT NULL DEFAULT 1 CHECK (protocol_max >= protocol_min),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX relay_cells_pool_status_idx ON relay_cells (pool_id, status, code);

CREATE TABLE relay_cell_endpoints (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cell_id uuid NOT NULL REFERENCES relay_cells(id) ON DELETE CASCADE,
    revision bigint NOT NULL CHECK (revision > 0),
    endpoint_type varchar(20) NOT NULL CHECK (endpoint_type IN ('domain', 'ip')),
    public_endpoint text NOT NULL CHECK (public_endpoint ~ '^wss://[^?#]+/v1/connect$'),
    status varchar(20) NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'validating', 'validated', 'active', 'draining', 'retired', 'failed')),
    validation_result jsonb NOT NULL DEFAULT '{}'::jsonb,
    certificate_not_after timestamptz,
    validated_at timestamptz,
    activated_at timestamptz,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cell_id, revision)
);

CREATE UNIQUE INDEX relay_cell_endpoints_one_active_idx
    ON relay_cell_endpoints (cell_id) WHERE status = 'active';
CREATE INDEX relay_cell_endpoints_cell_created_idx
    ON relay_cell_endpoints (cell_id, created_at DESC);

-- Idempotent development/acceptance topology: one Region, one Pool and two Cells.
INSERT INTO relay_regions (id, code, name, data_residency, status)
VALUES ('01700000-0000-4000-8000-000000000001', 'cn-dev', 'China Development', 'CN', 'active')
ON CONFLICT (code) DO NOTHING;

INSERT INTO relay_pools (id, region_id, code, name, status)
SELECT '01700000-0000-4000-8000-000000000002', id, 'standard', 'Standard', 'active'
FROM relay_regions WHERE code = 'cn-dev'
ON CONFLICT (region_id, code) DO NOTHING;

INSERT INTO relay_cells (id, pool_id, code, name, failure_domain, status)
SELECT seed.id, pool.id, seed.code, seed.name, seed.failure_domain, 'draft'
FROM relay_pools pool
CROSS JOIN (VALUES
    ('01700000-0000-4000-8000-000000000017'::uuid, 'r017', 'Relay Cell r017', 'dev-a'),
    ('01800000-0000-4000-8000-000000000018'::uuid, 'r018', 'Relay Cell r018', 'dev-b')
) AS seed(id, code, name, failure_domain)
WHERE pool.code = 'standard'
  AND pool.region_id = (SELECT id FROM relay_regions WHERE code = 'cn-dev')
ON CONFLICT (code) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS relay_cell_endpoints;
DROP TABLE IF EXISTS relay_cells;
DROP TABLE IF EXISTS relay_pools;
DROP TABLE IF EXISTS relay_regions;
