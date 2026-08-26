package membership

import (
	"errors"
	"fmt"
	"time"
)

type GrantType string

const (
	GrantDuration GrantType = "duration"
	GrantLifetime GrantType = "lifetime"
)

var ErrMembershipNotExtended = errors.New("grant would not extend membership")

type Membership struct {
	PlanCode  string     `json:"planCode"`
	PlanRank  int        `json:"planRank"`
	StartsAt  time.Time  `json:"startsAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

type Grant struct {
	PlanCode string
	PlanRank int
	Type     GrantType
	Days     int
}

func ApplyGrant(now time.Time, current *Membership, grant Grant) (Membership, error) {
	if grant.PlanCode == "" || grant.PlanRank < 0 {
		return Membership{}, errors.New("membership grant requires a valid plan")
	}
	if grant.Type != GrantDuration && grant.Type != GrantLifetime {
		return Membership{}, fmt.Errorf("unsupported grant type %q", grant.Type)
	}
	if grant.Type == GrantDuration && grant.Days <= 0 {
		return Membership{}, errors.New("duration grant requires positive days")
	}

	active := current != nil && (current.ExpiresAt == nil || current.ExpiresAt.After(now))
	if active && current.PlanRank > grant.PlanRank {
		return Membership{}, ErrMembershipNotExtended
	}
	if active && current.ExpiresAt == nil {
		return Membership{}, ErrMembershipNotExtended
	}

	if grant.Type == GrantLifetime {
		startsAt := now
		if active && current.PlanCode == grant.PlanCode {
			startsAt = current.StartsAt
		}
		return Membership{
			PlanCode:  grant.PlanCode,
			PlanRank:  grant.PlanRank,
			StartsAt:  startsAt,
			ExpiresAt: nil,
		}, nil
	}

	base := now
	startsAt := now
	if active && current.PlanCode == grant.PlanCode && current.ExpiresAt != nil {
		base = *current.ExpiresAt
		startsAt = current.StartsAt
	}
	expiresAt := base.AddDate(0, 0, grant.Days)
	return Membership{
		PlanCode:  grant.PlanCode,
		PlanRank:  grant.PlanRank,
		StartsAt:  startsAt,
		ExpiresAt: &expiresAt,
	}, nil
}
