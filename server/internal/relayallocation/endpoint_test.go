package relayallocation

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestValidDirectRelayEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"ws://127.0.0.1:8443/v2/connect",
		"wss://relay.example.test/v2/connect",
	} {
		if !validDirectRelayEndpoint(endpoint) {
			t.Fatalf("validDirectRelayEndpoint(%q) = false", endpoint)
		}
	}
	for _, endpoint := range []string{
		"http://relay.example.test/v2/connect",
		"wss://relay.example.test/other",
		"wss://user@relay.example.test/v2/connect",
		"wss://relay.example.test/v2/connect?unexpected=true",
	} {
		if validDirectRelayEndpoint(endpoint) {
			t.Fatalf("validDirectRelayEndpoint(%q) = true", endpoint)
		}
	}
}

func TestDirectRelayEndpointPreservesCellActivationGate(t *testing.T) {
	cellID := uuid.New()
	cells := []Cell{{
		ID: cellID.String(), Region: "cn-dev", Pool: "standard", Status: CellStatus("draft"),
		ProtocolMin: 2, ProtocolMax: 2, Weight: 1,
		ConnectionSoftLimit: 1000, ConnectionHardLimit: 1200,
		EgressSoftLimitMbps: 1000, MemorySoftLimitBytes: 1, WriteLoopLagLimit: 1000,
		Nodes: []Node{{ID: "relay-1", Healthy: true}},
	}}
	direct := Endpoint{CellID: cellID, EndpointRevision: (uint64(1) << 63) | 7, URL: "ws://203.0.113.17:3091/v2/connect"}
	applyDirectRelayEndpoints(cells, map[uuid.UUID]Endpoint{cellID: direct})
	if !cells[0].EndpointActive || cells[0].Endpoint != direct.URL || cells[0].EndpointRevision != direct.EndpointRevision {
		t.Fatalf("direct endpoint was not applied: %+v", cells[0])
	}

	request := Request{
		UserID: "user-1", Region: "cn-dev", Pool: "standard", ProtocolVersion: 2,
		MinimumHealthyN: 1, AssignmentID: func() string { return uuid.NewString() },
	}
	if _, err := (Scheduler{}).Allocate(request, cells); !errors.Is(err, ErrNoSchedulableCell) {
		t.Fatalf("draft Cell Allocate() error = %v, want %v", err, ErrNoSchedulableCell)
	}

	cells[0].Status = CellActive
	assignment, err := (Scheduler{}).Allocate(request, cells)
	if err != nil || assignment.Endpoint != direct.URL || assignment.EndpointRevision != direct.EndpointRevision {
		t.Fatalf("active Cell Allocate() = %+v, %v", assignment, err)
	}
}
