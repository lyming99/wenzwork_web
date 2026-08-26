package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

// RemoteDeviceLinkService is intentionally separate from RemoteControlService
// so existing v1-only deployments fail closed rather than accidentally
// exposing a v2-shaped endpoint backed by a v1 Peer issuer.
type RemoteDeviceLinkService interface {
	IssueDeviceLink(context.Context, remotecontrol.DeviceLinkInput) (remotecontrol.DeviceLink, error)
	RevokeDeviceLink(context.Context, remotecontrol.DeviceLinkRevocationInput) error
}

type createDeviceLinkRequest struct {
	TargetDeviceID              uuid.UUID `json:"targetDeviceId"`
	ClientIdentityKeyVersion    uint64    `json:"clientIdentityKeyVersion"`
	RequestedMaximumLifetimeSec *uint32   `json:"requestedMaximumLifetimeSeconds,omitempty"`
}

func registerRemoteV2ControlRoutes(v2 *gin.RouterGroup, authService AuthService, appAuth AppAuthService, service RemoteControlService, config AuthHTTPConfig) {
	links, ok := service.(RemoteDeviceLinkService)
	remote := v2.Group("/remote", requireAccountAuthentication(authService, appAuth, config, "remote.connect"))
	remote.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		if !ok || links == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_v2_unavailable", "远程 v2 连接暂不可用", "请升级控制端、Relay 和设备 Agent 后重试。")
			c.Abort()
			return
		}
		c.Next()
	})

	remote.POST("/controllers/:controllerId/device-links", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		controllerID, valid := remoteUUIDParam(c, "controllerId")
		if !valid {
			return
		}
		var request createDeviceLinkRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.TargetDeviceID == uuid.Nil || request.ClientIdentityKeyVersion == 0 {
			writeProblem(c, http.StatusBadRequest, "remote_control_invalid", "远程连接请求无效", "请检查目标设备和控制身份版本。")
			return
		}
		requestSessionID := session.ID
		if c.GetString(authenticationSourceContextKey) == authenticationSourceAppBearer {
			requestSessionID = uuid.Nil
		}
		result, err := links.IssueDeviceLink(c.Request.Context(), remotecontrol.DeviceLinkInput{
			UserID: session.User.ID, SessionID: requestSessionID, ControllerID: controllerID, TargetDeviceID: request.TargetDeviceID,
			ClientIdentityKeyVersion: request.ClientIdentityKeyVersion, RequestedMaximumLifetimeSec: request.RequestedMaximumLifetimeSec,
			IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusCreated, gin.H{"deviceLink": result})
	})

	remote.DELETE("/controllers/:controllerId/device-links/:grantId", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		controllerID, valid := remoteUUIDParam(c, "controllerId")
		if !valid {
			return
		}
		grantID, valid := remoteUUIDParam(c, "grantId")
		if !valid {
			return
		}
		if writeRemoteControlProblem(c, links.RevokeDeviceLink(c.Request.Context(), remotecontrol.DeviceLinkRevocationInput{
			UserID: session.User.ID, ControllerID: controllerID, GrantID: grantID,
		})) {
			return
		}
		c.Status(http.StatusNoContent)
	})
}
