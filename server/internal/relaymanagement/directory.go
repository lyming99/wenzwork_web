package relaymanagement

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (store *Store) RegisterInstance(ctx context.Context, identity NodeCertificateIdentity, input RegisterInstanceInput) (NodeInstance, error) {
	input.Version = strings.TrimSpace(input.Version)
	if identity.InstallationID == uuid.Nil || identity.CellID == uuid.Nil || len(identity.Thumbprint) != 64 ||
		input.InstanceID == uuid.Nil || !validPlainText(input.Version, 64, false) || input.ProtocolVersion != 2 || len(input.Addresses) > 16 {
		return NodeInstance{}, ErrInvalidInput
	}
	for _, address := range input.Addresses {
		if !validPlainText(strings.TrimSpace(address), 255, false) {
			return NodeInstance{}, ErrInvalidInput
		}
	}
	addresses, err := limitedJSON(input.Addresses, 16<<10)
	if err != nil {
		return NodeInstance{}, ErrInvalidInput
	}
	capabilities, err := limitedJSON(input.Capabilities, 64<<10)
	if err != nil {
		return NodeInstance{}, ErrInvalidInput
	}
	now := store.now().UTC()
	startedAt := input.StartedAt.UTC()
	if startedAt.IsZero() || startedAt.Before(now.Add(-24*time.Hour)) || startedAt.After(now.Add(5*time.Minute)) {
		startedAt = now
	}
	row := instanceRow{
		ID: input.InstanceID, InstallationID: identity.InstallationID, CellID: identity.CellID,
		Status: "starting", Version: input.Version, ProtocolVersion: input.ProtocolVersion,
		Addresses: addresses, Capabilities: capabilities, StartedAt: startedAt,
		LastHeartbeatAt: now, LeaseExpiresAt: now.Add(store.nodeLeaseDuration), CreatedAt: now,
	}
	var result NodeInstance
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", identity.InstallationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation for instance registration: %w", err)
		}
		if installation.CellID != identity.CellID || installation.IdentityThumbprint == nil ||
			*installation.IdentityThumbprint != identity.Thumbprint {
			return ErrIdentityMismatch
		}
		if installation.Status == "revoked" {
			return ErrInstallationRevoked
		}
		if installation.Status == "disabled" || installation.Status == "deleted" {
			return ErrConflict
		}
		var release releaseRow
		if installation.ReleaseID != nil {
			if err := tx.First(&release, "id = ?", *installation.ReleaseID).Error; err != nil ||
				release.Version != input.Version || input.ProtocolVersion < release.ProtocolMin || input.ProtocolVersion > release.ProtocolMax {
				return ErrConflict
			}
		}
		var existing instanceRow
		existingErr := tx.First(&existing, "id = ?", input.InstanceID).Error
		if existingErr == nil {
			if existing.InstallationID != identity.InstallationID || existing.CellID != identity.CellID {
				return ErrConflict
			}
			row = existing
		} else if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load Relay node instance: %w", existingErr)
		} else if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("register Relay node instance: %w", err)
		}
		updates := map[string]any{"current_instance_id": row.ID, "updated_at": now, "version": installation.Version + 1}
		if installation.Status == "enrolled" {
			updates["status"] = "pending_activation"
		}
		if err := tx.Model(&installation).Updates(updates).Error; err != nil {
			return fmt.Errorf("select current Relay node instance: %w", err)
		}
		result = instanceFromRow(row, now)
		return appendRelayEvent(tx, "relay_node_instance", row.ID, "relay.instance.registered", map[string]any{
			"installationId": installation.ID, "instanceId": row.ID, "cellId": row.CellID,
		}, now)
	})
	if err != nil {
		return NodeInstance{}, err
	}
	return result, nil
}

func (store *Store) Heartbeat(ctx context.Context, identity NodeCertificateIdentity, input HeartbeatInput) (HeartbeatResult, error) {
	if identity.InstallationID == uuid.Nil || identity.CellID == uuid.Nil || len(identity.Thumbprint) != 64 || input.InstanceID == uuid.Nil ||
		input.ActiveConnections < 0 || input.ActiveFileTransfers < 0 || input.MemoryBytes < 0 || !finiteNonnegative(input.IngressMbps) ||
		!finiteNonnegative(input.EgressMbps) || !finiteNonnegative(input.WriteLoopLagMS) || len(input.Addresses) > 16 ||
		validateHeartbeatRouteShapes(input) != nil {
		return HeartbeatResult{}, ErrInvalidInput
	}
	addresses, err := limitedJSON(input.Addresses, 16<<10)
	if err != nil {
		return HeartbeatResult{}, ErrInvalidInput
	}
	capabilities, err := limitedJSON(input.Capabilities, 64<<10)
	if err != nil {
		return HeartbeatResult{}, ErrInvalidInput
	}
	now := store.now().UTC()
	leaseExpiresAt := now.Add(store.nodeLeaseDuration)
	result := HeartbeatResult{LeaseExpiresAt: leaseExpiresAt}
	var acceptedRoutes []HeartbeatRoute
	var rejectedRoutes []HeartbeatRouteRejection
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", identity.InstallationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation for heartbeat: %w", err)
		}
		if installation.CellID != identity.CellID || installation.IdentityThumbprint == nil || *installation.IdentityThumbprint != identity.Thumbprint {
			return ErrIdentityMismatch
		}
		if installation.Status == "revoked" || installation.Status == "deleted" {
			result.Revoked = true
			result.LeaseExpiresAt = now
			return nil
		}
		if installation.CurrentInstanceID == nil || *installation.CurrentInstanceID != input.InstanceID {
			return ErrConflict
		}
		var instance instanceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&instance, "id = ? AND installation_id = ?", input.InstanceID, identity.InstallationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay node instance for heartbeat: %w", err)
		}
		result.Drain = installation.Status == "draining" || installation.Status == "disabled"
		result.RoutingReady = installation.Status == "active"
		status := "ready"
		if result.Drain {
			status = "draining"
		}
		if err := tx.Model(&instance).Updates(map[string]any{
			"status": status, "active_connections": input.ActiveConnections,
			"active_file_transfers": input.ActiveFileTransfers, "memory_bytes": input.MemoryBytes,
			"ingress_mbps": input.IngressMbps, "egress_mbps": input.EgressMbps,
			"write_loop_lag_ms": input.WriteLoopLagMS, "addresses": addresses, "capabilities": capabilities,
			"last_heartbeat_at": now, "lease_expires_at": leaseExpiresAt,
		}).Error; err != nil {
			return fmt.Errorf("update Relay node heartbeat: %w", err)
		}
		if err := tx.Model(&installSessionRow{}).Where("installation_id = ? AND status IN ?", installation.ID, []string{"waiting", "enrolled"}).Updates(map[string]any{
			"status": "heartbeat_received", "heartbeat_received_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("advance Relay install session heartbeat: %w", err)
		}
		if result.RoutingReady && !result.Drain && !result.Revoked {
			acceptedRoutes, rejectedRoutes, err = store.negotiateHeartbeatRoutes(tx, identity.CellID, input, now)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return HeartbeatResult{}, err
	}
	result.RejectedRoutes = rejectedRoutes
	if err := store.publishHeartbeatRoutes(ctx, input.InstanceID, identity.CellID, acceptedRoutes, result.LeaseExpiresAt, now); err != nil {
		return HeartbeatResult{}, fmt.Errorf("publish negotiated Relay routes: %w", err)
	}
	return result, nil
}

func finiteNonnegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (store *Store) UnregisterInstance(ctx context.Context, identity NodeCertificateIdentity, instanceID uuid.UUID) error {
	if identity.InstallationID == uuid.Nil || identity.CellID == uuid.Nil || instanceID == uuid.Nil {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", identity.InstallationID).Error; err != nil {
			return ErrNotFound
		}
		if installation.CellID != identity.CellID || installation.IdentityThumbprint == nil || *installation.IdentityThumbprint != identity.Thumbprint {
			return ErrIdentityMismatch
		}
		result := tx.Model(&instanceRow{}).Where("id = ? AND installation_id = ?", instanceID, installation.ID).Updates(map[string]any{
			"status": "stopped", "stopped_at": now, "lease_expires_at": now,
		})
		if result.Error != nil {
			return fmt.Errorf("unregister Relay node instance: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		if installation.CurrentInstanceID != nil && *installation.CurrentInstanceID == instanceID {
			if err := tx.Model(&installation).Updates(map[string]any{"current_instance_id": nil, "updated_at": now, "version": installation.Version + 1}).Error; err != nil {
				return fmt.Errorf("clear current Relay node instance: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) ExpireLeases(ctx context.Context) (int64, error) {
	now := store.now().UTC()
	var affected int64
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&instanceRow{}).
			Where("status IN ? AND lease_expires_at <= ?", []string{"starting", "ready", "draining"}, now).
			Updates(map[string]any{"status": "offline", "stopped_at": now})
		if result.Error != nil {
			return fmt.Errorf("expire Relay node leases: %w", result.Error)
		}
		affected = result.RowsAffected
		if affected == 0 {
			return nil
		}
		if err := tx.Exec(`
			UPDATE relay_node_installations installation
			SET current_instance_id = NULL, updated_at = ?, version = installation.version + 1
			FROM relay_node_instances instance
			WHERE installation.current_instance_id = instance.id AND instance.status = 'offline'`, now).Error; err != nil {
			return fmt.Errorf("clear expired Relay node instances: %w", err)
		}
		return nil
	})
	return affected, err
}

func DecodeCertificateIdentity(certificates []*x509.Certificate) (NodeCertificateIdentity, error) {
	if len(certificates) == 0 || certificates[0] == nil || len(certificates[0].URIs) != 1 {
		return NodeCertificateIdentity{}, ErrIdentityMismatch
	}
	installationRaw, cellRaw, err := relayidentity.ParseIdentityURI(certificates[0].URIs[0])
	if err != nil {
		return NodeCertificateIdentity{}, ErrIdentityMismatch
	}
	installationID, installationErr := uuid.Parse(installationRaw)
	cellID, cellErr := uuid.Parse(cellRaw)
	publicKey, ok := certificates[0].PublicKey.(ed25519.PublicKey)
	if installationErr != nil || cellErr != nil || !ok {
		return NodeCertificateIdentity{}, ErrIdentityMismatch
	}
	return NodeCertificateIdentity{InstallationID: installationID, CellID: cellID, Thumbprint: relayidentity.Thumbprint(publicKey)}, nil
}
