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
	ErrUserNotFound         = errors.New("user not found")
	ErrEmailNotVerified     = errors.New("email address is not verified")
	ErrCodeUnavailable      = errors.New("redemption code is unavailable")
	ErrMembershipPlanAbsent = errors.New("membership plan not found")
	ErrBatchNotFound        = errors.New("redemption code batch not found")
	ErrRedemptionRateLimit  = errors.New("redemption attempts are rate limited")
)

type Store struct {
	db    *gorm.DB
	codec *CodeCodec
	now   func() time.Time
}

type CreateBatchInput struct {
	Name         string
	PlanCode     string
	GrantType    GrantType
	GrantDays    *int
	Quantity     int
	RedeemBefore *time.Time
	Note         string
	CreatedBy    uuid.UUID
}

type CreatedBatch struct {
	ID        uuid.UUID
	Batch     BatchSummary
	Plaintext []string
}

type RedemptionResult struct {
	Membership Membership
	CodeHint   string
	RedeemedAt time.Time
	EventID    uuid.UUID
}

func NewStore(db *gorm.DB, codec *CodeCodec) (*Store, error) {
	if db == nil {
		return nil, errors.New("membership store database is required")
	}
	if codec == nil {
		return nil, errors.New("membership store code codec is required")
	}
	return &Store{db: db, codec: codec, now: time.Now}, nil
}

func (s *Store) CreateBatch(ctx context.Context, input CreateBatchInput) (CreatedBatch, error) {
	if input.Name == "" || input.PlanCode == "" || input.CreatedBy == uuid.Nil {
		return CreatedBatch{}, errors.New("batch name, plan, and creator are required")
	}
	if input.GrantType != GrantDuration && input.GrantType != GrantLifetime {
		return CreatedBatch{}, errors.New("unsupported batch grant type")
	}
	if input.GrantType == GrantDuration && (input.GrantDays == nil || *input.GrantDays <= 0) {
		return CreatedBatch{}, errors.New("duration batch requires positive grant days")
	}
	if input.GrantType == GrantLifetime && input.GrantDays != nil {
		return CreatedBatch{}, errors.New("lifetime batch cannot define grant days")
	}
	now := s.now().UTC()
	if input.RedeemBefore != nil && !input.RedeemBefore.After(now) {
		return CreatedBatch{}, errors.New("batch redemption deadline must be in the future")
	}

	issued, err := s.codec.GenerateBatch(input.Quantity)
	if err != nil {
		return CreatedBatch{}, err
	}

	result := CreatedBatch{Plaintext: make([]string, 0, len(issued))}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var plan membershipPlanRow
		if err := tx.Where("code = ? AND status = 'active'", input.PlanCode).First(&plan).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrMembershipPlanAbsent
			}
			return fmt.Errorf("load membership plan: %w", err)
		}

		batch := redemptionCodeBatchRow{
			ID:           uuid.New(),
			Name:         input.Name,
			PlanID:       plan.ID,
			GrantType:    string(input.GrantType),
			GrantDays:    input.GrantDays,
			Quantity:     input.Quantity,
			RedeemBefore: input.RedeemBefore,
			Status:       "active",
			Note:         input.Note,
			CreatedBy:    &input.CreatedBy,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := tx.Create(&batch).Error; err != nil {
			return fmt.Errorf("create redemption batch: %w", err)
		}

		rows := make([]redemptionCodeRow, 0, len(issued))
		for _, code := range issued {
			rows = append(rows, redemptionCodeRow{
				ID:         uuid.New(),
				BatchID:    batch.ID,
				CodeDigest: code.Digest,
				CodeHint:   code.Hint,
				Status:     "active",
				CreatedAt:  now,
			})
			result.Plaintext = append(result.Plaintext, code.Plaintext)
		}
		if err := tx.CreateInBatches(rows, 500).Error; err != nil {
			return fmt.Errorf("store redemption code digests: %w", err)
		}
		result.ID = batch.ID
		result.Batch = BatchSummary{
			ID: batch.ID, Name: batch.Name, PlanCode: input.PlanCode, GrantType: input.GrantType,
			GrantDays: input.GrantDays, Quantity: input.Quantity, Status: batch.Status,
			RedeemBefore: input.RedeemBefore, CreatedAt: now,
		}
		afterJSON, err := json.Marshal(result.Batch)
		if err != nil {
			return fmt.Errorf("serialize redemption batch audit state: %w", err)
		}
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &input.CreatedBy, Action: "redemption_batch.create",
			ResourceType: "redemption_code_batch", ResourceID: &batch.ID,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit redemption batch creation: %w", err)
		}
		return nil
	})
	if err != nil {
		return CreatedBatch{}, err
	}
	return result, nil
}

func (s *Store) Redeem(ctx context.Context, userID uuid.UUID, rawCode string) (RedemptionResult, error) {
	if userID == uuid.Nil {
		return RedemptionResult{}, ErrUserNotFound
	}
	_, digest, hint, err := s.codec.Inspect(rawCode)
	if err != nil {
		return RedemptionResult{}, ErrCodeUnavailable
	}
	now := s.now().UTC()
	var result RedemptionResult

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user userRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "email", "email_verified_at").
			First(&user, "id = ?", userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrUserNotFound
			}
			return fmt.Errorf("lock redemption user: %w", err)
		}
		if user.EmailVerifiedAt == nil {
			return ErrEmailNotVerified
		}

		var code redemptionCodeRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Preload("Batch.Plan").
			Where("code_digest = ?", digest).
			First(&code).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrCodeUnavailable
			}
			return fmt.Errorf("lock redemption code: %w", err)
		}
		if code.Status != "active" || code.Batch.Status != "active" ||
			(code.Batch.RedeemBefore != nil && !code.Batch.RedeemBefore.After(now)) {
			return ErrCodeUnavailable
		}

		isBetaCode := false
		var previousBetaRedemptions int64
		var promotionClaim betaPromotionClaimRow
		claimErr := tx.Table("promotion_claims AS claim").
			Select("claim.email, claim.campaign_code").
			Joins("JOIN promotion_campaigns AS campaign ON campaign.code = claim.campaign_code").
			Where("claim.code_id = ? AND campaign.redemption_policy = ?", code.ID, betaPromotionRedemptionPolicy).
			Take(&promotionClaim).Error
		switch {
		case claimErr == nil:
			isBetaCode = true
			if !redemptionEmailsMatch(user.Email, promotionClaim.Email) {
				return ErrCodeUnavailable
			}
			if err := tx.Table("promotion_claims AS claim").
				Joins("JOIN promotion_campaigns AS campaign ON campaign.code = claim.campaign_code").
				Joins("JOIN redemption_codes AS redeemed_code ON redeemed_code.id = claim.code_id").
				Where(
					"campaign.redemption_policy = ? AND lower(claim.email) = lower(?) AND redeemed_code.status = 'redeemed'",
					betaPromotionRedemptionPolicy,
					promotionClaim.Email,
				).
				Count(&previousBetaRedemptions).Error; err != nil {
				return fmt.Errorf("count prior beta code redemptions: %w", err)
			}
		case errors.Is(claimErr, gorm.ErrRecordNotFound):
			// Administrator-created redemption codes are not email-bound.
		default:
			return fmt.Errorf("load redemption code email binding: %w", claimErr)
		}

		isTrialCode := false
		var previousTrialRedemptions int64
		var trialClaim trialPromotionClaimRow
		trialClaimErr := tx.Table("trial_promotion_claims AS claim").
			Select("claim.email").
			Where("claim.code_id = ?", code.ID).
			Take(&trialClaim).Error
		switch {
		case trialClaimErr == nil:
			isTrialCode = true
			if !redemptionEmailsMatch(user.Email, trialClaim.Email) {
				return ErrCodeUnavailable
			}
			if err := tx.Table("trial_promotion_claims AS claim").
				Joins("JOIN redemption_codes AS redeemed_code ON redeemed_code.id = claim.code_id").
				Where("lower(claim.email) = lower(?) AND redeemed_code.status = 'redeemed'", trialClaim.Email).
				Count(&previousTrialRedemptions).Error; err != nil {
				return fmt.Errorf("count prior trial code redemptions: %w", err)
			}
		case errors.Is(trialClaimErr, gorm.ErrRecordNotFound):
			// Administrator-created and beta redemption codes are not trial-email-bound.
		default:
			return fmt.Errorf("load trial redemption code email binding: %w", trialClaimErr)
		}

		var currentRow membershipRow
		var current *Membership
		loadErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Preload("Plan").
			Where("user_id = ?", userID).
			First(&currentRow).Error
		switch {
		case loadErr == nil:
			current = &Membership{
				PlanCode:  currentRow.Plan.Code,
				PlanRank:  currentRow.Plan.Rank,
				StartsAt:  currentRow.StartsAt,
				ExpiresAt: currentRow.ExpiresAt,
			}
		case errors.Is(loadErr, gorm.ErrRecordNotFound):
			current = nil
		default:
			return fmt.Errorf("load current membership: %w", loadErr)
		}
		if err := validateBetaRedemptionEligibility(isBetaCode, previousBetaRedemptions, current); err != nil {
			return err
		}
		if err := validateTrialRedemptionEligibility(isTrialCode, previousTrialRedemptions, current); err != nil {
			return err
		}

		grant := Grant{
			PlanCode: code.Batch.Plan.Code,
			PlanRank: code.Batch.Plan.Rank,
			Type:     GrantType(code.Batch.GrantType),
		}
		if code.Batch.GrantDays != nil {
			grant.Days = *code.Batch.GrantDays
		}
		next, err := ApplyGrant(now, current, grant)
		if err != nil {
			return err
		}

		beforeJSON, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("serialize membership before state: %w", err)
		}
		afterJSON, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("serialize membership after state: %w", err)
		}

		if current == nil {
			currentRow = membershipRow{
				UserID:    userID,
				PlanID:    code.Batch.Plan.ID,
				StartsAt:  next.StartsAt,
				ExpiresAt: next.ExpiresAt,
				Source:    "redemption_code",
				Status:    "active",
				Version:   1,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if err := tx.Create(&currentRow).Error; err != nil {
				return fmt.Errorf("create membership: %w", err)
			}
		} else {
			updates := map[string]any{
				"plan_id":    code.Batch.Plan.ID,
				"starts_at":  next.StartsAt,
				"expires_at": next.ExpiresAt,
				"source":     "redemption_code",
				"status":     "active",
				"version":    gorm.Expr("version + 1"),
				"updated_at": now,
			}
			if err := tx.Model(&membershipRow{}).Where("user_id = ?", userID).Updates(updates).Error; err != nil {
				return fmt.Errorf("update membership: %w", err)
			}
		}

		codeUpdate := tx.Model(&redemptionCodeRow{}).
			Where("id = ? AND status = 'active'", code.ID).
			Updates(map[string]any{
				"status":      "redeemed",
				"redeemed_by": userID,
				"redeemed_at": now,
			})
		if codeUpdate.Error != nil {
			return fmt.Errorf("mark code redeemed: %w", codeUpdate.Error)
		}
		if codeUpdate.RowsAffected != 1 {
			return ErrCodeUnavailable
		}

		event := membershipEventRow{
			ID:          uuid.New(),
			UserID:      userID,
			EventType:   "redemption",
			SourceType:  "redemption_code",
			SourceID:    &code.ID,
			BeforeJSON:  beforeJSON,
			AfterJSON:   afterJSON,
			ActorUserID: &userID,
			CreatedAt:   now,
		}
		if err := tx.Create(&event).Error; err != nil {
			return fmt.Errorf("create membership event: %w", err)
		}

		var activeCodes int64
		if err := tx.Model(&redemptionCodeRow{}).
			Where("batch_id = ? AND status = 'active'", code.BatchID).
			Count(&activeCodes).Error; err != nil {
			return fmt.Errorf("count remaining redemption codes: %w", err)
		}
		if activeCodes == 0 {
			hasQuota, err := hasAvailablePromotionQuota(tx, code.BatchID)
			if err != nil {
				return err
			}
			if !hasQuota {
				if err := tx.Model(&redemptionCodeBatchRow{}).
					Where("id = ? AND status = 'active'", code.BatchID).
					Updates(map[string]any{"status": "exhausted", "updated_at": now}).Error; err != nil {
					return fmt.Errorf("mark redemption batch exhausted: %w", err)
				}
			}
		}

		result = RedemptionResult{
			Membership: next,
			CodeHint:   hint,
			RedeemedAt: now,
			EventID:    event.ID,
		}
		return nil
	})
	if err != nil {
		return RedemptionResult{}, err
	}
	return result, nil
}

type userRow struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Email           string     `gorm:"column:email"`
	EmailVerifiedAt *time.Time `gorm:"column:email_verified_at"`
}

func (userRow) TableName() string { return "users" }

func redemptionEmailsMatch(accountEmail, claimedEmail string) bool {
	return strings.EqualFold(strings.TrimSpace(accountEmail), strings.TrimSpace(claimedEmail))
}

func validateBetaRedemptionEligibility(isBetaCode bool, previousRedemptions int64, current *Membership) error {
	if !isBetaCode {
		return nil
	}
	if previousRedemptions > 0 {
		return ErrCodeUnavailable
	}
	if current != nil && current.ExpiresAt == nil {
		return ErrMembershipNotExtended
	}
	return nil
}

func validateTrialRedemptionEligibility(isTrialCode bool, previousRedemptions int64, current *Membership) error {
	if !isTrialCode {
		return nil
	}
	if previousRedemptions > 0 {
		return ErrCodeUnavailable
	}
	if current != nil && current.ExpiresAt == nil {
		return ErrMembershipNotExtended
	}
	return nil
}

type membershipPlanRow struct {
	ID     uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	Code   string    `gorm:"column:code"`
	Name   string    `gorm:"column:name"`
	Rank   int       `gorm:"column:rank"`
	Status string    `gorm:"column:status"`
}

func (membershipPlanRow) TableName() string { return "membership_plans" }

type membershipRow struct {
	UserID    uuid.UUID         `gorm:"column:user_id;type:uuid;primaryKey"`
	PlanID    uuid.UUID         `gorm:"column:plan_id;type:uuid"`
	Plan      membershipPlanRow `gorm:"foreignKey:PlanID"`
	StartsAt  time.Time         `gorm:"column:starts_at"`
	ExpiresAt *time.Time        `gorm:"column:expires_at"`
	Source    string            `gorm:"column:source"`
	Status    string            `gorm:"column:status"`
	Version   int64             `gorm:"column:version"`
	CreatedAt time.Time         `gorm:"column:created_at"`
	UpdatedAt time.Time         `gorm:"column:updated_at"`
}

func (membershipRow) TableName() string { return "memberships" }

type redemptionCodeBatchRow struct {
	ID           uuid.UUID         `gorm:"column:id;type:uuid;primaryKey"`
	Name         string            `gorm:"column:name"`
	PlanID       uuid.UUID         `gorm:"column:plan_id;type:uuid"`
	Plan         membershipPlanRow `gorm:"foreignKey:PlanID"`
	GrantType    string            `gorm:"column:grant_type"`
	GrantDays    *int              `gorm:"column:grant_days"`
	Quantity     int               `gorm:"column:quantity"`
	RedeemBefore *time.Time        `gorm:"column:redeem_before"`
	Status       string            `gorm:"column:status"`
	Note         string            `gorm:"column:note"`
	CreatedBy    *uuid.UUID        `gorm:"column:created_by;type:uuid"`
	CreatedAt    time.Time         `gorm:"column:created_at"`
	UpdatedAt    time.Time         `gorm:"column:updated_at"`
}

func (redemptionCodeBatchRow) TableName() string { return "redemption_code_batches" }

type redemptionCodeRow struct {
	ID         uuid.UUID              `gorm:"column:id;type:uuid;primaryKey"`
	BatchID    uuid.UUID              `gorm:"column:batch_id;type:uuid"`
	Batch      redemptionCodeBatchRow `gorm:"foreignKey:BatchID"`
	CodeDigest string                 `gorm:"column:code_digest"`
	CodeHint   string                 `gorm:"column:code_hint"`
	Status     string                 `gorm:"column:status"`
	RedeemedBy *uuid.UUID             `gorm:"column:redeemed_by;type:uuid"`
	RedeemedAt *time.Time             `gorm:"column:redeemed_at"`
	CreatedAt  time.Time              `gorm:"column:created_at"`
}

func (redemptionCodeRow) TableName() string { return "redemption_codes" }

type membershipEventRow struct {
	ID          uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID      uuid.UUID  `gorm:"column:user_id;type:uuid"`
	EventType   string     `gorm:"column:event_type"`
	SourceType  string     `gorm:"column:source_type"`
	SourceID    *uuid.UUID `gorm:"column:source_id;type:uuid"`
	BeforeJSON  []byte     `gorm:"column:before_json;type:jsonb"`
	AfterJSON   []byte     `gorm:"column:after_json;type:jsonb"`
	Reason      string     `gorm:"column:reason"`
	ActorUserID *uuid.UUID `gorm:"column:actor_user_id;type:uuid"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
}

func (membershipEventRow) TableName() string { return "membership_events" }
