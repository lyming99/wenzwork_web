package membership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	trialPromotionIPClaimLimit    = int64(3)
	trialPromotionPendingRetryAge = 2 * time.Minute
	trialPromotionCodeAAD         = "wenzwork:trial-promotion-code:v1"
	trialPromotionGrantDays       = 30
	trialPromotionMaxDailyQuota   = 5000
)

var (
	ErrTrialPromotionInvalidEmail = errors.New("trial promotion email is invalid")
	ErrTrialPromotionUnavailable  = errors.New("trial promotion is unavailable")
	ErrTrialPromotionRateLimit    = errors.New("trial promotion claim rate limit exceeded")
	ErrTrialPromotionDelivery     = errors.New("trial promotion email delivery failed")
	ErrTrialPromotionAdminInvalid = errors.New("trial promotion admin input is invalid")
	ErrTrialPromotionBatchRevoked = errors.New("trial promotion redemption batch is revoked")
)

var trialPromotionLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type TrialPromotionStatus struct {
	Enabled        bool      `json:"enabled"`
	Available      bool      `json:"available"`
	DailyLimit     int       `json:"dailyLimit"`
	ClaimedToday   int       `json:"claimedToday"`
	RemainingToday int       `json:"remainingToday"`
	GrantDays      int       `json:"grantDays"`
	RefreshesAt    time.Time `json:"refreshesAt"`
}

type TrialPromotionClaimResult struct {
	Promotion      TrialPromotionStatus `json:"promotion"`
	DeliveryStatus string               `json:"deliveryStatus"`
	AlreadyClaimed bool                 `json:"alreadyClaimed"`
	NewClaim       bool                 `json:"-"`
}

type TrialPromotionAdminOverview struct {
	Enabled              bool      `json:"enabled"`
	DailyQuota           int       `json:"dailyQuota"`
	Today                string    `json:"today"`
	TodayLimit           int       `json:"todayLimit"`
	ClaimedToday         int       `json:"claimedToday"`
	RemainingToday       int       `json:"remainingToday"`
	Available            bool      `json:"available"`
	GrantDays            int       `json:"grantDays"`
	RefreshesAt          time.Time `json:"refreshesAt"`
	TotalClaimCount      int64     `json:"totalClaimCount"`
	PendingDeliveryCount int64     `json:"pendingDeliveryCount"`
	SentDeliveryCount    int64     `json:"sentDeliveryCount"`
	FailedDeliveryCount  int64     `json:"failedDeliveryCount"`
	ActiveCodeCount      int64     `json:"activeCodeCount"`
	RedeemedCodeCount    int64     `json:"redeemedCodeCount"`
	RevokedCodeCount     int64     `json:"revokedCodeCount"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type TrialPromotionClaimFilter struct {
	Query            string
	DeliveryStatus   string
	RedemptionStatus string
	Limit            int
	Offset           int
}

type TrialPromotionAdminClaim struct {
	ID                    uuid.UUID  `json:"id"`
	Email                 string     `json:"email"`
	ClaimDate             string     `json:"claimDate"`
	CodeHint              string     `json:"codeHint"`
	DeliveryStatus        string     `json:"deliveryStatus"`
	RedemptionStatus      string     `json:"redemptionStatus"`
	DeliveryAttempts      int        `json:"deliveryAttempts"`
	LastDeliveryAttemptAt time.Time  `json:"lastDeliveryAttemptAt"`
	SentAt                *time.Time `json:"sentAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	RedeemedAt            *time.Time `json:"redeemedAt"`
}

type TrialPromotionAdminClaimList struct {
	Items  []TrialPromotionAdminClaim `json:"items"`
	Total  int64                      `json:"total"`
	Limit  int                        `json:"limit"`
	Offset int                        `json:"offset"`
}

type TrialPromotionService struct {
	db     *gorm.DB
	codec  *CodeCodec
	sender mailer.Sender
	cipher *redemptionCodeCipher
	now    func() time.Time
}

func NewTrialPromotionService(
	db *gorm.DB,
	codec *CodeCodec,
	sender mailer.Sender,
	encryptionKey string,
) (*TrialPromotionService, error) {
	if db == nil {
		return nil, errors.New("trial promotion database is required")
	}
	if codec == nil {
		return nil, errors.New("trial promotion code codec is required")
	}
	if sender == nil {
		return nil, errors.New("trial promotion mail sender is required")
	}
	codeCipher, err := newRedemptionCodeCipher(encryptionKey, trialPromotionCodeAAD)
	if err != nil {
		return nil, fmt.Errorf("create trial promotion code cipher: %w", err)
	}
	return &TrialPromotionService{
		db: db, codec: codec, sender: sender, cipher: codeCipher, now: time.Now,
	}, nil
}

func (s *TrialPromotionService) Status(ctx context.Context) (TrialPromotionStatus, error) {
	now := s.now().UTC()
	var result TrialPromotionStatus
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		settings, day, err := loadTrialPromotionState(tx, now, false)
		if err != nil {
			return err
		}
		result = trialPromotionStatusFromRows(settings, day, now)
		return nil
	})
	if err != nil {
		return TrialPromotionStatus{}, err
	}
	return result, nil
}

func (s *TrialPromotionService) AdminOverview(ctx context.Context) (TrialPromotionAdminOverview, error) {
	now := s.now().UTC()
	return s.loadAdminOverview(s.db.WithContext(ctx), now)
}

func (s *TrialPromotionService) loadAdminOverview(
	db *gorm.DB,
	now time.Time,
) (TrialPromotionAdminOverview, error) {
	var settings trialPromotionSettingsRow
	var day trialPromotionDayRow
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		settings, day, err = loadTrialPromotionState(tx, now, false)
		return err
	}); err != nil {
		return TrialPromotionAdminOverview{}, err
	}

	var counts struct {
		TotalClaimCount      int64
		PendingDeliveryCount int64
		SentDeliveryCount    int64
		FailedDeliveryCount  int64
		ActiveCodeCount      int64
		RedeemedCodeCount    int64
		RevokedCodeCount     int64
	}
	if err := db.Table("trial_promotion_claims AS claim").
		Joins("JOIN redemption_codes AS code ON code.id = claim.code_id").
		Select(`
			COUNT(*) AS total_claim_count,
			COUNT(*) FILTER (WHERE claim.delivery_status = 'pending') AS pending_delivery_count,
			COUNT(*) FILTER (WHERE claim.delivery_status = 'sent') AS sent_delivery_count,
			COUNT(*) FILTER (WHERE claim.delivery_status = 'failed') AS failed_delivery_count,
			COUNT(*) FILTER (WHERE code.status = 'active') AS active_code_count,
			COUNT(*) FILTER (WHERE code.status = 'redeemed') AS redeemed_code_count,
			COUNT(*) FILTER (WHERE code.status = 'revoked') AS revoked_code_count`).
		Scan(&counts).Error; err != nil {
		return TrialPromotionAdminOverview{}, fmt.Errorf("count trial promotion claim states: %w", err)
	}

	status := trialPromotionStatusFromRows(settings, day, now)
	return TrialPromotionAdminOverview{
		Enabled: settings.Enabled, DailyQuota: settings.DailyQuota,
		Today: trialPromotionDateString(now), TodayLimit: status.DailyLimit,
		ClaimedToday: status.ClaimedToday, RemainingToday: status.RemainingToday,
		Available: status.Available, GrantDays: status.GrantDays, RefreshesAt: status.RefreshesAt,
		TotalClaimCount:      counts.TotalClaimCount,
		PendingDeliveryCount: counts.PendingDeliveryCount, SentDeliveryCount: counts.SentDeliveryCount,
		FailedDeliveryCount: counts.FailedDeliveryCount, ActiveCodeCount: counts.ActiveCodeCount,
		RedeemedCodeCount: counts.RedeemedCodeCount, RevokedCodeCount: counts.RevokedCodeCount,
		UpdatedAt: settings.UpdatedAt,
	}, nil
}

func (s *TrialPromotionService) ListAdminClaims(
	ctx context.Context,
	filter TrialPromotionClaimFilter,
) (TrialPromotionAdminClaimList, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	filter.DeliveryStatus = strings.TrimSpace(filter.DeliveryStatus)
	filter.RedemptionStatus = strings.TrimSpace(filter.RedemptionStatus)
	if len(filter.Query) > 320 || !promotionDeliveryStatusValid(filter.DeliveryStatus) ||
		!promotionRedemptionStatusValid(filter.RedemptionStatus) || filter.Limit < 1 ||
		filter.Limit > 100 || filter.Offset < 0 || filter.Offset > 100000 {
		return TrialPromotionAdminClaimList{}, ErrTrialPromotionAdminInvalid
	}

	query := s.db.WithContext(ctx).Table("trial_promotion_claims AS claim").
		Joins("JOIN redemption_codes AS code ON code.id = claim.code_id")
	if filter.Query != "" {
		pattern := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").
			Replace(strings.ToLower(filter.Query))
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
		return TrialPromotionAdminClaimList{}, fmt.Errorf("count trial promotion claims: %w", err)
	}
	type scannedClaim struct {
		ID                    uuid.UUID
		Email                 string
		ClaimDate             time.Time
		CodeHint              string
		DeliveryStatus        string
		RedemptionStatus      string
		DeliveryAttempts      int
		LastDeliveryAttemptAt time.Time
		SentAt                *time.Time
		CreatedAt             time.Time
		RedeemedAt            *time.Time
	}
	rows := make([]scannedClaim, 0)
	if err := query.Select(`
		claim.id,
		claim.email,
		claim.claim_date,
		code.code_hint,
		claim.delivery_status,
		code.status AS redemption_status,
		claim.delivery_attempts,
		claim.last_delivery_attempt_at,
		claim.sent_at,
		claim.created_at,
		code.redeemed_at`).
		Order("claim.created_at DESC, claim.id DESC").
		Limit(filter.Limit).Offset(filter.Offset).Scan(&rows).Error; err != nil {
		return TrialPromotionAdminClaimList{}, fmt.Errorf("list trial promotion claims: %w", err)
	}
	items := make([]TrialPromotionAdminClaim, 0, len(rows))
	for _, row := range rows {
		items = append(items, TrialPromotionAdminClaim{
			ID: row.ID, Email: row.Email, ClaimDate: row.ClaimDate.Format(time.DateOnly),
			CodeHint: row.CodeHint, DeliveryStatus: row.DeliveryStatus,
			RedemptionStatus: row.RedemptionStatus, DeliveryAttempts: row.DeliveryAttempts,
			LastDeliveryAttemptAt: row.LastDeliveryAttemptAt, SentAt: row.SentAt,
			CreatedAt: row.CreatedAt, RedeemedAt: row.RedeemedAt,
		})
	}
	return TrialPromotionAdminClaimList{
		Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset,
	}, nil
}

func (s *TrialPromotionService) UpdateAdminSettings(
	ctx context.Context,
	actorUserID uuid.UUID,
	enabled bool,
	dailyQuota int,
) (TrialPromotionAdminOverview, error) {
	if actorUserID == uuid.Nil || dailyQuota < 1 || dailyQuota > trialPromotionMaxDailyQuota {
		return TrialPromotionAdminOverview{}, ErrTrialPromotionAdminInvalid
	}

	now := s.now().UTC()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		settings, day, err := loadTrialPromotionState(tx, now, true)
		if err != nil {
			return err
		}
		var batch redemptionCodeBatchRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&batch, "id = ?", settings.BatchID).Error; err != nil {
			return fmt.Errorf("lock trial promotion redemption batch: %w", err)
		}
		if enabled && batch.Status == "revoked" {
			return ErrTrialPromotionBatchRevoked
		}

		before := map[string]any{
			"enabled": settings.Enabled, "dailyQuota": settings.DailyQuota,
			"todayLimit": day.Quota, "claimedToday": day.ClaimedCount,
		}
		effectiveTodayQuota := dailyQuota
		if effectiveTodayQuota < day.ClaimedCount {
			effectiveTodayQuota = day.ClaimedCount
		}
		if err := tx.Model(&trialPromotionSettingsRow{}).Where("singleton = 1").
			Updates(map[string]any{
				"enabled": enabled, "daily_quota": dailyQuota, "updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("update trial promotion settings: %w", err)
		}
		if err := tx.Model(&trialPromotionDayRow{}).Where("claim_date = ?", day.ClaimDate).
			Updates(map[string]any{"quota": effectiveTodayQuota, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("update today's trial promotion quota: %w", err)
		}
		if err := tx.Model(&redemptionCodeBatchRow{}).Where("id = ?", batch.ID).
			Updates(map[string]any{"quantity": dailyQuota, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("update trial promotion redemption batch quota: %w", err)
		}
		if enabled && batch.Status == "exhausted" {
			if err := tx.Model(&redemptionCodeBatchRow{}).
				Where("id = ? AND status = 'exhausted'", batch.ID).
				Updates(map[string]any{"status": "active", "updated_at": now}).Error; err != nil {
				return fmt.Errorf("reactivate trial promotion redemption batch: %w", err)
			}
		}

		after := map[string]any{
			"enabled": enabled, "dailyQuota": dailyQuota,
			"todayLimit": effectiveTodayQuota, "claimedToday": day.ClaimedCount,
		}
		beforeJSON, _ := json.Marshal(before)
		afterJSON, _ := json.Marshal(after)
		resourceID := settings.BatchID
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &actorUserID, Action: "trial_promotion.settings.update",
			ResourceType: "trial_promotion", ResourceID: &resourceID,
			BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit trial promotion settings update: %w", err)
		}
		return nil
	})
	if err != nil {
		return TrialPromotionAdminOverview{}, err
	}
	return s.loadAdminOverview(s.db.WithContext(ctx), now)
}

func (s *TrialPromotionService) Claim(
	ctx context.Context,
	rawEmail string,
	clientIP string,
) (TrialPromotionClaimResult, error) {
	email, err := normalizeTrialPromotionEmail(rawEmail)
	if err != nil {
		return TrialPromotionClaimResult{}, err
	}
	now := s.now().UTC()
	ipDigest := s.codec.Digest("trial-pro-daily:ip:" + strings.TrimSpace(clientIP))

	var result TrialPromotionClaimResult
	var claim trialPromotionClaimRow
	var plaintext string
	shouldSend := false

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		settings, day, err := loadTrialPromotionState(tx, now, true)
		if err != nil {
			return err
		}

		existingErr := tx.Where("lower(email) = lower(?)", email).First(&claim).Error
		switch {
		case existingErr == nil:
			result = TrialPromotionClaimResult{
				Promotion:      trialPromotionStatusFromRows(settings, day, now),
				DeliveryStatus: claim.DeliveryStatus, AlreadyClaimed: true,
			}
			if claim.DeliveryStatus == "sent" {
				return nil
			}
			if claim.DeliveryStatus == "pending" &&
				claim.LastDeliveryAttemptAt.After(now.Add(-trialPromotionPendingRetryAge)) {
				return nil
			}
			decrypted, err := s.cipher.Decrypt(claim.ID, claim.CodeCiphertext)
			if err != nil {
				return fmt.Errorf("decrypt pending trial promotion code: %w", err)
			}
			plaintext = string(decrypted)
			shouldSend = true
			claim.DeliveryStatus = "pending"
			claim.DeliveryAttempts++
			claim.LastDeliveryAttemptAt = now
			claim.UpdatedAt = now
			if err := tx.Model(&trialPromotionClaimRow{}).Where("id = ?", claim.ID).
				Updates(map[string]any{
					"delivery_status": "pending", "delivery_attempts": claim.DeliveryAttempts,
					"last_delivery_attempt_at": now, "updated_at": now,
				}).Error; err != nil {
				return fmt.Errorf("schedule trial promotion email retry: %w", err)
			}
			result.DeliveryStatus = "pending"
			return nil
		case !errors.Is(existingErr, gorm.ErrRecordNotFound):
			return fmt.Errorf("load trial promotion claim: %w", existingErr)
		}

		if !settings.Enabled || day.ClaimedCount >= day.Quota {
			return ErrTrialPromotionUnavailable
		}
		var recentClaims int64
		if err := tx.Model(&trialPromotionClaimRow{}).
			Where("client_ip_digest = ? AND created_at >= ?", ipDigest, now.Add(-24*time.Hour)).
			Count(&recentClaims).Error; err != nil {
			return fmt.Errorf("count trial promotion IP claims: %w", err)
		}
		if recentClaims >= trialPromotionIPClaimLimit {
			return ErrTrialPromotionRateLimit
		}

		var batch redemptionCodeBatchRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&batch, "id = ?", settings.BatchID).Error; err != nil {
			return fmt.Errorf("lock trial promotion redemption batch: %w", err)
		}
		if batch.Status == "revoked" {
			return ErrTrialPromotionUnavailable
		}
		if batch.Status == "exhausted" {
			if err := tx.Model(&redemptionCodeBatchRow{}).
				Where("id = ? AND status = 'exhausted'", batch.ID).
				Updates(map[string]any{"status": "active", "updated_at": now}).Error; err != nil {
				return fmt.Errorf("reactivate trial promotion redemption batch: %w", err)
			}
		}

		issued, err := s.codec.Generate()
		if err != nil {
			return fmt.Errorf("generate trial promotion code: %w", err)
		}
		claimID := uuid.New()
		ciphertext, err := s.cipher.Encrypt(claimID, []byte(issued.Plaintext))
		if err != nil {
			return fmt.Errorf("encrypt trial promotion code: %w", err)
		}
		codeID := uuid.New()
		if err := tx.Create(&redemptionCodeRow{
			ID: codeID, BatchID: settings.BatchID, CodeDigest: issued.Digest,
			CodeHint: issued.Hint, Status: "active", CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("store trial promotion code digest: %w", err)
		}
		claim = trialPromotionClaimRow{
			ID: claimID, Email: email, ClaimDate: day.ClaimDate, CodeID: codeID,
			CodeCiphertext: ciphertext, ClientIPDigest: ipDigest, DeliveryStatus: "pending",
			DeliveryAttempts: 1, LastDeliveryAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&claim).Error; err != nil {
			return fmt.Errorf("create trial promotion claim: %w", err)
		}

		day.ClaimedCount++
		day.UpdatedAt = now
		if err := tx.Model(&trialPromotionDayRow{}).Where("claim_date = ?", day.ClaimDate).
			Updates(map[string]any{"claimed_count": day.ClaimedCount, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("reserve trial promotion daily quota: %w", err)
		}

		plaintext = issued.Plaintext
		shouldSend = true
		result = TrialPromotionClaimResult{
			Promotion:      trialPromotionStatusFromRows(settings, day, now),
			DeliveryStatus: "pending", NewClaim: true,
		}
		return nil
	})
	if err != nil {
		return TrialPromotionClaimResult{}, err
	}
	if !shouldSend {
		return result, nil
	}

	if err := s.sender.Send(ctx, trialPromotionMessage(email, plaintext)); err != nil {
		updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if updateErr := s.db.WithContext(updateCtx).Model(&trialPromotionClaimRow{}).
			Where("id = ? AND delivery_status = 'pending'", claim.ID).
			Updates(map[string]any{
				"delivery_status": "failed", "updated_at": s.now().UTC(),
			}).Error; updateErr != nil {
			return result, fmt.Errorf(
				"%w: send failed and delivery state update failed: %v",
				ErrTrialPromotionDelivery,
				updateErr,
			)
		}
		return result, fmt.Errorf("%w: %v", ErrTrialPromotionDelivery, err)
	}

	sentAt := s.now().UTC()
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.db.WithContext(updateCtx).Model(&trialPromotionClaimRow{}).
		Where("id = ? AND delivery_status = 'pending'", claim.ID).
		Updates(map[string]any{
			"delivery_status": "sent", "sent_at": sentAt,
			"code_ciphertext": nil, "updated_at": sentAt,
		}).Error; err != nil {
		return result, fmt.Errorf("record trial promotion email delivery: %w", err)
	}
	result.DeliveryStatus = "sent"
	return result, nil
}

func loadTrialPromotionState(
	tx *gorm.DB,
	now time.Time,
	lock bool,
) (trialPromotionSettingsRow, trialPromotionDayRow, error) {
	settingsQuery := tx
	if lock {
		settingsQuery = settingsQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var settings trialPromotionSettingsRow
	if err := settingsQuery.First(&settings, "singleton = 1").Error; err != nil {
		return settings, trialPromotionDayRow{}, fmt.Errorf("load trial promotion settings: %w", err)
	}

	claimDate := trialPromotionClaimDate(now)
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&trialPromotionDayRow{
		ClaimDate: claimDate, Quota: settings.DailyQuota, ClaimedCount: 0,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		return settings, trialPromotionDayRow{}, fmt.Errorf("refresh trial promotion daily quota: %w", err)
	}
	dayQuery := tx
	if lock {
		dayQuery = dayQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var day trialPromotionDayRow
	if err := dayQuery.First(&day, "claim_date = ?", claimDate).Error; err != nil {
		return settings, day, fmt.Errorf("load trial promotion daily quota: %w", err)
	}
	return settings, day, nil
}

func trialPromotionStatusFromRows(
	settings trialPromotionSettingsRow,
	day trialPromotionDayRow,
	now time.Time,
) TrialPromotionStatus {
	remaining := day.Quota - day.ClaimedCount
	if remaining < 0 {
		remaining = 0
	}
	return TrialPromotionStatus{
		Enabled: settings.Enabled, Available: settings.Enabled && remaining > 0,
		DailyLimit: day.Quota, ClaimedToday: day.ClaimedCount, RemainingToday: remaining,
		GrantDays: trialPromotionGrantDays, RefreshesAt: trialPromotionNextRefresh(now),
	}
}

func trialPromotionClaimDate(now time.Time) time.Time {
	local := now.In(trialPromotionLocation)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

func trialPromotionDateString(now time.Time) string {
	return now.In(trialPromotionLocation).Format(time.DateOnly)
}

func trialPromotionNextRefresh(now time.Time) time.Time {
	local := now.In(trialPromotionLocation)
	return time.Date(
		local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, trialPromotionLocation,
	).UTC()
}

func normalizeTrialPromotionEmail(raw string) (string, error) {
	email, err := normalizeRecipientEmail(raw)
	if err != nil {
		return "", ErrTrialPromotionInvalidEmail
	}
	return email, nil
}

func trialPromotionMessage(email, code string) mailer.Message {
	return mailer.Message{
		To:      email,
		Subject: "WenzWork 试用赠送：30 天 Pro 会员兑换码",
		Text: "你好：\n\n感谢体验 WenzWork。\n\n你的 30 天 Pro 试用兑换码：\n" + code +
			"\n\n此兑换码已与 " + email + " 绑定，只能由使用该邮箱注册且已验证的 WenzWork 账号兑换。" +
			"每个邮箱仅可领取并使用一次试用码，请妥善保管。" +
			"\n\n如有问题或希望加入内测群反馈，可联系：\n微信：lyming555\nQQ：44185539" +
			"\n\nWenzWork 团队\n",
	}
}

type trialPromotionSettingsRow struct {
	Singleton  int16     `gorm:"column:singleton;primaryKey"`
	BatchID    uuid.UUID `gorm:"column:batch_id;type:uuid"`
	Enabled    bool      `gorm:"column:enabled"`
	DailyQuota int       `gorm:"column:daily_quota"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

func (trialPromotionSettingsRow) TableName() string { return "trial_promotion_settings" }

type trialPromotionDayRow struct {
	ClaimDate    time.Time `gorm:"column:claim_date;type:date;primaryKey"`
	Quota        int       `gorm:"column:quota"`
	ClaimedCount int       `gorm:"column:claimed_count"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (trialPromotionDayRow) TableName() string { return "trial_promotion_days" }

type trialPromotionClaimRow struct {
	ID                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Email                 string     `gorm:"column:email"`
	ClaimDate             time.Time  `gorm:"column:claim_date;type:date"`
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

func (trialPromotionClaimRow) TableName() string { return "trial_promotion_claims" }
