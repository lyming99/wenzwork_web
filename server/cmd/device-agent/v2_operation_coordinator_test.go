package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
)

func TestV2OperationCoordinatorSameDigestExecutesOnce(t *testing.T) {
	state := &agentState{}
	operationID := uuid.NewString()
	digest := sha256.Sum256([]byte("same request"))
	first, owner, err := state.claimV2Operation(operationID, digest)
	if err != nil || !owner || first == nil {
		t.Fatalf("first claim owner=%t claim=%p err=%v", owner, first, err)
	}
	second, owner, err := state.claimV2Operation(operationID, digest)
	if err != nil || owner || second != first {
		t.Fatalf("second claim owner=%t same=%t err=%v", owner, second == first, err)
	}

	waited := make(chan *remotev2.RpcResponse, 1)
	go func() {
		response, waitErr := second.wait(context.Background())
		if waitErr == nil {
			waited <- response
		} else {
			waited <- nil
		}
	}()
	response := &remotev2.RpcResponse{OperationId: operationID, SafeErrorCode: "completed"}
	state.finishV2Operation(operationID, first, response)
	if replay := <-waited; replay == nil || replay.GetOperationId() != operationID || replay.GetSafeErrorCode() != "completed" {
		t.Fatalf("waited response = %#v", replay)
	}
	if count := state.v2InFlightOperationCount(); count != 0 {
		t.Fatalf("in-flight operations = %d, want 0", count)
	}
}

func TestV2OperationCoordinatorRejectsDifferentDigest(t *testing.T) {
	state := &agentState{}
	operationID := uuid.NewString()
	firstDigest := sha256.Sum256([]byte("first request"))
	secondDigest := sha256.Sum256([]byte("different request"))
	claim, owner, err := state.claimV2Operation(operationID, firstDigest)
	if err != nil || !owner {
		t.Fatalf("first claim owner=%t err=%v", owner, err)
	}
	if _, _, err := state.claimV2Operation(operationID, secondDigest); !errors.Is(err, errRPCIdempotency) {
		t.Fatalf("different digest error = %v, want idempotency conflict", err)
	}
	state.finishV2Operation(operationID, claim, nil)
}

func TestV2OperationCoordinatorAppliesDeviceCapacity(t *testing.T) {
	state := &agentState{}
	claims := make(map[string]*v2InFlightOperation, v2MaximumInFlightOperations)
	for index := 0; index < v2MaximumInFlightOperations; index++ {
		operationID := uuid.NewString()
		digest := sha256.Sum256([]byte(operationID))
		claim, owner, err := state.claimV2Operation(operationID, digest)
		if err != nil || !owner {
			t.Fatalf("claim %d owner=%t err=%v", index, owner, err)
		}
		claims[operationID] = claim
	}
	overflowID := uuid.NewString()
	overflowDigest := sha256.Sum256([]byte(overflowID))
	if _, _, err := state.claimV2Operation(overflowID, overflowDigest); !errors.Is(err, errRPCBusy) {
		t.Fatalf("overflow error = %v, want busy", err)
	}
	for operationID, claim := range claims {
		state.finishV2Operation(operationID, claim, nil)
	}
}

func TestV2RPCInFlightLimitsPerLinkAndController(t *testing.T) {
	registry := newV2AgentLinkRegistry(nil)
	releases := make([]func(), 0, v2MaximumRPCsPerLink)
	for index := 0; index < v2MaximumRPCsPerLink; index++ {
		release, ok := registry.tryAcquireRPC("controller-a", "link-a")
		if !ok || release == nil {
			t.Fatalf("link permit %d rejected", index)
		}
		releases = append(releases, release)
	}
	if _, ok := registry.tryAcquireRPC("controller-a", "link-a"); ok {
		t.Fatal("per-Link RPC limit accepted one extra permit")
	}
	if _, ok := registry.tryAcquireRPC("controller-a", "link-b"); ok {
		t.Fatal("per-controller RPC limit was bypassed through another Link")
	}
	for _, release := range releases {
		release()
		release() // Release is idempotent.
	}
	if snapshot := registry.resourceSnapshot(); snapshot.RPCInFlight != 0 {
		t.Fatalf("RPC permits after release = %d", snapshot.RPCInFlight)
	}
}

func TestV2RPCInFlightLimitPerDevice(t *testing.T) {
	registry := newV2AgentLinkRegistry(nil)
	releases := make([]func(), 0, v2MaximumRPCsPerDevice)
	for controller := 0; controller < v2MaximumRPCsPerDevice/v2MaximumRPCsPerController; controller++ {
		for index := 0; index < v2MaximumRPCsPerController; index++ {
			release, ok := registry.tryAcquireRPC(
				"controller-"+string(rune('a'+controller)),
				"link-"+string(rune('a'+controller)),
			)
			if !ok || release == nil {
				t.Fatalf("device permit controller=%d index=%d rejected", controller, index)
			}
			releases = append(releases, release)
		}
	}
	if _, ok := registry.tryAcquireRPC("controller-overflow", "link-overflow"); ok {
		t.Fatal("per-Device RPC limit accepted one extra permit")
	}
	if snapshot := registry.resourceSnapshot(); snapshot.RPCInFlight != v2MaximumRPCsPerDevice {
		t.Fatalf("RPC permits = %d, want %d", snapshot.RPCInFlight, v2MaximumRPCsPerDevice)
	}
	for _, release := range releases {
		release()
	}
}
