package remotecontrol

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

type syncStateRow struct {
	HighWatermark            int64      `gorm:"column:high_watermark"`
	MinimumAvailableSequence int64      `gorm:"column:minimum_available_sequence"`
	LastSyncAt               *time.Time `gorm:"column:last_sync_at"`
}

func (service *Service) PushChanges(ctx context.Context, principal DevicePrincipal, input PushChangesInput) (PushChangesResult, error) {
	if principal.UserID == uuid.Nil || principal.DeviceID == uuid.Nil || len(input.Changes) == 0 || len(input.Changes) > MaximumEventBatch {
		return PushChangesResult{}, ErrInvalidInput
	}
	now := service.now().UTC()
	var result PushChangesResult
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		grant, err := service.requireGrant(ctx, tx, principal.UserID, principal.DeviceID, "remote.project.sync", true)
		projectAuthorized := err == nil
		if err != nil {
			// Task-only batches are valid when the grant deliberately omits project
			// synchronization but includes task projection access.
			if !batchContainsOnlyTasks(input.Changes) {
				return err
			}
			if _, taskErr := service.requireGrant(ctx, tx, principal.UserID, principal.DeviceID, "remote.task.read", true); taskErr != nil {
				return taskErr
			}
		}
		_ = grant
		var state syncStateRow
		stateErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_device_sync_state").
			Where("device_id = ?", principal.DeviceID).Take(&state).Error
		if errors.Is(stateErr, gorm.ErrRecordNotFound) {
			state = syncStateRow{}
			if err := tx.Table("remote_device_sync_state").Create(map[string]any{
				"device_id": principal.DeviceID, "user_id": principal.UserID, "high_watermark": 0,
				"minimum_available_sequence": 0, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("create device sync state: %w", err)
			}
		} else if stateErr != nil {
			return stateErr
		}
		if input.Reset {
			if !projectAuthorized {
				return ErrForbidden
			}
			if input.BaseHighWatermark != 0 || input.Changes[0].Sequence != 1 {
				return ErrInvalidInput
			}
			if err := tx.Exec("DELETE FROM remote_changes WHERE device_id = ?", principal.DeviceID).Error; err != nil {
				return err
			}
			if err := tx.Exec("DELETE FROM remote_projects WHERE device_id = ?", principal.DeviceID).Error; err != nil {
				return err
			}
			state.HighWatermark, state.MinimumAvailableSequence = 0, 0
		}
		if input.BaseHighWatermark < uint64(state.MinimumAvailableSequence) {
			result = PushChangesResult{HighWatermark: uint64(state.HighWatermark), ResetRequired: true}
			return nil
		}
		if input.BaseHighWatermark > uint64(state.HighWatermark) {
			return ErrSequenceGap
		}
		expected := uint64(state.HighWatermark) + 1
		for index, change := range input.Changes {
			if err := validateDeviceChange(change, now); err != nil {
				return err
			}
			if index > 0 && change.Sequence != input.Changes[index-1].Sequence+1 {
				return ErrSequenceGap
			}
			if change.Sequence < expected {
				var count int64
				if err := tx.Table("remote_changes").Where(
					"device_id = ? AND sequence = ? AND resource_kind = ? AND resource_id = ? AND operation = ? AND revision = ?",
					principal.DeviceID, change.Sequence, change.Kind, change.ResourceID, change.Operation, change.Revision,
				).Count(&count).Error; err != nil {
					return err
				}
				if count != 1 {
					return ErrConflict
				}
				result.Replayed++
				continue
			}
			if change.Sequence != expected {
				return ErrSequenceGap
			}
			if err := service.applyDeviceChange(tx, principal, change, now); err != nil {
				return err
			}
			if err := tx.Table("remote_changes").Create(map[string]any{
				"device_id": principal.DeviceID, "sequence": change.Sequence, "user_id": principal.UserID,
				"resource_kind": change.Kind, "resource_id": change.ResourceID, "operation": change.Operation,
				"revision": change.Revision, "occurred_at": change.OccurredAt.UTC(), "created_at": now,
			}).Error; err != nil {
				return err
			}
			expected++
			result.Applied++
		}
		if expected > 0 {
			state.HighWatermark = int64(expected - 1)
		}
		minimum := state.MinimumAvailableSequence
		if minimum == 0 && state.HighWatermark > 0 {
			minimum = 1
		}
		if err := tx.Table("remote_device_sync_state").Where("device_id = ?", principal.DeviceID).Updates(map[string]any{
			"high_watermark": state.HighWatermark, "minimum_available_sequence": minimum,
			"last_sync_at": now, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		result.HighWatermark = uint64(state.HighWatermark)
		return nil
	})
	if err != nil {
		return PushChangesResult{}, mapStoreError(err)
	}
	return result, nil
}

func batchContainsOnlyTasks(changes []DeviceChange) bool {
	for _, change := range changes {
		if change.Kind != "task" {
			return false
		}
	}
	return true
}

func validateDeviceChange(change DeviceChange, now time.Time) error {
	if change.Sequence == 0 || change.ResourceID == uuid.Nil || change.Revision > uint64(^uint64(0)>>1) ||
		(change.Kind != "project" && change.Kind != "task") || (change.Operation != "upsert" && change.Operation != "tombstone") ||
		change.OccurredAt.IsZero() || change.OccurredAt.After(now.Add(5*time.Minute)) {
		return ErrInvalidInput
	}
	if len(change.Metadata) > 0 && string(change.Metadata) != "{}" && string(change.Metadata) != "null" {
		// Projection metadata is intentionally closed. Adding a field requires a
		// reviewed schema so secrets, prompts and file paths cannot leak here.
		return ErrInvalidInput
	}
	if change.Kind == "project" && change.Operation == "upsert" {
		if !validText(change.DisplayName, 200) || (change.State != "available" && change.State != "unavailable" && change.State != "removed") ||
			!validCapabilities(change.Capabilities, 64) {
			return ErrInvalidInput
		}
	}
	if change.Kind == "task" && change.Operation == "upsert" {
		if !validTaskProjectionType(change.TaskType) ||
			change.Title != TaskProjectionDisplayName(change.TaskType) || !validTaskStatus(change.Status) {
			return ErrInvalidInput
		}
	}
	if change.ResultCode != nil && (!capabilityPattern.MatchString(*change.ResultCode) || len(*change.ResultCode) > 80) {
		return ErrInvalidInput
	}
	return nil
}

func validCapabilities(values []string, maximum int) bool {
	if len(values) > maximum {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !capabilityPattern.MatchString(value) {
			return false
		}
		if _, ok := seen[value]; ok {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validTaskStatus(status string) bool {
	switch status {
	case "queued", "dispatched", "accepted", "running", "cancel_requested", "cancelled", "succeeded", "failed", "rejected", "expired", "timed_out":
		return true
	default:
		return false
	}
}

func terminalTaskStatus(status string) bool {
	switch status {
	case "cancelled", "succeeded", "failed", "rejected", "expired", "timed_out":
		return true
	default:
		return false
	}
}

func (service *Service) applyDeviceChange(tx *gorm.DB, principal DevicePrincipal, change DeviceChange, now time.Time) error {
	if change.Kind == "project" {
		state := change.State
		if change.Operation == "tombstone" {
			state = "removed"
			if change.DisplayName == "" {
				change.DisplayName = "Removed project"
			}
		}
		capabilityValues := change.Capabilities
		if capabilityValues == nil {
			capabilityValues = []string{}
		}
		capabilities, _ := json.Marshal(capabilityValues)
		return tx.Exec(`
			INSERT INTO remote_projects
			    (device_id, project_id, user_id, display_name, revision, capabilities, state, observed_at, device_sequence, updated_at)
			VALUES (?, ?, ?, ?, ?, ?::jsonb, ?, ?, ?, ?)
			ON CONFLICT (device_id, project_id) DO UPDATE SET
			    display_name = EXCLUDED.display_name, revision = EXCLUDED.revision,
			    capabilities = EXCLUDED.capabilities, state = EXCLUDED.state,
			    observed_at = EXCLUDED.observed_at, device_sequence = EXCLUDED.device_sequence,
			    updated_at = EXCLUDED.updated_at
			WHERE remote_projects.revision <= EXCLUDED.revision`, principal.DeviceID, change.ResourceID, principal.UserID,
			change.DisplayName, change.Revision, string(capabilities), state, change.OccurredAt.UTC(), change.Sequence, now).Error
	}
	status := change.Status
	if change.Operation == "tombstone" && !terminalTaskStatus(status) {
		status = "expired"
	}
	if change.Operation == "tombstone" {
		result := tx.Table("remote_tasks").Where("task_id = ? AND device_id = ? AND user_id = ? AND revision <= ?",
			change.ResourceID, principal.DeviceID, principal.UserID, change.Revision).Updates(map[string]any{
			"status": status, "revision": change.Revision, "result_code": change.ResultCode,
			"finished_at": change.OccurredAt.UTC(), "device_sequence": change.Sequence, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		// An already-pruned task can still have a durable tombstone in the
		// change stream; there is no safe metadata to synthesize a task row.
		return nil
	}
	return tx.Exec(`
		INSERT INTO remote_tasks
		    (task_id, device_id, user_id, project_id, task_type, title, status, revision,
		     result_code, started_at, finished_at, device_sequence, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (task_id) DO UPDATE SET
		    project_id = COALESCE(EXCLUDED.project_id, remote_tasks.project_id),
		    task_type = EXCLUDED.task_type, title = EXCLUDED.title, status = EXCLUDED.status,
		    revision = EXCLUDED.revision, result_code = EXCLUDED.result_code,
		    started_at = COALESCE(EXCLUDED.started_at, remote_tasks.started_at),
		    finished_at = COALESCE(EXCLUDED.finished_at, remote_tasks.finished_at),
		    device_sequence = EXCLUDED.device_sequence, updated_at = EXCLUDED.updated_at
		WHERE remote_tasks.device_id = EXCLUDED.device_id AND remote_tasks.user_id = EXCLUDED.user_id
		  AND remote_tasks.revision <= EXCLUDED.revision`, change.ResourceID, principal.DeviceID, principal.UserID,
		change.ProjectID, change.TaskType, change.Title, status, change.Revision, change.ResultCode, change.StartedAt,
		change.FinishedAt, change.Sequence, change.OccurredAt.UTC(), now).Error
}

func (service *Service) PollCommands(ctx context.Context, principal DevicePrincipal, limit int) (CommandPage, error) {
	if principal.UserID == uuid.Nil || principal.DeviceID == uuid.Nil {
		return CommandPage{}, ErrInvalidInput
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return CommandPage{}, ErrInvalidInput
	}
	now := service.now().UTC()
	result := CommandPage{Items: make([]Command, 0, limit), PollAfterMs: 1500}
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var grant grantRow
		if err := tx.Table("remote_access_grants access_grant").Select("access_grant.grant_version, access_grant.status AS grant_status, credential.status AS device_status, access_grant.scopes").
			Joins("JOIN remote_device_credentials credential ON credential.device_id = access_grant.device_id AND credential.user_id = access_grant.user_id").
			Where("access_grant.user_id = ? AND access_grant.device_id = ?", principal.UserID, principal.DeviceID).
			Clauses(clause.Locking{Strength: "UPDATE", Table: clause.Table{Name: "access_grant"}}).Take(&grant).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrForbidden
		} else if err != nil {
			return err
		}
		if grant.GrantStatus != "enabled" || grant.DeviceStatus != "active" {
			return ErrForbidden
		}
		if err := tx.Table("remote_commands").Where("device_id = ? AND status IN ? AND expires_at <= ?", principal.DeviceID, []string{"queued", "leased", "accepted"}, now).
			Updates(map[string]any{"status": "expired", "lease_token": nil, "lease_expires_at": nil, "completed_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		var rows []struct {
			CommandID        uuid.UUID       `gorm:"column:command_id"`
			Kind             string          `gorm:"column:kind"`
			TaskID           *uuid.UUID      `gorm:"column:task_id"`
			Body             json.RawMessage `gorm:"column:body"`
			GrantVersion     int64           `gorm:"column:grant_version"`
			ExpectedRevision *int64          `gorm:"column:expected_revision"`
			ExpiresAt        time.Time       `gorm:"column:expires_at"`
			CreatedAt        time.Time       `gorm:"column:created_at"`
		}
		if err := tx.Raw(`
			SELECT command_id, kind, task_id, body, grant_version, expected_revision, expires_at, created_at
			FROM remote_commands
			WHERE user_id = ? AND device_id = ? AND expires_at > ?
			  AND (status = 'queued' OR (status IN ('leased', 'accepted') AND lease_expires_at <= ?))
			ORDER BY created_at, command_id
			LIMIT ? FOR UPDATE SKIP LOCKED`, principal.UserID, principal.DeviceID, now, now, limit).Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if row.GrantVersion != grant.GrantVersion || !json.Valid(row.Body) {
				if err := tx.Table("remote_commands").Where("command_id = ?", row.CommandID).
					Updates(map[string]any{"status": "cancelled", "completed_at": now, "lease_token": nil, "lease_expires_at": nil, "updated_at": now}).Error; err != nil {
					return err
				}
				continue
			}
			leaseToken := uuid.New()
			leaseExpiresAt := now.Add(DefaultCommandLease)
			if err := tx.Table("remote_commands").Where("command_id = ?", row.CommandID).Updates(map[string]any{
				"status": "leased", "lease_token": leaseToken, "lease_expires_at": leaseExpiresAt,
				"attempt_count": gorm.Expr("attempt_count + 1"), "updated_at": now,
			}).Error; err != nil {
				return err
			}
			var expected *uint64
			if row.ExpectedRevision != nil {
				value := uint64(*row.ExpectedRevision)
				expected = &value
			}
			result.Items = append(result.Items, Command{
				ID: row.CommandID, Kind: row.Kind, TaskID: row.TaskID, Body: row.Body, GrantVersion: uint64(row.GrantVersion),
				ExpectedRevision: expected, LeaseToken: leaseToken, ExpiresAt: row.ExpiresAt.UTC(), CreatedAt: row.CreatedAt.UTC(),
			})
		}
		return nil
	})
	if err != nil {
		return CommandPage{}, mapStoreError(err)
	}
	return result, nil
}

func (service *Service) AckCommand(ctx context.Context, principal DevicePrincipal, commandID uuid.UUID, input AckCommandInput) error {
	input.Status, input.FailureCode = strings.TrimSpace(input.Status), strings.TrimSpace(input.FailureCode)
	if principal.UserID == uuid.Nil || principal.DeviceID == uuid.Nil || commandID == uuid.Nil || input.LeaseToken == uuid.Nil ||
		(input.Status != "accepted" && input.Status != "completed" && input.Status != "failed") ||
		(input.FailureCode != "" && !capabilityPattern.MatchString(input.FailureCode)) {
		return ErrInvalidInput
	}
	now := service.now().UTC()
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := service.requireGrant(ctx, tx, principal.UserID, principal.DeviceID, "remote.task.write", true); err != nil {
			// A project sync-only Agent can acknowledge its sync command.
			if _, projectErr := service.requireGrant(ctx, tx, principal.UserID, principal.DeviceID, "remote.project.sync", true); projectErr != nil {
				return err
			}
		}
		var row struct {
			TaskID         *uuid.UUID `gorm:"column:task_id"`
			Kind           string     `gorm:"column:kind"`
			Status         string     `gorm:"column:status"`
			LeaseToken     *uuid.UUID `gorm:"column:lease_token"`
			LeaseExpiresAt *time.Time `gorm:"column:lease_expires_at"`
			ExpiresAt      time.Time  `gorm:"column:expires_at"`
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_commands").
			Where("command_id = ? AND user_id = ? AND device_id = ?", commandID, principal.UserID, principal.DeviceID).Take(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if row.Status == input.Status && row.LeaseToken != nil && *row.LeaseToken == input.LeaseToken {
			return nil
		}
		if row.LeaseToken == nil || *row.LeaseToken != input.LeaseToken || row.ExpiresAt.Before(now) {
			return ErrConflict
		}
		if row.Status == "leased" && (row.LeaseExpiresAt == nil || row.LeaseExpiresAt.Before(now)) {
			return ErrConflict
		}
		if row.Status != "leased" && !(row.Status == "accepted" && (input.Status == "completed" || input.Status == "failed")) {
			return ErrConflict
		}
		updates := map[string]any{"status": input.Status, "acknowledged_at": now, "updated_at": now}
		if input.Status == "completed" || input.Status == "failed" {
			updates["completed_at"] = now
			updates["failure_code"] = nil
			if input.Status == "failed" {
				updates["failure_code"] = input.FailureCode
			}
		}
		if err := tx.Table("remote_commands").Where("command_id = ?", commandID).Updates(updates).Error; err != nil {
			return err
		}
		// Command acknowledgements update delivery state only. Task execution
		// state is authoritative on the Agent and arrives through the bounded
		// projection feed; in particular accepting task.cancel must not move a
		// cancel_requested task backwards to accepted.
		return nil
	})
	return mapStoreError(err)
}

func (service *Service) PushEvents(ctx context.Context, principal DevicePrincipal, input PushEventsInput) (PushEventsResult, error) {
	if principal.UserID == uuid.Nil || principal.DeviceID == uuid.Nil || len(input.Events) == 0 || len(input.Events) > MaximumEventBatch {
		return PushEventsResult{}, ErrInvalidInput
	}
	now := service.now().UTC()
	var result PushEventsResult
	err := service.store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := service.requireGrant(ctx, tx, principal.UserID, principal.DeviceID, "remote.task.read", true); err != nil {
			return err
		}
		for _, event := range input.Events {
			if err := validateDeviceEvent(event, now); err != nil {
				return err
			}
			var prior struct {
				EventID uuid.UUID `gorm:"column:event_id"`
			}
			priorErr := tx.Table("remote_device_events").Select("event_id").
				Where("device_id = ? AND (event_id = ? OR device_sequence = ?)", principal.DeviceID, event.EventID, event.DeviceSequence).Take(&prior).Error
			if priorErr == nil {
				if prior.EventID != event.EventID {
					return ErrConflict
				}
				result.Replayed++
				continue
			}
			if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
				return priorErr
			}
			var task taskRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("remote_tasks").
				Where("task_id = ? AND user_id = ? AND device_id = ?", event.TaskID, principal.UserID, principal.DeviceID).Take(&task).Error; errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			} else if err != nil {
				return err
			}
			payload := map[string]any{}
			status := strings.TrimPrefix(event.Type, "task.")
			if event.Status != "" && event.Status != status {
				return ErrInvalidInput
			}
			if event.Revision < uint64(task.Revision) {
				return ErrConflict
			}
			payload["status"] = status
			if event.ResultCode != nil {
				payload["resultCode"] = *event.ResultCode
			}
			updates := map[string]any{
				"status": status, "revision": event.Revision, "result_code": event.ResultCode,
				"device_sequence": event.DeviceSequence, "updated_at": now,
			}
			if event.StartedAt != nil {
				updates["started_at"] = event.StartedAt.UTC()
			} else if status == "running" && task.StartedAt == nil {
				updates["started_at"] = event.OccurredAt.UTC()
			}
			if event.FinishedAt != nil {
				updates["finished_at"] = event.FinishedAt.UTC()
			} else if terminalTaskStatus(status) {
				updates["finished_at"] = event.OccurredAt.UTC()
			}
			if err := tx.Table("remote_tasks").Where("task_id = ?", event.TaskID).Updates(updates).Error; err != nil {
				return err
			}
			payloadJSON, _ := json.Marshal(payload)
			if err := tx.Table("remote_device_events").Create(map[string]any{
				"event_id": event.EventID, "device_id": principal.DeviceID, "user_id": principal.UserID,
				"task_id": event.TaskID, "device_sequence": event.DeviceSequence, "event_type": event.Type,
				"revision": event.Revision, "payload": string(payloadJSON), "occurred_at": event.OccurredAt.UTC(), "created_at": now,
			}).Error; err != nil {
				return err
			}
			result.Accepted++
		}
		return nil
	})
	if err != nil {
		return PushEventsResult{}, mapStoreError(err)
	}
	return result, nil
}

func validateDeviceEvent(event DeviceEventInput, now time.Time) error {
	if event.EventID == uuid.Nil || event.TaskID == uuid.Nil || event.DeviceSequence == 0 || event.OccurredAt.IsZero() ||
		event.OccurredAt.After(now.Add(5*time.Minute)) {
		return ErrInvalidInput
	}
	if event.Type == "task.log" || event.Log != nil {
		// Task output and tool logs are device-local business data. They are
		// available only over the E2EE Peer RPC task.logs method.
		return ErrPeerRequired
	}
	status := strings.TrimPrefix(event.Type, "task.")
	if event.Type == status || !validTaskStatus(status) || status == "queued" || status == "dispatched" || status == "cancel_requested" || status == "expired" || status == "timed_out" {
		return ErrInvalidInput
	}
	if event.ResultCode != nil && (!capabilityPattern.MatchString(*event.ResultCode) || len(*event.ResultCode) > 80) {
		return ErrInvalidInput
	}
	return nil
}
