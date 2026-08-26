package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
)

func TestV2SideEffectPreparedCanResumeAfterRestart(t *testing.T) {
	state := newV2SideEffectTestState(t)
	value := newV2SideEffectTestMutation("file.mkdir", uuid.NewString())
	if got, err := state.business.prepareV2SideEffect(t.Context(), value); err != nil || got != v2SideEffectPrepared {
		t.Fatalf("first prepare state=%q error=%v", got, err)
	}

	restarted := &businessStore{path: state.business.path, deviceID: state.business.deviceID}
	if got, err := restarted.prepareV2SideEffect(t.Context(), value); err != nil || got != v2SideEffectPrepared {
		t.Fatalf("restart prepare state=%q error=%v", got, err)
	}
	tracker := &v2SideEffectTracker{store: restarted, value: value, state: v2SideEffectPrepared}
	ctx := withV2SideEffectTracker(t.Context(), tracker)
	if err := beginV2SideEffect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := commitV2SideEffect(ctx); err != nil {
		t.Fatal(err)
	}
	if got, found, err := restarted.loadV2SideEffect(t.Context(), value, time.Now().UTC()); err != nil || !found || got != v2SideEffectCommitted {
		t.Fatalf("committed side effect state=%q found=%v error=%v", got, found, err)
	}
}

func TestV2SideEffectStartedAndCommittedFenceRestart(t *testing.T) {
	state := newV2SideEffectTestState(t)
	for _, terminalState := range []v2SideEffectState{v2SideEffectStarted, v2SideEffectCommitted} {
		t.Run(string(terminalState), func(t *testing.T) {
			value := newV2SideEffectTestMutation("terminal.open", uuid.NewString())
			if _, err := state.business.prepareV2SideEffect(t.Context(), value); err != nil {
				t.Fatal(err)
			}
			if err := state.business.transitionV2SideEffect(t.Context(), value, v2SideEffectPrepared, v2SideEffectStarted); err != nil {
				t.Fatal(err)
			}
			if terminalState == v2SideEffectCommitted {
				if err := state.business.transitionV2SideEffect(t.Context(), value, v2SideEffectStarted, v2SideEffectCommitted); err != nil {
					t.Fatal(err)
				}
			}

			restarted := &businessStore{path: state.business.path, deviceID: state.business.deviceID}
			if got, err := restarted.prepareV2SideEffect(t.Context(), value); err != nil || got != terminalState {
				t.Fatalf("restart state=%q error=%v, want %q", got, err, terminalState)
			}
		})
	}
}

func TestV2SideEffectRejectsConflictingDigest(t *testing.T) {
	state := newV2SideEffectTestState(t)
	value := newV2SideEffectTestMutation("file.write-text", uuid.NewString())
	if _, err := state.business.prepareV2SideEffect(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	conflict := value
	conflict.Digest = sha256.Sum256([]byte("different request"))
	conflict.Now = conflict.Now.Add(time.Second)
	if _, err := state.business.prepareV2SideEffect(t.Context(), conflict); !errors.Is(err, errRPCIdempotency) {
		t.Fatalf("conflicting digest error = %v", err)
	}
}

func TestV2OperationResponseAtomicallyClearsSideEffect(t *testing.T) {
	state := newV2SideEffectTestState(t)
	value := newV2SideEffectTestMutation("file.delete", uuid.NewString())
	commitV2SideEffectForTest(t, state.business, value)
	response := &remotev2.RpcResponse{
		OperationId: value.OperationID,
		AttemptId:   uuid.NewString(),
		Payload:     []byte(`{"deleted":true}`),
	}
	if err := state.business.saveV2OperationScoped(t.Context(), value.OperationID, value.Digest, response, time.Now().UTC(), value.Controller, value.Project); err != nil {
		t.Fatal(err)
	}
	if _, found, err := state.business.loadV2SideEffect(t.Context(), value, time.Now().UTC()); err != nil || found {
		t.Fatalf("side effect after response save found=%v error=%v", found, err)
	}
	if stored, found, err := state.business.loadV2Operation(t.Context(), value.OperationID, value.Digest, time.Now().UTC()); err != nil || !found || string(stored.GetPayload()) != string(response.GetPayload()) {
		t.Fatalf("stored response=%#v found=%v error=%v", stored, found, err)
	}
	snapshot := state.business.v2OperationJournalSnapshot(t.Context())
	if snapshot.Rows != 1 || snapshot.SideEffectRows != 0 {
		t.Fatalf("journal snapshot after response save = %+v", snapshot)
	}
}

func TestV2OperationJournalFailureRetainsCommittedSideEffect(t *testing.T) {
	state := newV2SideEffectTestState(t)
	value := newV2SideEffectTestMutation("terminal.open", uuid.NewString())
	commitV2SideEffectForTest(t, state.business, value)
	state.business.operationJournalSaveHook = func() error { return errors.New("injected journal failure") }
	response := &remotev2.RpcResponse{OperationId: value.OperationID, AttemptId: uuid.NewString(), Payload: []byte(`{"opened":true}`)}
	if err := state.business.saveV2OperationScoped(t.Context(), value.OperationID, value.Digest, response, time.Now().UTC(), value.Controller, value.Project); err == nil {
		t.Fatal("operation journal failure was not returned")
	}
	if got, found, err := state.business.loadV2SideEffect(t.Context(), value, time.Now().UTC()); err != nil || !found || got != v2SideEffectCommitted {
		t.Fatalf("side effect after journal failure state=%q found=%v error=%v", got, found, err)
	}
	if _, found, err := state.business.loadV2Operation(t.Context(), value.OperationID, value.Digest, time.Now().UTC()); err != nil || found {
		t.Fatalf("operation response after injected failure found=%v error=%v", found, err)
	}
}

func TestV2SideEffectConfirmedRollbackDeletesIntent(t *testing.T) {
	state := newV2SideEffectTestState(t)
	value := newV2SideEffectTestMutation("project.create", "")
	if _, err := state.business.prepareV2SideEffect(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	tracker := &v2SideEffectTracker{store: state.business, value: value, state: v2SideEffectPrepared}
	ctx := withV2SideEffectTracker(t.Context(), tracker)
	if err := beginV2SideEffect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rollbackV2SideEffect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, err := state.business.loadV2SideEffect(t.Context(), value, time.Now().UTC()); err != nil || found {
		t.Fatalf("rolled-back side effect found=%v error=%v", found, err)
	}
}

func TestV2SideEffectSnapshotAndSweepIncludeIntents(t *testing.T) {
	state := newV2SideEffectTestState(t)
	value := newV2SideEffectTestMutation("agent.environment.update", "")
	if _, err := state.business.prepareV2SideEffect(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	if snapshot := state.business.v2OperationJournalSnapshot(t.Context()); snapshot.SideEffectRows != 1 {
		t.Fatalf("snapshot before sweep = %+v", snapshot)
	}
	if err := state.business.sweepV2Operations(t.Context(), value.Now.Add(v2OperationRetention+time.Second)); err != nil {
		t.Fatal(err)
	}
	if snapshot := state.business.v2OperationJournalSnapshot(t.Context()); snapshot.SideEffectRows != 0 {
		t.Fatalf("snapshot after sweep = %+v", snapshot)
	}
}

func TestV2SideEffectRowsConsumeControllerCapacity(t *testing.T) {
	state := newV2SideEffectTestState(t)
	maintenanceContext, cancel := context.WithTimeout(t.Context(), time.Second)
	state.business.stopV2OperationMaintenance(maintenanceContext)
	cancel()
	db, err := state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(t.Context(), `INSERT INTO remote_v2_side_effects(
		operation_id, request_digest, controller_id, project_id, method, state,
		created_at_ms, updated_at_ms, expires_at_ms
	) VALUES(?,?,?,?,?,'prepared',?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	controllerID := uuid.NewString()
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte("capacity"))
	for index := 0; index < v2OperationMaximumRowsPerController; index++ {
		operationID := fmt.Sprintf("00000000-0000-0000-0000-%012d", index)
		if _, err := statement.ExecContext(t.Context(), operationID, digest[:], controllerID, "", "file.mkdir", now.UnixMilli(), now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
			statement.Close()
			t.Fatalf("insert capacity row %d: %v", index, err)
		}
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := enforceV2OperationJournalCapacity(t.Context(), tx, controllerID, 0); !errors.Is(err, errV2OperationJournalCapacity) {
		t.Fatalf("side-effect capacity error = %v", err)
	}
}

func TestBusinessStoreMigratesV25ToV26SideEffects(t *testing.T) {
	state := newV2SideEffectTestState(t)
	maintenanceContext, cancel := context.WithTimeout(t.Context(), time.Second)
	state.business.stopV2OperationMaintenance(maintenanceContext)
	cancel()
	db, err := state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE remote_v2_side_effects`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 26`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := state.business.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	db, err = state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, tables int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'remote_v2_side_effects'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if version != 26 || tables != 1 {
		t.Fatalf("migrated version=%d side-effect tables=%d", version, tables)
	}
}

func TestV2FileMutationJournalFailureLeavesOneCommittedEffect(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.send")
	t.Cleanup(func() { _ = dispatch.state.close() })
	value := newV2SideEffectTestMutation("file.mkdir", dispatch.requestProjectID)
	if _, err := dispatch.state.business.prepareV2SideEffect(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	tracker := &v2SideEffectTracker{store: dispatch.state.business, value: value, state: v2SideEffectPrepared}
	ctx := withV2SideEffectTracker(withV2OperationMutationContext(t.Context(), value), tracker)
	result, _, err := dispatch.callFileRPC(ctx, "file.mkdir", rpcInput{"parentPath": "", "name": "durable-once"})
	if err != nil {
		t.Fatal(err)
	}
	if state, _, _ := tracker.responseDisposition(); state != v2SideEffectCommitted {
		t.Fatalf("file side-effect state = %q", state)
	}
	dispatch.state.business.operationJournalSaveHook = func() error { return errors.New("injected journal failure") }
	response := &remotev2.RpcResponse{OperationId: value.OperationID, AttemptId: uuid.NewString(), Payload: []byte(`{"created":true}`)}
	if err := dispatch.state.business.saveV2OperationScoped(t.Context(), value.OperationID, value.Digest, response, time.Now().UTC(), value.Controller, value.Project); err == nil {
		t.Fatal("journal failure was not returned")
	}
	if got, err := dispatch.state.business.prepareV2SideEffect(t.Context(), value); err != nil || got != v2SideEffectCommitted {
		t.Fatalf("retry state=%q error=%v", got, err)
	}
	entries, err := os.ReadDir(dispatch.state.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.Name() == "durable-once" && entry.IsDir() {
			count++
		}
	}
	if count != 1 || result == nil {
		t.Fatalf("committed directory count=%d result=%#v", count, result)
	}
}

func TestV2TerminalOpenJournalFailureStartsOneProcess(t *testing.T) {
	fixture := newTerminalTestFixture(t)
	value := newV2SideEffectTestMutation("terminal.open", fixture.project.ID.String())
	if _, err := fixture.state.business.prepareV2SideEffect(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	tracker := &v2SideEffectTracker{store: fixture.state.business, value: value, state: v2SideEffectPrepared}
	ctx := withV2SideEffectTracker(withV2OperationMutationContext(t.Context(), value), tracker)
	opened, err := fixture.service.OpenContext(ctx, fixture.project, rpcInput{
		"clientRequestId": uuid.NewString(), "cwd": "", "shell": "fake", "rows": float64(24), "columns": float64(80),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.state.business.operationJournalSaveHook = func() error { return errors.New("injected journal failure") }
	response := &remotev2.RpcResponse{OperationId: value.OperationID, AttemptId: uuid.NewString(), Payload: []byte(`{"opened":true}`)}
	if err := fixture.state.business.saveV2OperationScoped(t.Context(), value.OperationID, value.Digest, response, time.Now().UTC(), value.Controller, value.Project); err == nil {
		t.Fatal("journal failure was not returned")
	}
	if got, err := fixture.state.business.prepareV2SideEffect(t.Context(), value); err != nil || got != v2SideEffectCommitted {
		t.Fatalf("retry state=%q error=%v", got, err)
	}
	if len(fixture.starter.processes) != 1 || opened["sessionId"] == nil {
		t.Fatalf("started processes=%d opened=%#v", len(fixture.starter.processes), opened)
	}
}

func newV2SideEffectTestState(t *testing.T) *agentState {
	t.Helper()
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	return state
}

func newV2SideEffectTestMutation(method, projectID string) v2OperationMutationContext {
	operationID := uuid.NewString()
	return v2OperationMutationContext{
		OperationID: operationID,
		Digest:      sha256.Sum256([]byte(method + "\x00" + operationID)),
		Controller:  uuid.NewString(),
		Project:     projectID,
		Method:      method,
		Now:         time.Now().UTC(),
	}
}

func commitV2SideEffectForTest(t *testing.T, store *businessStore, value v2OperationMutationContext) {
	t.Helper()
	if _, err := store.prepareV2SideEffect(t.Context(), value); err != nil {
		t.Fatal(err)
	}
	if err := store.transitionV2SideEffect(t.Context(), value, v2SideEffectPrepared, v2SideEffectStarted); err != nil {
		t.Fatal(err)
	}
	if err := store.transitionV2SideEffect(t.Context(), value, v2SideEffectStarted, v2SideEffectCommitted); err != nil {
		t.Fatal(err)
	}
}
