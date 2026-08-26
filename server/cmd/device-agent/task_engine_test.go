package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeTaskRunnerProvider struct {
	mu             sync.Mutex
	prepared       []uuid.UUID
	errors         map[string]error
	parseCodexJSON bool
	promptPrefix   string
}

func (provider *fakeTaskRunnerProvider) Prepare(
	_ context.Context,
	project registeredProject,
	task taskV2Record,
	promptPath string,
) (taskRunnerInvocation, error) {
	provider.mu.Lock()
	provider.prepared = append(provider.prepared, task.Definition.ID)
	err := provider.errors[task.Definition.Kind]
	provider.mu.Unlock()
	if err != nil {
		return taskRunnerInvocation{}, err
	}
	return taskRunnerInvocation{
		Executable: filepath.Join(project.LocalPath, task.Definition.Kind+"-runner.exe"),
		Arguments:  []string{"--fake"}, Environment: []string{"TASK_RUNNER_TEST=1"}, UseStdinFile: task.Definition.Kind != "script",
		PromptPrefix: provider.promptPrefix, ParseCodexJSON: task.Definition.Kind == "codex" && provider.parseCodexJSON,
	}, nil
}

func (provider *fakeTaskRunnerProvider) Capabilities() []taskRunnerCapability {
	return []taskRunnerCapability{{Kind: "codex", Available: true, ProbeStatus: "ready", Models: []string{}, Features: map[string]bool{}}}
}

func (provider *fakeTaskRunnerProvider) Refresh(context.Context) {}

func (provider *fakeTaskRunnerProvider) preparedIDs() []uuid.UUID {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]uuid.UUID(nil), provider.prepared...)
}

type taskEngineTestHarness struct {
	engine     *taskEngine
	supervisor *rawProcessSupervisor
	starter    *fakeRawStarter
	runners    *fakeTaskRunnerProvider
}

func newTaskEngineTestHarness(t *testing.T, fixture taskV2StoreFixture, runners *fakeTaskRunnerProvider) taskEngineTestHarness {
	t.Helper()
	if runners == nil {
		runners = &fakeTaskRunnerProvider{errors: map[string]error{}}
	}
	if runners.errors == nil {
		runners.errors = map[string]error{}
	}
	fixture.state.tasksV2 = fixture.store
	starter := new(fakeRawStarter)
	supervisor := newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 8)
	supervisor.memoryPollInterval = time.Hour
	engine := newTaskEngine(fixture.state, supervisor, runners)
	engine.pollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		_ = engine.Close()
		_ = supervisor.Close()
	})
	return taskEngineTestHarness{engine: engine, supervisor: supervisor, starter: starter, runners: runners}
}

func TestTaskRunCompletionPreservesOutputLimitReasonFromLogWriter(t *testing.T) {
	status, code := taskRunCompletionStatus("", "", "output_limit", errTaskLogOutputLimit, 1)
	if status != "failed" || code != "output_limit" {
		t.Fatalf("output-limit completion = %s/%s", status, code)
	}
	status, code = taskRunCompletionStatus("cancelled", "cancelled", "output_limit", errTaskLogOutputLimit, 1)
	if status != "cancelled" || code != "cancelled" {
		t.Fatalf("explicit stop completion = %s/%s", status, code)
	}
}

func TestLoadOrCreateAgentStateNormalizesRelativePathsForPrivateTaskRuntime(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	t.Chdir(directory)
	state, err := loadOrCreateAgentState("state.json", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	if !filepath.IsAbs(state.path) || !filepath.IsAbs(state.Workspace) {
		t.Fatalf("state paths were not normalized: state=%q workspace=%q", state.path, state.Workspace)
	}
	prompt, err := createManagedTaskPrompt(state.path, uuid.New(), uuid.New(), []byte("run task"))
	if err != nil {
		t.Fatalf("create managed task prompt: %v", err)
	}
	if !filepath.IsAbs(prompt.Path) {
		t.Fatalf("managed task prompt path is relative: %q", prompt.Path)
	}
	if err := prompt.Cleanup(); err != nil {
		t.Fatal(err)
	}
}

func taskEngineTestDefinition(t *testing.T, project registeredProject, id uuid.UUID, title, mode string) taskV2Definition {
	t.Helper()
	definition := normalizeTaskV2TestDefinition(t, project, id)
	definition.Title = title
	definition.Execution.ScheduledAt = nil
	definition.Execution.RunImmediately = true
	definition.Execution.Mode = mode
	return definition
}

func fakeStarterSnapshot(starter *fakeRawStarter) ([]rawProcessLaunchSpec, []*fakeRawProcess) {
	starter.mu.Lock()
	defer starter.mu.Unlock()
	return append([]rawProcessLaunchSpec(nil), starter.specs...), append([]*fakeRawProcess(nil), starter.processes...)
}

func waitFakeProcessCount(t *testing.T, starter *fakeRawStarter, count int) []*fakeRawProcess {
	t.Helper()
	var result []*fakeRawProcess
	eventually(t, 3*time.Second, func() bool {
		_, current := fakeStarterSnapshot(starter)
		if len(current) != count {
			return false
		}
		result = current
		return true
	})
	return result
}

func waitTaskV2Status(t *testing.T, store *taskV2Store, id uuid.UUID, status string) taskV2Record {
	t.Helper()
	var result taskV2Record
	eventually(t, 3*time.Second, func() bool {
		current, err := store.Get(t.Context(), id)
		if err != nil || current.Status != status {
			return false
		}
		result = current
		return true
	})
	return result
}

func TestTaskEngineContinuesAfterRequestContextAndPersistsRawStreamLogs(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), "offline execution", "serial")
	requestContext, disconnect := context.WithCancel(context.Background())
	created, err := fixture.store.Create(requestContext, definition, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	disconnect()

	eventually(t, 3*time.Second, func() bool {
		_, processes := fakeStarterSnapshot(harness.starter)
		return len(processes) == 1
	})
	running := waitTaskV2Status(t, fixture.store, created.Definition.ID, "running")
	specs, processes := fakeStarterSnapshot(harness.starter)
	if len(specs) != 1 || len(processes) != 1 {
		t.Fatalf("started processes = %#v, %#v", specs, processes)
	}
	promptPath := specs[0].PrivateStdinPath
	if promptPath == "" {
		t.Fatalf("task stdin was not attached: %#v", specs[0])
	}
	if len(specs[0].Argv) == 0 || specs[0].Argv[0] != filepath.Join(fixture.project.LocalPath, "codex-runner.exe") ||
		strings.Contains(strings.Join(specs[0].Argv, " "), "internal-task-exec") {
		t.Fatalf("task did not start the target runner directly: %#v", specs[0].Argv)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil || !strings.Contains(string(prompt), "Review the current project") {
		t.Fatalf("managed prompt = %q, %v", prompt, err)
	}
	if err := processes[0].emitStdout([]byte("first output\n")); err != nil {
		t.Fatal(err)
	}
	if err := processes[0].emitStderr([]byte("warning output\n")); err != nil {
		t.Fatal(err)
	}
	if err := processes[0].emitStderr([]byte("helper diagnostic\n")); err != nil {
		t.Fatal(err)
	}
	processes[0].finish(0)
	awaiting := waitTaskV2Status(t, fixture.store, definition.ID, "awaitingAcceptance")
	if awaiting.Revision <= running.Revision || awaiting.ResultCode != "execution_succeeded" || awaiting.ExitCode == nil || *awaiting.ExitCode != 0 {
		t.Fatalf("finished task = %+v", awaiting)
	}
	if _, err := os.Stat(promptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed prompt still exists: %v", err)
	}
	db, err := fixture.store.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	var bodyRows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_logs WHERE task_id = ?`, definition.ID.String()).Scan(&bodyRows); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	if bodyRows != 0 {
		t.Fatalf("new run wrote %d task log body rows", bodyRows)
	}
	runs, err := fixture.store.ListRuns(t.Context(), definition.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != "awaitingAcceptance" || runs[0].ExitCode == nil || *runs[0].ExitCode != 0 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if !awaiting.LogAvailable || awaiting.LogState != taskLogStateSealed || awaiting.LogGeneration != 1 ||
		!runs[0].LogAvailable || runs[0].LogState != taskLogStateSealed || runs[0].LogGeneration != 1 {
		t.Fatalf("task/run log metadata = %+v / %+v", awaiting, runs[0])
	}
	logPath, err := taskRunLogPath(harness.engine.logRoot, definition.ID, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	relativeLogPath, err := filepath.Rel(harness.engine.logRoot, logPath)
	if err != nil || relativeLogPath == ".." || strings.HasPrefix(relativeLogPath, ".."+string(filepath.Separator)) {
		t.Fatalf("execution log escaped root: root=%q relative=%q error=%v", harness.engine.logRoot, relativeLogPath, err)
	}
	fileLog, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(fileLog), "Task execution started") ||
		!strings.Contains(string(fileLog), "[stdout] first output") || !strings.Contains(string(fileLog), "[stderr] warning output") ||
		!strings.HasSuffix(string(fileLog), "\n") {
		t.Fatalf("execution log = %q, %v", fileLog, err)
	}
	if info, statErr := os.Stat(logPath); statErr != nil || verifyStateFileSecurity(logPath) != nil {
		t.Fatalf("execution log permissions = %v, %v", info, statErr)
	}
}

func TestTaskEngineWritesRunnerPromptAdapterBeforeDirectLaunch(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	runners := &fakeTaskRunnerProvider{
		errors:       map[string]error{},
		promptPrefix: taskV2CodexGoalPromptPrefix,
	}
	harness := newTaskEngineTestHarness(t, fixture, runners)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), "adapted private prompt", "serial")
	created, err := fixture.store.Create(t.Context(), definition, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	waitTaskV2Status(t, fixture.store, created.Definition.ID, "running")
	waitFakeProcessCount(t, harness.starter, 1)
	specs, processes := fakeStarterSnapshot(harness.starter)
	if len(specs) != 1 || len(processes) != 1 || specs[0].PrivateStdinPath == "" {
		t.Fatalf("adapted task launch = %#v, %#v", specs, processes)
	}
	prompt, err := os.ReadFile(specs[0].PrivateStdinPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(prompt), taskV2CodexGoalPromptPrefix) ||
		!strings.Contains(string(prompt), "Review the current project") {
		t.Fatalf("adapted private prompt = %q", prompt)
	}
	if strings.Contains(strings.Join(specs[0].Argv, " "), taskV2CodexGoalPromptPrefix) {
		t.Fatalf("private Goal adapter leaked into argv: %#v", specs[0].Argv)
	}
	processes[0].finish(0)
	waitTaskV2Status(t, fixture.store, created.Definition.ID, "awaitingAcceptance")
}

func TestTaskEngineFormatsCodexJSONLogsAndCapturesSession(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	runners := &fakeTaskRunnerProvider{errors: map[string]error{}, parseCodexJSON: true}
	harness := newTaskEngineTestHarness(t, fixture, runners)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), "Codex structured output", "serial")
	if _, err := fixture.store.Create(t.Context(), definition, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	waitTaskV2Status(t, fixture.store, definition.ID, "running")
	processes := waitFakeProcessCount(t, harness.starter, 1)
	events := []map[string]any{
		{"type": "thread.started", "thread_id": "019c-thread-123"},
		{"type": "turn.started"},
		{"type": "item.started", "item": map[string]any{"type": "command_execution", "command": "go test ./..."}},
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "aggregated_output": "tests passed", "exit_code": 0}},
		{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "任务完成"}},
		{"type": "turn.completed"},
	}
	var output strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(encoded)
		output.WriteByte('\n')
	}
	structured := output.String()
	split := strings.Index(structured, "thread_id") + 4
	if err := processes[0].emitStdout([]byte(structured[:split])); err != nil {
		t.Fatal(err)
	}
	if err := processes[0].emitStdout([]byte(structured[split:])); err != nil {
		t.Fatal(err)
	}
	processes[0].finish(0)
	waitTaskV2Status(t, fixture.store, definition.ID, "awaitingAcceptance")

	runs, err := fixture.store.ListRuns(t.Context(), definition.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("Codex runs = %#v, %v", runs, err)
	}
	logPath, err := taskRunLogPath(fixture.store.logRoot, definition.ID, runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	result := string(rendered)
	for _, expected := range []string{"会话已连接", "正在执行任务", "go test ./...", "tests passed", "任务完成", "执行完成"} {
		if !strings.Contains(result, expected) {
			t.Fatalf("formatted logs %q do not contain %q", result, expected)
		}
	}
	if strings.Contains(result, `{"type":`) {
		t.Fatalf("raw Codex JSON leaked into logs: %q", result)
	}
	if runs[0].CliSessionID != "019c-thread-123" {
		t.Fatalf("Codex run/session = %#v, %v", runs, err)
	}
}

func TestTaskEngineStopWaitsForDurableCancellation(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), "stop execution", "serial")
	created, err := fixture.store.Create(t.Context(), definition, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	running := waitTaskV2Status(t, fixture.store, created.Definition.ID, "running")
	processes := waitFakeProcessCount(t, harness.starter, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cancelled, err := harness.engine.Stop(ctx, fixture.project.ID, definition.ID, running.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "cancelled" || cancelled.ResultCode != "cancelled" || !processes[0].isClosed() {
		t.Fatalf("cancelled task/process = %+v, closed=%v", cancelled, processes[0].isClosed())
	}
	persisted, err := fixture.store.Get(t.Context(), definition.ID)
	if err != nil || persisted.Status != "cancelled" || persisted.Revision != cancelled.Revision {
		t.Fatalf("persisted cancellation = %+v, %v", persisted, err)
	}
}

func TestTaskEngineEnforcesSerialLaneWhileAllowingParallelWork(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	definitions := []taskV2Definition{
		taskEngineTestDefinition(t, fixture.project, uuid.New(), "serial first", "serial"),
		taskEngineTestDefinition(t, fixture.project, uuid.New(), "serial second", "serial"),
		taskEngineTestDefinition(t, fixture.project, uuid.New(), "parallel", "parallel"),
	}
	for index, definition := range definitions {
		if _, err := fixture.store.Create(t.Context(), definition, time.Now().UTC().Add(time.Duration(index)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool {
		_, processes := fakeStarterSnapshot(harness.starter)
		return len(processes) == 2
	})
	prepared := harness.runners.preparedIDs()
	if !slices.Contains(prepared, definitions[0].ID) || !slices.Contains(prepared, definitions[2].ID) || slices.Contains(prepared, definitions[1].ID) {
		t.Fatalf("initial prepared tasks = %#v", prepared)
	}
	_, processes := fakeStarterSnapshot(harness.starter)
	processes[1].finish(0)
	waitTaskV2Status(t, fixture.store, definitions[2].ID, "awaitingAcceptance")
	time.Sleep(30 * time.Millisecond)
	if _, current := fakeStarterSnapshot(harness.starter); len(current) != 2 {
		t.Fatalf("second serial task started while first was active: %d", len(current))
	}
	processes[0].finish(0)
	waitTaskV2Status(t, fixture.store, definitions[0].ID, "awaitingAcceptance")
	eventually(t, 3*time.Second, func() bool {
		_, current := fakeStarterSnapshot(harness.starter)
		return len(current) == 3
	})
	_, processes = fakeStarterSnapshot(harness.starter)
	processes[2].finish(0)
	waitTaskV2Status(t, fixture.store, definitions[1].ID, "awaitingAcceptance")
}

func TestTaskEngineDoesNotLimitParallelTaskConcurrency(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	const taskCount = 6
	definitions := make([]taskV2Definition, 0, taskCount)
	for index := 0; index < taskCount; index++ {
		definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), fmt.Sprintf("parallel %d", index), "parallel")
		definitions = append(definitions, definition)
		if _, err := fixture.store.Create(t.Context(), definition, time.Now().UTC().Add(time.Duration(index)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	processes := waitFakeProcessCount(t, harness.starter, taskCount)
	for _, definition := range definitions {
		waitTaskV2Status(t, fixture.store, definition.ID, "running")
	}
	for _, process := range processes {
		process.finish(0)
	}
	for _, definition := range definitions {
		waitTaskV2Status(t, fixture.store, definition.ID, "awaitingAcceptance")
	}
}

func TestTaskEngineBlocksFailedDependenciesButRunsTheirFollowUp(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	parent := taskEngineTestDefinition(t, fixture.project, uuid.New(), "parent", "serial")
	if _, err := fixture.store.Create(t.Context(), parent, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	dependent := taskEngineTestDefinition(t, fixture.project, uuid.New(), "dependent", "serial")
	dependent.Execution.RelatedTaskIDs = []uuid.UUID{parent.ID}
	if _, err := fixture.store.Create(t.Context(), dependent, time.Now().UTC().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	waitTaskV2Status(t, fixture.store, parent.ID, "running")
	processes := waitFakeProcessCount(t, harness.starter, 1)
	processes[0].finish(7)
	waitTaskV2Status(t, fixture.store, parent.ID, "failed")
	blocked := waitTaskV2Status(t, fixture.store, dependent.ID, "blocked")
	if blocked.ResultCode != "dependency_failed" {
		t.Fatalf("dependent = %+v", blocked)
	}

	rootID := parent.ID
	followUp := taskEngineTestDefinition(t, fixture.project, uuid.New(), "follow-up", "serial")
	followUp.ParentTaskID, followUp.RootTaskID = &parent.ID, &rootID
	followUp.Execution.RelatedTaskIDs = []uuid.UUID{parent.ID}
	if _, err := fixture.store.Create(t.Context(), followUp, time.Now().UTC().Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	waitTaskV2Status(t, fixture.store, followUp.ID, "running")
	processes = waitFakeProcessCount(t, harness.starter, 2)
	processes[1].finish(0)
	waitTaskV2Status(t, fixture.store, followUp.ID, "awaitingAcceptance")
}

func TestTaskEngineUnavailableRunnerDoesNotBlockOtherKinds(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	runners := &fakeTaskRunnerProvider{errors: map[string]error{
		"hermes": taskRunnerPreparationError{code: "runner_unavailable", message: "Hermes is unavailable."},
	}}
	harness := newTaskEngineTestHarness(t, fixture, runners)
	hermes := taskEngineTestDefinition(t, fixture.project, uuid.New(), "missing Hermes", "parallel")
	hermes.Kind = "hermes"
	codex := taskEngineTestDefinition(t, fixture.project, uuid.New(), "available Codex", "parallel")
	if _, err := fixture.store.Create(t.Context(), hermes, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Create(t.Context(), codex, time.Now().UTC().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	blocked := waitTaskV2Status(t, fixture.store, hermes.ID, "blocked")
	if blocked.ResultCode != "runner_unavailable" {
		t.Fatalf("Hermes task = %+v", blocked)
	}
	waitTaskV2Status(t, fixture.store, codex.ID, "running")
	processes := waitFakeProcessCount(t, harness.starter, 1)
	processes[0].finish(0)
	waitTaskV2Status(t, fixture.store, codex.ID, "awaitingAcceptance")
}

func TestTaskEngineBlocksWhenInteractiveExecutionContextIsUnavailable(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	harness.starter.startErr = fmt.Errorf("launch task: %w", errTaskExecutionContextUnavailable)
	definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), "wait for desktop login", "serial")
	created, err := fixture.store.Create(t.Context(), definition, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	blocked := waitTaskV2Status(t, fixture.store, created.Definition.ID, "blocked")
	if blocked.ResultCode != "execution_context_unavailable" || blocked.ExitCode == nil || *blocked.ExitCode != 127 {
		t.Fatalf("execution-context failure = %+v", blocked)
	}
	runtimeDirectory := fixture.state.path + ".task-runtime"
	entries, err := os.ReadDir(runtimeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("private prompt was not cleaned after blocked launch: %#v", entries)
	}
}

func TestTaskEngineHonorsDueScheduleAndLeavesFutureTaskQueued(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	due := taskEngineTestDefinition(t, fixture.project, uuid.New(), "due", "serial")
	due.Execution.RunImmediately = false
	dueAt := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	due.Execution.ScheduledAt = &dueAt
	future := taskEngineTestDefinition(t, fixture.project, uuid.New(), "future", "serial")
	future.Execution.RunImmediately = false
	futureAt := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	future.Execution.ScheduledAt = &futureAt
	if _, err := fixture.store.Create(t.Context(), due, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	futureRecord, err := fixture.store.Create(t.Context(), future, time.Now().UTC().Add(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	running := waitTaskV2Status(t, fixture.store, due.ID, "running")
	if running.Definition.Execution.ScheduledAt != nil || running.DefinitionRevision < 2 {
		t.Fatalf("due task retained schedule = %+v", running)
	}
	processes := waitFakeProcessCount(t, harness.starter, 1)
	time.Sleep(30 * time.Millisecond)
	stillFuture, err := fixture.store.Get(t.Context(), future.ID)
	if err != nil || stillFuture.Status != "queued" || stillFuture.Revision != futureRecord.Revision || stillFuture.Definition.Execution.ScheduledAt == nil {
		t.Fatalf("future task = %+v, %v", stillFuture, err)
	}
	processes[0].finish(0)
	waitTaskV2Status(t, fixture.store, due.ID, "awaitingAcceptance")

	waiting, err := fixture.store.Transition(t.Context(), future.ID, stillFuture.Revision, "waiting", "", time.Now().UTC())
	if err != nil || waiting.Definition.Execution.ScheduledAt != nil {
		t.Fatalf("manual future activation = %+v, %v", waiting, err)
	}
	harness.engine.Wake()
	waitTaskV2Status(t, fixture.store, future.ID, "running")
	processes = waitFakeProcessCount(t, harness.starter, 2)
	processes[1].finish(0)
	waitTaskV2Status(t, fixture.store, future.ID, "awaitingAcceptance")
}

func TestTaskEnginePolicyRevocationStopsActiveTask(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), "revoked", "serial")
	if _, err := fixture.store.Create(t.Context(), definition, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	waitTaskV2Status(t, fixture.store, definition.ID, "running")
	processes := waitFakeProcessCount(t, harness.starter, 1)
	policy := fixture.project.Policy
	policy.AllowTaskExecution = false
	expectedRevision := fixture.project.Revision
	if _, err := fixture.state.business.updateProject(t.Context(), fixture.project.ID, nil, nil, &policy, &expectedRevision); err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	blocked := waitTaskV2Status(t, fixture.store, definition.ID, "blocked")
	if blocked.ResultCode != "policy_revoked" || !processes[0].isClosed() {
		t.Fatalf("revoked task/process = %+v, closed=%v", blocked, processes[0].isClosed())
	}
}

func TestTaskEngineCloseDurablyFinishesActiveTask(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	harness := newTaskEngineTestHarness(t, fixture, nil)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	definition := taskEngineTestDefinition(t, fixture.project, uuid.New(), "shutdown", "serial")
	if _, err := fixture.store.Create(t.Context(), definition, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	waitTaskV2Status(t, fixture.store, definition.ID, "running")
	processes := waitFakeProcessCount(t, harness.starter, 1)
	if err := harness.engine.Close(); err != nil {
		t.Fatal(err)
	}
	failed := waitTaskV2Status(t, fixture.store, definition.ID, "failed")
	if failed.ResultCode != "agent_shutdown" || !processes[0].isClosed() {
		t.Fatalf("shutdown task/process = %+v, closed=%v", failed, processes[0].isClosed())
	}
}

func TestTaskEngineExecutesWorkflowNodesWithoutLaunchingParentProcess(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, time.Now().UTC())
	if _, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	harness := newTaskEngineTestHarness(t, fixture, nil)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	processes := waitFakeProcessCount(t, harness.starter, 1)
	runningParent := waitTaskV2Status(t, fixture.store, parent.ID, "running")
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || snapshot.Revision.ID != revision.ID || len(snapshot.ChildTasks) != 1 || snapshot.ChildTasks[0].Status != "running" {
		t.Fatalf("running workflow snapshot = %+v, %v", snapshot, err)
	}
	prepared := harness.runners.preparedIDs()
	if slices.Contains(prepared, parent.ID) || len(prepared) != 1 || prepared[0] != snapshot.ChildTasks[0].Definition.ID {
		t.Fatalf("workflow prepared IDs = %#v", prepared)
	}
	processes[0].finish(0)
	childAwaiting := waitTaskV2Status(t, fixture.store, snapshot.ChildTasks[0].Definition.ID, "awaitingAcceptance")
	if _, _, err := fixture.store.Accept(t.Context(), childAwaiting.Definition.ID, childAwaiting.Revision,
		[]byte("workflow child accepted"), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	harness.engine.Wake()
	completedParent := waitTaskV2Status(t, fixture.store, parent.ID, "awaitingAcceptance")
	if completedParent.ResultCode != "workflow_succeeded" || completedParent.Revision <= runningParent.Revision {
		t.Fatalf("completed workflow parent = %+v", completedParent)
	}
	completedSnapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || completedSnapshot.TaskRun.WorkflowRevisionID == nil || *completedSnapshot.TaskRun.WorkflowRevisionID != revision.ID {
		t.Fatalf("completed workflow snapshot = %+v, %v", completedSnapshot, err)
	}
	statuses := map[string]string{}
	for _, run := range completedSnapshot.NodeRuns {
		statuses[run.NodeID] = run.Status
	}
	if statuses["execute"] != "succeeded" || statuses["recover"] != "skipped" || statuses["finish"] != "succeeded" {
		t.Fatalf("completed workflow node statuses = %#v", statuses)
	}
}

func TestTaskEngineWorkflowStopWaitsForRunningChildAndSealsDAG(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, time.Now().UTC())
	if _, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	harness := newTaskEngineTestHarness(t, fixture, nil)
	if err := harness.engine.Start(); err != nil {
		t.Fatal(err)
	}
	processes := waitFakeProcessCount(t, harness.starter, 1)
	runningParent := waitTaskV2Status(t, fixture.store, parent.ID, "running")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cancelled, err := harness.engine.Stop(ctx, fixture.project.ID, parent.ID, runningParent.Revision)
	if err != nil || cancelled.Status != "cancelled" || cancelled.ResultCode != "cancelled" || !processes[0].isClosed() {
		t.Fatalf("cancelled workflow = %+v, %v, process-closed=%v", cancelled, err, processes[0].isClosed())
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || snapshot.TaskRun.Status != "cancelled" || len(snapshot.ChildTasks) != 1 || snapshot.ChildTasks[0].Status != "cancelled" {
		t.Fatalf("cancelled workflow snapshot = %+v, %v", snapshot, err)
	}
	statuses := map[string]string{}
	for _, run := range snapshot.NodeRuns {
		statuses[run.NodeID] = run.Status
	}
	if statuses["start"] != "succeeded" || statuses["execute"] != "cancelled" || statuses["recover"] != "cancelled" || statuses["finish"] != "cancelled" {
		t.Fatalf("cancelled workflow node statuses = %#v", statuses)
	}
	time.Sleep(30 * time.Millisecond)
	if _, current := fakeStarterSnapshot(harness.starter); len(current) != 1 {
		t.Fatalf("workflow scheduled another child after cancellation: %d", len(current))
	}
}
