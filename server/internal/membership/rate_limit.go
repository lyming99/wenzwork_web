package membership

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	redemptionAttemptLimit = 10
	redemptionWindow       = 15 * time.Minute
	redemptionBlock        = 15 * time.Minute
)

type redemptionLimitKey struct {
	scope  string
	digest string
}

func (s *Store) RedeemFromIP(ctx context.Context, userID uuid.UUID, rawCode, rawIP string) (RedemptionResult, error) {
	keys := redemptionLimitKeys(userID, rawIP)
	now := s.now().UTC()
	for _, key := range keys {
		limited, err := s.isRedemptionLimited(ctx, key, now)
		if err != nil {
			return RedemptionResult{}, err
		}
		if limited {
			return RedemptionResult{}, ErrRedemptionRateLimit
		}
	}

	result, err := s.Redeem(ctx, userID, rawCode)
	if err != nil {
		if errors.Is(err, ErrCodeUnavailable) || errors.Is(err, ErrMembershipNotExtended) {
			for _, key := range keys {
				if recordErr := s.recordRedemptionFailure(ctx, key, now); recordErr != nil {
					return RedemptionResult{}, recordErr
				}
			}
		}
		return RedemptionResult{}, err
	}
	if err := s.clearRedemptionLimits(ctx, keys); err != nil {
		return RedemptionResult{}, err
	}
	return result, nil
}

func redemptionLimitKeys(userID uuid.UUID, rawIP string) []redemptionLimitKey {
	ip := strings.TrimSpace(rawIP)
	if parsed := net.ParseIP(ip); parsed != nil {
		ip = parsed.String()
	} else {
		ip = "invalid-client-address"
	}
	return []redemptionLimitKey{
		{scope: "redemption_user", digest: redemptionKeyDigest("redemption_user", userID.String())},
		{scope: "redemption_ip", digest: redemptionKeyDigest("redemption_ip", ip)},
	}
}

func redemptionKeyDigest(scope, value string) string {
	digest := sha256.Sum256([]byte(scope + "\x00" + value))
	return hex.EncodeToString(digest[:])
}

func (s *Store) isRedemptionLimited(ctx context.Context, key redemptionLimitKey, now time.Time) (bool, error) {
	var row redemptionRateLimitRow
	err := s.db.WithContext(ctx).First(&row, "scope = ? AND key_digest = ?", key.scope, key.digest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load redemption rate limit: %w", err)
	}
	return row.BlockedUntil != nil && row.BlockedUntil.After(now), nil
}

func (s *Store) recordRedemptionFailure(ctx context.Context, key redemptionLimitKey, now time.Time) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
			INSERT INTO auth_rate_limits (scope, key_digest, attempt_count, window_started_at, updated_at)
			VALUES (?, ?, 0, ?, ?)
			ON CONFLICT (scope, key_digest) DO NOTHING
		`, key.scope, key.digest, now, now).Error; err != nil {
			return fmt.Errorf("initialize redemption rate limit: %w", err)
		}
		var row redemptionRateLimitRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&row, "scope = ? AND key_digest = ?", key.scope, key.digest).Error; err != nil {
			return fmt.Errorf("lock redemption rate limit: %w", err)
		}
		attempts := row.AttemptCount
		windowStart := row.WindowStartedAt
		if now.Sub(windowStart) >= redemptionWindow || (row.BlockedUntil != nil && !row.BlockedUntil.After(now)) {
			attempts = 0
			windowStart = now
		}
		attempts++
		var blockedUntil *time.Time
		if attempts >= redemptionAttemptLimit {
			value := now.Add(redemptionBlock)
			blockedUntil = &value
		}
		if err := tx.Model(&redemptionRateLimitRow{}).
			Where("scope = ? AND key_digest = ?", key.scope, key.digest).
			Updates(map[string]any{
				"attempt_count": attempts, "window_started_at": windowStart,
				"blocked_until": blockedUntil, "updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("update redemption rate limit: %w", err)
		}
		return nil
	})
}

func (s *Store) clearRedemptionLimits(ctx context.Context, keys []redemptionLimitKey) error {
	for _, key := range keys {
		if err := s.db.WithContext(ctx).Delete(&redemptionRateLimitRow{}, "scope = ? AND key_digest = ?", key.scope, key.digest).Error; err != nil {
			return fmt.Errorf("clear redemption rate limit: %w", err)
		}
	}
	return nil
}

type redemptionRateLimitRow struct {
	Scope           string     `gorm:"column:scope;primaryKey"`
	KeyDigest       string     `gorm:"column:key_digest;primaryKey"`
	AttemptCount    int        `gorm:"column:attempt_count"`
	WindowStartedAt time.Time  `gorm:"column:window_started_at"`
	BlockedUntil    *time.Time `gorm:"column:blocked_until"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (redemptionRateLimitRow) TableName() string { return "auth_rate_limits" }
