//go:build integration

package remotecontrol

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/database"
)

type integrationPeerIssuer struct{ inputs []PeerIssueInput }

func (issuer *integrationPeerIssuer) IssueBrowserPeer(_ context.Context, input PeerIssueInput) (PeerSession, error) {
	issuer.inputs = append(issuer.inputs, input)
	return PeerSession{SessionID: uuid.New(), TargetKeyThumbprint: input.TargetKeyThumbprint, TargetKeyVersion: input.TargetKeyVersion}, nil
}

func TestRemoteControlLifecyclePersistsOnlyBoundedProjectionData(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	now := time.Now().UTC().Truncate(time.Microsecond)
	peerIssuer := &integrationPeerIssuer{}
	service, err := NewService(ServiceConfig{
		Database: db, CursorKey: []byte("integration-cursor-secret-123456789"), PeerIssuer: peerIssuer, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	userID, appSessionID, browserSessionID, deviceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	devicePublic, _, _ := ed25519.GenerateKey(rand.Reader)
	_, deviceThumbprint, _ := decodePublicKey(base64.RawURLEncoding.EncodeToString(devicePublic))
	suffix := uuid.NewString()
	if err := db.Exec(`
		INSERT INTO users (id, email, password_hash, display_name, status, email_verified_at)
		VALUES (?, ?, 'integration-only', 'Remote Control User', 'active', now())`,
		userID, "remote-control-"+suffix+"@example.test").Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO app_sessions (id, user_id, client_id, device_id, device_name, scope, last_seen_at, idle_expires_at)
		VALUES (?, ?, 'wenzwork-desktop', ?, 'integration-device', 'profile.read membership.read remote.connect', now(), now() + interval '1 hour')`,
		appSessionID, userID, deviceID).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO sessions (id, user_id, token_hash, csrf_token_hash, last_seen_at, idle_expires_at, absolute_expires_at)
		VALUES (?, ?, ?, ?, now(), now() + interval '1 hour', now() + interval '1 day')`,
		browserSessionID, userID,
		strings.Repeat("a", 32)+strings.ReplaceAll(browserSessionID.String(), "-", ""),
		strings.Repeat("b", 32)+strings.ReplaceAll(browserSessionID.String(), "-", "")).Error; err != nil {
		t.Fatalf("seed browser session: %v", err)
	}
	if err := db.Exec(`
		INSERT INTO remote_device_credentials
		    (device_id, user_id, registered_session_id, device_name, platform, agent_version,
		     protocol_min, protocol_max, capabilities, identity_public_key, public_key_thumbprint,
		     grant_version, status, last_connection_epoch, created_at, updated_at)
		VALUES (?, ?, ?, 'integration-device', 'linux', 'integration', 1, 1,
		        '["relay.ping"]'::jsonb, ?, ?, 1, 'active', 0, ?, ?)`,
		deviceID, userID, appSessionID, []byte(devicePublic), deviceThumbprint, now, now).Error; err != nil {
		t.Fatalf("seed device credential: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Exec("DELETE FROM relay_outbox WHERE aggregate_id = ?", deviceID).Error
		_ = db.Exec("DELETE FROM remote_controller_identities WHERE user_id = ?", userID).Error
		_ = db.Exec("DELETE FROM remote_device_credentials WHERE device_id = ?", deviceID).Error
		_ = db.Exec("DELETE FROM sessions WHERE id = ?", browserSessionID).Error
		_ = db.Exec("DELETE FROM app_sessions WHERE id = ?", appSessionID).Error
		_ = db.Exec("DELETE FROM users WHERE id = ?", userID).Error
	})

	access, err := service.EnableAccess(ctx, AccessInput{
		UserID: userID, DeviceID: deviceID, Confirmation: "enable_remote_access", IdempotencyKey: "enable-" + suffix,
	})
	if err != nil || access.GrantVersion != 2 || access.Status != "enabled" {
		t.Fatalf("EnableAccess() = %+v, %v", access, err)
	}
	replayedAccess, err := service.EnableAccess(ctx, AccessInput{
		UserID: userID, DeviceID: deviceID, Confirmation: "enable_remote_access", IdempotencyKey: "enable-" + suffix,
	})
	if err != nil || !replayedAccess.Replayed || replayedAccess.GrantVersion != access.GrantVersion {
		t.Fatalf("replayed EnableAccess() = %+v, %v", replayedAccess, err)
	}

	projectID := uuid.New()
	changeResult, err := service.PushChanges(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, PushChangesInput{
		BaseHighWatermark: 0,
		Changes: []DeviceChange{{
			Sequence: 1, Kind: "project", Operation: "upsert", ResourceID: projectID, Revision: 1,
			OccurredAt: now, DisplayName: "Integration Project", Capabilities: []string{"project.read"}, State: "available",
		}},
	})
	if err != nil || changeResult.HighWatermark != 1 || changeResult.Applied != 1 {
		t.Fatalf("PushChanges() = %+v, %v", changeResult, err)
	}
	projects, err := service.ListProjects(ctx, userID, deviceID, PageRequest{Limit: 10})
	if err != nil || len(projects.Items) != 1 || projects.Items[0].ID != projectID || projects.HighWatermark != 1 {
		t.Fatalf("ListProjects() = %+v, %v", projects, err)
	}

	if _, _, err := service.CreateTask(ctx, CreateTaskInput{
		UserID: userID, DeviceID: deviceID, ProjectID: &projectID, TaskType: "project.index", Title: "must reject",
		Input: json.RawMessage(`{"prompt":"must never reach cloud storage"}`), IdempotencyKey: "sensitive-" + suffix,
	}); !errors.Is(err, ErrPeerRequired) {
		t.Fatalf("sensitive CreateTask error = %v", err)
	}
	if _, _, err := service.CreateTask(ctx, CreateTaskInput{
		UserID: userID, DeviceID: deviceID, ProjectID: &projectID, TaskType: "workspace.inspect", Title: "Safe-looking task",
		Input: json.RawMessage(`{}`), IdempotencyKey: "safe-create-" + suffix,
	}); !errors.Is(err, ErrPeerRequired) {
		t.Fatalf("safe-looking cloud CreateTask error = %v", err)
	}

	taskID := uuid.New()
	changeResult, err = service.PushChanges(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, PushChangesInput{
		BaseHighWatermark: 1,
		Changes: []DeviceChange{{
			Sequence: 2, Kind: "task", Operation: "upsert", ResourceID: taskID, Revision: 1,
			OccurredAt: now, ProjectID: &projectID, TaskType: "codex", Title: TaskProjectionDisplayName("codex"), Status: "queued",
		}},
	})
	if err != nil || changeResult.HighWatermark != 2 || changeResult.Applied != 1 {
		t.Fatalf("PushChanges(task projection) = %+v, %v", changeResult, err)
	}
	task, err := service.GetTask(ctx, userID, taskID)
	if err != nil || task.Title != "Codex task" || task.Status != "queued" {
		t.Fatalf("GetTask(projection) = %+v, %v", task, err)
	}

	currentTaskPage, err := service.ListTasks(ctx, userID, deviceID, PageRequest{Limit: 20})
	if err != nil || len(currentTaskPage.Items) != 1 || currentTaskPage.HighWatermark == 0 {
		t.Fatalf("current task cache page = %+v, %v", currentTaskPage, err)
	}
	afterCurrent := currentTaskPage.HighWatermark
	unchangedTasks, err := service.ListTasks(ctx, userID, deviceID, PageRequest{Limit: 20, AfterRevision: &afterCurrent})
	if err != nil || len(unchangedTasks.Items) != 0 || unchangedTasks.ResetRequired || unchangedTasks.HighWatermark != afterCurrent {
		t.Fatalf("unchanged task cache page = %+v, %v", unchangedTasks, err)
	}
	afterStale := afterCurrent - 1
	changedTasks, err := service.ListTasks(ctx, userID, deviceID, PageRequest{Limit: 20, AfterRevision: &afterStale})
	if err != nil || len(changedTasks.Items) != 1 || !changedTasks.ResetRequired || changedTasks.HighWatermark != afterCurrent {
		t.Fatalf("changed task cache page = %+v, %v", changedTasks, err)
	}
	secondTaskID := uuid.New()
	changeResult, err = service.PushChanges(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, PushChangesInput{
		BaseHighWatermark: 2,
		Changes: []DeviceChange{{
			Sequence: 3, Kind: "task", Operation: "upsert", ResourceID: secondTaskID, Revision: 1,
			OccurredAt: now, ProjectID: &projectID, TaskType: "workflow", Title: TaskProjectionDisplayName("workflow"), Status: "queued",
		}},
	})
	if err != nil || changeResult.HighWatermark != 3 {
		t.Fatalf("PushChanges(second task) = %+v, %v", changeResult, err)
	}
	newTaskPage, err := service.ListTasks(ctx, userID, deviceID, PageRequest{Limit: 20, AfterRevision: &afterCurrent})
	if err != nil || !newTaskPage.ResetRequired || newTaskPage.HighWatermark <= afterCurrent || len(newTaskPage.Items) != 2 {
		t.Fatalf("new low-revision task cache page = %+v, %v", newTaskPage, err)
	}

	operation, err := service.CancelTask(ctx, CancelTaskInput{UserID: userID, TaskID: taskID, IdempotencyKey: "task-cancel-" + suffix})
	if err != nil || operation.ID == uuid.Nil {
		t.Fatalf("CancelTask() = %+v, %v", operation, err)
	}
	commands, err := service.PollCommands(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, 20)
	var cancelBody struct {
		TaskID uuid.UUID `json:"taskId"`
	}
	if err != nil || len(commands.Items) != 1 || commands.Items[0].ID != operation.ID || commands.Items[0].Kind != "task.cancel" ||
		json.Unmarshal(commands.Items[0].Body, &cancelBody) != nil || cancelBody.TaskID != taskID {
		t.Fatalf("metadata-only cancel command = %+v, %v", commands, err)
	}
	if err := service.AckCommand(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, operation.ID,
		AckCommandInput{LeaseToken: commands.Items[0].LeaseToken, Status: "accepted"}); err != nil {
		t.Fatalf("cancel accepted ACK error = %v", err)
	}
	cancelRequested, err := service.GetTask(ctx, userID, taskID)
	if err != nil || cancelRequested.Status != "cancel_requested" {
		t.Fatalf("task after cancel ACK = %+v, %v", cancelRequested, err)
	}
	// Accepted cancellation remains recoverable after an Agent restart.
	now = now.Add(DefaultCommandLease + time.Second)
	released, err := service.PollCommands(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, 20)
	if err != nil || len(released.Items) != 1 || released.Items[0].ID != operation.ID || released.Items[0].LeaseToken == commands.Items[0].LeaseToken {
		t.Fatalf("accepted cancellation re-lease = %+v, %v", released, err)
	}
	if err := service.AckCommand(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, operation.ID,
		AckCommandInput{LeaseToken: commands.Items[0].LeaseToken, Status: "completed"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale cancel lease completion error = %v", err)
	}
	if err := service.AckCommand(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, operation.ID,
		AckCommandInput{LeaseToken: released.Items[0].LeaseToken, Status: "completed"}); err != nil {
		t.Fatalf("completed cancellation ACK error = %v", err)
	}
	changeResult, err = service.PushChanges(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, PushChangesInput{
		BaseHighWatermark: 3,
		Changes: []DeviceChange{{
			Sequence: 4, Kind: "task", Operation: "upsert", ResourceID: taskID, Revision: cancelRequested.Revision,
			OccurredAt: now, ProjectID: &projectID, TaskType: "codex", Title: TaskProjectionDisplayName("codex"), Status: "cancelled",
		}},
	})
	if err != nil || changeResult.HighWatermark != 4 {
		t.Fatalf("PushChanges(cancelled projection) = %+v, %v", changeResult, err)
	}
	cancelledTask, err := service.GetTask(ctx, userID, taskID)
	if err != nil || cancelledTask.Status != "cancelled" {
		t.Fatalf("cancelled projection = %+v, %v", cancelledTask, err)
	}

	changeResult, err = service.PushChanges(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, PushChangesInput{
		BaseHighWatermark: 4,
		Changes: []DeviceChange{{
			Sequence: 5, Kind: "project", Operation: "tombstone", ResourceID: projectID, Revision: 2,
			OccurredAt: now,
		}},
	})
	if err != nil || changeResult.HighWatermark != 5 || changeResult.Applied != 1 {
		t.Fatalf("PushChanges(project tombstone) = %+v, %v", changeResult, err)
	}
	projects, err = service.ListProjects(ctx, userID, deviceID, PageRequest{Limit: 10})
	if err != nil || len(projects.Items) != 0 || projects.HighWatermark != 5 {
		t.Fatalf("ListProjects(after tombstone) = %+v, %v", projects, err)
	}

	changeResult, err = service.PushChanges(ctx, DevicePrincipal{UserID: userID, DeviceID: deviceID}, PushChangesInput{
		BaseHighWatermark: 5,
		Changes: []DeviceChange{{
			Sequence: 6, Kind: "project", Operation: "upsert", ResourceID: projectID, Revision: 3,
			OccurredAt: now, DisplayName: "Restored Integration Project", Capabilities: []string{"project.read"}, State: "available",
		}},
	})
	if err != nil || changeResult.HighWatermark != 6 || changeResult.Applied != 1 {
		t.Fatalf("PushChanges(restored project) = %+v, %v", changeResult, err)
	}
	projects, err = service.ListProjects(ctx, userID, deviceID, PageRequest{Limit: 10})
	if err != nil || len(projects.Items) != 1 || projects.Items[0].ID != projectID ||
		projects.Items[0].DisplayName != "Restored Integration Project" || projects.Items[0].Revision != 3 || projects.HighWatermark != 6 {
		t.Fatalf("ListProjects(after restore) = %+v, %v", projects, err)
	}

	controllerID := uuid.New()
	controllerPublic, controllerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	encodedControllerKey := base64.RawURLEncoding.EncodeToString(controllerPublic)
	proof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(controllerPrivate,
		controllerProofTranscript(userID, controllerID, encodedControllerKey, 1)))
	controller, err := service.RegisterController(ctx, RegisterControllerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, IdentityPublicKey: encodedControllerKey,
		Proof: proof, Scopes: []string{"remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.file.send", "remote.peer.file.receive"},
		IdempotencyKey: "controller-" + suffix,
	})
	if err != nil || controller.KeyVersion != 1 || controller.IdentityPublicKey != encodedControllerKey {
		t.Fatalf("RegisterController() = %+v, %v", controller, err)
	}
	var registeredSessionText string
	if err := db.Table("remote_controller_identities").Select("registered_session_id").
		Where("controller_id = ?", controllerID).Scan(&registeredSessionText).Error; err != nil {
		t.Fatalf("read controller browser session reference: %v", err)
	}
	registeredSessionID, err := uuid.Parse(registeredSessionText)
	if err != nil || registeredSessionID != browserSessionID {
		t.Fatalf("controller browser session reference = %q (%v); want %s", registeredSessionText, err, browserSessionID)
	}
	nativeControllerID := uuid.New()
	nativePublic, nativePrivate, _ := ed25519.GenerateKey(rand.Reader)
	encodedNativeKey := base64.RawURLEncoding.EncodeToString(nativePublic)
	nativeProof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(nativePrivate,
		controllerProofTranscript(userID, nativeControllerID, encodedNativeKey, 1)))
	if _, err := service.RegisterController(ctx, RegisterControllerInput{
		UserID: userID, ControllerID: nativeControllerID, IdentityPublicKey: encodedNativeKey,
		Proof: nativeProof, Scopes: []string{"remote.peer.query"}, IdempotencyKey: "native-controller-" + suffix,
	}); err != nil {
		t.Fatalf("RegisterController(native) = %v", err)
	}
	var nativeRegistrations int64
	if err := db.Table("remote_controller_identities").Where("controller_id = ? AND registered_session_id IS NULL", nativeControllerID).
		Count(&nativeRegistrations).Error; err != nil || nativeRegistrations != 1 {
		t.Fatalf("native controller browser-session reference = %d, %v", nativeRegistrations, err)
	}
	newPublic, newPrivate, _ := ed25519.GenerateKey(rand.Reader)
	encodedNewKey := base64.RawURLEncoding.EncodeToString(newPublic)
	rotationProof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(newPrivate,
		controllerProofTranscript(userID, controllerID, encodedNewKey, 2)))
	rotated, err := service.RotateController(ctx, RotateControllerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, IdentityPublicKey: encodedNewKey,
		Proof: rotationProof, IdempotencyKey: "rotate-controller-" + suffix,
	})
	if err != nil || rotated.KeyVersion != 2 || rotated.GrantVersion != 2 {
		t.Fatalf("RotateController() = %+v, %v", rotated, err)
	}
	replayedRotation, err := service.RotateController(ctx, RotateControllerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, IdentityPublicKey: encodedNewKey,
		Proof: rotationProof, IdempotencyKey: "rotate-controller-" + suffix,
	})
	if err != nil || replayedRotation.KeyVersion != 2 {
		t.Fatalf("replayed RotateController() = %+v, %v", replayedRotation, err)
	}
	for index, scope := range []string{"remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.terminal", "remote.peer.file.send", "remote.peer.file.receive"} {
		var boundProjectID *uuid.UUID
		if peerScopeRequiresProject(scope) {
			value := projectID
			boundProjectID = &value
		}
		if _, err := service.IssueBrowserPeer(ctx, BrowserPeerInput{
			UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, TargetDeviceID: deviceID,
			Scope: scope, ProjectID: boundProjectID, IdempotencyKey: "browser-peer-" + string(rune('a'+index)) + "-" + suffix,
		}); err != nil {
			t.Fatalf("IssueBrowserPeer(%s) error = %v", scope, err)
		}
	}
	if len(peerIssuer.inputs) != 6 {
		t.Fatalf("Peer issuer call count = %d", len(peerIssuer.inputs))
	}
	for index, input := range peerIssuer.inputs {
		if input.Scope == "" || input.ControllerID != controllerID || input.TargetDeviceID != deviceID ||
			input.ControllerKeyVersion != 2 || input.ControllerGrantVersion != 2 || input.TargetKeyVersion != 1 || input.TargetGrantVersion != 2 ||
			(peerScopeRequiresProject(input.Scope) && (input.ProjectID == nil || *input.ProjectID != projectID)) ||
			(!peerScopeRequiresProject(input.Scope) && input.ProjectID != nil) {
			t.Errorf("Peer issuer input[%d] = %+v", index, input)
		}
	}
	if _, err := service.IssueBrowserPeer(ctx, BrowserPeerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, TargetDeviceID: deviceID,
		Scope: "remote.peer.file.receive", IdempotencyKey: "missing-project-" + suffix,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("project-scoped session without project error = %v", err)
	}
	unknownProjectID := uuid.New()
	if _, err := service.IssueBrowserPeer(ctx, BrowserPeerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, TargetDeviceID: deviceID,
		Scope: "remote.peer.file.receive", ProjectID: &unknownProjectID, IdempotencyKey: "unknown-project-" + suffix,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project session error = %v", err)
	}
	if _, err := service.IssueBrowserPeer(ctx, BrowserPeerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, TargetDeviceID: deviceID,
		Scope: "remote.peer.query", ProjectID: &projectID, IdempotencyKey: "device-query-project-" + suffix,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("device-scoped session with project error = %v", err)
	}
	if len(peerIssuer.inputs) != 6 {
		t.Fatalf("issuer called for invalid project binding")
	}

	limitedControllerID := uuid.New()
	limitedPublic, limitedPrivate, _ := ed25519.GenerateKey(rand.Reader)
	encodedLimitedKey := base64.RawURLEncoding.EncodeToString(limitedPublic)
	limitedProof := base64.RawURLEncoding.EncodeToString(ed25519.Sign(limitedPrivate,
		controllerProofTranscript(userID, limitedControllerID, encodedLimitedKey, 1)))
	if _, err := service.RegisterController(ctx, RegisterControllerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: limitedControllerID, IdentityPublicKey: encodedLimitedKey,
		Proof: limitedProof, Scopes: []string{"remote.peer.query"}, IdempotencyKey: "limited-controller-" + suffix,
	}); err != nil {
		t.Fatalf("RegisterController(limited) error = %v", err)
	}
	if _, err := service.IssueBrowserPeer(ctx, BrowserPeerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: limitedControllerID, TargetDeviceID: deviceID,
		Scope: "remote.peer.ai.chat", ProjectID: &projectID, IdempotencyKey: "limited-peer-" + suffix,
	}); err != nil {
		t.Fatalf("limited controller must use the encrypted Peer connection without a scope grant: %v", err)
	}
	if len(peerIssuer.inputs) != 7 {
		t.Fatalf("Peer issuer call count after limited controller = %d", len(peerIssuer.inputs))
	}
	if err := db.Table("remote_access_grants").Where("device_id = ?", deviceID).
		Update("scopes", `["remote.peer.query"]`).Error; err != nil {
		t.Fatalf("narrow target grant: %v", err)
	}
	if _, err := service.IssueBrowserPeer(ctx, BrowserPeerInput{
		UserID: userID, SessionID: browserSessionID, ControllerID: controllerID, TargetDeviceID: deviceID,
		Scope: "remote.peer.file.receive", ProjectID: &projectID, IdempotencyKey: "target-limited-" + suffix,
	}); err != nil {
		t.Fatalf("enabled target must use the encrypted Peer connection without a scope grant: %v", err)
	}
	if len(peerIssuer.inputs) != 8 {
		t.Fatalf("Peer issuer call count after narrowed target grant = %d", len(peerIssuer.inputs))
	}
}
