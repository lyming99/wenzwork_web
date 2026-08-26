package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
)

type AdminLifetimeCodeService interface {
	ListLifetimeCodeDeliveries(context.Context, int) ([]membership.LifetimeCodeDelivery, error)
	SendLifetimeCode(context.Context, membership.LifetimeCodeDeliveryInput) (membership.LifetimeCodeDeliveryResult, error)
}

type sendLifetimeCodeRequest struct {
	RequestID uuid.UUID `json:"requestId"`
	Email     string    `json:"email"`
}

func registerAdminLifetimeCodeRoutes(group *gin.RouterGroup, service AdminLifetimeCodeService, authService AuthService, config AuthHTTPConfig) {
	admin := group.Group("/admin/lifetime-code-deliveries")
	admin.Use(
		requireSession(authService, config),
		RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA),
	)

	admin.GET("", func(c *gin.Context) {
		if !adminLifetimeCodeServiceAvailable(c, service) {
			return
		}
		limit := 20
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				writeProblem(c, http.StatusBadRequest, "lifetime_code_delivery_limit_invalid", "查询数量无效", "请使用 1 到 100 之间的整数。")
				return
			}
			limit = parsed
		}
		items, err := service.ListLifetimeCodeDeliveries(c.Request.Context(), limit)
		if errors.Is(err, membership.ErrLifetimeCodeDeliveryInvalid) {
			writeProblem(c, http.StatusBadRequest, "lifetime_code_delivery_limit_invalid", "查询数量无效", "请使用 1 到 100 之间的整数。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "lifetime_code_delivery_unavailable", "暂时无法读取永久激活码发送记录", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	admin.POST("", requireCSRF(config), func(c *gin.Context) {
		if !adminLifetimeCodeServiceAvailable(c, service) {
			return
		}
		var request sendLifetimeCodeRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.SendLifetimeCode(c.Request.Context(), membership.LifetimeCodeDeliveryInput{
			RequestID: request.RequestID, Email: request.Email, ActorUserID: session.User.ID,
		})
		switch {
		case errors.Is(err, membership.ErrLifetimeCodeDeliveryInvalid):
			writeProblem(c, http.StatusBadRequest, "lifetime_code_delivery_invalid", "无法发送永久激活码", "请填写正确的收件邮箱后重试。")
			return
		case errors.Is(err, membership.ErrLifetimeCodeUnavailable):
			writeProblem(c, http.StatusConflict, "lifetime_code_unavailable", "该永久激活码已不可发送", "激活码可能已兑换或撤销，请生成新的激活码。")
			return
		case errors.Is(err, membership.ErrMembershipPlanAbsent):
			writeProblem(c, http.StatusConflict, "membership_plan_invalid", "Pro 会员方案当前不可用", "请先检查会员方案配置。")
			return
		case errors.Is(err, membership.ErrLifetimeCodeEmailDeliveryFailed):
			writeProblem(c, http.StatusServiceUnavailable, "lifetime_code_email_delivery_failed", "永久激活码邮件暂时未能发出", "激活码已安全保留，请在发送记录中重试；系统会继续发送同一个激活码。")
			return
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "lifetime_code_delivery_unavailable", "暂时无法发送永久激活码", "请稍后重试。")
			return
		}

		status := http.StatusOK
		message := "永久 Pro 激活码已发送至邮箱。"
		if result.NewDelivery {
			status = http.StatusCreated
		} else if result.Delivery.DeliveryStatus == "pending" {
			status = http.StatusAccepted
			message = "永久 Pro 激活码邮件正在发送，请稍后刷新状态。"
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(status, gin.H{"message": message, "delivery": result.Delivery})
	})
}

func adminLifetimeCodeServiceAvailable(c *gin.Context, service AdminLifetimeCodeService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "lifetime_code_delivery_unavailable", "永久激活码发送服务暂不可用", "请稍后重试。")
	return false
}
