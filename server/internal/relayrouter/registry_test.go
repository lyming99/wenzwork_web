package relayrouter

import (
	"errors"
	"testing"
	"time"
)

func newTestRegistry() *Registry {
	registry := NewRegistry()
	registry.PutAssignmentFence("user-1", AssignmentFence{Version: 7, AllowedCellIDs: []string{"r017", "r018"}})
	registry.PutGrantFence("device-1", GrantFence{Version: 3, Status: DeviceActive})
	return registry
}

func testRoute(epoch uint64, nodeID string) Route {
	return Route{
		DeviceID: "device-1", UserID: "user-1", CellID: "r017", NodeID: nodeID,
		ConnectionID: nodeID + "-connection", ConnectionEpoch: epoch,
		AssignmentVersion: 7, GrantVersion: 3, ProtocolVersion: 1,
	}
}

func TestRegistryFencesOldConnectionEpoch(t *testing.T) {
	registry := newTestRegistry()
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	if err := registry.Register(testRoute(10, "node-a"), time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(testRoute(11, "node-b"), time.Minute, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Renew("device-1", "node-a-connection", 10, time.Minute, now.Add(2*time.Second)); !errors.Is(err, ErrConnectionStale) {
		t.Fatalf("old Renew() error = %v", err)
	}
	resolved, err := registry.Resolve("device-1", now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.NodeID != "node-b" || resolved.ConnectionEpoch != 11 {
		t.Fatalf("Resolve() = %+v", resolved)
	}
	if registry.CompareAndDelete("device-1", "node-a-connection", 10) {
		t.Fatal("old connection deleted the new route")
	}
}

func TestRegistryFailsClosedWithoutFence(t *testing.T) {
	registry := newTestRegistry()
	registry.SetFenceAvailable(false)
	err := registry.Register(testRoute(1, "node-a"), time.Minute, time.Now())
	if !errors.Is(err, ErrFenceUnavailable) {
		t.Fatalf("Register() error = %v", err)
	}
}

func TestRegistryRevocationInvalidatesExistingRoute(t *testing.T) {
	registry := newTestRegistry()
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	if err := registry.Register(testRoute(1, "node-a"), time.Minute, now); err != nil {
		t.Fatal(err)
	}
	registry.PutGrantFence("device-1", GrantFence{Version: 4, Status: DeviceRevoked})
	_, err := registry.Resolve("device-1", now.Add(time.Second))
	if !errors.Is(err, ErrGrantStale) {
		t.Fatalf("Resolve() error = %v", err)
	}
	if len(registry.Snapshot(now.Add(time.Second))) != 0 {
		t.Fatal("revoked route remained registered")
	}
}

func TestRegistryTTLExpiresCrashStaleRoute(t *testing.T) {
	registry := newTestRegistry()
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	if err := registry.Register(testRoute(1, "node-a"), time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve("device-1", now.Add(time.Minute)); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("Resolve() error = %v", err)
	}
}
