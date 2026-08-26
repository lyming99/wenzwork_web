package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumSearchEntries        = 10_000
	maximumSearchMatches        = 1_000
	maximumSearchContentBytes   = int64(8 << 20)
	maximumDeleteConfirmations  = 64
	maximumRecursiveDeleteBytes = int64(1 << 30)
)

var textFileExtensions = map[string]struct{}{
	".asm": {}, ".bat": {}, ".c": {}, ".cc": {}, ".cfg": {}, ".cmd": {}, ".conf": {}, ".cpp": {},
	".cs": {}, ".css": {}, ".csv": {}, ".cxx": {}, ".dart": {}, ".env": {}, ".fish": {},
	".gitattributes": {}, ".gitignore": {}, ".go": {}, ".gradle": {}, ".graphql": {}, ".h": {},
	".hpp": {}, ".ini": {}, ".java": {}, ".js": {}, ".json": {}, ".jsx": {}, ".kt": {}, ".kts": {},
	".less": {}, ".lock": {}, ".log": {}, ".lua": {}, ".m": {}, ".markdown": {}, ".md": {}, ".mdown": {},
	".mkd": {}, ".mdx": {}, ".mm": {}, ".php": {}, ".plist": {}, ".properties": {}, ".ps1": {},
	".py": {}, ".rb": {}, ".rs": {}, ".sass": {}, ".scss": {}, ".sh": {}, ".sql": {}, ".swift": {},
	".toml": {}, ".ts": {}, ".tsv": {}, ".tsx": {}, ".txt": {}, ".vue": {}, ".xml": {}, ".yaml": {},
	".yml": {}, ".zsh": {},
}

var fileCategoryExtensions = map[string]map[string]struct{}{
	"image": {
		".avif": {}, ".bmp": {}, ".gif": {}, ".heic": {}, ".heif": {}, ".ico": {}, ".jpeg": {},
		".jpg": {}, ".png": {}, ".tif": {}, ".tiff": {}, ".wbmp": {}, ".webp": {},
	},
	"video": {
		".3g2": {}, ".3gp": {}, ".avi": {}, ".flv": {}, ".m2ts": {}, ".m4v": {}, ".mkv": {},
		".mov": {}, ".mp4": {}, ".mpeg": {}, ".mpg": {}, ".mts": {}, ".ogv": {}, ".vob": {},
		".webm": {}, ".wmv": {},
	},
	"audio": {
		".aac": {}, ".flac": {}, ".m4a": {}, ".mp3": {}, ".ogg": {}, ".opus": {}, ".wav": {}, ".wma": {},
	},
	"archive": {
		".7z": {}, ".gz": {}, ".iso": {}, ".jar": {}, ".rar": {}, ".tar": {}, ".zip": {},
	},
}

var ignoredSearchDirectories = map[string]struct{}{
	".dart_tool": {}, ".git": {}, ".idea": {}, ".vscode": {}, "build": {}, "node_modules": {},
}

type deleteConfirmation struct {
	Token           string
	ProjectID       uuid.UUID
	ProjectRevision uint64
	RelativePath    string
	Revision        uint64
	TreeDigest      string
	ItemCount       uint64
	TotalBytes      int64
	ExpiresAt       time.Time
}

type deleteTreeSnapshot struct {
	Digest     string
	ItemCount  uint64
	TotalBytes int64
}

type fileSearchCursor struct {
	Version        int    `json:"v"`
	ProjectID      string `json:"i"`
	RelativePath   string `json:"p"`
	QueryDigest    string `json:"q"`
	SnapshotDigest string `json:"s"`
	Offset         int    `json:"o"`
}

type remoteFileSearchResult struct {
	Entry      remoteFileEntry `json:"entry"`
	ParentPath string          `json:"parentPath"`
	MatchKind  string          `json:"matchKind"`
	Snippet    string          `json:"snippet,omitempty"`
	score      int
}

func legacyReadOnlyFileMethod(method string) bool {
	switch {
	case method == "file.list", method == "file.stat", method == "file.read-text":
		return true
	case strings.HasPrefix(method, "file.download."):
		return true
	default:
		return false
	}
}

func (manager *fileRPCManager) canStartTransfer(projectID uuid.UUID, _ uint64) bool {
	if manager == nil || projectID == uuid.Nil || uint64(len(manager.downloads)+len(manager.uploads)) >= maximumActiveFileTransfers {
		return false
	}
	projectTransfers := 0
	for _, transfer := range manager.downloads {
		if transfer.ProjectID == projectID {
			projectTransfers++
		}
	}
	for _, transfer := range manager.uploads {
		if transfer.ProjectID == projectID {
			projectTransfers++
		}
	}
	return projectTransfers < maximumProjectFileTransfers
}

func (d dispatcher) fileCreateText(ctx context.Context, project registeredProject, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "parentPath", "name", "content") {
		return nil, 0, errRPCInvalid
	}
	parentRelative, okParent := optionalFilePathInput(input, "parentPath")
	name, okName := fileNameInput(input, "name")
	content := ""
	if raw, exists := input["content"]; exists {
		var ok bool
		content, ok = raw.(string)
		if !ok || !utf8.ValidString(content) || len([]byte(content)) > maximumTextReadBytes {
			return nil, 0, errRPCInvalid
		}
	}
	if !okParent || !okName {
		return nil, 0, errRPCInvalid
	}
	parent, normalizedParent, err := secureExistingProjectPath(project, parentRelative)
	if err != nil {
		return nil, 0, err
	}
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	target := filepath.Join(parent, name)
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		_ = rollbackV2SideEffect(ctx)
		return nil, 0, errRPCRevision
	}
	if err != nil {
		_ = rollbackV2SideEffect(ctx)
		return nil, 0, err
	}
	created := false
	fail := func(cause error) (any, uint64, error) {
		_ = file.Close()
		if !created {
			if removeErr := os.Remove(target); removeErr == nil {
				if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
					cause = errors.Join(cause, sideEffectErr)
				}
			}
		}
		return nil, 0, cause
	}
	if _, err := file.WriteString(content); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		return fail(err)
	}
	if err := d.state.persistMutation(); err != nil {
		return fail(err)
	}
	created = true
	info, err := os.Stat(target)
	if err != nil {
		return nil, 0, err
	}
	relative := filepath.ToSlash(filepath.Join(normalizedParent, name))
	entry := remoteFileEntryFromInfo(relative, info)
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{"entry": entry, "revision": entry.Revision, "encoding": "utf-8"}, entry.Revision, nil
}

func (d dispatcher) fileMove(ctx context.Context, project registeredProject, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "path", "targetDirectoryPath", "expectedRevision") {
		return nil, 0, errRPCInvalid
	}
	relative, okPath := filePathInput(input, "path")
	targetRelative, okTarget := optionalFilePathInput(input, "targetDirectoryPath")
	expected, present, okRevision := optionalUint64(input, "expectedRevision")
	if !okPath || !okTarget || !present || !okRevision {
		return nil, 0, errRPCInvalid
	}
	source, normalized, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	if normalized == "" {
		return nil, 0, errRPCForbidden
	}
	info, err := os.Stat(source)
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	if workspaceFileRevision(normalized, info) != expected {
		return nil, 0, errRPCRevision
	}
	targetDirectory, normalizedTarget, err := secureExistingProjectPath(project, targetRelative)
	if err != nil {
		return nil, 0, err
	}
	targetInfo, err := os.Stat(targetDirectory)
	if err != nil || !targetInfo.IsDir() {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	if info.IsDir() && (normalizedTarget == normalized || strings.HasPrefix(normalizedTarget, normalized+"/")) {
		return nil, 0, errRPCForbidden
	}
	destination := filepath.Join(targetDirectory, filepath.Base(source))
	if sameFilesystemPath(source, destination) {
		entry := remoteFileEntryFromInfo(normalized, info)
		if err := completeV2WithoutSideEffect(ctx); err != nil {
			return nil, 0, err
		}
		return map[string]any{"entry": entry, "revision": entry.Revision}, entry.Revision, nil
	}
	if _, err := os.Lstat(destination); err == nil {
		return nil, 0, errRPCRevision
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := os.Rename(source, destination); err != nil {
		return nil, 0, err
	}
	if err := d.state.persistMutation(); err != nil {
		if rollbackErr := os.Rename(destination, source); rollbackErr == nil {
			if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
				return nil, 0, errors.Join(err, sideEffectErr)
			}
		}
		return nil, 0, err
	}
	updated, err := os.Stat(destination)
	if err != nil {
		return nil, 0, err
	}
	destinationRelative := filepath.ToSlash(filepath.Join(normalizedTarget, filepath.Base(source)))
	entry := remoteFileEntryFromInfo(destinationRelative, updated)
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{"entry": entry, "revision": entry.Revision}, entry.Revision, nil
}

func (d dispatcher) fileDetails(project registeredProject, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "path") {
		return nil, 0, errRPCInvalid
	}
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
		return nil, 0, firstError(err, errRPCInvalid)
	}
	entry := remoteFileEntryFromInfo(normalized, info)
	result := map[string]any{
		"entry":      entry,
		"createdAt":  fileCreatedAt(info).UTC(),
		"modifiedAt": info.ModTime().UTC(),
		"category":   entry.Category,
		"extension":  entry.Extension,
	}
	if info.Mode().IsRegular() {
		textReadable, encoding := false, ""
		if info.Size() >= 0 && info.Size() <= maximumTextReadBytes {
			data, readErr := readBoundedFile(resolved, maximumTextReadBytes)
			if readErr == nil {
				_, encoding, readErr = decodeWorkspaceText(info.Name(), data)
				textReadable = readErr == nil
			}
		}
		result["text"] = map[string]any{
			"readable": textReadable, "editable": textReadable && entry.Writable,
			"encoding": encoding, "maximumBytes": maximumTextReadBytes,
		}
	}
	return result, entry.Revision, nil
}

func (d dispatcher) fileSearch(ctx context.Context, project registeredProject, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "path", "query", "limit", "cursor") {
		return nil, 0, errRPCInvalid
	}
	relative, okPath := optionalFilePathInput(input, "path")
	query, okQuery := inputString(input, "query", 256)
	limit, err := filePageLimit(input)
	if !okPath || !okQuery || err != nil {
		return nil, 0, errRPCInvalid
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, 0, errRPCInvalid
	}
	root, normalizedRoot, err := secureExistingProjectPath(project, relative)
	if err != nil {
		return nil, 0, err
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	queryLower := strings.ToLower(query)
	results := make([]remoteFileSearchResult, 0)
	pending := []struct {
		path     string
		relative string
	}{{path: root, relative: normalizedRoot}}
	visited := 0
	contentBytes := int64(0)
	truncated := false
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entries, readErr := os.ReadDir(current.path)
		if readErr != nil {
			continue
		}
		for index := len(entries) - 1; index >= 0; index-- {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			visited++
			if visited > maximumSearchEntries {
				truncated = true
				pending = nil
				break
			}
			entry := entries[index]
			childRelative := filepath.ToSlash(filepath.Join(current.relative, entry.Name()))
			childPath, normalizedChild, resolveErr := secureExistingProjectPath(project, childRelative)
			if resolveErr != nil {
				continue
			}
			info, infoErr := os.Stat(childPath)
			if infoErr != nil {
				continue
			}
			if info.IsDir() {
				if _, ignored := ignoredSearchDirectories[strings.ToLower(entry.Name())]; !ignored {
					pending = append(pending, struct {
						path     string
						relative string
					}{path: childPath, relative: normalizedChild})
				}
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			nameLower, pathLower := strings.ToLower(entry.Name()), strings.ToLower(normalizedChild)
			matchKind, snippet, score := "", "", 0
			switch {
			case nameLower == queryLower:
				matchKind, score = "fileName", 0
			case strings.HasPrefix(nameLower, queryLower):
				matchKind, score = "fileName", 10
			case strings.Contains(nameLower, queryLower):
				matchKind, score = "fileName", 20
			case strings.Contains(pathLower, queryLower):
				matchKind, score = "path", 30
			case fileCategory(entry.Name()) == "text" && info.Size() >= 0 && info.Size() <= maximumTextReadBytes && contentBytes+info.Size() <= maximumSearchContentBytes:
				contentBytes += info.Size()
				data, readErr := readBoundedFile(childPath, maximumTextReadBytes)
				if readErr == nil {
					content, _, decodeErr := decodeWorkspaceText(entry.Name(), data)
					if decodeErr == nil {
						matchIndex := strings.Index(strings.ToLower(content), queryLower)
						if matchIndex >= 0 {
							matchKind, score = "content", 40
							snippet = contentSnippet(content, matchIndex, len(query))
						}
					}
				}
			}
			if matchKind == "" {
				continue
			}
			parent := filepath.ToSlash(filepath.Dir(normalizedChild))
			if parent == "." {
				parent = ""
			}
			results = append(results, remoteFileSearchResult{
				Entry: remoteFileEntryFromInfo(normalizedChild, info), ParentPath: parent,
				MatchKind: matchKind, Snippet: snippet, score: score,
			})
			if len(results) >= maximumSearchMatches {
				truncated = true
				pending = nil
				break
			}
		}
	}
	slices.SortFunc(results, func(left, right remoteFileSearchResult) int {
		if left.score != right.score {
			return left.score - right.score
		}
		return strings.Compare(strings.ToLower(left.Entry.RelativePath), strings.ToLower(right.Entry.RelativePath))
	})
	queryDigest := shortDigest(queryLower)
	snapshotDigest := searchSnapshotDigest(results, truncated)
	start, reset, err := decodeFileSearchCursor(input, project.ID, normalizedRoot, queryDigest, snapshotDigest)
	if err != nil {
		return nil, 0, err
	}
	revision := d.state.revisionValue()
	if reset {
		return map[string]any{
			"items": []remoteFileSearchResult{}, "nextCursor": (*string)(nil), "highWatermark": revision,
			"resetRequired": true, "truncated": truncated,
		}, revision, nil
	}
	if start > len(results) {
		return nil, 0, errRPCInvalid
	}
	requestedEnd := min(len(results), start+limit)
	build := func(count int) (any, error) {
		end := start + count
		var next *string
		if end < len(results) {
			encoded, encodeErr := encodeFileSearchCursor(fileSearchCursor{
				Version: 1, ProjectID: project.ID.String(), RelativePath: normalizedRoot,
				QueryDigest: queryDigest, SnapshotDigest: snapshotDigest, Offset: end,
			})
			if encodeErr != nil {
				return nil, encodeErr
			}
			next = &encoded
		}
		return map[string]any{
			"items": results[start:end], "nextCursor": next, "highWatermark": revision,
			"resetRequired": false, "truncated": truncated,
		}, nil
	}
	count, err := rpcPagePrefixLengthE(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	result, err := build(count)
	return result, revision, err
}

func (d dispatcher) fileDeletePrepare(ctx context.Context, manager *fileRPCManager, project registeredProject, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "path", "expectedRevision") {
		return nil, 0, errRPCInvalid
	}
	relative, okPath := filePathInput(input, "path")
	expected, present, okRevision := optionalUint64(input, "expectedRevision")
	if !okPath || !present || !okRevision {
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
	if err != nil || workspaceFileRevision(normalized, info) != expected {
		return nil, 0, firstError(err, errRPCRevision)
	}
	if !info.IsDir() {
		return map[string]any{"requiresConfirmation": false, "revision": expected}, expected, nil
	}
	snapshot, err := snapshotDeleteTree(ctx, resolved)
	if err != nil {
		return nil, 0, err
	}
	if snapshot.ItemCount <= 1 {
		return map[string]any{"requiresConfirmation": false, "revision": expected}, expected, nil
	}
	if !project.Policy.AllowRecursiveDelete || !agentFeatureFlags(d.state)["recursiveDelete.confirmed"] {
		return nil, 0, errRPCCapability
	}
	if len(manager.deletions) >= maximumDeleteConfirmations {
		return nil, 0, errRPCBusy
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, 0, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expiresAt := d.now().UTC().Add(fileDeleteConfirmationTTL)
	manager.deletions[token] = &deleteConfirmation{
		Token: token, ProjectID: project.ID, ProjectRevision: project.Revision,
		RelativePath: normalized, Revision: expected, TreeDigest: snapshot.Digest,
		ItemCount: snapshot.ItemCount, TotalBytes: snapshot.TotalBytes, ExpiresAt: expiresAt,
	}
	return map[string]any{
		"requiresConfirmation": true, "confirmationToken": token, "relativePath": normalized,
		"revision": expected, "itemCount": snapshot.ItemCount, "totalBytes": snapshot.TotalBytes,
		"expiresAt": expiresAt,
	}, expected, nil
}

func (d dispatcher) fileDeleteConfirmed(ctx context.Context, manager *fileRPCManager, project registeredProject, resolved, normalized string, revision uint64, token string) (any, uint64, error) {
	confirmation := manager.deletions[token]
	if confirmation == nil {
		return nil, 0, errRPCInvalid
	}
	delete(manager.deletions, token)
	if !confirmation.ExpiresAt.After(d.now().UTC()) || confirmation.ProjectID != project.ID ||
		confirmation.ProjectRevision != project.Revision || confirmation.RelativePath != normalized ||
		confirmation.Revision != revision || !project.Policy.AllowRecursiveDelete ||
		!agentFeatureFlags(d.state)["recursiveDelete.confirmed"] {
		return nil, 0, errRPCRevision
	}
	snapshot, err := snapshotDeleteTree(ctx, resolved)
	if err != nil {
		return nil, 0, err
	}
	if snapshot.Digest != confirmation.TreeDigest || snapshot.ItemCount != confirmation.ItemCount || snapshot.TotalBytes != confirmation.TotalBytes {
		return nil, 0, errRPCRevision
	}
	tombstone := filepath.Join(filepath.Dir(resolved), ".wenzwork-delete-"+uuid.NewString())
	if _, err := os.Lstat(tombstone); !errors.Is(err, os.ErrNotExist) {
		return nil, 0, firstError(err, errRPCRevision)
	}
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := os.Rename(resolved, tombstone); err != nil {
		return nil, 0, err
	}
	if err := os.RemoveAll(tombstone); err != nil {
		if rollbackErr := os.Rename(tombstone, resolved); rollbackErr == nil {
			if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
				return nil, 0, errors.Join(err, sideEffectErr)
			}
		}
		return nil, 0, err
	}
	if err := d.state.persistMutation(); err != nil {
		return nil, 0, err
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"deleted": true, "recursive": true, "relativePath": normalized,
		"itemCount": confirmation.ItemCount, "totalBytes": confirmation.TotalBytes,
	}, d.state.revisionValue(), nil
}

func snapshotDeleteTree(ctx context.Context, root string) (deleteTreeSnapshot, error) {
	hash := sha256.New()
	snapshot := deleteTreeSnapshot{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errRPCForbidden
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !sameFilesystemPath(path, resolved) {
			return errRPCForbidden
		}
		snapshot.ItemCount++
		if snapshot.ItemCount > maximumRecursiveDeleteEntries {
			return errRPCBusy
		}
		if info.Mode().IsRegular() {
			if info.Size() < 0 || info.Size() > maximumRecursiveDeleteBytes-snapshot.TotalBytes {
				return errRPCBusy
			}
			snapshot.TotalBytes += info.Size()
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\n", filepath.ToSlash(relative), info.Size(), info.ModTime().UnixNano(), uint32(info.Mode()))
		return nil
	})
	if err != nil {
		return deleteTreeSnapshot{}, err
	}
	snapshot.Digest = base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
	return snapshot, nil
}

func decodeFileSearchCursor(input rpcInput, projectID uuid.UUID, relative, queryDigest, snapshotDigest string) (int, bool, error) {
	encoded, ok := optionalInputString(input, "cursor", 1024)
	if !ok {
		return 0, false, errRPCInvalid
	}
	if encoded == "" {
		return 0, false, nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(raw) > 768 || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return 0, false, errRPCInvalid
	}
	var cursor fileSearchCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cursor) != nil || decoder.Decode(new(any)) != io.EOF || cursor.Version != 1 || cursor.Offset < 0 || cursor.Offset > maximumSearchMatches {
		return 0, false, errRPCInvalid
	}
	if cursor.ProjectID != projectID.String() || cursor.RelativePath != relative || cursor.QueryDigest != queryDigest || cursor.SnapshotDigest != snapshotDigest {
		return 0, true, nil
	}
	return cursor.Offset, false, nil
}

func encodeFileSearchCursor(cursor fileSearchCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func searchSnapshotDigest(results []remoteFileSearchResult, truncated bool) string {
	hash := sha256.New()
	for _, result := range results {
		_, _ = fmt.Fprintf(hash, "%s\x00%d\x00%s\x00%s\n", result.Entry.RelativePath, result.Entry.Revision, result.MatchKind, result.Snippet)
	}
	if truncated {
		_, _ = hash.Write([]byte("truncated"))
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

func shortDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func contentSnippet(content string, index, length int) string {
	const contextBytes = 96
	index = min(max(0, index), len(content))
	length = max(0, length)
	start := max(0, index-contextBytes)
	end := min(len(content), index+length+contextBytes)
	for start < index && !utf8.RuneStart(content[start]) {
		start++
	}
	for end < len(content) && end > index && !utf8.RuneStart(content[end]) {
		end--
	}
	snippet := strings.TrimSpace(strings.Join(strings.Fields(content[start:end]), " "))
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(content) {
		snippet += "…"
	}
	return snippet
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errRPCInvalid
	}
	return data, nil
}

func fileExtension(name string) string {
	extension := strings.ToLower(filepath.Ext(name))
	if len(extension) > 32 {
		return ""
	}
	return extension
}

func fileCategory(name string) string {
	extension := fileExtension(name)
	if _, ok := textFileExtensions[extension]; ok {
		return "text"
	}
	for category, extensions := range fileCategoryExtensions {
		if _, ok := extensions[extension]; ok {
			return category
		}
	}
	return "other"
}

func truncateEncodedTextBytes(data []byte, maximum int) []byte {
	if maximum < 0 || len(data) <= maximum {
		return data
	}
	end := maximum
	if len(data) >= 2 && (data[0] == 0xff && data[1] == 0xfe || data[0] == 0xfe && data[1] == 0xff) {
		if end < 2 {
			return nil
		}
		end -= (end - 2) % 2
		return data[:end]
	}
	return trimIncompleteUTF8(data[:end])
}

func decodeWorkspaceText(name string, data []byte) (string, string, error) {
	if len(data) >= 2 && (data[0] == 0xff && data[1] == 0xfe || data[0] == 0xfe && data[1] == 0xff) {
		littleEndian := data[0] == 0xff
		body := data[2:]
		units := make([]uint16, 0, (len(body)+1)/2)
		for index := 0; index+1 < len(body); index += 2 {
			if littleEndian {
				units = append(units, uint16(body[index])|uint16(body[index+1])<<8)
			} else {
				units = append(units, uint16(body[index])<<8|uint16(body[index+1]))
			}
		}
		if len(body)%2 != 0 {
			units = append(units, utf8.RuneError)
		}
		encoding := "utf-16be"
		if littleEndian {
			encoding = "utf-16le"
		}
		return string(utf16.Decode(units)), encoding, nil
	}
	encoding := "utf-8"
	if len(data) >= 3 && bytes.Equal(data[:3], []byte{0xef, 0xbb, 0xbf}) {
		data = data[3:]
		encoding = "utf-8-bom"
	}
	_, knownText := textFileExtensions[fileExtension(name)]
	if !knownText && looksBinaryText(data) {
		return "", "", errRPCInvalid
	}
	if !utf8.Valid(data) {
		if !knownText {
			return "", "", errRPCInvalid
		}
		return strings.ToValidUTF8(string(data), string(utf8.RuneError)), encoding, nil
	}
	return string(data), encoding, nil
}

func looksBinaryText(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return true
	}
	controls, runes := 0, 0
	for _, character := range string(data) {
		runes++
		if character < 0x20 && character != '\t' && character != '\n' && character != '\r' || character >= 0x7f && character <= 0x9f {
			controls++
		}
	}
	maximum := max(1, (runes+19)/20)
	return controls > maximum
}

func truncateUTF8String(value string, maximum int) string {
	if maximum >= len(value) {
		return value
	}
	if maximum <= 0 {
		return ""
	}
	data := []byte(value)[:maximum]
	return string(trimIncompleteUTF8(data))
}

func onlyInputFields(input rpcInput, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range input {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}
