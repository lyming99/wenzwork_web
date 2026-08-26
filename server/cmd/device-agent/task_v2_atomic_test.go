package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTaskV2ActivationAtomicallyClearsSchedule(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	manualDefinition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	manualSchedule := fixture.now.Add(time.Hour)
	manualDefinition.Execution.ScheduledAt = &manualSchedule
	manual, err := fixture.store.Create(t.Context(), manualDefinition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), manual.Definition.ID, manual.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Definition.Execution.ScheduledAt != nil || waiting.DefinitionRevision != manual.DefinitionRevision+1 {
		t.Fatalf("manual activation did not clear schedule atomically: %+v", waiting)
	}
	persisted, err := fixture.store.Get(t.Context(), manual.Definition.ID)
	if err != nil || persisted.Definition.Execution.ScheduledAt != nil || persisted.Status != "waiting" {
		t.Fatalf("persisted manual activation = %+v, %v", persisted, err)
	}

	dueDefinition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	dueSchedule := fixture.now.Add(-time.Minute)
	dueDefinition.Execution.ScheduledAt = &dueSchedule
	due, err := fixture.store.Create(t.Context(), dueDefinition, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	activated, err := fixture.store.ActivateQueue(t.Context(), fixture.project.ID, nil, fixture.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if activated.AffectedCount != 1 || len(activated.Items) != 1 || activated.Items[0].Definition.ID != due.Definition.ID ||
		activated.Items[0].Definition.Execution.ScheduledAt != nil || activated.Items[0].DefinitionRevision != due.DefinitionRevision+1 {
		t.Fatalf("scheduled activation = %+v", activated)
	}
}

func TestTaskV2AcceptanceStateDoesNotAppendToSealedExecutionLog(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	definition.Execution.ScheduledAt = nil
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), definition.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := fixture.store.StartRun(t.Context(), definition.ID, waiting.Revision, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	awaiting, _, err := fixture.store.FinishRun(t.Context(), definition.ID, running.Revision, "awaitingAcceptance", 0, "execution_succeeded", "session-atomic", fixture.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if awaiting.Definition.Execution.CliSessionID != "session-atomic" || awaiting.DefinitionRevision != running.DefinitionRevision+1 {
		t.Fatalf("CLI session was not persisted with the run: %+v", awaiting)
	}

	completed, entry, err := fixture.store.Accept(t.Context(), definition.ID, awaiting.Revision, []byte("acceptance evidence"), fixture.now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" || completed.ResultCode != "accepted" || entry.Sequence != 0 {
		t.Fatalf("accepted task/log = %+v, %+v", completed, entry)
	}
	logs, err := fixture.store.ListLogs(t.Context(), definition.ID, "", 0, 1<<20)
	if err != nil || len(logs.Items) != 0 {
		t.Fatalf("acceptance appended execution-log body = %+v, %v", logs, err)
	}
}
