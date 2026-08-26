package remotedevice

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrKeyRotationProof = errors.New("remote device key rotation proof is invalid")

const keyRotationDomain = "wenzwork-device-key-rotation-v1"

type RotateKeyInput struct {
	UserID               uuid.UUID
	SessionID            uuid.UUID
	DeviceID             uuid.UUID
	IdempotencyKey       string
	ExpectedKeyVersion   uint64
	NewIdentityPublicKey string
	OldProof             string
	NewProof             string
}

type KeyRotation struct {
	Credential Credential
	Rotated    bool
}

func KeyRotationTranscript(sessionID, deviceID uuid.UUID, expectedKeyVersion uint64, oldPublicKey, newPublicKey ed25519.PublicKey) ([]byte, error) {
	if sessionID == uuid.Nil || deviceID == uuid.Nil || expectedKeyVersion == 0 ||
		len(oldPublicKey) != ed25519.PublicKeySize || len(newPublicKey) != ed25519.PublicKeySize ||
		oldPublicKey.Equal(newPublicKey) {
		return nil, ErrKeyRotationProof
	}
	return []byte(strings.Join([]string{
		keyRotationDomain,
		sessionID.String(),
		deviceID.String(),
		strconv.FormatUint(expectedKeyVersion, 10),
		base64.RawURLEncoding.EncodeToString(oldPublicKey),
		base64.RawURLEncoding.EncodeToString(newPublicKey),
	}, "\n")), nil
}

func SignKeyRotationProof(signingKey ed25519.PrivateKey, sessionID, deviceID uuid.UUID, expectedKeyVersion uint64, oldPublicKey, newPublicKey ed25519.PublicKey) (string, error) {
	if len(signingKey) != ed25519.PrivateKeySize {
		return "", ErrKeyRotationProof
	}
	transcript, err := KeyRotationTranscript(sessionID, deviceID, expectedKeyVersion, oldPublicKey, newPublicKey)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(signingKey, transcript)), nil
}

func VerifyKeyRotationProofs(oldPublicKey, newPublicKey ed25519.PublicKey, sessionID, deviceID uuid.UUID, expectedKeyVersion uint64, oldProof, newProof string) error {
	transcript, err := KeyRotationTranscript(sessionID, deviceID, expectedKeyVersion, oldPublicKey, newPublicKey)
	if err != nil {
		return err
	}
	oldSignature, oldOK := decodeSignature(oldProof)
	newSignature, newOK := decodeSignature(newProof)
	if !oldOK || !newOK || !ed25519.Verify(oldPublicKey, transcript, oldSignature) || !ed25519.Verify(newPublicKey, transcript, newSignature) {
		return ErrKeyRotationProof
	}
	return nil
}

func decodeSignature(encoded string) ([]byte, bool) {
	encoded = strings.TrimSpace(encoded)
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	return decoded, err == nil && len(decoded) == ed25519.SignatureSize && base64.RawURLEncoding.EncodeToString(decoded) == encoded
}

func (service *Service) RotateDeviceKey(ctx context.Context, input RotateKeyInput) (KeyRotation, error) {
	rotation, _, err := service.store.RotateDeviceKey(ctx, input)
	if err != nil {
		return KeyRotation{}, err
	}
	return rotation, nil
}

func (store *Store) RotateDeviceKey(ctx context.Context, input RotateKeyInput) (KeyRotation, uuid.UUID, error) {
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.NewIdentityPublicKey = strings.TrimSpace(input.NewIdentityPublicKey)
	if input.UserID == uuid.Nil || input.SessionID == uuid.Nil || input.DeviceID == uuid.Nil || input.ExpectedKeyVersion == 0 ||
		!idempotencyPattern.MatchString(input.IdempotencyKey) {
		return KeyRotation{}, uuid.Nil, ErrInvalidInput
	}
	newKeyBytes, err := base64.RawURLEncoding.Strict().DecodeString(input.NewIdentityPublicKey)
	if err != nil || len(newKeyBytes) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(newKeyBytes) != input.NewIdentityPublicKey {
		return KeyRotation{}, uuid.Nil, ErrInvalidInput
	}
	newKey := ed25519.PublicKey(newKeyBytes)
	requestHash := rotationRequestHash(input)
	now := store.now().UTC()
	var result KeyRotation
	var eventID uuid.UUID
	err = store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", input.DeviceID.String()).Error; err != nil {
			return fmt.Errorf("lock remote device rotation: %w", err)
		}
		var session struct{ ID uuid.UUID }
		if err := tx.Table("app_sessions").Select("id").Where(
			"id = ? AND user_id = ? AND device_id = ? AND revoked_at IS NULL AND idle_expires_at > ?",
			input.SessionID, input.UserID, input.DeviceID, now,
		).Take(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrForbidden
			}
			return fmt.Errorf("verify key rotation App Session: %w", err)
		}
		var prior requestKeyRow
		priorErr := tx.Where("user_id = ? AND device_id = ? AND operation = ? AND idempotency_key = ?",
			input.UserID, input.DeviceID, "key_rotation", input.IdempotencyKey).First(&prior).Error
		if priorErr == nil {
			if prior.RequestHash != requestHash {
				return ErrIdempotencyConflict
			}
			credential, err := loadCredential(tx, input.DeviceID)
			if err != nil || credential.UserID != input.UserID || credential.KeyVersion != input.ExpectedKeyVersion+1 ||
				!credential.IdentityPublicKey.Equal(newKey) {
				return ErrIdempotencyConflict
			}
			result = KeyRotation{Credential: credential, Rotated: false}
			if prior.OutboxID != nil {
				eventID = *prior.OutboxID
			}
			return nil
		}
		if !errors.Is(priorErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("load key rotation idempotency record: %w", priorErr)
		}
		var row credentialRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "device_id = ?", input.DeviceID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrForbidden
			}
			return fmt.Errorf("load remote device credential: %w", err)
		}
		if row.UserID != input.UserID {
			return ErrForbidden
		}
		if row.Status != "active" || row.KeyVersion < 1 || uint64(row.KeyVersion) != input.ExpectedKeyVersion || row.GrantVersion < 1 {
			return ErrKeyRotationRequired
		}
		oldKey := ed25519.PublicKey(row.IdentityPublicKey)
		if err := VerifyKeyRotationProofs(oldKey, newKey, input.SessionID, input.DeviceID, input.ExpectedKeyVersion, input.OldProof, input.NewProof); err != nil {
			return err
		}
		row.IdentityPublicKey = append([]byte(nil), newKey...)
		row.PublicKeyThumbprint = remoteauth.PublicKeyThumbprint(newKey)
		row.KeyVersion++
		row.GrantVersion++
		row.UpdatedAt = now
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("rotate remote device key: %w", err)
		}
		eventID = uuid.New()
		payload, _ := json.Marshal(map[string]any{
			"deviceId": row.DeviceID, "userId": row.UserID, "grantVersion": row.GrantVersion,
			"status": row.Status, "identityPublicKey": base64.RawURLEncoding.EncodeToString(row.IdentityPublicKey),
			"publicKeyThumbprint": row.PublicKeyThumbprint,
		})
		if err := tx.Table("relay_outbox").Create(map[string]any{
			"id": eventID, "aggregate_type": "remote_device", "aggregate_id": row.DeviceID,
			"event_type": "remote.device.changed", "payload": json.RawMessage(payload),
			"attempts": 0, "available_at": now, "published_at": now, "created_at": now,
		}).Error; err != nil {
			return fmt.Errorf("append key rotation Outbox event: %w", err)
		}
		if err := tx.Create(&requestKeyRow{
			ID: uuid.New(), UserID: input.UserID, DeviceID: input.DeviceID, Operation: "key_rotation",
			IdempotencyKey: input.IdempotencyKey, RequestHash: requestHash, OutboxID: &eventID,
			CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		}).Error; err != nil {
			return fmt.Errorf("save key rotation idempotency record: %w", err)
		}
		credential, err := credentialFromRow(row)
		if err != nil {
			return err
		}
		result = KeyRotation{Credential: credential, Rotated: true}
		return nil
	})
	if err != nil {
		return KeyRotation{}, uuid.Nil, err
	}
	return result, eventID, nil
}

func rotationRequestHash(input RotateKeyInput) string {
	payload, _ := json.Marshal(struct {
		ExpectedKeyVersion uint64 `json:"expectedKeyVersion"`
		NewPublicKey       string `json:"newIdentityPublicKey"`
	}{input.ExpectedKeyVersion, input.NewIdentityPublicKey})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}
