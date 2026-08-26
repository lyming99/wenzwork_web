package relayallocation

import (
	"errors"
	"testing"
	"time"
)

func healthyCell(id string, connections int64) Cell {
	return Cell{
		ID: id, Region: "cn-hangzhou", Pool: "standard", Status: CellActive,
		Endpoint: "wss://" + id + ".relay.example/v1/connect", EndpointRevision: 1, EndpointActive: true,
		ProtocolMin: 1, ProtocolMax: 1, Weight: 1, ActiveConnections: connections,
		ConnectionSoftLimit: 1000, ConnectionHardLimit: 1200, EgressSoftLimitMbps: 1000,
		MemorySoftLimitBytes: 1 << 30, WriteLoopLagLimit: 100,
		Nodes: []Node{{ID: id + "-0", Healthy: true}, {ID: id + "-1", Healthy: true}},
	}
}

func TestSchedulerKeepsHealthyCurrentCellAndVersion(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	current := Assignment{ID: "assignment-1", UserID: "user-1", CellID: "r017", Version: 7, Mode: "auto"}
	request := Request{UserID: "user-1", Region: "cn-hangzhou", Pool: "standard", ProtocolVersion: 1, Current: &current, Now: now}
	first, err := (Scheduler{}).Allocate(request, []Cell{healthyCell("r017", 700), healthyCell("r018", 10)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Scheduler{}).Allocate(request, []Cell{healthyCell("r018", 10), healthyCell("r017", 700)})
	if err != nil {
		t.Fatal(err)
	}
	if first.CellID != "r017" || second.CellID != "r017" || first.Version != 7 || second.Version != 7 {
		t.Fatalf("renewed assignments = %+v / %+v", first, second)
	}
}

func TestSchedulerMigratesFromDrainingCellAndIncrementsFence(t *testing.T) {
	now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
	current := Assignment{ID: "assignment-1", UserID: "user-1", CellID: "r017", Version: 7, Mode: "auto"}
	draining := healthyCell("r017", 100)
	draining.Status = CellDraining
	assignment, err := (Scheduler{}).Allocate(Request{
		UserID: "user-1", Region: "cn-hangzhou", Pool: "standard", ProtocolVersion: 1,
		Current: &current, Now: now, AssignmentID: func() string { return "assignment-2" },
	}, []Cell{draining, healthyCell("r018", 200)})
	if err != nil {
		t.Fatal(err)
	}
	if assignment.CellID != "r018" || assignment.Version != 8 || assignment.ID != "assignment-2" {
		t.Fatalf("assignment = %+v", assignment)
	}
}

func TestSchedulerPinCannotOverrideAvailability(t *testing.T) {
	pinned := healthyCell("r017", 100)
	pinned.Nodes[0].Healthy = false
	assignment, err := (Scheduler{}).Allocate(Request{
		UserID: "user-1", Region: "cn-hangzhou", Pool: "standard", ProtocolVersion: 1, PinnedCellID: "r017",
	}, []Cell{pinned, healthyCell("r018", 200)})
	if err != nil {
		t.Fatal(err)
	}
	if assignment.CellID != "r018" || assignment.Mode != "pinned" {
		t.Fatalf("assignment = %+v", assignment)
	}
}

func TestSchedulerRejectsHardWatermarkAndInvalidEndpoint(t *testing.T) {
	overloaded := healthyCell("r017", 1080)
	invalidEndpoint := healthyCell("r018", 0)
	invalidEndpoint.Endpoint = "https://r018.example/v1/connect"
	_, err := (Scheduler{}).Allocate(Request{
		UserID: "user-1", Region: "cn-hangzhou", Pool: "standard", ProtocolVersion: 1,
	}, []Cell{overloaded, invalidEndpoint})
	if !errors.Is(err, ErrNoSchedulableCell) {
		t.Fatalf("Allocate() error = %v", err)
	}
}

func TestSchedulerAcceptsPlainWebSocketEndpoint(t *testing.T) {
	cell := healthyCell("plain", 0)
	cell.Endpoint = "ws://127.0.0.1:8443/v1/connect"
	result, err := (Scheduler{}).Allocate(Request{
		UserID: "user-1", Region: cell.Region, Pool: cell.Pool, ProtocolVersion: 1, MinimumHealthyN: 1,
	}, []Cell{cell})
	if err != nil {
		t.Fatalf("Allocate() error = %v", err)
	}
	if result.Endpoint != cell.Endpoint {
		t.Fatalf("Allocate() endpoint = %q, want %q", result.Endpoint, cell.Endpoint)
	}
}
