package relaymanagement

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	dnsLabelPattern       = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

func (store *Store) ListNodes(ctx context.Context, cellID uuid.UUID) ([]NodeInstance, error) {
	if cellID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	var count int64
	if err := store.db.WithContext(ctx).Model(&cellRow{}).Where("id = ?", cellID).Count(&count).Error; err != nil {
		return nil, fmt.Errorf("load Relay Cell: %w", err)
	}
	if count == 0 {
		return nil, ErrNotFound
	}
	var rows []instanceRow
	if err := store.db.WithContext(ctx).Where("cell_id = ?", cellID).Order("started_at DESC, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list Relay nodes: %w", err)
	}
	items := make([]NodeInstance, 0, len(rows))
	for _, row := range rows {
		items = append(items, instanceFromRow(row, store.now()))
	}
	return items, nil
}

func (store *Store) ListEndpoints(ctx context.Context, cellID uuid.UUID) ([]ManagedEndpoint, error) {
	if cellID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	var rows []endpointRow
	if err := store.db.WithContext(ctx).Where("cell_id = ?", cellID).Order("revision DESC, id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list Relay endpoints: %w", err)
	}
	if len(rows) == 0 {
		var count int64
		if err := store.db.WithContext(ctx).Model(&cellRow{}).Where("id = ?", cellID).Count(&count).Error; err != nil {
			return nil, fmt.Errorf("load Relay Cell: %w", err)
		}
		if count == 0 {
			return nil, ErrNotFound
		}
	}
	items := make([]ManagedEndpoint, 0, len(rows))
	for _, row := range rows {
		items = append(items, endpointFromRow(row))
	}
	return items, nil
}

func (store *Store) GetEndpoint(ctx context.Context, endpointID uuid.UUID) (ManagedEndpoint, error) {
	if endpointID == uuid.Nil {
		return ManagedEndpoint{}, ErrNotFound
	}
	var row endpointRow
	if err := store.db.WithContext(ctx).First(&row, "id = ?", endpointID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ManagedEndpoint{}, ErrNotFound
		}
		return ManagedEndpoint{}, fmt.Errorf("load Relay endpoint: %w", err)
	}
	return endpointFromRow(row), nil
}

func (store *Store) ResolveEndpointIdentity(ctx context.Context, cellID, installationID, instanceID uuid.UUID) (ed25519.PublicKey, error) {
	if cellID == uuid.Nil || installationID == uuid.Nil || instanceID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	var row struct {
		PublicKey []byte `gorm:"column:identity_public_key"`
	}
	err := store.db.WithContext(ctx).Table("relay_node_installations installation").Select("installation.identity_public_key").
		Joins("JOIN relay_node_instances instance ON instance.id = installation.current_instance_id AND instance.installation_id = installation.id").
		Where(`installation.id = ? AND installation.cell_id = ? AND installation.status IN ?
			AND instance.id = ? AND instance.cell_id = ? AND instance.status = 'ready' AND instance.lease_expires_at > ?`,
			installationID, cellID, []string{"pending_activation", "active"}, instanceID, cellID, store.now().UTC()).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve Relay endpoint identity: %w", err)
	}
	if len(row.PublicKey) != ed25519.PublicKeySize {
		return nil, ErrIdentityMismatch
	}
	return ed25519.PublicKey(append([]byte(nil), row.PublicKey...)), nil
}

func (store *Store) RequestCellUpdate(ctx context.Context, cellID uuid.UUID, input UpdateCellInput) (Operation, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if cellID == uuid.Nil || input.ActorUserID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) ||
		(input.Status == nil && input.Weight == nil && input.ConnectionSoftLimit == nil && input.ConnectionHardLimit == nil &&
			input.FileBandwidthSoftLimitMbps == nil && input.FileBandwidthHardLimitMbps == nil) {
		return Operation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var operationID uuid.UUID
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if existing, ok, err := findIdempotentOperation(tx, input.ActorUserID, input.IdempotencyKey, "cell_update", "relay_cell", &cellID, input); err != nil {
			return err
		} else if ok {
			operationID = existing.ID
			return nil
		}
		var cell struct {
			Status                     string  `gorm:"column:status"`
			Weight                     float64 `gorm:"column:weight"`
			ConnectionSoftLimit        int64   `gorm:"column:connection_soft_limit"`
			ConnectionHardLimit        int64   `gorm:"column:connection_hard_limit"`
			FileBandwidthSoftLimitMbps float64 `gorm:"column:file_bandwidth_soft_limit_mbps"`
			FileBandwidthHardLimitMbps float64 `gorm:"column:file_bandwidth_hard_limit_mbps"`
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Table("relay_cells").Select("status, weight, connection_soft_limit, connection_hard_limit, file_bandwidth_soft_limit_mbps, file_bandwidth_hard_limit_mbps").Where("id = ?", cellID).Take(&cell).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay Cell: %w", err)
		}
		if input.Status != nil {
			cell.Status = strings.ToLower(strings.TrimSpace(*input.Status))
		}
		if input.Weight != nil {
			cell.Weight = *input.Weight
		}
		if input.ConnectionSoftLimit != nil {
			cell.ConnectionSoftLimit = *input.ConnectionSoftLimit
		}
		if input.ConnectionHardLimit != nil {
			cell.ConnectionHardLimit = *input.ConnectionHardLimit
		}
		if input.FileBandwidthSoftLimitMbps != nil {
			cell.FileBandwidthSoftLimitMbps = *input.FileBandwidthSoftLimitMbps
		}
		if input.FileBandwidthHardLimitMbps != nil {
			cell.FileBandwidthHardLimitMbps = *input.FileBandwidthHardLimitMbps
		}
		if !validCellUpdate(cell.Status, cell.Weight, cell.ConnectionSoftLimit, cell.ConnectionHardLimit, cell.FileBandwidthSoftLimitMbps, cell.FileBandwidthHardLimitMbps) {
			return ErrInvalidInput
		}
		created, err := createOperationTx(tx, CreateOperationInput{
			Type: "cell_update", TargetType: "relay_cell", TargetID: &cellID, Request: input,
			ActorUserID: input.ActorUserID, IdempotencyKey: input.IdempotencyKey,
		}, now)
		if err != nil {
			return err
		}
		operationID = created.ID
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay.cell.update.request", "relay_cell", cellID, nil, input, now)
	})
	if err != nil {
		return Operation{}, err
	}
	return store.GetOperation(ctx, operationID)
}

func (store *Store) CreateEndpoint(ctx context.Context, input CreateEndpointInput) (Operation, error) {
	input.EndpointType = strings.ToLower(strings.TrimSpace(input.EndpointType))
	input.PublicEndpoint = strings.TrimSpace(input.PublicEndpoint)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.CellID == uuid.Nil || input.ActorUserID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) ||
		validateEndpointSyntax(input.EndpointType, input.PublicEndpoint) != nil {
		return Operation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var operationID uuid.UUID
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if existing, ok, err := findIdempotentOperation(tx, input.ActorUserID, input.IdempotencyKey, "endpoint_validate", "relay_cell_endpoint", nil, input); err != nil {
			return err
		} else if ok {
			operationID = existing.ID
			return nil
		}
		var cell cellRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cell, "id = ?", input.CellID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay Cell: %w", err)
		}
		var revision int64
		if err := tx.Model(&endpointRow{}).Where("cell_id = ?", input.CellID).Select("COALESCE(MAX(revision), 0)").Scan(&revision).Error; err != nil {
			return fmt.Errorf("allocate Relay endpoint revision: %w", err)
		}
		row := endpointRow{
			ID: uuid.New(), CellID: input.CellID, Revision: revision + 1, EndpointType: input.EndpointType,
			PublicEndpoint: input.PublicEndpoint, Status: "validating", ValidationResult: json.RawMessage(`{}`),
			Version: 1, CreatedBy: uuidPointer(input.ActorUserID), CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("create Relay endpoint: %w", err)
		}
		created, err := createOperationTx(tx, CreateOperationInput{
			Type: "endpoint_validate", TargetType: "relay_cell_endpoint", TargetID: &row.ID,
			Request: input, ItemTargetType: "relay_cell_endpoint", ItemTargetIDs: []uuid.UUID{row.ID},
			ActorUserID: input.ActorUserID, IdempotencyKey: input.IdempotencyKey,
		}, now)
		if err != nil {
			return err
		}
		operationID = created.ID
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay.endpoint.create", "relay_cell_endpoint", row.ID, nil, endpointFromRow(row), now)
	})
	if err != nil {
		return Operation{}, err
	}
	return store.GetOperation(ctx, operationID)
}

func (store *Store) UpdateEndpoint(ctx context.Context, endpointID uuid.UUID, input UpdateEndpointInput) (Operation, error) {
	input.EndpointType = strings.ToLower(strings.TrimSpace(input.EndpointType))
	input.PublicEndpoint = strings.TrimSpace(input.PublicEndpoint)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if endpointID == uuid.Nil || input.ActorUserID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) ||
		validateEndpointSyntax(input.EndpointType, input.PublicEndpoint) != nil {
		return Operation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var operationID uuid.UUID
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if existing, ok, err := findIdempotentOperation(tx, input.ActorUserID, input.IdempotencyKey, "endpoint_validate", "relay_cell_endpoint", &endpointID, input); err != nil {
			return err
		} else if ok {
			operationID = existing.ID
			return nil
		}
		var row endpointRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", endpointID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay endpoint: %w", err)
		}
		if row.Status == "active" || row.Status == "draining" || row.Status == "retired" {
			return ErrConflict
		}
		before := endpointFromRow(row)
		row.EndpointType, row.PublicEndpoint, row.Status = input.EndpointType, input.PublicEndpoint, "validating"
		row.ValidationResult = json.RawMessage(`{}`)
		row.CertificateNotAfter, row.ValidatedAt = nil, nil
		row.Version++
		row.UpdatedAt = now
		if err := tx.Save(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return fmt.Errorf("update Relay endpoint: %w", err)
		}
		created, err := createOperationTx(tx, CreateOperationInput{
			Type: "endpoint_validate", TargetType: "relay_cell_endpoint", TargetID: &row.ID,
			Request: input, ItemTargetType: "relay_cell_endpoint", ItemTargetIDs: []uuid.UUID{row.ID},
			ActorUserID: input.ActorUserID, IdempotencyKey: input.IdempotencyKey,
		}, now)
		if err != nil {
			return err
		}
		operationID = created.ID
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay.endpoint.update", "relay_cell_endpoint", row.ID, before, endpointFromRow(row), now)
	})
	if err != nil {
		return Operation{}, err
	}
	return store.GetOperation(ctx, operationID)
}

func (store *Store) RequestEndpointValidation(ctx context.Context, endpointID, actorUserID uuid.UUID, idempotencyKey string) (Operation, error) {
	return store.requestEndpointOperation(ctx, endpointID, actorUserID, idempotencyKey, "endpoint_validate", []string{"draft", "failed", "validated"})
}

func (store *Store) RequestEndpointActivation(ctx context.Context, endpointID, actorUserID uuid.UUID, idempotencyKey string) (Operation, error) {
	return store.requestEndpointOperation(ctx, endpointID, actorUserID, idempotencyKey, "endpoint_activate", []string{"validated"})
}

func (store *Store) requestEndpointOperation(ctx context.Context, endpointID, actorUserID uuid.UUID, idempotencyKey, operationType string, statuses []string) (Operation, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if endpointID == uuid.Nil || actorUserID == uuid.Nil || !validIdempotencyKey(idempotencyKey) {
		return Operation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var operationID uuid.UUID
	request := map[string]any{"endpointId": endpointID}
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if existing, ok, err := findIdempotentOperation(tx, actorUserID, idempotencyKey, operationType, "relay_cell_endpoint", &endpointID, request); err != nil {
			return err
		} else if ok {
			operationID = existing.ID
			return nil
		}
		var row endpointRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", endpointID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay endpoint: %w", err)
		}
		allowed := false
		for _, status := range statuses {
			allowed = allowed || row.Status == status
		}
		if !allowed {
			return ErrConflict
		}
		if operationType == "endpoint_validate" {
			row.Status = "validating"
			row.ValidationResult = json.RawMessage(`{}`)
			row.CertificateNotAfter, row.ValidatedAt = nil, nil
			row.Version++
			row.UpdatedAt = now
			if err := tx.Save(&row).Error; err != nil {
				return fmt.Errorf("mark Relay endpoint validating: %w", err)
			}
		}
		created, err := createOperationTx(tx, CreateOperationInput{
			Type: operationType, TargetType: "relay_cell_endpoint", TargetID: &endpointID, Request: request,
			ItemTargetType: "relay_cell_endpoint", ItemTargetIDs: []uuid.UUID{endpointID},
			ActorUserID: actorUserID, IdempotencyKey: idempotencyKey,
		}, now)
		if err != nil {
			return err
		}
		operationID = created.ID
		return appendAudit(tx, uuidPointer(actorUserID), "relay."+strings.ReplaceAll(operationType, "_", ".")+".request", "relay_cell_endpoint", endpointID, nil, request, now)
	})
	if err != nil {
		return Operation{}, err
	}
	return store.GetOperation(ctx, operationID)
}

func (store *Store) RequestNodeDrain(ctx context.Context, nodeID, actorUserID uuid.UUID, idempotencyKey string) (Operation, error) {
	return store.requestDrain(ctx, "node_drain", "relay_node_instance", nodeID, actorUserID, idempotencyKey)
}

func (store *Store) RequestCellDrain(ctx context.Context, cellID, actorUserID uuid.UUID, idempotencyKey string) (Operation, error) {
	return store.requestDrain(ctx, "cell_drain", "relay_cell", cellID, actorUserID, idempotencyKey)
}

func (store *Store) requestDrain(ctx context.Context, operationType, targetType string, targetID, actorUserID uuid.UUID, idempotencyKey string) (Operation, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if targetID == uuid.Nil || actorUserID == uuid.Nil || !validIdempotencyKey(idempotencyKey) {
		return Operation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var operationID uuid.UUID
	request := map[string]any{"targetId": targetID, "deadlineSeconds": 900}
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if existing, ok, err := findIdempotentOperation(tx, actorUserID, idempotencyKey, operationType, targetType, &targetID, request); err != nil {
			return err
		} else if ok {
			operationID = existing.ID
			return nil
		}
		var itemIDs []uuid.UUID
		var additionalItems []OperationTarget
		var drainingCell *cellRow
		if operationType == "node_drain" {
			var node instanceRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&node, "id = ?", targetID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return fmt.Errorf("lock Relay node: %w", err)
			}
			if node.Status != "ready" && node.Status != "starting" {
				return ErrConflict
			}
			itemIDs = []uuid.UUID{targetID}
		} else {
			var cell cellRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cell, "id = ?", targetID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return fmt.Errorf("lock Relay Cell: %w", err)
			}
			if cell.Status == "disabled" || cell.Status == "draining" {
				return ErrConflict
			}
			cell.Status, cell.Version = "draining", cell.Version+1
			if err := tx.Model(&cell).Updates(map[string]any{
				"status": cell.Status, "version": cell.Version, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("mark Relay Cell draining: %w", err)
			}
			drainingCell = &cell
			if err := tx.Model(&instanceRow{}).Where("cell_id = ? AND status IN ?", targetID, []string{"starting", "ready", "draining"}).Pluck("id", &itemIDs).Error; err != nil {
				return fmt.Errorf("list Relay nodes for drain: %w", err)
			}
			var userIDs []uuid.UUID
			if err := tx.Model(&assignmentRow{}).Distinct("user_id").Where("cell_id = ? AND status IN ?", targetID, []string{"current", "pending"}).Order("user_id").Pluck("user_id", &userIDs).Error; err != nil {
				return fmt.Errorf("list Relay assignments for drain: %w", err)
			}
			additionalItems = make([]OperationTarget, 0, len(userIDs))
			for _, userID := range userIDs {
				additionalItems = append(additionalItems, OperationTarget{TargetType: "user", TargetID: userID})
			}
		}
		created, err := createOperationTx(tx, CreateOperationInput{
			Type: operationType, TargetType: targetType, TargetID: &targetID, Request: request,
			ItemTargetType: "relay_node_instance", ItemTargetIDs: itemIDs,
			AdditionalItems: additionalItems,
			ActorUserID:     actorUserID, IdempotencyKey: idempotencyKey,
		}, now)
		if err != nil {
			return err
		}
		operationID = created.ID
		if drainingCell != nil {
			var activeConnections int64
			if err := tx.Model(&instanceRow{}).Where("cell_id = ? AND status IN ? AND lease_expires_at > ?", drainingCell.ID, []string{"ready", "draining"}, now).
				Select("COALESCE(sum(active_connections), 0)").Scan(&activeConnections).Error; err != nil {
				return fmt.Errorf("sum draining Relay Cell connections: %w", err)
			}
			if err := appendRelayEvent(tx, "relay_cell", drainingCell.ID, "relay.cell.updated", map[string]any{
				"cellId": drainingCell.ID, "status": drainingCell.Status, "version": drainingCell.Version,
				"activeConnections": activeConnections, "connectionHardLimit": drainingCell.ConnectionHardLimit,
				"operationId": created.ID,
			}, now); err != nil {
				return err
			}
		}
		return appendAudit(tx, uuidPointer(actorUserID), "relay."+strings.ReplaceAll(operationType, "_", ".")+".request", targetType, targetID, nil, request, now)
	})
	if err != nil {
		return Operation{}, err
	}
	return store.GetOperation(ctx, operationID)
}

func (store *Store) ListAssignments(ctx context.Context, userID uuid.UUID) ([]Assignment, error) {
	if userID == uuid.Nil {
		return nil, ErrInvalidInput
	}
	var rows []assignmentRow
	if err := store.db.WithContext(ctx).Where("user_id = ?", userID).Order("assignment_version DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list Relay assignments: %w", err)
	}
	cellIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		cellIDs = append(cellIDs, row.CellID)
	}
	type cellCodeRow struct {
		ID   uuid.UUID `gorm:"column:id"`
		Code string    `gorm:"column:code"`
	}
	var cellRows []cellCodeRow
	if len(cellIDs) > 0 {
		if err := store.db.WithContext(ctx).Table("relay_cells").Select("id, code").Where("id IN ?", cellIDs).Scan(&cellRows).Error; err != nil {
			return nil, fmt.Errorf("list Relay assignment Cells: %w", err)
		}
	}
	cellCodes := make(map[uuid.UUID]string, len(cellRows))
	for _, row := range cellRows {
		cellCodes[row.ID] = row.Code
	}
	items := make([]Assignment, 0, len(rows))
	for _, row := range rows {
		items = append(items, assignmentFromRow(row, cellCodes[row.CellID]))
	}
	return items, nil
}

func (store *Store) RequestUserMigration(ctx context.Context, input MigrateUserInput) (Operation, error) {
	input.Mode = strings.ToLower(strings.TrimSpace(input.Mode))
	input.Confirmation = strings.TrimSpace(input.Confirmation)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.UserID == uuid.Nil || input.ActorUserID == uuid.Nil || !validIdempotencyKey(input.IdempotencyKey) ||
		input.Confirmation != "migrate_relay_user" || (input.Mode != "auto" && input.Mode != "pinned") ||
		(input.Mode == "pinned" && (input.TargetCellID == nil || *input.TargetCellID == uuid.Nil)) ||
		(input.Mode == "auto" && input.TargetCellID != nil) {
		return Operation{}, ErrInvalidInput
	}
	return store.requestUserOperation(ctx, input.UserID, input.ActorUserID, input.IdempotencyKey, "migrate_user", input)
}

func (store *Store) RequestUserUnpin(ctx context.Context, userID, actorUserID uuid.UUID, idempotencyKey string) (Operation, error) {
	request := map[string]any{"userId": userID, "mode": "auto"}
	return store.requestUserOperation(ctx, userID, actorUserID, strings.TrimSpace(idempotencyKey), "user_unpin", request)
}

func (store *Store) requestUserOperation(ctx context.Context, userID, actorUserID uuid.UUID, idempotencyKey, operationType string, request any) (Operation, error) {
	if userID == uuid.Nil || actorUserID == uuid.Nil || !validIdempotencyKey(idempotencyKey) {
		return Operation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var operationID uuid.UUID
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if existing, ok, err := findIdempotentOperation(tx, actorUserID, idempotencyKey, operationType, "user", &userID, request); err != nil {
			return err
		} else if ok {
			operationID = existing.ID
			return nil
		}
		var userCount int64
		if err := tx.Table("users").Where("id = ?", userID).Count(&userCount).Error; err != nil {
			return fmt.Errorf("load Relay assignment user: %w", err)
		}
		if userCount == 0 {
			return ErrNotFound
		}
		if migration, ok := request.(MigrateUserInput); ok && migration.TargetCellID != nil {
			var cellCount int64
			if err := tx.Model(&cellRow{}).Where("id = ? AND status = ?", *migration.TargetCellID, "active").Count(&cellCount).Error; err != nil {
				return fmt.Errorf("load target Relay Cell: %w", err)
			}
			if cellCount == 0 {
				return ErrInvalidInput
			}
		}
		created, err := createOperationTx(tx, CreateOperationInput{
			Type: operationType, TargetType: "user", TargetID: &userID, Request: request,
			ActorUserID: actorUserID, IdempotencyKey: idempotencyKey,
		}, now)
		if err != nil {
			return err
		}
		operationID = created.ID
		return appendAudit(tx, uuidPointer(actorUserID), "relay."+strings.ReplaceAll(operationType, "_", ".")+".request", "user", userID, nil, request, now)
	})
	if err != nil {
		return Operation{}, err
	}
	return store.GetOperation(ctx, operationID)
}

func (store *Store) GetOperation(ctx context.Context, operationID uuid.UUID) (Operation, error) {
	if operationID == uuid.Nil {
		return Operation{}, ErrNotFound
	}
	var row operationRow
	if err := store.db.WithContext(ctx).First(&row, "id = ?", operationID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Operation{}, ErrNotFound
		}
		return Operation{}, fmt.Errorf("load Relay operation: %w", err)
	}
	var itemRows []operationItemRow
	if err := store.db.WithContext(ctx).Where("operation_id = ?", operationID).Order("created_at, id").Find(&itemRows).Error; err != nil {
		return Operation{}, fmt.Errorf("list Relay operation items: %w", err)
	}
	return operationFromRows(row, itemRows), nil
}

func createOperationTx(tx *gorm.DB, input CreateOperationInput, now time.Time) (operationRow, error) {
	encoded, err := json.Marshal(input.Request)
	if err != nil {
		return operationRow{}, fmt.Errorf("marshal Relay operation request: %w", err)
	}
	row := operationRow{
		ID: uuid.New(), OperationType: input.Type, Status: "pending", TargetType: input.TargetType,
		TargetID: input.TargetID, RequestJSON: encoded, ProgressTotal: len(input.ItemTargetIDs) + len(input.AdditionalItems),
		IdempotencyKey: stringPointer(input.IdempotencyKey), CreatedBy: uuidPointer(input.ActorUserID),
		NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&row).Error; err != nil {
		if isUniqueViolation(err) {
			return operationRow{}, ErrConflict
		}
		return operationRow{}, fmt.Errorf("create Relay operation: %w", err)
	}
	for _, targetID := range input.ItemTargetIDs {
		copy := targetID
		item := operationItemRow{
			ID: uuid.New(), OperationID: row.ID, TargetType: input.ItemTargetType, TargetID: &copy,
			Status: "pending", CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&item).Error; err != nil {
			return operationRow{}, fmt.Errorf("create Relay operation item: %w", err)
		}
	}
	for _, target := range input.AdditionalItems {
		if target.TargetID == uuid.Nil || !validOperationItemTargetType(target.TargetType) {
			return operationRow{}, ErrInvalidInput
		}
		copy := target.TargetID
		item := operationItemRow{
			ID: uuid.New(), OperationID: row.ID, TargetType: target.TargetType, TargetID: &copy,
			Status: "pending", CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&item).Error; err != nil {
			return operationRow{}, fmt.Errorf("create Relay operation item: %w", err)
		}
	}
	return row, nil
}

func validOperationItemTargetType(value string) bool {
	switch value {
	case "relay_node_instance", "relay_cell_endpoint", "user":
		return true
	default:
		return false
	}
}

func findIdempotentOperation(tx *gorm.DB, actorUserID uuid.UUID, key, operationType, targetType string, targetID *uuid.UUID, request any) (operationRow, bool, error) {
	// Serialize requests sharing an actor/key before looking up the unique row.
	// This closes the read-then-insert race while preserving replay semantics.
	if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", actorUserID.String()+"|"+key).Error; err != nil {
		return operationRow{}, false, fmt.Errorf("lock Relay idempotency key: %w", err)
	}
	var row operationRow
	err := tx.Where("created_by = ? AND idempotency_key = ?", actorUserID, key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return operationRow{}, false, nil
	}
	if err != nil {
		return operationRow{}, false, fmt.Errorf("load idempotent Relay operation: %w", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return operationRow{}, false, ErrInvalidInput
	}
	if row.OperationType != operationType || row.TargetType != targetType || (targetID != nil && !equalUUIDPointers(row.TargetID, targetID)) ||
		!jsonBytesEqual(row.RequestJSON, encoded) {
		return operationRow{}, false, ErrConflict
	}
	return row, true, nil
}

func operationFromRows(row operationRow, itemRows []operationItemRow) Operation {
	status := row.Status
	if status == "pending" {
		status = "queued"
	} else if status == "timed_out" {
		status = "failed"
	}
	percent := 0
	if row.ProgressTotal > 0 {
		percent = row.ProgressCompleted * 100 / row.ProgressTotal
	} else if row.Status == "succeeded" {
		percent = 100
	}
	items := make([]OperationItem, 0, len(itemRows))
	for _, item := range itemRows {
		items = append(items, OperationItem{
			ID: item.ID, TargetType: item.TargetType, TargetID: item.TargetID, Status: item.Status,
			Attempts: item.Attempts, ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage,
			StartedAt: utcPointer(item.StartedAt), FinishedAt: utcPointer(item.FinishedAt),
		})
	}
	return Operation{
		ID: row.ID, Type: row.OperationType, Status: status, TargetType: row.TargetType, TargetID: row.TargetID,
		Request: row.RequestJSON, ProgressCompleted: row.ProgressCompleted, ProgressTotal: row.ProgressTotal,
		ProgressPercent: percent, ResultCode: row.ErrorCode, ErrorMessage: row.ErrorMessage, Items: items,
		StartedAt: utcPointer(row.StartedAt), FinishedAt: utcPointer(row.FinishedAt),
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func endpointFromRow(row endpointRow) ManagedEndpoint {
	validation := row.ValidationResult
	if len(validation) == 0 {
		validation = json.RawMessage(`{}`)
	}
	return ManagedEndpoint{
		ID: row.ID, CellID: row.CellID, Revision: row.Revision, EndpointType: row.EndpointType,
		PublicEndpoint: row.PublicEndpoint, Status: row.Status, ValidationResult: validation,
		CertificateNotAfter: utcPointer(row.CertificateNotAfter), ValidatedAt: utcPointer(row.ValidatedAt),
		ActivatedAt: utcPointer(row.ActivatedAt), DrainUntil: utcPointer(row.DrainUntil),
		SupersededAt: utcPointer(row.SupersededAt), Version: row.Version,
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func assignmentFromRow(row assignmentRow, cellCode string) Assignment {
	fallbacks := make([]uuid.UUID, 0)
	_ = json.Unmarshal(row.FallbackCellIDs, &fallbacks)
	status := row.Status
	if status == "current" {
		status = "effective"
	}
	return Assignment{
		ID: row.ID, UserID: row.UserID, CellID: row.CellID, CellCode: cellCode,
		AssignmentVersion: row.AssignmentVersion, Mode: row.Mode, Status: status,
		FallbackCellIDs: fallbacks, LeaseExpiresAt: row.LeaseExpiresAt.UTC(),
		EffectiveAt: utcPointer(row.EffectiveAt), SupersededAt: utcPointer(row.SupersededAt),
		CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func validateEndpointSyntax(endpointType, raw string) error {
	if endpointType != "domain" && endpointType != "ip" {
		return ErrInvalidInput
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "wss" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/v2/connect" {
		return ErrInvalidInput
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" || hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") {
		return ErrInvalidInput
	}
	address := net.ParseIP(hostname)
	if endpointType == "ip" {
		if address == nil || !publicIP(address) {
			return ErrInvalidInput
		}
		return nil
	}
	if address != nil || len(hostname) > 253 {
		return ErrInvalidInput
	}
	labels := strings.Split(hostname, ".")
	if len(labels) < 2 {
		return ErrInvalidInput
	}
	for _, label := range labels {
		if !dnsLabelPattern.MatchString(label) {
			return ErrInvalidInput
		}
	}
	return nil
}

func publicIP(address net.IP) bool {
	return address != nil && !address.IsUnspecified() && !address.IsLoopback() && !address.IsPrivate() &&
		!address.IsLinkLocalUnicast() && !address.IsLinkLocalMulticast() && !address.IsMulticast()
}

func validCellUpdate(status string, weight float64, soft, hard int64, bandwidthSoft, bandwidthHard float64) bool {
	return (status == "draft" || status == "active" || status == "draining" || status == "disabled") &&
		weight > 0 && weight <= 100 && soft > 0 && hard >= soft && bandwidthSoft > 0 && bandwidthHard >= bandwidthSoft
}

func validIdempotencyKey(value string) bool {
	return idempotencyKeyPattern.MatchString(value)
}

func equalUUIDPointers(left, right *uuid.UUID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func jsonBytesEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftCanonical, leftErr := json.Marshal(leftValue)
	rightCanonical, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftCanonical, rightCanonical)
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	copy := value
	return &copy
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}
