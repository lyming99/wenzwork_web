package relaymanagement

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const accessKeyPrefix = "relay_"

func (store *Store) CreateAccessKey(ctx context.Context, installationID, actorUserID uuid.UUID) (AccessKey, error) {
	if installationID == uuid.Nil || actorUserID == uuid.Nil {
		return AccessKey{}, ErrInvalidInput
	}
	secretBytes := make([]byte, 32)
	if _, err := io.ReadFull(store.random, secretBytes); err != nil {
		return AccessKey{}, fmt.Errorf("generate Relay access key: %w", err)
	}
	plaintext := accessKeyPrefix + base64.RawURLEncoding.EncodeToString(secretBytes)
	digest, ok := accessKeyDigest(plaintext)
	if !ok {
		return AccessKey{}, errors.New("generated Relay access key is invalid")
	}
	now := store.now().UTC()
	row := accessKeyRow{
		ID: uuid.New(), InstallationID: installationID, KeyPrefix: plaintext[:16], KeyDigest: digest,
		Status: "active", CreatedBy: uuidPointer(actorUserID), CreatedAt: now,
	}
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", installationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation for access key creation: %w", err)
		}
		if installation.Status == "revoked" || installation.Status == "deleted" {
			return ErrConflict
		}
		if err := tx.Model(&accessKeyRow{}).
			Where("installation_id = ? AND status = ?", installation.ID, "active").
			Updates(map[string]any{"status": "revoked", "revoked_at": now}).Error; err != nil {
			return fmt.Errorf("revoke prior Relay access key: %w", err)
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create Relay access key: %w", err)
		}
		before := installationFromRow(installation, "", nil)
		updates := map[string]any{"updated_at": now, "version": installation.Version + 1}
		if installation.Status == "draft" || installation.Status == "expired" {
			updates["status"] = "pending_enrollment"
		}
		if err := tx.Model(&installation).Updates(updates).Error; err != nil {
			return fmt.Errorf("prepare Relay installation for access key connection: %w", err)
		}
		auditAfter := map[string]any{
			"accessKeyId": row.ID, "installationId": row.InstallationID, "keyPrefix": row.KeyPrefix,
		}
		return appendAudit(tx, uuidPointer(actorUserID), "relay.access_key.create", "relay_node_installation", installation.ID, before, auditAfter, now)
	})
	if err != nil {
		return AccessKey{}, err
	}
	return AccessKey{
		ID: row.ID, InstallationID: row.InstallationID, Key: plaintext,
		KeyPrefix: row.KeyPrefix, CreatedAt: row.CreatedAt,
	}, nil
}

func (store *Store) ResolveAccessKey(ctx context.Context, plaintext string) (AccessKeyBinding, error) {
	var result AccessKeyBinding
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, installation, err := store.authorizeAccessKey(tx, plaintext, true)
		if err != nil {
			return err
		}
		var cell cellRow
		if err := tx.First(&cell, "id = ?", installation.CellID).Error; err != nil {
			return fmt.Errorf("load Relay Cell for access key configuration: %w", err)
		}
		configuration, configErr := store.agentRuntimeConfiguration(installation, cell)
		if configErr != nil {
			return configErr
		}
		result = AccessKeyBinding{
			InstallationID: installation.ID, CellID: installation.CellID, Status: installation.Status,
			ConfigurationVersion: installation.Version, Configuration: configuration,
		}
		return nil
	})
	return result, err
}

func (store *Store) RegisterInstanceWithAccessKey(ctx context.Context, plaintext string, input RegisterInstanceInput) (NodeInstance, error) {
	input.Version = strings.TrimSpace(input.Version)
	if input.InstanceID == uuid.Nil || !validPlainText(input.Version, 64, false) || input.ProtocolVersion != 2 || len(input.Addresses) > 16 {
		return NodeInstance{}, ErrInvalidInput
	}
	for _, address := range input.Addresses {
		if !validPlainText(strings.TrimSpace(address), 255, false) {
			return NodeInstance{}, ErrInvalidInput
		}
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
	var result NodeInstance
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, installation, err := store.authorizeAccessKey(tx, plaintext, true)
		if err != nil {
			return err
		}
		var cell cellRow
		if err := tx.First(&cell, "id = ?", installation.CellID).Error; err != nil {
			return fmt.Errorf("load Relay Cell for access-key registration: %w", err)
		}
		configuration, configErr := store.agentRuntimeConfiguration(installation, cell)
		if configErr != nil {
			return configErr
		}
		addresses, err := limitedJSON(configurationAddresses(configuration), 16<<10)
		if err != nil {
			return ErrInvalidInput
		}
		if installation.ReleaseID != nil {
			var release releaseRow
			if err := tx.First(&release, "id = ?", *installation.ReleaseID).Error; err != nil ||
				release.Version != input.Version || input.ProtocolVersion < release.ProtocolMin || input.ProtocolVersion > release.ProtocolMax {
				return ErrConflict
			}
		}
		row := instanceRow{
			ID: input.InstanceID, InstallationID: installation.ID, CellID: installation.CellID,
			Status: "starting", Version: input.Version, ProtocolVersion: input.ProtocolVersion,
			Addresses: addresses, Capabilities: capabilities, StartedAt: startedAt,
			LastHeartbeatAt: now, LeaseExpiresAt: now.Add(store.nodeLeaseDuration), CreatedAt: now,
		}
		var existing instanceRow
		existingErr := tx.First(&existing, "id = ?", input.InstanceID).Error
		if existingErr == nil {
			if existing.InstallationID != installation.ID || existing.CellID != installation.CellID {
				return ErrConflict
			}
			row = existing
		} else if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load Relay access-key instance: %w", existingErr)
		} else if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("register Relay access-key instance: %w", err)
		}
		if installation.CurrentInstanceID != nil && *installation.CurrentInstanceID != row.ID {
			if err := tx.Model(&instanceRow{}).
				Where("id = ? AND installation_id = ? AND status IN ?", *installation.CurrentInstanceID, installation.ID, []string{"starting", "ready", "draining"}).
				Updates(map[string]any{"status": "offline", "stopped_at": now, "lease_expires_at": now}).Error; err != nil {
				return fmt.Errorf("retire previous Relay instance: %w", err)
			}
		}
		installationStatus := "active"
		if installation.Status == "draining" || installation.Status == "disabled" {
			installationStatus = installation.Status
		}
		updates := map[string]any{
			"current_instance_id": row.ID, "status": installationStatus, "updated_at": now,
			"version": installation.Version + 1,
		}
		if installation.FirstEnrolledAt == nil {
			updates["first_enrolled_at"] = now
		}
		if installation.ActivatedAt == nil {
			updates["activated_at"] = now
		}
		if err := tx.Model(&installation).Updates(updates).Error; err != nil {
			return fmt.Errorf("activate Relay access-key installation: %w", err)
		}
		if err := tx.Model(&installSessionRow{}).
			Where("installation_id = ? AND status IN ?", installation.ID, []string{"waiting", "enrolled"}).
			Updates(map[string]any{"status": "heartbeat_received", "heartbeat_received_at": now, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("complete Relay access-key install session: %w", err)
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

func (store *Store) HeartbeatWithAccessKey(ctx context.Context, plaintext string, input HeartbeatInput) (HeartbeatResult, error) {
	if input.InstanceID == uuid.Nil || input.ActiveConnections < 0 || input.ActiveFileTransfers < 0 || input.MemoryBytes < 0 ||
		input.ConfigurationVersion < 0 ||
		!finiteNonnegative(input.IngressMbps) || !finiteNonnegative(input.EgressMbps) || !finiteNonnegative(input.WriteLoopLagMS) ||
		len(input.Addresses) > 16 || validateHeartbeatRouteShapes(input) != nil {
		return HeartbeatResult{}, ErrInvalidInput
	}
	capabilities, err := limitedJSON(input.Capabilities, 64<<10)
	if err != nil {
		return HeartbeatResult{}, ErrInvalidInput
	}
	now := store.now().UTC()
	result := HeartbeatResult{LeaseExpiresAt: now.Add(store.nodeLeaseDuration)}
	var routeCellID uuid.UUID
	var acceptedRoutes []HeartbeatRoute
	var rejectedRoutes []HeartbeatRouteRejection
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, installation, err := store.authorizeAccessKey(tx, plaintext, true)
		if err != nil {
			return err
		}
		var cell cellRow
		if err := tx.First(&cell, "id = ?", installation.CellID).Error; err != nil {
			return fmt.Errorf("load Relay Cell for access-key heartbeat: %w", err)
		}
		result.ConfigurationVersion = installation.Version
		configuration, configErr := store.agentRuntimeConfiguration(installation, cell)
		if configErr != nil {
			return configErr
		}
		result.Configuration = configuration
		// The runtime receives the complete installation-scoped configuration on
		// every heartbeat. Relay restarts its public listeners itself when the
		// address, listener settings, or TLS material changes.
		result.RestartRequired = false
		addresses, err := limitedJSON(configurationAddresses(result.Configuration), 16<<10)
		if err != nil {
			return ErrInvalidInput
		}
		if installation.CurrentInstanceID == nil || *installation.CurrentInstanceID != input.InstanceID {
			return ErrConflict
		}
		var instance instanceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&instance, "id = ? AND installation_id = ?", input.InstanceID, installation.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay access-key instance for heartbeat: %w", err)
		}
		result.Drain = installation.Status == "draining" || installation.Status == "disabled"
		result.RoutingReady = installation.Status == "active"
		routeCellID = installation.CellID
		status := "ready"
		if result.Drain {
			status = "draining"
		}
		if err := tx.Model(&instance).Updates(map[string]any{
			"status": status, "active_connections": input.ActiveConnections,
			"active_file_transfers": input.ActiveFileTransfers, "memory_bytes": input.MemoryBytes,
			"ingress_mbps": input.IngressMbps, "egress_mbps": input.EgressMbps,
			"write_loop_lag_ms": input.WriteLoopLagMS, "addresses": addresses, "capabilities": capabilities,
			"last_heartbeat_at": now, "lease_expires_at": result.LeaseExpiresAt,
		}).Error; err != nil {
			return fmt.Errorf("update Relay access-key heartbeat: %w", err)
		}
		if result.RoutingReady && !result.Drain {
			acceptedRoutes, rejectedRoutes, err = store.negotiateHeartbeatRoutes(tx, installation.CellID, input, now)
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
	if err := store.publishHeartbeatRoutes(ctx, input.InstanceID, routeCellID, acceptedRoutes, result.LeaseExpiresAt, now); err != nil {
		return HeartbeatResult{}, fmt.Errorf("publish negotiated Relay routes: %w", err)
	}
	return result, nil
}

func (store *Store) agentRuntimeConfiguration(installation installationRow, cell cellRow) (AgentRuntimeConfiguration, error) {
	store.agentConfigMu.RLock()
	configuration := cloneAgentRuntimeConfiguration(store.agentConfig)
	store.agentConfigMu.RUnlock()
	configuration.PublicEndpoint = installation.PublicEndpoint
	configuration.ListenAddress = listenerAddress(installation.ListenerPort)
	configuration.ConnectionHardLimit = int(cell.ConnectionHardLimit)
	return configuration, nil
}

func configurationAddresses(configuration AgentRuntimeConfiguration) []string {
	if endpoint := strings.TrimSpace(configuration.PublicEndpoint); endpoint != "" {
		return []string{endpoint}
	}
	return []string{}
}

func (store *Store) UnregisterInstanceWithAccessKey(ctx context.Context, plaintext string, instanceID uuid.UUID) error {
	if instanceID == uuid.Nil {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, installation, err := store.authorizeAccessKey(tx, plaintext, true)
		if err != nil {
			return err
		}
		result := tx.Model(&instanceRow{}).Where("id = ? AND installation_id = ?", instanceID, installation.ID).Updates(map[string]any{
			"status": "stopped", "stopped_at": now, "lease_expires_at": now,
		})
		if result.Error != nil {
			return fmt.Errorf("unregister Relay access-key instance: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrNotFound
		}
		if installation.CurrentInstanceID != nil && *installation.CurrentInstanceID == instanceID {
			if err := tx.Model(&installation).Updates(map[string]any{
				"current_instance_id": nil, "updated_at": now, "version": installation.Version + 1,
			}).Error; err != nil {
				return fmt.Errorf("clear Relay access-key instance: %w", err)
			}
		}
		return nil
	})
}

func (store *Store) authorizeAccessKey(tx *gorm.DB, plaintext string, touch bool) (accessKeyRow, installationRow, error) {
	digest, ok := accessKeyDigest(plaintext)
	if !ok {
		return accessKeyRow{}, installationRow{}, ErrAccessKeyInvalid
	}
	// Discover the installation without taking the key lock, then lock in the
	// same installation -> credential order used by rotate, revoke and delete.
	// Reloading the key after the installation lock closes the rotation race.
	var lookup accessKeyRow
	if err := tx.First(&lookup, "key_digest = ?", digest).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return accessKeyRow{}, installationRow{}, ErrAccessKeyInvalid
		}
		return accessKeyRow{}, installationRow{}, fmt.Errorf("load Relay access key: %w", err)
	}
	var installation installationRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", lookup.InstallationID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return accessKeyRow{}, installationRow{}, ErrAccessKeyInvalid
		}
		return accessKeyRow{}, installationRow{}, fmt.Errorf("load Relay access-key installation: %w", err)
	}
	if installation.Status == "revoked" || installation.Status == "deleted" {
		return accessKeyRow{}, installationRow{}, ErrInstallationRevoked
	}
	var key accessKeyRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&key, "id = ? AND key_digest = ?", lookup.ID, digest).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return accessKeyRow{}, installationRow{}, ErrAccessKeyInvalid
		}
		return accessKeyRow{}, installationRow{}, fmt.Errorf("lock Relay access key: %w", err)
	}
	if key.Status != "active" {
		return accessKeyRow{}, installationRow{}, ErrAccessKeyInvalid
	}
	if touch {
		now := store.now().UTC()
		if err := tx.Model(&key).Update("last_used_at", now).Error; err != nil {
			return accessKeyRow{}, installationRow{}, fmt.Errorf("record Relay access key use: %w", err)
		}
		key.LastUsedAt = &now
	}
	return key, installation, nil
}

func accessKeyDigest(plaintext string) (string, bool) {
	plaintext = strings.TrimSpace(plaintext)
	if len(plaintext) != len(accessKeyPrefix)+43 || !strings.HasPrefix(plaintext, accessKeyPrefix) {
		return "", false
	}
	encoded := plaintext[len(accessKeyPrefix):]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != 32 {
		return "", false
	}
	digest := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(digest[:]), true
}
