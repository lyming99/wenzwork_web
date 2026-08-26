package relaymanagement

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	routeRejectionGrant      = "grant_revoked"
	routeRejectionAssignment = "assignment_changed"
)

func validateHeartbeatRouteShapes(input HeartbeatInput) error {
	if len(input.ResidentRoutes) > 100_000 || int64(len(input.ResidentRoutes)) > input.ActiveConnections {
		return ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(input.ResidentRoutes))
	for _, route := range input.ResidentRoutes {
		deviceID, deviceErr := uuid.Parse(route.DeviceID)
		userID, userErr := uuid.Parse(route.UserID)
		connectionID, connectionErr := uuid.Parse(route.ConnectionID)
		if deviceErr != nil || userErr != nil || connectionErr != nil ||
			deviceID.String() != strings.ToLower(route.DeviceID) || userID.String() != strings.ToLower(route.UserID) ||
			connectionID.String() != strings.ToLower(route.ConnectionID) || route.ConnectionEpoch == 0 ||
			route.AssignmentVersion == 0 || route.GrantVersion == 0 || route.ProtocolVersion != 2 {
			return ErrInvalidInput
		}
		if _, duplicate := seen[route.DeviceID]; duplicate {
			return ErrInvalidInput
		}
		seen[route.DeviceID] = struct{}{}
	}
	return nil
}

func (store *Store) negotiateHeartbeatRoutes(tx *gorm.DB, cellID uuid.UUID, input HeartbeatInput, now time.Time) ([]HeartbeatRoute, []HeartbeatRouteRejection, error) {
	if len(input.ResidentRoutes) == 0 {
		return []HeartbeatRoute{}, []HeartbeatRouteRejection{}, nil
	}
	deviceIDs := make([]uuid.UUID, 0, len(input.ResidentRoutes))
	for _, route := range input.ResidentRoutes {
		deviceIDs = append(deviceIDs, uuid.MustParse(route.DeviceID))
	}
	type credentialState struct {
		DeviceID            uuid.UUID `gorm:"column:device_id"`
		UserID              uuid.UUID `gorm:"column:user_id"`
		GrantVersion        int64     `gorm:"column:grant_version"`
		Status              string    `gorm:"column:status"`
		LastConnectionEpoch int64     `gorm:"column:last_connection_epoch"`
	}
	var credentials []credentialState
	if err := tx.Table("remote_device_credentials").Select("device_id, user_id, grant_version, status, last_connection_epoch").
		Where("device_id IN ?", deviceIDs).Find(&credentials).Error; err != nil {
		return nil, nil, fmt.Errorf("load Relay heartbeat device authority: %w", err)
	}
	credentialByDevice := make(map[uuid.UUID]credentialState, len(credentials))
	userIDs := make([]uuid.UUID, 0, len(credentials))
	for _, credential := range credentials {
		credentialByDevice[credential.DeviceID] = credential
		userIDs = append(userIDs, credential.UserID)
	}
	type assignmentState struct {
		UserID            uuid.UUID       `gorm:"column:user_id"`
		CellID            uuid.UUID       `gorm:"column:cell_id"`
		AssignmentVersion int64           `gorm:"column:assignment_version"`
		FallbackCellIDs   json.RawMessage `gorm:"column:fallback_cell_ids"`
		LeaseExpiresAt    time.Time       `gorm:"column:lease_expires_at"`
		EffectiveAt       *time.Time      `gorm:"column:effective_at"`
	}
	var assignments []assignmentState
	if len(userIDs) > 0 {
		if err := tx.Table("relay_assignments").Select("user_id, cell_id, assignment_version, fallback_cell_ids, lease_expires_at, effective_at").
			Where("user_id IN ? AND status = 'current'", userIDs).Order("assignment_version DESC").Find(&assignments).Error; err != nil {
			return nil, nil, fmt.Errorf("load Relay heartbeat assignment authority: %w", err)
		}
	}
	assignmentByUser := make(map[uuid.UUID]assignmentState, len(assignments))
	for _, assignment := range assignments {
		if _, exists := assignmentByUser[assignment.UserID]; !exists {
			assignmentByUser[assignment.UserID] = assignment
		}
	}
	accepted := make([]HeartbeatRoute, 0, len(input.ResidentRoutes))
	rejected := make([]HeartbeatRouteRejection, 0)
	for _, route := range input.ResidentRoutes {
		deviceID := uuid.MustParse(route.DeviceID)
		userID := uuid.MustParse(route.UserID)
		credential, credentialOK := credentialByDevice[deviceID]
		if !credentialOK || credential.UserID != userID || credential.Status != "active" || credential.GrantVersion < 1 ||
			uint64(credential.GrantVersion) != route.GrantVersion || credential.LastConnectionEpoch < 1 ||
			uint64(credential.LastConnectionEpoch) != route.ConnectionEpoch {
			rejected = append(rejected, heartbeatRouteRejection(route, routeRejectionGrant))
			continue
		}
		assignment, assignmentOK := assignmentByUser[userID]
		allowed := assignmentOK && assignment.EffectiveAt != nil && assignment.LeaseExpiresAt.After(now) &&
			assignment.AssignmentVersion > 0 && uint64(assignment.AssignmentVersion) == route.AssignmentVersion && assignment.CellID == cellID
		if assignmentOK && !allowed && assignment.AssignmentVersion > 0 && uint64(assignment.AssignmentVersion) == route.AssignmentVersion &&
			assignment.EffectiveAt != nil && assignment.LeaseExpiresAt.After(now) {
			var fallbacks []uuid.UUID
			if json.Unmarshal(assignment.FallbackCellIDs, &fallbacks) == nil && len(fallbacks) <= 8 {
				allowed = slices.Contains(fallbacks, cellID)
			}
		}
		if !allowed {
			rejected = append(rejected, heartbeatRouteRejection(route, routeRejectionAssignment))
			continue
		}
		accepted = append(accepted, route)
	}
	return accepted, rejected, nil
}

func heartbeatRouteRejection(route HeartbeatRoute, reason string) HeartbeatRouteRejection {
	return HeartbeatRouteRejection{
		DeviceID: route.DeviceID, ConnectionID: route.ConnectionID, ConnectionEpoch: route.ConnectionEpoch, Reason: reason,
	}
}

func (store *Store) publishHeartbeatRoutes(ctx context.Context, nodeID, cellID uuid.UUID, routes []HeartbeatRoute, leaseExpiresAt, now time.Time) error {
	store.agentConfigMu.RLock()
	publisher := store.routePublisher
	store.agentConfigMu.RUnlock()
	if publisher == nil {
		return nil
	}
	ttl := leaseExpiresAt.Sub(now)
	if ttl <= 0 {
		routes = nil
		ttl = time.Second
	}
	return publisher.PublishRelayRoutes(ctx, nodeID, cellID, routes, ttl, now)
}
