package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type aiWorkspaceToolFixture struct {
	state           *agentState
	project         registeredProject
	executor        *aiWorkspaceToolExecutor
	context         aiWorkspaceToolContext
	terminalStarter *fakePTYStarter
}

func newAIWorkspaceToolFixture(t *testing.T, mode string) aiWorkspaceToolFixture {
	t.Helper()
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	project, err := state.business.projectByID(t.Context(), stableProjectID(state.DeviceID, ""))
	if err != nil {
		t.Fatal(err)
	}
	supervisor := newRawProcessSupervisorWithDependencies(new(fakeRawStarter), func(int) (uint64, error) { return 0, nil }, 4)
	supervisor.memoryPollInterval = time.Hour
	t.Cleanup(func() { _ = supervisor.Close() })
	terminalStarter := new(fakePTYStarter)
	terminalSupervisor := newProcessSupervisorWithDependencies(terminalStarter, func(int) (uint64, error) { return 0, nil }, 8)
	terminalSupervisor.memoryPollInterval = time.Hour
	t.Cleanup(func() { _ = terminalSupervisor.Close() })
	executor := newAIWorkspaceToolExecutor(state, supervisor, terminalSupervisor)
	t.Cleanup(func() { _ = executor.Close() })
	return aiWorkspaceToolFixture{
		state: state, project: project, executor: executor, terminalStarter: terminalStarter,
		context: aiWorkspaceToolContext{
			Project: project, ConversationID: uuid.NewString(), GenerationID: uuid.NewString(), WorkspaceMode: mode,
		},
	}
}

func planAIWorkspaceTool(t *testing.T, fixture aiWorkspaceToolFixture, name string, arguments map[string]any) aiWorkspaceToolPlan {
	t.Helper()
	plan, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{ID: uuid.NewString(), Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("plan %s: %v", name, err)
	}
	return plan
}

func TestAIWorkspaceToolDefinitionsHonorModes(t *testing.T) {
	readOnly := aiWorkspaceToolDefinitions("readOnly")
	workspaceWrite := aiWorkspaceToolDefinitions("workspaceWrite")
	full := aiWorkspaceToolDefinitions("fullAccess")
	if len(readOnly) != 10 || len(workspaceWrite) != 17 || len(full) != 17 || aiWorkspaceToolDefinitions("invalid") != nil {
		t.Fatalf("definition counts read=%d workspace-write=%d full=%d", len(readOnly), len(workspaceWrite), len(full))
	}
	want := []string{"list_files", "search_files", "read_file", "read_tool_result", "read_image", "web_search", "web_fetch", "terminal_open", "terminal_send", "terminal_read", "terminal_signal", "terminal_close", "terminal_list", "write_file", "replace_in_file", "rollback_file_change", "run_command"}
	for index, name := range want {
		if workspaceWrite[index].Name != name || workspaceWrite[index].InputSchema["additionalProperties"] != false {
			t.Fatalf("definition %d = %+v", index, workspaceWrite[index])
		}
	}
	permissionEnum := func(definitions []aiWorkspaceToolDefinition, name string) ([]string, bool) {
		t.Helper()
		for _, definition := range definitions {
			if definition.Name != name {
				continue
			}
			properties := definition.InputSchema["properties"].(map[string]any)
			permission, found := properties["sandbox_permissions"].(map[string]any)
			if !found {
				return nil, false
			}
			values, _ := permission["enum"].([]string)
			return values, true
		}
		t.Fatalf("tool definition %q missing", name)
		return nil, false
	}
	if values, found := permissionEnum(readOnly, "run_command"); !found || !reflect.DeepEqual(values, []string{aiWorkspaceModeWorkspaceWrite, aiWorkspaceModeFullAccess}) {
		t.Fatalf("read-only run_command sandbox_permissions = %v, found=%v", values, found)
	}
	for _, name := range []string{"write_file", "replace_in_file", "run_command", "terminal_open"} {
		if values, found := permissionEnum(workspaceWrite, name); !found || !reflect.DeepEqual(values, []string{aiWorkspaceModeFullAccess}) {
			t.Fatalf("workspace-write %s sandbox_permissions = %v, found=%v", name, values, found)
		}
		if values, found := permissionEnum(full, name); found {
			t.Fatalf("full-access %s unexpectedly exposes sandbox_permissions = %v", name, values)
		}
	}
}

func TestAIWorkspaceReadToolsStayInsideProjectAndHideSensitiveFiles(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, "readOnly")
	root := fixture.project.LocalPath
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "hidden"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "sample.txt"), []byte("alpha\nneedle value\nomega\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "hidden", "dependency.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	list := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "list_files", map[string]any{
		"recursive": true, "max_depth": float64(4), "limit": float64(100),
	}), false)
	if list.IsError || !strings.Contains(list.Content, "lib/sample.txt") || strings.Contains(list.Content, "node_modules") {
		t.Fatalf("list result = %+v", list)
	}
	search := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "search_files", map[string]any{
		"query": "needle", "max_results": float64(20),
	}), false)
	if search.IsError || !strings.Contains(search.Content, "lib/sample.txt:2") || strings.Contains(search.Content, ".env") || strings.Contains(search.Content, "dependency.txt") {
		t.Fatalf("search result = %+v", search)
	}
	readPlan := planAIWorkspaceTool(t, fixture, "read_file", map[string]any{"path": "lib/sample.txt", "start_line": float64(2), "max_lines": float64(1)})
	read := fixture.executor.Execute(t.Context(), fixture.context, readPlan, false)
	hash, _ := read.Metadata["content_hash"].(string)
	if read.IsError || len(hash) != 64 || !strings.Contains(read.Content, "2\tneedle value") || strings.Contains(read.Content, "1\talpha") {
		t.Fatalf("read result = %+v", read)
	}
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{ID: uuid.NewString(), Name: "read_file", Arguments: map[string]any{"path": "../outside.txt"}}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("traversal plan error = %v", err)
	}
	sensitive := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "read_file", map[string]any{"path": ".env"}), false)
	if !sensitive.IsError || sensitive.Metadata["error_code"] != "forbidden" {
		t.Fatalf("sensitive read = %+v", sensitive)
	}
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{ID: uuid.NewString(), Name: "write_file", Arguments: map[string]any{"path": "blocked.txt", "content": "x"}}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("read-only write plan error = %v", err)
	}
}

func TestAIWorkspaceWriteReplaceRollbackAreApprovedAtomicAndConflictSafe(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, "workspaceWrite")
	path := filepath.Join(fixture.project.LocalPath, "notes.md")
	writePlan := planAIWorkspaceTool(t, fixture, "write_file", map[string]any{"path": "notes.md", "content": "one\ntwo\n", "expected_hash": "absent"})
	if !writePlan.RequiresApproval || writePlan.Preview.Risk != "writesData" || writePlan.Preview.RelativePaths[0] != "notes.md" {
		t.Fatalf("write plan = %+v", writePlan)
	}
	denied := fixture.executor.Execute(t.Context(), fixture.context, writePlan, false)
	if !denied.IsError || denied.Metadata["error_code"] != "approval_required" {
		t.Fatalf("denied write = %+v", denied)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied write created file: %v", err)
	}
	writePlan = planAIWorkspaceTool(t, fixture, "write_file", map[string]any{"path": "notes.md", "content": "one\ntwo\n", "expected_hash": "absent"})
	written := fixture.executor.Execute(t.Context(), fixture.context, writePlan, true)
	rollbackID, _ := written.Metadata["rollback_id"].(string)
	writtenHash, _ := written.Metadata["content_hash"].(string)
	if written.IsError || uuid.Validate(rollbackID) != nil || len(writtenHash) != 64 {
		t.Fatalf("write result = %+v", written)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "one\ntwo\n" {
		t.Fatalf("written contents=%q error=%v", contents, err)
	}

	stalePlan := planAIWorkspaceTool(t, fixture, "replace_in_file", map[string]any{
		"path": "notes.md", "old_text": "two", "new_text": "second", "expected_hash": strings.Repeat("0", 64),
	})
	stale := fixture.executor.Execute(t.Context(), fixture.context, stalePlan, true)
	if !stale.IsError || stale.Metadata["error_code"] != "revision_conflict" {
		t.Fatalf("stale replace = %+v", stale)
	}
	replacePlan := planAIWorkspaceTool(t, fixture, "replace_in_file", map[string]any{
		"path": "notes.md", "old_text": "two", "new_text": "second", "expected_hash": writtenHash,
	})
	replaced := fixture.executor.Execute(t.Context(), fixture.context, replacePlan, true)
	replaceRollbackID, _ := replaced.Metadata["rollback_id"].(string)
	replacedHash, _ := replaced.Metadata["content_hash"].(string)
	if replaced.IsError || replaceRollbackID == "" || replacedHash == writtenHash {
		t.Fatalf("replace result = %+v", replaced)
	}

	other := fixture.context
	other.ConversationID = uuid.NewString()
	if _, err := fixture.executor.Plan(t.Context(), other, aiWorkspaceToolCall{ID: uuid.NewString(), Name: "rollback_file_change", Arguments: map[string]any{
		"rollback_id": replaceRollbackID, "expected_hash": replacedHash,
	}}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("cross-conversation rollback plan error = %v", err)
	}
	rollbackPlan := planAIWorkspaceTool(t, fixture, "rollback_file_change", map[string]any{"rollback_id": replaceRollbackID, "expected_hash": replacedHash})
	if err := os.WriteFile(path, []byte("local edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflict := fixture.executor.Execute(t.Context(), fixture.context, rollbackPlan, true)
	if !conflict.IsError || conflict.Metadata["error_code"] != "revision_conflict" {
		t.Fatalf("rollback conflict = %+v", conflict)
	}
	if contents, _ := os.ReadFile(path); string(contents) != "local edit\n" {
		t.Fatalf("rollback overwrote local change: %q", contents)
	}

	// Restore the exact updated contents and prove a valid rollback returns the
	// file to the pre-replace version without consuming another conversation's record.
	if err := os.WriteFile(path, []byte("one\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollbackPlan = planAIWorkspaceTool(t, fixture, "rollback_file_change", map[string]any{"rollback_id": replaceRollbackID, "expected_hash": replacedHash})
	rolledBack := fixture.executor.Execute(t.Context(), fixture.context, rollbackPlan, true)
	if rolledBack.IsError {
		t.Fatalf("rollback result = %+v", rolledBack)
	}
	if contents, _ := os.ReadFile(path); string(contents) != "one\ntwo\n" {
		t.Fatalf("rolled-back contents = %q", contents)
	}
	_ = rollbackID // The earlier new-file rollback remains independently valid.
}

func TestAIFullAccessFileWriteBypassesApproval(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	plan := planAIWorkspaceTool(t, fixture, "write_file", map[string]any{
		"path": "full-access.txt", "content": "trusted\n", "expected_hash": "absent",
	})
	if plan.RequiresApproval {
		t.Fatalf("full-access write unexpectedly requires approval: %+v", plan)
	}
	result := fixture.executor.Execute(t.Context(), fixture.context, plan, false)
	contents, err := os.ReadFile(filepath.Join(fixture.project.LocalPath, "full-access.txt"))
	if result.IsError || err != nil || string(contents) != "trusted\n" {
		t.Fatalf("full-access write result=%+v contents=%q error=%v", result, contents, err)
	}
}

func TestAIFullAccessSupportsExternalFilesAndWorkingDirectories(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	external := filepath.Join(filepath.Dir(fixture.project.LocalPath), "external")
	if err := os.MkdirAll(external, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(external, "outside.txt")
	if err := os.WriteFile(existing, []byte("external needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	read := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "read_file", map[string]any{
		"path": existing,
	}), false)
	if read.IsError || !strings.Contains(read.Content, "external needle") || read.Metadata["path"] != existing {
		t.Fatalf("external read = %+v", read)
	}
	listed := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "list_files", map[string]any{
		"path": external,
	}), false)
	if listed.IsError || !strings.Contains(listed.Content, existing) {
		t.Fatalf("external list = %+v", listed)
	}
	searched := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "search_files", map[string]any{
		"path": external, "query": "needle",
	}), false)
	if searched.IsError || !strings.Contains(searched.Content, existing+":1") {
		t.Fatalf("external search = %+v", searched)
	}

	created := filepath.Join(external, "created.txt")
	write := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "write_file", map[string]any{
		"path": created, "content": "created externally\n", "expected_hash": "absent",
	}), false)
	rollbackID, _ := write.Metadata["rollback_id"].(string)
	writtenHash, _ := write.Metadata["content_hash"].(string)
	if write.IsError || rollbackID == "" {
		t.Fatalf("external write = %+v", write)
	}
	rollback := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "rollback_file_change", map[string]any{
		"rollback_id": rollbackID, "expected_hash": writtenHash,
	}), false)
	if rollback.IsError {
		t.Fatalf("external rollback = %+v", rollback)
	}
	if _, err := os.Stat(created); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external rollback left target: %v", err)
	}

	relativeOutside, err := filepath.Rel(fixture.project.LocalPath, external)
	if err != nil {
		t.Fatal(err)
	}
	commandPlan := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{
		"command": "echo external", "working_directory": relativeOutside,
	})
	if !sameFilesystemPath(commandPlan.commandLaunch.WorkingDirectory, external) || commandPlan.Preview.WorkingDirectory != external {
		t.Fatalf("external command plan = %+v", commandPlan)
	}
	terminalPlan := planAIWorkspaceTool(t, fixture, "terminal_open", map[string]any{
		"type": "shell", "working_directory": external,
	})
	if !sameFilesystemPath(terminalPlan.commandLaunch.WorkingDirectory, external) || terminalPlan.terminalDisplayCWD != external {
		t.Fatalf("external terminal plan = %+v", terminalPlan)
	}

	restricted := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	if _, err := restricted.executor.Plan(t.Context(), restricted.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "read_file", Arguments: map[string]any{"path": existing},
	}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("workspace-write external read error = %v", err)
	}
}

func TestAIWorkspaceAuditAcceptsHashedRunningRecord(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, "workspaceWrite")
	const privatePath = "audit-private-marker.txt"
	const privateBody = "audit-private-body-marker-4f144334"
	plan := planAIWorkspaceTool(t, fixture, "write_file", map[string]any{"path": privatePath, "content": privateBody, "expected_hash": "absent"})
	record, err := fixture.executor.auditRecord(fixture.context, plan)
	if err != nil {
		t.Fatal(err)
	}
	record.Outcome, record.ApprovalDecision = "running", "allow_once"
	if !validAIToolAuditRecord(record) {
		t.Fatalf("audit record is invalid: %+v", record)
	}
	if err := fixture.state.business.recordAIToolAudit(t.Context(), record); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	finished := record.StartedAt.Add(time.Second)
	record.Outcome, record.ResultSHA256, record.FinishedAt = "succeeded", aiWorkspaceBytesHash([]byte("private-result-marker")), &finished
	if err := fixture.state.business.recordAIToolAudit(t.Context(), record); err != nil {
		t.Fatalf("finish audit: %v", err)
	}
	mutated := record
	mutated.Outcome, mutated.ErrorCode = "denied", "forged"
	if err := fixture.state.business.recordAIToolAudit(t.Context(), mutated); !errors.Is(err, errRPCRevision) {
		t.Fatalf("terminal audit rewrite error = %v", err)
	}
	database, err := os.ReadFile(fixture.state.business.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(database, []byte(privatePath)) || bytes.Contains(database, []byte(privateBody)) || bytes.Contains(database, []byte("private-result-marker")) {
		t.Fatal("AI tool audit retained plaintext arguments or result")
	}
}

func TestAIFullAccessCommandUsesRawSupervisorWithoutApproval(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, "fullAccess")
	starter := new(fakeRawStarter)
	fixture.executor.supervisor = newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	fixture.executor.supervisor.memoryPollInterval = time.Hour
	plan := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{
		"allow_network": false, "background": false, "command": "echo supervised", "network_hosts": []any{},
		"sandbox_permissions": "", "timeout_seconds": float64(10), "working_directory": "",
	})
	if plan.RequiresApproval || plan.Preview.Command != "echo supervised" || plan.Preview.Risk != "openWorld" ||
		!strings.Contains(plan.Preview.SandboxStatus, "danger-full-access") {
		t.Fatalf("command plan = %+v", plan)
	}
	resultChannel := make(chan aiWorkspaceToolResult, 1)
	go func() { resultChannel <- fixture.executor.Execute(context.Background(), fixture.context, plan, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for starter.latest() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	process := starter.latest()
	if process == nil {
		t.Fatal("supervised process did not start")
	}
	if err := process.emitStdout([]byte("supervised output\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := process.emitStderr([]byte("supervised warning\r\n")); err != nil {
		t.Fatal(err)
	}
	process.finish(0)
	result := <-resultChannel
	var output aiWorkspaceCommandResult
	if err := json.Unmarshal([]byte(result.Content), &output); err != nil {
		t.Fatalf("command result is not structured JSON: %q (%v)", result.Content, err)
	}
	if result.IsError || output.Stdout.Text != "supervised output\n" || output.Stderr.Text != "supervised warning\n" ||
		output.Stdout.SourceEncoding != "utf-8" || output.Stderr.SourceEncoding != "utf-8" ||
		output.Stdout.Binary || output.Stderr.Binary || output.ExitCode != 0 || result.Metadata["exit_code"] != 0 {
		t.Fatalf("command result = %+v", result)
	}
	starter.mu.Lock()
	spec := starter.specs[0]
	starter.mu.Unlock()
	if spec.ProjectID != fixture.project.ID || !sameFilesystemPath(spec.ProjectRoot, fixture.project.LocalPath) ||
		!sameFilesystemPath(spec.WorkingDirectory, fixture.project.LocalPath) || len(spec.Argv) < 3 ||
		!strings.Contains(strings.Join(spec.Argv, " "), "echo supervised") {
		t.Fatalf("process spec = %+v", spec)
	}
	for _, variable := range spec.Environment {
		if strings.HasPrefix(strings.ToUpper(variable), "WENZWORK_") {
			t.Fatalf("process inherited Agent variable %q", variable)
		}
	}

	networkPlan, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{ID: uuid.NewString(), Name: "run_command", Arguments: map[string]any{"command": "curl https://example.test"}})
	if err != nil || networkPlan.RequiresApproval || !networkPlan.commandLaunch.NetworkAllowed {
		t.Fatalf("full-access network plan=%+v error=%v", networkPlan, err)
	}
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{ID: uuid.NewString(), Name: "run_command", Arguments: map[string]any{
		"command": "curl https://example.test", "allow_network": true, "network_hosts": []any{"example.test"},
	}}); err != nil {
		t.Fatalf("explicit network plan: %v", err)
	}
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{ID: uuid.NewString(), Name: "run_command", Arguments: map[string]any{"command": "rm -rf /"}}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("catastrophic command plan error = %v", err)
	}
}

func TestAIWorkspaceWriteCommandRequiresApprovalAndExplicitNetwork(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	fixture.executor.sandbox = func(request aiCommandSandboxRequest) (aiCommandSandboxLaunch, error) {
		return aiCommandSandboxLaunch{
			Argv: append([]string(nil), request.Argv...), WorkingDirectory: request.WorkingDirectory,
			SandboxMode: request.Mode, Status: "test workspace-write sandbox",
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: !request.AllowNetwork,
		}, nil
	}
	starter := new(fakeRawStarter)
	fixture.executor.supervisor = newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	fixture.executor.supervisor.memoryPollInterval = time.Hour

	plan := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{"command": "echo supervised"})
	if !plan.RequiresApproval || plan.commandLaunch.NetworkAllowed || !plan.commandLaunch.HardNetworkIsolation {
		t.Fatalf("workspace-write command plan = %+v", plan)
	}
	denied := fixture.executor.Execute(t.Context(), fixture.context, plan, false)
	if !denied.IsError || denied.Metadata["error_code"] != "approval_required" || starter.latest() != nil {
		t.Fatalf("denied command=%+v process=%v", denied, starter.latest())
	}
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "run_command", Arguments: map[string]any{"command": "curl https://example.test"},
	}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("implicit network plan error = %v", err)
	}
	networkPlan, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "run_command", Arguments: map[string]any{
			"command": "curl https://example.test", "allow_network": true, "network_hosts": []any{"example.test"},
		},
	})
	if err != nil || !networkPlan.RequiresApproval || !networkPlan.commandLaunch.NetworkAllowed || networkPlan.commandLaunch.HardNetworkIsolation {
		t.Fatalf("explicit network plan=%+v error=%v", networkPlan, err)
	}
}

func TestAIWorkspaceToolRejectsArgumentsChangedAfterPlanning(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	arguments := map[string]any{"path": "planned.txt", "content": "approved"}
	plan := planAIWorkspaceTool(t, fixture, "write_file", arguments)
	arguments["content"] = "replaced after approval"

	result := fixture.executor.Execute(t.Context(), fixture.context, plan, true)
	if !result.IsError || result.Metadata["error_code"] != "tool_plan_changed" {
		t.Fatalf("mutated plan result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(fixture.project.LocalPath, "planned.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutated plan created file: %v", err)
	}
}

func TestAIWorkspaceCommandStreamRetainsBinarySummaryAndTruncatedUTF8Prefix(t *testing.T) {
	binary, err := readAIWorkspaceCommandStream(bytes.NewReader([]byte{0x00, 0xff, 0x01}))
	if err != nil || !binary.Binary || binary.SourceEncoding != "binary" || binary.Text != "" || binary.Bytes != 3 {
		t.Fatalf("binary command stream = %+v, %v", binary, err)
	}

	contents := append(bytes.Repeat([]byte("x"), maximumAIWorkspaceCommandOutput-2), 0xe4, 0xb8)
	truncated, err := readAIWorkspaceCommandStream(bytes.NewReader(append(contents, 'z')))
	if err != nil || truncated.Binary || !truncated.Truncated || truncated.SourceEncoding != "utf-8" ||
		!strings.HasPrefix(truncated.Text, "xxxx") || truncated.Bytes != len(contents)+1 {
		t.Fatalf("truncated command stream = %+v, %v", truncated, err)
	}
}

func TestWindowsPowerShellCommandUsesNoBOMBootstrapAndFreshExitCode(t *testing.T) {
	command := windowsPowerShellCommand("Write-Error 'failed'")
	for _, want := range []string{
		"[System.Text.UTF8Encoding]::new($false)",
		"$global:LASTEXITCODE = $null",
		"if (-not $__wenzworkSuccess)",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("PowerShell command missing %q: %s", want, command)
		}
	}
	if strings.Contains(command, "-File") {
		t.Fatalf("PowerShell command used a naked script launcher: %s", command)
	}
}

func TestAIWorkspaceWriteNeverLeavesHalfFileOnRejectedInput(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, "workspaceWrite")
	path := filepath.Join(fixture.project.LocalPath, "stable.txt")
	original := []byte("stable\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	_, planErr := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{ID: uuid.NewString(), Name: "write_file", Arguments: map[string]any{
		"path": "stable.txt", "content": strings.Repeat("x", maximumAIWorkspaceWriteBytes+1), "expected_hash": aiWorkspaceBytesHash(original),
	}})
	if !errors.Is(planErr, errRPCInvalid) {
		t.Fatalf("oversized write plan error = %v", planErr)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(contents, original) {
		t.Fatalf("stable file changed: %q error=%v", contents, err)
	}
}
