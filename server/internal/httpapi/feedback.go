package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/feedback"
)

type FeedbackService interface {
	Create(context.Context, feedback.CreateInput) (feedback.Entry, error)
	ListMine(context.Context, uuid.UUID, int) ([]feedback.Entry, error)
	ListAdmin(context.Context, feedback.AdminFilter) (feedback.AdminList, error)
	Update(context.Context, uuid.UUID, feedback.UpdateInput) (feedback.AdminEntry, error)
}

type createFeedbackRequest struct {
	Category     string `json:"category"`
	Subject      string `json:"subject"`
	Content      string `json:"content"`
	ContactEmail string `json:"contactEmail"`
}

type updateFeedbackRequest struct {
	Status       string `json:"status"`
	AdminReply   string `json:"adminReply"`
	InternalNote string `json:"internalNote"`
}

func registerFeedbackRoutes(group *gin.RouterGroup, service FeedbackService, authService AuthService, config AuthHTTPConfig) {
	me := group.Group("/me/feedback")
	me.Use(requireSession(authService, config))
	me.GET("", func(c *gin.Context) {
		if !feedbackServiceAvailable(c, service) {
			return
		}
		session, _ := authSessionFrom(c)
		items, err := service.ListMine(c.Request.Context(), session.User.ID, 50)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "feedback_unavailable", "暂时无法读取反馈记录", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	me.POST("", requireCSRF(config), func(c *gin.Context) {
		if !feedbackServiceAvailable(c, service) {
			return
		}
		var request createFeedbackRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		entry, err := service.Create(c.Request.Context(), feedback.CreateInput{
			UserID: session.User.ID, Category: request.Category, Subject: request.Subject,
			Content: request.Content, ContactEmail: request.ContactEmail,
		})
		switch {
		case errors.Is(err, feedback.ErrRateLimited):
			c.Header("Retry-After", "3600")
			writeProblem(c, http.StatusTooManyRequests, "feedback_rate_limited", "提交反馈过于频繁", "请稍后再试，或补充已有反馈。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "feedback_invalid", "无法提交反馈", "请检查类型、标题、内容和联系邮箱。")
		default:
			c.JSON(http.StatusCreated, gin.H{"feedback": entry})
		}
	})

	admin := group.Group("/admin/feedback")
	admin.Use(requireSession(authService, config), RequirePermission(auth.PermissionAdminFeedback, !config.DisableAdminMFA))
	admin.GET("", func(c *gin.Context) {
		if !feedbackServiceAvailable(c, service) {
			return
		}
		limit, offset, ok := parseAdminPagination(c, 50, 100)
		if !ok {
			return
		}
		result, err := service.ListAdmin(c.Request.Context(), feedback.AdminFilter{
			Query: c.Query("q"), Status: c.Query("status"), Category: c.Query("category"),
			Limit: limit, Offset: offset,
		})
		if errors.Is(err, feedback.ErrAdminFilterInvalid) {
			writeProblem(c, http.StatusBadRequest, "feedback_filter_invalid", "反馈筛选条件无效", "请检查搜索词、类型和状态。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "feedback_unavailable", "暂时无法读取反馈", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.PATCH("/:feedbackId", requireCSRF(config), func(c *gin.Context) {
		if !feedbackServiceAvailable(c, service) {
			return
		}
		feedbackID, ok := parseAdminUUID(c, "feedbackId", "feedback_id_invalid", "反馈标识无效")
		if !ok {
			return
		}
		var request updateFeedbackRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		entry, err := service.Update(c.Request.Context(), feedbackID, feedback.UpdateInput{
			Status: request.Status, AdminReply: request.AdminReply, InternalNote: request.InternalNote,
			ActorUserID: session.User.ID,
		})
		switch {
		case errors.Is(err, feedback.ErrNotFound):
			writeProblem(c, http.StatusNotFound, "feedback_not_found", "反馈不存在", "请刷新列表后重试。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "feedback_update_invalid", "无法更新反馈", "请检查处理状态和回复内容。")
		default:
			c.JSON(http.StatusOK, gin.H{"feedback": entry})
		}
	})
}

func feedbackServiceAvailable(c *gin.Context, service FeedbackService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "feedback_unavailable", "反馈服务暂不可用", "请稍后重试。")
	return false
}
