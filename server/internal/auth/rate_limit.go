package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	loginRateWindow   = 15 * time.Minute
	loginBlockBase    = 5 * time.Minute
	loginMaxBlock     = 24 * time.Hour
	loginAttemptLimit = 5
)

var ErrRateLimited = errors.New("authentication rate limit exceeded")

func (s *Service) loginRateLimited(ctx context.Context, email, clientIP string) (bool, error) {
	for _, item := range []struct {
		scope string
		value string
	}{{"login_email", email}, {"login_ip", normalizedRateValue(clientIP)}} {
		limited, err := s.rateLimited(ctx, item.scope, item.value)
		if err != nil {
			return false, err
		}
		if limited {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) rateLimited(ctx context.Context, scope, value string) (bool, error) {
	var row authRateLimitRow
	err := s.db.WithContext(ctx).First(&row, "scope = ? AND key_digest = ?", scope, rateKeyDigest(scope, value)).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load auth rate limit: %w", err)
	}
	return row.BlockedUntil != nil && row.BlockedUntil.After(s.now().UTC()), nil
}

func (s *Service) recordLoginFailure(ctx context.Context, email, clientIP string) error {
	for _, item := range []struct {
		scope string
		value string
	}{{"login_email", email}, {"login_ip", normalizedRateValue(clientIP)}} {
		if err := s.recordRateFailure(ctx, item.scope, item.value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) clearLoginEmailFailures(ctx context.Context, email string) error {
	if err := s.db.WithContext(ctx).Delete(&authRateLimitRow{}, "scope = ? AND key_digest = ?", "login_email", rateKeyDigest("login_email", email)).Error; err != nil {
		return fmt.Errorf("clear auth login failures: %w", err)
	}
	return nil
}

func (s *Service) recordRateFailure(ctx context.Context, scope, value string) error {
	now := s.now().UTC()
	digest := rateKeyDigest(scope, value)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
			INSERT INTO auth_rate_limits (scope, key_digest, attempt_count, window_started_at, updated_at)
			VALUES (?, ?, 0, ?, ?)
			ON CONFLICT (scope, key_digest) DO NOTHING
		`, scope, digest, now, now).Error; err != nil {
			return fmt.Errorf("initialize auth rate limit: %w", err)
		}
		var row authRateLimitRow
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "scope = ? AND key_digest = ?", scope, digest).Error
		if err != nil {
			return fmt.Errorf("lock auth rate limit: %w", err)
		}
		if row.WindowStartedAt.Before(now.Add(-loginRateWindow)) {
			row.AttemptCount = 1
			row.WindowStartedAt = now
			row.BlockedUntil = nil
		} else {
			row.AttemptCount++
			if row.AttemptCount >= loginAttemptLimit {
				block := loginBlockBase << min(row.AttemptCount-loginAttemptLimit, 8)
				if block > loginMaxBlock {
					block = loginMaxBlock
				}
				blockedUntil := now.Add(block)
				row.BlockedUntil = &blockedUntil
			}
		}
		row.UpdatedAt = now
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("update auth rate limit: %w", err)
		}
		return nil
	})
}

func normalizedRateValue(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "unknown"
	}
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

func rateKeyDigest(scope, value string) string {
	digest := sha256.Sum256([]byte(scope + "\x00" + normalizedRateValue(value)))
	return hex.EncodeToString(digest[:])
}

type authRateLimitRow struct {
	Scope           string     `gorm:"column:scope;primaryKey"`
	KeyDigest       string     `gorm:"column:key_digest;primaryKey"`
	AttemptCount    int        `gorm:"column:attempt_count"`
	WindowStartedAt time.Time  `gorm:"column:window_started_at"`
	BlockedUntil    *time.Time `gorm:"column:blocked_until"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (authRateLimitRow) TableName() string { return "auth_rate_limits" }
