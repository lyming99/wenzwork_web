package relaymanagement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const assignmentLeaseDuration = 24 * time.Hour

func (store *Store) ClaimOperation(ctx context.Context, staleAfter time.Duration) (OperationClaim, bool, error) {
	if staleAfter < 10*time.Second || staleAfter > 30*time.Minute {
		return OperationClaim{}, false, ErrInvalidInput
	}
	now := store.now().UTC()
	claimToken := uuid.New()
	var claimed operationRow
	claimedOK := false
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Raw(`
			SELECT operation.*
			FROM relay_operations operation
			WHERE ((operation.status = 'pending' AND operation.next_attempt_at <= ?)
			   OR (operation.status = 'running' AND (operation.worker_heartbeat_at IS NULL OR operation.worker_heartbeat_at < ?)))
			  AND NOT EXISTS (
			    SELECT 1 FROM relay_operations earlier
			    WHERE earlier.target_type = operation.target_type
			      AND earlier.target_id = operation.target_id
			      AND earlier.request_sequence < operation.request_sequence
			      AND earlier.status IN ('pending', 'running')
			  )
			ORDER BY CASE WHEN operation.status = 'running' THEN 0 ELSE 1 END, operation.request_sequence
			FOR UPDATE SKIP LOCKED
			LIMIT 1`, now, now.Add(-staleAfter)).Scan(&claimed)
		if result.Error != nil {
			return fmt.Errorf("claim Relay operation: %w", result.Error)
		}
		if result.RowsAffected == 0 || claimed.ID == uuid.Nil {
			return nil
		}
		if claimed.Status == "running" {
			if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status = ?", claimed.ID, "running").
				Updates(map[string]any{"status": "pending", "updated_at": now}).Error; err != nil {
				return fmt.Errorf("recover Relay operation items: %w", err)
			}
		}
		updates := map[string]any{
			"status": "running", "claim_token": claimToken, "worker_heartbeat_at": now,
			"updated_at": now,
		}
		if claimed.StartedAt == nil {
			updates["started_at"] = now
		}
		if err := tx.Model(&operationRow{}).Where("id = ?", claimed.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("mark Relay operation claimed: %w", err)
		}
		claimedOK = true
		return nil
	})
	if err != nil || !claimedOK {
		return OperationClaim{}, false, err
	}
	operation, err := store.GetOperation(ctx, claimed.ID)
	if err != nil {
		return OperationClaim{}, false, err
	}
	return OperationClaim{Operation: operation, ClaimToken: claimToken, ActorUserID: claimed.CreatedBy}, true, nil
}

func (store *Store) HeartbeatOperationClaim(ctx context.Context, claim OperationClaim) error {
	if claim.ID == uuid.Nil || claim.ClaimToken == uuid.Nil {
		return ErrInvalidInput
	}
	result := store.db.WithContext(ctx).Model(&operationRow{}).
		Where("id = ? AND claim_token = ? AND status = ?", claim.ID, claim.ClaimToken, "running").
		Updates(map[string]any{"worker_heartbeat_at": store.now().UTC(), "updated_at": store.now().UTC()})
	if result.Error != nil {
		return fmt.Errorf("heartbeat Relay operation claim: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (store *Store) FailOperation(ctx context.Context, claim OperationClaim, code, message string) error {
	code = strings.TrimSpace(code)
	message = strings.TrimSpace(message)
	if claim.ID == uuid.Nil || claim.ClaimToken == uuid.Nil || !validPlainText(code, 100, false) || !validPlainText(message, 500, false) {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClaim(tx, claim); err != nil {
			return err
		}
		if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status IN ?", claim.ID, []string{"pending", "running"}).
			Updates(map[string]any{
				"status": "failed", "attempts": gorm.Expr("attempts + 1"), "error_code": code,
				"error_message": message, "finished_at": now, "updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("fail Relay operation items: %w", err)
		}
		result := tx.Model(&operationRow{}).Where("id = ? AND claim_token = ? AND status = ?", claim.ID, claim.ClaimToken, "running").
			Updates(map[string]any{
				"status": "failed", "error_code": code, "error_message": message, "finished_at": now,
				"worker_heartbeat_at": now, "updated_at": now,
			})
		if result.Error != nil {
			return fmt.Errorf("fail Relay operation: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		return nil
	})
}

func (store *Store) ExecuteCellUpdate(ctx context.Context, claim OperationClaim) error {
	var input UpdateCellInput
	if claim.Type != "cell_update" || claim.TargetID == nil || json.Unmarshal(claim.Request, &input) != nil {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClaim(tx, claim); err != nil {
			return err
		}
		var row struct {
			Status                     string  `gorm:"column:status"`
			Weight                     float64 `gorm:"column:weight"`
			ConnectionSoftLimit        int64   `gorm:"column:connection_soft_limit"`
			ConnectionHardLimit        int64   `gorm:"column:connection_hard_limit"`
			FileBandwidthSoftLimitMbps float64 `gorm:"column:file_bandwidth_soft_limit_mbps"`
			FileBandwidthHardLimitMbps float64 `gorm:"column:file_bandwidth_hard_limit_mbps"`
			Version                    int64   `gorm:"column:version"`
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("relay_cells").Where("id = ?", *claim.TargetID).Take(&row).Error; err != nil {
			return normalizeNotFound(err, "lock Relay Cell for update")
		}
		before := row
		updates := map[string]any{"updated_at": now, "version": row.Version + 1}
		if input.Status != nil {
			row.Status = strings.ToLower(strings.TrimSpace(*input.Status))
			updates["status"] = row.Status
		}
		if input.Weight != nil {
			row.Weight = *input.Weight
			updates["weight"] = row.Weight
		}
		if input.ConnectionSoftLimit != nil {
			row.ConnectionSoftLimit = *input.ConnectionSoftLimit
			updates["connection_soft_limit"] = row.ConnectionSoftLimit
		}
		if input.ConnectionHardLimit != nil {
			row.ConnectionHardLimit = *input.ConnectionHardLimit
			updates["connection_hard_limit"] = row.ConnectionHardLimit
		}
		if input.FileBandwidthSoftLimitMbps != nil {
			row.FileBandwidthSoftLimitMbps = *input.FileBandwidthSoftLimitMbps
			updates["file_bandwidth_soft_limit_mbps"] = row.FileBandwidthSoftLimitMbps
		}
		if input.FileBandwidthHardLimitMbps != nil {
			row.FileBandwidthHardLimitMbps = *input.FileBandwidthHardLimitMbps
			updates["file_bandwidth_hard_limit_mbps"] = row.FileBandwidthHardLimitMbps
		}
		if !validCellUpdate(row.Status, row.Weight, row.ConnectionSoftLimit, row.ConnectionHardLimit, row.FileBandwidthSoftLimitMbps, row.FileBandwidthHardLimitMbps) {
			return ErrInvalidInput
		}
		if err := tx.Table("relay_cells").Where("id = ?", *claim.TargetID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update Relay Cell: %w", err)
		}
		var activeConnections int64
		if err := tx.Model(&instanceRow{}).Where("cell_id = ? AND status = ? AND lease_expires_at > ?", *claim.TargetID, "ready", now).
			Select("COALESCE(sum(active_connections), 0)").Scan(&activeConnections).Error; err != nil {
			return fmt.Errorf("sum Relay Cell connections: %w", err)
		}
		payload := map[string]any{
			"cellId": *claim.TargetID, "status": row.Status, "version": row.Version + 1,
			"activeConnections": activeConnections, "connectionHardLimit": row.ConnectionHardLimit,
		}
		if err := appendRelayEvent(tx, "relay_cell", *claim.TargetID, "relay.cell.updated", payload, now); err != nil {
			return err
		}
		if err := appendAudit(tx, claim.ActorUserID, "relay.cell.update", "relay_cell", *claim.TargetID, before, row, now); err != nil {
			return err
		}
		return finishClaimTx(tx, claim, now)
	})
}

func (store *Store) ExecuteDrain(ctx context.Context, claim OperationClaim) error {
	if (claim.Type != "node_drain" && claim.Type != "cell_drain") || claim.TargetID == nil {
		return ErrInvalidInput
	}
	var request struct {
		DeadlineSeconds int `json:"deadlineSeconds"`
	}
	if json.Unmarshal(claim.Request, &request) != nil || request.DeadlineSeconds < 1 || request.DeadlineSeconds > 1800 {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClaim(tx, claim); err != nil {
			return err
		}
		var items []operationItemRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("operation_id = ?", claim.ID).
			Order("created_at, id").Find(&items).Error; err != nil {
			return fmt.Errorf("lock Relay drain items: %w", err)
		}
		nodeIDs := make([]uuid.UUID, 0, len(items))
		userIDs := make([]uuid.UUID, 0, len(items))
		firstPass := len(items) == 0
		for _, item := range items {
			if item.TargetID == nil || *item.TargetID == uuid.Nil {
				return ErrInvalidInput
			}
			switch item.TargetType {
			case "relay_node_instance":
				nodeIDs = append(nodeIDs, *item.TargetID)
			case "user":
				if claim.Type != "cell_drain" {
					return ErrConflict
				}
				userIDs = append(userIDs, *item.TargetID)
			default:
				return ErrConflict
			}
			if item.Attempts == 0 {
				firstPass = true
			}
		}
		if claim.Type == "node_drain" && (len(nodeIDs) != 1 || nodeIDs[0] != *claim.TargetID) {
			return ErrConflict
		}

		if firstPass {
			if claim.Type == "cell_drain" {
				var cell cellRow
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cell, "id = ?", *claim.TargetID).Error; err != nil {
					return normalizeNotFound(err, "lock Relay Cell drain")
				}
				if cell.Status == "disabled" {
					return ErrConflict
				}
				if cell.Status != "draining" {
					cell.Status, cell.Version = "draining", cell.Version+1
					if err := tx.Model(&cell).Updates(map[string]any{"status": cell.Status, "version": cell.Version, "updated_at": now}).Error; err != nil {
						return fmt.Errorf("mark Relay Cell draining: %w", err)
					}
				}
				if err := tx.Model(&installationRow{}).Where("cell_id = ? AND status = ?", cell.ID, "active").Updates(map[string]any{
					"status": "draining", "version": gorm.Expr("version + 1"), "updated_at": now,
				}).Error; err != nil {
					return fmt.Errorf("mark Relay Cell installations draining: %w", err)
				}
				var activeConnections int64
				if err := tx.Model(&instanceRow{}).Where("cell_id = ? AND status IN ? AND lease_expires_at > ?", cell.ID, []string{"ready", "draining"}, now).
					Select("COALESCE(sum(active_connections), 0)").Scan(&activeConnections).Error; err != nil {
					return fmt.Errorf("sum draining Relay Cell connections: %w", err)
				}
				var cellDispatches int64
				if err := tx.Model(&outboxRow{}).Where("event_type = ? AND payload->>'operationId' = ?", "relay.cell.updated", claim.ID.String()).Count(&cellDispatches).Error; err != nil {
					return fmt.Errorf("check Relay Cell drain dispatch: %w", err)
				}
				if cellDispatches == 0 {
					if err := appendRelayEvent(tx, "relay_cell", cell.ID, "relay.cell.updated", map[string]any{
						"cellId": cell.ID, "status": cell.Status, "version": cell.Version,
						"activeConnections": activeConnections, "connectionHardLimit": cell.ConnectionHardLimit, "operationId": claim.ID,
					}, now); err != nil {
						return err
					}
				}
			}
			for _, nodeID := range nodeIDs {
				result := tx.Model(&instanceRow{}).Where("id = ? AND status IN ?", nodeID, []string{"starting", "ready", "draining"}).
					Updates(map[string]any{"status": "draining"})
				if result.Error != nil {
					return fmt.Errorf("mark Relay node draining: %w", result.Error)
				}
				if result.RowsAffected == 0 && claim.Type == "node_drain" {
					return ErrConflict
				}
				if err := appendRelayEvent(tx, "relay_node_instance", nodeID, "relay.node.drain", map[string]any{
					"nodeId": nodeID, "reason": claim.Type, "deadlineSeconds": request.DeadlineSeconds, "operationId": claim.ID,
				}, now); err != nil {
					return err
				}
			}
			if claim.Type == "node_drain" {
				if err := tx.Exec(`
					UPDATE relay_node_installations installation
					SET status = 'draining', version = installation.version + 1, updated_at = ?
					FROM relay_node_instances instance
					WHERE instance.id = ? AND installation.id = instance.installation_id
					  AND installation.status = 'active'`, now, *claim.TargetID).Error; err != nil {
					return fmt.Errorf("mark Relay installation draining: %w", err)
				}
			}
			for _, userID := range userIDs {
				idempotencyKey := cellDrainMigrationKey(claim.ID, userID)
				request := MigrateUserInput{
					UserID: userID, Mode: "auto", Confirmation: "migrate_relay_user",
				}
				if _, err := createOperationTx(tx, CreateOperationInput{
					Type: "migrate_user", TargetType: "user", TargetID: &userID, Request: request,
					ActorUserID: uuidValue(claim.ActorUserID), IdempotencyKey: idempotencyKey,
				}, now); err != nil {
					return fmt.Errorf("schedule Relay Cell drain assignment migration: %w", err)
				}
				if err := appendAudit(tx, claim.ActorUserID, "relay.cell.drain.assignment.schedule", "user", userID, nil, map[string]any{
					"cellId": *claim.TargetID, "operationId": claim.ID, "idempotencyKey": idempotencyKey,
				}, now); err != nil {
					return err
				}
			}
			if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND attempts = 0", claim.ID).Updates(map[string]any{
				"status": "running", "attempts": gorm.Expr("attempts + 1"), "started_at": now, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("start Relay drain items: %w", err)
			}
		} else if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status = ?", claim.ID, "pending").
			Updates(map[string]any{"status": "running", "updated_at": now}).Error; err != nil {
			return fmt.Errorf("resume Relay drain items: %w", err)
		}

		if len(nodeIDs) > 0 {
			type nodeDrainState struct {
				ID                uuid.UUID `gorm:"column:id"`
				Status            string    `gorm:"column:status"`
				ActiveConnections int64     `gorm:"column:active_connections"`
				LeaseExpiresAt    time.Time `gorm:"column:lease_expires_at"`
			}
			var nodes []nodeDrainState
			if err := tx.Model(&instanceRow{}).Select("id, status, active_connections, lease_expires_at").Where("id IN ?", nodeIDs).Find(&nodes).Error; err != nil {
				return fmt.Errorf("read Relay drain progress: %w", err)
			}
			for _, node := range nodes {
				done := node.ActiveConnections == 0 || !node.LeaseExpiresAt.After(now) ||
					(node.Status != "starting" && node.Status != "ready" && node.Status != "draining")
				var deliveryPending int64
				if done {
					if err := tx.Model(&outboxRow{}).Where("event_type = ? AND aggregate_id = ? AND payload->>'operationId' = ? AND published_at IS NULL",
						"relay.node.drain", node.ID, claim.ID.String()).Count(&deliveryPending).Error; err != nil {
						return fmt.Errorf("check Relay node drain delivery: %w", err)
					}
				}
				if done && deliveryPending == 0 {
					if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND target_id = ? AND status IN ?", claim.ID, node.ID, []string{"pending", "running"}).
						Updates(map[string]any{"status": "succeeded", "finished_at": now, "updated_at": now}).Error; err != nil {
						return fmt.Errorf("complete Relay drain item: %w", err)
					}
				}
			}
		}
		for _, userID := range userIDs {
			var migration operationRow
			err := tx.Where("operation_type = ? AND target_type = ? AND target_id = ? AND idempotency_key = ?",
				"migrate_user", "user", userID, cellDrainMigrationKey(claim.ID, userID)).First(&migration).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if updateErr := tx.Model(&operationItemRow{}).Where("operation_id = ? AND target_type = ? AND target_id = ? AND status IN ?",
					claim.ID, "user", userID, []string{"pending", "running"}).Updates(map[string]any{
					"status": "failed", "error_code": "assignment_migration_missing",
					"error_message": "Relay Cell drain assignment migration was not found", "finished_at": now, "updated_at": now,
				}).Error; updateErr != nil {
					return fmt.Errorf("fail missing Relay Cell drain assignment migration: %w", updateErr)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("read Relay Cell drain assignment migration: %w", err)
			}
			switch migration.Status {
			case "succeeded":
				if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND target_type = ? AND target_id = ? AND status IN ?",
					claim.ID, "user", userID, []string{"pending", "running"}).Updates(map[string]any{
					"status": "succeeded", "finished_at": now, "updated_at": now,
				}).Error; err != nil {
					return fmt.Errorf("complete Relay Cell drain assignment item: %w", err)
				}
			case "failed", "timed_out", "cancelled":
				if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND target_type = ? AND target_id = ? AND status IN ?",
					claim.ID, "user", userID, []string{"pending", "running"}).Updates(map[string]any{
					"status": "failed", "error_code": "assignment_migration_failed",
					"error_message": "Relay Cell drain assignment migration failed", "finished_at": now, "updated_at": now,
				}).Error; err != nil {
					return fmt.Errorf("fail Relay Cell drain assignment item: %w", err)
				}
			case "pending", "running":
			default:
				return ErrConflict
			}
		}
		var completed int64
		if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status = ?", claim.ID, "succeeded").Count(&completed).Error; err != nil {
			return fmt.Errorf("count Relay drain progress: %w", err)
		}
		var failed int64
		if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status = ?", claim.ID, "failed").Count(&failed).Error; err != nil {
			return fmt.Errorf("count failed Relay drain items: %w", err)
		}
		var undelivered int64
		if err := tx.Model(&outboxRow{}).Where("payload->>'operationId' = ? AND published_at IS NULL", claim.ID.String()).Count(&undelivered).Error; err != nil {
			return fmt.Errorf("check Relay drain delivery: %w", err)
		}
		if completed+failed == int64(len(items)) && undelivered == 0 {
			if failed > 0 {
				return failClaimTx(tx, claim, "drain_partial_failure", "One or more Relay drain items failed", now)
			}
			return finishClaimTx(tx, claim, now)
		}
		startedAt := now
		if claim.StartedAt != nil {
			startedAt = claim.StartedAt.UTC()
		}
		if !now.Before(startedAt.Add(time.Duration(request.DeadlineSeconds) * time.Second)) {
			if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status IN ?", claim.ID, []string{"pending", "running"}).Updates(map[string]any{
				"status": "failed", "error_code": "drain_timeout", "error_message": "Relay drain deadline expired",
				"finished_at": now, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("timeout Relay drain items: %w", err)
			}
			result := tx.Model(&operationRow{}).Where("id = ? AND claim_token = ? AND status = ?", claim.ID, claim.ClaimToken, "running").Updates(map[string]any{
				"status": "timed_out", "progress_completed": completed, "error_code": "drain_timeout",
				"error_message": "Relay drain deadline expired", "finished_at": now, "worker_heartbeat_at": now, "updated_at": now,
			})
			if result.Error != nil {
				return fmt.Errorf("timeout Relay drain operation: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return ErrConflict
			}
			return nil
		}
		return rescheduleClaimTx(tx, claim, now, 5*time.Second, completed)
	})
}

func cellDrainMigrationKey(operationID, userID uuid.UUID) string {
	return "cell-drain:" + operationID.String() + ":" + userID.String()
}

func uuidValue(value *uuid.UUID) uuid.UUID {
	if value == nil {
		return uuid.Nil
	}
	return *value
}

func (store *Store) CompleteEndpointValidation(ctx context.Context, claim OperationClaim, validation EndpointValidationResult) error {
	if claim.Type != "endpoint_validate" || claim.TargetID == nil || validation.CellID == uuid.Nil ||
		validation.InstallationID == uuid.Nil || validation.InstanceID == uuid.Nil || validation.CertificateNotAfter.IsZero() {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	encoded, err := json.Marshal(validation)
	if err != nil {
		return ErrInvalidInput
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClaim(tx, claim); err != nil {
			return err
		}
		var endpoint endpointRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&endpoint, "id = ?", *claim.TargetID).Error; err != nil {
			return normalizeNotFound(err, "lock Relay endpoint validation")
		}
		if endpoint.Status != "validating" || endpoint.CellID != validation.CellID {
			return ErrConflict
		}
		if err := tx.Model(&endpoint).Updates(map[string]any{
			"status": "validated", "validation_result": encoded, "certificate_not_after": validation.CertificateNotAfter.UTC(),
			"validated_at": now, "updated_at": now, "version": endpoint.Version + 1,
		}).Error; err != nil {
			return fmt.Errorf("complete Relay endpoint validation: %w", err)
		}
		if err := markOperationItemsSucceeded(tx, claim.ID, now); err != nil {
			return err
		}
		return finishClaimTx(tx, claim, now)
	})
}

func (store *Store) FailEndpointValidation(ctx context.Context, claim OperationClaim, code string) error {
	if claim.Type != "endpoint_validate" || claim.TargetID == nil {
		return ErrInvalidInput
	}
	code = strings.TrimSpace(code)
	if !validPlainText(code, 100, false) {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClaim(tx, claim); err != nil {
			return err
		}
		resultJSON, _ := json.Marshal(map[string]any{"valid": false, "code": code})
		if err := tx.Model(&endpointRow{}).Where("id = ? AND status = ?", *claim.TargetID, "validating").Updates(map[string]any{
			"status": "failed", "validation_result": resultJSON, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("fail Relay endpoint validation: %w", err)
		}
		return failClaimTx(tx, claim, "endpoint_validation_failed", "Relay endpoint validation failed", now)
	})
}

func (store *Store) ExecuteEndpointActivation(ctx context.Context, claim OperationClaim) error {
	if claim.Type != "endpoint_activate" || claim.TargetID == nil {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClaim(tx, claim); err != nil {
			return err
		}
		var endpoint endpointRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&endpoint, "id = ?", *claim.TargetID).Error; err != nil {
			return normalizeNotFound(err, "lock Relay endpoint activation")
		}
		if endpoint.Status != "validated" || endpoint.ValidatedAt == nil || endpoint.CertificateNotAfter == nil || !endpoint.CertificateNotAfter.After(now.Add(time.Hour)) {
			return ErrConflict
		}
		drainUntil := now.Add(15 * time.Minute)
		if err := tx.Model(&endpointRow{}).Where("cell_id = ? AND status = ?", endpoint.CellID, "active").Updates(map[string]any{
			"status": "draining", "drain_until": drainUntil, "superseded_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("drain previous Relay endpoint: %w", err)
		}
		if err := tx.Model(&endpoint).Updates(map[string]any{
			"status": "active", "activated_at": now, "updated_at": now, "version": endpoint.Version + 1,
		}).Error; err != nil {
			return fmt.Errorf("activate Relay endpoint: %w", err)
		}
		if err := tx.Model(&cellRow{}).Where("id = ?", endpoint.CellID).Updates(map[string]any{
			"version": gorm.Expr("version + 1"), "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("advance Relay Cell endpoint version: %w", err)
		}
		if err := appendRelayEvent(tx, "relay_cell_endpoint", endpoint.ID, "relay.endpoint.activated", map[string]any{
			"endpointId": endpoint.ID, "cellId": endpoint.CellID, "revision": endpoint.Revision,
		}, now); err != nil {
			return err
		}
		var nodeIDs []uuid.UUID
		if err := tx.Model(&instanceRow{}).Where("cell_id = ? AND status = ? AND lease_expires_at > ?", endpoint.CellID, "ready", now).Pluck("id", &nodeIDs).Error; err != nil {
			return fmt.Errorf("list Relay nodes for endpoint notification: %w", err)
		}
		for _, nodeID := range nodeIDs {
			if err := appendRelayEvent(tx, "relay_node_instance", nodeID, "relay.node.endpoint_changed", map[string]any{
				"nodeId": nodeID, "cellId": endpoint.CellID, "endpointId": endpoint.ID, "revision": endpoint.Revision,
			}, now); err != nil {
				return err
			}
		}
		if err := markOperationItemsSucceeded(tx, claim.ID, now); err != nil {
			return err
		}
		return finishClaimTx(tx, claim, now)
	})
}

func (store *Store) ExecuteUserAssignmentOperation(ctx context.Context, claim OperationClaim) error {
	if (claim.Type != "migrate_user" && claim.Type != "user_unpin") || claim.TargetID == nil {
		return ErrInvalidInput
	}
	mode := "auto"
	var targetCellID *uuid.UUID
	if claim.Type == "migrate_user" {
		var input MigrateUserInput
		if json.Unmarshal(claim.Request, &input) != nil {
			return ErrInvalidInput
		}
		mode, targetCellID = input.Mode, input.TargetCellID
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockClaim(tx, claim); err != nil {
			return err
		}
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", claim.TargetID.String()).Error; err != nil {
			return fmt.Errorf("lock Relay assignment user: %w", err)
		}
		var projected assignmentRow
		projectionErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("source_operation_id = ?", claim.ID).First(&projected).Error
		if projectionErr == nil {
			switch projected.Status {
			case "current":
				return finishClaimTx(tx, claim, now)
			case "pending":
				if err := tx.Model(&projected).Updates(map[string]any{
					"status": "current", "effective_at": now, "updated_at": now,
				}).Error; err != nil {
					return fmt.Errorf("activate legacy pending Relay assignment: %w", err)
				}
				return finishClaimTx(tx, claim, now)
			default:
				return ErrConflict
			}
		}
		if !errors.Is(projectionErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load Relay assignment projection state: %w", projectionErr)
		}
		var current assignmentRow
		currentError := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", *claim.TargetID).
			Order("assignment_version DESC").First(&current).Error
		if currentError != nil && !errors.Is(currentError, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load current Relay assignment: %w", currentError)
		}
		if claim.Type == "user_unpin" && (errors.Is(currentError, gorm.ErrRecordNotFound) || current.Mode != "pinned") {
			return ErrConflict
		}
		selectedCellID, err := store.selectAssignmentCell(tx, targetCellID, &current)
		if err != nil {
			return err
		}
		// Serialize the final eligibility check with Cell Drain. A drain takes an
		// UPDATE lock before snapshotting current and pending assignments, so no
		// assignment can be committed into the Cell after that snapshot.
		var selectedCell cellRow
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Select("id, status").First(&selectedCell, "id = ?", selectedCellID).Error; err != nil {
			return normalizeNotFound(err, "lock Relay assignment Cell")
		}
		if selectedCell.Status != "active" {
			return ErrConflict
		}
		schedulable, err := relayCellSchedulable(tx, selectedCellID)
		if err != nil {
			return err
		}
		if !schedulable {
			return ErrConflict
		}
		fallbacks, err := store.assignmentFallbacks(tx, selectedCellID)
		if err != nil {
			return err
		}
		fallbackJSON, _ := json.Marshal(fallbacks)
		version := int64(1)
		if !errors.Is(currentError, gorm.ErrRecordNotFound) {
			version = current.AssignmentVersion + 1
			if err := tx.Model(&current).Updates(map[string]any{
				"status": "historical", "superseded_at": now, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("supersede Relay assignment: %w", err)
			}
		}
		row := assignmentRow{
			ID: uuid.New(), UserID: *claim.TargetID, CellID: selectedCellID, AssignmentVersion: version,
			Mode: mode, Status: "current", FallbackCellIDs: fallbackJSON, LeaseExpiresAt: now.Add(assignmentLeaseDuration),
			EffectiveAt:       &now,
			SourceOperationID: &claim.ID, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("create Relay assignment: %w", err)
		}
		if mode == "pinned" {
			if err := tx.Exec(`
				INSERT INTO relay_assignment_pins (user_id, cell_id, reason, created_by, created_at, updated_at)
				VALUES (?, ?, 'administrator migration', ?, ?, ?)
				ON CONFLICT (user_id) DO UPDATE
				SET cell_id = EXCLUDED.cell_id, reason = EXCLUDED.reason, created_by = EXCLUDED.created_by,
				    expires_at = NULL, updated_at = EXCLUDED.updated_at`, *claim.TargetID, selectedCellID, claim.ActorUserID, now, now).Error; err != nil {
				return fmt.Errorf("pin Relay assignment: %w", err)
			}
		} else if err := tx.Exec("DELETE FROM relay_assignment_pins WHERE user_id = ?", *claim.TargetID).Error; err != nil {
			return fmt.Errorf("remove Relay assignment pin: %w", err)
		}
		details := map[string]any{"mode": mode, "toCellId": selectedCellID, "reason": claim.Type}
		if current.ID != uuid.Nil {
			details["fromCellId"] = current.CellID
		}
		if err := tx.Table("relay_assignment_events").Create(map[string]any{
			"id": uuid.New(), "assignment_id": row.ID, "event_type": claim.Type,
			"assignment_version": version, "details": details, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("append Relay assignment event: %w", err)
		}
		var deviceIDs []uuid.UUID
		if err := tx.Table("app_sessions").Where("user_id = ? AND revoked_at IS NULL AND idle_expires_at > ?", *claim.TargetID, now).
			Distinct("device_id").Order("device_id").Pluck("device_id", &deviceIDs).Error; err != nil {
			return fmt.Errorf("list Relay assignment devices: %w", err)
		}
		payload := map[string]any{
			"assignmentId": row.ID, "userId": row.UserID, "cellId": row.CellID,
			"assignmentVersion": version, "fallbackCellIds": fallbacks, "deviceIds": deviceIDs, "mode": mode,
		}
		if err := appendRelayEvent(tx, "relay_assignment", row.ID, "relay.assignment.changed", payload, now); err != nil {
			return err
		}
		if err := appendAudit(tx, claim.ActorUserID, "relay.assignment."+claim.Type, "user", *claim.TargetID, current, row, now); err != nil {
			return err
		}
		return finishClaimTx(tx, claim, now)
	})
}

func (store *Store) selectAssignmentCell(tx *gorm.DB, requested *uuid.UUID, current *assignmentRow) (uuid.UUID, error) {
	if requested != nil {
		schedulable, err := relayCellSchedulable(tx, *requested)
		if err != nil {
			return uuid.Nil, err
		}
		if !schedulable {
			return uuid.Nil, ErrInvalidInput
		}
		return *requested, nil
	}
	if current != nil && current.ID != uuid.Nil {
		schedulable, err := relayCellSchedulable(tx, current.CellID)
		if err != nil {
			return uuid.Nil, err
		}
		if schedulable {
			return current.CellID, nil
		}
	}
	var selectedIDText string
	err := tx.Raw(`
		SELECT cell.id::text
		FROM relay_cells cell
		JOIN relay_cell_endpoints endpoint ON endpoint.cell_id = cell.id AND endpoint.status = 'active'
		LEFT JOIN relay_node_instances instance ON instance.cell_id = cell.id
		  AND instance.status = 'ready' AND instance.lease_expires_at > now()
		WHERE cell.status = 'active'
		GROUP BY cell.id, cell.code, cell.weight, cell.connection_hard_limit
		HAVING count(instance.id) > 0 AND COALESCE(sum(instance.active_connections), 0) < cell.connection_hard_limit
		ORDER BY (COALESCE(sum(instance.active_connections), 0)::numeric / cell.connection_hard_limit) / cell.weight,
		         cell.code, cell.id
		LIMIT 1`).Scan(&selectedIDText).Error
	if err != nil {
		return uuid.Nil, fmt.Errorf("select Relay assignment Cell: %w", err)
	}
	selectedID, err := uuid.Parse(selectedIDText)
	if err != nil || selectedID == uuid.Nil {
		return uuid.Nil, ErrConflict
	}
	return selectedID, nil
}

func (store *Store) assignmentFallbacks(tx *gorm.DB, primary uuid.UUID) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0)
	if err := tx.Raw(`
		SELECT cell.id
		FROM relay_cells cell
		WHERE cell.id <> ? AND cell.status = 'active'
		  AND EXISTS (SELECT 1 FROM relay_cell_endpoints endpoint WHERE endpoint.cell_id = cell.id AND endpoint.status = 'active')
		  AND EXISTS (
			SELECT 1 FROM relay_node_instances instance
			WHERE instance.cell_id = cell.id AND instance.status = 'ready' AND instance.lease_expires_at > now()
		  )
		  AND (
			SELECT COALESCE(sum(instance.active_connections), 0)
			FROM relay_node_instances instance
			WHERE instance.cell_id = cell.id AND instance.status = 'ready' AND instance.lease_expires_at > now()
		  ) < cell.connection_hard_limit
		ORDER BY cell.code, cell.id
		LIMIT 8`, primary).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("list Relay fallback Cells: %w", err)
	}
	return ids, nil
}

func relayCellSchedulable(tx *gorm.DB, cellID uuid.UUID) (bool, error) {
	var schedulable bool
	err := tx.Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM relay_cells cell
			WHERE cell.id = ? AND cell.status = 'active'
			  AND EXISTS (SELECT 1 FROM relay_cell_endpoints endpoint WHERE endpoint.cell_id = cell.id AND endpoint.status = 'active')
			  AND EXISTS (
				SELECT 1 FROM relay_node_instances instance
				WHERE instance.cell_id = cell.id AND instance.status = 'ready' AND instance.lease_expires_at > now()
			  )
			  AND (
				SELECT COALESCE(sum(instance.active_connections), 0)
				FROM relay_node_instances instance
				WHERE instance.cell_id = cell.id AND instance.status = 'ready' AND instance.lease_expires_at > now()
			  ) < cell.connection_hard_limit
		)`, cellID).Scan(&schedulable).Error
	if err != nil {
		return false, fmt.Errorf("check Relay Cell schedulability: %w", err)
	}
	return schedulable, nil
}

func lockClaim(tx *gorm.DB, claim OperationClaim) error {
	var row operationRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND claim_token = ? AND status = ?", claim.ID, claim.ClaimToken, "running").First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrConflict
		}
		return fmt.Errorf("lock Relay operation claim: %w", err)
	}
	return nil
}

func finishClaimTx(tx *gorm.DB, claim OperationClaim, now time.Time) error {
	result := tx.Model(&operationRow{}).Where("id = ? AND claim_token = ? AND status = ?", claim.ID, claim.ClaimToken, "running").
		Updates(map[string]any{
			"status": "succeeded", "progress_completed": gorm.Expr("progress_total"),
			"error_code": nil, "error_message": nil, "finished_at": now,
			"worker_heartbeat_at": now, "updated_at": now,
		})
	if result.Error != nil {
		return fmt.Errorf("complete Relay operation: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func rescheduleClaimTx(tx *gorm.DB, claim OperationClaim, now time.Time, delay time.Duration, completed int64) error {
	if delay < time.Second || delay > time.Minute || completed < 0 {
		return ErrInvalidInput
	}
	result := tx.Model(&operationRow{}).Where("id = ? AND claim_token = ? AND status = ?", claim.ID, claim.ClaimToken, "running").Updates(map[string]any{
		"status": "pending", "claim_token": nil, "progress_completed": completed,
		"next_attempt_at": now.Add(delay), "worker_heartbeat_at": now, "updated_at": now,
	})
	if result.Error != nil {
		return fmt.Errorf("reschedule Relay operation: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func failClaimTx(tx *gorm.DB, claim OperationClaim, code, message string, now time.Time) error {
	if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status IN ?", claim.ID, []string{"pending", "running"}).Updates(map[string]any{
		"status": "failed", "attempts": gorm.Expr("attempts + 1"), "error_code": code,
		"error_message": message, "finished_at": now, "updated_at": now,
	}).Error; err != nil {
		return fmt.Errorf("fail Relay operation items: %w", err)
	}
	result := tx.Model(&operationRow{}).Where("id = ? AND claim_token = ? AND status = ?", claim.ID, claim.ClaimToken, "running").Updates(map[string]any{
		"status": "failed", "error_code": code, "error_message": message, "finished_at": now,
		"worker_heartbeat_at": now, "updated_at": now,
	})
	if result.Error != nil {
		return fmt.Errorf("fail Relay operation: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func markOperationItemsSucceeded(tx *gorm.DB, operationID uuid.UUID, now time.Time) error {
	if err := tx.Model(&operationItemRow{}).Where("operation_id = ? AND status IN ?", operationID, []string{"pending", "running"}).Updates(map[string]any{
		"status": "succeeded", "attempts": gorm.Expr("attempts + 1"), "started_at": now,
		"finished_at": now, "updated_at": now,
	}).Error; err != nil {
		return fmt.Errorf("complete Relay operation items: %w", err)
	}
	return nil
}

func normalizeNotFound(err error, action string) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return fmt.Errorf("%s: %w", action, err)
}
