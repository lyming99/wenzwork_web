package main

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
)

func TestV2OperationJournalPersistsCommittedMutationForReplay(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	state, err := loadOrCreateAgentState(
		filepath.Join(t.TempDir(), "agent-state.json"),
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })

	operationID := uuid.NewString()
	digest := sha256.Sum256([]byte("ai.config.update\x00default\x000"))
	now := time.Date(2026, 8, 24, 10, 53, 9, 0, time.UTC)
	response := &remotev2.RpcResponse{
		OperationId: operationID,
		AttemptId:   uuid.NewString(),
		Payload:     []byte(`{"id":"default","revision":1}`),
	}
	if err := state.business.saveV2Operation(
		t.Context(),
		operationID,
		digest,
		response,
		now,
	); err != nil {
		t.Fatal(err)
	}

	loaded, found, err := state.business.loadV2Operation(
		t.Context(),
		operationID,
		digest,
		now.Add(time.Minute),
	)
	if err != nil || !found || loaded.GetOperationId() != operationID ||
		string(loaded.GetPayload()) != string(response.GetPayload()) {
		t.Fatalf("loadV2Operation() = %+v, %v, %v", loaded, found, err)
	}

	conflictingDigest := sha256.Sum256([]byte("ai.config.update\x00other"))
	if _, _, err := state.business.loadV2Operation(
		t.Context(),
		operationID,
		conflictingDigest,
		now.Add(time.Minute),
	); !errors.Is(err, errRPCIdempotency) {
		t.Fatalf("conflicting operation error = %v", err)
	}

	if _, found, err := state.business.loadV2Operation(
		t.Context(),
		operationID,
		digest,
		now.Add(v2OperationRetention+time.Second),
	); err != nil || found {
		t.Fatalf("expired operation found=%v error=%v", found, err)
	}
}
