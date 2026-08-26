package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrAdminUserNotFound      = errors.New("admin user target not found")
	ErrAdminUserSelfDisable   = errors.New("administrator cannot disable their own account")
	ErrAdminUserFilterInvalid = errors.New("admin user filter is invalid")
	ErrLastSuperAdmin         = errors.New("last active super administrator cannot be disabled")
)

type AdminUserListFilter struct {
	Query  string
	Status string
	Limit  int
	Offset int
}

type AdminMembershipSummary struct {
	PlanCode  string     `json:"planCode"`
	PlanName  string     `json:"planName"`
	StartsAt  time.Time  `json:"startsAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Lifetime  bool       `json:"lifetime"`
}

type AdminUser struct {
	ID              uuid.UUID               `json:"id"`
	Email           string                  `json:"email"`
	DisplayName     string                  `json:"displayName"`
	Status          string                  `json:"status"`
	EmailVerifiedAt *time.Time              `json:"emailVerifiedAt"`
	Roles           []string                `json:"roles"`
	Membership      *AdminMembershipSummary `json:"membership"`
	CreatedAt       time.Time               `json:"createdAt"`
}

type AdminUserList struct {
	Items  []AdminUser `json:"items"`
	Total  int64       `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

type AdminCreateUserInput struct {
	Email       string
	Password    string
	DisplayName string
	ActorUserID uuid.UUID
}

type adminUserListRow struct {
	ID                  uuid.UUID       `gorm:"column:id"`
	Email               string          `gorm:"column:email"`
	DisplayName         string          `gorm:"column:display_name"`
	Status              string          `gorm:"column:status"`
	EmailVerifiedAt     *time.Time      `gorm:"column:email_verified_at"`
	CreatedAt           time.Time       `gorm:"column:created_at"`
	RolesJSON           json.RawMessage `gorm:"column:roles_json"`
	MembershipPlanCode  *string         `gorm:"column:membership_plan_code"`
	MembershipPlanName  *string         `gorm:"column:membership_plan_name"`
	MembershipStartsAt  *time.Time      `gorm:"column:membership_starts_at"`
	MembershipExpiresAt *time.Time      `gorm:"column:membership_expires_at"`
}

func (s *Service) ListAdminUsers(ctx context.Context, filter AdminUserListFilter) (AdminUserList, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	if len(filter.Query) > 320 {
		return AdminUserList{}, ErrAdminUserFilterInvalid
	}
	if filter.Status != "" && filter.Status != "active" && filter.Status != "disabled" && filter.Status != "pending" {
		return AdminUserList{}, ErrAdminUserFilterInvalid
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	query := s.db.WithContext(ctx).Table("users AS u")
	if filter.Query != "" {
		pattern := "%" + filter.Query + "%"
		query = query.Where("u.email ILIKE ? OR u.display_name ILIKE ?", pattern, pattern)
	}
	if filter.Status != "" {
		query = query.Where("u.status = ?", filter.Status)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return AdminUserList{}, fmt.Errorf("count admin users: %w", err)
	}

	var rows []adminUserListRow
	err := query.Select(`
		u.id, u.email, u.display_name, u.status, u.email_verified_at, u.created_at,
		COALESCE((
			SELECT jsonb_agg(r.code ORDER BY r.code)
			FROM user_roles ur JOIN roles r ON r.id = ur.role_id
			WHERE ur.user_id = u.id
		), '[]'::jsonb) AS roles_json,
		p.code AS membership_plan_code, p.name AS membership_plan_name,
		m.starts_at AS membership_starts_at, m.expires_at AS membership_expires_at
	`).Joins(`
		LEFT JOIN memberships m ON m.user_id = u.id
			AND m.status = 'active' AND (m.expires_at IS NULL OR m.expires_at > ?)
	`, s.now().UTC()).Joins("LEFT JOIN membership_plans p ON p.id = m.plan_id").
		Order("u.created_at DESC, u.id DESC").Limit(filter.Limit).Offset(filter.Offset).Scan(&rows).Error
	if err != nil {
		return AdminUserList{}, fmt.Errorf("list admin users: %w", err)
	}

	items := make([]AdminUser, 0, len(rows))
	for _, row := range rows {
		var roles []string
		if err := json.Unmarshal(row.RolesJSON, &roles); err != nil {
			return AdminUserList{}, fmt.Errorf("decode roles for user %s: %w", row.ID, err)
		}
		item := AdminUser{
			ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status,
			EmailVerifiedAt: row.EmailVerifiedAt, Roles: roles, CreatedAt: row.CreatedAt.UTC(),
		}
		if row.MembershipPlanCode != nil && row.MembershipPlanName != nil && row.MembershipStartsAt != nil {
			item.Membership = &AdminMembershipSummary{
				PlanCode: *row.MembershipPlanCode, PlanName: *row.MembershipPlanName,
				StartsAt: row.MembershipStartsAt.UTC(), ExpiresAt: row.MembershipExpiresAt,
				Lifetime: row.MembershipExpiresAt == nil,
			}
		}
		items = append(items, item)
	}
	return AdminUserList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Service) CreateAdminUser(ctx context.Context, input AdminCreateUserInput) (AdminUser, error) {
	email, err := normalizeEmail(input.Email)
	if err != nil {
		return AdminUser{}, err
	}
	displayName, err := normalizeDisplayName(input.DisplayName)
	if err != nil {
		return AdminUser{}, err
	}
	if input.ActorUserID == uuid.Nil {
		return AdminUser{}, errors.New("admin user creator is required")
	}
	passwordHash, err := HashPassword(input.Password, s.config.PasswordParams)
	if err != nil {
		return AdminUser{}, err
	}
	now := s.now().UTC()
	row := userRow{
		ID: uuid.New(), Email: email, PasswordHash: passwordHash, DisplayName: displayName,
		Status: "active", EmailVerifiedAt: &now, PasswordChanged: now, CreatedAt: now, UpdatedAt: now,
	}
	created := AdminUser{
		ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status,
		EmailVerifiedAt: row.EmailVerifiedAt, Roles: []string{"user"}, CreatedAt: now,
	}
	afterJSON, err := json.Marshal(created)
	if err != nil {
		return AdminUser{}, fmt.Errorf("serialize created admin user: %w", err)
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrEmailUnavailable
			}
			return fmt.Errorf("create admin user: %w", err)
		}
		if err := tx.Exec(`
			INSERT INTO user_roles (user_id, role_id, granted_by)
			SELECT ?, id, ? FROM roles WHERE code = 'user'
		`, row.ID, input.ActorUserID).Error; err != nil {
			return fmt.Errorf("grant admin-created user role: %w", err)
		}
		if err := tx.Table("audit_logs").Create(map[string]any{
			"id": uuid.New(), "actor_user_id": input.ActorUserID, "action": "user.create",
			"resource_type": "user", "resource_id": row.ID, "after_json": afterJSON, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("audit admin user creation: %w", err)
		}
		return nil
	})
	if err != nil {
		return AdminUser{}, err
	}
	return created, nil
}

func (s *Service) SetAdminUserStatus(ctx context.Context, userID, actorUserID uuid.UUID, status string) (AdminUser, error) {
	if userID == uuid.Nil || actorUserID == uuid.Nil || (status != "active" && status != "disabled") {
		return AdminUser{}, errors.New("valid user, actor, and status are required")
	}
	if userID == actorUserID && status == "disabled" {
		return AdminUser{}, ErrAdminUserSelfDisable
	}

	var result AdminUser
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row userRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAdminUserNotFound
			}
			return fmt.Errorf("lock admin user target: %w", err)
		}
		roles, err := s.loadRoles(ctx, tx, userID)
		if err != nil {
			return err
		}
		if status == "disabled" && containsRole(roles, "super_admin") {
			var activeSuperAdmins int64
			if err := tx.Table("users AS u").
				Joins("JOIN user_roles ur ON ur.user_id = u.id").
				Joins("JOIN roles r ON r.id = ur.role_id").
				Where("u.status = 'active' AND r.code = 'super_admin'").Count(&activeSuperAdmins).Error; err != nil {
				return fmt.Errorf("count active super administrators: %w", err)
			}
			if activeSuperAdmins <= 1 {
				return ErrLastSuperAdmin
			}
		}
		beforeJSON, _ := json.Marshal(map[string]any{"status": row.Status})
		now := s.now().UTC()
		if err := tx.Model(&userRow{}).Where("id = ?", userID).
			Updates(map[string]any{"status": status, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("set admin user status: %w", err)
		}
		if status == "disabled" {
			if err := revokeAllSessions(tx, userID, now, "account_disabled", uuid.Nil); err != nil {
				return err
			}
		}
		afterJSON, _ := json.Marshal(map[string]any{"status": status})
		if err := tx.Table("audit_logs").Create(map[string]any{
			"id": uuid.New(), "actor_user_id": actorUserID, "action": "user.status.update",
			"resource_type": "user", "resource_id": userID, "before_json": beforeJSON,
			"after_json": afterJSON, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("audit admin user status: %w", err)
		}
		row.Status = status
		result = AdminUser{
			ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Status: status,
			EmailVerifiedAt: row.EmailVerifiedAt, Roles: roles, CreatedAt: row.CreatedAt.UTC(),
		}
		return nil
	})
	if err != nil {
		return AdminUser{}, err
	}
	return result, nil
}

func containsRole(roles []string, target string) bool {
	for _, role := range roles {
		if role == target {
			return true
		}
	}
	return false
}
