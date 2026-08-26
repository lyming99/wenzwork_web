//go:build integration

package feedback

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
)

func TestFeedbackSubmissionAndAdminResolution(t *testing.T) {
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
	userID, adminID := uuid.New(), uuid.New()
	suffix := strings.ToLower(uuid.NewString()[:8])
	if err := db.Exec(`INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at) VALUES
		(?, ?, 'integration-only', 'Feedback User', 'active', now()),
		(?, ?, 'integration-only', 'Support Admin', 'active', now())`,
		userID, "feedback-user-"+suffix+"@example.test", adminID, "feedback-admin-"+suffix+"@example.test").Error; err != nil {
		t.Fatalf("insert users: %v", err)
	}
	var feedbackID uuid.UUID
	t.Cleanup(func() {
		if feedbackID != uuid.Nil {
			_ = db.Exec("DELETE FROM audit_logs WHERE resource_type = 'feedback' AND resource_id = ?", feedbackID).Error
			_ = db.Exec("DELETE FROM feedback_entries WHERE id = ?", feedbackID).Error
		}
		_ = db.Exec("DELETE FROM users WHERE id IN ?", []uuid.UUID{userID, adminID}).Error
	})

	created, err := store.Create(ctx, CreateInput{
		UserID: userID, Category: "bug", Subject: "无法导出", Content: "点击导出后没有响应。",
		ContactEmail: "feedback-user-" + suffix + "@example.test",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	feedbackID = created.ID
	mine, err := store.ListMine(ctx, userID, 50)
	if err != nil || len(mine) != 1 || mine[0].Status != "pending" {
		t.Fatalf("ListMine() = %+v, %v", mine, err)
	}
	adminList, err := store.ListAdmin(ctx, AdminFilter{Status: "pending", Category: "bug", Limit: 50})
	if err != nil || len(adminList.Items) != 1 || adminList.Items[0].UserName != "Feedback User" {
		t.Fatalf("ListAdmin() = %+v, %v", adminList, err)
	}
	updated, err := store.Update(ctx, feedbackID, UpdateInput{
		Status: "resolved", AdminReply: "已在下一版本修复。", InternalNote: "复现编号 QA-42", ActorUserID: adminID,
	})
	if err != nil || updated.Status != "resolved" || updated.ResolvedAt == nil || updated.InternalNote == "" {
		t.Fatalf("Update() = %+v, %v", updated, err)
	}
	mine, err = store.ListMine(ctx, userID, 50)
	if err != nil || len(mine) != 1 || mine[0].AdminReply != "已在下一版本修复。" || mine[0].ResolvedAt == nil {
		t.Fatalf("ListMine(after update) = %+v, %v", mine, err)
	}
}
