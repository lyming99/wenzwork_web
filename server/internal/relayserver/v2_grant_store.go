package relayserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

const v2GrantUseStoreTimeout = 750 * time.Millisecond

var consumeV2DeviceLinkGrantScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then
  return 0
end
if redis.call('SET', KEYS[1], 'used', 'PX', ARGV[1], 'NX') then
  return 1
end
return 0`)

// RedisV2GrantUseStore is the cross-Relay use/revocation fence for a v2
// DeviceConnectionGrant. Bounded legacy Grants remain single-use. Persistent
// PoP Grants are reusable and consult only the explicit revocation key. The
// store retains a digest of the non-bearer Grant ID, never the Grant JWT.
type RedisV2GrantUseStore struct {
	client  redis.UniversalClient
	prefix  string
	timeout time.Duration
}

func NewRedisV2GrantUseStore(client redis.UniversalClient) (*RedisV2GrantUseStore, error) {
	if client == nil {
		return nil, errors.New("remote/v2 grant use store client is required")
	}
	return &RedisV2GrantUseStore{client: client, prefix: "relay:v2:device-link-grant", timeout: v2GrantUseStoreTimeout}, nil
}

func NewRedisV2GrantUseStoreFromURL(rawURL string) (*RedisV2GrantUseStore, error) {
	options, err := redis.ParseURL(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, errors.New("remote/v2 grant use store Redis URL is invalid")
	}
	options.DialTimeout = v2GrantUseStoreTimeout
	options.ReadTimeout = v2GrantUseStoreTimeout
	options.WriteTimeout = v2GrantUseStoreTimeout
	options.MaxRetries = 0
	return NewRedisV2GrantUseStore(redis.NewClient(options))
}

func (store *RedisV2GrantUseStore) ConsumeDeviceLinkGrant(grantID string, expiresAt time.Time) (bool, error) {
	now := time.Now().UTC()
	grantID = strings.TrimSpace(grantID)
	persistent := remoteauth.IsPersistentDeviceLinkGrantExpiry(expiresAt)
	if store == nil || store.client == nil || grantID == "" || len(grantID) > 256 || expiresAt.IsZero() || !expiresAt.After(now) || (!persistent && expiresAt.After(now.Add(15*time.Minute))) {
		return false, ErrV2Route
	}
	usedKey, revokedKey := store.keys(grantID)
	requestContext, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	if persistent {
		revoked, err := store.client.Exists(requestContext, revokedKey).Result()
		if err != nil || (revoked != 0 && revoked != 1) {
			return false, ErrV2Route
		}
		return revoked == 0, nil
	}
	result, err := consumeV2DeviceLinkGrantScript.Run(requestContext, store.client, []string{usedKey, revokedKey}, expiresAt.Sub(now).Milliseconds()).Int()
	if err != nil || (result != 0 && result != 1) {
		return false, ErrV2Route
	}
	return result == 1, nil
}

// RevokeDeviceLinkGrant makes a DeviceConnectionGrant unusable on
// every Relay sharing this Redis namespace. Only a grant-ID digest is stored,
// never the signed JWT or Link/Channel content.
func (store *RedisV2GrantUseStore) RevokeDeviceLinkGrant(grantID string, expiresAt time.Time) error {
	now := time.Now().UTC()
	grantID = strings.TrimSpace(grantID)
	persistent := remoteauth.IsPersistentDeviceLinkGrantExpiry(expiresAt)
	if store == nil || store.client == nil || grantID == "" || len(grantID) > 256 || expiresAt.IsZero() || !expiresAt.After(now) || (!persistent && expiresAt.After(now.Add(15*time.Minute))) {
		return ErrV2Route
	}
	_, revokedKey := store.keys(grantID)
	requestContext, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	ttl := expiresAt.Sub(now)
	if persistent {
		ttl = 0
	}
	if err := store.client.Set(requestContext, revokedKey, "revoked", ttl).Err(); err != nil {
		return ErrV2Route
	}
	return nil
}

func (store *RedisV2GrantUseStore) Ping(ctx context.Context) error {
	if store == nil || store.client == nil {
		return ErrV2Route
	}
	requestContext, cancel := context.WithTimeout(ctx, store.timeout)
	defer cancel()
	if err := store.client.Ping(requestContext).Err(); err != nil {
		return ErrV2Route
	}
	return nil
}

func (store *RedisV2GrantUseStore) keys(grantID string) (string, string) {
	digest := sha256.Sum256([]byte(grantID))
	encoded := base64.RawURLEncoding.EncodeToString(digest[:])
	return store.prefix + ":" + encoded, store.prefix + ":revoked:" + encoded
}

func (store *RedisV2GrantUseStore) Close() error {
	if store == nil || store.client == nil {
		return nil
	}
	return store.client.Close()
}
