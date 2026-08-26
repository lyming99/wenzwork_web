package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	DesktopClientID          = "wenzwork-desktop"
	MobileClientID           = "wenzwork-mobile"
	DeviceAuthorizationScope = "profile.read membership.read remote.connect"
	DeviceGrantType          = "urn:ietf:params:oauth:grant-type:device_code"
	PasswordGrantType        = "password"
	RefreshTokenGrantType    = "refresh_token"
	devicePollInterval       = 5
	maximumPollInterval      = 60
)

// allowedAppClientIDs 是设备授权流程接受的官方客户端标识白名单。
// 新增原生客户端（如 wenzwork-mobile）时在此登记即可，校验逻辑统一走
// isAllowedAppClientID。
var allowedAppClientIDs = map[string]struct{}{
	DesktopClientID: {},
	MobileClientID:  {},
}

// isAllowedAppClientID 判断 client_id 是否为受信任的官方客户端。
func isAllowedAppClientID(clientID string) bool {
	_, ok := allowedAppClientIDs[strings.TrimSpace(clientID)]
	return ok
}

var (
	ErrDeviceClientInvalid         = errors.New("device client is invalid")
	ErrDeviceAuthorizationInvalid  = errors.New("device authorization is invalid")
	ErrDeviceAuthorizationPending  = errors.New("device authorization is pending")
	ErrDeviceAuthorizationSlowDown = errors.New("device authorization polling must slow down")
	ErrDeviceAuthorizationDenied   = errors.New("device authorization was denied")
	ErrDeviceAuthorizationExpired  = errors.New("device authorization expired")
	ErrAppTokenInvalid             = errors.New("app token is invalid")
	ErrAppRefreshReplay            = errors.New("app refresh token replay detected")
)

type CreateDeviceAuthorizationInput struct {
	ClientID   string
	DeviceID   string
	DeviceName string
	ClientIP   string
}

// PasswordAppLoginInput is accepted only by the official native clients. The
// resulting credentials are regular app access and refresh tokens; the
// password is never persisted or returned to the client.
type PasswordAppLoginInput struct {
	ClientID   string
	DeviceID   string
	DeviceName string
	Email      string
	Password   string
	ClientIP   string
	UserAgent  string
}

type DeviceAuthorization struct {
	RequestID               uuid.UUID
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int64
	Interval                int
}

type BrowserDeviceAuthorization struct {
	RequestID  uuid.UUID `json:"requestId"`
	ClientID   string    `json:"clientId"`
	DeviceID   uuid.UUID `json:"deviceId"`
	DeviceName string    `json:"deviceName"`
	UserCode   string    `json:"userCode"`
	Scope      string    `json:"scope"`
	Status     string    `json:"status"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type AppTokenResult struct {
	UserID           uuid.UUID
	SessionID        uuid.UUID
	AccessToken      string
	AccessExpiresIn  int64
	RefreshToken     string
	RefreshExpiresIn int64
	Scope            string
}

type AuthenticatedAppSession struct {
	SessionID        uuid.UUID
	User             User
	ClientID         string
	DeviceID         uuid.UUID
	Scope            string
	AccessExpiresAt  time.Time
	SessionExpiresAt time.Time
}

func (s *Service) CreateDeviceAuthorization(ctx context.Context, input CreateDeviceAuthorizationInput) (DeviceAuthorization, error) {
	if !isAllowedAppClientID(input.ClientID) {
		return DeviceAuthorization{}, ErrDeviceClientInvalid
	}
	limited, err := s.rateLimited(ctx, "device_authorization_ip", input.ClientIP)
	if err != nil {
		return DeviceAuthorization{}, err
	}
	if limited {
		return DeviceAuthorization{}, ErrRateLimited
	}
	deviceID, err := uuid.Parse(strings.TrimSpace(input.DeviceID))
	if err != nil || deviceID == uuid.Nil {
		return DeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	deviceName, err := normalizeDeviceName(input.DeviceName)
	if err != nil {
		return DeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	if err := s.recordRateFailure(ctx, "device_authorization_ip", input.ClientIP); err != nil {
		return DeviceAuthorization{}, err
	}

	now := s.now().UTC()
	for attempt := 0; attempt < 4; attempt++ {
		deviceCode, deviceDigest, err := NewOpaqueToken()
		if err != nil {
			return DeviceAuthorization{}, err
		}
		userCode, userDigest, err := newDeviceUserCode()
		if err != nil {
			return DeviceAuthorization{}, err
		}
		row := appAuthorizationRequestRow{
			ID: uuid.New(), ClientID: strings.TrimSpace(input.ClientID), DeviceCodeHash: deviceDigest,
			UserCodeHash: userDigest, DeviceID: deviceID, DeviceName: deviceName,
			Scope: DeviceAuthorizationScope, Status: "pending", PollIntervalSeconds: devicePollInterval,
			ExpiresAt: now.Add(s.config.DeviceAuthorizationTTL), CreatedAt: now, UpdatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			if isUniqueViolation(err) {
				continue
			}
			return DeviceAuthorization{}, fmt.Errorf("create device authorization: %w", err)
		}
		verificationURI, completeURI, err := s.deviceVerificationURLs(userCode)
		if err != nil {
			return DeviceAuthorization{}, err
		}
		return DeviceAuthorization{
			RequestID: row.ID, DeviceCode: deviceCode, UserCode: userCode,
			VerificationURI: verificationURI, VerificationURIComplete: completeURI,
			ExpiresIn: int64(s.config.DeviceAuthorizationTTL.Seconds()), Interval: devicePollInterval,
		}, nil
	}
	return DeviceAuthorization{}, errors.New("could not allocate a unique device authorization")
}

func (s *Service) GetDeviceAuthorization(ctx context.Context, userCode string, userID uuid.UUID) (BrowserDeviceAuthorization, error) {
	normalized, digest, err := inspectDeviceUserCode(userCode)
	if err != nil || userID == uuid.Nil {
		return BrowserDeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	var row appAuthorizationRequestRow
	if err := s.db.WithContext(ctx).First(&row, "user_code_hash = ?", digest).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return BrowserDeviceAuthorization{}, ErrDeviceAuthorizationInvalid
		}
		return BrowserDeviceAuthorization{}, fmt.Errorf("load device authorization: %w", err)
	}
	if !row.ExpiresAt.After(s.now().UTC()) {
		return BrowserDeviceAuthorization{}, ErrDeviceAuthorizationExpired
	}
	if row.Status == "denied" || row.Status == "consumed" {
		return BrowserDeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	if row.Status == "approved" && (row.UserID == nil || *row.UserID != userID) {
		return BrowserDeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	return browserDeviceAuthorization(row, formatDeviceUserCode(normalized)), nil
}

func (s *Service) ApproveDeviceAuthorization(ctx context.Context, userCode string, userID uuid.UUID) (BrowserDeviceAuthorization, error) {
	return s.decideDeviceAuthorization(ctx, userCode, userID, true)
}

func (s *Service) DenyDeviceAuthorization(ctx context.Context, userCode string, userID uuid.UUID) error {
	_, err := s.decideDeviceAuthorization(ctx, userCode, userID, false)
	return err
}

func (s *Service) decideDeviceAuthorization(ctx context.Context, userCode string, userID uuid.UUID, approve bool) (BrowserDeviceAuthorization, error) {
	normalized, digest, err := inspectDeviceUserCode(userCode)
	if err != nil || userID == uuid.Nil {
		return BrowserDeviceAuthorization{}, ErrDeviceAuthorizationInvalid
	}
	now := s.now().UTC()
	var result BrowserDeviceAuthorization
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row appAuthorizationRequestRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "user_code_hash = ?", digest).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDeviceAuthorizationInvalid
			}
			return fmt.Errorf("lock device authorization: %w", err)
		}
		if !row.ExpiresAt.After(now) {
			return ErrDeviceAuthorizationExpired
		}
		if row.Status == "approved" && approve && row.UserID != nil && *row.UserID == userID {
			result = browserDeviceAuthorization(row, formatDeviceUserCode(normalized))
			return nil
		}
		if row.Status != "pending" {
			return ErrDeviceAuthorizationInvalid
		}
		var user userRow
		if err := tx.First(&user, "id = ? AND status = 'active' AND email_verified_at IS NOT NULL", userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAccountUnavailable
			}
			return fmt.Errorf("load device authorization user: %w", err)
		}
		status := "denied"
		updates := map[string]any{"status": status, "user_id": userID, "updated_at": now}
		if approve {
			status = "approved"
			updates["status"] = status
			updates["approved_at"] = now
		}
		if err := tx.Model(&appAuthorizationRequestRow{}).Where("id = ? AND status = 'pending'", row.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("decide device authorization: %w", err)
		}
		row.Status = status
		row.UserID = &userID
		row.UpdatedAt = now
		if approve {
			row.ApprovedAt = &now
		}
		result = browserDeviceAuthorization(row, formatDeviceUserCode(normalized))
		return nil
	})
	if err != nil {
		return BrowserDeviceAuthorization{}, err
	}
	return result, nil
}

func (s *Service) ExchangeDeviceCode(ctx context.Context, clientID, deviceCode string) (AppTokenResult, error) {
	if !isAllowedAppClientID(clientID) {
		return AppTokenResult{}, ErrDeviceClientInvalid
	}
	normalizedClientID := strings.TrimSpace(clientID)
	digest, err := DigestOpaqueToken(strings.TrimSpace(deviceCode))
	if err != nil {
		return AppTokenResult{}, ErrDeviceAuthorizationInvalid
	}
	now := s.now().UTC()
	var result AppTokenResult
	var outcome error
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request appAuthorizationRequestRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "device_code_hash = ? AND client_id = ?", digest, normalizedClientID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				outcome = ErrDeviceAuthorizationInvalid
				return nil
			}
			return fmt.Errorf("lock device authorization exchange: %w", err)
		}
		if !request.ExpiresAt.After(now) || request.Status == "consumed" {
			outcome = ErrDeviceAuthorizationExpired
			return nil
		}
		if request.LastPolledAt != nil && now.Sub(*request.LastPolledAt) < time.Duration(request.PollIntervalSeconds)*time.Second {
			request.PollIntervalSeconds = min(request.PollIntervalSeconds+5, maximumPollInterval)
			if err := tx.Model(&appAuthorizationRequestRow{}).Where("id = ?", request.ID).Updates(map[string]any{
				"last_polled_at": now, "poll_interval_seconds": request.PollIntervalSeconds, "updated_at": now,
			}).Error; err != nil {
				return fmt.Errorf("slow device authorization polling: %w", err)
			}
			outcome = ErrDeviceAuthorizationSlowDown
			return nil
		}
		if err := tx.Model(&appAuthorizationRequestRow{}).Where("id = ?", request.ID).
			Updates(map[string]any{"last_polled_at": now, "updated_at": now}).Error; err != nil {
			return fmt.Errorf("record device authorization poll: %w", err)
		}
		switch request.Status {
		case "pending":
			outcome = ErrDeviceAuthorizationPending
			return nil
		case "denied":
			outcome = ErrDeviceAuthorizationDenied
			return nil
		case "approved":
		default:
			outcome = ErrDeviceAuthorizationInvalid
			return nil
		}
		if request.UserID == nil {
			outcome = ErrDeviceAuthorizationInvalid
			return nil
		}
		var user userRow
		if err := tx.First(&user, "id = ? AND status = 'active' AND email_verified_at IS NOT NULL", *request.UserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				outcome = ErrDeviceAuthorizationDenied
				return nil
			}
			return fmt.Errorf("load device token user: %w", err)
		}
		if err := revokeAppDeviceSessions(tx, user.ID, request.ClientID, request.DeviceID, now, "device_reauthorized"); err != nil {
			return err
		}
		issued, err := s.issueAppSession(tx, user.ID, request.ClientID, request.DeviceID, request.DeviceName, request.Scope, now)
		if err != nil {
			return err
		}
		if err := tx.Model(&appAuthorizationRequestRow{}).Where("id = ? AND status = 'approved'", request.ID).Updates(map[string]any{
			"status": "consumed", "consumed_at": now, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("consume device authorization: %w", err)
		}
		result = issued
		return nil
	})
	if err != nil {
		return AppTokenResult{}, err
	}
	if outcome != nil {
		return AppTokenResult{}, outcome
	}
	return result, nil
}

// LoginAppWithPassword authenticates an official native client without first
// opening a browser. It deliberately reuses Login so password validation,
// account-state checks, hash upgrades and rate limiting stay identical to the
// web sign-in path. The resulting app token has the normal client assurance
// level and is not a substitute for elevated browser-admin operations.
func (s *Service) LoginAppWithPassword(ctx context.Context, input PasswordAppLoginInput) (AppTokenResult, error) {
	if !isAllowedAppClientID(input.ClientID) {
		return AppTokenResult{}, ErrDeviceClientInvalid
	}
	deviceID, err := uuid.Parse(strings.TrimSpace(input.DeviceID))
	if err != nil || deviceID == uuid.Nil {
		return AppTokenResult{}, ErrDeviceAuthorizationInvalid
	}
	deviceName, err := normalizeDeviceName(input.DeviceName)
	if err != nil {
		return AppTokenResult{}, ErrDeviceAuthorizationInvalid
	}
	login, err := s.Login(ctx, LoginInput{
		Email: input.Email, Password: input.Password, RememberMe: false,
		UserAgent: input.UserAgent, ClientIP: input.ClientIP,
	})
	if err != nil {
		return AppTokenResult{}, err
	}
	// Login creates a web session as part of the shared credential-validation
	// path. It is never returned to the app and is immediately revoked.
	defer func() { _ = s.Logout(ctx, login.Session.Token) }()
	now := s.now().UTC()
	var result AppTokenResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := revokeAppDeviceSessions(tx, login.Session.User.ID, strings.TrimSpace(input.ClientID), deviceID, now, "password_reauthenticated"); err != nil {
			return err
		}
		issued, err := s.issueAppSession(tx, login.Session.User.ID, strings.TrimSpace(input.ClientID), deviceID, deviceName, DeviceAuthorizationScope, now)
		if err != nil {
			return err
		}
		result = issued
		return nil
	})
	if err != nil {
		return AppTokenResult{}, err
	}
	return result, nil
}

func (s *Service) RefreshAppToken(ctx context.Context, clientID, plaintext string) (AppTokenResult, error) {
	if !isAllowedAppClientID(clientID) {
		return AppTokenResult{}, ErrDeviceClientInvalid
	}
	digest, err := DigestOpaqueToken(strings.TrimSpace(plaintext))
	if err != nil {
		return AppTokenResult{}, ErrAppTokenInvalid
	}
	now := s.now().UTC()
	var result AppTokenResult
	var invalid, replay bool
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token appRefreshTokenRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&token, "token_hash = ?", digest).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				invalid = true
				return nil
			}
			return fmt.Errorf("lock app refresh token: %w", err)
		}
		var session appSessionRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&session, "id = ?", token.SessionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				invalid = true
				return nil
			}
			return fmt.Errorf("lock app session: %w", err)
		}
		if token.Status == "rotated" {
			if err := revokeAppSession(tx, session.ID, now, "refresh_token_reuse"); err != nil {
				return err
			}
			replay = true
			return nil
		}
		if token.Status != "active" || !isAllowedAppClientID(session.ClientID) || session.RevokedAt != nil ||
			!token.ExpiresAt.After(now) || !session.IdleExpiresAt.After(now) {
			invalid = true
			return nil
		}
		var user userRow
		if err := tx.First(&user, "id = ? AND status = 'active' AND email_verified_at IS NOT NULL", session.UserID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if err := revokeAppSession(tx, session.ID, now, "account_unavailable"); err != nil {
					return err
				}
				invalid = true
				return nil
			}
			return fmt.Errorf("load app refresh user: %w", err)
		}
		accessPlaintext, accessDigest, err := NewOpaqueToken()
		if err != nil {
			return err
		}
		refreshPlaintext, refreshDigest, err := NewOpaqueToken()
		if err != nil {
			return err
		}
		refreshExpiry := now.Add(s.config.AppRefreshTokenTTL)
		newRefresh := appRefreshTokenRow{
			ID: uuid.New(), SessionID: session.ID, TokenHash: refreshDigest, Status: "active",
			ExpiresAt: refreshExpiry, CreatedAt: now,
		}
		if err := tx.Create(&newRefresh).Error; err != nil {
			return fmt.Errorf("rotate app refresh token: %w", err)
		}
		if err := tx.Model(&appRefreshTokenRow{}).Where("id = ? AND status = 'active'", token.ID).Updates(map[string]any{
			"status": "rotated", "used_at": now, "replaced_by": newRefresh.ID,
		}).Error; err != nil {
			return fmt.Errorf("retire app refresh token: %w", err)
		}
		accessExpiry := now.Add(s.config.AppAccessTokenTTL)
		if err := tx.Create(&appAccessTokenRow{
			ID: uuid.New(), SessionID: session.ID, TokenHash: accessDigest,
			ExpiresAt: accessExpiry, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("create refreshed app access token: %w", err)
		}
		if err := tx.Model(&appSessionRow{}).Where("id = ? AND revoked_at IS NULL", session.ID).Updates(map[string]any{
			"last_seen_at": now, "idle_expires_at": refreshExpiry, "updated_at": now,
		}).Error; err != nil {
			return fmt.Errorf("extend app session: %w", err)
		}
		result = AppTokenResult{
			UserID: session.UserID, SessionID: session.ID, AccessToken: accessPlaintext,
			AccessExpiresIn: int64(s.config.AppAccessTokenTTL.Seconds()),
			RefreshToken:    refreshPlaintext, RefreshExpiresIn: int64(s.config.AppRefreshTokenTTL.Seconds()),
			Scope: session.Scope,
		}
		return nil
	})
	if err != nil {
		return AppTokenResult{}, err
	}
	if replay {
		return AppTokenResult{}, ErrAppRefreshReplay
	}
	if invalid {
		return AppTokenResult{}, ErrAppTokenInvalid
	}
	return result, nil
}

func (s *Service) AuthenticateAppAccessToken(ctx context.Context, plaintext string) (AuthenticatedAppSession, error) {
	digest, err := DigestOpaqueToken(strings.TrimSpace(plaintext))
	if err != nil {
		return AuthenticatedAppSession{}, ErrAppTokenInvalid
	}
	now := s.now().UTC()
	var row struct {
		SessionID        uuid.UUID `gorm:"column:session_id"`
		UserID           uuid.UUID `gorm:"column:user_id"`
		ClientID         string    `gorm:"column:client_id"`
		DeviceID         uuid.UUID `gorm:"column:device_id"`
		Scope            string    `gorm:"column:scope"`
		AccessExpiresAt  time.Time `gorm:"column:access_expires_at"`
		SessionExpiresAt time.Time `gorm:"column:session_expires_at"`
	}
	err = s.db.WithContext(ctx).Table("app_access_tokens AS t").Select(`
		s.id AS session_id, s.user_id, s.client_id, s.device_id, s.scope,
		t.expires_at AS access_expires_at, s.idle_expires_at AS session_expires_at
	`).Joins("JOIN app_sessions s ON s.id = t.session_id").
		Joins("JOIN users u ON u.id = s.user_id").
		Where("t.token_hash = ? AND t.expires_at > ? AND s.revoked_at IS NULL AND s.idle_expires_at > ? AND u.status = 'active' AND u.email_verified_at IS NOT NULL", digest, now, now).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AuthenticatedAppSession{}, ErrAppTokenInvalid
	}
	if err != nil {
		return AuthenticatedAppSession{}, fmt.Errorf("authenticate app access token: %w", err)
	}
	var user userRow
	if err := s.db.WithContext(ctx).First(&user, "id = ?", row.UserID).Error; err != nil {
		return AuthenticatedAppSession{}, fmt.Errorf("load app access user: %w", err)
	}
	roles, err := s.loadRoles(ctx, s.db, user.ID)
	if err != nil {
		return AuthenticatedAppSession{}, err
	}
	return AuthenticatedAppSession{
		SessionID: row.SessionID, User: s.userFromRow(user, roles), ClientID: row.ClientID,
		DeviceID: row.DeviceID, Scope: row.Scope, AccessExpiresAt: row.AccessExpiresAt,
		SessionExpiresAt: row.SessionExpiresAt,
	}, nil
}

func (s *Service) RevokeAppToken(ctx context.Context, clientID, plaintext string) error {
	if !isAllowedAppClientID(clientID) {
		return nil
	}
	digest, err := DigestOpaqueToken(strings.TrimSpace(plaintext))
	if err != nil {
		return nil
	}
	now := s.now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var sessionID uuid.UUID
		var refresh appRefreshTokenRow
		err := tx.First(&refresh, "token_hash = ?", digest).Error
		if err == nil {
			sessionID = refresh.SessionID
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("find app refresh token for revocation: %w", err)
		}
		if sessionID == uuid.Nil {
			var access appAccessTokenRow
			err = tx.First(&access, "token_hash = ?", digest).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("find app access token for revocation: %w", err)
			}
			sessionID = access.SessionID
		}
		var session appSessionRow
		if err := tx.First(&session, "id = ?", sessionID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return fmt.Errorf("load app session for revocation: %w", err)
		}
		if !isAllowedAppClientID(session.ClientID) {
			return nil
		}
		return revokeAppSession(tx, session.ID, now, "client_logout")
	})
}

func (s *Service) issueAppSession(tx *gorm.DB, userID uuid.UUID, clientID string, deviceID uuid.UUID, deviceName, scope string, now time.Time) (AppTokenResult, error) {
	accessPlaintext, accessDigest, err := NewOpaqueToken()
	if err != nil {
		return AppTokenResult{}, err
	}
	refreshPlaintext, refreshDigest, err := NewOpaqueToken()
	if err != nil {
		return AppTokenResult{}, err
	}
	refreshExpiry := now.Add(s.config.AppRefreshTokenTTL)
	session := appSessionRow{
		ID: uuid.New(), UserID: userID, ClientID: clientID, DeviceID: deviceID,
		DeviceName: deviceName, Scope: scope, LastSeenAt: now, IdleExpiresAt: refreshExpiry,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&session).Error; err != nil {
		return AppTokenResult{}, fmt.Errorf("create app session: %w", err)
	}
	accessExpiry := now.Add(s.config.AppAccessTokenTTL)
	if err := tx.Create(&appAccessTokenRow{
		ID: uuid.New(), SessionID: session.ID, TokenHash: accessDigest,
		ExpiresAt: accessExpiry, CreatedAt: now,
	}).Error; err != nil {
		return AppTokenResult{}, fmt.Errorf("create app access token: %w", err)
	}
	if err := tx.Create(&appRefreshTokenRow{
		ID: uuid.New(), SessionID: session.ID, TokenHash: refreshDigest, Status: "active",
		ExpiresAt: refreshExpiry, CreatedAt: now,
	}).Error; err != nil {
		return AppTokenResult{}, fmt.Errorf("create app refresh token: %w", err)
	}
	return AppTokenResult{
		UserID: userID, SessionID: session.ID, AccessToken: accessPlaintext,
		AccessExpiresIn: int64(s.config.AppAccessTokenTTL.Seconds()),
		RefreshToken:    refreshPlaintext, RefreshExpiresIn: int64(s.config.AppRefreshTokenTTL.Seconds()),
		Scope: scope,
	}, nil
}

func revokeAppDeviceSessions(tx *gorm.DB, userID uuid.UUID, clientID string, deviceID uuid.UUID, now time.Time, reason string) error {
	var sessionIDs []uuid.UUID
	if err := tx.Model(&appSessionRow{}).Where("user_id = ? AND client_id = ? AND device_id = ? AND revoked_at IS NULL", userID, clientID, deviceID).
		Pluck("id", &sessionIDs).Error; err != nil {
		return fmt.Errorf("find prior app device sessions: %w", err)
	}
	for _, sessionID := range sessionIDs {
		if err := revokeAppSession(tx, sessionID, now, reason); err != nil {
			return err
		}
	}
	return nil
}

func revokeAppSession(tx *gorm.DB, sessionID uuid.UUID, now time.Time, reason string) error {
	if err := tx.Model(&appSessionRow{}).Where("id = ? AND revoked_at IS NULL", sessionID).Updates(map[string]any{
		"revoked_at": now, "revoked_reason": reason, "updated_at": now,
	}).Error; err != nil {
		return fmt.Errorf("revoke app session: %w", err)
	}
	if err := tx.Model(&appRefreshTokenRow{}).Where("session_id = ? AND status = 'active'", sessionID).
		Update("status", "revoked").Error; err != nil {
		return fmt.Errorf("revoke app refresh tokens: %w", err)
	}
	return nil
}

func (s *Service) deviceVerificationURLs(userCode string) (string, string, error) {
	base, err := url.Parse(s.config.PublicBaseURL)
	if err != nil {
		return "", "", fmt.Errorf("parse device verification base URL: %w", err)
	}
	base.Path = "/app-login"
	base.RawQuery = ""
	base.Fragment = ""
	verificationURI := base.String()
	query := url.Values{}
	query.Set("code", userCode)
	base.RawQuery = query.Encode()
	return verificationURI, base.String(), nil
}

func newDeviceUserCode() (string, string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", "", fmt.Errorf("generate device user code: %w", err)
	}
	code := make([]byte, len(random))
	for index, value := range random {
		code[index] = alphabet[int(value)%len(alphabet)]
	}
	normalized := string(code)
	return formatDeviceUserCode(normalized), digestDeviceUserCode(normalized), nil
}

func inspectDeviceUserCode(raw string) (string, string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, " ", "")
	if len(normalized) != 8 {
		return "", "", ErrDeviceAuthorizationInvalid
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for _, value := range normalized {
		if !strings.ContainsRune(alphabet, value) {
			return "", "", ErrDeviceAuthorizationInvalid
		}
	}
	return normalized, digestDeviceUserCode(normalized), nil
}

func digestDeviceUserCode(normalized string) string {
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:])
}

func formatDeviceUserCode(normalized string) string {
	if len(normalized) != 8 {
		return normalized
	}
	return normalized[:4] + "-" + normalized[4:]
}

func normalizeDeviceName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 120 {
		return "", errors.New("valid device name is required")
	}
	for _, value := range name {
		if unicode.IsControl(value) {
			return "", errors.New("device name contains control characters")
		}
	}
	return name, nil
}

func browserDeviceAuthorization(row appAuthorizationRequestRow, userCode string) BrowserDeviceAuthorization {
	return BrowserDeviceAuthorization{
		RequestID: row.ID, ClientID: row.ClientID, DeviceID: row.DeviceID,
		DeviceName: row.DeviceName, UserCode: userCode, Scope: row.Scope,
		Status: row.Status, ExpiresAt: row.ExpiresAt,
	}
}

type appAuthorizationRequestRow struct {
	ID                  uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	ClientID            string     `gorm:"column:client_id"`
	DeviceCodeHash      string     `gorm:"column:device_code_hash"`
	UserCodeHash        string     `gorm:"column:user_code_hash"`
	DeviceID            uuid.UUID  `gorm:"column:device_id;type:uuid"`
	DeviceName          string     `gorm:"column:device_name"`
	Scope               string     `gorm:"column:scope"`
	Status              string     `gorm:"column:status"`
	UserID              *uuid.UUID `gorm:"column:user_id;type:uuid"`
	PollIntervalSeconds int        `gorm:"column:poll_interval_seconds"`
	LastPolledAt        *time.Time `gorm:"column:last_polled_at"`
	ApprovedAt          *time.Time `gorm:"column:approved_at"`
	ConsumedAt          *time.Time `gorm:"column:consumed_at"`
	ExpiresAt           time.Time  `gorm:"column:expires_at"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

func (appAuthorizationRequestRow) TableName() string { return "app_authorization_requests" }

type appSessionRow struct {
	ID            uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID        uuid.UUID  `gorm:"column:user_id;type:uuid"`
	ClientID      string     `gorm:"column:client_id"`
	DeviceID      uuid.UUID  `gorm:"column:device_id;type:uuid"`
	DeviceName    string     `gorm:"column:device_name"`
	Scope         string     `gorm:"column:scope"`
	LastSeenAt    time.Time  `gorm:"column:last_seen_at"`
	IdleExpiresAt time.Time  `gorm:"column:idle_expires_at"`
	RevokedAt     *time.Time `gorm:"column:revoked_at"`
	RevokedReason *string    `gorm:"column:revoked_reason"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
}

func (appSessionRow) TableName() string { return "app_sessions" }

type appAccessTokenRow struct {
	ID        uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	SessionID uuid.UUID `gorm:"column:session_id;type:uuid"`
	TokenHash string    `gorm:"column:token_hash"`
	ExpiresAt time.Time `gorm:"column:expires_at"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (appAccessTokenRow) TableName() string { return "app_access_tokens" }

type appRefreshTokenRow struct {
	ID         uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	SessionID  uuid.UUID  `gorm:"column:session_id;type:uuid"`
	TokenHash  string     `gorm:"column:token_hash"`
	Status     string     `gorm:"column:status"`
	ExpiresAt  time.Time  `gorm:"column:expires_at"`
	UsedAt     *time.Time `gorm:"column:used_at"`
	ReplacedBy *uuid.UUID `gorm:"column:replaced_by;type:uuid"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
}

func (appRefreshTokenRow) TableName() string { return "app_refresh_tokens" }
