package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/analytics"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

type AppAuthService interface {
	CreateDeviceAuthorization(context.Context, auth.CreateDeviceAuthorizationInput) (auth.DeviceAuthorization, error)
	GetDeviceAuthorization(context.Context, string, uuid.UUID) (auth.BrowserDeviceAuthorization, error)
	ApproveDeviceAuthorization(context.Context, string, uuid.UUID) (auth.BrowserDeviceAuthorization, error)
	DenyDeviceAuthorization(context.Context, string, uuid.UUID) error
	ExchangeDeviceCode(context.Context, string, string) (auth.AppTokenResult, error)
	LoginAppWithPassword(context.Context, auth.PasswordAppLoginInput) (auth.AppTokenResult, error)
	RefreshAppToken(context.Context, string, string) (auth.AppTokenResult, error)
	AuthenticateAppAccessToken(context.Context, string) (auth.AuthenticatedAppSession, error)
	RevokeAppToken(context.Context, string, string) error
}

type createDeviceAuthorizationRequest struct {
	ClientID   string `json:"client_id"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
}

type deviceAuthorizationDecisionRequest struct {
	UserCode string `json:"userCode"`
}

type deviceAuthorizationResponse struct {
	RequestID               uuid.UUID `json:"request_id"`
	DeviceCode              string    `json:"device_code"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete"`
	ExpiresIn               int64     `json:"expires_in"`
	Interval                int       `json:"interval"`
}

type appTokenResponse struct {
	TokenType        string    `json:"token_type"`
	AccessToken      string    `json:"access_token"`
	ExpiresIn        int64     `json:"expires_in"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresIn int64     `json:"refresh_expires_in"`
	SessionID        uuid.UUID `json:"session_id"`
	Scope            string    `json:"scope"`
}

type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func registerAppAuthRoutes(group *gin.RouterGroup, service AppAuthService, browserAuth AuthService, loginEvents LoginEventRecorder, config AuthHTTPConfig, log *slog.Logger) {
	oauth := group.Group("/oauth")
	oauth.POST("/device-authorization", func(c *gin.Context) {
		if !appAuthServiceAvailable(c, service) {
			return
		}
		var request createDeviceAuthorizationRequest
		if !decodeOAuthJSON(c, &request) {
			return
		}
		created, err := service.CreateDeviceAuthorization(c.Request.Context(), auth.CreateDeviceAuthorizationInput{
			ClientID: request.ClientID, DeviceID: request.DeviceID, DeviceName: request.DeviceName,
			ClientIP: c.ClientIP(),
		})
		switch {
		case errors.Is(err, auth.ErrDeviceClientInvalid), errors.Is(err, auth.ErrDeviceAuthorizationInvalid):
			writeOAuthError(c, http.StatusBadRequest, "invalid_request", "客户端或设备信息无效。")
		case errors.Is(err, auth.ErrRateLimited):
			c.Header("Retry-After", "300")
			writeOAuthError(c, http.StatusTooManyRequests, "slow_down", "登录窗口申请过于频繁。")
		case err != nil:
			writeOAuthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "暂时无法创建登录窗口。")
		default:
			c.Header("Cache-Control", "no-store")
			c.Header("Pragma", "no-cache")
			c.JSON(http.StatusOK, deviceAuthorizationResponse{
				RequestID: created.RequestID, DeviceCode: created.DeviceCode, UserCode: created.UserCode,
				VerificationURI: created.VerificationURI, VerificationURIComplete: created.VerificationURIComplete,
				ExpiresIn: created.ExpiresIn, Interval: created.Interval,
			})
		}
	})

	oauth.GET("/device-authorization", requireSession(browserAuth, config), func(c *gin.Context) {
		if !appAuthServiceAvailable(c, service) {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.GetDeviceAuthorization(c.Request.Context(), c.Query("userCode"), session.User.ID)
		if writeBrowserDeviceAuthorizationError(c, err) {
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	})

	oauth.POST("/device-authorization/approve", requireSession(browserAuth, config), requireCSRF(config), func(c *gin.Context) {
		if !appAuthServiceAvailable(c, service) {
			return
		}
		var request deviceAuthorizationDecisionRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.ApproveDeviceAuthorization(c.Request.Context(), request.UserCode, session.User.ID)
		if writeBrowserDeviceAuthorizationError(c, err) {
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	})

	oauth.POST("/device-authorization/deny", requireSession(browserAuth, config), requireCSRF(config), func(c *gin.Context) {
		if !appAuthServiceAvailable(c, service) {
			return
		}
		var request deviceAuthorizationDecisionRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		if writeBrowserDeviceAuthorizationError(c, service.DenyDeviceAuthorization(c.Request.Context(), request.UserCode, session.User.ID)) {
			return
		}
		c.Status(http.StatusNoContent)
	})

	oauth.POST("/token", func(c *gin.Context) {
		if !appAuthServiceAvailable(c, service) {
			return
		}
		if !parseOAuthForm(c) {
			return
		}
		clientID := c.PostForm("client_id")
		grantType := c.PostForm("grant_type")
		var result auth.AppTokenResult
		var err error
		switch grantType {
		case auth.DeviceGrantType:
			result, err = service.ExchangeDeviceCode(c.Request.Context(), clientID, c.PostForm("device_code"))
		case auth.PasswordGrantType:
			result, err = service.LoginAppWithPassword(c.Request.Context(), auth.PasswordAppLoginInput{
				ClientID: clientID, DeviceID: c.PostForm("device_id"), DeviceName: c.PostForm("device_name"),
				Email: c.PostForm("email"), Password: c.PostForm("password"), ClientIP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent"),
			})
		case auth.RefreshTokenGrantType:
			result, err = service.RefreshAppToken(c.Request.Context(), clientID, c.PostForm("refresh_token"))
		default:
			writeOAuthError(c, http.StatusBadRequest, "unsupported_grant_type", "不支持此授权类型。")
			return
		}
		if writeAppTokenError(c, err) {
			return
		}
		if (grantType == auth.DeviceGrantType || grantType == auth.PasswordGrantType) && loginEvents != nil {
			loginMethod := analytics.LoginMethodAppDevice
			if grantType == auth.PasswordGrantType {
				loginMethod = analytics.LoginMethodPassword
			}
			if err := loginEvents.RecordLogin(c.Request.Context(), analytics.LoginEventInput{
				UserID: result.UserID, AppSessionID: result.SessionID,
				LoginMethod: loginMethod, ClientIP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent"),
			}); err != nil {
				log.Warn("app login event recording failed", "request_id", requestIDFrom(c), "user_id", result.UserID, "error", err)
			}
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.JSON(http.StatusOK, appTokenResponse{
			TokenType: "Bearer", AccessToken: result.AccessToken, ExpiresIn: result.AccessExpiresIn,
			RefreshToken: result.RefreshToken, RefreshExpiresIn: result.RefreshExpiresIn,
			SessionID: result.SessionID, Scope: result.Scope,
		})
	})

	oauth.POST("/revoke", func(c *gin.Context) {
		if !appAuthServiceAvailable(c, service) {
			return
		}
		if !parseOAuthForm(c) {
			return
		}
		if err := service.RevokeAppToken(c.Request.Context(), c.PostForm("client_id"), c.PostForm("token")); err != nil {
			writeOAuthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "暂时无法退出客户端。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Status(http.StatusNoContent)
	})
}

func writeBrowserDeviceAuthorizationError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, auth.ErrDeviceAuthorizationExpired):
		writeProblem(c, http.StatusGone, "device_authorization_expired", "登录窗口已过期", "请返回客户端重新申请登录。")
	case errors.Is(err, auth.ErrDeviceAuthorizationInvalid):
		writeProblem(c, http.StatusNotFound, "device_authorization_not_found", "登录窗口无效", "请检查登录链接或返回客户端重试。")
	case errors.Is(err, auth.ErrAccountUnavailable):
		writeProblem(c, http.StatusForbidden, "account_unavailable", "账户暂不可用", "请更换账户或联系支持渠道。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "暂时无法处理客户端登录", "请稍后重试。")
	}
	return true
}

func writeAppTokenError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, auth.ErrDeviceAuthorizationPending):
		writeOAuthError(c, http.StatusBadRequest, "authorization_pending", "用户尚未确认登录。")
	case errors.Is(err, auth.ErrDeviceAuthorizationSlowDown):
		writeOAuthError(c, http.StatusBadRequest, "slow_down", "轮询过于频繁，请增加五秒间隔。")
	case errors.Is(err, auth.ErrDeviceAuthorizationDenied):
		writeOAuthError(c, http.StatusBadRequest, "access_denied", "用户拒绝了登录请求。")
	case errors.Is(err, auth.ErrDeviceAuthorizationExpired):
		writeOAuthError(c, http.StatusBadRequest, "expired_token", "登录窗口已经过期。")
	case errors.Is(err, auth.ErrDeviceClientInvalid), errors.Is(err, auth.ErrDeviceAuthorizationInvalid),
		errors.Is(err, auth.ErrAppTokenInvalid), errors.Is(err, auth.ErrAppRefreshReplay):
		writeOAuthError(c, http.StatusBadRequest, "invalid_grant", "登录凭证无效、过期或已被使用。")
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrEmailNotVerified), errors.Is(err, auth.ErrAccountUnavailable):
		writeOAuthError(c, http.StatusUnauthorized, "invalid_grant", "账号或密码错误，或账户暂不可用。")
	case errors.Is(err, auth.ErrRateLimited):
		c.Header("Retry-After", "300")
		writeOAuthError(c, http.StatusTooManyRequests, "slow_down", "登录尝试次数过多，请稍后再试。")
	default:
		writeOAuthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "认证服务暂时不可用。")
	}
	return true
}

func parseOAuthForm(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		writeOAuthError(c, http.StatusUnsupportedMediaType, "invalid_request", "请求必须使用 application/x-www-form-urlencoded。")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.Request.ParseForm(); err != nil {
		writeOAuthError(c, http.StatusBadRequest, "invalid_request", "无法解析请求参数。")
		return false
	}
	for _, values := range c.Request.PostForm {
		if len(values) != 1 {
			writeOAuthError(c, http.StatusBadRequest, "invalid_request", "同一请求参数只能出现一次。")
			return false
		}
	}
	return true
}

func decodeOAuthJSON(c *gin.Context, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeOAuthError(c, http.StatusUnsupportedMediaType, "invalid_request", "请求必须使用 application/json。")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeOAuthError(c, http.StatusBadRequest, "invalid_request", "无法解析请求参数。")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeOAuthError(c, http.StatusBadRequest, "invalid_request", "每个请求只能包含一个 JSON 对象。")
		return false
	}
	return true
}

func writeOAuthError(c *gin.Context, status int, code, description string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.AbortWithStatusJSON(status, oauthErrorResponse{Error: code, ErrorDescription: description})
}

func appAuthServiceAvailable(c *gin.Context, service AppAuthService) bool {
	if service != nil {
		return true
	}
	writeOAuthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "客户端认证服务暂不可用。")
	return false
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	returnValue := ""
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		returnValue = parts[1]
	}
	return returnValue, returnValue != ""
}

func scopeContains(scope, required string) bool {
	for _, item := range strings.Fields(scope) {
		if item == required {
			return true
		}
	}
	return false
}
