package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/systemsetup"
)

type SystemSetupService interface {
	Required() bool
	Get(context.Context) (systemsetup.Settings, error)
	Apply(context.Context, systemsetup.ApplyInput) (systemsetup.ApplyResult, error)
}

type applySystemSetupRequest struct {
	PublicBaseURL           string   `json:"publicBaseUrl"`
	DatabaseURL             string   `json:"databaseUrl"`
	RedisURL                string   `json:"redisUrl"`
	SMTPHost                string   `json:"smtpHost"`
	SMTPPort                int      `json:"smtpPort"`
	SMTPUser                string   `json:"smtpUser"`
	SMTPPassword            *string  `json:"smtpPassword"`
	ClearSMTPPassword       bool     `json:"clearSmtpPassword"`
	MailFrom                string   `json:"mailFrom"`
	CookieSecure            bool     `json:"cookieSecure"`
	AdminMFARequired        bool     `json:"adminMfaRequired"`
	RegistrationEnabled     bool     `json:"registrationEnabled"`
	AllowedOrigins          []string `json:"allowedOrigins"`
	WebGitHubRepository     string   `json:"webGithubRepository"`
	DesktopGitHubRepository string   `json:"desktopGithubRepository"`
	MobileGitHubRepository  string   `json:"mobileGithubRepository"`
}

func registerSystemSetupRoutes(group *gin.RouterGroup, service SystemSetupService, authService AuthService, config AuthHTTPConfig) {
	setup := group.Group("/admin/system-setup")
	setup.Use(requireSession(authService, config))
	permission := auth.PermissionAdminSuper

	setup.GET("", RequirePermission(permission, !config.DisableAdminMFA), func(c *gin.Context) {
		if !systemSetupServiceAvailable(c, service) {
			return
		}
		settings, err := service.Get(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "system_setup_unavailable", "无法读取系统配置", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, settings)
	})

	setup.PUT("", RequirePermission(permission, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !systemSetupServiceAvailable(c, service) {
			return
		}
		var request applySystemSetupRequest
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.Apply(c.Request.Context(), systemsetup.ApplyInput{
			PublicBaseURL: request.PublicBaseURL, DatabaseURL: request.DatabaseURL,
			RedisURL: request.RedisURL, SMTPHost: request.SMTPHost, SMTPPort: request.SMTPPort,
			SMTPUser: request.SMTPUser, SMTPPassword: request.SMTPPassword,
			ClearSMTPPassword: request.ClearSMTPPassword, MailFrom: request.MailFrom,
			CookieSecure:            request.CookieSecure,
			AdminMFARequired:        request.AdminMFARequired,
			RegistrationEnabled:     request.RegistrationEnabled,
			AllowedOrigins:          request.AllowedOrigins,
			WebGitHubRepository:     request.WebGitHubRepository,
			DesktopGitHubRepository: request.DesktopGitHubRepository,
			MobileGitHubRepository:  request.MobileGitHubRepository,
		})
		switch {
		case errors.Is(err, systemsetup.ErrAlreadyComplete):
			writeProblem(c, http.StatusConflict, "system_setup_complete", "系统已完成初始化", "请刷新页面读取当前状态。")
		case errors.Is(err, systemsetup.ErrInvalid):
			writeProblem(c, http.StatusBadRequest, "system_setup_invalid", "系统配置无效", err.Error())
		case errors.Is(err, systemsetup.ErrRedisUnavailable):
			writeProblem(c, http.StatusServiceUnavailable, "system_setup_redis_unavailable", "Redis 连接失败", "请检查 Redis 地址、凭据和网络连通性。")
		case errors.Is(err, systemsetup.ErrDatabaseUnavailable):
			writeProblem(c, http.StatusServiceUnavailable, "system_setup_database_unavailable", "数据库初始化失败", "请检查 PostgreSQL 地址、凭据、权限和迁移目录。")
		case errors.Is(err, systemsetup.ErrAdministratorBootstrap):
			writeProblem(c, http.StatusConflict, "system_setup_administrator_conflict", "管理员初始化失败", "目标数据库可能已经存在另一个超级管理员。")
		case errors.Is(err, systemsetup.ErrEnvironmentWrite):
			writeProblem(c, http.StatusInternalServerError, "system_setup_write_failed", "无法保存系统配置", "请检查安装目录 .env 的权限后重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "system_setup_unavailable", "系统初始化失败", "请检查依赖服务后重试。")
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, result)
		}
	})
}

func systemSetupRequired(service SystemSetupService) bool {
	return service != nil && service.Required()
}

func systemSetupServiceAvailable(c *gin.Context, service SystemSetupService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "system_setup_unavailable", "系统初始化不可用", "请检查 Host 启动配置。")
	return false
}
