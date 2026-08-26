package main

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func workflowV2TestDefinition(id uuid.UUID, title string) taskV2Definition {
	return taskV2Definition{
		ID: id, Kind: "codex", Title: title, CWD: ".",
		Config: json.RawMessage(`{
			"promptSource":"customText",
			"promptText":"Execute the workflow node",
			"attachedFilePaths":[],
			"launchMode":"cli",
			"goalMode":false,
			"reasoningEffort":"medium"
		}`),
		Environment: map[string]string{"TASK_NODE": title},
	}
}

func newWorkflowV2TestGraph(
	t *testing.T,
	fixture taskV2StoreFixture,
	parentID uuid.UUID,
	version uint64,
	now time.Time,
) (taskV2Definition, workflowV2Revision) {
	t.Helper()
	parent := taskV2Definition{
		ID: parentID, ProjectID: fixture.project.ID, Kind: "workflow", Title: "Release workflow", CWD: ".", Scope: "topLevel",
		Config: json.RawMessage(`{}`), Execution: taskV2ExecutionOptions{Relation: "dependency", Mode: "serial", RunImmediately: true},
		Environment: map[string]string{},
	}
	firstID, recoveryID := uuid.New(), uuid.New()
	revision := workflowV2Revision{
		ID: uuid.New(), WorkflowTaskID: parentID, Version: version, Description: "immutable test graph",
		FailurePolicy: "stopOnFailure", MaximumParallelism: 2,
		Nodes: []workflowV2Node{
			{ID: "start", Type: "start", Position: workflowV2Position{X: 0, Y: 100}},
			{ID: "execute", Type: "task", TaskDefinitionID: &firstID, TaskDefinition: taskDefinitionPointer(workflowV2TestDefinition(firstID, "Execute")), Position: workflowV2Position{X: 200, Y: 100}},
			{ID: "recover", Type: "task", TaskDefinitionID: &recoveryID, TaskDefinition: taskDefinitionPointer(workflowV2TestDefinition(recoveryID, "Recover")), Position: workflowV2Position{X: 400, Y: 200}},
			{ID: "finish", Type: "finish", Position: workflowV2Position{X: 600, Y: 100}},
		},
		Edges: []workflowV2Edge{
			{ID: "start-execute", SourceID: "start", TargetID: "execute", Type: "onSuccess"},
			{ID: "execute-finish", SourceID: "execute", TargetID: "finish", Type: "onSuccess"},
			{ID: "execute-recover", SourceID: "execute", TargetID: "recover", Type: "onFailure"},
			{ID: "recover-finish", SourceID: "recover", TargetID: "finish", Type: "always"},
		},
	}
	normalizedRevision, err := normalizeWorkflowV2Revision(fixture.project, parent, revision, version, now)
	if err != nil {
		t.Fatal(err)
	}
	normalizedParent, err := bindWorkflowV2Definition(fixture.project, parent, normalizedRevision)
	if err != nil {
		t.Fatal(err)
	}
	return normalizedParent, normalizedRevision
}

func taskDefinitionPointer(value taskV2Definition) *taskV2Definition { return &value }

func TestWorkflowV2RevisionValidationRejectsCyclesAndMutableNodeExecution(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	if len(revision.GraphDigest) != 64 || revision.Nodes[0].ID != "execute" {
		t.Fatalf("normalized revision = %+v", revision)
	}

	cyclic := revision
	cyclic.ID, cyclic.GraphDigest, cyclic.CreatedAt = uuid.New(), "", time.Time{}
	cyclic.Edges = append([]workflowV2Edge(nil), cyclic.Edges...)
	cyclic.Edges = append(cyclic.Edges, workflowV2Edge{ID: "cycle", SourceID: "finish", TargetID: "execute", Type: "always"})
	if _, err := normalizeWorkflowV2Revision(fixture.project, parent, cyclic, 1, fixture.now); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("cyclic revision error = %v", err)
	}

	mutable := revision
	mutable.ID, mutable.GraphDigest, mutable.CreatedAt = uuid.New(), "", time.Time{}
	mutable.Nodes = append([]workflowV2Node(nil), mutable.Nodes...)
	for index := range mutable.Nodes {
		if mutable.Nodes[index].Type != "task" {
			continue
		}
		definition := *mutable.Nodes[index].TaskDefinition
		definition.Execution.RunImmediately = true
		mutable.Nodes[index].TaskDefinition = &definition
		break
	}
	if _, err := normalizeWorkflowV2Revision(fixture.project, parent, mutable, 1, fixture.now); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("mutable node execution error = %v", err)
	}
}

func TestWorkflowV2StoreAtomicallyCreatesAndPublishesImmutableRevisions(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, storedRevision, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "queued" || created.Revision != 1 || created.LogAvailable || created.LogState != taskLogStateNone ||
		created.LogGeneration != 0 || storedRevision.GraphDigest != revision.GraphDigest {
		t.Fatalf("created workflow = %+v, %+v", created, storedRevision)
	}
	hydrated, err := fixture.store.GetWorkflowRevision(t.Context(), revision.ID)
	if err != nil || hydrated.GraphDigest != revision.GraphDigest || len(hydrated.Nodes) != len(revision.Nodes) || len(hydrated.Edges) != len(revision.Edges) {
		t.Fatalf("hydrated revision = %+v, %v", hydrated, err)
	}
	tasks, err := fixture.store.List(t.Context(), fixture.project.ID)
	if err != nil || len(tasks) != 3 {
		t.Fatalf("workflow task records = %+v, %v", tasks, err)
	}
	for _, task := range tasks {
		if task.Definition.ID == parent.ID {
			continue
		}
		if task.Definition.Scope != "workflowNode" || task.Definition.OwnerWorkflowTaskID == nil || *task.Definition.OwnerWorkflowTaskID != parent.ID ||
			task.Definition.Execution.WorkflowID == nil || *task.Definition.Execution.WorkflowID != parent.ID || task.Definition.Execution.Mode != "parallel" {
			t.Fatalf("workflow child definition = %+v", task)
		}
	}
	if _, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now.Add(time.Second)); !errors.Is(err, errRPCRevision) {
		t.Fatalf("duplicate atomic create error = %v", err)
	}

	updatedParent, second := newWorkflowV2TestGraph(t, fixture, parent.ID, 2, fixture.now.Add(time.Minute))
	published, publishedRevision, err := fixture.store.PublishWorkflowRevision(
		t.Context(), updatedParent, created.Revision, second, fixture.now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	currentRevisionID, currentVersion, err := workflowV2DefinitionRevision(published.Definition.Config)
	if err != nil || currentRevisionID != second.ID || currentVersion != 2 || published.DefinitionRevision != 2 || published.Revision != 2 {
		t.Fatalf("published workflow = %+v, %v", published, err)
	}
	if publishedRevision.GraphDigest != second.GraphDigest {
		t.Fatalf("published revision = %+v", publishedRevision)
	}
	revisions, err := fixture.store.ListWorkflowRevisions(t.Context(), parent.ID)
	if err != nil || len(revisions) != 2 || revisions[0].Version != 2 || revisions[1].Version != 1 ||
		!slices.EqualFunc(revisions[1].Edges, revision.Edges, func(left, right workflowV2Edge) bool { return left == right }) {
		t.Fatalf("workflow revisions = %+v, %v", revisions, err)
	}
	oldAgain, err := fixture.store.GetWorkflowRevision(t.Context(), revision.ID)
	if err != nil || oldAgain.GraphDigest != revision.GraphDigest {
		t.Fatalf("old revision mutated = %+v, %v", oldAgain, err)
	}
}

func TestWorkflowV2RunPinsRevisionWhileNewRevisionPublishes(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, first := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, first, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	running, run, pinned, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != "running" || run.WorkflowRevisionID == nil || *run.WorkflowRevisionID != first.ID || pinned.ID != first.ID {
		t.Fatalf("started workflow = %+v, %+v, %+v", running, run, pinned)
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || snapshot.Revision.ID != first.ID || len(snapshot.NodeRuns) != len(first.Nodes) {
		t.Fatalf("workflow snapshot = %+v, %v", snapshot, err)
	}
	for _, node := range first.Nodes {
		index := slices.IndexFunc(snapshot.NodeRuns, func(run workflowV2NodeRun) bool { return run.NodeID == node.ID })
		if index < 0 {
			t.Fatalf("missing node run %q", node.ID)
		}
		want := "pending"
		if node.Type == "start" {
			want = "succeeded"
		}
		if snapshot.NodeRuns[index].Status != want || snapshot.NodeRuns[index].Attempt != 0 {
			t.Fatalf("node run = %+v, want %s", snapshot.NodeRuns[index], want)
		}
	}

	updatedParent, second := newWorkflowV2TestGraph(t, fixture, parent.ID, 2, fixture.now.Add(time.Minute))
	published, _, err := fixture.store.PublishWorkflowRevision(t.Context(), updatedParent, running.Revision, second, fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if published.Status != "running" || published.CurrentRunID == nil || *published.CurrentRunID != run.ID {
		t.Fatalf("publishing changed active run = %+v", published)
	}
	pinnedAgain, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, &run.ID)
	if err != nil || pinnedAgain.Revision.ID != first.ID || pinnedAgain.TaskRun.WorkflowRevisionID == nil || *pinnedAgain.TaskRun.WorkflowRevisionID != first.ID {
		t.Fatalf("active run was not pinned = %+v, %v", pinnedAgain, err)
	}
}

func TestWorkflowV2TickClaimsPinnedChildRunAndCompletesSuccessBranch(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	running, _, _, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	firstTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(3*time.Second))
	if err != nil || firstTick.ScheduledCount != 1 || firstTick.Finished || firstTick.Task.Revision <= running.Revision {
		t.Fatalf("first workflow tick = %+v, %v", firstTick, err)
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 1 {
		t.Fatalf("scheduled workflow snapshot = %+v, %v", snapshot, err)
	}
	child := snapshot.ChildTasks[0]
	executeRunIndex := slices.IndexFunc(snapshot.NodeRuns, func(run workflowV2NodeRun) bool { return run.NodeID == "execute" })
	if executeRunIndex < 0 || snapshot.NodeRuns[executeRunIndex].Status != "running" || snapshot.NodeRuns[executeRunIndex].ChildTaskRunID == nil ||
		child.CurrentRunID == nil || *child.CurrentRunID != *snapshot.NodeRuns[executeRunIndex].ChildTaskRunID || child.Status != "waiting" {
		t.Fatalf("claimed workflow child = %+v, %+v", child, snapshot.NodeRuns)
	}
	claimedRunID := *child.CurrentRunID
	childRunning, childRun, err := fixture.store.StartRun(t.Context(), child.Definition.ID, child.Revision, fixture.now.Add(4*time.Second))
	if err != nil || childRun.ID != claimedRunID || childRun.ParentWorkflowTaskRunID == nil || childRun.WorkflowNodeID != "execute" {
		t.Fatalf("started workflow child = %+v, %+v, %v", childRunning, childRun, err)
	}
	childAwaiting, _, err := fixture.store.FinishRun(t.Context(), child.Definition.ID, childRunning.Revision, "awaitingAcceptance", 0,
		"execution_succeeded", "workflow-child-session", fixture.now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.Accept(t.Context(), child.Definition.ID, childAwaiting.Revision, []byte("child accepted"), fixture.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	completedTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(7*time.Second))
	if err != nil || !completedTick.Finished || completedTick.Task.Status != "awaitingAcceptance" || completedTick.Task.ResultCode != "workflow_succeeded" {
		t.Fatalf("completed workflow tick = %+v, %v", completedTick, err)
	}
	completedSnapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, nodeRun := range completedSnapshot.NodeRuns {
		statuses[nodeRun.NodeID] = nodeRun.Status
	}
	if statuses["start"] != "succeeded" || statuses["execute"] != "succeeded" || statuses["recover"] != "skipped" || statuses["finish"] != "succeeded" {
		t.Fatalf("completed node statuses = %#v", statuses)
	}
}

func TestWorkflowV2TickRoutesFailureEdgeWhenContinuationAllowed(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	revision.FailurePolicy, revision.GraphDigest, revision.CreatedAt = "continueOnFailure", "", time.Time{}
	var err error
	revision, err = normalizeWorkflowV2Revision(fixture.project, parent, revision, 1, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if tick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(3*time.Second)); err != nil || tick.ScheduledCount != 1 {
		t.Fatalf("initial tick = %+v, %v", tick, err)
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 1 {
		t.Fatalf("initial snapshot = %+v, %v", snapshot, err)
	}
	execute := snapshot.ChildTasks[0]
	executeRunning, _, err := fixture.store.StartRun(t.Context(), execute.Definition.ID, execute.Revision, fixture.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.FinishRun(t.Context(), execute.Definition.ID, executeRunning.Revision, "failed", 9,
		"runner_exit", "", fixture.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	failureTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(6*time.Second))
	if err != nil || failureTick.ScheduledCount != 1 || failureTick.Finished {
		t.Fatalf("failure route tick = %+v, %v", failureTick, err)
	}
	snapshot, err = fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 2 {
		t.Fatalf("recovery snapshot = %+v, %v", snapshot, err)
	}
	var recovery taskV2Record
	for _, child := range snapshot.ChildTasks {
		if child.Definition.Title == "Recover" {
			recovery = child
		}
	}
	if recovery.Definition.ID == uuid.Nil || recovery.Status != "waiting" {
		t.Fatalf("recovery child = %+v", recovery)
	}
	recoveryRunning, _, err := fixture.store.StartRun(t.Context(), recovery.Definition.ID, recovery.Revision, fixture.now.Add(7*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	recoveryAwaiting, _, err := fixture.store.FinishRun(t.Context(), recovery.Definition.ID, recoveryRunning.Revision,
		"awaitingAcceptance", 0, "execution_succeeded", "", fixture.now.Add(8*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.Accept(t.Context(), recovery.Definition.ID, recoveryAwaiting.Revision,
		[]byte("recovery accepted"), fixture.now.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(10*time.Second))
	if err != nil || !completed.Finished || completed.Task.Status != "awaitingAcceptance" ||
		completed.Task.ResultCode != "workflow_completed_with_failures" {
		t.Fatalf("continued workflow result = %+v, %v", completed, err)
	}
}

func TestWorkflowV2StopOnFailureBlocksAndRetryResumesPinnedRun(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, parentRun, _, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if tick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(3*time.Second)); err != nil || tick.ScheduledCount != 1 {
		t.Fatalf("initial tick = %+v, %v", tick, err)
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 1 {
		t.Fatalf("initial snapshot = %+v, %v", snapshot, err)
	}
	execute := snapshot.ChildTasks[0]
	executeRunning, firstChildRun, err := fixture.store.StartRun(t.Context(), execute.Definition.ID, execute.Revision, fixture.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.FinishRun(t.Context(), execute.Definition.ID, executeRunning.Revision, "failed", 3,
		"runner_exit", "", fixture.now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	failedTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(6*time.Second))
	if err != nil || !failedTick.Finished || failedTick.Task.Status != "failed" || failedTick.Task.ResultCode != "workflow_node_failed" {
		t.Fatalf("failed workflow tick = %+v, %v", failedTick, err)
	}
	failedSnapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, run := range failedSnapshot.NodeRuns {
		statuses[run.NodeID] = run.Status
	}
	if statuses["execute"] != "failed" || statuses["recover"] != "blocked" || statuses["finish"] != "blocked" {
		t.Fatalf("failed node statuses = %#v", statuses)
	}
	retried, err := fixture.store.RetryWorkflowNode(t.Context(), parent.ID, failedTick.Task.Revision, "execute", fixture.now.Add(7*time.Second))
	if err != nil || !retried.Resumed || retried.Task.Status != "running" || retried.TaskRun.ID != parentRun.ID ||
		retried.TaskRun.WorkflowRevisionID == nil || *retried.TaskRun.WorkflowRevisionID != revision.ID || retried.NodeRun.Attempt != 1 {
		t.Fatalf("retried workflow node = %+v, %v", retried, err)
	}
	if tick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(8*time.Second)); err != nil || tick.ScheduledCount != 1 {
		t.Fatalf("retry scheduling tick = %+v, %v", tick, err)
	}
	retrySnapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	var retriedChild taskV2Record
	for _, child := range retrySnapshot.ChildTasks {
		if child.Definition.ID == execute.Definition.ID {
			retriedChild = child
		}
	}
	if retriedChild.CurrentRunID == nil || *retriedChild.CurrentRunID == firstChildRun.ID || retriedChild.Status != "waiting" {
		t.Fatalf("retried child task = %+v", retriedChild)
	}
	secondRunning, secondChildRun, err := fixture.store.StartRun(t.Context(), retriedChild.Definition.ID, retriedChild.Revision, fixture.now.Add(9*time.Second))
	if err != nil || secondChildRun.ID != *retriedChild.CurrentRunID || secondChildRun.Attempt != firstChildRun.Attempt+1 {
		t.Fatalf("second child run = %+v, %v", secondChildRun, err)
	}
	secondAwaiting, _, err := fixture.store.FinishRun(t.Context(), retriedChild.Definition.ID, secondRunning.Revision,
		"awaitingAcceptance", 0, "execution_succeeded", "", fixture.now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.Accept(t.Context(), retriedChild.Definition.ID, secondAwaiting.Revision,
		[]byte("retry accepted"), fixture.now.Add(11*time.Second)); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(12*time.Second))
	if err != nil || !completed.Finished || completed.Task.Status != "awaitingAcceptance" || completed.Task.ResultCode != "workflow_succeeded" {
		t.Fatalf("retried workflow completion = %+v, %v", completed, err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var childRunCount, nodeAttemptCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_runs WHERE task_id = ?`, execute.Definition.ID.String()).Scan(&childRunCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_node_runs WHERE workflow_task_run_id = ? AND node_id = 'execute'`,
		parentRun.ID.String()).Scan(&nodeAttemptCount); err != nil {
		t.Fatal(err)
	}
	if childRunCount != 2 || nodeAttemptCount != 2 {
		t.Fatalf("retry history child-runs=%d node-attempts=%d", childRunCount, nodeAttemptCount)
	}
}

func TestWorkflowV2CancellationSealsGraphBeforeRunningChildStops(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if tick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(3*time.Second)); err != nil || tick.ScheduledCount != 1 {
		t.Fatalf("initial tick = %+v, %v", tick, err)
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 1 {
		t.Fatalf("initial snapshot = %+v, %v", snapshot, err)
	}
	child := snapshot.ChildTasks[0]
	childRunning, childRun, err := fixture.store.StartRun(t.Context(), child.Definition.ID, child.Revision, fixture.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	parentBeforeCancel, err := fixture.store.Get(t.Context(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := fixture.store.CancelWorkflow(t.Context(), parent.ID, parentBeforeCancel.Revision, fixture.now.Add(5*time.Second))
	if err != nil || cancelled.Task.Status != "cancelled" || cancelled.Task.ResultCode != "cancelled" ||
		len(cancelled.RunningChildren) != 1 || cancelled.RunningChildren[0].Definition.ID != child.Definition.ID {
		t.Fatalf("workflow cancellation = %+v, %v", cancelled, err)
	}
	sealedTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(6*time.Second))
	if err != nil || sealedTick.ScheduledCount != 0 || sealedTick.Finished || sealedTick.Task.Status != "cancelled" {
		t.Fatalf("sealed workflow tick = %+v, %v", sealedTick, err)
	}
	if _, _, err := fixture.store.FinishRun(t.Context(), child.Definition.ID, childRunning.Revision, "cancelled", 130,
		"cancelled", "", fixture.now.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	finalized, err := fixture.store.FinalizeCancelledWorkflow(t.Context(), parent.ID, cancelled.Task.Revision, fixture.now.Add(8*time.Second))
	if err != nil || finalized.Status != "cancelled" || finalized.Revision != cancelled.Task.Revision+1 {
		t.Fatalf("finalized workflow = %+v, %v", finalized, err)
	}
	finalSnapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, run := range finalSnapshot.NodeRuns {
		statuses[run.NodeID] = run.Status
	}
	if statuses["start"] != "succeeded" || statuses["execute"] != "cancelled" || statuses["recover"] != "cancelled" || statuses["finish"] != "cancelled" {
		t.Fatalf("cancelled node statuses = %#v (child run %s)", statuses, childRun.ID)
	}
}

func TestWorkflowV2RecoveryKeepsPinnedParentAndFailsInterruptedChild(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runningParent, parentRun, _, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if tick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(3*time.Second)); err != nil || tick.ScheduledCount != 1 {
		t.Fatalf("initial tick = %+v, %v", tick, err)
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 1 {
		t.Fatalf("initial snapshot = %+v, %v", snapshot, err)
	}
	child := snapshot.ChildTasks[0]
	childRunning, childRun, err := fixture.store.StartRun(t.Context(), child.Definition.ID, child.Revision, fixture.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := fixture.store.RecoverInterrupted(t.Context(), fixture.now.Add(5*time.Second))
	if err != nil || recovered != 1 {
		t.Fatalf("recovered interrupted workflow tasks = %d, %v", recovered, err)
	}
	parentAfterRecovery, err := fixture.store.Get(t.Context(), parent.ID)
	if err != nil || parentAfterRecovery.Status != "running" || parentAfterRecovery.Revision != runningParent.Revision+1 ||
		parentAfterRecovery.CurrentRunID == nil || *parentAfterRecovery.CurrentRunID != parentRun.ID {
		// Tick scheduled the child and advanced the parent once after StartWorkflowRun;
		// recovery itself must not advance it again.
		t.Fatalf("recovered parent = %+v, %v (started %+v)", parentAfterRecovery, err, runningParent)
	}
	childAfterRecovery, err := fixture.store.Get(t.Context(), child.Definition.ID)
	if err != nil || childAfterRecovery.Status != "failed" || childAfterRecovery.ResultCode != "agent_restarted" ||
		childAfterRecovery.Revision != childRunning.Revision+1 {
		t.Fatalf("recovered child = %+v, %v", childAfterRecovery, err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	var parentRunStatus, pinnedRevisionID, childRunStatus, childResultCode string
	if err := db.QueryRow(`SELECT status, workflow_revision_id FROM task_runs WHERE id = ?`, parentRun.ID.String()).Scan(&parentRunStatus, &pinnedRevisionID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status, result_code FROM task_runs WHERE id = ?`, childRun.ID.String()).Scan(&childRunStatus, &childResultCode); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if parentRunStatus != "running" || pinnedRevisionID != revision.ID.String() || childRunStatus != "failed" || childResultCode != "agent_restarted" {
		t.Fatalf("recovered runs parent=%q pinned=%q child=%q result=%q", parentRunStatus, pinnedRevisionID, childRunStatus, childResultCode)
	}
	failedTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(6*time.Second))
	if err != nil || !failedTick.Finished || failedTick.Task.Status != "failed" || failedTick.Task.ResultCode != "workflow_node_failed" {
		t.Fatalf("post-recovery tick = %+v, %v", failedTick, err)
	}
}

func TestWorkflowV2DeleteRemovesAllRevisionOwnedNodeDefinitions(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, first := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, first, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	secondParent, second := newWorkflowV2TestGraph(t, fixture, parent.ID, 2, fixture.now.Add(time.Second))
	published, _, err := fixture.store.PublishWorkflowRevision(t.Context(), secondParent, created.Revision, second, fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := fixture.store.Transition(t.Context(), parent.ID, published.Revision, "cancelled", "cancelled", fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Delete(t.Context(), parent.ID, cancelled.Revision, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"tasks", "task_runs", "task_logs", "workflow_revisions", "workflow_nodes", "workflow_edges", "workflow_runs", "workflow_node_runs"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s retained %d workflow rows", table, count)
		}
	}
	var deletedChanges int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_changes WHERE operation = 'delete'`).Scan(&deletedChanges); err != nil {
		t.Fatal(err)
	}
	if deletedChanges != 5 {
		t.Fatalf("workflow delete changes = %d, want parent plus four revision-owned children", deletedChanges)
	}
}

func TestWorkflowV2ClearFinishedCountsParentAndCleansInternalNodes(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parent, revision := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, fixture.now)
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "cancelled", "cancelled", fixture.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	cleared, err := fixture.store.ClearFinished(t.Context(), fixture.project.ID, nil, fixture.now.Add(2*time.Second))
	if err != nil || cleared.AffectedCount != 1 {
		t.Fatalf("cleared workflow = %+v, %v", cleared, err)
	}
	tasks, err := fixture.store.List(t.Context(), fixture.project.ID)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks after workflow clear = %+v, %v", tasks, err)
	}
}

func TestWorkflowV2RevisionParallelismLimitsReadyBranches(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	parentID, firstID, secondID := uuid.New(), uuid.New(), uuid.New()
	parent := taskV2Definition{
		ID: parentID, ProjectID: fixture.project.ID, Kind: "workflow", Title: "Parallel workflow", CWD: ".", Scope: "topLevel",
		Config: json.RawMessage(`{}`), Execution: taskV2ExecutionOptions{Relation: "dependency", Mode: "serial", RunImmediately: true},
		Environment: map[string]string{},
	}
	revision, err := normalizeWorkflowV2Revision(fixture.project, parent, workflowV2Revision{
		ID: uuid.New(), WorkflowTaskID: parentID, Version: 1, FailurePolicy: "stopOnFailure", MaximumParallelism: 1,
		Nodes: []workflowV2Node{
			{ID: "start", Type: "start", Position: workflowV2Position{X: 0, Y: 100}},
			{ID: "first", Type: "task", TaskDefinitionID: &firstID, TaskDefinition: taskDefinitionPointer(workflowV2TestDefinition(firstID, "First")), Position: workflowV2Position{X: 200, Y: 50}},
			{ID: "second", Type: "task", TaskDefinitionID: &secondID, TaskDefinition: taskDefinitionPointer(workflowV2TestDefinition(secondID, "Second")), Position: workflowV2Position{X: 200, Y: 150}},
			{ID: "finish", Type: "finish", Position: workflowV2Position{X: 400, Y: 100}},
		},
		Edges: []workflowV2Edge{
			{ID: "start-first", SourceID: "start", TargetID: "first", Type: "onSuccess"},
			{ID: "start-second", SourceID: "start", TargetID: "second", Type: "onSuccess"},
			{ID: "first-finish", SourceID: "first", TargetID: "finish", Type: "onSuccess"},
			{ID: "second-finish", SourceID: "second", TargetID: "finish", Type: "onSuccess"},
		},
	}, 1, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err = bindWorkflowV2Definition(fixture.project, parent, revision)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := fixture.store.CreateWorkflow(t.Context(), parent, revision, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), parent.ID, created.Revision, "waiting", "", fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	firstTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(3*time.Second))
	if err != nil || firstTick.ScheduledCount != 1 {
		t.Fatalf("first parallel tick = %+v, %v", firstTick, err)
	}
	snapshot, err := fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 1 {
		t.Fatalf("first parallel snapshot = %+v, %v", snapshot, err)
	}
	firstChild := snapshot.ChildTasks[0]
	firstRunning, _, err := fixture.store.StartRun(t.Context(), firstChild.Definition.ID, firstChild.Revision, fixture.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	firstAwaiting, _, err := fixture.store.FinishRun(t.Context(), firstChild.Definition.ID, firstRunning.Revision,
		"awaitingAcceptance", 0, "execution_succeeded", "", fixture.now.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.Accept(t.Context(), firstChild.Definition.ID, firstAwaiting.Revision,
		[]byte("first accepted"), fixture.now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	secondTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, fixture.now.Add(7*time.Second))
	if err != nil || secondTick.ScheduledCount != 1 || secondTick.Finished {
		t.Fatalf("second parallel tick = %+v, %v", secondTick, err)
	}
	snapshot, err = fixture.store.GetWorkflowRunSnapshot(t.Context(), parent.ID, nil)
	if err != nil || len(snapshot.ChildTasks) != 2 {
		t.Fatalf("second parallel snapshot = %+v, %v", snapshot, err)
	}
	var secondChild taskV2Record
	for _, child := range snapshot.ChildTasks {
		if child.Definition.ID != firstChild.Definition.ID {
			secondChild = child
		}
	}
	if secondChild.Status != "waiting" {
		t.Fatalf("second branch = %+v", secondChild)
	}
}
