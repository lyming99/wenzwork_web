package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	fileChunkBytes                = 32 << 10
	maximumTextReadBytes          = 512 << 10
	fileResponseBudget            = preferredRPCPagePayload
	fileTransferTTL               = 15 * time.Minute
	fileDeleteConfirmationTTL     = 2 * time.Minute
	maximumActiveFileTransfers    = uint64(16)
	maximumProjectFileTransfers   = 8
	maximumRecursiveDeleteEntries = uint64(10_000)
	maximumManagedFileBytes       = uint64(64 << 30)
	maxSafeJSONInteger            = uint64(1<<53 - 1)
)

var (
	fileTransferIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)
	fileRPCManagers       sync.Map // map[*agentState]*fileRPCManager
)

type remoteFileEntry struct {
	ID           string    `json:"id"`
	Revision     uint64    `json:"revision"`
	Name         string    `json:"name"`
	RelativePath string    `json:"relativePath"`
	Kind         string    `json:"kind"`
	Category     string    `json:"category"`
	Extension    string    `json:"extension"`
	Size         int64     `json:"size"`
	ModifiedAt   time.Time `json:"modifiedAt"`
	Readable     bool      `json:"readable"`
	Writable     bool      `json:"writable"`
}

type fileRPCManager struct {
	mu        sync.Mutex
	downloads map[string]*downloadTransfer
	uploads   map[string]*uploadTransfer
	deletions map[string]*deleteConfirmation
}

type downloadTransfer struct {
	ID              string
	SourceKind      string
	PeerSessionID   string
	ProjectID       uuid.UUID
	ProjectRevision uint64
	RelativePath    string
	Path            string
	Size            int64
	SHA256          string
	Revision        uint64
	ExpiresAt       time.Time
	TaskID          uuid.UUID
	RunID           uuid.UUID
	Generation      uint64
	FileName        string
	Sealed          bool
	releaseLease    func()
}

const (
	downloadSourceProjectFile = "projectFile"
	downloadSourceTaskLog     = "taskLog"
)

type uploadTransfer struct {
	ID               string
	ProjectID        uuid.UUID
	ProjectRevision  uint64
	RelativePath     string
	TargetPath       string
	Temporary        string
	Size             int64
	SHA256           string
	Offset           int64
	ExpectedRevision uint64
	Replace          bool
	ExpiresAt        time.Time
}

type fileListCursor struct {
	Version           int    `json:"v"`
	ProjectID         string `json:"i,omitempty"`
	RelativePath      string `json:"p"`
	Offset            int    `json:"o"`
	DirectoryRevision uint64 `json:"r"`
}

func fileRPCManagerFor(state *agentState) *fileRPCManager {
	if value, ok := fileRPCManagers.Load(state); ok {
		return value.(*fileRPCManager)
	}
	created := &fileRPCManager{
		downloads: make(map[string]*downloadTransfer),
		uploads:   make(map[string]*uploadTransfer),
		deletions: make(map[string]*deleteConfirmation),
	}
	actual, _ := fileRPCManagers.LoadOrStore(state, created)
	return actual.(*fileRPCManager)
}

func closeFileRPCManager(state *agentState) {
	if state == nil {
		return
	}
	value, ok := fileRPCManagers.LoadAndDelete(state)
	if !ok {
		return
	}
	manager := value.(*fileRPCManager)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for id, transfer := range manager.downloads {
		if transfer.releaseLease != nil {
			transfer.releaseLease()
			transfer.releaseLease = nil
		}
		delete(manager.downloads, id)
	}
	for id, transfer := range manager.uploads {
		_ = os.Remove(transfer.Temporary)
		delete(manager.uploads, id)
	}
	clear(manager.deletions)
}

func (d dispatcher) callFileRPC(ctx context.Context, method string, input rpcInput) (any, uint64, error) {
	manager := fileRPCManagerFor(d.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.cleanup(d.now().UTC())
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if strings.TrimSpace(d.requestProjectID) == "" && !legacyReadOnlyFileMethod(method) {
		return nil, 0, errRPCProject
	}
	project, err := d.fileProject()
	if err != nil {
		return nil, 0, err
	}

	switch method {
	case "file.list":
		return d.fileList(ctx, project, input)
	case "file.stat":
		return d.fileStat(project, input)
	case "file.details":
		return d.fileDetails(project, input)
	case "file.search":
		return d.fileSearch(ctx, project, input)
	case "file.read-text":
		return d.fileReadText(project, input)
	case "file.write-text":
		return d.fileWriteText(ctx, project, input)
	case "file.create-text":
		return d.fileCreateText(ctx, project, input)
	case "file.mkdir":
		return d.fileMkdir(ctx, project, input)
	case "file.rename":
		return d.fileRename(ctx, project, input)
	case "file.move":
		return d.fileMove(ctx, project, input)
	case "file.delete.prepare":
		return d.fileDeletePrepare(ctx, manager, project, input)
	case "file.delete":
		return d.fileDelete(ctx, manager, project, input)
	case "file.download.prepare":
		return d.fileDownloadPrepare(ctx, manager, project, input)
	case "file.download.chunk":
		return d.fileDownloadChunk(ctx, manager, project, input)
	case "file.download.complete":
		return d.fileDownloadComplete(ctx, manager, project, input)
	case "file.upload.prepare":
		return d.fileUploadPrepare(manager, project, input)
	case "file.upload.chunk":
		return d.fileUploadChunk(ctx, manager, project, input)
	case "file.upload.complete":
		return d.fileUploadComplete(ctx, manager, project, input)
	default:
		return nil, 0, errRPCNotFound
	}
}

func (d dispatcher) fileProject() (registeredProject, error) {
	if d.state == nil {
		return registeredProject{}, errRPCNotFound
	}
	projectID := strings.TrimSpace(d.requestProjectID)
	if projectID == "" {
		// Legacy v1 methods retain the old --workspace root. New Peer tickets
		// carry a project ID in the protobuf header and never enter this path.
		return registeredProject{ID: stableProjectID(d.state.DeviceID, ""), LocalPath: d.state.Workspace, State: "available"}, nil
	}
	id, err := uuid.Parse(projectID)
	if err != nil || id == uuid.Nil || d.state.business == nil {
		return registeredProject{}, errRPCProject
	}
	project, err := d.state.business.projectByID(context.Background(), id)
	if err != nil || project.State != "available" {
		return registeredProject{}, errRPCProject
	}
	return project, nil
}

func (manager *fileRPCManager) cleanup(now time.Time) {
	for id, transfer := range manager.downloads {
		if !transfer.ExpiresAt.After(now) {
			if transfer.releaseLease != nil {
				transfer.releaseLease()
				transfer.releaseLease = nil
			}
			delete(manager.downloads, id)
		}
	}
	for id, transfer := range manager.uploads {
		if !transfer.ExpiresAt.After(now) {
			_ = os.Remove(transfer.Temporary)
			delete(manager.uploads, id)
		}
	}
	for id, confirmation := range manager.deletions {
		if !confirmation.ExpiresAt.After(now) {
			delete(manager.deletions, id)
		}
	}
}

func (d dispatcher) fileList(ctx context.Context, project registeredProject, input rpcInput) (any, uint64, error) {
	relative, ok := optionalFilePathInput(input, "path")
	if !ok {
		return nil, 0, errRPCInvalid
	}
	limit, err := filePageLimit(input)
	if err != nil {
		return nil, 0, err
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	directoryInfo, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil || !directoryInfo.IsDir() {
		return nil, 0, errRPCInvalid
	}
	directoryRevision := workspaceFileRevision(normalized, directoryInfo)
	start, resetRequired, err := decodeFileListCursor(input, project.ID, normalized, directoryRevision)
	if err != nil {
		return nil, 0, err
	}
	if resetRequired {
		revision := d.state.revisionValue()
		return map[string]any{
			"items": []remoteFileEntry{}, "nextCursor": (*string)(nil),
			"highWatermark": revision, "resetRequired": true,
		}, revision, nil
	}
	directoryEntries, err := os.ReadDir(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	if len(directoryEntries) > 10000 {
		return nil, 0, errRPCInvalid
	}
	entries := make([]remoteFileEntry, 0, len(directoryEntries))
	for _, directoryEntry := range directoryEntries {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		info, err := directoryEntry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		childRelative := filepath.ToSlash(filepath.Join(normalized, directoryEntry.Name()))
		entry := remoteFileEntryFromInfo(childRelative, info)
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(left, right remoteFileEntry) int {
		if left.Kind != right.Kind {
			if left.Kind == "directory" {
				return -1
			}
			return 1
		}
		if result := strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name)); result != 0 {
			return result
		}
		return strings.Compare(left.Name, right.Name)
	})
	if start < 0 || start > len(entries) {
		return nil, 0, errRPCInvalid
	}
	revision := d.state.revisionValue()
	requestedEnd := min(len(entries), start+limit)
	build := func(count int) (any, error) {
		end := start + count
		var next *string
		if end < len(entries) {
			encoded, encodeErr := encodeFileListCursor(fileListCursor{
				Version: 1, ProjectID: project.ID.String(), RelativePath: normalized,
				Offset: end, DirectoryRevision: directoryRevision,
			})
			if encodeErr != nil {
				return nil, encodeErr
			}
			next = &encoded
		}
		return map[string]any{
			"items": entries[start:end], "nextCursor": next,
			"highWatermark": revision, "resetRequired": false,
		}, nil
	}
	count, err := rpcPagePrefixLengthE(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	result, err := build(count)
	return result, revision, err
}

func (d dispatcher) fileStat(project registeredProject, input rpcInput) (any, uint64, error) {
	relative, ok := filePathInput(input, "path")
	if !ok {
		return nil, 0, errRPCInvalid
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return nil, 0, errRPCInvalid
	}
	entry := remoteFileEntryFromInfo(normalized, info)
	return map[string]any{"entry": entry}, entry.Revision, nil
}

func (d dispatcher) fileReadText(project registeredProject, input rpcInput) (any, uint64, error) {
	relative, ok := filePathInput(input, "path")
	if !ok {
		return nil, 0, errRPCInvalid
	}
	maximum := uint64(maximumTextReadBytes)
	if value, present, valid := optionalUint64(input, "maxBytes"); !valid || (present && (value == 0 || value > maximumTextReadBytes)) {
		return nil, 0, errRPCInvalid
	} else if present {
		maximum = value
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	before, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil || !before.Mode().IsRegular() {
		return nil, 0, errRPCInvalid
	}
	if before.Size() < 0 || before.Size() > maximumTextReadBytes {
		return nil, 0, errRPCInvalid
	}
	data, err := readBoundedFile(resolved, maximumTextReadBytes)
	if err != nil {
		return nil, 0, err
	}
	after, err := os.Stat(resolved)
	if err != nil || workspaceFileRevision(normalized, before) != workspaceFileRevision(normalized, after) {
		return nil, 0, errRPCRevision
	}
	truncated := len(data) > int(maximum)
	if len(data) > int(maximum) {
		data = truncateEncodedTextBytes(data, int(maximum))
	}
	content, encoding, err := decodeWorkspaceText(before.Name(), data)
	if err != nil {
		return nil, 0, errRPCInvalid
	}
	revision := workspaceFileRevision(normalized, before)
	for {
		output := map[string]any{
			"content": content, "revision": revision, "truncated": truncated,
			"encoding": encoding, "category": fileCategory(before.Name()),
		}
		encoded, _ := json.Marshal(output)
		if len(encoded) <= fileResponseBudget {
			return output, revision, nil
		}
		truncated = true
		if len(content) < 2 {
			return nil, 0, errRPCInvalid
		}
		content = truncateUTF8String(content, len([]byte(content))*3/4)
	}
}

func (d dispatcher) fileWriteText(ctx context.Context, project registeredProject, input rpcInput) (any, uint64, error) {
	relative, okPath := filePathInput(input, "path")
	content, okContent := input["content"].(string)
	expected, present, okRevision := optionalUint64(input, "expectedRevision")
	if !okPath || !okContent || !utf8.ValidString(content) || len([]byte(content)) > maximumTextReadBytes || !present || !okRevision {
		return nil, 0, errRPCInvalid
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, errRPCInvalid
	}
	if workspaceFileRevision(normalized, info) != expected {
		return nil, 0, errRPCRevision
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := atomicWriteWorkspaceFile(resolved, normalized, expected, []byte(content), mode); err != nil {
		return nil, 0, err
	}
	updated, err := os.Stat(resolved)
	if err != nil {
		return nil, 0, err
	}
	entry := remoteFileEntryFromInfo(normalized, updated)
	if err := d.state.persistMutation(); err != nil {
		return nil, 0, err
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{"revision": entry.Revision, "entry": entry, "encoding": "utf-8"}, entry.Revision, nil
}

func (d dispatcher) fileMkdir(ctx context.Context, project registeredProject, input rpcInput) (any, uint64, error) {
	parentRelative, okParent := optionalFilePathInput(input, "parentPath")
	name, okName := fileNameInput(input, "name")
	if !okParent || !okName {
		return nil, 0, errRPCInvalid
	}
	parentPath, normalizedParent, err := secureExistingProjectPath(project, parentRelative)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(parentPath)
	if err != nil || !info.IsDir() {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, errRPCNotFound
		}
		return nil, 0, errRPCInvalid
	}
	target := filepath.Join(parentPath, name)
	if _, err := os.Lstat(target); err == nil {
		return nil, 0, errRPCRevision
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return nil, 0, err
	}
	if err := d.state.persistMutation(); err != nil {
		if rollbackErr := os.Remove(target); rollbackErr == nil {
			if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
				return nil, 0, errors.Join(err, sideEffectErr)
			}
		}
		return nil, 0, err
	}
	created, err := os.Stat(target)
	if err != nil {
		return nil, 0, err
	}
	relative := filepath.ToSlash(filepath.Join(normalizedParent, name))
	entry := remoteFileEntryFromInfo(relative, created)
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{"entry": entry, "revision": entry.Revision}, entry.Revision, nil
}

func (d dispatcher) fileRename(ctx context.Context, project registeredProject, input rpcInput) (any, uint64, error) {
	relative, okPath := filePathInput(input, "path")
	name, okName := fileNameInput(input, "name")
	expected, present, okRevision := optionalUint64(input, "expectedRevision")
	if !okPath || !okName || !present || !okRevision {
		return nil, 0, errRPCInvalid
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	if normalized == "" {
		return nil, 0, errRPCForbidden
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil || workspaceFileRevision(normalized, info) != expected {
		return nil, 0, errRPCRevision
	}
	target := filepath.Join(filepath.Dir(resolved), name)
	if _, err := os.Lstat(target); err == nil {
		return nil, 0, errRPCRevision
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := os.Rename(resolved, target); err != nil {
		return nil, 0, err
	}
	if err := d.state.persistMutation(); err != nil {
		if rollbackErr := os.Rename(target, resolved); rollbackErr == nil {
			if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
				return nil, 0, errors.Join(err, sideEffectErr)
			}
		}
		return nil, 0, err
	}
	updated, err := os.Stat(target)
	if err != nil {
		return nil, 0, err
	}
	targetRelative := filepath.ToSlash(filepath.Join(filepath.Dir(normalized), name))
	if targetRelative == "." {
		targetRelative = name
	}
	entry := remoteFileEntryFromInfo(targetRelative, updated)
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{"entry": entry, "revision": entry.Revision}, entry.Revision, nil
}

func (d dispatcher) fileDelete(ctx context.Context, manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "path", "expectedRevision", "confirmationToken") {
		return nil, 0, errRPCInvalid
	}
	relative, okPath := filePathInput(input, "path")
	expected, present, okRevision := optionalUint64(input, "expectedRevision")
	confirmationToken, okToken := optionalInputString(input, "confirmationToken", 128)
	if !okPath || !present || !okRevision || !okToken {
		return nil, 0, errRPCInvalid
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	if normalized == "" {
		return nil, 0, errRPCForbidden
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil || workspaceFileRevision(normalized, info) != expected {
		return nil, 0, errRPCRevision
	}
	if info.IsDir() {
		entries, err := os.ReadDir(resolved)
		if err != nil {
			return nil, 0, err
		}
		if len(entries) != 0 {
			if confirmationToken == "" {
				return nil, 0, errRPCInvalid
			}
			return d.fileDeleteConfirmed(ctx, manager, project, resolved, normalized, expected, confirmationToken)
		}
	}
	if confirmationToken != "" {
		return nil, 0, errRPCInvalid
	}
	// os.Remove intentionally refuses non-empty directories. Recursive deletion
	// is never exposed by the remote RPC surface.
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := os.Remove(resolved); err != nil {
		return nil, 0, err
	}
	if err := d.state.persistMutation(); err != nil {
		return nil, 0, err
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{"deleted": true, "relativePath": normalized}, d.state.revisionValue(), nil
}

func (d dispatcher) fileDownloadPrepare(ctx context.Context, manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	transferID, okID := transferIDInput(input)
	relative, okPath := filePathInput(input, "path")
	expected, expectedPresent, okExpected := optionalUint64(input, "expectedRevision")
	if !okID || !okPath || !okExpected {
		return nil, 0, errRPCInvalid
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, errRPCNotFound
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) > maximumManagedFileBytes {
		return nil, 0, errRPCInvalid
	}
	revision := workspaceFileRevision(normalized, info)
	if expectedPresent && revision != expected {
		return nil, 0, errRPCRevision
	}
	if existing := manager.downloads[transferID]; existing != nil {
		if existing.ProjectID != project.ID {
			return nil, 0, errRPCProject
		}
		if existing.ProjectRevision != project.Revision || existing.RelativePath != normalized || existing.Revision != revision || existing.Size != info.Size() {
			return nil, 0, errRPCRevision
		}
		existing.ExpiresAt = d.now().UTC().Add(fileTransferTTL)
		return downloadPrepareResponse(existing), revision, nil
	}
	if collision := manager.uploads[transferID]; collision != nil {
		if collision.ProjectID != project.ID {
			return nil, 0, errRPCProject
		}
		return nil, 0, errRPCRevision
	}
	if !manager.canStartTransfer(project.ID, 0) {
		return nil, 0, errRPCBusy
	}
	digest, size, err := hashWorkspaceFile(ctx, resolved)
	if err != nil || size != info.Size() {
		return nil, 0, firstError(err, errRPCRevision)
	}
	after, err := os.Stat(resolved)
	if err != nil || workspaceFileRevision(normalized, after) != revision {
		return nil, 0, errRPCRevision
	}
	transfer := &downloadTransfer{
		ID: transferID, SourceKind: downloadSourceProjectFile, ProjectID: project.ID, ProjectRevision: project.Revision,
		RelativePath: normalized, Path: resolved, Size: size, SHA256: digest,
		Revision: revision, ExpiresAt: d.now().UTC().Add(fileTransferTTL),
	}
	manager.downloads[transferID] = transfer
	return downloadPrepareResponse(transfer), revision, nil
}

func downloadPrepareResponse(transfer *downloadTransfer) map[string]any {
	return map[string]any{
		"transferId": transfer.ID, "size": transfer.Size, "sha256": transfer.SHA256,
		"chunkSize": fileChunkBytes, "acceptedOffset": 0, "projectRevision": transfer.ProjectRevision,
	}
}

func (d dispatcher) fileDownloadChunk(ctx context.Context, manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	transferID, okID := transferIDInput(input)
	offset, offsetPresent, okOffset := optionalUint64(input, "offset")
	maximum, maximumPresent, okMaximum := optionalUint64(input, "maxBytes")
	if !okID || !offsetPresent || !okOffset || (!okMaximum) {
		return nil, 0, errRPCInvalid
	}
	if !maximumPresent {
		maximum = fileChunkBytes
	}
	if maximum == 0 || maximum > fileChunkBytes {
		return nil, 0, errRPCInvalid
	}
	transfer := manager.downloads[transferID]
	if transfer == nil || transfer.SourceKind != downloadSourceProjectFile {
		return nil, 0, errRPCNotFound
	}
	if transfer.ProjectID != project.ID {
		return nil, 0, errRPCProject
	}
	if transfer.ProjectRevision != project.Revision {
		return nil, 0, errRPCRevision
	}
	if offset >= uint64(transfer.Size) || offset > uint64(maxSafeJSONInteger) {
		return nil, 0, errRPCInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	resolved, normalized, err := secureExistingProjectPath(project, transfer.RelativePath)
	if err != nil || normalized != transfer.RelativePath || !sameFilesystemPath(resolved, transfer.Path) {
		return nil, 0, firstError(err, errRPCRevision)
	}
	info, err := os.Stat(resolved)
	if err != nil || workspaceFileRevision(transfer.RelativePath, info) != transfer.Revision {
		return nil, 0, errRPCRevision
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	length := min(int64(maximum), transfer.Size-int64(offset))
	data := make([]byte, length)
	if _, err := file.ReadAt(data, int64(offset)); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	if len(data) == 0 {
		return nil, 0, errRPCInvalid
	}
	transfer.ExpiresAt = d.now().UTC().Add(fileTransferTTL)
	return map[string]any{
		"transferId": transferID, "offset": offset, "data": base64.RawURLEncoding.EncodeToString(data), "total": transfer.Size,
	}, transfer.Revision, nil
}

func (d dispatcher) fileDownloadComplete(ctx context.Context, manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	transferID, okID := transferIDInput(input)
	digest, okDigest := digestInput(input, "sha256")
	if !okID || !okDigest {
		return nil, 0, errRPCInvalid
	}
	transfer := manager.downloads[transferID]
	if transfer == nil || transfer.SourceKind != downloadSourceProjectFile {
		return nil, 0, errRPCNotFound
	}
	if transfer.ProjectID != project.ID {
		return nil, 0, errRPCProject
	}
	if transfer.ProjectRevision != project.Revision {
		return nil, 0, errRPCRevision
	}
	if digest != transfer.SHA256 {
		return nil, 0, errRPCRevision
	}
	resolved, normalized, err := secureExistingProjectPath(project, transfer.RelativePath)
	if err != nil || normalized != transfer.RelativePath || !sameFilesystemPath(resolved, transfer.Path) {
		return nil, 0, firstError(err, errRPCRevision)
	}
	currentDigest, size, err := hashWorkspaceFile(ctx, resolved)
	if err != nil || size != transfer.Size || currentDigest != transfer.SHA256 {
		return nil, 0, firstError(err, errRPCRevision)
	}
	info, err := os.Stat(resolved)
	if err != nil || workspaceFileRevision(transfer.RelativePath, info) != transfer.Revision {
		return nil, 0, errRPCRevision
	}
	if transfer.releaseLease != nil {
		transfer.releaseLease()
		transfer.releaseLease = nil
	}
	delete(manager.downloads, transferID)
	return map[string]any{"completed": true, "transferId": transferID, "sha256": digest}, transfer.Revision, nil
}

func (d dispatcher) fileUploadPrepare(manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	transferID, okID := transferIDInput(input)
	relative, okPath := filePathInput(input, "path")
	size, sizePresent, okSize := optionalUint64(input, "size")
	digest, okDigest := digestInput(input, "sha256")
	expected, expectedPresent, okExpected := optionalUint64(input, "expectedRevision")
	if !okID || !okPath || !sizePresent || !okSize || size > maxSafeJSONInteger || size > maximumManagedFileBytes || !okDigest || !okExpected {
		return nil, 0, errRPCInvalid
	}
	normalized, err := normalizeWorkspaceRelativePath(relative)
	if err != nil || normalized == "" {
		return nil, 0, errRPCInvalid
	}
	parentRelative := filepath.ToSlash(filepath.Dir(normalized))
	if parentRelative == "." {
		parentRelative = ""
	}
	parent, normalizedParent, err := secureExistingProjectPath(project, parentRelative)
	if err != nil {
		return nil, 0, err
	}
	name := filepath.Base(filepath.FromSlash(normalized))
	if !validFileName(name) || filepath.ToSlash(filepath.Join(normalizedParent, name)) != normalized {
		return nil, 0, errRPCInvalid
	}
	target := filepath.Join(parent, name)
	info, statErr := os.Lstat(target)
	replace := statErr == nil
	if replace {
		if !expectedPresent || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || workspaceFileRevision(normalized, info) != expected {
			return nil, 0, errRPCRevision
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, 0, statErr
	} else if expectedPresent {
		return nil, 0, errRPCRevision
	}
	if existing := manager.uploads[transferID]; existing != nil {
		if existing.ProjectID != project.ID {
			return nil, 0, errRPCProject
		}
		if existing.ProjectRevision != project.Revision || existing.RelativePath != normalized || existing.Size != int64(size) ||
			existing.SHA256 != digest || existing.TargetPath != target || existing.Replace != replace ||
			existing.ExpectedRevision != expected {
			return nil, 0, errRPCRevision
		}
		info, err := os.Stat(existing.Temporary)
		// The encrypted bulk Stream may have durably written a few chunks ahead
		// of the contiguous accepted offset. Those bounded sparse chunks are safe
		// to overwrite after resume; only the contiguous offset is authoritative.
		if err != nil || info.Size() < existing.Offset || info.Size() > existing.Size {
			return nil, 0, errRPCRevision
		}
		existing.ExpiresAt = d.now().UTC().Add(fileTransferTTL)
		return uploadPrepareResponse(existing), d.state.revisionValue(), nil
	}
	if collision := manager.downloads[transferID]; collision != nil {
		if collision.ProjectID != project.ID {
			return nil, 0, errRPCProject
		}
		return nil, 0, errRPCRevision
	}
	if !manager.canStartTransfer(project.ID, size) {
		return nil, 0, errRPCBusy
	}
	temporary, err := os.CreateTemp(parent, ".wenzwork-upload-*")
	if err != nil {
		return nil, 0, err
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return nil, 0, err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, 0, err
	}
	transfer := &uploadTransfer{
		ID: transferID, ProjectID: project.ID, ProjectRevision: project.Revision,
		RelativePath: normalized, TargetPath: target, Temporary: temporaryPath,
		Size: int64(size), SHA256: digest, Offset: 0, ExpectedRevision: expected, Replace: replace,
		ExpiresAt: d.now().UTC().Add(fileTransferTTL),
	}
	manager.uploads[transferID] = transfer
	return uploadPrepareResponse(transfer), d.state.revisionValue(), nil
}

func uploadPrepareResponse(transfer *uploadTransfer) map[string]any {
	return map[string]any{
		"transferId": transfer.ID, "chunkSize": fileChunkBytes, "acceptedOffset": transfer.Offset,
		"projectRevision": transfer.ProjectRevision, "replace": transfer.Replace,
	}
}

func (d dispatcher) fileUploadChunk(ctx context.Context, manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	transferID, okID := transferIDInput(input)
	offset, offsetPresent, okOffset := optionalUint64(input, "offset")
	encodedData, okData := inputString(input, "data", 48<<10)
	chunkDigest, okDigest := digestInput(input, "chunkSha256")
	if !okID || !offsetPresent || !okOffset || !okData || !okDigest || offset > uint64(maxSafeJSONInteger) {
		return nil, 0, errRPCInvalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(encodedData)
	if err != nil || len(data) == 0 || len(data) > fileChunkBytes || base64.RawURLEncoding.EncodeToString(data) != encodedData ||
		base64.RawURLEncoding.EncodeToString(sha256Bytes(data)) != chunkDigest {
		return nil, 0, errRPCInvalid
	}
	transfer := manager.uploads[transferID]
	if transfer == nil {
		return nil, 0, errRPCNotFound
	}
	if transfer.ProjectID != project.ID {
		return nil, 0, errRPCProject
	}
	if transfer.ProjectRevision != project.Revision {
		return nil, 0, errRPCRevision
	}
	if int64(offset)+int64(len(data)) > transfer.Size {
		return nil, 0, errRPCInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	file, err := os.OpenFile(transfer.Temporary, os.O_RDWR, 0)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	if int64(offset) < transfer.Offset {
		if int64(offset)+int64(len(data)) > transfer.Offset {
			return nil, 0, errRPCRevision
		}
		existing := make([]byte, len(data))
		if _, err := file.ReadAt(existing, int64(offset)); err != nil || !bytes.Equal(existing, data) {
			return nil, 0, errRPCRevision
		}
		return map[string]any{"transferId": transferID, "nextOffset": transfer.Offset, "replayed": true}, d.state.revisionValue(), nil
	}
	if int64(offset) != transfer.Offset {
		return nil, 0, errRPCRevision
	}
	if _, err := file.WriteAt(data, int64(offset)); err != nil {
		return nil, 0, err
	}
	if err := file.Sync(); err != nil {
		return nil, 0, err
	}
	transfer.Offset += int64(len(data))
	transfer.ExpiresAt = d.now().UTC().Add(fileTransferTTL)
	return map[string]any{"transferId": transferID, "nextOffset": transfer.Offset, "replayed": false}, d.state.revisionValue(), nil
}

func (d dispatcher) fileUploadComplete(ctx context.Context, manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	transferID, okID := transferIDInput(input)
	size, sizePresent, okSize := optionalUint64(input, "size")
	digest, okDigest := digestInput(input, "sha256")
	if !okID || !sizePresent || !okSize || !okDigest {
		return nil, 0, errRPCInvalid
	}
	transfer := manager.uploads[transferID]
	if transfer == nil {
		return nil, 0, errRPCNotFound
	}
	if transfer.ProjectID != project.ID {
		return nil, 0, errRPCProject
	}
	if transfer.ProjectRevision != project.Revision {
		return nil, 0, errRPCRevision
	}
	if int64(size) != transfer.Size || digest != transfer.SHA256 || transfer.Offset != transfer.Size {
		return nil, 0, errRPCRevision
	}
	computed, actualSize, err := hashWorkspaceFile(ctx, transfer.Temporary)
	if err != nil || actualSize != transfer.Size || computed != transfer.SHA256 {
		_ = os.Remove(transfer.Temporary)
		delete(manager.uploads, transferID)
		return nil, 0, firstError(err, errRPCRevision)
	}
	current, targetErr := os.Lstat(transfer.TargetPath)
	if transfer.Replace {
		if targetErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
			workspaceFileRevision(transfer.RelativePath, current) != transfer.ExpectedRevision {
			return nil, 0, errRPCRevision
		}
	} else if targetErr == nil {
		return nil, 0, errRPCRevision
	} else if !errors.Is(targetErr, os.ErrNotExist) {
		return nil, 0, targetErr
	}
	parentRelative := filepath.ToSlash(filepath.Dir(transfer.RelativePath))
	if parentRelative == "." {
		parentRelative = ""
	}
	parent, _, err := secureExistingProjectPath(project, parentRelative)
	if err != nil || !sameFilesystemPath(parent, filepath.Dir(transfer.TargetPath)) {
		return nil, 0, firstError(err, errRPCRevision)
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := d.state.persistMutation(); err != nil {
		return nil, 0, err
	}
	if transfer.Replace {
		// The temporary file is in the same directory and has been fsynced by
		// every acknowledged chunk. Rename publishes the complete replacement
		// atomically after the second revision check above.
		if err := os.Rename(transfer.Temporary, transfer.TargetPath); err != nil {
			return nil, 0, err
		}
	} else {
		// A same-directory hard link is an atomic, no-overwrite publication. A
		// racing creator wins rather than being overwritten.
		if err := os.Link(transfer.Temporary, transfer.TargetPath); err != nil {
			return nil, 0, err
		}
		if err := os.Remove(transfer.Temporary); err != nil {
			_ = os.Remove(transfer.TargetPath)
			return nil, 0, err
		}
	}
	delete(manager.uploads, transferID)
	info, err := os.Stat(transfer.TargetPath)
	if err != nil {
		return nil, 0, err
	}
	entry := remoteFileEntryFromInfo(transfer.RelativePath, info)
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"completed": true, "transferId": transferID, "size": transfer.Size,
		"sha256": transfer.SHA256, "revision": entry.Revision, "entry": entry,
	}, entry.Revision, nil
}

func filePageLimit(input rpcInput) (int, error) {
	value, present, ok := optionalUint64(input, "limit")
	if !ok {
		return 0, errRPCInvalid
	}
	if !present {
		return 50, nil
	}
	if value < 1 || value > 200 {
		return 0, errRPCInvalid
	}
	return int(value), nil
}

func decodeFileListCursor(input rpcInput, projectID uuid.UUID, relative string, revision uint64) (int, bool, error) {
	encoded, ok := optionalInputString(input, "cursor", 512)
	if !ok {
		return 0, false, errRPCInvalid
	}
	if encoded == "" {
		return 0, false, nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > 384 || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return 0, false, errRPCInvalid
	}
	var cursor fileListCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil || decoder.Decode(new(any)) != io.EOF || cursor.Version != 1 || cursor.RelativePath != relative || cursor.Offset < 0 || cursor.Offset > 10000 {
		return 0, false, errRPCInvalid
	}
	// Cursors are opaque device state. A cursor issued for one project must
	// never page another project that happens to have the same relative path.
	// Older v1 cursors lacked this field, so safely ask the client to refresh
	// instead of attempting to preserve an ambiguous position.
	if cursor.ProjectID != projectID.String() {
		return 0, true, nil
	}
	if cursor.DirectoryRevision != revision {
		return 0, true, nil
	}
	return cursor.Offset, false, nil
}

func encodeFileListCursor(cursor fileListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func normalizeWorkspaceRelativePath(value string) (string, error) {
	if value == "" || value == "." {
		return "", nil
	}
	if len(value) > 4096 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", errRPCForbidden
	}
	parts := strings.Split(value, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if !validFileName(part) {
			return "", errRPCForbidden
		}
		clean = append(clean, part)
	}
	normalized := strings.Join(clean, "/")
	if filepath.IsAbs(filepath.FromSlash(normalized)) || filepath.VolumeName(filepath.FromSlash(normalized)) != "" {
		return "", errRPCForbidden
	}
	return normalized, nil
}

func secureExistingWorkspacePath(state *agentState, relative string) (string, string, error) {
	if state == nil {
		return "", "", errRPCNotFound
	}
	return secureExistingProjectPath(registeredProject{
		ID: stableProjectID(state.DeviceID, ""), LocalPath: state.Workspace, State: "available",
	}, relative)
}

// secureExistingProjectPath resolves an untrusted project-relative path while
// rejecting symlinks, junctions and traversal on every component. Project
// records are device-local; callers only ever receive their stable UUID.
func secureExistingProjectPath(project registeredProject, relative string) (string, string, error) {
	if project.ID == uuid.Nil || strings.TrimSpace(project.LocalPath) == "" || project.State != "available" {
		return "", "", errRPCProject
	}
	normalized, err := normalizeWorkspaceRelativePath(relative)
	if err != nil {
		return "", "", err
	}
	root, err := filepath.Abs(project.LocalPath)
	if err != nil {
		return "", "", err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", errRPCNotFound
	}
	if err != nil || !sameFilesystemPath(resolvedRoot, root) {
		return "", "", errRPCForbidden
	}
	rootInfo, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", errRPCNotFound
	}
	if err != nil || !rootInfo.IsDir() {
		return "", "", errRPCProject
	}
	current := root
	if normalized == "" {
		return current, normalized, nil
	}
	parts := strings.Split(normalized, "/")
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return "", "", errRPCNotFound
		}
		if statErr != nil {
			return "", "", statErr
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return "", "", errRPCForbidden
		}
		// On Windows, directory junctions are reparse points and are not
		// consistently reported as ModeSymlink by every supported Go/volume
		// combination. Resolving each component also rejects those aliases.
		evaluated, evalErr := filepath.EvalSymlinks(current)
		if evalErr != nil || !sameFilesystemPath(evaluated, current) {
			return "", "", errRPCForbidden
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", "", errRPCNotFound
		}
	}
	absolute, err := filepath.Abs(current)
	if err != nil || (absolute != root && !strings.HasPrefix(absolute, root+string(filepath.Separator))) {
		return "", "", errRPCForbidden
	}
	return absolute, normalized, nil
}

func fileNameInput(input rpcInput, key string) (string, bool) {
	value, ok := input[key].(string)
	ok = ok && utf8.ValidString(value) && len(value) <= 255
	return value, ok && validFileName(value)
}

func validFileName(value string) bool {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value ||
		strings.ContainsAny(value, `/\\<>:"|?*`) || strings.HasSuffix(value, ".") || strings.HasSuffix(value, " ") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CLOCK$" ||
		(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		return false
	}
	return true
}

func filePathInput(input rpcInput, key string) (string, bool) {
	value, ok := input[key].(string)
	return value, ok && value != "" && utf8.ValidString(value) && len(value) <= 4096
}

func optionalFilePathInput(input rpcInput, key string) (string, bool) {
	value, exists := input[key]
	if !exists || value == nil {
		return "", true
	}
	text, ok := value.(string)
	return text, ok && utf8.ValidString(text) && len(text) <= 4096
}

func sameFilesystemPath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func transferIDInput(input rpcInput) (string, bool) {
	value, ok := inputString(input, "transferId", 128)
	return value, ok && fileTransferIDPattern.MatchString(value)
}

func digestInput(input rpcInput, key string) (string, bool) {
	value, ok := inputString(input, key, 64)
	if !ok {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return value, err == nil && len(decoded) == sha256.Size && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func remoteFileEntryFromInfo(relative string, info os.FileInfo) remoteFileEntry {
	relative = filepath.ToSlash(relative)
	kind, category, extension, size := "file", fileCategory(info.Name()), fileExtension(info.Name()), info.Size()
	if info.IsDir() {
		kind, category, extension, size = "directory", "directory", "", 0
	}
	writable := info.Mode().Perm()&0o222 != 0
	return remoteFileEntry{
		ID: workspaceFileID(relative), Revision: workspaceFileRevision(relative, info), Name: info.Name(),
		RelativePath: relative, Kind: kind, Category: category, Extension: extension,
		Size: size, ModifiedAt: info.ModTime().UTC(), Readable: true, Writable: writable,
	}
}

func workspaceFileID(relative string) string {
	digest := sha256.Sum256([]byte("wenzwork-file-id:v1\x00" + filepath.ToSlash(relative)))
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func workspaceFileRevision(relative string, info os.FileInfo) uint64 {
	payload := fmt.Sprintf("wenzwork-file-revision:v1\x00%s\x00%d\x00%d\x00%d", filepath.ToSlash(relative), info.Size(), info.ModTime().UnixNano(), uint32(info.Mode()))
	digest := sha256.Sum256([]byte(payload))
	revision := binary.BigEndian.Uint64(digest[:8]) & maxSafeJSONInteger
	if revision == 0 {
		return 1
	}
	return revision
}

func atomicWriteWorkspaceFile(target, relative string, expected uint64, contents []byte, mode os.FileMode) error {
	parent := filepath.Dir(target)
	temporary, err := os.CreateTemp(parent, ".wenzwork-text-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error {
		_ = temporary.Close()
		return cause
	}
	if err := temporary.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fail(err)
	}
	if err := temporary.Sync(); err != nil {
		return fail(err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// This second optimistic-concurrency check closes the potentially long
	// window spent writing and fsyncing the temporary file. os.Rename has no
	// portable compare-and-swap variant, so an external local writer could still
	// race between this Lstat and Rename; peer RPCs themselves are serialized by
	// fileRPCManager.mu.
	current, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) || err == nil && (current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || workspaceFileRevision(relative, current) != expected) {
		return errRPCRevision
	}
	if err != nil {
		return err
	}
	return os.Rename(temporaryPath, target)
}

func hashWorkspaceFile(ctx context.Context, path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	buffer := make([]byte, fileChunkBytes)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			total += int64(read)
			_, _ = hash.Write(buffer[:read])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil)), total, nil
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}

func trimIncompleteUTF8(value []byte) []byte {
	for len(value) > 0 && !utf8.Valid(value) {
		value = value[:len(value)-1]
	}
	return value
}

func firstError(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
