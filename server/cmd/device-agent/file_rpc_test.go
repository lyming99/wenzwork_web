package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestFileRPCListContractPaginationAndCursorReset(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.receive")
	mustMkdir(t, filepath.Join(dispatch.state.Workspace, "alpha"))
	mustWriteFile(t, filepath.Join(dispatch.state.Workspace, "bravo.txt"), []byte("bravo"))
	mustWriteFile(t, filepath.Join(dispatch.state.Workspace, "charlie.txt"), []byte("charlie"))

	first := mustFileRPC(t, dispatch, "file.list", rpcInput{"path": "", "limit": float64(2)})
	assertJSONKeys(t, first, "highWatermark", "items", "nextCursor", "resetRequired")
	if first["resetRequired"] != false || first["highWatermark"] != float64(dispatch.state.Revision) {
		t.Fatalf("first page metadata = %#v", first)
	}
	items := first["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["name"] != "alpha" || items[0].(map[string]any)["kind"] != "directory" {
		t.Fatalf("first page items = %#v", items)
	}
	for _, item := range items {
		assertJSONKeys(t, item.(map[string]any), "category", "extension", "id", "kind", "modifiedAt", "name", "readable", "relativePath", "revision", "size", "writable")
	}
	cursor, ok := first["nextCursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("nextCursor = %#v", first["nextCursor"])
	}
	second := mustFileRPC(t, dispatch, "file.list", rpcInput{"path": "", "limit": float64(2), "cursor": cursor})
	if len(second["items"].([]any)) != 1 || second["nextCursor"] != nil || second["resetRequired"] != false {
		t.Fatalf("second page = %#v", second)
	}

	mustWriteFile(t, filepath.Join(dispatch.state.Workspace, "delta.txt"), []byte("delta"))
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(dispatch.state.Workspace, future, future); err != nil {
		t.Fatal(err)
	}
	reset := mustFileRPC(t, dispatch, "file.list", rpcInput{"path": "", "limit": float64(2), "cursor": cursor})
	if reset["resetRequired"] != true || len(reset["items"].([]any)) != 0 || reset["nextCursor"] != nil {
		t.Fatalf("reset page = %#v", reset)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	tampered := base64.RawURLEncoding.EncodeToString(append(decoded, []byte(` {}`)...))
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.list", rpcInput{"cursor": tampered}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("trailing cursor error = %v", err)
	}
}

func TestFileRPCListBudgetsCompleteEncodedResponseAt48KiB(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.receive")
	for index := 0; index < 200; index++ {
		name := fmt.Sprintf("%03d-%s.txt", index, strings.Repeat("x", 160))
		mustWriteFile(t, filepath.Join(dispatch.state.Workspace, name), []byte("x"))
	}

	seen := make(map[string]struct{}, 200)
	var cursor *string
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 20 {
			t.Fatal("file pagination did not terminate")
		}
		input := rpcInput{"path": "", "limit": float64(200)}
		if cursor != nil {
			input["cursor"] = *cursor
		}
		output, _, err := dispatch.callFileRPC(context.Background(), "file.list", input)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(output)
		if err != nil || len(encoded) > preferredRPCPagePayload {
			t.Fatalf("file page bytes=%d error=%v", len(encoded), err)
		}
		page := output.(map[string]any)
		items := page["items"].([]remoteFileEntry)
		if len(items) == 0 {
			t.Fatal("file page did not advance")
		}
		for _, item := range items {
			if _, duplicate := seen[item.ID]; duplicate {
				t.Fatalf("duplicate file entry %q", item.Name)
			}
			seen[item.ID] = struct{}{}
		}
		next, _ := page["nextCursor"].(*string)
		cursor = next
		if cursor == nil {
			break
		}
	}
	if len(seen) != 200 {
		t.Fatalf("listed %d files, want 200", len(seen))
	}
}

func TestFileRPCRejectsPathEscapeSymlinksJunctionsAndWindowsNames(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.receive")
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), []byte("secret"))

	for _, path := range []string{"../secret.txt", "/etc/passwd", `C:/Windows/system.ini`, `folder\\child`, "folder//child", "CON/readme"} {
		if _, _, err := dispatch.callFileRPC(context.Background(), "file.stat", rpcInput{"path": path}); !errors.Is(err, errRPCForbidden) {
			t.Errorf("file.stat(%q) error = %v", path, err)
		}
	}

	invalidNames := []string{"CON", "con.txt", "PRN.log", "AUX", "NUL.data", "COM1", "com9.txt", "LPT1", "lpt9.md", "ads:stream", `quote"`, "less<", "more>", "pipe|", "question?", "star*", "tail.", "tail "}
	for _, name := range invalidNames {
		if validFileName(name) {
			t.Errorf("validFileName(%q) = true", name)
		}
	}
	for _, name := range []string{"readme.md", ".gitignore", "COM0", "LPT10", "space inside.txt"} {
		if !validFileName(name) {
			t.Errorf("validFileName(%q) = false", name)
		}
	}

	link := filepath.Join(dispatch.state.Workspace, "outside-link")
	if err := os.Symlink(outside, link); err == nil {
		if _, _, err := dispatch.callFileRPC(context.Background(), "file.stat", rpcInput{"path": "outside-link/secret.txt"}); !errors.Is(err, errRPCForbidden) {
			t.Fatalf("symlink traversal error = %v", err)
		}
	} else {
		t.Logf("symlink creation unavailable: %v", err)
	}

	if runtime.GOOS == "windows" {
		junction := filepath.Join(dispatch.state.Workspace, "outside-junction")
		if strings.ContainsAny(junction+outside, " &|<>()^") {
			t.Skip("temporary paths are not safe for the cmd.exe junction fixture")
		}
		commandLine := fmt.Sprintf("mklink /J %s %s", junction, outside)
		command := exec.Command("cmd.exe", "/d", "/c", commandLine)
		if output, err := command.CombinedOutput(); err == nil {
			t.Cleanup(func() { _ = os.Remove(junction) })
			if _, _, err := dispatch.callFileRPC(context.Background(), "file.stat", rpcInput{"path": "outside-junction/secret.txt"}); !errors.Is(err, errRPCForbidden) {
				t.Fatalf("junction traversal error = %v", err)
			}
		} else {
			t.Logf("junction creation unavailable: %v (%s)", err, output)
		}
	}
}

func TestFileRPCTextMutationLifecycleAndOptimisticConcurrency(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.send")
	target := filepath.Join(dispatch.state.Workspace, "note.md")
	mustWriteFile(t, target, []byte("hello"))
	receive := dispatch
	receive.scope = "remote.peer.file.receive"

	stat := mustFileRPC(t, receive, "file.stat", rpcInput{"path": "note.md"})
	revision := uint64(stat["entry"].(map[string]any)["revision"].(float64))
	read := mustFileRPC(t, receive, "file.read-text", rpcInput{"path": "note.md", "maxBytes": float64(maximumTextReadBytes)})
	if read["content"] != "hello" || read["revision"] != float64(revision) || read["truncated"] != false {
		t.Fatalf("read response = %#v", read)
	}
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.write-text", rpcInput{"path": "note.md", "expectedRevision": float64(revision + 1), "content": "lost"}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale write error = %v", err)
	}
	if got := string(mustReadFile(t, target)); got != "hello" {
		t.Fatalf("stale write changed file to %q", got)
	}
	written := mustFileRPC(t, dispatch, "file.write-text", rpcInput{"path": "note.md", "expectedRevision": float64(revision), "content": "updated ✓"})
	if written["revision"] == float64(revision) || string(mustReadFile(t, target)) != "updated ✓" {
		t.Fatalf("write response = %#v content=%q", written, mustReadFile(t, target))
	}
	if matches, err := filepath.Glob(filepath.Join(dispatch.state.Workspace, ".wenzwork-text-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary text files = %v, %v", matches, err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	expected := workspaceFileRevision("note.md", info)
	mustWriteFile(t, target, []byte("external writer"))
	future := time.Now().Add(3 * time.Second)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteWorkspaceFile(target, "note.md", expected, []byte("must not win"), 0o600); !errors.Is(err, errRPCRevision) {
		t.Fatalf("pre-replace revision check error = %v", err)
	}
	if got := string(mustReadFile(t, target)); got != "external writer" {
		t.Fatalf("pre-replace check overwrote external content: %q", got)
	}

	created := mustFileRPC(t, dispatch, "file.mkdir", rpcInput{"parentPath": "", "name": "docs"})
	directoryRevision := uint64(created["revision"].(float64))
	mustWriteFile(t, filepath.Join(dispatch.state.Workspace, "docs", "child.txt"), []byte("child"))
	statDirectory := mustFileRPC(t, receive, "file.stat", rpcInput{"path": "docs"})
	directoryRevision = uint64(statDirectory["entry"].(map[string]any)["revision"].(float64))
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.delete", rpcInput{"path": "docs", "expectedRevision": float64(directoryRevision)}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("non-empty directory delete error = %v", err)
	}
	childStat := mustFileRPC(t, receive, "file.stat", rpcInput{"path": "docs/child.txt"})
	childRevision := childStat["entry"].(map[string]any)["revision"].(float64)
	renamed := mustFileRPC(t, dispatch, "file.rename", rpcInput{"path": "docs/child.txt", "expectedRevision": childRevision, "name": "renamed.txt"})
	renamedRevision := renamed["revision"].(float64)
	mustFileRPC(t, dispatch, "file.delete", rpcInput{"path": "docs/renamed.txt", "expectedRevision": renamedRevision})
	directoryStat := mustFileRPC(t, receive, "file.stat", rpcInput{"path": "docs"})
	mustFileRPC(t, dispatch, "file.delete", rpcInput{"path": "docs", "expectedRevision": directoryStat["entry"].(map[string]any)["revision"]})
	if _, err := os.Stat(filepath.Join(dispatch.state.Workspace, "docs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted directory stat error = %v", err)
	}
}

func TestFileRPCDownloadLifecycleIntegrityAndEmptyFile(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.receive")
	payload := bytes.Repeat([]byte("download-payload-"), 7000)
	target := filepath.Join(dispatch.state.Workspace, "archive.bin")
	mustWriteFile(t, target, payload)
	digest := testDigest(payload)
	stat := mustFileRPC(t, dispatch, "file.stat", rpcInput{"path": "archive.bin"})
	revision := stat["entry"].(map[string]any)["revision"].(float64)
	if _, _, err := dispatch.callFileRPC(t.Context(), "file.download.prepare", rpcInput{
		"transferId": "download-stale", "path": "archive.bin", "expectedRevision": revision + 1,
	}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale download preparation error = %v", err)
	}

	prepared := mustFileRPC(t, dispatch, "file.download.prepare", rpcInput{
		"transferId": "download-0001", "path": "archive.bin", "expectedRevision": revision,
	})
	if prepared["size"] != float64(len(payload)) || prepared["sha256"] != digest || prepared["chunkSize"] != float64(fileChunkBytes) || prepared["acceptedOffset"] != float64(0) {
		t.Fatalf("prepare = %#v", prepared)
	}
	var assembled []byte
	for offset := 0; offset < len(payload); {
		chunk := mustFileRPC(t, dispatch, "file.download.chunk", rpcInput{"transferId": "download-0001", "offset": float64(offset), "maxBytes": float64(12345)})
		data, err := base64.RawURLEncoding.Strict().DecodeString(chunk["data"].(string))
		if err != nil || chunk["offset"] != float64(offset) || chunk["total"] != float64(len(payload)) {
			t.Fatalf("chunk at %d = %#v / %v", offset, chunk, err)
		}
		assembled = append(assembled, data...)
		offset += len(data)
	}
	if !bytes.Equal(assembled, payload) {
		t.Fatal("download reassembly differs")
	}
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.download.complete", rpcInput{"transferId": "download-0001", "sha256": testDigest([]byte("wrong"))}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("wrong completion hash error = %v", err)
	}
	completed := mustFileRPC(t, dispatch, "file.download.complete", rpcInput{"transferId": "download-0001", "sha256": digest})
	if completed["completed"] != true || completed["sha256"] != digest {
		t.Fatalf("complete = %#v", completed)
	}
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.download.chunk", rpcInput{"transferId": "download-0001", "offset": float64(0)}); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("completed transfer lookup error = %v", err)
	}

	mustWriteFile(t, filepath.Join(dispatch.state.Workspace, "empty.bin"), nil)
	emptyDigest := testDigest(nil)
	empty := mustFileRPC(t, dispatch, "file.download.prepare", rpcInput{"transferId": "download-empty", "path": "empty.bin"})
	if empty["size"] != float64(0) || empty["sha256"] != emptyDigest {
		t.Fatalf("empty prepare = %#v", empty)
	}
	mustFileRPC(t, dispatch, "file.download.complete", rpcInput{"transferId": "download-empty", "sha256": emptyDigest})
}

func TestFileRPCUploadResumeReplayHashAndAtomicPublish(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.send")
	payload := bytes.Repeat([]byte("upload-payload-"), 7000)
	digest := testDigest(payload)
	target := filepath.Join(dispatch.state.Workspace, "incoming.bin")

	prepared := mustFileRPC(t, dispatch, "file.upload.prepare", rpcInput{"transferId": "upload-000001", "path": "incoming.bin", "size": float64(len(payload)), "sha256": digest})
	if prepared["acceptedOffset"] != float64(0) || prepared["chunkSize"] != float64(fileChunkBytes) {
		t.Fatalf("prepare = %#v", prepared)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upload target visible before complete: %v", err)
	}
	secondOffset := min(fileChunkBytes, len(payload))
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.upload.chunk", uploadChunkInput("upload-000001", secondOffset, payload[secondOffset:min(secondOffset+fileChunkBytes, len(payload))])); !errors.Is(err, errRPCRevision) {
		t.Fatalf("out-of-order chunk error = %v", err)
	}
	firstData := payload[:secondOffset]
	bad := uploadChunkInput("upload-000001", 0, firstData)
	bad["chunkSha256"] = testDigest([]byte("wrong"))
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.upload.chunk", bad); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("bad chunk hash error = %v", err)
	}
	first := mustFileRPC(t, dispatch, "file.upload.chunk", uploadChunkInput("upload-000001", 0, firstData))
	if first["nextOffset"] != float64(secondOffset) || first["replayed"] != false {
		t.Fatalf("first chunk = %#v", first)
	}
	replayed := mustFileRPC(t, dispatch, "file.upload.chunk", uploadChunkInput("upload-000001", 0, firstData))
	if replayed["nextOffset"] != float64(secondOffset) || replayed["replayed"] != true {
		t.Fatalf("replayed chunk = %#v", replayed)
	}
	offset := secondOffset
	for offset < len(payload) {
		end := min(offset+fileChunkBytes, len(payload))
		mustFileRPC(t, dispatch, "file.upload.chunk", uploadChunkInput("upload-000001", offset, payload[offset:end]))
		offset = end
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("upload target visible at offset %d: %v", offset, err)
		}
	}
	resumed := mustFileRPC(t, dispatch, "file.upload.prepare", rpcInput{"transferId": "upload-000001", "path": "incoming.bin", "size": float64(len(payload)), "sha256": digest})
	if resumed["acceptedOffset"] != float64(len(payload)) {
		t.Fatalf("resumed prepare = %#v", resumed)
	}
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.upload.complete", rpcInput{"transferId": "upload-000001", "size": float64(len(payload)), "sha256": testDigest([]byte("wrong"))}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("wrong declared hash error = %v", err)
	}
	completed := mustFileRPC(t, dispatch, "file.upload.complete", rpcInput{"transferId": "upload-000001", "size": float64(len(payload)), "sha256": digest})
	if completed["completed"] != true || completed["size"] != float64(len(payload)) || !bytes.Equal(mustReadFile(t, target), payload) {
		t.Fatalf("complete = %#v", completed)
	}
	if matches, err := filepath.Glob(filepath.Join(dispatch.state.Workspace, ".wenzwork-upload-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary upload files = %v, %v", matches, err)
	}
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.upload.prepare", rpcInput{"transferId": "upload-000002", "path": "incoming.bin", "size": float64(len(payload)), "sha256": digest}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("existing target prepare error = %v", err)
	}

	badTarget := filepath.Join(dispatch.state.Workspace, "bad-hash.bin")
	wrongDigest := testDigest([]byte("not the payload"))
	mustFileRPC(t, dispatch, "file.upload.prepare", rpcInput{"transferId": "upload-badhash", "path": "bad-hash.bin", "size": float64(len(firstData)), "sha256": wrongDigest})
	mustFileRPC(t, dispatch, "file.upload.chunk", uploadChunkInput("upload-badhash", 0, firstData))
	if _, _, err := dispatch.callFileRPC(context.Background(), "file.upload.complete", rpcInput{"transferId": "upload-badhash", "size": float64(len(firstData)), "sha256": wrongDigest}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("computed hash mismatch error = %v", err)
	}
	if _, err := os.Stat(badTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bad-hash target exists: %v", err)
	}
}

func TestFileRPCUploadPrepareHasNoOneGiBBusinessLimit(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.send")
	logicalSize := uint64(1<<30 + fileChunkBytes)
	prepared := mustFileRPC(t, dispatch, "file.upload.prepare", rpcInput{
		"transferId": "upload-logical-large",
		"path":       "logical-large.bin",
		"size":       float64(logicalSize),
		"sha256":     testDigest(nil),
	})
	if prepared["acceptedOffset"] != float64(0) || prepared["chunkSize"] != float64(fileChunkBytes) {
		t.Fatalf("large logical upload prepare = %#v", prepared)
	}
	manager := fileRPCManagerFor(dispatch.state)
	manager.mu.Lock()
	transfer := manager.uploads["upload-logical-large"]
	manager.mu.Unlock()
	if transfer == nil || transfer.Size != int64(logicalSize) {
		t.Fatalf("large logical upload state = %#v", transfer)
	}
}

func TestFileRPCBindsPathsCursorsAndTransfersToRegisteredProject(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.receive")
	projectARoot := t.TempDir()
	projectBRoot := t.TempDir()
	projectA, err := dispatch.state.business.addProject(context.Background(), projectARoot, "Project A", "", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := dispatch.state.business.addProject(context.Background(), projectBRoot, "Project B", "", projectPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(projectARoot, "only-a.txt"), []byte("A"))
	mustWriteFile(t, filepath.Join(projectARoot, "also-a.txt"), []byte("A2"))
	mustWriteFile(t, filepath.Join(projectBRoot, "only-b.txt"), []byte("B"))

	readA := dispatch
	readA.requestProjectID = projectA.ID.String()
	readB := dispatch
	readB.requestProjectID = projectB.ID.String()
	listedA := mustFileRPC(t, readA, "file.list", rpcInput{"path": "", "limit": float64(10)})
	itemsA := listedA["items"].([]any)
	if len(itemsA) != 2 {
		t.Fatalf("project A listing = %#v", listedA)
	}
	listedB := mustFileRPC(t, readB, "file.list", rpcInput{"path": "", "limit": float64(10)})
	itemsB := listedB["items"].([]any)
	if len(itemsB) != 1 || itemsB[0].(map[string]any)["name"] != "only-b.txt" {
		t.Fatalf("project B listing = %#v", listedB)
	}

	// A cursor contains the issuing project ID. Reusing it against another
	// project resets rather than exposing a same-relative-path listing.
	firstPage := mustFileRPC(t, readA, "file.list", rpcInput{"path": "", "limit": float64(1)})
	cursor, ok := firstPage["nextCursor"].(string)
	if ok && cursor != "" {
		crossProject := mustFileRPC(t, readB, "file.list", rpcInput{"path": "", "limit": float64(1), "cursor": cursor})
		if crossProject["resetRequired"] != true {
			t.Fatalf("cross-project cursor response = %#v", crossProject)
		}
	}

	prepared := mustFileRPC(t, readA, "file.download.prepare", rpcInput{
		"transferId": "cross-project-download", "path": "only-a.txt",
	})
	if prepared["transferId"] != "cross-project-download" {
		t.Fatalf("download preparation = %#v", prepared)
	}
	if _, _, err := readB.callFileRPC(context.Background(), "file.download.chunk", rpcInput{
		"transferId": "cross-project-download", "offset": float64(0), "maxBytes": float64(1),
	}); !errors.Is(err, errRPCProject) {
		t.Fatalf("cross-project download transfer error = %v", err)
	}

	writeA := readA
	writeA.scope = "remote.peer.file.send"
	writeB := readB
	writeB.scope = "remote.peer.file.send"
	payload := []byte("project scoped upload")
	digest := testDigest(payload)
	mustFileRPC(t, writeA, "file.upload.prepare", rpcInput{
		"transferId": "cross-project-upload", "path": "created.txt", "size": float64(len(payload)), "sha256": digest,
	})
	if _, _, err := writeB.callFileRPC(context.Background(), "file.upload.prepare", rpcInput{
		"transferId": "cross-project-upload", "path": "created.txt", "size": float64(len(payload)), "sha256": digest,
	}); !errors.Is(err, errRPCProject) {
		t.Fatalf("cross-project upload transfer error = %v", err)
	}
}

func TestFileRPCV2CreateMoveDetailsSearchAndTextEncodings(t *testing.T) {
	write := newFileTestDispatcher(t, "remote.peer.file.send")
	read := write
	read.scope = "remote.peer.file.receive"

	mustFileRPC(t, write, "file.mkdir", rpcInput{"parentPath": "", "name": "docs"})
	mustFileRPC(t, write, "file.mkdir", rpcInput{"parentPath": "", "name": "archive"})
	created := mustFileRPC(t, write, "file.create-text", rpcInput{
		"parentPath": "docs", "name": "notes.md", "content": "needle in project content",
	})
	createdEntry := created["entry"].(map[string]any)
	if createdEntry["category"] != "text" || createdEntry["extension"] != ".md" {
		t.Fatalf("created entry metadata = %#v", createdEntry)
	}
	if _, _, err := write.callFileRPC(t.Context(), "file.create-text", rpcInput{
		"parentPath": "docs", "name": "notes.md",
	}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("duplicate create error = %v", err)
	}

	utf8BOM := []byte{0xef, 0xbb, 0xbf, 'h', 'e', 'l', 'l', 'o'}
	utf16LE := []byte{0xff, 0xfe, 'h', 0, 'e', 0, 'l', 0, 'l', 0, 'o', 0}
	mustWriteFile(t, filepath.Join(write.state.Workspace, "utf8.txt"), utf8BOM)
	mustWriteFile(t, filepath.Join(write.state.Workspace, "utf16.txt"), utf16LE)
	mustWriteFile(t, filepath.Join(write.state.Workspace, "binary.bin"), []byte{0, 1, 2, 3})
	for path, encoding := range map[string]string{"utf8.txt": "utf-8-bom", "utf16.txt": "utf-16le"} {
		result := mustFileRPC(t, read, "file.read-text", rpcInput{"path": path, "maxBytes": float64(maximumTextReadBytes)})
		if result["content"] != "hello" || result["encoding"] != encoding || result["truncated"] != false {
			t.Errorf("read %s = %#v", path, result)
		}
	}
	if _, _, err := read.callFileRPC(t.Context(), "file.read-text", rpcInput{
		"path": "binary.bin", "maxBytes": float64(maximumTextReadBytes),
	}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("binary text read error = %v", err)
	}

	details := mustFileRPC(t, read, "file.details", rpcInput{"path": "utf16.txt"})
	textDetails := details["text"].(map[string]any)
	if details["category"] != "text" || details["extension"] != ".txt" ||
		textDetails["readable"] != true || textDetails["encoding"] != "utf-16le" || details["createdAt"] == nil {
		t.Fatalf("file details = %#v", details)
	}
	search := mustFileRPC(t, read, "file.search", rpcInput{"query": "needle", "limit": float64(20)})
	searchItems := search["items"].([]any)
	if len(searchItems) != 1 || searchItems[0].(map[string]any)["matchKind"] != "content" ||
		searchItems[0].(map[string]any)["parentPath"] != "docs" {
		t.Fatalf("content search = %#v", search)
	}
	pathSearch := mustFileRPC(t, read, "file.search", rpcInput{"query": "docs/notes", "limit": float64(20)})
	if len(pathSearch["items"].([]any)) != 1 || pathSearch["items"].([]any)[0].(map[string]any)["matchKind"] != "path" {
		t.Fatalf("path search = %#v", pathSearch)
	}

	revision := createdEntry["revision"].(float64)
	moved := mustFileRPC(t, write, "file.move", rpcInput{
		"path": "docs/notes.md", "targetDirectoryPath": "archive", "expectedRevision": revision,
	})
	if moved["entry"].(map[string]any)["relativePath"] != "archive/notes.md" {
		t.Fatalf("move response = %#v", moved)
	}
	if _, err := os.Stat(filepath.Join(write.state.Workspace, "docs", "notes.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("move left source: %v", err)
	}
	if got := string(mustReadFile(t, filepath.Join(write.state.Workspace, "archive", "notes.md"))); got != "needle in project content" {
		t.Fatalf("moved content = %q", got)
	}
	archiveStat := mustFileRPC(t, read, "file.stat", rpcInput{"path": "archive"})
	if _, _, err := write.callFileRPC(t.Context(), "file.move", rpcInput{
		"path": "archive", "targetDirectoryPath": "archive", "expectedRevision": archiveStat["entry"].(map[string]any)["revision"],
	}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("directory self-move error = %v", err)
	}
}

func TestFileRPCConfirmedRecursiveDeleteIsPolicyBoundOneUseAndSnapshotSafe(t *testing.T) {
	write := newFileTestDispatcher(t, "remote.peer.file.send")
	projectID := stableProjectID(write.state.DeviceID, "")
	project, err := write.state.business.projectByID(t.Context(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	policy := project.Policy
	policy.AllowRecursiveDelete = false
	project, err = write.state.business.updateProject(t.Context(), projectID, nil, nil, &policy, &project.Revision)
	if err != nil {
		t.Fatal(err)
	}
	mustMkdir(t, filepath.Join(write.state.Workspace, "policy-disabled"))
	mustWriteFile(t, filepath.Join(write.state.Workspace, "policy-disabled", "child.txt"), []byte("child"))
	read := write
	read.scope = "remote.peer.file.receive"
	disabledStat := mustFileRPC(t, read, "file.stat", rpcInput{"path": "policy-disabled"})
	if _, _, err := write.callFileRPC(t.Context(), "file.delete.prepare", rpcInput{
		"path": "policy-disabled", "expectedRevision": disabledStat["entry"].(map[string]any)["revision"],
	}); !errors.Is(err, errRPCCapability) {
		t.Fatalf("policy-disabled recursive delete error = %v", err)
	}
	policy = project.Policy
	policy.AllowRecursiveDelete = true
	project, err = write.state.business.updateProject(t.Context(), projectID, nil, nil, &policy, &project.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !agentFeatureFlags(write.state)["recursiveDelete.confirmed"] {
		t.Fatal("recursive-delete policy was not advertised")
	}

	mustMkdir(t, filepath.Join(write.state.Workspace, "tree"))
	mustMkdir(t, filepath.Join(write.state.Workspace, "tree", "nested"))
	mustWriteFile(t, filepath.Join(write.state.Workspace, "tree", "nested", "data.txt"), []byte("first"))
	stat := mustFileRPC(t, read, "file.stat", rpcInput{"path": "tree"})
	revision := stat["entry"].(map[string]any)["revision"]
	prepared := mustFileRPC(t, write, "file.delete.prepare", rpcInput{"path": "tree", "expectedRevision": revision})
	if prepared["requiresConfirmation"] != true || prepared["itemCount"].(float64) != 3 || prepared["confirmationToken"] == "" {
		t.Fatalf("delete preparation = %#v", prepared)
	}
	token := prepared["confirmationToken"].(string)
	mustWriteFile(t, filepath.Join(write.state.Workspace, "tree", "nested", "data.txt"), []byte("changed after confirmation"))
	if _, _, err := write.callFileRPC(t.Context(), "file.delete", rpcInput{
		"path": "tree", "expectedRevision": revision, "confirmationToken": token,
	}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("changed-tree delete error = %v", err)
	}
	if _, _, err := write.callFileRPC(t.Context(), "file.delete", rpcInput{
		"path": "tree", "expectedRevision": revision, "confirmationToken": token,
	}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("replayed confirmation error = %v", err)
	}
	prepared = mustFileRPC(t, write, "file.delete.prepare", rpcInput{"path": "tree", "expectedRevision": revision})
	deleted := mustFileRPC(t, write, "file.delete", rpcInput{
		"path": "tree", "expectedRevision": revision, "confirmationToken": prepared["confirmationToken"],
	})
	if deleted["recursive"] != true || deleted["itemCount"].(float64) != 3 {
		t.Fatalf("recursive delete = %#v", deleted)
	}
	if _, err := os.Stat(filepath.Join(write.state.Workspace, "tree")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recursive delete left tree: %v", err)
	}
	if _, _, err := write.callFileRPC(t.Context(), "file.delete.prepare", rpcInput{
		"path": "", "expectedRevision": float64(1),
	}); !errors.Is(err, errRPCInvalid) && !errors.Is(err, errRPCForbidden) {
		t.Fatalf("project-root delete preparation error = %v", err)
	}
}

func TestFileRPCTransferReplacementProjectRevisionLimitsAndCleanup(t *testing.T) {
	write := newFileTestDispatcher(t, "remote.peer.file.send")
	read := write
	read.scope = "remote.peer.file.receive"
	target := filepath.Join(write.state.Workspace, "large.txt")
	mustWriteFile(t, target, []byte("original"))
	stat := mustFileRPC(t, read, "file.stat", rpcInput{"path": "large.txt"})
	expected := stat["entry"].(map[string]any)["revision"]
	payload := bytes.Repeat([]byte("replacement payload "), 6000)
	digest := testDigest(payload)
	prepared := mustFileRPC(t, write, "file.upload.prepare", rpcInput{
		"transferId": "replace-upload-0001", "path": "large.txt", "size": float64(len(payload)),
		"sha256": digest, "expectedRevision": expected,
	})
	if prepared["replace"] != true || prepared["projectRevision"] == nil {
		t.Fatalf("replacement preparation = %#v", prepared)
	}
	for offset := 0; offset < len(payload); {
		end := min(offset+fileChunkBytes, len(payload))
		mustFileRPC(t, write, "file.upload.chunk", uploadChunkInput("replace-upload-0001", offset, payload[offset:end]))
		offset = end
	}
	mustFileRPC(t, write, "file.upload.complete", rpcInput{
		"transferId": "replace-upload-0001", "size": float64(len(payload)), "sha256": digest,
	})
	if !bytes.Equal(mustReadFile(t, target), payload) {
		t.Fatal("atomic replacement content differs")
	}
	if _, _, err := write.callFileRPC(t.Context(), "file.upload.prepare", rpcInput{
		"transferId": "replace-upload-stale", "path": "large.txt", "size": float64(len(payload)),
		"sha256": digest, "expectedRevision": expected,
	}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("stale replacement error = %v", err)
	}

	projectID := stableProjectID(write.state.DeviceID, "")
	download := mustFileRPC(t, read, "file.download.prepare", rpcInput{"transferId": "revision-download", "path": "large.txt"})
	project, err := write.state.business.projectByID(t.Context(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	name := "Changed while transfer is active"
	if _, err := write.state.business.updateProject(t.Context(), projectID, &name, nil, nil, &project.Revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := read.callFileRPC(t.Context(), "file.download.chunk", rpcInput{
		"transferId": download["transferId"], "offset": float64(0), "maxBytes": float64(1024),
	}); !errors.Is(err, errRPCRevision) {
		t.Fatalf("project revision transfer error = %v", err)
	}

	manager := fileRPCManagerFor(write.state)
	manager.mu.Lock()
	for id, transfer := range manager.downloads {
		transfer.ExpiresAt = time.Now().Add(-time.Minute)
		if id != "revision-download" {
			t.Fatalf("unexpected active download %q", id)
		}
	}
	manager.cleanup(time.Now())
	if len(manager.downloads) != 0 {
		t.Fatalf("expired downloads = %#v", manager.downloads)
	}
	manager.mu.Unlock()

	emptyDigest := testDigest(nil)
	for index := 0; index < maximumProjectFileTransfers; index++ {
		mustFileRPC(t, write, "file.upload.prepare", rpcInput{
			"transferId": fmt.Sprintf("capacity-upload-%02d", index),
			"path":       fmt.Sprintf("capacity-%02d.bin", index),
			"size":       float64(0),
			"sha256":     emptyDigest,
		})
	}
	if _, _, err := write.callFileRPC(t.Context(), "file.upload.prepare", rpcInput{
		"transferId": "capacity-upload-overflow", "path": "capacity-overflow.bin",
		"size": float64(0), "sha256": emptyDigest,
	}); !errors.Is(err, errRPCBusy) {
		t.Fatalf("per-project transfer capacity error = %v", err)
	}
	manager.mu.Lock()
	temporaryPaths := make([]string, 0, len(manager.uploads))
	for _, transfer := range manager.uploads {
		temporaryPaths = append(temporaryPaths, transfer.Temporary)
		transfer.ExpiresAt = time.Now().Add(-time.Minute)
	}
	manager.cleanup(time.Now())
	if len(manager.uploads) != 0 {
		t.Fatalf("expired uploads = %#v", manager.uploads)
	}
	manager.mu.Unlock()
	for _, path := range temporaryPaths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expired upload temporary file remains: %s (%v)", path, err)
		}
	}
}

func TestFileRPCLegacyWorkspaceCompatibilityIsReadOnly(t *testing.T) {
	dispatch := newFileTestDispatcher(t, "remote.peer.file.receive")
	dispatch.requestProjectID = ""
	mustWriteFile(t, filepath.Join(dispatch.state.Workspace, "legacy.txt"), []byte("legacy"))
	if listed := mustFileRPC(t, dispatch, "file.list", rpcInput{"path": ""}); len(listed["items"].([]any)) != 1 {
		t.Fatalf("legacy list = %#v", listed)
	}
	dispatch.scope = "remote.peer.file.send"
	if _, _, err := dispatch.callFileRPC(t.Context(), "file.create-text", rpcInput{
		"parentPath": "", "name": "blocked.txt",
	}); !errors.Is(err, errRPCProject) {
		t.Fatalf("legacy write error = %v", err)
	}
}

func TestFileRPCMethodScopesAreExact(t *testing.T) {
	cases := map[string]string{
		"file.list":              "remote.peer.file.receive",
		"file.stat":              "remote.peer.file.receive",
		"file.details":           "remote.peer.file.receive",
		"file.search":            "remote.peer.file.receive",
		"file.read-text":         "remote.peer.file.receive",
		"file.download.prepare":  "remote.peer.file.receive",
		"file.download.chunk":    "remote.peer.file.receive",
		"file.download.complete": "remote.peer.file.receive",
		"file.write-text":        "remote.peer.file.send",
		"file.create-text":       "remote.peer.file.send",
		"file.mkdir":             "remote.peer.file.send",
		"file.rename":            "remote.peer.file.send",
		"file.move":              "remote.peer.file.send",
		"file.delete.prepare":    "remote.peer.file.send",
		"file.delete":            "remote.peer.file.send",
		"file.upload.prepare":    "remote.peer.file.send",
		"file.upload.chunk":      "remote.peer.file.send",
		"file.upload.complete":   "remote.peer.file.send",
	}
	for method, scope := range cases {
		if got := methodScope(method); got != scope {
			t.Errorf("methodScope(%q) = %q, want %q", method, got, scope)
		}
	}
	dispatch := newFileTestDispatcher(t, "remote.peer.file.receive")
	response := dispatchEnvelope(t, dispatch, "file.mkdir", `{"parentPath":"","name":"forbidden"}`)
	if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN {
		t.Fatalf("cross-scope response = %+v", response)
	}
}

func newFileTestDispatcher(t *testing.T, scope string) dispatcher {
	t.Helper()
	root := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(root, "state.json"), filepath.Join(root, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fileRPCManagers.Delete(state) })
	now := time.Now().UTC()
	return dispatcher{
		state: state, now: func() time.Time { return now }, scope: scope,
		requestProjectID: stableProjectID(state.DeviceID, "").String(),
	}
}

func mustFileRPC(t *testing.T, dispatch dispatcher, method string, input rpcInput) map[string]any {
	t.Helper()
	output, _, err := dispatch.callFileRPC(context.Background(), method, input)
	if err != nil {
		t.Fatalf("%s error = %v", method, err)
	}
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func uploadChunkInput(transferID string, offset int, data []byte) rpcInput {
	return rpcInput{
		"transferId": transferID, "offset": float64(offset),
		"data": base64.RawURLEncoding.EncodeToString(data), "chunkSha256": testDigest(data),
	}
}

func testDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func assertJSONKeys(t *testing.T, value map[string]any, expected ...string) {
	t.Helper()
	actual := make([]string, 0, len(value))
	for key := range value {
		actual = append(actual, key)
	}
	slices.Sort(actual)
	slices.Sort(expected)
	if !slices.Equal(actual, expected) {
		t.Fatalf("JSON keys = %v, want %v (value=%#v)", actual, expected, value)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}
