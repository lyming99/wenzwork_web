package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/deviceaccesskey"
	"github.com/wenzwork/wenzwork-web/server/internal/relayallocation"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteaccesspolicy"
	"github.com/wenzwork/wenzwork-web/server/internal/remotedevice"
)

const remoteAppSessionKey = "remote_app_session"

type RemoteDeviceService interface {
	Register(context.Context, remotedevice.RegisterInput) (remotedevice.Registration, error)
}

type remoteDeviceDirectHeartbeatService interface {
	HeartbeatDirect(context.Context, remotedevice.DirectHeartbeatInput) (remotedevice.DirectHeartbeatResult, error)
}

type remoteDeviceLinkTrustService interface {
	DeviceLinkGrantTrust() relayallocation.DeviceLinkGrantTrustBundle
}

type RemoteDeviceBootstrapService interface {
	BootstrapAccessKey(context.Context, deviceaccesskey.BootstrapInput) (deviceaccesskey.BootstrapResult, error)
}

type RemoteDeviceKeyRotationService interface {
	RotateDeviceKey(context.Context, remotedevice.RotateKeyInput) (remotedevice.KeyRotation, error)
}

type RemoteAllocationService interface {
	Create(context.Context, relayallocation.CreateInput) (relayallocation.Result, error)
	Refresh(context.Context, relayallocation.RefreshInput) (relayallocation.Result, error)
}

type registerRemoteDeviceRequest struct {
	DeviceName            string   `json:"deviceName"`
	Platform              string   `json:"platform"`
	AgentVersion          string   `json:"agentVersion"`
	ProtocolMin           uint32   `json:"protocolMin"`
	ProtocolMax           uint32   `json:"protocolMax"`
	Capabilities          []string `json:"capabilities"`
	IdentityAlgorithm     string   `json:"identityAlgorithm"`
	IdentityPublicKey     string   `json:"identityPublicKey"`
	Proof                 string   `json:"proof"`
	DirectEnabled         bool     `json:"directEnabled"`
	DirectTLSEnabled      bool     `json:"directTlsEnabled"`
	DirectIP              string   `json:"directIp"`
	DirectPort            uint32   `json:"directPort"`
	DirectConnectionEpoch uint64   `json:"directConnectionEpoch"`
}

type directRemoteDeviceHeartbeatRequest struct {
	IP              string `json:"ip"`
	Port            uint32 `json:"port"`
	ConnectionEpoch uint64 `json:"connectionEpoch"`
	TLSEnabled      bool   `json:"tlsEnabled"`
}

type bootstrapRemoteDeviceRequest struct {
	DeviceID   uuid.UUID `json:"deviceId"`
	DeviceName string    `json:"deviceName"`
}

type rotateRemoteDeviceKeyRequest struct {
	ExpectedKeyVersion   uint64 `json:"expectedKeyVersion"`
	NewIdentityPublicKey string `json:"newIdentityPublicKey"`
	OldProof             string `json:"oldProof"`
	NewProof             string `json:"newProof"`
}

type rotateRemoteDeviceKeyResponse struct {
	DeviceID            uuid.UUID `json:"deviceId"`
	IdentityPublicKey   string    `json:"identityPublicKey"`
	PublicKeyThumbprint string    `json:"publicKeyThumbprint"`
	KeyVersion          uint64    `json:"keyVersion"`
	GrantVersion        uint64    `json:"grantVersion"`
	Rotated             bool      `json:"rotated"`
}

type remoteDeviceResponse struct {
	Device               remoteDeviceView                            `json:"device"`
	PublicKeyThumbprint  string                                      `json:"publicKeyThumbprint"`
	ApprovalRequired     bool                                        `json:"approvalRequired"`
	DeviceLinkGrantTrust *relayallocation.DeviceLinkGrantTrustBundle `json:"deviceLinkGrantTrust,omitempty"`
}

type remoteDeviceView struct {
	ID                   uuid.UUID  `json:"id"`
	InstallationDeviceID uuid.UUID  `json:"installationDeviceId"`
	DeviceName           string     `json:"deviceName"`
	Platform             string     `json:"platform"`
	AgentVersion         string     `json:"agentVersion"`
	Status               string     `json:"status"`
	Presence             string     `json:"presence"`
	Capabilities         []string   `json:"capabilities"`
	Scopes               []string   `json:"scopes"`
	GrantVersion         uint64     `json:"grantVersion"`
	KeyVersion           uint64     `json:"keyVersion"`
	LastSeenAt           *time.Time `json:"lastSeenAt"`
	LastSyncAt           *time.Time `json:"lastSyncAt"`
	RemoteEnabledAt      *time.Time `json:"remoteEnabledAt"`
	ConnectionMode       string     `json:"connectionMode"`
	DirectModeEnabled    bool       `json:"directModeEnabled"`
	DirectAvailable      bool       `json:"directAvailable"`
	DirectTLSEnabled     bool       `json:"directTlsEnabled"`
	DirectIP             *string    `json:"directIp"`
	DirectPort           *uint32    `json:"directPort"`
}

type relayAllocationRequest struct {
	RemoteDeviceID      uuid.UUID `json:"remoteDeviceId"`
	ProtocolMin         uint32    `json:"protocolMin"`
	ProtocolMax         uint32    `json:"protocolMax"`
	FileProtocolVersion *uint32   `json:"fileProtocolVersion,omitempty"`
	ConnectionEpoch     uint64    `json:"connectionEpoch"`
	PreferredRegion     string    `json:"preferredRegion,omitempty"`
}

type relayAllocationRefreshRequest struct {
	Reason               string `json:"reason,omitempty"`
	LastEndpointRevision uint64 `json:"lastEndpointRevision,omitempty"`
}

func registerDeviceRelayRoutes(group *gin.RouterGroup, appAuth AppAuthService, devices RemoteDeviceService, allocations RemoteAllocationService) {
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.Next()
	})
	group.POST("/device/access-key-bootstrap", func(c *gin.Context) {
		bootstrapper, supported := devices.(RemoteDeviceBootstrapService)
		if !supported || bootstrapper == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_unavailable", "设备接入暂不可用", "请稍后重试。")
			return
		}
		key, ok := deviceKeyToken(c.GetHeader("Authorization"))
		if !ok {
			writeProblem(c, http.StatusUnauthorized, "device_key_invalid", "设备访问密钥无效", "请在管理端轮换或重新创建设备访问密钥。")
			return
		}
		var request bootstrapRemoteDeviceRequest
		if !decodeJSON(c, &request) {
			return
		}
		result, err := bootstrapper.BootstrapAccessKey(c.Request.Context(), deviceaccesskey.BootstrapInput{
			Key: key, DeviceID: request.DeviceID, DeviceName: request.DeviceName,
		})
		if errors.Is(err, deviceaccesskey.ErrUnauthorized) || errors.Is(err, deviceaccesskey.ErrInvalidInput) {
			writeProblem(c, http.StatusUnauthorized, "device_key_invalid", "设备访问密钥无效", "请在管理端轮换或重新创建设备访问密钥。")
			return
		}
		if errors.Is(err, remoteaccesspolicy.ErrMembershipRequired) {
			writeProblem(c, http.StatusForbidden, "membership_required", "当前套餐未开放设备接入", "请等待 Free 套餐临时开放，或使用已开放远程设备功能的有效会员套餐。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_unavailable", "设备接入暂不可用", "请稍后重试。")
			return
		}
		c.Header("Vary", "Authorization")
		c.JSON(http.StatusOK, result)
	})
	device := group.Group("/device", requireRemoteAppSession(appAuth))
	device.POST("/registrations", func(c *gin.Context) {
		if devices == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_unavailable", "远程接入暂不可用", "请检查 Redis 和票据签发配置。")
			return
		}
		var request registerRemoteDeviceRequest
		if !decodeJSON(c, &request) {
			return
		}
		var directTrust *relayallocation.DeviceLinkGrantTrustBundle
		if request.DirectEnabled {
			provider, supported := allocations.(remoteDeviceLinkTrustService)
			if !supported || provider == nil {
				writeProblem(c, http.StatusServiceUnavailable, "remote_direct_unavailable", "设备直连暂不可用", "管理端无法下发直连授权验签信息。")
				return
			}
			trust := provider.DeviceLinkGrantTrust()
			if trust.Issuer == "" || len(trust.Keys) == 0 {
				writeProblem(c, http.StatusServiceUnavailable, "remote_direct_unavailable", "设备直连暂不可用", "管理端直连授权验签信息尚未就绪。")
				return
			}
			directTrust = &trust
		}
		session, _ := remoteAppSessionFrom(c)
		result, err := devices.Register(c.Request.Context(), remotedevice.RegisterInput{
			UserID: session.User.ID, SessionID: session.SessionID, DeviceID: session.DeviceID,
			IdempotencyKey: c.GetHeader("Idempotency-Key"), DeviceName: request.DeviceName,
			Platform: request.Platform, AgentVersion: request.AgentVersion,
			ProtocolMin: request.ProtocolMin, ProtocolMax: request.ProtocolMax, Capabilities: request.Capabilities,
			IdentityAlgorithm: request.IdentityAlgorithm, IdentityPublicKey: request.IdentityPublicKey, Proof: request.Proof,
			DirectEnabled: request.DirectEnabled, DirectTLSEnabled: request.DirectTLSEnabled, DirectIP: request.DirectIP, DirectPort: request.DirectPort,
			DirectConnectionEpoch: request.DirectConnectionEpoch,
		})
		if writeRemoteDeviceProblem(c, err) {
			return
		}
		credential := result.Credential
		enabledAt := credential.CreatedAt.UTC()
		var directIP *string
		var directPort *uint32
		if credential.DirectEnabled {
			directIP, directPort = &credential.DirectIP, &credential.DirectPort
		}
		connectionMode := "relay"
		if credential.DirectModeEnabled {
			connectionMode = "direct"
		}
		c.JSON(http.StatusCreated, remoteDeviceResponse{
			Device: remoteDeviceView{
				ID: credential.DeviceID, InstallationDeviceID: credential.DeviceID, DeviceName: credential.DeviceName,
				Platform: credential.Platform, AgentVersion: credential.AgentVersion, Status: credential.Status,
				Presence: "offline", Capabilities: credential.Capabilities, Scopes: credential.Scopes,
				GrantVersion: credential.GrantVersion, KeyVersion: credential.KeyVersion, RemoteEnabledAt: &enabledAt,
				ConnectionMode: connectionMode, DirectModeEnabled: credential.DirectModeEnabled,
				DirectAvailable: credential.DirectEnabled, DirectTLSEnabled: credential.DirectTLSEnabled, DirectIP: directIP, DirectPort: directPort,
			},
			PublicKeyThumbprint: credential.PublicKeyThumbprint, ApprovalRequired: false, DeviceLinkGrantTrust: directTrust,
		})
	})
	device.POST("/direct-heartbeats", func(c *gin.Context) {
		heartbeats, supported := devices.(remoteDeviceDirectHeartbeatService)
		if !supported || heartbeats == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_direct_unavailable", "设备直连暂不可用", "请升级管理端后重试。")
			return
		}
		var request directRemoteDeviceHeartbeatRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := remoteAppSessionFrom(c)
		result, err := heartbeats.HeartbeatDirect(c.Request.Context(), remotedevice.DirectHeartbeatInput{
			UserID: session.User.ID, SessionID: session.SessionID, DeviceID: session.DeviceID,
			IP: request.IP, Port: request.Port, ConnectionEpoch: request.ConnectionEpoch, TLSEnabled: request.TLSEnabled,
		})
		if writeRemoteDeviceProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	device.POST("/key-rotations", func(c *gin.Context) {
		rotator, supported := devices.(RemoteDeviceKeyRotationService)
		if !supported || rotator == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_unavailable", "设备密钥轮换暂不可用", "请稍后重试。")
			return
		}
		var request rotateRemoteDeviceKeyRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := remoteAppSessionFrom(c)
		result, err := rotator.RotateDeviceKey(c.Request.Context(), remotedevice.RotateKeyInput{
			UserID: session.User.ID, SessionID: session.SessionID, DeviceID: session.DeviceID,
			IdempotencyKey: c.GetHeader("Idempotency-Key"), ExpectedKeyVersion: request.ExpectedKeyVersion,
			NewIdentityPublicKey: request.NewIdentityPublicKey, OldProof: request.OldProof, NewProof: request.NewProof,
		})
		if writeRemoteDeviceProblem(c, err) {
			return
		}
		credential := result.Credential
		c.JSON(http.StatusOK, rotateRemoteDeviceKeyResponse{
			DeviceID: credential.DeviceID, IdentityPublicKey: base64.RawURLEncoding.EncodeToString(credential.IdentityPublicKey),
			PublicKeyThumbprint: credential.PublicKeyThumbprint, KeyVersion: credential.KeyVersion,
			GrantVersion: credential.GrantVersion, Rotated: result.Rotated,
		})
	})
	device.POST("/relay-allocations", func(c *gin.Context) {
		if allocations == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_unavailable", "远程接入暂不可用", "请检查 Redis 和票据签发配置。")
			return
		}
		var request relayAllocationRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.FileProtocolVersion != nil && *request.FileProtocolVersion != 1 {
			writeProblem(c, http.StatusBadRequest, "file_protocol_invalid", "文件协议版本无效", "当前仅支持文件协议版本 1。")
			return
		}
		session, _ := remoteAppSessionFrom(c)
		result, err := allocations.Create(c.Request.Context(), relayallocation.CreateInput{
			UserID: session.User.ID, SessionID: session.SessionID, DeviceID: session.DeviceID,
			IdempotencyKey: c.GetHeader("Idempotency-Key"), RemoteDeviceID: request.RemoteDeviceID,
			ProtocolMin: request.ProtocolMin, ProtocolMax: request.ProtocolMax,
			ConnectionEpoch: request.ConnectionEpoch, PreferredRegion: request.PreferredRegion,
		})
		if writeRelayAllocationProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
	device.POST("/relay-allocations/:assignmentId/refresh", func(c *gin.Context) {
		if allocations == nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_unavailable", "远程接入暂不可用", "请检查 Redis 和票据签发配置。")
			return
		}
		assignmentID, err := uuid.Parse(strings.TrimSpace(c.Param("assignmentId")))
		if err != nil || assignmentID == uuid.Nil {
			writeProblem(c, http.StatusBadRequest, "relay_allocation_id_invalid", "中继归属编号无效", "请重新获取中继服务接入分配。")
			return
		}
		var request relayAllocationRefreshRequest
		if c.Request.ContentLength != 0 && !decodeJSON(c, &request) {
			return
		}
		session, _ := remoteAppSessionFrom(c)
		result, err := allocations.Refresh(c.Request.Context(), relayallocation.RefreshInput{
			UserID: session.User.ID, SessionID: session.SessionID, DeviceID: session.DeviceID,
			AssignmentID: assignmentID, IdempotencyKey: c.GetHeader("Idempotency-Key"),
			Reason: request.Reason, LastEndpointRevision: request.LastEndpointRevision,
		})
		if writeRelayAllocationProblem(c, err) {
			return
		}
		c.JSON(http.StatusOK, result)
	})
}

func deviceKeyToken(header string) (string, bool) {
	if len(header) != len("DeviceKey ")+len("device_")+43 || !strings.HasPrefix(header, "DeviceKey device_") {
		return "", false
	}
	token := header[len("DeviceKey "):]
	for _, character := range token {
		if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') &&
			!(character >= '0' && character <= '9') && character != '-' && character != '_' {
			return "", false
		}
	}
	return token, true
}

func requireRemoteAppSession(service AppAuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if service == nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "客户端认证服务暂不可用", "请稍后重试。")
			return
		}
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			writeProblem(c, http.StatusUnauthorized, "app_token_invalid", "客户端登录已失效", "请刷新凭证或重新登录。")
			return
		}
		session, err := service.AuthenticateAppAccessToken(c.Request.Context(), token)
		if errors.Is(err, auth.ErrAppTokenInvalid) {
			writeProblem(c, http.StatusUnauthorized, "app_token_invalid", "客户端登录已失效", "请刷新凭证或重新登录。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "auth_unavailable", "客户端认证服务暂不可用", "请稍后重试。")
			return
		}
		if !scopeContains(session.Scope, "remote.connect") {
			writeProblem(c, http.StatusForbidden, "insufficient_scope", "客户端权限不足", "当前凭证缺少 remote.connect。")
			return
		}
		c.Set(remoteAppSessionKey, session)
		c.Header("Vary", "Authorization")
		c.Next()
	}
}

func remoteAppSessionFrom(c *gin.Context) (auth.AuthenticatedAppSession, bool) {
	value, ok := c.Get(remoteAppSessionKey)
	if !ok {
		return auth.AuthenticatedAppSession{}, false
	}
	session, ok := value.(auth.AuthenticatedAppSession)
	return session, ok
}

func writeRemoteDeviceProblem(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, remotedevice.ErrRegistrationProof):
		writeProblem(c, http.StatusBadRequest, "registration_proof_invalid", "设备持钥证明无效", "请重新生成注册签名。")
	case errors.Is(err, remotedevice.ErrKeyRotationProof):
		writeProblem(c, http.StatusBadRequest, "key_rotation_proof_invalid", "设备密钥轮换证明无效", "请使用当前私钥与新私钥分别签署同一轮换内容。")
	case errors.Is(err, remotedevice.ErrInvalidInput):
		writeProblem(c, http.StatusBadRequest, "remote_device_invalid", "设备注册信息无效", "请检查设备、公钥和协议字段。")
	case errors.Is(err, remotedevice.ErrForbidden):
		writeProblem(c, http.StatusForbidden, "remote_device_forbidden", "设备不属于当前用户", "请使用当前登录凭证对应的设备标识。")
	case errors.Is(err, remoteaccesspolicy.ErrMembershipRequired):
		writeProblem(c, http.StatusForbidden, "membership_required", "当前套餐未开放设备接入", "请等待 Free 套餐临时开放，或使用已开放远程设备功能的有效会员套餐。")
	case errors.Is(err, remoteaccesspolicy.ErrDeviceLimitReached):
		writeProblem(c, http.StatusConflict, "device_limit_reached", "设备数量已达上限", "请先永久删除不再使用的设备，再接入新设备。")
	case errors.Is(err, remotedevice.ErrKeyRotationRequired):
		writeProblem(c, http.StatusConflict, "key_rotation_required", "设备公钥已存在", "MVP 不会静默覆盖旧公钥，请执行密钥轮换流程。")
	case errors.Is(err, remotedevice.ErrIdempotencyConflict):
		writeProblem(c, http.StatusConflict, "idempotency_conflict", "幂等键已用于其他请求", "请为不同请求使用新的 Idempotency-Key。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "remote_unavailable", "远程接入暂不可用", "请稍后重试。")
	}
	return true
}

func writeRelayAllocationProblem(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, relayallocation.ErrInvalidRequest):
		writeProblem(c, http.StatusBadRequest, "relay_allocation_invalid", "中继服务接入分配请求无效", "请检查请求字段。")
	case errors.Is(err, relayallocation.ErrDeviceForbidden):
		writeProblem(c, http.StatusForbidden, "remote_device_forbidden", "设备不属于当前用户", "remoteDeviceId 必须等于应用访问令牌中的 device_id。")
	case errors.Is(err, remoteaccesspolicy.ErrMembershipRequired):
		writeProblem(c, http.StatusForbidden, "membership_required", "当前套餐未开放设备接入", "当前账户没有可用的远程设备套餐权限，无法获取连接凭证。")
	case errors.Is(err, relayallocation.ErrDeviceInactive):
		writeProblem(c, http.StatusConflict, "remote_device_inactive", "设备当前不可连接", "请联系管理员检查设备状态。")
	case errors.Is(err, relayallocation.ErrStaleConnectionEpoch):
		writeProblem(c, http.StatusConflict, "connection_epoch_stale", "连接序号已过期", "请递增 connectionEpoch 后重试。")
	case errors.Is(err, relayallocation.ErrAllocationNotFound):
		writeProblem(c, http.StatusNotFound, "relay_allocation_not_found", "中继服务接入分配不存在", "请重新请求分配。")
	case errors.Is(err, relayallocation.ErrRequestConflict):
		writeProblem(c, http.StatusConflict, "idempotency_conflict", "请求状态冲突", "请核对设备协议或使用新的 Idempotency-Key。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "relay_unavailable", "中继组暂时不可用", "当前中继组没有可接入的中继节点，请检查中继组、公网接入地址、Redis 和短期连接凭证签名器，或稍后重试。")
	}
	return true
}
