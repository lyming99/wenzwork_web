package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	defaultTaskEnginePollInterval        = 500 * time.Millisecond
	defaultTaskMaximumLifetime           = 24 * time.Hour
	defaultTaskMaximumMemoryBytes uint64 = 4 << 30
	defaultTaskMaximumOutputBytes uint64 = 64 << 20
)

type taskEngine struct {
	state      *agentState
	store      *taskV2Store
	supervisor *rawProcessSupervisor
	runners    taskRunnerProvider

	now               func() time.Time
	pollInterval      time.Duration
	limits            processResourceLimits
	logRoot           string
	wake              chan struct{}
	closeCh           chan struct{}
	startOnce         sync.Once
	closeOnce         sync.Once
	backgroundContext context.Context
	cancelBackground  context.CancelFunc
	wg                sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	startErr error
	active   map[uuid.UUID]*activeTaskRun
}

type activeTaskRun struct {
	task           taskV2Record
	run            taskV2Run
	project        registeredProject
	process        *rawSupervisedProcess
	prompt         managedTaskPrompt
	parseCodexJSON bool
	logFile        *taskRunLogWriter
	done           chan struct{}
	doneOnce       sync.Once
	stopState      string
	stopCode       string
	result         taskV2Record
	err            error
}

func newTaskEngine(state *agentState, supervisor *rawProcessSupervisor, runners taskRunnerProvider) *taskEngine {
	ctx, cancel := context.WithCancel(context.Background())
	logRoot := ""
	if state != nil {
		logRoot, _ = taskRunLogRootForStateFile(state.path)
	}
	return &taskEngine{
		state: state, store: state.tasksV2, supervisor: supervisor, runners: runners,
		now: func() time.Time { return time.Now().UTC() }, pollInterval: defaultTaskEnginePollInterval,
		limits: processResourceLimits{
			MaximumLifetime: defaultTaskMaximumLifetime, MaximumMemoryBytes: defaultTaskMaximumMemoryBytes,
			MaximumOutputBytes: defaultTaskMaximumOutputBytes,
		},
		logRoot: logRoot,
		wake:    make(chan struct{}, 1), closeCh: make(chan struct{}), active: make(map[uuid.UUID]*activeTaskRun),
		backgroundContext: ctx, cancelBackground: cancel,
	}
}

func (engine *taskEngine) Start() error {
	if engine == nil || engine.state == nil || engine.store == nil || engine.supervisor == nil || engine.runners == nil || engine.logRoot == "" ||
		engine.pollInterval <= 0 {
		return errRPCCapability
	}
	engine.startOnce.Do(func() {
		if err := cleanupManagedTaskPrompts(engine.state.path); err != nil {
			engine.mu.Lock()
			engine.startErr = fmt.Errorf("clean stale private task prompts: %w", err)
			engine.mu.Unlock()
			return
		}
		engine.wg.Add(2)
		go func() {
			defer engine.wg.Done()
			engine.runners.Refresh(engine.backgroundContext)
		}()
		go func() {
			defer engine.wg.Done()
			engine.loop()
		}()
	})
	engine.mu.Lock()
	startErr := engine.startErr
	closed := engine.closed
	engine.mu.Unlock()
	if closed && startErr == nil {
		return errRPCCapability
	}
	if startErr == nil {
		engine.Wake()
	}
	return startErr
}

func (engine *taskEngine) Wake() {
	if engine == nil {
		return
	}
	select {
	case engine.wake <- struct{}{}:
	default:
	}
}

func (engine *taskEngine) loop() {
	ticker := time.NewTicker(engine.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-engine.closeCh:
			return
		case <-engine.wake:
		case <-ticker.C:
		}
		if err := engine.schedule(engine.backgroundContext); err != nil {
			// Scheduler errors are intentionally not printed: errors may contain
			// device-local paths. Persisted task state and the next bounded retry
			// are the observable recovery mechanism.
			continue
		}
	}
}

func (engine *taskEngine) schedule(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	projects, err := engine.state.business.listProjects(ctx, false)
	if err != nil {
		return err
	}
	projectByID := make(map[uuid.UUID]registeredProject, len(projects))
	for _, project := range projects {
		if project.State == "available" && project.Policy.AllowTaskExecution {
			projectByID[project.ID] = project
		}
	}
	if !agentFeatureFlags(engine.state)["tasks.v2"] {
		engine.enforceRevokedPolicies(map[uuid.UUID]registeredProject{})
		return nil
	}
	engine.enforceRevokedPolicies(projectByID)
	now := engine.now()
	allTasks := make([]taskV2Record, 0)
	for _, project := range projectByID {
		tasks, listErr := engine.store.List(ctx, project.ID)
		if listErr != nil {
			return listErr
		}
		for index := range tasks {
			task := tasks[index]
			if task.Status == "queued" && taskShouldEnterQueue(task, now) {
				promoted, transitionErr := engine.store.Transition(ctx, task.Definition.ID, task.Revision, "waiting", "", now)
				if transitionErr == nil {
					task = promoted
				} else if transitionErr != errRPCRevision {
					return transitionErr
				}
			}
			allTasks = append(allTasks, task)
		}
	}
	if err := engine.markBlockedDependencies(ctx, allTasks, projectByID, now); err != nil {
		return err
	}
	// Re-read after promotions/blocks so every launch claim uses authoritative
	// revisions and sees dependency changes made by another RPC concurrently.
	allTasks = allTasks[:0]
	for _, project := range projectByID {
		tasks, listErr := engine.store.List(ctx, project.ID)
		if listErr != nil {
			return listErr
		}
		allTasks = append(allTasks, tasks...)
	}
	sort.Slice(allTasks, func(left, right int) bool {
		if allTasks[left].CreatedAt.Equal(allTasks[right].CreatedAt) {
			return allTasks[left].Definition.ID.String() < allTasks[right].Definition.ID.String()
		}
		return allTasks[left].CreatedAt.Before(allTasks[right].CreatedAt)
	})
	for _, task := range allTasks {
		if task.Status != "waiting" || task.Definition.Kind != "workflow" || task.Definition.Scope != "topLevel" ||
			!taskDependenciesSatisfied(task, allTasks) || taskHasFutureSchedule(task, now) {
			continue
		}
		if _, _, _, startErr := engine.store.StartWorkflowRun(ctx, task.Definition.ID, task.Revision, now); startErr != nil &&
			startErr != errRPCRevision && startErr != errRPCBusy {
			return startErr
		}
	}
	allTasks = allTasks[:0]
	for _, project := range projectByID {
		tasks, listErr := engine.store.List(ctx, project.ID)
		if listErr != nil {
			return listErr
		}
		allTasks = append(allTasks, tasks...)
	}
	for _, task := range allTasks {
		switch {
		case task.Definition.Kind == "workflow" && task.Definition.Scope == "topLevel" && task.Status == "running":
			// The task engine does not impose a concurrency ceiling. The graph's
			// optional MaximumParallelism remains authoritative when explicitly set.
			if _, tickErr := engine.store.TickWorkflow(ctx, task.Definition.ID, maximumWorkflowV2Nodes, now); tickErr != nil && tickErr != errRPCRevision {
				return tickErr
			}
		case task.Definition.Kind == "workflow" && task.Definition.Scope == "topLevel" && task.Status == "cancelled" && task.CurrentRunID != nil:
			if _, finalizeErr := engine.store.FinalizeCancelledWorkflow(ctx, task.Definition.ID, task.Revision, now); finalizeErr != nil &&
				finalizeErr != errRPCRevision {
				return finalizeErr
			}
		}
	}
	allTasks = allTasks[:0]
	for _, project := range projectByID {
		tasks, listErr := engine.store.List(ctx, project.ID)
		if listErr != nil {
			return listErr
		}
		allTasks = append(allTasks, tasks...)
	}
	sort.Slice(allTasks, func(left, right int) bool {
		if allTasks[left].CreatedAt.Equal(allTasks[right].CreatedAt) {
			return allTasks[left].Definition.ID.String() < allTasks[right].Definition.ID.String()
		}
		return allTasks[left].CreatedAt.Before(allTasks[right].CreatedAt)
	})
	for _, task := range allTasks {
		if task.Status != "waiting" || task.Definition.Kind == "workflow" ||
			!taskDependenciesSatisfied(task, allTasks) || taskHasFutureSchedule(task, now) {
			continue
		}
		project, found := projectByID[task.Definition.ProjectID]
		if !found || !engine.hasCapacityFor(task) {
			continue
		}
		if err := engine.launch(ctx, project, task); err != nil && err != errRPCRevision && err != errRPCBusy {
			return err
		}
	}
	return nil
}

func taskShouldEnterQueue(task taskV2Record, now time.Time) bool {
	scheduled := task.Definition.Execution.ScheduledAt
	if scheduled != nil {
		return !scheduled.After(now)
	}
	return task.Definition.Execution.RunImmediately
}

func taskHasFutureSchedule(task taskV2Record, now time.Time) bool {
	return task.Definition.Execution.ScheduledAt != nil && task.Definition.Execution.ScheduledAt.After(now)
}

func (engine *taskEngine) hasCapacityFor(candidate taskV2Record) bool {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return false
	}
	projectHasTaskRun := false
	for _, active := range engine.active {
		if active.project.ID == candidate.Definition.ProjectID {
			projectHasTaskRun = projectHasTaskRun || active.task.Definition.Kind != "workflow"
		}
	}
	if candidate.Definition.Execution.Mode == "parallel" {
		return true
	}
	return !projectHasTaskRun
}

func (engine *taskEngine) launch(ctx context.Context, project registeredProject, waiting taskV2Record) error {
	running, run, err := engine.store.StartRun(ctx, waiting.Definition.ID, waiting.Revision, engine.now())
	if err != nil {
		return err
	}
	active := &activeTaskRun{task: running, run: run, project: project, done: make(chan struct{})}
	engine.mu.Lock()
	if engine.closed {
		engine.mu.Unlock()
		return engine.finishWithoutProcess(active, "failed", -1, "agent_shutdown", errors.New("task engine is closed"))
	}
	if _, exists := engine.active[running.Definition.ID]; exists {
		engine.mu.Unlock()
		return engine.finishWithoutProcess(active, "failed", -1, "duplicate_claim", errors.New("task already active"))
	}
	engine.active[running.Definition.ID] = active
	engine.mu.Unlock()
	active.logFile, err = engine.store.OpenRunLogWriter(ctx, running, run, func(error) {
		engine.mu.Lock()
		process := active.process
		if active.stopState == "" {
			active.stopState, active.stopCode = "failed", "log_store_error"
		}
		engine.mu.Unlock()
		if process != nil {
			_ = process.Close("log_store_error")
		}
	})
	if err != nil {
		_ = engine.store.markRunLogMissing(context.Background(), running.Definition.ID, run.ID, run.LogGeneration, 0, engine.now())
		return engine.finishWithoutProcess(active, "failed", 126, "log_store_error", err)
	}

	var managedInput []byte
	if running.Definition.Kind == "script" {
		managedInput, err = buildTaskV2ScriptInput(running)
	} else {
		managedInput, err = buildTaskV2Prompt(project, running)
	}
	if err != nil {
		return engine.finishPreparationFailure(active, err)
	}
	promptPath, err := managedTaskPromptPath(engine.state.path, running.Definition.ID, run.ID)
	if err != nil {
		return engine.finishWithoutProcess(active, "failed", 126, "prompt_store_failed", err)
	}
	invocation, err := engine.runners.Prepare(ctx, project, running, promptPath)
	if err != nil {
		return engine.finishPreparationFailure(active, err)
	}
	engine.mu.Lock()
	stopState, stopCode := active.stopState, active.stopCode
	engine.mu.Unlock()
	if stopState != "" {
		return engine.finishWithoutProcess(active, stopState, 130, stopCode, nil)
	}
	managedInput, err = applyTaskRunnerPromptPrefix(managedInput, invocation.PromptPrefix)
	if err != nil {
		return engine.finishPreparationFailure(active, err)
	}
	managed, err := createManagedTaskPrompt(engine.state.path, running.Definition.ID, run.ID, managedInput)
	if err != nil {
		return engine.finishWithoutProcess(active, "failed", 126, "prompt_store_failed", err)
	}
	active.prompt = managed
	workingDirectory, _, err := secureExistingProjectPath(project, running.Definition.CWD)
	if err != nil {
		return engine.finishWithoutProcess(active, "blocked", 126, "working_directory_unavailable", err)
	}
	arguments := append([]string{invocation.Executable}, invocation.Arguments...)
	privateStdinPath := ""
	if invocation.UseStdinFile {
		privateStdinPath = promptPath
	}
	process, err := engine.supervisor.Start(rawProcessLaunchSpec{
		ProjectID: project.ID, ProjectRoot: project.LocalPath, WorkingDirectory: workingDirectory,
		Argv: arguments, Environment: invocation.Environment, PrivateStdinPath: privateStdinPath,
		IgnoreConcurrencyLimit: true, Limits: engine.limits,
	})
	if err != nil {
		code := "runner_start_failed"
		status := "failed"
		if err == errRPCBusy {
			code, status = "process_capacity", "blocked"
		} else if errors.Is(err, errTaskExecutionContextUnavailable) {
			code, status = "execution_context_unavailable", "blocked"
		}
		return engine.finishWithoutProcess(active, status, 127, code, err)
	}
	engine.mu.Lock()
	active.process = process
	stopState, stopCode = active.stopState, active.stopCode
	engine.mu.Unlock()
	if stopState != "" {
		_ = process.Close(stopCode)
	}
	if err := engine.appendActiveLog(active, "system", []byte("[WenzWork] Task execution started.\n")); err != nil {
		_ = process.Close("log_store_error")
	}
	active.parseCodexJSON = invocation.ParseCodexJSON
	if invocation.CliSessionID != "" {
		active.task.Definition.Execution.CliSessionID = invocation.CliSessionID
	}
	engine.wg.Add(1)
	go func() {
		defer engine.wg.Done()
		engine.runActive(active)
	}()
	return nil
}

func (engine *taskEngine) finishPreparationFailure(active *activeTaskRun, err error) error {
	var preparation taskRunnerPreparationError
	if errors.As(err, &preparation) {
		status := "blocked"
		if preparation.code == "prompt_empty" || preparation.code == "config_invalid" {
			status = "failed"
		}
		return engine.finishWithoutProcess(active, status, 126, preparation.code, err)
	}
	return engine.finishWithoutProcess(active, "failed", 126, "runner_prepare_failed", err)
}

func (engine *taskEngine) finishWithoutProcess(active *activeTaskRun, status string, exitCode int, resultCode string, cause error) error {
	if active == nil {
		return cause
	}
	if active.prompt.Path != "" {
		_ = active.prompt.Cleanup()
	}
	message := "[WenzWork] Task execution could not start."
	if active.logFile != nil {
		_ = active.logFile.Append(context.Background(), "system", message, nil, engine.now())
		_, _ = engine.store.SealRunLog(context.Background(), active.task, active.run)
	}
	result, _, finishErr := engine.store.FinishRun(context.Background(), active.task.Definition.ID, active.task.Revision, status, exitCode, resultCode, "", engine.now())
	engine.completeActive(active, result, finishErr)
	if finishErr != nil {
		return finishErr
	}
	return nil
}

func (engine *taskEngine) runActive(active *activeTaskRun) {
	readDone := make(chan error, 1)
	go func() { readDone <- engine.readTaskOutput(active) }()
	exitCode := active.process.Wait()
	var readErr error
	select {
	case readErr = <-readDone:
	case <-time.After(750 * time.Millisecond):
		_ = active.process.Close("process_exit")
		readErr = <-readDone
	}
	reason := active.process.reason()
	_ = active.process.Close("process_exit")
	active.process.release()
	_, sealErr := engine.store.SealRunLog(context.Background(), active.task, active.run)
	readErr = errors.Join(readErr, sealErr)
	if active.prompt.Path != "" {
		_ = active.prompt.Cleanup()
	}

	engine.mu.Lock()
	stopState, stopCode := active.stopState, active.stopCode
	engine.mu.Unlock()
	status, resultCode := taskRunCompletionStatus(stopState, stopCode, reason, readErr, exitCode)
	cliSessionID := active.task.Definition.Execution.CliSessionID
	result, _, err := engine.store.FinishRun(context.Background(), active.task.Definition.ID, active.task.Revision, status, exitCode, resultCode, cliSessionID, engine.now())
	engine.completeActive(active, result, err)
	engine.Wake()
}

func taskRunCompletionStatus(stopState, stopCode, processReason string, readErr error, exitCode int) (string, string) {
	if stopState != "" {
		return stopState, stopCode
	}
	// Output readers return the same error that caused them to close the
	// process. Preserve the supervisor's specific reason (especially
	// output_limit) instead of flattening every such termination to a generic
	// log_store_error.
	if processReason != "" && processReason != "process_exit" {
		return taskStatusForProcessReason(processReason)
	}
	if readErr != nil {
		return "failed", "log_store_error"
	}
	if exitCode == 0 {
		return "awaitingAcceptance", "execution_succeeded"
	}
	return "failed", "runner_exit"
}

func taskStatusForProcessReason(reason string) (string, string) {
	switch reason {
	case "memory_limit", "output_limit", "lifetime_limit", "log_store_error":
		return "failed", reason
	case "policy_revoked":
		return "blocked", reason
	case "agent_exit", "agent_shutdown":
		return "failed", "agent_shutdown"
	case "cancelled":
		return "cancelled", "cancelled"
	default:
		return "failed", "runner_exit"
	}
}

func (engine *taskEngine) readTaskOutput(active *activeTaskRun) error {
	if active == nil || active.process == nil {
		return errRPCCapability
	}
	readStream := func(stream string, reader io.Reader, parseCodexJSON bool) error {
		decoder := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: !parseCodexJSON})
		var formatter *codexJSONOutputFormatter
		if parseCodexJSON {
			formatter = newCodexJSONOutputFormatter()
		}
		appendResult := func(result CommandTextDecodeResult) error {
			display := result.DisplayText
			if formatter != nil && !result.IsBinary {
				display = strings.Join(formatter.Feed([]byte(display)), "")
			}
			if len(result.RawBytes) == 0 && display == "" {
				return nil
			}
			raw := result.RawBytes
			if len(raw) > maximumTaskLogEntryBytes {
				raw = raw[:maximumTaskLogEntryBytes]
				result.HadDecodeErrors = true
			}
			var binary []byte
			if result.IsBinary {
				binary = raw
			}
			err := active.logFile.Append(context.Background(), stream, display, binary, engine.now())
			if err != nil {
				reason := "log_store_error"
				if errors.Is(err, errTaskLogOutputLimit) {
					reason = "output_limit"
				}
				_ = active.process.Close(reason)
			}
			return err
		}
		buffer := make([]byte, 32<<10)
		for {
			n, readErr := reader.Read(buffer)
			if n > 0 {
				for _, result := range decoder.Feed(buffer[:n]) {
					if err := appendResult(result); err != nil {
						return err
					}
				}
			}
			if readErr == nil {
				continue
			}
			if !errors.Is(readErr, io.EOF) && active.process.reason() == "" {
				_ = active.process.Close("log_store_error")
				return readErr
			}
			break
		}
		for _, result := range decoder.Flush() {
			if err := appendResult(result); err != nil {
				return err
			}
		}
		if formatter != nil {
			for _, formatted := range formatter.Flush() {
				if err := appendResult(CommandTextDecodeResult{
					DisplayText: formatted, SourceEncoding: "utf-8",
				}); err != nil {
					return err
				}
			}
			if sessionID := formatter.SessionID(); sessionID != "" {
				active.task.Definition.Execution.CliSessionID = sessionID
			}
		}
		return nil
	}
	errCh := make(chan error, 2)
	go func() { errCh <- readStream("stdout", active.process.Stdout(), active.parseCodexJSON) }()
	go func() { errCh <- readStream("stderr", active.process.Stderr(), false) }()
	return errors.Join(<-errCh, <-errCh)
}

func (engine *taskEngine) completeActive(active *activeTaskRun, result taskV2Record, err error) {
	engine.mu.Lock()
	active.result, active.err = result, err
	if engine.active[active.task.Definition.ID] == active {
		delete(engine.active, active.task.Definition.ID)
	}
	engine.mu.Unlock()
	active.doneOnce.Do(func() { close(active.done) })
}

func (engine *taskEngine) appendActiveLog(active *activeTaskRun, stream string, contents []byte) error {
	if engine == nil || active == nil || active.logFile == nil {
		return errRPCCapability
	}
	return active.logFile.Append(context.Background(), stream, string(contents), nil, engine.now())
}

func (engine *taskEngine) Stop(ctx context.Context, projectID, taskID uuid.UUID, expectedRevision uint64) (taskV2Record, error) {
	if engine == nil || projectID == uuid.Nil || taskID == uuid.Nil || expectedRevision == 0 {
		return taskV2Record{}, errRPCInvalid
	}
	task, err := engine.store.Get(ctx, taskID)
	if err != nil {
		return taskV2Record{}, err
	}
	if task.Definition.ProjectID != projectID {
		return taskV2Record{}, errRPCProject
	}
	if task.Revision != expectedRevision {
		return taskV2Record{}, errRPCRevision
	}
	if task.Status != "running" {
		return engine.store.Transition(ctx, taskID, expectedRevision, "cancelled", "cancelled", engine.now())
	}
	if task.Definition.Kind == "workflow" {
		cancelled, cancelErr := engine.store.CancelWorkflow(ctx, taskID, expectedRevision, engine.now())
		if cancelErr != nil {
			return taskV2Record{}, cancelErr
		}
		for _, child := range cancelled.RunningChildren {
			if _, stopErr := engine.Stop(ctx, projectID, child.Definition.ID, child.Revision); stopErr != nil &&
				stopErr != errRPCRevision && stopErr != errRPCNotFound {
				return cancelled.Task, stopErr
			}
		}
		finalized, finalizeErr := engine.store.FinalizeCancelledWorkflow(ctx, taskID, cancelled.Task.Revision, engine.now())
		if finalizeErr == errRPCRevision {
			return engine.store.Get(ctx, taskID)
		}
		return finalized, finalizeErr
	}
	engine.mu.Lock()
	active := engine.active[taskID]
	if active == nil || active.task.Revision != expectedRevision {
		engine.mu.Unlock()
		return taskV2Record{}, errRPCBusy
	}
	if active.stopState == "" {
		active.stopState, active.stopCode = "cancelled", "cancelled"
	}
	process := active.process
	engine.mu.Unlock()
	if process != nil {
		_ = process.Close("cancelled")
	}
	select {
	case <-ctx.Done():
		return taskV2Record{}, ctx.Err()
	case <-active.done:
		return active.result, active.err
	}
}

func (engine *taskEngine) enforceRevokedPolicies(projects map[uuid.UUID]registeredProject) {
	engine.mu.Lock()
	active := make([]*activeTaskRun, 0)
	for _, run := range engine.active {
		if _, allowed := projects[run.project.ID]; allowed {
			continue
		}
		if run.stopState == "" {
			run.stopState, run.stopCode = "blocked", "policy_revoked"
		}
		active = append(active, run)
	}
	engine.mu.Unlock()
	for _, run := range active {
		if run.process != nil {
			_ = run.process.Close("policy_revoked")
		}
	}
}

func (engine *taskEngine) Close() error {
	if engine == nil {
		return nil
	}
	engine.closeOnce.Do(func() {
		engine.mu.Lock()
		engine.closed = true
		close(engine.closeCh)
		engine.cancelBackground()
		active := make([]*activeTaskRun, 0, len(engine.active))
		for _, run := range engine.active {
			if run.stopState == "" {
				run.stopState, run.stopCode = "failed", "agent_shutdown"
			}
			active = append(active, run)
		}
		engine.mu.Unlock()
		for _, run := range active {
			if run.process != nil {
				_ = run.process.Close("agent_shutdown")
			}
		}
		engine.wg.Wait()
	})
	return nil
}

func (engine *taskEngine) markBlockedDependencies(
	ctx context.Context,
	tasks []taskV2Record,
	projects map[uuid.UUID]registeredProject,
	now time.Time,
) error {
	for _, task := range tasks {
		if task.Status != "waiting" {
			continue
		}
		missing, failed := taskDependencyProblem(task, tasks)
		if !missing && !failed {
			continue
		}
		if _, allowed := projects[task.Definition.ProjectID]; !allowed {
			continue
		}
		code := "dependency_failed"
		if missing {
			code = "dependency_missing"
		}
		_, err := engine.store.Transition(ctx, task.Definition.ID, task.Revision, "blocked", code, now)
		if err == errRPCRevision {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func taskDependencyProblem(task taskV2Record, all []taskV2Record) (missing, failed bool) {
	byID := taskRecordMap(all)
	if task.Definition.Execution.Relation == "sibling" {
		for _, siblingID := range task.Definition.Execution.RelatedTaskIDs {
			sibling, found := byID[siblingID]
			if !found || sibling.Definition.ProjectID != task.Definition.ProjectID {
				return true, false
			}
		}
	}
	for _, dependencyID := range taskDependencyIDs(task, byID, make(map[uuid.UUID]bool)) {
		dependency, found := byID[dependencyID]
		if !found || dependency.Definition.ProjectID != task.Definition.ProjectID {
			return true, false
		}
		if taskDependencySatisfied(task, dependency) {
			continue
		}
		if slices.Contains([]string{"failed", "blocked", "cancelled"}, dependency.Status) {
			return false, true
		}
	}
	return false, false
}

func taskDependenciesSatisfied(task taskV2Record, all []taskV2Record) bool {
	missing, failed := taskDependencyProblem(task, all)
	if missing || failed {
		return false
	}
	byID := taskRecordMap(all)
	for _, dependencyID := range taskDependencyIDs(task, byID, make(map[uuid.UUID]bool)) {
		dependency, found := byID[dependencyID]
		if !found || !taskDependencySatisfied(task, dependency) {
			return false
		}
	}
	return true
}

func taskDependencySatisfied(task, dependency taskV2Record) bool {
	if slices.Contains([]string{"awaitingAcceptance", "changesRequested", "completed", "succeeded"}, dependency.Status) {
		return true
	}
	return slices.Contains([]string{"failed", "blocked", "cancelled"}, dependency.Status) &&
		task.Definition.ParentTaskID != nil && *task.Definition.ParentTaskID == dependency.Definition.ID &&
		task.Definition.Execution.Relation == "dependency" && slices.Contains(task.Definition.Execution.RelatedTaskIDs, dependency.Definition.ID)
}

func taskDependencyIDs(task taskV2Record, byID map[uuid.UUID]taskV2Record, visiting map[uuid.UUID]bool) []uuid.UUID {
	if visiting[task.Definition.ID] {
		return nil
	}
	visiting[task.Definition.ID] = true
	set := make(map[uuid.UUID]struct{})
	if task.Definition.Execution.Relation == "dependency" {
		for _, id := range task.Definition.Execution.RelatedTaskIDs {
			set[id] = struct{}{}
		}
	} else {
		for _, siblingID := range task.Definition.Execution.RelatedTaskIDs {
			if sibling, found := byID[siblingID]; found {
				for _, id := range taskDependencyIDs(sibling, byID, visiting) {
					set[id] = struct{}{}
				}
			}
		}
	}
	delete(visiting, task.Definition.ID)
	delete(set, task.Definition.ID)
	result := make([]uuid.UUID, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].String() < result[right].String() })
	return result
}

func taskRecordMap(tasks []taskV2Record) map[uuid.UUID]taskV2Record {
	result := make(map[uuid.UUID]taskV2Record, len(tasks))
	for _, task := range tasks {
		result[task.Definition.ID] = task
	}
	return result
}
