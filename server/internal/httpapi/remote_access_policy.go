package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
)

type RemoteAccessPolicyService interface {
	GetSettings(context.Context) (remoteaccesspolicy.Settings, error)
	UpdateSettings(context.Context, remoteaccesspolicy.UpdateSettingsInput) (remoteaccesspolicy.Settings, error)
}

type updateRemoteAccessPolicyRequest struct {
	DeviceLimit     int   `json:"deviceLimit"`
	ExpectedVersion int64 `json:"expectedVersion"`
}

func registerRemoteAccessPolicyRoutes(group *gin.RouterGroup, policy RemoteAccessPolicyService, authService AuthService, config AuthHTTPConfig) {
	admin := group.Group("/admin/remote-access-policy", requireSession(authService, config))
	admin.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.Next()
	})
	admin.GET("", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), func(c *gin.Context) {
		if policy == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_access_policy_unavailable", "设备接入策略暂不可用", "请稍后重试。")
			return
		}
		settings, err := policy.GetSettings(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_access_policy_unavailable", "设备接入策略暂不可用", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, settings)
	})
	admin.PUT("", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if policy == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_access_policy_unavailable", "设备接入策略暂不可用", "请稍后重试。")
			return
		}
		var request updateRemoteAccessPolicyRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		settings, err := policy.UpdateSettings(c.Request.Context(), remoteaccesspolicy.UpdateSettingsInput{
			DeviceLimit: request.DeviceLimit, ExpectedVersion: request.ExpectedVersion, ActorUserID: session.User.ID,
		})
		switch {
		case errors.Is(err, remoteaccesspolicy.ErrSettingsInvalid):
			writeProblem(c, http.StatusBadRequest, "remote_access_policy_invalid", "设备接入上限无效", "每个账号的设备上限必须是 1 到 100000 之间的整数。")
		case errors.Is(err, remoteaccesspolicy.ErrSettingsConflict):
			writeProblem(c, http.StatusConflict, "remote_access_policy_conflict", "设备接入策略已被修改", "请刷新当前设置后重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "remote_access_policy_unavailable", "设备接入策略暂不可用", "请稍后重试。")
		default:
			c.JSON(http.StatusOK, settings)
		}
	})
}
