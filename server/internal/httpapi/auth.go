package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/analytics"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

const (
	authSessionContextKey             = "auth_session"
	authenticationSourceContextKey    = "authentication_source"
	authenticationSourceBrowserCookie = "browser_cookie"
	authenticationSourceAppBearer     = "app_bearer"
)

type AuthService interface {
	Register(context.Context, auth.RegisterInput) (auth.RegisterResult, error)
	ResendVerification(context.Context, string) error
	VerifyEmail(context.Context, string) (auth.User, error)
	Login(context.Context, auth.LoginInput) (auth.LoginResult, error)
	AuthenticateSession(context.Context, string) (auth.AuthenticatedSession, error)
	Logout(context.Context, string) error
	ListSessions(context.Context, uuid.UUID, uuid.UUID) ([]auth.SessionSummary, error)
	RevokeSession(context.Context, uuid.UUID, uuid.UUID) error
	UpdateProfile(context.Context, uuid.UUID, string) (auth.User, error)
	RequestPasswordReset(context.Context, string) error
	ResetPassword(context.Context, string, string) error
	ChangePassword(context.Context, uuid.UUID, uuid.UUID, string, string, bool) error
	GetMFAStatus(context.Context, uuid.UUID) (auth.MFAStatus, error)
	BeginTOTPEnrollment(context.Context, uuid.UUID, string) (auth.MFAEnrollment, error)
	ConfirmTOTPEnrollment(context.Context, auth.AuthenticatedSession, string) (auth.MFAConfirmation, error)
	VerifyMFA(context.Context, auth.AuthenticatedSession, string) (auth.Session, error)
	RegenerateRecoveryCodes(context.Context, auth.AuthenticatedSession, string) ([]string, error)
	DisableTOTP(context.Context, auth.AuthenticatedSession, string, string) error
}

type AuthHTTPConfig struct {
	CookieSecure    bool
	AllowedOrigins  []string
	DisableAdminMFA bool
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

type emailRequest struct {
	Email string `json:"email"`
}

type tokenRequest struct {
	Token string `json:"token"`
}

type loginRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	RememberMe bool   `json:"rememberMe"`
}

type resetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

type updateProfileRequest struct {
	DisplayName string `json:"displayName"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	RevokeOthers    bool   `json:"revokeOthers"`
}

type currentPasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
}

type mfaCodeRequest struct {
	Code string `json:"code"`
}

type disableTOTPRequest struct {
	CurrentPassword string `json:"currentPassword"`
	Code            string `json:"code"`
}

func registerAuthRoutes(group *gin.RouterGroup, service AuthService, appAuth AppAuthService, accountEvents AccountEventRecorder, setup SystemSetupService, config AuthHTTPConfig, log *slog.Logger) {
	authGroup := group.Group("/auth")
	authGroup.Use(requireAllowedOrigin(config.AllowedOrigins))

	authGroup.POST("/register", func(c *gin.Context) {
		if !authServiceAvailable(c, service) {
			return
		}
		var request registerRequest
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.Register(c.Request.Context(), auth.RegisterInput{
			Email: request.Email, Password: request.Password, DisplayName: request.DisplayName,
		})
		switch {
		case errors.Is(err, auth.ErrRegistrationDisabled):
			writeProblem(c, http.StatusForbidden, "registration_disabled", "暂未开放注册", "请稍后再试。")
		case errors.Is(err, auth.ErrInvalidPassword):
			writeProblem(c, http.StatusBadRequest, "invalid_password", "密码不符合要求", "密码需包含 8 到 128 个字符。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "invalid_registration", "无法提交注册", "请检查邮箱、显示名称和密码。")
		default:
			if accountEvents != nil && !result.AlreadyRegistered && result.User.ID != uuid.Nil {
				if err := accountEvents.RecordRegistration(c.Request.Context(), analytics.RegistrationEventInput{
					UserID: result.User.ID, ClientIP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent"),
				}); err != nil {
					log.Warn("registration event recording failed", "request_id", requestIDFrom(c), "error", err)
				}
			}
			if !result.VerificationSent && !result.AlreadyRegistered {
				log.Warn("verification email delivery failed", "request_id", requestIDFrom(c), "user_id", result.User.ID)
			}
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusAccepted, gin.H{
				"status":  "verification_required",
				"message": "如果该邮箱可用于注册，我们会发送验证邮件。",
			})
		}
	})

	authGroup.POST("/resend-verification", func(c *gin.Context) {
		if !authServiceAvailable(c, service) {
			return
		}
		var request emailRequest
		if !decodeJSON(c, &request) {
			return
		}
		if err := service.ResendVerification(c.Request.Context(), request.Email); err != nil {
			log.Warn("verification resend failed", "request_id", requestIDFrom(c), "error", err)
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusAccepted, gin.H{"message": "如果账户需要验证，我们会发送新邮件。"})
	})

	authGroup.POST("/verify-email", func(c *gin.Context) {
		if !authServiceAvailable(c, service) {
			return
		}
		var request tokenRequest
		if !decodeJSON(c, &request) {
			return
		}
		user, err := service.VerifyEmail(c.Request.Context(), request.Token)
		if errors.Is(err, auth.ErrVerificationToken) {
			writeProblem(c, http.StatusBadRequest, "verification_token_invalid", "验证链接无效", "链接可能已过期或使用。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法验证邮箱", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"user": user})
	})

	authGroup.POST("/login", func(c *gin.Context) {
		if !authServiceAvailable(c, service) {
			return
		}
		if !secureCookieTransportAvailable(c, config) {
			return
		}
		var request loginRequest
		if !decodeJSON(c, &request) {
			return
		}
		clientIP := c.ClientIP()
		userAgent := c.GetHeader("User-Agent")
		result, err := service.Login(c.Request.Context(), auth.LoginInput{
			Email: request.Email, Password: request.Password, RememberMe: request.RememberMe,
			UserAgent: userAgent, ClientIP: clientIP,
		})
		switch {
		case errors.Is(err, auth.ErrRateLimited):
			c.Header("Retry-After", "300")
			writeProblem(c, http.StatusTooManyRequests, "login_rate_limited", "尝试次数过多", "请稍后再试。")
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeProblem(c, http.StatusUnauthorized, "invalid_credentials", "邮箱或密码错误", "请检查后重试。")
		case errors.Is(err, auth.ErrEmailNotVerified):
			writeProblem(c, http.StatusForbidden, "email_not_verified", "邮箱尚未验证", "请先打开验证邮件。")
		case errors.Is(err, auth.ErrAccountUnavailable):
			writeProblem(c, http.StatusForbidden, "account_unavailable", "账户暂不可用", "如需帮助，请联系支持渠道。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法登录", "请稍后重试。")
		default:
			if accountEvents != nil {
				if err := accountEvents.RecordLogin(c.Request.Context(), analytics.LoginEventInput{
					UserID: result.Session.User.ID, WebSessionID: result.Session.ID,
					LoginMethod: analytics.LoginMethodPassword, ClientIP: clientIP, UserAgent: userAgent,
				}); err != nil {
					log.Warn("account login event recording failed", "request_id", requestIDFrom(c), "user_id", result.Session.User.ID, "error", err)
				}
			}
			setAuthCookies(c, config, result.Session)
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, gin.H{
				"user":                result.Session.User,
				"permissions":         auth.PermissionsForRoles(result.Session.User.Roles),
				"mfaRequired":         result.MFARequired,
				"mfaEnrolled":         result.MFAEnrolled,
				"mfaEnforced":         !config.DisableAdminMFA,
				"assuranceLevel":      result.Session.AssuranceLevel,
				"absoluteExpiresAt":   result.Session.AbsoluteExpiresAt,
				"systemSetupRequired": systemSetupRequired(setup),
			})
		}
	})

	authGroup.POST("/forgot-password", func(c *gin.Context) {
		if !authServiceAvailable(c, service) {
			return
		}
		var request emailRequest
		if !decodeJSON(c, &request) {
			return
		}
		if err := service.RequestPasswordReset(c.Request.Context(), request.Email); err != nil {
			log.Warn("password reset request failed", "request_id", requestIDFrom(c), "error", err)
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusAccepted, gin.H{"message": "如果账户符合条件，我们会发送重置邮件。"})
	})

	authGroup.POST("/reset-password", func(c *gin.Context) {
		if !authServiceAvailable(c, service) {
			return
		}
		var request resetPasswordRequest
		if !decodeJSON(c, &request) {
			return
		}
		err := service.ResetPassword(c.Request.Context(), request.Token, request.NewPassword)
		switch {
		case errors.Is(err, auth.ErrPasswordResetToken):
			writeProblem(c, http.StatusBadRequest, "reset_token_invalid", "重置链接无效", "链接可能已过期或使用。")
		case errors.Is(err, auth.ErrInvalidPassword):
			writeProblem(c, http.StatusBadRequest, "invalid_password", "密码不符合要求", "密码需包含 8 到 128 个字符。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法重置密码", "请稍后重试。")
		default:
			clearAuthCookies(c, config)
			c.Status(http.StatusNoContent)
		}
	})

	protectedAuth := authGroup.Group("")
	protectedAuth.Use(requireSession(service, config), requireCSRF(config))
	protectedAuth.POST("/mfa/totp/verify", func(c *gin.Context) {
		var request mfaCodeRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		rotated, err := service.VerifyMFA(c.Request.Context(), session, request.Code)
		if writeMFAError(c, err) {
			return
		}
		setAuthCookies(c, config, rotated)
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"user": rotated.User, "permissions": auth.PermissionsForRoles(rotated.User.Roles),
			"mfaEnforced": !config.DisableAdminMFA, "assuranceLevel": rotated.AssuranceLevel,
			"absoluteExpiresAt": rotated.AbsoluteExpiresAt, "systemSetupRequired": systemSetupRequired(setup),
		})
	})
	protectedAuth.POST("/logout", func(c *gin.Context) {
		token, _ := c.Cookie(sessionCookieName(config.CookieSecure))
		if err := service.Logout(c.Request.Context(), token); err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法退出", "请稍后重试。")
			return
		}
		clearAuthCookies(c, config)
		c.Status(http.StatusNoContent)
	})

	group.GET("/me", requireAccountAuthentication(service, appAuth, config, "profile.read"), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"user":                session.User,
			"permissions":         auth.PermissionsForRoles(session.User.Roles),
			"mfaEnforced":         !config.DisableAdminMFA,
			"assuranceLevel":      session.AssuranceLevel,
			"absoluteExpiresAt":   session.AbsoluteExpiresAt,
			"systemSetupRequired": systemSetupRequired(setup),
		})
	})

	me := group.Group("/me")
	me.Use(requireSession(service, config))
	me.PATCH("", requireCSRF(config), func(c *gin.Context) {
		var request updateProfileRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		user, err := service.UpdateProfile(c.Request.Context(), session.User.ID, request.DisplayName)
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "invalid_profile", "无法更新资料", "请检查显示名称。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"user": user})
	})
	me.PATCH("/password", requireCSRF(config), func(c *gin.Context) {
		var request changePasswordRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		err := service.ChangePassword(c.Request.Context(), session.User.ID, session.ID, request.CurrentPassword, request.NewPassword, request.RevokeOthers)
		switch {
		case errors.Is(err, auth.ErrCurrentPassword):
			writeProblem(c, http.StatusBadRequest, "current_password_invalid", "当前密码错误", "请重新输入。")
		case errors.Is(err, auth.ErrInvalidPassword):
			writeProblem(c, http.StatusBadRequest, "invalid_password", "新密码不符合要求", "密码需包含 8 到 128 个字符。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法修改密码", "请稍后重试。")
		default:
			c.Status(http.StatusNoContent)
		}
	})
	me.GET("/mfa", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		status, err := service.GetMFAStatus(c.Request.Context(), session.User.ID)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法读取二次验证状态", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, status)
	})
	me.POST("/mfa/totp", requireCSRF(config), func(c *gin.Context) {
		var request currentPasswordRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		enrollment, err := service.BeginTOTPEnrollment(c.Request.Context(), session.User.ID, request.CurrentPassword)
		if writeMFAError(c, err) {
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, enrollment)
	})
	me.POST("/mfa/totp/confirm", requireCSRF(config), func(c *gin.Context) {
		var request mfaCodeRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		confirmation, err := service.ConfirmTOTPEnrollment(c.Request.Context(), session, request.Code)
		if writeMFAError(c, err) {
			return
		}
		setAuthCookies(c, config, confirmation.Session)
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"recoveryCodes":     confirmation.RecoveryCodes,
			"assuranceLevel":    confirmation.Session.AssuranceLevel,
			"absoluteExpiresAt": confirmation.Session.AbsoluteExpiresAt,
		})
	})
	me.POST("/mfa/recovery-codes", requireCSRF(config), func(c *gin.Context) {
		var request currentPasswordRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		codes, err := service.RegenerateRecoveryCodes(c.Request.Context(), session, request.CurrentPassword)
		if writeMFAError(c, err) {
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"recoveryCodes": codes})
	})
	me.DELETE("/mfa/totp", requireCSRF(config), func(c *gin.Context) {
		var request disableTOTPRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		if writeMFAError(c, service.DisableTOTP(c.Request.Context(), session, request.CurrentPassword, request.Code)) {
			return
		}
		c.Status(http.StatusNoContent)
	})
	me.GET("/sessions", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		items, err := service.ListSessions(c.Request.Context(), session.User.ID, session.ID)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法读取会话", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	me.DELETE("/sessions/:sessionId", requireCSRF(config), func(c *gin.Context) {
		target, err := uuid.Parse(c.Param("sessionId"))
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "invalid_session_id", "会话标识无效", "请刷新后重试。")
			return
		}
		session, _ := authSessionFrom(c)
		err = service.RevokeSession(c.Request.Context(), session.User.ID, target)
		if errors.Is(err, auth.ErrSessionTargetNotFound) {
			writeProblem(c, http.StatusNotFound, "session_not_found", "会话不存在", "会话可能已经退出。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法撤销会话", "请稍后重试。")
			return
		}
		if target == session.ID {
			clearAuthCookies(c, config)
		}
		c.Status(http.StatusNoContent)
	})
}

func writeMFAError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, auth.ErrCurrentPassword):
		writeProblem(c, http.StatusBadRequest, "current_password_invalid", "当前密码错误", "请重新输入。")
	case errors.Is(err, auth.ErrMFAInvalidCode):
		writeProblem(c, http.StatusBadRequest, "mfa_code_invalid", "验证码无效", "请检查验证器时间或恢复码。")
	case errors.Is(err, auth.ErrMFAReplay):
		writeProblem(c, http.StatusConflict, "mfa_code_replayed", "验证码已使用", "请等待验证器生成新验证码。")
	case errors.Is(err, auth.ErrMFAAlreadyEnrolled):
		writeProblem(c, http.StatusConflict, "mfa_already_enrolled", "二次验证已配置", "如需重设，请先停用现有验证器。")
	case errors.Is(err, auth.ErrMFANotEnrolled):
		writeProblem(c, http.StatusBadRequest, "mfa_not_enrolled", "尚未配置二次验证", "请先配置验证器。")
	case errors.Is(err, auth.ErrMFAAssurance):
		writeProblem(c, http.StatusForbidden, "mfa_required", "需要多因素验证", "请先完成二次验证。")
	case errors.Is(err, auth.ErrSessionUnavailable):
		writeProblem(c, http.StatusUnauthorized, "session_expired", "登录已失效", "请重新登录。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "二次验证服务暂不可用", "请稍后重试。")
	}
	return true
}

func requireSession(service AuthService, config AuthHTTPConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !authServiceAvailable(c, service) {
			return
		}
		plaintext, err := c.Cookie(sessionCookieName(config.CookieSecure))
		if err != nil {
			writeProblem(c, http.StatusUnauthorized, "authentication_required", "需要登录", "请先登录账户。")
			return
		}
		session, err := service.AuthenticateSession(c.Request.Context(), plaintext)
		if errors.Is(err, auth.ErrSessionUnavailable) {
			clearAuthCookies(c, config)
			writeProblem(c, http.StatusUnauthorized, "session_expired", "登录已失效", "请重新登录。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "认证服务暂不可用", "请稍后重试。")
			return
		}
		c.Set(authSessionContextKey, session)
		c.Set(authenticationSourceContextKey, authenticationSourceBrowserCookie)
		c.Header("Cache-Control", "no-store")
		c.Header("Vary", "Cookie")
		c.Next()
	}
}

func requireAccountAuthentication(service AuthService, appService AppAuthService, config AuthHTTPConfig, requiredScope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookieToken, cookieErr := c.Cookie(sessionCookieName(config.CookieSecure))
		authorization := strings.TrimSpace(c.GetHeader("Authorization"))
		hasCookie := cookieErr == nil && cookieToken != ""
		hasAuthorization := authorization != ""
		if hasCookie && hasAuthorization {
			writeProblem(c, http.StatusBadRequest, "ambiguous_authentication", "认证信息冲突", "请勿同时发送浏览器 Cookie 和客户端 Bearer Token。")
			return
		}
		if hasAuthorization {
			if appService == nil {
				writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "客户端认证服务暂不可用", "请稍后重试。")
				return
			}
			plaintext, ok := bearerToken(authorization)
			if !ok {
				writeProblem(c, http.StatusUnauthorized, "app_token_invalid", "客户端登录已失效", "请刷新凭证或重新登录。")
				return
			}
			appSession, err := appService.AuthenticateAppAccessToken(c.Request.Context(), plaintext)
			if errors.Is(err, auth.ErrAppTokenInvalid) {
				writeProblem(c, http.StatusUnauthorized, "app_token_invalid", "客户端登录已失效", "请刷新凭证或重新登录。")
				return
			}
			if err != nil {
				writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "客户端认证服务暂不可用", "请稍后重试。")
				return
			}
			if requiredScope != "" && !scopeContains(appSession.Scope, requiredScope) {
				writeProblem(c, http.StatusForbidden, "insufficient_scope", "客户端权限不足", "当前凭证不能访问此接口。")
				return
			}
			c.Set(authSessionContextKey, auth.AuthenticatedSession{
				ID: appSession.SessionID, User: appSession.User, AssuranceLevel: 1,
				IdleExpiresAt: appSession.SessionExpiresAt, AbsoluteExpiresAt: appSession.SessionExpiresAt,
			})
			c.Set(authenticationSourceContextKey, authenticationSourceAppBearer)
			c.Header("Cache-Control", "no-store")
			c.Header("Vary", "Authorization, Cookie")
			c.Next()
			return
		}
		if !hasCookie {
			writeProblem(c, http.StatusUnauthorized, "authentication_required", "需要登录", "请先登录账户。")
			return
		}
		if !authServiceAvailable(c, service) {
			return
		}
		session, err := service.AuthenticateSession(c.Request.Context(), cookieToken)
		if errors.Is(err, auth.ErrSessionUnavailable) {
			clearAuthCookies(c, config)
			writeProblem(c, http.StatusUnauthorized, "session_expired", "登录已失效", "请重新登录。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "认证服务暂不可用", "请稍后重试。")
			return
		}
		c.Set(authSessionContextKey, session)
		c.Set(authenticationSourceContextKey, authenticationSourceBrowserCookie)
		c.Header("Cache-Control", "no-store")
		c.Header("Vary", "Authorization, Cookie")
		c.Next()
	}
}

func requireCSRF(config AuthHTTPConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if validateCSRF(c, config) {
			c.Next()
		}
	}
}

// requireCookieCSRF keeps browser cookie sessions protected against CSRF while
// allowing a validated native-app bearer token through. The latter is never
// ambient browser credential state, and requireAccountAuthentication has
// already rejected mixed cookie and bearer authentication.
func requireCookieCSRF(config AuthHTTPConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString(authenticationSourceContextKey) == authenticationSourceAppBearer {
			c.Next()
			return
		}
		if validateCSRF(c, config) {
			c.Next()
		}
	}
}

func validateCSRF(c *gin.Context, config AuthHTTPConfig) bool {
	if !originAllowed(c, config.AllowedOrigins) {
		writeProblem(c, http.StatusForbidden, "origin_rejected", "请求来源无效", "请从 WenzWork 官方页面重试。")
		return false
	}
	session, ok := authSessionFrom(c)
	if !ok {
		writeProblem(c, http.StatusUnauthorized, "authentication_required", "需要登录", "请先登录账户。")
		return false
	}
	headerToken := c.GetHeader("X-CSRF-Token")
	cookieToken, err := c.Cookie(csrfCookieName(config.CookieSecure))
	if err != nil || len(headerToken) != len(cookieToken) || subtle.ConstantTimeCompare([]byte(headerToken), []byte(cookieToken)) != 1 || !auth.VerifySessionCSRF(session, headerToken) {
		writeProblem(c, http.StatusForbidden, "csrf_rejected", "安全校验失败", "请刷新页面后重试。")
		return false
	}
	return true
}

func RequirePermission(permission string, requireMFA bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		session, ok := authSessionFrom(c)
		if !ok {
			writeProblem(c, http.StatusUnauthorized, "authentication_required", "需要登录", "请先登录账户。")
			return
		}
		if !auth.HasPermission(session.User, permission) {
			writeProblem(c, http.StatusForbidden, "permission_denied", "权限不足", "当前账户不能执行此操作。")
			return
		}
		if requireMFA && session.AssuranceLevel < 2 {
			writeProblem(c, http.StatusForbidden, "mfa_required", "需要多因素验证", "完成管理员二次验证后重试。")
			return
		}
		c.Next()
	}
}

func requireAllowedOrigin(allowed []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !originAllowed(c, allowed) {
			writeProblem(c, http.StatusForbidden, "origin_rejected", "请求来源无效", "请从 WenzWork 官方页面重试。")
			return
		}
		c.Next()
	}
}

func originAllowed(c *gin.Context, allowed []string) bool {
	parsed, err := browserRequestOrigin(c)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	origin := parsed.Scheme + "://" + parsed.Host
	requestScheme := "http"
	if c.Request.TLS != nil {
		requestScheme = "https"
	}
	// The configured list is still authoritative for reverse-proxied and
	// cross-origin deployments. Exact direct same-origin requests are also safe
	// and let a fresh Host be opened through its temporary IP/port before the
	// administrator has configured the final public URL.
	if parsed.Scheme == requestScheme && strings.EqualFold(parsed.Host, c.Request.Host) {
		return true
	}
	for _, candidate := range allowed {
		candidateURL, err := url.Parse(strings.TrimSpace(candidate))
		if err == nil && candidateURL.Scheme+"://"+candidateURL.Host == origin {
			return true
		}
	}
	return false
}

func secureCookieTransportAvailable(c *gin.Context, config AuthHTTPConfig) bool {
	if !config.CookieSecure {
		return true
	}
	parsed, err := browserRequestOrigin(c)
	if err == nil && strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	writeProblem(
		c,
		http.StatusBadRequest,
		"secure_transport_required",
		"登录需要 HTTPS",
		"当前 Host 使用安全会话 Cookie，请通过配置的 HTTPS 地址访问后重新登录。",
	)
	return false
}

func browserRequestOrigin(c *gin.Context) (*url.URL, error) {
	raw := strings.TrimSpace(c.GetHeader("Origin"))
	if raw == "" {
		raw = strings.TrimSpace(c.GetHeader("Referer"))
	}
	return url.Parse(raw)
}

func authSessionFrom(c *gin.Context) (auth.AuthenticatedSession, bool) {
	value, ok := c.Get(authSessionContextKey)
	if !ok {
		return auth.AuthenticatedSession{}, false
	}
	session, ok := value.(auth.AuthenticatedSession)
	return session, ok
}

func authServiceAvailable(c *gin.Context, service AuthService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "认证服务暂不可用", "请稍后重试。")
	return false
}

func decodeJSON(c *gin.Context, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeProblem(c, http.StatusUnsupportedMediaType, "json_required", "需要 JSON 请求", "请使用 application/json。")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeProblem(c, http.StatusBadRequest, "invalid_json", "请求内容无效", "请检查 JSON 字段和格式。")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeProblem(c, http.StatusBadRequest, "invalid_json", "请求内容无效", "每个请求只能包含一个 JSON 对象。")
		return false
	}
	return true
}

func setAuthCookies(c *gin.Context, config AuthHTTPConfig, session auth.Session) {
	maxAge := 0
	var expires time.Time
	if session.RememberMe {
		expires = session.AbsoluteExpiresAt
		maxAge = max(1, int(time.Until(expires).Seconds()))
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name: sessionCookieName(config.CookieSecure), Value: session.Token, Path: "/",
		Expires: expires, MaxAge: maxAge, Secure: config.CookieSecure, HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(c.Writer, &http.Cookie{
		Name: csrfCookieName(config.CookieSecure), Value: session.CSRFToken, Path: "/",
		Expires: expires, MaxAge: maxAge, Secure: config.CookieSecure, HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearAuthCookies(c *gin.Context, config AuthHTTPConfig) {
	for _, cookie := range []struct {
		name     string
		httpOnly bool
	}{{sessionCookieName(config.CookieSecure), true}, {csrfCookieName(config.CookieSecure), false}} {
		http.SetCookie(c.Writer, &http.Cookie{
			Name: cookie.name, Value: "", Path: "/", Expires: time.Unix(1, 0), MaxAge: -1,
			Secure: config.CookieSecure, HttpOnly: cookie.httpOnly, SameSite: http.SameSiteLaxMode,
		})
	}
}

func sessionCookieName(secure bool) string {
	if secure {
		return "__Host-wenzwork_session"
	}
	return "wenzwork_session"
}

func csrfCookieName(secure bool) string {
	if secure {
		return "__Host-wenzwork_csrf"
	}
	return "wenzwork_csrf"
}
