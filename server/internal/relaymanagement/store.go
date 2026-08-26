package relaymanagement

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrelease"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func NewStore(db *gorm.DB, issuer CertificateIssuer, options ...Option) (*Store, error) {
	if db == nil {
		return nil, errors.New("Relay management database is required")
	}
	store := &Store{
		db: db, issuer: issuer, now: func() time.Time { return time.Now().UTC() }, random: rand.Reader,
		tokenTTL: 10 * time.Minute, certificateTTL: 24 * time.Hour, nodeLeaseDuration: 45 * time.Second,
	}
	for _, option := range options {
		option(store)
	}
	if store.now == nil || store.random == nil || store.tokenTTL <= 0 || store.tokenTTL > 30*time.Minute ||
		store.certificateTTL < time.Minute || store.certificateTTL > 7*24*time.Hour ||
		store.nodeLeaseDuration < 10*time.Second || store.nodeLeaseDuration > 5*time.Minute {
		return nil, errors.New("Relay management options are invalid")
	}
	return store, nil
}

// SetAgentRuntimeConfiguration installs the configuration returned by the
// Access Key bootstrap endpoint. It is set during API startup before the HTTP
// server begins accepting requests.
func (store *Store) SetAgentRuntimeConfiguration(config AgentRuntimeConfiguration) {
	store.agentConfigMu.Lock()
	defer store.agentConfigMu.Unlock()
	store.agentConfig = cloneAgentRuntimeConfiguration(config)
}

// SetHeartbeatRoutePublisher connects the Host-owned route registry to the
// authenticated Relay heartbeat. It is configured once during Host startup.
func (store *Store) SetHeartbeatRoutePublisher(publisher HeartbeatRoutePublisher) {
	store.agentConfigMu.Lock()
	defer store.agentConfigMu.Unlock()
	store.routePublisher = publisher
}

func (store *Store) ListTopology(ctx context.Context) ([]Region, error) {
	type topologyRow struct {
		RegionID            uuid.UUID  `gorm:"column:region_id"`
		RegionCode          string     `gorm:"column:region_code"`
		RegionName          string     `gorm:"column:region_name"`
		DataResidency       string     `gorm:"column:data_residency"`
		RegionStatus        string     `gorm:"column:region_status"`
		PoolID              uuid.UUID  `gorm:"column:pool_id"`
		PoolCode            string     `gorm:"column:pool_code"`
		PoolName            string     `gorm:"column:pool_name"`
		PoolStatus          string     `gorm:"column:pool_status"`
		CellID              uuid.UUID  `gorm:"column:cell_id"`
		CellCode            string     `gorm:"column:cell_code"`
		CellName            string     `gorm:"column:cell_name"`
		FailureDomain       string     `gorm:"column:failure_domain"`
		CellStatus          string     `gorm:"column:cell_status"`
		Weight              float64    `gorm:"column:weight"`
		ConnectionSoftLimit int64      `gorm:"column:connection_soft_limit"`
		ConnectionHardLimit int64      `gorm:"column:connection_hard_limit"`
		ProtocolMin         int        `gorm:"column:protocol_min"`
		ProtocolMax         int        `gorm:"column:protocol_max"`
		EndpointID          *uuid.UUID `gorm:"column:endpoint_id"`
		EndpointRevision    *int64     `gorm:"column:endpoint_revision"`
		EndpointType        *string    `gorm:"column:endpoint_type"`
		PublicEndpoint      *string    `gorm:"column:public_endpoint"`
		EndpointStatus      *string    `gorm:"column:endpoint_status"`
		ValidatedAt         *time.Time `gorm:"column:validated_at"`
		ActivatedAt         *time.Time `gorm:"column:endpoint_activated_at"`
		InstallationCount   int64      `gorm:"column:installation_count"`
		HealthyInstances    int64      `gorm:"column:healthy_instances"`
	}
	var rows []topologyRow
	query := `
		SELECT region.id AS region_id, region.code AS region_code, region.name AS region_name,
		       region.data_residency, region.status AS region_status,
		       pool.id AS pool_id, pool.code AS pool_code, pool.name AS pool_name, pool.status AS pool_status,
		       cell.id AS cell_id, cell.code AS cell_code, cell.name AS cell_name,
		       cell.failure_domain, cell.status AS cell_status, cell.weight,
		       cell.connection_soft_limit, cell.connection_hard_limit, cell.protocol_min, cell.protocol_max,
		       endpoint.id AS endpoint_id, endpoint.revision AS endpoint_revision,
		       endpoint.endpoint_type, endpoint.public_endpoint, endpoint.status AS endpoint_status,
		       endpoint.validated_at, endpoint.activated_at AS endpoint_activated_at,
		       (SELECT count(*) FROM relay_node_installations installation
		          WHERE installation.cell_id = cell.id AND installation.status <> 'deleted') AS installation_count,
		       (SELECT count(*) FROM relay_node_instances instance
		          WHERE instance.cell_id = cell.id AND instance.status = 'ready' AND instance.lease_expires_at > now()) AS healthy_instances
		FROM relay_regions region
		JOIN relay_pools pool ON pool.region_id = region.id
		JOIN relay_cells cell ON cell.pool_id = pool.id
		LEFT JOIN relay_cell_endpoints endpoint ON endpoint.cell_id = cell.id AND endpoint.status = 'active'
		ORDER BY region.code, pool.code, cell.code`
	if err := store.db.WithContext(ctx).Raw(query).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list Relay topology: %w", err)
	}
	regions := make([]Region, 0)
	regionIndexes := make(map[uuid.UUID]int)
	poolIndexes := make(map[uuid.UUID][2]int)
	for _, row := range rows {
		regionIndex, ok := regionIndexes[row.RegionID]
		if !ok {
			regionIndex = len(regions)
			regionIndexes[row.RegionID] = regionIndex
			regions = append(regions, Region{
				ID: row.RegionID, Code: row.RegionCode, Name: row.RegionName,
				DataResidency: row.DataResidency, Status: row.RegionStatus, Pools: []Pool{},
			})
		}
		poolLocation, ok := poolIndexes[row.PoolID]
		if !ok {
			poolIndex := len(regions[regionIndex].Pools)
			poolLocation = [2]int{regionIndex, poolIndex}
			poolIndexes[row.PoolID] = poolLocation
			regions[regionIndex].Pools = append(regions[regionIndex].Pools, Pool{
				ID: row.PoolID, Code: row.PoolCode, Name: row.PoolName, Status: row.PoolStatus, Cells: []Cell{},
			})
		}
		cell := Cell{
			ID: row.CellID, Code: row.CellCode, Name: row.CellName, FailureDomain: row.FailureDomain,
			Status: row.CellStatus, Weight: row.Weight, ConnectionSoftLimit: row.ConnectionSoftLimit,
			ConnectionHardLimit: row.ConnectionHardLimit, ProtocolMin: row.ProtocolMin,
			ProtocolMax: row.ProtocolMax, InstallationCount: row.InstallationCount, HealthyInstances: row.HealthyInstances,
		}
		if row.EndpointID != nil {
			cell.ActiveEndpoint = &Endpoint{
				ID: *row.EndpointID, Revision: *row.EndpointRevision, EndpointType: *row.EndpointType,
				PublicEndpoint: *row.PublicEndpoint, Status: *row.EndpointStatus,
				ValidatedAt: utcPointer(row.ValidatedAt), ActivatedAt: utcPointer(row.ActivatedAt),
			}
		}
		regions[poolLocation[0]].Pools[poolLocation[1]].Cells = append(regions[poolLocation[0]].Pools[poolLocation[1]].Cells, cell)
	}
	return regions, nil
}

func (store *Store) ListInstallations(ctx context.Context, cellID *uuid.UUID) ([]Installation, error) {
	query := store.db.WithContext(ctx).Model(&installationRow{}).Where("status <> ?", "deleted")
	if cellID != nil {
		if *cellID == uuid.Nil {
			return nil, ErrInvalidInput
		}
		query = query.Where("cell_id = ?", *cellID)
	}
	var rows []installationRow
	if err := query.Order("created_at DESC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list Relay installations: %w", err)
	}
	items := make([]Installation, 0, len(rows))
	for _, row := range rows {
		item, err := store.hydrateInstallation(ctx, row, false)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (store *Store) GetInstallation(ctx context.Context, installationID uuid.UUID) (Installation, error) {
	if installationID == uuid.Nil {
		return Installation{}, ErrNotFound
	}
	var row installationRow
	if err := store.db.WithContext(ctx).First(&row, "id = ? AND status <> ?", installationID, "deleted").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Installation{}, ErrNotFound
		}
		return Installation{}, fmt.Errorf("get Relay installation: %w", err)
	}
	return store.hydrateInstallation(ctx, row, true)
}

func (store *Store) CreateInstallation(ctx context.Context, input CreateInstallationInput) (Installation, error) {
	input.Platform = strings.ToLower(strings.TrimSpace(input.Platform))
	input.Architecture = strings.ToLower(strings.TrimSpace(input.Architecture))
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Region = strings.TrimSpace(input.Region)
	input.Group = strings.TrimSpace(input.Group)
	input.FailureDomain = strings.TrimSpace(input.FailureDomain)
	input.OperationsNote = strings.TrimSpace(input.OperationsNote)
	input.PublicEndpoint = strings.TrimSpace(input.PublicEndpoint)
	if input.Platform == "" {
		input.Platform = "linux"
	}
	if input.Architecture == "" {
		input.Architecture = "amd64"
	}
	if input.ActorUserID == uuid.Nil || !relayrelease.SupportsTarget(input.Platform, input.Architecture) ||
		!validPlainText(input.DisplayName, 120, false) || !validPlainText(input.Region, 80, true) ||
		!validPlainText(input.Group, 80, true) || !validPlainText(input.FailureDomain, 120, true) ||
		!validPlainText(input.OperationsNote, 2000, true) || !validOptionalPublicEndpoint(input.PublicEndpoint) ||
		!validListenerPort(input.ListenerPort) {
		return Installation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	checklist, _ := json.Marshal(DeploymentChecklist{})
	row := installationRow{
		ID: uuid.New(), CellID: input.CellID, ReleaseID: input.ReleaseID, DisplayName: input.DisplayName,
		Region: input.Region, Group: input.Group, FailureDomain: input.FailureDomain, OperationsNote: input.OperationsNote,
		PublicEndpoint: input.PublicEndpoint, ListenerPort: input.ListenerPort, Platform: input.Platform,
		Architecture: input.Architecture, Status: "draft", DeploymentChecklist: checklist, Version: 1,
		CreatedBy: uuidPointer(input.ActorUserID), CreatedAt: now, UpdatedAt: now,
	}
	var result Installation
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var cell cellRow
		cellQuery := tx
		if input.CellID == uuid.Nil {
			cellQuery = cellQuery.Where("status <> ?", "disabled").
				Order("CASE status WHEN 'active' THEN 0 WHEN 'draft' THEN 1 WHEN 'draining' THEN 2 ELSE 3 END").
				Order("code")
		} else {
			cellQuery = cellQuery.Where("id = ?", input.CellID)
		}
		if err := cellQuery.First(&cell).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInvalidInput
			}
			return fmt.Errorf("load Relay Cell: %w", err)
		}
		if cell.Status == "disabled" {
			return ErrConflict
		}
		row.CellID = cell.ID
		if input.ReleaseID != nil {
			var release releaseRow
			if err := tx.First(&release, "id = ? AND status = ?", *input.ReleaseID, "published").Error; err != nil ||
				release.Platform != input.Platform || release.Architecture != input.Architecture {
				return ErrInvalidInput
			}
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create Relay installation: %w", err)
		}
		result = installationFromRow(row, cell.Code, nil)
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay.installation.create", "relay_node_installation", row.ID, nil, result, now)
	})
	if err != nil {
		return Installation{}, err
	}
	return result, nil
}

func (store *Store) UpdateInstallation(ctx context.Context, installationID uuid.UUID, input UpdateInstallationInput) (Installation, error) {
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.Region = strings.TrimSpace(input.Region)
	input.Group = strings.TrimSpace(input.Group)
	input.FailureDomain = strings.TrimSpace(input.FailureDomain)
	input.OperationsNote = strings.TrimSpace(input.OperationsNote)
	input.PublicEndpoint = strings.TrimSpace(input.PublicEndpoint)
	if installationID == uuid.Nil || input.ActorUserID == uuid.Nil || input.ExpectedVersion < 1 ||
		!validPlainText(input.DisplayName, 120, false) || !validPlainText(input.Region, 80, true) ||
		!validPlainText(input.Group, 80, true) || !validPlainText(input.FailureDomain, 120, true) ||
		!validPlainText(input.OperationsNote, 2000, true) || !validOptionalPublicEndpoint(input.PublicEndpoint) ||
		!validListenerPort(input.ListenerPort) {
		return Installation{}, ErrInvalidInput
	}
	var result Installation
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", installationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation: %w", err)
		}
		if row.Version != input.ExpectedVersion {
			return ErrVersionConflict
		}
		if row.Status == "revoked" || row.Status == "deleted" {
			return ErrConflict
		}
		if (row.Status == "active" || row.Status == "draining" || row.Status == "disabled") && input.PublicEndpoint == "" {
			return ErrInvalidInput
		}
		before := installationFromRow(row, "", nil)
		checklist, _ := json.Marshal(input.DeploymentChecklist)
		row.DisplayName, row.Region, row.Group = input.DisplayName, input.Region, input.Group
		row.FailureDomain, row.OperationsNote, row.PublicEndpoint = input.FailureDomain, input.OperationsNote, input.PublicEndpoint
		row.ListenerPort = input.ListenerPort
		row.DeploymentChecklist, row.UpdatedAt = checklist, store.now().UTC()
		row.Version++
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("update Relay installation: %w", err)
		}
		// Access-Key Relay instances advertise the management-owned endpoint.
		// Project it immediately so peer rendezvous never returns the stale
		// address while the running process waits for its next heartbeat.
		if row.CurrentInstanceID != nil && input.PublicEndpoint != before.PublicEndpoint {
			addressValues := []string{}
			if input.PublicEndpoint != "" {
				addressValues = append(addressValues, input.PublicEndpoint)
			}
			addresses, err := limitedJSON(addressValues, 16<<10)
			if err != nil {
				return ErrInvalidInput
			}
			if err := tx.Model(&instanceRow{}).
				Where("id = ? AND installation_id = ?", *row.CurrentInstanceID, row.ID).
				Update("addresses", addresses).Error; err != nil {
				return fmt.Errorf("update Relay instance public endpoint: %w", err)
			}
		}
		var cell cellRow
		if err := tx.First(&cell, "id = ?", row.CellID).Error; err != nil {
			return fmt.Errorf("load Relay Cell: %w", err)
		}
		result = installationFromRow(row, cell.Code, nil)
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay.installation.update", "relay_node_installation", row.ID, before, result, row.UpdatedAt)
	})
	if err != nil {
		return Installation{}, err
	}
	return result, nil
}

func (store *Store) DeleteInstallation(ctx context.Context, installationID, actorUserID uuid.UUID) error {
	if installationID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrInvalidInput
	}
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", installationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation for deletion: %w", err)
		}
		unregistered := len(row.IdentityPublicKey) == 0 && (row.Status == "draft" || row.Status == "pending_enrollment" || row.Status == "expired")
		if !unregistered && row.Status != "revoked" {
			return ErrConflict
		}
		before := installationFromRow(row, "", nil)
		now := store.now().UTC()
		if err := tx.Model(&row).Updates(map[string]any{
			"status": "deleted", "current_instance_id": nil, "identity_public_key": nil,
			"identity_thumbprint": nil, "version": row.Version + 1, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("delete Relay installation: %w", err)
		}
		if err := tx.Where("installation_id = ?", row.ID).Delete(&accessKeyRow{}).Error; err != nil {
			return fmt.Errorf("delete Relay access keys: %w", err)
		}
		if err := tx.Model(&enrollmentTokenRow{}).Where("installation_id = ? AND status = ?", row.ID, "active").Update("status", "revoked").Error; err != nil {
			return fmt.Errorf("revoke enrollment tokens: %w", err)
		}
		return appendAudit(tx, uuidPointer(actorUserID), "relay.installation.delete", "relay_node_installation", row.ID, before, nil, now)
	})
}

func (store *Store) CreateInstallSession(ctx context.Context, input CreateInstallSessionInput) (InstallSession, error) {
	input.Mode = strings.ToLower(strings.TrimSpace(input.Mode))
	input.Action = strings.ToLower(strings.TrimSpace(input.Action))
	if input.Action == "" {
		input.Action = "install"
	}
	if input.InstallationID == uuid.Nil || input.ReleaseID == uuid.Nil || input.ActorUserID == uuid.Nil ||
		(input.Mode != "download" && input.Mode != "script" && input.Mode != "manual") ||
		(input.Action != "install" && input.Action != "upgrade") ||
		(input.Action == "upgrade" && input.Mode == "manual") {
		return InstallSession{}, ErrInvalidInput
	}
	now := store.now().UTC()
	row := installSessionRow{
		ID: uuid.New(), InstallationID: input.InstallationID, ReleaseID: input.ReleaseID, Mode: input.Mode,
		Status: "waiting", CreatedBy: uuidPointer(input.ActorUserID), ExpiresAt: now.Add(30 * time.Minute),
		CreatedAt: now, UpdatedAt: now,
	}
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", input.InstallationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("load Relay installation: %w", err)
		}
		if installation.Status == "deleted" || installation.Status == "revoked" ||
			(input.Action == "install" && installation.Status != "draft" && installation.Status != "pending_enrollment") ||
			(input.Action == "upgrade" && installation.Status != "active" && installation.Status != "draining" && installation.Status != "disabled") {
			return ErrConflict
		}
		var release releaseRow
		if err := tx.First(&release, "id = ? AND status = ?", input.ReleaseID, "published").Error; err != nil ||
			release.Platform != installation.Platform || release.Architecture != installation.Architecture {
			return ErrInvalidInput
		}
		if err := tx.Model(&installSessionRow{}).Where("installation_id = ? AND status = ?", installation.ID, "waiting").Updates(map[string]any{"status": "cancelled", "updated_at": now}).Error; err != nil {
			return fmt.Errorf("cancel previous Relay install sessions: %w", err)
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create Relay install session: %w", err)
		}
		if err := tx.Model(&installation).Updates(map[string]any{"release_id": release.ID, "updated_at": now, "version": installation.Version + 1}).Error; err != nil {
			return fmt.Errorf("bind Relay release: %w", err)
		}
		return appendAudit(tx, uuidPointer(input.ActorUserID), "relay."+input.Action+"_session.create", "relay_node_installation", installation.ID, nil, row, now)
	})
	if err != nil {
		return InstallSession{}, err
	}
	return InstallSession{ID: row.ID, InstallationID: row.InstallationID, ReleaseID: row.ReleaseID, Mode: row.Mode, Status: row.Status, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt}, nil
}

func (store *Store) GetBootstrapReleaseArtifact(ctx context.Context, installationID uuid.UUID) (BootstrapReleaseArtifact, error) {
	if installationID == uuid.Nil {
		return BootstrapReleaseArtifact{}, ErrNotFound
	}
	var installation installationRow
	if err := store.db.WithContext(ctx).First(&installation, "id = ? AND status IN ?", installationID, []string{"draft", "pending_enrollment", "active", "draining", "disabled"}).Error; err != nil || installation.ReleaseID == nil {
		return BootstrapReleaseArtifact{}, ErrNotFound
	}
	var release releaseRow
	if err := store.db.WithContext(ctx).First(&release, "id = ? AND status = ?", *installation.ReleaseID, "published").Error; err != nil {
		return BootstrapReleaseArtifact{}, ErrNotFound
	}
	var artifact artifactRow
	if err := store.db.WithContext(ctx).Where("release_id = ? AND LOWER(file_name) LIKE ?", release.ID, "%.tar.gz").Order("file_name").First(&artifact).Error; err != nil {
		return BootstrapReleaseArtifact{}, ErrNotFound
	}
	if strings.TrimSpace(artifact.ObjectKey) == "" || strings.ContainsRune(artifact.ObjectKey, '\x00') {
		return BootstrapReleaseArtifact{}, ErrNotFound
	}
	if !relayPackageMatchesRelease(
		artifact.FileName, release.Version, release.Platform, release.Architecture,
	) {
		return BootstrapReleaseArtifact{}, ErrNotFound
	}
	return BootstrapReleaseArtifact{
		ReleaseVersion: release.Version,
		Platform:       release.Platform,
		Architecture:   release.Architecture,
		ObjectKey:      artifact.ObjectKey,
		FileName:       artifact.FileName,
	}, nil
}

func (store *Store) ListReleases(ctx context.Context) ([]Release, error) {
	var rows []releaseRow
	if err := store.db.WithContext(ctx).Where("status = ?", "published").Order("build_time DESC, version DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list Relay releases: %w", err)
	}
	items := make([]Release, 0, len(rows))
	for _, row := range rows {
		var artifacts []artifactRow
		if err := store.db.WithContext(ctx).Where("release_id = ?", row.ID).Order("file_name").Find(&artifacts).Error; err != nil {
			return nil, fmt.Errorf("list Relay release artifacts: %w", err)
		}
		item := releaseFromRow(row)
		item.Artifacts = make([]Artifact, 0, len(artifacts))
		for _, artifact := range artifacts {
			item.Artifacts = append(item.Artifacts, Artifact{ID: artifact.ID, FileName: artifact.FileName, FileSizeBytes: artifact.FileSizeBytes, SHA256: artifact.SHA256, Signature: artifact.Signature, ObjectKey: artifact.ObjectKey})
		}
		items = append(items, item)
	}
	return items, nil
}

func (store *Store) hydrateInstallation(ctx context.Context, row installationRow, includeHistory bool) (Installation, error) {
	var cell cellRow
	if err := store.db.WithContext(ctx).First(&cell, "id = ?", row.CellID).Error; err != nil {
		return Installation{}, fmt.Errorf("load Relay Cell for installation: %w", err)
	}
	var instances []instanceRow
	query := store.db.WithContext(ctx).Where("installation_id = ?", row.ID).Order("started_at DESC")
	if !includeHistory {
		query = query.Limit(1)
	}
	if err := query.Find(&instances).Error; err != nil {
		return Installation{}, fmt.Errorf("list Relay instances: %w", err)
	}
	converted := make([]NodeInstance, 0, len(instances))
	var current *NodeInstance
	for _, instance := range instances {
		item := instanceFromRow(instance, store.now())
		converted = append(converted, item)
		if row.CurrentInstanceID != nil && item.ID == *row.CurrentInstanceID {
			copy := item
			current = &copy
		}
	}
	result := installationFromRow(row, cell.Code, current)
	result.Instances = converted
	return result, nil
}

func installationFromRow(row installationRow, cellCode string, current *NodeInstance) Installation {
	checklist := DeploymentChecklist{}
	_ = json.Unmarshal(row.DeploymentChecklist, &checklist)
	return Installation{
		ID: row.ID, CellID: row.CellID, CellCode: cellCode, ReleaseID: row.ReleaseID,
		DisplayName: row.DisplayName, Region: row.Region, Group: row.Group, FailureDomain: row.FailureDomain,
		OperationsNote: row.OperationsNote, PublicEndpoint: row.PublicEndpoint, ListenerPort: row.ListenerPort,
		Platform: row.Platform, Architecture: row.Architecture, Status: row.Status,
		IdentityThumbprint: row.IdentityThumbprint, DeploymentChecklist: checklist,
		FirstEnrolledAt: utcPointer(row.FirstEnrolledAt), ActivatedAt: utcPointer(row.ActivatedAt),
		RevokedAt: utcPointer(row.RevokedAt), Version: row.Version, CurrentInstance: current,
		Instances: []NodeInstance{}, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func instanceFromRow(row instanceRow, now time.Time) NodeInstance {
	status := row.Status
	if (status == "starting" || status == "ready" || status == "draining") && !row.LeaseExpiresAt.After(now) {
		status = "offline"
	}
	addresses := []string{}
	capabilities := map[string]any{}
	_ = json.Unmarshal(row.Addresses, &addresses)
	_ = json.Unmarshal(row.Capabilities, &capabilities)
	return NodeInstance{
		ID: row.ID, InstallationID: row.InstallationID, CellID: row.CellID, Status: status,
		Version: row.Version, ProtocolVersion: row.ProtocolVersion, Addresses: addresses, Capabilities: capabilities,
		ActiveConnections: row.ActiveConnections, ActiveFileTransfers: row.ActiveFileTransfers,
		MemoryBytes: row.MemoryBytes, IngressMbps: row.IngressMbps, EgressMbps: row.EgressMbps,
		WriteLoopLagMS: row.WriteLoopLagMS, StartedAt: row.StartedAt.UTC(),
		LastHeartbeatAt: row.LastHeartbeatAt.UTC(), LeaseExpiresAt: row.LeaseExpiresAt.UTC(), StoppedAt: utcPointer(row.StoppedAt),
	}
}

func releaseFromRow(row releaseRow) Release {
	return Release{
		ID: row.ID, Version: row.Version, Platform: row.Platform, Architecture: row.Architecture,
		ProtocolMin: row.ProtocolMin, ProtocolMax: row.ProtocolMax, BuildCommit: row.BuildCommit,
		BuildTime: row.BuildTime.UTC(), SigningKeyID: row.SigningKeyID, ManifestSHA256: row.ManifestSHA256,
		ManifestSignature: row.ManifestSignature, Status: row.Status, Artifacts: []Artifact{},
	}
}

func appendAudit(tx *gorm.DB, actor *uuid.UUID, action, resourceType string, resourceID uuid.UUID, before, after any, now time.Time) error {
	beforeJSON, err := marshalOptional(before)
	if err != nil {
		return fmt.Errorf("marshal Relay audit before state: %w", err)
	}
	afterJSON, err := marshalOptional(after)
	if err != nil {
		return fmt.Errorf("marshal Relay audit after state: %w", err)
	}
	resource := resourceID
	if err := tx.Create(&auditRow{
		ID: uuid.New(), ActorUserID: actor, Action: action, ResourceType: resourceType,
		ResourceID: &resource, BeforeJSON: beforeJSON, AfterJSON: afterJSON, CreatedAt: now,
	}).Error; err != nil {
		return fmt.Errorf("append Relay audit: %w", err)
	}
	return nil
}

func appendRelayEvent(tx *gorm.DB, aggregateType string, aggregateID uuid.UUID, eventType string, payload any, now time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal Relay lifecycle event: %w", err)
	}
	publishedAt := now.UTC()
	if err := tx.Create(&outboxRow{
		ID: uuid.New(), AggregateType: aggregateType, AggregateID: aggregateID,
		EventType: eventType, Payload: encoded, AvailableAt: now, PublishedAt: &publishedAt, CreatedAt: now,
	}).Error; err != nil {
		return fmt.Errorf("append Relay lifecycle event: %w", err)
	}
	return nil
}

func marshalOptional(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	return json.Marshal(value)
}

func randomToken(random io.Reader) (string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("generate Relay enrollment token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validPlainText(value string, maximum int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum || (!allowEmpty && value == "") {
		return false
	}
	for _, character := range value {
		if character == 0 || (character < 0x20 && character != '\n' && character != '\t') {
			return false
		}
	}
	return true
}

func cloneAgentRuntimeConfiguration(config AgentRuntimeConfiguration) AgentRuntimeConfiguration {
	config.BrowserOriginPatterns = append([]string(nil), config.BrowserOriginPatterns...)
	config.TicketPublicKeys = cloneStringMap(config.TicketPublicKeys)
	config.DeviceLinkGrantPublicKeys = cloneStringMap(config.DeviceLinkGrantPublicKeys)
	return config
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func uuidPointer(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	copy := value
	return &copy
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
