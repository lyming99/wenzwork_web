package remotecontrol

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestDeviceLinkGrantWindowDefaultsToPersistentCredential(t *testing.T) {
	now := time.Date(2026, time.August, 24, 20, 0, 0, 0, time.UTC)
	maximumLifetime, expiresAt := deviceLinkGrantWindow(now, DefaultDeviceLinkGrantTTL)
	if maximumLifetime != 0 || !remoteauth.IsPersistentDeviceLinkGrantExpiry(expiresAt) {
		t.Fatalf("default Grant window = %d, %s; want persistent", maximumLifetime, expiresAt)
	}

	maximumLifetime, expiresAt = deviceLinkGrantWindow(now, 90*time.Second)
	if maximumLifetime != 90 || !expiresAt.Equal(now.Add(90*time.Second)) {
		t.Fatalf("bounded Grant window = %d, %s", maximumLifetime, expiresAt)
	}
}

func TestDirectDeviceLinkTargetUsesStableRouteIdentity(t *testing.T) {
	deviceID := uuid.New()
	first, err := directDeviceLinkTarget(deviceID, "192.0.2.45", 9443, 19, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := directDeviceLinkTarget(deviceID, "::ffff:192.0.2.45", 9443, 19, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ConnectionMode != "direct" || first.URL != "ws://192.0.2.45:9443/v2/connect" || first.ConnectionEpoch != 19 ||
		first.NodeID == uuid.Nil || first.CellID == uuid.Nil || first.NodeID != second.NodeID || first.CellID != second.CellID || first.URL != second.URL {
		t.Fatalf("direct targets = %+v / %+v", first, second)
	}
	secure, err := directDeviceLinkTarget(deviceID, "192.0.2.45", 9443, 19, true)
	if err != nil || secure.URL != "wss://192.0.2.45:9443/v2/connect" {
		t.Fatalf("secure direct target = %+v, %v", secure, err)
	}
	if _, err := directDeviceLinkTarget(deviceID, "0.0.0.0", 9443, 19, false); err == nil {
		t.Fatal("unspecified direct address was accepted")
	}
	if _, err := directDeviceLinkTarget(deviceID, "192.0.2.45", 0, 19, false); err == nil {
		t.Fatal("zero direct port was accepted")
	}
}
