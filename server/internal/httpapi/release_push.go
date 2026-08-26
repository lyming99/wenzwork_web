package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wenzwork/wenzwork-web/server/internal/catalog"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
)

type ReleasePushService interface {
	PushRelease(context.Context, catalog.PushReleaseInput) (catalog.AdminRelease, bool, error)
}

type ReleasePushAssetStore interface {
	Upload(context.Context, string, objectstore.ReleaseAssetUploadInput, io.Reader) (objectstore.ReleaseAssetUpload, error)
	Verify(context.Context, objectstore.ReleaseAssetCacheInput) error
}

// ReleaseAccessKeyVerifier is backed by the catalog database in production.
// The legacy string fallback is retained by the router only for lightweight
// callers/tests that do not provide a database-backed verifier.
type ReleaseAccessKeyVerifier interface {
	VerifyReleaseAccessKey(context.Context, string) (bool, error)
}

type pushReleaseRequest struct {
	Project      string                    `json:"project"`
	Version      string                    `json:"version"`
	Channel      string                    `json:"channel"`
	SoftwareName string                    `json:"softwareName"`
	Title        string                    `json:"title"`
	Summary      string                    `json:"summary"`
	ReleaseNotes string                    `json:"releaseNotes"`
	Publish      *bool                     `json:"publish"`
	Assets       []saveReleaseAssetRequest `json:"assets"`
}

type pushedReleaseAssetResponse struct {
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

func registerReleasePushRoutes(group *gin.RouterGroup, releases ReleasePushService, assets ReleasePushAssetStore, accessKeys ReleaseAccessKeyVerifier, accessKey string) {
	group.POST("/release-push/assets", func(c *gin.Context) {
		if !authorizeReleasePush(c, accessKeys, accessKey) {
			return
		}
		if assets == nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_push_unavailable", "本地推送暂不可用", "请检查 RELEASE_PUSH_STORAGE_DIR 配置。")
			return
		}
		extendReleaseTransferDeadlines(c, true)
		fileSizeBytes, err := strconv.ParseInt(strings.TrimSpace(c.Query("fileSizeBytes")), 10, 64)
		if err != nil || fileSizeBytes < 1 || fileSizeBytes > objectstore.MaxReleaseAssetBytes {
			writeProblem(c, http.StatusBadRequest, "release_push_asset_invalid", "无法推送安装文件", "文件大小参数无效。")
			return
		}
		if c.Request.ContentLength != fileSizeBytes {
			writeProblem(c, http.StatusBadRequest, "release_push_size_mismatch", "安装文件大小不一致", "请重新构建后推送。")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, objectstore.MaxReleaseAssetBytes)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Hour)
		defer cancel()
		project := strings.ToLower(strings.TrimSpace(c.Query("project")))
		platform := strings.ToLower(strings.TrimSpace(c.Query("platform")))
		architecture := strings.ToLower(strings.TrimSpace(c.Query("architecture")))
		fileName := strings.TrimSpace(c.Query("fileName"))
		signatureStatus := strings.ToLower(strings.TrimSpace(c.DefaultQuery("signatureStatus", "unknown")))
		if signatureStatus != "unknown" && signatureStatus != "unsigned" && signatureStatus != "valid" {
			writeProblem(c, http.StatusBadRequest, "release_push_asset_invalid", "无法推送安装文件", "签名状态参数无效。")
			return
		}
		result, err := assets.Upload(ctx, project, objectstore.ReleaseAssetUploadInput{
			Version: c.Query("version"), Platform: platform, Architecture: architecture,
			FileName: fileName, FileSizeBytes: fileSizeBytes, SHA256: c.Query("sha256"),
			ContentType: c.GetHeader("Content-Type"),
		}, c.Request.Body)
		switch {
		case errors.Is(err, objectstore.ErrReleaseUploadInvalid):
			writeProblem(c, http.StatusBadRequest, "release_push_asset_invalid", "无法推送安装文件", "请检查项目、版本、系统、架构、文件名、大小和 SHA-256。")
		case errors.Is(err, objectstore.ErrReleaseUploadTooLarge):
			writeProblem(c, http.StatusRequestEntityTooLarge, "release_push_asset_too_large", "安装文件过大", "单个安装文件不能超过 5 GB。")
		case errors.Is(err, objectstore.ErrReleaseUploadSizeMismatch), errors.Is(err, objectstore.ErrReleaseUploadChecksumMismatch):
			writeProblem(c, http.StatusUnprocessableEntity, "release_push_integrity_failed", "安装文件完整性校验失败", "实际文件与提交的大小或 SHA-256 不一致。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "release_push_storage_failed", "安装文件保存失败", "请检查本地推送存储目录的空间和权限。")
		default:
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusCreated, pushedReleaseAssetResponse{
				Platform: platform, Architecture: architecture, FileName: fileName,
				FileSizeBytes: result.FileSizeBytes, SHA256: result.SHA256, SignatureStatus: signatureStatus,
				Source: "local", ObjectKey: result.ObjectKey, DownloadURL: result.DownloadURL,
			})
		}
	})

	group.POST("/release-push", func(c *gin.Context) {
		if !authorizeReleasePush(c, accessKeys, accessKey) {
			return
		}
		if releases == nil || assets == nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_push_unavailable", "本地推送暂不可用", "版本目录服务尚未配置。")
			return
		}
		var request pushReleaseRequest
		if !decodeJSON(c, &request) {
			return
		}
		publish := true
		if request.Publish != nil {
			publish = *request.Publish
		}
		releaseAssets := make([]catalog.SaveReleaseAssetInput, 0, len(request.Assets))
		for _, asset := range request.Assets {
			if strings.EqualFold(strings.TrimSpace(asset.Source), "local") {
				if err := assets.Verify(c.Request.Context(), objectstore.ReleaseAssetCacheInput{
					ObjectKey: asset.ObjectKey, FileName: asset.FileName,
					FileSizeBytes: asset.FileSizeBytes, SHA256: asset.SHA256,
				}); err != nil {
					writeProblem(c, http.StatusUnprocessableEntity, "release_push_asset_missing", "本地推送文件不可用", "文件未完成上传、已被清理，或与提交的大小和 SHA-256 不一致。")
					return
				}
			}
			releaseAssets = append(releaseAssets, catalog.SaveReleaseAssetInput{
				Platform: asset.Platform, Architecture: asset.Architecture, FileName: asset.FileName,
				FileSizeBytes: asset.FileSizeBytes, SHA256: asset.SHA256, SignatureStatus: asset.SignatureStatus,
				Source: asset.Source, ObjectKey: asset.ObjectKey, DownloadURL: asset.DownloadURL,
			})
		}
		result, created, err := releases.PushRelease(c.Request.Context(), catalog.PushReleaseInput{
			Project: request.Project, Version: request.Version, Channel: request.Channel,
			SoftwareName: request.SoftwareName, Title: request.Title, Summary: request.Summary,
			ReleaseNotes: request.ReleaseNotes, Publish: publish, Assets: releaseAssets,
		})
		switch {
		case errors.Is(err, catalog.ErrReleaseInvalid):
			writeProblem(c, http.StatusBadRequest, "release_push_invalid", "无法推送软件版本", "请检查版本、公告、发布通道和安装文件。")
		case errors.Is(err, catalog.ErrReleaseAssetMismatch):
			writeProblem(c, http.StatusUnprocessableEntity, "release_push_asset_mismatch", "安装文件与版本不一致", "部署包文件名中的版本、系统或架构与发布信息不一致。")
		case errors.Is(err, catalog.ErrReleaseWithdrawn):
			writeProblem(c, http.StatusConflict, "release_push_withdrawn", "无法更新已下架版本", "请使用新的版本号，或先在管理后台处理已下架记录。")
		case errors.Is(err, catalog.ErrReleaseVersionConflict):
			writeProblem(c, http.StatusConflict, "release_push_conflict", "版本正在被更新", "另一项推送同时修改了该版本，请重试。")
		case err != nil:
			writeProblem(c, http.StatusServiceUnavailable, "release_push_failed", "软件版本推送失败", "版本目录暂不可用，请稍后重试。")
		default:
			status := http.StatusOK
			if created {
				status = http.StatusCreated
			}
			c.Header("Cache-Control", "no-store")
			c.JSON(status, gin.H{"release": result})
		}
	})
}

func authorizeReleasePush(c *gin.Context, accessKeys ReleaseAccessKeyVerifier, configuredKey string) bool {
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	provided := ""
	if len(authorization) > len("Bearer ") && strings.EqualFold(authorization[:len("Bearer ")], "Bearer ") {
		provided = strings.TrimSpace(authorization[len("Bearer "):])
	}
	if accessKeys != nil {
		valid, err := accessKeys.VerifyReleaseAccessKey(c.Request.Context(), provided)
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "release_push_unavailable", "本地推送暂不可用", "Release Access Key 尚未配置或数据库暂时不可用。")
			return false
		}
		if !valid {
			c.Header("WWW-Authenticate", `Bearer realm="release-push"`)
			writeProblem(c, http.StatusUnauthorized, "release_access_key_invalid", "Release Access Key 无效", "请通过 Authorization Bearer 提交正确的推送密钥。")
			return false
		}
		return true
	}
	configuredKey = strings.TrimSpace(configuredKey)
	if configuredKey == "" {
		writeProblem(c, http.StatusServiceUnavailable, "release_push_unavailable", "本地推送暂不可用", "Release Access Key 尚未配置。")
		return false
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(configuredKey)) != 1 {
		c.Header("WWW-Authenticate", `Bearer realm="release-push"`)
		writeProblem(c, http.StatusUnauthorized, "release_access_key_invalid", "Release Access Key 无效", "请通过 Authorization Bearer 提交正确的推送密钥。")
		return false
	}
	return true
}
