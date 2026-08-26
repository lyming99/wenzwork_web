package relayrouter

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisSchemaVersion     = "1"
	defaultRegistryTimeout = 750 * time.Millisecond
)

var redisIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type RedisRegistry struct {
	client   redis.UniversalClient
	prefix   string
	fenceTTL time.Duration
	timeout  time.Duration
}

func NewRedisRegistry(client redis.UniversalClient, prefix string, fenceTTL time.Duration) (*RedisRegistry, error) {
	prefix = strings.Trim(strings.TrimSpace(prefix), ":")
	if client == nil || prefix == "" || len(prefix) > 80 || strings.ContainsAny(prefix, "{} \t\r\n") || fenceTTL < time.Minute || fenceTTL > 24*time.Hour {
		return nil, errors.New("Redis Relay Registry configuration is invalid")
	}
	return &RedisRegistry{client: client, prefix: prefix, fenceTTL: fenceTTL, timeout: defaultRegistryTimeout}, nil
}

func NewRedisRegistryFromURL(rawURL string) (*RedisRegistry, error) {
	options, err := redis.ParseURL(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, errors.New("Relay Redis URL is invalid")
	}
	options.DialTimeout = defaultRegistryTimeout
	options.ReadTimeout = defaultRegistryTimeout
	options.WriteTimeout = defaultRegistryTimeout
	options.MaxRetries = 0
	return NewRedisRegistry(redis.NewClient(options), "relay:v1", 2*time.Hour)
}

func (registry *RedisRegistry) Close() error {
	if registry == nil || registry.client == nil {
		return nil
	}
	return registry.client.Close()
}

// PutAssignmentFence writes the user assignment snapshot next to one device's
// route key. The shared Redis hash tag lets all admission checks remain one
// atomic Lua operation even on Redis Cluster.
func (registry *RedisRegistry) PutAssignmentFence(userID, deviceID string, fence AssignmentFence) error {
	if !validRedisIdentifier(userID) || !validRedisIdentifier(deviceID) || fence.Version == 0 || len(fence.AllowedCellIDs) == 0 || len(fence.AllowedCellIDs) > 32 {
		return ErrAssignmentStale
	}
	for _, cellID := range fence.AllowedCellIDs {
		if !validRedisIdentifier(cellID) {
			return ErrAssignmentStale
		}
	}
	allowedCells, err := json.Marshal(fence.AllowedCellIDs)
	if err != nil {
		return ErrAssignmentStale
	}
	code, err := registry.evalCode(putAssignmentFenceScript, []string{registry.assignmentKey(deviceID)},
		redisSchemaVersion, userID, strconv.FormatUint(fence.Version, 10), string(allowedCells), registry.fenceTTL.Milliseconds())
	if err != nil {
		return err
	}
	if code != "OK" {
		return ErrAssignmentStale
	}
	return nil
}

func (registry *RedisRegistry) PutGrantFence(deviceID string, fence GrantFence) error {
	if !validRedisIdentifier(deviceID) || fence.Version == 0 || (fence.Status != DeviceActive && fence.Status != DeviceRevoked && fence.Status != DeviceQuarantined) {
		return ErrGrantStale
	}
	code, err := registry.evalCode(putGrantFenceScript, []string{registry.grantKey(deviceID)},
		redisSchemaVersion, strconv.FormatUint(fence.Version, 10), string(fence.Status), registry.fenceTTL.Milliseconds())
	if err != nil {
		return err
	}
	if code != "OK" {
		return ErrGrantStale
	}
	return nil
}

func (registry *RedisRegistry) PutDeviceCredential(deviceID string, credential DeviceCredential) error {
	if !validRedisIdentifier(deviceID) || credential.Version == 0 || len(credential.PublicKey) != ed25519.PublicKeySize ||
		(credential.Status != DeviceActive && credential.Status != DeviceRevoked && credential.Status != DeviceQuarantined) {
		return ErrGrantStale
	}
	encodedKey := base64.RawURLEncoding.EncodeToString(credential.PublicKey)
	digest := sha256.Sum256(credential.PublicKey)
	thumbprint := base64.RawURLEncoding.EncodeToString(digest[:])
	code, err := registry.evalCode(putDeviceCredentialScript, []string{registry.credentialKey(deviceID)},
		redisSchemaVersion, strconv.FormatUint(credential.Version, 10), string(credential.Status), encodedKey, thumbprint, registry.fenceTTL.Milliseconds())
	if err != nil || code != "OK" {
		return ErrGrantStale
	}
	return nil
}

func (registry *RedisRegistry) ResolveDeviceKey(ctx context.Context, deviceID, expectedThumbprint string) (ed25519.PublicKey, error) {
	if !validRedisIdentifier(deviceID) || expectedThumbprint == "" || len(expectedThumbprint) > 128 {
		return nil, ErrGrantStale
	}
	lookupContext, cancel := context.WithTimeout(ctx, registry.timeout)
	defer cancel()
	values, err := registry.client.HGetAll(lookupContext, registry.credentialKey(deviceID)).Result()
	if err != nil || values["schema_version"] != redisSchemaVersion {
		return nil, ErrFenceUnavailable
	}
	if values["status"] != string(DeviceActive) || values["thumbprint"] != expectedThumbprint {
		return nil, ErrGrantStale
	}
	raw, err := base64.RawURLEncoding.DecodeString(values["public_key"])
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, ErrFenceUnavailable
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyPeerDeviceState binds a Peer Session Ticket to the current projected
// Grant version and identity key. It intentionally reads only Redis projection
// metadata; no Peer payload or ticket plaintext is stored here.
func (registry *RedisRegistry) VerifyPeerDeviceState(ctx context.Context, deviceID string, grantVersion uint64, expectedThumbprint string) (ed25519.PublicKey, error) {
	if !validRedisIdentifier(deviceID) || grantVersion == 0 || expectedThumbprint == "" || len(expectedThumbprint) > 128 {
		return nil, ErrGrantStale
	}
	lookupContext, cancel := context.WithTimeout(ctx, registry.timeout)
	defer cancel()
	values, err := registry.client.HGetAll(lookupContext, registry.credentialKey(deviceID)).Result()
	if err != nil || values["schema_version"] != redisSchemaVersion {
		return nil, ErrFenceUnavailable
	}
	projectedVersion, err := strconv.ParseUint(values["version"], 10, 64)
	if err != nil {
		return nil, ErrFenceUnavailable
	}
	if projectedVersion != grantVersion || values["status"] != string(DeviceActive) || values["thumbprint"] != expectedThumbprint {
		return nil, ErrGrantStale
	}
	raw, err := base64.RawURLEncoding.DecodeString(values["public_key"])
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, ErrFenceUnavailable
	}
	return ed25519.PublicKey(raw), nil
}

// ConsumePeerTicket performs the global one-time jti fence required before a
// PEER_OPEN is routed. Only a digest-derived Redis key and a constant marker are
// retained until the ticket expires.
func (registry *RedisRegistry) ConsumePeerTicket(ctx context.Context, jwtID string, expiresAt, now time.Time) error {
	return registry.consumeSessionTicket(ctx, "peer-ticket-jti", jwtID, expiresAt, now)
}

// ConsumeFileTicket keeps File and Peer replay fences in separate namespaces.
// Only a digest of the jti and a constant marker are retained.
func (registry *RedisRegistry) ConsumeFileTicket(ctx context.Context, jwtID string, expiresAt, now time.Time) error {
	return registry.consumeSessionTicket(ctx, "file-ticket-jti", jwtID, expiresAt, now)
}

const maxSessionTicketReplayFenceLifetime = 30 * 24 * time.Hour

func (registry *RedisRegistry) consumeSessionTicket(ctx context.Context, namespace, jwtID string, expiresAt, now time.Time) error {
	jwtID = strings.TrimSpace(jwtID)
	if (namespace != "peer-ticket-jti" && namespace != "file-ticket-jti") || !validRedisIdentifier(jwtID) ||
		expiresAt.IsZero() || now.IsZero() || !expiresAt.After(now) || expiresAt.After(now.Add(maxSessionTicketReplayFenceLifetime)) {
		return ErrPeerTicketReplay
	}
	digest := sha256.Sum256([]byte(jwtID))
	key := registry.prefix + ":" + namespace + ":" + base64.RawURLEncoding.EncodeToString(digest[:])
	consumeContext, cancel := context.WithTimeout(ctx, registry.timeout)
	defer cancel()
	created, err := registry.client.SetNX(consumeContext, key, "used", expiresAt.Sub(now)).Result()
	if err != nil {
		return ErrFenceUnavailable
	}
	if !created {
		return ErrPeerTicketReplay
	}
	return nil
}

// VerifyAdmissionState prevents the control plane from issuing a Ticket until
// all three Redis admission records match the PostgreSQL facts just committed.
func (registry *RedisRegistry) VerifyAdmissionState(ctx context.Context, userID, deviceID string, assignmentVersion, grantVersion uint64, allowedCellIDs []string, thumbprint string) error {
	if !validRedisIdentifier(userID) || !validRedisIdentifier(deviceID) || assignmentVersion == 0 || grantVersion == 0 ||
		len(allowedCellIDs) == 0 || thumbprint == "" {
		return ErrFenceUnavailable
	}
	for _, cellID := range allowedCellIDs {
		if !validRedisIdentifier(cellID) {
			return ErrFenceUnavailable
		}
	}
	lookupContext, cancel := context.WithTimeout(ctx, registry.timeout)
	defer cancel()
	pipe := registry.client.Pipeline()
	assignmentCommand := pipe.HGetAll(lookupContext, registry.assignmentKey(deviceID))
	grantCommand := pipe.HGetAll(lookupContext, registry.grantKey(deviceID))
	credentialCommand := pipe.HGetAll(lookupContext, registry.credentialKey(deviceID))
	if _, err := pipe.Exec(lookupContext); err != nil {
		return ErrFenceUnavailable
	}
	assignment, grant, credential := assignmentCommand.Val(), grantCommand.Val(), credentialCommand.Val()
	if assignment["schema_version"] != redisSchemaVersion || grant["schema_version"] != redisSchemaVersion || credential["schema_version"] != redisSchemaVersion {
		return ErrFenceUnavailable
	}
	assignmentValue, assignmentErr := strconv.ParseUint(assignment["version"], 10, 64)
	var projectedCells []string
	cellsErr := json.Unmarshal([]byte(assignment["allowed_cells"]), &projectedCells)
	if assignmentErr != nil || cellsErr != nil || assignment["user_id"] != userID || assignmentValue != assignmentVersion {
		return ErrAssignmentStale
	}
	expectedCells := slices.Clone(allowedCellIDs)
	slices.Sort(expectedCells)
	slices.Sort(projectedCells)
	if !slices.Equal(expectedCells, projectedCells) {
		return ErrAssignmentStale
	}
	grantValue, grantErr := strconv.ParseUint(grant["version"], 10, 64)
	credentialValue, credentialErr := strconv.ParseUint(credential["version"], 10, 64)
	if grantErr != nil || credentialErr != nil || grantValue != grantVersion || credentialValue != grantVersion ||
		grant["status"] != string(DeviceActive) || credential["status"] != string(DeviceActive) || credential["thumbprint"] != thumbprint {
		return ErrGrantStale
	}
	return nil
}

func (registry *RedisRegistry) Register(route Route, ttl time.Duration, now time.Time) error {
	if err := validateRedisRoute(route, ttl); err != nil {
		return err
	}
	code, err := registry.evalCode(registerRouteScript, registry.routeKeys(route.DeviceID),
		redisSchemaVersion, route.UserID, route.CellID, route.NodeID, route.ConnectionID,
		strconv.FormatUint(route.ConnectionEpoch, 10), strconv.FormatUint(route.AssignmentVersion, 10),
		strconv.FormatUint(route.GrantVersion, 10), strconv.FormatUint(uint64(route.ProtocolVersion), 10),
		now.UTC().UnixMilli(), ttl.Milliseconds())
	return routeCodeError(code, err)
}

func (registry *RedisRegistry) Renew(deviceID, connectionID string, epoch uint64, ttl time.Duration, now time.Time) error {
	if !validRedisIdentifier(deviceID) || !validRedisIdentifier(connectionID) || epoch == 0 || ttl <= 0 || ttl > 10*time.Minute {
		return ErrConnectionStale
	}
	code, err := registry.evalCode(renewRouteScript, registry.routeKeys(deviceID),
		redisSchemaVersion, connectionID, strconv.FormatUint(epoch, 10), now.UTC().UnixMilli(), ttl.Milliseconds())
	return routeCodeError(code, err)
}

func (registry *RedisRegistry) Resolve(deviceID string, now time.Time) (Route, error) {
	return registry.ResolveContext(context.Background(), deviceID, now)
}

// ResolveContext is used by first-screen ticket issuance so Redis cannot keep
// a request alive after its caller has already given up.  Other callers may
// retain the compatibility Resolve method above.
func (registry *RedisRegistry) ResolveContext(ctx context.Context, deviceID string, now time.Time) (Route, error) {
	if !validRedisIdentifier(deviceID) {
		return Route{}, ErrRouteNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, registry.timeout)
	defer cancel()
	value, err := registry.client.Eval(ctx, resolveRouteScript, registry.routeKeys(deviceID), redisSchemaVersion, now.UTC().UnixMilli()).Result()
	if err != nil {
		return Route{}, ErrFenceUnavailable
	}
	values, ok := value.([]interface{})
	if !ok || len(values) == 0 {
		return Route{}, ErrFenceUnavailable
	}
	code := redisString(values[0])
	if code != "OK" {
		return Route{}, routeCodeError(code, nil)
	}
	return routeFromRedisFields(deviceID, now, values[1:])
}

// ResolveVerifiedPeerRoute resolves a target's identity key and current
// Relay route in one atomic Redis operation.  All keys share the device hash
// tag, so this is valid on Redis Cluster and cannot observe a credential and
// route from two different projection moments.
func (registry *RedisRegistry) ResolveVerifiedPeerRoute(ctx context.Context, deviceID string, grantVersion uint64, expectedThumbprint string, now time.Time) (ed25519.PublicKey, Route, error) {
	if !validRedisIdentifier(deviceID) || grantVersion == 0 || expectedThumbprint == "" || len(expectedThumbprint) > 128 {
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerCredentialValidation, ErrGrantStale)
	}
	lookupContext, cancel := context.WithTimeout(ctx, registry.timeout)
	defer cancel()
	keys := append(registry.routeKeys(deviceID), registry.credentialKey(deviceID))
	value, err := registry.client.Eval(
		lookupContext,
		resolveVerifiedPeerRouteScript,
		keys,
		redisSchemaVersion,
		strconv.FormatUint(grantVersion, 10),
		expectedThumbprint,
	).Result()
	if err != nil {
		// The legacy sequence checked this credential first, so a transport
		// failure remains fail-closed as a credential-validation failure.
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerCredentialValidation, ErrFenceUnavailable)
	}
	values, ok := value.([]interface{})
	if !ok || len(values) == 0 {
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerCredentialValidation, ErrFenceUnavailable)
	}
	code := redisString(values[0])
	switch code {
	case "CREDENTIAL_STALE":
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerCredentialValidation, ErrGrantStale)
	case "CREDENTIAL_UNAVAILABLE":
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerCredentialValidation, ErrFenceUnavailable)
	case "ROUTE_NOT_FOUND":
		return nil, Route{}, ErrRouteNotFound
	case "ROUTE_FENCE_UNAVAILABLE":
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerRouteValidation, ErrFenceUnavailable)
	case "ROUTE_ASSIGNMENT_STALE":
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerRouteValidation, ErrAssignmentStale)
	case "ROUTE_GRANT_STALE":
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerRouteValidation, ErrGrantStale)
	case "OK":
		// Continue below.
	default:
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerRouteValidation, ErrFenceUnavailable)
	}
	if len(values) != 13 {
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerCredentialValidation, ErrFenceUnavailable)
	}
	encodedKey := redisString(values[1])
	raw, decodeErr := base64.RawURLEncoding.DecodeString(encodedKey)
	if decodeErr != nil || len(raw) != ed25519.PublicKeySize {
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerCredentialValidation, ErrFenceUnavailable)
	}
	route, routeErr := routeFromRedisFields(deviceID, now, values[2:])
	if routeErr != nil {
		return nil, Route{}, fmt.Errorf("%w: %w", ErrPeerRouteValidation, routeErr)
	}
	return ed25519.PublicKey(raw), route, nil
}

func routeFromRedisFields(deviceID string, now time.Time, fields []interface{}) (Route, error) {
	if len(fields) != 11 {
		return Route{}, ErrFenceUnavailable
	}
	epoch, epochErr := strconv.ParseUint(redisString(fields[4]), 10, 64)
	assignmentVersion, assignmentErr := strconv.ParseUint(redisString(fields[5]), 10, 64)
	grantVersion, grantErr := strconv.ParseUint(redisString(fields[6]), 10, 64)
	protocolVersion, protocolErr := strconv.ParseUint(redisString(fields[7]), 10, 32)
	connectedMillis, connectedErr := strconv.ParseInt(redisString(fields[8]), 10, 64)
	heartbeatMillis, heartbeatErr := strconv.ParseInt(redisString(fields[9]), 10, 64)
	ttlMillis, ttlErr := strconv.ParseInt(redisString(fields[10]), 10, 64)
	if errors.Join(epochErr, assignmentErr, grantErr, protocolErr, connectedErr, heartbeatErr, ttlErr) != nil || ttlMillis <= 0 {
		return Route{}, ErrFenceUnavailable
	}
	return Route{
		DeviceID: deviceID, UserID: redisString(fields[0]), CellID: redisString(fields[1]),
		NodeID: redisString(fields[2]), ConnectionID: redisString(fields[3]), ConnectionEpoch: epoch,
		AssignmentVersion: assignmentVersion, GrantVersion: grantVersion, ProtocolVersion: uint32(protocolVersion),
		ConnectedAt: time.UnixMilli(connectedMillis).UTC(), LastHeartbeatAt: time.UnixMilli(heartbeatMillis).UTC(),
		ExpiresAt: now.UTC().Add(time.Duration(ttlMillis) * time.Millisecond),
	}, nil
}

func (registry *RedisRegistry) CompareAndDelete(deviceID, connectionID string, epoch uint64) bool {
	if !validRedisIdentifier(deviceID) || !validRedisIdentifier(connectionID) || epoch == 0 {
		return false
	}
	code, err := registry.evalCode(deleteRouteScript, []string{registry.routeKey(deviceID)}, connectionID, strconv.FormatUint(epoch, 10))
	return err == nil && code == "DELETED"
}

func (registry *RedisRegistry) Ping(ctx context.Context) error {
	if err := registry.client.Ping(ctx).Err(); err != nil {
		return ErrFenceUnavailable
	}
	return nil
}

func (registry *RedisRegistry) routeKeys(deviceID string) []string {
	return []string{registry.routeKey(deviceID), registry.assignmentKey(deviceID), registry.grantKey(deviceID)}
}

func (registry *RedisRegistry) routeKey(deviceID string) string {
	return registry.devicePrefix(deviceID) + ":route"
}

func (registry *RedisRegistry) assignmentKey(deviceID string) string {
	return registry.devicePrefix(deviceID) + ":assignment-fence"
}

func (registry *RedisRegistry) grantKey(deviceID string) string {
	return registry.devicePrefix(deviceID) + ":grant-fence"
}

func (registry *RedisRegistry) credentialKey(deviceID string) string {
	return registry.devicePrefix(deviceID) + ":credential"
}

func (registry *RedisRegistry) devicePrefix(deviceID string) string {
	return registry.prefix + ":{device:" + deviceID + "}"
}

func (registry *RedisRegistry) evalCode(script string, keys []string, arguments ...any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), registry.timeout)
	defer cancel()
	value, err := registry.client.Eval(ctx, script, keys, arguments...).Result()
	if err != nil {
		return "", ErrFenceUnavailable
	}
	return redisString(value), nil
}

func redisString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(value)
	}
}

func validRedisIdentifier(value string) bool {
	return redisIdentifierPattern.MatchString(value)
}

func validateRedisRoute(route Route, ttl time.Duration) error {
	if !validRedisIdentifier(route.DeviceID) || !validRedisIdentifier(route.UserID) || !validRedisIdentifier(route.CellID) ||
		!validRedisIdentifier(route.NodeID) || !validRedisIdentifier(route.ConnectionID) || route.ConnectionEpoch == 0 ||
		route.AssignmentVersion == 0 || route.GrantVersion == 0 || route.ProtocolVersion == 0 || ttl <= 0 || ttl > 10*time.Minute {
		return ErrConnectionStale
	}
	return nil
}

func routeCodeError(code string, err error) error {
	if err != nil {
		return ErrFenceUnavailable
	}
	switch code {
	case "OK":
		return nil
	case "ASSIGNMENT_STALE":
		return ErrAssignmentStale
	case "GRANT_STALE":
		return ErrGrantStale
	case "CONNECTION_STALE":
		return ErrConnectionStale
	case "ROUTE_NOT_FOUND":
		return ErrRouteNotFound
	default:
		return ErrFenceUnavailable
	}
}

const luaDecimalCompare = `
local function normalize(value)
  value = tostring(value or '')
  value = string.gsub(value, '^0+', '')
  if value == '' then return '0' end
  return value
end
local function decimal_compare(left, right)
  left = normalize(left)
  right = normalize(right)
  if string.len(left) < string.len(right) then return -1 end
  if string.len(left) > string.len(right) then return 1 end
  if left < right then return -1 end
  if left > right then return 1 end
  return 0
end
`

const putAssignmentFenceScript = luaDecimalCompare + `
local current_version = redis.call('HGET', KEYS[1], 'version')
if current_version then
  local comparison = decimal_compare(current_version, ARGV[3])
  if comparison > 0 then return 'STALE' end
  if comparison == 0 then
    if redis.call('HGET', KEYS[1], 'user_id') ~= ARGV[2] or redis.call('HGET', KEYS[1], 'allowed_cells') ~= ARGV[4] then
      return 'CONFLICT'
    end
  end
end
redis.call('HSET', KEYS[1], 'schema_version', ARGV[1], 'user_id', ARGV[2], 'version', ARGV[3], 'allowed_cells', ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 'OK'
`

const putGrantFenceScript = luaDecimalCompare + `
local current_version = redis.call('HGET', KEYS[1], 'version')
if current_version then
  local comparison = decimal_compare(current_version, ARGV[2])
  if comparison > 0 then return 'STALE' end
  if comparison == 0 and redis.call('HGET', KEYS[1], 'status') ~= ARGV[3] then return 'CONFLICT' end
end
redis.call('HSET', KEYS[1], 'schema_version', ARGV[1], 'version', ARGV[2], 'status', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
return 'OK'
`

const putDeviceCredentialScript = luaDecimalCompare + `
local current_version = redis.call('HGET', KEYS[1], 'version')
if current_version then
  local comparison = decimal_compare(current_version, ARGV[2])
  if comparison > 0 then return 'STALE' end
  if comparison == 0 and (
    redis.call('HGET', KEYS[1], 'status') ~= ARGV[3] or
    redis.call('HGET', KEYS[1], 'public_key') ~= ARGV[4] or
    redis.call('HGET', KEYS[1], 'thumbprint') ~= ARGV[5]
  ) then return 'CONFLICT' end
end
redis.call('HSET', KEYS[1],
  'schema_version', ARGV[1], 'version', ARGV[2], 'status', ARGV[3],
  'public_key', ARGV[4], 'thumbprint', ARGV[5])
redis.call('PEXPIRE', KEYS[1], ARGV[6])
return 'OK'
`

const registerRouteScript = luaDecimalCompare + `
if redis.call('HGET', KEYS[2], 'schema_version') ~= ARGV[1] or redis.call('HGET', KEYS[3], 'schema_version') ~= ARGV[1] then
  return 'FENCE_UNAVAILABLE'
end
if redis.call('HGET', KEYS[2], 'user_id') ~= ARGV[2] or decimal_compare(redis.call('HGET', KEYS[2], 'version'), ARGV[7]) ~= 0 then
  return 'ASSIGNMENT_STALE'
end
local allowed = cjson.decode(redis.call('HGET', KEYS[2], 'allowed_cells'))
local cell_allowed = false
for _, cell in ipairs(allowed) do if cell == ARGV[3] then cell_allowed = true end end
if not cell_allowed then return 'ASSIGNMENT_STALE' end
if decimal_compare(redis.call('HGET', KEYS[3], 'version'), ARGV[8]) ~= 0 or redis.call('HGET', KEYS[3], 'status') ~= 'active' then
  return 'GRANT_STALE'
end
local current_epoch = redis.call('HGET', KEYS[1], 'connection_epoch')
if current_epoch and decimal_compare(ARGV[6], current_epoch) <= 0 then return 'CONNECTION_STALE' end
redis.call('HSET', KEYS[1],
  'schema_version', ARGV[1], 'user_id', ARGV[2], 'cell_id', ARGV[3], 'node_id', ARGV[4],
  'connection_id', ARGV[5], 'connection_epoch', ARGV[6], 'assignment_version', ARGV[7],
  'grant_version', ARGV[8], 'protocol_version', ARGV[9], 'connected_at_ms', ARGV[10], 'heartbeat_at_ms', ARGV[10])
redis.call('PEXPIRE', KEYS[1], ARGV[11])
return 'OK'
`

const renewRouteScript = luaDecimalCompare + `
if redis.call('EXISTS', KEYS[1]) == 0 then return 'ROUTE_NOT_FOUND' end
if redis.call('HGET', KEYS[1], 'connection_id') ~= ARGV[2] or decimal_compare(redis.call('HGET', KEYS[1], 'connection_epoch'), ARGV[3]) ~= 0 then
  return 'CONNECTION_STALE'
end
if redis.call('HGET', KEYS[2], 'schema_version') ~= ARGV[1] or redis.call('HGET', KEYS[3], 'schema_version') ~= ARGV[1] then
  redis.call('DEL', KEYS[1]); return 'FENCE_UNAVAILABLE'
end
if redis.call('HGET', KEYS[2], 'user_id') ~= redis.call('HGET', KEYS[1], 'user_id') or
   decimal_compare(redis.call('HGET', KEYS[2], 'version'), redis.call('HGET', KEYS[1], 'assignment_version')) ~= 0 then
  redis.call('DEL', KEYS[1]); return 'ASSIGNMENT_STALE'
end
local allowed = cjson.decode(redis.call('HGET', KEYS[2], 'allowed_cells'))
local cell_allowed = false
local route_cell = redis.call('HGET', KEYS[1], 'cell_id')
for _, cell in ipairs(allowed) do if cell == route_cell then cell_allowed = true end end
if not cell_allowed then redis.call('DEL', KEYS[1]); return 'ASSIGNMENT_STALE' end
if decimal_compare(redis.call('HGET', KEYS[3], 'version'), redis.call('HGET', KEYS[1], 'grant_version')) ~= 0 or redis.call('HGET', KEYS[3], 'status') ~= 'active' then
  redis.call('DEL', KEYS[1]); return 'GRANT_STALE'
end
redis.call('HSET', KEYS[1], 'heartbeat_at_ms', ARGV[4])
redis.call('PEXPIRE', KEYS[1], ARGV[5])
return 'OK'
`

const resolveRouteScript = luaDecimalCompare + `
if redis.call('EXISTS', KEYS[1]) == 0 then return {'ROUTE_NOT_FOUND'} end
if redis.call('HGET', KEYS[2], 'schema_version') ~= ARGV[1] or redis.call('HGET', KEYS[3], 'schema_version') ~= ARGV[1] then
  redis.call('DEL', KEYS[1]); return {'FENCE_UNAVAILABLE'}
end
if redis.call('HGET', KEYS[2], 'user_id') ~= redis.call('HGET', KEYS[1], 'user_id') or
   decimal_compare(redis.call('HGET', KEYS[2], 'version'), redis.call('HGET', KEYS[1], 'assignment_version')) ~= 0 then
  redis.call('DEL', KEYS[1]); return {'ASSIGNMENT_STALE'}
end
local allowed = cjson.decode(redis.call('HGET', KEYS[2], 'allowed_cells'))
local cell_allowed = false
local route_cell = redis.call('HGET', KEYS[1], 'cell_id')
for _, cell in ipairs(allowed) do if cell == route_cell then cell_allowed = true end end
if not cell_allowed then redis.call('DEL', KEYS[1]); return {'ASSIGNMENT_STALE'} end
if decimal_compare(redis.call('HGET', KEYS[3], 'version'), redis.call('HGET', KEYS[1], 'grant_version')) ~= 0 or redis.call('HGET', KEYS[3], 'status') ~= 'active' then
  redis.call('DEL', KEYS[1]); return {'GRANT_STALE'}
end
return {'OK', redis.call('HGET', KEYS[1], 'user_id'), route_cell,
  redis.call('HGET', KEYS[1], 'node_id'), redis.call('HGET', KEYS[1], 'connection_id'),
  redis.call('HGET', KEYS[1], 'connection_epoch'), redis.call('HGET', KEYS[1], 'assignment_version'),
  redis.call('HGET', KEYS[1], 'grant_version'), redis.call('HGET', KEYS[1], 'protocol_version'),
  redis.call('HGET', KEYS[1], 'connected_at_ms'), redis.call('HGET', KEYS[1], 'heartbeat_at_ms'),
  redis.call('PTTL', KEYS[1])}
`

// KEYS are route, assignment fence, grant fence, credential.  They share one
// device hash tag.  Check the credential first to preserve the browser-ticket
// issuer's original fail-closed order, then return the route snapshot from the
// same Redis execution point.
const resolveVerifiedPeerRouteScript = luaDecimalCompare + `
if redis.call('HGET', KEYS[4], 'schema_version') ~= ARGV[1] then
  return {'CREDENTIAL_UNAVAILABLE'}
end
if decimal_compare(redis.call('HGET', KEYS[4], 'version'), ARGV[2]) ~= 0 or
   redis.call('HGET', KEYS[4], 'status') ~= 'active' or
   redis.call('HGET', KEYS[4], 'thumbprint') ~= ARGV[3] then
  return {'CREDENTIAL_STALE'}
end
local credential_key = redis.call('HGET', KEYS[4], 'public_key')
if not credential_key or credential_key == '' then
  return {'CREDENTIAL_UNAVAILABLE'}
end
if redis.call('EXISTS', KEYS[1]) == 0 then return {'ROUTE_NOT_FOUND'} end
if redis.call('HGET', KEYS[2], 'schema_version') ~= ARGV[1] or redis.call('HGET', KEYS[3], 'schema_version') ~= ARGV[1] then
  redis.call('DEL', KEYS[1]); return {'ROUTE_FENCE_UNAVAILABLE'}
end
if redis.call('HGET', KEYS[2], 'user_id') ~= redis.call('HGET', KEYS[1], 'user_id') or
   decimal_compare(redis.call('HGET', KEYS[2], 'version'), redis.call('HGET', KEYS[1], 'assignment_version')) ~= 0 then
  redis.call('DEL', KEYS[1]); return {'ROUTE_ASSIGNMENT_STALE'}
end
local decoded, allowed = pcall(cjson.decode, redis.call('HGET', KEYS[2], 'allowed_cells'))
if not decoded or type(allowed) ~= 'table' then
  redis.call('DEL', KEYS[1]); return {'ROUTE_FENCE_UNAVAILABLE'}
end
local cell_allowed = false
local route_cell = redis.call('HGET', KEYS[1], 'cell_id')
for _, cell in ipairs(allowed) do if cell == route_cell then cell_allowed = true end end
if not cell_allowed then redis.call('DEL', KEYS[1]); return {'ROUTE_ASSIGNMENT_STALE'} end
if decimal_compare(redis.call('HGET', KEYS[3], 'version'), redis.call('HGET', KEYS[1], 'grant_version')) ~= 0 or redis.call('HGET', KEYS[3], 'status') ~= 'active' then
  redis.call('DEL', KEYS[1]); return {'ROUTE_GRANT_STALE'}
end
return {'OK', credential_key, redis.call('HGET', KEYS[1], 'user_id'), route_cell,
  redis.call('HGET', KEYS[1], 'node_id'), redis.call('HGET', KEYS[1], 'connection_id'),
  redis.call('HGET', KEYS[1], 'connection_epoch'), redis.call('HGET', KEYS[1], 'assignment_version'),
  redis.call('HGET', KEYS[1], 'grant_version'), redis.call('HGET', KEYS[1], 'protocol_version'),
  redis.call('HGET', KEYS[1], 'connected_at_ms'), redis.call('HGET', KEYS[1], 'heartbeat_at_ms'),
  redis.call('PTTL', KEYS[1])}
`

const deleteRouteScript = luaDecimalCompare + `
if redis.call('EXISTS', KEYS[1]) == 0 then return 'NOT_FOUND' end
if redis.call('HGET', KEYS[1], 'connection_id') ~= ARGV[1] or decimal_compare(redis.call('HGET', KEYS[1], 'connection_epoch'), ARGV[2]) ~= 0 then
  return 'STALE'
end
redis.call('DEL', KEYS[1])
return 'DELETED'
`
