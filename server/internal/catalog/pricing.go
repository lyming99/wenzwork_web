package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrPricingPlanInvalid              = errors.New("pricing plan input is invalid")
	ErrPricingPlanNotFound             = errors.New("pricing plan not found")
	ErrPricingPlanCodeConflict         = errors.New("pricing plan code already exists")
	ErrPricingPlanVersionConflict      = errors.New("pricing plan version conflict")
	ErrPricingPlanConfirmationRequired = errors.New("pricing plan confirmation is required")
	ErrPricingPlanStateConflict        = errors.New("pricing plan state conflict")
)

var (
	pricingCodePattern     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	pricingCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

type AdminPricingPlan struct {
	ID                    uuid.UUID  `json:"id"`
	Code                  string     `json:"code"`
	Name                  string     `json:"name"`
	Description           string     `json:"description"`
	PriceMinor            *int64     `json:"priceMinor"`
	OriginalPriceMinor    *int64     `json:"originalPriceMinor"`
	Currency              string     `json:"currency"`
	BillingPeriod         string     `json:"billingPeriod"`
	Features              []string   `json:"features"`
	Status                string     `json:"status"`
	SortOrder             int        `json:"sortOrder"`
	Version               int64      `json:"version"`
	PublishedVersion      *int64     `json:"publishedVersion"`
	HasUnpublishedChanges bool       `json:"hasUnpublishedChanges"`
	PublishedAt           *time.Time `json:"publishedAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

type SavePricingPlanInput struct {
	Code               string
	Name               string
	Description        string
	PriceMinor         *int64
	OriginalPriceMinor *int64
	Currency           string
	BillingPeriod      string
	Features           []string
	SortOrder          int
	ExpectedVersion    int64
	ConfirmPriceChange bool
	ActorUserID        uuid.UUID
}

type PricingPlanActionInput struct {
	ExpectedVersion int64
	Confirm         bool
	ActorUserID     uuid.UUID
}

type pricingPlanVersionRow struct {
	ID                 uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	PricingPlanID      uuid.UUID       `gorm:"column:pricing_plan_id;type:uuid"`
	Version            int64           `gorm:"column:version"`
	Code               string          `gorm:"column:code"`
	Name               string          `gorm:"column:name"`
	Description        string          `gorm:"column:description"`
	PriceMinor         *int64          `gorm:"column:price_minor"`
	OriginalPriceMinor *int64          `gorm:"column:original_price_minor"`
	Currency           string          `gorm:"column:currency"`
	BillingPeriod      string          `gorm:"column:billing_period"`
	FeaturesJSON       json.RawMessage `gorm:"column:features_json;type:jsonb"`
	Status             string          `gorm:"column:status"`
	SortOrder          int             `gorm:"column:sort_order"`
	PublishedAt        *time.Time      `gorm:"column:published_at"`
	ChangeType         string          `gorm:"column:change_type"`
	ChangedBy          *uuid.UUID      `gorm:"column:changed_by;type:uuid"`
	CreatedAt          time.Time       `gorm:"column:created_at"`
}

func (pricingPlanVersionRow) TableName() string { return "pricing_plan_versions" }

func (s *Store) ListAdminPricingPlans(ctx context.Context) ([]AdminPricingPlan, error) {
	var rows []pricingPlanRow
	if err := s.db.WithContext(ctx).Order("sort_order ASC, code ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list admin pricing plans: %w", err)
	}
	items := make([]AdminPricingPlan, 0, len(rows))
	for _, row := range rows {
		item, err := adminPricingPlanFromRow(row)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Store) CreatePricingPlan(ctx context.Context, input SavePricingPlanInput) (AdminPricingPlan, error) {
	input, err := validatePricingPlanInput(input, false)
	if err != nil {
		return AdminPricingPlan{}, err
	}
	featuresJSON, _ := json.Marshal(input.Features)
	now := time.Now().UTC()
	row := pricingPlanRow{
		ID: uuid.New(), Code: input.Code, Name: input.Name, Description: input.Description,
		PriceMinor: input.PriceMinor, OriginalPriceMinor: input.OriginalPriceMinor,
		Currency: input.Currency, BillingPeriod: input.BillingPeriod,
		FeaturesJSON: featuresJSON, Status: "draft", SortOrder: input.SortOrder, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	result, _ := adminPricingPlanFromRow(row)
	afterJSON, _ := json.Marshal(result)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			if isCatalogUniqueViolation(err) {
				return ErrPricingPlanCodeConflict
			}
			return fmt.Errorf("create pricing plan: %w", err)
		}
		if err := appendPricingPlanVersion(tx, row, "create", input.ActorUserID, now); err != nil {
			return err
		}
		return appendPricingPlanAudit(tx, input.ActorUserID, "pricing_plan.create", row.ID, nil, afterJSON, now)
	})
	if err != nil {
		return AdminPricingPlan{}, err
	}
	return result, nil
}

func (s *Store) UpdatePricingPlan(ctx context.Context, planID uuid.UUID, input SavePricingPlanInput) (AdminPricingPlan, error) {
	if planID == uuid.Nil {
		return AdminPricingPlan{}, ErrPricingPlanNotFound
	}
	input, err := validatePricingPlanInput(input, true)
	if err != nil {
		return AdminPricingPlan{}, err
	}
	var result AdminPricingPlan
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row pricingPlanRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", planID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPricingPlanNotFound
			}
			return fmt.Errorf("lock pricing plan: %w", err)
		}
		if row.Version != input.ExpectedVersion {
			return ErrPricingPlanVersionConflict
		}
		if row.PublishedVersion != nil && row.Code != input.Code {
			return ErrPricingPlanInvalid
		}
		if pricingTermsChanged(row, input) && !input.ConfirmPriceChange {
			return ErrPricingPlanConfirmationRequired
		}
		before, err := adminPricingPlanFromRow(row)
		if err != nil {
			return err
		}
		beforeJSON, _ := json.Marshal(before)
		featuresJSON, _ := json.Marshal(input.Features)
		now := time.Now().UTC()
		row.Code, row.Name, row.Description = input.Code, input.Name, input.Description
		row.PriceMinor, row.OriginalPriceMinor = input.PriceMinor, input.OriginalPriceMinor
		row.Currency, row.BillingPeriod = input.Currency, input.BillingPeriod
		row.FeaturesJSON, row.SortOrder = featuresJSON, input.SortOrder
		row.Version++
		row.UpdatedAt = now
		if err := tx.Save(&row).Error; err != nil {
			if isCatalogUniqueViolation(err) {
				return ErrPricingPlanCodeConflict
			}
			return fmt.Errorf("update pricing plan: %w", err)
		}
		result, err = adminPricingPlanFromRow(row)
		if err != nil {
			return err
		}
		afterJSON, _ := json.Marshal(result)
		if err := appendPricingPlanVersion(tx, row, "update", input.ActorUserID, now); err != nil {
			return err
		}
		return appendPricingPlanAudit(tx, input.ActorUserID, "pricing_plan.update", row.ID, beforeJSON, afterJSON, now)
	})
	if err != nil {
		return AdminPricingPlan{}, err
	}
	return result, nil
}

func (s *Store) PublishPricingPlan(ctx context.Context, planID uuid.UUID, input PricingPlanActionInput) (AdminPricingPlan, error) {
	if planID == uuid.Nil || input.ActorUserID == uuid.Nil || input.ExpectedVersion < 1 {
		return AdminPricingPlan{}, ErrPricingPlanInvalid
	}
	if !input.Confirm {
		return AdminPricingPlan{}, ErrPricingPlanConfirmationRequired
	}
	var result AdminPricingPlan
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row pricingPlanRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", planID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPricingPlanNotFound
			}
			return fmt.Errorf("lock pricing plan for publication: %w", err)
		}
		if row.Version != input.ExpectedVersion {
			return ErrPricingPlanVersionConflict
		}
		if row.Status == "published" && row.PublishedVersion != nil && *row.PublishedVersion == row.Version {
			current, conversionErr := adminPricingPlanFromRow(row)
			result = current
			return conversionErr
		}
		before, err := adminPricingPlanFromRow(row)
		if err != nil {
			return err
		}
		beforeJSON, _ := json.Marshal(before)
		now := time.Now().UTC()
		row.Status = "published"
		row.Version++
		publishedVersion := row.Version
		row.PublishedVersion, row.PublishedAt = &publishedVersion, &now
		row.UpdatedAt = now
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("publish pricing plan: %w", err)
		}
		current, conversionErr := adminPricingPlanFromRow(row)
		if conversionErr != nil {
			return conversionErr
		}
		result = current
		afterJSON, _ := json.Marshal(result)
		if err := appendPricingPlanVersion(tx, row, "publish", input.ActorUserID, now); err != nil {
			return err
		}
		return appendPricingPlanAudit(tx, input.ActorUserID, "pricing_plan.publish", row.ID, beforeJSON, afterJSON, now)
	})
	if err != nil {
		return AdminPricingPlan{}, err
	}
	return result, nil
}

func (s *Store) ArchivePricingPlan(ctx context.Context, planID uuid.UUID, input PricingPlanActionInput) (AdminPricingPlan, error) {
	if planID == uuid.Nil || input.ActorUserID == uuid.Nil || input.ExpectedVersion < 1 {
		return AdminPricingPlan{}, ErrPricingPlanInvalid
	}
	if !input.Confirm {
		return AdminPricingPlan{}, ErrPricingPlanConfirmationRequired
	}
	var result AdminPricingPlan
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row pricingPlanRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", planID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPricingPlanNotFound
			}
			return fmt.Errorf("lock pricing plan for archival: %w", err)
		}
		if row.Version != input.ExpectedVersion {
			return ErrPricingPlanVersionConflict
		}
		if row.Status == "archived" {
			current, conversionErr := adminPricingPlanFromRow(row)
			result = current
			return conversionErr
		}
		if row.Status != "published" || row.PublishedVersion == nil {
			return ErrPricingPlanStateConflict
		}
		before, err := adminPricingPlanFromRow(row)
		if err != nil {
			return err
		}
		beforeJSON, _ := json.Marshal(before)
		now := time.Now().UTC()
		row.Status = "archived"
		row.Version++
		row.UpdatedAt = now
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("archive pricing plan: %w", err)
		}
		current, conversionErr := adminPricingPlanFromRow(row)
		if conversionErr != nil {
			return conversionErr
		}
		result = current
		afterJSON, _ := json.Marshal(result)
		if err := appendPricingPlanVersion(tx, row, "archive", input.ActorUserID, now); err != nil {
			return err
		}
		return appendPricingPlanAudit(tx, input.ActorUserID, "pricing_plan.archive", row.ID, beforeJSON, afterJSON, now)
	})
	if err != nil {
		return AdminPricingPlan{}, err
	}
	return result, nil
}

func validatePricingPlanInput(input SavePricingPlanInput, requireVersion bool) (SavePricingPlanInput, error) {
	input.Code = strings.ToLower(strings.TrimSpace(input.Code))
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.Currency = strings.ToUpper(strings.TrimSpace(input.Currency))
	input.BillingPeriod = strings.TrimSpace(input.BillingPeriod)
	if input.ActorUserID == uuid.Nil || !pricingCodePattern.MatchString(input.Code) ||
		!validPricingText(input.Name, 80, false) || !validPricingText(input.Description, 500, true) ||
		!pricingCurrencyPattern.MatchString(input.Currency) ||
		!validBillingPeriod(input.BillingPeriod) || input.SortOrder < -100000 || input.SortOrder > 100000 ||
		(input.PriceMinor != nil && *input.PriceMinor < 0) ||
		(input.OriginalPriceMinor != nil && (input.PriceMinor == nil || *input.OriginalPriceMinor <= *input.PriceMinor)) ||
		len(input.Features) > 30 ||
		(requireVersion && input.ExpectedVersion < 1) {
		return SavePricingPlanInput{}, ErrPricingPlanInvalid
	}
	seen := make(map[string]struct{}, len(input.Features))
	for index := range input.Features {
		input.Features[index] = strings.TrimSpace(input.Features[index])
		if !validPricingText(input.Features[index], 120, false) {
			return SavePricingPlanInput{}, ErrPricingPlanInvalid
		}
		key := strings.ToLower(input.Features[index])
		if _, exists := seen[key]; exists {
			return SavePricingPlanInput{}, ErrPricingPlanInvalid
		}
		seen[key] = struct{}{}
	}
	return input, nil
}

func validBillingPeriod(value string) bool {
	switch value {
	case "free", "month", "year", "one_time", "redemption":
		return true
	default:
		return false
	}
}

func validPricingText(value string, maximum int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum || (!allowEmpty && value == "") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func pricingTermsChanged(row pricingPlanRow, input SavePricingPlanInput) bool {
	return !equalPriceMinor(row.PriceMinor, input.PriceMinor) ||
		!equalPriceMinor(row.OriginalPriceMinor, input.OriginalPriceMinor) ||
		row.Currency != input.Currency || row.BillingPeriod != input.BillingPeriod
}

func equalPriceMinor(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func adminPricingPlanFromRow(row pricingPlanRow) (AdminPricingPlan, error) {
	var features []string
	if err := json.Unmarshal(row.FeaturesJSON, &features); err != nil {
		return AdminPricingPlan{}, fmt.Errorf("decode pricing plan %s features: %w", row.Code, err)
	}
	if features == nil {
		features = []string{}
	}
	var publishedAt *time.Time
	if row.PublishedAt != nil {
		value := row.PublishedAt.UTC()
		publishedAt = &value
	}
	return AdminPricingPlan{
		ID: row.ID, Code: row.Code, Name: row.Name, Description: row.Description,
		PriceMinor: row.PriceMinor, OriginalPriceMinor: row.OriginalPriceMinor,
		Currency: row.Currency, BillingPeriod: row.BillingPeriod,
		Features: features, Status: row.Status, SortOrder: row.SortOrder, Version: row.Version,
		PublishedVersion:      row.PublishedVersion,
		HasUnpublishedChanges: row.PublishedVersion == nil || *row.PublishedVersion != row.Version,
		PublishedAt:           publishedAt, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}, nil
}

func appendPricingPlanVersion(tx *gorm.DB, row pricingPlanRow, changeType string, actor uuid.UUID, now time.Time) error {
	if err := tx.Create(&pricingPlanVersionRow{
		ID: uuid.New(), PricingPlanID: row.ID, Version: row.Version, Code: row.Code, Name: row.Name,
		Description: row.Description, PriceMinor: row.PriceMinor, OriginalPriceMinor: row.OriginalPriceMinor,
		Currency:      row.Currency,
		BillingPeriod: row.BillingPeriod, FeaturesJSON: row.FeaturesJSON, Status: row.Status,
		SortOrder: row.SortOrder, PublishedAt: row.PublishedAt, ChangeType: changeType,
		ChangedBy: &actor, CreatedAt: now,
	}).Error; err != nil {
		if isCatalogUniqueViolation(err) {
			return ErrPricingPlanVersionConflict
		}
		return fmt.Errorf("append pricing plan version: %w", err)
	}
	return nil
}

func appendPricingPlanAudit(tx *gorm.DB, actor uuid.UUID, action string, resource uuid.UUID, before, after []byte, now time.Time) error {
	if err := tx.Create(&catalogAuditLogRow{
		ID: uuid.New(), ActorUserID: &actor, Action: action, ResourceType: "pricing_plan",
		ResourceID: &resource, BeforeJSON: before, AfterJSON: after, CreatedAt: now,
	}).Error; err != nil {
		return fmt.Errorf("append pricing plan audit: %w", err)
	}
	return nil
}
