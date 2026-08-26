package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	"google.golang.org/protobuf/proto"
)

const (
	v2OperationRetention                 = 24 * time.Hour
	v2OperationMaximumRowsPerController  = 20_000
	v2OperationMaximumBytesPerController = 256 << 20
	v2OperationMaximumRowsPerDevice      = 100_000
	v2OperationMaximumBytesPerDevice     = 1 << 30
)

var errV2OperationJournalCapacity = errors.New("remote/v2 operation journal capacity is exhausted")

const v2OperationMaintenanceInterval = 15 * time.Minute

func (store *businessStore) startV2OperationMaintenance() {
	if store == nil || store.operationMaintenanceStop != nil {
		return
	}
	store.operationMaintenanceStop = make(chan struct{})
	store.operationMaintenanceDone = make(chan struct{})
	go func() {
		defer close(store.operationMaintenanceDone)
		_ = store.sweepV2Operations(context.Background(), time.Now().UTC())
		ticker := time.NewTicker(v2OperationMaintenanceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_ = store.sweepV2Operations(ctx, time.Now().UTC())
				cancel()
			case <-store.operationMaintenanceStop:
				return
			}
		}
	}()
}

func (store *businessStore) stopV2OperationMaintenance(ctx context.Context) {
	if store == nil || store.operationMaintenanceStop == nil {
		return
	}
	store.operationMaintenanceOnce.Do(func() { close(store.operationMaintenanceStop) })
	select {
	case <-store.operationMaintenanceDone:
	case <-ctx.Done():
	}
}

func (store *businessStore) sweepV2Operations(ctx context.Context, now time.Time) error {
	if store == nil || now.IsZero() {
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
	if err := sweepV2OperationRows(ctx, tx, now.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func sweepV2OperationRows(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if tx == nil || now.IsZero() {
		return errRPCInvalid
	}
	for _, table := range []string{"remote_v2_operations", "remote_v2_operation_claims", "remote_v2_side_effects"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at_ms <= ?`, now.UTC().UnixMilli()); err != nil {
			return err
		}
	}
	return nil
}

type v2OperationJournalSnapshot struct {
	Rows           int64 `json:"rows"`
	ClaimRows      int64 `json:"claimRows"`
	SideEffectRows int64 `json:"sideEffectRows"`
	BlobBytes      int64 `json:"blobBytes"`
	PageCount      int64 `json:"pageCount"`
	FreelistCount  int64 `json:"freelistCount"`
	FileBytes      int64 `json:"fileBytes"`
}

func (store *businessStore) v2OperationJournalSnapshot(ctx context.Context) v2OperationJournalSnapshot {
	var snapshot v2OperationJournalSnapshot
	if store == nil {
		return snapshot
	}
	db, err := store.openReadDB()
	if err != nil {
		return snapshot
	}
	defer db.Close()
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(response_blob)), 0) FROM remote_v2_operations`).Scan(&snapshot.Rows, &snapshot.BlobBytes)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_v2_operation_claims`).Scan(&snapshot.ClaimRows)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_v2_side_effects`).Scan(&snapshot.SideEffectRows)
	_ = db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&snapshot.PageCount)
	_ = db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&snapshot.FreelistCount)
	if info, statErr := os.Stat(store.path); statErr == nil {
		snapshot.FileBytes = info.Size()
	}
	return snapshot
}

// migrateRemoteV2OperationSchemaV21 creates the device-side idempotency
// journal. The response is business data, so it stays on the Device and is
// never copied to Relay or the control plane. Grant material, Link keys and
// ciphertext are intentionally absent from this table.
func migrateRemoteV2OperationSchemaV21(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("remote/v2 operation migration transaction is required")
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS remote_v2_operations (
        operation_id TEXT PRIMARY KEY,
        request_digest BLOB NOT NULL,
        response_blob BLOB NOT NULL,
		controller_id TEXT NOT NULL DEFAULT '',
		project_id TEXT NOT NULL DEFAULT '',
        created_at_ms INTEGER NOT NULL,
        expires_at_ms INTEGER NOT NULL,
        CHECK(length(operation_id) = 36),
        CHECK(length(request_digest) = 32),
        CHECK(length(response_blob) > 0),
        CHECK(expires_at_ms > created_at_ms)
	)`); err != nil {
		return fmt.Errorf("create remote/v2 operation journal: %w", err)
	}
	if err := ensureV2OperationColumn(ctx, tx, "controller_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureV2OperationColumn(ctx, tx, "project_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS remote_v2_operations_expiry_idx ON remote_v2_operations(expires_at_ms)`); err != nil {
		return fmt.Errorf("create remote/v2 operation expiry index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS remote_v2_operations_controller_idx ON remote_v2_operations(controller_id, expires_at_ms)`); err != nil {
		return fmt.Errorf("create remote/v2 operation controller index: %w", err)
	}
	if err := migrateRemoteV2OperationClaimSchemaV25(ctx, tx); err != nil {
		return err
	}
	return nil
}

// remote_v2_operation_claims records the exact transaction boundary at which
// a SQLite-backed RPC mutation became durable. The full response may be
// encoded and journaled a few instructions later; retaining this compact
// marker across that gap prevents a restart or journal-write failure from
// executing the same business side effect again.
func migrateRemoteV2OperationClaimSchemaV25(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("remote/v2 operation claim migration transaction is required")
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS remote_v2_operation_claims (
		operation_id TEXT PRIMARY KEY,
		request_digest BLOB NOT NULL,
		controller_id TEXT NOT NULL DEFAULT '',
		project_id TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at_ms INTEGER NOT NULL,
		expires_at_ms INTEGER NOT NULL,
		CHECK(length(operation_id) = 36),
		CHECK(length(request_digest) = 32),
		CHECK(length(method) BETWEEN 1 AND 120),
		CHECK(state IN ('effect_committed')),
		CHECK(expires_at_ms > created_at_ms)
	)`); err != nil {
		return fmt.Errorf("create remote/v2 operation claim journal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS remote_v2_operation_claims_expiry_idx
		ON remote_v2_operation_claims(expires_at_ms)`); err != nil {
		return fmt.Errorf("create remote/v2 operation claim expiry index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS remote_v2_operation_claims_controller_idx
		ON remote_v2_operation_claims(controller_id, expires_at_ms)`); err != nil {
		return fmt.Errorf("create remote/v2 operation claim controller index: %w", err)
	}
	return nil
}

type v2OperationMutationContext struct {
	OperationID string
	Digest      [sha256.Size]byte
	Controller  string
	Project     string
	Method      string
	Now         time.Time
}

type v2OperationMutationContextKey struct{}

func withV2OperationMutationContext(ctx context.Context, value v2OperationMutationContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, v2OperationMutationContextKey{}, value)
}

// commitBusinessTransaction atomically adds an operation outcome marker to
// the same SQLite transaction as the business mutation. Contexts outside a
// remote/v2 mutation pay no database cost and retain their existing behavior.
func commitBusinessTransaction(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errRPCInvalid
	}
	value, ok := ctx.Value(v2OperationMutationContextKey{}).(v2OperationMutationContext)
	if ok {
		if err := markV2OperationEffectCommitted(ctx, tx, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func markV2OperationEffectCommitted(ctx context.Context, tx *sql.Tx, value v2OperationMutationContext) error {
	if tx == nil || !validV2OperationMutationContext(value) {
		return errRPCInvalid
	}
	now := value.Now.UTC()
	if _, err := tx.ExecContext(ctx, `DELETE FROM remote_v2_operation_claims WHERE expires_at_ms <= ?`, now.UnixMilli()); err != nil {
		return err
	}
	var completedDigest []byte
	completedErr := tx.QueryRowContext(ctx, `SELECT request_digest FROM remote_v2_operations WHERE operation_id = ?`, value.OperationID).Scan(&completedDigest)
	if completedErr == nil {
		if len(completedDigest) != sha256.Size || string(completedDigest) != string(value.Digest[:]) {
			return errRPCIdempotency
		}
		return nil
	}
	if !errors.Is(completedErr, sql.ErrNoRows) {
		return completedErr
	}
	var existingDigest []byte
	err := tx.QueryRowContext(ctx, `SELECT request_digest FROM remote_v2_operation_claims WHERE operation_id = ?`, value.OperationID).Scan(&existingDigest)
	if err == nil {
		if len(existingDigest) != sha256.Size || string(existingDigest) != string(value.Digest[:]) {
			return errRPCIdempotency
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := enforceV2OperationJournalCapacity(ctx, tx, value.Controller, 0); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO remote_v2_operation_claims(
		operation_id, request_digest, controller_id, project_id, method, state, created_at_ms, expires_at_ms
	) VALUES(?,?,?,?,?,'effect_committed',?,?)`, value.OperationID, value.Digest[:], value.Controller,
		value.Project, value.Method, now.UnixMilli(), now.Add(v2OperationRetention).UnixMilli())
	return err
}

func (store *businessStore) loadV2OperationClaim(ctx context.Context, operationID string, digest [sha256.Size]byte, now time.Time) (bool, error) {
	if store == nil || uuid.Validate(operationID) != nil || now.IsZero() {
		return false, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return false, err
	}
	defer db.Close()
	var storedDigest []byte
	err = db.QueryRowContext(ctx, `SELECT request_digest FROM remote_v2_operation_claims
		WHERE operation_id = ? AND expires_at_ms > ?`, operationID, now.UTC().UnixMilli()).Scan(&storedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || len(storedDigest) != sha256.Size {
		return false, firstError(err, errors.New("remote/v2 operation claim row is invalid"))
	}
	if string(storedDigest) != string(digest[:]) {
		return false, errRPCIdempotency
	}
	return true, nil
}

func ensureV2OperationColumn(ctx context.Context, tx *sql.Tx, name, declaration string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(remote_v2_operations)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE remote_v2_operations ADD COLUMN `+name+` `+declaration); err != nil {
		return fmt.Errorf("add remote/v2 operation %s column: %w", name, err)
	}
	return nil
}

func (store *businessStore) loadV2Operation(ctx context.Context, operationID string, digest [sha256.Size]byte, now time.Time) (*remotev2.RpcResponse, bool, error) {
	if store == nil || uuid.Validate(operationID) != nil || now.IsZero() {
		return nil, false, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	var storedDigest, responseBlob []byte
	err = db.QueryRowContext(ctx, `SELECT request_digest, response_blob FROM remote_v2_operations
        WHERE operation_id = ? AND expires_at_ms > ?`, operationID, now.UTC().UnixMilli()).Scan(&storedDigest, &responseBlob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil || len(storedDigest) != sha256.Size || len(responseBlob) == 0 {
		return nil, false, firstError(err, errors.New("remote/v2 operation row is invalid"))
	}
	if string(storedDigest) != string(digest[:]) {
		return nil, false, errRPCIdempotency
	}
	response := new(remotev2.RpcResponse)
	if proto.Unmarshal(responseBlob, response) != nil || len(response.ProtoReflect().GetUnknown()) != 0 || response.GetOperationId() != operationID {
		return nil, false, errors.New("remote/v2 stored response is invalid")
	}
	return response, true, nil
}

func (store *businessStore) saveV2Operation(ctx context.Context, operationID string, digest [sha256.Size]byte, response *remotev2.RpcResponse, now time.Time) error {
	return store.saveV2OperationScoped(ctx, operationID, digest, response, now, "", "")
}

func (store *businessStore) saveV2OperationScoped(ctx context.Context, operationID string, digest [sha256.Size]byte, response *remotev2.RpcResponse, now time.Time, controllerID, projectID string) error {
	if store == nil || uuid.Validate(operationID) != nil || response == nil || response.GetOperationId() != operationID || now.IsZero() {
		return errRPCInvalid
	}
	if (controllerID != "" && uuid.Validate(controllerID) != nil) || (projectID != "" && uuid.Validate(projectID) != nil) {
		return errRPCInvalid
	}
	encoded, err := proto.Marshal(response)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumPeerRPCPlaintext {
		return firstError(err, errRPCInvalid)
	}
	if store.operationJournalSaveHook != nil {
		if err := store.operationJournalSaveHook(); err != nil {
			return err
		}
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
	if err := sweepV2OperationRows(ctx, tx, now.UTC()); err != nil {
		return err
	}
	var existingDigest []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest FROM remote_v2_operations WHERE operation_id = ?`, operationID).Scan(&existingDigest)
	if err == nil {
		if len(existingDigest) != sha256.Size || string(existingDigest) != string(digest[:]) {
			return errRPCIdempotency
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_v2_operation_claims WHERE operation_id = ? AND request_digest = ?`, operationID, digest[:]); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_v2_side_effects WHERE operation_id = ? AND request_digest = ?`, operationID, digest[:]); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var claimDigest []byte
	claimErr := tx.QueryRowContext(ctx, `SELECT request_digest FROM remote_v2_operation_claims WHERE operation_id = ?`, operationID).Scan(&claimDigest)
	if claimErr == nil {
		if len(claimDigest) != sha256.Size || string(claimDigest) != string(digest[:]) {
			return errRPCIdempotency
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_v2_operation_claims WHERE operation_id = ?`, operationID); err != nil {
			return err
		}
	} else if !errors.Is(claimErr, sql.ErrNoRows) {
		return claimErr
	}
	var sideEffectDigest []byte
	sideEffectErr := tx.QueryRowContext(ctx, `SELECT request_digest FROM remote_v2_side_effects WHERE operation_id = ?`, operationID).Scan(&sideEffectDigest)
	if sideEffectErr == nil {
		if len(sideEffectDigest) != sha256.Size || string(sideEffectDigest) != string(digest[:]) {
			return errRPCIdempotency
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM remote_v2_side_effects WHERE operation_id = ?`, operationID); err != nil {
			return err
		}
	} else if !errors.Is(sideEffectErr, sql.ErrNoRows) {
		return sideEffectErr
	}
	if err := enforceV2OperationJournalCapacity(ctx, tx, controllerID, int64(len(encoded))); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO remote_v2_operations(operation_id, request_digest, response_blob, controller_id, project_id, created_at_ms, expires_at_ms)
		VALUES(?,?,?,?,?,?,?)`, operationID, digest[:], encoded, controllerID, projectID, now.UTC().UnixMilli(), now.UTC().Add(v2OperationRetention).UnixMilli())
	if err != nil {
		return err
	}
	if rows, rowErr := result.RowsAffected(); rowErr != nil || rows != 1 {
		var existingDigest []byte
		if readErr := tx.QueryRowContext(ctx, `SELECT request_digest FROM remote_v2_operations WHERE operation_id = ?`, operationID).Scan(&existingDigest); readErr != nil || len(existingDigest) != sha256.Size || string(existingDigest) != string(digest[:]) {
			return firstError(readErr, errRPCIdempotency)
		}
	}
	return tx.Commit()
}

func enforceV2OperationJournalCapacity(ctx context.Context, tx *sql.Tx, controllerID string, incomingBytes int64) error {
	var deviceRows, deviceBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(response_blob)), 0) FROM remote_v2_operations`).Scan(&deviceRows, &deviceBytes); err != nil {
		return err
	}
	var deviceClaims int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_v2_operation_claims`).Scan(&deviceClaims); err != nil {
		return err
	}
	var deviceSideEffects int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_v2_side_effects`).Scan(&deviceSideEffects); err != nil {
		return err
	}
	if deviceRows+deviceClaims+deviceSideEffects >= v2OperationMaximumRowsPerDevice || deviceBytes+incomingBytes > v2OperationMaximumBytesPerDevice {
		return errV2OperationJournalCapacity
	}
	var controllerRows, controllerBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(response_blob)), 0) FROM remote_v2_operations WHERE controller_id = ?`, controllerID).Scan(&controllerRows, &controllerBytes); err != nil {
		return err
	}
	var controllerClaims int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_v2_operation_claims WHERE controller_id = ?`, controllerID).Scan(&controllerClaims); err != nil {
		return err
	}
	var controllerSideEffects int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_v2_side_effects WHERE controller_id = ?`, controllerID).Scan(&controllerSideEffects); err != nil {
		return err
	}
	if controllerRows+controllerClaims+controllerSideEffects >= v2OperationMaximumRowsPerController || controllerBytes+incomingBytes > v2OperationMaximumBytesPerController {
		return errV2OperationJournalCapacity
	}
	return nil
}
