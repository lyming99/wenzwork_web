package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

func TestTaskV2ProjectionContainsOnlyReviewedMetadata(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	fixture.state.tasksV2 = fixture.store
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	definition.Title = "Private customer path C:/accounts/acme"
	definition.Environment["TASK_TEST_TOKEN"] = "projection-secret-marker"
	var config map[string]any
	if err := json.Unmarshal(definition.Config, &config); err != nil {
		t.Fatal(err)
	}
	config["promptText"] = "projection-prompt-marker"
	config["attachedFilePaths"] = []string{"projection-attachment-marker.md"}
	definition.Config, _ = json.Marshal(config)
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	controlStore, err := loadControlState(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	loop := &deviceControlLoop{state: fixture.state, store: controlStore, now: func() time.Time { return fixture.now }}
	if err := loop.reconcileTaskV2Projections(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := controlStore.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sync.Pending) != 1 {
		t.Fatalf("pending projections = %+v", snapshot.Sync.Pending)
	}
	projection := snapshot.Sync.Pending[0]
	if projection.Kind != "task" || projection.TaskType != "codex" || projection.Title != remotecontrol.TaskProjectionDisplayName("codex") ||
		projection.ResourceID != created.Definition.ID || projection.Status != "queued" || projection.ProjectID == nil || *projection.ProjectID != fixture.project.ID {
		t.Fatalf("projection = %+v", projection)
	}
	encoded, err := json.Marshal(snapshot.Sync.Pending)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		definition.Title, "projection-secret-marker", "projection-prompt-marker", "projection-attachment-marker.md",
		"promptText", "attachedFilePaths", "environment", "config", "cwd",
	} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("projection leaked %q: %s", marker, encoded)
		}
	}
}

func TestMetadataOnlyOfflineCancellationStopsTaskV2AtCurrentRevision(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	fixture.state.tasksV2 = fixture.store
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	loop := &deviceControlLoop{state: fixture.state, now: func() time.Time { return fixture.now.Add(time.Minute) }}
	handled, err := loop.cancelTaskV2(t.Context(), created.Definition.ID)
	if err != nil || !handled {
		t.Fatalf("cancelTaskV2() = %v, %v", handled, err)
	}
	cancelled, err := fixture.store.Get(t.Context(), created.Definition.ID)
	if err != nil || cancelled.Status != "cancelled" || cancelled.ResultCode != "cancelled" || cancelled.Revision <= created.Revision {
		t.Fatalf("cancelled task = %+v, %v", cancelled, err)
	}

	missing, err := loop.cancelTaskV2(t.Context(), uuid.New())
	if err != nil || missing {
		t.Fatalf("missing cancellation = %v, %v", missing, err)
	}
}
