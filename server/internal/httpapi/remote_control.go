package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

type RemoteControlService interface {
	ListDevices(context.Context, uuid.UUID, remotecontrol.PageRequest) (remotecontrol.DevicePage, error)
	GetDevice(context.Context, uuid.UUID, uuid.UUID) (remotecontrol.Device, error)
	UpdateDevice(context.Context, remotecontrol.DeviceUpdateInput) (remotecontrol.Device, error)
	DeleteDevice(context.Context, remotecontrol.DeviceDeletionInput) error
	EnableAccess(context.Context, remotecontrol.AccessInput) (remotecontrol.AccessResult, error)
	RevokeAccess(context.Context, remotecontrol.AccessInput) (remotecontrol.AccessResult, error)
	ListProjects(context.Context, uuid.UUID, uuid.UUID, remotecontrol.PageRequest) (remotecontrol.ProjectPage, error)
	RequestProjectSync(context.Context, remotecontrol.SyncProjectInput) (remotecontrol.Operation, error)
	ListTasks(context.Context, uuid.UUID, uuid.UUID, remotecontrol.PageRequest) (remotecontrol.TaskPage, error)
	GetTask(context.Context, uuid.UUID, uuid.UUID) (remotecontrol.Task, error)
	CancelTask(context.Context, remotecontrol.CancelTaskInput) (remotecontrol.Operation, error)
	ListTaskEvents(context.Context, uuid.UUID, uuid.UUID, uint64, int) (remotecontrol.TaskEventPage, error)
	RegisterController(context.Context, remotecontrol.RegisterControllerInput) (remotecontrol.ControllerIdentity, error)
	RotateController(context.Context, remotecontrol.RotateControllerInput) (remotecontrol.ControllerIdentity, error)
	RevokeController(context.Context, remotecontrol.RevokeControllerInput) (remotecontrol.ControllerIdentity, error)
	GetController(context.Context, uuid.UUID, uuid.UUID) (remotecontrol.ControllerIdentity, error)
	PushChanges(context.Context, remotecontrol.DevicePrincipal, remotecontrol.PushChangesInput) (remotecontrol.PushChangesResult, error)
	PollCommands(context.Context, remotecontrol.DevicePrincipal, int) (remotecontrol.CommandPage, error)
	AckCommand(context.Context, remotecontrol.DevicePrincipal, uuid.UUID, remotecontrol.AckCommandInput) error
	PushEvents(context.Context, remotecontrol.DevicePrincipal, remotecontrol.PushEventsInput) (remotecontrol.PushEventsResult, error)
}

type accessRequest struct {
	Scopes       []string `json:"scopes"`
	Confirmation string   `json:"confirmation"`
}

type updateDeviceRequest struct {
	DeviceName        string `json:"deviceName"`
	DirectModeEnabled *bool  `json:"directModeEnabled,omitempty"`
}

type syncProjectRequest struct {
	ProjectID     *uuid.UUID `json:"projectId,omitempty"`
	AfterSequence uint64     `json:"afterSequence,omitempty"`
	HighWatermark uint64     `json:"highWatermark,omitempty"`
}

type registerControllerRequest struct {
	ControllerID      uuid.UUID `json:"controllerId"`
	IdentityAlgorithm string    `json:"identityAlgorithm"`
	IdentityPublicKey string    `json:"identityPublicKey"`
	Proof             string    `json:"proof"`
	Scopes            []string  `json:"scopes"`
}

type rotateControllerRequest struct {
	IdentityAlgorithm string `json:"identityAlgorithm"`
	IdentityPublicKey string `json:"identityPublicKey"`
	Proof             string `json:"proof"`
}

func registerRemoteControlRoutes(browserV1, deviceV1 *gin.RouterGroup, authService AuthService, appAuth AppAuthService, service RemoteControlService, config AuthHTTPConfig) {
	// Browser sessions and native-app bearer sessions both access account-scoped
	// remote control. Browser mutations retain CSRF protection; a validated app
	// bearer token is non-ambient credential state and must be allowed to create
	// its controller identity and encrypted peer sessions.
	remote := browserV1.Group("/remote", requireAccountAuthentication(authService, appAuth, config, "remote.connect"))
	remote.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		if service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_control_unavailable", "远程控制暂不可用", "请稍后重试。")
			c.Abort()
			return
		}
		c.Next()
	})

	remote.GET("/devices", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		page, ok := remotePageRequest(c)
		if !ok {
			return
		}
		result, err := service.ListDevices(c.Request.Context(), session.User.ID, page)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	remote.GET("/devices/:deviceId", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		device, err := service.GetDevice(c.Request.Context(), session.User.ID, deviceID)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"device": device})
	})
	remote.PATCH("/devices/:deviceId", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		var request updateDeviceRequest
		if !decodeJSON(c, &request) {
			return
		}
		device, err := service.UpdateDevice(c.Request.Context(), remotecontrol.DeviceUpdateInput{
			UserID: session.User.ID, DeviceID: deviceID, DeviceName: request.DeviceName,
			DirectModeEnabled: request.DirectModeEnabled,
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"device": device})
	})
	remote.DELETE("/devices/:deviceId", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		if writeRemoteControlProblem(c, service.DeleteDevice(c.Request.Context(), remotecontrol.DeviceDeletionInput{
			UserID: session.User.ID, DeviceID: deviceID,
		})) {
			return
		}
		c.Status(http.StatusNoContent)
	})
	remote.POST("/devices/:deviceId/remote-access", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		var request accessRequest
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.EnableAccess(c.Request.Context(), remotecontrol.AccessInput{
			UserID: session.User.ID, DeviceID: deviceID, Scopes: request.Scopes, Confirmation: request.Confirmation,
			IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.Header("Location", "/api/v1/remote/devices/"+deviceID.String())
		c.JSON(http.StatusAccepted, gin.H{"access": result})
	})
	remote.DELETE("/devices/:deviceId/remote-access", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		result, err := service.RevokeAccess(c.Request.Context(), remotecontrol.AccessInput{
			UserID: session.User.ID, DeviceID: deviceID, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.Header("Location", "/api/v1/remote/devices/"+deviceID.String())
		c.JSON(http.StatusAccepted, gin.H{"access": result})
	})
	remote.GET("/devices/:deviceId/projects", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		page, ok := remotePageRequest(c)
		if !ok {
			return
		}
		result, err := service.ListProjects(c.Request.Context(), session.User.ID, deviceID, page)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	remote.POST("/devices/:deviceId/project-syncs", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		var request syncProjectRequest
		if c.Request.ContentLength != 0 && !decodeJSON(c, &request) {
			return
		}
		operation, err := service.RequestProjectSync(c.Request.Context(), remotecontrol.SyncProjectInput{
			UserID: session.User.ID, DeviceID: deviceID, ProjectID: request.ProjectID, AfterSequence: request.AfterSequence,
			HighWatermark: request.HighWatermark, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.Header("Location", "/api/v1/remote/operations/"+operation.ID.String())
		c.JSON(http.StatusAccepted, gin.H{"operation": operation})
	})
	remote.GET("/devices/:deviceId/tasks", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		deviceID, ok := remoteUUIDParam(c, "deviceId")
		if !ok {
			return
		}
		page, ok := remotePageRequest(c)
		if !ok {
			return
		}
		if raw, present := c.GetQuery("afterRevision"); present {
			value, parseErr := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
			if parseErr != nil || strings.TrimSpace(raw) == "" {
				writeProblem(c, http.StatusBadRequest, "remote_query_invalid", "查询参数无效", "请检查分页或序号参数。")
				return
			}
			page.AfterRevision = &value
		}
		result, err := service.ListTasks(c.Request.Context(), session.User.ID, deviceID, page)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	remote.POST("/devices/:deviceId/tasks", requireCookieCSRF(config), func(c *gin.Context) {
		// Task definitions can contain prompts, paths, environment values, and
		// runner arguments. Do not decode them on the control plane.
		writeRemoteControlProblem(c, remotecontrol.ErrPeerRequired)
	})
	remote.GET("/tasks/:taskId", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		taskID, ok := remoteUUIDParam(c, "taskId")
		if !ok {
			return
		}
		task, err := service.GetTask(c.Request.Context(), session.User.ID, taskID)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"task": task})
	})
	remote.POST("/tasks/:taskId/cancellations", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		taskID, ok := remoteUUIDParam(c, "taskId")
		if !ok {
			return
		}
		operation, err := service.CancelTask(c.Request.Context(), remotecontrol.CancelTaskInput{
			UserID: session.User.ID, TaskID: taskID, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.Header("Location", "/api/v1/remote/tasks/"+taskID.String())
		c.JSON(http.StatusAccepted, gin.H{"operation": operation})
	})
	remote.POST("/tasks/:taskId/retries", requireCookieCSRF(config), func(c *gin.Context) {
		writeRemoteControlProblem(c, remotecontrol.ErrPeerRequired)
	})
	remote.GET("/tasks/:taskId/logs", func(c *gin.Context) {
		writeRemoteControlProblem(c, remotecontrol.ErrPeerRequired)
	})
	remote.GET("/tasks/:taskId/events", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		taskID, ok := remoteUUIDParam(c, "taskId")
		if !ok {
			return
		}
		after, ok := remoteUintQuery(c, "afterEventId", 0)
		if !ok {
			return
		}
		limit, ok := remoteIntQuery(c, "limit", 200)
		if !ok {
			return
		}
		result, err := service.ListTaskEvents(c.Request.Context(), session.User.ID, taskID, after, limit)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})

	remote.POST("/controllers", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		registeredSessionID := session.ID
		if c.GetString(authenticationSourceContextKey) == authenticationSourceAppBearer {
			registeredSessionID = uuid.Nil
		}
		var request registerControllerRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.IdentityAlgorithm != "Ed25519" {
			writeProblem(c, http.StatusBadRequest, "controller_identity_invalid", "控制身份无效", "仅支持 Ed25519 控制身份。")
			return
		}
		result, err := service.RegisterController(c.Request.Context(), remotecontrol.RegisterControllerInput{
			UserID: session.User.ID, SessionID: registeredSessionID, ControllerID: request.ControllerID,
			IdentityPublicKey: request.IdentityPublicKey, Proof: request.Proof, Scopes: request.Scopes,
			IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.Header("Location", "/api/v1/remote/controllers/"+result.ID.String())
		c.JSON(http.StatusCreated, gin.H{"controller": result})
	})
	remote.GET("/controllers/:controllerId", func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		controllerID, ok := remoteUUIDParam(c, "controllerId")
		if !ok {
			return
		}
		result, err := service.GetController(c.Request.Context(), session.User.ID, controllerID)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"controller": result})
	})
	remote.PUT("/controllers/:controllerId/key", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		registeredSessionID := session.ID
		if c.GetString(authenticationSourceContextKey) == authenticationSourceAppBearer {
			registeredSessionID = uuid.Nil
		}
		controllerID, ok := remoteUUIDParam(c, "controllerId")
		if !ok {
			return
		}
		var request rotateControllerRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.IdentityAlgorithm != "Ed25519" {
			writeProblem(c, http.StatusBadRequest, "controller_identity_invalid", "控制身份无效", "仅支持 Ed25519 控制身份。")
			return
		}
		result, err := service.RotateController(c.Request.Context(), remotecontrol.RotateControllerInput{
			UserID: session.User.ID, SessionID: registeredSessionID, ControllerID: controllerID,
			IdentityPublicKey: request.IdentityPublicKey, Proof: request.Proof, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"controller": result})
	})
	remote.DELETE("/controllers/:controllerId", requireCookieCSRF(config), func(c *gin.Context) {
		session, _ := authSessionFrom(c)
		controllerID, ok := remoteUUIDParam(c, "controllerId")
		if !ok {
			return
		}
		result, err := service.RevokeController(c.Request.Context(), remotecontrol.RevokeControllerInput{
			UserID: session.User.ID, ControllerID: controllerID, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"controller": result})
	})
	device := deviceV1.Group("/device/remote-control", requireRemoteAppSession(appAuth))
	device.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		if service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_control_unavailable", "远程控制暂不可用", "请稍后重试。")
			c.Abort()
			return
		}
		c.Next()
	})
	device.POST("/changes", func(c *gin.Context) {
		var request remotecontrol.PushChangesInput
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.PushChanges(c.Request.Context(), remoteDevicePrincipal(c), request)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	device.GET("/commands", func(c *gin.Context) {
		limit, ok := remoteIntQuery(c, "limit", 20)
		if !ok {
			return
		}
		result, err := service.PollCommands(c.Request.Context(), remoteDevicePrincipal(c), limit)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	device.POST("/commands/:commandId/ack", func(c *gin.Context) {
		commandID, ok := remoteUUIDParam(c, "commandId")
		if !ok {
			return
		}
		var request remotecontrol.AckCommandInput
		if !decodeJSON(c, &request) {
			return
		}
		if writeRemoteControlProblem(c, service.AckCommand(c.Request.Context(), remoteDevicePrincipal(c), commandID, request)) {
			return
		}
		c.Status(http.StatusNoContent)
	})
	device.POST("/events", func(c *gin.Context) {
		var request remotecontrol.PushEventsInput
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.PushEvents(c.Request.Context(), remoteDevicePrincipal(c), request)
		if writeRemoteControlProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
}

func remoteDevicePrincipal(c *gin.Context) remotecontrol.DevicePrincipal {
	session, _ := remoteAppSessionFrom(c)
	return remotecontrol.DevicePrincipal{UserID: session.User.ID, DeviceID: session.DeviceID}
}

func remoteUUIDParam(c *gin.Context, name string) (uuid.UUID, bool) {
	value, err := uuid.Parse(strings.TrimSpace(c.Param(name)))
	if err != nil || value == uuid.Nil {
		writeProblem(c, http.StatusBadRequest, "remote_id_invalid", "远程资源编号无效", "请检查请求路径。")
		return uuid.Nil, false
	}
	return value, true
}

func remotePageRequest(c *gin.Context) (remotecontrol.PageRequest, bool) {
	limit, ok := remoteIntQuery(c, "limit", 50)
	if !ok {
		return remotecontrol.PageRequest{}, false
	}
	return remotecontrol.PageRequest{Cursor: strings.TrimSpace(c.Query("cursor")), Limit: limit}, true
}

func remoteIntQuery(c *gin.Context, name string, fallback int) (int, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		writeProblem(c, http.StatusBadRequest, "remote_query_invalid", "查询参数无效", "请检查分页或序号参数。")
		return 0, false
	}
	return value, true
}

func remoteUintQuery(c *gin.Context, name string, fallback uint64) (uint64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		writeProblem(c, http.StatusBadRequest, "remote_query_invalid", "查询参数无效", "请检查分页或序号参数。")
		return 0, false
	}
	return value, true
}

func writeRemoteControlProblem(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, remotecontrol.ErrInvalidInput):
		writeProblem(c, http.StatusBadRequest, "remote_control_invalid", "远程控制请求无效", "请检查请求字段、游标和幂等键。")
	case errors.Is(err, remotecontrol.ErrNotFound):
		writeProblem(c, http.StatusNotFound, "remote_control_not_found", "远程资源不存在", "资源不存在或不属于当前账户。")
	case errors.Is(err, remotecontrol.ErrForbidden):
		writeProblem(c, http.StatusForbidden, "remote_control_forbidden", "远程连接授权暂时不可用", "无需单独开启 Scope；请刷新设备连接状态后重试。")
	case errors.Is(err, remotecontrol.ErrConflict), errors.Is(err, remotecontrol.ErrSequenceGap):
		writeProblem(c, http.StatusConflict, "remote_control_conflict", "远程状态已变化", "请刷新游标或投影后重试。")
	case errors.Is(err, remotecontrol.ErrIdempotencyConflict):
		writeProblem(c, http.StatusConflict, "idempotency_conflict", "幂等键已用于其他请求", "请为不同请求使用新的 Idempotency-Key。")
	case errors.Is(err, remotecontrol.ErrPeerRequired):
		writeProblem(c, http.StatusConflict, "remote_peer_required", "需要端到端加密连接", "请通过项目绑定的 Peer RPC 创建、重试或读取任务正文。")
	case errors.Is(err, remotecontrol.ErrProtocolVersion):
		writeProblem(c, http.StatusUpgradeRequired, "relay_protocol_version_invalid", "设备协议版本不兼容", "请升级目标设备 Agent 后重试。")
	case errors.Is(err, remotecontrol.ErrDirectUnavailable):
		writeProblem(c, http.StatusConflict, "remote_direct_unavailable", "设备直连入口不可用", "请先在 Device Agent 配置中开启直连 IP 和端口，并确认心跳在线。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "remote_control_unavailable", "远程控制暂不可用", "请稍后重试。")
	}
	return true
}
