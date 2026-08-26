package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	projectDirectoryHandleTTL   = 10 * time.Minute
	maximumProjectDirectoryPage = 256
	maximumProjectDirectoryIDs  = 4096
)

type remoteProjectDirectory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type projectDirectoryHandle struct {
	Path      string
	ExpiresAt time.Time
}

// Directory paths stay on the device. Controllers receive random, short-lived
// identifiers which can only be resolved by this Agent process.
type projectDirectoryBrowser struct {
	mu      sync.Mutex
	handles map[string]projectDirectoryHandle
	paths   map[string]string
}

var projectDirectoryBrowsers sync.Map // map[*agentState]*projectDirectoryBrowser

func projectDirectoryBrowserFor(state *agentState) *projectDirectoryBrowser {
	if value, ok := projectDirectoryBrowsers.Load(state); ok {
		return value.(*projectDirectoryBrowser)
	}
	created := &projectDirectoryBrowser{
		handles: make(map[string]projectDirectoryHandle),
		paths:   make(map[string]string),
	}
	actual, _ := projectDirectoryBrowsers.LoadOrStore(state, created)
	return actual.(*projectDirectoryBrowser)
}

func (d dispatcher) callProjectDirectoryList(input rpcInput) (any, uint64, error) {
	if d.state == nil || d.state.business == nil {
		return nil, 0, errRPCCapability
	}
	for key := range input {
		if key != "directoryId" && key != "cursor" && key != "limit" {
			return nil, 0, errRPCInvalid
		}
	}
	directoryID, ok := optionalInputString(input, "directoryId", 128)
	if !ok {
		return nil, 0, errRPCInvalid
	}
	now := dispatcherNow(d)
	browser := projectDirectoryBrowserFor(d.state)
	if directoryID == "" {
		items := make([]remoteProjectDirectory, 0, 8)
		for _, root := range projectDirectoryRoots() {
			item, err := browser.issue(root, now)
			if err == nil {
				items = append(items, item)
			}
		}
		if len(items) == 0 {
			return nil, 0, errRPCCapability
		}
		return map[string]any{"current": nil, "parent": nil, "items": items}, d.state.revisionValue(), nil
	}

	currentPath, err := browser.resolve(directoryID, now)
	if err != nil {
		return nil, 0, err
	}
	currentInfo, err := os.Stat(currentPath)
	if err != nil {
		return nil, 0, mapProjectDirectoryError(err)
	}
	if !currentInfo.IsDir() {
		return nil, 0, errRPCInvalid
	}
	entries, err := os.ReadDir(currentPath)
	if err != nil {
		return nil, 0, mapProjectDirectoryError(err)
	}
	if len(entries) > maximumProjectDirectoryIDs {
		return nil, 0, errRPCBusy
	}
	items := make([]remoteProjectDirectory, 0, min(len(entries), maximumProjectDirectoryIDs))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			continue
		}
		item, issueErr := browser.issue(filepath.Join(currentPath, entry.Name()), now)
		if issueErr == nil {
			items = append(items, item)
		}
	}
	current, err := browser.issue(currentPath, now)
	if err != nil {
		return nil, 0, err
	}
	var parent *remoteProjectDirectory
	parentPath := filepath.Dir(currentPath)
	if !sameFilesystemPath(parentPath, currentPath) {
		value, parentErr := browser.issue(parentPath, now)
		if parentErr != nil {
			return nil, 0, parentErr
		}
		parent = &value
	}
	directoryRevision := workspaceFileRevision(directoryID, currentInfo)
	pageWatermark, err := rpcPageSnapshotWatermark(map[string]any{
		"method": "project.directory.list", "directoryId": directoryID,
		"directoryRevision": directoryRevision, "items": items,
	})
	if err != nil {
		return nil, 0, err
	}
	start, requestedEnd, _, err := versionedPageWindow(input, len(items), pageWatermark)
	if err != nil {
		return nil, 0, err
	}
	build := func(count int) any {
		end := start + count
		return map[string]any{
			"current": current, "parent": parent, "items": items[start:end],
			"nextCursor":    versionedPageCursor(pageWatermark, end, len(items)),
			"highWatermark": pageWatermark, "directoryRevision": directoryRevision, "resetRequired": false,
		}
	}
	count, err := rpcPagePrefixLength(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), pageWatermark, nil
}

func (d dispatcher) callProjectCreate(ctx context.Context, input rpcInput) (any, uint64, error) {
	if d.state == nil || d.state.business == nil {
		return nil, 0, errRPCCapability
	}
	allowed := map[string]struct{}{
		"name": {}, "displayName": {}, "gitUrl": {}, "directoryId": {}, "parentDirectoryId": {},
	}
	for key := range input {
		if _, ok := allowed[key]; !ok {
			return nil, 0, errRPCInvalid
		}
	}
	name, ok := fileNameInput(input, "name")
	if !ok {
		return nil, 0, errRPCInvalid
	}
	displayName, displayOK := optionalInputString(input, "displayName", 200)
	gitURL, gitOK := optionalInputString(input, "gitUrl", 2048)
	directoryID, directoryOK := optionalInputString(input, "directoryId", 128)
	parentDirectoryID, parentOK := optionalInputString(input, "parentDirectoryId", 128)
	if !displayOK || !gitOK || !directoryOK || !parentOK || !validProjectGitURL(gitURL) ||
		(directoryID == "") == (parentDirectoryID == "") {
		return nil, 0, errRPCInvalid
	}
	if displayName == "" {
		displayName = name
	}

	browser := projectDirectoryBrowserFor(d.state)
	now := dispatcherNow(d)
	projectPath := ""
	createdDirectory := false
	createdPath := ""
	var err error
	if directoryID != "" {
		projectPath, err = browser.resolve(directoryID, now)
		if err == nil {
			err = completeV2WithoutSideEffect(ctx)
		}
	} else {
		var parentPath string
		parentPath, err = browser.resolve(parentDirectoryID, now)
		if err == nil {
			projectPath = filepath.Join(parentPath, name)
			if !sameFilesystemPath(filepath.Dir(projectPath), parentPath) {
				return nil, 0, errRPCInvalid
			}
			if err = beginV2SideEffect(ctx); err != nil {
				return nil, 0, err
			}
			err = os.Mkdir(projectPath, 0o700)
			if os.IsExist(err) {
				_ = rollbackV2SideEffect(ctx)
				return nil, 0, errRPCRevision
			}
			if err == nil {
				createdDirectory = true
				createdPath = projectPath
				projectPath, err = canonicalProjectPath(projectPath)
			}
		}
	}
	if err != nil {
		if createdDirectory {
			if rollbackErr := os.Remove(createdPath); rollbackErr == nil {
				if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
					return nil, 0, errors.Join(err, sideEffectErr)
				}
			}
		}
		return nil, 0, mapProjectDirectoryError(err)
	}
	project, err := d.state.business.addProject(ctx, projectPath, displayName, gitURL, defaultProjectPolicy)
	if err != nil {
		if createdDirectory {
			if rollbackErr := os.Remove(createdPath); rollbackErr == nil {
				if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
					return nil, 0, errors.Join(err, sideEffectErr)
				}
			}
		}
		return nil, 0, err
	}
	if createdDirectory {
		if err := commitV2SideEffect(ctx); err != nil {
			return nil, 0, err
		}
	}
	resultRevision := project.Revision
	if d.controlStore != nil {
		// Publish the device-local projection before returning the mutation. The
		// controller can then use one unambiguous projection revision for an
		// immediate delete, and the control loop can flush the pending upsert
		// without waiting for its periodic 30-second scan.
		if _, syncErr := reconcileWorkspaceProjects(ctx, d.state, d.controlStore, now, false, nil); syncErr == nil {
			if snapshot, snapshotErr := d.controlStore.snapshot(); snapshotErr == nil {
				if projected, found := snapshot.Sync.Projects[project.ID.String()]; found && projected.RegistryRevision == project.Revision {
					resultRevision = projected.Revision
				}
			}
		}
	}
	return map[string]any{
		"id": project.ID, "displayName": project.DisplayName, "gitUrl": project.GitURL,
		"state": project.State, "revision": resultRevision,
		"capabilities": append([]string(nil), projectCapabilities...), "createdAt": project.CreatedAt.UTC(),
	}, resultRevision, nil
}

func (d dispatcher) callProjectRemove(ctx context.Context, input rpcInput) (any, uint64, error) {
	if d.state == nil || d.state.business == nil {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "projectId", "expectedRevision") {
		return nil, 0, errRPCInvalid
	}
	projectIDText, ok := optionalInputString(input, "projectId", 36)
	expectedRevision, revisionPresent, revisionOK := optionalUint64(input, "expectedRevision")
	projectID, parseErr := uuid.Parse(projectIDText)
	if !ok || projectIDText == "" || parseErr != nil || projectID == uuid.Nil ||
		!revisionPresent || !revisionOK || expectedRevision == 0 || expectedRevision == ^uint64(0) {
		return nil, 0, errRPCInvalid
	}
	registryRevision := expectedRevision
	resultRevision := expectedRevision + 1
	if d.controlStore != nil {
		// Controllers receive the control-plane projection revision, while the
		// SQLite registry owns a separate optimistic-concurrency revision. Validate
		// the token in its own revision domain, then use the current registry
		// revision for the transactional mutation. Comparing the unrelated counters
		// made projects with refreshed directory snapshots impossible to remove.
		// This lookup also keeps unavailable workspaces removable without requiring
		// a successful filesystem scan first.
		snapshot, err := d.controlStore.snapshot()
		if err != nil {
			return nil, 0, err
		}
		project, exists := snapshot.Sync.Projects[projectID.String()]
		registered, lookupErr := d.state.business.projectByID(ctx, projectID)
		if lookupErr != nil {
			return nil, 0, lookupErr
		}
		if registered.State == "removed" || !exists || project.State == "removed" {
			if result, revision, removed, replayErr := d.replayRemovedProject(ctx, projectID, expectedRevision, snapshot); removed || replayErr != nil {
				return result, revision, replayErr
			}
			// If the best-effort local projection write after project.create could
			// not complete, its authoritative registry revision remains a valid CAS
			// for an immediate create -> remove sequence.
			if registered.State != "removed" && registered.Revision == expectedRevision {
				registryRevision = registered.Revision
			} else {
				return nil, 0, errRPCRevision
			}
		} else {
			projectionMatches := project.Revision == expectedRevision &&
				project.DisplayName == registered.DisplayName &&
				project.RegistryRevision != 0 &&
				project.RegistryRevision == registered.Revision
			if !projectionMatches {
				return nil, 0, errRPCRevision
			}
			registryRevision = registered.Revision
		}
	}
	removed, err := d.state.business.removeProject(ctx, projectID, &registryRevision)
	if err != nil {
		return nil, 0, err
	}
	if d.controlStore == nil {
		resultRevision = removed.Revision
	} else {
		// The registry mutation is already committed. Projection persistence is
		// deliberately best-effort here: the control loop will converge it on the
		// next scan if this immediate write fails, without turning a successful
		// soft delete into a misleading retryable failure.
		if _, syncErr := reconcileWorkspaceProjects(ctx, d.state, d.controlStore, dispatcherNow(d), false, nil); syncErr == nil {
			if snapshot, snapshotErr := d.controlStore.snapshot(); snapshotErr == nil {
				if revision, found := projectTombstoneRevision(snapshot, projectID, expectedRevision); found {
					resultRevision = revision
				}
			}
		}
	}
	return map[string]any{
		"removed": true, "projectId": removed.ID, "state": removed.State, "revision": resultRevision,
	}, resultRevision, nil
}

// replayRemovedProject makes project.remove idempotent across controllers. A
// second controller can still hold the last control-plane upsert while the
// device registry and local projection have already committed the tombstone.
// Only an authoritative removed registry row qualifies; an available project
// with a missing or stale projection remains a revision conflict.
func (d dispatcher) replayRemovedProject(
	ctx context.Context,
	projectID uuid.UUID,
	expectedRevision uint64,
	snapshot controlPersistentState,
) (any, uint64, bool, error) {
	registered, err := d.state.business.projectByID(ctx, projectID)
	if err != nil {
		return nil, 0, false, err
	}
	if registered.State != "removed" {
		return nil, 0, false, nil
	}
	resultRevision := expectedRevision + 1
	if revision, found := projectTombstoneRevision(snapshot, projectID, expectedRevision); found {
		resultRevision = revision
	} else if _, reconcileErr := reconcileWorkspaceProjects(ctx, d.state, d.controlStore, dispatcherNow(d), false, nil); reconcileErr == nil {
		if refreshed, snapshotErr := d.controlStore.snapshot(); snapshotErr == nil {
			if revision, found := projectTombstoneRevision(refreshed, projectID, expectedRevision); found {
				resultRevision = revision
			}
		}
	}
	return map[string]any{
		"removed": true, "projectId": projectID, "state": "removed", "revision": resultRevision,
	}, resultRevision, true, nil
}

func projectTombstoneRevision(snapshot controlPersistentState, projectID uuid.UUID, after uint64) (uint64, bool) {
	for index := len(snapshot.ProjectChanges) - 1; index >= 0; index-- {
		change := snapshot.ProjectChanges[index]
		if change.Deleted && change.Project.ID == projectID && change.Project.Revision > after {
			return change.Project.Revision, true
		}
	}
	return 0, false
}

func (browser *projectDirectoryBrowser) issue(path string, now time.Time) (remoteProjectDirectory, error) {
	canonical, err := canonicalProjectPath(path)
	if err != nil {
		return remoteProjectDirectory{}, mapProjectDirectoryError(err)
	}
	browser.mu.Lock()
	defer browser.mu.Unlock()
	browser.cleanup(now)
	pathKey := projectDirectoryPathKey(canonical)
	if existingID := browser.paths[pathKey]; existingID != "" {
		handle := browser.handles[existingID]
		handle.ExpiresAt = now.Add(projectDirectoryHandleTTL)
		browser.handles[existingID] = handle
		return remoteProjectDirectory{ID: existingID, Name: projectDirectoryName(canonical)}, nil
	}
	if len(browser.handles) >= maximumProjectDirectoryIDs {
		browser.evictOldest()
	}
	id := uuid.NewString()
	browser.handles[id] = projectDirectoryHandle{Path: canonical, ExpiresAt: now.Add(projectDirectoryHandleTTL)}
	browser.paths[pathKey] = id
	return remoteProjectDirectory{ID: id, Name: projectDirectoryName(canonical)}, nil
}

func (browser *projectDirectoryBrowser) resolve(id string, now time.Time) (string, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return "", errRPCNotFound
	}
	browser.mu.Lock()
	defer browser.mu.Unlock()
	browser.cleanup(now)
	handle, ok := browser.handles[id]
	if !ok {
		return "", errRPCNotFound
	}
	canonical, err := canonicalProjectPath(handle.Path)
	if err != nil {
		delete(browser.handles, id)
		delete(browser.paths, projectDirectoryPathKey(handle.Path))
		return "", mapProjectDirectoryError(err)
	}
	if !sameFilesystemPath(canonical, handle.Path) {
		delete(browser.handles, id)
		delete(browser.paths, projectDirectoryPathKey(handle.Path))
		return "", errRPCForbidden
	}
	handle.ExpiresAt = now.Add(projectDirectoryHandleTTL)
	browser.handles[id] = handle
	return canonical, nil
}

func (browser *projectDirectoryBrowser) cleanup(now time.Time) {
	for id, handle := range browser.handles {
		if !handle.ExpiresAt.After(now) {
			delete(browser.handles, id)
			delete(browser.paths, projectDirectoryPathKey(handle.Path))
		}
	}
}

func (browser *projectDirectoryBrowser) evictOldest() {
	oldestID := ""
	var oldest time.Time
	for id, handle := range browser.handles {
		if oldestID == "" || handle.ExpiresAt.Before(oldest) {
			oldestID, oldest = id, handle.ExpiresAt
		}
	}
	if oldestID != "" {
		handle := browser.handles[oldestID]
		delete(browser.handles, oldestID)
		delete(browser.paths, projectDirectoryPathKey(handle.Path))
	}
}

func projectDirectoryRoots() []string {
	if runtime.GOOS != "windows" {
		return []string{string(os.PathSeparator)}
	}
	result := make([]string, 0, 4)
	for drive := 'A'; drive <= 'Z'; drive++ {
		root := string(drive) + ":" + string(os.PathSeparator)
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			result = append(result, root)
		}
	}
	return result
}

func projectDirectoryName(path string) string {
	cleaned := filepath.Clean(path)
	if parent := filepath.Dir(cleaned); sameFilesystemPath(parent, cleaned) {
		return cleaned
	}
	name := filepath.Base(cleaned)
	if name == "" || name == "." {
		return cleaned
	}
	return name
}

func projectDirectoryPathKey(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func dispatcherNow(d dispatcher) time.Time {
	if d.now != nil {
		return d.now().UTC()
	}
	return time.Now().UTC()
}

func mapProjectDirectoryError(err error) error {
	switch {
	case err == nil:
		return nil
	case os.IsNotExist(err):
		return errRPCNotFound
	case os.IsPermission(err):
		return errRPCForbidden
	default:
		return err
	}
}
