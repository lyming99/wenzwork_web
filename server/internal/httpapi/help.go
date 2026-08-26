package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/helpdocs"
)

type HelpDocumentReader interface {
	ListPublished(context.Context) ([]helpdocs.PublicDocumentSummary, error)
	GetPublished(context.Context, string) (helpdocs.PublicDocument, error)
}

type AdminHelpDocumentService interface {
	ListAdmin(context.Context, helpdocs.AdminDocumentFilter) (helpdocs.AdminDocumentList, error)
	GetAdmin(context.Context, uuid.UUID) (helpdocs.AdminDocument, error)
	Create(context.Context, helpdocs.SaveDocumentInput) (helpdocs.AdminDocument, error)
	Update(context.Context, uuid.UUID, helpdocs.SaveDocumentInput) (helpdocs.AdminDocument, error)
	Publish(context.Context, uuid.UUID, uuid.UUID) (helpdocs.AdminDocument, error)
	Archive(context.Context, uuid.UUID, uuid.UUID) error
}

type saveHelpDocumentRequest struct {
	Slug            string `json:"slug"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	Category        string `json:"category"`
	SortOrder       int    `json:"sortOrder"`
	ContentMarkdown string `json:"contentMarkdown"`
	Version         int64  `json:"version"`
}

func registerHelpRoutes(group *gin.RouterGroup, reader HelpDocumentReader, adminService AdminHelpDocumentService, authService AuthService, config AuthHTTPConfig) {
	group.GET("/help-documents", func(c *gin.Context) {
		if !helpReaderAvailable(c, reader) {
			return
		}
		items, err := reader.ListPublished(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "help_unavailable", "帮助中心暂不可用", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "public, max-age=60, stale-while-revalidate=300")
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	group.GET("/help-documents/:slug", func(c *gin.Context) {
		if !helpReaderAvailable(c, reader) {
			return
		}
		document, err := reader.GetPublished(c.Request.Context(), c.Param("slug"))
		if errors.Is(err, helpdocs.ErrDocumentNotFound) {
			writeProblem(c, http.StatusNotFound, "help_document_not_found", "帮助文章不存在", "文章可能尚未发布或已经归档。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "help_unavailable", "帮助文章暂不可用", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "public, max-age=300, stale-while-revalidate=3600")
		c.JSON(http.StatusOK, gin.H{"document": document})
	})

	admin := group.Group("/admin/help-documents")
	admin.Use(requireSession(authService, config), RequirePermission(auth.PermissionAdminHelpDocuments, !config.DisableAdminMFA))
	admin.GET("", func(c *gin.Context) {
		if !adminHelpServiceAvailable(c, adminService) {
			return
		}
		limit, offset, ok := parseAdminPagination(c, 50, 100)
		if !ok {
			return
		}
		result, err := adminService.ListAdmin(c.Request.Context(), helpdocs.AdminDocumentFilter{
			Query: c.Query("q"), Status: c.Query("status"), Limit: limit, Offset: offset,
		})
		if errors.Is(err, helpdocs.ErrDocumentFilterInvalid) {
			writeProblem(c, http.StatusBadRequest, "help_filter_invalid", "帮助文档筛选条件无效", "请检查搜索词和状态。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "help_admin_unavailable", "暂时无法读取帮助文档", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.GET("/:documentId", func(c *gin.Context) {
		if !adminHelpServiceAvailable(c, adminService) {
			return
		}
		documentID, ok := parseAdminUUID(c, "documentId", "help_document_id_invalid", "帮助文档标识无效")
		if !ok {
			return
		}
		document, err := adminService.GetAdmin(c.Request.Context(), documentID)
		if errors.Is(err, helpdocs.ErrDocumentNotFound) {
			writeProblem(c, http.StatusNotFound, "help_document_not_found", "帮助文档不存在", "请刷新列表后重试。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "help_admin_unavailable", "暂时无法读取帮助文档", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"document": document})
	})

	save := func(c *gin.Context, documentID uuid.UUID) {
		if !adminHelpServiceAvailable(c, adminService) {
			return
		}
		var request saveHelpDocumentRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		input := helpdocs.SaveDocumentInput{
			Slug: request.Slug, Title: request.Title, Description: request.Description,
			Category: request.Category, SortOrder: request.SortOrder, ContentMarkdown: request.ContentMarkdown,
			ExpectedVersion: request.Version, ActorUserID: session.User.ID,
		}
		var result helpdocs.AdminDocument
		var err error
		status := http.StatusCreated
		if documentID == uuid.Nil {
			result, err = adminService.Create(c.Request.Context(), input)
		} else {
			result, err = adminService.Update(c.Request.Context(), documentID, input)
			status = http.StatusOK
		}
		switch {
		case errors.Is(err, helpdocs.ErrDocumentNotFound):
			writeProblem(c, http.StatusNotFound, "help_document_not_found", "帮助文档不存在", "请刷新列表后重试。")
		case errors.Is(err, helpdocs.ErrDocumentSlugConflict):
			writeProblem(c, http.StatusConflict, "help_slug_conflict", "文章路径已被使用", "请更换 slug 后重试。")
		case errors.Is(err, helpdocs.ErrDocumentVersionConflict):
			writeProblem(c, http.StatusConflict, "help_version_conflict", "文档已被其他操作更新", "请刷新后合并更改。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "help_document_invalid", "无法保存帮助文档", "请检查标题、路径、分类和 Markdown 正文。")
		default:
			c.JSON(status, gin.H{"document": result})
		}
	}

	admin.POST("", requireCSRF(config), func(c *gin.Context) { save(c, uuid.Nil) })
	admin.PUT("/:documentId", requireCSRF(config), func(c *gin.Context) {
		documentID, ok := parseAdminUUID(c, "documentId", "help_document_id_invalid", "帮助文档标识无效")
		if ok {
			save(c, documentID)
		}
	})
	admin.POST("/:documentId/publish", requireCSRF(config), func(c *gin.Context) {
		if !adminHelpServiceAvailable(c, adminService) {
			return
		}
		documentID, ok := parseAdminUUID(c, "documentId", "help_document_id_invalid", "帮助文档标识无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		document, err := adminService.Publish(c.Request.Context(), documentID, session.User.ID)
		switch {
		case errors.Is(err, helpdocs.ErrDocumentNotFound):
			writeProblem(c, http.StatusNotFound, "help_document_not_found", "帮助文档不存在", "请刷新列表后重试。")
		case errors.Is(err, helpdocs.ErrDocumentAlreadyArchived):
			writeProblem(c, http.StatusConflict, "help_document_archived", "已归档文档不能发布", "请创建一篇新文档。")
		case errors.Is(err, helpdocs.ErrDocumentVersionConflict):
			writeProblem(c, http.StatusConflict, "help_version_conflict", "文档发布版本冲突", "请刷新后重试。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "help_publish_invalid", "无法发布帮助文档", "请确认 Markdown 正文可生成安全的静态内容。")
		default:
			c.JSON(http.StatusOK, gin.H{"document": document})
		}
	})
	admin.DELETE("/:documentId", requireCSRF(config), func(c *gin.Context) {
		if !adminHelpServiceAvailable(c, adminService) {
			return
		}
		documentID, ok := parseAdminUUID(c, "documentId", "help_document_id_invalid", "帮助文档标识无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		if err := adminService.Archive(c.Request.Context(), documentID, session.User.ID); err != nil {
			if errors.Is(err, helpdocs.ErrDocumentNotFound) {
				writeProblem(c, http.StatusNotFound, "help_document_not_found", "帮助文档不存在", "请刷新列表后重试。")
				return
			}
			writeProblem(c, http.StatusServiceUnavailable, "help_admin_unavailable", "暂时无法归档帮助文档", "请稍后重试。")
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func helpReaderAvailable(c *gin.Context, service HelpDocumentReader) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "help_unavailable", "帮助中心暂不可用", "请稍后重试。")
	return false
}

func adminHelpServiceAvailable(c *gin.Context, service AdminHelpDocumentService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "help_admin_unavailable", "帮助文档管理暂不可用", "请稍后重试。")
	return false
}
