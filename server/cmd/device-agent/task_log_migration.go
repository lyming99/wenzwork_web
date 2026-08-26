package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type taskLogMigrationOutput struct {
	file   *os.File
	buffer *bufio.Writer
	digest hash.Hash
	size   uint64
	path   string
}

func (store *taskV2Store) ReconcileTaskLogFiles(ctx context.Context) error {
	if store == nil || store.business == nil {
		return errRPCInvalid
	}
	db, err := store.business.openReadDB()
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT `+taskV2RunSelectColumns+`
		FROM task_runs WHERE log_state IN ('creating','active') ORDER BY created_at_ms, id`)
	if err != nil {
		_ = db.Close()
		return err
	}
	runs := make([]taskV2Run, 0)
	for rows.Next() {
		run, scanErr := scanTaskV2Run(rows)
		if scanErr != nil {
			_ = rows.Close()
			_ = db.Close()
			return scanErr
		}
		runs = append(runs, run)
	}
	err = rows.Close()
	_ = db.Close()
	if err != nil {
		return err
	}
	for _, run := range runs {
		file, _, openErr := openPrivateTaskLogFile(store.logRoot, run.TaskID, run.ID)
		if openErr != nil {
			if errors.Is(openErr, errTaskLogUnsafe) {
				_ = store.markRunLogReplaced(ctx, run.TaskID, run.ID, run.LogGeneration, time.Now().UTC())
			} else {
				_ = store.markRunLogMissing(ctx, run.TaskID, run.ID, run.LogGeneration, run.LogSizeBytes, time.Now().UTC())
			}
			continue
		}
		snapshot, verifyErr := hashValidatedTaskLog(file, run.LogGeneration)
		_ = file.Close()
		if verifyErr != nil {
			_ = store.markRunLogReplaced(ctx, run.TaskID, run.ID, run.LogGeneration, time.Now().UTC())
			continue
		}
		if snapshot.Size < run.LogSizeBytes {
			_ = store.markRunLogReplaced(ctx, run.TaskID, run.ID, run.LogGeneration, time.Now().UTC())
			continue
		}
		if err := store.persistSealedRunLog(ctx, run.TaskID, run.ID, snapshot, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func hashValidatedTaskLog(file *os.File, generation uint64) (taskRunLogSnapshot, error) {
	if file == nil || generation == 0 {
		return taskRunLogSnapshot{}, errRPCInvalid
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return taskRunLogSnapshot{}, err
	}
	digest := sha256.New()
	reader := bufio.NewReaderSize(file, maximumTaskLogPhysicalLineBytes+1)
	var size uint64
	for {
		line, err := reader.ReadSlice('\n')
		if len(line) > 0 {
			if err == bufio.ErrBufferFull || len(line) > maximumTaskLogPhysicalLineBytes || !utf8.Valid(line) || line[len(line)-1] != '\n' ||
				size+uint64(len(line)) > maximumTaskRunLogFileBytes {
				return taskRunLogSnapshot{}, errTaskLogUnsafe
			}
			_, _ = digest.Write(line)
			size += uint64(len(line))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return taskRunLogSnapshot{}, err
		}
	}
	return taskRunLogSnapshot{
		Generation: generation, Size: size,
		SHA256: base64.RawURLEncoding.EncodeToString(digest.Sum(nil)),
	}, nil
}

// MigrateLegacyTaskLogs exports one sealed run at a time. It never keeps more
// than one bounded row/chunk in memory and deletes source rows only after the
// file is synced, renamed, and its metadata transaction is committed.
func (store *taskV2Store) MigrateLegacyTaskLogs(ctx context.Context) error {
	if store == nil || store.business == nil {
		return errRPCInvalid
	}
	store.business.taskLogMigrationMu.Lock()
	defer store.business.taskLogMigrationMu.Unlock()
	if err := store.cleanupCommittedLegacyTaskLogRows(ctx); err != nil {
		return fmt.Errorf("clean up committed legacy task log rows: %w", err)
	}
	for {
		runs, err := store.legacyTaskLogCandidates(ctx, 8)
		if err != nil {
			return err
		}
		if len(runs) == 0 {
			break
		}
		for _, run := range runs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := store.migrateLegacyTaskRun(ctx, run); err != nil {
				// Leave the run in migrating state. The next startup removes the
				// bounded temporary file and retries from its unchanged source.
				return fmt.Errorf("migrate legacy task run: %w", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if err := store.migrateLegacyUnscopedLogs(ctx); err != nil {
		return fmt.Errorf("migrate legacy unscoped logs: %w", err)
	}
	return nil
}

// cleanupCommittedLegacyTaskLogRows closes the crash window between the
// durable metadata commit in finishLegacyRunMigration and its source-row
// deletion. Source text is removed only after the final private file is opened
// safely and its complete size/digest still match the sealed metadata.
func (store *taskV2Store) cleanupCommittedLegacyTaskLogRows(ctx context.Context) error {
	for {
		db, err := store.business.openReadDB()
		if err != nil {
			return err
		}
		run, scanErr := scanTaskV2Run(db.QueryRowContext(ctx, `SELECT `+taskV2RunSelectColumns+`
			FROM task_runs AS run
			WHERE run.log_state = 'sealed'
			  AND EXISTS(SELECT 1 FROM task_logs AS body WHERE body.task_id = run.task_id AND body.run_id = run.id)
			ORDER BY run.created_at_ms, run.id LIMIT 1`))
		_ = db.Close()
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil
		}
		if scanErr != nil {
			return scanErr
		}
		if run.LogGeneration == 0 || run.LogFormatVersion != 1 || run.LogSizeBytes > maximumTaskRunLogFileBytes || run.LogSHA256 == "" {
			return errTaskLogCorrupt
		}
		file, _, openErr := openPrivateTaskLogFile(store.logRoot, run.TaskID, run.ID)
		if openErr != nil {
			return errTaskLogCorrupt
		}
		snapshot, verifyErr := hashValidatedTaskLog(file, run.LogGeneration)
		_ = file.Close()
		if verifyErr != nil || snapshot.Generation != run.LogGeneration || snapshot.Size != run.LogSizeBytes || snapshot.SHA256 != run.LogSHA256 {
			return errTaskLogCorrupt
		}
		store.business.mu.Lock()
		db, err = store.business.openDB()
		if err == nil {
			_, err = db.ExecContext(ctx, `DELETE FROM task_logs
				WHERE task_id = ? AND run_id = ?
				  AND EXISTS(SELECT 1 FROM task_runs AS run
					WHERE run.id = ? AND run.task_id = ? AND run.log_state = 'sealed'
					  AND run.log_generation = ? AND run.log_format_version = 1
					  AND run.log_size_bytes = ? AND run.log_sha256 = ?)`,
				run.TaskID.String(), run.ID.String(), run.ID.String(), run.TaskID.String(),
				run.LogGeneration, run.LogSizeBytes, run.LogSHA256)
		}
		if db != nil {
			_ = db.Close()
		}
		store.business.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (store *taskV2Store) legacyTaskLogCandidates(ctx context.Context, limit int) ([]taskV2Run, error) {
	db, err := store.business.openReadDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT `+taskV2RunSelectColumns+`
		FROM task_runs AS run
		WHERE run.status NOT IN ('queued','waiting','running')
		  AND run.log_state IN ('none','migrating')
		  AND (run.log_path <> '' OR EXISTS(SELECT 1 FROM task_logs AS body WHERE body.run_id = run.id))
		ORDER BY run.created_at_ms, run.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]taskV2Run, 0, limit)
	for rows.Next() {
		run, err := scanTaskV2Run(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func (store *taskV2Store) migrateLegacyTaskRun(ctx context.Context, run taskV2Run) error {
	if run.ID == uuid.Nil || run.TaskID == uuid.Nil || run.Status == "running" || run.Status == "waiting" {
		return errRPCInvalid
	}
	releaseCapacity, err := store.reserveTaskLogCapacity(ctx, run.ID)
	if err != nil {
		return err
	}
	defer releaseCapacity()
	generation, err := store.markRunLogMigrating(ctx, run)
	if err != nil {
		return err
	}
	finalPath, err := taskRunLogPath(store.logRoot, run.TaskID, run.ID)
	if err != nil {
		return err
	}
	if final, _, openErr := openPrivateTaskLogFile(store.logRoot, run.TaskID, run.ID); openErr == nil {
		snapshot, verifyErr := hashValidatedTaskLog(final, generation)
		_ = final.Close()
		if verifyErr == nil {
			return store.finishLegacyRunMigration(ctx, run, snapshot)
		}
		if err := os.Remove(finalPath); err != nil {
			return err
		}
		if err := syncTaskLogDirectory(filepath.Dir(finalPath)); err != nil {
			return err
		}
		generation, err = store.advanceMigratingLogGeneration(ctx, run, generation)
		if err != nil {
			return err
		}
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return openErr
	}
	temporaryPath := finalPath + ".migrating"
	if err := removeSafeMigrationTemporary(temporaryPath); err != nil {
		return err
	}
	path, file, err := createPrivateTaskLogFile(store.logRoot, run.TaskID, run.ID, ".log.migrating")
	if err != nil {
		return err
	}
	output := &taskLogMigrationOutput{
		file: file, buffer: bufio.NewWriterSize(file, taskRunLogFlushBytes), digest: sha256.New(), path: path,
	}
	output.buffer = bufio.NewWriterSize(io.MultiWriter(file, output.digest), taskRunLogFlushBytes)
	sourceErr := store.exportLegacyRun(ctx, run, output)
	if sourceErr == nil {
		sourceErr = output.buffer.Flush()
	}
	if sourceErr == nil {
		sourceErr = output.file.Sync()
	}
	closeErr := output.file.Close()
	if sourceErr == nil {
		sourceErr = closeErr
	}
	if sourceErr != nil {
		_ = os.Remove(path)
		return sourceErr
	}
	if err := os.Rename(path, finalPath); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := syncTaskLogDirectory(filepath.Dir(finalPath)); err != nil {
		return err
	}
	snapshot := taskRunLogSnapshot{
		Generation: generation, Size: output.size,
		SHA256: base64.RawURLEncoding.EncodeToString(output.digest.Sum(nil)),
	}
	return store.finishLegacyRunMigration(ctx, run, snapshot)
}

func (store *taskV2Store) advanceMigratingLogGeneration(ctx context.Context, run taskV2Run, generation uint64) (uint64, error) {
	if generation == 0 || generation == ^uint64(0) {
		return 0, errTaskLogCorrupt
	}
	next := generation + 1
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	result, err := db.ExecContext(ctx, `UPDATE task_runs SET log_generation = ?, log_size_bytes = 0,
		log_sha256 = '', log_updated_at_ms = ? WHERE id = ? AND task_id = ? AND log_state = 'migrating' AND log_generation = ?`,
		next, time.Now().UTC().UnixMilli(), run.ID.String(), run.TaskID.String(), generation)
	if err != nil {
		return 0, err
	}
	if err := requireSingleTaskMutation(result); err != nil {
		return 0, err
	}
	return next, nil
}

func (store *taskV2Store) markRunLogMigrating(ctx context.Context, run taskV2Run) (uint64, error) {
	generation := run.LogGeneration
	if generation == 0 {
		generation = 1
	}
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err := store.business.openDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	result, err := db.ExecContext(ctx, `UPDATE task_runs SET log_state = 'migrating', log_generation = ?,
		log_format_version = 0, log_updated_at_ms = ? WHERE id = ? AND task_id = ? AND log_state IN ('none','migrating')`,
		generation, time.Now().UTC().UnixMilli(), run.ID.String(), run.TaskID.String())
	if err != nil {
		return 0, err
	}
	if err := requireSingleTaskMutation(result); err != nil {
		return 0, err
	}
	return generation, nil
}

func (store *taskV2Store) exportLegacyRun(ctx context.Context, run taskV2Run, output *taskLogMigrationOutput) error {
	if trustedLegacyTaskLogPath(run.LegacyLogPath, run.TaskID, run.ID) {
		if file, err := openTrustedLegacyTaskLog(run.LegacyLogPath); err == nil {
			defer file.Close()
			return exportLegacyTaskLogFile(ctx, file, run, output)
		}
	}
	return store.exportLegacyRunRows(ctx, run, output)
}

func openTrustedLegacyTaskLog(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, firstError(err, errTaskLogUnsafe)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || !taskLogFileHasSingleLink(file) {
		_ = file.Close()
		return nil, firstError(err, errTaskLogUnsafe)
	}
	return file, nil
}

func trustedLegacyTaskLogPath(path string, taskID, runID uuid.UUID) bool {
	if path == "" || !validTaskRunLogPath(path) {
		return false
	}
	expected, err := taskRunLogPath(defaultTaskRunLogRoot(), taskID, runID)
	if err != nil || !sameFilesystemPath(expected, path) {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || info.Size() < 0 || info.Size() > maximumTaskRunLogFileBytes {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && sameFilesystemPath(resolved, path)
}

func exportLegacyTaskLogFile(ctx context.Context, source *os.File, run taskV2Run, output *taskLogMigrationOutput) error {
	decoder := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
	buffer := make([]byte, 32<<10)
	when := run.CreatedAt
	if run.FinishedAt != nil {
		when = *run.FinishedAt
	}
	appendResults := func(results []CommandTextDecodeResult) error {
		for _, result := range results {
			text := "[legacy] " + result.DisplayText
			var binary []byte
			if result.IsBinary {
				text, binary = "", result.RawBytes
			}
			if err := output.append("system", text, binary, when); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := source.Read(buffer)
		if n > 0 {
			if appendErr := appendResults(decoder.Feed(buffer[:n])); appendErr != nil {
				return appendErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return appendResults(decoder.Flush())
}

func (store *taskV2Store) exportLegacyRunRows(ctx context.Context, run taskV2Run, output *taskLogMigrationOutput) error {
	db, err := store.business.openReadDB()
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT task_id, run_id, sequence, stream, content, display_content, source_encoding,
		is_binary, had_decode_errors, raw_available, occurred_at_ms FROM task_logs
		WHERE task_id = ? AND run_id = ? ORDER BY sequence`, run.TaskID.String(), run.ID.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	decoders := make(map[string]*CommandTextDecoder)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry, err := scanTaskV2Log(rows)
		if err != nil {
			return err
		}
		if entry.IsBinary {
			if err := output.append(entry.Stream, "", entry.Content, entry.OccurredAt); err != nil {
				return err
			}
			continue
		}
		text := entry.DisplayText
		if text == "" {
			decoder := decoders[entry.Stream]
			if decoder == nil {
				decoder = newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
				decoders[entry.Stream] = decoder
			}
			for _, decoded := range decoder.Feed(entry.Content) {
				if decoded.IsBinary {
					if err := output.append(entry.Stream, "", decoded.RawBytes, entry.OccurredAt); err != nil {
						return err
					}
				} else if err := output.append(entry.Stream, decoded.DisplayText, nil, entry.OccurredAt); err != nil {
					return err
				}
			}
			continue
		}
		if err := output.append(entry.Stream, text, nil, entry.OccurredAt); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for stream, decoder := range decoders {
		for _, decoded := range decoder.Flush() {
			if err := output.append(stream, decoded.DisplayText, decoded.RawBytesIfBinary(), run.CreatedAt); err != nil {
				return err
			}
		}
	}
	return nil
}

func (result CommandTextDecodeResult) RawBytesIfBinary() []byte {
	if result.IsBinary {
		return result.RawBytes
	}
	return nil
}

func (output *taskLogMigrationOutput) append(stream, text string, binary []byte, when time.Time) error {
	contents := encodeTaskRunLogRecords(stream, text, binary, when)
	if len(contents) == 0 {
		return nil
	}
	if output.size+uint64(len(contents)) > maximumTaskRunLogFileBytes {
		return errTaskLogOutputLimit
	}
	if _, err := output.buffer.Write(contents); err != nil {
		return err
	}
	output.size += uint64(len(contents))
	return nil
}

func (store *taskV2Store) finishLegacyRunMigration(ctx context.Context, run taskV2Run, snapshot taskRunLogSnapshot) error {
	store.business.mu.Lock()
	db, err := store.business.openDB()
	if err != nil {
		store.business.mu.Unlock()
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		_ = db.Close()
		store.business.mu.Unlock()
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE task_runs SET log_state = 'sealed', log_generation = ?, log_format_version = 1,
		log_size_bytes = ?, log_sha256 = ?, log_updated_at_ms = ?, log_path = ''
		WHERE id = ? AND task_id = ? AND log_state = 'migrating'`, snapshot.Generation, snapshot.Size, snapshot.SHA256,
		time.Now().UTC().UnixMilli(), run.ID.String(), run.TaskID.String())
	if err == nil {
		err = requireSingleTaskMutation(result)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO task_log_migration_reports(task_id, migrated_run_count, migrated_byte_count, updated_at_ms)
			VALUES(?, 1, ?, ?) ON CONFLICT(task_id) DO UPDATE SET
			migrated_run_count = migrated_run_count + 1,
			migrated_byte_count = migrated_byte_count + excluded.migrated_byte_count,
			updated_at_ms = excluded.updated_at_ms`, run.TaskID.String(), snapshot.Size, time.Now().UTC().UnixMilli())
	}
	if err == nil {
		err = commitBusinessTransaction(ctx, tx)
	} else {
		_ = tx.Rollback()
	}
	_ = db.Close()
	store.business.mu.Unlock()
	if err != nil {
		return err
	}
	// Source deletion is a separate short transaction after the durable file
	// metadata commit. A crash here merely leaves harmless rows for retry GC.
	store.business.mu.Lock()
	defer store.business.mu.Unlock()
	db, err = store.business.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `DELETE FROM task_logs WHERE task_id = ? AND run_id = ?`, run.TaskID.String(), run.ID.String())
	return err
}

func removeSafeMigrationTemporary(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errTaskLogUnsafe
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(resolved, path) {
		return errTaskLogUnsafe
	}
	return os.Remove(path)
}

func (store *taskV2Store) migrateLegacyUnscopedLogs(ctx context.Context) error {
	db, err := store.business.openReadDB()
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT task_id FROM task_logs WHERE run_id IS NULL ORDER BY task_id`)
	if err != nil {
		_ = db.Close()
		return err
	}
	tasks := make([]uuid.UUID, 0)
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			_ = rows.Close()
			_ = db.Close()
			return err
		}
		id, err := uuid.Parse(text)
		if err != nil || id == uuid.Nil {
			_ = rows.Close()
			_ = db.Close()
			return errTaskLogUnsafe
		}
		tasks = append(tasks, id)
	}
	err = rows.Close()
	_ = db.Close()
	if err != nil {
		return err
	}
	for _, taskID := range tasks {
		if err := store.migrateLegacyUnscopedTask(ctx, taskID); err != nil {
			return err
		}
	}
	return nil
}

func (store *taskV2Store) migrateLegacyUnscopedTask(ctx context.Context, taskID uuid.UUID) error {
	if err := store.checkTaskLogDiskSpace(maximumTaskRunLogFileBytes); err != nil {
		return err
	}
	directory, err := preparePrivateTaskLogDirectory(store.logRoot, taskID)
	if err != nil {
		return err
	}
	finalPath := filepath.Join(directory, "legacy-unscoped.log")
	temporaryPath := finalPath + ".migrating"
	var snapshot taskRunLogSnapshot
	var rowCount uint64
	if existing, _, err := openPrivateTaskLogNamedFile(store.logRoot, taskID, "legacy-unscoped.log"); err == nil {
		snapshot, err = hashValidatedTaskLog(existing, 1)
		_ = existing.Close()
		if err != nil {
			return err
		}
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := removeSafeMigrationTemporary(temporaryPath); err != nil {
			return err
		}
		createdPath, file, err := createPrivateTaskLogNamedFile(store.logRoot, taskID, "legacy-unscoped.log.migrating")
		if err != nil {
			return err
		}
		if createdPath != temporaryPath {
			_ = file.Close()
			_ = os.Remove(createdPath)
			return errTaskLogUnsafe
		}
		digest := sha256.New()
		output := &taskLogMigrationOutput{
			file: file, buffer: bufio.NewWriterSize(io.MultiWriter(file, digest), taskRunLogFlushBytes),
			digest: digest, path: temporaryPath,
		}
		db, err := store.business.openReadDB()
		if err != nil {
			_ = file.Close()
			_ = os.Remove(temporaryPath)
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT task_id, run_id, sequence, stream, content, display_content, source_encoding,
			is_binary, had_decode_errors, raw_available, occurred_at_ms FROM task_logs
			WHERE task_id = ? AND run_id IS NULL ORDER BY sequence`, taskID.String())
		if err == nil {
			for rows.Next() {
				entry, scanErr := scanTaskV2Log(rows)
				if scanErr != nil {
					err = scanErr
					break
				}
				text, binary := entry.DisplayText, []byte(nil)
				if entry.IsBinary {
					binary = entry.Content
				} else if text == "" {
					decoder := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
					for _, decoded := range append(decoder.Feed(entry.Content), decoder.Flush()...) {
						if decoded.IsBinary {
							binary = decoded.RawBytes
						} else {
							text += decoded.DisplayText
						}
					}
				}
				if appendErr := output.append(entry.Stream, "[legacy-unscoped] "+text, binary, entry.OccurredAt); appendErr != nil {
					err = appendErr
					break
				}
				rowCount++
			}
			if rowsErr := rows.Close(); err == nil {
				err = rowsErr
			}
		}
		_ = db.Close()
		if err == nil {
			err = output.buffer.Flush()
		}
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(temporaryPath)
			return err
		}
		if err := os.Rename(temporaryPath, finalPath); err != nil {
			_ = os.Remove(temporaryPath)
			return err
		}
		if err := syncTaskLogDirectory(directory); err != nil {
			return err
		}
		snapshot = taskRunLogSnapshot{Generation: 1, Size: output.size, SHA256: base64.RawURLEncoding.EncodeToString(digest.Sum(nil))}
	}
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
	if rowCount == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_logs WHERE task_id = ? AND run_id IS NULL`, taskID.String()).Scan(&rowCount); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_log_migration_reports(
		task_id, legacy_unscoped_rows, legacy_unscoped_sha256, updated_at_ms)
		VALUES(?, ?, ?, ?) ON CONFLICT(task_id) DO UPDATE SET
		legacy_unscoped_rows = excluded.legacy_unscoped_rows,
		legacy_unscoped_sha256 = excluded.legacy_unscoped_sha256,
		updated_at_ms = excluded.updated_at_ms`, taskID.String(), rowCount, snapshot.SHA256, time.Now().UTC().UnixMilli()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_logs WHERE task_id = ? AND run_id IS NULL`, taskID.String()); err != nil {
		return err
	}
	return commitBusinessTransaction(ctx, tx)
}
