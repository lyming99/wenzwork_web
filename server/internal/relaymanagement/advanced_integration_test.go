//go:build integration

package relaymanagement

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"gorm.io/gorm"
)

func TestOperationRecoveryIdempotencyAndAssignmentSerialization(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	store, err := NewStore(db, nil, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	actorID, userID := uuid.New(), uuid.New()
	deviceID, sessionID := uuid.New(), uuid.New()
	regionID, poolID := uuid.New(), uuid.New()
	cellA, cellB := uuid.New(), uuid.New()
	installationA, installationB := uuid.New(), uuid.New()
	nodeA, nodeB := uuid.New(), uuid.New()
	endpointA, endpointB := uuid.New(), uuid.New()
	codeSuffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM relay_outbox WHERE aggregate_id IN ? OR payload->>'userId' = ?", []uuid.UUID{cellA, cellB, nodeA, nodeB, endpointA, endpointB}, userID.String()).Error
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM relay_operations WHERE created_by = ?", actorID).Error
		_ = db.Exec("DELETE FROM relay_assignment_pins WHERE user_id = ?", userID).Error
		_ = db.Exec("DELETE FROM relay_assignments WHERE user_id = ?", userID).Error
		_ = db.Exec("DELETE FROM remote_device_credentials WHERE device_id = ?", deviceID).Error
		_ = db.Exec("DELETE FROM app_sessions WHERE id = ?", sessionID).Error
		_ = db.Exec("UPDATE relay_node_installations SET current_instance_id = NULL WHERE id IN ?", []uuid.UUID{installationA, installationB}).Error
		_ = db.Exec("DELETE FROM relay_node_instances WHERE id IN ?", []uuid.UUID{nodeA, nodeB}).Error
		_ = db.Exec("DELETE FROM relay_node_installations WHERE id IN ?", []uuid.UUID{installationA, installationB}).Error
		_ = db.Exec("DELETE FROM relay_cell_endpoints WHERE id IN ?", []uuid.UUID{endpointA, endpointB}).Error
		_ = db.Exec("DELETE FROM relay_cells WHERE id IN ?", []uuid.UUID{cellA, cellB}).Error
		_ = db.Exec("DELETE FROM relay_pools WHERE id = ?", poolID).Error
		_ = db.Exec("DELETE FROM relay_regions WHERE id = ?", regionID).Error
		_ = db.Exec("DELETE FROM users WHERE id IN ?", []uuid.UUID{actorID, userID}).Error
	})
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at) VALUES
			(?, ?, 'integration-test-hash', 'Relay Host Maintenance Actor', 'active', now()),
			(?, ?, 'integration-test-hash', 'Relay Assignment User', 'active', now())`,
			actorID, "relay-host-maintenance-actor-"+codeSuffix+"@example.test",
			userID, "relay-assignment-user-"+codeSuffix+"@example.test").Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO app_sessions
			(id, user_id, client_id, device_id, device_name, scope, last_seen_at, idle_expires_at, created_at, updated_at)
			VALUES (?, ?, 'desktop', ?, 'Relay Projection Device', 'remote', ?, ?, ?, ?)`,
			sessionID, userID, deviceID, now, now.Add(time.Hour), now, now).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO remote_device_credentials
			(device_id, user_id, registered_session_id, device_name, platform, agent_version,
			 protocol_min, protocol_max, capabilities, identity_public_key, public_key_thumbprint,
			 grant_version, status)
			VALUES (?, ?, ?, 'Relay Projection Device', 'linux', 'integration', 1, 1, '[]',
			 decode(repeat('11', 32), 'hex'), repeat('a', 43), 1, 'active')`,
			deviceID, userID, sessionID).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO relay_regions (id, code, name, status) VALUES (?, ?, 'Relay Host Region', 'active')", regionID, "host-"+codeSuffix).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO relay_pools (id, region_id, code, name, status) VALUES (?, ?, 'host', 'Relay Host Pool', 'active')", poolID, regionID).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO relay_cells
			(id, pool_id, code, name, failure_domain, status, weight, connection_soft_limit, connection_hard_limit)
		VALUES
			(?, ?, ?, 'Relay Host Cell A', 'host-a', 'active', 1, 10, 20),
			(?, ?, ?, 'Relay Host Cell B', 'host-b', 'active', 1, 10, 20)`,
			cellA, poolID, "host-a-"+codeSuffix, cellB, poolID, "host-b-"+codeSuffix).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO relay_cell_endpoints
			(id, cell_id, revision, endpoint_type, public_endpoint, status, validation_result, certificate_not_after, validated_at, activated_at)
		VALUES
			(?, ?, 1, 'domain', ?, 'active', '{"checks":{"integration":true}}', now() + interval '30 days', now(), now()),
			(?, ?, 1, 'domain', ?, 'active', '{"checks":{"integration":true}}', now() + interval '30 days', now(), now())`,
			endpointA, cellA, "wss://host-a-"+codeSuffix+".example.test/v2/connect",
			endpointB, cellB, "wss://host-b-"+codeSuffix+".example.test/v2/connect").Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO relay_node_installations
			(id, cell_id, display_name, failure_domain, status, identity_public_key, identity_thumbprint, activated_at, created_by)
		VALUES
			(?, ?, 'Relay Host Node A', 'host-a', 'active', decode(repeat('11', 32), 'hex'), repeat('a', 64), now(), ?),
			(?, ?, 'Relay Host Node B', 'host-b', 'active', decode(repeat('22', 32), 'hex'), repeat('b', 64), now(), ?)`,
			installationA, cellA, actorID, installationB, cellB, actorID).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO relay_node_instances
			(id, installation_id, cell_id, status, version, protocol_version, started_at, last_heartbeat_at, lease_expires_at)
		VALUES
			(?, ?, ?, 'ready', 'integration', 1, now(), now(), now() + interval '10 minutes'),
			(?, ?, ?, 'ready', 'integration', 1, now(), now(), now() + interval '10 minutes')`,
			nodeA, installationA, cellA, nodeB, installationB, cellB).Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE relay_node_installations SET current_instance_id = ? WHERE id = ?", nodeA, installationA).Error; err != nil {
			return err
		}
		return tx.Exec("UPDATE relay_node_installations SET current_instance_id = ? WHERE id = ?", nodeB, installationB).Error
	}); err != nil {
		t.Fatalf("seed Relay Host maintenance integration data: %v", err)
	}

	weight := 2.0
	update := UpdateCellInput{Weight: &weight, ActorUserID: actorID, IdempotencyKey: "cell-update-concurrent-" + codeSuffix}
	type operationResult struct {
		operation Operation
		err       error
	}
	results := make(chan operationResult, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			operation, requestErr := store.RequestCellUpdate(ctx, cellA, update)
			results <- operationResult{operation: operation, err: requestErr}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	var operationID uuid.UUID
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent RequestCellUpdate() error = %v", result.err)
		}
		if operationID == uuid.Nil {
			operationID = result.operation.ID
		} else if result.operation.ID != operationID {
			t.Fatalf("idempotent operations differ: %s != %s", result.operation.ID, operationID)
		}
	}
	changedWeight := 3.0
	if _, err := store.RequestCellUpdate(ctx, cellA, UpdateCellInput{
		Weight: &changedWeight, ActorUserID: actorID, IdempotencyKey: update.IdempotencyKey,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotent request error = %v, want ErrConflict", err)
	}

	firstClaim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || firstClaim.ID != operationID {
		t.Fatalf("ClaimOperation(first) = %+v, %v, %v", firstClaim, ok, err)
	}
	if _, ok, err := store.ClaimOperation(ctx, 10*time.Second); err != nil || ok {
		t.Fatalf("ClaimOperation(while leased) = %v, %v, want no work", ok, err)
	}
	now = now.Add(11 * time.Second)
	recoveredClaim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || recoveredClaim.ID != operationID || recoveredClaim.ClaimToken == firstClaim.ClaimToken {
		t.Fatalf("ClaimOperation(recovered) = %+v, %v, %v", recoveredClaim, ok, err)
	}
	if err := store.FailOperation(ctx, firstClaim, "stale_worker", "stale worker"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale claim mutation error = %v, want ErrConflict", err)
	}
	if err := store.ExecuteCellUpdate(ctx, recoveredClaim); err != nil {
		t.Fatalf("ExecuteCellUpdate() error = %v", err)
	}
	completed, err := store.GetOperation(ctx, operationID)
	if err != nil || completed.Status != "succeeded" || completed.ProgressPercent != 100 {
		t.Fatalf("completed cell operation = %+v, %v", completed, err)
	}

	if _, err := store.CreateEndpoint(ctx, CreateEndpointInput{
		CellID: cellA, EndpointType: "ip", PublicEndpoint: "wss://10.0.0.1/v2/connect",
		ActorUserID: actorID, IdempotencyKey: "unsafe-endpoint-" + codeSuffix,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("private endpoint error = %v, want ErrInvalidInput", err)
	}

	assignmentOperationIDs := make([]uuid.UUID, 0, 2)
	for index, cellID := range []uuid.UUID{cellA, cellB} {
		operation, err := store.RequestUserMigration(ctx, MigrateUserInput{
			UserID: userID, Mode: "pinned", TargetCellID: &cellID, Confirmation: "migrate_relay_user",
			ActorUserID: actorID, IdempotencyKey: "assignment-" + codeSuffix + "-" + string(rune('a'+index)),
		})
		if err != nil {
			t.Fatalf("RequestUserMigration(%d) error = %v", index, err)
		}
		assignmentOperationIDs = append(assignmentOperationIDs, operation.ID)
	}
	for index, operationID := range assignmentOperationIDs {
		claim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
		if err != nil || !ok || claim.ID != operationID {
			t.Fatalf("ClaimOperation(assignment %d initial) = %+v, %v, %v", index, claim, ok, err)
		}
		if err := store.ExecuteUserAssignmentOperation(ctx, claim); err != nil {
			t.Fatalf("ExecuteUserAssignmentOperation(%d initial) error = %v", index, err)
		}
		completed, err := store.GetOperation(ctx, operationID)
		if err != nil || completed.Status != "succeeded" {
			t.Fatalf("direct assignment operation = %+v, %v", completed, err)
		}
	}
	assignments, err := store.ListAssignments(ctx, userID)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("ListAssignments() = %+v, %v", assignments, err)
	}
	if assignments[0].AssignmentVersion != 2 || assignments[0].Status != "effective" || assignments[1].AssignmentVersion != 1 || assignments[1].Status != "historical" {
		t.Fatalf("assignment history is not monotonic/current: %+v", assignments)
	}
	var currentCount, assignmentEventCount, auditCount int64
	if err := db.Raw("SELECT count(*) FROM relay_assignments WHERE user_id = ? AND status = 'current'", userID).Scan(&currentCount).Error; err != nil {
		t.Fatalf("count current assignments: %v", err)
	}
	if err := db.Raw("SELECT count(*) FROM relay_outbox WHERE event_type = 'relay.assignment.changed' AND payload->>'userId' = ?", userID.String()).Scan(&assignmentEventCount).Error; err != nil {
		t.Fatalf("count assignment lifecycle events: %v", err)
	}
	if err := db.Raw("SELECT count(*) FROM audit_logs WHERE actor_user_id = ? AND (action LIKE 'relay.assignment.%' OR action = 'relay.migrate.user.request')", actorID).Scan(&auditCount).Error; err != nil {
		t.Fatalf("count assignment audits: %v", err)
	}
	if currentCount != 1 || assignmentEventCount != 2 || auditCount < 4 {
		t.Fatalf("assignment evidence current=%d events=%d audit=%d", currentCount, assignmentEventCount, auditCount)
	}
	cellDrain, err := store.RequestCellDrain(ctx, cellB, actorID, "cell-drain-"+codeSuffix)
	if err != nil {
		t.Fatalf("RequestCellDrain() error = %v", err)
	}
	var requestedCellStatus string
	if err := db.Model(&cellRow{}).Where("id = ?", cellB).Pluck("status", &requestedCellStatus).Error; err != nil {
		t.Fatalf("read requested Cell drain status: %v", err)
	}
	if requestedCellStatus != "draining" {
		t.Fatalf("Cell status immediately after drain request = %q, want draining", requestedCellStatus)
	}
	cellDrainReplay, err := store.RequestCellDrain(ctx, cellB, actorID, "cell-drain-"+codeSuffix)
	if err != nil || cellDrainReplay.ID != cellDrain.ID {
		t.Fatalf("RequestCellDrain(idempotent replay) = %+v, %v", cellDrainReplay, err)
	}
	if _, err := store.RequestCellDrain(ctx, cellB, actorID, "cell-drain-conflict-"+codeSuffix); !errors.Is(err, ErrConflict) {
		t.Fatalf("RequestCellDrain(new request while draining) error = %v, want ErrConflict", err)
	}
	cellDrainClaim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || cellDrainClaim.ID != cellDrain.ID {
		t.Fatalf("ClaimOperation(cell drain initial) = %+v, %v, %v", cellDrainClaim, ok, err)
	}
	if err := store.ExecuteDrain(ctx, cellDrainClaim); err != nil {
		t.Fatalf("ExecuteDrain(cell initial) error = %v", err)
	}
	heartbeat, err := store.Heartbeat(ctx, NodeCertificateIdentity{
		InstallationID: installationB, CellID: cellB, Thumbprint: strings.Repeat("b", 64),
	}, HeartbeatInput{InstanceID: nodeB})
	if err != nil || !heartbeat.Drain || heartbeat.RoutingReady {
		t.Fatalf("Heartbeat(during Cell drain) = %+v, %v", heartbeat, err)
	}
	var drainedInstallationStatus, drainedInstanceStatus string
	if err := db.Model(&installationRow{}).Where("id = ?", installationB).Pluck("status", &drainedInstallationStatus).Error; err != nil {
		t.Fatalf("read drained installation status: %v", err)
	}
	if err := db.Model(&instanceRow{}).Where("id = ?", nodeB).Pluck("status", &drainedInstanceStatus).Error; err != nil {
		t.Fatalf("read drained instance status: %v", err)
	}
	if drainedInstallationStatus != "draining" || drainedInstanceStatus != "draining" {
		t.Fatalf("Cell drain statuses installation=%q instance=%q", drainedInstallationStatus, drainedInstanceStatus)
	}
	cellDrainPending, err := store.GetOperation(ctx, cellDrain.ID)
	if err != nil || cellDrainPending.Status != "queued" || cellDrainPending.ProgressTotal != 2 {
		t.Fatalf("cell drain before migration = %+v, %v", cellDrainPending, err)
	}
	assignmentClaim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || assignmentClaim.Type != "migrate_user" || assignmentClaim.TargetID == nil || *assignmentClaim.TargetID != userID {
		t.Fatalf("ClaimOperation(cell drain assignment initial) = %+v, %v, %v", assignmentClaim, ok, err)
	}
	if err := store.ExecuteUserAssignmentOperation(ctx, assignmentClaim); err != nil {
		t.Fatalf("ExecuteUserAssignmentOperation(cell drain initial) error = %v", err)
	}
	now = now.Add(3 * time.Second)
	cellDrainClaim, ok, err = store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || cellDrainClaim.ID != cellDrain.ID {
		t.Fatalf("ClaimOperation(cell drain finalize) = %+v, %v, %v", cellDrainClaim, ok, err)
	}
	if err := store.ExecuteDrain(ctx, cellDrainClaim); err != nil {
		t.Fatalf("ExecuteDrain(cell finalize) error = %v", err)
	}
	cellDrainComplete, err := store.GetOperation(ctx, cellDrain.ID)
	if err != nil || cellDrainComplete.Status != "succeeded" || cellDrainComplete.ProgressPercent != 100 {
		t.Fatalf("completed cell drain = %+v, %v", cellDrainComplete, err)
	}
	drain, err := store.RequestNodeDrain(ctx, nodeA, actorID, "node-drain-"+codeSuffix)
	if err != nil {
		t.Fatalf("RequestNodeDrain() error = %v", err)
	}
	drainClaim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || drainClaim.ID != drain.ID {
		t.Fatalf("ClaimOperation(drain initial) = %+v, %v, %v", drainClaim, ok, err)
	}
	if err := store.ExecuteDrain(ctx, drainClaim); err != nil {
		t.Fatalf("ExecuteDrain(initial) error = %v", err)
	}
	drainComplete, err := store.GetOperation(ctx, drain.ID)
	if err != nil || drainComplete.Status != "succeeded" {
		t.Fatalf("direct node drain = %+v, %v", drainComplete, err)
	}
	metrics, err := store.OperationalMetrics(ctx)
	if err != nil {
		t.Fatalf("OperationalMetrics() error = %v", err)
	}
	if metrics.Installations["active"]+metrics.Installations["draining"] < 2 || metrics.Instances["ready"]+metrics.Instances["draining"] < 2 ||
		metrics.Operations["succeeded"] < 4 || len(metrics.Cells) < 2 {
		t.Fatalf("operational metrics = %+v", metrics)
	}
	if err := db.Model(&instanceRow{}).Where("id = ?", nodeB).Updates(map[string]any{
		"status": "ready", "active_connections": 1, "lease_expires_at": now.Add(time.Hour),
	}).Error; err != nil {
		t.Fatalf("prepare Relay drain timeout: %v", err)
	}
	timeoutDrain, err := store.RequestNodeDrain(ctx, nodeB, actorID, "node-drain-timeout-"+codeSuffix)
	if err != nil {
		t.Fatalf("RequestNodeDrain(timeout) error = %v", err)
	}
	timeoutClaim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || timeoutClaim.ID != timeoutDrain.ID {
		t.Fatalf("ClaimOperation(timeout initial) = %+v, %v, %v", timeoutClaim, ok, err)
	}
	if err := store.ExecuteDrain(ctx, timeoutClaim); err != nil {
		t.Fatalf("ExecuteDrain(timeout initial) error = %v", err)
	}
	now = now.Add(901 * time.Second)
	timeoutClaim, ok, err = store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || timeoutClaim.ID != timeoutDrain.ID {
		t.Fatalf("ClaimOperation(timeout final) = %+v, %v, %v", timeoutClaim, ok, err)
	}
	if err := store.ExecuteDrain(ctx, timeoutClaim); err != nil {
		t.Fatalf("ExecuteDrain(timeout final) error = %v", err)
	}
	timedOut, err := store.GetOperation(ctx, timeoutDrain.ID)
	if err != nil || timedOut.Status != "failed" || timedOut.ResultCode == nil || *timedOut.ResultCode != "drain_timeout" {
		t.Fatalf("timed-out drain = %+v, %v", timedOut, err)
	}
}

func TestEmptyCellDrainMarksInstallationsAndCompletesDirectly(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	store, err := NewStore(db, nil, WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	actorID, regionID, poolID, cellID, installationID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	codeSuffix := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM relay_outbox WHERE aggregate_id = ?", cellID).Error
		_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ?", actorID).Error
		_ = db.Exec("DELETE FROM relay_operations WHERE created_by = ?", actorID).Error
		_ = db.Exec("DELETE FROM relay_node_installations WHERE id = ?", installationID).Error
		_ = db.Exec("DELETE FROM relay_cells WHERE id = ?", cellID).Error
		_ = db.Exec("DELETE FROM relay_pools WHERE id = ?", poolID).Error
		_ = db.Exec("DELETE FROM relay_regions WHERE id = ?", regionID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
			VALUES (?, ?, 'integration-test-hash', 'Empty Cell Drain Actor', 'active', now())`,
			actorID, "empty-cell-drain-"+codeSuffix+"@example.test").Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO relay_regions (id, code, name, status) VALUES (?, ?, 'Empty Cell Region', 'active')", regionID, "empty-"+codeSuffix).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO relay_pools (id, region_id, code, name, status) VALUES (?, ?, 'empty', 'Empty Cell Pool', 'active')", poolID, regionID).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO relay_cells
			(id, pool_id, code, name, failure_domain, status, weight, connection_soft_limit, connection_hard_limit)
			VALUES (?, ?, ?, 'Empty Relay Cell', 'empty', 'active', 1, 10, 20)`, cellID, poolID, "empty-"+codeSuffix).Error; err != nil {
			return err
		}
		return tx.Exec(`INSERT INTO relay_node_installations
			(id, cell_id, display_name, failure_domain, status, identity_public_key, identity_thumbprint, activated_at, created_by)
			VALUES (?, ?, 'Empty Cell Installation', 'empty', 'active', decode(repeat('33', 32), 'hex'), repeat('c', 64), now(), ?)`,
			installationID, cellID, actorID).Error
	}); err != nil {
		t.Fatalf("seed empty Cell drain data: %v", err)
	}

	operation, err := store.RequestCellDrain(ctx, cellID, actorID, "empty-cell-drain-"+codeSuffix)
	if err != nil || operation.ProgressTotal != 0 {
		t.Fatalf("RequestCellDrain(empty) = %+v, %v", operation, err)
	}
	claim, ok, err := store.ClaimOperation(ctx, 10*time.Second)
	if err != nil || !ok || claim.ID != operation.ID {
		t.Fatalf("ClaimOperation(empty initial) = %+v, %v, %v", claim, ok, err)
	}
	if err := store.ExecuteDrain(ctx, claim); err != nil {
		t.Fatalf("ExecuteDrain(empty initial) error = %v", err)
	}
	var installationStatus string
	if err := db.Model(&installationRow{}).Where("id = ?", installationID).Pluck("status", &installationStatus).Error; err != nil || installationStatus != "draining" {
		t.Fatalf("empty Cell installation status = %q, %v", installationStatus, err)
	}
	completed, err := store.GetOperation(ctx, operation.ID)
	if err != nil || completed.Status != "succeeded" || completed.ProgressPercent != 100 {
		t.Fatalf("empty Cell direct drain = %+v, %v", completed, err)
	}
}
