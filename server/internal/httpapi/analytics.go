package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wenzwork/wenzwork-web/server/internal/analytics"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
)

type AnalyticsService interface {
	RecordPageView(context.Context, analytics.PageViewInput) error
	RecordLogin(context.Context, analytics.LoginEventInput) error
	RecordDownload(context.Context, analytics.DownloadEventInput) error
	RecordRegistration(context.Context, analytics.RegistrationEventInput) error
	Overview(context.Context, analytics.ReportRange) (analytics.Overview, error)
	ListLoginEvents(context.Context, analytics.LoginEventFilter) (analytics.LoginEventList, error)
}

type LoginEventRecorder interface {
	RecordLogin(context.Context, analytics.LoginEventInput) error
}

type AccountEventRecorder interface {
	LoginEventRecorder
	RecordRegistration(context.Context, analytics.RegistrationEventInput) error
}

type DownloadEventRecorder interface {
	RecordDownload(context.Context, analytics.DownloadEventInput) error
}

type pageViewRequest struct {
	Path     string `json:"path"`
	Referrer string `json:"referrer"`
}

func registerAnalyticsRoutes(group *gin.RouterGroup, service AnalyticsService, authService AuthService, config AuthHTTPConfig, log *slog.Logger) {
	group.POST("/analytics/page-view", func(c *gin.Context) {
		if !analyticsServiceAvailable(c, service) {
			return
		}
		var request pageViewRequest
		if !decodeJSON(c, &request) {
			return
		}
		err := service.RecordPageView(c.Request.Context(), analytics.PageViewInput{
			Path: request.Path, Referrer: request.Referrer,
			ClientIP: c.ClientIP(), UserAgent: c.GetHeader("User-Agent"),
		})
		if errors.Is(err, analytics.ErrPageViewInvalid) {
			writeProblem(c, http.StatusBadRequest, "page_view_invalid", "访问记录无效", "请检查页面路径。")
			return
		}
		if err != nil {
			log.Warn("page view recording failed", "request_id", requestIDFrom(c), "error", err)
			writeProblem(c, http.StatusServiceUnavailable, "analytics_unavailable", "访问统计暂不可用", "请稍后重试。")
			return
		}
		c.Status(http.StatusNoContent)
	})

	admin := group.Group("/admin/analytics")
	admin.Use(requireSession(authService, config), RequirePermission(auth.PermissionAdminAuditRead, !config.DisableAdminMFA))
	admin.GET("/overview", func(c *gin.Context) {
		if !analyticsServiceAvailable(c, service) {
			return
		}
		reportRange, ok := parseAnalyticsRange(c)
		if !ok {
			return
		}
		result, err := service.Overview(c.Request.Context(), reportRange)
		if errors.Is(err, analytics.ErrReportRangeInvalid) {
			writeProblem(c, http.StatusBadRequest, "analytics_range_invalid", "统计时间范围无效", "时间范围必须大于零且不能超过 366 天；按小时统计最多支持 31 天。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "analytics_unavailable", "访问统计暂不可用", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	})
	admin.GET("/login-events", func(c *gin.Context) {
		if !analyticsServiceAvailable(c, service) {
			return
		}
		reportRange, ok := parseAnalyticsRange(c)
		if !ok {
			return
		}
		limit, offset, ok := parseAdminPagination(c, 50, 100)
		if !ok {
			return
		}
		result, err := service.ListLoginEvents(c.Request.Context(), analytics.LoginEventFilter{
			ReportRange: reportRange, Query: c.Query("q"), Limit: limit, Offset: offset,
		})
		if errors.Is(err, analytics.ErrLoginFilterInvalid) || errors.Is(err, analytics.ErrReportRangeInvalid) {
			writeProblem(c, http.StatusBadRequest, "login_event_filter_invalid", "登录记录筛选无效", "请检查时间、搜索内容和分页参数。")
			return
		}
		if err != nil {
			writeProblem(c, http.StatusServiceUnavailable, "analytics_unavailable", "登录记录暂不可用", "请稍后重试。")
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, result)
	})
}

func parseAnalyticsRange(c *gin.Context) (analytics.ReportRange, bool) {
	granularity := strings.TrimSpace(c.DefaultQuery("granularity", analytics.GranularityDay))
	if granularity != analytics.GranularityHour && granularity != analytics.GranularityDay {
		writeProblem(c, http.StatusBadRequest, "analytics_range_invalid", "统计粒度无效", "granularity 仅支持 hour 或 day。")
		return analytics.ReportRange{}, false
	}
	rawFrom := strings.TrimSpace(c.Query("from"))
	rawTo := strings.TrimSpace(c.Query("to"))
	if rawFrom == "" && rawTo == "" {
		now := time.Now().In(time.FixedZone(analytics.ReportTimezone, 8*60*60))
		to := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
		return analytics.ReportRange{From: to.AddDate(0, 0, -7).UTC(), To: to.UTC(), Granularity: granularity}, true
	}
	if rawFrom == "" || rawTo == "" {
		writeProblem(c, http.StatusBadRequest, "analytics_range_invalid", "统计时间范围无效", "from 与 to 必须同时提供。")
		return analytics.ReportRange{}, false
	}
	from, fromErr := time.Parse(time.RFC3339, rawFrom)
	to, toErr := time.Parse(time.RFC3339, rawTo)
	if fromErr != nil || toErr != nil {
		writeProblem(c, http.StatusBadRequest, "analytics_range_invalid", "统计时间范围无效", "from 与 to 必须是 RFC 3339 时间。")
		return analytics.ReportRange{}, false
	}
	return analytics.ReportRange{From: from.UTC(), To: to.UTC(), Granularity: granularity}, true
}

func analyticsServiceAvailable(c *gin.Context, service AnalyticsService) bool {
	if service != nil {
		return true
	}
	writeProblem(c, http.StatusServiceUnavailable, "analytics_unavailable", "访问统计暂不可用", "请稍后重试。")
	return false
}
