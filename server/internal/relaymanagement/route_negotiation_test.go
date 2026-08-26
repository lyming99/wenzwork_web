package relaymanagement

import (
	"testing"

	"github.com/google/uuid"
)

func TestValidateHeartbeatRouteShapesRequiresV2(t *testing.T) {
	route := HeartbeatRoute{
		DeviceID: uuid.NewString(), UserID: uuid.NewString(), ConnectionID: uuid.NewString(),
		ConnectionEpoch: 1, AssignmentVersion: 1, GrantVersion: 1, ProtocolVersion: 2,
	}
	input := HeartbeatInput{ActiveConnections: 1, ResidentRoutes: []HeartbeatRoute{route}}
	if err := validateHeartbeatRouteShapes(input); err != nil {
		t.Fatalf("validateHeartbeatRouteShapes(v2) error = %v", err)
	}
	input.ResidentRoutes[0].ProtocolVersion = 1
	if err := validateHeartbeatRouteShapes(input); err == nil {
		t.Fatal("validateHeartbeatRouteShapes() accepted a legacy route")
	}
}
