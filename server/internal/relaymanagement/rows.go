package relaymanagement

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type cellRow struct {
	ID                  uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	PoolID              uuid.UUID `gorm:"column:pool_id;type:uuid"`
	Code                string    `gorm:"column:code"`
	Name                string    `gorm:"column:name"`
	FailureDomain       string    `gorm:"column:failure_domain"`
	Status              string    `gorm:"column:status"`
	Weight              float64   `gorm:"column:weight"`
	ConnectionSoftLimit int64     `gorm:"column:connection_soft_limit"`
	ConnectionHardLimit int64     `gorm:"column:connection_hard_limit"`
	ProtocolMin         int       `gorm:"column:protocol_min"`
	ProtocolMax         int       `gorm:"column:protocol_max"`
	Version             int64     `gorm:"column:version"`
}

func (cellRow) TableName() string { return "relay_cells" }

type installationRow struct {
	ID                  uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	CellID              uuid.UUID       `gorm:"column:cell_id;type:uuid"`
	ReleaseID           *uuid.UUID      `gorm:"column:release_id;type:uuid"`
	DisplayName         string          `gorm:"column:display_name"`
	Region              string          `gorm:"column:region"`
	Group               string          `gorm:"column:relay_group"`
	FailureDomain       string          `gorm:"column:failure_domain"`
	OperationsNote      string          `gorm:"column:operations_note"`
	PublicEndpoint      string          `gorm:"column:public_endpoint"`
	ListenerPort        int             `gorm:"column:listener_port"`
	Platform            string          `gorm:"column:platform"`
	Architecture        string          `gorm:"column:architecture"`
	Status              string          `gorm:"column:status"`
	IdentityPublicKey   []byte          `gorm:"column:identity_public_key"`
	IdentityThumbprint  *string         `gorm:"column:identity_thumbprint"`
	CurrentInstanceID   *uuid.UUID      `gorm:"column:current_instance_id;type:uuid"`
	DeploymentChecklist json.RawMessage `gorm:"column:deployment_checklist;type:jsonb"`
	FirstEnrolledAt     *time.Time      `gorm:"column:first_enrolled_at"`
	ActivatedAt         *time.Time      `gorm:"column:activated_at"`
	RevokedAt           *time.Time      `gorm:"column:revoked_at"`
	Version             int64           `gorm:"column:version"`
	CreatedBy           *uuid.UUID      `gorm:"column:created_by;type:uuid"`
	CreatedAt           time.Time       `gorm:"column:created_at"`
	UpdatedAt           time.Time       `gorm:"column:updated_at"`
}

func (installationRow) TableName() string { return "relay_node_installations" }

type instanceRow struct {
	ID                  uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	InstallationID      uuid.UUID       `gorm:"column:installation_id;type:uuid"`
	CellID              uuid.UUID       `gorm:"column:cell_id;type:uuid"`
	Status              string          `gorm:"column:status"`
	Version             string          `gorm:"column:version"`
	ProtocolVersion     int             `gorm:"column:protocol_version"`
	Addresses           json.RawMessage `gorm:"column:addresses;type:jsonb"`
	Capabilities        json.RawMessage `gorm:"column:capabilities;type:jsonb"`
	ActiveConnections   int64           `gorm:"column:active_connections"`
	ActiveFileTransfers int64           `gorm:"column:active_file_transfers"`
	MemoryBytes         int64           `gorm:"column:memory_bytes"`
	IngressMbps         float64         `gorm:"column:ingress_mbps"`
	EgressMbps          float64         `gorm:"column:egress_mbps"`
	WriteLoopLagMS      float64         `gorm:"column:write_loop_lag_ms"`
	StartedAt           time.Time       `gorm:"column:started_at"`
	LastHeartbeatAt     time.Time       `gorm:"column:last_heartbeat_at"`
	LeaseExpiresAt      time.Time       `gorm:"column:lease_expires_at"`
	StoppedAt           *time.Time      `gorm:"column:stopped_at"`
	CreatedAt           time.Time       `gorm:"column:created_at"`
}

func (instanceRow) TableName() string { return "relay_node_instances" }

type enrollmentTokenRow struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	InstallationID    uuid.UUID  `gorm:"column:installation_id;type:uuid"`
	CellID            uuid.UUID  `gorm:"column:cell_id;type:uuid"`
	ReleaseID         *uuid.UUID `gorm:"column:release_id;type:uuid"`
	Platform          string     `gorm:"column:platform"`
	Architecture      string     `gorm:"column:architecture"`
	TokenDigest       string     `gorm:"column:token_digest"`
	Status            string     `gorm:"column:status"`
	FailedAttempts    int        `gorm:"column:failed_attempts"`
	MaxFailedAttempts int        `gorm:"column:max_failed_attempts"`
	CreatedBy         *uuid.UUID `gorm:"column:created_by;type:uuid"`
	ExpiresAt         time.Time  `gorm:"column:expires_at"`
	ConsumedAt        *time.Time `gorm:"column:consumed_at"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
}

func (enrollmentTokenRow) TableName() string { return "relay_node_enrollment_tokens" }

type accessKeyRow struct {
	ID             uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	InstallationID uuid.UUID  `gorm:"column:installation_id;type:uuid"`
	KeyPrefix      string     `gorm:"column:key_prefix"`
	KeyDigest      string     `gorm:"column:key_digest"`
	Status         string     `gorm:"column:status"`
	CreatedBy      *uuid.UUID `gorm:"column:created_by;type:uuid"`
	LastUsedAt     *time.Time `gorm:"column:last_used_at"`
	RevokedAt      *time.Time `gorm:"column:revoked_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
}

func (accessKeyRow) TableName() string { return "relay_node_access_keys" }

type certificateRow struct {
	ID                 uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	InstallationID     uuid.UUID  `gorm:"column:installation_id;type:uuid"`
	CellID             uuid.UUID  `gorm:"column:cell_id;type:uuid"`
	SerialNumber       string     `gorm:"column:serial_number"`
	CertificateSHA256  string     `gorm:"column:certificate_sha256"`
	IdentityThumbprint string     `gorm:"column:identity_thumbprint"`
	Status             string     `gorm:"column:status"`
	NotBefore          time.Time  `gorm:"column:not_before"`
	NotAfter           time.Time  `gorm:"column:not_after"`
	RevokedAt          *time.Time `gorm:"column:revoked_at"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
}

func (certificateRow) TableName() string { return "relay_node_certificates" }

type installSessionRow struct {
	ID                  uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	InstallationID      uuid.UUID  `gorm:"column:installation_id;type:uuid"`
	ReleaseID           uuid.UUID  `gorm:"column:release_id;type:uuid"`
	Mode                string     `gorm:"column:mode"`
	Status              string     `gorm:"column:status"`
	CreatedBy           *uuid.UUID `gorm:"column:created_by;type:uuid"`
	ExpiresAt           time.Time  `gorm:"column:expires_at"`
	EnrolledAt          *time.Time `gorm:"column:enrolled_at"`
	HeartbeatReceivedAt *time.Time `gorm:"column:heartbeat_received_at"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

func (installSessionRow) TableName() string { return "relay_node_install_sessions" }

type releaseRow struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Version           string     `gorm:"column:version"`
	Platform          string     `gorm:"column:platform"`
	Architecture      string     `gorm:"column:architecture"`
	ProtocolMin       int        `gorm:"column:protocol_min"`
	ProtocolMax       int        `gorm:"column:protocol_max"`
	BuildCommit       string     `gorm:"column:build_commit"`
	BuildTime         time.Time  `gorm:"column:build_time"`
	SigningKeyID      string     `gorm:"column:signing_key_id"`
	ManifestSHA256    string     `gorm:"column:manifest_sha256"`
	ManifestSignature string     `gorm:"column:manifest_signature"`
	Status            string     `gorm:"column:status"`
	CreatedBy         *uuid.UUID `gorm:"column:created_by;type:uuid"`
}

func (releaseRow) TableName() string { return "relay_server_releases" }

type artifactRow struct {
	ID            uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ReleaseID     uuid.UUID `gorm:"column:release_id;type:uuid"`
	FileName      string    `gorm:"column:file_name"`
	FileSizeBytes int64     `gorm:"column:file_size_bytes"`
	SHA256        string    `gorm:"column:sha256"`
	Signature     string    `gorm:"column:signature"`
	ObjectKey     string    `gorm:"column:object_key"`
}

func (artifactRow) TableName() string { return "relay_server_release_artifacts" }

type endpointRow struct {
	ID                  uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	CellID              uuid.UUID       `gorm:"column:cell_id;type:uuid"`
	Revision            int64           `gorm:"column:revision"`
	EndpointType        string          `gorm:"column:endpoint_type"`
	PublicEndpoint      string          `gorm:"column:public_endpoint"`
	Status              string          `gorm:"column:status"`
	ValidationResult    json.RawMessage `gorm:"column:validation_result;type:jsonb"`
	CertificateNotAfter *time.Time      `gorm:"column:certificate_not_after"`
	ValidatedAt         *time.Time      `gorm:"column:validated_at"`
	ActivatedAt         *time.Time      `gorm:"column:activated_at"`
	DrainUntil          *time.Time      `gorm:"column:drain_until"`
	SupersededAt        *time.Time      `gorm:"column:superseded_at"`
	Version             int64           `gorm:"column:version"`
	CreatedBy           *uuid.UUID      `gorm:"column:created_by;type:uuid"`
	CreatedAt           time.Time       `gorm:"column:created_at"`
	UpdatedAt           time.Time       `gorm:"column:updated_at"`
}

func (endpointRow) TableName() string { return "relay_cell_endpoints" }

type assignmentRow struct {
	ID                uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	UserID            uuid.UUID       `gorm:"column:user_id;type:uuid"`
	CellID            uuid.UUID       `gorm:"column:cell_id;type:uuid"`
	AssignmentVersion int64           `gorm:"column:assignment_version"`
	Mode              string          `gorm:"column:mode"`
	Status            string          `gorm:"column:status"`
	FallbackCellIDs   json.RawMessage `gorm:"column:fallback_cell_ids;type:jsonb"`
	LeaseExpiresAt    time.Time       `gorm:"column:lease_expires_at"`
	EffectiveAt       *time.Time      `gorm:"column:effective_at"`
	SupersededAt      *time.Time      `gorm:"column:superseded_at"`
	SourceOperationID *uuid.UUID      `gorm:"column:source_operation_id;type:uuid"`
	CreatedAt         time.Time       `gorm:"column:created_at"`
	UpdatedAt         time.Time       `gorm:"column:updated_at"`
}

func (assignmentRow) TableName() string { return "relay_assignments" }

type operationRow struct {
	ID                uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	OperationType     string          `gorm:"column:operation_type"`
	Status            string          `gorm:"column:status"`
	TargetType        string          `gorm:"column:target_type"`
	TargetID          *uuid.UUID      `gorm:"column:target_id;type:uuid"`
	RequestJSON       json.RawMessage `gorm:"column:request_json;type:jsonb"`
	ProgressCompleted int             `gorm:"column:progress_completed"`
	ProgressTotal     int             `gorm:"column:progress_total"`
	IdempotencyKey    *string         `gorm:"column:idempotency_key"`
	ErrorCode         *string         `gorm:"column:error_code"`
	ErrorMessage      *string         `gorm:"column:error_message"`
	CreatedBy         *uuid.UUID      `gorm:"column:created_by;type:uuid"`
	RequestSequence   int64           `gorm:"column:request_sequence;autoIncrement"`
	ClaimToken        *uuid.UUID      `gorm:"column:claim_token;type:uuid"`
	WorkerHeartbeatAt *time.Time      `gorm:"column:worker_heartbeat_at"`
	NextAttemptAt     time.Time       `gorm:"column:next_attempt_at"`
	StartedAt         *time.Time      `gorm:"column:started_at"`
	FinishedAt        *time.Time      `gorm:"column:finished_at"`
	CreatedAt         time.Time       `gorm:"column:created_at"`
	UpdatedAt         time.Time       `gorm:"column:updated_at"`
}

func (operationRow) TableName() string { return "relay_operations" }

type operationItemRow struct {
	ID           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	OperationID  uuid.UUID  `gorm:"column:operation_id;type:uuid"`
	TargetType   string     `gorm:"column:target_type"`
	TargetID     *uuid.UUID `gorm:"column:target_id;type:uuid"`
	Status       string     `gorm:"column:status"`
	Attempts     int        `gorm:"column:attempts"`
	ErrorCode    *string    `gorm:"column:error_code"`
	ErrorMessage *string    `gorm:"column:error_message"`
	StartedAt    *time.Time `gorm:"column:started_at"`
	FinishedAt   *time.Time `gorm:"column:finished_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (operationItemRow) TableName() string { return "relay_operation_items" }

type auditRow struct {
	ID           uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	ActorUserID  *uuid.UUID      `gorm:"column:actor_user_id;type:uuid"`
	Action       string          `gorm:"column:action"`
	ResourceType string          `gorm:"column:resource_type"`
	ResourceID   *uuid.UUID      `gorm:"column:resource_id;type:uuid"`
	BeforeJSON   json.RawMessage `gorm:"column:before_json;type:jsonb"`
	AfterJSON    json.RawMessage `gorm:"column:after_json;type:jsonb"`
	CreatedAt    time.Time       `gorm:"column:created_at"`
}

func (auditRow) TableName() string { return "audit_logs" }

type outboxRow struct {
	ID               uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	AggregateType    string          `gorm:"column:aggregate_type"`
	AggregateID      uuid.UUID       `gorm:"column:aggregate_id;type:uuid"`
	EventType        string          `gorm:"column:event_type"`
	Payload          json.RawMessage `gorm:"column:payload;type:jsonb"`
	Attempts         int             `gorm:"column:attempts"`
	AvailableAt      time.Time       `gorm:"column:available_at"`
	ClaimedAt        *time.Time      `gorm:"column:claimed_at"`
	ClaimToken       *uuid.UUID      `gorm:"column:claim_token;type:uuid"`
	PublishedAt      *time.Time      `gorm:"column:published_at"`
	DeadLetteredAt   *time.Time      `gorm:"column:dead_lettered_at"`
	DeadLetterReason *string         `gorm:"column:dead_letter_reason"`
	LastError        *string         `gorm:"column:last_error"`
	CreatedAt        time.Time       `gorm:"column:created_at"`
}

func (outboxRow) TableName() string { return "relay_outbox" }
