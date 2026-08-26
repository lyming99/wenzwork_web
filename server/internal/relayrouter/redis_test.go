package relayrouter

import (
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisRegistryFailsClosedWhenRedisIsUnavailable(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: 10 * time.Millisecond,
		ReadTimeout: 10 * time.Millisecond, WriteTimeout: 10 * time.Millisecond, MaxRetries: 0,
	})
	registry, err := NewRedisRegistry(client, "relay:test-unavailable", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registry.timeout = 50 * time.Millisecond
	t.Cleanup(func() { _ = registry.Close() })
	err = registry.PutGrantFence("device-1", GrantFence{Version: 1, Status: DeviceActive})
	if !errors.Is(err, ErrFenceUnavailable) {
		t.Fatalf("PutGrantFence() error = %v, want ErrFenceUnavailable", err)
	}
	err = registry.PutCellCapacity("cell-1", CellCapacity{
		Version: 1, Status: CapacityActive, ActiveConnections: 1, HardLimit: 10, UpdatedAt: time.Now().UTC(),
	})
	if !errors.Is(err, ErrCapacityUnavailable) {
		t.Fatalf("PutCellCapacity() error = %v, want ErrCapacityUnavailable", err)
	}
}

func TestRedisRegistryRejectsHashTagAndSubjectInjection(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	registry, err := NewRedisRegistry(client, "relay:test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	if err := registry.PutGrantFence("device}evil", GrantFence{Version: 1, Status: DeviceActive}); !errors.Is(err, ErrGrantStale) {
		t.Fatalf("injected device ID error = %v", err)
	}
	if _, err := NewRedisRegistry(client, "relay:{shared}", time.Hour); err == nil {
		t.Fatal("NewRedisRegistry accepted a prefix containing a Redis hash tag")
	}
	if err := registry.ReserveCellCapacity("cell}evil", "reservation-1", 1, time.Now().UTC(), time.Minute, time.Minute); !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("injected Cell ID error = %v", err)
	}
}
