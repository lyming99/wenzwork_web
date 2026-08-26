package relayrouter

import (
	"crypto/ed25519"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"
)

var (
	ErrRouteStoreUnavailable = errors.New("route registry unavailable")
	ErrFenceUnavailable      = errors.New("admission fence unavailable")
	ErrAssignmentStale       = errors.New("assignment fence mismatch")
	ErrGrantStale            = errors.New("grant fence mismatch")
	ErrConnectionStale       = errors.New("connection epoch is stale")
	ErrRouteNotFound         = errors.New("device route not found")
	ErrPeerTicketReplay      = errors.New("Peer Session Ticket was already used")
	// These two sentinels preserve the control plane's fail-closed response
	// semantics when one atomic Redis operation validates both a target
	// credential and its live route.
	ErrPeerCredentialValidation = errors.New("Peer device credential validation failed")
	ErrPeerRouteValidation      = errors.New("Peer device route validation failed")
)

type DeviceStatus string

const (
	DeviceActive      DeviceStatus = "active"
	DeviceRevoked     DeviceStatus = "revoked"
	DeviceQuarantined DeviceStatus = "quarantined"
)

type AssignmentFence struct {
	Version        uint64
	AllowedCellIDs []string
}

type GrantFence struct {
	Version uint64
	Status  DeviceStatus
}

type DeviceCredential struct {
	Version   uint64
	Status    DeviceStatus
	PublicKey ed25519.PublicKey
}

type Route struct {
	DeviceID          string
	UserID            string
	CellID            string
	NodeID            string
	ConnectionID      string
	ConnectionEpoch   uint64
	AssignmentVersion uint64
	GrantVersion      uint64
	ProtocolVersion   uint32
	ConnectedAt       time.Time
	LastHeartbeatAt   time.Time
	ExpiresAt         time.Time
}

type Registry struct {
	mu               sync.Mutex
	fenceAvailable   bool
	assignmentFences map[string]AssignmentFence
	grantFences      map[string]GrantFence
	routes           map[string]Route
}

func NewRegistry() *Registry {
	return &Registry{
		fenceAvailable: true, assignmentFences: make(map[string]AssignmentFence),
		grantFences: make(map[string]GrantFence), routes: make(map[string]Route),
	}
}

func (r *Registry) SetFenceAvailable(available bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fenceAvailable = available
}

func (r *Registry) PutAssignmentFence(userID string, fence AssignmentFence) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fence.AllowedCellIDs = slices.Clone(fence.AllowedCellIDs)
	r.assignmentFences[userID] = fence
}

func (r *Registry) PutGrantFence(deviceID string, fence GrantFence) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grantFences[deviceID] = fence
}

func (r *Registry) Register(route Route, ttl time.Duration, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateFenceLocked(route); err != nil {
		return err
	}
	if current, ok := r.routes[route.DeviceID]; ok && current.ExpiresAt.After(now) && route.ConnectionEpoch <= current.ConnectionEpoch {
		return ErrConnectionStale
	}
	if ttl <= 0 {
		return errors.New("route TTL must be positive")
	}
	route.ConnectedAt = now.UTC()
	route.LastHeartbeatAt = now.UTC()
	route.ExpiresAt = now.Add(ttl).UTC()
	r.routes[route.DeviceID] = route
	return nil
}

func (r *Registry) Renew(deviceID, connectionID string, epoch uint64, ttl time.Duration, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.routes[deviceID]
	if !ok || !current.ExpiresAt.After(now) {
		delete(r.routes, deviceID)
		return ErrRouteNotFound
	}
	if current.ConnectionID != connectionID || current.ConnectionEpoch != epoch {
		return ErrConnectionStale
	}
	if err := r.validateFenceLocked(current); err != nil {
		delete(r.routes, deviceID)
		return err
	}
	if ttl <= 0 {
		return errors.New("route TTL must be positive")
	}
	current.LastHeartbeatAt = now.UTC()
	current.ExpiresAt = now.Add(ttl).UTC()
	r.routes[deviceID] = current
	return nil
}

func (r *Registry) Resolve(deviceID string, now time.Time) (Route, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.routes[deviceID]
	if !ok || !current.ExpiresAt.After(now) {
		delete(r.routes, deviceID)
		return Route{}, ErrRouteNotFound
	}
	if err := r.validateFenceLocked(current); err != nil {
		delete(r.routes, deviceID)
		return Route{}, err
	}
	return current, nil
}

func (r *Registry) CompareAndDelete(deviceID, connectionID string, epoch uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.routes[deviceID]
	if !ok || current.ConnectionID != connectionID || current.ConnectionEpoch != epoch {
		return false
	}
	delete(r.routes, deviceID)
	return true
}

func (r *Registry) Snapshot(now time.Time) map[string]Route {
	r.mu.Lock()
	defer r.mu.Unlock()
	for deviceID, route := range r.routes {
		if !route.ExpiresAt.After(now) {
			delete(r.routes, deviceID)
		}
	}
	return maps.Clone(r.routes)
}

func (r *Registry) validateFenceLocked(route Route) error {
	if !r.fenceAvailable {
		return ErrFenceUnavailable
	}
	assignment, ok := r.assignmentFences[route.UserID]
	if !ok || assignment.Version != route.AssignmentVersion || !slices.Contains(assignment.AllowedCellIDs, route.CellID) {
		return ErrAssignmentStale
	}
	grant, ok := r.grantFences[route.DeviceID]
	if !ok || grant.Version != route.GrantVersion || grant.Status != DeviceActive {
		return ErrGrantStale
	}
	return nil
}
