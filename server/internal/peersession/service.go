package peersession

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"gorm.io/gorm"
)

const (
	// A Peer ticket is an admission credential.  It may remain available for
	// thirty days, but an accepted Peer does not use this duration as a session
	// lease after PeerReady.
	DefaultTicketTTL          = 30 * 24 * time.Hour
	DefaultMaxDuration        = 30 * 24 * time.Hour
	DefaultMaxBytes    uint64 = 16 << 20
)

var (
	ErrInvalidRequest      = errors.New("peer session ticket request is invalid")
	ErrSourceForbidden     = errors.New("peer session source is forbidden")
	ErrTargetNotFound      = errors.New("peer session target was not found")
	ErrTargetForbidden     = errors.New("peer session target is forbidden")
	ErrDeviceInactive      = errors.New("peer session device is inactive")
	ErrTargetOffline       = errors.New("peer session target is offline")
	ErrRelayUnavailable    = errors.New("peer session target Relay is unavailable")
	ErrIdempotencyConflict = errors.New("peer session idempotency key conflicts with another request")
	ErrUnavailable         = errors.New("peer session ticket service is unavailable")
	idempotencyPattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
)

type IdempotencyStore interface {
	Reserve(context.Context, string, string, string, string, remoteauth.Claims, time.Duration) (remoteauth.Claims, error)
}

type CredentialReader interface {
	LoadPeerCredentials(context.Context, uuid.UUID, uuid.UUID) (map[uuid.UUID]Credential, error)
}

type RouteResolver interface {
	Resolve(string, time.Time) (relayrouter.Route, error)
}

type EndpointReader interface {
	LoadRelayEndpoint(context.Context, uuid.UUID, uuid.UUID, time.Time) (string, error)
}

type ProjectReader interface {
	ProjectAvailable(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (bool, error)
}

type Credential struct {
	DeviceID            uuid.UUID
	UserID              uuid.UUID
	IdentityPublicKey   ed25519.PublicKey
	PublicKeyThumbprint string
	KeyVersion          uint64
	GrantVersion        uint64
	Scopes              []string
	Capabilities        []string
	Status              string
}

type Config struct {
	Database    *gorm.DB
	Credentials CredentialReader
	// Admission is ignored and retained only for source compatibility. Host
	// authorizes credentials from PostgreSQL before signing the ticket.
	Admission   any
	Routes      RouteResolver
	Endpoints   EndpointReader
	Projects    ProjectReader
	Issuer      remoteauth.Issuer
	Idempotency IdempotencyStore
	TicketTTL   time.Duration
	MaxDuration time.Duration
	MaxBytes    uint64
	Now         func() time.Time
}

type Service struct {
	credentials CredentialReader
	routes      RouteResolver
	endpoints   EndpointReader
	projects    ProjectReader
	issuer      remoteauth.Issuer
	idempotency IdempotencyStore
	ticketTTL   time.Duration
	maxDuration time.Duration
	maxBytes    uint64
	now         func() time.Time
}

type IssueInput struct {
	UserID                      uuid.UUID
	SessionID                   uuid.UUID
	SourceDeviceID              uuid.UUID
	TargetDeviceID              uuid.UUID
	Scope                       string
	ProjectID                   *uuid.UUID
	RequestedMaxDurationSeconds *uint32
	RequestedMaxBytes           *uint64
	IdempotencyKey              string
}

type Result struct {
	SessionID             uuid.UUID `json:"sessionId"`
	PeerSessionTicket     string    `json:"peerSessionTicket"`
	ExpiresAt             time.Time `json:"expiresAt"`
	MaxDurationSeconds    uint32    `json:"maxDurationSeconds"`
	MaxBytes              uint64    `json:"maxBytes"`
	TargetKeyThumbprint   string    `json:"targetKeyThumbprint"`
	TargetIdentityKey     string    `json:"targetIdentityPublicKey"`
	TargetKeyVersion      uint64    `json:"targetKeyVersion"`
	SourceKeyThumbprint   string    `json:"sourceKeyThumbprint"`
	SourceIdentityKey     string    `json:"sourceIdentityPublicKey"`
	SourceKeyVersion      uint64    `json:"sourceKeyVersion"`
	RelayURL              string    `json:"relayUrl"`
	RelayNodeID           uuid.UUID `json:"relayNodeId"`
	RelayCellID           uuid.UUID `json:"relayCellId"`
	TargetConnectionEpoch uint64    `json:"targetConnectionEpoch"`
}

func NewService(config Config) (*Service, error) {
	ticketTTL := config.TicketTTL
	if ticketTTL == 0 {
		ticketTTL = DefaultTicketTTL
	}
	maxDuration := config.MaxDuration
	if maxDuration == 0 {
		maxDuration = DefaultMaxDuration
	}
	maxBytes := config.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxBytes
	}
	credentials := config.Credentials
	if credentials == nil && config.Database != nil {
		credentials = postgresCredentialReader{db: config.Database}
	}
	endpoints := config.Endpoints
	if endpoints == nil && config.Database != nil {
		endpoints = postgresEndpointReader{db: config.Database}
	}
	projects := config.Projects
	if projects == nil && config.Database != nil {
		projects = postgresProjectReader{db: config.Database}
	}
	if credentials == nil || config.Routes == nil || endpoints == nil || projects == nil || config.Idempotency == nil ||
		config.Issuer.Issuer == "" || config.Issuer.KeyID == "" || len(config.Issuer.PrivateKey) != ed25519.PrivateKeySize ||
		ticketTTL < time.Second || ticketTTL > DefaultTicketTTL || maxDuration < time.Second || maxDuration > DefaultMaxDuration ||
		maxBytes == 0 || maxBytes > DefaultMaxBytes {
		return nil, errors.New("Peer Session Ticket service configuration is invalid")
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		credentials: credentials, routes: config.Routes, endpoints: endpoints, projects: projects, issuer: config.Issuer,
		idempotency: config.Idempotency, ticketTTL: ticketTTL, maxDuration: maxDuration,
		maxBytes: maxBytes, now: now,
	}, nil
}

func validPeerScope(scope string) bool {
	switch scope {
	case "remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.terminal.interactive", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.task.control", "remote.peer.ai.tools", "remote.peer.events":
		return true
	default:
		return false
	}
}

func peerScopeRequiresProject(scope string) bool {
	switch scope {
	case "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.terminal.interactive", "remote.peer.file.send", "remote.peer.file.receive", "remote.peer.task.control", "remote.peer.ai.tools", "remote.peer.events":
		return true
	default:
		return false
	}
}

func projectIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

type postgresProjectReader struct{ db *gorm.DB }

func (reader postgresProjectReader) ProjectAvailable(ctx context.Context, userID, deviceID, projectID uuid.UUID) (bool, error) {
	if reader.db == nil || userID == uuid.Nil || deviceID == uuid.Nil || projectID == uuid.Nil {
		return false, ErrUnavailable
	}
	var count int64
	err := reader.db.WithContext(ctx).Table("remote_projects").Where(
		"user_id = ? AND device_id = ? AND project_id = ? AND state = 'available'", userID, deviceID, projectID,
	).Count(&count).Error
	return count == 1, err
}

func (service *Service) Issue(ctx context.Context, input IssueInput) (Result, error) {
	input.Scope = strings.TrimSpace(input.Scope)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.SourceDeviceID == uuid.Nil ||
		input.TargetDeviceID == uuid.Nil || input.SourceDeviceID == input.TargetDeviceID ||
		!idempotencyPattern.MatchString(input.IdempotencyKey) ||
		!validPeerScope(input.Scope) ||
		(input.ProjectID != nil && *input.ProjectID == uuid.Nil) ||
		(peerScopeRequiresProject(input.Scope) && input.ProjectID == nil) ||
		(!peerScopeRequiresProject(input.Scope) && input.ProjectID != nil) {
		return Result{}, ErrInvalidRequest
	}
	maxDuration := service.maxDuration
	if input.RequestedMaxDurationSeconds != nil {
		requested := time.Duration(*input.RequestedMaxDurationSeconds) * time.Second
		if requested < time.Second || requested > DefaultMaxDuration {
			return Result{}, ErrInvalidRequest
		}
		if requested < maxDuration {
			maxDuration = requested
		}
	}
	maxBytes := service.maxBytes
	if input.RequestedMaxBytes != nil {
		if *input.RequestedMaxBytes == 0 || *input.RequestedMaxBytes > DefaultMaxBytes {
			return Result{}, ErrInvalidRequest
		}
		if *input.RequestedMaxBytes < maxBytes {
			maxBytes = *input.RequestedMaxBytes
		}
	}

	credentials, err := service.credentials.LoadPeerCredentials(ctx, input.SourceDeviceID, input.TargetDeviceID)
	if err != nil {
		return Result{}, err
	}
	source, sourceOK := credentials[input.SourceDeviceID]
	target, targetOK := credentials[input.TargetDeviceID]
	if !sourceOK {
		return Result{}, ErrSourceForbidden
	}
	if !targetOK {
		return Result{}, ErrTargetNotFound
	}
	if source.UserID != input.UserID {
		return Result{}, ErrSourceForbidden
	}
	// The MVP authorization boundary is deliberately narrow: both active
	// devices must belong to the authenticated account. Cross-account/project
	// sharing needs a separate grant model and must not be inferred here.
	if target.UserID != input.UserID {
		return Result{}, ErrTargetForbidden
	}
	if input.ProjectID != nil {
		available, err := service.projects.ProjectAvailable(ctx, input.UserID, target.DeviceID, *input.ProjectID)
		if err != nil {
			return Result{}, fmt.Errorf("%w: verify target project", ErrUnavailable)
		}
		if !available {
			return Result{}, ErrTargetForbidden
		}
	}
	if source.Status != "active" || target.Status != "active" {
		return Result{}, ErrDeviceInactive
	}
	now := service.now().UTC()
	route, err := service.routes.Resolve(target.DeviceID.String(), now)
	if err != nil {
		if errors.Is(err, relayrouter.ErrRouteNotFound) {
			return Result{}, ErrTargetOffline
		}
		return Result{}, fmt.Errorf("%w: resolve target route", ErrRelayUnavailable)
	}
	relayNodeID, nodeErr := uuid.Parse(route.NodeID)
	relayCellID, cellErr := uuid.Parse(route.CellID)
	if nodeErr != nil || cellErr != nil || route.DeviceID != target.DeviceID.String() || route.UserID != input.UserID.String() ||
		route.GrantVersion != target.GrantVersion || route.ConnectionEpoch == 0 {
		return Result{}, ErrRelayUnavailable
	}
	relayURL, err := service.endpoints.LoadRelayEndpoint(ctx, relayNodeID, relayCellID, now)
	if err != nil {
		return Result{}, ErrRelayUnavailable
	}
	relayURL, err = validateRelayEndpoint(relayURL)
	if err != nil {
		return Result{}, ErrRelayUnavailable
	}
	claims := remoteauth.Claims{
		Audience: "relay-peer", Subject: source.DeviceID.String(), UserID: input.UserID.String(),
		SessionID: uuid.NewString(), SourceDeviceID: source.DeviceID.String(), TargetDeviceID: target.DeviceID.String(),
		SourceGrantVersion: source.GrantVersion, TargetGrantVersion: target.GrantVersion,
		SourceKeyThumbprint: source.PublicKeyThumbprint, TargetKeyThumbprint: target.PublicKeyThumbprint,
		SourceIdentityKey: base64.RawURLEncoding.EncodeToString(source.IdentityPublicKey),
		TargetIdentityKey: base64.RawURLEncoding.EncodeToString(target.IdentityPublicKey),
		SourceKeyVersion:  source.KeyVersion, TargetKeyVersion: target.KeyVersion, SourceCredentialType: "device",
		Confirmation: source.PublicKeyThumbprint, RelayNodeID: relayNodeID.String(), RelayCellID: relayCellID.String(),
		TargetConnectionEpoch: route.ConnectionEpoch,
		Scopes:                []string{input.Scope}, MaxDurationSeconds: uint32(maxDuration / time.Second), MaxBytes: maxBytes,
		JWTID: uuid.NewString(), IssuedAt: now.Unix(), NotBefore: now.Add(-time.Second).Unix(),
		ExpiresAt: now.Add(service.ticketTTL).Unix(),
	}
	if input.ProjectID != nil {
		claims.ProjectID = input.ProjectID.String()
	}
	requestHash := issueRequestHash(input, maxDuration, maxBytes)
	claims, err = service.idempotency.Reserve(ctx, input.UserID.String(), input.SourceDeviceID.String(), input.IdempotencyKey, requestHash, claims, service.ticketTTL)
	if err != nil {
		return Result{}, err
	}
	if claims.SourceDeviceID != source.DeviceID.String() || claims.TargetDeviceID != target.DeviceID.String() ||
		!claims.HasScope(input.Scope) || claims.SourceGrantVersion != source.GrantVersion ||
		claims.TargetGrantVersion != target.GrantVersion || claims.SourceKeyThumbprint != source.PublicKeyThumbprint ||
		claims.TargetKeyThumbprint != target.PublicKeyThumbprint || claims.SourceKeyVersion != source.KeyVersion ||
		claims.TargetKeyVersion != target.KeyVersion || claims.SourceCredentialType != "device" || claims.SourceIdentityKey != base64.RawURLEncoding.EncodeToString(source.IdentityPublicKey) ||
		claims.TargetIdentityKey != base64.RawURLEncoding.EncodeToString(target.IdentityPublicKey) || claims.RelayNodeID != relayNodeID.String() ||
		claims.RelayCellID != relayCellID.String() || claims.TargetConnectionEpoch != route.ConnectionEpoch || claims.ProjectID != projectIDString(input.ProjectID) || claims.ExpiresAt <= now.Unix() {
		return Result{}, ErrIdempotencyConflict
	}
	ticket, err := service.issuer.Sign(claims)
	if err != nil {
		return Result{}, fmt.Errorf("%w: sign Peer Session Ticket", ErrUnavailable)
	}
	sessionID, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return Result{}, ErrUnavailable
	}
	return Result{
		SessionID: sessionID, PeerSessionTicket: ticket, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
		MaxDurationSeconds: claims.MaxDurationSeconds, MaxBytes: claims.MaxBytes,
		TargetKeyThumbprint: claims.TargetKeyThumbprint, TargetIdentityKey: claims.TargetIdentityKey, TargetKeyVersion: claims.TargetKeyVersion,
		SourceKeyThumbprint: claims.SourceKeyThumbprint, SourceIdentityKey: claims.SourceIdentityKey, SourceKeyVersion: claims.SourceKeyVersion,
		RelayURL:    relayURL,
		RelayNodeID: relayNodeID, RelayCellID: relayCellID, TargetConnectionEpoch: route.ConnectionEpoch,
	}, nil
}

type postgresEndpointReader struct {
	db *gorm.DB
}

func (reader postgresEndpointReader) LoadRelayEndpoint(ctx context.Context, nodeID, cellID uuid.UUID, now time.Time) (string, error) {
	if reader.db == nil || nodeID == uuid.Nil || cellID == uuid.Nil || now.IsZero() {
		return "", ErrRelayUnavailable
	}
	var row struct {
		Addresses json.RawMessage `gorm:"column:addresses"`
	}
	err := reader.db.WithContext(ctx).Raw(`
		SELECT instance.addresses
		FROM relay_node_instances instance
		JOIN relay_node_installations installation
		  ON installation.id = instance.installation_id
		 AND installation.current_instance_id = instance.id
		WHERE instance.id = ? AND instance.cell_id = ?
		  AND instance.status = 'ready' AND instance.lease_expires_at > ?
		  AND installation.status = 'active'`, nodeID, cellID, now.UTC()).Take(&row).Error
	if err != nil {
		return "", ErrRelayUnavailable
	}
	var addresses []string
	if json.Unmarshal(row.Addresses, &addresses) != nil || len(addresses) == 0 || len(addresses) > 16 {
		return "", ErrRelayUnavailable
	}
	for _, address := range addresses {
		if endpoint, endpointErr := validateRelayEndpoint(address); endpointErr == nil {
			return endpoint, nil
		}
	}
	return "", ErrRelayUnavailable
}

func validateRelayEndpoint(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "/v1/connect" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrRelayUnavailable
	}
	return parsed.String(), nil
}

type postgresCredentialReader struct {
	db *gorm.DB
}

func (reader postgresCredentialReader) LoadPeerCredentials(ctx context.Context, sourceID, targetID uuid.UUID) (map[uuid.UUID]Credential, error) {
	var rows []credentialRow
	if err := reader.db.WithContext(ctx).Table("remote_device_credentials").
		Select("device_id, user_id, identity_public_key, public_key_thumbprint, key_version, grant_version, scopes, capabilities, status").
		Where("device_id IN ?", []uuid.UUID{sourceID, targetID}).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("%w: load Peer devices", ErrUnavailable)
	}
	result := make(map[uuid.UUID]Credential, len(rows))
	for _, row := range rows {
		var scopes, capabilities []string
		if json.Unmarshal(row.Scopes, &scopes) != nil || json.Unmarshal(row.Capabilities, &capabilities) != nil ||
			row.DeviceID == uuid.Nil || row.UserID == uuid.Nil || len(row.IdentityPublicKey) != ed25519.PublicKeySize ||
			row.KeyVersion < 1 || row.GrantVersion < 1 || len(row.PublicKeyThumbprint) != 43 ||
			remoteauth.PublicKeyThumbprint(ed25519.PublicKey(row.IdentityPublicKey)) != row.PublicKeyThumbprint {
			return nil, ErrUnavailable
		}
		result[row.DeviceID] = Credential{
			DeviceID: row.DeviceID, UserID: row.UserID, IdentityPublicKey: ed25519.PublicKey(append([]byte(nil), row.IdentityPublicKey...)),
			PublicKeyThumbprint: row.PublicKeyThumbprint, KeyVersion: uint64(row.KeyVersion),
			GrantVersion: uint64(row.GrantVersion), Scopes: scopes, Capabilities: capabilities, Status: row.Status,
		}
	}
	return result, nil
}

func issueRequestHash(input IssueInput, duration time.Duration, maxBytes uint64) string {
	projectID := ""
	if input.ProjectID != nil {
		projectID = input.ProjectID.String()
	}
	payload, _ := json.Marshal(struct {
		TargetID           string `json:"targetDeviceId"`
		Scope              string `json:"scope"`
		ProjectID          string `json:"projectId,omitempty"`
		MaxDurationSeconds uint32 `json:"maxDurationSeconds"`
		MaxBytes           uint64 `json:"maxBytes"`
	}{input.TargetDeviceID.String(), input.Scope, projectID, uint32(duration / time.Second), maxBytes})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

type credentialRow struct {
	DeviceID            uuid.UUID `gorm:"column:device_id"`
	UserID              uuid.UUID `gorm:"column:user_id"`
	IdentityPublicKey   []byte    `gorm:"column:identity_public_key"`
	PublicKeyThumbprint string    `gorm:"column:public_key_thumbprint"`
	KeyVersion          int64     `gorm:"column:key_version"`
	GrantVersion        int64     `gorm:"column:grant_version"`
	Scopes              []byte    `gorm:"column:scopes"`
	Capabilities        []byte    `gorm:"column:capabilities"`
	Status              string    `gorm:"column:status"`
}

type redisRecord struct {
	RequestHash string            `json:"requestHash"`
	Claims      remoteauth.Claims `json:"claims"`
}

type RedisIdempotencyStore struct {
	client  redis.UniversalClient
	prefix  string
	timeout time.Duration
}

func NewRedisIdempotencyStoreFromURL(rawURL string) (*RedisIdempotencyStore, error) {
	options, err := redis.ParseURL(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, errors.New("Peer Session Redis URL is invalid")
	}
	options.DialTimeout = 2 * time.Second
	options.ReadTimeout = 2 * time.Second
	options.WriteTimeout = 2 * time.Second
	options.MaxRetries = 0
	return &RedisIdempotencyStore{client: redis.NewClient(options), prefix: "relay:v1:peer-ticket-request", timeout: 2 * time.Second}, nil
}

func (store *RedisIdempotencyStore) Close() error {
	if store == nil || store.client == nil {
		return nil
	}
	return store.client.Close()
}

func (store *RedisIdempotencyStore) Ping(ctx context.Context) error {
	if store == nil || store.client == nil {
		return ErrUnavailable
	}
	requestContext, cancel := context.WithTimeout(ctx, store.timeout)
	defer cancel()
	if err := store.client.Ping(requestContext).Err(); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (store *RedisIdempotencyStore) Reserve(ctx context.Context, userID, deviceID, key, requestHash string, proposed remoteauth.Claims, ttl time.Duration) (remoteauth.Claims, error) {
	if store == nil || store.client == nil || userID == "" || deviceID == "" || !idempotencyPattern.MatchString(key) ||
		len(requestHash) != 64 || proposed.SessionID == "" || proposed.JWTID == "" || ttl < time.Second || ttl > DefaultTicketTTL {
		return remoteauth.Claims{}, ErrInvalidRequest
	}
	record := redisRecord{RequestHash: requestHash, Claims: proposed}
	payload, err := json.Marshal(record)
	if err != nil || len(payload) > 16<<10 {
		return remoteauth.Claims{}, ErrUnavailable
	}
	digest := sha256.Sum256([]byte(userID + "\x00" + deviceID + "\x00" + key))
	redisKey := store.prefix + ":" + hex.EncodeToString(digest[:])
	requestContext, cancel := context.WithTimeout(ctx, store.timeout)
	defer cancel()
	created, err := store.client.SetNX(requestContext, redisKey, payload, ttl).Result()
	if err != nil {
		return remoteauth.Claims{}, ErrUnavailable
	}
	if created {
		return proposed, nil
	}
	existingPayload, err := store.client.Get(requestContext, redisKey).Bytes()
	if err != nil || len(existingPayload) == 0 || len(existingPayload) > 16<<10 {
		return remoteauth.Claims{}, ErrUnavailable
	}
	var existing redisRecord
	if err := json.Unmarshal(existingPayload, &existing); err != nil || existing.RequestHash != requestHash {
		return remoteauth.Claims{}, ErrIdempotencyConflict
	}
	return existing.Claims, nil
}
