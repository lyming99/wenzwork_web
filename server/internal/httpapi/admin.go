package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/catalog"
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
	"github.com/wenzwork/wenzwork-web/server/internal/releaseassets"
)

type AdminUserService interface {
	ListAdminUsers(context.Context, auth.AdminUserListFilter) (auth.AdminUserList, error)
	CreateAdminUser(context.Context, auth.AdminCreateUserInput) (auth.AdminUser, error)
	SetAdminUserStatus(context.Context, uuid.UUID, uuid.UUID, string) (auth.AdminUser, error)
}

type AdminMembershipService interface {
	SetUserMembership(context.Context, uuid.UUID, membership.SetMembershipInput) (membership.MembershipStatus, error)
	CancelUserMembership(context.Context, uuid.UUID, uuid.UUID, string) error
	ListRedemptionCodes(context.Context, membership.RedemptionCodeFilter) (membership.RedemptionCodeList, error)
	RevokeRedemptionCode(context.Context, uuid.UUID, uuid.UUID) error
}

type AdminReleaseService interface {
	ListAdminReleases(context.Context, int) ([]catalog.AdminRelease, error)
	CreateRelease(context.Context, catalog.SaveReleaseInput) (catalog.AdminRelease, error)
	UpdateRelease(context.Context, uuid.UUID, catalog.SaveReleaseInput) (catalog.AdminRelease, error)
	PublishRelease(context.Context, uuid.UUID, uuid.UUID) (catalog.AdminRelease, error)
	WithdrawRelease(context.Context, uuid.UUID, uuid.UUID) error
	DeleteRelease(context.Context, uuid.UUID, uuid.UUID) error
	GetReleaseDeliverySettings(context.Context) (catalog.ReleaseDeliverySettings, error)
	UpdateReleaseDeliverySettings(context.Context, catalog.UpdateReleaseDeliverySettingsInput) (catalog.ReleaseDeliverySettings, error)
	GetReleaseSourceSettings(context.Context) (catalog.ReleaseSourceSettings, error)
	GetReleaseSourceCredentials(context.Context) (catalog.ReleaseSourceCredentials, error)
	UpdateReleaseSourceSettings(context.Context, catalog.UpdateReleaseSourceSettingsInput) (catalog.ReleaseSourceSettings, error)
}

type AdminReleaseAccessKeyService interface {
	ReleaseAccessKeyVerifier
	GetReleaseAccessKeySettings(context.Context) (catalog.ReleaseAccessKeySettings, error)
	UpdateReleaseAccessKeySettings(context.Context, catalog.UpdateReleaseAccessKeySettingsInput) (catalog.ReleaseAccessKeySettings, error)
}

type projectReleaseSourceService interface {
	ListReleaseSourceSettings(context.Context) ([]catalog.ReleaseSourceSettings, error)
	GetReleaseSourceSettingsForProject(context.Context, string) (catalog.ReleaseSourceSettings, error)
	GetReleaseSourceCredentialsForProject(context.Context, string) (catalog.ReleaseSourceCredentials, error)
}

type ReleaseAssetUploadService interface {
	Upload(context.Context, objectstore.ReleaseAssetUploadInput, io.Reader) (objectstore.ReleaseAssetUpload, error)
}

type ReleaseAssetSourceService interface {
	ImportRemote(context.Context, releaseassets.RemoteImportInput) (releaseassets.StoredAsset, error)
	LatestGitHubRelease(context.Context, string, string) (releaseassets.GitHubRelease, error)
	ImportLatestMirrorRelease(context.Context, string, string) (releaseassets.MirrorReleaseImport, error)
}

type AdminPricingService interface {
	ListAdminPricingPlans(context.Context) ([]catalog.AdminPricingPlan, error)
	CreatePricingPlan(context.Context, catalog.SavePricingPlanInput) (catalog.AdminPricingPlan, error)
	UpdatePricingPlan(context.Context, uuid.UUID, catalog.SavePricingPlanInput) (catalog.AdminPricingPlan, error)
	PublishPricingPlan(context.Context, uuid.UUID, catalog.PricingPlanActionInput) (catalog.AdminPricingPlan, error)
	ArchivePricingPlan(context.Context, uuid.UUID, catalog.PricingPlanActionInput) (catalog.AdminPricingPlan, error)
}

type createAdminUserRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

type setAdminUserStatusRequest struct {
	Status string `json:"status"`
}

type setAdminMembershipRequest struct {
	PlanCode  string     `json:"planCode"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Reason    string     `json:"reason"`
}

type saveReleaseAssetRequest struct {
	Platform        string `json:"platform"`
	Architecture    string `json:"architecture"`
	FileName        string `json:"fileName"`
	FileSizeBytes   int64  `json:"fileSizeBytes"`
	SHA256          string `json:"sha256"`
	SignatureStatus string `json:"signatureStatus"`
	Source          string `json:"source"`
	ObjectKey       string `json:"objectKey"`
	DownloadURL     string `json:"downloadUrl"`
}

type importReleaseAssetRequest struct {
	Version      string `json:"version"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
	DownloadURL  string `json:"downloadUrl"`
}

type updateReleaseDeliverySettingsRequest struct {
	DownloadMode    string `json:"downloadMode"`
	S3URLPrefix     string `json:"s3UrlPrefix"`
	ExpectedVersion int64  `json:"expectedVersion"`
}

type updateReleaseAccessKeySettingsRequest struct {
	AccessKey       string `json:"accessKey"`
	ExpectedVersion int64  `json:"expectedVersion"`
}

type updateReleaseSourceSettingsRequest struct {
	Project          string  `json:"project"`
	GitHubRepository string  `json:"githubRepository"`
	GitHubToken      *string `json:"githubToken"`
	ClearGitHubToken bool    `json:"clearGithubToken"`
	MirrorBaseURL    string  `json:"mirrorBaseUrl"`
	ExpectedVersion  int64   `json:"expectedVersion"`
}

type saveReleaseRequest struct {
	Project      string                    `json:"project"`
	Version      string                    `json:"version"`
	Channel      string                    `json:"channel"`
	Title        string                    `json:"title"`
	Summary      string                    `json:"summary"`
	ReleaseNotes string                    `json:"releaseNotes"`
	Status       string                    `json:"status"`
	Assets       []saveReleaseAssetRequest `json:"assets"`
}

type savePricingPlanRequest struct {
	Code               string   `json:"code"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	PriceMinor         *int64   `json:"priceMinor"`
	OriginalPriceMinor *int64   `json:"originalPriceMinor"`
	Currency           string   `json:"currency"`
	BillingPeriod      string   `json:"billingPeriod"`
	Features           []string `json:"features"`
	SortOrder          int      `json:"sortOrder"`
	ExpectedVersion    int64    `json:"expectedVersion"`
	ConfirmPriceChange bool     `json:"confirmPriceChange"`
}

type pricingPlanActionRequest struct {
	ExpectedVersion int64 `json:"expectedVersion"`
	Confirm         bool  `json:"confirm"`
}

func registerAdminRoutes(group *gin.RouterGroup, users AdminUserService, memberships AdminMembershipService, releases AdminReleaseService, uploads ReleaseAssetUploadService, sources ReleaseAssetSourceService, accessKeys AdminReleaseAccessKeyService, pricing AdminPricingService, authService AuthService, config AuthHTTPConfig) {
	admin := group.Group("/admin")
	admin.Use(requireSession(authService, config))

	admin.GET("/users", RequirePermission(auth.PermissionAdminUsersRead, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminUserServiceAvailable(c, users) {
			return
		}
		limit, offset, ok := parseAdminPagination(c, 50, 100)
		if !ok {
			return
		}
		result, err := users.ListAdminUsers(c.Request.Context(), auth.AdminUserListFilter{
			Query: c.Query("q"), Status: c.Query("status"), Limit: limit, Offset: offset,
		})
		if errors.Is(err, auth.ErrAdminUserFilterInvalid) {
			writeProblem(c, http.StatusBadRequest, "admin_user_filter_invalid", "无法读取账户列表", "请检查搜索条件后重试。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "admin_users_unavailable", "账户列表暂不可用", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, result)
	})

	admin.POST("/users", RequirePermission(auth.PermissionAdminUsersManage, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminUserServiceAvailable(c, users) {
			return
		}
		var request createAdminUserRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		created, err := users.CreateAdminUser(c.Request.Context(), auth.AdminCreateUserInput{
			Email: request.Email, Password: request.Password, DisplayName: request.DisplayName,
			ActorUserID: session.User.ID,
		})
		switch {
		case errors.Is(err, auth.ErrEmailUnavailable):
			writeProblem(c, http.StatusConflict, "email_unavailable", "邮箱已被使用", "请使用其他邮箱地址。")
		case errors.Is(err, auth.ErrInvalidPassword):
			writeProblem(c, http.StatusBadRequest, "invalid_password", "密码不符合要求", "密码需包含 8 到 128 个字符。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "admin_user_invalid", "无法创建账户", "请检查邮箱、显示名称和初始密码。")
		default:
			c.JSON(http.StatusCreated, gin.H{"user": created})
		}
	})

	admin.PATCH("/users/:userId/status", RequirePermission(auth.PermissionAdminUsersManage, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminUserServiceAvailable(c, users) {
			return
		}
		userID, ok := parseAdminUUID(c, "userId", "user_id_invalid", "账户标识无效")
		if !ok {
			return
		}
		var request setAdminUserStatusRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		updated, err := users.SetAdminUserStatus(c.Request.Context(), userID, session.User.ID, strings.TrimSpace(request.Status))
		switch {
		case errors.Is(err, auth.ErrAdminUserNotFound):
			writeProblem(c, http.StatusNotFound, "admin_user_not_found", "账户不存在", "请刷新列表后重试。")
		case errors.Is(err, auth.ErrAdminUserSelfDisable), errors.Is(err, auth.ErrLastSuperAdmin):
			writeProblem(c, http.StatusConflict, "admin_user_protected", "不能禁用此账户", "当前操作会使后台失去可用的超级管理员。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "admin_user_status_invalid", "无法更新账户状态", "请刷新后重试。")
		default:
			c.JSON(http.StatusOK, gin.H{"user": updated})
		}
	})

	admin.PUT("/users/:userId/membership", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminMembershipServiceAvailable(c, memberships) {
			return
		}
		userID, ok := parseAdminUUID(c, "userId", "user_id_invalid", "账户标识无效")
		if !ok {
			return
		}
		var request setAdminMembershipRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		status, err := memberships.SetUserMembership(c.Request.Context(), userID, membership.SetMembershipInput{
			PlanCode: request.PlanCode, ExpiresAt: request.ExpiresAt, Reason: request.Reason,
			ActorUserID: session.User.ID,
		})
		switch {
		case errors.Is(err, membership.ErrUserNotFound):
			writeProblem(c, http.StatusNotFound, "admin_user_not_found", "账户不存在或已禁用", "请刷新账户列表后重试。")
		case errors.Is(err, membership.ErrMembershipPlanAbsent):
			writeProblem(c, http.StatusBadRequest, "membership_plan_invalid", "会员方案无效", "请选择可用的会员方案。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "membership_adjustment_invalid", "无法设置会员权限", "请选择未来的到期时间，或设置为长期会员。")
		default:
			c.JSON(http.StatusOK, gin.H{"membership": status})
		}
	})

	admin.DELETE("/users/:userId/membership", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminMembershipServiceAvailable(c, memberships) {
			return
		}
		userID, ok := parseAdminUUID(c, "userId", "user_id_invalid", "账户标识无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		err := memberships.CancelUserMembership(c.Request.Context(), userID, session.User.ID, "管理员取消会员权限")
		if errors.Is(err, membership.ErrMembershipNotFound) {
			writeProblem(c, http.StatusNotFound, "membership_not_found", "账户当前没有可取消的会员权限", "请刷新账户列表。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法取消会员权限", "请稍后重试。")
			return
		}
		c.Status(http.StatusNoContent)
	})

	admin.GET("/redemption-codes", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminMembershipServiceAvailable(c, memberships) {
			return
		}
		limit, offset, ok := parseAdminPagination(c, 100, 200)
		if !ok {
			return
		}
		var batchID uuid.UUID
		if raw := strings.TrimSpace(c.Query("batchId")); raw != "" {
			batchID, _ = uuid.Parse(raw)
			if batchID == uuid.Nil {
				writeProblem(c, http.StatusBadRequest, "batch_id_invalid", "批次标识无效", "请重新选择兑换码批次。")
				return
			}
		}
		result, err := memberships.ListRedemptionCodes(c.Request.Context(), membership.RedemptionCodeFilter{
			BatchID: batchID, Status: c.Query("status"), Limit: limit, Offset: offset,
		})
		if errors.Is(err, membership.ErrCodeFilterInvalid) {
			writeProblem(c, http.StatusBadRequest, "redemption_code_filter_invalid", "无法读取兑换码状态", "请检查筛选条件。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法读取兑换码状态", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, result)
	})

	admin.DELETE("/redemption-codes/:codeId", RequirePermission(auth.PermissionAdminMemberships, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminMembershipServiceAvailable(c, memberships) {
			return
		}
		codeID, ok := parseAdminUUID(c, "codeId", "redemption_code_id_invalid", "兑换码标识无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		err := memberships.RevokeRedemptionCode(c.Request.Context(), codeID, session.User.ID)
		switch {
		case errors.Is(err, membership.ErrCodeNotFound):
			writeProblem(c, http.StatusNotFound, "redemption_code_not_found", "兑换码不存在", "请刷新列表后重试。")
		case errors.Is(err, membership.ErrCodeAlreadyUsed):
			writeProblem(c, http.StatusConflict, "redemption_code_redeemed", "已兑换的兑换码不能删除", "已发放的会员权益不会被自动撤销。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "暂时无法删除兑换码", "请稍后重试。")
		default:
			c.Status(http.StatusNoContent)
		}
	})

	admin.GET("/pricing-plans", RequirePermission(auth.PermissionAdminPricing, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminPricingServiceAvailable(c, pricing) {
			return
		}
		items, err := pricing.ListAdminPricingPlans(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "pricing_admin_unavailable", "暂时无法读取价格套餐", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	savePricingPlan := func(c *gin.Context, planID uuid.UUID) {
		if !adminPricingServiceAvailable(c, pricing) {
			return
		}
		var request savePricingPlanRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		input := catalog.SavePricingPlanInput{
			Code: request.Code, Name: request.Name, Description: request.Description,
			PriceMinor: request.PriceMinor, OriginalPriceMinor: request.OriginalPriceMinor,
			Currency: request.Currency, BillingPeriod: request.BillingPeriod,
			Features: request.Features, SortOrder: request.SortOrder, ExpectedVersion: request.ExpectedVersion,
			ConfirmPriceChange: request.ConfirmPriceChange, ActorUserID: session.User.ID,
		}
		var result catalog.AdminPricingPlan
		var err error
		status := http.StatusCreated
		if planID == uuid.Nil {
			result, err = pricing.CreatePricingPlan(c.Request.Context(), input)
		} else {
			result, err = pricing.UpdatePricingPlan(c.Request.Context(), planID, input)
			status = http.StatusOK
		}
		if err != nil {
			writePricingPlanError(c, err)
			return
		}
		c.JSON(status, gin.H{"plan": result})
	}

	admin.POST("/pricing-plans", RequirePermission(auth.PermissionAdminPricing, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		savePricingPlan(c, uuid.Nil)
	})
	admin.PUT("/pricing-plans/:planId", RequirePermission(auth.PermissionAdminPricing, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		planID, ok := parseAdminUUID(c, "planId", "pricing_plan_id_invalid", "价格套餐标识无效")
		if !ok {
			return
		}
		savePricingPlan(c, planID)
	})
	admin.POST("/pricing-plans/:planId/publish", RequirePermission(auth.PermissionAdminPricing, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminPricingServiceAvailable(c, pricing) {
			return
		}
		planID, ok := parseAdminUUID(c, "planId", "pricing_plan_id_invalid", "价格套餐标识无效")
		if !ok {
			return
		}
		var request pricingPlanActionRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := pricing.PublishPricingPlan(c.Request.Context(), planID, catalog.PricingPlanActionInput{
			ExpectedVersion: request.ExpectedVersion, Confirm: request.Confirm, ActorUserID: session.User.ID,
		})
		if err != nil {
			writePricingPlanError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"plan": result})
	})
	admin.POST("/pricing-plans/:planId/archive", RequirePermission(auth.PermissionAdminPricing, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminPricingServiceAvailable(c, pricing) {
			return
		}
		planID, ok := parseAdminUUID(c, "planId", "pricing_plan_id_invalid", "价格套餐标识无效")
		if !ok {
			return
		}
		var request pricingPlanActionRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := pricing.ArchivePricingPlan(c.Request.Context(), planID, catalog.PricingPlanActionInput{
			ExpectedVersion: request.ExpectedVersion, Confirm: request.Confirm, ActorUserID: session.User.ID,
		})
		if err != nil {
			writePricingPlanError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"plan": result})
	})

	admin.POST("/release-assets/upload", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !releaseAssetUploadServiceAvailable(c, uploads) {
			return
		}
		extendReleaseTransferDeadlines(c, true)
		fileSizeBytes, err := strconv.ParseInt(strings.TrimSpace(c.Query("fileSizeBytes")), 10, 64)
		if err != nil || fileSizeBytes < 1 || fileSizeBytes > objectstore.MaxReleaseAssetBytes {
			writeProblem(c, http.StatusBadRequest, "release_upload_invalid", "无法上传安装文件", "文件大小参数无效。")
			return
		}
		if c.Request.ContentLength >= 0 && c.Request.ContentLength != fileSizeBytes {
			writeProblem(c, http.StatusBadRequest, "release_upload_size_mismatch", "安装文件大小不一致", "请重新选择文件后上传。")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, objectstore.MaxReleaseAssetBytes)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Hour)
		defer cancel()
		result, err := uploads.Upload(ctx, objectstore.ReleaseAssetUploadInput{
			Version: c.Query("version"), Platform: c.Query("platform"), Architecture: c.Query("architecture"),
			FileName: c.Query("fileName"), FileSizeBytes: fileSizeBytes,
			SHA256: c.Query("sha256"), ContentType: c.GetHeader("Content-Type"),
		}, c.Request.Body)
		switch {
		case errors.Is(err, objectstore.ErrReleaseUploadInvalid):
			writeProblem(c, http.StatusBadRequest, "release_upload_invalid", "无法上传安装文件", "请检查版本号、平台、架构、文件名、文件大小和 SHA-256。")
		case errors.Is(err, objectstore.ErrReleaseUploadTooLarge):
			writeProblem(c, http.StatusRequestEntityTooLarge, "release_upload_too_large", "安装文件过大", "单个安装文件不能超过 5 GB。")
		case errors.Is(err, objectstore.ErrReleaseUploadSizeMismatch), errors.Is(err, objectstore.ErrReleaseUploadChecksumMismatch):
			writeProblem(c, http.StatusUnprocessableEntity, "release_upload_integrity_failed", "安装文件完整性校验失败", "请重新选择原始文件后上传。")
		case err != nil:
			writeProblem(c, http.StatusBadGateway, "release_upload_unavailable", "对象存储写入失败", "请在管理端检查 S3 配置与连通性后重试。")
		default:
			c.JSON(http.StatusCreated, result)
		}
	})

	admin.POST("/release-assets/import", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !releaseAssetSourceServiceAvailable(c, sources) {
			return
		}
		extendReleaseTransferDeadlines(c, false)
		var request importReleaseAssetRequest
		if !decodeJSON(c, &request) {
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Hour)
		defer cancel()
		result, err := sources.ImportRemote(ctx, releaseassets.RemoteImportInput{
			Version: request.Version, Platform: request.Platform,
			Architecture: request.Architecture, DownloadURL: request.DownloadURL,
		})
		writeReleaseAssetImportResult(c, result, err)
	})

	admin.GET("/github-releases/latest", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) || !releaseAssetSourceServiceAvailable(c, sources) {
			return
		}
		project := strings.ToLower(strings.TrimSpace(c.DefaultQuery("project", catalog.ReleaseProjectDesktop)))
		if !catalog.ValidReleaseProject(project) {
			writeProblem(c, http.StatusBadRequest, "release_project_invalid", "项目类型无效", "project 仅支持 web、desktop 或 mobile。")
			return
		}
		var credentials catalog.ReleaseSourceCredentials
		var err error
		if projectSources, ok := releases.(projectReleaseSourceService); ok {
			credentials, err = projectSources.GetReleaseSourceCredentialsForProject(c.Request.Context(), project)
		} else {
			credentials, err = releases.GetReleaseSourceCredentials(c.Request.Context())
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_source_unavailable", "暂时无法读取 GitHub 仓库设置", "请检查系统加密密钥和数据库配置后重试。")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		result, err := sources.LatestGitHubRelease(ctx, credentials.GitHubRepository, credentials.GitHubToken)
		switch {
		case errors.Is(err, releaseassets.ErrGitHubReleaseNotFound):
			detail := "请确认仓库 " + credentials.GitHubRepository + " 已发布正式 Release，且当前 Token 可以访问该仓库。"
			if credentials.GitHubToken == "" {
				detail = "若仓库 " + credentials.GitHubRepository + " 为私有仓库，请先在发布设置中配置访问 Token；若为公开仓库，请确认已发布正式 Release。"
			}
			writeProblem(c, http.StatusNotFound, "github_release_not_found", "无法读取 GitHub 正式 Release", detail)
		case errors.Is(err, releaseassets.ErrGitHubAuthentication):
			writeProblem(c, http.StatusBadGateway, "github_authentication_failed", "GitHub Token 无效或权限不足", "请在发布设置中更换可访问当前仓库的 Token。")
		case errors.Is(err, releaseassets.ErrGitHubRateLimited):
			writeProblem(c, http.StatusTooManyRequests, "github_rate_limited", "GitHub 查询次数已受限", "请稍后重试，或在发布设置中配置访问 Token 以提高查询限额。")
		case errors.Is(err, releaseassets.ErrGitHubUnconfigured):
			writeProblem(c, http.StatusServiceUnavailable, "github_release_unconfigured", "GitHub Release 尚未配置", "请先在发布设置中填写并保存 GitHub 仓库。")
		case err != nil:
			writeProblem(c, http.StatusBadGateway, "github_release_unavailable", "暂时无法读取 GitHub Release", "请检查 GitHub 网络与仓库配置。")
		default:
			c.JSON(http.StatusOK, result)
		}
	})

	admin.POST("/mirror-releases/latest/import", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) || !releaseAssetSourceServiceAvailable(c, sources) {
			return
		}
		project := strings.ToLower(strings.TrimSpace(c.DefaultQuery("project", catalog.ReleaseProjectDesktop)))
		if !catalog.ValidReleaseProject(project) {
			writeProblem(c, http.StatusBadRequest, "release_project_invalid", "项目类型无效", "project 仅支持 web、desktop 或 mobile。")
			return
		}
		var sourceSettings catalog.ReleaseSourceSettings
		var err error
		if projectSources, ok := releases.(projectReleaseSourceService); ok {
			sourceSettings, err = projectSources.GetReleaseSourceSettingsForProject(c.Request.Context(), project)
		} else {
			sourceSettings, err = releases.GetReleaseSourceSettings(c.Request.Context())
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_source_unavailable", "暂时无法读取镜像站设置", "请检查数据库配置后重试。")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		result, err := sources.ImportLatestMirrorRelease(ctx, sourceSettings.MirrorBaseURL, project)
		switch {
		case errors.Is(err, releaseassets.ErrMirrorUnconfigured):
			writeProblem(c, http.StatusConflict, "release_mirror_unconfigured", "镜像站尚未配置", "请先为当前项目填写并保存镜像站地址。")
		case errors.Is(err, releaseassets.ErrMirrorURLInvalid), errors.Is(err, releaseassets.ErrRemoteAddressForbidden):
			writeProblem(c, http.StatusBadRequest, "release_mirror_url_invalid", "镜像站地址不可用", "镜像站必须是可公开访问的 HTTP(S) 地址，不能包含凭据、查询参数或片段。")
		case errors.Is(err, releaseassets.ErrMirrorReleaseNotFound):
			writeProblem(c, http.StatusNotFound, "release_mirror_release_not_found", "镜像站没有可拉取版本", "请确认镜像站已发布当前项目的稳定版本。")
		case errors.Is(err, releaseassets.ErrRemoteAssetTooLarge):
			writeProblem(c, http.StatusRequestEntityTooLarge, "release_mirror_asset_too_large", "镜像安装包过大", "单个安装文件不能超过 5 GB。")
		case errors.Is(err, releaseassets.ErrMirrorCatalogInvalid):
			writeProblem(c, http.StatusUnprocessableEntity, "release_mirror_integrity_failed", "镜像版本目录无效", "镜像目录中的项目、版本、文件名、大小、SHA-256 或下载链接不符合要求。")
		case errors.Is(err, releaseassets.ErrMirrorUnavailable):
			writeProblem(c, http.StatusBadGateway, "release_mirror_unavailable", "暂时无法从镜像站读取版本", "请检查镜像站地址、网络和版本目录状态。")
		case err != nil:
			writeProblem(c, http.StatusBadGateway, "release_mirror_import_failed", "镜像版本读取失败", "请检查镜像站配置与连通性后重试。")
		default:
			c.JSON(http.StatusCreated, result)
		}
	})

	admin.GET("/release-source-settings", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		var settings []catalog.ReleaseSourceSettings
		var err error
		if projectSources, ok := releases.(projectReleaseSourceService); ok {
			settings, err = projectSources.ListReleaseSourceSettings(c.Request.Context())
		} else {
			var setting catalog.ReleaseSourceSettings
			setting, err = releases.GetReleaseSourceSettings(c.Request.Context())
			settings = []catalog.ReleaseSourceSettings{setting}
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_source_unavailable", "暂时无法读取 Release 来源设置", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": settings})
	})

	admin.PUT("/release-source-settings", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		var request updateReleaseSourceSettingsRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		settings, err := releases.UpdateReleaseSourceSettings(c.Request.Context(), catalog.UpdateReleaseSourceSettingsInput{
			Project:          request.Project,
			GitHubRepository: request.GitHubRepository,
			GitHubToken:      request.GitHubToken,
			ClearGitHubToken: request.ClearGitHubToken,
			MirrorBaseURL:    request.MirrorBaseURL,
			ExpectedVersion:  request.ExpectedVersion,
			ActorUserID:      session.User.ID,
		})
		switch {
		case errors.Is(err, catalog.ErrReleaseSourceInvalid):
			writeProblem(c, http.StatusBadRequest, "release_source_invalid", "无法保存 Release 来源设置", "请检查 owner/repository、Token 和镜像站 HTTP(S) 地址，并且不要同时替换和清除 Token。")
		case errors.Is(err, catalog.ErrReleaseSourceConflict):
			writeProblem(c, http.StatusConflict, "release_source_conflict", "Release 来源设置已被其他管理员修改", "请刷新后重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "release_source_unavailable", "暂时无法保存 Release 来源设置", "请稍后重试。")
		default:
			c.JSON(http.StatusOK, settings)
		}
	})

	admin.GET("/release-delivery-settings", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		settings, err := releases.GetReleaseDeliverySettings(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_delivery_unavailable", "暂时无法读取下载设置", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, settings)
	})

	admin.PUT("/release-delivery-settings", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		var request updateReleaseDeliverySettingsRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		settings, err := releases.UpdateReleaseDeliverySettings(c.Request.Context(), catalog.UpdateReleaseDeliverySettingsInput{
			DownloadMode: request.DownloadMode, S3URLPrefix: request.S3URLPrefix,
			ExpectedVersion: request.ExpectedVersion, ActorUserID: session.User.ID,
		})
		switch {
		case errors.Is(err, catalog.ErrReleaseDeliveryInvalid):
			writeProblem(c, http.StatusBadRequest, "release_delivery_invalid", "无法保存下载设置", "下载方式无效；S3 链接模式必须填写有效的 HTTP(S) 链接前缀。")
		case errors.Is(err, catalog.ErrReleaseDeliveryConflict):
			writeProblem(c, http.StatusConflict, "release_delivery_conflict", "下载设置已被其他管理员修改", "请刷新后重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "release_delivery_unavailable", "暂时无法保存下载设置", "请稍后重试。")
		default:
			c.JSON(http.StatusOK, settings)
		}
	})

	admin.GET("/release-access-key-settings", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), func(c *gin.Context) {
		if !releaseAccessKeyServiceAvailable(c, accessKeys) {
			return
		}
		settings, err := accessKeys.GetReleaseAccessKeySettings(c.Request.Context())
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_access_key_unavailable", "暂时无法读取 Release 推送密钥设置", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, settings)
	})

	admin.PUT("/release-access-key-settings", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !releaseAccessKeyServiceAvailable(c, accessKeys) {
			return
		}
		var request updateReleaseAccessKeySettingsRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		settings, err := accessKeys.UpdateReleaseAccessKeySettings(c.Request.Context(), catalog.UpdateReleaseAccessKeySettingsInput{
			AccessKey: request.AccessKey, ExpectedVersion: request.ExpectedVersion, ActorUserID: session.User.ID,
		})
		switch {
		case errors.Is(err, catalog.ErrReleaseAccessKeyInvalid):
			writeProblem(c, http.StatusBadRequest, "release_access_key_invalid", "无法保存 Release 推送密钥", "密钥必须是 release_ 开头、长度正确的安全密钥。")
		case errors.Is(err, catalog.ErrReleaseAccessKeyConflict):
			writeProblem(c, http.StatusConflict, "release_access_key_conflict", "Release 推送密钥已被其他管理员修改", "请刷新后重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "release_access_key_unavailable", "暂时无法保存 Release 推送密钥", "请稍后重试。")
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, settings)
		}
	})

	admin.GET("/releases", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		limit, _, ok := parseAdminPagination(c, 50, 100)
		if !ok {
			return
		}
		items, err := releases.ListAdminReleases(c.Request.Context(), limit)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_admin_unavailable", "暂时无法读取软件版本", "请稍后重试。")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})

	saveRelease := func(c *gin.Context, releaseID uuid.UUID) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		var request saveReleaseRequest
		if !decodeJSON(c, &request) {
			return
		}
		session, _ := authSessionFrom(c)
		assets := make([]catalog.SaveReleaseAssetInput, 0, len(request.Assets))
		for _, asset := range request.Assets {
			assets = append(assets, catalog.SaveReleaseAssetInput{
				Platform: asset.Platform, Architecture: asset.Architecture, FileName: asset.FileName,
				FileSizeBytes: asset.FileSizeBytes, SHA256: asset.SHA256,
				SignatureStatus: asset.SignatureStatus, Source: asset.Source,
				ObjectKey: asset.ObjectKey, DownloadURL: asset.DownloadURL,
			})
		}
		input := catalog.SaveReleaseInput{
			Project: request.Project, Version: request.Version, Channel: request.Channel, Title: request.Title,
			Summary: request.Summary, ReleaseNotes: request.ReleaseNotes, Status: request.Status,
			Assets: assets, ActorUserID: session.User.ID,
		}
		var result catalog.AdminRelease
		var err error
		status := http.StatusCreated
		if releaseID == uuid.Nil {
			result, err = releases.CreateRelease(c.Request.Context(), input)
		} else {
			result, err = releases.UpdateRelease(c.Request.Context(), releaseID, input)
			status = http.StatusOK
		}
		switch {
		case errors.Is(err, catalog.ErrReleaseNotFound):
			writeProblem(c, http.StatusNotFound, "release_not_found", "软件版本不存在", "请刷新版本列表。")
		case errors.Is(err, catalog.ErrReleaseAssetMismatch):
			writeProblem(c, http.StatusBadRequest, "release_asset_mismatch", "部署包与版本不一致", "Web 部署包文件名中的版本、平台和架构必须与发布记录完全一致。")
		case errors.Is(err, catalog.ErrReleaseVersionConflict):
			writeProblem(c, http.StatusConflict, "release_version_conflict", "版本号已存在", "请使用唯一的软件版本号。")
		case errors.Is(err, catalog.ErrReleaseWithdrawn):
			writeProblem(c, http.StatusConflict, "release_withdrawn", "已下架版本不能编辑", "请创建一个新版本。")
		case err != nil:
			writeProblem(c, http.StatusBadRequest, "release_invalid", "无法保存软件版本", "请检查版本号、更新公告和文件信息。发布版本必须至少包含一个文件。")
		default:
			c.JSON(status, gin.H{"release": result})
		}
	}

	admin.POST("/releases", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		saveRelease(c, uuid.Nil)
	})
	admin.PUT("/releases/:releaseId", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		releaseID, ok := parseAdminUUID(c, "releaseId", "release_id_invalid", "软件版本标识无效")
		if !ok {
			return
		}
		saveRelease(c, releaseID)
	})
	admin.POST("/releases/:releaseId/publish", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		releaseID, ok := parseAdminUUID(c, "releaseId", "release_id_invalid", "软件版本标识无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		result, err := releases.PublishRelease(c.Request.Context(), releaseID, session.User.ID)
		switch {
		case errors.Is(err, catalog.ErrReleaseNotFound):
			writeProblem(c, http.StatusNotFound, "release_not_found", "软件版本不存在", "请刷新版本列表。")
		case errors.Is(err, catalog.ErrReleaseWithdrawn):
			writeProblem(c, http.StatusConflict, "release_withdrawn", "已下架版本不能重新发布", "请创建一个新版本。")
		case errors.Is(err, catalog.ErrReleaseAssetMismatch):
			writeProblem(c, http.StatusConflict, "release_asset_mismatch", "部署包与版本不一致", "请重新关联与当前版本、平台和架构一致的 Web 部署包。")
		case errors.Is(err, catalog.ErrReleaseInvalid):
			writeProblem(c, http.StatusConflict, "release_not_ready", "草稿尚不能发布", "请先补充至少一个安装文件。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "release_admin_unavailable", "暂时无法发布软件版本", "请稍后重试。")
		default:
			c.JSON(http.StatusOK, gin.H{"release": result})
		}
	})
	admin.DELETE("/releases/:releaseId", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		releaseID, ok := parseAdminUUID(c, "releaseId", "release_id_invalid", "软件版本标识无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		if err := releases.WithdrawRelease(c.Request.Context(), releaseID, session.User.ID); err != nil {
			if errors.Is(err, catalog.ErrReleaseNotFound) {
				writeProblem(c, http.StatusNotFound, "release_not_found", "软件版本不存在", "请刷新版本列表。")
				return
			}
			writeProblem(c, http.StatusServiceUnavailable, "release_admin_unavailable", "暂时无法下架软件版本", "请稍后重试。")
			return
		}
		c.Status(http.StatusNoContent)
	})
	admin.DELETE("/releases/:releaseId/permanent", RequirePermission(auth.PermissionAdminReleases, !config.DisableAdminMFA), requireCSRF(config), func(c *gin.Context) {
		if !adminReleaseServiceAvailable(c, releases) {
			return
		}
		releaseID, ok := parseAdminUUID(c, "releaseId", "release_id_invalid", "软件版本标识无效")
		if !ok {
			return
		}
		session, _ := authSessionFrom(c)
		if err := releases.DeleteRelease(c.Request.Context(), releaseID, session.User.ID); err != nil {
			if errors.Is(err, catalog.ErrReleaseNotFound) {
				writeProblem(c, http.StatusNotFound, "release_not_found", "软件版本不存在", "请刷新版本列表。")
				return
			}
			writeProblem(c, http.StatusServiceUnavailable, "release_delete_unavailable", "暂时无法删除软件版本", "请稍后重试。")
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func extendReleaseTransferDeadlines(c *gin.Context, includeRequestBody bool) {
	deadline := time.Now().Add(2 * time.Hour)
	controller := http.NewResponseController(c.Writer)
	if includeRequestBody {
		_ = controller.SetReadDeadline(deadline)
	}
	_ = controller.SetWriteDeadline(deadline)
}

func parseAdminPagination(c *gin.Context, defaultLimit, maximumLimit int) (int, int, bool) {
	limit := defaultLimit
	offset := 0
	var err error
	if raw := c.Query("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maximumLimit {
			writeProblem(c, http.StatusBadRequest, "pagination_invalid", "分页参数无效", "请使用有效的 limit。")
			return 0, 0, false
		}
	}
	if raw := c.Query("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			writeProblem(c, http.StatusBadRequest, "pagination_invalid", "分页参数无效", "请使用有效的 offset。")
			return 0, 0, false
		}
	}
	return limit, offset, true
}

func parseAdminUUID(c *gin.Context, parameter, code, title string) (uuid.UUID, bool) {
	parsed, err := uuid.Parse(c.Param(parameter))
	if err != nil {
		writeProblem(c, http.StatusBadRequest, code, title, "请刷新页面后重试。")
		return uuid.Nil, false
	}
	return parsed, true
}

func adminUserServiceAvailable(c *gin.Context, service AdminUserService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "admin_users_unavailable", "账户管理服务暂不可用", "请稍后重试。")
	return false
}

func adminMembershipServiceAvailable(c *gin.Context, service AdminMembershipService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "membership_unavailable", "会员管理服务暂不可用", "请稍后重试。")
	return false
}

func adminReleaseServiceAvailable(c *gin.Context, service AdminReleaseService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "release_admin_unavailable", "软件版本管理服务暂不可用", "请稍后重试。")
	return false
}

func releaseAccessKeyServiceAvailable(c *gin.Context, service AdminReleaseAccessKeyService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "release_access_key_unavailable", "Release 推送密钥服务尚未配置", "请检查服务端数据库配置。")
	return false
}

func releaseAssetUploadServiceAvailable(c *gin.Context, service ReleaseAssetUploadService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "release_upload_unavailable", "安装文件上传暂不可用", "请检查对象存储配置后重试。")
	return false
}

func releaseAssetSourceServiceAvailable(c *gin.Context, service ReleaseAssetSourceService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "release_source_unavailable", "安装文件导入暂不可用", "请检查 GitHub、镜像站与可选对象存储配置后重试。")
	return false
}

func writeReleaseAssetImportResult(c *gin.Context, result releaseassets.StoredAsset, err error) {
	switch {
	case errors.Is(err, releaseassets.ErrRemoteURLInvalid), errors.Is(err, releaseassets.ErrRemoteAddressForbidden):
		writeProblem(c, http.StatusBadRequest, "release_import_url_invalid", "外链地址不可用", "仅支持可公开访问的 HTTP(S) 下载地址，不能访问内网或本机地址。")
	case errors.Is(err, releaseassets.ErrRemoteTargetInvalid):
		writeProblem(c, http.StatusUnprocessableEntity, "release_import_target_invalid", "无法识别安装包平台", "请确认文件名包含平台和架构，或在导入前选择对应平台。")
	case errors.Is(err, releaseassets.ErrRemoteAssetTooLarge), errors.Is(err, objectstore.ErrReleaseUploadTooLarge):
		writeProblem(c, http.StatusRequestEntityTooLarge, "release_import_too_large", "安装文件过大", "单个安装文件不能超过 5 GB。")
	case errors.Is(err, releaseassets.ErrRemoteAssetEmpty), errors.Is(err, objectstore.ErrReleaseUploadSizeMismatch), errors.Is(err, objectstore.ErrReleaseUploadChecksumMismatch):
		writeProblem(c, http.StatusUnprocessableEntity, "release_import_integrity_failed", "安装文件检测失败", "远端文件为空、大小变化或完整性校验失败。")
	case errors.Is(err, releaseassets.ErrRemoteDownloadFailed):
		writeProblem(c, http.StatusBadGateway, "release_import_download_failed", "无法下载外链安装包", "请确认外链无需登录且服务器可以访问。")
	case errors.Is(err, releaseassets.ErrStorageUnavailable):
		writeProblem(c, http.StatusServiceUnavailable, "release_upload_unavailable", "对象存储尚未配置", "请先配置并检测 S3。")
	case err != nil:
		writeProblem(c, http.StatusBadGateway, "release_import_failed", "安装文件转存失败", "请检查外链与 S3 连通性后重试。")
	default:
		c.JSON(http.StatusCreated, result)
	}
}

func adminPricingServiceAvailable(c *gin.Context, service AdminPricingService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "pricing_admin_unavailable", "价格管理服务暂不可用", "请稍后重试。")
	return false
}

func writePricingPlanError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, catalog.ErrPricingPlanNotFound):
		writeProblem(c, http.StatusNotFound, "pricing_plan_not_found", "价格套餐不存在", "请刷新套餐列表后重试。")
	case errors.Is(err, catalog.ErrPricingPlanCodeConflict):
		writeProblem(c, http.StatusConflict, "pricing_plan_code_conflict", "套餐代码已存在", "请使用唯一的套餐代码。")
	case errors.Is(err, catalog.ErrPricingPlanVersionConflict):
		writeProblem(c, http.StatusConflict, "pricing_plan_version_conflict", "价格套餐已被其他管理员更新", "请刷新列表并重新确认本次操作。")
	case errors.Is(err, catalog.ErrPricingPlanConfirmationRequired):
		writeProblem(c, http.StatusBadRequest, "pricing_plan_confirmation_required", "需要再次确认价格操作", "请确认价格变更或上下架操作后重试。")
	case errors.Is(err, catalog.ErrPricingPlanStateConflict):
		writeProblem(c, http.StatusConflict, "pricing_plan_state_conflict", "当前套餐状态不支持此操作", "只有已发布套餐可以下架。")
	case errors.Is(err, catalog.ErrPricingPlanInvalid):
		writeProblem(c, http.StatusBadRequest, "pricing_plan_invalid", "无法保存价格套餐", "请检查代码、金额、币种、计费周期、功能列表和展示顺序。")
	default:
		writeProblem(c, http.StatusServiceUnavailable, "pricing_admin_unavailable", "价格管理服务暂不可用", "请稍后重试。")
	}
}
