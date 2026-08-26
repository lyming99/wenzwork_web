package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/analytics"
	"github.com/wenzwork/wenzwork-web/server/internal/catalog"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
	"github.com/wenzwork/wenzwork-web/server/internal/releaseassets"
)

const publicCacheControl = "public, max-age=60, stale-while-revalidate=300"
const releaseCatalogCacheControl = "no-cache"

type CatalogReader interface {
	ListPricingPlans(context.Context) ([]catalog.PricingPlan, error)
	LatestRelease(context.Context, catalog.ReleaseFilter) (catalog.Release, error)
	ListReleases(context.Context, catalog.ReleaseFilter) ([]catalog.Release, error)
	ReleaseAssetDownload(context.Context, uuid.UUID) (catalog.ReleaseAssetDownload, error)
	GetReleaseDeliverySettings(context.Context) (catalog.ReleaseDeliverySettings, error)
}

type ReleaseAssetDownloadService interface {
	Open(context.Context, releaseassets.DeliveryAsset) (objectstore.CachedReleaseAsset, error)
	GitHubRedirect(context.Context, releaseassets.DeliveryAsset) (string, error)
}

func registerCatalogRoutes(group *gin.RouterGroup, reader CatalogReader, downloads ReleaseAssetDownloadService, downloadEvents DownloadEventRecorder, log *slog.Logger) {
	group.GET("/pricing-plans", func(c *gin.Context) {
		if reader == nil {
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "目录服务暂不可用", "请稍后重试。")
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		plans, err := reader.ListPricingPlans(ctx)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "价格暂不可用", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", publicCacheControl)
		c.JSON(http.StatusOK, gin.H{"items": plans})
	})

	group.GET("/releases", func(c *gin.Context) {
		filter, ok := parseReleaseFilter(c)
		if !ok {
			return
		}
		if reader == nil {
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "版本目录暂不可用", "请稍后重试。")
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		releases, err := reader.ListReleases(ctx, filter)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "版本目录暂不可用", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", releaseCatalogCacheControl)
		c.JSON(http.StatusOK, gin.H{"items": releases})
	})

	group.GET("/releases/latest", func(c *gin.Context) {
		filter, ok := parseReleaseFilter(c)
		if !ok {
			return
		}
		if reader == nil {
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "版本目录暂不可用", "请稍后重试。")
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		release, err := reader.LatestRelease(ctx, filter)
		switch {
		case errors.Is(err, catalog.ErrReleaseNotFound):
			writeProblem(c, http.StatusNotFound, "release_not_found", "尚无可下载版本", "正式版本发布后会在这里提供。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "版本目录暂不可用", "请稍后重试。")
		default:
			c.Header("Cache-Control", releaseCatalogCacheControl)
			c.JSON(http.StatusOK, release)
		}
	})

	group.Match([]string{http.MethodGet, http.MethodHead}, "/release-assets/:assetId/download", func(c *gin.Context) {
		assetID, err := uuid.Parse(c.Param("assetId"))
		if err != nil {
			writeProblem(c, http.StatusBadRequest, "invalid_asset_id", "文件标识无效", "请检查下载地址。")
			return
		}
		if reader == nil {
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "下载服务暂不可用", "请稍后重试。")
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		defer cancel()
		asset, err := reader.ReleaseAssetDownload(ctx, assetID)
		switch {
		case errors.Is(err, catalog.ErrAssetNotFound):
			writeProblem(c, http.StatusNotFound, "asset_not_found", "安装包不存在", "文件可能尚未发布或已经下架。")
			return
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "catalog_unavailable", "下载服务暂不可用", "请稍后重试。")
			return
		}
		source := releaseAssetDownloadSource(asset)
		if source == "custom" {
			target, err := legacyReleaseAssetURL(asset.DownloadURL)
			if err != nil {
				writeProblem(c, http.StatusServiceUnavailable, "asset_url_invalid", "安装包暂不可用", "旧版安装包地址无效，请在管理端重新上传到 S3。")
				return
			}
			recordReleaseDownload(c, downloadEvents, log, assetID)
			c.Header("Cache-Control", "no-store")
			if strings.Contains(c.GetHeader("Accept"), "application/json") {
				c.JSON(http.StatusOK, gin.H{"url": target})
				return
			}
			c.Redirect(http.StatusFound, target)
			return
		}

		settings, err := reader.GetReleaseDeliverySettings(ctx)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_delivery_unavailable", "下载服务暂不可用", "请稍后重试。")
			return
		}
		proxyOnlySource := source == "mirror" || source == "local"
		if !proxyOnlySource && settings.DownloadMode == catalog.ReleaseDownloadS3Redirect {
			if source != "s3" {
				writeProblem(c, http.StatusServiceUnavailable, "release_source_incompatible", "安装包暂不可用", "当前版本文件来自 GitHub，不能使用 S3 链接模式；请切换为直链或 GitHub 链接。")
				return
			}
			target, err := releaseAssetS3URL(settings.S3URLPrefix, asset.ObjectKey)
			if err != nil {
				writeProblem(c, http.StatusServiceUnavailable, "asset_url_invalid", "安装包暂不可用", "S3 下载前缀尚未正确配置。")
				return
			}
			recordReleaseDownload(c, downloadEvents, log, assetID)
			c.Header("Cache-Control", "no-store")
			if strings.Contains(c.GetHeader("Accept"), "application/json") {
				c.JSON(http.StatusOK, gin.H{"url": target})
				return
			}
			c.Redirect(http.StatusFound, target)
			return
		}
		if !proxyOnlySource && settings.DownloadMode == catalog.ReleaseDownloadGitHubRedirect {
			if source != "github" || downloads == nil {
				writeProblem(c, http.StatusServiceUnavailable, "release_source_incompatible", "安装包暂不可用", "当前版本文件不来自 GitHub，不能使用 GitHub 链接模式；请切换为直链或 S3 链接。")
				return
			}
			redirectCtx, cancelRedirect := context.WithTimeout(c.Request.Context(), 30*time.Second)
			defer cancelRedirect()
			target, err := downloads.GitHubRedirect(redirectCtx, releaseDeliveryAsset(source, asset))
			if err != nil {
				writeGitHubAssetDeliveryProblem(c, err)
				return
			}
			recordReleaseDownload(c, downloadEvents, log, assetID)
			c.Header("Cache-Control", "no-store")
			if strings.Contains(c.GetHeader("Accept"), "application/json") {
				c.JSON(http.StatusOK, gin.H{"url": target})
				return
			}
			c.Redirect(http.StatusFound, target)
			return
		}
		if (!proxyOnlySource && settings.DownloadMode != catalog.ReleaseDownloadProxyCached) || downloads == nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_delivery_unavailable", "下载服务暂不可用", "请检查主机缓存、S3、GitHub 与镜像站配置。")
			return
		}
		if strings.Contains(c.GetHeader("Accept"), "application/json") {
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, gin.H{"url": c.Request.URL.Path})
			return
		}
		extendReleaseTransferDeadlines(c, false)
		downloadCtx, cancelDownload := context.WithTimeout(c.Request.Context(), 2*time.Hour)
		defer cancelDownload()
		cached, err := downloads.Open(downloadCtx, releaseDeliveryAsset(source, asset))
		if err != nil {
			if source == "github" {
				writeGitHubAssetDeliveryProblem(c, err)
			} else if source == "mirror" {
				writeMirrorAssetDeliveryProblem(c, err)
			} else if source == "local" {
				writeProblem(c, http.StatusServiceUnavailable, "local_release_asset_unavailable", "本地推送安装包不可用", "请确认 RELEASE_PUSH_STORAGE_DIR 未被清理，并重新推送该版本。")
			} else {
				writeProblem(c, http.StatusBadGateway, "asset_cache_failed", "安装包暂时无法下载", "主机缓存无法从 S3 取得或校验文件。")
			}
			return
		}
		defer cached.File.Close()
		recordReleaseDownload(c, downloadEvents, log, assetID)
		contentType := mime.TypeByExtension(path.Ext(asset.FileName))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		extendReleaseTransferDeadlines(c, false)
		c.Header("Content-Type", contentType)
		c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": asset.FileName}))
		c.Header("ETag", `"`+asset.SHA256+`"`)
		c.Header("Cache-Control", "private, no-cache")
		http.ServeContent(c.Writer, c.Request, asset.FileName, cached.ModTime, cached.File)
	})
}

func recordReleaseDownload(c *gin.Context, recorder DownloadEventRecorder, log *slog.Logger, assetID uuid.UUID) {
	if c.Request.Method != http.MethodGet || recorder == nil {
		return
	}
	if err := recorder.RecordDownload(c.Request.Context(), analytics.DownloadEventInput{
		AssetID: assetID, ClientIP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent"),
	}); err != nil && log != nil {
		log.Warn("download event recording failed", "request_id", requestIDFrom(c), "asset_id", assetID, "error", err)
	}
}

func releaseAssetDownloadSource(asset catalog.ReleaseAssetDownload) string {
	source := strings.TrimSpace(asset.Source)
	if source != "" {
		return source
	}
	switch {
	case strings.HasPrefix(asset.ObjectKey, "releases/"):
		return "s3"
	case strings.HasPrefix(asset.ObjectKey, "github/"):
		return "github"
	case strings.HasPrefix(asset.ObjectKey, "mirror/"):
		return "mirror"
	case strings.HasPrefix(asset.ObjectKey, "local/"):
		return "local"
	default:
		return "custom"
	}
}

func releaseDeliveryAsset(source string, asset catalog.ReleaseAssetDownload) releaseassets.DeliveryAsset {
	return releaseassets.DeliveryAsset{
		Source: source, ObjectKey: asset.ObjectKey, DownloadURL: asset.DownloadURL, FileName: asset.FileName,
		FileSizeBytes: asset.FileSizeBytes, SHA256: asset.SHA256,
	}
}

func writeGitHubAssetDeliveryProblem(c *gin.Context, err error) {
	switch {
	case errors.Is(err, releaseassets.ErrGitHubAuthentication):
		writeProblem(c, http.StatusBadGateway, "github_authentication_failed", "GitHub 安装包暂时无法下载", "已保存的 GitHub Token 无效或没有 Contents 只读权限。")
	case errors.Is(err, releaseassets.ErrGitHubRateLimited):
		writeProblem(c, http.StatusBadGateway, "github_rate_limited", "GitHub 安装包暂时无法下载", "GitHub API 请求已被限流，请稍后重试。")
	case errors.Is(err, releaseassets.ErrGitHubAssetNotFound):
		writeProblem(c, http.StatusBadGateway, "github_asset_not_found", "GitHub 安装包暂时无法下载", "请确认 Release 附件仍然存在，并且当前 Token 可以访问附件所属仓库。")
	case errors.Is(err, releaseassets.ErrGitHubAssetInvalid):
		writeProblem(c, http.StatusServiceUnavailable, "github_asset_invalid", "GitHub 安装包配置无效", "请在管理端重新读取并保存 GitHub Release。")
	default:
		writeProblem(c, http.StatusBadGateway, "asset_cache_failed", "GitHub 安装包暂时无法下载", "服务端无法通过 GitHub Asset API 拉取或校验文件，请检查服务器网络与 Token。")
	}
}

func writeMirrorAssetDeliveryProblem(c *gin.Context, err error) {
	switch {
	case errors.Is(err, releaseassets.ErrRemoteURLInvalid), errors.Is(err, releaseassets.ErrRemoteAddressForbidden):
		writeProblem(c, http.StatusServiceUnavailable, "mirror_asset_invalid", "镜像安装包配置无效", "请在管理端重新读取并保存镜像版本。")
	case errors.Is(err, releaseassets.ErrMirrorAssetMismatch), errors.Is(err, objectstore.ErrReleaseCacheCorrupt):
		writeProblem(c, http.StatusBadGateway, "mirror_asset_integrity_failed", "镜像安装包校验失败", "镜像文件的实际大小或 SHA-256 与版本目录不一致。")
	default:
		writeProblem(c, http.StatusBadGateway, "asset_cache_failed", "镜像安装包暂时无法下载", "服务端无法从镜像链接拉取并校验文件，请检查镜像站网络与文件状态。")
	}
}

func legacyReleaseAssetURL(raw string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", errors.New("invalid legacy release download URL")
	}
	return parsed.String(), nil
}

func releaseAssetS3URL(prefix, objectKey string) (string, error) {
	prefix = strings.TrimRight(strings.TrimSpace(prefix), "/")
	parsed, err := url.Parse(prefix)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!strings.HasPrefix(objectKey, "releases/") || path.Clean(objectKey) != objectKey {
		return "", errors.New("invalid S3 release download URL")
	}
	segments := strings.Split(objectKey, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return prefix + "/" + strings.Join(segments, "/"), nil
}

func parseReleaseFilter(c *gin.Context) (catalog.ReleaseFilter, bool) {
	filter := catalog.ReleaseFilter{
		Project:      strings.ToLower(strings.TrimSpace(c.DefaultQuery("project", catalog.ReleaseProjectDesktop))),
		Channel:      c.DefaultQuery("channel", "stable"),
		Platform:     c.Query("platform"),
		Architecture: c.Query("architecture"),
		Limit:        20,
	}
	if !catalog.ValidReleaseProject(filter.Project) {
		writeProblem(c, http.StatusBadRequest, "invalid_project", "项目类型无效", "project 仅支持 web、desktop 或 mobile。")
		return catalog.ReleaseFilter{}, false
	}
	if filter.Channel != "stable" && filter.Channel != "beta" {
		writeProblem(c, http.StatusBadRequest, "invalid_channel", "版本通道无效", "channel 仅支持 stable 或 beta。")
		return catalog.ReleaseFilter{}, false
	}
	if filter.Platform != "" && filter.Platform != "web" && filter.Platform != "windows" && filter.Platform != "macos" &&
		filter.Platform != "linux" && filter.Platform != "android" && filter.Platform != "ios" {
		writeProblem(c, http.StatusBadRequest, "invalid_platform", "平台无效", "platform 仅支持 web、windows、macos、linux、android 或 ios。")
		return catalog.ReleaseFilter{}, false
	}
	if filter.Architecture != "" && filter.Architecture != "x64" && filter.Architecture != "arm64" && filter.Architecture != "universal" {
		writeProblem(c, http.StatusBadRequest, "invalid_architecture", "处理器架构无效", "architecture 仅支持 x64、arm64 或 universal。")
		return catalog.ReleaseFilter{}, false
	}
	if rawLimit := c.Query("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > 50 {
			writeProblem(c, http.StatusBadRequest, "invalid_limit", "分页数量无效", "limit 必须是 1 到 50 的整数。")
			return catalog.ReleaseFilter{}, false
		}
		filter.Limit = limit
	}
	return filter, true
}
