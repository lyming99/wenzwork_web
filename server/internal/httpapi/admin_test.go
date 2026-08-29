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
	"github.com/wenzwork/wenzwork-web/server/internal/catalog"
	"github.com/wenzwork/wenzwork-web/server/internal/membership"
	"github.com/wenzwork/wenzwork-web/server/internal/objectstore"
	"github.com/wenzwork/wenzwork-web/server/internal/releaseassets"
)

type fakeAdminUserService struct {
	listResult  auth.AdminUserList
	created     auth.AdminUser
	createInput auth.AdminCreateUserInput
	status      string
}

func (f *fakeAdminUserService) ListAdminUsers(context.Context, auth.AdminUserListFilter) (auth.AdminUserList, error) {
	return f.listResult, nil
}

func (f *fakeAdminUserService) CreateAdminUser(_ context.Context, input auth.AdminCreateUserInput) (auth.AdminUser, error) {
	f.createInput = input
	return f.created, nil
}

func (f *fakeAdminUserService) SetAdminUserStatus(_ context.Context, _, _ uuid.UUID, status string) (auth.AdminUser, error) {
	f.status = status
	return f.created, nil
}

type fakeAdminMembershipService struct {
	setInput    membership.SetMembershipInput
	setUserID   uuid.UUID
	revokedCode uuid.UUID
	codes       membership.RedemptionCodeList
}

func (f *fakeAdminMembershipService) SetUserMembership(_ context.Context, userID uuid.UUID, input membership.SetMembershipInput) (membership.MembershipStatus, error) {
	f.setUserID = userID
	f.setInput = input
	return membership.MembershipStatus{
		PlanCode: "pro", PlanName: "Pro", StartsAt: time.Now(), Lifetime: true, Source: "admin_adjustment",
	}, nil
}

func (f *fakeAdminMembershipService) CancelUserMembership(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

func (f *fakeAdminMembershipService) ListRedemptionCodes(context.Context, membership.RedemptionCodeFilter) (membership.RedemptionCodeList, error) {
	return f.codes, nil
}

func (f *fakeAdminMembershipService) RevokeRedemptionCode(_ context.Context, codeID, _ uuid.UUID) error {
	f.revokedCode = codeID
	return nil
}

type fakeAdminReleaseService struct {
	input              catalog.SaveReleaseInput
	settings           catalog.ReleaseDeliverySettings
	sourceSettings     catalog.ReleaseSourceSettings
	sourceInput        catalog.UpdateReleaseSourceSettingsInput
	sourceToken        string
	deletedReleaseID   uuid.UUID
	deletedActorUserID uuid.UUID
}

type fakeReleaseAssetUploader struct {
	input  objectstore.ReleaseAssetUploadInput
	result objectstore.ReleaseAssetUpload
	body   string
}

func (f *fakeReleaseAssetUploader) Upload(_ context.Context, input objectstore.ReleaseAssetUploadInput, body io.Reader) (objectstore.ReleaseAssetUpload, error) {
	f.input = input
	payload, _ := io.ReadAll(body)
	f.body = string(payload)
	return f.result, nil
}

type fakeReleaseAssetSource struct {
	input         releaseassets.RemoteImportInput
	stored        releaseassets.StoredAsset
	latest        releaseassets.GitHubRelease
	latestErr     error
	mirror        releaseassets.MirrorReleaseImport
	mirrorErr     error
	mirrorURL     string
	mirrorProject string
	repository    string
	token         string
}

func (f *fakeReleaseAssetSource) ImportRemote(_ context.Context, input releaseassets.RemoteImportInput) (releaseassets.StoredAsset, error) {
	f.input = input
	return f.stored, nil
}

func (f *fakeReleaseAssetSource) LatestGitHubRelease(_ context.Context, repository, token string) (releaseassets.GitHubRelease, error) {
	f.repository = repository
	f.token = token
	return f.latest, f.latestErr
}

func (f *fakeReleaseAssetSource) ImportLatestMirrorRelease(_ context.Context, mirrorBaseURL, project string) (releaseassets.MirrorReleaseImport, error) {
	f.mirrorURL = mirrorBaseURL
	f.mirrorProject = project
	return f.mirror, f.mirrorErr
}

func (f *fakeAdminReleaseService) ListAdminReleases(context.Context, int) ([]catalog.AdminRelease, error) {
	return []catalog.AdminRelease{}, nil
}

func (f *fakeAdminReleaseService) CreateRelease(_ context.Context, input catalog.SaveReleaseInput) (catalog.AdminRelease, error) {
	f.input = input
	return catalog.AdminRelease{ID: uuid.New(), Version: input.Version, Title: input.Title, Status: input.Status, Assets: []catalog.AdminReleaseAsset{}}, nil
}

func (f *fakeAdminReleaseService) UpdateRelease(_ context.Context, _ uuid.UUID, input catalog.SaveReleaseInput) (catalog.AdminRelease, error) {
	f.input = input
	return catalog.AdminRelease{ID: uuid.New(), Version: input.Version, Title: input.Title, Status: input.Status, Assets: []catalog.AdminReleaseAsset{}}, nil
}

func (f *fakeAdminReleaseService) PublishRelease(_ context.Context, releaseID uuid.UUID, _ uuid.UUID) (catalog.AdminRelease, error) {
	return catalog.AdminRelease{ID: releaseID, Status: "published"}, nil
}

func (f *fakeAdminReleaseService) WithdrawRelease(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func (f *fakeAdminReleaseService) DeleteRelease(_ context.Context, releaseID, actorUserID uuid.UUID) error {
	f.deletedReleaseID = releaseID
	f.deletedActorUserID = actorUserID
	return nil
}

func (f *fakeAdminReleaseService) GetReleaseDeliverySettings(context.Context) (catalog.ReleaseDeliverySettings, error) {
	return f.settings, nil
}

func (f *fakeAdminReleaseService) UpdateReleaseDeliverySettings(_ context.Context, input catalog.UpdateReleaseDeliverySettingsInput) (catalog.ReleaseDeliverySettings, error) {
	f.settings = catalog.ReleaseDeliverySettings{
		DownloadMode: input.DownloadMode, S3URLPrefix: input.S3URLPrefix,
		Version: input.ExpectedVersion + 1, UpdatedAt: time.Now(),
	}
	return f.settings, nil
}

func (f *fakeAdminReleaseService) GetReleaseSourceSettings(context.Context) (catalog.ReleaseSourceSettings, error) {
	return f.sourceSettings, nil
}

func (f *fakeAdminReleaseService) GetReleaseSourceSettingsForProject(_ context.Context, project string) (catalog.ReleaseSourceSettings, error) {
	settings := f.sourceSettings
	settings.Project = project
	return settings, nil
}

func (f *fakeAdminReleaseService) GetReleaseSourceCredentials(context.Context) (catalog.ReleaseSourceCredentials, error) {
	return catalog.ReleaseSourceCredentials{
		GitHubRepository: f.sourceSettings.GitHubRepository,
		GitHubToken:      f.sourceToken,
		MirrorBaseURL:    f.sourceSettings.MirrorBaseURL,
	}, nil
}

func (f *fakeAdminReleaseService) UpdateReleaseSourceSettings(_ context.Context, input catalog.UpdateReleaseSourceSettingsInput) (catalog.ReleaseSourceSettings, error) {
	f.sourceInput = input
	if input.ClearGitHubToken {
		f.sourceToken = ""
	} else if input.GitHubToken != nil && strings.TrimSpace(*input.GitHubToken) != "" {
		f.sourceToken = strings.TrimSpace(*input.GitHubToken)
	}
	f.sourceSettings = catalog.ReleaseSourceSettings{
		Project:               input.Project,
		GitHubRepository:      input.GitHubRepository,
		GitHubTokenConfigured: f.sourceToken != "",
		MirrorBaseURL:         input.MirrorBaseURL,
		Version:               input.ExpectedVersion + 1,
		UpdatedAt:             time.Now(),
	}
	return f.sourceSettings, nil
}

type fakeAdminPricingService struct {
	plans       []catalog.AdminPricingPlan
	saveInput   catalog.SavePricingPlanInput
	actionInput catalog.PricingPlanActionInput
	planID      uuid.UUID
}

func (f *fakeAdminPricingService) ListAdminPricingPlans(context.Context) ([]catalog.AdminPricingPlan, error) {
	return f.plans, nil
}

func (f *fakeAdminPricingService) CreatePricingPlan(_ context.Context, input catalog.SavePricingPlanInput) (catalog.AdminPricingPlan, error) {
	f.saveInput = input
	return catalog.AdminPricingPlan{ID: uuid.New(), Code: input.Code, Name: input.Name, Status: "draft", Version: 1, Features: []string{}, RemoteAccessEnabled: input.RemoteAccessEnabled, DeviceLimit: input.DeviceLimit, MonthlyTrafficLimitGB: input.MonthlyTrafficLimitGB}, nil
}

func (f *fakeAdminPricingService) UpdatePricingPlan(_ context.Context, planID uuid.UUID, input catalog.SavePricingPlanInput) (catalog.AdminPricingPlan, error) {
	f.planID, f.saveInput = planID, input
	return catalog.AdminPricingPlan{ID: planID, Code: input.Code, Name: input.Name, Status: "draft", Version: input.ExpectedVersion + 1, Features: input.Features, RemoteAccessEnabled: input.RemoteAccessEnabled, DeviceLimit: input.DeviceLimit, MonthlyTrafficLimitGB: input.MonthlyTrafficLimitGB}, nil
}

func (f *fakeAdminPricingService) PublishPricingPlan(_ context.Context, planID uuid.UUID, input catalog.PricingPlanActionInput) (catalog.AdminPricingPlan, error) {
	if !input.Confirm {
		return catalog.AdminPricingPlan{}, catalog.ErrPricingPlanConfirmationRequired
	}
	f.planID, f.actionInput = planID, input
	return catalog.AdminPricingPlan{ID: planID, Code: "pro", Name: "Pro", Status: "published", Version: input.ExpectedVersion + 1, Features: []string{}}, nil
}

func (f *fakeAdminPricingService) ArchivePricingPlan(_ context.Context, planID uuid.UUID, input catalog.PricingPlanActionInput) (catalog.AdminPricingPlan, error) {
	f.planID, f.actionInput = planID, input
	return catalog.AdminPricingPlan{ID: planID, Code: "pro", Name: "Pro", Status: "archived", Version: input.ExpectedVersion + 1, Features: []string{}}, nil
}

func TestAdminMembershipRoutesAllowMembershipRoleAndExposeCodeStatus(t *testing.T) {
	actorID := uuid.New()
	userID := uuid.New()
	codeID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"membership_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	users := &fakeAdminUserService{listResult: auth.AdminUserList{Items: []auth.AdminUser{{
		ID: userID, Email: "member@example.test", DisplayName: "Member", Status: "active", Roles: []string{"user"}, CreatedAt: time.Now(),
	}}, Total: 1, Limit: 50}}
	memberships := &fakeAdminMembershipService{codes: membership.RedemptionCodeList{
		Items: []membership.RedemptionCodeSummary{{
			ID: codeID, BatchID: uuid.New(), BatchName: "Launch", CodeHint: "ABCD", Status: "active", CreatedAt: time.Now(),
		}}, Total: 1, Limit: 100,
	}}
	router := adminTestRouter(authService, users, memberships, nil)

	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, adminRequest(http.MethodGet, "/api/v1/admin/users", "", "", ""))
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), "member@example.test") {
		t.Fatalf("user list response = %d %s", listResponse.Code, listResponse.Body.String())
	}

	setResponse := httptest.NewRecorder()
	setBody := `{"planCode":"pro","expiresAt":null,"reason":"support"}`
	router.ServeHTTP(setResponse, adminRequest(http.MethodPut, "/api/v1/admin/users/"+userID.String()+"/membership", setBody, csrfToken, csrfToken))
	if setResponse.Code != http.StatusOK || memberships.setUserID != userID || memberships.setInput.ActorUserID != actorID {
		t.Fatalf("membership response = %d %s input=%+v", setResponse.Code, setResponse.Body.String(), memberships.setInput)
	}

	codesResponse := httptest.NewRecorder()
	router.ServeHTTP(codesResponse, adminRequest(http.MethodGet, "/api/v1/admin/redemption-codes", "", "", ""))
	if codesResponse.Code != http.StatusOK || !strings.Contains(codesResponse.Body.String(), `"status":"active"`) {
		t.Fatalf("code list response = %d %s", codesResponse.Code, codesResponse.Body.String())
	}

	revokeResponse := httptest.NewRecorder()
	router.ServeHTTP(revokeResponse, adminRequest(http.MethodDelete, "/api/v1/admin/redemption-codes/"+codeID.String(), "", csrfToken, csrfToken))
	if revokeResponse.Code != http.StatusNoContent || memberships.revokedCode != codeID {
		t.Fatalf("revoke response = %d %s code=%s", revokeResponse.Code, revokeResponse.Body.String(), memberships.revokedCode)
	}

	createResponse := httptest.NewRecorder()
	router.ServeHTTP(createResponse, adminRequest(http.MethodPost, "/api/v1/admin/users", `{"email":"new@example.test","password":"long enough password","displayName":"New"}`, csrfToken, csrfToken))
	if createResponse.Code != http.StatusForbidden {
		t.Fatalf("membership administrator created user: %d %s", createResponse.Code, createResponse.Body.String())
	}
}

func TestAdminUserAndReleaseWritesRequireCSRFAndDispatchActor(t *testing.T) {
	actorID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"super_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	users := &fakeAdminUserService{created: auth.AdminUser{
		ID: uuid.New(), Email: "created@example.test", DisplayName: "Created", Status: "active", Roles: []string{"user"}, CreatedAt: time.Now(),
	}}
	releases := &fakeAdminReleaseService{}
	router := adminTestRouter(authService, users, &fakeAdminMembershipService{}, releases)

	missingCSRF := httptest.NewRecorder()
	router.ServeHTTP(missingCSRF, adminRequest(http.MethodPost, "/api/v1/admin/users", `{"email":"created@example.test","password":"long enough password","displayName":"Created"}`, csrfToken, ""))
	if missingCSRF.Code != http.StatusForbidden || !strings.Contains(missingCSRF.Body.String(), `"code":"csrf_rejected"`) {
		t.Fatalf("missing CSRF response = %d %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	createdResponse := httptest.NewRecorder()
	router.ServeHTTP(createdResponse, adminRequest(http.MethodPost, "/api/v1/admin/users", `{"email":"created@example.test","password":"long enough password","displayName":"Created"}`, csrfToken, csrfToken))
	if createdResponse.Code != http.StatusCreated || users.createInput.ActorUserID != actorID {
		t.Fatalf("create user response = %d %s input=%+v", createdResponse.Code, createdResponse.Body.String(), users.createInput)
	}

	releaseResponse := httptest.NewRecorder()
	releaseBody := `{"version":"1.2.3","channel":"stable","title":"Update","summary":"Summary","releaseNotes":"Fixed things","status":"draft","assets":[]}`
	router.ServeHTTP(releaseResponse, adminRequest(http.MethodPost, "/api/v1/admin/releases", releaseBody, csrfToken, csrfToken))
	if releaseResponse.Code != http.StatusCreated || releases.input.ActorUserID != actorID || releases.input.ReleaseNotes != "Fixed things" {
		t.Fatalf("create release response = %d %s input=%+v", releaseResponse.Code, releaseResponse.Body.String(), releases.input)
	}

	releaseID := uuid.New()
	deleteWithoutCSRF := httptest.NewRecorder()
	router.ServeHTTP(deleteWithoutCSRF, adminRequest(http.MethodDelete, "/api/v1/admin/releases/"+releaseID.String()+"/permanent", "", csrfToken, ""))
	if deleteWithoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("delete release without CSRF response = %d %s", deleteWithoutCSRF.Code, deleteWithoutCSRF.Body.String())
	}

	deleteResponse := httptest.NewRecorder()
	router.ServeHTTP(deleteResponse, adminRequest(http.MethodDelete, "/api/v1/admin/releases/"+releaseID.String()+"/permanent", "", csrfToken, csrfToken))
	if deleteResponse.Code != http.StatusNoContent || releases.deletedReleaseID != releaseID || releases.deletedActorUserID != actorID {
		t.Fatalf("delete release response = %d %s release=%s actor=%s", deleteResponse.Code, deleteResponse.Body.String(), releases.deletedReleaseID, releases.deletedActorUserID)
	}
}

func TestAdminReleaseAssetUploadUsesAuthenticatedSameOriginStream(t *testing.T) {
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: []string{"release_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	uploader := &fakeReleaseAssetUploader{result: objectstore.ReleaseAssetUpload{
		ObjectKey:     "releases/1.2.3/windows/x64/id/WenzWork.exe",
		DownloadURL:   "https://downloads.example.test/releases/1.2.3/windows/x64/id/WenzWork.exe",
		FileSizeBytes: 4, SHA256: strings.Repeat("a", 64),
	}}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		ReleaseUploads: uploader, AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
	response := httptest.NewRecorder()
	request := adminRequest(http.MethodPost, "/api/v1/admin/release-assets/upload?version=1.2.3&platform=windows&architecture=x64&fileName=WenzWork.exe&fileSizeBytes=4&sha256="+strings.Repeat("a", 64), "data", csrfToken, csrfToken)
	request.Header.Set("Content-Type", "application/octet-stream")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"objectKey":"releases/1.2.3/windows/x64/id/WenzWork.exe"`) {
		t.Fatalf("upload response = %d %s", response.Code, response.Body.String())
	}
	if uploader.input.Version != "1.2.3" || uploader.input.FileSizeBytes != 4 || uploader.input.Platform != "windows" || uploader.body != "data" {
		t.Fatalf("upload input = %+v body=%q", uploader.input, uploader.body)
	}
}

func TestAdminReleaseImportsRemoteAndGitHubAssetsAndUpdatesDeliverySettings(t *testing.T) {
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: uuid.New(), Status: "active", Roles: []string{"release_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	sources := &fakeReleaseAssetSource{
		stored: releaseassets.StoredAsset{AssetMetadata: releaseassets.AssetMetadata{
			FileName: "WenzWork.exe", FileSizeBytes: 42, SHA256: strings.Repeat("b", 64),
			DownloadURL: "https://downloads.example.test/releases/id/WenzWork.exe", Platform: "windows", Architecture: "x64",
		}, ObjectKey: "releases/1.2.3/windows/x64/id/WenzWork.exe"},
		latest: releaseassets.GitHubRelease{Repository: "acme/wenzwork", TagName: "v1.2.3", Version: "1.2.3", Name: "Release", Assets: []releaseassets.AssetMetadata{}},
		mirror: releaseassets.MirrorReleaseImport{
			MirrorBaseURL: "https://mirror.example.test", Project: "desktop", Version: "1.2.3", Channel: "stable",
			Title: "Release", Assets: []releaseassets.MirrorReleaseAsset{{
				FileName: "WenzWork-windows-x64.exe", FileSizeBytes: 42, SHA256: strings.Repeat("b", 64),
				ContentType: "application/octet-stream", DownloadURL: "https://mirror.example.test/api/v1/release-assets/id/download",
				Source: "mirror", ObjectKey: "mirror/" + strings.Repeat("c", 64) + "/WenzWork-windows-x64.exe",
				Platform: "windows", Architecture: "x64", SignatureStatus: "valid",
			}},
		},
	}
	releases := &fakeAdminReleaseService{
		settings: catalog.ReleaseDeliverySettings{DownloadMode: catalog.ReleaseDownloadProxyCached, Version: 1, UpdatedAt: time.Now()},
		sourceSettings: catalog.ReleaseSourceSettings{
			Project: "desktop", GitHubRepository: "acme/wenzwork", GitHubTokenConfigured: true,
			MirrorBaseURL: "https://mirror.example.test", Version: 3, UpdatedAt: time.Now(),
		},
		sourceToken: "token-initial",
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		CatalogAdmin: releases, ReleaseSources: sources,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	importResponse := httptest.NewRecorder()
	importBody := `{"version":"1.2.3","platform":"windows","architecture":"x64","downloadUrl":"https://github.com/acme/wenzwork/file.exe"}`
	router.ServeHTTP(importResponse, adminRequest(http.MethodPost, "/api/v1/admin/release-assets/import", importBody, csrfToken, csrfToken))
	if importResponse.Code != http.StatusCreated || sources.input.DownloadURL != "https://github.com/acme/wenzwork/file.exe" || !strings.Contains(importResponse.Body.String(), `"objectKey":"releases/1.2.3/windows/x64/id/WenzWork.exe"`) {
		t.Fatalf("import response = %d %s input=%+v", importResponse.Code, importResponse.Body.String(), sources.input)
	}

	getSourceSettingsResponse := httptest.NewRecorder()
	router.ServeHTTP(getSourceSettingsResponse, adminRequest(http.MethodGet, "/api/v1/admin/release-source-settings", "", "", ""))
	if getSourceSettingsResponse.Code != http.StatusOK || !strings.Contains(getSourceSettingsResponse.Body.String(), `"githubRepository":"acme/wenzwork"`) || !strings.Contains(getSourceSettingsResponse.Body.String(), `"githubTokenConfigured":true`) || !strings.Contains(getSourceSettingsResponse.Body.String(), `"mirrorBaseUrl":"https://mirror.example.test"`) || strings.Contains(getSourceSettingsResponse.Body.String(), "token-initial") {
		t.Fatalf("get source settings response = %d %s", getSourceSettingsResponse.Code, getSourceSettingsResponse.Body.String())
	}

	githubResponse := httptest.NewRecorder()
	router.ServeHTTP(githubResponse, adminRequest(http.MethodGet, "/api/v1/admin/github-releases/latest", "", "", ""))
	if githubResponse.Code != http.StatusOK || !strings.Contains(githubResponse.Body.String(), `"tagName":"v1.2.3"`) || sources.repository != "acme/wenzwork" || sources.token != "token-initial" {
		t.Fatalf("GitHub response = %d %s repository=%q token=%q", githubResponse.Code, githubResponse.Body.String(), sources.repository, sources.token)
	}

	mirrorResponse := httptest.NewRecorder()
	router.ServeHTTP(mirrorResponse, adminRequest(http.MethodPost, "/api/v1/admin/mirror-releases/latest/import?project=desktop", "", csrfToken, csrfToken))
	if mirrorResponse.Code != http.StatusCreated || !strings.Contains(mirrorResponse.Body.String(), `"mirrorBaseUrl":"https://mirror.example.test"`) ||
		!strings.Contains(mirrorResponse.Body.String(), `"source":"mirror"`) || sources.mirrorURL != "https://mirror.example.test" || sources.mirrorProject != "desktop" {
		t.Fatalf("mirror response = %d %s mirrorURL=%q project=%q", mirrorResponse.Code, mirrorResponse.Body.String(), sources.mirrorURL, sources.mirrorProject)
	}

	sourceSettingsResponse := httptest.NewRecorder()
	sourceSettingsBody := `{"project":"desktop","githubRepository":"acme/wenzwork-next","githubToken":"token-replacement","clearGithubToken":false,"mirrorBaseUrl":"https://mirror-next.example.test","expectedVersion":3}`
	router.ServeHTTP(sourceSettingsResponse, adminRequest(http.MethodPut, "/api/v1/admin/release-source-settings", sourceSettingsBody, csrfToken, csrfToken))
	if sourceSettingsResponse.Code != http.StatusOK || releases.sourceSettings.GitHubRepository != "acme/wenzwork-next" || releases.sourceSettings.MirrorBaseURL != "https://mirror-next.example.test" || !releases.sourceSettings.GitHubTokenConfigured || releases.sourceSettings.Version != 4 || releases.sourceInput.ActorUserID != authService.authenticated.User.ID || releases.sourceToken != "token-replacement" || strings.Contains(sourceSettingsResponse.Body.String(), "token-replacement") {
		t.Fatalf("source settings response = %d %s settings=%+v input=%+v", sourceSettingsResponse.Code, sourceSettingsResponse.Body.String(), releases.sourceSettings, releases.sourceInput)
	}

	updatedGitHubResponse := httptest.NewRecorder()
	router.ServeHTTP(updatedGitHubResponse, adminRequest(http.MethodGet, "/api/v1/admin/github-releases/latest", "", "", ""))
	if updatedGitHubResponse.Code != http.StatusOK || sources.repository != "acme/wenzwork-next" || sources.token != "token-replacement" {
		t.Fatalf("updated GitHub response = %d %s repository=%q token=%q", updatedGitHubResponse.Code, updatedGitHubResponse.Body.String(), sources.repository, sources.token)
	}

	sources.latestErr = releaseassets.ErrGitHubReleaseNotFound
	missingGitHubResponse := httptest.NewRecorder()
	router.ServeHTTP(missingGitHubResponse, adminRequest(http.MethodGet, "/api/v1/admin/github-releases/latest", "", "", ""))
	if missingGitHubResponse.Code != http.StatusNotFound || !strings.Contains(missingGitHubResponse.Body.String(), "acme/wenzwork-next") || strings.Contains(missingGitHubResponse.Body.String(), "请先在配置的仓库发布 Release") {
		t.Fatalf("missing GitHub response = %d %s", missingGitHubResponse.Code, missingGitHubResponse.Body.String())
	}

	releases.sourceToken = ""
	missingPrivateGitHubResponse := httptest.NewRecorder()
	router.ServeHTTP(missingPrivateGitHubResponse, adminRequest(http.MethodGet, "/api/v1/admin/github-releases/latest", "", "", ""))
	if missingPrivateGitHubResponse.Code != http.StatusNotFound || !strings.Contains(missingPrivateGitHubResponse.Body.String(), "发布设置中配置访问 Token") {
		t.Fatalf("missing private GitHub response = %d %s", missingPrivateGitHubResponse.Code, missingPrivateGitHubResponse.Body.String())
	}

	releases.sourceToken = "invalid-token"
	sources.latestErr = releaseassets.ErrGitHubAuthentication
	authenticationResponse := httptest.NewRecorder()
	router.ServeHTTP(authenticationResponse, adminRequest(http.MethodGet, "/api/v1/admin/github-releases/latest", "", "", ""))
	if authenticationResponse.Code != http.StatusBadGateway || !strings.Contains(authenticationResponse.Body.String(), `"code":"github_authentication_failed"`) {
		t.Fatalf("GitHub authentication response = %d %s", authenticationResponse.Code, authenticationResponse.Body.String())
	}

	settingsResponse := httptest.NewRecorder()
	settingsBody := `{"downloadMode":"s3_redirect","s3UrlPrefix":"https://cdn.example.test/files","expectedVersion":1}`
	router.ServeHTTP(settingsResponse, adminRequest(http.MethodPut, "/api/v1/admin/release-delivery-settings", settingsBody, csrfToken, csrfToken))
	if settingsResponse.Code != http.StatusOK || releases.settings.DownloadMode != catalog.ReleaseDownloadS3Redirect || releases.settings.Version != 2 {
		t.Fatalf("settings response = %d %s settings=%+v", settingsResponse.Code, settingsResponse.Body.String(), releases.settings)
	}
}

func TestAdminReleaseAccessKeySettingsRotateWithoutReturningPlaintext(t *testing.T) {
	actorID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"release_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	oldKey := "release_" + strings.Repeat("i", 43)
	newKey := "release_" + strings.Repeat("j", 43)
	accessKeys := &fakeReleaseAccessKeyService{
		key: oldKey,
		settings: catalog.ReleaseAccessKeySettings{
			AccessKeyConfigured: true, KeyPrefix: oldKey[:16], Version: 3, UpdatedAt: time.Now().UTC(),
		},
	}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		ReleaseAccessKeys: accessKeys,
		AuthHTTP:          AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, adminRequest(http.MethodGet, "/api/v1/admin/release-access-key-settings", "", "", ""))
	if getResponse.Code != http.StatusOK || !strings.Contains(getResponse.Body.String(), `"keyPrefix":"`+oldKey[:16]+`"`) || strings.Contains(getResponse.Body.String(), oldKey) {
		t.Fatalf("get access key settings response = %d %s", getResponse.Code, getResponse.Body.String())
	}

	updateResponse := httptest.NewRecorder()
	updateBody := `{"accessKey":"` + newKey + `","expectedVersion":3}`
	router.ServeHTTP(updateResponse, adminRequest(http.MethodPut, "/api/v1/admin/release-access-key-settings", updateBody, csrfToken, csrfToken))
	if updateResponse.Code != http.StatusOK || accessKeys.input.ActorUserID != actorID || accessKeys.input.AccessKey != newKey || accessKeys.settings.Version != 4 || strings.Contains(updateResponse.Body.String(), newKey) {
		t.Fatalf("update access key settings response = %d %s input=%+v", updateResponse.Code, updateResponse.Body.String(), accessKeys.input)
	}
	if valid, verifyErr := accessKeys.VerifyReleaseAccessKey(context.Background(), oldKey); verifyErr != nil || valid {
		t.Fatalf("old key remained valid after rotation: valid=%v error=%v", valid, verifyErr)
	}
	if valid, verifyErr := accessKeys.VerifyReleaseAccessKey(context.Background(), newKey); verifyErr != nil || !valid {
		t.Fatalf("new key was not active after rotation: valid=%v error=%v", valid, verifyErr)
	}
}

func TestAdminPricingRoutesRequireContentPermissionConfirmationAndDispatchVersion(t *testing.T) {
	actorID := uuid.New()
	planID := uuid.New()
	csrfToken, csrfHash, err := auth.NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	authService := &fakeAuthService{authenticated: auth.AuthenticatedSession{
		ID: uuid.New(), User: auth.User{ID: actorID, Status: "active", Roles: []string{"content_admin"}},
		CSRFTokenHash: csrfHash, AssuranceLevel: 2, AbsoluteExpiresAt: time.Now().Add(time.Hour),
	}}
	pricing := &fakeAdminPricingService{plans: []catalog.AdminPricingPlan{{
		ID: planID, Code: "pro", Name: "Pro", Status: "published", Version: 3, Features: []string{"Fast"}, RemoteAccessEnabled: true, DeviceLimit: 10,
	}}}
	router := NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService, PricingAdmin: pricing,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})

	listResponse := httptest.NewRecorder()
	router.ServeHTTP(listResponse, adminRequest(http.MethodGet, "/api/v1/admin/pricing-plans", "", "", ""))
	if listResponse.Code != http.StatusOK || !strings.Contains(listResponse.Body.String(), `"code":"pro"`) {
		t.Fatalf("pricing list response = %d %s", listResponse.Code, listResponse.Body.String())
	}

	updateResponse := httptest.NewRecorder()
	updateBody := `{"code":"pro","name":"Pro Plus","description":"Updated","priceMinor":12800,"originalPriceMinor":16800,"currency":"CNY","billingPeriod":"year","features":["Fast"],"remoteAccessEnabled":true,"deviceLimit":24,"monthlyTrafficLimitGb":100,"sortOrder":20,"expectedVersion":3,"confirmPriceChange":true}`
	router.ServeHTTP(updateResponse, adminRequest(http.MethodPut, "/api/v1/admin/pricing-plans/"+planID.String(), updateBody, csrfToken, csrfToken))
	if updateResponse.Code != http.StatusOK || pricing.planID != planID || pricing.saveInput.ActorUserID != actorID ||
		pricing.saveInput.ExpectedVersion != 3 || pricing.saveInput.OriginalPriceMinor == nil ||
		*pricing.saveInput.OriginalPriceMinor != 16800 || !pricing.saveInput.ConfirmPriceChange ||
		!pricing.saveInput.RemoteAccessEnabled || pricing.saveInput.DeviceLimit != 24 ||
		pricing.saveInput.MonthlyTrafficLimitGB == nil || *pricing.saveInput.MonthlyTrafficLimitGB != 100 {
		t.Fatalf("pricing update response = %d %s input=%+v", updateResponse.Code, updateResponse.Body.String(), pricing.saveInput)
	}

	publishResponse := httptest.NewRecorder()
	router.ServeHTTP(publishResponse, adminRequest(http.MethodPost, "/api/v1/admin/pricing-plans/"+planID.String()+"/publish", `{"expectedVersion":4,"confirm":true}`, csrfToken, csrfToken))
	if publishResponse.Code != http.StatusOK || pricing.actionInput.ActorUserID != actorID || pricing.actionInput.ExpectedVersion != 4 || !pricing.actionInput.Confirm {
		t.Fatalf("pricing publish response = %d %s input=%+v", publishResponse.Code, publishResponse.Body.String(), pricing.actionInput)
	}

	missingConfirmation := httptest.NewRecorder()
	router.ServeHTTP(missingConfirmation, adminRequest(http.MethodPost, "/api/v1/admin/pricing-plans/"+planID.String()+"/publish", `{"expectedVersion":4,"confirm":false}`, csrfToken, csrfToken))
	if missingConfirmation.Code != http.StatusBadRequest || !strings.Contains(missingConfirmation.Body.String(), `"code":"pricing_plan_confirmation_required"`) {
		t.Fatalf("missing confirmation response = %d %s", missingConfirmation.Code, missingConfirmation.Body.String())
	}

	releaseResponse := httptest.NewRecorder()
	router.ServeHTTP(releaseResponse, adminRequest(http.MethodPost, "/api/v1/admin/releases", `{}`, csrfToken, csrfToken))
	if releaseResponse.Code != http.StatusForbidden {
		t.Fatalf("content administrator accessed releases: %d %s", releaseResponse.Code, releaseResponse.Body.String())
	}
}

func adminTestRouter(authService AuthService, users AdminUserService, memberships AdminMembershipService, releases AdminReleaseService) http.Handler {
	return NewRouter(Dependencies{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: authService,
		UserAdmin: users, MembershipAdmin: memberships, CatalogAdmin: releases,
		AuthHTTP: AuthHTTPConfig{AllowedOrigins: []string{"http://localhost:5173"}},
	})
}

func adminRequest(method, path, body, csrfCookie, csrfHeader string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	if csrfHeader != "" {
		request.Header.Set("X-CSRF-Token", csrfHeader)
	}
	request.AddCookie(&http.Cookie{Name: "wenzwork_session", Value: "session"})
	if csrfCookie != "" {
		request.AddCookie(&http.Cookie{Name: "wenzwork_csrf", Value: csrfCookie})
	}
	return request
}
