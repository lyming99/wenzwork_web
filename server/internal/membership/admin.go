package membership

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
	ErrMembershipNotFound = errors.New("active membership not found")
	ErrMembershipInvalid  = errors.New("membership adjustment is invalid")
	ErrCodeNotFound       = errors.New("redemption code not found")
	ErrCodeAlreadyUsed    = errors.New("redeemed code cannot be revoked")
	ErrCodeFilterInvalid  = errors.New("redemption code filter is invalid")
)

type SetMembershipInput struct {
	PlanCode    string
	ExpiresAt   *time.Time
	Reason      string
	ActorUserID uuid.UUID
}

type RedemptionCodeFilter struct {
	BatchID uuid.UUID
	Status  string
	Limit   int
	Offset  int
}

type RedemptionCodeSummary struct {
	ID            uuid.UUID  `json:"id"`
	BatchID       uuid.UUID  `json:"batchId"`
	BatchName     string     `json:"batchName"`
	CodeHint      string     `json:"codeHint"`
	Status        string     `json:"status"`
	RedeemedBy    *uuid.UUID `json:"redeemedBy"`
	RedeemedEmail *string    `json:"redeemedEmail"`
	RedeemedAt    *time.Time `json:"redeemedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type RedemptionCodeList struct {
	Items  []RedemptionCodeSummary `json:"items"`
	Total  int64                   `json:"total"`
	Limit  int                     `json:"limit"`
	Offset int                     `json:"offset"`
}

func (s *Store) SetUserMembership(ctx context.Context, userID uuid.UUID, input SetMembershipInput) (MembershipStatus, error) {
	input.PlanCode = strings.TrimSpace(input.PlanCode)
	input.Reason = strings.TrimSpace(input.Reason)
	if userID == uuid.Nil || input.ActorUserID == uuid.Nil || input.PlanCode == "" {
		return MembershipStatus{}, ErrMembershipInvalid
	}
	now := s.now().UTC()
	if input.ExpiresAt != nil {
		expiresAt := input.ExpiresAt.UTC()
		if !expiresAt.After(now) {
			return MembershipStatus{}, ErrMembershipInvalid
		}
		input.ExpiresAt = &expiresAt
	}

	var result MembershipStatus
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var target struct {
			ID     uuid.UUID `gorm:"column:id"`
			Status string    `gorm:"column:status"`
		}
		if err := tx.Table("users").Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "status").Where("id = ?", userID).Take(&target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return fmt.Errorf("lock membership user: %w", err)
		}
		if target.Status == "disabled" {
			return ErrUserNotFound
		}

		var plan membershipPlanRow
		if err := tx.Where("code = ? AND code <> 'free' AND status = 'active'", input.PlanCode).
			First(&plan).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrMembershipPlanAbsent
			}
			return fmt.Errorf("load membership adjustment plan: %w", err)
		}

		var current membershipRow
		loadErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Plan").
			Where("user_id = ?", userID).First(&current).Error
		if loadErr != nil && !errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load membership adjustment target: %w", loadErr)
		}
		var beforeJSON []byte
		if loadErr == nil {
			beforeJSON, _ = json.Marshal(map[string]any{
				"planCode": current.Plan.Code, "startsAt": current.StartsAt,
				"expiresAt": current.ExpiresAt, "status": current.Status,
			})
		}

		startsAt := now
		if loadErr == nil && current.Status == "active" && current.PlanID == plan.ID &&
			(current.ExpiresAt == nil || current.ExpiresAt.After(now)) {
			startsAt = current.StartsAt
		}
		result = MembershipStatus{
			PlanCode: plan.Code, PlanName: plan.Name, StartsAt: startsAt,
			ExpiresAt: input.ExpiresAt, Lifetime: input.ExpiresAt == nil, Source: "admin_adjustment",
		}
		afterJSON, _ := json.Marshal(result)
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			current = membershipRow{
				UserID: userID, PlanID: plan.ID, StartsAt: startsAt, ExpiresAt: input.ExpiresAt,
				Source: "admin_adjustment", Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&current).Error; err != nil {
				return fmt.Errorf("create admin membership adjustment: %w", err)
			}
		} else {
			if err := tx.Model(&membershipRow{}).Where("user_id = ?", userID).Updates(map[string]any{
				"plan_id": plan.ID, "starts_at": startsAt, "expires_at": input.ExpiresAt,
				"source": "admin_adjustment", "status": "active",
				"version": gorm.Expr("version + 1"), "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("update admin membership adjustment: %w", err)
			}
		}
		if err := tx.Create(&membershipEventRow{
			ID: uuid.New(), UserID: userID, EventType: "admin_adjustment", SourceType: "admin",
			BeforeJSON: beforeJSON, AfterJSON: afterJSON, Reason: input.Reason,
			ActorUserID: &input.ActorUserID, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("record admin membership adjustment: %w", err)
		}
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "membership.set",
			ResourceType: "user", ResourceID: &userID, BeforeJSON: beforeJSON,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit admin membership adjustment: %w", err)
		}
		return nil
	})
	if err != nil {
		return MembershipStatus{}, err
	}
	return result, nil
}

func (s *Store) CancelUserMembership(ctx context.Context, userID, actorUserID uuid.UUID, reason string) error {
	if userID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrMembershipInvalid
	}
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current membershipRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Plan").
			Where("user_id = ? AND status = 'active'", userID).First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrMembershipNotFound
			}
			return fmt.Errorf("lock membership cancellation target: %w", err)
		}
		beforeJSON, _ := json.Marshal(map[string]any{
			"planCode": current.Plan.Code, "startsAt": current.StartsAt,
			"expiresAt": current.ExpiresAt, "status": current.Status,
		})
		afterJSON, _ := json.Marshal(map[string]any{"status": "revoked", "revokedAt": now})
		if err := tx.Model(&membershipRow{}).Where("user_id = ? AND status = 'active'", userID).
			Updates(map[string]any{"status": "revoked", "version": gorm.Expr("version + 1"), "updated_at": now}).Error; err != nil {
			return fmt.Errorf("cancel user membership: %w", err)
		}
		reason = strings.TrimSpace(reason)
		if err := tx.Create(&membershipEventRow{
			ID: uuid.New(), UserID: userID, EventType: "revocation", SourceType: "admin",
			BeforeJSON: beforeJSON, AfterJSON: afterJSON, Reason: reason,
			ActorUserID: &actorUserID, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("record membership cancellation: %w", err)
		}
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "membership.cancel",
			ResourceType: "user", ResourceID: &userID, BeforeJSON: beforeJSON,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit membership cancellation: %w", err)
		}
		return nil
	})
}

func (s *Store) ListRedemptionCodes(ctx context.Context, filter RedemptionCodeFilter) (RedemptionCodeList, error) {
	if filter.Status != "" && filter.Status != "active" && filter.Status != "redeemed" && filter.Status != "revoked" {
		return RedemptionCodeList{}, ErrCodeFilterInvalid
	}
	if filter.Limit < 1 || filter.Limit > 200 {
		filter.Limit = 100
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	query := s.db.WithContext(ctx).Table("redemption_codes AS c").
		Where("NOT EXISTS (SELECT 1 FROM promotion_campaigns AS campaign WHERE campaign.batch_id = c.batch_id)")
	if filter.BatchID != uuid.Nil {
		query = query.Where("c.batch_id = ?", filter.BatchID)
	}
	if filter.Status != "" {
		query = query.Where("c.status = ?", filter.Status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return RedemptionCodeList{}, fmt.Errorf("count redemption codes: %w", err)
	}
	items := make([]RedemptionCodeSummary, 0)
	if err := query.Select(`
		c.id, c.batch_id, b.name AS batch_name, c.code_hint, c.status,
		c.redeemed_by, u.email AS redeemed_email, c.redeemed_at, c.created_at
	`).Joins("JOIN redemption_code_batches b ON b.id = c.batch_id").
		Joins("LEFT JOIN users u ON u.id = c.redeemed_by").
		Order("c.created_at DESC, c.id DESC").Limit(filter.Limit).Offset(filter.Offset).
		Scan(&items).Error; err != nil {
		return RedemptionCodeList{}, fmt.Errorf("list redemption codes: %w", err)
	}
	return RedemptionCodeList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Store) RevokeRedemptionCode(ctx context.Context, codeID, actorUserID uuid.UUID) error {
	if codeID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrCodeNotFound
	}
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var code redemptionCodeRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&code, "id = ?", codeID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCodeNotFound
			}
			return fmt.Errorf("lock redemption code: %w", err)
		}
		if code.Status == "redeemed" {
			return ErrCodeAlreadyUsed
		}
		if code.Status == "revoked" {
			return nil
		}
		if err := tx.Model(&redemptionCodeRow{}).Where("id = ? AND status = 'active'", codeID).
			Update("status", "revoked").Error; err != nil {
			return fmt.Errorf("revoke redemption code: %w", err)
		}
		var activeCount int64
		if err := tx.Model(&redemptionCodeRow{}).Where("batch_id = ? AND status = 'active'", code.BatchID).
			Count(&activeCount).Error; err != nil {
			return fmt.Errorf("count active codes after revocation: %w", err)
		}
		if activeCount == 0 {
			hasQuota, err := hasAvailablePromotionQuota(tx, code.BatchID)
			if err != nil {
				return err
			}
			if !hasQuota {
				if err := tx.Model(&redemptionCodeBatchRow{}).Where("id = ? AND status = 'active'", code.BatchID).
					Updates(map[string]any{"status": "exhausted", "updated_at": now}).Error; err != nil {
					return fmt.Errorf("finish redemption code batch: %w", err)
				}
			}
		}
		beforeJSON, _ := json.Marshal(map[string]any{"status": "active", "codeHint": code.CodeHint})
		afterJSON, _ := json.Marshal(map[string]any{"status": "revoked", "codeHint": code.CodeHint})
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "redemption_code.revoke",
			ResourceType: "redemption_code", ResourceID: &codeID,
			BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit redemption code revocation: %w", err)
		}
		return nil
	})
}
