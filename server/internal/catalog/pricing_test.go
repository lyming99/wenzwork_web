package catalog

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestValidatePricingPlanInputNormalizesSafeValues(t *testing.T) {
	price := int64(12800)
	originalPrice := int64(16800)
	input, err := validatePricingPlanInput(SavePricingPlanInput{
		Code: " Pro_Year ", Name: " Pro 年付 ", Description: " 创作者方案 ", PriceMinor: &price,
		OriginalPriceMinor: &originalPrice,
		Currency:           "cny", BillingPeriod: "year", Features: []string{" 快速启动 ", "项目文档"},
		SortOrder: 20, ExpectedVersion: 2, ActorUserID: uuid.New(),
	}, true)
	if err != nil {
		t.Fatalf("validatePricingPlanInput() error = %v", err)
	}
	if input.Code != "pro_year" || input.Name != "Pro 年付" || input.Currency != "CNY" ||
		input.OriginalPriceMinor == nil || *input.OriginalPriceMinor != originalPrice || input.Features[0] != "快速启动" {
		t.Fatalf("normalized input = %+v", input)
	}
}

func TestValidatePricingPlanInputRejectsUnsafeOrAmbiguousValues(t *testing.T) {
	price := int64(100)
	equalOriginalPrice := int64(100)
	originalWithoutPrice := int64(200)
	tests := []SavePricingPlanInput{
		{Code: "Bad Code", Name: "Plan", Currency: "CNY", BillingPeriod: "year", ActorUserID: uuid.New()},
		{Code: "plan", Name: "Plan", Currency: "RMB", BillingPeriod: "weekly", ActorUserID: uuid.New()},
		{Code: "plan", Name: "Plan", Currency: "CNY", BillingPeriod: "year", Features: []string{"Same", "same"}, ActorUserID: uuid.New()},
		{Code: "plan", Name: "Plan", PriceMinor: &price, OriginalPriceMinor: &equalOriginalPrice, Currency: "CNY", BillingPeriod: "year", ActorUserID: uuid.New()},
		{Code: "plan", Name: "Plan", OriginalPriceMinor: &originalWithoutPrice, Currency: "CNY", BillingPeriod: "year", ActorUserID: uuid.New()},
	}
	for _, input := range tests {
		if _, err := validatePricingPlanInput(input, false); !errors.Is(err, ErrPricingPlanInvalid) {
			t.Fatalf("validatePricingPlanInput(%+v) error = %v", input, err)
		}
	}
}

func TestPricingTermsChangedHandlesNullableMinorUnits(t *testing.T) {
	current := int64(100)
	same := int64(100)
	changed := int64(101)
	currentOriginal := int64(150)
	sameOriginal := int64(150)
	changedOriginal := int64(160)
	row := pricingPlanRow{PriceMinor: &current, OriginalPriceMinor: &currentOriginal, Currency: "CNY", BillingPeriod: "month"}
	if pricingTermsChanged(row, SavePricingPlanInput{PriceMinor: &same, OriginalPriceMinor: &sameOriginal, Currency: "CNY", BillingPeriod: "month"}) {
		t.Fatal("equal pricing terms reported as changed")
	}
	if !pricingTermsChanged(row, SavePricingPlanInput{PriceMinor: &changed, Currency: "CNY", BillingPeriod: "month"}) ||
		!pricingTermsChanged(row, SavePricingPlanInput{PriceMinor: &same, OriginalPriceMinor: &changedOriginal, Currency: "CNY", BillingPeriod: "month"}) ||
		!pricingTermsChanged(row, SavePricingPlanInput{PriceMinor: nil, Currency: "CNY", BillingPeriod: "month"}) {
		t.Fatal("changed or nullable pricing terms were not detected")
	}
}
