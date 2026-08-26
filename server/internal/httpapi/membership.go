package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
)

type MembershipService interface {
	GetMembership(context.Context, uuid.UUID) (membership.MembershipStatus, error)
	ListRedemptions(context.Context, uuid.UUID, int) ([]membership.RedemptionRecord, error)
	RedeemFromIP(context.Context, uuid.UUID, string, string) (membership.RedemptionResult, error)
	CreateBatch(context.Context, membership.CreateBatchInput) (membership.CreatedBatch, error)
	ListBatches(context.Context, int) ([]membership.BatchSummary, error)
	RevokeBatch(context.Context, uuid.UUID, uuid.UUID) error
}

type redeemMembershipRequest struct {
	Code string `json:"code"`
}

type createRedemptionBatchRequest struct {
	Name         string               `json:"name"`
	PlanCode     string               `json:"planCode"`
	GrantType    membership.GrantType `json:"grantType"`
	GrantDays    *int                 `json:"grantDays"`
	Quantity     int                  `json:"quantity"`
	RedeemBefore *time.Time           `json:"redeemBefore"`
	Note         string               `json:"note"`
}

func registerMembershipRoutes(group *gin.RouterGroup, service MembershipService, authService AuthService, appAuth AppAuthService, config AuthHTTPConfig) {
	group.GET("/me/membership", requireAccountAuthentication(authService, appAuth, config, "membership.read"), func(c *gin.Context) {
		if !membershipServiceAvailable(c, service) {
			return
		}
		session, _ := authSessionFrom(c)
		status, err := service.GetMembership(c.Request.Context(), session.User.ID)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法读取会员状态", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, status)
	})

	me := group.Group("/me")
	me.Use(requireSession(authService, config))
	me.GET("/redemptions", func(c *gin.Context) {
		if !membershipServiceAvailable(c, service) {
			return
		}
		session, _ := authSessionFrom(c)
		items, err := service.ListRedemptions(c.Request.Context(), session.User.ID, 50)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法读取兑换记录", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	me.POST("/redemptions", requireCSRF(config), RequirePermission(auth.PermissionMembershipRedeem, false), func(c *gin.Context) {
		if !membershipServiceAvailable(c, service) {
			return
		}
		var request redeemMembershipRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := service.RedeemFromIP(c.Request.Context(), session.User.ID, request.Code, c.ClientIP())
		switch {
		case errors.Is(err, membership.ErrRedemptionRateLimit):
			c.Header("Retry-After", "900")
			writeProblem(c, http.StatusTooManyRequests, "redemption_rate_limited", "尝试次数过多", "请稍后再试。")
			return
		case errors.Is(err, membership.ErrEmailNotVerified):
			writeProblem(c, http.StatusForbidden, "email_not_verified", "邮箱尚未验证", "请先验证邮箱。")
			return
		case errors.Is(err, membership.ErrCodeUnavailable), errors.Is(err, membership.ErrMembershipNotExtended):
			writeProblem(c, http.StatusConflict, "redemption_unavailable", "无法使用此兑换码", "兑换码无效、已使用、已撤销、与当前邮箱账号不匹配、该邮箱已使用过内测码，或账号已经是永久会员。")
			return
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法兑换", "请稍后重试。")
			return
		}
		status, err := service.GetMembership(c.Request.Context(), session.User.ID)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "兑换已处理但暂时无法读取结果", "请刷新会员状态。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{
			"membership": status, "codeHint": result.CodeHint, "redeemedAt": result.RedeemedAt,
		})
	})

	admin := group.Group("/admin/redemption-code-batches")
	admin.Use(requireSession(authService, config), RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA))
	admin.GET("", func(c *gin.Context) {
		if !membershipServiceAvailable(c, service) {
			return
		}
		items, err := service.ListBatches(c.Request.Context(), 100)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法读取兑换码批次", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	admin.POST("", requireCSRF(config), func(c *gin.Context) {
		if !membershipServiceAvailable(c, service) {
			return
		}
		var request createRedemptionBatchRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		created, err := service.CreateBatch(c.Request.Context(), membership.CreateBatchInput{
			Name: strings.TrimSpace(request.Name), PlanCode: strings.TrimSpace(request.PlanCode),
			GrantType: request.GrantType, GrantDays: request.GrantDays, Quantity: request.Quantity,
			RedeemBefore: request.RedeemBefore, Note: strings.TrimSpace(request.Note), CreatedBy: session.User.ID,
		})
		switch {
		case errors.Is(err, membership.ErrMembershipPlanAbsent):
			writeProblem(c, http.StatusBadRequest, "membership_plan_invalid", "会员方案无效", "请选择可用的会员方案。")
			return
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "redemption_batch_invalid", "无法创建兑换码批次", "请检查数量、权益类型和有效期。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusCreated, gin.H{"batch": created.Batch, "codes": created.Plaintext})
	})
	admin.DELETE("/:batchId", requireCSRF(config), func(c *gin.Context) {
		if !membershipServiceAvailable(c, service) {
			return
		}
		batchID, err := uuid.Parse(c.Param("batchId"))
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "batch_id_invalid", "批次标识无效", "请刷新后重试。")
			return
		}
		session, _ := authSessionFrom(c)
		if err := service.RevokeBatch(c.Request.Context(), batchID, session.User.ID); err != nil {
			if errors.Is(err, membership.ErrBatchNotFound) {
				writeProblem(c, http.StatusNotFound, "redemption_batch_not_found", "兑换码批次不存在", "批次可能已被删除。")
				return
			}
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法撤销兑换码批次", "请稍后重试。")
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func membershipServiceAvailable(c *gin.Context, service MembershipService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "会员服务暂不可用", "请稍后重试。")
	return false
}
