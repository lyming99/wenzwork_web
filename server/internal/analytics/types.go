package analytics

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

const ReportTimezone = "Asia/Shanghai"

const (
	LoginMethodPassword  = "password"
	LoginMethodAppDevice = "app_device"
	GranularityHour      = "hour"
	GranularityDay       = "day"
)

var (
	ErrPageViewInvalid     = errors.New("page view is invalid")
	ErrDownloadInvalid     = errors.New("download event is invalid")
	ErrRegistrationInvalid = errors.New("registration event is invalid")
	ErrReportRangeInvalid  = errors.New("analytics report range is invalid")
	ErrLoginFilterInvalid  = errors.New("login event filter is invalid")
)

type Location struct {
	CountryCode string `json:"countryCode"`
	CountryName string `json:"countryName"`
	RegionName  string `json:"regionName"`
	CityName    string `json:"cityName"`
	Source      string `json:"-" gorm:"-"`
}

type LocationResolver interface {
	Lookup(netip.Addr) (Location, error)
}

type contextLocationResolver interface {
	LookupContext(context.Context, netip.Addr) (Location, error)
}

type PageViewInput struct {
	Path      string
	Referrer  string
	ClientIP  string
	UserAgent string
}

type LoginEventInput struct {
	UserID       uuid.UUID
	WebSessionID uuid.UUID
	AppSessionID uuid.UUID
	LoginMethod  string
	ClientIP     string
	UserAgent    string
}

type DownloadEventInput struct {
	AssetID   uuid.UUID
	ClientIP  string
	UserAgent string
}

type RegistrationEventInput struct {
	UserID    uuid.UUID
	ClientIP  string
	UserAgent string
}

type ReportRange struct {
	From        time.Time
	To          time.Time
	Granularity string
}

type Overview struct {
	Range        OverviewRange  `json:"range"`
	Summary      Summary        `json:"summary"`
	Daily        []DailyStat    `json:"daily"`
	Timeline     []TimelineStat `json:"timeline"`
	Regions      []RegionStat   `json:"regions"`
	IPs          []IPStat       `json:"ips"`
	RecentNewIPs []RecentNewIP  `json:"recentNewIps"`
	Sources      []SourceStat   `json:"sources"`
	Paths        []PathStat     `json:"paths"`
}

type OverviewRange struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Timezone    string    `json:"timezone"`
	Granularity string    `json:"granularity"`
}

type Summary struct {
	PageViews               int64   `json:"pageViews"`
	UniqueIPs               int64   `json:"uniqueIps"`
	DownloadEvents          int64   `json:"downloadEvents"`
	LoginEvents             int64   `json:"loginEvents"`
	UniqueLoginIPs          int64   `json:"uniqueLoginIps"`
	DownloadedVisitorIPs    int64   `json:"downloadedVisitorIps"`
	RegisteredVisitorIPs    int64   `json:"registeredVisitorIps"`
	VisitorDownloadRate     float64 `json:"visitorDownloadRate"`
	VisitorRegistrationRate float64 `json:"visitorRegistrationRate"`
}

type DailyStat struct {
	Date           string `json:"date"`
	PageViews      int64  `json:"pageViews"`
	UniqueIPs      int64  `json:"uniqueIps"`
	DownloadEvents int64  `json:"downloadEvents"`
	LoginEvents    int64  `json:"loginEvents"`
}

type TimelineStat struct {
	BucketStart             time.Time `json:"bucketStart"`
	PageViews               int64     `json:"pageViews"`
	UniqueIPs               int64     `json:"uniqueIps"`
	DownloadEvents          int64     `json:"downloadEvents"`
	LoginEvents             int64     `json:"loginEvents"`
	DownloadedVisitorIPs    int64     `json:"downloadedVisitorIps"`
	RegisteredVisitorIPs    int64     `json:"registeredVisitorIps"`
	VisitorDownloadRate     float64   `json:"visitorDownloadRate"`
	VisitorRegistrationRate float64   `json:"visitorRegistrationRate"`
}

type RegionStat struct {
	Location
	PageViews int64 `json:"pageViews"`
	UniqueIPs int64 `json:"uniqueIps"`
}

type IPStat struct {
	IP string `json:"ip"`
	Location
	PageViews  int64     `json:"pageViews"`
	LastSeenAt time.Time `json:"lastSeenAt"`
}

type RecentNewIP struct {
	IP string `json:"ip"`
	Location
	PageViews         int64     `json:"pageViews"`
	FirstSeenAt       time.Time `json:"firstSeenAt"`
	LastSeenAt        time.Time `json:"lastSeenAt"`
	DownloadedSameDay bool      `json:"downloadedSameDay"`
	RegisteredSameDay bool      `json:"registeredSameDay"`
}

type SourceStat struct {
	ReferrerHost string `json:"referrerHost"`
	PageViews    int64  `json:"pageViews"`
	UniqueIPs    int64  `json:"uniqueIps"`
}

type PathStat struct {
	Path      string `json:"path"`
	PageViews int64  `json:"pageViews"`
	UniqueIPs int64  `json:"uniqueIps"`
}

type LoginEventFilter struct {
	ReportRange
	Query  string
	Limit  int
	Offset int
}

type LoginEventList struct {
	Items  []LoginEvent `json:"items"`
	Total  int64        `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

type LoginEvent struct {
	ID          int64     `json:"id"`
	UserID      uuid.UUID `json:"userId"`
	Email       string    `json:"email"`
	DisplayName string    `json:"displayName"`
	IP          string    `json:"ip"`
	Location
	UserAgentSummary string    `json:"userAgentSummary"`
	LoginMethod      string    `json:"loginMethod"`
	LoggedInAt       time.Time `json:"loggedInAt"`
}
