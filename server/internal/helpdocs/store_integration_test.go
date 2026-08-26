//go:build integration

package helpdocs

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
)

func TestDocumentDraftPublicationSnapshotAndArchive(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store, _ := NewStore(db)
	actorID := uuid.New()
	slug := "integration-help-" + strings.ToLower(uuid.NewString()[:8])
	if err := db.Exec(`INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-only', 'Help Editor', 'active', now())`, actorID, slug+"@example.test").Error; err != nil {
		t.Fatalf("insert actor: %v", err)
	}
	var documentID uuid.UUID
	t.Cleanup(func() {
		if documentID != uuid.Nil {
			_ = db.Exec("DELETE FROM audit_logs WHERE resource_type = 'help_document' AND resource_id = ?", documentID).Error
			_ = db.Exec("DELETE FROM help_documents WHERE id = ?", documentID).Error
		}
		_ = db.Exec("DELETE FROM users WHERE id = ?", actorID).Error
	})

	created, err := store.Create(ctx, SaveDocumentInput{
		Slug: slug, Title: "第一版", Description: "说明", Category: "测试", SortOrder: 7,
		ContentMarkdown: "# 第一版\n\n正文<script>alert(1)</script>", ActorUserID: actorID,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	documentID = created.ID
	if items, err := store.ListPublished(ctx); err != nil || len(items) != 0 {
		t.Fatalf("ListPublished(before publish) = %+v, %v", items, err)
	}
	published, err := store.Publish(ctx, documentID, actorID)
	if err != nil || published.Status != "published" || published.PublishedVersion == nil {
		t.Fatalf("Publish() = %+v, %v", published, err)
	}
	publicV1, err := store.GetPublished(ctx, slug)
	if err != nil || publicV1.Title != "第一版" || strings.Contains(strings.ToLower(publicV1.HTML), "<script") {
		t.Fatalf("GetPublished(v1) = %+v, %v", publicV1, err)
	}
	updated, err := store.Update(ctx, documentID, SaveDocumentInput{
		Slug: slug, Title: "第二版草稿", Description: "说明", Category: "测试", SortOrder: 7,
		ContentMarkdown: "# 第二版\n\n新正文", ExpectedVersion: created.Version, ActorUserID: actorID,
	})
	if err != nil || !updated.HasUnpublishedChanges {
		t.Fatalf("Update() = %+v, %v", updated, err)
	}
	stillV1, err := store.GetPublished(ctx, slug)
	if err != nil || stillV1.Title != "第一版" {
		t.Fatalf("GetPublished(draft isolation) = %+v, %v", stillV1, err)
	}
	if _, err := store.Publish(ctx, documentID, actorID); err != nil {
		t.Fatalf("Publish(v2) error = %v", err)
	}
	publicV2, err := store.GetPublished(ctx, slug)
	if err != nil || publicV2.Title != "第二版草稿" || !strings.Contains(publicV2.HTML, "新正文") {
		t.Fatalf("GetPublished(v2) = %+v, %v", publicV2, err)
	}
	if err := store.Archive(ctx, documentID, actorID); err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if _, err := store.GetPublished(ctx, slug); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("GetPublished(after archive) error = %v", err)
	}
}
