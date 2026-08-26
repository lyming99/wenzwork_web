package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
)

type PromotionService interface {
	Status(context.Context) (membership.BetaPromotionStatus, error)
	GroupQRCode(context.Context) (membership.BetaPromotionGroupQRCode, error)
	Claim(context.Context, string, string) (membership.BetaPromotionClaimResult, error)
}

type AdminPromotionService interface {
	AdminOverview(context.Context) (membership.BetaPromotionAdminOverview, error)
	ListAdminClaims(context.Context, membership.BetaPromotionClaimFilter) (membership.BetaPromotionAdminClaimList, error)
	UpdateAdminRemaining(context.Context, uuid.UUID, int) (membership.BetaPromotionAdminOverview, error)
	UpdateAdminGroupQRCode(context.Context, uuid.UUID, string, []byte) (membership.BetaPromotionAdminOverview, error)
	RemoveAdminGroupQRCode(context.Context, uuid.UUID) (membership.BetaPromotionAdminOverview, error)
}

type betaPromotionClaimRequest struct {
	Email string `json:"email"`
}

type updateBetaPromotionRequest struct {
	Remaining *int `json:"remaining"`
}

func registerPromotionRoutes(group *gin.RouterGroup, service PromotionService, config AuthHTTPConfig, log *slog.Logger) {
	promotion := group.Group("/promotions/beta-pro")
	promotion.GET("", func(c *gin.Context) {
		if !promotionServiceAvailable(c, service) {
			return
		}
		status, err := service.Status(c.Request.Context())
		if err != nil {
			log.Error("beta promotion status failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(c, http.StatusServiceUnavailable, "promotion_unavailable", "暂时无法读取内测名额", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, status)
	})

	promotion.GET("/group-qr", func(c *gin.Context) {
		if !promotionServiceAvailable(c, service) {
			return
		}
		result, err := service.GroupQRCode(c.Request.Context())
		switch {
		case errors.Is(err, membership.ErrBetaPromotionGroupQRCodeNotConfigured):
			writeProblem(c, http.StatusNotFound, "promotion_group_qr_not_configured", "内测群二维码暂未配置", "请通过邮件中的联系方式加入内测群。")
			return
		case err != nil:
			log.Error("beta promotion group QR code failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(c, http.StatusServiceUnavailable, "promotion_unavailable", "暂时无法读取内测群二维码", "请稍后重试。")
			return
		}

		etag := fmt.Sprintf(`"beta-group-qr-%d"`, result.UpdatedAt.UTC().UnixNano())
		c.Header("Cache-Control", "public, max-age=300")
		c.Header("ETag", etag)
		c.Header("X-Content-Type-Options", "nosniff")
		if c.GetHeader("If-None-Match") == etag {
			c.Status(http.StatusNotModified)
			return
		}
		c.Data(http.StatusOK, result.ContentType, result.Content)
	})

	promotion.POST("/claims", requireAllowedOrigin(config.AllowedOrigins), func(c *gin.Context) {
		if !promotionServiceAvailable(c, service) {
			return
		}
		var request betaPromotionClaimRequest
		if !decodeJSON(c, &request) {
			return
		}
		result, err := service.Claim(c.Request.Context(), request.Email, c.ClientIP())
		switch {
		case errors.Is(err, membership.ErrBetaPromotionInvalidEmail):
			writeProblem(c, http.StatusBadRequest, "promotion_email_invalid", "邮箱格式不正确", "请输入可以正常收信的邮箱地址。")
			return
		case errors.Is(err, membership.ErrBetaPromotionExhausted):
			writeProblem(c, http.StatusConflict, "promotion_exhausted", "内测赠送名额已领完", "当前没有可领取的内测码名额。")
			return
		case errors.Is(err, membership.ErrBetaPromotionRateLimit):
			c.Header("Retry-After", "86400")
			writeProblem(c, http.StatusTooManyRequests, "promotion_rate_limited", "今日领取次数过多", "请勿重复提交，或在 24 小时后重试。")
			return
		case errors.Is(err, membership.ErrBetaPromotionDelivery):
			log.Warn("beta promotion email delivery failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(c, http.StatusServiceUnavailable, "promotion_email_delivery_failed", "兑换码邮件暂时未能发出", "名额已经保留，再次提交相同邮箱即可重试发送，不会重复占用名额。")
			return
		case err != nil:
			log.Error("beta promotion claim failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(c, http.StatusServiceUnavailable, "promotion_unavailable", "暂时无法加入内测", "请稍后使用相同邮箱重试。")
			return
		}

		status := http.StatusOK
		message := "该邮箱已经领取过，兑换码已发送至邮箱，请检查收件箱和垃圾邮件。"
		if result.NewClaim {
			status = http.StatusCreated
			message = "领取成功，1 年 Pro 兑换码已发送至邮箱。"
		} else if result.DeliveryStatus == "pending" {
			status = http.StatusAccepted
			message = "兑换码邮件正在发送，请稍后检查收件箱和垃圾邮件。"
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(status, gin.H{
			"message": message, "promotion": result.Promotion,
			"deliveryStatus": result.DeliveryStatus, "alreadyClaimed": result.AlreadyClaimed,
			"groupQRCodeUrl": result.GroupQRCodeURL,
		})
	})
}

func promotionServiceAvailable(c *gin.Context, service PromotionService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "promotion_unavailable", "内测活动暂不可用", "请稍后重试。")
	return false
}

func registerAdminPromotionRoutes(group *gin.RouterGroup, service AdminPromotionService, authService AuthService, config AuthHTTPConfig) {
	admin := group.Group("/admin/beta-promotion")
	admin.Use(requireSession(authService, config))

	admin.GET("", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminPromotionServiceAvailable(c, service) {
			return
		}
		result, err := service.AdminOverview(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "beta_promotion_admin_unavailable", "暂时无法读取内测码活动", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	})

	admin.GET("/claims", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminPromotionServiceAvailable(c, service) {
			return
		}
		limit, offset, ok := parseAdminPagination(c, 50, 100)
		if !ok {
			return
		}
		result, err := service.ListAdminClaims(c.Request.Context(), membership.BetaPromotionClaimFilter{
			Query: c.Query("q"), DeliveryStatus: c.Query("deliveryStatus"),
			RedemptionStatus: c.Query("redemptionStatus"), Limit: limit, Offset: offset,
		})
		switch {
		case errors.Is(err, membership.ErrBetaPromotionAdminInvalid):
			writeProblem(c, http.StatusBadRequest, "beta_promotion_filter_invalid", "内测码筛选条件无效", "请检查搜索内容和状态后重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "beta_promotion_admin_unavailable", "暂时无法读取内测码记录", "请稍后重试。")
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, result)
		}
	})

	admin.PUT("", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminPromotionServiceAvailable(c, service) {
			return
		}
		var request updateBetaPromotionRequest
		if !decodeJSON(c, &request) {
			return
		}
		if request.Remaining == nil {
			writeProblem(c, http.StatusBadRequest, "beta_promotion_remaining_invalid", "剩余名额无效", "请填写 0 到 5000 之间的整数。")
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.UpdateAdminRemaining(c.Request.Context(), session.User.ID, *request.Remaining)
		switch {
		case errors.Is(err, membership.ErrBetaPromotionAdminInvalid):
			writeProblem(c, http.StatusBadRequest, "beta_promotion_remaining_invalid", "剩余名额无效", "活动总名额不能超过 5000，请调整后重试。")
		case errors.Is(err, membership.ErrBetaPromotionBatchRevoked):
			writeProblem(c, http.StatusConflict, "beta_promotion_batch_revoked", "内测码批次已被撤销", "请先恢复或重新配置活动批次。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "beta_promotion_admin_unavailable", "暂时无法更新内测码名额", "请稍后重试。")
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, result)
		}
	})

	admin.PUT("/group-qr", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminPromotionServiceAvailable(c, service) {
			return
		}

		contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "beta_promotion_group_qr_invalid", "内测群二维码格式无效", "请选择 PNG 或 JPEG 图片。")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, membership.BetaPromotionGroupQRCodeMaxBytes)
		content, err := io.ReadAll(c.Request.Body)
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeProblem(c, http.StatusRequestEntityTooLarge, "beta_promotion_group_qr_too_large", "内测群二维码图片过大", "图片大小不能超过 2 MiB。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "beta_promotion_group_qr_invalid", "无法读取内测群二维码", "请重新选择图片后再试。")
			return
		}

		session, _ := authSessionFrom(c)
		result, err := service.UpdateAdminGroupQRCode(
			c.Request.Context(),
			session.User.ID,
			contentType,
			content,
		)
		switch {
		case errors.Is(err, membership.ErrBetaPromotionAdminInvalid),
			errors.Is(err, membership.ErrBetaPromotionGroupQRCodeInvalid):
			writeProblem(c, http.StatusBadRequest, "beta_promotion_group_qr_invalid", "内测群二维码格式无效", "请选择内容有效、尺寸不超过 4096×4096 的 PNG 或 JPEG 图片。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "beta_promotion_admin_unavailable", "暂时无法保存内测群二维码", "请稍后重试。")
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, result)
		}
	})

	admin.DELETE("/group-qr", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminPromotionServiceAvailable(c, service) {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.RemoveAdminGroupQRCode(c.Request.Context(), session.User.ID)
		switch {
		case errors.Is(err, membership.ErrBetaPromotionAdminInvalid):
			writeProblem(c, http.StatusBadRequest, "beta_promotion_group_qr_invalid", "无法移除内测群二维码", "请刷新页面后重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "beta_promotion_admin_unavailable", "暂时无法移除内测群二维码", "请稍后重试。")
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, result)
		}
	})
}

func adminPromotionServiceAvailable(c *gin.Context, service AdminPromotionService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "beta_promotion_admin_unavailable", "内测码管理暂不可用", "请稍后重试。")
	return false
}
