package membership

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTrialPromotionUsesChinaNaturalDay(t *testing.T) {
	beforeMidnight := time.Date(2026, 7, 25, 15, 59, 59, 0, time.UTC)
	if got := trialPromotionDateString(beforeMidnight); got != "2026-07-25" {
		t.Fatalf("date before China midnight = %q", got)
	}
	if got := trialPromotionNextRefresh(beforeMidnight); !got.Equal(
		time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC),
	) {
		t.Fatalf("next refresh before midnight = %s", got)
	}

	atMidnight := beforeMidnight.Add(time.Second)
	if got := trialPromotionDateString(atMidnight); got != "2026-07-26" {
		t.Fatalf("date at China midnight = %q", got)
	}
	if got := trialPromotionClaimDate(atMidnight); got.Format(time.DateOnly) != "2026-07-26" {
		t.Fatalf("claim date at China midnight = %s", got)
	}
}

func TestTrialPromotionStatusFromRows(t *testing.T) {
	now := time.Date(2026, 7, 25, 8, 0, 0, 0, time.UTC)
	status := trialPromotionStatusFromRows(
		trialPromotionSettingsRow{Enabled: true, DailyQuota: 100},
		trialPromotionDayRow{Quota: 100, ClaimedCount: 37},
		now,
	)
	if !status.Available || !status.Enabled || status.DailyLimit != 100 ||
		status.ClaimedToday != 37 || status.RemainingToday != 63 ||
		status.GrantDays != trialPromotionGrantDays {
		t.Fatalf("status = %+v", status)
	}

	disabled := trialPromotionStatusFromRows(
		trialPromotionSettingsRow{Enabled: false, DailyQuota: 100},
		trialPromotionDayRow{Quota: 100, ClaimedCount: 37},
		now,
	)
	if disabled.Available || disabled.Enabled {
		t.Fatalf("disabled status = %+v", disabled)
	}

	exhausted := trialPromotionStatusFromRows(
		trialPromotionSettingsRow{Enabled: true, DailyQuota: 100},
		trialPromotionDayRow{Quota: 100, ClaimedCount: 100},
		now,
	)
	if exhausted.Available || exhausted.RemainingToday != 0 {
		t.Fatalf("exhausted status = %+v", exhausted)
	}
}

func TestTrialPromotionEmailAndMessage(t *testing.T) {
	email, err := normalizeTrialPromotionEmail(" Trial@Example.COM ")
	if err != nil || email != "trial@example.com" {
		t.Fatalf("normalizeTrialPromotionEmail() = %q, %v", email, err)
	}
	if _, err := normalizeTrialPromotionEmail("not-an-email"); !errors.Is(
		err,
		ErrTrialPromotionInvalidEmail,
	) {
		t.Fatalf("invalid email error = %v", err)
	}

	message := trialPromotionMessage(
		"trial@example.com",
		"WZM-2345-6789-ABCD-EFGH-JKMN",
	)
	for _, expected := range []string{
		"30 天 Pro", "WZM-2345-6789-ABCD-EFGH-JKMN", "trial@example.com",
		"每个邮箱仅可领取并使用一次", "lyming555", "44185539",
	} {
		if !strings.Contains(message.Subject+message.Text, expected) {
			t.Fatalf("trial email does not contain %q", expected)
		}
	}
}

func TestValidateTrialRedemptionEligibility(t *testing.T) {
	now := time.Date(2026, 7, 25, 8, 0, 0, 0, time.UTC)
	expiresAt := now.Add(30 * 24 * time.Hour)
	tests := []struct {
		name                string
		isTrialCode         bool
		previousRedemptions int64
		current             *Membership
		wantErr             error
	}{
		{name: "fresh trial account", isTrialCode: true},
		{
			name: "trial already used", isTrialCode: true, previousRedemptions: 1,
			wantErr: ErrCodeUnavailable,
		},
		{
			name: "trial blocked for lifetime member", isTrialCode: true,
			current: &Membership{
				PlanCode: "pro", PlanRank: 10, StartsAt: now, ExpiresAt: nil,
			},
			wantErr: ErrMembershipNotExtended,
		},
		{
			name: "trial can extend expiring membership", isTrialCode: true,
			current: &Membership{
				PlanCode: "pro", PlanRank: 10, StartsAt: now, ExpiresAt: &expiresAt,
			},
		},
		{
			name: "ordinary code remains unaffected",
			current: &Membership{
				PlanCode: "pro", PlanRank: 10, StartsAt: now, ExpiresAt: nil,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateTrialRedemptionEligibility(
				test.isTrialCode,
				test.previousRedemptions,
				test.current,
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("validateTrialRedemptionEligibility() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestTrialPromotionAdminRejectsInvalidInputBeforeDatabaseAccess(t *testing.T) {
	service := &TrialPromotionService{}
	for _, filter := range []TrialPromotionClaimFilter{
		{Limit: 0},
		{Limit: 101},
		{Limit: 50, Offset: -1},
		{Limit: 50, DeliveryStatus: "unknown"},
		{Limit: 50, RedemptionStatus: "unknown"},
		{Limit: 50, Query: strings.Repeat("a", 321)},
	} {
		if _, err := service.ListAdminClaims(context.Background(), filter); !errors.Is(
			err,
			ErrTrialPromotionAdminInvalid,
		) {
			t.Fatalf("ListAdminClaims(%+v) error = %v, want invalid input", filter, err)
		}
	}
	for _, input := range []struct {
		actor uuid.UUID
		quota int
	}{
		{actor: uuid.Nil, quota: 100},
		{actor: uuid.New(), quota: 0},
		{actor: uuid.New(), quota: trialPromotionMaxDailyQuota + 1},
	} {
		if _, err := service.UpdateAdminSettings(
			context.Background(),
			input.actor,
			true,
			input.quota,
		); !errors.Is(err, ErrTrialPromotionAdminInvalid) {
			t.Fatalf("UpdateAdminSettings(%+v) error = %v, want invalid input", input, err)
		}
	}
}
