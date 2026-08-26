package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // TOTP authenticators standardize on HMAC-SHA-1 for compatibility.
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	totpPeriodSeconds = int64(30)
	totpSecretBytes   = 20
	recoveryCodeCount = 8
	recoveryAlphabet  = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
)

var (
	ErrMFAAlreadyEnrolled = errors.New("MFA is already enrolled")
	ErrMFANotEnrolled     = errors.New("MFA is not enrolled")
	ErrMFAInvalidCode     = errors.New("MFA code is invalid")
	ErrMFAReplay          = errors.New("MFA code was already used")
	ErrMFAAssurance       = errors.New("MFA assurance is required")
)

type MFAEnrollment struct {
	Secret     string `json:"secret"`
	OTPAuthURI string `json:"otpauthUri"`
}

type MFAStatus struct {
	Enrolled               bool  `json:"enrolled"`
	RecoveryCodesRemaining int64 `json:"recoveryCodesRemaining"`
}

type MFAConfirmation struct {
	Session       Session  `json:"-"`
	RecoveryCodes []string `json:"recoveryCodes"`
}

func (s *Service) GetMFAStatus(ctx context.Context, userID uuid.UUID) (MFAStatus, error) {
	var credentialCount int64
	if err := s.db.WithContext(ctx).Model(&totpCredentialRow{}).
		Where("user_id = ? AND verified_at IS NOT NULL", userID).Count(&credentialCount).Error; err != nil {
		return MFAStatus{}, fmt.Errorf("load MFA credential status: %w", err)
	}
	var recoveryCount int64
	if credentialCount > 0 {
		if err := s.db.WithContext(ctx).Model(&recoveryCodeRow{}).
			Where("user_id = ? AND used_at IS NULL", userID).Count(&recoveryCount).Error; err != nil {
			return MFAStatus{}, fmt.Errorf("load MFA recovery status: %w", err)
		}
	}
	return MFAStatus{Enrolled: credentialCount > 0, RecoveryCodesRemaining: recoveryCount}, nil
}

func (s *Service) BeginTOTPEnrollment(ctx context.Context, userID uuid.UUID, currentPassword string) (MFAEnrollment, error) {
	secretBytes := make([]byte, totpSecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return MFAEnrollment{}, fmt.Errorf("generate TOTP secret: %w", err)
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secretBytes)
	ciphertext, err := encryptMFASecret(s.mfaKey[:], userID, secretBytes)
	if err != nil {
		return MFAEnrollment{}, err
	}
	now := s.now().UTC()
	var email string
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user userRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ? AND status = 'active'", userID).Error; err != nil {
			return ErrAccountUnavailable
		}
		valid, err := VerifyPassword(user.PasswordHash, currentPassword)
		if err != nil {
			return fmt.Errorf("verify MFA enrollment password: %w", err)
		}
		if !valid {
			return ErrCurrentPassword
		}
		var existing totpCredentialRow
		loadErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing, "user_id = ?", userID).Error
		if loadErr == nil && existing.VerifiedAt != nil {
			return ErrMFAAlreadyEnrolled
		}
		if loadErr != nil && !errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("lock MFA credential: %w", loadErr)
		}
		credential := totpCredentialRow{
			UserID: userID, SecretCiphertext: ciphertext, LastUsedStep: -1,
			CreatedAt: now, UpdatedAt: now,
		}
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if err := tx.Create(&credential).Error; err != nil {
				return fmt.Errorf("create MFA credential: %w", err)
			}
		} else if err := tx.Model(&totpCredentialRow{}).Where("user_id = ? AND verified_at IS NULL", userID).
			Updates(map[string]any{
				"secret_ciphertext": ciphertext, "last_used_step": -1, "updated_at": now,
			}).Error; err != nil {
			return fmt.Errorf("replace unverified MFA credential: %w", err)
		}
		email = user.Email
		return nil
	})
	if err != nil {
		return MFAEnrollment{}, err
	}
	uri := url.URL{Scheme: "otpauth", Host: "totp", Path: "/WenzWork:" + email}
	query := uri.Query()
	query.Set("secret", secret)
	query.Set("issuer", "WenzWork")
	query.Set("algorithm", "SHA1")
	query.Set("digits", "6")
	query.Set("period", strconv.FormatInt(totpPeriodSeconds, 10))
	uri.RawQuery = query.Encode()
	return MFAEnrollment{Secret: secret, OTPAuthURI: uri.String()}, nil
}

func (s *Service) ConfirmTOTPEnrollment(ctx context.Context, session AuthenticatedSession, code string) (MFAConfirmation, error) {
	codes, hashes, err := s.newRecoveryCodes()
	if err != nil {
		return MFAConfirmation{}, err
	}
	now := s.now().UTC()
	var rotated Session
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var credential totpCredentialRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&credential, "user_id = ?", session.User.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrMFANotEnrolled
			}
			return fmt.Errorf("lock MFA enrollment: %w", err)
		}
		if credential.VerifiedAt != nil {
			return ErrMFAAlreadyEnrolled
		}
		secret, err := decryptMFASecret(s.mfaKey[:], session.User.ID, credential.SecretCiphertext)
		if err != nil {
			return err
		}
		step, matched := findTOTPStep(secret, code, now)
		if !matched {
			return ErrMFAInvalidCode
		}
		if step <= credential.LastUsedStep {
			return ErrMFAReplay
		}
		if err := tx.Model(&totpCredentialRow{}).Where("user_id = ?", session.User.ID).
			Updates(map[string]any{"verified_at": now, "last_used_step": step, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("confirm MFA enrollment: %w", err)
		}
		if err := replaceRecoveryCodes(tx, session.User.ID, codes, hashes, now); err != nil {
			return err
		}
		rotated, err = s.rotateSessionWithMFA(tx, session, now)
		return err
	})
	if err != nil {
		return MFAConfirmation{}, err
	}
	return MFAConfirmation{Session: rotated, RecoveryCodes: codes}, nil
}

func (s *Service) VerifyMFA(ctx context.Context, session AuthenticatedSession, code string) (Session, error) {
	now := s.now().UTC()
	var rotated Session
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := s.consumeMFACode(tx, session.User.ID, code, now); err != nil {
			return err
		}
		var err error
		rotated, err = s.rotateSessionWithMFA(tx, session, now)
		return err
	})
	return rotated, err
}

func (s *Service) RegenerateRecoveryCodes(ctx context.Context, session AuthenticatedSession, currentPassword string) ([]string, error) {
	if session.AssuranceLevel < 2 {
		return nil, ErrMFAAssurance
	}
	codes, hashes, err := s.newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := verifyCurrentPasswordTx(tx, session.User.ID, currentPassword); err != nil {
			return err
		}
		var credentialCount int64
		if err := tx.Model(&totpCredentialRow{}).Where("user_id = ? AND verified_at IS NOT NULL", session.User.ID).
			Count(&credentialCount).Error; err != nil {
			return fmt.Errorf("load MFA credential: %w", err)
		}
		if credentialCount == 0 {
			return ErrMFANotEnrolled
		}
		return replaceRecoveryCodes(tx, session.User.ID, codes, hashes, now)
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

func (s *Service) DisableTOTP(ctx context.Context, session AuthenticatedSession, currentPassword, code string) error {
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := verifyCurrentPasswordTx(tx, session.User.ID, currentPassword); err != nil {
			return err
		}
		if err := s.consumeMFACode(tx, session.User.ID, code, now); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", session.User.ID).Delete(&recoveryCodeRow{}).Error; err != nil {
			return fmt.Errorf("delete MFA recovery codes: %w", err)
		}
		if err := tx.Where("user_id = ?", session.User.ID).Delete(&totpCredentialRow{}).Error; err != nil {
			return fmt.Errorf("delete MFA credential: %w", err)
		}
		if err := tx.Model(&sessionRow{}).Where("user_id = ? AND revoked_at IS NULL", session.User.ID).
			Updates(map[string]any{"assurance_level": 1, "mfa_verified_at": nil, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("downgrade MFA sessions: %w", err)
		}
		return nil
	})
}

func (s *Service) consumeMFACode(tx *gorm.DB, userID uuid.UUID, rawCode string, now time.Time) error {
	var credential totpCredentialRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&credential, "user_id = ? AND verified_at IS NOT NULL", userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrMFANotEnrolled
		}
		return fmt.Errorf("lock MFA credential: %w", err)
	}
	code := strings.ToUpper(strings.TrimSpace(rawCode))
	if len(code) == 6 && allASCIIDigits(code) {
		secret, err := decryptMFASecret(s.mfaKey[:], userID, credential.SecretCiphertext)
		if err != nil {
			return err
		}
		step, matched := findTOTPStep(secret, code, now)
		if !matched {
			return ErrMFAInvalidCode
		}
		if step <= credential.LastUsedStep {
			return ErrMFAReplay
		}
		if err := tx.Model(&totpCredentialRow{}).Where("user_id = ?", userID).
			Updates(map[string]any{"last_used_step": step, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("consume TOTP code: %w", err)
		}
		return nil
	}

	var recoveryRows []recoveryCodeRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("user_id = ? AND used_at IS NULL", userID).Order("created_at ASC").Find(&recoveryRows).Error; err != nil {
		return fmt.Errorf("lock MFA recovery codes: %w", err)
	}
	for _, row := range recoveryRows {
		valid, err := VerifyPassword(row.CodeHash, code)
		if err != nil {
			return fmt.Errorf("verify MFA recovery code: %w", err)
		}
		if valid {
			result := tx.Model(&recoveryCodeRow{}).Where("id = ? AND used_at IS NULL", row.ID).Update("used_at", now)
			if result.Error != nil {
				return fmt.Errorf("consume MFA recovery code: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return ErrMFAReplay
			}
			return nil
		}
	}
	return ErrMFAInvalidCode
}

func (s *Service) rotateSessionWithMFA(tx *gorm.DB, current AuthenticatedSession, now time.Time) (Session, error) {
	var source sessionRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&source,
		"id = ? AND user_id = ? AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?",
		current.ID, current.User.ID, now, now).Error; err != nil {
		return Session{}, ErrSessionUnavailable
	}
	token, tokenHash, err := NewOpaqueToken()
	if err != nil {
		return Session{}, err
	}
	csrfToken, csrfHash, err := NewOpaqueToken()
	if err != nil {
		return Session{}, err
	}
	newID := uuid.New()
	idleExpiry := minTime(now.Add(s.config.SessionIdleTTL), source.AbsoluteExpiresAt)
	verifiedAt := now
	row := sessionRow{
		ID: newID, UserID: source.UserID, TokenHash: tokenHash, CSRFTokenHash: csrfHash,
		UserAgentSummary: source.UserAgentSummary, RememberMe: source.RememberMe, AssuranceLevel: 2,
		LastSeenAt: now, IdleExpiresAt: idleExpiry, AbsoluteExpiresAt: source.AbsoluteExpiresAt,
		MFAVerifiedAt: &verifiedAt, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Model(&sessionRow{}).Where("id = ? AND revoked_at IS NULL", source.ID).
		Updates(map[string]any{"revoked_at": now, "revoked_reason": "mfa_rotation", "updated_at": now}).Error; err != nil {
		return Session{}, fmt.Errorf("revoke pre-MFA session: %w", err)
	}
	if err := tx.Create(&row).Error; err != nil {
		return Session{}, fmt.Errorf("create MFA session: %w", err)
	}
	return Session{
		ID: newID, User: current.User, Token: token, CSRFToken: csrfToken,
		RememberMe: source.RememberMe, AssuranceLevel: 2, LastSeenAt: now,
		IdleExpiresAt: idleExpiry, AbsoluteExpiresAt: source.AbsoluteExpiresAt,
	}, nil
}

func (s *Service) newRecoveryCodes() ([]string, []string, error) {
	codes := make([]string, 0, recoveryCodeCount)
	hashes := make([]string, 0, recoveryCodeCount)
	for range recoveryCodeCount {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return nil, nil, fmt.Errorf("generate MFA recovery code: %w", err)
		}
		var builder strings.Builder
		for index, value := range random {
			if index > 0 && index%4 == 0 {
				builder.WriteByte('-')
			}
			builder.WriteByte(recoveryAlphabet[int(value)&31])
		}
		code := builder.String()
		hash, err := HashPassword(code, s.config.PasswordParams)
		if err != nil {
			return nil, nil, fmt.Errorf("hash MFA recovery code: %w", err)
		}
		codes = append(codes, code)
		hashes = append(hashes, hash)
	}
	return codes, hashes, nil
}

func replaceRecoveryCodes(tx *gorm.DB, userID uuid.UUID, codes, hashes []string, now time.Time) error {
	if len(codes) != len(hashes) || len(codes) == 0 {
		return errors.New("invalid MFA recovery code set")
	}
	if err := tx.Where("user_id = ?", userID).Delete(&recoveryCodeRow{}).Error; err != nil {
		return fmt.Errorf("invalidate MFA recovery codes: %w", err)
	}
	rows := make([]recoveryCodeRow, 0, len(hashes))
	for _, hash := range hashes {
		rows = append(rows, recoveryCodeRow{ID: uuid.New(), UserID: userID, CodeHash: hash, CreatedAt: now})
	}
	if err := tx.Create(&rows).Error; err != nil {
		return fmt.Errorf("create MFA recovery codes: %w", err)
	}
	return nil
}

func verifyCurrentPasswordTx(tx *gorm.DB, userID uuid.UUID, password string) error {
	var user userRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ? AND status = 'active'", userID).Error; err != nil {
		return ErrAccountUnavailable
	}
	valid, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return fmt.Errorf("verify current password for MFA: %w", err)
	}
	if !valid {
		return ErrCurrentPassword
	}
	return nil
}

func encryptMFASecret(key []byte, userID uuid.UUID, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create MFA cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create MFA AEAD: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate MFA encryption nonce: %w", err)
	}
	result := make([]byte, 1, 1+len(nonce)+len(plaintext)+gcm.Overhead())
	result[0] = 1
	result = append(result, nonce...)
	result = gcm.Seal(result, nonce, plaintext, userID[:])
	return result, nil
}

func decryptMFASecret(key []byte, userID uuid.UUID, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create MFA cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create MFA AEAD: %w", err)
	}
	if len(ciphertext) < 1+gcm.NonceSize()+gcm.Overhead() || ciphertext[0] != 1 {
		return nil, errors.New("MFA secret ciphertext is invalid")
	}
	nonce := ciphertext[1 : 1+gcm.NonceSize()]
	plaintext, err := gcm.Open(nil, nonce, ciphertext[1+gcm.NonceSize():], userID[:])
	if err != nil {
		return nil, errors.New("MFA secret authentication failed")
	}
	return plaintext, nil
}

func findTOTPStep(secret []byte, rawCode string, now time.Time) (int64, bool) {
	code := strings.TrimSpace(rawCode)
	if len(code) != 6 || !allASCIIDigits(code) {
		return 0, false
	}
	current := now.Unix() / totpPeriodSeconds
	for _, offset := range []int64{0, -1, 1} {
		step := current + offset
		if step < 0 {
			continue
		}
		candidate := hotp(secret, uint64(step), 6)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

func hotp(secret []byte, counter uint64, digits int) string {
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	digest := hmac.New(sha1.New, secret)
	_, _ = digest.Write(message[:])
	sum := digest.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	binaryCode := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	modulus := uint32(1)
	for range digits {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", digits, binaryCode%modulus)
}

func allASCIIDigits(value string) bool {
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

type totpCredentialRow struct {
	UserID           uuid.UUID  `gorm:"column:user_id;primaryKey"`
	SecretCiphertext []byte     `gorm:"column:secret_ciphertext"`
	VerifiedAt       *time.Time `gorm:"column:verified_at"`
	LastUsedStep     int64      `gorm:"column:last_used_step"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

func (totpCredentialRow) TableName() string { return "mfa_totp_credentials" }

type recoveryCodeRow struct {
	ID        uuid.UUID  `gorm:"column:id;primaryKey"`
	UserID    uuid.UUID  `gorm:"column:user_id"`
	CodeHash  string     `gorm:"column:code_hash"`
	UsedAt    *time.Time `gorm:"column:used_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
}

func (recoveryCodeRow) TableName() string { return "mfa_recovery_codes" }
