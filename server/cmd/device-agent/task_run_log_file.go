package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/sync/semaphore"
)

const (
	maximumTaskRunLogPathBytes      = 4096
	maximumTaskRunLogQueueBytes     = 1 << 20
	taskRunLogFlushBytes            = 64 << 10
	taskRunLogFlushInterval         = 100 * time.Millisecond
	taskRunLogSyncBytes             = 1 << 20
	taskRunLogSyncInterval          = time.Second
	maximumTaskLogPhysicalLineBytes = maximumTaskRunLogRecordBytes + 96
)

var (
	errTaskLogOutputLimit  = errors.New("task log output limit reached")
	errTaskLogUnsafe       = errors.New("task log file is unsafe")
	errTaskLogDiskPressure = errors.New("task log disk pressure")
)

func defaultTaskRunLogRoot() string {
	// Kept only for validating legacy v22 log_path values during migration.
	return filepath.Join(os.TempDir(), "wenzwork", "task-logs")
}

func taskRunLogRootForStateFile(stateFile string) (string, error) {
	stateFile, err := absoluteAgentPath(stateFile)
	if err != nil {
		return "", err
	}
	return normalizeTaskRunLogRoot(filepath.Join(filepath.Dir(stateFile), "logs", "tasks"))
}

func normalizeTaskRunLogRoot(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.IndexByte(value, 0) >= 0 || len(value) > maximumTaskRunLogPathBytes {
		return "", errRPCInvalid
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if !filepath.IsAbs(absolute) || len(absolute) > maximumTaskRunLogPathBytes {
		return "", errRPCInvalid
	}
	return absolute, nil
}

func existingTaskLogProbePath(path string) (string, error) {
	path, err := normalizeTaskRunLogRoot(path)
	if err != nil {
		return "", err
	}
	for {
		info, statErr := os.Lstat(path)
		if statErr == nil {
			if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				return "", errTaskLogUnsafe
			}
			return path, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", statErr
		}
		path = parent
	}
}

func taskRunLogPath(root string, taskID, runID uuid.UUID) (string, error) {
	if taskID == uuid.Nil || runID == uuid.Nil {
		return "", errRPCInvalid
	}
	normalizedRoot, err := normalizeTaskRunLogRoot(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(normalizedRoot, taskID.String(), runID.String()+".log")
	relative, err := filepath.Rel(normalizedRoot, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		!validTaskRunLogPath(path) {
		return "", errRPCInvalid
	}
	return path, nil
}

func validTaskRunLogPath(value string) bool {
	return value == "" || len(value) <= maximumTaskRunLogPathBytes && strings.IndexByte(value, 0) < 0 &&
		filepath.IsAbs(value) && filepath.Clean(value) == value
}

func preparePrivateTaskLogDirectory(root string, taskID uuid.UUID) (string, error) {
	root, err := normalizeTaskRunLogRoot(root)
	if err != nil || taskID == uuid.Nil {
		return "", firstError(err, errRPCInvalid)
	}
	directories := []string{root, filepath.Join(root, taskID.String())}
	if filepath.Base(root) == "tasks" && filepath.Base(filepath.Dir(root)) == "logs" {
		directories = append([]string{filepath.Dir(root)}, directories...)
	}
	for _, directory := range directories {
		created := false
		if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(directory, 0o700); err != nil {
				return "", err
			}
			created = true
		} else if err != nil {
			return "", err
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return "", errTaskLogUnsafe
		}
		resolved, err := filepath.EvalSymlinks(directory)
		if err != nil || !sameFilesystemPath(resolved, directory) {
			return "", errTaskLogUnsafe
		}
		// secureStateFile applies the managed DACL on Windows. Restore directory
		// search bits after its Unix implementation applies a file mode.
		if err := secureStateFile(directory); err != nil {
			return "", err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return "", err
		}
		if created {
			if err := syncTaskLogDirectory(filepath.Dir(directory)); err != nil {
				return "", err
			}
		}
	}
	return filepath.Join(root, taskID.String()), nil
}

func createPrivateTaskLogFile(root string, taskID, runID uuid.UUID, suffix string) (string, *os.File, error) {
	if suffix != ".log" && suffix != ".log.migrating" {
		return "", nil, errRPCInvalid
	}
	return createPrivateTaskLogNamedFile(root, taskID, runID.String()+suffix)
}

func createPrivateTaskLogNamedFile(root string, taskID uuid.UUID, name string) (string, *os.File, error) {
	if taskID == uuid.Nil || filepath.Base(name) != name ||
		name != "legacy-unscoped.log.migrating" && uuid.Validate(strings.TrimSuffix(strings.TrimSuffix(name, ".migrating"), ".log")) != nil {
		return "", nil, errRPCInvalid
	}
	directory, err := preparePrivateTaskLogDirectory(root, taskID)
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	fail := func(cause error) (string, *os.File, error) {
		_ = file.Close()
		_ = os.Remove(path)
		return "", nil, cause
	}
	if err := secureStateFile(path); err != nil {
		return fail(err)
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !taskLogFileHasSingleLink(file) {
		return fail(errTaskLogUnsafe)
	}
	if err := syncTaskLogDirectory(directory); err != nil {
		return fail(err)
	}
	return path, file, nil
}

// openPrivateTaskLogFile derives the path exclusively from UUIDs and verifies
// both the directory chain and the opened file identity. Callers never accept
// a database path or a client-supplied path.
func openPrivateTaskLogFile(root string, taskID, runID uuid.UUID) (*os.File, os.FileInfo, error) {
	path, err := taskRunLogPath(root, taskID, runID)
	if err != nil {
		return nil, nil, err
	}
	return openPrivateTaskLogNamedFile(root, taskID, filepath.Base(path))
}

func openPrivateTaskLogNamedFile(root string, taskID uuid.UUID, name string) (*os.File, os.FileInfo, error) {
	if taskID == uuid.Nil || filepath.Base(name) != name ||
		name != "legacy-unscoped.log" && uuid.Validate(strings.TrimSuffix(name, ".log")) != nil {
		return nil, nil, errRPCInvalid
	}
	root, err := normalizeTaskRunLogRoot(root)
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(root, taskID.String(), name)
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, os.ErrNotExist
	}
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, nil, errTaskLogUnsafe
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || !sameFilesystemPath(resolved, directory) {
		return nil, nil, errTaskLogUnsafe
	}
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, os.ErrNotExist
	}
	if err != nil || !before.Mode().IsRegular() || before.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, nil, errTaskLogUnsafe
	}
	resolved, err = filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(resolved, path) {
		return nil, nil, errTaskLogUnsafe
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || !taskLogFileHasSingleLink(file) {
		_ = file.Close()
		return nil, nil, errTaskLogUnsafe
	}
	return file, after, nil
}

type taskRunLogSnapshot struct {
	Generation uint64
	Size       uint64
	SHA256     string
}

type taskRunLogRequest struct {
	contents []byte
	weight   int64
	snapshot chan taskRunLogResult
	close    chan taskRunLogResult
}

type taskRunLogResult struct {
	snapshot taskRunLogSnapshot
	err      error
}

// taskRunLogWriter is the sole body writer for one run. Its weighted queue
// bounds queued encoded bytes; the worker publishes only complete flushed
// records and maintains the prefix digest used by active-log downloads.
type taskRunLogWriter struct {
	taskID          uuid.UUID
	runID           uuid.UUID
	generation      uint64
	path            string
	file            *os.File
	identity        os.FileInfo
	digest          hash.Hash
	buffer          *bufio.Writer
	queue           chan taskRunLogRequest
	capacity        *semaphore.Weighted
	done            chan struct{}
	failureCh       chan struct{}
	onPublish       func(taskRunLogSnapshot, bool)
	onFailure       func(error)
	diskCheck       func() error
	releaseCapacity func()

	enqueueMu           sync.Mutex
	mu                  sync.Mutex
	closed              bool
	reserved            uint64
	published           uint64
	failure             error
	closeOnce           sync.Once
	failureOnce         sync.Once
	failureNotifyOnce   sync.Once
	capacityReleaseOnce sync.Once
	close               taskRunLogResult
	diskCheckMu         sync.Mutex
	lastDiskCheck       time.Time
}

func openTaskRunLogWriter(
	root string,
	taskID, runID uuid.UUID,
	generation uint64,
	onPublish func(taskRunLogSnapshot, bool),
	onFailure func(error),
	diskCheck func() error,
	releaseCapacity func(),
) (*taskRunLogWriter, error) {
	if generation == 0 {
		return nil, errRPCInvalid
	}
	path, file, err := createPrivateTaskLogFile(root, taskID, runID, ".log")
	if err != nil {
		return nil, err
	}
	identity, err := file.Stat()
	if err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	digest := sha256.New()
	writer := &taskRunLogWriter{
		taskID: taskID, runID: runID, generation: generation, path: path, file: file, identity: identity, digest: digest,
		buffer: bufio.NewWriterSize(io.MultiWriter(file, digest), taskRunLogFlushBytes),
		queue:  make(chan taskRunLogRequest, 256), capacity: semaphore.NewWeighted(maximumTaskRunLogQueueBytes),
		done: make(chan struct{}), failureCh: make(chan struct{}), onPublish: onPublish, onFailure: onFailure,
		diskCheck: diskCheck, releaseCapacity: releaseCapacity, lastDiskCheck: time.Now(),
	}
	go writer.run()
	return writer, nil
}

func (writer *taskRunLogWriter) matchesFile(info os.FileInfo) bool {
	return writer != nil && writer.identity != nil && info != nil && os.SameFile(writer.identity, info)
}

func (writer *taskRunLogWriter) notifyFailure(cause error) {
	if writer == nil || cause == nil {
		return
	}
	writer.failureNotifyOnce.Do(func() {
		if writer.onFailure != nil {
			writer.onFailure(cause)
		}
	})
}

func (writer *taskRunLogWriter) Append(ctx context.Context, stream string, text string, binary []byte, occurredAt time.Time) error {
	if writer == nil || !validTaskV2LogStream(stream) || occurredAt.IsZero() {
		return errRPCInvalid
	}
	contents := encodeTaskRunLogRecords(stream, text, binary, occurredAt)
	if len(contents) == 0 {
		return nil
	}
	if len(contents) > maximumTaskRunLogQueueBytes {
		return errTaskLogOutputLimit
	}
	if err := writer.checkDiskPressure(); err != nil {
		return err
	}
	weight := int64(len(contents))
	if err := writer.capacity.Acquire(ctx, weight); err != nil {
		return err
	}
	writer.enqueueMu.Lock()
	defer writer.enqueueMu.Unlock()
	writer.mu.Lock()
	if writer.closed || writer.failure != nil {
		err := firstError(writer.failure, os.ErrClosed)
		writer.mu.Unlock()
		writer.capacity.Release(weight)
		return err
	}
	if writer.reserved+uint64(len(contents)) > maximumTaskRunLogFileBytes {
		writer.mu.Unlock()
		writer.capacity.Release(weight)
		return errTaskLogOutputLimit
	}
	writer.reserved += uint64(len(contents))
	writer.mu.Unlock()
	select {
	case writer.queue <- taskRunLogRequest{contents: contents, weight: weight}:
		select {
		case <-writer.failureCh:
			return firstError(writer.err(), os.ErrClosed)
		default:
		}
		return nil
	case <-ctx.Done():
		writer.releaseUnqueuedAppend(uint64(len(contents)), weight)
		return ctx.Err()
	case <-writer.failureCh:
		writer.releaseUnqueuedAppend(uint64(len(contents)), weight)
		return firstError(writer.err(), os.ErrClosed)
	case <-writer.done:
		writer.releaseUnqueuedAppend(uint64(len(contents)), weight)
		return firstError(writer.err(), os.ErrClosed)
	}
}

func (writer *taskRunLogWriter) releaseUnqueuedAppend(size uint64, weight int64) {
	writer.mu.Lock()
	if writer.reserved >= size {
		writer.reserved -= size
	}
	writer.mu.Unlock()
	writer.capacity.Release(weight)
}

func (writer *taskRunLogWriter) Snapshot(ctx context.Context) (taskRunLogSnapshot, error) {
	if writer == nil {
		return taskRunLogSnapshot{}, errRPCInvalid
	}
	response := make(chan taskRunLogResult, 1)
	writer.enqueueMu.Lock()
	writer.mu.Lock()
	closed := writer.closed
	failure := writer.failure
	result := writer.close
	writer.mu.Unlock()
	if failure != nil {
		writer.enqueueMu.Unlock()
		return writer.currentSnapshot(), failure
	}
	if closed {
		writer.enqueueMu.Unlock()
		select {
		case <-writer.done:
			writer.mu.Lock()
			result = writer.close
			if result.err == nil {
				result.err = writer.failure
			}
			writer.mu.Unlock()
			return result.snapshot, result.err
		case <-ctx.Done():
			return taskRunLogSnapshot{}, ctx.Err()
		}
	}
	select {
	case writer.queue <- taskRunLogRequest{snapshot: response}:
		writer.enqueueMu.Unlock()
	case <-ctx.Done():
		writer.enqueueMu.Unlock()
		return taskRunLogSnapshot{}, ctx.Err()
	case <-writer.failureCh:
		writer.enqueueMu.Unlock()
		return writer.currentSnapshot(), firstError(writer.err(), os.ErrClosed)
	case <-writer.done:
		writer.enqueueMu.Unlock()
		return taskRunLogSnapshot{}, firstError(writer.err(), os.ErrClosed)
	}
	select {
	case result := <-response:
		return result.snapshot, result.err
	case <-ctx.Done():
		return taskRunLogSnapshot{}, ctx.Err()
	case <-writer.failureCh:
		return writer.currentSnapshot(), firstError(writer.err(), os.ErrClosed)
	}
}

func (writer *taskRunLogWriter) Close() error {
	_, err := writer.Seal(context.Background())
	writer.releaseCapacityReservation()
	return err
}

func (writer *taskRunLogWriter) Seal(ctx context.Context) (taskRunLogSnapshot, error) {
	if writer == nil {
		return taskRunLogSnapshot{}, nil
	}
	// Once sealing begins it must always reach the worker. In particular, an
	// already-cancelled request context must not abandon the close request and
	// leave the caller waiting forever for done. Closing is run lifecycle work,
	// so it deliberately outlives the initiating RPC context.
	_ = ctx
	writer.closeOnce.Do(func() {
		response := make(chan taskRunLogResult, 1)
		writer.enqueueMu.Lock()
		writer.mu.Lock()
		writer.closed = true
		failure := writer.failure
		writer.mu.Unlock()
		if failure != nil {
			writer.enqueueMu.Unlock()
			return
		}
		select {
		case writer.queue <- taskRunLogRequest{close: response}:
			writer.enqueueMu.Unlock()
		case <-writer.failureCh:
			writer.enqueueMu.Unlock()
		case <-writer.done:
			writer.enqueueMu.Unlock()
		}
	})
	<-writer.done
	writer.mu.Lock()
	result := writer.close
	if result.err == nil {
		result.err = writer.failure
	}
	writer.mu.Unlock()
	return result.snapshot, result.err
}

func (writer *taskRunLogWriter) releaseCapacityReservation() {
	if writer == nil {
		return
	}
	writer.capacityReleaseOnce.Do(func() {
		if writer.releaseCapacity != nil {
			writer.releaseCapacity()
		}
	})
}

func (writer *taskRunLogWriter) checkDiskPressure() error {
	if writer == nil || writer.diskCheck == nil {
		return nil
	}
	writer.diskCheckMu.Lock()
	defer writer.diskCheckMu.Unlock()
	if time.Since(writer.lastDiskCheck) < taskRunLogSyncInterval {
		return nil
	}
	writer.lastDiskCheck = time.Now()
	return writer.diskCheck()
}

func (writer *taskRunLogWriter) PublishedSize() uint64 {
	if writer == nil {
		return 0
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.published
}

func (writer *taskRunLogWriter) err() error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.failure
}

func (writer *taskRunLogWriter) run() {
	defer close(writer.done)
	flushTicker := time.NewTicker(taskRunLogFlushInterval)
	defer flushTicker.Stop()
	lastSync := time.Now()
	var pending, sinceSync uint64
	flush := func(final bool) error {
		if pending > 0 {
			if err := writer.buffer.Flush(); err != nil {
				return err
			}
			writer.mu.Lock()
			writer.published += pending
			published := writer.published
			writer.mu.Unlock()
			sinceSync += pending
			pending = 0
			now := time.Now()
			if sinceSync >= taskRunLogSyncBytes || now.Sub(lastSync) >= taskRunLogSyncInterval || final {
				if err := writer.file.Sync(); err != nil {
					return err
				}
				sinceSync, lastSync = 0, now
			}
			if writer.onPublish != nil {
				snapshot := taskRunLogSnapshot{Generation: writer.generation, Size: published, SHA256: base64.RawURLEncoding.EncodeToString(writer.digest.Sum(nil))}
				writer.onPublish(snapshot, final)
			}
		} else if final || sinceSync > 0 && time.Since(lastSync) >= taskRunLogSyncInterval {
			if err := writer.file.Sync(); err != nil {
				return err
			}
			sinceSync, lastSync = 0, time.Now()
			if writer.onPublish != nil {
				writer.onPublish(writer.currentSnapshot(), final)
			}
		}
		return nil
	}
	fail := func(cause error) {
		writer.mu.Lock()
		if writer.failure == nil {
			writer.failure = cause
		}
		writer.closed = true
		writer.mu.Unlock()
		// Wake blocked senders before waiting for enqueueMu. A sender can hold
		// enqueueMu while a full queue waits for this worker to consume; without
		// this independent signal the two sides deadlock on a flush failure.
		writer.failureOnce.Do(func() { close(writer.failureCh) })
		writer.enqueueMu.Lock()
		writer.drainQueue(cause)
		writer.enqueueMu.Unlock()
		result := taskRunLogResult{snapshot: writer.currentSnapshot(), err: cause}
		writer.mu.Lock()
		writer.close = result
		writer.mu.Unlock()
		writer.notifyFailure(cause)
	}
	for {
		select {
		case request := <-writer.queue:
			if len(request.contents) > 0 {
				_, err := writer.buffer.Write(request.contents)
				pending += uint64(len(request.contents))
				writer.capacity.Release(request.weight)
				if err != nil {
					fail(err)
					_ = writer.file.Close()
					return
				}
				if pending >= taskRunLogFlushBytes {
					if err := flush(false); err != nil {
						fail(err)
						_ = writer.file.Close()
						return
					}
				}
				continue
			}
			if request.snapshot != nil {
				err := flush(false)
				if err != nil {
					fail(err)
				}
				request.snapshot <- taskRunLogResult{snapshot: writer.currentSnapshot(), err: err}
				if err != nil {
					_ = writer.file.Close()
					return
				}
				continue
			}
			if request.close != nil {
				err := flush(true)
				closeErr := writer.file.Close()
				err = errors.Join(err, closeErr)
				if err != nil {
					fail(err)
				}
				result := taskRunLogResult{snapshot: writer.currentSnapshot(), err: err}
				writer.mu.Lock()
				writer.close = result
				writer.mu.Unlock()
				request.close <- result
				return
			}
		case <-flushTicker.C:
			if pending > 0 || sinceSync > 0 && time.Since(lastSync) >= taskRunLogSyncInterval {
				if err := flush(false); err != nil {
					fail(err)
					_ = writer.file.Close()
					return
				}
			}
		}
	}
}

func (writer *taskRunLogWriter) currentSnapshot() taskRunLogSnapshot {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return taskRunLogSnapshot{
		Generation: writer.generation,
		Size:       writer.published,
		SHA256:     base64.RawURLEncoding.EncodeToString(writer.digest.Sum(nil)),
	}
}

func (writer *taskRunLogWriter) drainQueue(cause error) {
	for {
		select {
		case request := <-writer.queue:
			if request.weight > 0 {
				writer.capacity.Release(request.weight)
			}
			result := taskRunLogResult{snapshot: writer.currentSnapshot(), err: cause}
			if request.snapshot != nil {
				request.snapshot <- result
			}
			if request.close != nil {
				request.close <- result
			}
		default:
			return
		}
	}
}

func encodeTaskRunLogRecords(stream, text string, binary []byte, occurredAt time.Time) []byte {
	if !validTaskV2LogStream(stream) || occurredAt.IsZero() {
		return nil
	}
	if len(binary) > 0 {
		digest := sha256.Sum256(binary)
		text = fmt.Sprintf("<binary output omitted: bytes=%d sha256=%x>", len(binary), digest)
	}
	text = strings.TrimPrefix(text, "\ufeff")
	text = escapeTaskLogControls(text)
	sanitizer := newVTTextSanitizer()
	text = sanitizer.Feed(text) + sanitizer.Flush()
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if text == "" {
		return nil
	}
	prefix := occurredAt.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z") + " [" + stream + "] "
	var output strings.Builder
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	for _, logical := range lines {
		if logical == "" {
			output.WriteString(prefix)
			output.WriteByte('\n')
			continue
		}
		for _, part := range splitTaskRunLogBody(logical) {
			output.WriteString(prefix)
			output.WriteString(part)
			output.WriteByte('\n')
		}
	}
	return []byte(output.String())
}

func escapeTaskLogControls(text string) string {
	if text == "" {
		return ""
	}
	var output strings.Builder
	for _, character := range strings.ToValidUTF8(text, "�") {
		switch {
		case character == '\n' || character == '\r' || character == '\t' || character == 0x1b:
			output.WriteRune(character)
		case character < 0x20 || character == 0x7f:
			fmt.Fprintf(&output, "\\x%02X", character)
		default:
			output.WriteRune(character)
		}
	}
	return output.String()
}

func splitTaskRunLogBody(value string) []string {
	if value == "" {
		return nil
	}
	result := make([]string, 0, (len(value)+maximumTaskRunLogRecordBytes-1)/maximumTaskRunLogRecordBytes)
	for len(value) > 0 {
		cut := min(len(value), maximumTaskRunLogRecordBytes)
		for cut > 0 && !utf8.ValidString(value[:cut]) {
			cut--
		}
		if cut == 0 {
			_, width := utf8.DecodeRuneInString(value)
			cut = width
		}
		result = append(result, value[:cut])
		value = value[cut:]
	}
	return result
}
