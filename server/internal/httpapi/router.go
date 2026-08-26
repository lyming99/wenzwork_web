package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

type Dependencies struct {
	Logger                  *slog.Logger
	Readiness               func(context.Context) error
	RemoteReadiness         func(context.Context) error
	Catalog                 CatalogReader
	CatalogAdmin            AdminReleaseService
	ReleaseUploads          ReleaseAssetUploadService
	ReleaseSources          ReleaseAssetSourceService
	ReleaseDownloads        ReleaseAssetDownloadService
	ReleasePush             ReleasePushService
	ReleasePushAssets       ReleasePushAssetStore
	ReleaseAccessKeys       AdminReleaseAccessKeyService
	ReleaseAccessKey        string
	PricingAdmin            AdminPricingService
	Auth                    AuthService
	AppAuth                 AppAuthService
	UserAdmin               AdminUserService
	SystemSetup             SystemSetupService
	AuthHTTP                AuthHTTPConfig
	Membership              MembershipService
	MembershipAdmin         AdminMembershipService
	LifetimeCodeAdmin       AdminLifetimeCodeService
	Promotion               PromotionService
	PromotionAdmin          AdminPromotionService
	TrialPromotion          TrialPromotionService
	TrialAdmin              AdminTrialPromotionService
	Help                    HelpDocumentReader
	HelpAdmin               AdminHelpDocumentService
	Feedback                FeedbackService
	Analytics               AnalyticsService
	TrustedProxies          []string
	Relay                   relaymanagement.Service
	RelayDefaultRegion      string
	RelayDefaultPool        string
	RelayDefaultCell        string
	RemoteDevice            RemoteDeviceService
	RemoteAllocation        RemoteAllocationService
	RemoteAccessPolicy      RemoteAccessPolicyService
	RemoteControl           RemoteControlService
	PublicBaseURL           string
	RelayDirectoryURL       string
	RelayArtifactBaseURL    string
	RelayBootstrapAssetsDir string
}

func NewRouter(deps Dependencies) *gin.Engine {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Readiness == nil {
		deps.Readiness = func(context.Context) error { return nil }
	}
	if deps.RemoteReadiness == nil {
		deps.RemoteReadiness = func(context.Context) error { return errors.New("remote capability is not configured") }
	}

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	if err := router.SetTrustedProxies(deps.TrustedProxies); err != nil {
		panic(err)
	}
	router.Use(
		requestIDMiddleware(),
		securityHeadersMiddleware(),
		accessLogMiddleware(deps.Logger),
		recoveryMiddleware(deps.Logger),
	)

	v1 := router.Group("/api/v1")
	v2 := router.Group("/api/v2")
	v1.GET("/health/live", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	v1.GET("/health/ready", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := deps.Readiness(ctx); err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "not_ready", "服务尚未就绪", "依赖服务暂时不可用。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	v1.GET("/health/remote-ready", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		c.Header("Cache-Control", "no-store")
		if err := deps.RemoteReadiness(ctx); err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "remote_not_ready", "远程接入尚未就绪", "请检查 PostgreSQL、Redis 和票据签名器。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	registerCatalogRoutes(v1, deps.Catalog, deps.ReleaseDownloads, deps.Analytics, deps.Logger)
	registerReleasePushRoutes(v1, deps.ReleasePush, deps.ReleasePushAssets, deps.ReleaseAccessKeys, deps.ReleaseAccessKey)
	registerAuthRoutes(v1, deps.Auth, deps.AppAuth, deps.Analytics, deps.SystemSetup, deps.AuthHTTP, deps.Logger)
	registerAppAuthRoutes(v1, deps.AppAuth, deps.Auth, deps.Analytics, deps.AuthHTTP, deps.Logger)
	registerAnalyticsRoutes(v1, deps.Analytics, deps.Auth, deps.AuthHTTP, deps.Logger)
	registerMembershipRoutes(v1, deps.Membership, deps.Auth, deps.AppAuth, deps.AuthHTTP)
	registerAdminLifetimeCodeRoutes(v1, deps.LifetimeCodeAdmin, deps.Auth, deps.AuthHTTP)
	registerPromotionRoutes(v1, deps.Promotion, deps.AuthHTTP, deps.Logger)
	registerAdminPromotionRoutes(v1, deps.PromotionAdmin, deps.Auth, deps.AuthHTTP)
	registerTrialPromotionRoutes(v1, deps.TrialPromotion, deps.AuthHTTP, deps.Logger)
	registerAdminTrialPromotionRoutes(v1, deps.TrialAdmin, deps.Auth, deps.AuthHTTP)
	registerAdminRoutes(v1, deps.UserAdmin, deps.MembershipAdmin, deps.CatalogAdmin, deps.ReleaseUploads, deps.ReleaseSources, deps.ReleaseAccessKeys, deps.PricingAdmin, deps.Auth, deps.AuthHTTP)
	registerRemoteAccessPolicyRoutes(v1, deps.RemoteAccessPolicy, deps.Auth, deps.AuthHTTP)
	registerSystemSetupRoutes(v1, deps.SystemSetup, deps.Auth, deps.AuthHTTP)
	registerHelpRoutes(v1, deps.Help, deps.HelpAdmin, deps.Auth, deps.AuthHTTP)
	registerFeedbackRoutes(v1, deps.Feedback, deps.Auth, deps.AuthHTTP)
	registerRelayRoutes(v1, deps.Relay, deps.Auth, deps.AuthHTTP, deps.PublicBaseURL, deps.RelayDirectoryURL, deps.RelayArtifactBaseURL, deps.RelayBootstrapAssetsDir, deps.RelayDefaultRegion, deps.RelayDefaultPool, deps.RelayDefaultCell)
	registerDeviceAccessKeyRoutes(v1, deps.Auth, deps.AppAuth, deps.RemoteDevice, deps.AuthHTTP)
	deviceV1 := router.Group("/v1")
	registerDeviceRelayRoutes(deviceV1, deps.AppAuth, deps.RemoteDevice, deps.RemoteAllocation)
	registerRemoteControlRoutes(v1, deviceV1, deps.Auth, deps.AppAuth, deps.RemoteControl, deps.AuthHTTP)
	registerRemoteV2ControlRoutes(v2, deps.Auth, deps.AppAuth, deps.RemoteControl, deps.AuthHTTP)

	router.NoRoute(func(c *gin.Context) {
		writeProblem(c, http.StatusNotFound, "not_found", "接口不存在", "请检查请求路径。")
	})
	return router
}
