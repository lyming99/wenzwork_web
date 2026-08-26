package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrRegistrationDisabled  = errors.New("registration is disabled")
	ErrEmailUnavailable      = errors.New("email is unavailable")
	ErrInvalidCredentials    = errors.New("invalid credentials")
	ErrEmailNotVerified      = errors.New("email is not verified")
	ErrAccountUnavailable    = errors.New("account is unavailable")
	ErrSessionUnavailable    = errors.New("session is unavailable")
	ErrVerificationToken     = errors.New("verification token is unavailable")
	ErrPasswordResetToken    = errors.New("password reset token is unavailable")
	ErrCurrentPassword       = errors.New("current password is incorrect")
	ErrSessionTargetNotFound = errors.New("session target not found")
)

type ServiceConfig struct {
	RegistrationEnabled    bool
	PublicBaseURL          string
	PasswordParams         Argon2Params
	SessionIdleTTL         time.Duration
	SessionAbsoluteTTL     time.Duration
	RememberAbsoluteTTL    time.Duration
	VerificationTTL        time.Duration
	PasswordResetTTL       time.Duration
	DeviceAuthorizationTTL time.Duration
	AppAccessTokenTTL      time.Duration
	AppRefreshTokenTTL     time.Duration
	MFAEncryptionKey       string
}

func DefaultServiceConfig() ServiceConfig {
	return ServiceConfig{
		RegistrationEnabled:    true,
		PublicBaseURL:          "http://localhost:5173",
		PasswordParams:         DefaultArgon2Params(),
		SessionIdleTTL:         30 * time.Minute,
		SessionAbsoluteTTL:     7 * 24 * time.Hour,
		RememberAbsoluteTTL:    30 * 24 * time.Hour,
		VerificationTTL:        24 * time.Hour,
		PasswordResetTTL:       time.Hour,
		DeviceAuthorizationTTL: 10 * time.Minute,
		AppAccessTokenTTL:      15 * time.Minute,
		AppRefreshTokenTTL:     30 * 24 * time.Hour,
		MFAEncryptionKey:       "wenzwork-development-mfa-key-change-me",
	}
}

type Service struct {
	db        *gorm.DB
	mailer    mailer.Sender
	config    ServiceConfig
	dummyHash string
	mfaKey    [32]byte
	now       func() time.Time
}

type User struct {
	ID              uuid.UUID  `json:"id"`
	Email           string     `json:"email"`
	DisplayName     string     `json:"displayName"`
	Status          string     `json:"status"`
	EmailVerifiedAt *time.Time `json:"emailVerifiedAt"`
	Roles           []string   `json:"roles"`
}

type RegisterInput struct {
	Email       string
	Password    string
	DisplayName string
}

type RegisterResult struct {
	User                User
	VerificationSent    bool
	AlreadyRegistered   bool
	VerificationExpires time.Time
}

type LoginInput struct {
	Email       string
	Password    string
	RememberMe  bool
	UserAgent   string
	ClientIP    string
	CurrentTime time.Time
}

type LoginResult struct {
	Session     Session
	MFARequired bool
	MFAEnrolled bool
}

type Session struct {
	ID                uuid.UUID
	User              User
	Token             string
	CSRFToken         string
	RememberMe        bool
	AssuranceLevel    int16
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

type AuthenticatedSession struct {
	ID                uuid.UUID
	User              User
	CSRFTokenHash     string
	RememberMe        bool
	AssuranceLevel    int16
	LastSeenAt        time.Time
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

type SessionSummary struct {
	ID                uuid.UUID `json:"id"`
	Current           bool      `json:"current"`
	UserAgentSummary  string    `json:"userAgentSummary"`
	RememberMe        bool      `json:"rememberMe"`
	AssuranceLevel    int16     `json:"assuranceLevel"`
	LastSeenAt        time.Time `json:"lastSeenAt"`
	AbsoluteExpiresAt time.Time `json:"absoluteExpiresAt"`
	CreatedAt         time.Time `json:"createdAt"`
}

func NewService(db *gorm.DB, sender mailer.Sender, config ServiceConfig) (*Service, error) {
	if db == nil {
		return nil, errors.New("auth service database is required")
	}
	if sender == nil {
		return nil, errors.New("auth service mailer is required")
	}
	if err := config.PasswordParams.Validate(); err != nil {
		return nil, fmt.Errorf("auth password parameters: %w", err)
	}
	if _, err := url.ParseRequestURI(config.PublicBaseURL); err != nil {
		return nil, fmt.Errorf("auth public base URL: %w", err)
	}
	if config.SessionIdleTTL <= 0 || config.SessionAbsoluteTTL <= config.SessionIdleTTL ||
		config.RememberAbsoluteTTL < config.SessionAbsoluteTTL || config.VerificationTTL <= 0 || config.PasswordResetTTL <= 0 ||
		config.DeviceAuthorizationTTL <= 0 || config.AppAccessTokenTTL <= 0 ||
		config.AppRefreshTokenTTL <= config.AppAccessTokenTTL {
		return nil, errors.New("auth service TTL configuration is invalid")
	}
	if len(config.MFAEncryptionKey) < 32 {
		return nil, errors.New("auth MFA encryption key must contain at least 32 bytes")
	}
	dummyHash, err := HashPassword("not-a-real-user-password", config.PasswordParams)
	if err != nil {
		return nil, fmt.Errorf("create dummy password hash: %w", err)
	}
	return &Service{
		db: db, mailer: sender, config: config, dummyHash: dummyHash,
		mfaKey: sha256.Sum256([]byte(config.MFAEncryptionKey)), now: time.Now,
	}, nil
}

func (s *Service) Register(ctx context.Context, input RegisterInput) (RegisterResult, error) {
	if !s.config.RegistrationEnabled {
		return RegisterResult{}, ErrRegistrationDisabled
	}
	email, err := normalizeEmail(input.Email)
	if err != nil {
		return RegisterResult{}, err
	}
	displayName, err := normalizeDisplayName(input.DisplayName)
	if err != nil {
		return RegisterResult{}, err
	}
	passwordHash, err := HashPassword(input.Password, s.config.PasswordParams)
	if err != nil {
		return RegisterResult{}, err
	}
	plaintextToken, tokenHash, err := NewOpaqueToken()
	if err != nil {
		return RegisterResult{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(s.config.VerificationTTL)
	row := userRow{
		ID:              uuid.New(),
		Email:           email,
		PasswordHash:    passwordHash,
		DisplayName:     displayName,
		Status:          "pending",
		PasswordChanged: now,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrEmailUnavailable
			}
			return fmt.Errorf("create auth user: %w", err)
		}
		if err := tx.Exec(`
			INSERT INTO user_roles (user_id, role_id)
			SELECT ?, id FROM roles WHERE code = 'user'
		`, row.ID).Error; err != nil {
			return fmt.Errorf("grant default user role: %w", err)
		}
		if err := tx.Create(&emailVerificationTokenRow{
			ID:        uuid.New(),
			UserID:    row.ID,
			TokenHash: tokenHash,
			ExpiresAt: expiresAt,
			CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("create verification token: %w", err)
		}
		return nil
	})
	if errors.Is(err, ErrEmailUnavailable) {
		// Do not reveal whether an address already has an account.
		return RegisterResult{AlreadyRegistered: true}, nil
	}
	if err != nil {
		return RegisterResult{}, err
	}

	sent := s.sendVerificationEmail(ctx, email, plaintextToken, expiresAt) == nil
	return RegisterResult{
		User:                s.userFromRow(row, []string{"user"}),
		VerificationSent:    sent,
		VerificationExpires: expiresAt,
	}, nil
}

func (s *Service) ResendVerification(ctx context.Context, rawEmail string) error {
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return nil
	}
	var row userRow
	if err := s.db.WithContext(ctx).Where("email = ?", email).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("load verification user: %w", err)
	}
	if row.EmailVerifiedAt != nil || row.Status == "disabled" {
		return nil
	}
	plaintext, digest, err := NewOpaqueToken()
	if err != nil {
		return err
	}
	now := s.now().UTC()
	expiresAt := now.Add(s.config.VerificationTTL)
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&emailVerificationTokenRow{}).
			Where("user_id = ? AND used_at IS NULL", row.ID).
			Update("used_at", now).Error; err != nil {
			return fmt.Errorf("invalidate verification tokens: %w", err)
		}
		if err := tx.Create(&emailVerificationTokenRow{
			ID: uuid.New(), UserID: row.ID, TokenHash: digest, ExpiresAt: expiresAt, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("create replacement verification token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return s.sendVerificationEmail(ctx, email, plaintext, expiresAt)
}

func (s *Service) VerifyEmail(ctx context.Context, plaintextToken string) (User, error) {
	digest, err := DigestOpaqueToken(plaintextToken)
	if err != nil {
		return User{}, ErrVerificationToken
	}
	now := s.now().UTC()
	var user userRow
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token emailVerificationTokenRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", digest).First(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrVerificationToken
			}
			return fmt.Errorf("lock verification token: %w", err)
		}
		if token.UsedAt != nil || !token.ExpiresAt.After(now) {
			return ErrVerificationToken
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", token.UserID).Error; err != nil {
			return fmt.Errorf("load verification user: %w", err)
		}
		if user.Status == "disabled" {
			return ErrVerificationToken
		}
		if err := tx.Model(&emailVerificationTokenRow{}).Where("id = ?", token.ID).Update("used_at", now).Error; err != nil {
			return fmt.Errorf("consume verification token: %w", err)
		}
		if err := tx.Model(&userRow{}).Where("id = ?", user.ID).Updates(map[string]any{
			"email_verified_at": now,
			"status":            "active",
			"updated_at":        now,
		}).Error; err != nil {
			return fmt.Errorf("verify auth user: %w", err)
		}
		user.EmailVerifiedAt = &now
		user.Status = "active"
		return nil
	})
	if err != nil {
		return User{}, err
	}
	roles, err := s.loadRoles(ctx, s.db, user.ID)
	if err != nil {
		return User{}, err
	}
	return s.userFromRow(user, roles), nil
}

func (s *Service) Login(ctx context.Context, input LoginInput) (LoginResult, error) {
	email, normalizeErr := normalizeEmail(input.Email)
	if normalizeErr != nil {
		email = "invalid@example.invalid"
	}
	limited, err := s.loginRateLimited(ctx, email, input.ClientIP)
	if err != nil {
		return LoginResult{}, err
	}
	if limited {
		return LoginResult{}, ErrRateLimited
	}
	var row userRow
	loadErr := s.db.WithContext(ctx).Where("email = ?", email).First(&row).Error
	encoded := s.dummyHash
	if loadErr == nil {
		encoded = row.PasswordHash
	} else if !errors.Is(loadErr, gorm.ErrRecordNotFound) {
		return LoginResult{}, fmt.Errorf("load login user: %w", loadErr)
	}
	valid, verifyErr := VerifyPassword(encoded, input.Password)
	if verifyErr != nil {
		return LoginResult{}, fmt.Errorf("verify stored password: %w", verifyErr)
	}
	if loadErr != nil || normalizeErr != nil || !valid {
		if err := s.recordLoginFailure(ctx, email, input.ClientIP); err != nil {
			return LoginResult{}, err
		}
		return LoginResult{}, ErrInvalidCredentials
	}
	if row.Status == "disabled" {
		return LoginResult{}, ErrAccountUnavailable
	}
	if row.EmailVerifiedAt == nil || row.Status == "pending" {
		return LoginResult{}, ErrEmailNotVerified
	}
	if row.Status != "active" {
		return LoginResult{}, ErrAccountUnavailable
	}
	if err := s.clearLoginEmailFailures(ctx, email); err != nil {
		return LoginResult{}, err
	}

	if PasswordHashNeedsUpgrade(row.PasswordHash, s.config.PasswordParams) {
		upgraded, err := HashPassword(input.Password, s.config.PasswordParams)
		if err != nil {
			return LoginResult{}, err
		}
		if err := s.db.WithContext(ctx).Model(&userRow{}).Where("id = ?", row.ID).
			Updates(map[string]any{"password_hash": upgraded, "updated_at": s.now().UTC()}).Error; err != nil {
			return LoginResult{}, fmt.Errorf("upgrade password hash: %w", err)
		}
	}

	roles, err := s.loadRoles(ctx, s.db, row.ID)
	if err != nil {
		return LoginResult{}, err
	}
	mfaEnrolled, err := s.hasVerifiedTOTP(ctx, row.ID)
	if err != nil {
		return LoginResult{}, err
	}
	session, err := s.createSession(ctx, row, roles, input.RememberMe, input.UserAgent)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		Session:     session,
		MFARequired: hasAdministrativeRole(roles),
		MFAEnrolled: mfaEnrolled,
	}, nil
}

func (s *Service) AuthenticateSession(ctx context.Context, plaintextToken string) (AuthenticatedSession, error) {
	digest, err := DigestOpaqueToken(plaintextToken)
	if err != nil {
		return AuthenticatedSession{}, ErrSessionUnavailable
	}
	now := s.now().UTC()
	var session sessionRow
	err = s.db.WithContext(ctx).
		Preload("User").
		Where("token_hash = ? AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?", digest, now, now).
		First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AuthenticatedSession{}, ErrSessionUnavailable
	}
	if err != nil {
		return AuthenticatedSession{}, fmt.Errorf("load session: %w", err)
	}
	if session.User.Status != "active" {
		return AuthenticatedSession{}, ErrSessionUnavailable
	}
	roles, err := s.loadRoles(ctx, s.db, session.UserID)
	if err != nil {
		return AuthenticatedSession{}, err
	}
	if now.Sub(session.LastSeenAt) >= 5*time.Minute {
		idleExpiry := minTime(now.Add(s.config.SessionIdleTTL), session.AbsoluteExpiresAt)
		if err := s.db.WithContext(ctx).Model(&sessionRow{}).Where("id = ? AND revoked_at IS NULL", session.ID).
			Updates(map[string]any{"last_seen_at": now, "idle_expires_at": idleExpiry, "updated_at": now}).Error; err != nil {
			return AuthenticatedSession{}, fmt.Errorf("refresh session activity: %w", err)
		}
		session.LastSeenAt = now
		session.IdleExpiresAt = idleExpiry
	}
	return AuthenticatedSession{
		ID:                session.ID,
		User:              s.userFromRow(session.User, roles),
		CSRFTokenHash:     session.CSRFTokenHash,
		RememberMe:        session.RememberMe,
		AssuranceLevel:    session.AssuranceLevel,
		LastSeenAt:        session.LastSeenAt,
		IdleExpiresAt:     session.IdleExpiresAt,
		AbsoluteExpiresAt: session.AbsoluteExpiresAt,
	}, nil
}

func VerifySessionCSRF(session AuthenticatedSession, plaintextToken string) bool {
	digest, err := DigestOpaqueToken(plaintextToken)
	if err != nil || len(digest) != len(session.CSRFTokenHash) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(digest), []byte(session.CSRFTokenHash)) == 1
}

func (s *Service) Logout(ctx context.Context, plaintextToken string) error {
	digest, err := DigestOpaqueToken(plaintextToken)
	if err != nil {
		return nil
	}
	now := s.now().UTC()
	if err := s.db.WithContext(ctx).Model(&sessionRow{}).
		Where("token_hash = ? AND revoked_at IS NULL", digest).
		Updates(map[string]any{"revoked_at": now, "revoked_reason": "logout", "updated_at": now}).Error; err != nil {
		return fmt.Errorf("revoke logout session: %w", err)
	}
	return nil
}

func (s *Service) ListSessions(ctx context.Context, userID, currentSessionID uuid.UUID) ([]SessionSummary, error) {
	var rows []sessionRow
	if err := s.db.WithContext(ctx).
		Where("user_id = ? AND revoked_at IS NULL AND absolute_expires_at > ?", userID, s.now().UTC()).
		Order("last_seen_at DESC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list auth sessions: %w", err)
	}
	items := make([]SessionSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, SessionSummary{
			ID: row.ID, Current: row.ID == currentSessionID, UserAgentSummary: row.UserAgentSummary,
			RememberMe: row.RememberMe, AssuranceLevel: row.AssuranceLevel, LastSeenAt: row.LastSeenAt,
			AbsoluteExpiresAt: row.AbsoluteExpiresAt, CreatedAt: row.CreatedAt,
		})
	}
	return items, nil
}

func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	now := s.now().UTC()
	result := s.db.WithContext(ctx).Model(&sessionRow{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", sessionID, userID).
		Updates(map[string]any{"revoked_at": now, "revoked_reason": "user_revoked", "updated_at": now})
	if result.Error != nil {
		return fmt.Errorf("revoke user session: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrSessionTargetNotFound
	}
	return nil
}

func (s *Service) UpdateProfile(ctx context.Context, userID uuid.UUID, rawDisplayName string) (User, error) {
	displayName, err := normalizeDisplayName(rawDisplayName)
	if err != nil {
		return User{}, err
	}
	now := s.now().UTC()
	if err := s.db.WithContext(ctx).Model(&userRow{}).Where("id = ? AND status = 'active'", userID).
		Updates(map[string]any{"display_name": displayName, "updated_at": now}).Error; err != nil {
		return User{}, fmt.Errorf("update auth profile: %w", err)
	}
	return s.GetUser(ctx, userID)
}

func (s *Service) GetUser(ctx context.Context, userID uuid.UUID) (User, error) {
	var row userRow
	if err := s.db.WithContext(ctx).First(&row, "id = ? AND status = 'active'", userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return User{}, ErrAccountUnavailable
		}
		return User{}, fmt.Errorf("load auth user: %w", err)
	}
	roles, err := s.loadRoles(ctx, s.db, userID)
	if err != nil {
		return User{}, err
	}
	return s.userFromRow(row, roles), nil
}

func (s *Service) RequestPasswordReset(ctx context.Context, rawEmail string) error {
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return nil
	}
	var row userRow
	if err := s.db.WithContext(ctx).Where("email = ?", email).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("load password reset user: %w", err)
	}
	if row.Status != "active" || row.EmailVerifiedAt == nil {
		return nil
	}
	plaintext, digest, err := NewOpaqueToken()
	if err != nil {
		return err
	}
	now := s.now().UTC()
	expiresAt := now.Add(s.config.PasswordResetTTL)
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&passwordResetTokenRow{}).Where("user_id = ? AND used_at IS NULL", row.ID).
			Update("used_at", now).Error; err != nil {
			return fmt.Errorf("invalidate reset tokens: %w", err)
		}
		if err := tx.Create(&passwordResetTokenRow{
			ID: uuid.New(), UserID: row.ID, TokenHash: digest, ExpiresAt: expiresAt, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("create password reset token: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return s.sendPasswordResetEmail(ctx, email, plaintext, expiresAt)
}

func (s *Service) ResetPassword(ctx context.Context, plaintextToken, newPassword string) error {
	passwordHash, err := HashPassword(newPassword, s.config.PasswordParams)
	if err != nil {
		return err
	}
	digest, err := DigestOpaqueToken(plaintextToken)
	if err != nil {
		return ErrPasswordResetToken
	}
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token passwordResetTokenRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("token_hash = ?", digest).First(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPasswordResetToken
			}
			return fmt.Errorf("lock password reset token: %w", err)
		}
		if token.UsedAt != nil || !token.ExpiresAt.After(now) {
			return ErrPasswordResetToken
		}
		if err := tx.Model(&userRow{}).Where("id = ? AND status = 'active'", token.UserID).
			Updates(map[string]any{"password_hash": passwordHash, "password_changed_at": now, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("reset auth password: %w", err)
		}
		if err := tx.Model(&passwordResetTokenRow{}).Where("id = ?", token.ID).Update("used_at", now).Error; err != nil {
			return fmt.Errorf("consume reset token: %w", err)
		}
		if err := revokeAllSessions(tx, token.UserID, now, "password_reset", uuid.Nil); err != nil {
			return err
		}
		return nil
	})
}

func (s *Service) ChangePassword(ctx context.Context, userID, currentSessionID uuid.UUID, currentPassword, newPassword string, revokeOthers bool) error {
	var row userRow
	if err := s.db.WithContext(ctx).First(&row, "id = ? AND status = 'active'", userID).Error; err != nil {
		return ErrAccountUnavailable
	}
	valid, err := VerifyPassword(row.PasswordHash, currentPassword)
	if err != nil {
		return fmt.Errorf("verify current password: %w", err)
	}
	if !valid {
		return ErrCurrentPassword
	}
	hash, err := HashPassword(newPassword, s.config.PasswordParams)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&userRow{}).Where("id = ?", userID).Updates(map[string]any{
			"password_hash": hash, "password_changed_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("change auth password: %w", err)
		}
		if revokeOthers {
			return revokeAllSessions(tx, userID, now, "password_changed", currentSessionID)
		}
		return nil
	})
}

func (s *Service) createSession(ctx context.Context, user userRow, roles []string, remember bool, rawUserAgent string) (Session, error) {
	token, tokenHash, err := NewOpaqueToken()
	if err != nil {
		return Session{}, err
	}
	csrfToken, csrfHash, err := NewOpaqueToken()
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	absoluteTTL := s.config.SessionAbsoluteTTL
	if remember {
		absoluteTTL = s.config.RememberAbsoluteTTL
	}
	absoluteExpiry := now.Add(absoluteTTL)
	idleExpiry := minTime(now.Add(s.config.SessionIdleTTL), absoluteExpiry)
	row := sessionRow{
		ID: uuid.New(), UserID: user.ID, TokenHash: tokenHash, CSRFTokenHash: csrfHash,
		UserAgentSummary: summarizeUserAgent(rawUserAgent), RememberMe: remember, AssuranceLevel: 1,
		LastSeenAt: now, IdleExpiresAt: idleExpiry, AbsoluteExpiresAt: absoluteExpiry,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Session{}, fmt.Errorf("create auth session: %w", err)
	}
	return Session{
		ID: row.ID, User: s.userFromRow(user, roles), Token: token, CSRFToken: csrfToken,
		RememberMe: remember, AssuranceLevel: 1, LastSeenAt: now,
		IdleExpiresAt: idleExpiry, AbsoluteExpiresAt: absoluteExpiry,
	}, nil
}

func (s *Service) loadRoles(ctx context.Context, db *gorm.DB, userID uuid.UUID) ([]string, error) {
	var roles []string
	if err := db.WithContext(ctx).Table("roles").
		Select("roles.code").
		Joins("JOIN user_roles ON user_roles.role_id = roles.id").
		Where("user_roles.user_id = ?", userID).
		Order("roles.code ASC").Scan(&roles).Error; err != nil {
		return nil, fmt.Errorf("load auth roles: %w", err)
	}
	if len(roles) == 0 {
		roles = []string{"user"}
	}
	return roles, nil
}

func (s *Service) hasVerifiedTOTP(ctx context.Context, userID uuid.UUID) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Table("mfa_totp_credentials").
		Where("user_id = ? AND verified_at IS NOT NULL", userID).Count(&count).Error; err != nil {
		return false, fmt.Errorf("load auth MFA status: %w", err)
	}
	return count > 0, nil
}

func (s *Service) userFromRow(row userRow, roles []string) User {
	return User{
		ID: row.ID, Email: row.Email, DisplayName: row.DisplayName, Status: row.Status,
		EmailVerifiedAt: row.EmailVerifiedAt, Roles: roles,
	}
}

func (s *Service) sendVerificationEmail(ctx context.Context, email, token string, expiresAt time.Time) error {
	verificationURL, err := s.publicURL("/verify-email", token)
	if err != nil {
		return err
	}
	return s.mailer.Send(ctx, mailer.Message{
		To: email, Subject: "验证你的 WenzWork 邮箱",
		Text: fmt.Sprintf("请打开下面的链接验证邮箱：\n\n%s\n\n链接将在 %s 失效。如果不是你发起的，请忽略本邮件。", verificationURL, expiresAt.Format(time.RFC3339)),
	})
}

func (s *Service) sendPasswordResetEmail(ctx context.Context, email, token string, expiresAt time.Time) error {
	resetURL, err := s.publicURL("/reset-password", token)
	if err != nil {
		return err
	}
	return s.mailer.Send(ctx, mailer.Message{
		To: email, Subject: "重置你的 WenzWork 密码",
		Text: fmt.Sprintf("请打开下面的链接重置密码：\n\n%s\n\n链接将在 %s 失效且只能使用一次。如果不是你发起的，请忽略本邮件。", resetURL, expiresAt.Format(time.RFC3339)),
	})
}

func (s *Service) publicURL(path, token string) (string, error) {
	base, err := url.Parse(s.config.PublicBaseURL)
	if err != nil {
		return "", fmt.Errorf("parse public base URL: %w", err)
	}
	base.Path = path
	base.RawQuery = url.Values{"token": []string{token}}.Encode()
	base.Fragment = ""
	return base.String(), nil
}

func normalizeEmail(raw string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	if normalized == "" || len(normalized) > 320 || strings.ContainsAny(normalized, "\r\n") {
		return "", errors.New("valid email is required")
	}
	parsed, err := mail.ParseAddress(normalized)
	if err != nil || parsed.Name != "" || parsed.Address != normalized || !strings.Contains(normalized, "@") {
		return "", errors.New("valid email is required")
	}
	return normalized, nil
}

func normalizeDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("display name is required")
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 120 {
		return "", errors.New("display name must contain at most 120 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", errors.New("display name contains control characters")
		}
	}
	return name, nil
}

func summarizeUserAgent(raw string) string {
	runes := make([]rune, 0, min(len(raw), 255))
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) {
			continue
		}
		runes = append(runes, r)
		if len(runes) == 255 {
			break
		}
	}
	if len(runes) == 0 {
		return "未知设备"
	}
	return string(runes)
}

func hasAdministrativeRole(roles []string) bool {
	for _, role := range roles {
		if role != "user" {
			return true
		}
	}
	return false
}

func revokeAllSessions(tx *gorm.DB, userID uuid.UUID, now time.Time, reason string, except uuid.UUID) error {
	query := tx.Model(&sessionRow{}).Where("user_id = ? AND revoked_at IS NULL", userID)
	if except != uuid.Nil {
		query = query.Where("id <> ?", except)
	}
	if err := query.Updates(map[string]any{"revoked_at": now, "revoked_reason": reason, "updated_at": now}).Error; err != nil {
		return fmt.Errorf("revoke auth sessions: %w", err)
	}
	var appSessionIDs []uuid.UUID
	if err := tx.Model(&appSessionRow{}).Where("user_id = ? AND revoked_at IS NULL", userID).
		Pluck("id", &appSessionIDs).Error; err != nil {
		return fmt.Errorf("find app sessions to revoke: %w", err)
	}
	for _, appSessionID := range appSessionIDs {
		if err := revokeAppSession(tx, appSessionID, now, reason); err != nil {
			return err
		}
	}
	return nil
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

type userRow struct {
	ID              uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Email           string     `gorm:"column:email"`
	PasswordHash    string     `gorm:"column:password_hash"`
	DisplayName     string     `gorm:"column:display_name"`
	Status          string     `gorm:"column:status"`
	EmailVerifiedAt *time.Time `gorm:"column:email_verified_at"`
	PasswordChanged time.Time  `gorm:"column:password_changed_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

func (userRow) TableName() string { return "users" }

type sessionRow struct {
	ID                uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID            uuid.UUID  `gorm:"column:user_id;type:uuid"`
	User              userRow    `gorm:"foreignKey:UserID"`
	TokenHash         string     `gorm:"column:token_hash"`
	CSRFTokenHash     string     `gorm:"column:csrf_token_hash"`
	UserAgentSummary  string     `gorm:"column:user_agent_summary"`
	RememberMe        bool       `gorm:"column:remember_me"`
	AssuranceLevel    int16      `gorm:"column:assurance_level"`
	LastSeenAt        time.Time  `gorm:"column:last_seen_at"`
	IdleExpiresAt     time.Time  `gorm:"column:idle_expires_at"`
	AbsoluteExpiresAt time.Time  `gorm:"column:absolute_expires_at"`
	MFAVerifiedAt     *time.Time `gorm:"column:mfa_verified_at"`
	RevokedAt         *time.Time `gorm:"column:revoked_at"`
	RevokedReason     *string    `gorm:"column:revoked_reason"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

func (sessionRow) TableName() string { return "sessions" }

type emailVerificationTokenRow struct {
	ID        uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID    uuid.UUID  `gorm:"column:user_id;type:uuid"`
	TokenHash string     `gorm:"column:token_hash"`
	ExpiresAt time.Time  `gorm:"column:expires_at"`
	UsedAt    *time.Time `gorm:"column:used_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
}

func (emailVerificationTokenRow) TableName() string { return "email_verification_tokens" }

type passwordResetTokenRow struct {
	ID        uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID    uuid.UUID  `gorm:"column:user_id;type:uuid"`
	TokenHash string     `gorm:"column:token_hash"`
	ExpiresAt time.Time  `gorm:"column:expires_at"`
	UsedAt    *time.Time `gorm:"column:used_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
}

func (passwordResetTokenRow) TableName() string { return "password_reset_tokens" }
