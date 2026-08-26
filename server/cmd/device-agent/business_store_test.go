package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
)

func TestBusinessStorePersistsIndependentProjectsRevisionsAndTombstones(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	stateDirectory := t.TempDir()
	statePath := filepath.Join(stateDirectory, "state.json")
	workspace := filepath.Join(stateDirectory, "legacy-workspace")
	projectRootA, projectRootB := t.TempDir(), t.TempDir()
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	projectA, err := state.business.addProject(t.Context(), projectRootA, "Independent A", "https://example.test/a.git", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := state.business.addProject(t.Context(), projectRootB, "Independent B", "ssh://example.test/team/b.git", projectPolicy{AllowInteractiveTerminal: true})
	if err != nil {
		t.Fatal(err)
	}
	if projectA.ID == projectB.ID || projectA.LocalPath == projectB.LocalPath {
		t.Fatalf("independent projects = %+v / %+v", projectA, projectB)
	}
	stateJSON, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stateJSON, []byte(projectRootA)) || bytes.Contains(stateJSON, []byte(projectRootB)) {
		t.Fatal("identity state contains explicitly registered project paths")
	}

	wrongRevision := projectB.Revision + 1
	newName := "Independent B Updated"
	if _, err := state.business.updateProject(t.Context(), projectB.ID, &newName, nil, nil, &wrongRevision); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale project update error = %v", err)
	}
	expectedRevision := projectB.Revision
	updatedB, err := state.business.updateProject(t.Context(), projectB.ID, &newName, nil, nil, &expectedRevision)
	if err != nil || updatedB.Revision != projectB.Revision+1 || updatedB.DisplayName != newName {
		t.Fatalf("updated project = %+v, %v", updatedB, err)
	}
	unchangedB, err := state.business.updateProject(t.Context(), projectB.ID, &newName, nil, nil, &updatedB.Revision)
	if err != nil || unchangedB.Revision != updatedB.Revision {
		t.Fatalf("idempotent project update = %+v, %v", unchangedB, err)
	}

	removeRevision := projectA.Revision
	removedA, err := state.business.removeProject(t.Context(), projectA.ID, &removeRevision)
	if err != nil || removedA.State != "removed" || removedA.Revision != projectA.Revision+1 {
		t.Fatalf("removed project = %+v, %v", removedA, err)
	}
	if info, err := os.Stat(projectRootA); err != nil || !info.IsDir() {
		t.Fatalf("removeProject deleted the local directory: %v, %v", info, err)
	}
	restoredA, err := state.business.addProject(t.Context(), projectRootA, "Independent A Restored", "", projectPolicy{})
	if err != nil || restoredA.ID != projectA.ID || restoredA.Revision != removedA.Revision+1 || restoredA.State != "available" {
		t.Fatalf("restored project = %+v, %v", restoredA, err)
	}

	db, err := state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var schemaVersion, changes int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_changes`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != businessSchemaVersion || changes < 6 {
		t.Fatalf("schema version=%d project changes=%d", schemaVersion, changes)
	}

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := reloaded.business.listProjects(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]registeredProject, len(projects))
	for _, project := range projects {
		byPath[project.LocalPath] = project
	}
	if byPath[projectRootA].ID != projectA.ID || byPath[projectRootB].ID != projectB.ID || byPath[projectRootB].Revision != updatedB.Revision {
		t.Fatalf("reloaded projects = %#v", byPath)
	}
}

func TestLegacyWorkspaceMigrationIsIdempotentAndLeavesIdentitySourceIntact(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	workspace := filepath.Join(directory, "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "state.json")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanWorkspaceProjects(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	first, err := state.business.listProjects(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	firstIDs := sortedProjectIDs(first)
	db, err := state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	var firstChanges int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_changes`).Scan(&firstChanges); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scanWorkspaceProjects(t.Context(), reloaded); err != nil {
		t.Fatal(err)
	}
	second, err := reloaded.business.listProjects(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	afterState, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	db, err = reloaded.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var secondChanges int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_changes`).Scan(&secondChanges); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) || !equalUUIDs(firstIDs, sortedProjectIDs(second)) || firstChanges != secondChanges {
		t.Fatalf("migration changed on restart: ids=%v/%v changes=%d/%d stateEqual=%v", firstIDs, sortedProjectIDs(second), firstChanges, secondChanges, bytes.Equal(beforeState, afterState))
	}
}

func TestLegacyWorkspaceMigrationPreservesExplicitChildProject(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	workspace := filepath.Join(directory, "workspace")
	child := filepath.Join(workspace, "explicit-project")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	explicit, err := state.business.addProject(t.Context(), child, "Explicit project", "", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := state.business.ensureLegacyWorkspace(t.Context(), state); err != nil {
		t.Fatalf("scan legacy workspace: %v", err)
	}
	projects, err := state.business.listProjects(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	var matching []registeredProject
	for _, project := range projects {
		if sameFilesystemPath(project.LocalPath, child) {
			matching = append(matching, project)
		}
	}
	if len(matching) != 1 || matching[0].ID != explicit.ID {
		t.Fatalf("projects at explicit child path = %#v, want preserved project %s", matching, explicit.ID)
	}
}

func TestBusinessStoreMigratesVersionOnePolicySchemaTransactionally(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version >= 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE terminal_session_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE ai_tool_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE rpc_compatibility_metrics`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"task_log_migration_reports",
		"workflow_node_runs", "workflow_runs", "workflow_edges", "workflow_nodes", "workflow_revisions",
		"task_logs", "task_runs", "task_changes", "tasks",
	} {
		if _, err := db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE projects DROP COLUMN allow_task_execution`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE projects DROP COLUMN allow_recursive_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE projects DROP COLUMN allow_ai_workspace_tools`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	db, err = reloaded.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, recursivePolicy, taskPolicy, aiToolsPolicy int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT allow_recursive_delete, allow_task_execution, allow_ai_workspace_tools FROM projects LIMIT 1`).Scan(&recursivePolicy, &taskPolicy, &aiToolsPolicy); err != nil {
		t.Fatal(err)
	}
	var taskTables, aiAuditTables, compatibilityMetricTables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('tasks','task_runs','task_logs','workflow_revisions','workflow_nodes','workflow_edges','workflow_runs','workflow_node_runs')`).Scan(&taskTables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'ai_tool_audit'`).Scan(&aiAuditTables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'rpc_compatibility_metrics'`).Scan(&compatibilityMetricTables); err != nil {
		t.Fatal(err)
	}
	if version != businessSchemaVersion || recursivePolicy != 1 || taskPolicy != 1 || aiToolsPolicy != 1 || taskTables != 8 || aiAuditTables != 1 || compatibilityMetricTables != 1 {
		t.Fatalf("migrated schema version=%d recursive-delete=%d task-policy=%d ai-tools-policy=%d task-tables=%d ai-audit=%d compatibility-metrics=%d", version, recursivePolicy, taskPolicy, aiToolsPolicy, taskTables, aiAuditTables, compatibilityMetricTables)
	}
}

func TestProjectCLIManagesRegistryWithoutDeletingDirectories(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	projectRoot := t.TempDir()
	base := []string{"--state-file", statePath, "--workspace", workspace}
	var output bytes.Buffer
	arguments := append([]string{"project", "add"}, base...)
	arguments = append(arguments, "--path", projectRoot, "--name", "CLI Project", "--git-url", "https://example.test/cli.git", "--allow-task-execution", "--allow-ai-workspace-tools", "--allow-recursive-delete")
	if err := run(arguments, bytes.NewReader(nil), &output, &output); err != nil {
		t.Fatal(err)
	}
	var added map[string]any
	if err := json.Unmarshal(output.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	projectID, ok := added["id"].(string)
	policy, policyOK := added["policy"].(map[string]any)
	if !ok || added["path"] != projectRoot || !policyOK || policy["allowTaskExecution"] != true || policy["allowAIWorkspaceTools"] != true || policy["allowRecursiveDelete"] != true {
		t.Fatalf("project add output = %#v", added)
	}
	output.Reset()
	arguments = append([]string{"project", "update"}, base...)
	arguments = append(arguments, "--id", projectID, "--name", "CLI Updated", "--expected-revision", "1")
	if err := run(arguments, bytes.NewReader(nil), &output, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	arguments = append([]string{"project", "list"}, base...)
	if err := run(arguments, bytes.NewReader(nil), &output, &output); err != nil || !bytes.Contains(output.Bytes(), []byte("CLI Updated")) {
		t.Fatalf("project list output=%s error=%v", output.String(), err)
	}
	output.Reset()
	arguments = append([]string{"project", "remove"}, base...)
	arguments = append(arguments, "--id", projectID, "--expected-revision", "2")
	if err := run(arguments, bytes.NewReader(nil), &output, &output); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(projectRoot); err != nil || !info.IsDir() {
		t.Fatalf("CLI remove deleted directory: %v, %v", info, err)
	}
}

func TestRegisteredProjectRootReplacementCannotEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("child junction escape is covered by TestFileRPCRejectsPathEscapeSymlinksJunctionsAndWindowsNames")
	}
	directory := t.TempDir()
	root := filepath.Join(directory, "registered")
	outside := filepath.Join(directory, "outside")
	backup := filepath.Join(directory, "registered-original")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	project, err := state.business.addProject(context.Background(), root, "Root", "", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, _, err := secureExistingProjectPath(project, ""); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("replaced project root error = %v", err)
	}
}

func equalUUIDs(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
