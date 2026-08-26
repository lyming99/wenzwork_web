package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
	peerv2 "github.com/wenzwork/wenzwork-web/server/internal/peerprotocol/v2"
	"golang.org/x/sync/semaphore"
)

type taskLogShortWriter struct{}

func (taskLogShortWriter) Write(contents []byte) (int, error) {
	if len(contents) == 0 {
		return 0, nil
	}
	return len(contents) - 1, nil
}

var errTaskLogInjectedWrite = errors.New("injected task-log write failure")

type taskLogBlockingErrorWriter struct {
	started chan struct{}
	release chan struct{}
}

func (writer taskLogBlockingErrorWriter) Write([]byte) (int, error) {
	select {
	case writer.started <- struct{}{}:
	default:
	}
	<-writer.release
	return 0, errTaskLogInjectedWrite
}

func openTaskLogTestRun(t *testing.T, fixture taskV2StoreFixture) (taskV2Record, taskV2Run, *taskRunLogWriter) {
	t.Helper()
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	definition.Execution.ScheduledAt = nil
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
	writer, err := fixture.store.OpenRunLogWriter(t.Context(), running, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	return running, run, writer
}

func TestTaskRunLogWriterFormatsBoundsAndSealsWithoutSQLiteBody(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	when := time.Date(2026, 8, 20, 11, 7, 12, 123456789, time.UTC)
	if err := writer.Append(t.Context(), "stdout", "alpha\rprogress\x00\x1b[31mred\x1b[0m\n"+strings.Repeat("界", 3000), nil, when); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 1, 2, 0xff, 0xfe}
	if err := writer.Append(t.Context(), "stderr", "", binary, when.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(t.Context(), "tool", "blank before\n\nblank after", nil, when.Add(2*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.SealRunLog(t.Context(), task, run)
	if err != nil {
		t.Fatal(err)
	}
	path, err := taskRunLogPath(fixture.store.logRoot, task.Definition.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(contents) || len(contents) == 0 || contents[len(contents)-1] != '\n' ||
		strings.Contains(string(contents), "\x1b[") || !strings.Contains(string(contents), `progress\x00red`) ||
		!strings.Contains(string(contents), "2026-08-20T11:07:12.123Z [stdout] alpha") ||
		!strings.Contains(string(contents), "[stderr] <binary output omitted: bytes=5 sha256=") ||
		!strings.Contains(string(contents), "2026-08-20T11:07:12.125Z [tool] blank before\n2026-08-20T11:07:12.125Z [tool] \n2026-08-20T11:07:12.125Z [tool] blank after\n") {
		t.Fatalf("formatted task log = %q", contents)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n") {
		if len([]byte(line)) > maximumTaskLogPhysicalLineBytes {
			t.Fatalf("physical line has %d bytes", len([]byte(line)))
		}
	}
	digest := sha256.Sum256(contents)
	if snapshot.Size != uint64(len(contents)) || snapshot.SHA256 != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatalf("sealed snapshot = %+v, bytes=%d", snapshot, len(contents))
	}
	persisted, err := fixture.store.GetRun(t.Context(), task.Definition.ID, run.ID)
	if err != nil || persisted.LogState != taskLogStateSealed || persisted.LogSizeBytes != snapshot.Size || persisted.LogSHA256 != snapshot.SHA256 {
		t.Fatalf("persisted log metadata = %+v, %v", persisted, err)
	}
	db, err := fixture.store.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_logs WHERE task_id = ?`, task.Definition.ID.String()).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("task log body rows=%d, error=%v", rows, err)
	}
}

func TestTaskRunLogWriterShortWriteFailsWithoutHangingSnapshotOrSeal(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "task-log-short-write-*")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	failure := make(chan error, 1)
	writer := &taskRunLogWriter{
		taskID: uuid.New(), runID: uuid.New(), generation: 1, file: file, digest: digest,
		buffer: bufio.NewWriterSize(io.MultiWriter(taskLogShortWriter{}, digest), 16),
		queue:  make(chan taskRunLogRequest, 4), capacity: semaphore.NewWeighted(maximumTaskRunLogQueueBytes),
		done: make(chan struct{}), failureCh: make(chan struct{}), onFailure: func(err error) { failure <- err },
	}
	go writer.run()
	if err := writer.Append(t.Context(), "stdout", strings.Repeat("x", 128), nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := writer.Snapshot(ctx); err == nil {
		t.Fatal("short write did not fail the snapshot")
	}
	select {
	case err := <-failure:
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("writer failure = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("writer failure callback did not complete")
	}
	if _, err := writer.Seal(ctx); err == nil {
		t.Fatal("short write did not fail sealing")
	}
}

func TestTaskRunLogWriterFullQueueWriteFailureDoesNotDeadlock(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "task-log-queue-failure-*")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	digest := sha256.New()
	failure := make(chan error, 1)
	writer := &taskRunLogWriter{
		taskID: uuid.New(), runID: uuid.New(), generation: 1, file: file, digest: digest,
		buffer: bufio.NewWriterSize(io.MultiWriter(taskLogBlockingErrorWriter{started: started, release: release}, digest), 16),
		queue:  make(chan taskRunLogRequest), capacity: semaphore.NewWeighted(maximumTaskRunLogQueueBytes),
		done: make(chan struct{}), failureCh: make(chan struct{}), onFailure: func(err error) { failure <- err },
	}
	go writer.run()
	if err := writer.Append(t.Context(), "stdout", strings.Repeat("a", 128), nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach injected blocking write")
	}
	appendResult := make(chan error, 1)
	go func() {
		appendResult <- writer.Append(context.Background(), "stdout", strings.Repeat("b", 128), nil, time.Now().UTC())
	}()
	deadline := time.Now().Add(time.Second)
	for writer.enqueueMu.TryLock() {
		writer.enqueueMu.Unlock()
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("second append did not block on the full queue")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	select {
	case err := <-appendResult:
		if !errors.Is(err, errTaskLogInjectedWrite) {
			t.Fatalf("blocked append error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked append deadlocked with writer failure")
	}
	select {
	case err := <-failure:
		if !errors.Is(err, errTaskLogInjectedWrite) {
			t.Fatalf("writer failure = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer failure callback did not complete")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := writer.Seal(ctx); !errors.Is(err, errTaskLogInjectedWrite) {
		t.Fatalf("seal error = %v", err)
	}
}

func TestTaskRunLogWriterSealIgnoresCancelledInitiatingContext(t *testing.T) {
	taskID, runID := uuid.New(), uuid.New()
	writer, err := openTaskRunLogWriter(t.TempDir(), taskID, runID, 1, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(t.Context(), "system", "tail must be sealed", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan taskRunLogResult, 1)
	go func() {
		snapshot, err := writer.Seal(ctx)
		result <- taskRunLogResult{snapshot: snapshot, err: err}
	}()
	select {
	case sealed := <-result:
		if sealed.err != nil || sealed.snapshot.Size == 0 || sealed.snapshot.SHA256 == "" {
			t.Fatalf("cancelled-context seal = %+v", sealed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled context abandoned the writer close request")
	}
	contents, err := os.ReadFile(writer.path)
	if err != nil || !bytes.Contains(contents, []byte("tail must be sealed")) || !bytes.HasSuffix(contents, []byte("\n")) {
		t.Fatalf("sealed tail = %q, %v", contents, err)
	}
}

func TestTaskLogDiskPressureStopsAppendAndExposesContentFreeMetric(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	available := ^uint64(0)
	fixture.store.diskFreeBytes = func(string) (uint64, error) { return available, nil }
	task, run, writer := openTaskLogTestRun(t, fixture)
	available = minimumTaskLogDiskFreeBytes - 1
	writer.diskCheckMu.Lock()
	writer.lastDiskCheck = time.Time{}
	writer.diskCheckMu.Unlock()
	if err := writer.Append(t.Context(), "stdout", "must not reach disk", nil, fixture.now); !errors.Is(err, errTaskLogDiskPressure) {
		t.Fatalf("disk-pressure append error = %v", err)
	}
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
	fixture.state.tasksV2 = fixture.store
	metrics := agentCapabilities(fixture.state)["taskLogMetrics"].(map[string]any)
	if metrics["diskPressureCount"].(uint64) == 0 || metrics["lastDiskPressureReason"] != "safety_reserve" {
		t.Fatalf("task log metrics = %#v", metrics)
	}
}

func TestTaskLogHardLimitRejectsNewReservation(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	fixture.store.diskFreeBytes = func(string) (uint64, error) { return ^uint64(0), nil }
	fixture.store.maximumLogDiskBytes = maximumTaskRunLogFileBytes - 1
	if release, err := fixture.store.reserveTaskLogCapacity(t.Context(), uuid.New()); !errors.Is(err, errTaskLogDiskPressure) {
		if release != nil {
			release()
		}
		t.Fatalf("hard-limit reservation error = %v", err)
	}
	metrics := fixture.store.taskLogMetricSnapshot()
	if metrics["lastDiskPressureReason"] != "hard_limit" {
		t.Fatalf("task log metrics = %#v", metrics)
	}
}

func TestTaskLogMaintenanceRemovesOrphansPeriodicallyAndStopsCleanly(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	fixture.store.maintenanceInterval = 10 * time.Millisecond
	orphan := filepath.Join(fixture.store.logRoot, uuid.NewString())
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.store.startTaskLogMaintenance()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(orphan)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic GC did not remove orphan: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	fixture.store.closeTaskLogMaintenance()
	retained := filepath.Join(fixture.store.logRoot, uuid.NewString())
	if err := os.MkdirAll(retained, 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * fixture.store.maintenanceInterval)
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("maintenance continued after close: %v", err)
	}
}

func TestTaskLogDirectoryGCStartsOnlyAfterDeleteCommitAndHonorsLease(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	if err := writer.Append(t.Context(), "stdout", "retained until commit", nil, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
	finished, _, err := fixture.store.FinishRun(t.Context(), task.Definition.ID, task.Revision, "failed", 1, "runner_exit", "", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := taskRunLogPath(fixture.store.logRoot, task.Definition.ID, run.ID)
	if err := fixture.store.Delete(t.Context(), task.Definition.ID, finished.Revision+1, fixture.now); !errors.Is(err, errRPCRevision) {
		t.Fatalf("rolled-back delete error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rolled-back delete removed log: %v", err)
	}
	release := fixture.store.acquireRunLogLease(run.ID)
	if err := fixture.store.Delete(t.Context(), task.Definition.ID, finished.Revision, fixture.now); err != nil {
		release()
		t.Fatal(err)
	}
	fixture.store.maintenanceInterval = 10 * time.Millisecond
	fixture.store.startTaskLogMaintenance()
	time.Sleep(30 * time.Millisecond)
	if _, err := os.Stat(path); err != nil {
		release()
		fixture.store.closeTaskLogMaintenance()
		t.Fatalf("leased log was removed: %v", err)
	}
	release()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(filepath.Dir(path))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			fixture.store.closeTaskLogMaintenance()
			t.Fatalf("committed task directory was not reclaimed: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	fixture.store.closeTaskLogMaintenance()
}

func TestTaskLogRetentionCommitsExpiredStateBeforeRemovingAndRetriesFile(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	if err := writer.Append(t.Context(), "stdout", "retention ordering", nil, fixture.now); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.store.SealRunLog(t.Context(), task, run)
	if err != nil {
		t.Fatal(err)
	}
	path, err := taskRunLogPath(fixture.store.logRoot, task.Definition.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = fixture.store.expireRunLogFile(cancelled, retainedTaskLog{
		runID: run.ID, taskID: task.Definition.ID, size: snapshot.Size,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retention error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("failed metadata transition removed sealed file: %v", err)
	}
	persisted, err := fixture.store.GetRun(t.Context(), task.Definition.ID, run.ID)
	if err != nil || persisted.LogState != taskLogStateSealed {
		t.Fatalf("failed metadata transition state = %+v, %v", persisted, err)
	}
	fixture.store.business.mu.Lock()
	db, err := fixture.store.business.openDB()
	if err == nil {
		_, err = db.ExecContext(t.Context(), `UPDATE task_runs SET log_state = 'expired', log_sha256 = '', log_updated_at_ms = ?
			WHERE id = ? AND task_id = ? AND log_state = 'sealed'`, fixture.now.UnixMilli(), run.ID.String(), task.Definition.ID.String())
	}
	if db != nil {
		_ = db.Close()
	}
	fixture.store.business.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.markRunLogMissing(t.Context(), task.Definition.ID, run.ID, run.LogGeneration,
		snapshot.Size, fixture.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	persisted, err = fixture.store.GetRun(t.Context(), task.Definition.ID, run.ID)
	if err != nil || persisted.LogState != taskLogStateExpired {
		t.Fatalf("stale missing update overwrote expired state = %+v, %v", persisted, err)
	}
	if err := fixture.store.PruneTaskLogFiles(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired file retry result = %v", err)
	}
}

func TestTaskLogRejectsLinkedDirectoryWithoutCreatingOutsideRoot(t *testing.T) {
	rootParent := t.TempDir()
	root := filepath.Join(rootParent, "logs", "tasks")
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(rootParent, "logs")); err != nil {
		t.Skipf("directory symlinks are unavailable: %v", err)
	}
	if _, err := preparePrivateTaskLogDirectory(root, uuid.New()); !errors.Is(err, errTaskLogUnsafe) {
		t.Fatalf("linked log directory error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "tasks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe directory creation escaped the private root: %v", err)
	}
}

func TestTaskLogRejectsHardLinkedFile(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	if err := writer.Append(t.Context(), "stdout", "one", nil, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
	path, _ := taskRunLogPath(fixture.store.logRoot, task.Definition.ID, run.ID)
	if err := os.Link(path, path+".linked"); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	if _, err := fixture.store.ReadRunLog(t.Context(), task.Definition.ID, taskLogSeekRequest{
		RunID: &run.ID, Mode: taskLogSeekTail, TailBytes: 32, LimitBytes: maximumTaskLogSeekBytes,
	}); !errors.Is(err, errTaskLogCorrupt) {
		t.Fatalf("hard-linked log read error = %v", err)
	}
}

func TestTaskLogRejectsFileReplacementAfterOpen(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	if err := writer.Append(t.Context(), "stdout", "original", nil, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
	path, _ := taskRunLogPath(fixture.store.logRoot, task.Definition.ID, run.ID)
	opened, _, err := openPrivateTaskLogFile(fixture.store.logRoot, task.Definition.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	openedInfo, err := opened.Stat()
	if closeErr := opened.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".replaced"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(current, openedInfo) {
		t.Fatalf("replacement identity was not detected: current=%v opened=%v err=%v", current, openedInfo, err)
	}
	if _, err := fixture.store.ReadRunLog(t.Context(), task.Definition.ID, taskLogSeekRequest{
		RunID: &run.ID, Mode: taskLogSeekTail, TailBytes: 32, LimitBytes: maximumTaskLogSeekBytes,
	}); !errors.Is(err, errTaskLogCorrupt) {
		t.Fatalf("replacement log read error = %v", err)
	}
	replaced, err := fixture.store.GetRun(t.Context(), task.Definition.ID, run.ID)
	if err != nil || replaced.LogState != taskLogStateMissing || replaced.LogGeneration != run.LogGeneration+1 {
		t.Fatalf("replacement metadata = %+v, %v", replaced, err)
	}
	reset, err := fixture.store.ReadRunLog(t.Context(), task.Definition.ID, taskLogSeekRequest{
		RunID: &run.ID, Generation: &run.LogGeneration, Mode: taskLogSeekForward, LimitBytes: maximumTaskLogSeekBytes,
	})
	if err != nil || !reset.ResetRequired || reset.Generation != replaced.LogGeneration {
		t.Fatalf("replacement generation reset = %+v, %v", reset, err)
	}
}

func TestTaskLogSeekUsesByteWindowsAndGenerationReset(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	when := fixture.now.Add(3 * time.Second)
	for _, value := range []string{"first", strings.Repeat("中", 1800), "third", "fourth"} {
		if err := writer.Append(t.Context(), "stdout", value, nil, when); err != nil {
			t.Fatal(err)
		}
		when = when.Add(time.Millisecond)
	}
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
	tail, err := fixture.store.ReadRunLog(t.Context(), task.Definition.ID, taskLogSeekRequest{
		RunID: &run.ID, Generation: &run.LogGeneration, Mode: taskLogSeekTail, TailBytes: 256, LimitBytes: maximumTaskLogSeekBytes,
	})
	if err != nil || tail.Content == "" || !tail.EOF || !tail.HasMoreBefore || tail.StartOffset == 0 || tail.NextOffset != tail.FileSize {
		t.Fatalf("tail page = %+v, %v", tail, err)
	}
	older, err := fixture.store.ReadRunLog(t.Context(), task.Definition.ID, taskLogSeekRequest{
		RunID: &run.ID, Generation: &run.LogGeneration, Mode: taskLogSeekBefore,
		BeforeOffset: tail.StartOffset, LimitBytes: maximumTaskLogSeekBytes,
	})
	if err != nil || older.NextOffset != tail.StartOffset || older.Content == "" || !strings.Contains(older.Content, "first") {
		t.Fatalf("older page = %+v, %v", older, err)
	}
	inside := older.StartOffset + 3
	forward, err := fixture.store.ReadRunLog(t.Context(), task.Definition.ID, taskLogSeekRequest{
		RunID: &run.ID, Generation: &run.LogGeneration, Mode: taskLogSeekForward, Offset: inside, LimitBytes: maximumTaskLogSeekBytes,
	})
	if err != nil || !forward.CursorAdjusted || forward.StartOffset >= inside || !strings.Contains(forward.Content, "first") {
		t.Fatalf("adjusted page = %+v, %v", forward, err)
	}
	wrong := run.LogGeneration + 1
	reset, err := fixture.store.ReadRunLog(t.Context(), task.Definition.ID, taskLogSeekRequest{
		RunID: &run.ID, Generation: &wrong, Mode: taskLogSeekForward, Offset: forward.NextOffset, LimitBytes: maximumTaskLogSeekBytes,
	})
	if err != nil || !reset.ResetRequired || reset.Content != "" || reset.Generation != run.LogGeneration {
		t.Fatalf("generation reset = %+v, %v", reset, err)
	}
}

func TestTaskLogByteEventContainsOnlyRunGenerationAndWatermark(t *testing.T) {
	event := newTaskLogBytesAvailableAgentEvent(uuid.New(), uuid.New(), uuid.New(), 2, 32768, "invalidate", time.Now().UTC())
	if !validAgentEvent(event) || event.Cursor.Kind != "task_log_bytes" || event.Cursor.Value != 32768 ||
		event.Operation != "invalidate" || event.Data["generation"] != uint64(2) || event.Data["highWatermark"] != uint64(32768) {
		t.Fatalf("task log byte event = %+v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"content", "logPath", "path", "stdout", "stderr"} {
		if bytes.Contains(bytes.ToLower(encoded), bytes.ToLower([]byte(forbidden))) {
			t.Fatalf("task log event leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestTaskLogDownloadPreparationBindsSnapshotToPeerSession(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	if err := writer.Append(t.Context(), "stdout", "snapshot prefix", nil, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.state.tasksV2 = fixture.store
	sessionID := uuid.NewString()
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() },
		peerSessionID: sessionID, requestProjectID: fixture.project.ID.String(),
	}
	transferID := uuid.NewString()
	value, _, err := dispatch.taskLogDownloadPrepare(t.Context(), fixture.store, fixture.project, rpcInput{
		"taskId": task.Definition.ID.String(), "runId": run.ID.String(), "generation": float64(run.LogGeneration), "transferId": transferID,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || strings.Contains(string(encoded), fixture.store.logRoot) || strings.Contains(string(encoded), `"path"`) {
		t.Fatalf("prepared response leaked a path: %s, %v", encoded, err)
	}
	prepared := value.(map[string]any)
	if prepared["transferId"] != transferID || prepared["generation"] != run.LogGeneration || prepared["snapshot"] != true {
		t.Fatalf("prepared task log = %#v", prepared)
	}
	if err := writer.Append(context.Background(), "stdout", "later suffix", nil, fixture.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	manager := fileRPCManagerFor(fixture.state)
	manager.mu.Lock()
	source := manager.downloads[transferID]
	manager.mu.Unlock()
	if source == nil || source.SourceKind != downloadSourceTaskLog || source.PeerSessionID != sessionID || source.Size != int64(prepared["size"].(int64)) {
		t.Fatalf("managed task-log source = %+v", source)
	}
	other := dispatch
	other.peerSessionID = uuid.NewString()
	if _, _, err := other.taskLogDownloadPrepare(t.Context(), fixture.store, fixture.project, rpcInput{
		"taskId": task.Definition.ID.String(), "runId": run.ID.String(), "generation": float64(run.LogGeneration), "transferId": transferID,
	}); err != errRPCRevision {
		t.Fatalf("cross-session transfer takeover error = %v", err)
	}
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLogDownloadEmptySealedFileExpiresAndCommits(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, _ := openTaskLogTestRun(t, fixture)
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
	fixture.state.tasksV2 = fixture.store
	sessionID := uuid.NewString()
	now := time.Now().UTC()
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return now },
		peerSessionID: sessionID, requestProjectID: fixture.project.ID.String(),
	}
	expiringTransferID := uuid.NewString()
	value, _, err := dispatch.taskLogDownloadPrepare(t.Context(), fixture.store, fixture.project, rpcInput{
		"taskId": task.Definition.ID.String(), "runId": run.ID.String(), "generation": float64(run.LogGeneration), "transferId": expiringTransferID,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared := value.(map[string]any)
	want := sha256.Sum256(nil)
	if prepared["size"] != int64(0) || prepared["sealed"] != true || prepared["sha256"] != base64.RawURLEncoding.EncodeToString(want[:]) {
		t.Fatalf("empty sealed download = %#v", prepared)
	}
	manager := fileRPCManagerFor(fixture.state)
	if !fixture.store.runLogLeased(run.ID) {
		t.Fatal("prepared empty task log did not acquire a file lease")
	}
	manager.mu.Lock()
	manager.downloads[expiringTransferID].ExpiresAt = now.Add(-time.Second)
	manager.cleanup(now)
	_, retainedAfterExpiry := manager.downloads[expiringTransferID]
	manager.mu.Unlock()
	if retainedAfterExpiry || fixture.store.runLogLeased(run.ID) {
		t.Fatal("expired empty task-log transfer retained its source or lease")
	}

	transferID := uuid.NewString()
	if _, _, err := dispatch.taskLogDownloadPrepare(t.Context(), fixture.store, fixture.project, rpcInput{
		"taskId": task.Definition.ID.String(), "runId": run.ID.String(), "generation": float64(run.LogGeneration), "transferId": transferID,
	}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	source := *manager.downloads[transferID]
	manager.mu.Unlock()
	digest, err := base64.RawURLEncoding.Strict().DecodeString(source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	channelID, streamID := uuid.NewString(), uuid.NewString()
	transfer := &v2AgentDownloadTransfer{
		transferID: transferID, sourceKind: source.SourceKind, peerSessionID: source.PeerSessionID,
		channelID: channelID, streamID: streamID, projectID: source.ProjectID.String(),
		path: source.Path, totalLength: uint64(source.Size), chunkSize: fileChunkBytes,
		revision: source.Revision, projectRevision: source.ProjectRevision, expectedRevision: source.Generation,
		taskID: source.TaskID, runID: source.RunID, generation: source.Generation, sealed: source.Sealed,
	}
	copy(transfer.sha256[:], digest)
	rootKey := bytes.Repeat([]byte{0x5a}, peerv2.RootKeySize)
	keys, err := peerv2.NewLinkState(sessionID, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Close()
	link := &v2AgentLink{
		id: sessionID, state: fixture.state, keys: keys,
		downloads: map[string]*v2AgentDownloadTransfer{transferID: transfer},
	}
	if err := link.handleV2DownloadCommit(t.Context(),
		&remotev2.EncryptedRecord{ChannelId: channelID, StreamId: streamID},
		&remotev2.FileCommit{TransferId: transferID, Sha256: digest, ExpectedRevision: source.Generation}, transfer); err != nil {
		manager.mu.Lock()
		_, retained := manager.downloads[transferID]
		manager.mu.Unlock()
		t.Fatalf("empty commit error = %v (committed=%t retained=%t)", err, transfer.committed, retained)
	}
	manager.mu.Lock()
	_, retainedAfterCommit := manager.downloads[transferID]
	manager.mu.Unlock()
	if !transfer.committed || retainedAfterCommit || fixture.store.runLogLeased(run.ID) {
		t.Fatalf("empty task-log commit = committed:%t retained:%t leased:%t",
			transfer.committed, retainedAfterCommit, fixture.store.runLogLeased(run.ID))
	}
}

func TestLegacyTaskLogMigrationStreamsRowsAndIsReentrant(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	definition.Execution.ScheduledAt = nil
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
	if _, err := fixture.store.AppendDecodedLog(t.Context(), definition.ID, &run.ID, "stdout", []byte("legacy raw\n"), "legacy display\n", "utf-8", false, false, true, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, nil, "system", []byte("unscoped legacy\n"), fixture.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.FinishRun(t.Context(), definition.ID, running.Revision, "failed", 1, "legacy_failure", "", fixture.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.store.business.mu.Lock()
	db, err := fixture.store.business.openDB()
	if err != nil {
		fixture.store.business.mu.Unlock()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE task_runs SET log_state = 'none', log_generation = 0,
		log_format_version = 0, log_size_bytes = 0, log_sha256 = '', log_updated_at_ms = NULL WHERE id = ?`, run.ID.String()); err != nil {
		_ = db.Close()
		fixture.store.business.mu.Unlock()
		t.Fatal(err)
	}
	_ = db.Close()
	fixture.store.business.mu.Unlock()
	if err := fixture.store.MigrateLegacyTaskLogs(t.Context()); err != nil {
		t.Fatal(err)
	}
	migrated, err := fixture.store.GetRun(t.Context(), definition.ID, run.ID)
	if err != nil || migrated.LogState != taskLogStateSealed || migrated.LogGeneration != 1 || migrated.LogSHA256 == "" {
		t.Fatalf("migrated metadata = %+v, %v", migrated, err)
	}
	path, _ := taskRunLogPath(fixture.store.logRoot, definition.ID, run.ID)
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), "[stdout] legacy display") {
		t.Fatalf("migrated log = %q, %v", contents, err)
	}
	archive := strings.TrimSuffix(path, run.ID.String()+".log") + "legacy-unscoped.log"
	archiveContents, err := os.ReadFile(archive)
	if err != nil || !strings.Contains(string(archiveContents), "legacy-unscoped") {
		t.Fatalf("legacy unscoped archive = %q, %v", archiveContents, err)
	}
	db, err = fixture.store.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_logs WHERE task_id = ?`, definition.ID.String()).Scan(&rows); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	if rows != 0 {
		t.Fatalf("legacy body rows remain: %d", rows)
	}
	if err := fixture.store.MigrateLegacyTaskLogs(t.Context()); err != nil {
		t.Fatalf("reentrant migration: %v", err)
	}
}

func TestLegacyTaskLogMigrationCleansRowsLeftAfterMetadataCommit(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	definition.Execution.ScheduledAt = nil
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), definition.ID, created.Revision, "waiting", "", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	running, run, err := fixture.store.StartRun(t.Context(), definition.ID, waiting.Revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, &run.ID, "stdout", []byte("legacy source row"), fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.FinishRun(t.Context(), definition.ID, running.Revision, "failed", 1, "legacy_failure", "", fixture.now); err != nil {
		t.Fatal(err)
	}
	contents := encodeTaskRunLogRecords("stdout", "legacy source row", nil, fixture.now)
	path, file, err := createPrivateTaskLogFile(fixture.store.logRoot, definition.ID, run.ID, ".log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(contents); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	sha := base64.RawURLEncoding.EncodeToString(digest[:])
	wrongDigest := sha256.Sum256([]byte("mismatched"))
	wrongSHA := base64.RawURLEncoding.EncodeToString(wrongDigest[:])
	fixture.store.business.mu.Lock()
	db, err := fixture.store.business.openDB()
	if err == nil {
		_, err = db.ExecContext(t.Context(), `UPDATE task_runs SET log_state = 'sealed', log_generation = 1,
			log_format_version = 1, log_size_bytes = ?, log_sha256 = ?, log_updated_at_ms = ?, log_path = ''
			WHERE id = ? AND task_id = ?`, len(contents), wrongSHA, fixture.now.UnixMilli(), run.ID.String(), definition.ID.String())
	}
	if db != nil {
		_ = db.Close()
	}
	fixture.store.business.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MigrateLegacyTaskLogs(t.Context()); !errors.Is(err, errTaskLogCorrupt) {
		t.Fatalf("mismatched committed file cleanup error = %v", err)
	}
	countRows := func() int {
		t.Helper()
		db, err := fixture.store.business.openReadDB()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var count int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_logs WHERE task_id = ? AND run_id = ?`,
			definition.ID.String(), run.ID.String()).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if count := countRows(); count != 1 {
		t.Fatalf("unverified legacy source rows = %d, want 1", count)
	}
	fixture.store.business.mu.Lock()
	db, err = fixture.store.business.openDB()
	if err == nil {
		_, err = db.ExecContext(t.Context(), `UPDATE task_runs SET log_sha256 = ? WHERE id = ? AND task_id = ?`,
			sha, run.ID.String(), definition.ID.String())
	}
	if db != nil {
		_ = db.Close()
	}
	fixture.store.business.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MigrateLegacyTaskLogs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if count := countRows(); count != 0 {
		t.Fatalf("verified committed legacy source rows remain = %d", count)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("committed migration file changed = %q, %v", got, err)
	}
}

func TestLegacyTaskLogMigrationRebuildsInvalidFinalWithNewGeneration(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	definition.Execution.ScheduledAt = nil
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), definition.ID, created.Revision, "waiting", "", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	running, run, err := fixture.store.StartRun(t.Context(), definition.ID, waiting.Revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.AppendLog(t.Context(), definition.ID, &run.ID, "stdout", []byte("recoverable legacy"), fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.FinishRun(t.Context(), definition.ID, running.Revision, "failed", 1, "legacy_failure", "", fixture.now); err != nil {
		t.Fatal(err)
	}
	fixture.store.business.mu.Lock()
	db, err := fixture.store.business.openDB()
	if err == nil {
		_, err = db.ExecContext(t.Context(), `UPDATE task_runs SET log_state='none', log_generation=0,
			log_format_version=0, log_size_bytes=0, log_sha256='', log_updated_at_ms=NULL WHERE id=?`, run.ID.String())
	}
	if db != nil {
		_ = db.Close()
	}
	fixture.store.business.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	path, file, err := createPrivateTaskLogFile(fixture.store.logRoot, definition.ID, run.ID, ".log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("invalid file without a newline")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.MigrateLegacyTaskLogs(t.Context()); err != nil {
		t.Fatal(err)
	}
	migrated, err := fixture.store.GetRun(t.Context(), definition.ID, run.ID)
	if err != nil || migrated.LogState != taskLogStateSealed || migrated.LogGeneration != 2 {
		t.Fatalf("rebuilt migration metadata = %+v, %v", migrated, err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(contents, []byte("recoverable legacy")) || !bytes.HasSuffix(contents, []byte("\n")) {
		t.Fatalf("rebuilt migration contents = %q, %v", contents, err)
	}
}

func TestV2TaskLogDownloadReadsOnlyPreparedPrefixAndRejectsAnotherLink(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	task, run, writer := openTaskLogTestRun(t, fixture)
	prefix := strings.Repeat("prepared prefix\n", 3000)
	if err := writer.Append(t.Context(), "stdout", prefix, nil, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.state.tasksV2 = fixture.store
	sessionID, transferID := uuid.NewString(), uuid.NewString()
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() },
		peerSessionID: sessionID, requestProjectID: fixture.project.ID.String(),
	}
	if _, _, err := dispatch.taskLogDownloadPrepare(t.Context(), fixture.store, fixture.project, rpcInput{
		"taskId": task.Definition.ID.String(), "runId": run.ID.String(), "generation": float64(run.LogGeneration), "transferId": transferID,
	}); err != nil {
		t.Fatal(err)
	}
	manager := fileRPCManagerFor(fixture.state)
	manager.mu.Lock()
	source := *manager.downloads[transferID]
	manager.mu.Unlock()
	if err := writer.Append(t.Context(), "stdout", "not part of prepared snapshot", nil, fixture.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	digest, err := base64.RawURLEncoding.Strict().DecodeString(source.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	transfer := &v2AgentDownloadTransfer{
		transferID: transferID, sourceKind: source.SourceKind, peerSessionID: source.PeerSessionID,
		projectID: source.ProjectID.String(), path: source.Path, totalLength: uint64(source.Size), chunkSize: fileChunkBytes,
		revision: source.Revision, projectRevision: source.ProjectRevision, expectedRevision: source.Generation,
		taskID: source.TaskID, runID: source.RunID, generation: source.Generation, sealed: source.Sealed,
	}
	copy(transfer.sha256[:], digest)
	link := &v2AgentLink{id: sessionID, state: fixture.state}
	var downloaded []byte
	for index := uint64(0); uint64(len(downloaded)) < transfer.totalLength; index++ {
		chunk, err := link.readV2TaskLogDownloadChunk(t.Context(), transfer, index)
		if err != nil {
			t.Fatal(err)
		}
		downloaded = append(downloaded, chunk...)
	}
	file, _, err := openPrivateTaskLogFile(fixture.store.logRoot, task.Definition.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := make([]byte, source.Size)
	if _, err := file.ReadAt(expected, 0); err != nil && !errors.Is(err, io.EOF) {
		_ = file.Close()
		t.Fatal(err)
	}
	_ = file.Close()
	if !bytes.Equal(downloaded, expected) || bytes.Contains(downloaded, []byte("not part of prepared snapshot")) {
		t.Fatal("active task-log download did not preserve its prepared prefix")
	}
	if err := link.verifyV2TaskLogDownload(t.Context(), transfer); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.downloads[transferID].ExpiresAt = time.Now().UTC().Add(-time.Second)
	manager.mu.Unlock()
	if _, err := link.readV2TaskLogDownloadChunk(t.Context(), transfer, 0); !errors.Is(err, errV2AgentLink) {
		t.Fatalf("expired task-log transfer read error = %v", err)
	}
	if fixture.store.runLogLeased(run.ID) {
		t.Fatal("expired task-log transfer retained its file lease")
	}
	otherLink := &v2AgentLink{id: uuid.NewString(), state: fixture.state}
	if _, err := otherLink.readV2TaskLogDownloadChunk(t.Context(), transfer, 0); !errors.Is(err, errV2AgentLink) {
		t.Fatalf("cross-Link task log read error = %v", err)
	}
	manager.mu.Lock()
	if current := manager.downloads[transferID]; current != nil {
		if current.releaseLease != nil {
			current.releaseLease()
		}
		delete(manager.downloads, transferID)
	}
	manager.mu.Unlock()
	if _, err := fixture.store.SealRunLog(t.Context(), task, run); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLogStressEightConcurrentRunsWithSeekAndRPC(t *testing.T) {
	if testing.Short() || os.Getenv("WENZWORK_TASK_LOG_STRESS") != "1" {
		t.Skip("set WENZWORK_TASK_LOG_STRESS=1 to write and verify the 256 MiB task-log stress scenario")
	}
	fixture := newTaskV2StoreFixture(t)
	fixture.state.tasksV2 = fixture.store
	const runCount = 8
	const bytesPerRun = 32 << 20
	const appendBytes = 32 << 10
	type stressRun struct {
		task   taskV2Record
		run    taskV2Run
		writer *taskRunLogWriter
	}
	runs := make([]stressRun, 0, runCount)
	for range runCount {
		task, run, writer := openTaskLogTestRun(t, fixture)
		runs = append(runs, stressRun{task: task, run: run, writer: writer})
	}

	monitorCtx, stopMonitor := context.WithCancel(context.Background())
	monitorDone := make(chan error, 1)
	var latencyMu sync.Mutex
	latencies := make([]time.Duration, 0, 4096)
	go func() {
		index := 0
		for {
			select {
			case <-monitorCtx.Done():
				monitorDone <- nil
				return
			default:
			}
			started := time.Now()
			if _, err := fixture.store.List(monitorCtx, fixture.project.ID); err != nil {
				if errors.Is(err, context.Canceled) {
					monitorDone <- nil
				} else {
					monitorDone <- err
				}
				return
			}
			selected := runs[index%len(runs)]
			if _, err := fixture.store.Get(monitorCtx, selected.task.Definition.ID); err != nil {
				if errors.Is(err, context.Canceled) {
					monitorDone <- nil
				} else {
					monitorDone <- err
				}
				return
			}
			if _, err := fixture.store.ReadRunLog(monitorCtx, selected.task.Definition.ID, taskLogSeekRequest{
				RunID: &selected.run.ID, Generation: &selected.run.LogGeneration,
				Mode: taskLogSeekTail, TailBytes: 4 << 10, LimitBytes: maximumTaskLogSeekBytes,
			}); err != nil {
				if errors.Is(err, context.Canceled) {
					monitorDone <- nil
				} else {
					monitorDone <- err
				}
				return
			}
			latencyMu.Lock()
			latencies = append(latencies, time.Since(started))
			latencyMu.Unlock()
			index++
			time.Sleep(time.Millisecond)
		}
	}()

	payload := strings.Repeat("x", appendBytes-1) + "\n"
	started := time.Now()
	errCh := make(chan error, runCount)
	var writers sync.WaitGroup
	for index := range runs {
		current := runs[index]
		writers.Add(1)
		go func() {
			defer writers.Done()
			when := fixture.now
			for written := 0; written < bytesPerRun; written += appendBytes {
				if err := current.writer.Append(context.Background(), "stdout", payload, nil, when); err != nil {
					errCh <- err
					return
				}
				when = when.Add(time.Microsecond)
			}
		}()
	}
	writers.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			stopMonitor()
			<-monitorDone
			t.Fatal(err)
		}
	}
	stopMonitor()
	if err := <-monitorDone; err != nil {
		t.Fatal(err)
	}

	var total uint64
	for _, current := range runs {
		snapshot, err := fixture.store.SealRunLog(t.Context(), current.task, current.run)
		if err != nil {
			t.Fatal(err)
		}
		file, _, err := openPrivateTaskLogFile(fixture.store.logRoot, current.task.Definition.ID, current.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		verified, verifyErr := hashValidatedTaskLog(file, current.run.LogGeneration)
		_ = file.Close()
		if verifyErr != nil || verified != snapshot {
			t.Fatalf("stress log verification = %+v, sealed=%+v, error=%v", verified, snapshot, verifyErr)
		}
		total += snapshot.Size
	}
	if total < runCount*bytesPerRun {
		t.Fatalf("formatted stress log total = %d, want at least %d", total, runCount*bytesPerRun)
	}
	db, err := fixture.store.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	var bodyRows int
	err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM task_logs`).Scan(&bodyRows)
	_ = db.Close()
	if err != nil || bodyRows != 0 {
		t.Fatalf("stress SQLite task-log bodies = %d, error=%v", bodyRows, err)
	}
	latencyMu.Lock()
	slices.Sort(latencies)
	observations := len(latencies)
	var p95 time.Duration
	if observations > 0 {
		p95 = latencies[min(observations-1, (observations*95+99)/100-1)]
	}
	latencyMu.Unlock()
	if observations == 0 || p95 > 500*time.Millisecond {
		t.Fatalf("stress RPC/seek latency observations=%d p95=%s", observations, p95)
	}
	t.Logf("wrote and verified %d bytes across %d runs in %s; concurrent list/get/seek p95=%s (%d samples)",
		total, runCount, time.Since(started), p95, observations)
}
