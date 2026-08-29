package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/deviceaccesskey"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
)

type RemoteDeviceAccessKeyService interface {
	CreateAccessKey(context.Context, deviceaccesskey.CreateInput) (deviceaccesskey.AccessKey, error)
	RotateAccessKey(context.Context, deviceaccesskey.RotateInput) (deviceaccesskey.AccessKey, error)
	RevokeAccessKey(context.Context, uuid.UUID, uuid.UUID) error
	DeleteAccessKey(context.Context, uuid.UUID, uuid.UUID) error
	ListAccessKeys(context.Context, uuid.UUID) ([]deviceaccesskey.AccessKey, error)
}

type createDeviceAccessKeyRequest struct {
	Label string `json:"label"`
	// Scopes is accepted for compatibility only. The service applies the full
	// Device Agent permission profile atomically.
	Scopes        []string   `json:"scopes"`
	BoundDeviceID *uuid.UUID `json:"boundDeviceId,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
}

func registerDeviceAccessKeyRoutes(group *gin.RouterGroup, authService AuthService, appAuth AppAuthService, devices RemoteDeviceService, config AuthHTTPConfig) {
	keys := group.Group("/remote/device-access-keys", requireAccountAuthentication(authService, appAuth, config, "remote.connect"))
	keys.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
	})
	keys.GET("", func(c *gin.Context) {
		service, ok := devices.(RemoteDeviceAccessKeyService)
		if !ok || service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "device_access_keys_unavailable", "设备访问密钥暂不可用", "请稍后重试。")
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.ListAccessKeys(c.Request.Context(), session.User.ID)
		if writeDeviceAccessKeyProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": result})
	})
	keys.POST("", requireCookieCSRF(config), func(c *gin.Context) {
		service, ok := devices.(RemoteDeviceAccessKeyService)
		if !ok || service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "device_access_keys_unavailable", "设备访问密钥暂不可用", "请稍后重试。")
			return
		}
		var request createDeviceAccessKeyRequest
		if !decodeJSON(c, &request) {
			return
		}
		idempotencyKey, ok := deviceAccessKeyIdempotencyKey(c)
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.CreateAccessKey(c.Request.Context(), deviceaccesskey.CreateInput{
			UserID: session.User.ID, IdempotencyKey: idempotencyKey, Label: request.Label, Scopes: request.Scopes,
			BoundDeviceID: request.BoundDeviceID, ExpiresAt: request.ExpiresAt,
		})
		if writeDeviceAccessKeyProblem(c, err) {
			return
		}
		c.Header("Location", "/api/v1/remote/device-access-keys/"+result.ID.String())
		c.JSON(http.StatusCreated, result)
	})
	keys.POST("/:keyId/rotation", requireCookieCSRF(config), func(c *gin.Context) {
		service, ok := devices.(RemoteDeviceAccessKeyService)
		if !ok || service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "device_access_keys_unavailable", "设备访问密钥暂不可用", "请稍后重试。")
			return
		}
		keyID, ok := deviceAccessKeyID(c)
		if !ok {
			return
		}
		idempotencyKey, ok := deviceAccessKeyIdempotencyKey(c)
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.RotateAccessKey(c.Request.Context(), deviceaccesskey.RotateInput{
			KeyID: keyID, UserID: session.User.ID, IdempotencyKey: idempotencyKey,
		})
		if writeDeviceAccessKeyProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	keys.DELETE("/:keyId", requireCookieCSRF(config), func(c *gin.Context) {
		service, ok := devices.(RemoteDeviceAccessKeyService)
		if !ok || service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "device_access_keys_unavailable", "设备访问密钥暂不可用", "请稍后重试。")
			return
		}
		keyID, ok := deviceAccessKeyID(c)
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		if writeDeviceAccessKeyProblem(c, service.RevokeAccessKey(c.Request.Context(), keyID, session.User.ID)) {
			return
		}
		c.Status(http.StatusNoContent)
	})
	keys.DELETE("/:keyId/permanent", requireCookieCSRF(config), func(c *gin.Context) {
		service, ok := devices.(RemoteDeviceAccessKeyService)
		if !ok || service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "device_access_keys_unavailable", "设备访问密钥暂不可用", "请稍后重试。")
			return
		}
		keyID, ok := deviceAccessKeyID(c)
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		if writeDeviceAccessKeyProblem(c, service.DeleteAccessKey(c.Request.Context(), keyID, session.User.ID)) {
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func deviceAccessKeyIdempotencyKey(c *gin.Context) (string, bool) {
	value, ok := deviceaccesskey.ParseIdempotencyKey(c.GetHeader("Idempotency-Key"))
	if !ok {
		writeProblem(c, http.StatusBadRequest, "idempotency_key_invalid", "幂等键无效", "请提供 8 到 128 位且仅含字母、数字、点、下划线、冒号或连字符的 Idempotency-Key。")
		return "", false
	}
	return value, true
}

func deviceAccessKeyID(c *gin.Context) (uuid.UUID, bool) {
	keyID, err := uuid.Parse(strings.TrimSpace(c.Param("keyId")))
	if err != nil || keyID == uuid.Nil {
		writeProblem(c, http.StatusBadRequest, "device_access_key_invalid", "设备访问密钥编号无效", "请刷新密钥列表后重试。")
		return uuid.Nil, false
	}
	return keyID, true
}

func writeDeviceAccessKeyProblem(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, remoteaccesspolicy.ErrMembershipRequired):
		writeProblem(c, http.StatusForbidden, "membership_required", "当前套餐未开放设备接入", "请等待 Free 套餐临时开放，或使用已开放远程设备功能的有效会员套餐。")
	case errors.Is(err, deviceaccesskey.ErrInvalidInput):
		writeProblem(c, http.StatusBadRequest, "device_access_key_invalid", "设备访问密钥配置无效", "请检查标签、绑定设备与过期时间。")
	case errors.Is(err, deviceaccesskey.ErrNotFound):
		writeProblem(c, http.StatusNotFound, "device_access_key_not_found", "设备访问密钥不存在", "请刷新密钥列表后重试。")
	case errors.Is(err, deviceaccesskey.ErrConflict):
		writeProblem(c, http.StatusConflict, "device_access_key_conflict", "设备访问密钥状态已变化", "请刷新密钥列表后重试。")
	case errors.Is(err, deviceaccesskey.ErrIdempotencyConflict):
		writeProblem(c, http.StatusConflict, "idempotency_conflict", "幂等键已用于其他请求", "请为不同请求使用新的 Idempotency-Key。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "device_access_keys_unavailable", "设备访问密钥暂不可用", "请稍后重试。")
	}
	return true
}
