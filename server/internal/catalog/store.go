package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrReleaseNotFound = errors.New("release not found")
	ErrAssetNotFound   = errors.New("release asset not found")
)

type Store struct {
	db                       *gorm.DB
	releaseSourceTokenCipher *releaseSourceTokenCipher
}

type StoreOption func(*Store) error

type PricingPlan struct {
	Code               string   `json:"code"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	PriceMinor         *int64   `json:"priceMinor"`
	OriginalPriceMinor *int64   `json:"originalPriceMinor"`
	Currency           string   `json:"currency"`
	BillingPeriod      string   `json:"billingPeriod"`
	Features           []string `json:"features"`
}

type ReleaseFilter struct {
	Project      string
	Channel      string
	Platform     string
	Architecture string
	Limit        int
}

type Release struct {
	ID           uuid.UUID      `json:"id"`
	Project      string         `json:"project"`
	Version      string         `json:"version"`
	Channel      string         `json:"channel"`
	Title        string         `json:"title"`
	Summary      string         `json:"summary"`
	ReleaseNotes string         `json:"releaseNotes"`
	PublishedAt  time.Time      `json:"publishedAt"`
	Assets       []ReleaseAsset `json:"assets"`
}

type ReleaseAsset struct {
	ID              uuid.UUID `json:"id"`
	Platform        string    `json:"platform"`
	Architecture    string    `json:"architecture"`
	FileName        string    `json:"fileName"`
	FileSizeBytes   int64     `json:"fileSizeBytes"`
	SHA256          string    `json:"sha256"`
	SignatureStatus string    `json:"signatureStatus"`
	DownloadURL     string    `json:"downloadUrl"`
}

type ReleaseAssetDownload struct {
	Source        string
	ObjectKey     string
	FileName      string
	FileSizeBytes int64
	SHA256        string
	DownloadURL   string
}

func NewStore(db *gorm.DB, options ...StoreOption) (*Store, error) {
	if db == nil {
		return nil, errors.New("catalog store database is required")
	}
	store := &Store{db: db}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(store); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *Store) ListPricingPlans(ctx context.Context) ([]PricingPlan, error) {
	var rows []pricingPlanRow
	if err := s.db.WithContext(ctx).Table("pricing_plans AS plan").
		Select(`published.code, published.name, published.description, published.price_minor, published.original_price_minor,
			published.currency, published.billing_period, published.features_json`).
		Joins(`JOIN pricing_plan_versions AS published
			ON published.pricing_plan_id = plan.id AND published.version = plan.published_version`).
		Where("plan.status = 'published'").
		Order("published.sort_order ASC, published.code ASC").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("list published pricing plans: %w", err)
	}

	plans := make([]PricingPlan, 0, len(rows))
	for _, row := range rows {
		var features []string
		if err := json.Unmarshal(row.FeaturesJSON, &features); err != nil {
			return nil, fmt.Errorf("decode pricing plan %s features: %w", row.Code, err)
		}
		plans = append(plans, PricingPlan{
			Code:               row.Code,
			Name:               row.Name,
			Description:        row.Description,
			PriceMinor:         row.PriceMinor,
			OriginalPriceMinor: row.OriginalPriceMinor,
			Currency:           row.Currency,
			BillingPeriod:      row.BillingPeriod,
			Features:           features,
		})
	}
	return plans, nil
}

func (s *Store) LatestRelease(ctx context.Context, filter ReleaseFilter) (Release, error) {
	filter.Limit = 1
	releases, err := s.listReleases(ctx, filter)
	if err != nil {
		return Release{}, err
	}
	if len(releases) == 0 {
		return Release{}, ErrReleaseNotFound
	}
	return releases[0], nil
}

func (s *Store) ListReleases(ctx context.Context, filter ReleaseFilter) ([]Release, error) {
	if filter.Limit <= 0 || filter.Limit > 50 {
		filter.Limit = 20
	}
	return s.listReleases(ctx, filter)
}

func (s *Store) ReleaseAssetDownload(ctx context.Context, assetID uuid.UUID) (ReleaseAssetDownload, error) {
	var row releaseAssetRow
	err := s.db.WithContext(ctx).
		Joins("JOIN releases ON releases.id = release_assets.release_id").
		Where("release_assets.id = ? AND release_assets.status = 'published' AND releases.status = 'published'", assetID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ReleaseAssetDownload{}, ErrAssetNotFound
	}
	if err != nil {
		return ReleaseAssetDownload{}, fmt.Errorf("load public release asset: %w", err)
	}
	return ReleaseAssetDownload{
		Source: releaseAssetSource(row.ObjectKey), ObjectKey: row.ObjectKey, FileName: row.FileName,
		FileSizeBytes: row.FileSizeBytes, SHA256: row.SHA256, DownloadURL: row.DownloadURL,
	}, nil
}

func (s *Store) listReleases(ctx context.Context, filter ReleaseFilter) ([]Release, error) {
	project := filter.Project
	if project == "" {
		project = ReleaseProjectDesktop
	}
	channel := filter.Channel
	if channel == "" {
		channel = "stable"
	}

	query := s.db.WithContext(ctx).
		Model(&releaseRow{}).
		Where("releases.status = 'published' AND releases.project = ? AND releases.channel = ?", project, channel)

	if filter.Platform != "" || filter.Architecture != "" {
		subquery := s.db.Table("release_assets").
			Select("1").
			Where("release_assets.release_id = releases.id AND release_assets.status = 'published'")
		if filter.Platform != "" {
			subquery = subquery.Where("release_assets.platform = ?", filter.Platform)
		}
		if filter.Architecture != "" {
			subquery = subquery.Where("release_assets.architecture = ?", filter.Architecture)
		}
		query = query.Where("EXISTS (?)", subquery)
	}

	assetConditions := "status = 'published'"
	assetArguments := make([]any, 0, 2)
	if filter.Platform != "" {
		assetConditions += " AND platform = ?"
		assetArguments = append(assetArguments, filter.Platform)
	}
	if filter.Architecture != "" {
		assetConditions += " AND architecture = ?"
		assetArguments = append(assetArguments, filter.Architecture)
	}

	var rows []releaseRow
	if err := query.
		Preload("Assets", append([]any{assetConditions}, assetArguments...)...).
		Order("published_at DESC, id DESC").
		Limit(filter.Limit).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list public releases: %w", err)
	}

	releases := make([]Release, 0, len(rows))
	for _, row := range rows {
		assets := make([]ReleaseAsset, 0, len(row.Assets))
		for _, asset := range row.Assets {
			if !deploymentAssetMatchesRelease(
				row.Project, row.Version, asset.Platform, asset.Architecture, asset.FileName,
			) {
				continue
			}
			assets = append(assets, ReleaseAsset{
				ID:              asset.ID,
				Platform:        asset.Platform,
				Architecture:    asset.Architecture,
				FileName:        asset.FileName,
				FileSizeBytes:   asset.FileSizeBytes,
				SHA256:          asset.SHA256,
				SignatureStatus: asset.SignatureStatus,
				DownloadURL:     "/api/v1/release-assets/" + asset.ID.String() + "/download",
			})
		}
		releases = append(releases, Release{
			ID:           row.ID,
			Project:      row.Project,
			Version:      row.Version,
			Channel:      row.Channel,
			Title:        row.Title,
			Summary:      row.Summary,
			ReleaseNotes: row.ReleaseNotes,
			PublishedAt:  row.PublishedAt.UTC(),
			Assets:       assets,
		})
	}
	return releases, nil
}

type pricingPlanRow struct {
	ID                 uuid.UUID       `gorm:"column:id;type:uuid;primaryKey"`
	Code               string          `gorm:"column:code"`
	Name               string          `gorm:"column:name"`
	Description        string          `gorm:"column:description"`
	PriceMinor         *int64          `gorm:"column:price_minor"`
	OriginalPriceMinor *int64          `gorm:"column:original_price_minor"`
	Currency           string          `gorm:"column:currency"`
	BillingPeriod      string          `gorm:"column:billing_period"`
	FeaturesJSON       json.RawMessage `gorm:"column:features_json;type:jsonb"`
	Status             string          `gorm:"column:status"`
	SortOrder          int             `gorm:"column:sort_order"`
	Version            int64           `gorm:"column:version"`
	PublishedVersion   *int64          `gorm:"column:published_version"`
	PublishedAt        *time.Time      `gorm:"column:published_at"`
	CreatedAt          time.Time       `gorm:"column:created_at"`
	UpdatedAt          time.Time       `gorm:"column:updated_at"`
}

func (pricingPlanRow) TableName() string { return "pricing_plans" }

type releaseRow struct {
	ID           uuid.UUID         `gorm:"column:id;type:uuid;primaryKey"`
	Project      string            `gorm:"column:project"`
	Version      string            `gorm:"column:version"`
	Channel      string            `gorm:"column:channel"`
	Title        string            `gorm:"column:title"`
	Summary      string            `gorm:"column:summary"`
	ReleaseNotes string            `gorm:"column:release_notes"`
	Status       string            `gorm:"column:status"`
	PublishedAt  time.Time         `gorm:"column:published_at"`
	Assets       []releaseAssetRow `gorm:"foreignKey:ReleaseID"`
}

func (releaseRow) TableName() string { return "releases" }

type releaseAssetRow struct {
	ID              uuid.UUID `gorm:"column:id;type:uuid;primaryKey"`
	ReleaseID       uuid.UUID `gorm:"column:release_id;type:uuid"`
	Platform        string    `gorm:"column:platform"`
	Architecture    string    `gorm:"column:architecture"`
	FileName        string    `gorm:"column:file_name"`
	FileSizeBytes   int64     `gorm:"column:file_size_bytes"`
	SHA256          string    `gorm:"column:sha256"`
	SignatureStatus string    `gorm:"column:signature_status"`
	ObjectKey       string    `gorm:"column:object_key"`
	DownloadURL     string    `gorm:"column:download_url"`
	Status          string    `gorm:"column:status"`
}

func (releaseAssetRow) TableName() string { return "release_assets" }
