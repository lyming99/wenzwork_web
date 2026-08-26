package relayrouter

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// NegotiatedRegistry contains only Relay-originated live routes. Unlike the
// legacy projection registry, resolving a route never depends on periodically
// refreshed Assignment/Grant/Credential fence keys: Host validates those
// PostgreSQL facts on every authenticated Relay heartbeat before publishing.
type NegotiatedRegistry struct {
	registry *RedisRegistry
}

func NewNegotiatedRedisRegistryFromURL(rawURL string) (*NegotiatedRegistry, error) {
	registry, err := NewRedisRegistryFromURL(rawURL)
	if err != nil {
		return nil, err
	}
	return &NegotiatedRegistry{registry: registry}, nil
}

func (registry *NegotiatedRegistry) Close() error {
	if registry == nil || registry.registry == nil {
		return nil
	}
	return registry.registry.Close()
}

func (registry *NegotiatedRegistry) Ping(ctx context.Context) error {
	if registry == nil || registry.registry == nil {
		return ErrRouteStoreUnavailable
	}
	return registry.registry.Ping(ctx)
}

func (registry *NegotiatedRegistry) Publish(ctx context.Context, nodeID string, routes []Route, ttl time.Duration, now time.Time) error {
	if registry == nil || registry.registry == nil || !validRedisIdentifier(nodeID) || ttl <= 0 || ttl > 10*time.Minute {
		return ErrRouteStoreUnavailable
	}
	indexKey := registry.registry.prefix + ":{node:" + nodeID + "}:resident-routes"
	previous, err := registry.registry.client.SMembers(ctx, indexKey).Result()
	if err != nil {
		return ErrRouteStoreUnavailable
	}
	current := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		if route.NodeID != nodeID {
			return ErrConnectionStale
		}
		if err := validateRedisRoute(route, ttl); err != nil {
			return err
		}
		value, err := registry.registry.client.Eval(ctx, negotiatedRegisterRouteScript, []string{registry.registry.routeKey(route.DeviceID)},
			redisSchemaVersion, route.UserID, route.CellID, route.NodeID, route.ConnectionID,
			strconv.FormatUint(route.ConnectionEpoch, 10), strconv.FormatUint(route.AssignmentVersion, 10),
			strconv.FormatUint(route.GrantVersion, 10), strconv.FormatUint(uint64(route.ProtocolVersion), 10),
			now.UTC().UnixMilli(), ttl.Milliseconds()).Result()
		if err != nil {
			return ErrRouteStoreUnavailable
		}
		if routeErr := routeCodeError(redisString(value), nil); routeErr != nil {
			if errors.Is(routeErr, ErrConnectionStale) {
				continue
			}
			return routeErr
		}
		current[negotiatedRouteIndexValue(route)] = struct{}{}
	}
	for _, encoded := range previous {
		if _, retained := current[encoded]; retained {
			continue
		}
		deviceID, connectionID, epoch, ok := parseNegotiatedRouteIndexValue(encoded)
		if ok {
			registry.registry.CompareAndDelete(deviceID, connectionID, epoch)
		}
	}
	_, err = registry.registry.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Del(ctx, indexKey)
		if len(current) > 0 {
			members := make([]interface{}, 0, len(current))
			for value := range current {
				members = append(members, value)
			}
			pipe.SAdd(ctx, indexKey, members...)
			pipe.PExpire(ctx, indexKey, ttl)
		}
		return nil
	})
	if err != nil {
		return ErrRouteStoreUnavailable
	}
	return nil
}

func negotiatedRouteIndexValue(route Route) string {
	return route.DeviceID + "|" + route.ConnectionID + "|" + strconv.FormatUint(route.ConnectionEpoch, 10)
}

func parseNegotiatedRouteIndexValue(value string) (string, string, uint64, bool) {
	parts := strings.Split(value, "|")
	if len(parts) != 3 || !validRedisIdentifier(parts[0]) || !validRedisIdentifier(parts[1]) {
		return "", "", 0, false
	}
	epoch, err := strconv.ParseUint(parts[2], 10, 64)
	return parts[0], parts[1], epoch, err == nil && epoch > 0
}

func (registry *NegotiatedRegistry) Resolve(deviceID string, now time.Time) (Route, error) {
	return registry.ResolveContext(context.Background(), deviceID, now)
}

func (registry *NegotiatedRegistry) ResolveContext(ctx context.Context, deviceID string, now time.Time) (Route, error) {
	if registry == nil || registry.registry == nil || !validRedisIdentifier(deviceID) {
		return Route{}, ErrRouteNotFound
	}
	lookupContext, cancel := context.WithTimeout(ctx, registry.registry.timeout)
	defer cancel()
	value, err := registry.registry.client.Eval(lookupContext, negotiatedResolveRouteScript,
		[]string{registry.registry.routeKey(deviceID)}, redisSchemaVersion).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return Route{}, ErrRouteStoreUnavailable
	}
	values, ok := value.([]interface{})
	if !ok || len(values) == 0 {
		return Route{}, ErrRouteStoreUnavailable
	}
	code := redisString(values[0])
	if code == "FENCE_UNAVAILABLE" {
		return Route{}, ErrRouteStoreUnavailable
	}
	if code != "OK" {
		return Route{}, routeCodeError(code, nil)
	}
	return routeFromRedisFields(deviceID, now, values[1:])
}

const negotiatedRegisterRouteScript = luaDecimalCompare + `
local current_epoch = redis.call('HGET', KEYS[1], 'connection_epoch')
if current_epoch then
  local comparison = decimal_compare(ARGV[6], current_epoch)
  if comparison < 0 then return 'CONNECTION_STALE' end
  if comparison == 0 then
    if redis.call('HGET', KEYS[1], 'connection_id') ~= ARGV[5] or
       redis.call('HGET', KEYS[1], 'node_id') ~= ARGV[4] or
       redis.call('HGET', KEYS[1], 'user_id') ~= ARGV[2] or
       redis.call('HGET', KEYS[1], 'cell_id') ~= ARGV[3] or
       redis.call('HGET', KEYS[1], 'assignment_version') ~= ARGV[7] or
       redis.call('HGET', KEYS[1], 'grant_version') ~= ARGV[8] then
      return 'CONNECTION_STALE'
    end
    redis.call('HSET', KEYS[1], 'heartbeat_at_ms', ARGV[10])
    redis.call('PEXPIRE', KEYS[1], ARGV[11])
    return 'OK'
  end
end
redis.call('HSET', KEYS[1],
  'schema_version', ARGV[1], 'user_id', ARGV[2], 'cell_id', ARGV[3], 'node_id', ARGV[4],
  'connection_id', ARGV[5], 'connection_epoch', ARGV[6], 'assignment_version', ARGV[7],
  'grant_version', ARGV[8], 'protocol_version', ARGV[9], 'connected_at_ms', ARGV[10], 'heartbeat_at_ms', ARGV[10])
redis.call('PEXPIRE', KEYS[1], ARGV[11])
return 'OK'
`

const negotiatedResolveRouteScript = `
if redis.call('EXISTS', KEYS[1]) == 0 then return {'ROUTE_NOT_FOUND'} end
if redis.call('HGET', KEYS[1], 'schema_version') ~= ARGV[1] then return {'FENCE_UNAVAILABLE'} end
return {'OK', redis.call('HGET', KEYS[1], 'user_id'), redis.call('HGET', KEYS[1], 'cell_id'),
  redis.call('HGET', KEYS[1], 'node_id'), redis.call('HGET', KEYS[1], 'connection_id'),
  redis.call('HGET', KEYS[1], 'connection_epoch'), redis.call('HGET', KEYS[1], 'assignment_version'),
  redis.call('HGET', KEYS[1], 'grant_version'), redis.call('HGET', KEYS[1], 'protocol_version'),
  redis.call('HGET', KEYS[1], 'connected_at_ms'), redis.call('HGET', KEYS[1], 'heartbeat_at_ms'),
  redis.call('PTTL', KEYS[1])}
`
