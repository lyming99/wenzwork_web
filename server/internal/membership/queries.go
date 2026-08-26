package membership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MembershipStatus struct {
	PlanCode  string     `json:"planCode"`
	PlanName  string     `json:"planName"`
	StartsAt  time.Time  `json:"startsAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Lifetime  bool       `json:"lifetime"`
	Source    string     `json:"source"`
}

type RedemptionRecord struct {
	ID              uuid.UUID  `json:"id"`
	CodeHint        string     `json:"codeHint"`
	PlanCode        string     `json:"planCode"`
	RedeemedAt      time.Time  `json:"redeemedAt"`
	ResultExpiresAt *time.Time `json:"resultExpiresAt"`
}

type BatchSummary struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	PlanCode      string     `json:"planCode"`
	GrantType     GrantType  `json:"grantType"`
	GrantDays     *int       `json:"grantDays"`
	Quantity      int        `json:"quantity"`
	ActiveCount   int        `json:"activeCount"`
	RedeemedCount int        `json:"redeemedCount"`
	RevokedCount  int        `json:"revokedCount"`
	Status        string     `json:"status"`
	RedeemBefore  *time.Time `json:"redeemBefore"`
	CreatedAt     time.Time  `json:"createdAt"`
}

func (s *Store) GetMembership(ctx context.Context, userID uuid.UUID) (MembershipStatus, error) {
	var user struct {
		CreatedAt time.Time `gorm:"column:created_at"`
	}
	if err := s.db.WithContext(ctx).Table("users").Select("created_at").
		Where("id = ? AND status = 'active'", userID).Scan(&user).Error; err != nil {
		return MembershipStatus{}, fmt.Errorf("load membership user: %w", err)
	}
	if user.CreatedAt.IsZero() {
		return MembershipStatus{}, ErrUserNotFound
	}

	var row struct {
		PlanCode  string     `gorm:"column:plan_code"`
		PlanName  string     `gorm:"column:plan_name"`
		StartsAt  time.Time  `gorm:"column:starts_at"`
		ExpiresAt *time.Time `gorm:"column:expires_at"`
		Source    string     `gorm:"column:source"`
	}
	err := s.db.WithContext(ctx).Table("memberships AS m").
		Select("p.code AS plan_code, p.name AS plan_name, m.starts_at, m.expires_at, m.source").
		Joins("JOIN membership_plans p ON p.id = m.plan_id").
		Where("m.user_id = ? AND m.status = 'active'", userID).Take(&row).Error
	if err == nil && (row.ExpiresAt == nil || row.ExpiresAt.After(s.now().UTC())) {
		return MembershipStatus{
			PlanCode: row.PlanCode, PlanName: row.PlanName, StartsAt: row.StartsAt,
			ExpiresAt: row.ExpiresAt, Lifetime: row.ExpiresAt == nil, Source: row.Source,
		}, nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return MembershipStatus{}, fmt.Errorf("load membership: %w", err)
	}

	var freePlan membershipPlanRow
	if err := s.db.WithContext(ctx).Where("code = 'free' AND status = 'active'").First(&freePlan).Error; err != nil {
		return MembershipStatus{}, fmt.Errorf("load free membership plan: %w", err)
	}
	return MembershipStatus{
		PlanCode: freePlan.Code, PlanName: freePlan.Name, StartsAt: user.CreatedAt,
		ExpiresAt: nil, Lifetime: true, Source: "system",
	}, nil
}

func (s *Store) ListRedemptions(ctx context.Context, userID uuid.UUID, limit int) ([]RedemptionRecord, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	records := make([]RedemptionRecord, 0)
	if err := s.db.WithContext(ctx).Raw(`
		SELECT c.id, c.code_hint, p.code AS plan_code, c.redeemed_at,
		       NULLIF(e.after_json->>'expiresAt', '')::timestamptz AS result_expires_at
		FROM redemption_codes c
		JOIN redemption_code_batches b ON b.id = c.batch_id
		JOIN membership_plans p ON p.id = b.plan_id
		LEFT JOIN membership_events e ON e.source_id = c.id AND e.event_type = 'redemption'
		WHERE c.redeemed_by = ? AND c.status = 'redeemed'
		ORDER BY c.redeemed_at DESC
		LIMIT ?
	`, userID, limit).Scan(&records).Error; err != nil {
		return nil, fmt.Errorf("list membership redemptions: %w", err)
	}
	return records, nil
}

func (s *Store) ListBatches(ctx context.Context, limit int) ([]BatchSummary, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	batches := make([]BatchSummary, 0)
	if err := s.db.WithContext(ctx).Raw(`
		SELECT b.id, b.name, p.code AS plan_code, b.grant_type, b.grant_days,
		       b.quantity,
		       COUNT(c.id) FILTER (WHERE c.status = 'active')::integer AS active_count,
		       COUNT(c.id) FILTER (WHERE c.status = 'redeemed')::integer AS redeemed_count,
		       COUNT(c.id) FILTER (WHERE c.status = 'revoked')::integer AS revoked_count,
		       b.status, b.redeem_before, b.created_at
		FROM redemption_code_batches b
		JOIN membership_plans p ON p.id = b.plan_id
		LEFT JOIN redemption_codes c ON c.batch_id = b.id
		WHERE NOT EXISTS (
			SELECT 1 FROM promotion_campaigns campaign WHERE campaign.batch_id = b.id
		)
		GROUP BY b.id, p.code
		ORDER BY b.created_at DESC
		LIMIT ?
	`, limit).Scan(&batches).Error; err != nil {
		return nil, fmt.Errorf("list redemption batches: %w", err)
	}
	return batches, nil
}

func (s *Store) RevokeBatch(ctx context.Context, batchID, actorUserID uuid.UUID) error {
	if batchID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrBatchNotFound
	}
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var batch redemptionCodeBatchRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Preload("Plan").First(&batch, "id = ?", batchID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrBatchNotFound
			}
			return fmt.Errorf("lock redemption batch: %w", err)
		}
		if batch.Status == "revoked" {
			return nil
		}
		before := BatchSummary{
			ID: batch.ID, Name: batch.Name, PlanCode: batch.Plan.Code, GrantType: GrantType(batch.GrantType),
			GrantDays: batch.GrantDays, Quantity: batch.Quantity, Status: batch.Status,
			RedeemBefore: batch.RedeemBefore, CreatedAt: batch.CreatedAt,
		}
		if err := tx.Model(&redemptionCodeRow{}).Where("batch_id = ? AND status = 'active'", batchID).
			Update("status", "revoked").Error; err != nil {
			return fmt.Errorf("revoke unused redemption codes: %w", err)
		}
		if err := tx.Model(&redemptionCodeBatchRow{}).Where("id = ?", batchID).
			Updates(map[string]any{"status": "revoked", "updated_at": now}).Error; err != nil {
			return fmt.Errorf("revoke redemption batch: %w", err)
		}
		after := before
		after.Status = "revoked"
		beforeJSON, _ := json.Marshal(before)
		afterJSON, _ := json.Marshal(after)
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "redemption_batch.revoke",
			ResourceType: "redemption_code_batch", ResourceID: &batchID,
			BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit redemption batch revocation: %w", err)
		}
		return nil
	})
}

type auditLogRow struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	ActorUserID  *uuid.UUID `gorm:"column:actor_user_id"`
	Action       string     `gorm:"column:action"`
	ResourceType string     `gorm:"column:resource_type"`
	ResourceID   *uuid.UUID `gorm:"column:resource_id"`
	BeforeJSON   []byte     `gorm:"column:before_json;type:jsonb"`
	AfterJSON    []byte     `gorm:"column:after_json;type:jsonb"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

func (auditLogRow) TableName() string { return "audit_logs" }
