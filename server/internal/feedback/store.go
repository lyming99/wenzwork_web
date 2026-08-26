package feedback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalid            = errors.New("feedback input is invalid")
	ErrNotFound           = errors.New("feedback entry not found")
	ErrRateLimited        = errors.New("feedback submission rate limited")
	ErrAdminFilterInvalid = errors.New("feedback admin filter is invalid")
)

type Entry struct {
	ID           uuid.UUID  `json:"id"`
	Category     string     `json:"category"`
	Subject      string     `json:"subject"`
	Content      string     `json:"content"`
	ContactEmail *string    `json:"contactEmail"`
	Status       string     `json:"status"`
	AdminReply   string     `json:"adminReply"`
	ResolvedAt   *time.Time `json:"resolvedAt"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

type AdminEntry struct {
	Entry
	UserID       uuid.UUID `json:"userId"`
	UserEmail    string    `json:"userEmail"`
	UserName     string    `json:"userName"`
	InternalNote string    `json:"internalNote"`
}

type AdminList struct {
	Items  []AdminEntry `json:"items"`
	Total  int64        `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

type CreateInput struct {
	UserID       uuid.UUID
	Category     string
	Subject      string
	Content      string
	ContactEmail string
}

type AdminFilter struct {
	Query    string
	Status   string
	Category string
	Limit    int
	Offset   int
}

type UpdateInput struct {
	Status       string
	AdminReply   string
	InternalNote string
	ActorUserID  uuid.UUID
}

type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("feedback database is required")
	}
	return &Store{db: db}, nil
}

type entryRow struct {
	ID           uuid.UUID  `gorm:"column:id;type:uuid;primaryKey"`
	UserID       uuid.UUID  `gorm:"column:user_id;type:uuid"`
	Category     string     `gorm:"column:category"`
	Subject      string     `gorm:"column:subject"`
	Content      string     `gorm:"column:content"`
	ContactEmail *string    `gorm:"column:contact_email"`
	Status       string     `gorm:"column:status"`
	AdminReply   string     `gorm:"column:admin_reply"`
	InternalNote string     `gorm:"column:internal_note"`
	HandledBy    *uuid.UUID `gorm:"column:handled_by;type:uuid"`
	ResolvedAt   *time.Time `gorm:"column:resolved_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

func (entryRow) TableName() string { return "feedback_entries" }

type adminEntryRow struct {
	ID           uuid.UUID  `gorm:"column:id;type:uuid"`
	UserID       uuid.UUID  `gorm:"column:user_id;type:uuid"`
	Category     string     `gorm:"column:category"`
	Subject      string     `gorm:"column:subject"`
	Content      string     `gorm:"column:content"`
	ContactEmail *string    `gorm:"column:contact_email"`
	Status       string     `gorm:"column:status"`
	AdminReply   string     `gorm:"column:admin_reply"`
	InternalNote string     `gorm:"column:internal_note"`
	HandledBy    *uuid.UUID `gorm:"column:handled_by;type:uuid"`
	ResolvedAt   *time.Time `gorm:"column:resolved_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
	UserEmail    string     `gorm:"column:user_email"`
	UserName     string     `gorm:"column:user_name"`
}

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

func (s *Store) Create(ctx context.Context, input CreateInput) (Entry, error) {
	input, contactEmail, err := validateCreate(input)
	if err != nil {
		return Entry{}, err
	}
	var recentCount int64
	if err := s.db.WithContext(ctx).Model(&entryRow{}).
		Where("user_id = ? AND created_at >= ?", input.UserID, time.Now().UTC().Add(-24*time.Hour)).
		Count(&recentCount).Error; err != nil {
		return Entry{}, fmt.Errorf("count recent feedback: %w", err)
	}
	if recentCount >= 20 {
		return Entry{}, ErrRateLimited
	}
	now := time.Now().UTC()
	row := entryRow{
		ID: uuid.New(), UserID: input.UserID, Category: input.Category, Subject: input.Subject,
		Content: input.Content, ContactEmail: contactEmail, Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Entry{}, fmt.Errorf("create feedback: %w", err)
	}
	return entryFromRow(row), nil
}

func (s *Store) ListMine(ctx context.Context, userID uuid.UUID, limit int) ([]Entry, error) {
	if userID == uuid.Nil {
		return nil, ErrInvalid
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	var rows []entryRow
	if err := s.db.WithContext(ctx).Where("user_id = ?", userID).
		Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list user feedback: %w", err)
	}
	items := make([]Entry, 0, len(rows))
	for _, row := range rows {
		items = append(items, entryFromRow(row))
	}
	return items, nil
}

func (s *Store) ListAdmin(ctx context.Context, filter AdminFilter) (AdminList, error) {
	filter.Query = strings.TrimSpace(filter.Query)
	filter.Status = strings.TrimSpace(filter.Status)
	filter.Category = strings.TrimSpace(filter.Category)
	if filter.Limit < 1 || filter.Limit > 100 || filter.Offset < 0 || utf8.RuneCountInString(filter.Query) > 160 ||
		(filter.Status != "" && !validStatus(filter.Status)) || (filter.Category != "" && !validCategory(filter.Category)) {
		return AdminList{}, ErrAdminFilterInvalid
	}
	query := s.db.WithContext(ctx).Table("feedback_entries AS feedback").
		Joins("JOIN users AS feedback_user ON feedback_user.id = feedback.user_id")
	if filter.Status != "" {
		query = query.Where("feedback.status = ?", filter.Status)
	}
	if filter.Category != "" {
		query = query.Where("feedback.category = ?", filter.Category)
	}
	if filter.Query != "" {
		like := "%" + strings.ToLower(filter.Query) + "%"
		query = query.Where("lower(feedback.subject) LIKE ? OR lower(feedback.content) LIKE ? OR lower(feedback_user.email) LIKE ?", like, like, like)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return AdminList{}, fmt.Errorf("count admin feedback: %w", err)
	}
	var rows []adminEntryRow
	if err := query.Select("feedback.*, feedback_user.email AS user_email, feedback_user.display_name AS user_name").
		Order("feedback.updated_at DESC, feedback.id DESC").Limit(filter.Limit).Offset(filter.Offset).Scan(&rows).Error; err != nil {
		return AdminList{}, fmt.Errorf("list admin feedback: %w", err)
	}
	items := make([]AdminEntry, 0, len(rows))
	for _, row := range rows {
		items = append(items, adminEntryFromRow(row))
	}
	return AdminList{Items: items, Total: total, Limit: filter.Limit, Offset: filter.Offset}, nil
}

func (s *Store) Update(ctx context.Context, feedbackID uuid.UUID, input UpdateInput) (AdminEntry, error) {
	input.Status = strings.TrimSpace(input.Status)
	input.AdminReply = strings.TrimSpace(input.AdminReply)
	input.InternalNote = strings.TrimSpace(input.InternalNote)
	if feedbackID == uuid.Nil || input.ActorUserID == uuid.Nil || !validStatus(input.Status) ||
		utf8.RuneCountInString(input.AdminReply) > 5000 || utf8.RuneCountInString(input.InternalNote) > 5000 ||
		!safeMultiline(input.AdminReply) || !safeMultiline(input.InternalNote) {
		return AdminEntry{}, ErrInvalid
	}
	var result AdminEntry
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row entryRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", feedbackID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return fmt.Errorf("lock feedback: %w", err)
		}
		before, _ := json.Marshal(row)
		now := time.Now().UTC()
		row.Status, row.AdminReply, row.InternalNote = input.Status, input.AdminReply, input.InternalNote
		row.HandledBy, row.UpdatedAt = &input.ActorUserID, now
		if input.Status == "resolved" || input.Status == "closed" {
			row.ResolvedAt = &now
		} else {
			row.ResolvedAt = nil
		}
		if err := tx.Save(&row).Error; err != nil {
			return fmt.Errorf("update feedback: %w", err)
		}
		var joined adminEntryRow
		if err := tx.Table("feedback_entries AS feedback").
			Select("feedback.*, feedback_user.email AS user_email, feedback_user.display_name AS user_name").
			Joins("JOIN users AS feedback_user ON feedback_user.id = feedback.user_id").
			Where("feedback.id = ?", row.ID).Scan(&joined).Error; err != nil {
			return fmt.Errorf("load updated feedback: %w", err)
		}
		result = adminEntryFromRow(joined)
		after, _ := json.Marshal(result)
		if err := tx.Create(&auditRow{
			ID: uuid.New(), ActorUserID: &input.ActorUserID, Action: "feedback.update",
			ResourceType: "feedback", ResourceID: &row.ID, BeforeJSON: before, AfterJSON: after, CreatedAt: now,
		}).Error; err != nil {
			return fmt.Errorf("append feedback audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return AdminEntry{}, err
	}
	return result, nil
}

func validateCreate(input CreateInput) (CreateInput, *string, error) {
	input.Category = strings.TrimSpace(input.Category)
	input.Subject = strings.TrimSpace(input.Subject)
	input.Content = strings.TrimSpace(input.Content)
	input.ContactEmail = strings.ToLower(strings.TrimSpace(input.ContactEmail))
	if input.UserID == uuid.Nil || !validCategory(input.Category) || !plainText(input.Subject, 160) ||
		utf8.RuneCountInString(input.Content) < 1 || utf8.RuneCountInString(input.Content) > 10000 || !safeMultiline(input.Content) {
		return CreateInput{}, nil, ErrInvalid
	}
	var contact *string
	if input.ContactEmail != "" {
		parsed, err := mail.ParseAddress(input.ContactEmail)
		if err != nil || !strings.EqualFold(parsed.Address, input.ContactEmail) || utf8.RuneCountInString(input.ContactEmail) > 320 {
			return CreateInput{}, nil, ErrInvalid
		}
		contact = &input.ContactEmail
	}
	return input, contact, nil
}

func validCategory(value string) bool {
	return value == "suggestion" || value == "bug" || value == "question" || value == "other"
}

func validStatus(value string) bool {
	return value == "pending" || value == "processing" || value == "resolved" || value == "closed"
}

func plainText(value string, maximum int) bool {
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func safeMultiline(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == 0 || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return false
		}
	}
	return true
}

func entryFromRow(row entryRow) Entry {
	return Entry{
		ID: row.ID, Category: row.Category, Subject: row.Subject, Content: row.Content,
		ContactEmail: row.ContactEmail, Status: row.Status, AdminReply: row.AdminReply,
		ResolvedAt: row.ResolvedAt, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC(),
	}
}

func adminEntryFromRow(row adminEntryRow) AdminEntry {
	return AdminEntry{
		Entry: entryFromRow(entryRow{
			ID: row.ID, UserID: row.UserID, Category: row.Category, Subject: row.Subject,
			Content: row.Content, ContactEmail: row.ContactEmail, Status: row.Status,
			AdminReply: row.AdminReply, InternalNote: row.InternalNote, HandledBy: row.HandledBy,
			ResolvedAt: row.ResolvedAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}), UserID: row.UserID, UserEmail: row.UserEmail,
		UserName: row.UserName, InternalNote: row.InternalNote,
	}
}
