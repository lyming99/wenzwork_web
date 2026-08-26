package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	taskLogMetadataCheckpointBytes    = 1 << 20
	taskLogMetadataCheckpointInterval = 5 * time.Second
	defaultTaskLogMaintenanceInterval = 15 * time.Minute
	minimumTaskLogDiskFreeBytes       = 128 << 20
	// Keep a fixed transient allowance for active writers. This is a disk
	// safety margin, not a task concurrency ceiling.
	taskLogDiskSafetyRunAllowance = 4
	maximumTaskLogDiskBytes       = maximumTaskLogBytesGlobal + taskLogDiskSafetyRunAllowance*maximumTaskRunLogFileBytes
)

// startTaskLogMaintenance runs the potentially large legacy export and the
// small metadata-only retention/GC passes outside startup's critical path.
// One cancellable worker owns all periodic maintenance so shutdown cannot race
// the BusinessStore or a temporary test directory being closed.
func (store *taskV2Store) startTaskLogMaintenance() {
	if store == nil {
		return
	}
	store.maintenanceMu.Lock()
	if store.maintenanceCancel != nil {
		store.maintenanceMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	interval := store.maintenanceInterval
	if interval <= 0 {
		interval = defaultTaskLogMaintenanceInterval
	}
	store.maintenanceCancel, store.maintenanceDone = cancel, done
	store.maintenanceMu.Unlock()
	go func() {
		defer close(done)
		run := func() {
			if ctx.Err() != nil {
				return
			}
			_ = store.PruneTaskLogFiles(ctx)
			if ctx.Err() != nil {
				return
			}
			_ = store.MigrateLegacyTaskLogs(ctx)
			if ctx.Err() != nil {
				return
			}
			_ = store.RemoveOrphanTaskLogDirectories(ctx)
		}
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case taskID := <-store.taskLogGC:
				_ = store.removeTaskLogDirectory(taskID)
			case <-store.taskLogMaintenanceWake:
				run()
			case <-ticker.C:
				run()
			}
		}
	}()
}

func (store *taskV2Store) queueTaskLogMaintenance() {
	if store == nil || store.taskLogMaintenanceWake == nil {
		return
	}
	select {
	case store.taskLogMaintenanceWake <- struct{}{}:
	default:
	}
}

func (store *taskV2Store) queueTaskLogDirectoryGC(taskID uuid.UUID) {
	if store == nil || taskID == uuid.Nil || store.taskLogGC == nil {
		return
	}
	select {
	case store.taskLogGC <- taskID:
	default:
		// The periodic orphan scan is the durable fallback if the in-memory
		// acceleration queue is full or the process exits before consumption.
	}
}

func (store *taskV2Store) closeTaskLogMaintenance() {
	if store == nil {
		return
	}
	store.maintenanceMu.Lock()
	cancel, done := store.maintenanceCancel, store.maintenanceDone
	store.maintenanceCancel, store.maintenanceDone = nil, nil
	store.maintenanceMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (store *taskV2Store) closeTaskLogRuntime() {
	if store == nil {
		return
	}
	store.closeTaskLogMaintenance()
	store.closeTaskLogHints()
	store.fileLogMu.Lock()
	store.fileLogClosed = true
	for _, state := range store.fileLogPublishes {
		if state != nil && state.trailingEventTimer != nil {
			state.trailingEventTimer.Stop()
		}
	}
	store.fileLogPublishes = make(map[uuid.UUID]*taskFileLogPublishState)
	store.fileLogMu.Unlock()
	store.fileLogWG.Wait()
}

func (store *taskV2Store) GetRun(ctx context.Context, taskID, runID uuid.UUID) (taskV2Run, error) {
	if store == nil || store.business == nil || taskID == uuid.Nil || runID == uuid.Nil {
		return taskV2Run{}, errRPCInvalid
	}
	db, err := store.business.openReadDB()
	if err != nil {
		return taskV2Run{}, err
	}
	defer db.Close()
	run, err := scanTaskV2Run(db.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
		FROM task_runs WHERE id = ? AND task_id = ?`, runID.String(), taskID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return taskV2Run{}, errRPCNotFound
	}
	return run, err
}

func (store *taskV2Store) ResolveLogRun(ctx context.Context, taskID uuid.UUID, requested *uuid.UUID) (taskV2Run, error) {
	if store == nil || taskID == uuid.Nil {
		return taskV2Run{}, errRPCInvalid
	}
	if requested != nil {
		return store.GetRun(ctx, taskID, *requested)
	}
	runs, err := store.ListRuns(ctx, taskID)
	if err != nil {
		return taskV2Run{}, err
	}
	if len(runs) == 0 {
		return taskV2Run{}, errRPCNotFound
	}
	return runs[0], nil
}

func (store *taskV2Store) OpenRunLogWriter(
	ctx context.Context,
	task taskV2Record,
	run taskV2Run,
	onFailure func(error),
) (*taskRunLogWriter, error) {
	if store == nil || store.business == nil || task.Definition.ID == uuid.Nil || run.ID == uuid.Nil ||
		run.TaskID != task.Definition.ID || run.LogState != taskLogStateCreating || run.LogGeneration == 0 {
		return nil, errRPCInvalid
	}
	store.writersMu.Lock()
	defer store.writersMu.Unlock()
	if store.writers[run.ID] != nil {
		return nil, errRPCBusy
	}
	releaseCapacity, err := store.reserveTaskLogCapacity(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	writer, err := openTaskRunLogWriter(store.logRoot, task.Definition.ID, run.ID, run.LogGeneration,
		func(snapshot taskRunLogSnapshot, final bool) {
			store.publishFileLog(task.Definition.ProjectID, task.Definition.ID, run.ID, snapshot, final)
		}, onFailure, func() error { return store.checkTaskLogDiskSpace(0) }, releaseCapacity)
	if err != nil {
		releaseCapacity()
		return nil, err
	}
	if err := store.activateRunLog(ctx, task.Definition.ID, run.ID, run.LogGeneration, time.Now().UTC()); err != nil {
		_, _ = writer.Seal(context.Background())
		writer.releaseCapacityReservation()
		_ = os.Remove(writer.path)
		return nil, err
	}
	store.writers[run.ID] = writer
	store.fileLogMu.Lock()
	store.fileLogPublishes[run.ID] = &taskFileLogPublishState{
		projectID: task.Definition.ProjectID, taskID: task.Definition.ID, runID: run.ID,
		generation: run.LogGeneration, lastCheckpointAt: time.Now().UTC(),
	}
	store.fileLogMu.Unlock()
	return writer, nil
}

func (store *taskV2Store) reserveTaskLogCapacity(ctx context.Context, runID uuid.UUID) (func(), error) {
	if store == nil || runID == uuid.Nil {
		return nil, errRPCInvalid
	}
	if err := store.checkTaskLogDiskSpace(maximumTaskRunLogFileBytes); err != nil {
		return nil, err
	}
	store.logCapacityMu.Lock()
	defer store.logCapacityMu.Unlock()
	if store.logReservations[runID] != 0 {
		return nil, errRPCBusy
	}
	db, err := store.business.openReadDB()
	if err != nil {
		return nil, err
	}
	var stored int64
	err = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(log_size_bytes), 0) FROM task_runs
		WHERE log_state IN ('sealed','migrating')`).Scan(&stored)
	_ = db.Close()
	if err != nil || stored < 0 {
		return nil, firstError(err, errTaskLogCorrupt)
	}
	used := uint64(stored)
	for _, reserved := range store.logReservations {
		if used > ^uint64(0)-reserved {
			store.recordTaskLogDiskPressure("hard_limit")
			return nil, errTaskLogDiskPressure
		}
		used += reserved
	}
	if store.maximumLogDiskBytes > 0 && (used > store.maximumLogDiskBytes || maximumTaskRunLogFileBytes > store.maximumLogDiskBytes-used) {
		store.recordTaskLogDiskPressure("hard_limit")
		return nil, errTaskLogDiskPressure
	}
	store.logReservations[runID] = maximumTaskRunLogFileBytes
	var once sync.Once
	return func() {
		once.Do(func() {
			store.logCapacityMu.Lock()
			delete(store.logReservations, runID)
			store.logCapacityMu.Unlock()
		})
	}, nil
}

func (store *taskV2Store) checkTaskLogDiskSpace(required uint64) error {
	if store == nil || store.diskFreeBytes == nil {
		return errRPCCapability
	}
	available, err := store.diskFreeBytes(store.logRoot)
	if err != nil {
		store.recordTaskLogDiskPressure("probe_failed")
		return errors.Join(errTaskLogDiskPressure, err)
	}
	minimum := store.minimumLogDiskFree
	if required > ^uint64(0)-minimum || available < minimum+required {
		store.recordTaskLogDiskPressure("safety_reserve")
		return errTaskLogDiskPressure
	}
	return nil
}

func (store *taskV2Store) recordTaskLogDiskPressure(reason string) {
	store.diskPressureMu.Lock()
	store.diskPressureCount++
	store.diskPressureReason = reason
	store.diskPressureMu.Unlock()
}

func (store *taskV2Store) taskLogMetricSnapshot() map[string]any {
	if store == nil {
		return map[string]any{"diskPressureCount": uint64(0), "lastDiskPressureReason": ""}
	}
	store.diskPressureMu.Lock()
	defer store.diskPressureMu.Unlock()
	return map[string]any{
		"diskPressureCount":      store.diskPressureCount,
		"lastDiskPressureReason": store.diskPressureReason,
	}
}

func (store *taskV2Store) activateRunLog(ctx context.Context, taskID, runID uuid.UUID, generation uint64, now time.Time) error {
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.ExecContext(ctx, `UPDATE task_runs SET log_state = 'active', log_size_bytes = 0,
		log_sha256 = '', log_updated_at_ms = ?, log_path = ''
		WHERE id = ? AND task_id = ? AND log_generation = ? AND log_state = 'creating'`,
		now.UTC().UnixMilli(), runID.String(), taskID.String(), generation)
	if err != nil {
		return err
	}
	return requireSingleTaskMutation(result)
}

func (store *taskV2Store) activeRunLogWriter(runID uuid.UUID) *taskRunLogWriter {
	if store == nil || runID == uuid.Nil {
		return nil
	}
	store.writersMu.RLock()
	defer store.writersMu.RUnlock()
	return store.writers[runID]
}

func (store *taskV2Store) SealRunLog(ctx context.Context, task taskV2Record, run taskV2Run) (taskRunLogSnapshot, error) {
	if store == nil || task.Definition.ID == uuid.Nil || run.ID == uuid.Nil || run.TaskID != task.Definition.ID {
		return taskRunLogSnapshot{}, errRPCInvalid
	}
	writer := store.activeRunLogWriter(run.ID)
	if writer == nil {
		return taskRunLogSnapshot{}, errRPCNotFound
	}
	defer writer.releaseCapacityReservation()
	snapshot, sealErr := writer.Seal(ctx)
	store.writersMu.Lock()
	if store.writers[run.ID] == writer {
		delete(store.writers, run.ID)
	}
	store.writersMu.Unlock()
	if sealErr != nil {
		_ = store.markRunLogMissing(context.Background(), task.Definition.ID, run.ID, run.LogGeneration, snapshot.Size, time.Now().UTC())
		return snapshot, sealErr
	}
	file, info, err := openPrivateTaskLogFile(store.logRoot, task.Definition.ID, run.ID)
	if err != nil {
		if errors.Is(err, errTaskLogUnsafe) {
			_ = store.markRunLogReplaced(context.Background(), task.Definition.ID, run.ID, run.LogGeneration, time.Now().UTC())
		} else {
			_ = store.markRunLogMissing(context.Background(), task.Definition.ID, run.ID, run.LogGeneration, snapshot.Size, time.Now().UTC())
		}
		return snapshot, err
	}
	_ = file.Close()
	if info.Size() < 0 || uint64(info.Size()) != snapshot.Size || !writer.matchesFile(info) {
		_ = store.markRunLogReplaced(context.Background(), task.Definition.ID, run.ID, run.LogGeneration, time.Now().UTC())
		return snapshot, errTaskLogUnsafe
	}
	if err := store.persistSealedRunLog(ctx, task.Definition.ID, run.ID, snapshot, time.Now().UTC()); err != nil {
		return snapshot, err
	}
	store.publishFileLog(task.Definition.ProjectID, task.Definition.ID, run.ID, snapshot, true)
	store.queueTaskLogMaintenance()
	return snapshot, nil
}

func (store *taskV2Store) persistSealedRunLog(ctx context.Context, taskID, runID uuid.UUID, snapshot taskRunLogSnapshot, now time.Time) error {
	if snapshot.Generation == 0 || snapshot.Size > maximumTaskRunLogFileBytes || snapshot.SHA256 == "" {
		return errRPCInvalid
	}
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := db.ExecContext(ctx, `UPDATE task_runs SET log_state = 'sealed', log_format_version = 1,
		log_size_bytes = ?, log_sha256 = ?, log_updated_at_ms = ?, log_path = ''
		WHERE id = ? AND task_id = ? AND log_generation = ? AND log_state IN ('creating','active')`,
		snapshot.Size, snapshot.SHA256, now.UTC().UnixMilli(), runID.String(), taskID.String(), snapshot.Generation)
	if err != nil {
		return err
	}
	return requireSingleTaskMutation(result)
}

func (store *taskV2Store) markRunLogMissing(ctx context.Context, taskID, runID uuid.UUID, generation, size uint64, now time.Time) error {
	if store == nil || generation == 0 {
		return errRPCInvalid
	}
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `UPDATE task_runs SET log_state = 'missing', log_size_bytes = ?,
		log_sha256 = '', log_updated_at_ms = ?, log_path = ''
		WHERE id = ? AND task_id = ? AND log_generation = ?
		  AND log_state IN ('creating','active','sealed','migrating')`,
		size, now.UTC().UnixMilli(), runID.String(), taskID.String(), generation)
	return err
}

func (store *taskV2Store) markRunLogReplaced(ctx context.Context, taskID, runID uuid.UUID, generation uint64, now time.Time) error {
	if store == nil || taskID == uuid.Nil || runID == uuid.Nil || generation == 0 || generation == ^uint64(0) {
		return errRPCInvalid
	}
	next := generation + 1
	store.business.mu.Lock()
	db, err := store.business.openDB()
	if err != nil {
		store.business.mu.Unlock()
		return err
	}
	var projectText string
	err = db.QueryRowContext(ctx, `SELECT project_id FROM tasks WHERE id = ?`, taskID.String()).Scan(&projectText)
	if err == nil {
		var result sql.Result
		result, err = db.ExecContext(ctx, `UPDATE task_runs SET log_state = 'missing', log_generation = ?,
			log_size_bytes = 0, log_sha256 = '', log_updated_at_ms = ?, log_path = ''
			WHERE id = ? AND task_id = ? AND log_generation = ? AND log_state IN ('creating','active','sealed','migrating')`,
			next, now.UTC().UnixMilli(), runID.String(), taskID.String(), generation)
		if err == nil {
			err = requireSingleTaskMutation(result)
		}
	}
	_ = db.Close()
	store.business.mu.Unlock()
	if err != nil {
		return err
	}
	projectID, parseErr := uuid.Parse(projectText)
	store.fileLogMu.Lock()
	if state := store.fileLogPublishes[runID]; state != nil && state.generation <= next {
		state.generation, state.size = next, 0
		state.lastCheckpointSize, state.lastCheckpointAt, state.lastEventAt = 0, time.Time{}, time.Time{}
	}
	store.fileLogMu.Unlock()
	if writer := store.activeRunLogWriter(runID); writer != nil && writer.generation == generation {
		writer.notifyFailure(errTaskLogUnsafe)
	}
	if parseErr == nil && projectID != uuid.Nil {
		_ = store.persistFileLogEvent(context.Background(), projectID, taskID, runID, next, 0, "invalidate", now)
	}
	return nil
}

func (store *taskV2Store) publishFileLog(projectID, taskID, runID uuid.UUID, snapshot taskRunLogSnapshot, final bool) {
	if store == nil || projectID == uuid.Nil || taskID == uuid.Nil || runID == uuid.Nil || snapshot.Generation == 0 {
		return
	}
	now := time.Now().UTC()
	store.fileLogMu.Lock()
	if store.fileLogClosed {
		store.fileLogMu.Unlock()
		return
	}
	state := store.fileLogPublishes[runID]
	if state == nil {
		state = &taskFileLogPublishState{projectID: projectID, taskID: taskID, runID: runID, generation: snapshot.Generation}
		store.fileLogPublishes[runID] = state
	}
	if snapshot.Generation < state.generation {
		store.fileLogMu.Unlock()
		return
	}
	invalidated := state.generation != snapshot.Generation || snapshot.Size < state.size
	if invalidated {
		state.generation = snapshot.Generation
		state.lastCheckpointSize, state.lastCheckpointAt, state.lastEventAt = 0, time.Time{}, time.Time{}
	}
	state.size = snapshot.Size
	checkpointDue := final || state.lastCheckpointAt.IsZero() || snapshot.Size-state.lastCheckpointSize >= taskLogMetadataCheckpointBytes ||
		now.Sub(state.lastCheckpointAt) >= taskLogMetadataCheckpointInterval
	if checkpointDue {
		state.checkpointPending = true
		if !state.checkpointRunning {
			state.checkpointRunning = true
			store.fileLogWG.Add(1)
			go func() {
				defer store.fileLogWG.Done()
				store.runFileLogCheckpoint(runID)
			}()
		}
	}
	eventDue := final || state.lastEventAt.IsZero() || now.Sub(state.lastEventAt) >= agentEventTaskLogHintInterval
	if eventDue {
		if invalidated {
			state.eventOperation = "invalidate"
		} else if state.eventOperation == "" {
			state.eventOperation = "status"
		}
		state.eventPending = true
		if !state.eventRunning {
			state.eventRunning = true
			store.fileLogWG.Add(1)
			go func() {
				defer store.fileLogWG.Done()
				store.runFileLogEvent(runID)
			}()
		}
	} else {
		remaining := agentEventTaskLogHintInterval - now.Sub(state.lastEventAt)
		if state.trailingEventTimer != nil {
			state.trailingEventTimer.Stop()
		}
		state.trailingEventTimer = time.AfterFunc(remaining, func() {
			store.fileLogMu.Lock()
			current := store.fileLogPublishes[runID]
			if current != nil && !store.fileLogClosed {
				current.eventPending = true
				if current.eventOperation == "" {
					current.eventOperation = "status"
				}
				if !current.eventRunning {
					current.eventRunning = true
					store.fileLogWG.Add(1)
					go func() {
						defer store.fileLogWG.Done()
						store.runFileLogEvent(runID)
					}()
				}
			}
			store.fileLogMu.Unlock()
		})
	}
	store.fileLogMu.Unlock()
}

func (store *taskV2Store) runFileLogCheckpoint(runID uuid.UUID) {
	for {
		store.fileLogMu.Lock()
		state := store.fileLogPublishes[runID]
		if state == nil || !state.checkpointPending {
			if state != nil {
				state.checkpointRunning = false
			}
			store.fileLogMu.Unlock()
			return
		}
		state.checkpointPending = false
		taskID, generation, size := state.taskID, state.generation, state.size
		store.fileLogMu.Unlock()
		err := store.checkpointActiveRunLog(context.Background(), taskID, runID, generation, size, time.Now().UTC())
		store.fileLogMu.Lock()
		state = store.fileLogPublishes[runID]
		if state != nil && err == nil && state.generation == generation {
			state.lastCheckpointSize, state.lastCheckpointAt = size, time.Now().UTC()
		}
		store.fileLogMu.Unlock()
	}
}

func (store *taskV2Store) checkpointActiveRunLog(ctx context.Context, taskID, runID uuid.UUID, generation, size uint64, now time.Time) error {
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `UPDATE task_runs SET log_size_bytes = ?, log_updated_at_ms = ?
		WHERE id = ? AND task_id = ? AND log_generation = ? AND log_state = 'active' AND log_size_bytes <= ?`,
		size, now.UTC().UnixMilli(), runID.String(), taskID.String(), generation, size)
	return err
}

func (store *taskV2Store) runFileLogEvent(runID uuid.UUID) {
	for {
		store.fileLogMu.Lock()
		state := store.fileLogPublishes[runID]
		if state == nil || !state.eventPending {
			if state != nil {
				state.eventRunning = false
			}
			store.fileLogMu.Unlock()
			return
		}
		state.eventPending = false
		projectID, taskID, generation, size, operation := state.projectID, state.taskID, state.generation, state.size, state.eventOperation
		state.eventOperation = ""
		store.fileLogMu.Unlock()
		_ = store.persistFileLogEvent(context.Background(), projectID, taskID, runID, generation, size, operation, time.Now().UTC())
		store.fileLogMu.Lock()
		state = store.fileLogPublishes[runID]
		if state != nil && state.generation == generation {
			state.lastEventAt = time.Now().UTC()
		}
		store.fileLogMu.Unlock()
	}
}

func (store *taskV2Store) persistFileLogEvent(
	ctx context.Context,
	projectID, taskID, runID uuid.UUID,
	generation, size uint64,
	operation string,
	now time.Time,
) error {
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := store.business.appendAgentEvent(ctx, tx, newTaskLogBytesAvailableAgentEvent(projectID, taskID, runID, generation, size, operation, now)); err != nil {
		return err
	}
	return commitBusinessTransaction(ctx, tx)
}

func (store *taskV2Store) acquireRunLogLease(runID uuid.UUID) func() {
	if store == nil || runID == uuid.Nil {
		return func() {}
	}
	store.logLeaseMu.Lock()
	store.logLeases[runID]++
	store.logLeaseMu.Unlock()
	var released bool
	return func() {
		store.logLeaseMu.Lock()
		if !released {
			released = true
			if store.logLeases[runID] <= 1 {
				delete(store.logLeases, runID)
			} else {
				store.logLeases[runID]--
			}
		}
		store.logLeaseMu.Unlock()
	}
}

func (store *taskV2Store) runLogLeased(runID uuid.UUID) bool {
	store.logLeaseMu.Lock()
	defer store.logLeaseMu.Unlock()
	return store.logLeases[runID] > 0
}

type retainedTaskLog struct {
	runID      uuid.UUID
	taskID     uuid.UUID
	size       uint64
	finishedAt int64
	attempt    uint32
	protected  bool
	removed    bool
}

// PruneTaskLogFiles applies the existing byte targets by removing whole sealed
// run files. Active logs, the newest sealed run per task, and leased files are
// never truncated or removed.
func (store *taskV2Store) PruneTaskLogFiles(ctx context.Context) error {
	if store == nil || store.business == nil {
		return errRPCInvalid
	}
	// A prior pass may have committed expired metadata and then lost the race
	// to a lease or encountered a transient filesystem error. Retry those
	// physical removals before calculating the next sealed-log retention pass.
	_ = store.cleanupExpiredRunLogFiles(ctx)
	db, err := store.business.openReadDB()
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT run.id, run.task_id, run.log_size_bytes,
		COALESCE(run.finished_at_ms, run.created_at_ms), run.attempt
		FROM task_runs AS run
		WHERE run.log_state = 'sealed'
		  AND NOT EXISTS(SELECT 1 FROM task_logs AS body WHERE body.task_id = run.task_id AND body.run_id = run.id)
		ORDER BY run.task_id, run.attempt DESC, run.created_at_ms DESC`)
	if err != nil {
		_ = db.Close()
		return err
	}
	logs := make([]retainedTaskLog, 0)
	latest := make(map[uuid.UUID]bool)
	for rows.Next() {
		var runText, taskText string
		var item retainedTaskLog
		if err := rows.Scan(&runText, &taskText, &item.size, &item.finishedAt, &item.attempt); err != nil {
			_ = rows.Close()
			_ = db.Close()
			return err
		}
		item.runID, err = uuid.Parse(runText)
		if err != nil {
			_ = rows.Close()
			_ = db.Close()
			return err
		}
		item.taskID, err = uuid.Parse(taskText)
		if err != nil {
			_ = rows.Close()
			_ = db.Close()
			return err
		}
		item.protected = !latest[item.taskID]
		latest[item.taskID] = true
		logs = append(logs, item)
	}
	err = rows.Close()
	_ = db.Close()
	if err != nil {
		return err
	}
	byTask := make(map[uuid.UUID]uint64)
	var global uint64
	for _, item := range logs {
		byTask[item.taskID] += item.size
		global += item.size
	}
	oldest := append([]retainedTaskLog(nil), logs...)
	slices.SortFunc(oldest, func(left, right retainedTaskLog) int {
		if left.finishedAt < right.finishedAt {
			return -1
		}
		if left.finishedAt > right.finishedAt {
			return 1
		}
		return strings.Compare(left.runID.String(), right.runID.String())
	})
	for index := range oldest {
		item := &oldest[index]
		if item.protected || store.runLogLeased(item.runID) ||
			byTask[item.taskID] <= store.maximumLogBytesPerTask && global <= store.maximumLogBytesGlobal {
			continue
		}
		if err := store.expireRunLogFile(ctx, *item); err != nil {
			continue
		}
		item.removed = true
		byTask[item.taskID] -= item.size
		global -= item.size
	}
	return nil
}

func (store *taskV2Store) expireRunLogFile(ctx context.Context, item retainedTaskLog) error {
	store.logLeaseMu.Lock()
	defer store.logLeaseMu.Unlock()
	if store.logLeases[item.runID] > 0 {
		return errRPCBusy
	}
	store.business.mu.Lock()
	db, err := store.business.openDB()
	if err != nil {
		store.business.mu.Unlock()
		return err
	}
	result, err := db.ExecContext(ctx, `UPDATE task_runs SET log_state = 'expired', log_sha256 = '', log_updated_at_ms = ?
		WHERE id = ? AND task_id = ? AND log_state = 'sealed'`, time.Now().UTC().UnixMilli(), item.runID.String(), item.taskID.String())
	if err == nil {
		err = requireSingleTaskMutation(result)
	}
	_ = db.Close()
	store.business.mu.Unlock()
	if err != nil {
		return err
	}
	return store.removeRunLogFileLocked(item.taskID, item.runID)
}

func (store *taskV2Store) cleanupExpiredRunLogFiles(ctx context.Context) error {
	db, err := store.business.openReadDB()
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, task_id FROM task_runs
		WHERE log_state = 'expired' ORDER BY COALESCE(finished_at_ms, created_at_ms), id`)
	if err != nil {
		_ = db.Close()
		return err
	}
	items := make([]retainedTaskLog, 0)
	for rows.Next() {
		var runText, taskText string
		if err := rows.Scan(&runText, &taskText); err != nil {
			_ = rows.Close()
			_ = db.Close()
			return err
		}
		runID, runErr := uuid.Parse(runText)
		taskID, taskErr := uuid.Parse(taskText)
		if runErr != nil || taskErr != nil || runID == uuid.Nil || taskID == uuid.Nil {
			_ = rows.Close()
			_ = db.Close()
			return errTaskLogCorrupt
		}
		items = append(items, retainedTaskLog{runID: runID, taskID: taskID})
	}
	err = rows.Close()
	_ = db.Close()
	if err != nil {
		return err
	}
	var firstErr error
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		store.logLeaseMu.Lock()
		if store.logLeases[item.runID] > 0 {
			store.logLeaseMu.Unlock()
			continue
		}
		err := store.removeRunLogFileLocked(item.taskID, item.runID)
		store.logLeaseMu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// removeRunLogFileLocked requires logLeaseMu. Keeping the lease check and
// unlink in the same critical section prevents a seek or prepared download
// from acquiring a handle between them.
func (store *taskV2Store) removeRunLogFileLocked(taskID, runID uuid.UUID) error {
	path, err := taskRunLogPath(store.logRoot, taskID, runID)
	if err != nil {
		return err
	}
	file, _, openErr := openPrivateTaskLogFile(store.logRoot, taskID, runID)
	if openErr == nil {
		_ = file.Close()
		if err := os.Remove(path); err != nil {
			return err
		}
		return syncTaskLogDirectory(filepath.Dir(path))
	}
	if !errors.Is(openErr, os.ErrNotExist) {
		return openErr
	}
	directory := filepath.Dir(path)
	if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return syncTaskLogDirectory(directory)
}

func (store *taskV2Store) removeTaskLogDirectory(taskID uuid.UUID) error {
	if store == nil || taskID == uuid.Nil {
		return errRPCInvalid
	}
	store.logLeaseMu.Lock()
	defer store.logLeaseMu.Unlock()
	directory := filepath.Join(store.logRoot, taskID.String())
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errTaskLogUnsafe
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || !sameFilesystemPath(resolved, directory) {
		return errTaskLogUnsafe
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errTaskLogUnsafe
		}
		name := entry.Name()
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".migrating"), ".log")
		if name != "legacy-unscoped.log" && uuid.Validate(base) != nil {
			return errTaskLogUnsafe
		}
		if runID, parseErr := uuid.Parse(base); parseErr == nil && store.logLeases[runID] > 0 {
			return errRPCBusy
		}
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	if err := os.Remove(directory); err != nil {
		return err
	}
	return syncTaskLogDirectory(store.logRoot)
}

func (store *taskV2Store) RemoveOrphanTaskLogDirectories(ctx context.Context) error {
	if store == nil || store.business == nil {
		return errRPCInvalid
	}
	info, err := os.Lstat(store.logRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errTaskLogUnsafe
	}
	resolved, err := filepath.EvalSymlinks(store.logRoot)
	if err != nil || !sameFilesystemPath(resolved, store.logRoot) {
		return errTaskLogUnsafe
	}
	entries, err := os.ReadDir(store.logRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() || uuid.Validate(entry.Name()) != nil {
			continue
		}
		taskID, _ := uuid.Parse(entry.Name())
		db, err := store.business.openReadDB()
		if err != nil {
			return err
		}
		var exists int
		err = db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, taskID.String()).Scan(&exists)
		_ = db.Close()
		if errors.Is(err, sql.ErrNoRows) {
			_ = store.removeTaskLogDirectory(taskID)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
