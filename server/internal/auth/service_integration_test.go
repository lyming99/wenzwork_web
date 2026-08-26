//go:build integration

package auth

import (
	"bytes"
	"context"
	"encoding/base32"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
	"github.com/wenzwork/wenzwork-web/server/internal/mailer"
	"gorm.io/gorm"
)

type recordingMailer struct {
	mu       sync.Mutex
	messages []mailer.Message
}

func (m *recordingMailer) Send(_ context.Context, message mailer.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, message)
	return nil
}

func (m *recordingMailer) lastToken(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.messages) == 0 {
		t.Fatal("no email was recorded")
	}
	for _, field := range strings.Fields(m.messages[len(m.messages)-1].Text) {
		if !strings.HasPrefix(field, "http") {
			continue
		}
		parsed, err := url.Parse(field)
		if err == nil && parsed.Query().Get("token") != "" {
			return parsed.Query().Get("token")
		}
	}
	t.Fatalf("email did not contain a token link: %s", m.messages[len(m.messages)-1].Text)
	return ""
}

func TestServiceRegistrationSessionAndPasswordResetFlow(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })

	recorder := &recordingMailer{}
	config := DefaultServiceConfig()
	config.PasswordParams = Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	service, err := NewService(db, recorder, config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	fixedNow := time.Date(2026, 7, 21, 5, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	email := "auth-integration-" + uuid.NewString() + "@example.test"
	clientIP := uniqueIntegrationClientIP()
	password := "correct horse battery staple"
	newPassword := "new correct horse battery staple"
	var userID uuid.UUID
	t.Cleanup(func() {
		cleanupAuthIntegrationUser(db, userID, email)
		cleanupAuthIntegrationRateLimits(db, email, clientIP)
	})

	registered, err := service.Register(ctx, RegisterInput{Email: strings.ToUpper(email), Password: password, DisplayName: " 集成测试用户 "})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	userID = registered.User.ID
	if !registered.VerificationSent || registered.User.Email != email || registered.User.Status != "pending" {
		t.Fatalf("Register() result = %+v", registered)
	}

	var stored userRow
	if err := db.First(&stored, "id = ?", userID).Error; err != nil {
		t.Fatalf("load registered user: %v", err)
	}
	if stored.PasswordHash == password || !strings.HasPrefix(stored.PasswordHash, "$argon2id$") {
		t.Fatal("database contains a plaintext or non-Argon2 password")
	}
	verificationToken := recorder.lastToken(t)
	verificationDigest, _ := DigestOpaqueToken(verificationToken)
	var verificationRow emailVerificationTokenRow
	if err := db.First(&verificationRow, "user_id = ?", userID).Error; err != nil {
		t.Fatalf("load verification token: %v", err)
	}
	if verificationRow.TokenHash != verificationDigest || verificationRow.TokenHash == verificationToken {
		t.Fatal("verification token was not stored as a digest")
	}

	if _, err := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP}); !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("Login(before verification) error = %v", err)
	}
	verified, err := service.VerifyEmail(ctx, verificationToken)
	if err != nil || verified.EmailVerifiedAt == nil || verified.Status != "active" {
		t.Fatalf("VerifyEmail() = %+v, %v", verified, err)
	}
	if _, err := service.VerifyEmail(ctx, verificationToken); !errors.Is(err, ErrVerificationToken) {
		t.Fatalf("second VerifyEmail() error = %v", err)
	}

	login, err := service.Login(ctx, LoginInput{
		Email: email, Password: password, RememberMe: true, UserAgent: "Integration Browser\r\nInjected", ClientIP: clientIP,
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if login.Session.Token == "" || login.Session.CSRFToken == "" || !login.Session.RememberMe {
		t.Fatalf("Login() session = %+v", login.Session)
	}
	if login.Session.AbsoluteExpiresAt.Sub(fixedNow) != 30*24*time.Hour {
		t.Fatalf("remember expiry = %s", login.Session.AbsoluteExpiresAt)
	}
	var storedSession sessionRow
	if err := db.First(&storedSession, "id = ?", login.Session.ID).Error; err != nil {
		t.Fatalf("load auth session: %v", err)
	}
	if storedSession.TokenHash == login.Session.Token || storedSession.CSRFTokenHash == login.Session.CSRFToken {
		t.Fatal("database contains plaintext session or CSRF token")
	}
	if strings.ContainsAny(storedSession.UserAgentSummary, "\r\n") {
		t.Fatal("user agent summary contains control characters")
	}

	authenticated, err := service.AuthenticateSession(ctx, login.Session.Token)
	if err != nil || authenticated.User.ID != userID {
		t.Fatalf("AuthenticateSession() = %+v, %v", authenticated, err)
	}
	if !VerifySessionCSRF(authenticated, login.Session.CSRFToken) || VerifySessionCSRF(authenticated, login.Session.Token) {
		t.Fatal("session CSRF verification accepted the wrong token or rejected the right one")
	}

	if err := service.RequestPasswordReset(ctx, email); err != nil {
		t.Fatalf("RequestPasswordReset() error = %v", err)
	}
	resetToken := recorder.lastToken(t)
	if err := service.ResetPassword(ctx, resetToken, newPassword); err != nil {
		t.Fatalf("ResetPassword() error = %v", err)
	}
	if err := service.ResetPassword(ctx, resetToken, newPassword); !errors.Is(err, ErrPasswordResetToken) {
		t.Fatalf("second ResetPassword() error = %v", err)
	}
	if _, err := service.AuthenticateSession(ctx, login.Session.Token); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("old session after reset error = %v", err)
	}
	if _, err := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("old password Login() error = %v", err)
	}
	newLogin, err := service.Login(ctx, LoginInput{Email: email, Password: newPassword, ClientIP: clientIP})
	if err != nil {
		t.Fatalf("new password Login() error = %v", err)
	}
	if err := service.Logout(ctx, newLogin.Session.Token); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := service.AuthenticateSession(ctx, newLogin.Session.Token); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("logged-out session error = %v", err)
	}

	duplicate, err := service.Register(ctx, RegisterInput{Email: email, Password: password, DisplayName: "Duplicate"})
	if err != nil || !duplicate.AlreadyRegistered {
		t.Fatalf("duplicate Register() = %+v, %v", duplicate, err)
	}
}

func TestServiceLoginRateLimitUsesDigestsAndRecoversAfterBlock(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	recorder := &recordingMailer{}
	config := DefaultServiceConfig()
	config.PasswordParams = Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	service, err := NewService(db, recorder, config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	fixedNow := time.Date(2026, 7, 21, 7, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixedNow }

	email := "auth-rate-" + uuid.NewString() + "@example.test"
	clientIP := uniqueIntegrationClientIP()
	password := "correct rate limit password"
	registered, err := service.Register(ctx, RegisterInput{Email: email, Password: password, DisplayName: "Rate User"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	t.Cleanup(func() {
		cleanupAuthIntegrationUser(db, registered.User.ID, email)
		cleanupAuthIntegrationRateLimits(db, email, clientIP)
	})
	if _, err := service.VerifyEmail(ctx, recorder.lastToken(t)); err != nil {
		t.Fatalf("VerifyEmail() error = %v", err)
	}

	for attempt := 1; attempt <= loginAttemptLimit; attempt++ {
		_, err := service.Login(ctx, LoginInput{Email: email, Password: "wrong password value", ClientIP: clientIP})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d error = %v, want ErrInvalidCredentials", attempt, err)
		}
	}
	if _, err := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("blocked Login() error = %v, want ErrRateLimited", err)
	}

	var limits []authRateLimitRow
	if err := db.Find(&limits, "key_digest IN ?", []string{rateKeyDigest("login_email", email), rateKeyDigest("login_ip", clientIP)}).Error; err != nil {
		t.Fatalf("load auth rate limits: %v", err)
	}
	if len(limits) != 2 {
		t.Fatalf("rate limit row count = %d, want 2", len(limits))
	}
	for _, limit := range limits {
		if strings.Contains(limit.KeyDigest, email) || strings.Contains(limit.KeyDigest, clientIP) || len(limit.KeyDigest) != 64 {
			t.Fatalf("rate limit key leaks identifier: %q", limit.KeyDigest)
		}
	}

	service.now = func() time.Time { return fixedNow.Add(loginBlockBase + time.Minute) }
	if _, err := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP}); err != nil {
		t.Fatalf("Login() after block expiry error = %v", err)
	}
}

func TestAppDeviceAuthorizationTokenRotationAndReplayRevocation(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	recorder := &recordingMailer{}
	config := DefaultServiceConfig()
	config.PasswordParams = Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	service, err := NewService(db, recorder, config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	currentTime := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return currentTime }

	email := "app-auth-integration-" + uuid.NewString() + "@example.test"
	clientIP := uniqueIntegrationClientIP()
	deviceID := uuid.New()
	password := "correct app authorization password"
	registered, err := service.Register(ctx, RegisterInput{Email: email, Password: password, DisplayName: "App User"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	t.Cleanup(func() {
		cleanupAuthIntegrationUser(db, registered.User.ID, email)
		cleanupAuthIntegrationRateLimits(db, email, clientIP)
		_ = db.Delete(&authRateLimitRow{}, "scope = ? AND key_digest = ?", "device_authorization_ip", rateKeyDigest("device_authorization_ip", clientIP)).Error
	})
	if _, err := service.VerifyEmail(ctx, recorder.lastToken(t)); err != nil {
		t.Fatalf("VerifyEmail() error = %v", err)
	}

	created, err := service.CreateDeviceAuthorization(ctx, CreateDeviceAuthorizationInput{
		ClientID: DesktopClientID, DeviceID: deviceID.String(), DeviceName: "DESKTOP-INTEGRATION", ClientIP: clientIP,
	})
	if err != nil {
		t.Fatalf("CreateDeviceAuthorization() error = %v", err)
	}
	if created.ExpiresIn != 600 || created.Interval != 5 || strings.Contains(created.VerificationURIComplete, created.DeviceCode) {
		t.Fatalf("created device authorization = %+v", created)
	}
	var storedRequest appAuthorizationRequestRow
	if err := db.First(&storedRequest, "id = ?", created.RequestID).Error; err != nil {
		t.Fatalf("load device authorization row: %v", err)
	}
	if storedRequest.DeviceCodeHash == created.DeviceCode || storedRequest.UserCodeHash == created.UserCode {
		t.Fatal("database contains a plaintext device or user code")
	}
	if _, err := service.ExchangeDeviceCode(ctx, DesktopClientID, created.DeviceCode); !errors.Is(err, ErrDeviceAuthorizationPending) {
		t.Fatalf("pending ExchangeDeviceCode() error = %v", err)
	}
	approved, err := service.ApproveDeviceAuthorization(ctx, created.UserCode, registered.User.ID)
	if err != nil || approved.Status != "approved" {
		t.Fatalf("ApproveDeviceAuthorization() = %+v, %v", approved, err)
	}
	currentTime = currentTime.Add(5 * time.Second)
	issued, err := service.ExchangeDeviceCode(ctx, DesktopClientID, created.DeviceCode)
	if err != nil {
		t.Fatalf("approved ExchangeDeviceCode() error = %v", err)
	}
	if issued.AccessToken == "" || issued.RefreshToken == "" || issued.RefreshExpiresIn != int64((30*24*time.Hour).Seconds()) {
		t.Fatalf("issued app token = %+v", issued)
	}
	if _, err := service.ExchangeDeviceCode(ctx, DesktopClientID, created.DeviceCode); !errors.Is(err, ErrDeviceAuthorizationExpired) {
		t.Fatalf("replayed device code error = %v", err)
	}
	authenticated, err := service.AuthenticateAppAccessToken(ctx, issued.AccessToken)
	if err != nil || authenticated.User.ID != registered.User.ID || authenticated.DeviceID != deviceID {
		t.Fatalf("AuthenticateAppAccessToken() = %+v, %v", authenticated, err)
	}
	var storedAccess appAccessTokenRow
	if err := db.First(&storedAccess, "session_id = ?", issued.SessionID).Error; err != nil {
		t.Fatalf("load app access token: %v", err)
	}
	if storedAccess.TokenHash == issued.AccessToken {
		t.Fatal("database contains a plaintext app access token")
	}

	currentTime = currentTime.Add(time.Hour)
	refreshed, err := service.RefreshAppToken(ctx, DesktopClientID, issued.RefreshToken)
	if err != nil || refreshed.RefreshToken == issued.RefreshToken {
		t.Fatalf("RefreshAppToken() = %+v, %v", refreshed, err)
	}
	if _, err := service.AuthenticateAppAccessToken(ctx, refreshed.AccessToken); err != nil {
		t.Fatalf("refreshed access token error = %v", err)
	}
	if _, err := service.RefreshAppToken(ctx, DesktopClientID, issued.RefreshToken); !errors.Is(err, ErrAppRefreshReplay) {
		t.Fatalf("reused refresh token error = %v", err)
	}
	if _, err := service.AuthenticateAppAccessToken(ctx, refreshed.AccessToken); !errors.Is(err, ErrAppTokenInvalid) {
		t.Fatalf("access token after refresh replay error = %v", err)
	}
}

func TestServiceMFAEncryptionRotationRecoveryAndReplayProtection(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	recorder := &recordingMailer{}
	config := DefaultServiceConfig()
	config.PasswordParams = Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	config.MFAEncryptionKey = "integration-MFA-key-that-is-never-production"
	service, err := NewService(db, recorder, config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	currentNow := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return currentNow }

	email := "auth-mfa-" + uuid.NewString() + "@example.test"
	clientIP := uniqueIntegrationClientIP()
	password := "correct MFA integration password"
	registered, err := service.Register(ctx, RegisterInput{Email: email, Password: password, DisplayName: "MFA User"})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	t.Cleanup(func() {
		cleanupAuthIntegrationUser(db, registered.User.ID, email)
		cleanupAuthIntegrationRateLimits(db, email, clientIP)
	})
	if _, err := service.VerifyEmail(ctx, recorder.lastToken(t)); err != nil {
		t.Fatalf("VerifyEmail() error = %v", err)
	}
	login, err := service.Login(ctx, LoginInput{Email: email, Password: password, UserAgent: "MFA Integration Browser", ClientIP: clientIP})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	authenticated, err := service.AuthenticateSession(ctx, login.Session.Token)
	if err != nil {
		t.Fatalf("AuthenticateSession() error = %v", err)
	}

	if _, err := service.BeginTOTPEnrollment(ctx, registered.User.ID, "wrong current password"); !errors.Is(err, ErrCurrentPassword) {
		t.Fatalf("BeginTOTPEnrollment(wrong password) error = %v", err)
	}
	enrollment, err := service.BeginTOTPEnrollment(ctx, registered.User.ID, password)
	if err != nil {
		t.Fatalf("BeginTOTPEnrollment() error = %v", err)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret)
	if err != nil || len(secret) != totpSecretBytes {
		t.Fatalf("TOTP secret = %q, %v", enrollment.Secret, err)
	}
	var credential totpCredentialRow
	if err := db.First(&credential, "user_id = ?", registered.User.ID).Error; err != nil {
		t.Fatalf("load MFA credential: %v", err)
	}
	if bytes.Contains(credential.SecretCiphertext, secret) || bytes.Contains(credential.SecretCiphertext, []byte(enrollment.Secret)) {
		t.Fatal("database contains the plaintext TOTP secret")
	}

	initialCode := hotp(secret, uint64(currentNow.Unix()/totpPeriodSeconds), 6)
	confirmation, err := service.ConfirmTOTPEnrollment(ctx, authenticated, initialCode)
	if err != nil {
		t.Fatalf("ConfirmTOTPEnrollment() error = %v", err)
	}
	if confirmation.Session.AssuranceLevel != 2 || len(confirmation.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("MFA confirmation = %+v", confirmation)
	}
	if _, err := service.AuthenticateSession(ctx, login.Session.Token); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("pre-MFA session after rotation error = %v", err)
	}
	rotatedAuth, err := service.AuthenticateSession(ctx, confirmation.Session.Token)
	if err != nil || rotatedAuth.AssuranceLevel != 2 {
		t.Fatalf("rotated MFA session = %+v, %v", rotatedAuth, err)
	}

	currentNow = currentNow.Add(time.Duration(totpPeriodSeconds) * time.Second)
	replayLogin, err := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP})
	if err != nil {
		t.Fatalf("Login(for replay) error = %v", err)
	}
	replayAuth, _ := service.AuthenticateSession(ctx, replayLogin.Session.Token)
	nextCode := hotp(secret, uint64(currentNow.Unix()/totpPeriodSeconds), 6)
	verifiedSession, err := service.VerifyMFA(ctx, replayAuth, nextCode)
	if err != nil || verifiedSession.AssuranceLevel != 2 {
		t.Fatalf("VerifyMFA() = %+v, %v", verifiedSession, err)
	}
	secondLogin, _ := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP})
	secondAuth, _ := service.AuthenticateSession(ctx, secondLogin.Session.Token)
	if _, err := service.VerifyMFA(ctx, secondAuth, nextCode); !errors.Is(err, ErrMFAReplay) {
		t.Fatalf("replayed VerifyMFA() error = %v", err)
	}

	currentNow = currentNow.Add(time.Duration(totpPeriodSeconds) * time.Second)
	concurrentCode := hotp(secret, uint64(currentNow.Unix()/totpPeriodSeconds), 6)
	loginA, _ := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP})
	loginB, _ := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP})
	authA, _ := service.AuthenticateSession(ctx, loginA.Session.Token)
	authB, _ := service.AuthenticateSession(ctx, loginB.Session.Token)
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, candidate := range []AuthenticatedSession{authA, authB} {
		wait.Add(1)
		go func(session AuthenticatedSession) {
			defer wait.Done()
			_, verifyErr := service.VerifyMFA(ctx, session, concurrentCode)
			results <- verifyErr
		}(candidate)
	}
	wait.Wait()
	close(results)
	var successes, replays int
	for result := range results {
		if result == nil {
			successes++
		} else if errors.Is(result, ErrMFAReplay) {
			replays++
		} else {
			t.Fatalf("concurrent VerifyMFA() error = %v", result)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("concurrent MFA results successes=%d replays=%d", successes, replays)
	}

	recoveryLogin, _ := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP})
	recoveryAuth, _ := service.AuthenticateSession(ctx, recoveryLogin.Session.Token)
	recoverySession, err := service.VerifyMFA(ctx, recoveryAuth, confirmation.RecoveryCodes[0])
	if err != nil {
		t.Fatalf("VerifyMFA(recovery) error = %v", err)
	}
	usedRecoveryLogin, _ := service.Login(ctx, LoginInput{Email: email, Password: password, ClientIP: clientIP})
	usedRecoveryAuth, _ := service.AuthenticateSession(ctx, usedRecoveryLogin.Session.Token)
	if _, err := service.VerifyMFA(ctx, usedRecoveryAuth, confirmation.RecoveryCodes[0]); !errors.Is(err, ErrMFAInvalidCode) {
		t.Fatalf("reused recovery code error = %v", err)
	}

	recoveryAuthenticated, err := service.AuthenticateSession(ctx, recoverySession.Token)
	if err != nil {
		t.Fatalf("AuthenticateSession(recovery rotation) error = %v", err)
	}
	newRecoveryCodes, err := service.RegenerateRecoveryCodes(ctx, recoveryAuthenticated, password)
	if err != nil || len(newRecoveryCodes) != recoveryCodeCount {
		t.Fatalf("RegenerateRecoveryCodes() = %d codes, %v", len(newRecoveryCodes), err)
	}

	currentNow = currentNow.Add(time.Duration(totpPeriodSeconds) * time.Second)
	disableCode := hotp(secret, uint64(currentNow.Unix()/totpPeriodSeconds), 6)
	if err := service.DisableTOTP(ctx, recoveryAuthenticated, password, disableCode); err != nil {
		t.Fatalf("DisableTOTP() error = %v", err)
	}
	status, err := service.GetMFAStatus(ctx, registered.User.ID)
	if err != nil || status.Enrolled || status.RecoveryCodesRemaining != 0 {
		t.Fatalf("GetMFAStatus(after disable) = %+v, %v", status, err)
	}
	downgraded, err := service.AuthenticateSession(ctx, recoverySession.Token)
	if err != nil || downgraded.AssuranceLevel != 1 {
		t.Fatalf("session after MFA disable = %+v, %v", downgraded, err)
	}
}

func TestBootstrapSuperAdminIsSingleUseIdempotentAndAudited(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	var existing int64
	if err := db.Table("user_roles ur").Joins("JOIN roles r ON r.id = ur.role_id").
		Where("r.code = 'super_admin'").Count(&existing).Error; err != nil {
		t.Fatalf("count existing super administrators: %v", err)
	}
	if existing > 0 {
		t.Skip("integration database already contains a super administrator")
	}
	if email, initialized, err := SuperAdminEmail(ctx, db); err != nil || initialized || email != "" {
		t.Fatalf("SuperAdminEmail(before bootstrap) = %q, %v, %v", email, initialized, err)
	}
	email := "bootstrap-" + uuid.NewString() + "@example.test"
	password := "bootstrap administrator password"
	params := Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	result, err := BootstrapSuperAdmin(ctx, db, email, password, "Bootstrap Admin", params)
	if err != nil || !result.Created || result.User.Status != "active" {
		t.Fatalf("BootstrapSuperAdmin() = %+v, %v", result, err)
	}
	if statusEmail, initialized, err := SuperAdminEmail(ctx, db); err != nil || !initialized || statusEmail != email {
		t.Fatalf("SuperAdminEmail(after bootstrap) = %q, %v, %v", statusEmail, initialized, err)
	}
	t.Cleanup(func() { cleanupAuthIntegrationUser(db, result.User.ID, email) })
	var row userRow
	if err := db.First(&row, "id = ?", result.User.ID).Error; err != nil {
		t.Fatalf("load bootstrap user: %v", err)
	}
	valid, err := VerifyPassword(row.PasswordHash, password)
	if err != nil || !valid || row.EmailVerifiedAt == nil {
		t.Fatalf("bootstrap password/verification state valid=%v error=%v", valid, err)
	}
	var roleCount int64
	if err := db.Table("user_roles ur").Joins("JOIN roles r ON r.id = ur.role_id").
		Where("ur.user_id = ? AND r.code IN ('user', 'super_admin')", result.User.ID).Count(&roleCount).Error; err != nil || roleCount != 2 {
		t.Fatalf("bootstrap role count = %d, %v", roleCount, err)
	}
	var auditCount int64
	if err := db.Table("audit_logs").Where("actor_user_id = ? AND action = 'admin.bootstrap'", result.User.ID).Count(&auditCount).Error; err != nil || auditCount != 1 {
		t.Fatalf("bootstrap audit count = %d, %v", auditCount, err)
	}
	repeated, err := BootstrapSuperAdmin(ctx, db, email, password, "Bootstrap Admin", params)
	if err != nil || repeated.Created || repeated.User.ID != result.User.ID {
		t.Fatalf("repeated BootstrapSuperAdmin() = %+v, %v", repeated, err)
	}
	otherEmail := "bootstrap-other-" + uuid.NewString() + "@example.test"
	if _, err := BootstrapSuperAdmin(ctx, db, otherEmail, password, "Other Admin", params); !errors.Is(err, ErrBootstrapAlreadyComplete) {
		t.Fatalf("second BootstrapSuperAdmin() error = %v", err)
	}
}

func TestServiceAdminCreatesListsDisablesAndReenablesUser(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	config := DefaultServiceConfig()
	config.PasswordParams = Argon2Params{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	service, err := NewService(db, &recordingMailer{}, config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	actorID := uuid.New()
	actorEmail := "user-admin-" + actorID.String() + "@example.test"
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-test-hash', 'User Admin', 'active', ?)
	`, actorID, actorEmail, now).Error; err != nil {
		t.Fatalf("insert admin actor: %v", err)
	}
	createdEmail := "admin-created-" + uuid.NewString() + "@example.test"
	var createdID uuid.UUID
	t.Cleanup(func() {
		cleanupAuthIntegrationUser(db, actorID, actorEmail)
		cleanupAuthIntegrationUser(db, createdID, createdEmail)
	})

	password := "admin supplied initial password"
	created, err := service.CreateAdminUser(ctx, AdminCreateUserInput{
		Email: strings.ToUpper(createdEmail), Password: password, DisplayName: " Admin Created ", ActorUserID: actorID,
	})
	if err != nil {
		t.Fatalf("CreateAdminUser() error = %v", err)
	}
	createdID = created.ID
	if created.Email != createdEmail || created.DisplayName != "Admin Created" || created.Status != "active" || created.EmailVerifiedAt == nil {
		t.Fatalf("CreateAdminUser() = %+v", created)
	}
	var stored userRow
	if err := db.First(&stored, "id = ?", createdID).Error; err != nil {
		t.Fatalf("load admin-created user: %v", err)
	}
	valid, err := VerifyPassword(stored.PasswordHash, password)
	if err != nil || !valid {
		t.Fatalf("VerifyPassword(admin-created user) = %v, %v", valid, err)
	}
	listed, err := service.ListAdminUsers(ctx, AdminUserListFilter{Query: createdEmail, Limit: 20})
	if err != nil || listed.Total != 1 || len(listed.Items) != 1 || listed.Items[0].ID != createdID || listed.Items[0].Membership != nil {
		t.Fatalf("ListAdminUsers() = %+v, %v", listed, err)
	}

	sessionID := uuid.New()
	if err := db.Exec(`
		INSERT INTO sessions (
			id, user_id, token_hash, csrf_token_hash, user_agent_summary, assurance_level,
			last_seen_at, idle_expires_at, absolute_expires_at, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'Integration browser', 1, ?, ?, ?, ?, ?)
	`, sessionID, createdID, strings.Repeat("a", 64), strings.Repeat("b", 64), now, now.Add(time.Hour), now.Add(2*time.Hour), now, now).Error; err != nil {
		t.Fatalf("insert user session: %v", err)
	}
	disabled, err := service.SetAdminUserStatus(ctx, createdID, actorID, "disabled")
	if err != nil || disabled.Status != "disabled" {
		t.Fatalf("SetAdminUserStatus(disabled) = %+v, %v", disabled, err)
	}
	var revoked sessionRow
	if err := db.First(&revoked, "id = ?", sessionID).Error; err != nil || revoked.RevokedAt == nil || revoked.RevokedReason == nil || *revoked.RevokedReason != "account_disabled" {
		t.Fatalf("disabled session = %+v, %v", revoked, err)
	}
	reenabled, err := service.SetAdminUserStatus(ctx, createdID, actorID, "active")
	if err != nil || reenabled.Status != "active" {
		t.Fatalf("SetAdminUserStatus(active) = %+v, %v", reenabled, err)
	}
	var auditCount int64
	if err := db.Table("audit_logs").Where("actor_user_id = ? AND resource_id = ? AND action IN ?", actorID, createdID, []string{"user.create", "user.status.update"}).Count(&auditCount).Error; err != nil || auditCount != 3 {
		t.Fatalf("admin user audit count = %d, %v", auditCount, err)
	}
}

func cleanupAuthIntegrationUser(db *gorm.DB, userID uuid.UUID, email string) {
	if userID == uuid.Nil {
		var row userRow
		if err := db.Where("email = ?", email).First(&row).Error; err == nil {
			userID = row.ID
		}
	}
	if userID == uuid.Nil {
		return
	}
	_ = db.Exec("DELETE FROM audit_logs WHERE actor_user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM membership_events WHERE user_id = ? OR actor_user_id = ?", userID, userID).Error
	_ = db.Exec("DELETE FROM redemption_codes WHERE redeemed_by = ?", userID).Error
	_ = db.Exec("DELETE FROM redemption_code_batches WHERE created_by = ?", userID).Error
	_ = db.Exec("DELETE FROM memberships WHERE user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM sessions WHERE user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM email_verification_tokens WHERE user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM password_reset_tokens WHERE user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM mfa_recovery_codes WHERE user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM mfa_totp_credentials WHERE user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM user_roles WHERE user_id = ?", userID).Error
	_ = db.Exec("DELETE FROM users WHERE id = ?", userID).Error
}

func uniqueIntegrationClientIP() string {
	value := strings.ReplaceAll(uuid.NewString(), "-", "")
	return strings.Join([]string{
		"2001", "db8",
		value[0:4], value[4:8], value[8:12],
		value[12:16], value[16:20], value[20:24],
	}, ":")
}

func cleanupAuthIntegrationRateLimits(db *gorm.DB, email, clientIP string) {
	_ = db.Delete(&authRateLimitRow{}, "scope = 'login_email' AND key_digest = ?", rateKeyDigest("login_email", email)).Error
	_ = db.Delete(&authRateLimitRow{}, "scope = 'login_ip' AND key_digest = ?", rateKeyDigest("login_ip", clientIP)).Error
}
