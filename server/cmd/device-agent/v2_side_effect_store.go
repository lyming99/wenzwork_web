package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

type v2SideEffectState string

const (
	v2SideEffectPrepared  v2SideEffectState = "prepared"
	v2SideEffectStarted   v2SideEffectState = "effect_started"
	v2SideEffectCommitted v2SideEffectState = "effect_committed"
)

// remote_v2_side_effects is deliberately separate from
// remote_v2_operation_claims. Claims are written in the same transaction as a
// SQLite business mutation and can only represent effect_committed. External
// filesystem, process and SecretStore effects need a prepared state before the
// Agent crosses an OS boundary and an uncertainty fence once execution starts.
func migrateRemoteV2SideEffectSchemaV26(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("remote/v2 side-effect migration transaction is required")
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS remote_v2_side_effects (
		operation_id TEXT PRIMARY KEY,
		request_digest BLOB NOT NULL,
		controller_id TEXT NOT NULL DEFAULT '',
		project_id TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at_ms INTEGER NOT NULL,
		updated_at_ms INTEGER NOT NULL,
		expires_at_ms INTEGER NOT NULL,
		CHECK(length(operation_id) = 36),
		CHECK(length(request_digest) = 32),
		CHECK(length(method) BETWEEN 1 AND 120),
		CHECK(state IN ('prepared', 'effect_started', 'effect_committed')),
		CHECK(updated_at_ms >= created_at_ms),
		CHECK(expires_at_ms > updated_at_ms)
	)`); err != nil {
		return fmt.Errorf("create remote/v2 side-effect journal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS remote_v2_side_effects_expiry_idx
		ON remote_v2_side_effects(expires_at_ms)`); err != nil {
		return fmt.Errorf("create remote/v2 side-effect expiry index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS remote_v2_side_effects_controller_idx
		ON remote_v2_side_effects(controller_id, expires_at_ms)`); err != nil {
		return fmt.Errorf("create remote/v2 side-effect controller index: %w", err)
	}
	return nil
}

func v2RPCDurableSideEffectMethod(method string) bool {
	switch method {
	case "file.write-text", "file.create-text", "file.mkdir", "file.rename", "file.move", "file.delete", "file.upload.complete",
		"terminal.open", "terminal.execute", "terminal.write", "terminal.resize", "terminal.signal", "terminal.close",
		"project.create", "agent.environment.update", "ai.config.update", "ai.config.delete":
		return true
	default:
		return false
	}
}

func validV2OperationMutationContext(value v2OperationMutationContext) bool {
	return uuid.Validate(value.OperationID) == nil && !value.Now.IsZero() &&
		len(value.Method) >= 1 && len(value.Method) <= 120 &&
		(value.Controller == "" || uuid.Validate(value.Controller) == nil) &&
		(value.Project == "" || uuid.Validate(value.Project) == nil)
}

// prepareV2SideEffect durably reserves an external-effect operation before
// dispatch. A matching prepared row is safe to resume after restart; started
// and committed rows must never be dispatched again because the OS outcome may
// already have occurred even when no response was journaled.
func (store *businessStore) prepareV2SideEffect(ctx context.Context, value v2OperationMutationContext) (v2SideEffectState, error) {
	if store == nil || !validV2OperationMutationContext(value) || !v2RPCDurableSideEffectMethod(value.Method) {
		return "", errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := value.Now.UTC()
	if err := sweepV2OperationRows(ctx, tx, now); err != nil {
		return "", err
	}
	for _, table := range []string{"remote_v2_operations", "remote_v2_operation_claims"} {
		var durableDigest []byte
		err := tx.QueryRowContext(ctx, `SELECT request_digest FROM `+table+` WHERE operation_id = ?`, value.OperationID).Scan(&durableDigest)
		if err == nil {
			if len(durableDigest) != sha256.Size || string(durableDigest) != string(value.Digest[:]) {
				return "", errRPCIdempotency
			}
			if err := tx.Commit(); err != nil {
				return "", err
			}
			return v2SideEffectCommitted, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}

	state, found, err := loadV2SideEffectTx(ctx, tx, value)
	if err != nil {
		return "", err
	}
	if found {
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return state, nil
	}
	if err := enforceV2OperationJournalCapacity(ctx, tx, value.Controller, 0); err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO remote_v2_side_effects(
		operation_id, request_digest, controller_id, project_id, method, state,
		created_at_ms, updated_at_ms, expires_at_ms
	) VALUES(?,?,?,?,?,'prepared',?,?,?)`, value.OperationID, value.Digest[:], value.Controller,
		value.Project, value.Method, now.UnixMilli(), now.UnixMilli(), now.Add(v2OperationRetention).UnixMilli())
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return v2SideEffectPrepared, nil
}

func (store *businessStore) loadV2SideEffect(ctx context.Context, value v2OperationMutationContext, now time.Time) (v2SideEffectState, bool, error) {
	if store == nil || !validV2OperationMutationContext(value) || now.IsZero() {
		return "", false, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	var digest []byte
	var controllerID, projectID, method, state string
	err = db.QueryRowContext(ctx, `SELECT request_digest, controller_id, project_id, method, state
		FROM remote_v2_side_effects WHERE operation_id = ? AND expires_at_ms > ?`,
		value.OperationID, now.UTC().UnixMilli()).Scan(&digest, &controllerID, &projectID, &method, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	parsed, err := validateV2SideEffectRow(value, digest, controllerID, projectID, method, state)
	if err != nil {
		return "", false, err
	}
	return parsed, true, nil
}

func loadV2SideEffectTx(ctx context.Context, tx *sql.Tx, value v2OperationMutationContext) (v2SideEffectState, bool, error) {
	var digest []byte
	var controllerID, projectID, method, state string
	err := tx.QueryRowContext(ctx, `SELECT request_digest, controller_id, project_id, method, state
		FROM remote_v2_side_effects WHERE operation_id = ?`, value.OperationID).
		Scan(&digest, &controllerID, &projectID, &method, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	parsed, err := validateV2SideEffectRow(value, digest, controllerID, projectID, method, state)
	if err != nil {
		return "", false, err
	}
	return parsed, true, nil
}

func validateV2SideEffectRow(value v2OperationMutationContext, digest []byte, controllerID, projectID, method, state string) (v2SideEffectState, error) {
	if len(digest) != sha256.Size {
		return "", errors.New("remote/v2 side-effect row is invalid")
	}
	if string(digest) != string(value.Digest[:]) {
		return "", errRPCIdempotency
	}
	if controllerID != value.Controller || projectID != value.Project || method != value.Method {
		return "", errors.New("remote/v2 side-effect metadata is invalid")
	}
	parsed := v2SideEffectState(state)
	switch parsed {
	case v2SideEffectPrepared, v2SideEffectStarted, v2SideEffectCommitted:
		return parsed, nil
	default:
		return "", errors.New("remote/v2 side-effect state is invalid")
	}
}

func (store *businessStore) transitionV2SideEffect(ctx context.Context, value v2OperationMutationContext, from, to v2SideEffectState) error {
	if store == nil || !validV2OperationMutationContext(value) ||
		(from != v2SideEffectPrepared || to != v2SideEffectStarted) &&
			(from != v2SideEffectStarted || to != v2SideEffectCommitted) {
		return errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, found, err := loadV2SideEffectTx(ctx, tx, value)
	if err != nil {
		return err
	}
	if !found || current != from {
		return errors.New("remote/v2 side-effect transition is invalid")
	}
	now := time.Now().UTC()
	if now.Before(value.Now) {
		now = value.Now.UTC()
	}
	result, err := tx.ExecContext(ctx, `UPDATE remote_v2_side_effects
		SET state = ?, updated_at_ms = MAX(updated_at_ms, ?),
			expires_at_ms = MAX(updated_at_ms, ?) + ?
		WHERE operation_id = ? AND request_digest = ? AND state = ?`,
		string(to), now.UnixMilli(), now.UnixMilli(), v2OperationRetention.Milliseconds(),
		value.OperationID, value.Digest[:], string(from))
	if err != nil {
		return err
	}
	if rows, rowErr := result.RowsAffected(); rowErr != nil || rows != 1 {
		return firstError(rowErr, errors.New("remote/v2 side-effect transition was not applied"))
	}
	return tx.Commit()
}

func (store *businessStore) deleteV2SideEffect(ctx context.Context, value v2OperationMutationContext) error {
	if store == nil || !validV2OperationMutationContext(value) {
		return errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, found, err := loadV2SideEffectTx(ctx, tx, value)
	if err != nil {
		return err
	}
	if found {
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_v2_side_effects
			WHERE operation_id = ? AND request_digest = ?`, value.OperationID, value.Digest[:]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type v2SideEffectTrackerOutcome uint8

const (
	v2SideEffectActive v2SideEffectTrackerOutcome = iota
	v2SideEffectNoEffect
	v2SideEffectRolledBack
)

// v2SideEffectTracker is request-local. Its durable source of truth remains the
// SQLite row; the local flags only let executeRPC decide whether a handler
// response is safe to return or must be replaced by unknown-commit.
type v2SideEffectTracker struct {
	store *businessStore
	value v2OperationMutationContext

	mu        sync.Mutex
	state     v2SideEffectState
	outcome   v2SideEffectTrackerOutcome
	uncertain bool
}

type v2SideEffectTrackerContextKey struct{}

func withV2SideEffectTracker(ctx context.Context, tracker *v2SideEffectTracker) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if tracker == nil {
		return ctx
	}
	return context.WithValue(ctx, v2SideEffectTrackerContextKey{}, tracker)
}

func v2SideEffectTrackerFromContext(ctx context.Context) *v2SideEffectTracker {
	if ctx == nil {
		return nil
	}
	tracker, _ := ctx.Value(v2SideEffectTrackerContextKey{}).(*v2SideEffectTracker)
	return tracker
}

func beginV2SideEffect(ctx context.Context) error {
	tracker := v2SideEffectTrackerFromContext(ctx)
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.uncertain || tracker.outcome != v2SideEffectActive || tracker.state != v2SideEffectPrepared {
		return errors.New("remote/v2 side-effect cannot start")
	}
	if err := tracker.store.transitionV2SideEffect(ctx, tracker.value, v2SideEffectPrepared, v2SideEffectStarted); err != nil {
		tracker.uncertain = true
		return err
	}
	tracker.state = v2SideEffectStarted
	return nil
}

func commitV2SideEffect(ctx context.Context) error {
	tracker := v2SideEffectTrackerFromContext(ctx)
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.uncertain || tracker.outcome != v2SideEffectActive || tracker.state != v2SideEffectStarted {
		return errors.New("remote/v2 side-effect cannot commit")
	}
	commitContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := tracker.store.transitionV2SideEffect(commitContext, tracker.value, v2SideEffectStarted, v2SideEffectCommitted); err != nil {
		tracker.uncertain = true
		return err
	}
	tracker.state = v2SideEffectCommitted
	return nil
}

func completeV2WithoutSideEffect(ctx context.Context) error {
	tracker := v2SideEffectTrackerFromContext(ctx)
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.uncertain || tracker.outcome != v2SideEffectActive || tracker.state != v2SideEffectPrepared {
		return errors.New("remote/v2 side-effect cannot complete without an effect")
	}
	// Keep the prepared row until the full response is inserted. If the process
	// crashes before then, the operation remains safe to execute again.
	tracker.outcome = v2SideEffectNoEffect
	return nil
}

func rollbackV2SideEffect(ctx context.Context) error {
	tracker := v2SideEffectTrackerFromContext(ctx)
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.uncertain || tracker.outcome != v2SideEffectActive || tracker.state != v2SideEffectStarted {
		return errors.New("remote/v2 side-effect cannot roll back")
	}
	rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := tracker.store.deleteV2SideEffect(rollbackContext, tracker.value); err != nil {
		tracker.uncertain = true
		return err
	}
	tracker.outcome = v2SideEffectRolledBack
	return nil
}

func (tracker *v2SideEffectTracker) responseDisposition() (state v2SideEffectState, outcome v2SideEffectTrackerOutcome, uncertain bool) {
	if tracker == nil {
		return "", v2SideEffectNoEffect, false
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.state, tracker.outcome, tracker.uncertain
}

func (tracker *v2SideEffectTracker) discardPrepared(ctx context.Context) error {
	if tracker == nil {
		return nil
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.uncertain || tracker.outcome == v2SideEffectRolledBack {
		return nil
	}
	if tracker.state != v2SideEffectPrepared {
		return errors.New("remote/v2 started side-effect cannot be discarded")
	}
	discardContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := tracker.store.deleteV2SideEffect(discardContext, tracker.value); err != nil {
		return err
	}
	tracker.outcome = v2SideEffectRolledBack
	return nil
}
