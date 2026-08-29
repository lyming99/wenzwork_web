package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/emailsettings"
)

type SystemEmailService interface {
	GetSettings(context.Context) (emailsettings.Settings, error)
	Update(context.Context, emailsettings.UpdateInput) (emailsettings.Settings, error)
	ResetToLocal(context.Context, int64, uuid.UUID) (emailsettings.Settings, error)
	Test(context.Context, emailsettings.TestInput) error
}

type systemEmailDraftRequest struct {
	SMTPHost          string  `json:"smtpHost"`
	SMTPPort          int     `json:"smtpPort"`
	SMTPUser          string  `json:"smtpUser"`
	SMTPPassword      *string `json:"smtpPassword"`
	ClearSMTPPassword bool    `json:"clearSmtpPassword"`
	MailFrom          string  `json:"mailFrom"`
}

type updateSystemEmailRequest struct {
	systemEmailDraftRequest
	ExpectedVersion int64 `json:"expectedVersion"`
}

type testSystemEmailRequest struct {
	systemEmailDraftRequest
	Recipient string `json:"recipient"`
}

type resetSystemEmailRequest struct {
	ExpectedVersion int64 `json:"expectedVersion"`
}

func registerSystemEmailRoutes(group *gin.RouterGroup, service SystemEmailService, authService AuthService, config AuthHTTPConfig) {
	admin := group.Group("/admin/system-email", requireSession(authService, config))
	admin.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("Pragma", "no-cache")
		c.Next()
	})
	permission := auth.PermissionAdminSuper

	admin.GET("", RequirePermission(permission, !config.DisableAdminMFA), func(c *gin.Context) {
		if !systemEmailServiceAvailable(c, service) {
			return
		}
		settings, err := service.GetSettings(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "system_email_unavailable", "暂时无法读取系统邮箱设置", "请稍后重试；本地配置仍会作为邮件投递的保底配置。")
			return
		}
		c.JSON(http.StatusOK, settings)
	})

	admin.PUT("", RequirePermission(permission, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !systemEmailServiceAvailable(c, service) {
			return
		}
		var request updateSystemEmailRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		settings, err := service.Update(c.Request.Context(), emailsettings.UpdateInput{
			SMTPHost: request.SMTPHost, SMTPPort: request.SMTPPort, SMTPUser: request.SMTPUser,
			SMTPPassword: request.SMTPPassword, ClearSMTPPassword: request.ClearSMTPPassword,
			MailFrom: request.MailFrom, ExpectedVersion: request.ExpectedVersion, ActorUserID: session.User.ID,
		})
		writeSystemEmailResult(c, settings, err)
	})

	admin.POST("/test", RequirePermission(permission, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !systemEmailServiceAvailable(c, service) {
			return
		}
		var request testSystemEmailRequest
		if !decodeJSON(c, &request) {
			return
		}
		err := service.Test(c.Request.Context(), emailsettings.TestInput{
			SMTPHost: request.SMTPHost, SMTPPort: request.SMTPPort, SMTPUser: request.SMTPUser,
			SMTPPassword: request.SMTPPassword, ClearSMTPPassword: request.ClearSMTPPassword,
			MailFrom: request.MailFrom, Recipient: request.Recipient,
		})
		switch {
		case errors.Is(err, emailsettings.ErrInvalid), errors.Is(err, emailsettings.ErrNotConfigured):
			writeProblem(c, http.StatusBadRequest, "system_email_invalid", "系统邮箱配置无效", "请完整填写 SMTP 主机、端口、发件人和测试收件地址；填写用户名时还需提供密码。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "system_email_test_failed", "测试邮件发送失败", "请检查 SMTP 地址、凭据、TLS、发件人、收件地址和网络连通性。")
		default:
			c.Status(http.StatusNoContent)
		}
	})

	admin.POST("/reset", RequirePermission(permission, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !systemEmailServiceAvailable(c, service) {
			return
		}
		var request resetSystemEmailRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		settings, err := service.ResetToLocal(c.Request.Context(), request.ExpectedVersion, session.User.ID)
		writeSystemEmailResult(c, settings, err)
	})
}

func writeSystemEmailResult(c *gin.Context, settings emailsettings.Settings, err error) {
	switch {
	case errors.Is(err, emailsettings.ErrInvalid):
		writeProblem(c, http.StatusBadRequest, "system_email_invalid", "无法保存系统邮箱设置", "请完整填写有效的 SMTP 配置，并且不要同时替换和清除密码。")
	case errors.Is(err, emailsettings.ErrConflict):
		writeProblem(c, http.StatusConflict, "system_email_conflict", "系统邮箱设置已被其他管理员修改", "请刷新后重试。")
	case err != nil:
		writeProblem(c, http.StatusServiceUnavailable, "system_email_unavailable", "暂时无法保存系统邮箱设置", "请稍后重试。")
	default:
		c.JSON(http.StatusOK, settings)
	}
}

func systemEmailServiceAvailable(c *gin.Context, service SystemEmailService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "system_email_unavailable", "系统邮箱设置暂不可用", "请检查数据库迁移和 Host 配置。")
	return false
}
