package relayrouter

import (
	"errors"
	"strconv"
	"time"
)

var (
	ErrCapacityUnavailable = errors.New("Relay Cell capacity projection unavailable")
	ErrCapacityStale       = errors.New("Relay Cell capacity projection is stale")
	ErrCapacityExceeded    = errors.New("Relay Cell capacity is exhausted")
	ErrReservationConflict = errors.New("Relay Cell reservation conflicts with an existing request")
	ErrReservationNotFound = errors.New("Relay Cell reservation not found")
)

type CellCapacity struct {
	Version           uint64
	Status            CapacityStatus
	ActiveConnections int64
	HardLimit         int64
	UpdatedAt         time.Time
}

type CapacityStatus string

const (
	CapacityActive   CapacityStatus = "active"
	CapacityDraining CapacityStatus = "draining"
	CapacityDisabled CapacityStatus = "disabled"
)

func (registry *RedisRegistry) PutCellCapacity(cellID string, capacity CellCapacity) error {
	if !validRedisIdentifier(cellID) || capacity.Version == 0 ||
		(capacity.Status != CapacityActive && capacity.Status != CapacityDraining && capacity.Status != CapacityDisabled) ||
		capacity.ActiveConnections < 0 || capacity.HardLimit <= 0 || capacity.HardLimit > 1_000_000_000_000 ||
		capacity.ActiveConnections > 1_000_000_000_000 || capacity.UpdatedAt.IsZero() {
		return ErrCapacityStale
	}
	code, err := registry.evalCode(putCellCapacityScript, registry.capacityKeys(cellID),
		redisSchemaVersion, strconv.FormatUint(capacity.Version, 10), string(capacity.Status),
		capacity.ActiveConnections, capacity.HardLimit, capacity.UpdatedAt.UTC().UnixMilli(), registry.fenceTTL.Milliseconds())
	return capacityCodeError(code, err)
}

// ReserveCellCapacity atomically prunes expired reservations and reserves
// connection slots. All keys share the Cell hash tag, so concurrent Scheduler
// replicas cannot over-assign even when Redis Cluster is used.
func (registry *RedisRegistry) ReserveCellCapacity(cellID, reservationID string, count int64, now time.Time, ttl, maximumProjectionAge time.Duration) error {
	if !validRedisIdentifier(cellID) || !validRedisIdentifier(reservationID) || count <= 0 || count > 1_000_000 ||
		now.IsZero() || ttl < time.Second || ttl > 10*time.Minute || ttl >= registry.fenceTTL ||
		maximumProjectionAge < time.Second || maximumProjectionAge > 10*time.Minute {
		return ErrReservationConflict
	}
	code, err := registry.evalCode(reserveCellCapacityScript, registry.capacityKeys(cellID),
		redisSchemaVersion, reservationID, count, now.UTC().UnixMilli(), ttl.Milliseconds(), maximumProjectionAge.Milliseconds(), registry.fenceTTL.Milliseconds())
	return capacityCodeError(code, err)
}

func (registry *RedisRegistry) ReleaseCellReservation(cellID, reservationID string) error {
	if !validRedisIdentifier(cellID) || !validRedisIdentifier(reservationID) {
		return ErrReservationNotFound
	}
	code, err := registry.evalCode(releaseCellReservationScript, registry.capacityKeys(cellID), reservationID)
	return capacityCodeError(code, err)
}

func (registry *RedisRegistry) capacityKeys(cellID string) []string {
	prefix := registry.prefix + ":{cell:" + cellID + "}"
	return []string{prefix + ":capacity", prefix + ":reservations", prefix + ":reservation-counts"}
}

func capacityCodeError(code string, err error) error {
	if err != nil {
		return ErrCapacityUnavailable
	}
	switch code {
	case "OK", "RELEASED":
		return nil
	case "CAPACITY_STALE":
		return ErrCapacityStale
	case "CAPACITY_EXCEEDED":
		return ErrCapacityExceeded
	case "RESERVATION_CONFLICT":
		return ErrReservationConflict
	case "RESERVATION_NOT_FOUND":
		return ErrReservationNotFound
	default:
		return ErrCapacityUnavailable
	}
}

const pruneReservationsLua = `
local function prune_reservations(now_ms)
  local expired = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now_ms)
  local reserved = tonumber(redis.call('HGET', KEYS[1], 'reserved_connections') or '0')
  for _, reservation_id in ipairs(expired) do
    local count = tonumber(redis.call('HGET', KEYS[3], reservation_id) or '0')
    reserved = reserved - count
    redis.call('HDEL', KEYS[3], reservation_id)
  end
  if #expired > 0 then redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now_ms) end
  if reserved < 0 then reserved = 0 end
  redis.call('HSET', KEYS[1], 'reserved_connections', reserved)
  return reserved
end
`

const putCellCapacityScript = luaDecimalCompare + `
local current_version = redis.call('HGET', KEYS[1], 'version')
if current_version then
  local comparison = decimal_compare(current_version, ARGV[2])
  if comparison > 0 then return 'CAPACITY_STALE' end
  if comparison == 0 then
    local current_updated_at = redis.call('HGET', KEYS[1], 'updated_at_ms') or '0'
    local time_comparison = decimal_compare(current_updated_at, ARGV[6])
    if time_comparison > 0 then return 'CAPACITY_STALE' end
    if time_comparison == 0 and (
      redis.call('HGET', KEYS[1], 'status') ~= ARGV[3] or
      redis.call('HGET', KEYS[1], 'active_connections') ~= ARGV[4] or
      redis.call('HGET', KEYS[1], 'hard_limit') ~= ARGV[5]
    ) then return 'RESERVATION_CONFLICT' end
  end
end
local reserved = redis.call('HGET', KEYS[1], 'reserved_connections') or '0'
redis.call('HSET', KEYS[1],
  'schema_version', ARGV[1], 'version', ARGV[2], 'status', ARGV[3],
  'active_connections', ARGV[4], 'hard_limit', ARGV[5],
  'updated_at_ms', ARGV[6], 'reserved_connections', reserved)
redis.call('PEXPIRE', KEYS[1], ARGV[7])
return 'OK'
`

const reserveCellCapacityScript = pruneReservationsLua + `
if redis.call('HGET', KEYS[1], 'schema_version') ~= ARGV[1] then return 'CAPACITY_UNAVAILABLE' end
local now_ms = tonumber(ARGV[4])
local updated_at_ms = tonumber(redis.call('HGET', KEYS[1], 'updated_at_ms') or '0')
if updated_at_ms <= 0 or now_ms - updated_at_ms > tonumber(ARGV[6]) or updated_at_ms - now_ms > tonumber(ARGV[6]) then
  return 'CAPACITY_STALE'
end
if redis.call('HGET', KEYS[1], 'status') ~= 'active' then return 'CAPACITY_STALE' end
local reserved = prune_reservations(now_ms)
local existing = redis.call('HGET', KEYS[3], ARGV[2])
if existing then
  if existing == ARGV[3] then return 'OK' end
  return 'RESERVATION_CONFLICT'
end
local active = tonumber(redis.call('HGET', KEYS[1], 'active_connections') or '-1')
local hard_limit = tonumber(redis.call('HGET', KEYS[1], 'hard_limit') or '-1')
local requested = tonumber(ARGV[3])
if active < 0 or hard_limit <= 0 then return 'CAPACITY_UNAVAILABLE' end
if active + reserved + requested > hard_limit then return 'CAPACITY_EXCEEDED' end
redis.call('HSET', KEYS[3], ARGV[2], ARGV[3])
redis.call('ZADD', KEYS[2], now_ms + tonumber(ARGV[5]), ARGV[2])
redis.call('HSET', KEYS[1], 'reserved_connections', reserved + requested)
redis.call('PEXPIRE', KEYS[1], ARGV[7])
redis.call('PEXPIRE', KEYS[2], ARGV[7])
redis.call('PEXPIRE', KEYS[3], ARGV[7])
return 'OK'
`

const releaseCellReservationScript = `
local count = redis.call('HGET', KEYS[3], ARGV[1])
if not count then return 'RESERVATION_NOT_FOUND' end
redis.call('HDEL', KEYS[3], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
if redis.call('EXISTS', KEYS[1]) == 1 then
  local reserved = tonumber(redis.call('HGET', KEYS[1], 'reserved_connections') or '0') - tonumber(count)
  if reserved < 0 then reserved = 0 end
  redis.call('HSET', KEYS[1], 'reserved_connections', reserved)
end
return 'RELEASED'
`
