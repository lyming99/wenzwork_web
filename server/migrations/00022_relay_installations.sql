-- +goose Up
CREATE TABLE relay_server_releases (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version varchar(64) NOT NULL,
    platform varchar(32) NOT NULL CHECK (platform IN ('linux')),
    architecture varchar(32) NOT NULL CHECK (architecture IN ('amd64')),
    protocol_min integer NOT NULL CHECK (protocol_min > 0),
    protocol_max integer NOT NULL CHECK (protocol_max >= protocol_min),
    build_commit varchar(64) NOT NULL,
    build_time timestamptz NOT NULL,
    signing_key_id varchar(120) NOT NULL,
    manifest_sha256 char(64) NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    manifest_signature text NOT NULL,
    status varchar(20) NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'published', 'revoked', 'retired')),
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (version, platform, architecture)
);

CREATE TABLE relay_server_release_artifacts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id uuid NOT NULL REFERENCES relay_server_releases(id) ON DELETE CASCADE,
    file_name varchar(255) NOT NULL,
    file_size_bytes bigint NOT NULL CHECK (file_size_bytes > 0),
    sha256 char(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    signature text NOT NULL,
    object_key text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (release_id, file_name)
);

CREATE TABLE relay_node_installations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cell_id uuid NOT NULL REFERENCES relay_cells(id) ON DELETE RESTRICT,
    release_id uuid REFERENCES relay_server_releases(id) ON DELETE RESTRICT,
    display_name varchar(120) NOT NULL,
    failure_domain varchar(120) NOT NULL DEFAULT '',
    operations_note text NOT NULL DEFAULT '',
    platform varchar(32) NOT NULL DEFAULT 'linux' CHECK (platform IN ('linux')),
    architecture varchar(32) NOT NULL DEFAULT 'amd64' CHECK (architecture IN ('amd64')),
    status varchar(32) NOT NULL DEFAULT 'draft'
        CHECK (status IN (
            'draft', 'pending_enrollment', 'enrolled', 'pending_activation', 'active',
            'draining', 'disabled', 'revoked', 'expired', 'deleted'
        )),
    identity_public_key bytea,
    identity_thumbprint char(64),
    current_instance_id uuid,
    deployment_checklist jsonb NOT NULL DEFAULT '{"lb":false,"dns":false,"port":false,"tls":false}'::jsonb,
    first_enrolled_at timestamptz,
    activated_at timestamptz,
    revoked_at timestamptz,
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((identity_public_key IS NULL) = (identity_thumbprint IS NULL)),
    CHECK (identity_thumbprint IS NULL OR identity_thumbprint ~ '^[0-9a-f]{64}$')
);

CREATE INDEX relay_node_installations_cell_status_idx
    ON relay_node_installations (cell_id, status, updated_at DESC);

CREATE TABLE relay_node_instances (
    id uuid PRIMARY KEY,
    installation_id uuid NOT NULL REFERENCES relay_node_installations(id) ON DELETE RESTRICT,
    cell_id uuid NOT NULL REFERENCES relay_cells(id) ON DELETE RESTRICT,
    status varchar(24) NOT NULL DEFAULT 'starting'
        CHECK (status IN ('starting', 'ready', 'draining', 'stopped', 'failed', 'offline', 'forced_offline')),
    version varchar(64) NOT NULL,
    protocol_version integer NOT NULL CHECK (protocol_version > 0),
    addresses jsonb NOT NULL DEFAULT '[]'::jsonb,
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    active_connections bigint NOT NULL DEFAULT 0 CHECK (active_connections >= 0),
    active_file_transfers bigint NOT NULL DEFAULT 0 CHECK (active_file_transfers >= 0),
    memory_bytes bigint NOT NULL DEFAULT 0 CHECK (memory_bytes >= 0),
    ingress_mbps numeric(14,3) NOT NULL DEFAULT 0 CHECK (ingress_mbps >= 0),
    egress_mbps numeric(14,3) NOT NULL DEFAULT 0 CHECK (egress_mbps >= 0),
    write_loop_lag_ms numeric(14,3) NOT NULL DEFAULT 0 CHECK (write_loop_lag_ms >= 0),
    started_at timestamptz NOT NULL,
    last_heartbeat_at timestamptz NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    stopped_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (lease_expires_at >= last_heartbeat_at)
);

ALTER TABLE relay_node_installations
    ADD CONSTRAINT relay_node_installations_current_instance_fk
    FOREIGN KEY (current_instance_id) REFERENCES relay_node_instances(id) ON DELETE SET NULL;

CREATE INDEX relay_node_instances_installation_started_idx
    ON relay_node_instances (installation_id, started_at DESC);
CREATE INDEX relay_node_instances_lease_idx
    ON relay_node_instances (lease_expires_at) WHERE status IN ('starting', 'ready', 'draining');

CREATE TABLE relay_node_enrollment_tokens (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid NOT NULL REFERENCES relay_node_installations(id) ON DELETE CASCADE,
    cell_id uuid NOT NULL REFERENCES relay_cells(id) ON DELETE RESTRICT,
    release_id uuid REFERENCES relay_server_releases(id) ON DELETE RESTRICT,
    platform varchar(32) NOT NULL CHECK (platform IN ('linux')),
    architecture varchar(32) NOT NULL CHECK (architecture IN ('amd64')),
    token_digest char(64) NOT NULL UNIQUE CHECK (token_digest ~ '^[0-9a-f]{64}$'),
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'consumed', 'expired', 'locked', 'revoked')),
    failed_attempts integer NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
    max_failed_attempts integer NOT NULL DEFAULT 5 CHECK (max_failed_attempts BETWEEN 1 AND 20),
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at),
    CHECK ((status = 'consumed') = (consumed_at IS NOT NULL))
);

CREATE INDEX relay_node_enrollment_tokens_installation_idx
    ON relay_node_enrollment_tokens (installation_id, created_at DESC);
CREATE INDEX relay_node_enrollment_tokens_expiry_idx
    ON relay_node_enrollment_tokens (expires_at) WHERE status = 'active';

CREATE TABLE relay_node_certificates (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid NOT NULL REFERENCES relay_node_installations(id) ON DELETE CASCADE,
    cell_id uuid NOT NULL REFERENCES relay_cells(id) ON DELETE RESTRICT,
    serial_number varchar(128) NOT NULL UNIQUE,
    certificate_sha256 char(64) NOT NULL UNIQUE CHECK (certificate_sha256 ~ '^[0-9a-f]{64}$'),
    identity_thumbprint char(64) NOT NULL CHECK (identity_thumbprint ~ '^[0-9a-f]{64}$'),
    status varchar(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'expired', 'revoked', 'superseded')),
    not_before timestamptz NOT NULL,
    not_after timestamptz NOT NULL,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (not_after > not_before),
    CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);

CREATE UNIQUE INDEX relay_node_certificates_one_active_idx
    ON relay_node_certificates (installation_id) WHERE status = 'active';
CREATE INDEX relay_node_certificates_expiry_idx
    ON relay_node_certificates (not_after) WHERE status = 'active';

CREATE TABLE relay_node_install_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id uuid NOT NULL REFERENCES relay_node_installations(id) ON DELETE CASCADE,
    release_id uuid NOT NULL REFERENCES relay_server_releases(id) ON DELETE RESTRICT,
    mode varchar(24) NOT NULL CHECK (mode IN ('download', 'script', 'manual')),
    status varchar(24) NOT NULL DEFAULT 'waiting'
        CHECK (status IN ('waiting', 'enrolled', 'heartbeat_received', 'expired', 'cancelled')),
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    expires_at timestamptz NOT NULL,
    enrolled_at timestamptz,
    heartbeat_received_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (expires_at > created_at)
);

CREATE INDEX relay_node_install_sessions_installation_idx
    ON relay_node_install_sessions (installation_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS relay_node_install_sessions;
DROP TABLE IF EXISTS relay_node_certificates;
DROP TABLE IF EXISTS relay_node_enrollment_tokens;
ALTER TABLE relay_node_installations DROP CONSTRAINT IF EXISTS relay_node_installations_current_instance_fk;
DROP TABLE IF EXISTS relay_node_instances;
DROP TABLE IF EXISTS relay_node_installations;
DROP TABLE IF EXISTS relay_server_release_artifacts;
DROP TABLE IF EXISTS relay_server_releases;
