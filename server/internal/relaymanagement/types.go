package relaymanagement

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"gorm.io/gorm"
)

var (
	ErrInvalidInput        = errors.New("relay management input is invalid")
	ErrNotFound            = errors.New("relay management resource not found")
	ErrConflict            = errors.New("relay management state conflict")
	ErrVersionConflict     = errors.New("relay management version conflict")
	ErrEnrollmentInvalid   = errors.New("relay enrollment credentials are invalid")
	ErrEnrollmentExpired   = errors.New("relay enrollment token expired")
	ErrEnrollmentConsumed  = errors.New("relay enrollment token already consumed")
	ErrAccessKeyInvalid    = errors.New("relay access key is invalid")
	ErrIdentityMismatch    = errors.New("relay identity does not match installation")
	ErrActivationBlocked   = errors.New("relay installation is not ready for activation")
	ErrInstallationRevoked = errors.New("relay installation has been revoked")
)

type CertificateIssuer interface {
	IssueNode(ed25519.PublicKey, string, string, string, time.Time, time.Duration) (relayidentity.IssuedCertificate, error)
}

type Store struct {
	db                *gorm.DB
	issuer            CertificateIssuer
	now               func() time.Time
	random            io.Reader
	tokenTTL          time.Duration
	certificateTTL    time.Duration
	nodeLeaseDuration time.Duration
	agentConfigMu     sync.RWMutex
	agentConfig       AgentRuntimeConfiguration
	routePublisher    HeartbeatRoutePublisher
}

type Option func(*Store)

func WithClock(now func() time.Time) Option {
	return func(store *Store) { store.now = now }
}

func WithRandom(random io.Reader) Option {
	return func(store *Store) { store.random = random }
}

func WithTokenTTL(ttl time.Duration) Option {
	return func(store *Store) { store.tokenTTL = ttl }
}

func WithCertificateTTL(ttl time.Duration) Option {
	return func(store *Store) { store.certificateTTL = ttl }
}

func WithNodeLeaseDuration(ttl time.Duration) Option {
	return func(store *Store) { store.nodeLeaseDuration = ttl }
}

type Region struct {
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	DataResidency string    `json:"dataResidency"`
	Status        string    `json:"status"`
	Pools         []Pool    `json:"pools"`
}

type Pool struct {
	ID     uuid.UUID `json:"id"`
	Code   string    `json:"code"`
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Cells  []Cell    `json:"cells"`
}

type Cell struct {
	ID                  uuid.UUID `json:"id"`
	Code                string    `json:"code"`
	Name                string    `json:"name"`
	FailureDomain       string    `json:"failureDomain"`
	Status              string    `json:"status"`
	Weight              float64   `json:"weight"`
	ConnectionSoftLimit int64     `json:"connectionSoftLimit"`
	ConnectionHardLimit int64     `json:"connectionHardLimit"`
	ProtocolMin         int       `json:"protocolMin"`
	ProtocolMax         int       `json:"protocolMax"`
	ActiveEndpoint      *Endpoint `json:"activeEndpoint"`
	InstallationCount   int64     `json:"installationCount"`
	HealthyInstances    int64     `json:"healthyInstances"`
}

type Endpoint struct {
	ID             uuid.UUID  `json:"id"`
	Revision       int64      `json:"revision"`
	EndpointType   string     `json:"endpointType"`
	PublicEndpoint string     `json:"publicEndpoint"`
	Status         string     `json:"status"`
	ValidatedAt    *time.Time `json:"validatedAt"`
	ActivatedAt    *time.Time `json:"activatedAt"`
}

type ManagedEndpoint struct {
	ID                  uuid.UUID       `json:"id"`
	CellID              uuid.UUID       `json:"cellId"`
	Revision            int64           `json:"revision"`
	EndpointType        string          `json:"endpointType"`
	PublicEndpoint      string          `json:"publicEndpoint"`
	Status              string          `json:"status"`
	ValidationResult    json.RawMessage `json:"validationResult"`
	CertificateNotAfter *time.Time      `json:"certificateNotAfter"`
	ValidatedAt         *time.Time      `json:"validatedAt"`
	ActivatedAt         *time.Time      `json:"activatedAt"`
	DrainUntil          *time.Time      `json:"drainUntil"`
	SupersededAt        *time.Time      `json:"supersededAt"`
	Version             int64           `json:"version"`
	CreatedAt           time.Time       `json:"createdAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
}

type DeploymentChecklist struct {
	LoadBalancer bool `json:"lb"`
	DNS          bool `json:"dns"`
	Port         bool `json:"port"`
	TLS          bool `json:"tls"`
}

func (checklist DeploymentChecklist) Complete() bool {
	return checklist.LoadBalancer && checklist.DNS && checklist.Port && checklist.TLS
}

type Installation struct {
	ID                  uuid.UUID           `json:"id"`
	CellID              uuid.UUID           `json:"cellId"`
	CellCode            string              `json:"cellCode"`
	ReleaseID           *uuid.UUID          `json:"releaseId"`
	DisplayName         string              `json:"displayName"`
	Region              string              `json:"region"`
	Group               string              `json:"group"`
	FailureDomain       string              `json:"failureDomain"`
	OperationsNote      string              `json:"operationsNote"`
	PublicEndpoint      string              `json:"publicEndpoint"`
	ListenerPort        int                 `json:"listenerPort"`
	Platform            string              `json:"platform"`
	Architecture        string              `json:"architecture"`
	Status              string              `json:"status"`
	IdentityThumbprint  *string             `json:"identityThumbprint"`
	DeploymentChecklist DeploymentChecklist `json:"deploymentChecklist"`
	FirstEnrolledAt     *time.Time          `json:"firstEnrolledAt"`
	ActivatedAt         *time.Time          `json:"activatedAt"`
	RevokedAt           *time.Time          `json:"revokedAt"`
	Version             int64               `json:"version"`
	CurrentInstance     *NodeInstance       `json:"currentInstance"`
	Instances           []NodeInstance      `json:"instances"`
	CreatedAt           time.Time           `json:"createdAt"`
	UpdatedAt           time.Time           `json:"updatedAt"`
}

type NodeInstance struct {
	ID                  uuid.UUID      `json:"id"`
	InstallationID      uuid.UUID      `json:"installationId"`
	CellID              uuid.UUID      `json:"cellId"`
	Status              string         `json:"status"`
	Version             string         `json:"version"`
	ProtocolVersion     int            `json:"protocolVersion"`
	Addresses           []string       `json:"addresses"`
	Capabilities        map[string]any `json:"capabilities"`
	ActiveConnections   int64          `json:"activeConnections"`
	ActiveFileTransfers int64          `json:"activeFileTransfers"`
	MemoryBytes         int64          `json:"memoryBytes"`
	IngressMbps         float64        `json:"ingressMbps"`
	EgressMbps          float64        `json:"egressMbps"`
	WriteLoopLagMS      float64        `json:"writeLoopLagMs"`
	StartedAt           time.Time      `json:"startedAt"`
	LastHeartbeatAt     time.Time      `json:"lastHeartbeatAt"`
	LeaseExpiresAt      time.Time      `json:"leaseExpiresAt"`
	StoppedAt           *time.Time     `json:"stoppedAt"`
}

type Release struct {
	ID                uuid.UUID  `json:"id"`
	Version           string     `json:"version"`
	Platform          string     `json:"platform"`
	Architecture      string     `json:"architecture"`
	ProtocolMin       int        `json:"protocolMin"`
	ProtocolMax       int        `json:"protocolMax"`
	BuildCommit       string     `json:"buildCommit"`
	BuildTime         time.Time  `json:"buildTime"`
	SigningKeyID      string     `json:"signingKeyId"`
	ManifestSHA256    string     `json:"manifestSha256"`
	ManifestSignature string     `json:"manifestSignature"`
	Status            string     `json:"status"`
	Artifacts         []Artifact `json:"artifacts"`
}

type Artifact struct {
	ID            uuid.UUID `json:"id"`
	FileName      string    `json:"fileName"`
	FileSizeBytes int64     `json:"fileSizeBytes"`
	SHA256        string    `json:"sha256"`
	Signature     string    `json:"signature"`
	ObjectKey     string    `json:"objectKey"`
}

type SaveReleaseArtifactInput struct {
	FileName      string `json:"fileName"`
	FileSizeBytes int64  `json:"fileSizeBytes"`
	SHA256        string `json:"sha256"`
	Signature     string `json:"signature"`
	ObjectKey     string `json:"objectKey"`
}

type SaveReleaseInput struct {
	Version           string                     `json:"version"`
	Platform          string                     `json:"platform"`
	Architecture      string                     `json:"architecture"`
	ProtocolMin       int                        `json:"protocolMin"`
	ProtocolMax       int                        `json:"protocolMax"`
	BuildCommit       string                     `json:"buildCommit"`
	BuildTime         time.Time                  `json:"buildTime"`
	SigningKeyID      string                     `json:"signingKeyId"`
	ManifestSHA256    string                     `json:"manifestSha256"`
	ManifestSignature string                     `json:"manifestSignature"`
	Artifacts         []SaveReleaseArtifactInput `json:"artifacts"`
	ActorUserID       uuid.UUID                  `json:"-"`
}

type BootstrapReleaseArtifact struct {
	ReleaseVersion string
	Platform       string
	Architecture   string
	ObjectKey      string
	FileName       string
}

type CreateInstallationInput struct {
	CellID         uuid.UUID
	ReleaseID      *uuid.UUID
	DisplayName    string
	Region         string
	Group          string
	FailureDomain  string
	OperationsNote string
	PublicEndpoint string
	ListenerPort   int
	Platform       string
	Architecture   string
	ActorUserID    uuid.UUID
}

type UpdateInstallationInput struct {
	DisplayName         string
	Region              string
	Group               string
	FailureDomain       string
	OperationsNote      string
	PublicEndpoint      string
	ListenerPort        int
	DeploymentChecklist DeploymentChecklist
	ExpectedVersion     int64
	ActorUserID         uuid.UUID
}

type EnrollmentToken struct {
	ID             uuid.UUID `json:"id"`
	InstallationID uuid.UUID `json:"installationId"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

// AccessKey is a long-lived, revocable credential for one Relay installation.
// Key is returned only when the credential is created and is never persisted
// in plaintext by the management service.
type AccessKey struct {
	ID             uuid.UUID `json:"id"`
	InstallationID uuid.UUID `json:"installationId"`
	Key            string    `json:"key"`
	KeyPrefix      string    `json:"keyPrefix"`
	CreatedAt      time.Time `json:"createdAt"`
}

type AccessKeyBinding struct {
	InstallationID       uuid.UUID                 `json:"installationId"`
	CellID               uuid.UUID                 `json:"cellId"`
	Status               string                    `json:"status"`
	ConfigurationVersion int64                     `json:"configurationVersion"`
	Configuration        AgentRuntimeConfiguration `json:"configuration"`
}

// AgentRuntimeConfiguration is the complete, management-owned runtime
// configuration returned to a Relay after Access Key authentication. Public
// verification keys are encoded as unpadded base64url Ed25519 keys so Relay
// hosts do not need key files provisioned separately.
type AgentRuntimeConfiguration struct {
	ProtocolVersion           int               `json:"protocolVersion"`
	PublicEndpoint            string            `json:"publicEndpoint"`
	BrowserOriginPatterns     []string          `json:"browserOriginPatterns"`
	ListenAddress             string            `json:"listenAddress"`
	HealthAddress             string            `json:"healthAddress"`
	RedisURL                  string            `json:"redisUrl"`
	TicketIssuer              string            `json:"ticketIssuer"`
	TicketPublicKeys          map[string]string `json:"ticketPublicKeys"`
	DeviceLinkGrantPublicKeys map[string]string `json:"deviceLinkGrantPublicKeys"`
	ConnectionHardLimit       int               `json:"connectionHardLimit"`
	HandshakeConcurrency      int               `json:"handshakeConcurrency"`
}

type EnrollmentRequest struct {
	InstallationID  string         `json:"installationId"`
	CellID          string         `json:"cellId"`
	PublicKey       string         `json:"publicKey"`
	Nonce           string         `json:"nonce"`
	Timestamp       time.Time      `json:"timestamp"`
	Signature       string         `json:"signature"`
	Version         string         `json:"version"`
	ProtocolVersion int            `json:"protocolVersion"`
	Addresses       []string       `json:"addresses"`
	Capabilities    map[string]any `json:"capabilities"`
}

type EnrollmentResult struct {
	InstallationID          uuid.UUID `json:"installationId"`
	CellID                  uuid.UUID `json:"cellId"`
	IdentityThumbprint      string    `json:"identityThumbprint"`
	CertificatePEM          string    `json:"certificatePem"`
	CertificateAuthorityPEM string    `json:"certificateAuthorityPem"`
	CertificateExpiresAt    time.Time `json:"certificateExpiresAt"`
	DirectoryURL            string    `json:"directoryUrl"`
}

type BootstrapInstallation struct {
	InstallationID uuid.UUID `json:"installationId"`
	CellID         uuid.UUID `json:"cellId"`
	Platform       string    `json:"platform"`
	Architecture   string    `json:"architecture"`
	ReleaseVersion string    `json:"releaseVersion"`
	ProtocolMin    int       `json:"protocolMin"`
	ProtocolMax    int       `json:"protocolMax"`
}

type CreateInstallSessionInput struct {
	InstallationID uuid.UUID
	ReleaseID      uuid.UUID
	Mode           string
	Action         string
	ActorUserID    uuid.UUID
}

type InstallSession struct {
	ID             uuid.UUID `json:"id"`
	InstallationID uuid.UUID `json:"installationId"`
	ReleaseID      uuid.UUID `json:"releaseId"`
	Mode           string    `json:"mode"`
	Status         string    `json:"status"`
	ExpiresAt      time.Time `json:"expiresAt"`
	CreatedAt      time.Time `json:"createdAt"`
}

type ActivateInstallationInput struct {
	ExpectedThumbprint string
	Checklist          DeploymentChecklist
	Confirmation       string
	ActorUserID        uuid.UUID
}

type NodeCertificateIdentity struct {
	InstallationID uuid.UUID
	CellID         uuid.UUID
	Thumbprint     string
}

type RegisterInstanceInput struct {
	InstanceID      uuid.UUID      `json:"instanceId"`
	Version         string         `json:"version"`
	ProtocolVersion int            `json:"protocolVersion"`
	Addresses       []string       `json:"addresses"`
	Capabilities    map[string]any `json:"capabilities"`
	StartedAt       time.Time      `json:"startedAt"`
}

type HeartbeatInput struct {
	InstanceID           uuid.UUID        `json:"instanceId"`
	ConfigurationVersion int64            `json:"configurationVersion,omitempty"`
	ActiveConnections    int64            `json:"activeConnections"`
	ActiveFileTransfers  int64            `json:"activeFileTransfers"`
	MemoryBytes          int64            `json:"memoryBytes"`
	IngressMbps          float64          `json:"ingressMbps"`
	EgressMbps           float64          `json:"egressMbps"`
	WriteLoopLagMS       float64          `json:"writeLoopLagMs"`
	Addresses            []string         `json:"addresses"`
	Capabilities         map[string]any   `json:"capabilities"`
	ResidentRoutes       []HeartbeatRoute `json:"residentRoutes,omitempty"`
}

// HeartbeatRoute is the resident connection fact Relay presents to Host. Cell
// and node identity are derived from the authenticated Relay installation and
// cannot be supplied by the Relay payload.
type HeartbeatRoute struct {
	DeviceID          string `json:"deviceId"`
	UserID            string `json:"userId"`
	ConnectionID      string `json:"connectionId"`
	ConnectionEpoch   uint64 `json:"connectionEpoch"`
	AssignmentVersion uint64 `json:"assignmentVersion"`
	GrantVersion      uint64 `json:"grantVersion"`
	ProtocolVersion   uint32 `json:"protocolVersion"`
}

type HeartbeatRouteRejection struct {
	DeviceID        string `json:"deviceId"`
	ConnectionID    string `json:"connectionId"`
	ConnectionEpoch uint64 `json:"connectionEpoch"`
	Reason          string `json:"reason"`
}

type HeartbeatRoutePublisher interface {
	PublishRelayRoutes(context.Context, uuid.UUID, uuid.UUID, []HeartbeatRoute, time.Duration, time.Time) error
}

type HeartbeatResult struct {
	LeaseExpiresAt       time.Time                 `json:"leaseExpiresAt"`
	Drain                bool                      `json:"drain"`
	Revoked              bool                      `json:"revoked"`
	RoutingReady         bool                      `json:"routingReady"`
	ConfigurationVersion int64                     `json:"configurationVersion"`
	Configuration        AgentRuntimeConfiguration `json:"configuration"`
	RestartRequired      bool                      `json:"restartRequired"`
	RejectedRoutes       []HeartbeatRouteRejection `json:"rejectedRoutes,omitempty"`
}

type UpdateCellInput struct {
	Status                     *string   `json:"status,omitempty"`
	Weight                     *float64  `json:"weight,omitempty"`
	ConnectionSoftLimit        *int64    `json:"connectionSoftLimit,omitempty"`
	ConnectionHardLimit        *int64    `json:"connectionHardLimit,omitempty"`
	FileBandwidthSoftLimitMbps *float64  `json:"fileBandwidthSoftLimitMbps,omitempty"`
	FileBandwidthHardLimitMbps *float64  `json:"fileBandwidthHardLimitMbps,omitempty"`
	ActorUserID                uuid.UUID `json:"-"`
	IdempotencyKey             string    `json:"-"`
}

type CreateEndpointInput struct {
	CellID         uuid.UUID `json:"cellId"`
	EndpointType   string    `json:"endpointType"`
	PublicEndpoint string    `json:"publicEndpoint"`
	ActorUserID    uuid.UUID `json:"-"`
	IdempotencyKey string    `json:"-"`
}

type UpdateEndpointInput struct {
	EndpointType   string    `json:"endpointType"`
	PublicEndpoint string    `json:"publicEndpoint"`
	ActorUserID    uuid.UUID `json:"-"`
	IdempotencyKey string    `json:"-"`
}

type Assignment struct {
	ID                uuid.UUID   `json:"id"`
	UserID            uuid.UUID   `json:"userId"`
	CellID            uuid.UUID   `json:"cellId"`
	CellCode          string      `json:"cellCode"`
	AssignmentVersion int64       `json:"assignmentVersion"`
	Mode              string      `json:"mode"`
	Status            string      `json:"status"`
	FallbackCellIDs   []uuid.UUID `json:"fallbackCellIds"`
	LeaseExpiresAt    time.Time   `json:"leaseExpiresAt"`
	EffectiveAt       *time.Time  `json:"effectiveAt"`
	SupersededAt      *time.Time  `json:"supersededAt"`
	CreatedAt         time.Time   `json:"createdAt"`
	UpdatedAt         time.Time   `json:"updatedAt"`
}

type MigrateUserInput struct {
	UserID         uuid.UUID  `json:"userId"`
	Mode           string     `json:"mode"`
	TargetCellID   *uuid.UUID `json:"targetCellId"`
	Confirmation   string     `json:"confirmation"`
	ActorUserID    uuid.UUID  `json:"-"`
	IdempotencyKey string     `json:"-"`
}

type OperationItem struct {
	ID           uuid.UUID  `json:"id"`
	TargetType   string     `json:"targetType"`
	TargetID     *uuid.UUID `json:"targetId"`
	Status       string     `json:"status"`
	Attempts     int        `json:"attempts"`
	ErrorCode    *string    `json:"errorCode"`
	ErrorMessage *string    `json:"errorMessage"`
	StartedAt    *time.Time `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt"`
}

type Operation struct {
	ID                uuid.UUID       `json:"id"`
	Type              string          `json:"type"`
	Status            string          `json:"status"`
	TargetType        string          `json:"targetType"`
	TargetID          *uuid.UUID      `json:"targetId"`
	Request           json.RawMessage `json:"-"`
	ProgressCompleted int             `json:"progressCompleted"`
	ProgressTotal     int             `json:"progressTotal"`
	ProgressPercent   int             `json:"progressPercent"`
	ResultCode        *string         `json:"resultCode"`
	ErrorMessage      *string         `json:"errorMessage"`
	Items             []OperationItem `json:"items"`
	StartedAt         *time.Time      `json:"-"`
	FinishedAt        *time.Time      `json:"-"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

type OperationClaim struct {
	Operation
	ClaimToken  uuid.UUID
	ActorUserID *uuid.UUID
}

type EndpointValidationResult struct {
	Checks              map[string]bool `json:"checks"`
	ResolvedAddresses   []string        `json:"resolvedAddresses"`
	CertificateNotAfter time.Time       `json:"certificateNotAfter"`
	InstallationID      uuid.UUID       `json:"installationId"`
	InstanceID          uuid.UUID       `json:"instanceId"`
	CellID              uuid.UUID       `json:"cellId"`
}

type OperationalMetrics struct {
	Installations             map[string]int64
	Instances                 map[string]int64
	AccessKeys                map[string]int64
	EnrollmentTokens          map[string]int64
	EnrollmentFailures        int64
	Certificates              map[string]int64
	Operations                map[string]int64
	OldestOperationAge        time.Duration
	CurrentLeaseExpired       int64
	OldestCurrentHeartbeatAge time.Duration
	Cells                     []CellOperationalMetrics
}

type CellOperationalMetrics struct {
	Code                string
	Status              string
	ActiveConnections   int64
	ConnectionHardLimit int64
	HealthyInstances    int64
}

type CreateOperationInput struct {
	Type            string
	TargetType      string
	TargetID        *uuid.UUID
	Request         any
	ItemTargetType  string
	ItemTargetIDs   []uuid.UUID
	AdditionalItems []OperationTarget
	ActorUserID     uuid.UUID
	IdempotencyKey  string
}

type OperationTarget struct {
	TargetType string
	TargetID   uuid.UUID
}

// AdvancedService contains the asynchronous management surface. Keeping it
// separate lets bootstrap-only test doubles implement the smaller Service.
type AdvancedService interface {
	ListNodes(context.Context, uuid.UUID) ([]NodeInstance, error)
	ListEndpoints(context.Context, uuid.UUID) ([]ManagedEndpoint, error)
	RequestCellUpdate(context.Context, uuid.UUID, UpdateCellInput) (Operation, error)
	CreateEndpoint(context.Context, CreateEndpointInput) (Operation, error)
	UpdateEndpoint(context.Context, uuid.UUID, UpdateEndpointInput) (Operation, error)
	RequestEndpointValidation(context.Context, uuid.UUID, uuid.UUID, string) (Operation, error)
	RequestEndpointActivation(context.Context, uuid.UUID, uuid.UUID, string) (Operation, error)
	RequestNodeDrain(context.Context, uuid.UUID, uuid.UUID, string) (Operation, error)
	RequestCellDrain(context.Context, uuid.UUID, uuid.UUID, string) (Operation, error)
	ListAssignments(context.Context, uuid.UUID) ([]Assignment, error)
	RequestUserMigration(context.Context, MigrateUserInput) (Operation, error)
	RequestUserUnpin(context.Context, uuid.UUID, uuid.UUID, string) (Operation, error)
	GetOperation(context.Context, uuid.UUID) (Operation, error)
}

// ReleaseAdminService is intentionally separate from Service so lightweight
// Relay Agent and bootstrap test doubles do not need management-only methods.
type ReleaseAdminService interface {
	ListManagedReleases(context.Context) ([]Release, error)
	CreateRelease(context.Context, SaveReleaseInput) (Release, error)
	UpdateRelease(context.Context, uuid.UUID, SaveReleaseInput) (Release, error)
	PublishRelease(context.Context, uuid.UUID, uuid.UUID) (Release, error)
	RetireRelease(context.Context, uuid.UUID, uuid.UUID) (Release, error)
	DeleteRelease(context.Context, uuid.UUID, uuid.UUID) error
}

// Service is the shared boundary used by HTTP handlers and command programs.
type Service interface {
	ListTopology(context.Context) ([]Region, error)
	ListInstallations(context.Context, *uuid.UUID) ([]Installation, error)
	GetInstallation(context.Context, uuid.UUID) (Installation, error)
	CreateInstallation(context.Context, CreateInstallationInput) (Installation, error)
	UpdateInstallation(context.Context, uuid.UUID, UpdateInstallationInput) (Installation, error)
	DeleteInstallation(context.Context, uuid.UUID, uuid.UUID) error
	CreateAccessKey(context.Context, uuid.UUID, uuid.UUID) (AccessKey, error)
	ResolveAccessKey(context.Context, string) (AccessKeyBinding, error)
	RegisterInstanceWithAccessKey(context.Context, string, RegisterInstanceInput) (NodeInstance, error)
	HeartbeatWithAccessKey(context.Context, string, HeartbeatInput) (HeartbeatResult, error)
	UnregisterInstanceWithAccessKey(context.Context, string, uuid.UUID) error
	CreateEnrollmentToken(context.Context, uuid.UUID, uuid.UUID) (EnrollmentToken, error)
	GetBootstrapInstallation(context.Context, uuid.UUID) (BootstrapInstallation, error)
	Enroll(context.Context, string, EnrollmentRequest) (EnrollmentResult, error)
	CreateInstallSession(context.Context, CreateInstallSessionInput) (InstallSession, error)
	GetBootstrapReleaseArtifact(context.Context, uuid.UUID) (BootstrapReleaseArtifact, error)
	ActivateInstallation(context.Context, uuid.UUID, ActivateInstallationInput) (Installation, error)
	RevokeInstallation(context.Context, uuid.UUID, uuid.UUID, string) error
	ListReleases(context.Context) ([]Release, error)
}
