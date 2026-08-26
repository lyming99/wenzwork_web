package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (d dispatcher) taskLogDownloadPrepare(
	ctx context.Context,
	store *taskV2Store,
	project registeredProject,
	input rpcInput,
) (any, uint64, error) {
	if !taskV2InputHasOnly(input, "taskId", "runId", "generation", "transferId") {
		return nil, 0, errRPCInvalid
	}
	transferID, transferOK := transferIDInput(input)
	taskID, taskOK := inputUUID(input, "taskId")
	runID, runOK := inputUUID(input, "runId")
	generation, generationPresent, generationOK := optionalUint64(input, "generation")
	peerSessionID := strings.TrimSpace(d.peerSessionID)
	if !transferOK || !taskOK || !runOK || !generationPresent || !generationOK || generation == 0 ||
		uuid.Validate(peerSessionID) != nil {
		return nil, 0, errRPCInvalid
	}
	task, err := store.Get(ctx, taskID)
	if err != nil {
		return nil, 0, err
	}
	if task.Definition.ProjectID != project.ID {
		return nil, 0, errRPCProject
	}
	run, err := store.GetRun(ctx, taskID, runID)
	if err != nil {
		return nil, 0, err
	}
	leaseRelease := store.acquireRunLogLease(runID)
	leaseAdopted := false
	defer func() {
		if !leaseAdopted {
			leaseRelease()
		}
	}()
	// Pair the authorization lookup with the filesystem operation under the
	// same run lease. A retention transition that won before this lease must be
	// observed as LOG_EXPIRED, even if its physical unlink needs a retry.
	run, err = store.GetRun(ctx, taskID, runID)
	if err != nil {
		return nil, 0, err
	}
	if run.LogGeneration != generation {
		return nil, 0, errRPCRevision
	}
	var snapshot taskRunLogSnapshot
	switch run.LogState {
	case taskLogStateActive:
		writer := store.activeRunLogWriter(run.ID)
		if writer == nil || writer.generation != generation {
			return nil, 0, errTaskLogCorrupt
		}
		snapshot, err = writer.Snapshot(ctx)
		if err != nil {
			return nil, 0, errTaskLogCorrupt
		}
	case taskLogStateSealed:
		snapshot = taskRunLogSnapshot{Generation: generation, Size: run.LogSizeBytes, SHA256: run.LogSHA256}
	case taskLogStateExpired:
		return nil, 0, errTaskLogExpired
	case taskLogStateMigrating:
		return nil, 0, errTaskLogMigrating
	case taskLogStateMissing:
		return nil, 0, errTaskLogCorrupt
	default:
		return nil, 0, errRPCBusy
	}
	if snapshot.Size > maximumTaskRunLogFileBytes || snapshot.SHA256 == "" {
		return nil, 0, errTaskLogCorrupt
	}
	file, info, err := openPrivateTaskLogFile(store.logRoot, taskID, runID)
	if err != nil {
		if errors.Is(err, errTaskLogUnsafe) {
			_ = store.markRunLogReplaced(ctx, taskID, runID, generation, d.now().UTC())
			return nil, 0, errTaskLogCorrupt
		}
		if errors.Is(err, os.ErrNotExist) {
			_ = store.markRunLogMissing(ctx, taskID, runID, generation, snapshot.Size, d.now().UTC())
			return nil, 0, errTaskLogCorrupt
		}
		return nil, 0, err
	}
	_ = file.Close()
	if run.LogState == taskLogStateActive {
		writer := store.activeRunLogWriter(run.ID)
		if writer == nil || !writer.matchesFile(info) {
			_ = store.markRunLogReplaced(ctx, taskID, runID, generation, d.now().UTC())
			return nil, 0, errTaskLogCorrupt
		}
	}
	if info.Size() < 0 || uint64(info.Size()) < snapshot.Size || run.LogState == taskLogStateSealed && uint64(info.Size()) != snapshot.Size {
		_ = store.markRunLogReplaced(ctx, taskID, runID, generation, d.now().UTC())
		return nil, 0, errTaskLogCorrupt
	}
	manager := fileRPCManagerFor(d.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := d.now().UTC()
	manager.cleanup(now)
	if existing := manager.downloads[transferID]; existing != nil {
		if existing.SourceKind != downloadSourceTaskLog || existing.PeerSessionID != peerSessionID || existing.ProjectID != project.ID ||
			existing.TaskID != taskID || existing.RunID != runID || existing.Generation != generation ||
			existing.Size != int64(snapshot.Size) || existing.SHA256 != snapshot.SHA256 {
			return nil, 0, errRPCRevision
		}
		existing.ExpiresAt = now.Add(fileTransferTTL)
		return taskLogDownloadPrepareResponse(existing), generation, nil
	}
	if manager.uploads[transferID] != nil {
		return nil, 0, errRPCRevision
	}
	if !manager.canStartTransfer(project.ID, 0) {
		return nil, 0, errRPCBusy
	}
	path, err := taskRunLogPath(store.logRoot, taskID, runID)
	if err != nil {
		return nil, 0, err
	}
	transfer := &downloadTransfer{
		ID: transferID, SourceKind: downloadSourceTaskLog, PeerSessionID: peerSessionID,
		ProjectID: project.ID, ProjectRevision: project.Revision, Path: path,
		Size: int64(snapshot.Size), SHA256: snapshot.SHA256, Revision: generation,
		TaskID: taskID, RunID: runID, Generation: generation,
		FileName: fmt.Sprintf("task-%s-run-%s.log", taskID, runID), Sealed: run.LogState == taskLogStateSealed,
		ExpiresAt: now.Add(fileTransferTTL), releaseLease: leaseRelease,
	}
	manager.downloads[transferID] = transfer
	leaseAdopted = true
	return taskLogDownloadPrepareResponse(transfer), generation, nil
}

func taskLogDownloadPrepareResponse(transfer *downloadTransfer) map[string]any {
	return map[string]any{
		"transferId": transfer.ID, "fileName": transfer.FileName, "size": transfer.Size,
		"sha256": transfer.SHA256, "chunkSize": fileChunkBytes, "generation": transfer.Generation,
		"snapshot": true, "sealed": transfer.Sealed, "expiresAt": transfer.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
}
