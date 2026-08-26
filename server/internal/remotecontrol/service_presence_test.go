package remotecontrol

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrouter"
)

type routeResolverStub struct {
	route relayrouter.Route
	err   error
}

func (stub routeResolverStub) Resolve(string, time.Time) (relayrouter.Route, error) {
	return stub.route, stub.err
}

func TestDevicePresenceUsesLiveRelayRouteInsteadOfAllocationAge(t *testing.T) {
	now := time.Date(2026, time.August, 9, 15, 0, 0, 0, time.UTC)
	userID, deviceID := uuid.New(), uuid.New()
	heartbeatAt := now.Add(-20 * time.Second)
	allocationAt := now.Add(-3 * time.Minute)
	service, err := NewService(ServiceConfig{
		Store:     &Store{},
		CursorKey: []byte("device-presence-test-cursor-key-123"),
		Now:       func() time.Time { return now },
		RouteResolver: routeResolverStub{route: relayrouter.Route{
			DeviceID: deviceID.String(), UserID: userID.String(), LastHeartbeatAt: heartbeatAt, ExpiresAt: now.Add(40 * time.Second),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	device, err := service.deviceFromRow(deviceRow{
		DeviceID: deviceID, UserID: userID, DeviceName: "test-device", Platform: "windows", AgentVersion: "test",
		CredentialStatus: "active", GrantStatus: stringPointer("enabled"), Capabilities: []byte(`[]`), Scopes: []byte(`[]`), GrantVersion: 1, LastAllocationAt: &allocationAt,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if device.Presence != "online" {
		t.Fatalf("presence = %q, want online", device.Presence)
	}
	if device.LastSeenAt == nil || !device.LastSeenAt.Equal(heartbeatAt) {
		t.Fatalf("last seen = %v, want live heartbeat %s", device.LastSeenAt, heartbeatAt)
	}
}

func TestDevicePresenceFailsClosedWhenRouteIsExpired(t *testing.T) {
	now := time.Date(2026, time.August, 9, 15, 0, 0, 0, time.UTC)
	userID, deviceID := uuid.New(), uuid.New()
	allocationAt := now.Add(-time.Minute)
	service, err := NewService(ServiceConfig{
		Store:     &Store{},
		CursorKey: []byte("device-presence-test-cursor-key-123"),
		Now:       func() time.Time { return now },
		RouteResolver: routeResolverStub{route: relayrouter.Route{
			DeviceID: deviceID.String(), UserID: userID.String(), LastHeartbeatAt: now.Add(-time.Minute), ExpiresAt: now,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	device, err := service.deviceFromRow(deviceRow{
		DeviceID: deviceID, UserID: userID, DeviceName: "test-device", Platform: "windows", AgentVersion: "test",
		CredentialStatus: "active", GrantStatus: stringPointer("enabled"), Capabilities: []byte(`[]`), Scopes: []byte(`[]`), GrantVersion: 1, LastAllocationAt: &allocationAt,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if device.Presence != "offline" {
		t.Fatalf("presence = %q, want offline", device.Presence)
	}
	if device.LastSeenAt == nil || !device.LastSeenAt.Equal(allocationAt) {
		t.Fatalf("last seen = %v, want allocation timestamp %s", device.LastSeenAt, allocationAt)
	}
}

func stringPointer(value string) *string { return &value }
