package analytics

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	maxReportDuration            = 366 * 24 * time.Hour
	maxHourlyReportDuration      = 31 * 24 * time.Hour
	locationCacheSuccessTTL      = 30 * 24 * time.Hour
	locationCacheNotFoundTTL     = 24 * time.Hour
	locationCacheFailureTTL      = 5 * time.Minute
	maxLocationLookupsPerQuery   = 50
	maxConcurrentLocationLookups = 8
	locationCacheStatusResolved  = "resolved"
	locationCacheStatusNotFound  = "not_found"
	locationCacheStatusFailed    = "failed"
)

var reportLocation = time.FixedZone(ReportTimezone, 8*60*60)

type Store struct {
	db          *gorm.DB
	resolver    LocationResolver
	now         func() time.Time
	lookupGroup singleflight.Group
	lookupSlots chan struct{}
}

func NewStore(db *gorm.DB, resolver LocationResolver) (*Store, error) {
	if db == nil {
		return nil, errors.New("analytics database is required")
	}
	if resolver == nil {
		resolver = NoopLocationResolver{}
	}
	return &Store{
		db: db, resolver: resolver, now: time.Now,
		lookupSlots: make(chan struct{}, maxConcurrentLocationLookups),
	}, nil
}

func (s *Store) RecordPageView(ctx context.Context, input PageViewInput) error {
	pagePath, err := normalizePagePath(input.Path)
	if err != nil {
		return err
	}
	ip, err := normalizeIP(input.ClientIP)
	if err != nil {
		return ErrPageViewInvalid
	}
	row := pageViewRow{
		Path: pagePath, ClientIP: ip.String(),
		ReferrerHost: referrerHost(input.Referrer), UserAgentSummary: summarizeUserAgent(input.UserAgent),
		OccurredAt: s.now().UTC(),
	}
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Exec(`
			INSERT INTO website_visitors (client_ip, first_seen_at, last_seen_at, page_views)
			VALUES (?, ?, ?, 1)
			ON CONFLICT (client_ip) DO UPDATE SET
				first_seen_at = LEAST(website_visitors.first_seen_at, EXCLUDED.first_seen_at),
				last_seen_at = GREATEST(website_visitors.last_seen_at, EXCLUDED.last_seen_at),
				page_views = website_visitors.page_views + 1`,
			row.ClientIP, row.OccurredAt, row.OccurredAt).Error
	}); err != nil {
		return fmt.Errorf("record website page view: %w", err)
	}
	return nil
}

func (s *Store) RecordLogin(ctx context.Context, input LoginEventInput) error {
	if input.LoginMethod == "" {
		input.LoginMethod = LoginMethodPassword
	}
	if input.UserID == uuid.Nil ||
		(input.LoginMethod == LoginMethodPassword && (input.WebSessionID == uuid.Nil || input.AppSessionID != uuid.Nil)) ||
		(input.LoginMethod == LoginMethodAppDevice && (input.AppSessionID == uuid.Nil || input.WebSessionID != uuid.Nil)) ||
		(input.LoginMethod != LoginMethodPassword && input.LoginMethod != LoginMethodAppDevice) {
		return errors.New("login event user, method, and session are invalid")
	}
	ip, err := normalizeIP(input.ClientIP)
	if err != nil {
		return errors.New("login event IP is invalid")
	}
	row := loginEventRow{
		UserID: input.UserID, LoginMethod: input.LoginMethod, ClientIP: ip.String(),
		UserAgentSummary: summarizeUserAgent(input.UserAgent), OccurredAt: s.now().UTC(),
	}
	if input.WebSessionID != uuid.Nil {
		row.SessionID = &input.WebSessionID
	}
	if input.AppSessionID != uuid.Nil {
		row.AppSessionID = &input.AppSessionID
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return fmt.Errorf("record account login event: %w", err)
	}
	return nil
}

func (s *Store) RecordDownload(ctx context.Context, input DownloadEventInput) error {
	if input.AssetID == uuid.Nil {
		return ErrDownloadInvalid
	}
	ip, err := normalizeIP(input.ClientIP)
	if err != nil {
		return ErrDownloadInvalid
	}
	row := downloadEventRow{
		AssetID: input.AssetID, ClientIP: ip.String(),
		UserAgentSummary: summarizeUserAgent(input.UserAgent), OccurredAt: s.now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return fmt.Errorf("record release download event: %w", err)
	}
	return nil
}

func (s *Store) RecordRegistration(ctx context.Context, input RegistrationEventInput) error {
	if input.UserID == uuid.Nil {
		return ErrRegistrationInvalid
	}
	ip, err := normalizeIP(input.ClientIP)
	if err != nil {
		return ErrRegistrationInvalid
	}
	row := registrationEventRow{
		UserID: input.UserID, ClientIP: ip.String(),
		UserAgentSummary: summarizeUserAgent(input.UserAgent), OccurredAt: s.now().UTC(),
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return fmt.Errorf("record account registration event: %w", err)
	}
	return nil
}

func (s *Store) Overview(ctx context.Context, reportRange ReportRange) (Overview, error) {
	if err := validateReportRange(reportRange); err != nil {
		return Overview{}, err
	}
	granularity := normalizedGranularity(reportRange.Granularity)
	result := Overview{
		Range: OverviewRange{
			From: reportRange.From.UTC(), To: reportRange.To.UTC(),
			Timezone: ReportTimezone, Granularity: granularity,
		},
		Daily: make([]DailyStat, 0), Regions: make([]RegionStat, 0),
		Timeline: make([]TimelineStat, 0), IPs: make([]IPStat, 0),
		RecentNewIPs: make([]RecentNewIP, 0), Sources: make([]SourceStat, 0),
		Paths: make([]PathStat, 0),
	}

	var pageSummary struct {
		PageViews int64 `gorm:"column:page_views"`
		UniqueIPs int64 `gorm:"column:unique_ips"`
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT COUNT(*)::bigint AS page_views, COUNT(DISTINCT client_ip)::bigint AS unique_ips
		FROM website_page_views
		WHERE occurred_at >= ? AND occurred_at < ?`, reportRange.From, reportRange.To).Scan(&pageSummary).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize website page views: %w", err)
	}
	var loginSummary struct {
		LoginEvents    int64 `gorm:"column:login_events"`
		UniqueLoginIPs int64 `gorm:"column:unique_login_ips"`
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT COUNT(*)::bigint AS login_events, COUNT(DISTINCT client_ip)::bigint AS unique_login_ips
		FROM account_login_events
		WHERE occurred_at >= ? AND occurred_at < ?`, reportRange.From, reportRange.To).Scan(&loginSummary).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize account login events: %w", err)
	}
	var downloadSummary struct {
		DownloadEvents int64 `gorm:"column:download_events"`
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT COUNT(*)::bigint AS download_events
		FROM release_download_events
		WHERE occurred_at >= ? AND occurred_at < ?`, reportRange.From, reportRange.To).Scan(&downloadSummary).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize release download events: %w", err)
	}
	result.Summary = Summary{
		PageViews: pageSummary.PageViews, UniqueIPs: pageSummary.UniqueIPs,
		DownloadEvents: downloadSummary.DownloadEvents,
		LoginEvents:    loginSummary.LoginEvents, UniqueLoginIPs: loginSummary.UniqueLoginIPs,
	}
	conversionFrom, conversionTo := conversionDayBounds(reportRange)
	var conversionSummary struct {
		DownloadedVisitorIPs int64 `gorm:"column:downloaded_visitor_ips"`
		RegisteredVisitorIPs int64 `gorm:"column:registered_visitor_ips"`
	}
	if err := s.db.WithContext(ctx).Raw(`
		WITH visitor_days AS (
			SELECT DISTINCT client_ip, (occurred_at AT TIME ZONE 'Asia/Shanghai')::date AS visit_date
			FROM website_page_views
			WHERE occurred_at >= ? AND occurred_at < ?
		), download_days AS (
			SELECT DISTINCT client_ip, (occurred_at AT TIME ZONE 'Asia/Shanghai')::date AS event_date
			FROM release_download_events
			WHERE occurred_at >= ? AND occurred_at < ?
		), registration_days AS (
			SELECT DISTINCT client_ip, (occurred_at AT TIME ZONE 'Asia/Shanghai')::date AS event_date
			FROM account_registration_events
			WHERE occurred_at >= ? AND occurred_at < ?
		), visitor_outcomes AS (
			SELECT visitors.client_ip,
			       BOOL_OR(downloads.client_ip IS NOT NULL) AS downloaded,
			       BOOL_OR(registrations.client_ip IS NOT NULL) AS registered
			FROM visitor_days AS visitors
			LEFT JOIN download_days AS downloads
			  ON downloads.client_ip = visitors.client_ip AND downloads.event_date = visitors.visit_date
			LEFT JOIN registration_days AS registrations
			  ON registrations.client_ip = visitors.client_ip AND registrations.event_date = visitors.visit_date
			GROUP BY visitors.client_ip
		)
		SELECT COUNT(*) FILTER (WHERE downloaded)::bigint AS downloaded_visitor_ips,
		       COUNT(*) FILTER (WHERE registered)::bigint AS registered_visitor_ips
		FROM visitor_outcomes`,
		reportRange.From, reportRange.To,
		conversionFrom, conversionTo, conversionFrom, conversionTo,
	).Scan(&conversionSummary).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize visitor conversions: %w", err)
	}
	result.Summary.DownloadedVisitorIPs = conversionSummary.DownloadedVisitorIPs
	result.Summary.RegisteredVisitorIPs = conversionSummary.RegisteredVisitorIPs
	result.Summary.VisitorDownloadRate = conversionRate(conversionSummary.DownloadedVisitorIPs, pageSummary.UniqueIPs)
	result.Summary.VisitorRegistrationRate = conversionRate(conversionSummary.RegisteredVisitorIPs, pageSummary.UniqueIPs)

	daily, err := s.dailyStats(ctx, reportRange)
	if err != nil {
		return Overview{}, err
	}
	result.Daily = daily
	timeline, err := s.timelineStats(ctx, reportRange, granularity)
	if err != nil {
		return Overview{}, err
	}
	result.Timeline = timeline
	pageViewIPs, err := s.pageViewIPs(ctx, reportRange)
	if err != nil {
		return Overview{}, err
	}
	recentNewIPAddresses, err := s.recentNewIPAddresses(ctx, reportRange)
	if err != nil {
		return Overview{}, err
	}
	if err := s.ensureLocationCache(ctx, append(recentNewIPAddresses, pageViewIPs...)); err != nil {
		return Overview{}, err
	}
	cacheNow := s.now().UTC()
	if err := s.db.WithContext(ctx).Raw(`
		WITH scoped_views AS (
			SELECT views.client_ip,
			       COALESCE(NULLIF(cache.country_code, ''), views.country_code) AS country_code,
			       COALESCE(NULLIF(cache.country_name, ''), views.country_name) AS country_name,
			       COALESCE(NULLIF(cache.region_name, ''), views.region_name) AS region_name,
			       COALESCE(NULLIF(cache.city_name, ''), views.city_name) AS city_name
			FROM website_page_views AS views
			LEFT JOIN ip_geolocation_cache AS cache
			  ON cache.client_ip = views.client_ip
			 AND cache.lookup_status = 'resolved'
			 AND cache.expires_at > ?
			WHERE views.occurred_at >= ? AND views.occurred_at < ?
		)
		SELECT country_code, COALESCE(NULLIF(country_name, ''), '未知') AS country_name,
		       region_name, city_name, COUNT(*)::bigint AS page_views,
		       COUNT(DISTINCT client_ip)::bigint AS unique_ips
		FROM scoped_views
		GROUP BY country_code, country_name, region_name, city_name
		ORDER BY page_views DESC, unique_ips DESC, country_name ASC, region_name ASC, city_name ASC
		LIMIT 50`, cacheNow, reportRange.From, reportRange.To).Scan(&result.Regions).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize website regions: %w", err)
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT host(views.client_ip) AS ip,
		       COALESCE(NULLIF(cache.country_code, ''), MAX(NULLIF(views.country_code, '')), '') AS country_code,
		       COALESCE(NULLIF(cache.country_name, ''), MAX(NULLIF(views.country_name, '')), '未知') AS country_name,
		       COALESCE(NULLIF(cache.region_name, ''), MAX(NULLIF(views.region_name, '')), '') AS region_name,
		       COALESCE(NULLIF(cache.city_name, ''), MAX(NULLIF(views.city_name, '')), '') AS city_name,
		       COUNT(*)::bigint AS page_views, MAX(views.occurred_at) AS last_seen_at
		FROM website_page_views AS views
		LEFT JOIN ip_geolocation_cache AS cache
		  ON cache.client_ip = views.client_ip
		 AND cache.lookup_status = 'resolved'
		 AND cache.expires_at > ?
		WHERE views.occurred_at >= ? AND views.occurred_at < ?
		GROUP BY views.client_ip, cache.country_code, cache.country_name, cache.region_name, cache.city_name
		ORDER BY page_views DESC, last_seen_at DESC
		LIMIT 50`, cacheNow, reportRange.From, reportRange.To).Scan(&result.IPs).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize website IPs: %w", err)
	}
	recentNewIPs, err := s.recentNewIPs(ctx, reportRange, cacheNow)
	if err != nil {
		return Overview{}, err
	}
	result.RecentNewIPs = recentNewIPs
	if err := s.db.WithContext(ctx).Raw(`
		SELECT referrer_host, COUNT(*)::bigint AS page_views,
		       COUNT(DISTINCT client_ip)::bigint AS unique_ips
		FROM website_page_views
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY referrer_host
		ORDER BY page_views DESC, unique_ips DESC, referrer_host ASC
		LIMIT 50`, reportRange.From, reportRange.To).Scan(&result.Sources).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize website sources: %w", err)
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT path, COUNT(*)::bigint AS page_views,
		       COUNT(DISTINCT client_ip)::bigint AS unique_ips
		FROM website_page_views
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY path
		ORDER BY page_views DESC, unique_ips DESC, path ASC
		LIMIT 50`, reportRange.From, reportRange.To).Scan(&result.Paths).Error; err != nil {
		return Overview{}, fmt.Errorf("summarize website paths: %w", err)
	}
	return result, nil
}

func (s *Store) ListLoginEvents(ctx context.Context, filter LoginEventFilter) (LoginEventList, error) {
	if err := validateReportRange(filter.ReportRange); err != nil || filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 || filter.Offset > 100_000 {
		return LoginEventList{}, ErrLoginFilterInvalid
	}
	query := strings.TrimSpace(filter.Query)
	if utf8.RuneCountInString(query) > 100 {
		return LoginEventList{}, ErrLoginFilterInvalid
	}
	base := s.db.WithContext(ctx).Table("account_login_events AS events").
		Joins("JOIN users ON users.id = events.user_id").
		Where("events.occurred_at >= ? AND events.occurred_at < ?", filter.From, filter.To)
	if query != "" {
		pattern := "%" + escapeLike(strings.ToLower(query)) + "%"
		base = base.Where(`LOWER(users.email) LIKE ? ESCAPE '\' OR LOWER(users.display_name) LIKE ? ESCAPE '\' OR host(events.client_ip) LIKE ? ESCAPE '\'`, pattern, pattern, pattern)
	}
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return LoginEventList{}, fmt.Errorf("count account login events: %w", err)
	}
	items := make([]LoginEvent, 0)
	if err := base.Select(`events.id, events.user_id, users.email, users.display_name,
		host(events.client_ip) AS ip, events.country_code, events.country_name,
		events.region_name, events.city_name, events.user_agent_summary, events.login_method,
		events.occurred_at AS logged_in_at`).
		Order("events.occurred_at DESC, events.id DESC").Limit(filter.Limit).Offset(filter.Offset).
		Scan(&items).Error; err != nil {
		return LoginEventList{}, fmt.Errorf("list account login events: %w", err)
	}
	ips := make([]string, 0, len(items))
	for _, item := range items {
		ips = append(ips, item.IP)
	}
	if err := s.ensureLocationCache(ctx, ips); err != nil {
		return LoginEventList{}, err
	}
	cachedLocations, err := s.cachedLocations(ctx, ips, s.now().UTC())
	if err != nil {
		return LoginEventList{}, err
	}
	for index := range items {
		if location, ok := cachedLocations[items[index].IP]; ok {
			items[index].Location = location
		}
		if strings.TrimSpace(items[index].CountryName) == "" {
			items[index].CountryName = "未知"
		}
	}
	return LoginEventList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Store) pageViewIPs(ctx context.Context, reportRange ReportRange) ([]string, error) {
	var rows []struct {
		IP string `gorm:"column:ip"`
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT host(client_ip) AS ip
		FROM website_page_views
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY client_ip
		ORDER BY COUNT(*) DESC, MAX(occurred_at) DESC
		LIMIT 50`, reportRange.From, reportRange.To).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list website IPs for geolocation: %w", err)
	}
	ips := make([]string, 0, len(rows))
	for _, row := range rows {
		ips = append(ips, row.IP)
	}
	return ips, nil
}

func (s *Store) ensureLocationCache(ctx context.Context, rawIPs []string) error {
	ips := uniqueNormalizedIPs(rawIPs)
	if len(ips) == 0 {
		return nil
	}
	now := s.now().UTC()
	rows, err := s.freshLocationCacheRows(ctx, ips, now)
	if err != nil {
		return err
	}
	fresh := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		fresh[row.ClientIP] = struct{}{}
	}

	missing := make([]string, 0, len(ips)-len(fresh))
	for _, ip := range ips {
		if _, ok := fresh[ip]; !ok {
			missing = append(missing, ip)
			if len(missing) == maxLocationLookupsPerQuery {
				break
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}

	group, groupCtx := errgroup.WithContext(ctx)
	for _, rawIP := range missing {
		rawIP := rawIP
		group.Go(func() error {
			_, err, _ := s.lookupGroup.Do(rawIP, func() (any, error) {
				select {
				case s.lookupSlots <- struct{}{}:
					defer func() { <-s.lookupSlots }()
				case <-groupCtx.Done():
					return nil, groupCtx.Err()
				}
				fresh, err := s.locationCacheFresh(groupCtx, rawIP, s.now().UTC())
				if err != nil || fresh {
					return nil, err
				}
				return nil, s.resolveAndCacheLocation(groupCtx, rawIP)
			})
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return fmt.Errorf("refresh IP geolocation cache: %w", err)
	}
	return nil
}

func (s *Store) locationCacheFresh(ctx context.Context, ip string, at time.Time) (bool, error) {
	var count int64
	if err := s.db.WithContext(ctx).Table("ip_geolocation_cache").
		Where("client_ip = ? AND expires_at > ?", ip, at).Count(&count).Error; err != nil {
		return false, fmt.Errorf("check IP geolocation cache: %w", err)
	}
	return count > 0, nil
}

func (s *Store) resolveAndCacheLocation(ctx context.Context, rawIP string) error {
	ip, err := normalizeIP(rawIP)
	if err != nil {
		return nil
	}
	resolvedAt := s.now().UTC()
	location, lookupErr := s.resolveLocation(ctx, ip)
	status := locationCacheStatusResolved
	ttl := locationCacheSuccessTTL
	if lookupErr != nil {
		location = Location{}
		status = locationCacheStatusFailed
		ttl = locationCacheFailureTTL
	} else if !hasLocation(location) {
		status = locationCacheStatusNotFound
		ttl = locationCacheNotFoundTTL
	}
	row := locationCacheRow{
		ClientIP: rawIP, CountryCode: location.CountryCode, CountryName: location.CountryName,
		RegionName: location.RegionName, CityName: location.CityName, Source: location.Source,
		LookupStatus: status, ResolvedAt: resolvedAt, ExpiresAt: resolvedAt.Add(ttl), UpdatedAt: resolvedAt,
	}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "client_ip"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"country_code", "country_name", "region_name", "city_name", "source",
			"lookup_status", "resolved_at", "expires_at", "updated_at",
		}),
	}).Create(&row).Error; err != nil {
		return fmt.Errorf("store IP geolocation cache: %w", err)
	}
	return nil
}

func (s *Store) freshLocationCacheRows(ctx context.Context, ips []string, at time.Time) ([]locationCacheRow, error) {
	if len(ips) == 0 {
		return nil, nil
	}
	rows := make([]locationCacheRow, 0, len(ips))
	if err := s.db.WithContext(ctx).Table("ip_geolocation_cache").
		Select(`host(client_ip) AS client_ip, country_code, country_name, region_name,
			city_name, source, lookup_status, resolved_at, expires_at, updated_at`).
		Where("client_ip IN ? AND expires_at > ?", ips, at).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("read IP geolocation cache: %w", err)
	}
	return rows, nil
}

func (s *Store) cachedLocations(ctx context.Context, ips []string, at time.Time) (map[string]Location, error) {
	ips = uniqueNormalizedIPs(ips)
	rows, err := s.freshLocationCacheRows(ctx, ips, at)
	if err != nil {
		return nil, err
	}
	locations := make(map[string]Location, len(rows))
	for _, row := range rows {
		if row.LookupStatus != locationCacheStatusResolved {
			continue
		}
		locations[row.ClientIP] = Location{
			CountryCode: row.CountryCode, CountryName: row.CountryName,
			RegionName: row.RegionName, CityName: row.CityName, Source: row.Source,
		}
	}
	return locations, nil
}

func uniqueNormalizedIPs(rawIPs []string) []string {
	seen := make(map[string]struct{}, len(rawIPs))
	result := make([]string, 0, len(rawIPs))
	for _, rawIP := range rawIPs {
		ip, err := normalizeIP(rawIP)
		if err != nil {
			continue
		}
		value := ip.String()
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (s *Store) timelineStats(ctx context.Context, reportRange ReportRange, granularity string) ([]TimelineStat, error) {
	bucketExpression := fmt.Sprintf(
		"date_trunc('%s', occurred_at AT TIME ZONE 'Asia/Shanghai') AT TIME ZONE 'Asia/Shanghai'",
		granularity,
	)
	conversionFrom, conversionTo := conversionDayBounds(reportRange)
	type timelineRow struct {
		BucketStart          time.Time `gorm:"column:bucket_start"`
		PageViews            int64     `gorm:"column:page_views"`
		UniqueIPs            int64     `gorm:"column:unique_ips"`
		DownloadedVisitorIPs int64     `gorm:"column:downloaded_visitor_ips"`
		RegisteredVisitorIPs int64     `gorm:"column:registered_visitor_ips"`
	}
	rows := make([]timelineRow, 0)
	query := fmt.Sprintf(`
		WITH visits AS (
			SELECT %s AS bucket_start,
			       (occurred_at AT TIME ZONE 'Asia/Shanghai')::date AS visit_date,
			       client_ip, COUNT(*)::bigint AS page_views
			FROM website_page_views
			WHERE occurred_at >= ? AND occurred_at < ?
			GROUP BY bucket_start, visit_date, client_ip
		), download_days AS (
			SELECT DISTINCT client_ip, (occurred_at AT TIME ZONE 'Asia/Shanghai')::date AS event_date
			FROM release_download_events
			WHERE occurred_at >= ? AND occurred_at < ?
		), registration_days AS (
			SELECT DISTINCT client_ip, (occurred_at AT TIME ZONE 'Asia/Shanghai')::date AS event_date
			FROM account_registration_events
			WHERE occurred_at >= ? AND occurred_at < ?
		)
		SELECT visits.bucket_start, SUM(visits.page_views)::bigint AS page_views,
		       COUNT(*)::bigint AS unique_ips,
		       COUNT(*) FILTER (WHERE downloads.client_ip IS NOT NULL)::bigint AS downloaded_visitor_ips,
		       COUNT(*) FILTER (WHERE registrations.client_ip IS NOT NULL)::bigint AS registered_visitor_ips
		FROM visits
		LEFT JOIN download_days AS downloads
		  ON downloads.client_ip = visits.client_ip AND downloads.event_date = visits.visit_date
		LEFT JOIN registration_days AS registrations
		  ON registrations.client_ip = visits.client_ip AND registrations.event_date = visits.visit_date
		GROUP BY visits.bucket_start
		ORDER BY visits.bucket_start`, bucketExpression)
	if err := s.db.WithContext(ctx).Raw(query,
		reportRange.From, reportRange.To,
		conversionFrom, conversionTo, conversionFrom, conversionTo,
	).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("summarize analytics timeline: %w", err)
	}

	type downloadTimelineRow struct {
		BucketStart    time.Time `gorm:"column:bucket_start"`
		DownloadEvents int64     `gorm:"column:download_events"`
	}
	downloadRows := make([]downloadTimelineRow, 0)
	downloadQuery := fmt.Sprintf(`
		SELECT %s AS bucket_start, COUNT(*)::bigint AS download_events
		FROM release_download_events
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY bucket_start
		ORDER BY bucket_start`, bucketExpression)
	if err := s.db.WithContext(ctx).Raw(downloadQuery, reportRange.From, reportRange.To).Scan(&downloadRows).Error; err != nil {
		return nil, fmt.Errorf("summarize timeline downloads: %w", err)
	}

	type loginTimelineRow struct {
		BucketStart time.Time `gorm:"column:bucket_start"`
		LoginEvents int64     `gorm:"column:login_events"`
	}
	loginRows := make([]loginTimelineRow, 0)
	loginQuery := fmt.Sprintf(`
		SELECT %s AS bucket_start, COUNT(*)::bigint AS login_events
		FROM account_login_events
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY bucket_start
		ORDER BY bucket_start`, bucketExpression)
	if err := s.db.WithContext(ctx).Raw(loginQuery, reportRange.From, reportRange.To).Scan(&loginRows).Error; err != nil {
		return nil, fmt.Errorf("summarize timeline logins: %w", err)
	}

	byBucket := make(map[int64]TimelineStat, len(rows)+len(downloadRows)+len(loginRows))
	for _, row := range rows {
		bucket := row.BucketStart.UTC()
		byBucket[bucket.Unix()] = TimelineStat{
			BucketStart: bucket, PageViews: row.PageViews, UniqueIPs: row.UniqueIPs,
			DownloadedVisitorIPs: row.DownloadedVisitorIPs, RegisteredVisitorIPs: row.RegisteredVisitorIPs,
		}
	}
	for _, row := range downloadRows {
		bucket := row.BucketStart.UTC()
		item := byBucket[bucket.Unix()]
		item.BucketStart = bucket
		item.DownloadEvents = row.DownloadEvents
		byBucket[bucket.Unix()] = item
	}
	for _, row := range loginRows {
		bucket := row.BucketStart.UTC()
		item := byBucket[bucket.Unix()]
		item.BucketStart = bucket
		item.LoginEvents = row.LoginEvents
		byBucket[bucket.Unix()] = item
	}

	start := reportRange.From.In(reportLocation)
	if granularity == GranularityHour {
		start = start.Truncate(time.Hour)
	} else {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, reportLocation)
	}
	end := reportRange.To.In(reportLocation)
	items := make([]TimelineStat, 0)
	for bucket := start; bucket.Before(end); {
		bucketUTC := bucket.UTC()
		item := byBucket[bucketUTC.Unix()]
		item.BucketStart = bucketUTC
		item.VisitorDownloadRate = conversionRate(item.DownloadedVisitorIPs, item.UniqueIPs)
		item.VisitorRegistrationRate = conversionRate(item.RegisteredVisitorIPs, item.UniqueIPs)
		items = append(items, item)
		if granularity == GranularityHour {
			bucket = bucket.Add(time.Hour)
		} else {
			bucket = bucket.AddDate(0, 0, 1)
		}
	}
	return items, nil
}

func (s *Store) recentNewIPAddresses(ctx context.Context, reportRange ReportRange) ([]string, error) {
	var rows []struct {
		IP string `gorm:"column:ip"`
	}
	if err := s.db.WithContext(ctx).Raw(`
		SELECT host(client_ip) AS ip
		FROM website_visitors
		WHERE first_seen_at >= ? AND first_seen_at < ?
		ORDER BY first_seen_at DESC, client_ip
		LIMIT 20`, reportRange.From, reportRange.To).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list recent new visitor IPs: %w", err)
	}
	ips := make([]string, 0, len(rows))
	for _, row := range rows {
		ips = append(ips, row.IP)
	}
	return ips, nil
}

func (s *Store) recentNewIPs(ctx context.Context, reportRange ReportRange, cacheNow time.Time) ([]RecentNewIP, error) {
	items := make([]RecentNewIP, 0)
	if err := s.db.WithContext(ctx).Raw(`
		SELECT host(visitors.client_ip) AS ip,
		       COALESCE(cache.country_code, '') AS country_code,
		       COALESCE(NULLIF(cache.country_name, ''), '未知') AS country_name,
		       COALESCE(cache.region_name, '') AS region_name,
		       COALESCE(cache.city_name, '') AS city_name,
		       visitors.page_views, visitors.first_seen_at, visitors.last_seen_at,
		       EXISTS (
				SELECT 1 FROM release_download_events AS downloads
				WHERE downloads.client_ip = visitors.client_ip
				  AND (downloads.occurred_at AT TIME ZONE 'Asia/Shanghai')::date =
				      (visitors.first_seen_at AT TIME ZONE 'Asia/Shanghai')::date
		       ) AS downloaded_same_day,
		       EXISTS (
				SELECT 1 FROM account_registration_events AS registrations
				WHERE registrations.client_ip = visitors.client_ip
				  AND (registrations.occurred_at AT TIME ZONE 'Asia/Shanghai')::date =
				      (visitors.first_seen_at AT TIME ZONE 'Asia/Shanghai')::date
		       ) AS registered_same_day
		FROM website_visitors AS visitors
		LEFT JOIN ip_geolocation_cache AS cache
		  ON cache.client_ip = visitors.client_ip
		 AND cache.lookup_status = 'resolved'
		 AND cache.expires_at > ?
		WHERE visitors.first_seen_at >= ? AND visitors.first_seen_at < ?
		ORDER BY visitors.first_seen_at DESC, visitors.client_ip
		LIMIT 20`, cacheNow, reportRange.From, reportRange.To).Scan(&items).Error; err != nil {
		return nil, fmt.Errorf("list recent new visitor IPs: %w", err)
	}
	return items, nil
}

func normalizedGranularity(value string) string {
	if strings.TrimSpace(value) == GranularityHour {
		return GranularityHour
	}
	return GranularityDay
}

func conversionDayBounds(reportRange ReportRange) (time.Time, time.Time) {
	fromLocal := reportRange.From.In(reportLocation)
	from := time.Date(fromLocal.Year(), fromLocal.Month(), fromLocal.Day(), 0, 0, 0, 0, reportLocation)
	toLocal := reportRange.To.In(reportLocation)
	to := time.Date(toLocal.Year(), toLocal.Month(), toLocal.Day(), 0, 0, 0, 0, reportLocation)
	if toLocal.After(to) {
		to = to.AddDate(0, 0, 1)
	}
	return from.UTC(), to.UTC()
}

func conversionRate(converted, visitors int64) float64 {
	if visitors <= 0 || converted <= 0 {
		return 0
	}
	return float64(converted) / float64(visitors)
}

func (s *Store) dailyStats(ctx context.Context, reportRange ReportRange) ([]DailyStat, error) {
	type dailyRow struct {
		Date      string `gorm:"column:date"`
		Count     int64  `gorm:"column:event_count"`
		UniqueIPs int64  `gorm:"column:unique_ips"`
	}
	var pageRows []dailyRow
	if err := s.db.WithContext(ctx).Raw(`
		SELECT to_char(occurred_at AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD') AS date,
		       COUNT(*)::bigint AS event_count, COUNT(DISTINCT client_ip)::bigint AS unique_ips
		FROM website_page_views
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY date ORDER BY date`, reportRange.From, reportRange.To).Scan(&pageRows).Error; err != nil {
		return nil, fmt.Errorf("summarize daily website page views: %w", err)
	}
	var loginRows []dailyRow
	if err := s.db.WithContext(ctx).Raw(`
		SELECT to_char(occurred_at AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD') AS date,
		       COUNT(*)::bigint AS event_count, 0::bigint AS unique_ips
		FROM account_login_events
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY date ORDER BY date`, reportRange.From, reportRange.To).Scan(&loginRows).Error; err != nil {
		return nil, fmt.Errorf("summarize daily account logins: %w", err)
	}
	var downloadRows []dailyRow
	if err := s.db.WithContext(ctx).Raw(`
		SELECT to_char(occurred_at AT TIME ZONE 'Asia/Shanghai', 'YYYY-MM-DD') AS date,
		       COUNT(*)::bigint AS event_count, 0::bigint AS unique_ips
		FROM release_download_events
		WHERE occurred_at >= ? AND occurred_at < ?
		GROUP BY date ORDER BY date`, reportRange.From, reportRange.To).Scan(&downloadRows).Error; err != nil {
		return nil, fmt.Errorf("summarize daily release downloads: %w", err)
	}
	byDate := make(map[string]DailyStat, len(pageRows)+len(downloadRows)+len(loginRows))
	for _, row := range pageRows {
		byDate[row.Date] = DailyStat{Date: row.Date, PageViews: row.Count, UniqueIPs: row.UniqueIPs}
	}
	for _, row := range loginRows {
		item := byDate[row.Date]
		item.Date = row.Date
		item.LoginEvents = row.Count
		byDate[row.Date] = item
	}
	for _, row := range downloadRows {
		item := byDate[row.Date]
		item.Date = row.Date
		item.DownloadEvents = row.Count
		byDate[row.Date] = item
	}

	start := reportRange.From.In(reportLocation)
	day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, reportLocation)
	end := reportRange.To.In(reportLocation)
	items := make([]DailyStat, 0, int(reportRange.To.Sub(reportRange.From)/(24*time.Hour))+2)
	for day.Before(end) {
		date := day.Format(time.DateOnly)
		item := byDate[date]
		item.Date = date
		items = append(items, item)
		day = day.AddDate(0, 0, 1)
	}
	return items, nil
}

func validateReportRange(value ReportRange) error {
	if value.From.IsZero() || value.To.IsZero() || !value.To.After(value.From) || value.To.Sub(value.From) > maxReportDuration {
		return ErrReportRangeInvalid
	}
	granularity := strings.TrimSpace(value.Granularity)
	if granularity != "" && granularity != GranularityHour && granularity != GranularityDay {
		return ErrReportRangeInvalid
	}
	if granularity == GranularityHour && value.To.Sub(value.From) > maxHourlyReportDuration {
		return ErrReportRangeInvalid
	}
	return nil
}

func (s *Store) resolveLocation(ctx context.Context, ip netip.Addr) (Location, error) {
	location, err := lookupLocation(ctx, s.resolver, ip)
	if err != nil {
		return Location{}, err
	}
	location.CountryCode = strings.ToUpper(truncate(location.CountryCode, 2))
	location.CountryName = truncate(location.CountryName, 120)
	location.RegionName = truncate(location.RegionName, 160)
	location.CityName = truncate(location.CityName, 160)
	location.Source = strings.ToLower(truncate(strings.TrimSpace(location.Source), 32))
	return location, nil
}

func normalizeIP(raw string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, err
	}
	return ip.WithZone("").Unmap(), nil
}

func normalizePagePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || !strings.HasPrefix(raw, "/") {
		return "", ErrPageViewInvalid
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "", ErrPageViewInvalid
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(parsed.Path, "/"))
	if cleaned == "." {
		cleaned = "/"
	}
	if len(cleaned) > 512 || strings.ContainsAny(cleaned, "\r\n\x00") {
		return "", ErrPageViewInvalid
	}
	return cleaned, nil
}

func referrerHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	return truncate(strings.ToLower(parsed.Hostname()), 255)
}

func summarizeUserAgent(raw string) string {
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	return truncate(raw, 255)
}

func truncate(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

type pageViewRow struct {
	ID               int64     `gorm:"column:id;primaryKey"`
	Path             string    `gorm:"column:path"`
	ClientIP         string    `gorm:"column:client_ip"`
	CountryCode      string    `gorm:"column:country_code"`
	CountryName      string    `gorm:"column:country_name"`
	RegionName       string    `gorm:"column:region_name"`
	CityName         string    `gorm:"column:city_name"`
	ReferrerHost     string    `gorm:"column:referrer_host"`
	UserAgentSummary string    `gorm:"column:user_agent_summary"`
	OccurredAt       time.Time `gorm:"column:occurred_at"`
}

func (pageViewRow) TableName() string { return "website_page_views" }

type loginEventRow struct {
	ID               int64      `gorm:"column:id;primaryKey"`
	UserID           uuid.UUID  `gorm:"column:user_id;type:uuid"`
	SessionID        *uuid.UUID `gorm:"column:session_id;type:uuid"`
	AppSessionID     *uuid.UUID `gorm:"column:app_session_id;type:uuid"`
	LoginMethod      string     `gorm:"column:login_method"`
	ClientIP         string     `gorm:"column:client_ip"`
	CountryCode      string     `gorm:"column:country_code"`
	CountryName      string     `gorm:"column:country_name"`
	RegionName       string     `gorm:"column:region_name"`
	CityName         string     `gorm:"column:city_name"`
	UserAgentSummary string     `gorm:"column:user_agent_summary"`
	OccurredAt       time.Time  `gorm:"column:occurred_at"`
}

func (loginEventRow) TableName() string { return "account_login_events" }

type downloadEventRow struct {
	ID               int64     `gorm:"column:id;primaryKey"`
	AssetID          uuid.UUID `gorm:"column:asset_id;type:uuid"`
	ClientIP         string    `gorm:"column:client_ip"`
	UserAgentSummary string    `gorm:"column:user_agent_summary"`
	OccurredAt       time.Time `gorm:"column:occurred_at"`
}

func (downloadEventRow) TableName() string { return "release_download_events" }

type registrationEventRow struct {
	ID               int64     `gorm:"column:id;primaryKey"`
	UserID           uuid.UUID `gorm:"column:user_id;type:uuid"`
	ClientIP         string    `gorm:"column:client_ip"`
	UserAgentSummary string    `gorm:"column:user_agent_summary"`
	OccurredAt       time.Time `gorm:"column:occurred_at"`
}

func (registrationEventRow) TableName() string { return "account_registration_events" }

type locationCacheRow struct {
	ClientIP     string    `gorm:"column:client_ip;primaryKey"`
	CountryCode  string    `gorm:"column:country_code"`
	CountryName  string    `gorm:"column:country_name"`
	RegionName   string    `gorm:"column:region_name"`
	CityName     string    `gorm:"column:city_name"`
	Source       string    `gorm:"column:source"`
	LookupStatus string    `gorm:"column:lookup_status"`
	ResolvedAt   time.Time `gorm:"column:resolved_at"`
	ExpiresAt    time.Time `gorm:"column:expires_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (locationCacheRow) TableName() string { return "ip_geolocation_cache" }
