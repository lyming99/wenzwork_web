package remotecontrol

import (
	"testing"
	"time"

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
