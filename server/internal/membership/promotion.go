package membership

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	betaPromotionCampaignCode     = "beta-pro-launch"
	betaPromotionIPClaimLimit     = int64(3)
	betaPromotionPendingRetryAge  = 2 * time.Minute
	betaPromotionCodeAAD          = "wenzwork:beta-promotion-code:v1"
	betaPromotionMaxQuota         = 5000
	betaPromotionRedemptionPolicy = "beta_email_once"
	betaPromotionMaxQRDimension   = 4096

	BetaPromotionGroupQRCodeMaxBytes = 2 * 1024 * 1024
)

var (
	ErrBetaPromotionInvalidEmail             = errors.New("beta promotion email is invalid")
	ErrBetaPromotionExhausted                = errors.New("beta promotion is exhausted")
	ErrBetaPromotionRateLimit                = errors.New("beta promotion claim rate limit exceeded")
	ErrBetaPromotionDelivery                 = errors.New("beta promotion email delivery failed")
	ErrBetaPromotionAdminInvalid             = errors.New("beta promotion admin input is invalid")
	ErrBetaPromotionBatchRevoked             = errors.New("beta promotion redemption batch is revoked")
	ErrBetaPromotionGroupQRCodeInvalid       = errors.New("beta promotion group QR code is invalid")
	ErrBetaPromotionGroupQRCodeNotConfigured = errors.New("beta promotion group QR code is not configured")
)

type BetaPromotionStatus struct {
	Limit     int  `json:"limit"`
	Claimed   int  `json:"claimed"`
	Remaining int  `json:"remaining"`
	Available bool `json:"available"`
}

type BetaPromotionClaimResult struct {
	Promotion      BetaPromotionStatus `json:"promotion"`
	DeliveryStatus string              `json:"deliveryStatus"`
	AlreadyClaimed bool                `json:"alreadyClaimed"`
	GroupQRCodeURL *string             `json:"groupQRCodeUrl"`
	NewClaim       bool                `json:"-"`
}

type BetaPromotionGroupQRCode struct {
	Content     []byte
	ContentType string
	UpdatedAt   time.Time
}

type BetaPromotionAdminOverview struct {
	Code                  string     `json:"code"`
	Status                string     `json:"status"`
	Limit                 int        `json:"limit"`
	Claimed               int        `json:"claimed"`
	Remaining             int        `json:"remaining"`
	Available             bool       `json:"available"`
	PendingDeliveryCount  int64      `json:"pendingDeliveryCount"`
	SentDeliveryCount     int64      `json:"sentDeliveryCount"`
	FailedDeliveryCount   int64      `json:"failedDeliveryCount"`
	ActiveCodeCount       int64      `json:"activeCodeCount"`
	RedeemedCodeCount     int64      `json:"redeemedCodeCount"`
	RevokedCodeCount      int64      `json:"revokedCodeCount"`
	GroupQRCodeConfigured bool       `json:"groupQRCodeConfigured"`
	GroupQRCodeURL        *string    `json:"groupQRCodeUrl"`
	GroupQRCodeUpdatedAt  *time.Time `json:"groupQRCodeUpdatedAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

type BetaPromotionClaimFilter struct {
	Query            string
	DeliveryStatus   string
	RedemptionStatus string
	Limit            int
	Offset           int
}

type BetaPromotionAdminClaim struct {
	ID                    uuid.UUID  `json:"id"`
	Email                 string     `json:"email"`
	CodeHint              string     `json:"codeHint"`
	DeliveryStatus        string     `json:"deliveryStatus"`
	RedemptionStatus      string     `json:"redemptionStatus"`
	DeliveryAttempts      int        `json:"deliveryAttempts"`
	LastDeliveryAttemptAt time.Time  `json:"lastDeliveryAttemptAt"`
	SentAt                *time.Time `json:"sentAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	RedeemedAt            *time.Time `json:"redeemedAt"`
}

type BetaPromotionAdminClaimList struct {
	Items  []BetaPromotionAdminClaim `json:"items"`
	Total  int64                     `json:"total"`
	Limit  int                       `json:"limit"`
	Offset int                       `json:"offset"`
}

type BetaPromotionService struct {
	db           *gorm.DB
	codec        *CodeCodec
	sender       mailer.Sender
	cipher       *redemptionCodeCipher
	campaignCode string
	now          func() time.Time
}

func NewBetaPromotionService(db *gorm.DB, codec *CodeCodec, sender mailer.Sender, encryptionKey string) (*BetaPromotionService, error) {
	if db == nil {
		return nil, errors.New("beta promotion database is required")
	}
	if codec == nil {
		return nil, errors.New("beta promotion code codec is required")
	}
	if sender == nil {
		return nil, errors.New("beta promotion mail sender is required")
	}
	codeCipher, err := newRedemptionCodeCipher(encryptionKey, betaPromotionCodeAAD)
	if err != nil {
		return nil, fmt.Errorf("create beta promotion code cipher: %w", err)
	}

	return &BetaPromotionService{
		db: db, codec: codec, sender: sender, cipher: codeCipher,
		campaignCode: betaPromotionCampaignCode, now: time.Now,
	}, nil
}

func (s *BetaPromotionService) Status(ctx context.Context) (BetaPromotionStatus, error) {
	var campaign betaPromotionCampaignRow
	if err := s.db.WithContext(ctx).First(&campaign, "code = ?", s.campaignCode).Error; err != nil {
		return BetaPromotionStatus{}, fmt.Errorf("load beta promotion campaign: %w", err)
	}
	return betaPromotionStatusFromRow(campaign), nil
}

func (s *BetaPromotionService) GroupQRCode(ctx context.Context) (BetaPromotionGroupQRCode, error) {
	var campaign betaPromotionCampaignRow
	if err := s.db.WithContext(ctx).
		Select("group_qr_code", "group_qr_content_type", "group_qr_updated_at").
		First(&campaign, "code = ?", s.campaignCode).Error; err != nil {
		return BetaPromotionGroupQRCode{}, fmt.Errorf("load beta promotion group QR code: %w", err)
	}
	if len(campaign.GroupQRCode) == 0 || campaign.GroupQRCodeContentType == nil ||
		campaign.GroupQRCodeUpdatedAt == nil {
		return BetaPromotionGroupQRCode{}, ErrBetaPromotionGroupQRCodeNotConfigured
	}
	return BetaPromotionGroupQRCode{
		Content:     campaign.GroupQRCode,
		ContentType: *campaign.GroupQRCodeContentType,
		UpdatedAt:   campaign.GroupQRCodeUpdatedAt.UTC(),
	}, nil
}

func (s *BetaPromotionService) AdminOverview(ctx context.Context) (BetaPromotionAdminOverview, error) {
	return s.loadAdminOverview(s.db.WithContext(ctx))
}

func (s *BetaPromotionService) ListAdminClaims(ctx context.Context, filter BetaPromotionClaimFilter) (BetaPromotionAdminClaimList, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	filter.DeliveryStatus = strings.TrimSpace(filter.DeliveryStatus)
	filter.RedemptionStatus = strings.TrimSpace(filter.RedemptionStatus)
	if len(filter.Query) > 320 || !promotionDeliveryStatusValid(filter.DeliveryStatus) ||
		!promotionRedemptionStatusValid(filter.RedemptionStatus) || filter.Limit < 1 ||
		filter.Limit > 100 || filter.Offset < 0 || filter.Offset > 100000 {
		return BetaPromotionAdminClaimList{}, ErrBetaPromotionAdminInvalid
	}

	query := s.db.WithContext(ctx).Table("promotion_claims AS claim").
		Joins("JOIN redemption_codes AS code ON code.id = claim.code_id").
		Where("claim.campaign_code = ?", s.campaignCode)
	if filter.Query != "" {
		pattern := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(filter.Query))
		query = query.Where("lower(claim.email) LIKE ? ESCAPE '!'", "%"+pattern+"%")
	}
	if filter.DeliveryStatus != "" {
		query = query.Where("claim.delivery_status = ?", filter.DeliveryStatus)
	}
	if filter.RedemptionStatus != "" {
		query = query.Where("code.status = ?", filter.RedemptionStatus)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return BetaPromotionAdminClaimList{}, fmt.Errorf("count beta promotion claims: %w", err)
	}
	items := make([]BetaPromotionAdminClaim, 0)
	if err := query.Select(`
		claim.id,
		claim.email,
		code.code_hint,
		claim.delivery_status,
		code.status AS redemption_status,
		claim.delivery_attempts,
		claim.last_delivery_attempt_at,
		claim.sent_at,
		claim.created_at,
		code.redeemed_at`).
		Order("claim.created_at DESC, claim.id DESC").
		Limit(filter.Limit).Offset(filter.Offset).Scan(&items).Error; err != nil {
		return BetaPromotionAdminClaimList{}, fmt.Errorf("list beta promotion claims: %w", err)
	}
	return BetaPromotionAdminClaimList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *BetaPromotionService) UpdateAdminRemaining(ctx context.Context, actorUserID uuid.UUID, remaining int) (BetaPromotionAdminOverview, error) {
	if actorUserID == uuid.Nil || remaining < 0 || remaining > betaPromotionMaxQuota {
		return BetaPromotionAdminOverview{}, ErrBetaPromotionAdminInvalid
	}
	now := s.now().UTC()
	var result BetaPromotionAdminOverview
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var campaign betaPromotionCampaignRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&campaign, "code = ?", s.campaignCode).Error; err != nil {
			return fmt.Errorf("lock beta promotion campaign for admin update: %w", err)
		}
		if campaign.ClaimedCount+remaining > betaPromotionMaxQuota {
			return ErrBetaPromotionAdminInvalid
		}

		var batch redemptionCodeBatchRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&batch, "id = ?", campaign.BatchID).Error; err != nil {
			return fmt.Errorf("lock beta promotion redemption batch: %w", err)
		}
		if remaining > 0 && batch.Status == "revoked" {
			return ErrBetaPromotionBatchRevoked
		}

		before := map[string]any{
			"status": campaign.Status, "quota": campaign.Quota,
			"claimed": campaign.ClaimedCount, "remaining": betaPromotionStatusFromRow(campaign).Remaining,
		}
		campaign.Quota = campaign.ClaimedCount + remaining
		if remaining == 0 {
			campaign.Status = "disabled"
		} else {
			campaign.Status = "active"
		}
		campaign.UpdatedAt = now
		if err := tx.Model(&betaPromotionCampaignRow{}).Where("code = ?", campaign.Code).Updates(map[string]any{
			"quota": campaign.Quota, "status": campaign.Status, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("update beta promotion remaining quota: %w", err)
		}
		if remaining > 0 && batch.Status == "exhausted" {
			if err := tx.Model(&redemptionCodeBatchRow{}).Where("id = ? AND status = 'exhausted'", batch.ID).
				Updates(map[string]any{"status": "active", "updated_at": now}).Error; err != nil {
				return fmt.Errorf("reactivate beta promotion redemption batch: %w", err)
			}
		}

		after := map[string]any{
			"status": campaign.Status, "quota": campaign.Quota,
			"claimed": campaign.ClaimedCount, "remaining": remaining,
		}
		beforeJSON, _ := json.Marshal(before)
		afterJSON, _ := json.Marshal(after)
		resourceID := campaign.BatchID
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "beta_promotion.remaining.update",
			ResourceType: "promotion_campaign", ResourceID: &resourceID,
			BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit beta promotion remaining update: %w", err)
		}

		var err error
		result, err = s.loadAdminOverview(tx)
		return err
	})
	if err != nil {
		return BetaPromotionAdminOverview{}, err
	}
	return result, nil
}

func (s *BetaPromotionService) UpdateAdminGroupQRCode(
	ctx context.Context,
	actorUserID uuid.UUID,
	contentType string,
	content []byte,
) (BetaPromotionAdminOverview, error) {
	if actorUserID == uuid.Nil {
		return BetaPromotionAdminOverview{}, ErrBetaPromotionAdminInvalid
	}
	normalizedContentType, err := validateBetaPromotionGroupQRCode(contentType, content)
	if err != nil {
		return BetaPromotionAdminOverview{}, err
	}

	now := s.now().UTC()
	var result BetaPromotionAdminOverview
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var campaign betaPromotionCampaignRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&campaign, "code = ?", s.campaignCode).Error; err != nil {
			return fmt.Errorf("lock beta promotion campaign for group QR code update: %w", err)
		}

		before := betaPromotionGroupQRCodeAuditState(campaign)
		if err := tx.Model(&betaPromotionCampaignRow{}).Where("code = ?", campaign.Code).
			Updates(map[string]any{
				"group_qr_code":         append([]byte(nil), content...),
				"group_qr_content_type": normalizedContentType,
				"group_qr_updated_at":   now,
				"updated_at":            now,
			}).Error; err != nil {
			return fmt.Errorf("update beta promotion group QR code: %w", err)
		}
		campaign.GroupQRCode = content
		campaign.GroupQRCodeContentType = &normalizedContentType
		campaign.GroupQRCodeUpdatedAt = &now
		campaign.UpdatedAt = now

		if err := createBetaPromotionGroupQRCodeAudit(
			tx,
			actorUserID,
			campaign.BatchID,
			"beta_promotion.group_qr.update",
			before,
			betaPromotionGroupQRCodeAuditState(campaign),
			now,
		); err != nil {
			return err
		}

		var err error
		result, err = s.loadAdminOverview(tx)
		return err
	})
	if err != nil {
		return BetaPromotionAdminOverview{}, err
	}
	return result, nil
}

func (s *BetaPromotionService) RemoveAdminGroupQRCode(
	ctx context.Context,
	actorUserID uuid.UUID,
) (BetaPromotionAdminOverview, error) {
	if actorUserID == uuid.Nil {
		return BetaPromotionAdminOverview{}, ErrBetaPromotionAdminInvalid
	}

	now := s.now().UTC()
	var result BetaPromotionAdminOverview
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var campaign betaPromotionCampaignRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&campaign, "code = ?", s.campaignCode).Error; err != nil {
			return fmt.Errorf("lock beta promotion campaign for group QR code removal: %w", err)
		}

		if len(campaign.GroupQRCode) > 0 || campaign.GroupQRCodeContentType != nil ||
			campaign.GroupQRCodeUpdatedAt != nil {
			before := betaPromotionGroupQRCodeAuditState(campaign)
			if err := tx.Model(&betaPromotionCampaignRow{}).Where("code = ?", campaign.Code).
				Updates(map[string]any{
					"group_qr_code":         nil,
					"group_qr_content_type": nil,
					"group_qr_updated_at":   nil,
					"updated_at":            now,
				}).Error; err != nil {
				return fmt.Errorf("remove beta promotion group QR code: %w", err)
			}
			campaign.GroupQRCode = nil
			campaign.GroupQRCodeContentType = nil
			campaign.GroupQRCodeUpdatedAt = nil
			campaign.UpdatedAt = now

			if err := createBetaPromotionGroupQRCodeAudit(
				tx,
				actorUserID,
				campaign.BatchID,
				"beta_promotion.group_qr.remove",
				before,
				betaPromotionGroupQRCodeAuditState(campaign),
				now,
			); err != nil {
				return err
			}
		}

		var err error
		result, err = s.loadAdminOverview(tx)
		return err
	})
	if err != nil {
		return BetaPromotionAdminOverview{}, err
	}
	return result, nil
}

func (s *BetaPromotionService) loadAdminOverview(db *gorm.DB) (BetaPromotionAdminOverview, error) {
	var campaign betaPromotionCampaignRow
	if err := db.First(&campaign, "code = ?", s.campaignCode).Error; err != nil {
		return BetaPromotionAdminOverview{}, fmt.Errorf("load beta promotion campaign overview: %w", err)
	}
	var counts struct {
		PendingDeliveryCount int64
		SentDeliveryCount    int64
		FailedDeliveryCount  int64
		ActiveCodeCount      int64
		RedeemedCodeCount    int64
		RevokedCodeCount     int64
	}
	if err := db.Table("promotion_claims AS claim").
		Joins("JOIN redemption_codes AS code ON code.id = claim.code_id").
		Where("claim.campaign_code = ?", s.campaignCode).
		Select(`
			COUNT(*) FILTER (WHERE claim.delivery_status = 'pending') AS pending_delivery_count,
			COUNT(*) FILTER (WHERE claim.delivery_status = 'sent') AS sent_delivery_count,
			COUNT(*) FILTER (WHERE claim.delivery_status = 'failed') AS failed_delivery_count,
			COUNT(*) FILTER (WHERE code.status = 'active') AS active_code_count,
			COUNT(*) FILTER (WHERE code.status = 'redeemed') AS redeemed_code_count,
			COUNT(*) FILTER (WHERE code.status = 'revoked') AS revoked_code_count`).Scan(&counts).Error; err != nil {
		return BetaPromotionAdminOverview{}, fmt.Errorf("count beta promotion claim states: %w", err)
	}
	status := betaPromotionStatusFromRow(campaign)
	groupQRCodeURL := betaPromotionGroupQRCodeURL(campaign)
	return BetaPromotionAdminOverview{
		Code: campaign.Code, Status: campaign.Status, Limit: status.Limit, Claimed: status.Claimed,
		Remaining: status.Remaining, Available: status.Available,
		PendingDeliveryCount: counts.PendingDeliveryCount, SentDeliveryCount: counts.SentDeliveryCount,
		FailedDeliveryCount: counts.FailedDeliveryCount, ActiveCodeCount: counts.ActiveCodeCount,
		RedeemedCodeCount: counts.RedeemedCodeCount, RevokedCodeCount: counts.RevokedCodeCount,
		GroupQRCodeConfigured: groupQRCodeURL != nil, GroupQRCodeURL: groupQRCodeURL,
		GroupQRCodeUpdatedAt: campaign.GroupQRCodeUpdatedAt, UpdatedAt: campaign.UpdatedAt,
	}, nil
}

func validateBetaPromotionGroupQRCode(contentType string, content []byte) (string, error) {
	normalizedContentType := strings.ToLower(strings.TrimSpace(contentType))
	if normalizedContentType != "image/png" && normalizedContentType != "image/jpeg" {
		return "", ErrBetaPromotionGroupQRCodeInvalid
	}
	if len(content) == 0 || len(content) > BetaPromotionGroupQRCodeMaxBytes {
		return "", ErrBetaPromotionGroupQRCodeInvalid
	}

	config, format, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || config.Width < 1 || config.Height < 1 ||
		config.Width > betaPromotionMaxQRDimension || config.Height > betaPromotionMaxQRDimension {
		return "", ErrBetaPromotionGroupQRCodeInvalid
	}
	if (format == "png" && normalizedContentType != "image/png") ||
		(format == "jpeg" && normalizedContentType != "image/jpeg") ||
		(format != "png" && format != "jpeg") {
		return "", ErrBetaPromotionGroupQRCodeInvalid
	}
	return normalizedContentType, nil
}

func betaPromotionGroupQRCodeURL(campaign betaPromotionCampaignRow) *string {
	if len(campaign.GroupQRCode) == 0 || campaign.GroupQRCodeContentType == nil ||
		campaign.GroupQRCodeUpdatedAt == nil {
		return nil
	}
	value := fmt.Sprintf(
		"/api/v1/promotions/beta-pro/group-qr?v=%d",
		campaign.GroupQRCodeUpdatedAt.UTC().UnixNano(),
	)
	return &value
}

func betaPromotionGroupQRCodeAuditState(campaign betaPromotionCampaignRow) map[string]any {
	state := map[string]any{"configured": false}
	if len(campaign.GroupQRCode) == 0 || campaign.GroupQRCodeContentType == nil ||
		campaign.GroupQRCodeUpdatedAt == nil {
		return state
	}
	state["configured"] = true
	state["contentType"] = *campaign.GroupQRCodeContentType
	state["byteSize"] = len(campaign.GroupQRCode)
	state["updatedAt"] = campaign.GroupQRCodeUpdatedAt.UTC()
	return state
}

func createBetaPromotionGroupQRCodeAudit(
	tx *gorm.DB,
	actorUserID uuid.UUID,
	resourceID uuid.UUID,
	action string,
	before map[string]any,
	after map[string]any,
	createdAt time.Time,
) error {
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if err := tx.Create(&auditLogRow{
		ID: uuid.New(), ActorUserID: &actorUserID, Action: action,
		ResourceType: "promotion_campaign", ResourceID: &resourceID,
		BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: createdAt,
	}).Error; err != nil {
		return fmt.Errorf("audit beta promotion group QR code update: %w", err)
	}
	return nil
}

func promotionDeliveryStatusValid(status string) bool {
	return status == "" || status == "pending" || status == "sent" || status == "failed"
}

func promotionRedemptionStatusValid(status string) bool {
	return status == "" || status == "active" || status == "redeemed" || status == "revoked"
}

func hasAvailablePromotionQuota(tx *gorm.DB, batchID uuid.UUID) (bool, error) {
	var count int64
	if err := tx.Model(&betaPromotionCampaignRow{}).
		Where("batch_id = ? AND status = 'active' AND claimed_count < quota", batchID).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("check promotion quota for redemption batch: %w", err)
	}
	if count > 0 {
		return true, nil
	}
	if err := tx.Model(&trialPromotionSettingsRow{}).
		Where("batch_id = ? AND enabled = true", batchID).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("check trial promotion availability for redemption batch: %w", err)
	}
	return count > 0, nil
}

func (s *BetaPromotionService) Claim(ctx context.Context, rawEmail, clientIP string) (BetaPromotionClaimResult, error) {
	email, err := normalizePromotionEmail(rawEmail)
	if err != nil {
		return BetaPromotionClaimResult{}, err
	}
	now := s.now().UTC()
	ipDigest := s.codec.Digest(s.campaignCode + ":ip:" + strings.TrimSpace(clientIP))

	var result BetaPromotionClaimResult
	var claim betaPromotionClaimRow
	var plaintext string
	shouldSend := false

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var campaign betaPromotionCampaignRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&campaign, "code = ?", s.campaignCode).Error; err != nil {
			return fmt.Errorf("lock beta promotion campaign: %w", err)
		}
		if campaign.RedemptionPolicy != betaPromotionRedemptionPolicy {
			return errors.New("beta promotion redemption policy is not configured")
		}

		existingErr := tx.First(&claim, "campaign_code = ? AND email = ?", s.campaignCode, email).Error
		switch {
		case existingErr == nil:
			result = BetaPromotionClaimResult{
				Promotion: betaPromotionStatusFromRow(campaign), DeliveryStatus: claim.DeliveryStatus,
				AlreadyClaimed: true, GroupQRCodeURL: betaPromotionGroupQRCodeURL(campaign),
			}
			if claim.DeliveryStatus == "sent" {
				return nil
			}
			if claim.DeliveryStatus == "pending" && claim.LastDeliveryAttemptAt.After(now.Add(-betaPromotionPendingRetryAge)) {
				return nil
			}
			decrypted, err := s.decryptCode(claim.ID, claim.CodeCiphertext)
			if err != nil {
				return fmt.Errorf("decrypt pending beta promotion code: %w", err)
			}
			plaintext = string(decrypted)
			shouldSend = true
			claim.DeliveryStatus = "pending"
			claim.DeliveryAttempts++
			claim.LastDeliveryAttemptAt = now
			claim.UpdatedAt = now
			if err := tx.Model(&betaPromotionClaimRow{}).Where("id = ?", claim.ID).Updates(map[string]any{
				"delivery_status": "pending", "delivery_attempts": claim.DeliveryAttempts,
				"last_delivery_attempt_at": now, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("schedule beta promotion email retry: %w", err)
			}
			result.DeliveryStatus = "pending"
			return nil
		case !errors.Is(existingErr, gorm.ErrRecordNotFound):
			return fmt.Errorf("load beta promotion claim: %w", existingErr)
		}

		if campaign.Status != "active" || campaign.ClaimedCount >= campaign.Quota {
			return ErrBetaPromotionExhausted
		}

		var recentClaims int64
		if err := tx.Model(&betaPromotionClaimRow{}).
			Where("campaign_code = ? AND client_ip_digest = ? AND created_at >= ?", s.campaignCode, ipDigest, now.Add(-24*time.Hour)).
			Count(&recentClaims).Error; err != nil {
			return fmt.Errorf("count beta promotion IP claims: %w", err)
		}
		if recentClaims >= betaPromotionIPClaimLimit {
			return ErrBetaPromotionRateLimit
		}
		var batch redemptionCodeBatchRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&batch, "id = ?", campaign.BatchID).Error; err != nil {
			return fmt.Errorf("lock beta promotion redemption batch: %w", err)
		}
		if batch.Status == "revoked" {
			return ErrBetaPromotionExhausted
		}
		if batch.Status == "exhausted" {
			if err := tx.Model(&redemptionCodeBatchRow{}).Where("id = ? AND status = 'exhausted'", batch.ID).
				Updates(map[string]any{"status": "active", "updated_at": now}).Error; err != nil {
				return fmt.Errorf("reactivate beta promotion redemption batch: %w", err)
			}
		}

		issued, err := s.codec.Generate()
		if err != nil {
			return fmt.Errorf("generate beta promotion code: %w", err)
		}
		claimID := uuid.New()
		ciphertext, err := s.encryptCode(claimID, []byte(issued.Plaintext))
		if err != nil {
			return err
		}
		codeID := uuid.New()
		if err := tx.Create(&redemptionCodeRow{
			ID: codeID, BatchID: campaign.BatchID, CodeDigest: issued.Digest,
			CodeHint: issued.Hint, Status: "active", CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("store beta promotion code digest: %w", err)
		}
		claim = betaPromotionClaimRow{
			ID: claimID, CampaignCode: s.campaignCode, Email: email, CodeID: codeID,
			CodeCiphertext: ciphertext, ClientIPDigest: ipDigest, DeliveryStatus: "pending",
			DeliveryAttempts: 1, LastDeliveryAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&claim).Error; err != nil {
			return fmt.Errorf("create beta promotion claim: %w", err)
		}

		campaign.ClaimedCount++
		if campaign.ClaimedCount >= campaign.Quota {
			campaign.Status = "exhausted"
		}
		campaign.UpdatedAt = now
		if err := tx.Model(&betaPromotionCampaignRow{}).Where("code = ?", campaign.Code).Updates(map[string]any{
			"claimed_count": campaign.ClaimedCount, "status": campaign.Status, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("reserve beta promotion quota: %w", err)
		}

		plaintext = issued.Plaintext
		shouldSend = true
		result = BetaPromotionClaimResult{
			Promotion: betaPromotionStatusFromRow(campaign), DeliveryStatus: "pending",
			GroupQRCodeURL: betaPromotionGroupQRCodeURL(campaign), NewClaim: true,
		}
		return nil
	})
	if err != nil {
		return BetaPromotionClaimResult{}, err
	}
	if !shouldSend {
		return result, nil
	}

	if err := s.sender.Send(ctx, betaPromotionMessage(email, plaintext)); err != nil {
		updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if updateErr := s.db.WithContext(updateCtx).Model(&betaPromotionClaimRow{}).Where("id = ? AND delivery_status = 'pending'", claim.ID).
			Updates(map[string]any{"delivery_status": "failed", "updated_at": s.now().UTC()}).Error; updateErr != nil {
			return result, fmt.Errorf("%w: send failed and delivery state update failed: %v", ErrBetaPromotionDelivery, updateErr)
		}
		return result, fmt.Errorf("%w: %v", ErrBetaPromotionDelivery, err)
	}

	sentAt := s.now().UTC()
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.db.WithContext(updateCtx).Model(&betaPromotionClaimRow{}).Where("id = ? AND delivery_status = 'pending'", claim.ID).
		Updates(map[string]any{
			"delivery_status": "sent", "sent_at": sentAt, "code_ciphertext": nil, "updated_at": sentAt,
		}).Error; err != nil {
		return result, fmt.Errorf("record beta promotion email delivery: %w", err)
	}
	result.DeliveryStatus = "sent"
	return result, nil
}

func normalizePromotionEmail(raw string) (string, error) {
	email, err := normalizeRecipientEmail(raw)
	if err != nil {
		return "", ErrBetaPromotionInvalidEmail
	}
	return email, nil
}

func betaPromotionStatusFromRow(row betaPromotionCampaignRow) BetaPromotionStatus {
	remaining := row.Quota - row.ClaimedCount
	if remaining < 0 {
		remaining = 0
	}
	return BetaPromotionStatus{
		Limit: row.Quota, Claimed: row.ClaimedCount, Remaining: remaining,
		Available: row.Status == "active" && remaining > 0,
	}
}

func betaPromotionMessage(email, code string) mailer.Message {
	return mailer.Message{
		To:      email,
		Subject: "WenzWork 内测赠送：1 年 Pro 会员兑换码",
		Text: "你好：\n\n感谢加入 WenzWork 内测。\n\n你的 1 年 Pro 会员兑换码：\n" + code +
			"\n\n此兑换码已与 " + email + " 绑定，只能由使用该邮箱注册且已验证的 WenzWork 账号兑换。" +
			"兑换码仅限使用一次，请妥善保管。" +
			"\n\n加入内测群或联系我们：\n微信：lyming555\nQQ：44185539" +
			"\n\nWenzWork 团队\n",
	}
}

func (s *BetaPromotionService) encryptCode(claimID uuid.UUID, plaintext []byte) ([]byte, error) {
	return s.cipher.Encrypt(claimID, plaintext)
}

func (s *BetaPromotionService) decryptCode(claimID uuid.UUID, ciphertext []byte) ([]byte, error) {
	return s.cipher.Decrypt(claimID, ciphertext)
}

type betaPromotionCampaignRow struct {
	Code                   string     `gorm:"column:code;primaryKey"`
	BatchID                uuid.UUID  `gorm:"column:batch_id;type:uuid"`
	Quota                  int        `gorm:"column:quota"`
	ClaimedCount           int        `gorm:"column:claimed_count"`
	Status                 string     `gorm:"column:status"`
	RedemptionPolicy       string     `gorm:"column:redemption_policy"`
	GroupQRCode            []byte     `gorm:"column:group_qr_code"`
	GroupQRCodeContentType *string    `gorm:"column:group_qr_content_type"`
	GroupQRCodeUpdatedAt   *time.Time `gorm:"column:group_qr_updated_at"`
	CreatedAt              time.Time  `gorm:"column:created_at"`
	UpdatedAt              time.Time  `gorm:"column:updated_at"`
}

func (betaPromotionCampaignRow) TableName() string { return "promotion_campaigns" }

type betaPromotionClaimRow struct {
	ID                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	CampaignCode          string     `gorm:"column:campaign_code"`
	Email                 string     `gorm:"column:email"`
	CodeID                uuid.UUID  `gorm:"column:code_id;type:uuid"`
	CodeCiphertext        []byte     `gorm:"column:code_ciphertext"`
	ClientIPDigest        string     `gorm:"column:client_ip_digest"`
	DeliveryStatus        string     `gorm:"column:delivery_status"`
	DeliveryAttempts      int        `gorm:"column:delivery_attempts"`
	LastDeliveryAttemptAt time.Time  `gorm:"column:last_delivery_attempt_at"`
	SentAt                *time.Time `gorm:"column:sent_at"`
	CreatedAt             time.Time  `gorm:"column:created_at"`
	UpdatedAt             time.Time  `gorm:"column:updated_at"`
}

func (betaPromotionClaimRow) TableName() string { return "promotion_claims" }
