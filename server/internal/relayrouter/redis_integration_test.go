//go:build integration

package relayrouter

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestRedisRegistryLuaCASAndFenceLifecycle(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if rawURL == "" {
		t.Skip("REDIS_URL is not set")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	prefix := "relay:test:" + strings.ReplaceAll(uuid.NewString(), "-", "")
	registry, err := NewRedisRegistry(client, prefix, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	deviceID, userID, cellID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	otherCellID, nodeID := uuid.NewString(), uuid.NewString()
	firstConnectionID, secondConnectionID := uuid.NewString(), uuid.NewString()
	t.Cleanup(func() {
		_ = client.Del(t.Context(), append(registry.routeKeys(deviceID), registry.credentialKey(deviceID))...).Err()
	})
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.PutDeviceCredential(deviceID, DeviceCredential{Version: 3, Status: DeviceActive, PublicKey: publicKey}); err != nil {
		t.Fatalf("PutDeviceCredential() error = %v", err)
	}
	thumbprint := remoteauth.PublicKeyThumbprint(publicKey)
	resolvedKey, err := registry.ResolveDeviceKey(t.Context(), deviceID, thumbprint)
	if err != nil || !resolvedKey.Equal(publicKey) {
		t.Fatalf("ResolveDeviceKey() = %v, %v", resolvedKey, err)
	}
	if err := registry.PutDeviceCredential(deviceID, DeviceCredential{Version: 2, Status: DeviceActive, PublicKey: publicKey}); !errors.Is(err, ErrGrantStale) {
		t.Fatalf("stale Device Credential error = %v", err)
	}

	if err := registry.PutAssignmentFence(userID, deviceID, AssignmentFence{Version: 7, AllowedCellIDs: []string{cellID, otherCellID}}); err != nil {
		t.Fatalf("PutAssignmentFence() error = %v", err)
	}
	if err := registry.PutGrantFence(deviceID, GrantFence{Version: 3, Status: DeviceActive}); err != nil {
		t.Fatalf("PutGrantFence() error = %v", err)
	}
	if err := registry.VerifyAdmissionState(t.Context(), userID, deviceID, 7, 3, []string{cellID, otherCellID}, thumbprint); err != nil {
		t.Fatalf("VerifyAdmissionState() error = %v", err)
	}
	peerPublicKey, err := registry.VerifyPeerDeviceState(t.Context(), deviceID, 3, thumbprint)
	if err != nil || !peerPublicKey.Equal(publicKey) {
		t.Fatalf("VerifyPeerDeviceState() = %v, %v", peerPublicKey, err)
	}
	if err := registry.VerifyAdmissionState(t.Context(), userID, deviceID, 7, 3, []string{cellID}, thumbprint); !errors.Is(err, ErrAssignmentStale) {
		t.Fatalf("VerifyAdmissionState(stale cells) error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	peerTicketJWTID := uuid.NewString()
	if err := registry.ConsumePeerTicket(t.Context(), peerTicketJWTID, now.Add(time.Minute), now); err != nil {
		t.Fatalf("ConsumePeerTicket(first) error = %v", err)
	}
	if err := registry.ConsumePeerTicket(t.Context(), peerTicketJWTID, now.Add(time.Minute), now); !errors.Is(err, ErrPeerTicketReplay) {
		t.Fatalf("ConsumePeerTicket(replay) error = %v", err)
	}
	peerTicketKeys, err := client.Keys(t.Context(), prefix+":peer-ticket-jti:*").Result()
	if err != nil || len(peerTicketKeys) != 1 || strings.Contains(peerTicketKeys[0], peerTicketJWTID) {
		t.Fatalf("Peer Ticket replay marker keys = %v, %v", peerTicketKeys, err)
	}
	if marker, err := client.Get(t.Context(), peerTicketKeys[0]).Result(); err != nil || marker != "used" {
		t.Fatalf("Peer Ticket replay marker = %q, %v", marker, err)
	}
	firstEpoch := uint64(9_007_199_254_740_993)
	firstRoute := Route{
		DeviceID: deviceID, UserID: userID, CellID: cellID, NodeID: nodeID,
		ConnectionID: firstConnectionID, ConnectionEpoch: firstEpoch,
		AssignmentVersion: 7, GrantVersion: 3, ProtocolVersion: 1,
	}
	if err := registry.Register(firstRoute, time.Minute, now); err != nil {
		t.Fatalf("Register(first) error = %v", err)
	}
	if err := registry.Register(firstRoute, time.Minute, now.Add(time.Millisecond)); !errors.Is(err, ErrConnectionStale) {
		t.Fatalf("Register(same epoch) error = %v", err)
	}
	secondRoute := firstRoute
	secondRoute.ConnectionID = secondConnectionID
	secondRoute.ConnectionEpoch++
	if err := registry.Register(secondRoute, time.Minute, now.Add(time.Second)); err != nil {
		t.Fatalf("Register(second) error = %v", err)
	}
	if err := registry.Renew(deviceID, firstConnectionID, firstEpoch, time.Minute, now.Add(2*time.Second)); !errors.Is(err, ErrConnectionStale) {
		t.Fatalf("Renew(old route) error = %v", err)
	}
	if registry.CompareAndDelete(deviceID, firstConnectionID, firstEpoch) {
		t.Fatal("old connection deleted the new Redis Route")
	}
	resolved, err := registry.Resolve(deviceID, now.Add(3*time.Second))
	if err != nil || resolved.ConnectionID != secondConnectionID || resolved.ConnectionEpoch != secondRoute.ConnectionEpoch || resolved.NodeID != nodeID {
		t.Fatalf("Resolve() = %+v, %v", resolved, err)
	}
	verifiedPeerKey, verifiedRoute, err := registry.ResolveVerifiedPeerRoute(t.Context(), deviceID, 3, thumbprint, now.Add(3*time.Second))
	if err != nil || !verifiedPeerKey.Equal(publicKey) || verifiedRoute.ConnectionID != secondConnectionID || verifiedRoute.ConnectionEpoch != secondRoute.ConnectionEpoch || verifiedRoute.NodeID != nodeID {
		t.Fatalf("ResolveVerifiedPeerRoute() = key:%v route:%+v err:%v", verifiedPeerKey, verifiedRoute, err)
	}
	if _, _, err := registry.ResolveVerifiedPeerRoute(t.Context(), deviceID, 3, "wrong-thumbprint", now.Add(3*time.Second)); !errors.Is(err, ErrPeerCredentialValidation) || !errors.Is(err, ErrGrantStale) {
		t.Fatalf("ResolveVerifiedPeerRoute(stale credential) error = %v", err)
	}
	if err := registry.Renew(deviceID, secondConnectionID, secondRoute.ConnectionEpoch, time.Minute, now.Add(4*time.Second)); err != nil {
		t.Fatalf("Renew(current route) error = %v", err)
	}

	if err := registry.PutAssignmentFence(userID, deviceID, AssignmentFence{Version: 6, AllowedCellIDs: []string{cellID}}); !errors.Is(err, ErrAssignmentStale) {
		t.Fatalf("older Assignment Fence error = %v", err)
	}
	if err := registry.PutAssignmentFence(userID, deviceID, AssignmentFence{Version: 8, AllowedCellIDs: []string{otherCellID}}); err != nil {
		t.Fatalf("new Assignment Fence error = %v", err)
	}
	if _, err := registry.Resolve(deviceID, now.Add(5*time.Second)); !errors.Is(err, ErrAssignmentStale) {
		t.Fatalf("Resolve(stale assignment) error = %v", err)
	}
	if _, err := registry.Resolve(deviceID, now.Add(5*time.Second)); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("stale Route was not removed: %v", err)
	}

	if err := registry.PutAssignmentFence(userID, deviceID, AssignmentFence{Version: 9, AllowedCellIDs: []string{cellID}}); err != nil {
		t.Fatal(err)
	}
	secondRoute.AssignmentVersion = 9
	secondRoute.ConnectionEpoch++
	if err := registry.Register(secondRoute, time.Minute, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := registry.PutGrantFence(deviceID, GrantFence{Version: 4, Status: DeviceRevoked}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resolve(deviceID, now.Add(7*time.Second)); !errors.Is(err, ErrGrantStale) {
		t.Fatalf("Resolve(revoked grant) error = %v", err)
	}
	if err := registry.PutDeviceCredential(deviceID, DeviceCredential{Version: 4, Status: DeviceRevoked, PublicKey: publicKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ResolveDeviceKey(t.Context(), deviceID, thumbprint); !errors.Is(err, ErrGrantStale) {
		t.Fatalf("ResolveDeviceKey(revoked) error = %v", err)
	}
}

func TestNegotiatedRegistryPublishesFullNodeSnapshotWithoutFences(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if rawURL == "" {
		t.Skip("REDIS_URL is not set")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	prefix := "relay:negotiated-test:" + strings.ReplaceAll(uuid.NewString(), "-", "")
	legacy, err := NewRedisRegistry(client, prefix, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registry := &NegotiatedRegistry{registry: legacy}
	t.Cleanup(func() {
		keys, _ := client.Keys(t.Context(), prefix+"*").Result()
		if len(keys) > 0 {
			_ = client.Del(t.Context(), keys...).Err()
		}
		_ = client.Close()
	})

	now := time.Now().UTC().Truncate(time.Millisecond)
	deviceID, userID, cellID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	newNodeID, oldNodeID := uuid.NewString(), uuid.NewString()
	newRoute := Route{
		DeviceID: deviceID, UserID: userID, CellID: cellID, NodeID: newNodeID,
		ConnectionID: uuid.NewString(), ConnectionEpoch: 2, AssignmentVersion: 4, GrantVersion: 3, ProtocolVersion: 1,
	}
	if err := registry.Publish(t.Context(), newNodeID, []Route{newRoute}, time.Minute, now); err != nil {
		t.Fatalf("Publish(new route) error = %v", err)
	}
	resolved, err := registry.ResolveContext(t.Context(), deviceID, now)
	if err != nil || resolved.ConnectionID != newRoute.ConnectionID || resolved.ConnectionEpoch != 2 {
		t.Fatalf("ResolveContext(new route) = %+v, %v", resolved, err)
	}

	staleRoute := newRoute
	staleRoute.NodeID = oldNodeID
	staleRoute.ConnectionID = uuid.NewString()
	staleRoute.ConnectionEpoch = 1
	if err := registry.Publish(t.Context(), oldNodeID, []Route{staleRoute}, time.Minute, now.Add(time.Second)); err != nil {
		t.Fatalf("Publish(stale route snapshot) error = %v", err)
	}
	resolved, err = registry.ResolveContext(t.Context(), deviceID, now.Add(time.Second))
	if err != nil || resolved.ConnectionID != newRoute.ConnectionID || resolved.ConnectionEpoch != 2 {
		t.Fatalf("stale snapshot replaced current route: %+v, %v", resolved, err)
	}

	if err := registry.Publish(t.Context(), newNodeID, nil, time.Minute, now.Add(2*time.Second)); err != nil {
		t.Fatalf("Publish(empty snapshot) error = %v", err)
	}
	if _, err := registry.ResolveContext(t.Context(), deviceID, now.Add(2*time.Second)); !errors.Is(err, ErrRouteNotFound) {
		t.Fatalf("ResolveContext(after omission) error = %v, want ErrRouteNotFound", err)
	}
}

func TestRedisCellCapacityReservationsNeverOverAllocate(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if rawURL == "" {
		t.Skip("REDIS_URL is not set")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	client := redis.NewClient(options)
	registry, err := NewRedisRegistry(client, "relay:test:"+strings.ReplaceAll(uuid.NewString(), "-", ""), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	cellID := uuid.NewString()
	t.Cleanup(func() { _ = client.Del(t.Context(), registry.capacityKeys(cellID)...).Err() })

	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := registry.PutCellCapacity(cellID, CellCapacity{
		Version: 9_007_199_254_740_999, Status: CapacityActive, ActiveConnections: 90,
		HardLimit: 100, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 25)
	for index := 0; index < 25; index++ {
		go func(index int) {
			<-start
			results <- registry.ReserveCellCapacity(cellID, fmt.Sprintf("reservation-%d", index), 1, now, time.Minute, time.Minute)
		}(index)
	}
	close(start)
	successes := 0
	for index := 0; index < 25; index++ {
		err := <-results
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrCapacityExceeded) {
			t.Fatalf("ReserveCellCapacity() error = %v", err)
		}
	}
	if successes != 10 {
		t.Fatalf("successful reservations = %d, want 10", successes)
	}
	if err := registry.PutCellCapacity(cellID, CellCapacity{
		Version: 9_007_199_254_740_998, Status: CapacityActive, ActiveConnections: 1,
		HardLimit: 100, UpdatedAt: now,
	}); !errors.Is(err, ErrCapacityStale) {
		t.Fatalf("stale capacity version error = %v", err)
	}
	if err := registry.PutCellCapacity(cellID, CellCapacity{
		Version: 9_007_199_254_740_999, Status: CapacityActive, ActiveConnections: 80,
		HardLimit: 100, UpdatedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("newer capacity observation error = %v", err)
	}
	if err := registry.PutCellCapacity(cellID, CellCapacity{
		Version: 9_007_199_254_740_999, Status: CapacityActive, ActiveConnections: 81,
		HardLimit: 100, UpdatedAt: now,
	}); !errors.Is(err, ErrCapacityStale) {
		t.Fatalf("older capacity observation error = %v", err)
	}
	if err := registry.PutCellCapacity(cellID, CellCapacity{
		Version: 9_007_199_254_740_999, Status: CapacityActive, ActiveConnections: 81,
		HardLimit: 100, UpdatedAt: now.Add(time.Second),
	}); !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("conflicting capacity observation error = %v", err)
	}
}
