package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestRemoteProjectCreateIsDeviceScopedAndSupportsSelectedDirectories(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	now := time.Now().UTC()
	if !agentFeatureFlags(state)["project.remoteCreate"] || !agentFeatureFlags(state)["project.remoteRoots"] {
		t.Fatal("device-level project creation capabilities are disabled")
	}
	browser := projectDirectoryBrowserFor(state)
	parent, err := browser.issue(directory, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: state, now: func() time.Time { return now }, scope: "remote.peer.query",
		enforceProjectBinding: true,
	}

	response := dispatchProjectRPC(t, dispatch, "project.create", "", mustProjectJSON(t, map[string]any{
		"name": "child", "displayName": "Child Project", "parentDirectoryId": parent.ID,
		"gitUrl": "https://example.test/team/child.git",
	}))
	if response.GetError() != nil {
		t.Fatalf("project.create error = %+v", response.GetError())
	}
	if bytes.Contains(response.GetJsonPayload(), []byte(directory)) || bytes.Contains(response.GetJsonPayload(), []byte(`"path"`)) {
		t.Fatalf("project.create leaked a local path: %s", response.GetJsonPayload())
	}
	var created map[string]any
	if err := json.Unmarshal(response.GetJsonPayload(), &created); err != nil {
		t.Fatal(err)
	}
	createdID, err := uuid.Parse(created["id"].(string))
	if err != nil || createdID == uuid.Nil || created["displayName"] != "Child Project" {
		t.Fatalf("created project = %#v", created)
	}
	childPath := filepath.Join(directory, "child")
	if info, err := os.Stat(childPath); err != nil || !info.IsDir() {
		t.Fatalf("created directory = %v, %v", info, err)
	}
	registered, err := state.business.projectByID(t.Context(), createdID)
	if err != nil || registered.LocalPath != childPath || registered.Policy != defaultProjectPolicy {
		t.Fatalf("registered project = %+v, %v", registered, err)
	}

	existingPath := filepath.Join(directory, "existing")
	if err := os.Mkdir(existingPath, 0o700); err != nil {
		t.Fatal(err)
	}
	existing, err := browser.issue(existingPath, now)
	if err != nil {
		t.Fatal(err)
	}
	registeredExisting := dispatchProjectRPC(t, dispatch, "project.create", "", mustProjectJSON(t, map[string]any{
		"name": "existing", "directoryId": existing.ID,
	}))
	if registeredExisting.GetError() != nil {
		t.Fatalf("register existing directory error = %+v", registeredExisting.GetError())
	}

	forged := dispatchProjectRPC(t, dispatch, "project.create", uuid.NewString(), mustProjectJSON(t, map[string]any{
		"name": "forged", "parentDirectoryId": parent.ID,
	}))
	if forged.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH {
		t.Fatalf("project-bound create error = %+v", forged.GetError())
	}
}

func TestRemoteProjectCRUDAllowsThreeCreatesImmediateRemoveAndRestore(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	controlStore, err := loadControlState(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	dispatch := dispatcher{
		state: state, controlStore: controlStore, now: func() time.Time { return now },
		scope: "remote.peer.query", enforceProjectBinding: true,
	}
	type projectMutation struct {
		ID       uuid.UUID `json:"id"`
		Revision uint64    `json:"revision"`
	}

	browser := projectDirectoryBrowserFor(state)
	created := make([]projectMutation, 0, 3)
	projectRoots := make([]string, 0, 3)
	var lastHandle remoteProjectDirectory
	for index := 0; index < 3; index++ {
		root := t.TempDir()
		handle, err := browser.issue(root, now)
		if err != nil {
			t.Fatal(err)
		}
		response := dispatchProjectRPC(t, dispatch, "project.create", "", mustProjectJSON(t, map[string]any{
			"name": "project-" + string(rune('a'+index)), "directoryId": handle.ID,
		}))
		if response.GetError() != nil {
			t.Fatalf("project.create #%d error = %+v", index+1, response.GetError())
		}
		var result projectMutation
		if err := json.Unmarshal(response.GetJsonPayload(), &result); err != nil || result.ID == uuid.Nil || result.Revision == 0 {
			t.Fatalf("project.create #%d result = %+v, %v", index+1, result, err)
		}
		for _, previous := range created {
			if previous.ID == result.ID {
				t.Fatalf("project.create #%d reused active project ID %s", index+1, result.ID)
			}
		}
		created = append(created, result)
		projectRoots = append(projectRoots, root)
		lastHandle = handle
	}

	// E2EE creation must synchronously populate the Agent-local projection, so
	// the returned revision is immediately valid even though cloud delivery is
	// still asynchronous.
	before, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	projected, projectedExists := before.Sync.Projects[created[2].ID.String()]
	if !projectedExists || projected.Revision != created[2].Revision || projected.RegistryRevision == 0 {
		t.Fatalf("newly created project projection = %+v, exists = %t", projected, projectedExists)
	}
	removed := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": created[2].ID, "expectedRevision": created[2].Revision,
	}))
	if removed.GetError() != nil {
		t.Fatalf("immediate project.remove error = %+v", removed.GetError())
	}
	if info, err := os.Stat(projectRoots[2]); err != nil || !info.IsDir() {
		t.Fatalf("project.remove changed the selected directory: %v, %v", info, err)
	}

	// Re-registering the same directory intentionally restores the stable ID,
	// but advances the authoritative registry generation.
	restoredResponse := dispatchProjectRPC(t, dispatch, "project.create", "", mustProjectJSON(t, map[string]any{
		"name": "project-c-restored", "directoryId": lastHandle.ID,
	}))
	if restoredResponse.GetError() != nil {
		t.Fatalf("restored project.create error = %+v", restoredResponse.GetError())
	}
	var restored projectMutation
	if err := json.Unmarshal(restoredResponse.GetJsonPayload(), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ID != created[2].ID || restored.Revision <= created[2].Revision+1 {
		t.Fatalf("restored project = %+v, original = %+v", restored, created[2])
	}

	staleRemove := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": restored.ID, "expectedRevision": created[2].Revision,
	}))
	if staleRemove.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT {
		t.Fatalf("stale restored project.remove error = %+v", staleRemove.GetError())
	}
	for index := 0; index < 3; index++ {
		if _, err := reconcileWorkspaceProjects(t.Context(), state, controlStore, now.Add(time.Duration(index+1)*time.Second), true, nil); err != nil {
			t.Fatal(err)
		}
	}
	refreshedSnapshot, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	refreshed := refreshedSnapshot.Sync.Projects[restored.ID.String()]
	if refreshed.Revision <= restored.Revision {
		t.Fatalf("forced restored projection did not advance: %+v", refreshed)
	}
	currentRemove := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": restored.ID, "expectedRevision": refreshed.Revision,
	}))
	if currentRemove.GetError() != nil {
		t.Fatalf("restored project.remove error = %+v", currentRemove.GetError())
	}

	// A later restore must advance beyond the prior projection tombstone even
	// when the independent registry revision is numerically lower.
	removedSnapshot, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	removedProjectionRevision, found := projectTombstoneRevision(removedSnapshot, restored.ID, refreshed.Revision)
	if !found {
		t.Fatalf("restored project tombstone was not recorded: %+v", removedSnapshot.ProjectChanges)
	}
	secondRestoreResponse := dispatchProjectRPC(t, dispatch, "project.create", "", mustProjectJSON(t, map[string]any{
		"name": "project-c-restored-again", "directoryId": lastHandle.ID,
	}))
	if secondRestoreResponse.GetError() != nil {
		t.Fatalf("second restored project.create error = %+v", secondRestoreResponse.GetError())
	}
	var secondRestore projectMutation
	if err := json.Unmarshal(secondRestoreResponse.GetJsonPayload(), &secondRestore); err != nil {
		t.Fatal(err)
	}
	if secondRestore.ID != restored.ID || secondRestore.Revision <= removedProjectionRevision {
		t.Fatalf("second restored project = %+v, tombstone revision = %d", secondRestore, removedProjectionRevision)
	}
}

func TestProjectProjectionRegistryRevisionMigratesWithoutPublicChange(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	project, err := state.business.addProject(t.Context(), t.TempDir(), "Migration", "", defaultProjectPolicy)
	if err != nil {
		t.Fatal(err)
	}
	controlStore, err := loadControlState(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := reconcileWorkspaceProjects(t.Context(), state, controlStore, now, false, nil); err != nil {
		t.Fatal(err)
	}
	before, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	projected := before.Sync.Projects[project.ID.String()]
	if projected.RegistryRevision != project.Revision {
		t.Fatalf("initial registry revision mapping = %+v", projected)
	}
	if err := controlStore.update(func(persisted *controlPersistentState) error {
		legacy := persisted.Sync.Projects[project.ID.String()]
		legacy.RegistryRevision = 0
		persisted.Sync.Projects[project.ID.String()] = legacy
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileWorkspaceProjects(t.Context(), state, controlStore, now.Add(time.Second), false, nil); err != nil {
		t.Fatal(err)
	}
	after, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	migrated := after.Sync.Projects[project.ID.String()]
	if migrated.RegistryRevision != project.Revision || migrated.Revision != projected.Revision || len(after.Sync.Pending) != len(before.Sync.Pending) {
		t.Fatalf("migrated projection = %+v; before = %+v", migrated, projected)
	}
}

func TestRemoteProjectDirectoryListReturnsOpaqueNavigableHandles(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	if err := os.Mkdir(filepath.Join(directory, "visible"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	browser := projectDirectoryBrowserFor(state)
	current, err := browser.issue(directory, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.query", enforceProjectBinding: true}
	roots := dispatchProjectRPC(t, dispatch, "project.directory.list", "", []byte(`{}`))
	if roots.GetError() != nil || bytes.Contains(roots.GetJsonPayload(), []byte(`"path"`)) {
		t.Fatalf("project directory roots = %s, %+v", roots.GetJsonPayload(), roots.GetError())
	}
	response := dispatchProjectRPC(t, dispatch, "project.directory.list", "", mustProjectJSON(t, map[string]any{"directoryId": current.ID}))
	if response.GetError() != nil {
		t.Fatalf("project.directory.list error = %+v", response.GetError())
	}
	if bytes.Contains(response.GetJsonPayload(), []byte(directory)) || bytes.Contains(response.GetJsonPayload(), []byte(`"path"`)) {
		t.Fatalf("directory list leaked a local path: %s", response.GetJsonPayload())
	}
	var page struct {
		Current remoteProjectDirectory   `json:"current"`
		Parent  *remoteProjectDirectory  `json:"parent"`
		Items   []remoteProjectDirectory `json:"items"`
	}
	if err := json.Unmarshal(response.GetJsonPayload(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Current.ID != current.ID || page.Current.Name == "" || page.Parent == nil {
		t.Fatalf("directory page = %+v", page)
	}
	found := false
	for _, item := range page.Items {
		if item.Name == "visible" {
			found = true
			if _, err := browser.resolve(item.ID, now); err != nil {
				t.Fatalf("child handle cannot be resolved: %v", err)
			}
		}
	}
	if !found {
		t.Fatalf("directory page omitted visible child: %+v", page.Items)
	}
	if _, err := browser.resolve(current.ID, now.Add(projectDirectoryHandleTTL+time.Second)); err != errRPCNotFound {
		t.Fatalf("expired directory handle error = %v", err)
	}
}

func TestRemoteProjectCreateRejectsPathsUnknownFieldsAndUnsafeNames(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	now := time.Now().UTC()
	parent, err := projectDirectoryBrowserFor(state).issue(directory, now)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: func() time.Time { return now }, scope: "remote.peer.query", enforceProjectBinding: true}
	for _, input := range []map[string]any{
		{"name": "../escape", "parentDirectoryId": parent.ID},
		{"name": "C:", "parentDirectoryId": parent.ID},
		{"name": "CON", "parentDirectoryId": parent.ID},
		{"name": "safe", "path": directory, "parentDirectoryId": parent.ID},
		{"name": "safe", "relativePath": "child", "parentDirectoryId": parent.ID},
		{"name": "safe", "projectId": uuid.NewString(), "parentDirectoryId": parent.ID},
		{"name": "safe"},
		{"name": "safe", "directoryId": parent.ID, "parentDirectoryId": parent.ID},
		{"name": "safe", "parentDirectoryId": parent.ID, "gitUrl": "https://user@example.test/private.git"},
	} {
		response := dispatchProjectRPC(t, dispatch, "project.create", "", mustProjectJSON(t, input))
		if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT {
			t.Errorf("project.create(%v) error = %+v", input, response.GetError())
		}
	}
}

func TestRemoteProjectRemoveSoftDeletesOnlyTheRegistryRecord(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	projectRoot := t.TempDir()
	project, err := state.business.addProject(t.Context(), projectRoot, "Disposable", "", defaultProjectPolicy)
	if err != nil {
		t.Fatal(err)
	}
	controlStore, err := loadControlState(state)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := reconcileWorkspaceProjects(t.Context(), state, controlStore, now, false, nil); err != nil {
		t.Fatal(err)
	}
	// A forced projection refresh advances the public project revision without
	// changing the SQLite registry revision. This is the production shape that
	// previously made a valid deletion fail with revision_conflict.
	if _, err := reconcileWorkspaceProjects(t.Context(), state, controlStore, now.Add(time.Second), true, nil); err != nil {
		t.Fatal(err)
	}
	before, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	projected, exists := before.Sync.Projects[project.ID.String()]
	if !exists || projected.Revision == project.Revision {
		t.Fatalf("projection revision = %d, registry revision = %d", projected.Revision, project.Revision)
	}
	dispatch := dispatcher{
		state: state, controlStore: controlStore, now: func() time.Time { return now.Add(2 * time.Second) },
		scope: "remote.peer.query", enforceProjectBinding: true,
	}
	stale := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": project.ID, "expectedRevision": projected.Revision - 1,
	}))
	if stale.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT {
		t.Fatalf("stale project.remove error = %+v", stale.GetError())
	}
	response := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": project.ID, "expectedRevision": projected.Revision,
	}))
	if response.GetError() != nil {
		t.Fatalf("project.remove error = %+v", response.GetError())
	}
	if bytes.Contains(response.GetJsonPayload(), []byte(projectRoot)) || bytes.Contains(response.GetJsonPayload(), []byte(`"path"`)) {
		t.Fatalf("project.remove leaked a local path: %s", response.GetJsonPayload())
	}
	var result struct {
		Removed   bool      `json:"removed"`
		ProjectID uuid.UUID `json:"projectId"`
		State     string    `json:"state"`
		Revision  uint64    `json:"revision"`
	}
	if err := json.Unmarshal(response.GetJsonPayload(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Removed || result.ProjectID != project.ID || result.State != "removed" || result.Revision != projected.Revision+1 {
		t.Fatalf("project.remove result = %+v", result)
	}
	stored, err := state.business.projectByID(t.Context(), project.ID)
	if err != nil || stored.State != "removed" || stored.Revision != project.Revision+1 {
		t.Fatalf("soft-deleted project = %+v, %v", stored, err)
	}
	after, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := after.Sync.Projects[project.ID.String()]; exists {
		t.Fatal("soft-deleted project remained in the control projection")
	}
	if revision, found := projectTombstoneRevision(after, project.ID, projected.Revision); !found || revision != result.Revision {
		t.Fatalf("project tombstone revision = %d, found = %t", revision, found)
	}
	// Another controller may still display the final control-plane upsert. Its
	// repeated delete must converge locally instead of surfacing a misleading
	// revision conflict, and it must not append a duplicate tombstone.
	replayed := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": project.ID, "expectedRevision": projected.Revision,
	}))
	if replayed.GetError() != nil {
		t.Fatalf("replayed project.remove error = %+v", replayed.GetError())
	}
	var replayedResult struct {
		Removed   bool      `json:"removed"`
		ProjectID uuid.UUID `json:"projectId"`
		State     string    `json:"state"`
		Revision  uint64    `json:"revision"`
	}
	if err := json.Unmarshal(replayed.GetJsonPayload(), &replayedResult); err != nil {
		t.Fatal(err)
	}
	if replayedResult != result {
		t.Fatalf("replayed project.remove result = %+v, want %+v", replayedResult, result)
	}
	afterReplay, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if afterReplay.ProjectHighWatermark != after.ProjectHighWatermark || len(afterReplay.ProjectChanges) != len(after.ProjectChanges) || len(afterReplay.Sync.Pending) != len(after.Sync.Pending) {
		t.Fatalf("replayed project.remove appended changes: before=%+v after=%+v", after, afterReplay)
	}
	if info, err := os.Stat(projectRoot); err != nil || !info.IsDir() {
		t.Fatalf("project.remove deleted the project directory: %v, %v", info, err)
	}
	active, err := state.business.listProjects(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range active {
		if item.ID == project.ID {
			t.Fatalf("removed project remained in the active registry: %+v", item)
		}
	}
}

func TestRemovedLegacyProjectIsNotRestoredByWorkspaceDiscovery(t *testing.T) {
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	rootID := stableProjectID(state.DeviceID, "")
	root, err := state.business.projectByID(t.Context(), rootID)
	if err != nil {
		_ = state.close()
		t.Fatal(err)
	}
	if _, err := state.business.removeProject(t.Context(), rootID, &root.Revision); err != nil {
		_ = state.close()
		t.Fatal(err)
	}
	projects, err := scanWorkspaceProjects(t.Context(), state)
	if err != nil {
		_ = state.close()
		t.Fatal(err)
	}
	if _, exists := projects[rootID.String()]; exists {
		_ = state.close()
		t.Fatal("legacy workspace discovery restored a removed project")
	}
	if err := state.close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	stored, err := reopened.business.projectByID(t.Context(), rootID)
	if err != nil || stored.State != "removed" || stored.Revision != root.Revision+1 {
		t.Fatalf("legacy project after restart = %+v, %v", stored, err)
	}
	if info, err := os.Stat(workspace); err != nil || !info.IsDir() {
		t.Fatalf("legacy workspace directory was changed: %v, %v", info, err)
	}
}

func TestRemoteProjectRemoveOnlyBlocksActiveConversationAndTaskWork(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	now := time.Now().UTC()
	controlStore, err := loadControlState(state)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: state, controlStore: controlStore, now: func() time.Time { return now },
		scope: "remote.peer.query", enforceProjectBinding: true,
	}

	conversationHistoryProject, err := state.business.addProject(t.Context(), t.TempDir(), "Conversation history", "", defaultProjectPolicy)
	if err != nil {
		t.Fatal(err)
	}
	config := installTestAIConfig(state)
	if _, err := state.business.createAIConversation(t.Context(), conversationHistoryProject.ID, "", "Keep history", "readOnly", config, now); err != nil {
		t.Fatal(err)
	}
	conversationHistoryProjection := currentProjectProjection(t, state, controlStore, conversationHistoryProject.ID, now)
	conversationHistoryRemoved := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": conversationHistoryProject.ID, "expectedRevision": conversationHistoryProjection.Revision,
	}))
	if conversationHistoryRemoved.GetError() != nil {
		t.Fatalf("idle conversation history blocked project removal = %+v", conversationHistoryRemoved.GetError())
	}
	if count := projectRelationCount(t, state, `SELECT COUNT(*) FROM ai_conversations WHERE project_id = ?`, conversationHistoryProject.ID.String()); count != 1 {
		t.Fatalf("retained conversation history count = %d", count)
	}

	activeConversationProject, err := state.business.addProject(t.Context(), t.TempDir(), "Active conversation", "", defaultProjectPolicy)
	if err != nil {
		t.Fatal(err)
	}
	activeConversation, err := state.business.createAIConversation(t.Context(), activeConversationProject.ID, "", "Generating", "readOnly", config, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.business.beginAIConversationTurn(
		t.Context(), activeConversationProject.ID, activeConversation.ID, uuid.NewString(), "Keep working", "readOnly", nil, config, now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	conversationProjection := currentProjectProjection(t, state, controlStore, activeConversationProject.ID, now.Add(3*time.Second))
	conversationBlocked := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": activeConversationProject.ID, "expectedRevision": conversationProjection.Revision,
	}))
	if conversationBlocked.GetError().GetSafeMessage() != "PROJECT_HAS_AI_CONVERSATIONS" {
		t.Fatalf("active-conversation project removal = %+v", conversationBlocked.GetError())
	}

	taskHistoryRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(taskHistoryRoot, "context.md"), []byte("task context"), 0o600); err != nil {
		t.Fatal(err)
	}
	taskHistoryProject, err := state.business.addProject(t.Context(), taskHistoryRoot, "Task history", "", defaultProjectPolicy)
	if err != nil {
		t.Fatal(err)
	}
	taskStore := newTaskV2Store(state.business)
	finishedTask, err := taskStore.Create(t.Context(), normalizeTaskV2TestDefinition(t, taskHistoryProject, uuid.New()), now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskStore.Transition(
		t.Context(), finishedTask.Definition.ID, finishedTask.Revision, "cancelled", "cancelled", now.Add(5*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	taskHistoryProjection := currentProjectProjection(t, state, controlStore, taskHistoryProject.ID, now.Add(6*time.Second))
	taskHistoryRemoved := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": taskHistoryProject.ID, "expectedRevision": taskHistoryProjection.Revision,
	}))
	if taskHistoryRemoved.GetError() != nil {
		t.Fatalf("finished task history blocked project removal = %+v", taskHistoryRemoved.GetError())
	}
	if count := projectRelationCount(t, state, `SELECT COUNT(*) FROM tasks WHERE project_id = ?`, taskHistoryProject.ID.String()); count != 1 {
		t.Fatalf("retained task history count = %d", count)
	}

	activeTaskRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(activeTaskRoot, "context.md"), []byte("active task context"), 0o600); err != nil {
		t.Fatal(err)
	}
	activeTaskProject, err := state.business.addProject(t.Context(), activeTaskRoot, "Active task", "", defaultProjectPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := taskStore.Create(t.Context(), normalizeTaskV2TestDefinition(t, activeTaskProject, uuid.New()), now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	taskProjection := currentProjectProjection(t, state, controlStore, activeTaskProject.ID, now.Add(8*time.Second))
	taskBlocked := dispatchProjectRPC(t, dispatch, "project.remove", "", mustProjectJSON(t, map[string]any{
		"projectId": activeTaskProject.ID, "expectedRevision": taskProjection.Revision,
	}))
	if taskBlocked.GetError().GetSafeMessage() != "PROJECT_HAS_TASKS" {
		t.Fatalf("active-task project removal = %+v", taskBlocked.GetError())
	}
}

func projectRelationCount(t *testing.T, state *agentState, query string, arguments ...any) int {
	t.Helper()
	db, err := state.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRowContext(t.Context(), query, arguments...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func currentProjectProjection(
	t *testing.T,
	state *agentState,
	controlStore *controlStateStore,
	projectID uuid.UUID,
	now time.Time,
) localProject {
	t.Helper()
	if _, err := reconcileWorkspaceProjects(t.Context(), state, controlStore, now, false, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	project, exists := snapshot.Sync.Projects[projectID.String()]
	if !exists {
		t.Fatalf("project %s is missing from the control projection", projectID)
	}
	return project
}

func dispatchProjectRPC(t *testing.T, dispatch dispatcher, method, projectID string, input []byte) *remotev1.RpcResponse {
	t.Helper()
	envelope, err := newCallEnvelope(uuid.NewString(), method, input, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	envelope.GetRequest().GetHeader().ProjectId = projectID
	return dispatch.dispatch(t.Context(), envelope).GetResponse()
}

func mustProjectJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
