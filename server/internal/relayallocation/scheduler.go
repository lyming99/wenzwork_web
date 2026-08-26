package relayallocation

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

var ErrNoSchedulableCell = errors.New("no schedulable relay cell")

type CellStatus string

const (
	CellActive   CellStatus = "active"
	CellDraining CellStatus = "draining"
	CellDisabled CellStatus = "disabled"
)

type Node struct {
	ID      string
	Healthy bool
}

type Cell struct {
	ID                    string
	Region                string
	Pool                  string
	Status                CellStatus
	Endpoint              string
	EndpointRevision      uint64
	EndpointActive        bool
	ProtocolMin           uint32
	ProtocolMax           uint32
	Weight                float64
	ActiveConnections     int64
	ConnectionSoftLimit   int64
	ConnectionHardLimit   int64
	EgressMbps            float64
	EgressSoftLimitMbps   float64
	MemoryBytes           int64
	MemorySoftLimitBytes  int64
	WriteLoopLagMillisP99 float64
	WriteLoopLagLimit     float64
	Nodes                 []Node
}

func (c Cell) HealthyNodeCount() int {
	count := 0
	for _, node := range c.Nodes {
		if node.Healthy {
			count++
		}
	}
	return count
}

func (c Cell) HardUtilization() float64 {
	return ratio(float64(c.ActiveConnections), float64(c.ConnectionHardLimit))
}

func (c Cell) LoadScore() float64 {
	return 0.45*ratio(float64(c.ActiveConnections), float64(c.ConnectionSoftLimit)) +
		0.25*ratio(c.EgressMbps, c.EgressSoftLimitMbps) +
		0.20*ratio(float64(c.MemoryBytes), float64(c.MemorySoftLimitBytes)) +
		0.10*ratio(c.WriteLoopLagMillisP99, c.WriteLoopLagLimit)
}

func ratio(value, limit float64) float64 {
	if limit <= 0 {
		return math.Inf(1)
	}
	if value < 0 {
		return 0
	}
	return value / limit
}

type Assignment struct {
	ID               string
	UserID           string
	CellID           string
	Version          uint64
	Mode             string
	LeaseExpiresAt   time.Time
	Endpoint         string
	EndpointRevision uint64
	FallbackCellIDs  []string
}

type Request struct {
	UserID           string
	Region           string
	Pool             string
	ProtocolVersion  uint32
	PinnedCellID     string
	Current          *Assignment
	Now              time.Time
	LeaseDuration    time.Duration
	CandidateSetSize int
	MinimumHealthyN  int
	AssignmentID     func() string
}

type Scheduler struct{}

func (Scheduler) Allocate(request Request, cells []Cell) (Assignment, error) {
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	if request.LeaseDuration <= 0 {
		request.LeaseDuration = 24 * time.Hour
	}
	if request.CandidateSetSize <= 0 {
		request.CandidateSetSize = 5
	}
	if request.MinimumHealthyN <= 0 {
		request.MinimumHealthyN = 2
	}
	if request.AssignmentID == nil {
		request.AssignmentID = func() string { return request.UserID + ":v1" }
	}

	eligible := make([]rankedCell, 0, len(cells))
	for _, cell := range cells {
		if !eligibleCell(request, cell) {
			continue
		}
		eligible = append(eligible, rankedCell{cell: cell, rendezvous: rendezvousRank(request.UserID, cell.ID, cell.Weight)})
	}
	if len(eligible) == 0 {
		return Assignment{}, ErrNoSchedulableCell
	}

	if request.Current != nil && request.PinnedCellID == "" {
		for _, candidate := range eligible {
			if candidate.cell.ID == request.Current.CellID {
				return renewAssignment(*request.Current, candidate.cell, request.Now.Add(request.LeaseDuration), fallbackIDs(eligible, candidate.cell.ID)), nil
			}
		}
	}

	mode := "auto"
	if request.PinnedCellID != "" {
		mode = "pinned"
		for _, candidate := range eligible {
			if candidate.cell.ID == request.PinnedCellID {
				return newAssignment(request, candidate.cell, mode, fallbackIDs(eligible, candidate.cell.ID)), nil
			}
		}
		// A pin is an affinity hint, not an availability override. Fall through to
		// automatic selection when the pinned Cell is unhealthy or draining.
	}

	slices.SortFunc(eligible, func(a, b rankedCell) int {
		return compareFloat(a.rendezvous, b.rendezvous)
	})
	if len(eligible) > request.CandidateSetSize {
		eligible = eligible[:request.CandidateSetSize]
	}
	slices.SortFunc(eligible, func(a, b rankedCell) int {
		if comparison := compareFloat(a.cell.LoadScore(), b.cell.LoadScore()); comparison != 0 {
			return comparison
		}
		return compareFloat(a.rendezvous, b.rendezvous)
	})
	selected := eligible[0].cell
	return newAssignment(request, selected, mode, fallbackIDs(eligible, selected.ID)), nil
}

type rankedCell struct {
	cell       Cell
	rendezvous float64
}

func eligibleCell(request Request, cell Cell) bool {
	return cell.Status == CellActive && cell.EndpointActive && (strings.HasPrefix(cell.Endpoint, "ws://") || strings.HasPrefix(cell.Endpoint, "wss://")) &&
		cell.Region == request.Region && cell.Pool == request.Pool &&
		request.ProtocolVersion >= cell.ProtocolMin && request.ProtocolVersion <= cell.ProtocolMax &&
		cell.HealthyNodeCount() >= request.MinimumHealthyN && cell.HardUtilization() < 0.90
}

func rendezvousRank(userID, cellID string, weight float64) float64 {
	if weight <= 0 {
		weight = 1
	}
	digest := sha256.Sum256([]byte(userID + "\x00" + cellID))
	raw := binary.BigEndian.Uint64(digest[:8])
	u := (float64(raw) + 1) / (float64(math.MaxUint64) + 2)
	return -math.Log(u) / weight
}

func compareFloat(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func newAssignment(request Request, cell Cell, mode string, fallbacks []string) Assignment {
	version := uint64(1)
	if request.Current != nil {
		version = request.Current.Version + 1
	}
	return Assignment{
		ID: request.AssignmentID(), UserID: request.UserID, CellID: cell.ID, Version: version,
		Mode: mode, LeaseExpiresAt: request.Now.Add(request.LeaseDuration), Endpoint: cell.Endpoint,
		EndpointRevision: cell.EndpointRevision, FallbackCellIDs: fallbacks,
	}
}

func renewAssignment(current Assignment, cell Cell, expiresAt time.Time, fallbacks []string) Assignment {
	current.LeaseExpiresAt = expiresAt
	current.Endpoint = cell.Endpoint
	current.EndpointRevision = cell.EndpointRevision
	current.FallbackCellIDs = fallbacks
	return current
}

func fallbackIDs(candidates []rankedCell, primaryID string) []string {
	fallbacks := make([]string, 0, 2)
	for _, candidate := range candidates {
		if candidate.cell.ID == primaryID || slices.Contains(fallbacks, candidate.cell.ID) {
			continue
		}
		fallbacks = append(fallbacks, candidate.cell.ID)
		if len(fallbacks) == 2 {
			break
		}
	}
	return fallbacks
}
