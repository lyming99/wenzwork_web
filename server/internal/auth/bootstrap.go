package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrBootstrapAlreadyComplete = errors.New("a super administrator already exists")

type BootstrapResult struct {
	User    User
	Created bool
}

// SuperAdminEmail reports whether bootstrap has already created or assigned a
// super administrator without requiring the one-time bootstrap password.
func SuperAdminEmail(ctx context.Context, db *gorm.DB) (string, bool, error) {
	if db == nil {
		return "", false, errors.New("bootstrap database is required")
	}
	var result struct {
		Email string `gorm:"column:email"`
	}
	err := db.WithContext(ctx).
		Table("users u").
		Select("u.email").
		Joins("JOIN user_roles ur ON ur.user_id = u.id").
		Joins("JOIN roles r ON r.id = ur.role_id").
		Where("r.code = 'super_admin'").
		Order("u.created_at ASC, u.id ASC").
		Take(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("find super administrator: %w", err)
	}
	return result.Email, true, nil
}

// SuperAdminID returns the oldest super administrator for trusted local
// deployment bootstrap commands. It never changes roles or authentication
// state and is not exposed through HTTP.
func SuperAdminID(ctx context.Context, db *gorm.DB) (uuid.UUID, bool, error) {
	if db == nil {
		return uuid.Nil, false, errors.New("bootstrap database is required")
	}
	var result struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	err := db.WithContext(ctx).
		Table("users u").
		Select("u.id").
		Joins("JOIN user_roles ur ON ur.user_id = u.id").
		Joins("JOIN roles r ON r.id = ur.role_id").
		Where("r.code = 'super_admin'").
		Order("u.created_at ASC, u.id ASC").
		Take(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("find super administrator ID: %w", err)
	}
	return result.ID, true, nil
}

// BootstrapSuperAdmin creates exactly the first super administrator. Once any
// super administrator exists, this function refuses to elevate another account.
func BootstrapSuperAdmin(ctx context.Context, db *gorm.DB, rawEmail, password, rawDisplayName string, params Argon2Params) (BootstrapResult, error) {
	if db == nil {
		return BootstrapResult{}, errors.New("bootstrap database is required")
	}
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return BootstrapResult{}, err
	}
	displayName, err := normalizeDisplayName(rawDisplayName)
	if err != nil {
		return BootstrapResult{}, err
	}
	passwordHash, err := HashPassword(password, params)
	if err != nil {
		return BootstrapResult{}, err
	}
	now := time.Now().UTC()
	var result BootstrapResult
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var superRole struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Table("roles").Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").Where("code = 'super_admin'").Take(&superRole).Error; err != nil {
			return fmt.Errorf("lock super administrator role: %w", err)
		}

		var existingSuperCount int64
		if err := tx.Table("user_roles ur").Joins("JOIN roles r ON r.id = ur.role_id").
			Where("r.code = 'super_admin'").Count(&existingSuperCount).Error; err != nil {
			return fmt.Errorf("count super administrators: %w", err)
		}
		if existingSuperCount > 0 {
			var existing userRow
			loadErr := tx.Joins("JOIN user_roles ur ON ur.user_id = users.id").
				Where("users.email = ? AND ur.role_id = ?", email, superRole.ID).First(&existing).Error
			if loadErr == nil {
				result = BootstrapResult{User: User{
					ID: existing.ID, Email: existing.Email, DisplayName: existing.DisplayName,
					Status: existing.Status, EmailVerifiedAt: existing.EmailVerifiedAt,
					Roles: []string{"super_admin", "user"},
				}, Created: false}
				return nil
			}
			if !errors.Is(loadErr, gorm.ErrRecordNotFound) {
				return fmt.Errorf("load existing bootstrap administrator: %w", loadErr)
			}
			return ErrBootstrapAlreadyComplete
		}

		var existingUsers int64
		if err := tx.Model(&userRow{}).Where("email = ?", email).Count(&existingUsers).Error; err != nil {
			return fmt.Errorf("check bootstrap email: %w", err)
		}
		if existingUsers > 0 {
			return ErrEmailUnavailable
		}
		row := userRow{
			ID: uuid.New(), Email: email, PasswordHash: passwordHash, DisplayName: displayName,
			Status: "active", EmailVerifiedAt: &now, PasswordChanged: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create bootstrap administrator: %w", err)
		}
		if err := tx.Exec(`
			INSERT INTO user_roles (user_id, role_id, granted_by, created_at)
			SELECT ?, id, ?, ? FROM roles WHERE code IN ('user', 'super_admin')
		`, row.ID, row.ID, now).Error; err != nil {
			return fmt.Errorf("grant bootstrap administrator roles: %w", err)
		}
		if err := tx.Exec(`
			INSERT INTO audit_logs (id, actor_user_id, action, resource_type, resource_id, after_json, created_at)
			VALUES (?, ?, 'admin.bootstrap', 'user', ?, jsonb_build_object('userId', ?::text, 'role', 'super_admin'), ?)
		`, uuid.New(), row.ID, row.ID, row.ID, now).Error; err != nil {
			return fmt.Errorf("audit bootstrap administrator: %w", err)
		}
		result = BootstrapResult{
			User: User{
				ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status,
				EmailVerifiedAt: row.EmailVerifiedAt, Roles: []string{"super_admin", "user"},
			},
			Created: true,
		}
		return nil
	})
	return result, err
}
