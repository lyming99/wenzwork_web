package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/auth"
	"github.com/wenzwork/wenzwork-web/server/internal/feedback"
	"github.com/wenzwork/wenzwork-web/server/internal/helpdocs"
)

type fakeHelpService struct {
	createdInput helpdocs.SaveDocumentInput
	publishedID  uuid.UUID
}

func (f *fakeHelpService) ListPublished(context.Context) ([]helpdocs.PublicDocumentSummary, error) {
	return []helpdocs.PublicDocumentSummary{{Slug: "managed-help", Title: "Managed", UpdatedAt: time.Now()}}, nil
}
func (f *fakeHelpService) GetPublished(context.Context, string) (helpdocs.PublicDocument, error) {
	return helpdocs.PublicDocument{PublicDocumentSummary: helpdocs.PublicDocumentSummary{Slug: "managed-help", Title: "Managed", UpdatedAt: time.Now()}, HTML: "<h1>Managed</h1>"}, nil
}
func (f *fakeHelpService) ListAdmin(context.Context, helpdocs.AdminDocumentFilter) (helpdocs.AdminDocumentList, error) {
	return helpdocs.AdminDocumentList{Items: []helpdocs.AdminDocument{}, Limit: 50}, nil
}
func (f *fakeHelpService) GetAdmin(context.Context, uuid.UUID) (helpdocs.AdminDocument, error) {
	return helpdocs.AdminDocument{}, nil
}
func (f *fakeHelpService) Create(_ context.Context, input helpdocs.SaveDocumentInput) (helpdocs.AdminDocument, error) {
	f.createdInput = input
	return helpdocs.AdminDocument{ID: uuid.New(), Slug: input.Slug, Title: input.Title, Status: "draft", Version: 1}, nil
}
func (f *fakeHelpService) Update(context.Context, uuid.UUID, helpdocs.SaveDocumentInput) (helpdocs.AdminDocument, error) {
	return helpdocs.AdminDocument{}, nil
}
func (f *fakeHelpService) Publish(_ context.Context, documentID, _ uuid.UUID) (helpdocs.AdminDocument, error) {
	f.publishedID = documentID
	return helpdocs.AdminDocument{ID: documentID, Status: "published", Version: 1}, nil
}
func (f *fakeHelpService) Archive(context.Context, uuid.UUID, uuid.UUID) error { return nil }

type fakeFeedbackService struct {
	createInput feedback.CreateInput
	updateInput feedback.UpdateInput
	updatedID   uuid.UUID
}

func (f *fakeFeedbackService) Create(_ context.Context, input feedback.CreateInput) (feedback.Entry, error) {
	f.createInput = input
	return feedback.Entry{ID: uuid.New(), Category: input.Category, Subject: input.Subject, Status: "pending", CreatedAt: time.Now(), UpdatedAt: time.Now()}, nil
}
func (f *fakeFeedbackService) ListMine(context.Context, uuid.UUID, int) ([]feedback.Entry, error) {
	return []feedback.Entry{}, nil
}
func (f *fakeFeedbackService) ListAdmin(context.Context, feedback.AdminFilter) (feedback.AdminList, error) {
	return feedback.AdminList{Items: []feedback.AdminEntry{}, Limit: 50}, nil
}
func (f *fakeFeedbackService) Update(_ context.Context, feedbackID uuid.UUID, input feedback.UpdateInput) (feedback.AdminEntry, error) {
	f.updatedID, f.updateInput = feedbackID, input
	return feedback.AdminEntry{Entry: feedback.Entry{ID: feedbackID, Status: input.Status, CreatedAt: time.Now(), UpdatedAt: time.Now()}}, nil
}

func TestHelpRoutesExposeSnapshotsAndRequireContentPermissionForWrites(t *testing.T) {
	actorID := uuid.New()
	csrfToken, csrfHash, _ := auth.NewOpaqueToken()
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"content_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	help := &fakeHelpService{}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, Help: help, HelpAdmin: help,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	publicResponse := httptest.NewRecorder()
	router.ServeHTTP(publicResponse, httptest.NewRequest(http.MethodGet, "/api/v1/help-documents/managed-help", nil))
	if publicResponse.Code != http.StatusOK || !strings.Contains(publicResponse.Body.String(), `"html":"\u003ch1\u003eManaged`) || publicResponse.Header().Get("Cache-Control") == "" {
		t.Fatalf("public help response = %d %s headers=%v", publicResponse.Code, publicResponse.Body.String(), publicResponse.Header())
	}

	createResponse := httptest.NewRecorder()
	body := `{"slug":"new-help","title":"New help","description":"","category":"Basics","sortOrder":1,"contentMarkdown":"# Body"}`
	router.ServeHTTP(createResponse, adminRequest(http.MethodPost, "/api/v1/admin/help-documents", body, csrfToken, csrfToken))
	if createResponse.Code != http.StatusCreated || help.createdInput.ActorUserID != actorID || help.createdInput.Slug != "new-help" {
		t.Fatalf("create help response = %d %s input=%+v", createResponse.Code, createResponse.Body.String(), help.createdInput)
	}
	documentID := uuid.New()
	publishResponse := httptest.NewRecorder()
	router.ServeHTTP(publishResponse, adminRequest(http.MethodPost, "/api/v1/admin/help-documents/"+documentID.String()+"/publish", "", csrfToken, csrfToken))
	if publishResponse.Code != http.StatusOK || help.publishedID != documentID {
		t.Fatalf("publish help response = %d %s id=%s", publishResponse.Code, publishResponse.Body.String(), help.publishedID)
	}
}

func TestFeedbackRoutesDispatchMemberSubmissionAndSupportUpdate(t *testing.T) {
	memberID := uuid.New()
	memberCSRF, memberHash, _ := auth.NewOpaqueToken()
	service := &fakeFeedbackService{}
	memberRouter := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Feedback: service,
		Auth: &fakeAuthService{authenticated: auth.AuthenticatedSession{
			ID: uuid.New(), User: auth.User{ID: memberID, Status: "active", Roles: []string{"user"}},
			CSRFTokenHash: memberHash, AssuranceLevel: 1, AbsoluteExpiresAt: time.Now().Add(time.Hour),
		}}, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
	createResponse := httptest.NewRecorder()
	memberRouter.ServeHTTP(createResponse, adminRequest(http.MethodPost, "/api/v1/me/feedback", `{"category":"bug","subject":"Issue","content":"Details"}`, memberCSRF, memberCSRF))
	if createResponse.Code != http.StatusCreated || service.createInput.UserID != memberID {
		t.Fatalf("create feedback response = %d %s input=%+v", createResponse.Code, createResponse.Body.String(), service.createInput)
	}

	adminID, feedbackID := uuid.New(), uuid.New()
	adminCSRF, adminHash, _ := auth.NewOpaqueToken()
	adminRouter := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Feedback: service,
		Auth: &fakeAuthService{authenticated: auth.AuthenticatedSession{
			ID: uuid.New(), User: auth.User{ID: adminID, Status: "active", Roles: []string{"support_admin"}},
			CSRFTokenHash: adminHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
		}}, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
	updateResponse := httptest.NewRecorder()
	adminRouter.ServeHTTP(updateResponse, adminRequest(http.MethodPatch, "/api/v1/admin/feedback/"+feedbackID.String(), `{"status":"resolved","adminReply":"Fixed","internalNote":"QA"}`, adminCSRF, adminCSRF))
	if updateResponse.Code != http.StatusOK || service.updatedID != feedbackID || service.updateInput.ActorUserID != adminID {
		t.Fatalf("update feedback response = %d %s input=%+v", updateResponse.Code, updateResponse.Body.String(), service.updateInput)
	}
}
