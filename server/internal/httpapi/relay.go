package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
	"github.com/wenzwork/wenzwork-web/server/internal/relayrelease"
)

type createRelayInstallationRequest struct {
	ReleaseID      *uuid.UUID `json:"releaseId"`
	DisplayName    string     `json:"displayName"`
	Region         string     `json:"region"`
	Group          string     `json:"group"`
	FailureDomain  string     `json:"failureDomain"`
	OperationsNote string     `json:"operationsNote"`
	PublicEndpoint string     `json:"publicEndpoint"`
	ListenerPort   int        `json:"listenerPort"`
	Platform       string     `json:"platform"`
	Architecture   string     `json:"architecture"`
}

type updateRelayInstallationRequest struct {
	DisplayName         string                              `json:"displayName"`
	Region              string                              `json:"region"`
	Group               string                              `json:"group"`
	FailureDomain       string                              `json:"failureDomain"`
	OperationsNote      string                              `json:"operationsNote"`
	PublicEndpoint      string                              `json:"publicEndpoint"`
	ListenerPort        int                                 `json:"listenerPort"`
	DeploymentChecklist relaymanagement.DeploymentChecklist `json:"deploymentChecklist"`
	ExpectedVersion     int64                               `json:"expectedVersion"`
}

type createRelayInstallSessionRequest struct {
	ReleaseID uuid.UUID `json:"releaseId"`
	Mode      string    `json:"mode"`
	Action    string    `json:"action"`
}

type activateRelayInstallationRequest struct {
	ExpectedThumbprint  string                              `json:"expectedThumbprint"`
	DeploymentChecklist relaymanagement.DeploymentChecklist `json:"deploymentChecklist"`
	Confirmation        string                              `json:"confirmation"`
}

type confirmationRequest struct {
	Confirmation string `json:"confirmation"`
}

type drainRelayRequest struct {
	Confirmation string `json:"confirmation"`
}

type migrateRelayUserRequest struct {
	Mode         string     `json:"mode"`
	TargetCellID *uuid.UUID `json:"targetCellId"`
	Confirmation string     `json:"confirmation"`
}

type relayNodeResponse struct {
	ID                  uuid.UUID `json:"id"`
	CellID              uuid.UUID `json:"cellId"`
	Status              string    `json:"status"`
	Version             string    `json:"version"`
	Protocols           []string  `json:"protocols"`
	ActiveConnections   int64     `json:"activeConnections"`
	ActiveFileTransfers int64     `json:"activeFileTransfers"`
	MemoryBytes         int64     `json:"memoryBytes"`
	IngressMbps         float64   `json:"ingressMbps"`
	EgressMbps          float64   `json:"egressMbps"`
	LastHeartbeatAt     time.Time `json:"lastHeartbeatAt"`
}

func relayNodeView(node relaymanagement.NodeInstance) relayNodeResponse {
	status := "offline"
	switch node.Status {
	case "starting":
		status = "starting"
	case "ready":
		status = "active"
	case "draining":
		status = "draining"
	}
	return relayNodeResponse{
		ID: node.ID, CellID: node.CellID, Status: status, Version: node.Version,
		Protocols:         []string{fmt.Sprintf("relay/%d", node.ProtocolVersion)},
		ActiveConnections: node.ActiveConnections, ActiveFileTransfers: node.ActiveFileTransfers,
		MemoryBytes: node.MemoryBytes, IngressMbps: node.IngressMbps, EgressMbps: node.EgressMbps,
		LastHeartbeatAt: node.LastHeartbeatAt,
	}
}

func preferredRelayCellID(topology []relaymanagement.Region, regionCode, poolCode, cellCode string) (uuid.UUID, bool) {
	regionCode, poolCode, cellCode = strings.TrimSpace(regionCode), strings.TrimSpace(poolCode), strings.TrimSpace(cellCode)
	var fallback uuid.UUID
	for _, region := range topology {
		for _, pool := range region.Pools {
			for _, cell := range pool.Cells {
				if cell.ID == uuid.Nil || cell.Status == "disabled" {
					continue
				}
				if fallback == uuid.Nil {
					fallback = cell.ID
				}
				if region.Code == regionCode && pool.Code == poolCode && cell.Code == cellCode {
					return cell.ID, true
				}
			}
		}
	}
	return fallback, fallback != uuid.Nil
}

func registerRelayRoutes(group *gin.RouterGroup, service relaymanagement.Service, authService AuthService, config AuthHTTPConfig, publicBaseURL, directoryURL, artifactBaseURL, bootstrapAssetsDir, defaultRegion, defaultPool, defaultCell string) {
	bootstrap := group.Group("/relay/bootstrap")
	bootstrap.GET("/install.sh", relayBootstrapAsset(bootstrapAssetsDir, "install.sh"))
	bootstrap.GET("/install.sh.sha256", relayBootstrapAsset(bootstrapAssetsDir, "install.sh.sha256"))
	bootstrap.GET("/upgrade.sh", relayBootstrapAsset(bootstrapAssetsDir, "upgrade.sh"))
	bootstrap.GET("/upgrade.sh.sha256", relayBootstrapAsset(bootstrapAssetsDir, "upgrade.sh.sha256"))
	bootstrap.GET("/lib/common.sh", relayBootstrapAsset(bootstrapAssetsDir, "lib/common.sh"))
	bootstrap.GET("/darwin/install.sh", relayBootstrapAsset(bootstrapAssetsDir, "darwin/install.sh"))
	bootstrap.GET("/darwin/install.sh.sha256", relayBootstrapAsset(bootstrapAssetsDir, "darwin/install.sh.sha256"))
	bootstrap.GET("/darwin/upgrade.sh", relayBootstrapAsset(bootstrapAssetsDir, "darwin/upgrade.sh"))
	bootstrap.GET("/darwin/upgrade.sh.sha256", relayBootstrapAsset(bootstrapAssetsDir, "darwin/upgrade.sh.sha256"))
	bootstrap.GET("/darwin/lib/common.sh", relayBootstrapAsset(bootstrapAssetsDir, "darwin/lib/common.sh"))
	bootstrap.GET("/darwin/relayctl-amd64", relayBootstrapAsset(bootstrapAssetsDir, "darwin/relayctl-amd64"))
	bootstrap.GET("/darwin/relayctl-amd64.sha256", relayBootstrapAsset(bootstrapAssetsDir, "darwin/relayctl-amd64.sha256"))
	bootstrap.GET("/darwin/relayctl-arm64", relayBootstrapAsset(bootstrapAssetsDir, "darwin/relayctl-arm64"))
	bootstrap.GET("/darwin/relayctl-arm64.sha256", relayBootstrapAsset(bootstrapAssetsDir, "darwin/relayctl-arm64.sha256"))
	bootstrap.GET("/windows/Install.ps1", relayBootstrapAsset(bootstrapAssetsDir, "windows/Install.ps1"))
	bootstrap.GET("/windows/Install.ps1.sha256", relayBootstrapAsset(bootstrapAssetsDir, "windows/Install.ps1.sha256"))
	bootstrap.GET("/windows/Upgrade.ps1", relayBootstrapAsset(bootstrapAssetsDir, "windows/Upgrade.ps1"))
	bootstrap.GET("/windows/Upgrade.ps1.sha256", relayBootstrapAsset(bootstrapAssetsDir, "windows/Upgrade.ps1.sha256"))
	bootstrap.GET("/windows/lib/RelayCommon.psm1", relayBootstrapAsset(bootstrapAssetsDir, "windows/lib/RelayCommon.psm1"))
	bootstrap.GET("/windows/relayctl-amd64.exe", relayBootstrapAsset(bootstrapAssetsDir, "windows/relayctl-amd64.exe"))
	bootstrap.GET("/windows/relayctl-amd64.exe.sha256", relayBootstrapAsset(bootstrapAssetsDir, "windows/relayctl-amd64.exe.sha256"))
	bootstrap.GET("/windows/relayctl-arm64.exe", relayBootstrapAsset(bootstrapAssetsDir, "windows/relayctl-arm64.exe"))
	bootstrap.GET("/windows/relayctl-arm64.exe.sha256", relayBootstrapAsset(bootstrapAssetsDir, "windows/relayctl-arm64.exe.sha256"))
	bootstrap.GET("/release-signing-public-key.pem", relayBootstrapAsset(bootstrapAssetsDir, "release-signing-public-key.pem"))
	bootstrap.GET("/node-installations/:installationId", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if service == nil || c.Request.URL.RawQuery != "" || c.GetHeader("Cookie") != "" {
			writeProblem(c, http.StatusNotFound, "relay_installation_not_found", "待注册节点不存在", "请检查节点安装编号。")
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		metadata, err := service.GetBootstrapInstallation(c.Request.Context(), installationID)
		if err != nil {
			writeProblem(c, http.StatusNotFound, "relay_installation_not_found", "待注册节点不存在", "请检查节点安装编号或生成新的一次性注册令牌。")
			return
		}
		c.JSON(http.StatusOK, metadata)
	})
	bootstrap.POST("/enrollments", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		if service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "relay_unavailable", "中继节点注册暂不可用", "请稍后重试。")
			return
		}
		if c.Request.URL.RawQuery != "" || c.GetHeader("Cookie") != "" {
			writeProblem(c, http.StatusBadRequest, "relay_enrollment_transport_invalid", "注册请求传输方式无效", "请勿在 URL 或 Cookie 中传递注册凭据。")
			return
		}
		token, ok := enrollmentAuthorization(c.GetHeader("Authorization"))
		if !ok {
			writeProblem(c, http.StatusUnauthorized, "relay_enrollment_invalid", "注册凭据无效", "请生成新的一次性注册令牌后重试。")
			return
		}
		var request relaymanagement.EnrollmentRequest
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.Enroll(c.Request.Context(), token, request)
		if err != nil {
			if errors.Is(err, relaymanagement.ErrEnrollmentInvalid) || errors.Is(err, relaymanagement.ErrEnrollmentExpired) ||
				errors.Is(err, relaymanagement.ErrEnrollmentConsumed) || errors.Is(err, relaymanagement.ErrIdentityMismatch) {
				writeProblem(c, http.StatusUnauthorized, "relay_enrollment_invalid", "注册凭据无效", "请生成新的一次性注册令牌后重试。")
				return
			}
			writeRelayProblem(c, err)
			return
		}
		result.DirectoryURL = directoryURL
		c.JSON(http.StatusCreated, result)
	})

	agent := group.Group("/relay/agent")
	agent.GET("/configuration", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		key, ok := relayAccessKeyRequest(c)
		if !ok || service == nil {
			writeRelayAgentProblem(c, relaymanagement.ErrAccessKeyInvalid)
			return
		}
		binding, err := service.ResolveAccessKey(c.Request.Context(), key)
		if err != nil {
			writeRelayAgentProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, binding)
	})
	agent.POST("/instances", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		key, ok := relayAccessKeyRequest(c)
		if !ok || service == nil {
			writeRelayAgentProblem(c, relaymanagement.ErrAccessKeyInvalid)
			return
		}
		var request relaymanagement.RegisterInstanceInput
		if !decodeJSON(c, &request) {
			return
		}
		instance, err := service.RegisterInstanceWithAccessKey(c.Request.Context(), key, request)
		if err != nil {
			writeRelayAgentProblem(c, err)
			return
		}
		c.JSON(http.StatusCreated, instance)
	})
	agent.POST("/instances/:instanceId/heartbeats", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		key, ok := relayAccessKeyRequest(c)
		if !ok || service == nil {
			writeRelayAgentProblem(c, relaymanagement.ErrAccessKeyInvalid)
			return
		}
		instanceID, ok := parseRelayUUID(c, "instanceId", "relay_instance_id_invalid", "中继进程编号无效")
		if !ok {
			return
		}
		var request relaymanagement.HeartbeatInput
		if !decodeJSON(c, &request) {
			return
		}
		if request.InstanceID != uuid.Nil && request.InstanceID != instanceID {
			writeRelayAgentProblem(c, relaymanagement.ErrInvalidInput)
			return
		}
		request.InstanceID = instanceID
		result, err := service.HeartbeatWithAccessKey(c.Request.Context(), key, request)
		if err != nil {
			writeRelayAgentProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	agent.DELETE("/instances/:instanceId", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		key, ok := relayAccessKeyRequest(c)
		if !ok || service == nil {
			writeRelayAgentProblem(c, relaymanagement.ErrAccessKeyInvalid)
			return
		}
		instanceID, ok := parseRelayUUID(c, "instanceId", "relay_instance_id_invalid", "中继进程编号无效")
		if !ok {
			return
		}
		if err := service.UnregisterInstanceWithAccessKey(c.Request.Context(), key, instanceID); err != nil {
			writeRelayAgentProblem(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	admin := group.Group("/admin/relay")
	admin.Use(requireSession(authService, config), RequirePermission(auth.PermissionAdminRelay, !config.DisableAdminMFA))

	admin.GET("/regions", func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		items, err := service.ListTopology(c.Request.Context())
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		defaultCellID, ok := preferredRelayCellID(items, defaultRegion, defaultPool, defaultCell)
		if !ok {
			writeProblem(c, http.StatusServiceUnavailable, "relay_internal_route_unavailable", "中继内部路由不可用", "请确认数据库迁移已经完成。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items, "defaultCellId": defaultCellID})
	})

	admin.GET("/node-installations", func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		var cellID *uuid.UUID
		if raw := strings.TrimSpace(c.Query("cellId")); raw != "" {
			parsed, err := uuid.Parse(raw)
			if err != nil {
				writeProblem(c, http.StatusBadRequest, "relay_cell_id_invalid", "中继组编号无效", "请刷新拓扑后重试。")
				return
			}
			cellID = &parsed
		}
		items, err := service.ListInstallations(c.Request.Context(), cellID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	admin.POST("/node-installations", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		var request createRelayInstallationRequest
		if !decodeJSON(c, &request) {
			return
		}
		// Cell/Pool are internal routing details. Prefer the configured route
		// when it exists and otherwise let the store select the first available
		// internal Cell. The admin client never needs to initialize or submit it.
		var cellID uuid.UUID
		if topology, err := service.ListTopology(c.Request.Context()); err == nil {
			cellID, _ = preferredRelayCellID(topology, defaultRegion, defaultPool, defaultCell)
		}
		session, _ := authSessionFrom(c)
		created, err := service.CreateInstallation(c.Request.Context(), relaymanagement.CreateInstallationInput{
			CellID: cellID, ReleaseID: request.ReleaseID, DisplayName: request.DisplayName,
			Region: request.Region, Group: request.Group, FailureDomain: request.FailureDomain,
			OperationsNote: request.OperationsNote, PublicEndpoint: request.PublicEndpoint, ListenerPort: request.ListenerPort,
			Platform: request.Platform, Architecture: request.Architecture, ActorUserID: session.User.ID,
		})
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusCreated, created)
	})

	admin.POST("/cells/:cellId/node-installations", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		cellID, ok := parseRelayUUID(c, "cellId", "relay_cell_id_invalid", "中继组编号无效")
		if !ok {
			return
		}
		var request createRelayInstallationRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		created, err := service.CreateInstallation(c.Request.Context(), relaymanagement.CreateInstallationInput{
			CellID: cellID, ReleaseID: request.ReleaseID, DisplayName: request.DisplayName,
			Region: request.Region, Group: request.Group, FailureDomain: request.FailureDomain,
			OperationsNote: request.OperationsNote, PublicEndpoint: request.PublicEndpoint, ListenerPort: request.ListenerPort,
			Platform: request.Platform, Architecture: request.Architecture, ActorUserID: session.User.ID,
		})
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusCreated, created)
	})

	admin.GET("/node-installations/:installationId", func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		item, err := service.GetInstallation(c.Request.Context(), installationID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, item)
	})

	admin.PATCH("/node-installations/:installationId", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		var request updateRelayInstallationRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		updated, err := service.UpdateInstallation(c.Request.Context(), installationID, relaymanagement.UpdateInstallationInput{
			DisplayName: request.DisplayName, Region: request.Region, Group: request.Group, FailureDomain: request.FailureDomain,
			OperationsNote: request.OperationsNote, PublicEndpoint: request.PublicEndpoint, ListenerPort: request.ListenerPort,
			DeploymentChecklist: request.DeploymentChecklist,
			ExpectedVersion:     request.ExpectedVersion, ActorUserID: session.User.ID,
		})
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, updated)
	})

	admin.DELETE("/node-installations/:installationId", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		if err := service.DeleteInstallation(c.Request.Context(), installationID, session.User.ID); err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	admin.POST("/node-installations/:installationId/enrollment-tokens", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		token, err := service.CreateEnrollmentToken(c.Request.Context(), installationID, session.User.ID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.JSON(http.StatusCreated, token)
	})

	admin.POST("/node-installations/:installationId/access-keys", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		key, err := service.CreateAccessKey(c.Request.Context(), installationID, session.User.ID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.JSON(http.StatusCreated, key)
	})

	admin.POST("/node-installations/:installationId/install-sessions", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		if strings.TrimSpace(bootstrapAssetsDir) == "" {
			writeProblem(c, http.StatusServiceUnavailable, "relay_bootstrap_unavailable", "中继节点在线引导暂不可用", "Host 未配置完整且受信任的 Relay 引导资产，请安装正式签名包后重试。")
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		var request createRelayInstallSessionRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		created, err := service.CreateInstallSession(c.Request.Context(), relaymanagement.CreateInstallSessionInput{
			InstallationID: installationID, ReleaseID: request.ReleaseID, Mode: request.Mode, Action: request.Action, ActorUserID: session.User.ID,
		})
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		artifact, err := service.GetBootstrapReleaseArtifact(c.Request.Context(), installationID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		baseURL := strings.TrimRight(publicBaseURL, "/")
		command, commandErr := relayInstallSessionCommand(
			artifact, installationID, request.Mode, request.Action, baseURL, artifactBaseURL,
		)
		if commandErr != nil {
			writeProblem(c, http.StatusServiceUnavailable, "relay_artifact_unavailable", "中继节点安装包暂不可用", commandErr.Error())
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusCreated, gin.H{"session": created, "installCommand": command})
	})

	admin.POST("/node-installations/:installationId/activations", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		var request activateRelayInstallationRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		activated, err := service.ActivateInstallation(c.Request.Context(), installationID, relaymanagement.ActivateInstallationInput{
			ExpectedThumbprint: request.ExpectedThumbprint, Checklist: request.DeploymentChecklist,
			Confirmation: request.Confirmation, ActorUserID: session.User.ID,
		})
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, activated)
	})

	admin.POST("/node-installations/:installationId/revocations", requireCSRF(config), func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		installationID, ok := parseRelayUUID(c, "installationId", "relay_installation_id_invalid", "节点安装编号无效")
		if !ok {
			return
		}
		var request confirmationRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		if err := service.RevokeInstallation(c.Request.Context(), installationID, session.User.ID, request.Confirmation); err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	admin.GET("/releases", func(c *gin.Context) {
		if !relayServiceAvailable(c, service) {
			return
		}
		var (
			items []relaymanagement.Release
			err   error
		)
		if releases, ok := service.(relaymanagement.ReleaseAdminService); ok {
			items, err = releases.ListManagedReleases(c.Request.Context())
		} else {
			items, err = service.ListReleases(c.Request.Context())
		}
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	admin.POST("/releases", requireCSRF(config), func(c *gin.Context) {
		releases, ok := relayReleaseAdminService(c, service)
		if !ok {
			return
		}
		var input relaymanagement.SaveReleaseInput
		if !decodeJSON(c, &input) {
			return
		}
		session, _ := authSessionFrom(c)
		input.ActorUserID = session.User.ID
		created, err := releases.CreateRelease(c.Request.Context(), input)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusCreated, created)
	})

	admin.PUT("/releases/:releaseId", requireCSRF(config), func(c *gin.Context) {
		releases, ok := relayReleaseAdminService(c, service)
		if !ok {
			return
		}
		releaseID, parsed := parseRelayUUID(c, "releaseId", "relay_release_id_invalid", "中继程序版本编号无效")
		if !parsed {
			return
		}
		var input relaymanagement.SaveReleaseInput
		if !decodeJSON(c, &input) {
			return
		}
		session, _ := authSessionFrom(c)
		input.ActorUserID = session.User.ID
		updated, err := releases.UpdateRelease(c.Request.Context(), releaseID, input)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, updated)
	})

	admin.POST("/releases/:releaseId/publications", requireCSRF(config), func(c *gin.Context) {
		releases, ok := relayReleaseAdminService(c, service)
		if !ok {
			return
		}
		releaseID, parsed := parseRelayUUID(c, "releaseId", "relay_release_id_invalid", "中继程序版本编号无效")
		if !parsed {
			return
		}
		session, _ := authSessionFrom(c)
		published, err := releases.PublishRelease(c.Request.Context(), releaseID, session.User.ID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, published)
	})

	admin.POST("/releases/:releaseId/retirements", requireCSRF(config), func(c *gin.Context) {
		releases, ok := relayReleaseAdminService(c, service)
		if !ok {
			return
		}
		releaseID, parsed := parseRelayUUID(c, "releaseId", "relay_release_id_invalid", "中继程序版本编号无效")
		if !parsed {
			return
		}
		session, _ := authSessionFrom(c)
		retired, err := releases.RetireRelease(c.Request.Context(), releaseID, session.User.ID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, retired)
	})

	admin.DELETE("/releases/:releaseId", requireCSRF(config), func(c *gin.Context) {
		releases, ok := relayReleaseAdminService(c, service)
		if !ok {
			return
		}
		releaseID, parsed := parseRelayUUID(c, "releaseId", "relay_release_id_invalid", "中继程序版本编号无效")
		if !parsed {
			return
		}
		session, _ := authSessionFrom(c)
		if err := releases.DeleteRelease(c.Request.Context(), releaseID, session.User.ID); err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	admin.GET("/cells/:cellId/nodes", func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		cellID, parsed := parseRelayUUID(c, "cellId", "relay_cell_id_invalid", "中继组编号无效")
		if !parsed {
			return
		}
		items, err := advanced.ListNodes(c.Request.Context(), cellID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		views := make([]relayNodeResponse, 0, len(items))
		for _, item := range items {
			views = append(views, relayNodeView(item))
		}
		c.JSON(http.StatusOK, gin.H{"items": views, "observedAt": time.Now().UTC()})
	})

	admin.GET("/cells/:cellId/endpoints", func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		cellID, parsed := parseRelayUUID(c, "cellId", "relay_cell_id_invalid", "中继组编号无效")
		if !parsed {
			return
		}
		items, err := advanced.ListEndpoints(c.Request.Context(), cellID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	admin.PATCH("/cells/:cellId", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		cellID, parsed := parseRelayUUID(c, "cellId", "relay_cell_id_invalid", "中继组编号无效")
		if !parsed {
			return
		}
		var request relaymanagement.UpdateCellInput
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		request.ActorUserID, request.IdempotencyKey = session.User.ID, c.GetHeader("Idempotency-Key")
		operation, err := advanced.RequestCellUpdate(c.Request.Context(), cellID, request)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.POST("/cells/:cellId/endpoints", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		cellID, parsed := parseRelayUUID(c, "cellId", "relay_cell_id_invalid", "中继组编号无效")
		if !parsed {
			return
		}
		var request relaymanagement.CreateEndpointInput
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		request.CellID, request.ActorUserID, request.IdempotencyKey = cellID, session.User.ID, c.GetHeader("Idempotency-Key")
		operation, err := advanced.CreateEndpoint(c.Request.Context(), request)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.PATCH("/endpoints/:endpointId", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		endpointID, parsed := parseRelayUUID(c, "endpointId", "relay_endpoint_id_invalid", "公网接入地址编号无效")
		if !parsed {
			return
		}
		var request relaymanagement.UpdateEndpointInput
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		request.ActorUserID, request.IdempotencyKey = session.User.ID, c.GetHeader("Idempotency-Key")
		operation, err := advanced.UpdateEndpoint(c.Request.Context(), endpointID, request)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.POST("/endpoints/:endpointId/validations", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		endpointID, parsed := parseRelayUUID(c, "endpointId", "relay_endpoint_id_invalid", "公网接入地址编号无效")
		if !parsed {
			return
		}
		session, _ := authSessionFrom(c)
		operation, err := advanced.RequestEndpointValidation(c.Request.Context(), endpointID, session.User.ID, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.POST("/endpoints/:endpointId/activations", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		endpointID, parsed := parseRelayUUID(c, "endpointId", "relay_endpoint_id_invalid", "公网接入地址编号无效")
		if !parsed {
			return
		}
		var request confirmationRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.Confirmation != "activate_relay_endpoint" {
			writeProblem(c, http.StatusBadRequest, "relay_confirmation_invalid", "确认文本无效", "请重新确认公网接入地址启用操作。")
			return
		}
		session, _ := authSessionFrom(c)
		operation, err := advanced.RequestEndpointActivation(c.Request.Context(), endpointID, session.User.ID, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.POST("/nodes/:nodeId/drain-operations", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		nodeID, parsed := parseRelayUUID(c, "nodeId", "relay_node_id_invalid", "中继节点编号无效")
		if !parsed {
			return
		}
		var request drainRelayRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.Confirmation != "drain_relay_node" {
			writeProblem(c, http.StatusBadRequest, "relay_confirmation_invalid", "确认文本无效", "请重新确认中继节点排空操作。")
			return
		}
		session, _ := authSessionFrom(c)
		operation, err := advanced.RequestNodeDrain(c.Request.Context(), nodeID, session.User.ID, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.POST("/cells/:cellId/drain-operations", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		cellID, parsed := parseRelayUUID(c, "cellId", "relay_cell_id_invalid", "中继组编号无效")
		if !parsed {
			return
		}
		var request drainRelayRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.Confirmation != "drain_relay_cell" {
			writeProblem(c, http.StatusBadRequest, "relay_confirmation_invalid", "确认文本无效", "请重新确认中继组排空操作。")
			return
		}
		session, _ := authSessionFrom(c)
		operation, err := advanced.RequestCellDrain(c.Request.Context(), cellID, session.User.ID, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.GET("/assignments", func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		userID, err := uuid.Parse(strings.TrimSpace(c.Query("userId")))
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "relay_user_id_invalid", "用户标识无效", "请输入有效用户 ID。")
			return
		}
		items, err := advanced.ListAssignments(c.Request.Context(), userID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	admin.POST("/users/:userId/migration-operations", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		userID, parsed := parseRelayUUID(c, "userId", "relay_user_id_invalid", "用户标识无效")
		if !parsed {
			return
		}
		var request migrateRelayUserRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		operation, err := advanced.RequestUserMigration(c.Request.Context(), relaymanagement.MigrateUserInput{
			UserID: userID, Mode: request.Mode, TargetCellID: request.TargetCellID, Confirmation: request.Confirmation,
			ActorUserID: session.User.ID, IdempotencyKey: c.GetHeader("Idempotency-Key"),
		})
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.DELETE("/users/:userId/pin", requireCSRF(config), func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		userID, parsed := parseRelayUUID(c, "userId", "relay_user_id_invalid", "用户标识无效")
		if !parsed {
			return
		}
		session, _ := authSessionFrom(c)
		operation, err := advanced.RequestUserUnpin(c.Request.Context(), userID, session.User.ID, c.GetHeader("Idempotency-Key"))
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		writeAcceptedRelayOperation(c, operation)
	})

	admin.GET("/operations/:operationId", func(c *gin.Context) {
		advanced, ok := advancedRelayService(c, service)
		if !ok {
			return
		}
		operationID, parsed := parseRelayUUID(c, "operationId", "relay_operation_id_invalid", "异步操作编号无效")
		if !parsed {
			return
		}
		operation, err := advanced.GetOperation(c.Request.Context(), operationID)
		if err != nil {
			writeRelayProblem(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"operation": operation})
	})
}

func relayBootstrapAsset(directory, name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if c.Request.URL.RawQuery != "" || strings.TrimSpace(directory) == "" {
			c.Status(http.StatusNotFound)
			return
		}
		allowed := map[string]string{
			"install.sh": "install.sh", "upgrade.sh": "upgrade.sh", "lib/common.sh": filepath.Join("lib", "common.sh"),
			"darwin/install.sh": filepath.Join("darwin", "install.sh"), "darwin/upgrade.sh": filepath.Join("darwin", "upgrade.sh"),
			"darwin/lib/common.sh":  filepath.Join("darwin", "lib", "common.sh"),
			"darwin/relayctl-amd64": filepath.Join("darwin", "relayctl-amd64"), "darwin/relayctl-arm64": filepath.Join("darwin", "relayctl-arm64"),
			"windows/Install.ps1": filepath.Join("windows", "Install.ps1"), "windows/Upgrade.ps1": filepath.Join("windows", "Upgrade.ps1"),
			"windows/lib/RelayCommon.psm1":   filepath.Join("windows", "lib", "RelayCommon.psm1"),
			"windows/relayctl-amd64.exe":     filepath.Join("windows", "relayctl-amd64.exe"),
			"windows/relayctl-arm64.exe":     filepath.Join("windows", "relayctl-arm64.exe"),
			"release-signing-public-key.pem": "release-signing-public-key.pem",
		}
		if strings.HasSuffix(name, ".sha256") {
			sourceName := strings.TrimSuffix(name, ".sha256")
			relative, ok := allowed[sourceName]
			if !ok {
				c.Status(http.StatusNotFound)
				return
			}
			maximumBytes := 1 << 20
			if strings.Contains(filepath.Base(sourceName), "relayctl-") {
				maximumBytes = 64 << 20
			}
			contents, err := os.ReadFile(filepath.Join(directory, relative))
			if err != nil || len(contents) > maximumBytes {
				c.Status(http.StatusNotFound)
				return
			}
			digest := sha256.Sum256(contents)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(hex.EncodeToString(digest[:])+"  "+filepath.Base(sourceName)+"\n"))
			return
		}
		relative, ok := allowed[name]
		if !ok {
			c.Status(http.StatusNotFound)
			return
		}
		maximumBytes := 1 << 20
		if strings.Contains(filepath.Base(name), "relayctl-") {
			maximumBytes = 64 << 20
		}
		contents, err := os.ReadFile(filepath.Join(directory, relative))
		if err != nil || len(contents) > maximumBytes {
			c.Status(http.StatusNotFound)
			return
		}
		contentType := "text/plain; charset=utf-8"
		if strings.HasSuffix(name, ".sh") {
			contentType = "text/x-shellscript; charset=utf-8"
		} else if strings.HasSuffix(name, ".ps1") || strings.HasSuffix(name, ".psm1") {
			contentType = "text/plain; charset=utf-8"
		} else if strings.HasSuffix(name, ".pem") {
			contentType = "application/x-pem-file"
		} else if strings.HasSuffix(name, ".exe") || strings.Contains(filepath.Base(name), "relayctl-") {
			contentType = "application/octet-stream"
		}
		c.Data(http.StatusOK, contentType, contents)
	}
}

func relayArtifactURL(base, objectKey string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("Relay artifact base URL is invalid")
	}
	objectKey = strings.Trim(strings.TrimSpace(objectKey), "/")
	if objectKey == "" || strings.ContainsAny(objectKey, "\\?#'\" \t\r\n") {
		return "", errors.New("Relay artifact object key is invalid")
	}
	for _, segment := range strings.Split(objectKey, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", errors.New("Relay artifact object key is invalid")
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + objectKey
	return parsed.String(), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func powershellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func relayInstallSessionCommand(artifact relaymanagement.BootstrapReleaseArtifact, installationID uuid.UUID, mode, action, baseURL, artifactBaseURL string) (string, error) {
	platform := strings.ToLower(strings.TrimSpace(artifact.Platform))
	architecture := strings.ToLower(strings.TrimSpace(artifact.Architecture))
	if !relayrelease.SupportsTarget(platform, architecture) {
		return "", errors.New("中继程序版本的平台或架构不受支持。")
	}
	archiveName := filepath.Base(artifact.FileName)
	if archiveName == "." || archiveName != artifact.FileName || !strings.HasSuffix(strings.ToLower(archiveName), ".tar.gz") {
		return "", errors.New("中继程序版本附件的文件名不安全。")
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "install"
	}
	if action != "install" && action != "upgrade" {
		return "", errors.New("安装操作无效。")
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "script"
	}
	bootstrapURL := strings.TrimRight(baseURL, "/") + "/api/v1/relay/bootstrap"

	if mode == "manual" {
		switch platform {
		case relayrelease.PlatformLinux:
			return fmt.Sprintf("sudo /opt/wenzwork-relay/current/bin/relayctl enroll --control-url %s --installation-id %s --token-stdin", shellQuote(baseURL), shellQuote(installationID.String())), nil
		case relayrelease.PlatformDarwin:
			return fmt.Sprintf("sudo /usr/local/lib/wenzwork-relay/current/bin/relayctl enroll --control-url %s --installation-id %s --token-stdin", shellQuote(baseURL), shellQuote(installationID.String())), nil
		case relayrelease.PlatformWindows:
			return fmt.Sprintf("& (Join-Path $env:ProgramFiles 'WenzWork\\Relay\\current\\bin\\relayctl.exe') enroll --control-url %s --installation-id %s --token-stdin", powershellQuote(baseURL), powershellQuote(installationID.String())), nil
		}
	}

	if mode == "download" {
		switch platform {
		case relayrelease.PlatformLinux:
			if action == "upgrade" {
				return fmt.Sprintf("sudo ./scripts/upgrade.sh --package-file %s --checksums-file ./SHA256SUMS --checksums-signature-file ./SHA256SUMS.sig --signing-key-file ./release-signing-public-key.pem", shellQuote("./"+archiveName)), nil
			}
			return fmt.Sprintf("sudo ./scripts/install.sh --management-url %s --package-file %s --checksums-file ./SHA256SUMS --checksums-signature-file ./SHA256SUMS.sig --signing-key-file ./release-signing-public-key.pem --access-key-stdin", shellQuote(baseURL), shellQuote("./"+archiveName)), nil
		case relayrelease.PlatformDarwin:
			if action == "upgrade" {
				return fmt.Sprintf("sudo ./scripts/upgrade.sh --package-file %s --checksums-file ./SHA256SUMS --checksums-signature-file ./SHA256SUMS.sig --signing-key-file ./release-signing-public-key.pem", shellQuote("./"+archiveName)), nil
			}
			verifierURL := bootstrapURL + "/darwin/relayctl-" + architecture
			return fmt.Sprintf("verifier_sha=$(%s %s | awk 'NR == 1 {print $1}') && sudo ./scripts/install.sh --management-url %s --package-file %s --checksums-file ./SHA256SUMS --checksums-signature-file ./SHA256SUMS.sig --signing-key-file ./release-signing-public-key.pem --verifier-url %s --verifier-sha256 \"$verifier_sha\" --access-key-stdin",
				relayBootstrapCurl(), shellQuote(verifierURL+".sha256"), shellQuote(baseURL), shellQuote("./"+archiveName), shellQuote(verifierURL)), nil
		case relayrelease.PlatformWindows:
			script := ".\\scripts\\Install.ps1"
			arguments := " -ManagementUrl " + powershellQuote(baseURL)
			if action == "upgrade" {
				script = ".\\scripts\\Upgrade.ps1"
				arguments = ""
				return "& powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File " + powershellQuote(script) +
					" -PackageFile " + powershellQuote(".\\"+archiveName) +
					" -ChecksumsFile '.\\SHA256SUMS' -ChecksumsSignatureFile '.\\SHA256SUMS.sig' -SigningKeyFile '.\\release-signing-public-key.pem'", nil
			}
			verifierName := "relayctl-" + architecture + ".exe"
			verifierURL := bootstrapURL + "/windows/" + verifierName
			return "$verifierDir = Join-Path ([IO.Path]::GetTempPath()) ('wenzwork-relay-verifier-' + [guid]::NewGuid().ToString('N')); " +
				"[void](New-Item -ItemType Directory -Path $verifierDir); try { " +
				"Invoke-WebRequest -UseBasicParsing -Uri " + powershellQuote(verifierURL) + " -OutFile (Join-Path $verifierDir 'relayctl.exe'); " +
				"Invoke-WebRequest -UseBasicParsing -Uri " + powershellQuote(verifierURL+".sha256") + " -OutFile (Join-Path $verifierDir 'relayctl.exe.sha256'); " +
				"$expected = ((Get-Content -Raw -LiteralPath (Join-Path $verifierDir 'relayctl.exe.sha256')) -split '\\s+')[0].ToLowerInvariant(); " +
				"$actual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $verifierDir 'relayctl.exe')).Hash.ToLowerInvariant(); " +
				"if ($actual -ne $expected) { throw 'Relay bootstrap verifier SHA-256 mismatch' }; " +
				"& powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File " + powershellQuote(script) +
				" -PackageFile " + powershellQuote(".\\"+archiveName) +
				" -ChecksumsFile '.\\SHA256SUMS' -ChecksumsSignatureFile '.\\SHA256SUMS.sig' -SigningKeyFile '.\\release-signing-public-key.pem'" +
				" -VerifierFile (Join-Path $verifierDir 'relayctl.exe') -VerifierSha256 $actual" + arguments +
				"; if ($LASTEXITCODE -ne 0) { throw 'Relay install failed' } } finally { Remove-Item -LiteralPath $verifierDir -Recurse -Force }", nil
		}
	}
	if mode != "script" {
		return "", errors.New("安装模式无效。")
	}

	artifactURL, err := relayArtifactURL(artifactBaseURL, artifact.ObjectKey)
	if err != nil {
		return "", errors.New("请检查中继程序版本附件的存储配置。")
	}
	artifactDirectory := artifactURL[:strings.LastIndex(artifactURL, "/")]
	switch platform {
	case relayrelease.PlatformLinux:
		return relayUnixBootstrapCommand(
			"", action, architecture, baseURL, bootstrapURL, artifactURL, artifactDirectory, false,
		), nil
	case relayrelease.PlatformDarwin:
		return relayUnixBootstrapCommand(
			"darwin/", action, architecture, baseURL, bootstrapURL, artifactURL, artifactDirectory, true,
		), nil
	case relayrelease.PlatformWindows:
		return relayWindowsBootstrapCommand(action, architecture, baseURL, bootstrapURL, artifactURL, artifactDirectory), nil
	default:
		return "", errors.New("中继程序版本的平台不受支持。")
	}
}

func relayUnixBootstrapCommand(prefix, action, architecture, baseURL, bootstrapURL, artifactURL, artifactDirectory string, darwin bool) string {
	scriptName := "install.sh"
	if action == "upgrade" {
		scriptName = "upgrade.sh"
	}
	download := func(destination, source string) string {
		return relayBootstrapCurl() + " --output " + destination + " " + shellQuote(source)
	}
	checksumCommand := "sha256sum -c " + scriptName + ".sha256"
	if darwin {
		checksumCommand = "shasum -a 256 -c " + scriptName + ".sha256"
	}
	parts := []string{
		`bootstrap_dir=$(mktemp -d /tmp/wenzwork-relay-bootstrap.XXXXXX)`,
		`install -d -m 0700 "$bootstrap_dir/lib"`,
		download(`"$bootstrap_dir/`+scriptName+`"`, bootstrapURL+"/"+prefix+scriptName),
		download(`"$bootstrap_dir/`+scriptName+`.sha256"`, bootstrapURL+"/"+prefix+scriptName+".sha256"),
		download(`"$bootstrap_dir/lib/common.sh"`, bootstrapURL+"/"+prefix+"lib/common.sh"),
		download(`"$bootstrap_dir/release-signing-public-key.pem"`, bootstrapURL+"/release-signing-public-key.pem"),
		`(cd "$bootstrap_dir" && ` + checksumCommand + `)`,
	}
	verifierArguments := ""
	if darwin && action == "install" {
		verifierURL := bootstrapURL + "/darwin/relayctl-" + architecture
		parts = append(parts,
			download(`"$bootstrap_dir/relayctl.sha256"`, verifierURL+".sha256"),
			`verifier_sha=$(awk 'NR == 1 {print $1}' "$bootstrap_dir/relayctl.sha256")`,
		)
		verifierArguments = " --verifier-url " + shellQuote(verifierURL) + " --verifier-sha256 \"$verifier_sha\""
	}
	parts = append(parts,
		"sudo bash \"$bootstrap_dir/"+scriptName+"\" --artifact-url "+shellQuote(artifactURL)+
			" --checksums-url "+shellQuote(artifactDirectory+"/SHA256SUMS")+
			" --checksums-signature-url "+shellQuote(artifactDirectory+"/SHA256SUMS.sig")+
			" --signing-key-file \"$bootstrap_dir/release-signing-public-key.pem\""+
			verifierArguments+relayInstallCommandSuffix(action, baseURL),
	)
	return strings.Join(parts, " && ")
}

func relayWindowsBootstrapCommand(action, architecture, baseURL, bootstrapURL, artifactURL, artifactDirectory string) string {
	scriptName := "Install.ps1"
	if action == "upgrade" {
		scriptName = "Upgrade.ps1"
	}
	download := func(source, destination string) string {
		return "Invoke-WebRequest -UseBasicParsing -Uri " + powershellQuote(source) + " -OutFile " + destination
	}
	parts := []string{
		"$ErrorActionPreference = 'Stop'",
		"$ProgressPreference = 'SilentlyContinue'",
		"$bootstrapDir = Join-Path ([IO.Path]::GetTempPath()) ('wenzwork-relay-bootstrap-' + [guid]::NewGuid().ToString('N'))",
		"[void](New-Item -ItemType Directory -Path (Join-Path $bootstrapDir 'lib') -Force)",
		"try {",
		download(bootstrapURL+"/windows/"+scriptName, "(Join-Path $bootstrapDir '"+scriptName+"')"),
		download(bootstrapURL+"/windows/"+scriptName+".sha256", "(Join-Path $bootstrapDir '"+scriptName+".sha256')"),
		download(bootstrapURL+"/windows/lib/RelayCommon.psm1", "(Join-Path $bootstrapDir 'lib\\RelayCommon.psm1')"),
		download(bootstrapURL+"/release-signing-public-key.pem", "(Join-Path $bootstrapDir 'release-signing-public-key.pem')"),
		"$scriptExpected = ((Get-Content -Raw -LiteralPath (Join-Path $bootstrapDir '" + scriptName + ".sha256')) -split '\\s+')[0].ToLowerInvariant()",
		"$scriptActual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $bootstrapDir '" + scriptName + "')).Hash.ToLowerInvariant()",
		"if ($scriptActual -ne $scriptExpected) { throw 'Relay bootstrap script SHA-256 mismatch' }",
	}
	verifierArguments := ""
	if action == "install" {
		verifierName := "relayctl-" + architecture + ".exe"
		parts = append(parts,
			download(bootstrapURL+"/windows/"+verifierName, "(Join-Path $bootstrapDir 'relayctl.exe')"),
			download(bootstrapURL+"/windows/"+verifierName+".sha256", "(Join-Path $bootstrapDir 'relayctl.exe.sha256')"),
			"$verifierExpected = ((Get-Content -Raw -LiteralPath (Join-Path $bootstrapDir 'relayctl.exe.sha256')) -split '\\s+')[0].ToLowerInvariant()",
			"$verifierActual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $bootstrapDir 'relayctl.exe')).Hash.ToLowerInvariant()",
			"if ($verifierActual -ne $verifierExpected) { throw 'Relay bootstrap verifier SHA-256 mismatch' }",
		)
		verifierArguments = " -VerifierFile (Join-Path $bootstrapDir 'relayctl.exe') -VerifierSha256 $verifierActual"
	}
	arguments := " -ArtifactUrl " + powershellQuote(artifactURL) +
		" -ChecksumsUrl " + powershellQuote(artifactDirectory+"/SHA256SUMS") +
		" -ChecksumsSignatureUrl " + powershellQuote(artifactDirectory+"/SHA256SUMS.sig") +
		" -SigningKeyFile (Join-Path $bootstrapDir 'release-signing-public-key.pem')" + verifierArguments
	if action == "install" {
		arguments += " -ManagementUrl " + powershellQuote(baseURL)
	}
	parts = append(parts,
		"& powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File (Join-Path $bootstrapDir '"+scriptName+"')"+arguments,
		"if ($LASTEXITCODE -ne 0) { throw 'Relay bootstrap script failed' }",
		"} finally { if (Test-Path -LiteralPath $bootstrapDir) { Remove-Item -LiteralPath $bootstrapDir -Recurse -Force } }",
	)
	return strings.Join(parts, "; ")
}

func relayBootstrapCurl() string {
	return "curl --fail --silent --show-error --location --proto '=http,https' --proto-redir '=http,https' --tlsv1.2 --connect-timeout 10 --max-time 60"
}

func relayInstallCommandSuffix(action, baseURL string) string {
	if action == "upgrade" {
		return ""
	}
	return " --management-url " + shellQuote(baseURL) + " --access-key-stdin"
}

func relayServiceAvailable(c *gin.Context, service relaymanagement.Service) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "relay_unavailable", "中继服务管理暂不可用", "请稍后重试。")
	return false
}

func advancedRelayService(c *gin.Context, service relaymanagement.Service) (relaymanagement.AdvancedService, bool) {
	if !relayServiceAvailable(c, service) {
		return nil, false
	}
	advanced, ok := service.(relaymanagement.AdvancedService)
	if !ok {
		writeProblem(c, http.StatusServiceUnavailable, "relay_host_maintenance_unavailable", "管理端中继维护功能暂不可用", "请稍后重试。")
		return nil, false
	}
	return advanced, true
}

func relayReleaseAdminService(c *gin.Context, service relaymanagement.Service) (relaymanagement.ReleaseAdminService, bool) {
	if !relayServiceAvailable(c, service) {
		return nil, false
	}
	releases, ok := service.(relaymanagement.ReleaseAdminService)
	if !ok {
		writeProblem(c, http.StatusServiceUnavailable, "relay_release_admin_unavailable", "中继程序版本管理暂不可用", "请稍后重试。")
		return nil, false
	}
	return releases, true
}

func writeAcceptedRelayOperation(c *gin.Context, operation relaymanagement.Operation) {
	c.Header("Location", "/api/v1/admin/relay/operations/"+operation.ID.String())
	c.JSON(http.StatusAccepted, gin.H{"operation": operation})
}

func parseRelayUUID(c *gin.Context, parameter, code, title string) (uuid.UUID, bool) {
	value, err := uuid.Parse(strings.TrimSpace(c.Param(parameter)))
	if err != nil {
		writeProblem(c, http.StatusBadRequest, code, title, "请刷新页面后重试。")
		return uuid.Nil, false
	}
	return value, true
}

func enrollmentAuthorization(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || parts[0] != "Enrollment" || len(parts[1]) < 43 || len(parts[1]) > 128 {
		return "", false
	}
	return parts[1], true
}

func relayAccessKeyRequest(c *gin.Context) (string, bool) {
	if c.Request.URL.RawQuery != "" || c.GetHeader("Cookie") != "" {
		return "", false
	}
	parts := strings.Fields(c.GetHeader("Authorization"))
	if len(parts) != 2 || parts[0] != "RelayKey" || len(parts[1]) != len("relay_")+43 {
		return "", false
	}
	return parts[1], true
}

func writeRelayAgentProblem(c *gin.Context, err error) {
	switch {
	case errors.Is(err, relaymanagement.ErrAccessKeyInvalid):
		writeProblem(c, http.StatusUnauthorized, "relay_access_key_invalid", "中继访问密钥无效", "请检查 RELAY_ACCESS_KEY 或在管理端重新生成密钥。")
	case errors.Is(err, relaymanagement.ErrInstallationRevoked):
		writeProblem(c, http.StatusForbidden, "relay_access_revoked", "中继主机已吊销", "该主机已被管理端吊销，不能继续连接。")
	case errors.Is(err, relaymanagement.ErrInvalidInput):
		writeProblem(c, http.StatusBadRequest, "relay_agent_input_invalid", "中继进程请求无效", "请检查 Relay 环境变量和程序版本。")
	case errors.Is(err, relaymanagement.ErrNotFound):
		writeProblem(c, http.StatusNotFound, "relay_agent_instance_not_found", "中继进程不存在", "请重新注册当前 Relay 进程。")
	case errors.Is(err, relaymanagement.ErrConflict):
		writeProblem(c, http.StatusConflict, "relay_agent_state_conflict", "中继进程状态冲突", "请重新连接管理端。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "relay_agent_unavailable", "中继管理连接暂不可用", "Relay 将自动重试。")
	}
}

func writeRelayProblem(c *gin.Context, err error) {
	switch {
	case errors.Is(err, relaymanagement.ErrInvalidInput):
		writeProblem(c, http.StatusBadRequest, "relay_input_invalid", "中继服务请求内容无效", "请检查字段和确认文本后重试。")
	case errors.Is(err, relaymanagement.ErrNotFound):
		writeProblem(c, http.StatusNotFound, "relay_resource_not_found", "中继服务资源不存在", "请刷新页面后重试。")
	case errors.Is(err, relaymanagement.ErrVersionConflict):
		writeProblem(c, http.StatusConflict, "relay_version_conflict", "中继服务记录已更新", "请刷新最新状态后重试。")
	case errors.Is(err, relaymanagement.ErrConflict), errors.Is(err, relaymanagement.ErrActivationBlocked):
		writeProblem(c, http.StatusConflict, "relay_state_conflict", "当前中继服务状态不允许此操作", "请检查注册、心跳和部署检查项。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "relay_unavailable", "中继服务暂不可用", "请稍后重试。")
	}
}
