package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestWorkflowV2PeerRPCCoversDraftRevisionRunAndNodeRetry(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	parent, first := newWorkflowV2TestGraph(t, fixture, uuid.New(), 1, time.Now().UTC())
	validated := dispatchTaskV2(t, dispatch, "workflow.validate", map[string]any{
		"definition": parent, "revision": first,
	}, fixture.project.ID.String())
	if validated.GetError() != nil || !bytes.Contains(validated.GetJsonPayload(), []byte(`"valid":true`)) ||
		!bytes.Contains(validated.GetJsonPayload(), []byte(first.GraphDigest)) {
		t.Fatalf("workflow validation = %s, %+v", validated.GetJsonPayload(), validated.GetError())
	}
	createdResponse := dispatchTaskV2(t, dispatch, "workflow.create", map[string]any{
		"definition": parent, "revision": first,
	}, fixture.project.ID.String())
	if createdResponse.GetError() != nil {
		t.Fatalf("workflow create error = %+v", createdResponse.GetError())
	}
	var created struct {
		Task     taskV2Record       `json:"task"`
		Revision workflowV2Revision `json:"revision"`
	}
	if err := json.Unmarshal(createdResponse.GetJsonPayload(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Task.Definition.ID != parent.ID || created.Revision.ID != first.ID || created.Task.Status != "queued" {
		t.Fatalf("created workflow = %+v", created)
	}

	getResponse := dispatchTaskV2(t, dispatch, "workflow.get", map[string]any{"taskId": parent.ID}, fixture.project.ID.String())
	revisionsResponse := dispatchTaskV2(t, dispatch, "workflow.revisions", map[string]any{"taskId": parent.ID}, fixture.project.ID.String())
	revisionResponse := dispatchTaskV2(t, dispatch, "workflow.revision.get", map[string]any{"revisionId": first.ID}, fixture.project.ID.String())
	if getResponse.GetError() != nil || revisionsResponse.GetError() != nil || revisionResponse.GetError() != nil ||
		!bytes.Contains(getResponse.GetJsonPayload(), []byte(first.ID.String())) ||
		!bytes.Contains(revisionsResponse.GetJsonPayload(), []byte(first.GraphDigest)) ||
		!bytes.Contains(revisionResponse.GetJsonPayload(), []byte(`"version":1`)) {
		t.Fatalf("workflow reads get=%s revisions=%s revision=%s", getResponse.GetJsonPayload(), revisionsResponse.GetJsonPayload(), revisionResponse.GetJsonPayload())
	}

	genericUpdate := dispatchTaskV2(t, dispatch, "task.update", map[string]any{
		"definition": created.Task.Definition, "expectedRevision": created.Task.Revision,
	}, fixture.project.ID.String())
	if genericUpdate.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("generic workflow update error = %+v", genericUpdate.GetError())
	}
	startResponse := dispatchTaskV2(t, dispatch, "task.start", map[string]any{
		"taskId": parent.ID, "expectedRevision": created.Task.Revision,
	}, fixture.project.ID.String())
	if startResponse.GetError() != nil {
		t.Fatalf("workflow start error = %+v", startResponse.GetError())
	}
	var waiting taskV2Record
	if err := json.Unmarshal(startResponse.GetJsonPayload(), &waiting); err != nil {
		t.Fatal(err)
	}
	runningParent, parentRun, _, err := fixture.store.StartWorkflowRun(t.Context(), parent.ID, waiting.Revision, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	secondParent, second := newWorkflowV2TestGraph(t, fixture, parent.ID, 2, time.Now().UTC().Add(time.Second))
	publishResponse := dispatchTaskV2(t, dispatch, "workflow.revision.publish", map[string]any{
		"definition": secondParent, "revision": second, "expectedRevision": runningParent.Revision,
	}, fixture.project.ID.String())
	if publishResponse.GetError() != nil {
		t.Fatalf("workflow publish error = %+v", publishResponse.GetError())
	}
	var published struct {
		Task     taskV2Record       `json:"task"`
		Revision workflowV2Revision `json:"revision"`
	}
	if err := json.Unmarshal(publishResponse.GetJsonPayload(), &published); err != nil {
		t.Fatal(err)
	}
	if published.Revision.ID != second.ID || published.Task.Status != "running" || published.Task.CurrentRunID == nil || *published.Task.CurrentRunID != parentRun.ID {
		t.Fatalf("published workflow = %+v", published)
	}
	if tick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, time.Now().UTC().Add(2*time.Second)); err != nil || tick.ScheduledCount != 1 {
		t.Fatalf("workflow tick = %+v, %v", tick, err)
	}
	pinnedResponse := dispatchTaskV2(t, dispatch, "workflow.run.get", map[string]any{
		"taskId": parent.ID, "runId": parentRun.ID,
	}, fixture.project.ID.String())
	if pinnedResponse.GetError() != nil {
		t.Fatalf("workflow run get error = %+v", pinnedResponse.GetError())
	}
	var pinned workflowV2RunSnapshot
	if err := json.Unmarshal(pinnedResponse.GetJsonPayload(), &pinned); err != nil {
		t.Fatal(err)
	}
	if pinned.Revision.ID != first.ID || len(pinned.ChildTasks) != 1 {
		t.Fatalf("pinned workflow snapshot = %+v", pinned)
	}
	child := pinned.ChildTasks[0]
	genericChildStart := dispatchTaskV2(t, dispatch, "task.start", map[string]any{
		"taskId": child.Definition.ID, "expectedRevision": child.Revision,
	}, fixture.project.ID.String())
	if genericChildStart.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("generic child start error = %+v", genericChildStart.GetError())
	}
	childRunning, _, err := fixture.store.StartRun(t.Context(), child.Definition.ID, child.Revision, time.Now().UTC().Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.store.FinishRun(t.Context(), child.Definition.ID, childRunning.Revision, "failed", 2,
		"runner_exit", "", time.Now().UTC().Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	failedTick, err := fixture.store.TickWorkflow(t.Context(), parent.ID, 4, time.Now().UTC().Add(5*time.Second))
	if err != nil || failedTick.Task.Status != "failed" {
		t.Fatalf("failed workflow tick = %+v, %v", failedTick, err)
	}
	retryResponse := dispatchTaskV2(t, dispatch, "workflow.node.retry", map[string]any{
		"taskId": parent.ID, "nodeId": "execute", "expectedRevision": failedTick.Task.Revision,
	}, fixture.project.ID.String())
	if retryResponse.GetError() != nil {
		t.Fatalf("workflow node retry error = %+v", retryResponse.GetError())
	}
	var retried workflowV2NodeRetryResult
	if err := json.Unmarshal(retryResponse.GetJsonPayload(), &retried); err != nil {
		t.Fatal(err)
	}
	if !retried.Resumed || retried.Task.Status != "running" || retried.NodeRun.Attempt != 1 || retried.TaskRun.ID != parentRun.ID ||
		retried.TaskRun.WorkflowRevisionID == nil || *retried.TaskRun.WorkflowRevisionID != first.ID {
		t.Fatalf("workflow node retry = %+v", retried)
	}
}

func TestWorkflowV2MethodsRequirePeerTaskControlScope(t *testing.T) {
	methods := []string{
		"workflow.validate", "workflow.create", "workflow.get", "workflow.revisions", "workflow.revision.get",
		"workflow.revision.publish", "workflow.run.get", "workflow.node.retry",
	}
	for _, method := range methods {
		if methodScope(method) != "remote.peer.task.control" || methodAllowsScope(method, "remote.task.read") ||
			methodAllowsScope(method, "remote.task.write") || maximumRPCPayloadForMethod(method) != maximumRPCPayload {
			t.Errorf("workflow method %q has unsafe routing", method)
		}
	}
}
