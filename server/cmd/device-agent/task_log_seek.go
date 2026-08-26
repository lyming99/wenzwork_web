package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type taskLogSeekMode uint8

const (
	taskLogSeekForward taskLogSeekMode = iota
	taskLogSeekTail
	taskLogSeekBefore
)

type taskLogSeekRequest struct {
	RunID        *uuid.UUID
	Generation   *uint64
	Mode         taskLogSeekMode
	Offset       uint64
	TailBytes    uint64
	BeforeOffset uint64
	LimitBytes   uint64
}

type taskLogSeekPage struct {
	TaskID         uuid.UUID `json:"taskId"`
	RunID          uuid.UUID `json:"runId"`
	Generation     uint64    `json:"generation"`
	FormatVersion  uint32    `json:"formatVersion"`
	LogState       string    `json:"logState"`
	Content        string    `json:"content"`
	StartOffset    uint64    `json:"startOffset"`
	NextOffset     uint64    `json:"nextOffset"`
	FileSize       uint64    `json:"fileSize"`
	EOF            bool      `json:"eof"`
	HasMoreBefore  bool      `json:"hasMoreBefore"`
	Sealed         bool      `json:"sealed"`
	CursorAdjusted bool      `json:"cursorAdjusted"`
	ResetRequired  bool      `json:"resetRequired"`
	mode           taskLogSeekMode
}

func (store *taskV2Store) ReadRunLog(ctx context.Context, taskID uuid.UUID, request taskLogSeekRequest) (taskLogSeekPage, error) {
	if store == nil || taskID == uuid.Nil || request.LimitBytes == 0 || request.LimitBytes > maximumTaskLogSeekBytes {
		return taskLogSeekPage{}, errRPCInvalid
	}
	run, err := store.ResolveLogRun(ctx, taskID, request.RunID)
	if err != nil {
		return taskLogSeekPage{}, err
	}
	leaseRelease := store.acquireRunLogLease(run.ID)
	defer leaseRelease()
	// Retention can commit expired after the initial lookup but before this
	// request acquires its lease. Re-read while leased so an expired run is
	// never served from a leftover file or downgraded to missing by a stale
	// open failure. Once leased, retention cannot transition/remove this run.
	run, err = store.GetRun(ctx, taskID, run.ID)
	if err != nil {
		return taskLogSeekPage{}, err
	}
	page := taskLogSeekPage{
		TaskID: taskID, RunID: run.ID, Generation: run.LogGeneration, FormatVersion: run.LogFormatVersion,
		LogState: run.LogState, Sealed: run.LogState == taskLogStateSealed, mode: request.Mode,
	}
	if request.Generation != nil && *request.Generation != run.LogGeneration {
		page.ResetRequired = true
		return page, nil
	}
	switch run.LogState {
	case taskLogStateNone, taskLogStateCreating:
		return page, nil
	case taskLogStateExpired:
		return taskLogSeekPage{}, errTaskLogExpired
	case taskLogStateMigrating:
		return taskLogSeekPage{}, errTaskLogMigrating
	case taskLogStateMissing:
		return taskLogSeekPage{}, errTaskLogCorrupt
	case taskLogStateActive, taskLogStateSealed:
	default:
		return taskLogSeekPage{}, errTaskLogCorrupt
	}
	snapshotSize := run.LogSizeBytes
	if run.LogState == taskLogStateActive {
		writer := store.activeRunLogWriter(run.ID)
		if writer == nil || writer.generation != run.LogGeneration {
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
		snapshotSize = writer.PublishedSize()
	}
	if snapshotSize > maximumTaskRunLogFileBytes {
		return taskLogSeekPage{}, errTaskLogCorrupt
	}
	page.FileSize = snapshotSize
	requestedOffset := request.Offset
	if request.Mode == taskLogSeekBefore {
		requestedOffset = request.BeforeOffset
	}
	if request.Generation != nil && requestedOffset > snapshotSize {
		page.ResetRequired = true
		return page, nil
	}
	file, info, err := openPrivateTaskLogFile(store.logRoot, taskID, run.ID)
	if err != nil {
		if errors.Is(err, errTaskLogUnsafe) {
			_ = store.markRunLogReplaced(ctx, taskID, run.ID, run.LogGeneration, storeTimeNow())
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
		if errors.Is(err, os.ErrNotExist) {
			_ = store.markRunLogMissing(ctx, taskID, run.ID, run.LogGeneration, snapshotSize, storeTimeNow())
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
		return taskLogSeekPage{}, err
	}
	defer file.Close()
	if run.LogState == taskLogStateActive {
		writer := store.activeRunLogWriter(run.ID)
		if writer == nil || !writer.matchesFile(info) {
			_ = store.markRunLogReplaced(ctx, taskID, run.ID, run.LogGeneration, storeTimeNow())
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
	}
	if info.Size() < 0 || uint64(info.Size()) < snapshotSize || run.LogState == taskLogStateSealed && uint64(info.Size()) != snapshotSize {
		_ = store.markRunLogReplaced(ctx, taskID, run.ID, run.LogGeneration, storeTimeNow())
		return taskLogSeekPage{}, errTaskLogCorrupt
	}
	if snapshotSize == 0 {
		page.EOF = true
		return page, nil
	}
	var start, end uint64
	switch request.Mode {
	case taskLogSeekForward:
		if request.Offset > snapshotSize {
			page.ResetRequired = true
			return page, nil
		}
		start, page.CursorAdjusted, err = taskLogRecordStartAtOrBefore(file, request.Offset, snapshotSize)
		if err != nil {
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
		end, _, err = taskLogRecordStartAtOrBefore(file, min(snapshotSize, start+request.LimitBytes), snapshotSize)
		if err != nil {
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
	case taskLogSeekTail:
		window := min(request.TailBytes, request.LimitBytes)
		candidate := snapshotSize - min(snapshotSize, window)
		start, page.CursorAdjusted, err = taskLogRecordStartAtOrAfter(file, candidate, snapshotSize)
		if err != nil {
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
		end = snapshotSize
	case taskLogSeekBefore:
		if request.BeforeOffset > snapshotSize {
			page.ResetRequired = true
			return page, nil
		}
		end, page.CursorAdjusted, err = taskLogRecordStartAtOrBefore(file, request.BeforeOffset, snapshotSize)
		if err != nil {
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
		candidate := end - min(end, request.LimitBytes)
		start, _, err = taskLogRecordStartAtOrAfter(file, candidate, end)
		if err != nil {
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
	default:
		return taskLogSeekPage{}, errRPCInvalid
	}
	if end < start || end-start > request.LimitBytes || end-start > maximumTaskLogSeekBytes {
		return taskLogSeekPage{}, errTaskLogCorrupt
	}
	content := make([]byte, int(end-start))
	if len(content) > 0 {
		if _, err := file.ReadAt(content, int64(start)); err != nil && !errors.Is(err, io.EOF) {
			return taskLogSeekPage{}, err
		}
		if !utf8.Valid(content) || content[len(content)-1] != '\n' {
			return taskLogSeekPage{}, errTaskLogCorrupt
		}
	}
	page.Content = string(content)
	page.StartOffset, page.NextOffset = start, end
	page.HasMoreBefore = start > 0
	page.EOF = end == snapshotSize
	return page, nil
}

func taskLogRecordStartAtOrBefore(file *os.File, offset, size uint64) (uint64, bool, error) {
	if file == nil || offset > size {
		return 0, false, errRPCInvalid
	}
	if offset == 0 {
		return 0, false, nil
	}
	previous := []byte{0}
	if _, err := file.ReadAt(previous, int64(offset-1)); err != nil {
		return 0, false, err
	}
	if previous[0] == '\n' {
		return offset, false, nil
	}
	base := offset - min(offset, maximumTaskLogPhysicalLineBytes)
	data := make([]byte, int(offset-base))
	if _, err := file.ReadAt(data, int64(base)); err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}
	if index := bytes.LastIndexByte(data, '\n'); index >= 0 {
		return base + uint64(index) + 1, true, nil
	}
	if base != 0 {
		return 0, false, errTaskLogUnsafe
	}
	return 0, offset != 0, nil
}

func taskLogRecordStartAtOrAfter(file *os.File, offset, size uint64) (uint64, bool, error) {
	if file == nil || offset > size {
		return 0, false, errRPCInvalid
	}
	if offset == 0 || offset == size {
		return offset, false, nil
	}
	previous := []byte{0}
	if _, err := file.ReadAt(previous, int64(offset-1)); err != nil {
		return 0, false, err
	}
	if previous[0] == '\n' {
		return offset, false, nil
	}
	length := min(size-offset, maximumTaskLogPhysicalLineBytes)
	data := make([]byte, int(length))
	if _, err := file.ReadAt(data, int64(offset)); err != nil && !errors.Is(err, io.EOF) {
		return 0, false, err
	}
	index := bytes.IndexByte(data, '\n')
	if index < 0 {
		return 0, false, errTaskLogUnsafe
	}
	return offset + uint64(index) + 1, true, nil
}

var storeTimeNow = func() time.Time { return time.Now().UTC() }
