package relaymanagement

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/relayidentity"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (store *Store) CreateEnrollmentToken(ctx context.Context, installationID, actorUserID uuid.UUID) (EnrollmentToken, error) {
	if installationID == uuid.Nil || actorUserID == uuid.Nil {
		return EnrollmentToken{}, ErrInvalidInput
	}
	plaintext, err := randomToken(store.random)
	if err != nil {
		return EnrollmentToken{}, err
	}
	now := store.now().UTC()
	row := enrollmentTokenRow{
		ID: uuid.New(), InstallationID: installationID, TokenDigest: relayidentity.TokenDigest(plaintext),
		Status: "active", MaxFailedAttempts: 5, CreatedBy: uuidPointer(actorUserID),
		ExpiresAt: now.Add(store.tokenTTL), CreatedAt: now,
	}
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", installationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation for enrollment: %w", err)
		}
		if installation.Status == "active" || installation.Status == "draining" || installation.Status == "disabled" ||
			installation.Status == "revoked" || installation.Status == "deleted" || len(installation.IdentityPublicKey) != 0 {
			return ErrConflict
		}
		row.CellID, row.ReleaseID = installation.CellID, installation.ReleaseID
		row.Platform, row.Architecture = installation.Platform, installation.Architecture
		if err := tx.Model(&enrollmentTokenRow{}).
			Where("installation_id = ? AND status = ?", installationID, "active").
			Update("status", "revoked").Error; err != nil {
			return fmt.Errorf("revoke prior Relay enrollment token: %w", err)
		}
		if err := tx.Create(&row).Error; err != nil {
			return fmt.Errorf("create Relay enrollment token: %w", err)
		}
		before := installationFromRow(installation, "", nil)
		installation.Status, installation.UpdatedAt = "pending_enrollment", now
		installation.Version++
		if err := tx.Save(&installation).Error; err != nil {
			return fmt.Errorf("mark Relay installation pending enrollment: %w", err)
		}
		auditAfter := map[string]any{
			"tokenId": row.ID, "installationId": row.InstallationID, "expiresAt": row.ExpiresAt,
			"platform": row.Platform, "architecture": row.Architecture,
		}
		if err := appendAudit(tx, uuidPointer(actorUserID), "relay.enrollment_token.create", "relay_node_installation", installation.ID, before, auditAfter, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return EnrollmentToken{}, err
	}
	return EnrollmentToken{ID: row.ID, InstallationID: row.InstallationID, Token: plaintext, ExpiresAt: row.ExpiresAt}, nil
}

func (store *Store) GetBootstrapInstallation(ctx context.Context, installationID uuid.UUID) (BootstrapInstallation, error) {
	if installationID == uuid.Nil {
		return BootstrapInstallation{}, ErrNotFound
	}
	var installation installationRow
	if err := store.db.WithContext(ctx).First(&installation, "id = ? AND status = ?", installationID, "pending_enrollment").Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return BootstrapInstallation{}, ErrNotFound
		}
		return BootstrapInstallation{}, fmt.Errorf("get Relay bootstrap installation: %w", err)
	}
	result := BootstrapInstallation{
		InstallationID: installation.ID, CellID: installation.CellID, Platform: installation.Platform,
		Architecture: installation.Architecture, ProtocolMin: 2, ProtocolMax: 2,
	}
	if installation.ReleaseID != nil {
		var release releaseRow
		if err := store.db.WithContext(ctx).First(&release, "id = ? AND status = ?", *installation.ReleaseID, "published").Error; err != nil {
			return BootstrapInstallation{}, ErrNotFound
		}
		result.ReleaseVersion, result.ProtocolMin, result.ProtocolMax = release.Version, release.ProtocolMin, release.ProtocolMax
	}
	return result, nil
}

func (store *Store) Enroll(ctx context.Context, plaintextToken string, request EnrollmentRequest) (EnrollmentResult, error) {
	if store.issuer == nil {
		return EnrollmentResult{}, errors.New("Relay certificate authority is unavailable")
	}
	plaintextToken = strings.TrimSpace(plaintextToken)
	installationID, installationErr := uuid.Parse(strings.TrimSpace(request.InstallationID))
	cellID, cellErr := uuid.Parse(strings.TrimSpace(request.CellID))
	publicKey, publicKeyErr := relayidentity.DecodePublicKey(request.PublicKey)
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(request.Nonce))
	now := store.now().UTC()
	if len(plaintextToken) < 43 || len(plaintextToken) > 128 || installationErr != nil || cellErr != nil || publicKeyErr != nil ||
		nonceErr != nil || len(nonce) < 16 || len(nonce) > 64 || request.Timestamp.IsZero() ||
		request.Timestamp.Before(now.Add(-5*time.Minute)) || request.Timestamp.After(now.Add(5*time.Minute)) ||
		!validPlainText(strings.TrimSpace(request.Version), 64, false) || request.ProtocolVersion != 2 ||
		len(request.Addresses) > 16 {
		return EnrollmentResult{}, ErrEnrollmentInvalid
	}
	for _, address := range request.Addresses {
		if !validPlainText(strings.TrimSpace(address), 255, false) {
			return EnrollmentResult{}, ErrEnrollmentInvalid
		}
	}
	digest := relayidentity.TokenDigest(plaintextToken)
	proof := relayidentity.EnrollmentProof{
		InstallationID: installationID.String(), CellID: cellID.String(), PublicKey: strings.TrimSpace(request.PublicKey),
		TokenDigest: digest, Nonce: strings.TrimSpace(request.Nonce), Timestamp: request.Timestamp.UTC(),
	}
	if err := relayidentity.VerifyEnrollment(publicKey, proof, request.Signature); err != nil {
		store.recordEnrollmentFailure(ctx, digest)
		return EnrollmentResult{}, ErrEnrollmentInvalid
	}
	capabilities, err := limitedJSON(request.Capabilities, 64<<10)
	if err != nil {
		store.recordEnrollmentFailure(ctx, digest)
		return EnrollmentResult{}, ErrEnrollmentInvalid
	}
	_ = capabilities

	var result EnrollmentResult
	var domainErr error
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token enrollmentTokenRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&token, "token_digest = ?", digest).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				domainErr = ErrEnrollmentInvalid
				return nil
			}
			return fmt.Errorf("lock Relay enrollment token: %w", err)
		}
		if token.Status == "consumed" {
			domainErr = ErrEnrollmentConsumed
			return nil
		}
		if token.Status != "active" || token.FailedAttempts >= token.MaxFailedAttempts {
			domainErr = ErrEnrollmentInvalid
			return nil
		}
		if !token.ExpiresAt.After(now) {
			if err := tx.Model(&token).Update("status", "expired").Error; err != nil {
				return fmt.Errorf("expire Relay enrollment token: %w", err)
			}
			domainErr = ErrEnrollmentExpired
			return nil
		}
		if token.InstallationID != installationID || token.CellID != cellID {
			domainErr = ErrEnrollmentInvalid
			return incrementLockedTokenFailure(tx, &token)
		}
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", installationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				domainErr = ErrEnrollmentInvalid
				return nil
			}
			return fmt.Errorf("lock Relay installation during enrollment: %w", err)
		}
		if installation.CellID != cellID || installation.Platform != token.Platform || installation.Architecture != token.Architecture ||
			installation.Status != "pending_enrollment" || len(installation.IdentityPublicKey) != 0 {
			domainErr = ErrEnrollmentInvalid
			return incrementLockedTokenFailure(tx, &token)
		}
		if (installation.ReleaseID == nil) != (token.ReleaseID == nil) ||
			(installation.ReleaseID != nil && *installation.ReleaseID != *token.ReleaseID) {
			domainErr = ErrEnrollmentInvalid
			return incrementLockedTokenFailure(tx, &token)
		}
		if token.ReleaseID != nil {
			var release releaseRow
			if err := tx.First(&release, "id = ? AND status = ?", *token.ReleaseID, "published").Error; err != nil ||
				release.Version != strings.TrimSpace(request.Version) || request.ProtocolVersion < release.ProtocolMin || request.ProtocolVersion > release.ProtocolMax {
				domainErr = ErrEnrollmentInvalid
				return incrementLockedTokenFailure(tx, &token)
			}
		}
		thumbprint := relayidentity.Thumbprint(publicKey)
		issued, issueErr := store.issuer.IssueNode(publicKey, installation.ID.String(), installation.CellID.String(), thumbprint, now, store.certificateTTL)
		if issueErr != nil {
			return fmt.Errorf("issue Relay node certificate: %w", issueErr)
		}
		if err := tx.Model(&certificateRow{}).Where("installation_id = ? AND status = ?", installation.ID, "active").Update("status", "superseded").Error; err != nil {
			return fmt.Errorf("supersede Relay node certificate: %w", err)
		}
		certificate := certificateRow{
			ID: uuid.New(), InstallationID: installation.ID, CellID: installation.CellID,
			SerialNumber: issued.SerialNumber, CertificateSHA256: issued.SHA256, IdentityThumbprint: thumbprint,
			Status: "active", NotBefore: issued.NotBefore, NotAfter: issued.NotAfter, CreatedAt: now,
		}
		if err := tx.Create(&certificate).Error; err != nil {
			return fmt.Errorf("save Relay node certificate: %w", err)
		}
		before := installationFromRow(installation, "", nil)
		installation.IdentityPublicKey, installation.IdentityThumbprint = append([]byte(nil), publicKey...), &thumbprint
		installation.Status, installation.FirstEnrolledAt, installation.UpdatedAt = "pending_activation", &now, now
		installation.Version++
		if err := tx.Save(&installation).Error; err != nil {
			return fmt.Errorf("bind Relay node identity: %w", err)
		}
		if err := tx.Model(&token).Updates(map[string]any{"status": "consumed", "consumed_at": now}).Error; err != nil {
			return fmt.Errorf("consume Relay enrollment token: %w", err)
		}
		if err := tx.Model(&installSessionRow{}).Where("installation_id = ? AND status = ?", installation.ID, "waiting").Updates(map[string]any{
			"status": "enrolled", "enrolled_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("advance Relay install session: %w", err)
		}
		auditAfter := map[string]any{
			"installationId": installation.ID, "cellId": installation.CellID, "identityThumbprint": thumbprint,
			"certificateSerial": issued.SerialNumber, "certificateExpiresAt": issued.NotAfter,
		}
		if err := appendAudit(tx, nil, "relay.installation.enroll", "relay_node_installation", installation.ID, before, auditAfter, now); err != nil {
			return err
		}
		if err := appendRelayEvent(tx, "relay_node_installation", installation.ID, "relay.installation.enrolled", auditAfter, now); err != nil {
			return err
		}
		result = EnrollmentResult{
			InstallationID: installation.ID, CellID: installation.CellID, IdentityThumbprint: thumbprint,
			CertificatePEM: string(issued.CertificatePEM), CertificateAuthorityPEM: string(issued.CAPEM),
			CertificateExpiresAt: issued.NotAfter.UTC(),
		}
		return nil
	})
	if err != nil {
		return EnrollmentResult{}, err
	}
	if domainErr != nil {
		return EnrollmentResult{}, domainErr
	}
	return result, nil
}

func (store *Store) ActivateInstallation(ctx context.Context, installationID uuid.UUID, input ActivateInstallationInput) (Installation, error) {
	input.ExpectedThumbprint = strings.ToLower(strings.TrimSpace(input.ExpectedThumbprint))
	if installationID == uuid.Nil || input.ActorUserID == uuid.Nil || input.Confirmation != "activate_relay_installation" ||
		(len(input.ExpectedThumbprint) != 0 && len(input.ExpectedThumbprint) != 64) {
		return Installation{}, ErrInvalidInput
	}
	now := store.now().UTC()
	var result Installation
	err := store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", installationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation for activation: %w", err)
		}
		resuming := installation.Status == "draining" || installation.Status == "disabled"
		legacyIdentityMatches := installation.IdentityThumbprint != nil && *installation.IdentityThumbprint == input.ExpectedThumbprint
		accessKeyMatches := false
		if resuming && installation.IdentityThumbprint == nil && input.ExpectedThumbprint == "" {
			var activeKeyCount int64
			if err := tx.Model(&accessKeyRow{}).Where("installation_id = ? AND status = ?", installation.ID, "active").Count(&activeKeyCount).Error; err != nil {
				return fmt.Errorf("check Relay access key for resume: %w", err)
			}
			accessKeyMatches = activeKeyCount == 1
		}
		legacyActivation := installation.Status == "pending_activation" && legacyIdentityMatches && input.Checklist.Complete()
		accessKeyResume := resuming && accessKeyMatches
		legacyResume := resuming && legacyIdentityMatches && input.Checklist.Complete()
		if (!legacyActivation && !legacyResume && !accessKeyResume) || installation.CurrentInstanceID == nil {
			return ErrActivationBlocked
		}
		var cell cellRow
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&cell, "id = ?", installation.CellID).Error; err != nil {
			return fmt.Errorf("load Relay Cell for activation: %w", err)
		}
		if cell.Status != "active" {
			return ErrActivationBlocked
		}
		var instance instanceRow
		if err := tx.First(&instance, "id = ? AND installation_id = ?", *installation.CurrentInstanceID, installation.ID).Error; err != nil ||
			(!resuming && instance.Status != "ready") || (resuming && instance.Status != "ready" && instance.Status != "draining") ||
			!instance.LeaseExpiresAt.After(now) {
			return ErrActivationBlocked
		}
		before := installationFromRow(installation, "", nil)
		checklist, _ := limitedJSON(input.Checklist, 4096)
		installation.Status, installation.DeploymentChecklist, installation.UpdatedAt = "active", checklist, now
		if installation.ActivatedAt == nil {
			installation.ActivatedAt = &now
		}
		installation.Version++
		if err := tx.Save(&installation).Error; err != nil {
			return fmt.Errorf("activate Relay installation: %w", err)
		}
		current := instanceFromRow(instance, now)
		if resuming && instance.Status != "ready" {
			instance.Status = "ready"
			if err := tx.Model(&instance).Updates(map[string]any{"status": "ready"}).Error; err != nil {
				return fmt.Errorf("resume Relay node instance: %w", err)
			}
			current = instanceFromRow(instance, now)
		}
		result = installationFromRow(installation, cell.Code, &current)
		action := "relay.installation.activate"
		if resuming {
			action = "relay.installation.resume"
		}
		if err := appendAudit(tx, uuidPointer(input.ActorUserID), action, "relay_node_installation", installation.ID, before, result, now); err != nil {
			return err
		}
		if err := appendRelayEvent(tx, "relay_node_installation", installation.ID, "relay.installation.activated", map[string]any{"installationId": installation.ID, "cellId": installation.CellID}, now); err != nil {
			return err
		}
		if resuming {
			return appendRelayEvent(tx, "relay_node_instance", instance.ID, "relay.node.resume", map[string]any{
				"nodeId": instance.ID, "installationId": installation.ID,
			}, now)
		}
		return nil
	})
	if err != nil {
		return Installation{}, err
	}
	return result, nil
}

func (store *Store) RevokeInstallation(ctx context.Context, installationID, actorUserID uuid.UUID, confirmation string) error {
	if installationID == uuid.Nil || actorUserID == uuid.Nil || confirmation != "revoke_relay_installation" {
		return ErrInvalidInput
	}
	now := store.now().UTC()
	return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var installation installationRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&installation, "id = ?", installationID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock Relay installation for revocation: %w", err)
		}
		if installation.Status == "deleted" {
			return ErrNotFound
		}
		if installation.Status == "revoked" {
			return nil
		}
		before := installationFromRow(installation, "", nil)
		var nodeIDs []uuid.UUID
		if err := tx.Model(&instanceRow{}).Where("installation_id = ? AND status IN ?", installation.ID, []string{"starting", "ready", "draining"}).Pluck("id", &nodeIDs).Error; err != nil {
			return fmt.Errorf("list Relay nodes for revocation: %w", err)
		}
		installation.Status, installation.RevokedAt, installation.UpdatedAt = "revoked", &now, now
		installation.CurrentInstanceID = nil
		installation.Version++
		if err := tx.Save(&installation).Error; err != nil {
			return fmt.Errorf("revoke Relay installation: %w", err)
		}
		if err := tx.Model(&enrollmentTokenRow{}).Where("installation_id = ? AND status = ?", installation.ID, "active").Update("status", "revoked").Error; err != nil {
			return fmt.Errorf("revoke Relay enrollment token: %w", err)
		}
		if err := tx.Model(&accessKeyRow{}).Where("installation_id = ? AND status = ?", installation.ID, "active").Updates(map[string]any{
			"status": "revoked", "revoked_at": now,
		}).Error; err != nil {
			return fmt.Errorf("revoke Relay access key: %w", err)
		}
		if err := tx.Model(&certificateRow{}).Where("installation_id = ? AND status = ?", installation.ID, "active").Updates(map[string]any{"status": "revoked", "revoked_at": now}).Error; err != nil {
			return fmt.Errorf("revoke Relay node certificate: %w", err)
		}
		if len(nodeIDs) > 0 {
			if err := tx.Model(&instanceRow{}).Where("id IN ?", nodeIDs).Updates(map[string]any{
				"status": "forced_offline", "stopped_at": now, "lease_expires_at": now,
			}).Error; err != nil {
				return fmt.Errorf("force revoked Relay nodes offline: %w", err)
			}
			for _, nodeID := range nodeIDs {
				if err := appendRelayEvent(tx, "relay_node_instance", nodeID, "relay.node.revoke", map[string]any{
					"nodeId": nodeID, "installationId": installation.ID, "reason": "installation_revoked",
				}, now); err != nil {
					return err
				}
			}
		}
		if err := appendAudit(tx, uuidPointer(actorUserID), "relay.installation.revoke", "relay_node_installation", installation.ID, before, map[string]any{"status": "revoked", "revokedAt": now}, now); err != nil {
			return err
		}
		return appendRelayEvent(tx, "relay_node_installation", installation.ID, "relay.installation.revoked", map[string]any{"installationId": installation.ID}, now)
	})
}

func (store *Store) recordEnrollmentFailure(ctx context.Context, digest string) {
	_ = store.db.WithContext(ctx).Exec(`
		UPDATE relay_node_enrollment_tokens
		SET failed_attempts = failed_attempts + 1,
		    status = CASE WHEN failed_attempts + 1 >= max_failed_attempts THEN 'locked' ELSE status END
		WHERE token_digest = ? AND status = 'active'`, digest).Error
}

func incrementLockedTokenFailure(tx *gorm.DB, token *enrollmentTokenRow) error {
	token.FailedAttempts++
	updates := map[string]any{"failed_attempts": token.FailedAttempts}
	if token.FailedAttempts >= token.MaxFailedAttempts {
		token.Status = "locked"
		updates["status"] = "locked"
	}
	if err := tx.Model(token).Updates(updates).Error; err != nil {
		return fmt.Errorf("record Relay enrollment failure: %w", err)
	}
	return nil
}

func limitedJSON(value any, maximum int) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maximum {
		return nil, errors.New("JSON value exceeds Relay limit")
	}
	return encoded, nil
}
