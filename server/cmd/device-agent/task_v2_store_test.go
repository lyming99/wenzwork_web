package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type taskV2StoreFixture struct {
	state   *agentState
	store   *taskV2Store
	project registeredProject
	now     time.Time
}

func newTaskV2StoreFixture(t *testing.T) taskV2StoreFixture {
	t.Helper()
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	workspace := filepath.Join(directory, "legacy-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(directory, "task-project")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "context.md"), []byte("task context"), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := state.business.addProject(t.Context(), projectRoot, "Task project", "", projectPolicy{AllowTaskExecution: true})
	if err != nil {
		t.Fatal(err)
	}
	store := newTaskV2Store(state.business)
	t.Cleanup(func() {
		// This fixture owns a store separate from the one created while loading
		// agentState. Drain its task-log checkpoint/event workers before the
		// state's transfer manager, event pumps, BusinessStore, and TempDir are
		// closed.
		store.closeTaskLogRuntime()
		if err := state.close(); err != nil {
			t.Errorf("close task store fixture: %v", err)
		}
	})
	return taskV2StoreFixture{
		state: state, store: store, project: project,
		now: time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC),
	}
}

func normalizeTaskV2TestDefinition(t *testing.T, project registeredProject, id uuid.UUID) taskV2Definition {
	t.Helper()
	scheduledAt := time.Date(2026, 8, 9, 10, 30, 0, 0, time.FixedZone("test", 8*60*60))
	definition, err := normalizeTaskV2Definition(project, taskV2Definition{
		ID: id, ProjectID: project.ID, Kind: "codex", Title: "Review task", CWD: ".", Scope: "topLevel",
		Config: json.RawMessage(`{
            "promptSource":"customText",
            "promptText":"Review the current project",
            "attachedFilePaths":["context.md"],
            "model":"gpt-5",
            "launchMode":"cli",
            "goalMode":true,
            "reasoningEffort":"high"
        }`),
		Execution: taskV2ExecutionOptions{
			Relation: "dependency", Mode: "serial", RunImmediately: false, ScheduledAt: &scheduledAt,
		},
		Environment: map[string]string{"TASK_TEST_TOKEN": "device-only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func TestTaskV2StoreLifecycleCASLogsAndDeletion(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())

	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "queued" || created.Revision != 1 || created.DefinitionRevision != 1 || created.ChangeSequence == 0 {
		t.Fatalf("created task = %+v", created)
	}
	if _, err := fixture.store.Create(t.Context(), definition, fixture.now.Add(time.Second)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("duplicate create error = %v", err)
	}
	listed, err := fixture.store.List(t.Context(), fixture.project.ID)
	if err != nil || len(listed) != 1 || listed[0].Definition.ID != definition.ID || listed[0].ChangeSequence != created.ChangeSequence {
		t.Fatalf("listed tasks = %+v, %v", listed, err)
	}

	left, right := definition, definition
	left.Title, right.Title = "Concurrent left", "Concurrent right"
	type updateResult struct {
		record taskV2Record
		err    error
	}
	results := make(chan updateResult, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, candidate := range []taskV2Definition{left, right} {
		candidate := candidate
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			record, updateErr := fixture.store.UpdateDefinition(t.Context(), candidate, created.Revision, fixture.now.Add(time.Second))
			results <- updateResult{record: record, err: updateErr}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	var winner taskV2Record
	successes, conflicts := 0, 0
	for result := range results {
		switch {
		case result.err == nil:
			successes++
			winner = result.record
		case errors.Is(result.err, errRPCRevision):
			conflicts++
		default:
			t.Fatalf("concurrent update error = %v", result.err)
		}
	}
	if successes != 1 || conflicts != 1 || winner.Revision != 2 || winner.DefinitionRevision != 2 {
		t.Fatalf("concurrent updates successes=%d conflicts=%d winner=%+v", successes, conflicts, winner)
	}

	waiting, err := fixture.store.Transition(t.Context(), definition.ID, winner.Revision, "waiting", "", fixture.now.Add(2*time.Second))
	if err != nil || waiting.Status != "waiting" || waiting.Revision != 3 {
		t.Fatalf("waiting task = %+v, %v", waiting, err)
	}
	if _, _, err := fixture.store.StartRun(t.Context(), definition.ID, winner.Revision, fixture.now.Add(3*time.Second)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale start error = %v", err)
	}
	running, run, err := fixture.store.StartRun(t.Context(), definition.ID, waiting.Revision, fixture.now.Add(3*time.Second))
	if err != nil || running.Status != "running" || running.Revision != 4 || run.Attempt != 0 || running.CurrentRunID == nil || *running.CurrentRunID != run.ID {
		t.Fatalf("running task/run = %+v / %+v, %v", running, run, err)
	}

	fixture.store.maximumLogBytesPerTask = 10
	fixture.store.maximumLogBytesGlobal = 100
	first, err := fixture.store.AppendLog(t.Context(), definition.ID, &run.ID, "stdout", []byte("123456"), fixture.now.Add(4*time.Second))
	if err != nil || first.Sequence != 1 {
		t.Fatalf("first log = %+v, %v", first, err)
	}
	second, err := fixture.store.AppendLog(t.Context(), definition.ID, &run.ID, "stderr", []byte("abcdef"), fixture.now.Add(5*time.Second))
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second log = %+v, %v", second, err)
	}
	reset, err := fixture.store.ListLogs(t.Context(), definition.ID, "", 0, 64)
	if err != nil || !reset.ResetRequired || reset.MinimumAvailableSequence != 2 || len(reset.Items) != 0 {
		t.Fatalf("retained log reset = %+v, %v", reset, err)
	}

	fixture.store.maximumLogBytesPerTask = 100
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, &run.ID, "tool", []byte("cccc"), fixture.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, &run.ID, "system", []byte("d"), fixture.now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.store.ListLogs(t.Context(), definition.ID, "", 1, 7)
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 2 || page.AckedThroughSequence != 2 || !page.HasMore {
		t.Fatalf("bounded log page = %+v, %v", page, err)
	}
	fixture.store.maximumLogBytesGlobal = 10
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, &run.ID, "stdout", []byte("e"), fixture.now.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	globalReset, err := fixture.store.ListLogs(t.Context(), definition.ID, "", 1, 64)
	if err != nil || !globalReset.ResetRequired || globalReset.MinimumAvailableSequence != 3 {
		t.Fatalf("global log reset = %+v, %v", globalReset, err)
	}
	beforeFinish, err := fixture.store.Get(t.Context(), definition.ID)
	if err != nil || beforeFinish.Status != "running" || beforeFinish.Revision != running.Revision ||
		beforeFinish.CurrentRunID == nil || *beforeFinish.CurrentRunID != run.ID {
		t.Fatalf("task before finish = %+v, %v", beforeFinish, err)
	}

	awaiting, finishedRun, err := fixture.store.FinishRun(t.Context(), definition.ID, running.Revision, "awaitingAcceptance", 0, "", "codex-session-1", fixture.now.Add(9*time.Second))
	if err != nil || awaiting.Status != "awaitingAcceptance" || finishedRun.Status != "awaitingAcceptance" || finishedRun.CliSessionID != "codex-session-1" {
		t.Fatalf("awaiting acceptance = %+v / %+v, %v", awaiting, finishedRun, err)
	}
	completed, err := fixture.store.Transition(t.Context(), definition.ID, awaiting.Revision, "completed", "accepted", fixture.now.Add(10*time.Second))
	if err != nil || completed.Status != "completed" || completed.ResultCode != "accepted" {
		t.Fatalf("completed task = %+v, %v", completed, err)
	}
	if err := fixture.store.Delete(t.Context(), definition.ID, completed.Revision, fixture.now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Get(t.Context(), definition.ID); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("deleted get error = %v", err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var runs, logs, deletes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_runs WHERE task_id = ?`, definition.ID.String()).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_logs WHERE task_id = ?`, definition.ID.String()).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_changes WHERE task_id = ? AND operation = 'delete'`, definition.ID.String()).Scan(&deletes); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || logs != 0 || deletes != 1 {
		t.Fatalf("delete cascade runs=%d logs=%d tombstones=%d", runs, logs, deletes)
	}
}

func TestTaskV2StorePersistsRawOutputAndDisplayProjection(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	if _, err := fixture.store.Create(t.Context(), definition, fixture.now); err != nil {
		t.Fatal(err)
	}
	raw := []byte{0xd6, 0xd0, 0xce, 0xc4}
	entry, err := fixture.store.AppendDecodedLog(
		t.Context(), definition.ID, nil, "stdout", raw, "中文", "gb18030", false, false, true, fixture.now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(entry.Content, raw) || entry.DisplayText != "中文" || entry.SourceEncoding != "gb18030" || !entry.RawAvailable {
		t.Fatalf("decoded entry = %+v", entry)
	}
	binaryEntry, err := fixture.store.AppendDecodedLog(
		t.Context(), definition.ID, nil, "stderr", []byte{0xff, 0x00}, "", "binary", true, false, true, fixture.now.Add(2*time.Second),
	)
	if err != nil || !binaryEntry.IsBinary {
		t.Fatalf("binary entry = %+v, %v", binaryEntry, err)
	}
	page, err := fixture.store.ListLogs(t.Context(), definition.ID, "", 0, 1024)
	if err != nil || len(page.Items) != 2 || !bytes.Equal(page.Items[0].Content, raw) || page.Items[0].DisplayText != "中文" ||
		!page.Items[1].IsBinary || page.Items[1].SourceEncoding != "binary" {
		t.Fatalf("stored projections = %+v, %v", page, err)
	}
}

func TestTaskV2StoreListLogsDoesNotDecodeAcknowledgedPrefix(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	if _, err := fixture.store.Create(t.Context(), definition, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, nil, "stdout", []byte("acknowledged\n"), fixture.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, nil, "stdout", []byte("new output\n"), fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Corrupt an already-acknowledged payload. A true sequence seek never reads
	// or decodes this row; the former full scan failed before it reached the new
	// output, which also made every refresh scale with the complete history.
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE task_logs SET occurred_at_ms = 0 WHERE task_id = ? AND sequence = 1`, definition.ID.String()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	page, err := fixture.store.ListLogs(t.Context(), definition.ID, "", 1, 1024)
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 2 || page.AckedThroughSequence != 2 || page.HasMore {
		t.Fatalf("incremental task log page = %+v, %v", page, err)
	}
}

func TestTaskV2StoreListsNewestHundredLogLinesAndPaginatesOlder(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	if _, err := fixture.store.Create(t.Context(), definition, fixture.now); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 105; index++ {
		content := []byte(fmt.Sprintf("line %03d\n", index))
		if _, err := fixture.store.AppendLog(t.Context(), definition.ID, nil, "stdout", content, fixture.now.Add(time.Duration(index)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	tail, err := fixture.store.ListLogsBefore(t.Context(), definition.ID, "", 0, 100)
	if err != nil || len(tail.Items) != 100 || tail.Items[0].Sequence != 6 || tail.Items[len(tail.Items)-1].Sequence != 105 ||
		!tail.HasMore || tail.NextBeforeSequence != 5 || tail.HighWatermark != 105 || tail.LineCount != 100 {
		t.Fatalf("latest line window = %+v, %v", tail, err)
	}
	older, err := fixture.store.ListLogsBefore(t.Context(), definition.ID, "", tail.NextBeforeSequence, 100)
	if err != nil || len(older.Items) != 5 || older.Items[0].Sequence != 1 || older.Items[4].Sequence != 5 ||
		older.HasMore || older.NextBeforeSequence != 0 {
		t.Fatalf("older line window = %+v, %v", older, err)
	}
	fixture.store.maximumLogBytesPerTask = 5 * maximumTaskLogLineBytes
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, nil, "stdout", []byte("retained"), fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.store.maximumLogBytesPerTask = 8
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, nil, "stdout", []byte("prune"), fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	pruned, err := fixture.store.ListLogsBefore(t.Context(), definition.ID, "", 1, 100)
	if err != nil || !pruned.ResetRequired || len(pruned.Items) != 0 {
		t.Fatalf("pruned reverse cursor = %+v, %v", pruned, err)
	}
	fixture.store.maximumLogBytesPerTask = maximumTaskLogLineBytes * 4
	long := bytes.Repeat([]byte("x"), maximumTaskLogLineBytes*2+17)
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, nil, "stderr", long, fixture.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.store.ListLogsBefore(t.Context(), definition.ID, "stderr", 0, 100)
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("long log window = %+v, %v", page, err)
	}
	for _, entry := range page.Items {
		if len(entry.Content) > maximumTaskLogLineBytes {
			t.Fatalf("log line size = %d, want <= %d", len(entry.Content), maximumTaskLogLineBytes)
		}
	}
}

func TestTaskV2StoreStopsActiveQueueWithProjectWatermarkCAS(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	firstDefinition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	secondDefinition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	first, err := fixture.store.Create(t.Context(), firstDefinition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.store.Create(t.Context(), secondDefinition, fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	initial, err := fixture.store.ListChanges(t.Context(), fixture.project.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.StopAll(t.Context(), fixture.project.ID, ptrUint64(initial.HighWatermark-1), fixture.now.Add(2*time.Second)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale stop-all error = %v", err)
	}
	expected := initial.HighWatermark
	result, err := fixture.store.StopAll(t.Context(), fixture.project.ID, &expected, fixture.now.Add(2*time.Second))
	if err != nil || result.AffectedCount != 2 || len(result.Items) != 2 {
		t.Fatalf("stop-all result = %+v, %v", result, err)
	}
	for _, id := range []uuid.UUID{first.Definition.ID, second.Definition.ID} {
		task, getErr := fixture.store.Get(t.Context(), id)
		if getErr != nil || task.Status != "cancelled" || task.ResultCode != "cancelled" {
			t.Fatalf("stopped task = %+v, %v", task, getErr)
		}
	}
}

func ptrUint64(value uint64) *uint64 { return &value }

func TestTaskV2StoreRecoversInterruptedRunExactlyOnce(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), definition.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	running, run, err := fixture.store.StartRun(t.Context(), definition.ID, waiting.Revision, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadOrCreateAgentState(fixture.state.path, fixture.state.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.close()
	recovered, err := reloaded.tasksV2.RecoverInterrupted(t.Context(), fixture.now.Add(4*time.Second))
	if err != nil || recovered != 0 {
		t.Fatalf("repeated recovery = %d, %v", recovered, err)
	}
	failed, err := reloaded.tasksV2.Get(t.Context(), definition.ID)
	if err != nil || failed.Status != "failed" || failed.ResultCode != "agent_restarted" || failed.Revision != running.Revision+1 {
		t.Fatalf("recovered task = %+v, %v", failed, err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var runStatus, resultCode string
	var runCount int
	if err := db.QueryRow(`SELECT status, result_code FROM task_runs WHERE id = ?`, run.ID.String()).Scan(&runStatus, &resultCode); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_runs WHERE task_id = ?`, definition.ID.String()).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runStatus != "failed" || resultCode != "agent_restarted" || runCount != 1 {
		t.Fatalf("recovered run status=%q result=%q count=%d", runStatus, resultCode, runCount)
	}
}

func TestTaskV2StoreChangeFeedQueueScheduleAndAtomicClear(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	fixture.store.maximumChanges = 3
	definitions := make([]taskV2Definition, 3)
	for index := range definitions {
		definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
		definition.Title = []string{"Ready without schedule", "Ready past schedule", "Future schedule"}[index]
		switch index {
		case 0:
			definition.Execution.ScheduledAt = nil
		case 1:
			value := fixture.now.Add(-time.Minute)
			definition.Execution.ScheduledAt = &value
		case 2:
			value := fixture.now.Add(time.Hour)
			definition.Execution.ScheduledAt = &value
		}
		var err error
		definitions[index], err = normalizeTaskV2Definition(fixture.project, definition)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.Create(t.Context(), definitions[index], fixture.now.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	firstPage, err := fixture.store.ListChanges(t.Context(), fixture.project.ID, 0, 2)
	if err != nil || firstPage.ResetRequired || len(firstPage.Items) != 2 || !firstPage.HasMore ||
		firstPage.AckedThroughSequence != 2 || firstPage.HighWatermark != 3 || firstPage.MinimumAvailableSequence != 1 {
		t.Fatalf("first change page = %+v, %v", firstPage, err)
	}
	secondPage, err := fixture.store.ListChanges(t.Context(), fixture.project.ID, firstPage.AckedThroughSequence, 2)
	if err != nil || len(secondPage.Items) != 1 || secondPage.Items[0].Sequence != 3 || secondPage.HasMore {
		t.Fatalf("second change page = %+v, %v", secondPage, err)
	}
	wrongHighWatermark := uint64(2)
	if _, err := fixture.store.ActivateQueue(t.Context(), fixture.project.ID, &wrongHighWatermark, fixture.now); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale queue activation error = %v", err)
	}
	expectedHighWatermark := uint64(3)
	activated, err := fixture.store.ActivateQueue(t.Context(), fixture.project.ID, &expectedHighWatermark, fixture.now)
	if err != nil || activated.AffectedCount != 2 || len(activated.Items) != 2 || activated.HighWatermark != 5 {
		t.Fatalf("activated queue = %+v, %v", activated, err)
	}
	remaining, err := fixture.store.Get(t.Context(), definitions[2].ID)
	if err != nil || remaining.Status != "queued" {
		t.Fatalf("future task = %+v, %v", remaining, err)
	}
	reset, err := fixture.store.ListChanges(t.Context(), fixture.project.ID, 1, 10)
	if err != nil || !reset.ResetRequired || reset.MinimumAvailableSequence != 3 || len(reset.Items) != 0 {
		t.Fatalf("pruned change reset = %+v, %v", reset, err)
	}
	for _, task := range activated.Items {
		cancelled, cancelErr := fixture.store.Transition(t.Context(), task.Definition.ID, task.Revision, "cancelled", "cancelled", fixture.now.Add(time.Minute))
		if cancelErr != nil || cancelled.Status != "cancelled" {
			t.Fatalf("cancelled task = %+v, %v", cancelled, cancelErr)
		}
	}
	staleClear := uint64(5)
	if _, err := fixture.store.ClearFinished(t.Context(), fixture.project.ID, &staleClear, fixture.now.Add(2*time.Minute)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale clear error = %v", err)
	}
	latestChanges, err := fixture.store.ListChanges(t.Context(), fixture.project.ID, 0, 10)
	if err != nil || latestChanges.HighWatermark != 7 {
		t.Fatalf("latest changes = %+v, %v", latestChanges, err)
	}
	clearHighWatermark := latestChanges.HighWatermark
	cleared, err := fixture.store.ClearFinished(t.Context(), fixture.project.ID, &clearHighWatermark, fixture.now.Add(2*time.Minute))
	if err != nil || cleared.AffectedCount != 2 || cleared.HighWatermark != 9 {
		t.Fatalf("cleared tasks = %+v, %v", cleared, err)
	}
	items, err := fixture.store.List(t.Context(), fixture.project.ID)
	if err != nil || len(items) != 1 || items[0].Definition.ID != definitions[2].ID {
		t.Fatalf("remaining tasks = %+v, %v", items, err)
	}
}

func TestTaskV2ValidationAndProjectPolicyRejectUnsafeInputs(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	base := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())

	for _, name := range []string{"PATH", "APPDATA", "XDG_CONFIG_HOME", "SSH_AUTH_SOCK"} {
		unsafeEnvironment := base
		unsafeEnvironment.Environment = map[string]string{name: "untrusted-override"}
		if _, err := normalizeTaskV2Definition(fixture.project, unsafeEnvironment); !errors.Is(err, errRPCInvalid) {
			t.Fatalf("protected environment %s error = %v", name, err)
		}
	}
	unsafeConfig := base
	unsafeConfig.Config = json.RawMessage(`{"promptSource":"currentMarkdownFile","promptFilePath":"../outside.md","attachedFilePaths":[]}`)
	if _, err := normalizeTaskV2Definition(fixture.project, unsafeConfig); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("path traversal error = %v", err)
	}
	unknownConfig := base
	unknownConfig.Config = json.RawMessage(`{"promptSource":"customText","promptText":"safe","attachedFilePaths":[],"unexpected":true}`)
	if _, err := normalizeTaskV2Definition(fixture.project, unknownConfig); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("unknown config error = %v", err)
	}

	disabledRoot := filepath.Join(filepath.Dir(fixture.project.LocalPath), "disabled-project")
	if err := os.MkdirAll(disabledRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disabledRoot, "context.md"), []byte("disabled context"), 0o600); err != nil {
		t.Fatal(err)
	}
	disabled, err := fixture.state.business.addProject(t.Context(), disabledRoot, "Disabled task project", "", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	disabledDefinition := normalizeTaskV2TestDefinition(t, disabled, uuid.New())
	if _, err := fixture.store.Create(t.Context(), disabledDefinition, fixture.now); !errors.Is(err, errRPCCapability) {
		t.Fatalf("disabled task policy error = %v", err)
	}

	missingRelationship := base
	missingRelationship.ID = uuid.New()
	missingRelationship.Execution.RelatedTaskIDs = []uuid.UUID{uuid.New()}
	if _, err := fixture.store.Create(t.Context(), missingRelationship, fixture.now); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("missing relationship error = %v", err)
	}
}

func TestTaskV2ChangesRequestedCanBeRerun(t *testing.T) {
	if !validTaskV2Transition("changesRequested", "waiting") {
		t.Fatal("changes-requested tasks must support the same rerun action as WenzMark")
	}
	if !validTaskV2Transition("changesRequested", "completed") {
		t.Fatal("changes-requested tasks must remain directly acceptable")
	}
}
