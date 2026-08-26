package main

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTaskPayloadV2PrepareChunkCommitReplayAndReconnect(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	store, err := openTaskPayloadStore(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.NewString()
	transferID := uuid.NewString()
	payload := []byte(`{"prompt":"` + strings.Repeat("界", 40_000) + `"}`)
	prepare := rpcInput{
		"transferId": transferID, "targetMethod": "task.create", "totalBytes": float64(len(payload)), "sha256": sha256Hex(payload),
	}
	manifest, err := store.prepare(projectID, prepare)
	if err != nil || manifest.(map[string]any)["chunkBytes"] != taskPayloadChunkBytes {
		t.Fatalf("prepare = %#v, error=%v", manifest, err)
	}

	for offset := 0; offset < len(payload); offset += taskPayloadChunkBytes {
		end := min(offset+taskPayloadChunkBytes, len(payload))
		chunk := payload[offset:end]
		input := rpcInput{
			"transferId": transferID, "offset": float64(offset), "base64Data": base64.StdEncoding.EncodeToString(chunk),
			"chunkSha256": sha256Hex(chunk),
		}
		result, err := store.chunk(projectID, input)
		if err != nil || result.(map[string]any)["nextOffset"] != int64(end) {
			t.Fatalf("chunk %d = %#v, error=%v", offset, result, err)
		}
		if offset == 0 {
			replay, replayErr := store.chunk(projectID, input)
			if replayErr != nil || replay.(map[string]any)["replayed"] != true {
				t.Fatalf("chunk replay = %#v, error=%v", replay, replayErr)
			}
		}
	}

	committed, err := store.commit(projectID, rpcInput{"transferId": transferID, "idempotencyKey": "commit-1"})
	if err != nil || committed.(map[string]any)["payloadTransferId"] != transferID {
		t.Fatalf("commit = %#v, error=%v", committed, err)
	}
	replayed, err := store.commit(projectID, rpcInput{"transferId": transferID, "idempotencyKey": "commit-1"})
	if err != nil || replayed.(map[string]any)["replayed"] != true {
		t.Fatalf("commit replay = %#v, error=%v", replayed, err)
	}
	if _, err := store.commit(projectID, rpcInput{"transferId": transferID, "idempotencyKey": "commit-conflict"}); !errors.Is(err, errRPCIdempotency) {
		t.Fatalf("commit conflict = %v", err)
	}

	reconnected, err := openTaskPayloadStore(root, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := reconnected.resolve(projectID, "task.create", transferID)
	if err != nil || resolved["prompt"] != strings.Repeat("界", 40_000) {
		t.Fatalf("reconnected resolve = %d fields, error=%v", len(resolved), err)
	}
	if _, err := reconnected.resolve(uuid.NewString(), "task.create", transferID); !errors.Is(err, errRPCProject) {
		t.Fatalf("cross-project resolve = %v", err)
	}
}

func TestTaskPayloadV2RejectsOffsetHashTTLAndScopeViolations(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	store, err := openTaskPayloadStore(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	projectID, transferID := uuid.NewString(), uuid.NewString()
	payload := []byte(`{"value":"` + strings.Repeat("x", maximumRPCPayload) + `"}`)
	_, err = store.prepare(projectID, rpcInput{
		"transferId": transferID, "targetMethod": "workflow.create", "totalBytes": float64(len(payload)), "sha256": sha256Hex(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	chunk := payload[:taskPayloadChunkBytes]
	base := rpcInput{
		"transferId": transferID, "offset": float64(1), "base64Data": base64.StdEncoding.EncodeToString(chunk), "chunkSha256": sha256Hex(chunk),
	}
	if _, err := store.chunk(projectID, base); !errors.Is(err, errRPCRevision) {
		t.Fatalf("out-of-order chunk error = %v", err)
	}
	base["offset"] = float64(0)
	base["chunkSha256"] = strings.Repeat("0", 64)
	if _, err := store.chunk(projectID, base); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("invalid hash error = %v", err)
	}
	if methodScope("task.payload.prepare") != "remote.peer.task.control" ||
		methodAllowsScope("task.payload.prepare", "remote.peer.query") {
		t.Fatal("task payload methods escaped their dedicated scope")
	}
	now = now.Add(taskPayloadTTL + time.Second)
	store.mu.Lock()
	store.cleanupExpiredLocked()
	_, found := store.transfers[transferID]
	store.mu.Unlock()
	if found {
		t.Fatal("expired task payload was retained")
	}
}

func TestTaskPayloadV2EnforcesQuotaConcurrencyAndAbortCleanup(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	projectID := uuid.NewString()
	payload := []byte(`{"value":"` + strings.Repeat("x", maximumRPCPayload) + `"}`)
	prepareInput := func(id string) rpcInput {
		return rpcInput{
			"transferId": id, "targetMethod": "task.create", "totalBytes": float64(len(payload)), "sha256": sha256Hex(payload),
		}
	}

	quotaStore, err := openTaskPayloadStore(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	quotaStore.quotaBytes = int64(len(payload)) + 1
	firstID, secondID := uuid.NewString(), uuid.NewString()
	if _, err := quotaStore.prepare(projectID, prepareInput(firstID)); err != nil {
		t.Fatal(err)
	}
	if _, err := quotaStore.prepare(projectID, prepareInput(secondID)); !errors.Is(err, errRPCBusy) {
		t.Fatalf("quota error = %v", err)
	}
	if _, err := quotaStore.abort(projectID, rpcInput{"transferId": firstID}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{quotaStore.metaPath(firstID), quotaStore.partPath(firstID), quotaStore.readyPath(firstID)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("aborted artifact %q was retained: %v", path, err)
		}
	}
	if _, err := quotaStore.prepare(projectID, prepareInput(secondID)); err != nil {
		t.Fatalf("prepare after abort = %v", err)
	}

	concurrencyStore, err := openTaskPayloadStore(t.TempDir(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	concurrencyStore.maximumTransfers = 1
	if _, err := concurrencyStore.prepare(projectID, prepareInput(uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	if _, err := concurrencyStore.prepare(projectID, prepareInput(uuid.NewString())); !errors.Is(err, errRPCBusy) {
		t.Fatalf("concurrency error = %v", err)
	}
}

func TestTaskPayloadV2RejectsCorruptionAndRecoversCrashArtifacts(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	store, err := openTaskPayloadStore(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	projectID, corruptID := uuid.NewString(), uuid.NewString()
	payload := []byte(`{"value":"` + strings.Repeat("z", maximumRPCPayload) + `"}`)
	prepare := func(id string) {
		t.Helper()
		if _, err := store.prepare(projectID, rpcInput{
			"transferId": id, "targetMethod": "workflow.create", "totalBytes": float64(len(payload)), "sha256": sha256Hex(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}
	prepare(corruptID)
	writeTaskPayloadChunks(t, store, projectID, corruptID, payload, 0)
	corrupted := append([]byte(nil), payload...)
	corrupted[len(corrupted)-1] ^= 0xff
	if err := os.WriteFile(store.partPath(corruptID), corrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.commit(projectID, rpcInput{"transferId": corruptID, "idempotencyKey": "corrupt-commit"}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("corrupt commit error = %v", err)
	}
	if _, err := store.abort(projectID, rpcInput{"transferId": corruptID}); err != nil {
		t.Fatal(err)
	}

	resumeID := uuid.NewString()
	prepare(resumeID)
	firstChunk := payload[:taskPayloadChunkBytes]
	writeTaskPayloadChunks(t, store, projectID, resumeID, firstChunk, 0)
	file, err := os.OpenFile(store.partPath(resumeID), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("crash-tail")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openTaskPayloadStore(root, func() time.Time { return now.Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(store.partPath(resumeID)); err != nil || info.Size() != int64(len(firstChunk)) {
		t.Fatalf("recovered partial size = %v, error=%v", info, err)
	}
	writeTaskPayloadChunks(t, store, projectID, resumeID, payload[len(firstChunk):], len(firstChunk))
	if err := os.Rename(store.partPath(resumeID), store.readyPath(resumeID)); err != nil {
		t.Fatal(err)
	}
	store, err = openTaskPayloadStore(root, func() time.Time { return now.Add(2 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.partPath(resumeID)); err != nil {
		t.Fatalf("interrupted commit was not rolled back on restart: %v", err)
	}
	if _, err := store.commit(projectID, rpcInput{"transferId": resumeID, "idempotencyKey": "resume-commit"}); err != nil {
		t.Fatalf("commit after crash recovery = %v", err)
	}

	orphanID := uuid.NewString()
	if err := os.WriteFile(store.partPath(orphanID), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.metaPath(orphanID)+".tmp", []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openTaskPayloadStore(root, func() time.Time { return now.Add(3 * time.Minute) }); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{store.partPath(orphanID), store.metaPath(orphanID) + ".tmp"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan artifact %q was retained: %v", path, err)
		}
	}
}

func writeTaskPayloadChunks(t *testing.T, store *taskPayloadStore, projectID, transferID string, payload []byte, startingOffset int) {
	t.Helper()
	for relativeOffset := 0; relativeOffset < len(payload); relativeOffset += taskPayloadChunkBytes {
		end := min(relativeOffset+taskPayloadChunkBytes, len(payload))
		chunk := payload[relativeOffset:end]
		offset := startingOffset + relativeOffset
		if _, err := store.chunk(projectID, rpcInput{
			"transferId": transferID, "offset": float64(offset), "base64Data": base64.StdEncoding.EncodeToString(chunk), "chunkSha256": sha256Hex(chunk),
		}); err != nil {
			t.Fatalf("chunk at %d = %v", offset, err)
		}
	}
}
