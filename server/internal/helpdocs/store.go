package helpdocs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrDocumentInvalid         = errors.New("help document is invalid")
	ErrDocumentNotFound        = errors.New("help document not found")
	ErrDocumentSlugConflict    = errors.New("help document slug already exists")
	ErrDocumentVersionConflict = errors.New("help document version conflict")
	ErrDocumentAlreadyArchived = errors.New("help document is archived")
	ErrDocumentFilterInvalid   = errors.New("help document filter is invalid")
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type PublicDocumentSummary struct {
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	SortOrder   int       `json:"sortOrder"`
	UpdatedAt   time.Time `json:"updatedAt"`
	SearchText  string    `json:"searchText"`
}

type PublicDocument struct {
	PublicDocumentSummary
	HTML string `json:"html"`
}

type AdminDocument struct {
	ID                    uuid.UUID  `json:"id"`
	Slug                  string     `json:"slug"`
	Title                 string     `json:"title"`
	Description           string     `json:"description"`
	Category              string     `json:"category"`
	SortOrder             int        `json:"sortOrder"`
	ContentMarkdown       string     `json:"contentMarkdown"`
	Status                string     `json:"status"`
	Version               int64      `json:"version"`
	PublishedVersion      *int64     `json:"publishedVersion"`
	HasUnpublishedChanges bool       `json:"hasUnpublishedChanges"`
	PublishedAt           *time.Time `json:"publishedAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

type AdminDocumentList struct {
	Items  []AdminDocument `json:"items"`
	Total  int64           `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

type AdminDocumentFilter struct {
	Query  string
	Status string
	Limit  int
	Offset int
}

type SaveDocumentInput struct {
	Slug            string
	Title           string
	Description     string
	Category        string
	SortOrder       int
	ContentMarkdown string
	ExpectedVersion int64
	ActorUserID     uuid.UUID
}

type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("help document database is required")
	}
	return &Store{db: db}, nil
}

type documentRow struct {
	ID               uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	Slug             string     `gorm:"column:slug"`
	Title            string     `gorm:"column:title"`
	Description      string     `gorm:"column:description"`
	Category         string     `gorm:"column:category"`
	SortOrder        int        `gorm:"column:sort_order"`
	ContentMarkdown  string     `gorm:"column:content_markdown"`
	Status           string     `gorm:"column:status"`
	Version          int64      `gorm:"column:version"`
	PublishedVersion *int64     `gorm:"column:published_version"`
	CreatedBy        uuid.UUID  `gorm:"column:created_by;type:uuid"`
	UpdatedBy        uuid.UUID  `gorm:"column:updated_by;type:uuid"`
	PublishedBy      *uuid.UUID `gorm:"column:published_by;type:uuid"`
	PublishedAt      *time.Time `gorm:"column:published_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

func (documentRow) TableName() string { return "help_documents" }

type publicationRow struct {
	ID          uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	DocumentID  uuid.UUID  `gorm:"column:document_id;type:uuid"`
	Version     int64      `gorm:"column:version"`
	Slug        string     `gorm:"column:slug"`
	Title       string     `gorm:"column:title"`
	Description string     `gorm:"column:description"`
	Category    string     `gorm:"column:category"`
	SortOrder   int        `gorm:"column:sort_order"`
	ContentHTML string     `gorm:"column:content_html"`
	SearchText  string     `gorm:"column:search_text"`
	PublishedBy *uuid.UUID `gorm:"column:published_by;type:uuid"`
	PublishedAt time.Time  `gorm:"column:published_at"`
}

func (publicationRow) TableName() string { return "help_document_publications" }

type auditRow struct {
	ID           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	ActorUserID  *uuid.UUID `gorm:"column:actor_user_id;type:uuid"`
	Action       string     `gorm:"column:action"`
	ResourceType string     `gorm:"column:resource_type"`
	ResourceID   *uuid.UUID `gorm:"column:resource_id;type:uuid"`
	BeforeJSON   []byte     `gorm:"column:before_json;type:jsonb"`
	AfterJSON    []byte     `gorm:"column:after_json;type:jsonb"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

func (auditRow) TableName() string { return "audit_logs" }

func (s *Store) ListPublished(ctx context.Context) ([]PublicDocumentSummary, error) {
	var rows []publicationRow
	err := s.db.WithContext(ctx).Table("help_document_publications AS publication").
		Select("publication.*").
		Joins("JOIN help_documents AS document ON document.id = publication.document_id AND document.published_version = publication.version").
		Where("document.status = ?", "published").
		Order("publication.sort_order ASC, publication.title ASC, publication.id ASC").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list published help documents: %w", err)
	}
	items := make([]PublicDocumentSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, publicSummaryFromPublication(row))
	}
	return items, nil
}

func (s *Store) GetPublished(ctx context.Context, slug string) (PublicDocument, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if !slugPattern.MatchString(slug) {
		return PublicDocument{}, ErrDocumentNotFound
	}
	var row publicationRow
	err := s.db.WithContext(ctx).Table("help_document_publications AS publication").
		Select("publication.*").
		Joins("JOIN help_documents AS document ON document.id = publication.document_id AND document.published_version = publication.version").
		Where("document.status = ? AND publication.slug = ?", "published", slug).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PublicDocument{}, ErrDocumentNotFound
	}
	if err != nil {
		return PublicDocument{}, fmt.Errorf("get published help document: %w", err)
	}
	return PublicDocument{PublicDocumentSummary: publicSummaryFromPublication(row), HTML: row.ContentHTML}, nil
}

func (s *Store) ListAdmin(ctx context.Context, filter AdminDocumentFilter) (AdminDocumentList, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	filter.Status = strings.TrimSpace(filter.Status)
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 ||
		(filter.Status != "" && filter.Status != "draft" && filter.Status != "published" && filter.Status != "archived") || utf8.RuneCountInString(filter.Query) > 160 {
		return AdminDocumentList{}, ErrDocumentFilterInvalid
	}
	query := s.db.WithContext(ctx).Model(&documentRow{})
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	if filter.Query != "" {
		like := "%" + strings.ToLower(filter.Query) + "%"
		query = query.Where("lower(title) LIKE ? OR lower(slug) LIKE ? OR lower(category) LIKE ?", like, like, like)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return AdminDocumentList{}, fmt.Errorf("count help documents: %w", err)
	}
	var rows []documentRow
	if err := query.Order("sort_order ASC, updated_at DESC, id ASC").Limit(filter.Limit).Offset(filter.Offset).Find(&rows).Error; err != nil {
		return AdminDocumentList{}, fmt.Errorf("list help documents: %w", err)
	}
	items := make([]AdminDocument, 0, len(rows))
	for _, row := range rows {
		items = append(items, adminDocumentFromRow(row))
	}
	return AdminDocumentList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Store) GetAdmin(ctx context.Context, documentID uuid.UUID) (AdminDocument, error) {
	var row documentRow
	if err := s.db.WithContext(ctx).First(&row, "id = ?", documentID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AdminDocument{}, ErrDocumentNotFound
		}
		return AdminDocument{}, fmt.Errorf("get help document: %w", err)
	}
	return adminDocumentFromRow(row), nil
}

func (s *Store) Create(ctx context.Context, input SaveDocumentInput) (AdminDocument, error) {
	input, err := validateInput(input, false)
	if err != nil {
		return AdminDocument{}, err
	}
	now := time.Now().UTC()
	row := documentRow{
		ID: uuid.New(), Slug: input.Slug, Title: input.Title, Description: input.Description,
		Category: input.Category, SortOrder: input.SortOrder, ContentMarkdown: input.ContentMarkdown,
		Status: "draft", Version: 1, CreatedBy: input.ActorUserID, UpdatedBy: input.ActorUserID,
		CreatedAt: now, UpdatedAt: now,
	}
	result := adminDocumentFromRow(row)
	after, _ := json.Marshal(result)
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrDocumentSlugConflict
			}
			return fmt.Errorf("create help document: %w", err)
		}
		return appendAudit(tx, input.ActorUserID, "help_document.create", row.ID, nil, after, now)
	})
	if err != nil {
		return AdminDocument{}, err
	}
	return result, nil
}

func (s *Store) Update(ctx context.Context, documentID uuid.UUID, input SaveDocumentInput) (AdminDocument, error) {
	if documentID == uuid.Nil {
		return AdminDocument{}, ErrDocumentNotFound
	}
	input, err := validateInput(input, true)
	if err != nil {
		return AdminDocument{}, err
	}
	var result AdminDocument
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row documentRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", documentID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDocumentNotFound
			}
			return fmt.Errorf("lock help document: %w", err)
		}
		if row.Version != input.ExpectedVersion {
			return ErrDocumentVersionConflict
		}
		// Once a URL has been published it remains reserved by this document;
		// otherwise a draft slug edit could let two live snapshots claim the
		// same public route before the edited draft is republished.
		if row.PublishedVersion != nil && row.Slug != input.Slug {
			return ErrDocumentInvalid
		}
		before, _ := json.Marshal(adminDocumentFromRow(row))
		now := time.Now().UTC()
		row.Slug, row.Title, row.Description = input.Slug, input.Title, input.Description
		row.Category, row.SortOrder, row.ContentMarkdown = input.Category, input.SortOrder, input.ContentMarkdown
		row.Version++
		row.UpdatedBy, row.UpdatedAt = input.ActorUserID, now
		if err := tx.Save(&row).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrDocumentSlugConflict
			}
			return fmt.Errorf("update help document: %w", err)
		}
		result = adminDocumentFromRow(row)
		after, _ := json.Marshal(result)
		return appendAudit(tx, input.ActorUserID, "help_document.update", row.ID, before, after, now)
	})
	if err != nil {
		return AdminDocument{}, err
	}
	return result, nil
}

func (s *Store) Publish(ctx context.Context, documentID, actorUserID uuid.UUID) (AdminDocument, error) {
	if documentID == uuid.Nil || actorUserID == uuid.Nil {
		return AdminDocument{}, ErrDocumentNotFound
	}
	var result AdminDocument
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row documentRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", documentID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDocumentNotFound
			}
			return fmt.Errorf("lock help document for publication: %w", err)
		}
		if row.Status == "archived" {
			return ErrDocumentAlreadyArchived
		}
		if row.PublishedVersion != nil && *row.PublishedVersion == row.Version && row.Status == "published" {
			result = adminDocumentFromRow(row)
			return nil
		}
		contentHTML, bodySearch, err := RenderStatic(row.ContentMarkdown)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		searchText := strings.Join(strings.Fields(strings.ToLower(strings.Join([]string{row.Title, row.Description, row.Category, bodySearch}, " "))), " ")
		publication := publicationRow{
			ID: uuid.New(), DocumentID: row.ID, Version: row.Version, Slug: row.Slug,
			Title: row.Title, Description: row.Description, Category: row.Category, SortOrder: row.SortOrder,
			ContentHTML: contentHTML, SearchText: searchText, PublishedBy: &actorUserID, PublishedAt: now,
		}
		if err := tx.Create(&publication).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrDocumentVersionConflict
			}
			return fmt.Errorf("create static help publication: %w", err)
		}
		before, _ := json.Marshal(adminDocumentFromRow(row))
		row.Status, row.PublishedVersion = "published", &row.Version
		row.PublishedAt, row.PublishedBy = &now, &actorUserID
		row.UpdatedBy, row.UpdatedAt = actorUserID, now
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("publish help document: %w", err)
		}
		result = adminDocumentFromRow(row)
		after, _ := json.Marshal(result)
		return appendAudit(tx, actorUserID, "help_document.publish", row.ID, before, after, now)
	})
	if err != nil {
		return AdminDocument{}, err
	}
	return result, nil
}

func (s *Store) Archive(ctx context.Context, documentID, actorUserID uuid.UUID) error {
	if documentID == uuid.Nil || actorUserID == uuid.Nil {
		return ErrDocumentNotFound
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row documentRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", documentID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrDocumentNotFound
			}
			return fmt.Errorf("lock help document for archival: %w", err)
		}
		if row.Status == "archived" {
			return nil
		}
		before, _ := json.Marshal(adminDocumentFromRow(row))
		now := time.Now().UTC()
		row.Status, row.UpdatedBy, row.UpdatedAt = "archived", actorUserID, now
		row.Version++
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("archive help document: %w", err)
		}
		after, _ := json.Marshal(adminDocumentFromRow(row))
		return appendAudit(tx, actorUserID, "help_document.archive", row.ID, before, after, now)
	})
}

func validateInput(input SaveDocumentInput, requireVersion bool) (SaveDocumentInput, error) {
	input.Slug = strings.ToLower(strings.TrimSpace(input.Slug))
	input.Title = strings.TrimSpace(input.Title)
	input.Description = strings.TrimSpace(input.Description)
	input.Category = strings.TrimSpace(input.Category)
	input.ContentMarkdown = strings.TrimSpace(input.ContentMarkdown)
	if input.ActorUserID == uuid.Nil || !slugPattern.MatchString(input.Slug) ||
		!plainText(input.Title, 160, false) || !plainText(input.Description, 500, true) ||
		!plainText(input.Category, 80, false) || input.SortOrder < -100000 || input.SortOrder > 100000 ||
		utf8.RuneCountInString(input.ContentMarkdown) < 1 || utf8.RuneCountInString(input.ContentMarkdown) > 100000 ||
		(requireVersion && input.ExpectedVersion < 1) {
		return SaveDocumentInput{}, ErrDocumentInvalid
	}
	for _, r := range input.ContentMarkdown {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return SaveDocumentInput{}, ErrDocumentInvalid
		}
	}
	return input, nil
}

func plainText(value string, maximum int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum || (!allowEmpty && value == "") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func adminDocumentFromRow(row documentRow) AdminDocument {
	return AdminDocument{
		ID: row.ID, Slug: row.Slug, Title: row.Title, Description: row.Description,
		Category: row.Category, SortOrder: row.SortOrder, ContentMarkdown: row.ContentMarkdown,
		Status: row.Status, Version: row.Version, PublishedVersion: row.PublishedVersion,
		HasUnpublishedChanges: row.PublishedVersion == nil || *row.PublishedVersion != row.Version,
		PublishedAt:           row.PublishedAt, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func publicSummaryFromPublication(row publicationRow) PublicDocumentSummary {
	return PublicDocumentSummary{
		Slug: row.Slug, Title: row.Title, Description: row.Description, Category: row.Category,
		SortOrder: row.SortOrder, UpdatedAt: row.PublishedAt.UTC(), SearchText: row.SearchText,
	}
}

func appendAudit(tx *gorm.DB, actor uuid.UUID, action string, resource uuid.UUID, before, after []byte, now time.Time) error {
	if err := tx.Create(&auditRow{
		ID: uuid.New(), ActorUserID: &actor, Action: action, ResourceType: "help_document",
		ResourceID: &resource, BeforeJSON: before, AfterJSON: after, CreatedAt: now,
	}).Error; err != nil {
		return fmt.Errorf("append help document audit: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "23505") || strings.Contains(message, "duplicate key") || strings.Contains(message, "unique constraint")
}
