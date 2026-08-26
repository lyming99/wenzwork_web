package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
)

type TrialPromotionService interface {
	Status(context.Context) (membership.TrialPromotionStatus, error)
	Claim(context.Context, string, string) (membership.TrialPromotionClaimResult, error)
}

type AdminTrialPromotionService interface {
	AdminOverview(context.Context) (membership.TrialPromotionAdminOverview, error)
	ListAdminClaims(
		context.Context,
		membership.TrialPromotionClaimFilter,
	) (membership.TrialPromotionAdminClaimList, error)
	UpdateAdminSettings(
		context.Context,
		uuid.UUID,
		bool,
		int,
	) (membership.TrialPromotionAdminOverview, error)
}

type trialPromotionClaimRequest struct {
	Email string `json:"email"`
}

type updateTrialPromotionRequest struct {
	Enabled    *bool `json:"enabled"`
	DailyQuota *int  `json:"dailyQuota"`
}

func registerTrialPromotionRoutes(
	group *gin.RouterGroup,
	service TrialPromotionService,
	config AuthHTTPConfig,
	log *slog.Logger,
) {
	promotion := group.Group("/promotions/trial-pro")
	promotion.GET("", func(c *gin.Context) {
		if !trialPromotionServiceAvailable(c, service) {
			return
		}
		status, err := service.Status(c.Request.Context())
		if err != nil {
			log.Error("trial promotion status failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(
				c,
				http.StatusServiceUnavailable,
				"trial_promotion_unavailable",
				"暂时无法读取试用码名额",
				"请稍后重试。",
			)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, status)
	})

	promotion.POST("/claims", requireAllowedOrigin(config.AllowedOrigins), func(c *gin.Context) {
		if !trialPromotionServiceAvailable(c, service) {
			return
		}
		var request trialPromotionClaimRequest
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.Claim(c.Request.Context(), request.Email, c.ClientIP())
		switch {
		case errors.Is(err, membership.ErrTrialPromotionInvalidEmail):
			writeProblem(
				c,
				http.StatusBadRequest,
				"trial_promotion_email_invalid",
				"邮箱格式不正确",
				"请输入可以正常收信的邮箱地址。",
			)
			return
		case errors.Is(err, membership.ErrTrialPromotionUnavailable):
			writeProblem(
				c,
				http.StatusConflict,
				"trial_promotion_unavailable",
				"今日试用码已领完或活动已关闭",
				"请在下一次刷新后重试。",
			)
			return
		case errors.Is(err, membership.ErrTrialPromotionRateLimit):
			c.Header("Retry-After", "86400")
			writeProblem(
				c,
				http.StatusTooManyRequests,
				"trial_promotion_rate_limited",
				"今日领取次数过多",
				"请勿重复提交，或在 24 小时后重试。",
			)
			return
		case errors.Is(err, membership.ErrTrialPromotionDelivery):
			log.Warn("trial promotion email delivery failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(
				c,
				http.StatusServiceUnavailable,
				"trial_promotion_email_delivery_failed",
				"试用码邮件暂时未能发出",
				"名额已经保留，再次提交相同邮箱即可重试发送，不会重复占用名额。",
			)
			return
		case err != nil:
			log.Error("trial promotion claim failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(
				c,
				http.StatusServiceUnavailable,
				"trial_promotion_unavailable",
				"暂时无法领取试用码",
				"请稍后使用相同邮箱重试。",
			)
			return
		}

		status := http.StatusOK
		message := "该邮箱已经领取过，30 天 Pro 试用码已发送至邮箱，请检查收件箱和垃圾邮件。"
		if result.NewClaim {
			status = http.StatusCreated
			message = "领取成功，30 天 Pro 试用码已发送至邮箱。"
		} else if result.DeliveryStatus == "pending" {
			status = http.StatusAccepted
			message = "试用码邮件正在发送，请稍后检查收件箱和垃圾邮件。"
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(status, gin.H{
			"message": message, "promotion": result.Promotion,
			"deliveryStatus": result.DeliveryStatus, "alreadyClaimed": result.AlreadyClaimed,
		})
	})
}

func trialPromotionServiceAvailable(c *gin.Context, service TrialPromotionService) bool {
	if service != nil {
		return true
	}
	writeProblem(
		c,
		http.StatusServiceUnavailable,
		"trial_promotion_unavailable",
		"试用码活动暂不可用",
		"请稍后重试。",
	)
	return false
}

func registerAdminTrialPromotionRoutes(
	group *gin.RouterGroup,
	service AdminTrialPromotionService,
	authService AuthService,
	config AuthHTTPConfig,
) {
	admin := group.Group("/admin/trial-promotion")
	admin.Use(requireSession(authService, config))

	admin.GET("", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminTrialPromotionServiceAvailable(c, service) {
			return
		}
		result, err := service.AdminOverview(c.Request.Context())
		if err != nil {
			writeProblem(
				c,
				http.StatusServiceUnavailable,
				"trial_promotion_admin_unavailable",
				"暂时无法读取试用码活动",
				"请稍后重试。",
			)
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	})

	admin.GET("/claims", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminTrialPromotionServiceAvailable(c, service) {
			return
		}
		limit, offset, ok := parseAdminPagination(c, 50, 100)
		if !ok {
			return
		}
		result, err := service.ListAdminClaims(
			c.Request.Context(),
			membership.TrialPromotionClaimFilter{
				Query: c.Query("q"), DeliveryStatus: c.Query("deliveryStatus"),
				RedemptionStatus: c.Query("redemptionStatus"), Limit: limit, Offset: offset,
			},
		)
		switch {
		case errors.Is(err, membership.ErrTrialPromotionAdminInvalid):
			writeProblem(
				c,
				http.StatusBadRequest,
				"trial_promotion_filter_invalid",
				"试用码筛选条件无效",
				"请检查搜索内容和状态后重试。",
			)
		case err != nil:
			writeProblem(
				c,
				http.StatusServiceUnavailable,
				"trial_promotion_admin_unavailable",
				"暂时无法读取试用码记录",
				"请稍后重试。",
			)
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, result)
		}
	})

	admin.PUT("", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminTrialPromotionServiceAvailable(c, service) {
			return
		}
		var request updateTrialPromotionRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.Enabled == nil || request.DailyQuota == nil {
			writeProblem(
				c,
				http.StatusBadRequest,
				"trial_promotion_settings_invalid",
				"试用码设置无效",
				"请填写开启状态和 1 到 5000 之间的每日刷新数量。",
			)
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.UpdateAdminSettings(
			c.Request.Context(),
			session.User.ID,
			*request.Enabled,
			*request.DailyQuota,
		)
		switch {
		case errors.Is(err, membership.ErrTrialPromotionAdminInvalid):
			writeProblem(
				c,
				http.StatusBadRequest,
				"trial_promotion_settings_invalid",
				"试用码设置无效",
				"每日刷新数量必须是 1 到 5000 之间的整数。",
			)
		case errors.Is(err, membership.ErrTrialPromotionBatchRevoked):
			writeProblem(
				c,
				http.StatusConflict,
				"trial_promotion_batch_revoked",
				"试用码批次已被撤销",
				"请先恢复或重新配置活动批次。",
			)
		case err != nil:
			writeProblem(
				c,
				http.StatusServiceUnavailable,
				"trial_promotion_admin_unavailable",
				"暂时无法更新试用码设置",
				"请稍后重试。",
			)
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, result)
		}
	})
}

func adminTrialPromotionServiceAvailable(
	c *gin.Context,
	service AdminTrialPromotionService,
) bool {
	if service != nil {
		return true
	}
	writeProblem(
		c,
		http.StatusServiceUnavailable,
		"trial_promotion_admin_unavailable",
		"试用码管理暂不可用",
		"请稍后重试。",
	)
	return false
}
