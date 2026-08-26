package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

const maximumLocalProjects = 500

var projectCapabilities = []string{"ai.summarize", "markdown.render", "workspace.inspect"}

func scanWorkspaceProjects(ctx context.Context, state *agentState) (map[string]localProject, error) {
	if state == nil || state.business == nil {
		return nil, errors.New("workspace registry is unavailable")
	}
	// A direct-child compatibility scan is kept for pre-v2 --workspace
	// installations. Explicitly registered projects are never inferred from a
	// client supplied path and can live anywhere on the device.
	if err := state.business.ensureLegacyWorkspace(ctx, state); err != nil {
		return nil, err
	}
	registered, err := state.business.listProjects(ctx, false)
	if err != nil {
		return nil, err
	}
	result := make(map[string]localProject, len(registered))
	for _, record := range registered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(result) >= maximumLocalProjects {
			break
		}
		project := localProject{
			ID: record.ID, DisplayName: record.DisplayName, RelativePath: record.LegacyRelativePath,
			Revision: record.Revision, RegistryRevision: record.Revision, ObservedAt: record.UpdatedAt, State: record.State,
		}
		info, pathErr := os.Lstat(record.LocalPath)
		if pathErr != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			project.State = "unavailable"
			project.Fingerprint = "unavailable"
		} else if resolved, resolveErr := canonicalProjectPath(record.LocalPath); resolveErr != nil {
			project.State = "unavailable"
			project.Fingerprint = "unavailable"
		} else {
			project.State = "available"
			project.Fingerprint = projectFingerprint(project, info, resolved)
		}
		result[project.ID.String()] = project
	}
	return result, nil
}

func reconcileWorkspaceProjects(ctx context.Context, state *agentState, store *controlStateStore, now time.Time, force bool, onlyProject *uuid.UUID) (uint64, error) {
	scanned, err := scanWorkspaceProjects(ctx, state)
	if err != nil {
		return 0, err
	}
	var required uint64
	err = store.update(func(persisted *controlPersistentState) error {
		keys := make([]string, 0, len(scanned))
		for key := range scanned {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		next := persisted.Sync.HighWatermark + uint64(len(persisted.Sync.Pending)) + 1
		for _, key := range keys {
			current := scanned[key]
			previous, existed := persisted.Sync.Projects[key]
			previousRevision := previous.Revision
			if !existed {
				previousRevision = latestProjectProjectionRevision(persisted, current.ID)
			}
			if existed {
				current.Revision = previous.Revision
				current.ObservedAt = previous.ObservedAt
			}
			selected := onlyProject == nil || current.ID == *onlyProject
			visibleChanged := !existed || previous.ObservedAt.IsZero() || current.Fingerprint != previous.Fingerprint || current.DisplayName != previous.DisplayName || current.State != previous.State
			// States written before registryRevision was introduced are upgraded in
			// place when their visible projection still describes the current
			// registration. This records the two revision domains without creating a
			// spurious public project change during an Agent upgrade.
			migrationOnly := existed && previous.RegistryRevision == 0 && !visibleChanged
			changed := visibleChanged || (!migrationOnly && current.RegistryRevision != previous.RegistryRevision)
			if (changed || force) && selected {
				current.Revision = nextProjectProjectionRevision(previousRevision, current.RegistryRevision)
				current.ObservedAt = now.UTC()
				persisted.Sync.Pending = append(persisted.Sync.Pending, remotecontrol.DeviceChange{
					Sequence: next, Kind: "project", Operation: "upsert", ResourceID: current.ID,
					Revision: current.Revision, OccurredAt: now.UTC(), DisplayName: current.DisplayName,
					Capabilities: append([]string(nil), projectCapabilities...), State: "available",
				})
				appendProjectChange(persisted, current, false)
				required, next = next, next+1
			}
			scanned[key] = current
		}
		if onlyProject == nil {
			removedKeys := make([]string, 0)
			for key := range persisted.Sync.Projects {
				if _, exists := scanned[key]; !exists {
					removedKeys = append(removedKeys, key)
				}
			}
			slices.Sort(removedKeys)
			for _, key := range removedKeys {
				removed := persisted.Sync.Projects[key]
				removed.State, removed.Revision, removed.ObservedAt = "removed", removed.Revision+1, now.UTC()
				persisted.Sync.Pending = append(persisted.Sync.Pending, remotecontrol.DeviceChange{
					Sequence: next, Kind: "project", Operation: "tombstone", ResourceID: removed.ID,
					Revision: removed.Revision, OccurredAt: now.UTC(), State: "removed",
				})
				appendProjectChange(persisted, removed, true)
				required, next = next, next+1
			}
		}
		persisted.Sync.Projects = scanned
		return nil
	})
	return required, err
}

func prepareWorkspaceProjectReset(ctx context.Context, state *agentState, store *controlStateStore, now time.Time) error {
	scanned, err := scanWorkspaceProjects(ctx, state)
	if err != nil {
		return err
	}
	return store.update(func(persisted *controlPersistentState) error {
		removedKeys := make([]string, 0)
		for key := range persisted.Sync.Projects {
			if _, exists := scanned[key]; !exists {
				removedKeys = append(removedKeys, key)
			}
		}
		slices.Sort(removedKeys)
		for _, key := range removedKeys {
			removed := persisted.Sync.Projects[key]
			removed.State, removed.Revision, removed.ObservedAt = "removed", removed.Revision+1, now.UTC()
			appendProjectChange(persisted, removed, true)
		}
		keys := make([]string, 0, len(scanned))
		for key := range scanned {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		pending := make([]remotecontrol.DeviceChange, 0, len(keys))
		for index, key := range keys {
			project := scanned[key]
			prior := persisted.Sync.Projects[key]
			priorRevision := max(prior.Revision, latestProjectProjectionRevision(persisted, project.ID))
			project.Revision = nextProjectProjectionRevision(priorRevision, project.RegistryRevision)
			project.ObservedAt = now.UTC()
			scanned[key] = project
			appendProjectChange(persisted, project, false)
			pending = append(pending, remotecontrol.DeviceChange{
				Sequence: uint64(index + 1), Kind: "project", Operation: "upsert", ResourceID: project.ID,
				Revision: project.Revision, OccurredAt: now.UTC(), DisplayName: project.DisplayName,
				Capabilities: append([]string(nil), projectCapabilities...), State: "available",
			})
		}
		persisted.Sync = controlSyncState{
			HighWatermark: 0, Reset: true, Projects: scanned,
			TaskProjections: map[string]taskProjectionCursor{}, Pending: pending,
		}
		return nil
	})
}

// Project mutations use the SQLite registry revision while controllers see a
// projection revision. Keeping the public revision at or above the registry
// revision makes a restored registration distinguishable from an older
// projection without giving up monotonic projection refreshes.
func nextProjectProjectionRevision(previous, registry uint64) uint64 {
	next := uint64(1)
	if previous < ^uint64(0) {
		next = previous + 1
	} else {
		next = previous
	}
	if registry > next {
		next = registry
	}
	return next
}

func latestProjectProjectionRevision(state *controlPersistentState, projectID uuid.UUID) uint64 {
	if state == nil || projectID == uuid.Nil {
		return 0
	}
	revision := state.Sync.Projects[projectID.String()].Revision
	for index := len(state.ProjectChanges) - 1; index >= 0; index-- {
		change := state.ProjectChanges[index]
		if change.Project.ID == projectID && change.Project.Revision > revision {
			revision = change.Project.Revision
		}
	}
	return revision
}

func appendProjectChange(state *controlPersistentState, project localProject, deleted bool) {
	if state.ProjectHighWatermark < ^uint64(0) {
		state.ProjectHighWatermark++
	}
	state.ProjectChanges = append(state.ProjectChanges, localProjectChange{
		Sequence: state.ProjectHighWatermark, Deleted: deleted, Project: project,
	})
	if len(state.ProjectChanges) > maximumPersistedProjectChanges {
		state.ProjectChanges = append([]localProjectChange(nil), state.ProjectChanges[len(state.ProjectChanges)-maximumPersistedProjectChanges:]...)
	}
	if len(state.ProjectChanges) > 0 {
		state.ProjectMinimumAvailableSequence = state.ProjectChanges[0].Sequence
	} else if state.ProjectHighWatermark < ^uint64(0) {
		state.ProjectMinimumAvailableSequence = state.ProjectHighWatermark + 1
	}
}

func stableProjectID(deviceID uuid.UUID, relative string) uuid.UUID {
	return uuid.NewSHA1(deviceID, []byte("wenzwork-workspace-project:v1\x00"+filepath.ToSlash(relative)))
}

func safeProjectDisplayName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) {
		return ""
	}
	var builder strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) {
			continue
		}
		builder.WriteRune(character)
		if builder.Len() >= 200 {
			break
		}
	}
	return strings.TrimSpace(builder.String())
}

func projectFingerprint(project localProject, info os.FileInfo, resolved string) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "wenzwork-project-fingerprint:v1\x00%s\x00%s\x00%d\x00%d\x00%d", project.ID, project.DisplayName, info.Size(), info.ModTime().UnixNano(), uint32(info.Mode()))
	for _, marker := range []string{".git", "go.mod", "package.json", "Cargo.toml", "pyproject.toml", "README.md"} {
		markerInfo, err := os.Lstat(filepath.Join(resolved, marker))
		if err != nil || markerInfo.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			continue
		}
		_, _ = fmt.Fprintf(hash, "\x00%s\x00%d\x00%d\x00%d", marker, markerInfo.Size(), markerInfo.ModTime().UnixNano(), uint32(markerInfo.Mode()))
	}
	return base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}
