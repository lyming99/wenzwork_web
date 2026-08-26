package membership

import (
	"errors"
	"testing"
	"time"
)

func TestApplyGrantCreatesDurationMembership(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	got, err := ApplyGrant(now, nil, Grant{PlanCode: "pro", PlanRank: 10, Type: GrantDuration, Days: 30})
	if err != nil {
		t.Fatalf("ApplyGrant() error = %v", err)
	}
	wantExpiry := now.AddDate(0, 0, 30)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, wantExpiry)
	}
}

func TestApplyGrantExtendsActiveSamePlanFromCurrentExpiry(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	currentExpiry := now.AddDate(0, 0, 15)
	current := &Membership{PlanCode: "pro", PlanRank: 10, StartsAt: now.AddDate(0, 0, -10), ExpiresAt: &currentExpiry}

	got, err := ApplyGrant(now, current, Grant{PlanCode: "pro", PlanRank: 10, Type: GrantDuration, Days: 30})
	if err != nil {
		t.Fatalf("ApplyGrant() error = %v", err)
	}
	wantExpiry := currentExpiry.AddDate(0, 0, 30)
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, wantExpiry)
	}
	if !got.StartsAt.Equal(current.StartsAt) {
		t.Fatalf("StartsAt = %v, want %v", got.StartsAt, current.StartsAt)
	}
}

func TestApplyGrantDoesNotDowngradeLifetimeMembership(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	current := &Membership{PlanCode: "pro", PlanRank: 10, StartsAt: now.AddDate(-1, 0, 0), ExpiresAt: nil}

	_, err := ApplyGrant(now, current, Grant{PlanCode: "pro", PlanRank: 10, Type: GrantDuration, Days: 30})
	if !errors.Is(err, ErrMembershipNotExtended) {
		t.Fatalf("ApplyGrant() error = %v, want ErrMembershipNotExtended", err)
	}
}

func TestApplyGrantUpgradesDurationToLifetime(t *testing.T) {
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	currentExpiry := now.AddDate(0, 0, 15)
	current := &Membership{PlanCode: "pro", PlanRank: 10, StartsAt: now.AddDate(0, 0, -10), ExpiresAt: &currentExpiry}

	got, err := ApplyGrant(now, current, Grant{PlanCode: "pro", PlanRank: 10, Type: GrantLifetime})
	if err != nil {
		t.Fatalf("ApplyGrant() error = %v", err)
	}
	if got.ExpiresAt != nil {
		t.Fatalf("ExpiresAt = %v, want nil lifetime expiry", got.ExpiresAt)
	}
	if !got.StartsAt.Equal(current.StartsAt) {
		t.Fatalf("StartsAt = %v, want %v", got.StartsAt, current.StartsAt)
	}
}
