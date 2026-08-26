package main

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (d dispatcher) callWorkflowV2RPC(
	ctx context.Context,
	method string,
	store *taskV2Store,
	project registeredProject,
	input rpcInput,
) (any, uint64, error) {
	switch method {
	case "workflow.validate":
		if !taskV2InputHasOnly(input, "definition", "revision") {
			return nil, 0, errRPCInvalid
		}
		definition, revision, err := workflowV2DraftFromInput(project, input, 0, d.now())
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"valid": true, "definition": definition, "revision": revision}, revision.Version, nil
	case "workflow.create":
		if !taskV2InputHasOnly(input, "definition", "revision") {
			return nil, 0, errRPCInvalid
		}
		definition, revision, err := workflowV2DraftFromInput(project, input, 1, d.now())
		if err != nil {
			return nil, 0, err
		}
		task, storedRevision, err := store.CreateWorkflow(ctx, definition, revision, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		return map[string]any{"task": task, "revision": storedRevision}, task.Revision, nil
	case "workflow.get":
		if !taskV2InputHasOnly(input, "taskId") {
			return nil, 0, errRPCInvalid
		}
		task, err := workflowV2TaskForProject(ctx, store, project.ID, input)
		if err != nil {
			return nil, 0, err
		}
		revisionID, _, err := workflowV2DefinitionRevision(task.Definition.Config)
		if err != nil {
			return nil, 0, err
		}
		revision, err := store.GetWorkflowRevision(ctx, revisionID)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"task": task, "revision": revision}, task.Revision, nil
	case "workflow.revisions":
		if !taskV2InputHasOnly(input, "taskId", "cursor", "limit") {
			return nil, 0, errRPCInvalid
		}
		task, err := workflowV2TaskForProject(ctx, store, project.ID, input)
		if err != nil {
			return nil, 0, err
		}
		revisions, err := store.ListWorkflowRevisions(ctx, task.Definition.ID)
		if err != nil {
			return nil, 0, err
		}
		pageWatermark, err := rpcPageSnapshotWatermark(map[string]any{
			"method": "workflow.revisions", "taskId": task.Definition.ID,
			"taskRevision": task.Revision, "items": revisions,
		})
		if err != nil {
			return nil, 0, err
		}
		start, requestedEnd, _, err := versionedPageWindow(input, len(revisions), pageWatermark)
		if err != nil {
			return nil, 0, err
		}
		build := func(count int) any {
			end := start + count
			return map[string]any{
				"items": revisions[start:end], "taskRevision": task.Revision,
				"nextCursor":    versionedPageCursor(pageWatermark, end, len(revisions)),
				"highWatermark": pageWatermark,
			}
		}
		count, err := rpcPagePrefixLength(requestedEnd-start, build)
		if err != nil {
			return nil, 0, err
		}
		return build(count), task.Revision, nil
	case "workflow.revision.get":
		if !taskV2InputHasOnly(input, "revisionId") {
			return nil, 0, errRPCInvalid
		}
		revisionID, ok := inputUUID(input, "revisionId")
		if !ok {
			return nil, 0, errRPCInvalid
		}
		revision, err := store.GetWorkflowRevision(ctx, revisionID)
		if err != nil {
			return nil, 0, err
		}
		task, err := store.Get(ctx, revision.WorkflowTaskID)
		if err != nil {
			return nil, 0, err
		}
		if task.Definition.ProjectID != project.ID || task.Definition.Kind != "workflow" {
			return nil, 0, errRPCProject
		}
		return revision, task.Revision, nil
	case "workflow.revision.publish":
		if !taskV2InputHasOnly(input, "definition", "revision", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		expectedRevision, ok := taskV2ExpectedRevision(input, "expectedRevision")
		if !ok {
			return nil, 0, errRPCInvalid
		}
		rawDefinition, err := taskV2DefinitionFromInput(project, input)
		if err != nil || rawDefinition.Kind != "workflow" || rawDefinition.Scope != "topLevel" {
			return nil, 0, errRPCInvalid
		}
		current, err := store.Get(ctx, rawDefinition.ID)
		if err != nil {
			return nil, 0, err
		}
		if current.Definition.ProjectID != project.ID {
			return nil, 0, errRPCProject
		}
		if current.Revision != expectedRevision || current.Definition.Kind != "workflow" {
			return nil, 0, errRPCRevision
		}
		_, currentVersion, err := workflowV2DefinitionRevision(current.Definition.Config)
		if err != nil || currentVersion == ^uint64(0) {
			return nil, 0, errRPCRevision
		}
		definition, revision, err := workflowV2DraftFromInput(project, input, currentVersion+1, d.now())
		if err != nil {
			return nil, 0, err
		}
		published, storedRevision, err := store.PublishWorkflowRevision(ctx, definition, expectedRevision, revision, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		return map[string]any{"task": published, "revision": storedRevision}, published.Revision, nil
	case "workflow.run.get":
		if !taskV2InputHasOnly(input, "taskId", "runId") {
			return nil, 0, errRPCInvalid
		}
		task, err := workflowV2TaskForProject(ctx, store, project.ID, input)
		if err != nil {
			return nil, 0, err
		}
		var runID *uuid.UUID
		if raw, found := input["runId"]; found && raw != nil {
			text, ok := raw.(string)
			parsed, parseErr := uuid.Parse(strings.TrimSpace(text))
			if !ok || parseErr != nil || parsed == uuid.Nil {
				return nil, 0, errRPCInvalid
			}
			runID = &parsed
		}
		snapshot, err := store.GetWorkflowRunSnapshot(ctx, task.Definition.ID, runID)
		if err != nil {
			return nil, 0, err
		}
		return snapshot, snapshot.Task.Revision, nil
	case "workflow.node.retry":
		if !taskV2InputHasOnly(input, "taskId", "nodeId", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		task, err := workflowV2TaskForProject(ctx, store, project.ID, input)
		if err != nil {
			return nil, 0, err
		}
		expectedRevision, revisionOK := taskV2ExpectedRevision(input, "expectedRevision")
		nodeID, nodeOK := inputString(input, "nodeId", maximumWorkflowV2Identifier)
		if !revisionOK || !nodeOK {
			return nil, 0, errRPCInvalid
		}
		if expectedRevision != task.Revision {
			return nil, 0, errRPCRevision
		}
		result, err := store.RetryWorkflowNode(ctx, task.Definition.ID, expectedRevision, nodeID, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		return result, result.Task.Revision, nil
	default:
		return nil, 0, errRPCInvalid
	}
}

func workflowV2DraftFromInput(
	project registeredProject,
	input rpcInput,
	expectedVersion uint64,
	now time.Time,
) (taskV2Definition, workflowV2Revision, error) {
	definition, err := taskV2DefinitionFromInput(project, input)
	if err != nil || definition.Kind != "workflow" || definition.Scope != "topLevel" || definition.OwnerWorkflowTaskID != nil {
		return taskV2Definition{}, workflowV2Revision{}, errRPCInvalid
	}
	revision, err := decodeWorkflowV2Revision(input["revision"])
	if err != nil {
		return taskV2Definition{}, workflowV2Revision{}, err
	}
	if expectedVersion == 0 {
		expectedVersion = revision.Version
	}
	normalizedRevision, err := normalizeWorkflowV2Revision(project, definition, revision, expectedVersion, now)
	if err != nil {
		return taskV2Definition{}, workflowV2Revision{}, err
	}
	bound, err := bindWorkflowV2Definition(project, definition, normalizedRevision)
	if err != nil {
		return taskV2Definition{}, workflowV2Revision{}, err
	}
	return bound, normalizedRevision, nil
}

func workflowV2TaskForProject(
	ctx context.Context,
	store *taskV2Store,
	projectID uuid.UUID,
	input rpcInput,
) (taskV2Record, error) {
	task, err := taskV2RecordForProject(ctx, store, projectID, input)
	if err != nil {
		return taskV2Record{}, err
	}
	if task.Definition.Kind != "workflow" || task.Definition.Scope != "topLevel" {
		return taskV2Record{}, errRPCInvalid
	}
	return task, nil
}
