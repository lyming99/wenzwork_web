package remotecontrol

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	DefaultPageLimit       = 50
	MaxPageLimit           = 200
	DefaultCommandLease    = 30 * time.Second
	DefaultCommandTTL      = 24 * time.Hour
	DefaultEventPageLimit  = 200
	MaximumTaskInputBytes  = 16 << 10
	MaximumEventBatch      = 200
	MaximumLogContentBytes = 256 << 10
)

var (
	ErrInvalidInput        = errors.New("remote control input is invalid")
	ErrNotFound            = errors.New("remote control resource was not found")
	ErrForbidden           = errors.New("remote control operation is forbidden")
	ErrConflict            = errors.New("remote control resource revision conflicts")
	ErrIdempotencyConflict = errors.New("remote control idempotency key conflicts with another request")
	ErrSequenceGap         = errors.New("remote control device sequence has a gap")
	ErrPeerRequired        = errors.New("remote task content requires an end-to-end encrypted peer session")
	ErrProtocolVersion     = errors.New("remote peer protocol version has no supported intersection")
	ErrDirectUnavailable   = errors.New("remote direct connection is unavailable")
	ErrUnavailable         = errors.New("remote control service is unavailable")

	idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	capabilityPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,79}$`)
)

var allowedTaskProjectionTypes = map[string]struct{}{
	"codex": {}, "cursor": {}, "hermes": {}, "jcode": {}, "opencode": {},
	"claude": {}, "kimi": {}, "pi": {}, "script": {}, "workflow": {},
	"workspace.inspect": {}, "markdown.render": {}, "ai.summarize": {}, "remote": {},
}

var allowedDeviceScopes = map[string]struct{}{
	"remote.connect":                   {},
	"remote.project.read":              {},
	"remote.project.sync":              {},
	"remote.task.read":                 {},
	"remote.task.write":                {},
	"remote.peer.query":                {},
	"remote.peer.ai.config":            {},
	"remote.peer.ai.chat":              {},
	"remote.peer.ai.tools":             {},
	"remote.peer.terminal":             {},
	"remote.peer.terminal.interactive": {},
	"remote.peer.file.send":            {},
	"remote.peer.file.receive":         {},
	"remote.peer.task.control":         {},
	"remote.peer.events":               {},
}

var allowedControllerScopes = map[string]struct{}{
	"remote.peer.query":                {},
	"remote.peer.ai.config":            {},
	"remote.peer.ai.chat":              {},
	"remote.peer.ai.tools":             {},
	"remote.peer.terminal":             {},
	"remote.peer.terminal.interactive": {},
	"remote.peer.file.send":            {},
	"remote.peer.file.receive":         {},
	"remote.peer.task.control":         {},
	"remote.peer.events":               {},
}

func peerScopeRequiresProject(scope string) bool {
	switch scope {
	case "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.terminal.interactive", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.task.control", "remote.peer.ai.tools", "remote.peer.events":
		return true
	default:
		return false
	}
}

type PageRequest struct {
	Cursor        string
	Limit         int
	AfterRevision *uint64
}

type Device struct {
	ID                   uuid.UUID  `json:"id"`
	InstallationDeviceID uuid.UUID  `json:"installationDeviceId"`
	DeviceName           string     `json:"deviceName"`
	Platform             string     `json:"platform"`
	AgentVersion         string     `json:"agentVersion"`
	Status               string     `json:"status"`
	Presence             string     `json:"presence"`
	Capabilities         []string   `json:"capabilities"`
	Scopes               []string   `json:"scopes"`
	GrantVersion         uint64     `json:"grantVersion"`
	LastSeenAt           *time.Time `json:"lastSeenAt"`
	LastSyncAt           *time.Time `json:"lastSyncAt"`
	RemoteEnabledAt      *time.Time `json:"remoteEnabledAt"`
	ConnectionMode       string     `json:"connectionMode"`
	DirectModeEnabled    bool       `json:"directModeEnabled"`
	DirectAvailable      bool       `json:"directAvailable"`
	DirectTLSEnabled     bool       `json:"directTlsEnabled"`
	DirectIP             *string    `json:"directIp"`
	DirectPort           *uint32    `json:"directPort"`
	UpdatedAt            time.Time  `json:"-"`
}

type DevicePage struct {
	Items      []Device  `json:"items"`
	NextCursor *string   `json:"nextCursor"`
	ObservedAt time.Time `json:"observedAt"`
}

type AccessInput struct {
	UserID         uuid.UUID
	DeviceID       uuid.UUID
	Scopes         []string
	Confirmation   string
	IdempotencyKey string
}

// DeviceDeletionInput identifies a user-owned remote device that should be
// permanently removed from the control plane.
type DeviceDeletionInput struct {
	UserID   uuid.UUID
	DeviceID uuid.UUID
}

// DeviceUpdateInput contains the account-owned metadata a controller may
// change. Agent-reported identity, platform and version fields stay immutable.
type DeviceUpdateInput struct {
	UserID            uuid.UUID
	DeviceID          uuid.UUID
	DeviceName        string
	DirectModeEnabled *bool
}

type AccessResult struct {
	DeviceID     uuid.UUID `json:"deviceId"`
	Status       string    `json:"status"`
	Scopes       []string  `json:"scopes"`
	GrantVersion uint64    `json:"grantVersion"`
	Replayed     bool      `json:"replayed"`
}

type Project struct {
	ID           uuid.UUID `json:"id"`
	DisplayName  string    `json:"displayName"`
	Revision     uint64    `json:"revision"`
	Capabilities []string  `json:"capabilities"`
	ObservedAt   time.Time `json:"observedAt"`
	State        string    `json:"state"`
	UpdatedAt    time.Time `json:"-"`
}

type ProjectPage struct {
	Items         []Project `json:"items"`
	ObservedAt    time.Time `json:"observedAt"`
	DeviceOnline  bool      `json:"deviceOnline"`
	Stale         bool      `json:"stale"`
	NextCursor    *string   `json:"nextCursor"`
	HighWatermark uint64    `json:"highWatermark"`
}

type Task struct {
	ID         uuid.UUID  `json:"id"`
	DeviceID   uuid.UUID  `json:"deviceId"`
	ProjectID  *uuid.UUID `json:"projectId"`
	TaskType   string     `json:"taskType"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	Revision   uint64     `json:"revision"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	ResultCode *string    `json:"resultCode"`
	UpdatedAt  time.Time  `json:"-"`
}

// TaskProjectionDisplayName returns a deliberately generic, reviewed label.
// User-authored titles can contain prompts, paths, customer names, or other
// private text and therefore must never be copied into the cloud projection.
func TaskProjectionDisplayName(taskType string) string {
	switch taskType {
	case "codex":
		return "Codex task"
	case "cursor":
		return "Cursor task"
	case "hermes":
		return "Hermes task"
	case "jcode":
		return "JCode task"
	case "opencode":
		return "OpenCode task"
	case "claude":
		return "Claude task"
	case "kimi":
		return "Kimi task"
	case "pi":
		return "Pi task"
	case "script":
		return "Script task"
	case "workflow":
		return "Workflow task"
	case "workspace.inspect":
		return "Workspace inspection"
	case "markdown.render":
		return "Markdown rendering"
	case "ai.summarize":
		return "AI summary"
	case "remote":
		return "Remote task"
	default:
		return "Remote task"
	}
}

func validTaskProjectionType(taskType string) bool {
	_, ok := allowedTaskProjectionTypes[taskType]
	return ok
}

type TaskPage struct {
	Items         []Task  `json:"items"`
	NextCursor    *string `json:"nextCursor"`
	HighWatermark uint64  `json:"highWatermark"`
	ResetRequired bool    `json:"resetRequired"`
}

type CreateTaskInput struct {
	UserID           uuid.UUID
	DeviceID         uuid.UUID
	ProjectID        *uuid.UUID
	TaskType         string
	Title            string
	ExpectedRevision *uint64
	Input            json.RawMessage
	IdempotencyKey   string
}

type CancelTaskInput struct {
	UserID         uuid.UUID
	TaskID         uuid.UUID
	IdempotencyKey string
}

// RetryTaskInput creates a new run from a finished reviewed task.  The source
// task is retained unchanged so its logs remain an immutable execution record.
type RetryTaskInput struct {
	UserID         uuid.UUID
	TaskID         uuid.UUID
	IdempotencyKey string
}

type SyncProjectInput struct {
	UserID         uuid.UUID
	DeviceID       uuid.UUID
	ProjectID      *uuid.UUID
	AfterSequence  uint64
	HighWatermark  uint64
	IdempotencyKey string
}

type Operation struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	Replayed  bool      `json:"replayed"`
}

type TaskLog struct {
	Stream     string    `json:"stream"`
	Sequence   uint64    `json:"sequence"`
	OccurredAt time.Time `json:"occurredAt"`
	Content    string    `json:"content"`
}

type TaskLogPage struct {
	Items                []TaskLog `json:"items"`
	AckedThroughSequence uint64    `json:"ackedThroughSequence"`
	HasMore              bool      `json:"hasMore"`
}

type TaskEvent struct {
	ID         uint64          `json:"id"`
	EventID    uuid.UUID       `json:"eventId"`
	Type       string          `json:"type"`
	Revision   uint64          `json:"revision"`
	OccurredAt time.Time       `json:"occurredAt"`
	Payload    json.RawMessage `json:"payload"`
}

type TaskEventPage struct {
	Items       []TaskEvent `json:"items"`
	NextEventID uint64      `json:"nextEventId"`
	HasMore     bool        `json:"hasMore"`
}

type DevicePrincipal struct {
	UserID   uuid.UUID
	DeviceID uuid.UUID
}

type DeviceChange struct {
	Sequence     uint64          `json:"sequence"`
	Kind         string          `json:"kind"`
	Operation    string          `json:"operation"`
	ResourceID   uuid.UUID       `json:"resourceId"`
	Revision     uint64          `json:"revision"`
	OccurredAt   time.Time       `json:"occurredAt"`
	DisplayName  string          `json:"displayName,omitempty"`
	Capabilities []string        `json:"capabilities,omitempty"`
	State        string          `json:"state,omitempty"`
	TaskType     string          `json:"taskType,omitempty"`
	Title        string          `json:"title,omitempty"`
	ProjectID    *uuid.UUID      `json:"projectId,omitempty"`
	Status       string          `json:"status,omitempty"`
	ResultCode   *string         `json:"resultCode,omitempty"`
	StartedAt    *time.Time      `json:"startedAt,omitempty"`
	FinishedAt   *time.Time      `json:"finishedAt,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

type PushChangesInput struct {
	BaseHighWatermark uint64         `json:"baseHighWatermark"`
	Reset             bool           `json:"reset"`
	Changes           []DeviceChange `json:"changes"`
}

type PushChangesResult struct {
	HighWatermark uint64 `json:"highWatermark"`
	Applied       int    `json:"applied"`
	Replayed      int    `json:"replayed"`
	ResetRequired bool   `json:"resetRequired"`
}

type Command struct {
	ID               uuid.UUID       `json:"id"`
	Kind             string          `json:"kind"`
	TaskID           *uuid.UUID      `json:"taskId,omitempty"`
	Body             json.RawMessage `json:"body"`
	GrantVersion     uint64          `json:"grantVersion"`
	ExpectedRevision *uint64         `json:"expectedRevision,omitempty"`
	LeaseToken       uuid.UUID       `json:"leaseToken"`
	ExpiresAt        time.Time       `json:"expiresAt"`
	CreatedAt        time.Time       `json:"createdAt"`
}

type CommandPage struct {
	Items       []Command `json:"items"`
	PollAfterMs int       `json:"pollAfterMs"`
}

type AckCommandInput struct {
	LeaseToken  uuid.UUID `json:"leaseToken"`
	Status      string    `json:"status"`
	FailureCode string    `json:"failureCode,omitempty"`
}

type DeviceEventInput struct {
	EventID        uuid.UUID  `json:"eventId"`
	TaskID         uuid.UUID  `json:"taskId"`
	DeviceSequence uint64     `json:"deviceSequence"`
	Type           string     `json:"type"`
	Revision       uint64     `json:"revision"`
	OccurredAt     time.Time  `json:"occurredAt"`
	Status         string     `json:"status,omitempty"`
	ResultCode     *string    `json:"resultCode,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	Log            *TaskLog   `json:"log,omitempty"`
}

type PushEventsInput struct {
	Events []DeviceEventInput `json:"events"`
}

type PushEventsResult struct {
	Accepted int `json:"accepted"`
	Replayed int `json:"replayed"`
}

type ControllerIdentity struct {
	ID                  uuid.UUID  `json:"id"`
	IdentityAlgorithm   string     `json:"identityAlgorithm"`
	IdentityPublicKey   string     `json:"identityPublicKey"`
	PublicKeyThumbprint string     `json:"publicKeyThumbprint"`
	KeyVersion          uint64     `json:"keyVersion"`
	GrantVersion        uint64     `json:"grantVersion"`
	Scopes              []string   `json:"scopes"`
	Status              string     `json:"status"`
	LastUsedAt          *time.Time `json:"lastUsedAt"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

type RegisterControllerInput struct {
	UserID            uuid.UUID
	SessionID         uuid.UUID
	ControllerID      uuid.UUID
	IdentityPublicKey string
	Proof             string
	Scopes            []string
	IdempotencyKey    string
}

type RotateControllerInput struct {
	UserID            uuid.UUID
	SessionID         uuid.UUID
	ControllerID      uuid.UUID
	IdentityPublicKey string
	Proof             string
	IdempotencyKey    string
}

type RevokeControllerInput struct {
	UserID         uuid.UUID
	ControllerID   uuid.UUID
	IdempotencyKey string
}

type BrowserPeerInput struct {
	UserID                      uuid.UUID
	SessionID                   uuid.UUID
	ControllerID                uuid.UUID
	TargetDeviceID              uuid.UUID
	Scope                       string
	ProjectID                   *uuid.UUID
	RequestedMaxDurationSeconds *uint32
	RequestedMaxBytes           *uint64
	IdempotencyKey              string
}

// DeviceLinkInput is deliberately device-scoped. Project identifiers and
// operation scopes are negotiated only after the Client–Device Link is
// encrypted, so neither can appear in this Control Plane request.
type DeviceLinkInput struct {
	UserID                      uuid.UUID
	SessionID                   uuid.UUID
	ControllerID                uuid.UUID
	TargetDeviceID              uuid.UUID
	ClientIdentityKeyVersion    uint64
	RequestedMaximumLifetimeSec *uint32
	IdempotencyKey              string
}

// DeviceLinkRevocationInput identifies a previously-issued v2
// DeviceConnectionGrant. GrantID is a non-bearer UUID; the signed Grant is
// never persisted or sent to this endpoint.
type DeviceLinkRevocationInput struct {
	UserID       uuid.UUID
	ControllerID uuid.UUID
	GrantID      uuid.UUID
}

type PeerIssueInput struct {
	UserID                      uuid.UUID
	SessionID                   uuid.UUID
	ControllerID                uuid.UUID
	ControllerPublicKey         ed25519.PublicKey
	ControllerKeyThumbprint     string
	ControllerKeyVersion        uint64
	ControllerGrantVersion      uint64
	TargetDeviceID              uuid.UUID
	TargetPublicKey             ed25519.PublicKey
	TargetKeyThumbprint         string
	TargetKeyVersion            uint64
	TargetGrantVersion          uint64
	Scope                       string
	ProjectID                   *uuid.UUID
	RequestedMaxDurationSeconds *uint32
	RequestedMaxBytes           *uint64
	IdempotencyKey              string
}

type DeviceLinkIssueInput struct {
	UserID                      uuid.UUID
	SessionID                   uuid.UUID
	ControllerID                uuid.UUID
	ControllerPublicKey         ed25519.PublicKey
	ControllerKeyThumbprint     string
	ControllerKeyVersion        uint64
	ControllerGrantVersion      uint64
	TargetDeviceID              uuid.UUID
	TargetPublicKey             ed25519.PublicKey
	TargetKeyThumbprint         string
	TargetKeyVersion            uint64
	TargetGrantVersion          uint64
	AllowedScopes               []string
	RequestedMaximumLifetimeSec *uint32
	IdempotencyKey              string
}

// DeviceLink is returned only over the authenticated Control Plane response.
// device_connection_grant is never suitable for a URL, log field, UI store or
// persistence; its holder must additionally prove the Client identity key.
type DeviceLink struct {
	// GrantID is a non-bearer revocation handle. It is intentionally distinct
	// from Grant, which remains proof-bound and must never be exposed in logs.
	GrantID                  uuid.UUID `json:"grantId"`
	Grant                    string    `json:"deviceConnectionGrant"`
	ExpiresAt                time.Time `json:"expiresAt"`
	MaximumLifetimeSeconds   uint32    `json:"maximumLifetimeSeconds"`
	ConnectionMode           string    `json:"connectionMode"`
	ConnectionURL            string    `json:"connectionUrl"`
	RelayURL                 string    `json:"relayUrl"`
	RelayNodeID              uuid.UUID `json:"relayNodeId"`
	RelayCellID              uuid.UUID `json:"relayCellId"`
	TargetConnectionEpoch    uint64    `json:"targetConnectionEpoch"`
	DeviceIdentityAlgorithm  string    `json:"deviceIdentityAlgorithm"`
	DeviceIdentityPublicKey  string    `json:"deviceIdentityPublicKey"`
	DeviceKeyThumbprint      string    `json:"deviceKeyThumbprint"`
	DeviceIdentityKeyVersion uint64    `json:"deviceIdentityKeyVersion"`
}

type PeerSession struct {
	SessionID               uuid.UUID             `json:"sessionId"`
	PeerSessionTicket       string                `json:"peerSessionTicket"`
	WebSocketSubprotocols   []string              `json:"webSocketSubprotocols"`
	ExpiresAt               time.Time             `json:"expiresAt"`
	MaxDurationSeconds      uint32                `json:"maxDurationSeconds"`
	MaxBytes                uint64                `json:"maxBytes"`
	TargetIdentityAlgorithm string                `json:"targetIdentityAlgorithm"`
	TargetIdentityPublicKey string                `json:"targetIdentityPublicKey"`
	TargetKeyThumbprint     string                `json:"targetKeyThumbprint"`
	TargetKeyVersion        uint64                `json:"targetKeyVersion"`
	RelayURL                string                `json:"relayUrl"`
	RelayNodeID             uuid.UUID             `json:"relayNodeId"`
	RelayCellID             uuid.UUID             `json:"relayCellId"`
	TargetConnectionEpoch   uint64                `json:"targetConnectionEpoch"`
	ProtocolMinimum         uint32                `json:"protocolMinimum"`
	ProtocolMaximum         uint32                `json:"protocolMaximum"`
	NegotiatedProtocol      uint32                `json:"negotiatedProtocolVersion"`
	Limits                  PeerProtocolLimits    `json:"limits"`
	EventCapabilities       PeerEventCapabilities `json:"eventCapabilities"`
}

type PeerProtocolLimits struct {
	PeerPlaintextBytes    uint32 `json:"peerPlaintextBytes"`
	RPCJSONBytes          uint32 `json:"rpcJsonBytes"`
	PreferredPageBytes    uint32 `json:"preferredPageBytes"`
	TaskPayloadBytes      uint32 `json:"taskPayloadBytes"`
	TaskPayloadChunkBytes uint32 `json:"taskPayloadChunkBytes"`
}

type PeerEventCapabilities struct {
	ContractVersion uint32   `json:"contractVersion"`
	AcceptedKinds   []string `json:"acceptedEventKinds"`
	CollaborationV1 bool     `json:"collaborationV1"`
}

type PeerIssuer interface {
	IssueBrowserPeer(context.Context, PeerIssueInput) (PeerSession, error)
}

type DeviceLinkIssuer interface {
	IssueDeviceLink(context.Context, DeviceLinkIssueInput) (DeviceLink, error)
}

// DeviceLinkGrantRevoker projects an authenticated Control Plane revocation
// to the Relay fleet. Implementations retain only a grant-ID digest, never a
// signed DeviceConnectionGrant.
type DeviceLinkGrantRevoker interface {
	RevokeDeviceLinkGrant(grantID string, expiresAt time.Time) error
}

type cursorPayload struct {
	Version int       `json:"v"`
	Kind    string    `json:"k"`
	Time    time.Time `json:"t"`
	ID      uuid.UUID `json:"id"`
}

type cursorCodec struct{ key [32]byte }

func newCursorCodec(secret []byte) (cursorCodec, error) {
	if len(secret) < 16 {
		return cursorCodec{}, errors.New("remote control cursor key must contain at least 16 bytes")
	}
	return cursorCodec{key: sha256.Sum256(secret)}, nil
}

func (codec cursorCodec) encode(kind string, timestamp time.Time, id uuid.UUID) (string, error) {
	payload, err := json.Marshal(cursorPayload{Version: 1, Kind: kind, Time: timestamp.UTC(), ID: id})
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, codec.key[:])
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (codec cursorCodec) decode(value, kind string) (cursorPayload, error) {
	if strings.TrimSpace(value) == "" {
		return cursorPayload{}, nil
	}
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return cursorPayload{}, ErrInvalidInput
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(payload) > 512 {
		return cursorPayload{}, ErrInvalidInput
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return cursorPayload{}, ErrInvalidInput
	}
	mac := hmac.New(sha256.New, codec.key[:])
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return cursorPayload{}, ErrInvalidInput
	}
	var result cursorPayload
	if json.Unmarshal(payload, &result) != nil || result.Version != 1 || result.Kind != kind || result.Time.IsZero() || result.ID == uuid.Nil {
		return cursorPayload{}, ErrInvalidInput
	}
	return result, nil
}

func normalizeLimit(value int) (int, error) {
	if value == 0 {
		return DefaultPageLimit, nil
	}
	if value < 1 || value > MaxPageLimit {
		return 0, ErrInvalidInput
	}
	return value, nil
}

func normalizeScopes(values []string, allowed map[string]struct{}) ([]string, error) {
	if len(values) == 0 || len(values) > len(allowed) {
		return nil, ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if _, ok := allowed[value]; !ok {
			return nil, ErrInvalidInput
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, ErrInvalidInput
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func containsScope(scopes []string, required string) bool {
	for _, scope := range scopes {
		if scope == required {
			return true
		}
	}
	return false
}

// deviceLinkAllowedScopes is the encrypted-Link authorization ceiling for an
// owner-authorised controller. It contains no project identity; project
// selection is independently checked when the Device receives CHANNEL_OPEN.
func deviceLinkAllowedScopes() []string {
	result := make([]string, 0, len(allowedControllerScopes))
	for scope := range allowedControllerScopes {
		result = append(result, scope)
	}
	sort.Strings(result)
	return result
}

func decodePublicKey(encoded string) (ed25519.PublicKey, string, error) {
	encoded = strings.TrimSpace(encoded)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return nil, "", ErrInvalidInput
	}
	digest := sha256.Sum256(raw)
	return ed25519.PublicKey(raw), base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func controllerProofTranscript(userID, controllerID uuid.UUID, publicKey string, keyVersion uint64) []byte {
	return []byte(fmt.Sprintf("wenzwork-browser-controller:v2\n%s\n%s\n%s\n%d", userID, controllerID, publicKey, keyVersion))
}

func verifyControllerProof(userID, controllerID uuid.UUID, publicKey ed25519.PublicKey, encodedKey, proof string, keyVersion uint64) error {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(proof))
	if err != nil || len(raw) != ed25519.SignatureSize || !ed25519.Verify(publicKey, controllerProofTranscript(userID, controllerID, encodedKey, keyVersion), raw) {
		return ErrInvalidInput
	}
	return nil
}

func validateTaskInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > MaximumTaskInputBytes || !json.Valid(raw) {
		return nil, ErrInvalidInput
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, ErrInvalidInput
	}
	if _, ok := value.(map[string]any); !ok || containsSensitiveField(value) {
		return nil, ErrInvalidInput
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalidInput
	}
	return canonical, nil
}

func containsSensitiveField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
			switch normalized {
			case "prompt", "systemprompt", "reply", "response", "message", "messages", "content", "body", "text",
				"apikey", "apiaccesskey", "secret", "token", "password", "credential", "filepath", "path", "filename", "filecontent",
				"attachment", "attachments", "attachedfilepaths", "environment", "env", "command", "arguments", "args",
				"stdout", "stderr", "output", "toolresult", "tooloutput":
				return true
			}
			if containsSensitiveField(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsSensitiveField(nested) {
				return true
			}
		}
	}
	return false
}

func validText(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 && r != '\t' {
			return false
		}
	}
	return true
}

func requestHash(value any) string {
	payload, _ := json.Marshal(value)
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func validIdempotencyKey(value string) bool {
	return idempotencyPattern.MatchString(strings.TrimSpace(value))
}
