package membership

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	lifetimeCodeDeliveryAAD         = "wenzwork:lifetime-code-delivery:v1"
	lifetimeCodeDeliveryPendingAge  = 2 * time.Minute
	lifetimeCodeDeliveryBatchPrefix = "付费用户永久 Pro"
)

var (
	ErrLifetimeCodeDeliveryInvalid     = errors.New("lifetime code delivery input is invalid")
	ErrLifetimeCodeUnavailable         = errors.New("lifetime code is unavailable")
	ErrLifetimeCodeEmailDeliveryFailed = errors.New("lifetime code email delivery failed")
)

type LifetimeCodeDeliveryInput struct {
	RequestID   uuid.UUID
	Email       string
	ActorUserID uuid.UUID
}

type LifetimeCodeDelivery struct {
	ID                    uuid.UUID  `json:"id"`
	Email                 string     `json:"email"`
	CodeHint              string     `json:"codeHint"`
	DeliveryStatus        string     `json:"deliveryStatus"`
	RedemptionStatus      string     `json:"redemptionStatus"`
	DeliveryAttempts      int        `json:"deliveryAttempts"`
	LastDeliveryAttemptAt time.Time  `json:"lastDeliveryAttemptAt"`
	SentAt                *time.Time `json:"sentAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

type LifetimeCodeDeliveryResult struct {
	Delivery    LifetimeCodeDelivery `json:"delivery"`
	NewDelivery bool                 `json:"-"`
}

type LifetimeCodeDeliveryService struct {
	db     *gorm.DB
	codec  *CodeCodec
	sender mailer.Sender
	cipher *redemptionCodeCipher
	now    func() time.Time
}

func NewLifetimeCodeDeliveryService(db *gorm.DB, codec *CodeCodec, sender mailer.Sender, encryptionKey string) (*LifetimeCodeDeliveryService, error) {
	if db == nil {
		return nil, errors.New("lifetime code delivery database is required")
	}
	if codec == nil {
		return nil, errors.New("lifetime code delivery codec is required")
	}
	if sender == nil {
		return nil, errors.New("lifetime code delivery mail sender is required")
	}
	codeCipher, err := newRedemptionCodeCipher(encryptionKey, lifetimeCodeDeliveryAAD)
	if err != nil {
		return nil, fmt.Errorf("create lifetime code delivery cipher: %w", err)
	}
	return &LifetimeCodeDeliveryService{
		db: db, codec: codec, sender: sender, cipher: codeCipher, now: time.Now,
	}, nil
}

func (s *LifetimeCodeDeliveryService) ListLifetimeCodeDeliveries(ctx context.Context, limit int) ([]LifetimeCodeDelivery, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrLifetimeCodeDeliveryInvalid
	}
	items := make([]LifetimeCodeDelivery, 0)
	if err := s.db.WithContext(ctx).Table("lifetime_code_deliveries AS delivery").
		Select(`
			delivery.id,
			delivery.email,
			code.code_hint,
			delivery.delivery_status,
			code.status AS redemption_status,
			delivery.delivery_attempts,
			delivery.last_delivery_attempt_at,
			delivery.sent_at,
			delivery.created_at,
			delivery.updated_at`).
		Joins("JOIN redemption_codes AS code ON code.id = delivery.code_id").
		Order("delivery.created_at DESC, delivery.id DESC").
		Limit(limit).
		Scan(&items).Error; err != nil {
		return nil, fmt.Errorf("list lifetime code deliveries: %w", err)
	}
	return items, nil
}

func (s *LifetimeCodeDeliveryService) SendLifetimeCode(ctx context.Context, input LifetimeCodeDeliveryInput) (LifetimeCodeDeliveryResult, error) {
	email, err := normalizeRecipientEmail(input.Email)
	if err != nil || input.RequestID == uuid.Nil || input.ActorUserID == uuid.Nil {
		return LifetimeCodeDeliveryResult{}, ErrLifetimeCodeDeliveryInvalid
	}

	now := s.now().UTC()
	var row lifetimeCodeDeliveryRow
	var plaintext string
	var result LifetimeCodeDeliveryResult
	shouldSend := false

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		existingErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&row, "id = ?", input.RequestID).Error
		switch {
		case existingErr == nil:
			if !strings.EqualFold(row.Email, email) {
				return ErrLifetimeCodeDeliveryInvalid
			}
			var code redemptionCodeRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Preload("Batch").First(&code, "id = ?", row.CodeID).Error; err != nil {
				return fmt.Errorf("load lifetime delivery code: %w", err)
			}
			result.Delivery = lifetimeCodeDeliveryFromRows(row, code)
			if row.DeliveryStatus == "sent" {
				return nil
			}
			if code.Status != "active" || code.Batch.Status != "active" {
				return ErrLifetimeCodeUnavailable
			}
			if row.DeliveryStatus == "pending" &&
				row.LastDeliveryAttemptAt.After(now.Add(-lifetimeCodeDeliveryPendingAge)) {
				return nil
			}
			decrypted, err := s.cipher.Decrypt(row.ID, row.CodeCiphertext)
			if err != nil {
				return fmt.Errorf("decrypt lifetime delivery code: %w", err)
			}
			plaintext = string(decrypted)
			shouldSend = true
			beforeJSON, _ := json.Marshal(map[string]any{
				"deliveryStatus":   row.DeliveryStatus,
				"deliveryAttempts": row.DeliveryAttempts,
			})
			row.DeliveryStatus = "pending"
			row.DeliveryAttempts++
			row.LastDeliveryAttemptAt = now
			row.UpdatedAt = now
			if err := tx.Model(&lifetimeCodeDeliveryRow{}).Where("id = ?", row.ID).
				Updates(map[string]any{
					"delivery_status":          row.DeliveryStatus,
					"delivery_attempts":        row.DeliveryAttempts,
					"last_delivery_attempt_at": row.LastDeliveryAttemptAt,
					"updated_at":               row.UpdatedAt,
				}).Error; err != nil {
				return fmt.Errorf("schedule lifetime code email retry: %w", err)
			}
			afterJSON, _ := json.Marshal(map[string]any{
				"deliveryStatus":   row.DeliveryStatus,
				"deliveryAttempts": row.DeliveryAttempts,
			})
			resourceID := row.ID
			if err := tx.Create(&auditLogRow{
				ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "lifetime_code.delivery.retry",
				ResourceType: "lifetime_code_delivery", ResourceID: &resourceID,
				BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
			}).Error; err != nil {
				return fmt.Errorf("audit lifetime code delivery retry: %w", err)
			}
			result.Delivery = lifetimeCodeDeliveryFromRows(row, code)
			return nil
		case !errors.Is(existingErr, gorm.ErrRecordNotFound):
			return fmt.Errorf("load lifetime code delivery: %w", existingErr)
		}

		var plan membershipPlanRow
		if err := tx.Where("code = 'pro' AND status = 'active'").First(&plan).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrMembershipPlanAbsent
			}
			return fmt.Errorf("load lifetime code delivery plan: %w", err)
		}
		issued, err := s.codec.Generate()
		if err != nil {
			return fmt.Errorf("generate lifetime code: %w", err)
		}
		codeID := uuid.New()
		batchID := uuid.New()
		batch := redemptionCodeBatchRow{
			ID: batchID, Name: lifetimeDeliveryBatchName(email, issued.Hint), PlanID: plan.ID,
			GrantType: string(GrantLifetime), Quantity: 1, Status: "active",
			Note: "管理员邮件发放永久 Pro 激活码至 " + email, CreatedBy: &input.ActorUserID,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&batch).Error; err != nil {
			return fmt.Errorf("create lifetime code delivery batch: %w", err)
		}
		code := redemptionCodeRow{
			ID: codeID, BatchID: batchID, Batch: batch, CodeDigest: issued.Digest,
			CodeHint: issued.Hint, Status: "active", CreatedAt: now,
		}
		if err := tx.Omit("Batch").Create(&code).Error; err != nil {
			return fmt.Errorf("store lifetime code digest: %w", err)
		}
		ciphertext, err := s.cipher.Encrypt(input.RequestID, []byte(issued.Plaintext))
		if err != nil {
			return err
		}
		row = lifetimeCodeDeliveryRow{
			ID: input.RequestID, Email: email, CodeID: codeID, CodeCiphertext: ciphertext,
			DeliveryStatus: "pending", DeliveryAttempts: 1, LastDeliveryAttemptAt: now,
			CreatedBy: input.ActorUserID, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create lifetime code delivery: %w", err)
		}
		afterJSON, _ := json.Marshal(map[string]any{
			"email": email, "codeHint": issued.Hint, "deliveryStatus": row.DeliveryStatus,
			"grantType": GrantLifetime, "planCode": plan.Code,
		})
		resourceID := row.ID
		if err := tx.Create(&auditLogRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "lifetime_code.delivery.create",
			ResourceType: "lifetime_code_delivery", ResourceID: &resourceID,
			AfterJSON: afterJSON, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("audit lifetime code delivery creation: %w", err)
		}

		plaintext = issued.Plaintext
		shouldSend = true
		result.NewDelivery = true
		result.Delivery = lifetimeCodeDeliveryFromRows(row, code)
		return nil
	})
	if err != nil {
		return LifetimeCodeDeliveryResult{}, err
	}
	if !shouldSend {
		return result, nil
	}

	if err := s.sender.Send(ctx, lifetimeCodeDeliveryMessage(email, plaintext)); err != nil {
		updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		failedAt := s.now().UTC()
		if updateErr := s.db.WithContext(updateCtx).Model(&lifetimeCodeDeliveryRow{}).
			Where("id = ? AND delivery_status = 'pending'", row.ID).
			Updates(map[string]any{"delivery_status": "failed", "updated_at": failedAt}).Error; updateErr != nil {
			return result, fmt.Errorf("%w: send failed and delivery state update failed: %v", ErrLifetimeCodeEmailDeliveryFailed, updateErr)
		}
		result.Delivery.DeliveryStatus = "failed"
		result.Delivery.UpdatedAt = failedAt
		return result, fmt.Errorf("%w: %v", ErrLifetimeCodeEmailDeliveryFailed, err)
	}

	sentAt := s.now().UTC()
	updateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.db.WithContext(updateCtx).Model(&lifetimeCodeDeliveryRow{}).
		Where("id = ? AND delivery_status = 'pending'", row.ID).
		Updates(map[string]any{
			"delivery_status": "sent", "sent_at": sentAt, "code_ciphertext": nil, "updated_at": sentAt,
		}).Error; err != nil {
		return result, fmt.Errorf("record lifetime code email delivery: %w", err)
	}
	result.Delivery.DeliveryStatus = "sent"
	result.Delivery.SentAt = &sentAt
	result.Delivery.UpdatedAt = sentAt
	return result, nil
}

func lifetimeDeliveryBatchName(email, codeHint string) string {
	const maximumEmailRunes = 64
	displayEmail := email
	if utf8.RuneCountInString(displayEmail) > maximumEmailRunes {
		runes := []rune(displayEmail)
		displayEmail = string(runes[:maximumEmailRunes-1]) + "…"
	}
	return lifetimeCodeDeliveryBatchPrefix + " · " + displayEmail + " · " + codeHint
}

func lifetimeCodeDeliveryFromRows(delivery lifetimeCodeDeliveryRow, code redemptionCodeRow) LifetimeCodeDelivery {
	return LifetimeCodeDelivery{
		ID: delivery.ID, Email: delivery.Email, CodeHint: code.CodeHint,
		DeliveryStatus: delivery.DeliveryStatus, RedemptionStatus: code.Status,
		DeliveryAttempts:      delivery.DeliveryAttempts,
		LastDeliveryAttemptAt: delivery.LastDeliveryAttemptAt,
		SentAt:                delivery.SentAt, CreatedAt: delivery.CreatedAt, UpdatedAt: delivery.UpdatedAt,
	}
}

func lifetimeCodeDeliveryMessage(email, code string) mailer.Message {
	return mailer.Message{
		To:      email,
		Subject: "WenzWork 永久 Pro 激活码",
		Text: "你好：\n\n感谢购买 WenzWork 永久 Pro 会员。\n\n你的永久 Pro 激活码：\n" + code +
			"\n\n请登录 WenzWork，在“账户中心 → 会员中心”输入激活码完成兑换。" +
			"激活码仅可使用一次，请妥善保管，不要转发给他人。" +
			"\n\n如遇问题，请联系客服：\n微信：lyming555\nQQ：44185539" +
			"\n\nWenzWork 团队\n",
	}
}

type lifetimeCodeDeliveryRow struct {
	ID                    uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Email                 string     `gorm:"column:email"`
	CodeID                uuid.UUID  `gorm:"column:code_id;type:uuid"`
	CodeCiphertext        []byte     `gorm:"column:code_ciphertext"`
	DeliveryStatus        string     `gorm:"column:delivery_status"`
	DeliveryAttempts      int        `gorm:"column:delivery_attempts"`
	LastDeliveryAttemptAt time.Time  `gorm:"column:last_delivery_attempt_at"`
	SentAt                *time.Time `gorm:"column:sent_at"`
	CreatedBy             uuid.UUID  `gorm:"column:created_by;type:uuid"`
	CreatedAt             time.Time  `gorm:"column:created_at"`
	UpdatedAt             time.Time  `gorm:"column:updated_at"`
}

func (lifetimeCodeDeliveryRow) TableName() string { return "lifetime_code_deliveries" }
