package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type taskV2LogRPCView struct {
	TaskID         uuid.UUID  `json:"taskId"`
	RunID          *uuid.UUID `json:"runId,omitempty"`
	Sequence       uint64     `json:"sequence"`
	Stream         string     `json:"stream"`
	Content        string     `json:"content,omitempty"`
	ContentBase64  string     `json:"contentBase64,omitempty"`
	Encoding       string     `json:"encoding"`
	SourceEncoding string     `json:"sourceEncoding,omitempty"`
	RawAvailable   bool       `json:"rawAvailable"`
	DecodeWarning  string     `json:"decodeWarning,omitempty"`
	OccurredAt     time.Time  `json:"occurredAt"`
}

func (d dispatcher) callTaskV2RPC(ctx context.Context, method string, input rpcInput) (any, uint64, error) {
	if method == "task.list" {
		// Keep the full read path (project policy, capability check and SQLite
		// snapshot) inside the LAN list budget. A stale page can be retried; a
		// five-second SQLite wait is never a useful task-list response.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, localListReadBudget)
		defer cancel()
	}
	store, project, err := d.boundTaskV2Store(ctx)
	if err != nil {
		return nil, 0, err
	}
	if strings.HasPrefix(method, "workflow.") {
		if !agentFeatureFlags(d.state)["workflow.v2"] {
			return nil, 0, errRPCCapability
		}
		return d.callWorkflowV2RPC(ctx, method, store, project, input)
	}
	switch method {
	case "task.list":
		return d.callTaskV2List(ctx, store, project, input)
	case "task.get":
		if !taskV2InputHasOnly(input, "taskId") {
			return nil, 0, errRPCInvalid
		}
		task, err := taskV2RecordForProject(ctx, store, project.ID, input)
		if err != nil {
			return nil, 0, err
		}
		return task, task.Revision, nil
	case "task.runs":
		if !taskV2InputHasOnly(input, "taskId", "cursor", "limit") {
			return nil, 0, errRPCInvalid
		}
		task, err := taskV2RecordForProject(ctx, store, project.ID, input)
		if err != nil {
			return nil, 0, err
		}
		runs, err := store.ListRuns(ctx, task.Definition.ID)
		if err != nil {
			return nil, 0, err
		}
		pageWatermark, err := rpcPageSnapshotWatermark(map[string]any{
			"method": "task.runs", "taskId": task.Definition.ID,
			"taskRevision": task.Revision, "items": runs,
		})
		if err != nil {
			return nil, 0, err
		}
		start, requestedEnd, _, err := versionedPageWindow(input, len(runs), pageWatermark)
		if err != nil {
			return nil, 0, err
		}
		build := func(count int) any {
			end := start + count
			return map[string]any{
				"items": runs[start:end], "taskRevision": task.Revision,
				"nextCursor":    versionedPageCursor(pageWatermark, end, len(runs)),
				"highWatermark": pageWatermark,
			}
		}
		count, err := rpcPagePrefixLength(requestedEnd-start, build)
		if err != nil {
			return nil, 0, err
		}
		return build(count), task.Revision, nil
	case "task.create":
		if !taskV2InputHasOnly(input, "definition") {
			return nil, 0, errRPCInvalid
		}
		definition, err := taskV2DefinitionFromInput(project, input)
		if err != nil {
			return nil, 0, err
		}
		if definition.Kind == "workflow" || definition.Scope != "topLevel" {
			return nil, 0, errRPCInvalid
		}
		task, err := store.Create(ctx, definition, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		return task, task.Revision, nil
	case "task.update":
		if !taskV2InputHasOnly(input, "definition", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		definition, err := taskV2DefinitionFromInput(project, input)
		if err != nil {
			return nil, 0, err
		}
		expectedRevision, ok := taskV2ExpectedRevision(input, "expectedRevision")
		if !ok {
			return nil, 0, errRPCInvalid
		}
		current, err := store.Get(ctx, definition.ID)
		if err != nil {
			return nil, 0, err
		}
		if current.Definition.ProjectID != project.ID {
			return nil, 0, errRPCProject
		}
		if current.Definition.Kind == "workflow" || current.Definition.Scope != "topLevel" ||
			definition.Kind == "workflow" || definition.Scope != "topLevel" {
			return nil, 0, errRPCInvalid
		}
		task, err := store.UpdateDefinition(ctx, definition, expectedRevision, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		return task, task.Revision, nil
	case "task.start":
		if !taskV2InputHasOnly(input, "taskId", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		return d.callTaskV2StartNow(ctx, store, project.ID, input)
	case "task.cancel", "task.stop":
		if !taskV2InputHasOnly(input, "taskId", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		return d.callTaskV2Stop(ctx, store, project.ID, input)
	case "task.retry":
		if !taskV2InputHasOnly(input, "taskId", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		result, revision, err := transitionTaskV2FromRPC(ctx, store, project.ID, input, "waiting", "", d.now())
		if err == nil {
			d.state.wakeTaskEngine()
		}
		return result, revision, err
	case "task.undo-acceptance":
		if !taskV2InputHasOnly(input, "taskId", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		result, revision, err := transitionTaskV2FromRPC(ctx, store, project.ID, input, "awaitingAcceptance", "acceptance_undone", d.now())
		if err == nil {
			d.state.wakeTaskEngine()
		}
		return result, revision, err
	case "task.accept":
		result, revision, err := d.callTaskV2Accept(ctx, store, project.ID, input)
		if err == nil {
			d.state.wakeTaskEngine()
		}
		return result, revision, err
	case "task.follow-up":
		result, revision, err := d.callTaskV2FollowUp(ctx, store, project, input)
		if err == nil {
			d.state.wakeTaskEngine()
		}
		return result, revision, err
	case "task.delete":
		if !taskV2InputHasOnly(input, "taskId", "expectedRevision") {
			return nil, 0, errRPCInvalid
		}
		task, err := taskV2RecordForProject(ctx, store, project.ID, input)
		if err != nil {
			return nil, 0, err
		}
		if task.Definition.Scope == "workflowNode" {
			return nil, 0, errRPCInvalid
		}
		expectedRevision, ok := taskV2ExpectedRevision(input, "expectedRevision")
		if !ok || expectedRevision != task.Revision {
			return nil, 0, errRPCRevision
		}
		if err := store.Delete(ctx, task.Definition.ID, expectedRevision, d.now()); err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		changes, err := store.ListChanges(ctx, project.ID, 0, 1)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"deleted": true, "taskId": task.Definition.ID, "highWatermark": changes.HighWatermark}, changes.HighWatermark, nil
	case "task.clear":
		if !taskV2InputHasOnly(input, "expectedHighWatermark") {
			return nil, 0, errRPCInvalid
		}
		expected, err := taskV2OptionalRevisionPointer(input, "expectedHighWatermark")
		if err != nil {
			return nil, 0, err
		}
		result, err := store.ClearFinished(ctx, project.ID, expected, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		// The ordered task change journal is the paginated source of truth. A
		// mutation response carries only bounded acknowledgement metadata.
		result.Items = []taskV2Record{}
		return result, result.HighWatermark, nil
	case "task.queue.start":
		if !taskV2InputHasOnly(input, "expectedHighWatermark") {
			return nil, 0, errRPCInvalid
		}
		expected, err := taskV2OptionalRevisionPointer(input, "expectedHighWatermark")
		if err != nil {
			return nil, 0, err
		}
		result, err := store.ActivateQueue(ctx, project.ID, expected, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		result.Items = []taskV2Record{}
		return result, result.HighWatermark, nil
	case "task.queue.stop":
		if !taskV2InputHasOnly(input, "expectedHighWatermark") {
			return nil, 0, errRPCInvalid
		}
		expected, err := taskV2OptionalRevisionPointer(input, "expectedHighWatermark")
		if err != nil {
			return nil, 0, err
		}
		result, err := store.StopAll(ctx, project.ID, expected, d.now())
		if err != nil {
			return nil, 0, err
		}
		d.state.wakeTaskEngine()
		result.Items = []taskV2Record{}
		return result, result.HighWatermark, nil
	case "task.logs":
		return callTaskV2FileLogs(ctx, store, project.ID, input)
	case "task.logs.download.prepare":
		return d.taskLogDownloadPrepare(ctx, store, project, input)
	default:
		return nil, 0, errRPCInvalid
	}
}

func callTaskV2FileLogs(ctx context.Context, store *taskV2Store, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	for _, legacy := range []string{"stream", "afterSequence", "beforeSequence", "limitLines"} {
		if _, present := input[legacy]; present {
			return nil, 0, errTaskLogUpgradeRequired
		}
	}
	if !taskV2InputHasOnly(input, "taskId", "runId", "generation", "offset", "tailBytes", "beforeOffset", "limitBytes") {
		return nil, 0, errRPCInvalid
	}
	task, err := taskV2RecordForProject(ctx, store, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	var request taskLogSeekRequest
	request.LimitBytes = maximumTaskLogSeekBytes
	if value, present, valid := optionalUint64(input, "limitBytes"); !valid || present && (value == 0 || value > maximumTaskLogSeekBytes) {
		return nil, 0, errRPCInvalid
	} else if present {
		request.LimitBytes = value
	}
	if raw, present := input["runId"]; present {
		text, ok := raw.(string)
		parsed, parseErr := uuid.Parse(strings.TrimSpace(text))
		if !ok || parseErr != nil || parsed == uuid.Nil || parsed.String() != strings.TrimSpace(text) {
			return nil, 0, errRPCInvalid
		}
		request.RunID = &parsed
	}
	if value, present, valid := optionalUint64(input, "generation"); !valid || present && value == 0 {
		return nil, 0, errRPCInvalid
	} else if present {
		request.Generation = &value
	}
	offset, offsetPresent, offsetValid := optionalUint64(input, "offset")
	tail, tailPresent, tailValid := optionalUint64(input, "tailBytes")
	before, beforePresent, beforeValid := optionalUint64(input, "beforeOffset")
	if !offsetValid || !tailValid || !beforeValid || tailPresent && (tail == 0 || tail > maximumTaskLogSeekBytes) ||
		beforePresent && offsetPresent || tailPresent && offsetPresent || tailPresent && beforePresent {
		return nil, 0, errRPCInvalid
	}
	switch {
	case tailPresent:
		request.Mode, request.TailBytes = taskLogSeekTail, tail
	case beforePresent:
		request.Mode, request.BeforeOffset = taskLogSeekBefore, before
	default:
		request.Mode, request.Offset = taskLogSeekForward, offset
	}
	page, err := store.ReadRunLog(ctx, task.Definition.ID, request)
	if err != nil {
		return nil, 0, err
	}
	fitTaskLogSeekResponse(&page)
	return page, page.FileSize, nil
}

func fitTaskLogSeekResponse(page *taskLogSeekPage) {
	if page == nil {
		return
	}
	for page.Content != "" {
		encoded, err := json.Marshal(page)
		if err == nil && len(encoded) <= preferredRPCPagePayload {
			return
		}
		contents := []byte(page.Content)
		if page.mode == taskLogSeekForward {
			end := len(contents) - 1
			if end > 0 {
				end = strings.LastIndexByte(string(contents[:end]), '\n') + 1
			}
			if end <= 0 {
				page.Content = ""
				page.NextOffset = page.StartOffset
			} else {
				page.Content = string(contents[:end])
				page.NextOffset = page.StartOffset + uint64(end)
			}
			page.EOF = false
			continue
		}
		first := strings.IndexByte(page.Content, '\n')
		if first < 0 || first+1 >= len(contents) {
			page.StartOffset = page.NextOffset
			page.Content = ""
		} else {
			page.StartOffset += uint64(first + 1)
			page.Content = page.Content[first+1:]
		}
		page.HasMoreBefore = page.StartOffset > 0
	}
}

func (d dispatcher) callTaskV2StartNow(
	ctx context.Context,
	store *taskV2Store,
	projectID uuid.UUID,
	input rpcInput,
) (any, uint64, error) {
	task, err := taskV2RecordForProject(ctx, store, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	expectedRevision, ok := taskV2ExpectedRevision(input, "expectedRevision")
	if !ok || expectedRevision != task.Revision {
		return nil, 0, errRPCRevision
	}
	if task.Definition.Scope != "topLevel" || task.Definition.Execution.WorkflowID != nil {
		return nil, 0, errRPCInvalid
	}
	if task.Status == "waiting" && task.Definition.Kind != "workflow" {
		tasks, listErr := store.List(ctx, projectID)
		if listErr != nil {
			return nil, 0, listErr
		}
		if !taskDependenciesSatisfied(task, tasks) {
			return nil, 0, errRPCBusy
		}
	}
	started, err := store.StartNow(ctx, task.Definition.ID, expectedRevision, d.now())
	if err != nil {
		return nil, 0, err
	}
	d.state.wakeTaskEngine()
	return started, started.Revision, nil
}

func (d dispatcher) callTaskV2Stop(
	ctx context.Context,
	store *taskV2Store,
	projectID uuid.UUID,
	input rpcInput,
) (any, uint64, error) {
	task, err := taskV2RecordForProject(ctx, store, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	expectedRevision, ok := taskV2ExpectedRevision(input, "expectedRevision")
	if !ok || expectedRevision != task.Revision {
		return nil, 0, errRPCRevision
	}
	if task.Status == "cancelled" {
		return task, task.Revision, nil
	}
	if task.Definition.Scope == "workflowNode" {
		return nil, 0, errRPCInvalid
	}
	if engine := d.state.currentTaskEngine(); engine != nil {
		stopped, err := engine.Stop(ctx, projectID, task.Definition.ID, expectedRevision)
		if err != nil {
			return nil, 0, err
		}
		engine.Wake()
		return stopped, stopped.Revision, nil
	}
	result, revision, err := transitionTaskV2FromRPC(ctx, store, projectID, input, "cancelled", "cancelled", d.now())
	if err == nil {
		d.state.wakeTaskEngine()
	}
	return result, revision, err
}

func (d dispatcher) callTaskV2FollowUp(
	ctx context.Context,
	store *taskV2Store,
	project registeredProject,
	input rpcInput,
) (any, uint64, error) {
	if !taskV2InputHasOnly(input, "sourceTaskId", "taskId", "expectedRevision", "feedback") {
		return nil, 0, errRPCInvalid
	}
	sourceTaskID, sourceOK := inputUUID(input, "sourceTaskId")
	followUpID, followUpOK := inputUUID(input, "taskId")
	expectedRevision, revisionOK := taskV2ExpectedRevision(input, "expectedRevision")
	feedback, feedbackOK := inputString(input, "feedback", 64<<10)
	if !sourceOK || !followUpOK || sourceTaskID == followUpID || !revisionOK || !feedbackOK {
		return nil, 0, errRPCInvalid
	}
	source, err := store.Get(ctx, sourceTaskID)
	if err != nil {
		return nil, 0, err
	}
	if source.Definition.ProjectID != project.ID {
		return nil, 0, errRPCProject
	}
	if source.Revision != expectedRevision {
		return nil, 0, errRPCRevision
	}
	config := make(map[string]any)
	if err := json.Unmarshal(source.Definition.Config, &config); err != nil {
		return nil, 0, errors.New("stored task config is invalid")
	}
	sourceKind := source.Definition.Kind
	if sourceKind == "script" || sourceKind == "workflow" || source.Definition.Scope == "workflowNode" {
		return nil, 0, errRPCCapability
	}
	attachments, ok := taskStringList(config["attachedFilePaths"], maximumTaskAttachments, 4096)
	if !ok {
		return nil, 0, errors.New("stored task attachments are invalid")
	}
	originalContext := "Original task request:\n"
	if config["promptSource"] == "currentMarkdownFile" {
		promptFile, ok := config["promptFilePath"].(string)
		if !ok || promptFile == "" {
			return nil, 0, errors.New("stored task prompt file is invalid")
		}
		originalContext = "Original task file: " + promptFile
		if !slices.Contains(attachments, promptFile) {
			attachments = append([]string{promptFile}, attachments...)
		}
	} else if prompt, ok := config["promptText"].(string); ok && prompt != "" {
		originalContext += prompt
	} else {
		return nil, 0, errors.New("stored task prompt is invalid")
	}
	config["promptSource"] = "customText"
	config["promptText"] = "Continue the task after review feedback. Preserve the original context, address every requested change, and verify the result.\n\n" +
		originalContext + "\n\nReview feedback:\n" + feedback
	config["attachedFilePaths"] = attachments
	delete(config, "promptFilePath")
	encodedConfig, err := json.Marshal(config)
	if err != nil {
		return nil, 0, err
	}
	rootID := source.Definition.ID
	if source.Definition.RootTaskID != nil {
		rootID = *source.Definition.RootTaskID
	}
	environment := make(map[string]string, len(source.Definition.Environment))
	for name, value := range source.Definition.Environment {
		environment[name] = value
	}
	followUp, err := normalizeTaskV2Definition(project, taskV2Definition{
		ID: followUpID, ProjectID: project.ID, Kind: sourceKind, Title: taskV2FollowUpTitle(source.Definition.Title),
		CWD: source.Definition.CWD, Config: encodedConfig, Scope: "topLevel", ParentTaskID: &sourceTaskID, RootTaskID: &rootID,
		AcceptanceFeedback: feedback, Environment: environment,
		Execution: taskV2ExecutionOptions{
			Relation: "dependency", Mode: "serial", RelatedTaskIDs: []uuid.UUID{sourceTaskID}, RunImmediately: true,
		},
	})
	if err != nil {
		return nil, 0, err
	}
	result, err := store.CreateFollowUp(ctx, sourceTaskID, expectedRevision, followUp, d.now())
	if err != nil {
		return nil, 0, err
	}
	return result, result.HighWatermark, nil
}

func taskV2FollowUpTitle(source string) string {
	title := "Follow-up: " + strings.TrimSpace(source)
	for len([]byte(title)) > 200 {
		_, size := utf8.DecodeLastRuneInString(title)
		if size < 1 {
			return "Follow-up"
		}
		title = title[:len(title)-size]
	}
	return strings.TrimSpace(title)
}

func (d dispatcher) boundTaskV2Store(ctx context.Context) (*taskV2Store, registeredProject, error) {
	if d.state == nil || d.state.business == nil || d.state.tasksV2 == nil {
		return nil, registeredProject{}, errRPCCapability
	}
	projectID, err := uuid.Parse(strings.TrimSpace(d.requestProjectID))
	if err != nil || projectID == uuid.Nil {
		return nil, registeredProject{}, errRPCProject
	}
	project, err := d.state.business.projectByID(ctx, projectID)
	if err != nil || project.State != "available" {
		return nil, registeredProject{}, errRPCProject
	}
	if !project.Policy.AllowTaskExecution || !agentFeatureFlagsWithContext(ctx, d.state)["tasks.v2"] {
		return nil, registeredProject{}, errRPCCapability
	}
	return d.state.tasksV2, project, nil
}

func (d dispatcher) callTaskV2List(ctx context.Context, store *taskV2Store, project registeredProject, input rpcInput) (any, uint64, error) {
	if !taskV2InputHasOnly(input, "afterRevision", "cursor", "limit") {
		return nil, 0, errRPCInvalid
	}
	afterRevision, hasAfterRevision, ok := optionalUint64(input, "afterRevision")
	if !ok {
		return nil, 0, errRPCInvalid
	}
	if hasAfterRevision {
		if _, hasCursor := input["cursor"]; hasCursor {
			return nil, 0, errRPCInvalid
		}
		limit := 100
		if value, present, valid := optionalUint64(input, "limit"); !valid || present && (value < 1 || value > 256) {
			return nil, 0, errRPCInvalid
		} else if present {
			limit = int(value)
		}
		page, err := store.ListChanges(ctx, project.ID, afterRevision, limit)
		if err != nil {
			return nil, 0, err
		}
		originalHasMore := page.HasMore
		build := func(count int) any {
			result := page
			result.Items = page.Items[:count]
			result.AckedThroughSequence = afterRevision
			if count > 0 {
				result.AckedThroughSequence = page.Items[count-1].Sequence
			}
			result.HasMore = originalHasMore || count < len(page.Items)
			return result
		}
		count, err := rpcPagePrefixLength(len(page.Items), build)
		if err != nil {
			return nil, 0, err
		}
		return build(count), page.HighWatermark, nil
	}
	limit := uint64(10)
	if value, present, valid := optionalUint64(input, "limit"); !valid || present && (value < 1 || value > 20) {
		return nil, 0, errRPCInvalid
	} else if present {
		limit = value
	}
	pageInput := rpcInput{"limit": float64(limit)}
	if cursor, found := input["cursor"]; found {
		pageInput["cursor"] = cursor
	}
	page, err := store.ListVersionedPage(ctx, project.ID, pageInput)
	if err != nil {
		return nil, 0, err
	}
	build := func(count int) any {
		end := page.Start + count
		return map[string]any{
			"items":                    page.Items[:count],
			"nextCursor":               versionedPageCursor(page.HighWatermark, end, page.Total),
			"highWatermark":            page.HighWatermark,
			"minimumAvailableSequence": page.MinimumAvailableSequence, "resetRequired": false,
		}
	}
	count, err := rpcPagePrefixLength(len(page.Items), build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), page.HighWatermark, nil
}

func taskV2DefinitionFromInput(project registeredProject, input rpcInput) (taskV2Definition, error) {
	raw, found := input["definition"]
	if !found || raw == nil {
		return taskV2Definition{}, errRPCInvalid
	}
	encoded, err := json.Marshal(raw)
	if err != nil || len(encoded) == 0 || len(encoded) > maximumTaskDefinitionBytes {
		return taskV2Definition{}, errRPCInvalid
	}
	definition, err := decodeTaskV2Definition(encoded)
	if err != nil {
		return taskV2Definition{}, err
	}
	return normalizeTaskV2Definition(project, definition)
}

func taskV2RecordForProject(ctx context.Context, store *taskV2Store, projectID uuid.UUID, input rpcInput) (taskV2Record, error) {
	taskID, ok := inputUUID(input, "taskId")
	if !ok {
		return taskV2Record{}, errRPCInvalid
	}
	task, err := store.Get(ctx, taskID)
	if err != nil {
		return taskV2Record{}, err
	}
	if task.Definition.ProjectID != projectID {
		return taskV2Record{}, errRPCProject
	}
	return task, nil
}

func transitionTaskV2FromRPC(
	ctx context.Context,
	store *taskV2Store,
	projectID uuid.UUID,
	input rpcInput,
	nextStatus string,
	resultCode string,
	now time.Time,
) (any, uint64, error) {
	task, err := taskV2RecordForProject(ctx, store, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	expectedRevision, ok := taskV2ExpectedRevision(input, "expectedRevision")
	if !ok || expectedRevision != task.Revision {
		return nil, 0, errRPCRevision
	}
	if task.Definition.Scope == "workflowNode" {
		return nil, 0, errRPCInvalid
	}
	if task.Status == nextStatus {
		return task, task.Revision, nil
	}
	updated, err := store.Transition(ctx, task.Definition.ID, expectedRevision, nextStatus, resultCode, now)
	if err != nil {
		return nil, 0, err
	}
	return updated, updated.Revision, nil
}

func (d dispatcher) callTaskV2Accept(ctx context.Context, store *taskV2Store, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !taskV2InputHasOnly(input, "taskId", "expectedRevision", "evidence") {
		return nil, 0, errRPCInvalid
	}
	task, err := taskV2RecordForProject(ctx, store, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	expectedRevision, ok := taskV2ExpectedRevision(input, "expectedRevision")
	evidence, evidenceOK := optionalInputString(input, "evidence", maximumTaskLogEntryBytes-256)
	if !ok || !evidenceOK {
		return nil, 0, errRPCInvalid
	}
	if expectedRevision != task.Revision {
		return nil, 0, errRPCRevision
	}
	message := "[WenzWork] Task accepted."
	if evidence != "" {
		message += "\n[Acceptance evidence]\n" + evidence
	}
	updated, _, err := store.Accept(ctx, task.Definition.ID, expectedRevision, []byte(message), d.now())
	if err != nil {
		return nil, 0, fmt.Errorf("persist task acceptance log: %w", err)
	}
	return updated, updated.Revision, nil
}

func callTaskV2Logs(ctx context.Context, store *taskV2Store, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !taskV2InputHasOnly(input, "taskId", "stream", "afterSequence", "beforeSequence", "limitBytes", "limitLines") {
		return nil, 0, errRPCInvalid
	}
	task, err := taskV2RecordForProject(ctx, store, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	stream, ok := optionalInputString(input, "stream", 16)
	afterSequence, afterPresent, sequenceOK := optionalUint64(input, "afterSequence")
	beforeSequence, beforePresent, beforeOK := optionalUint64(input, "beforeSequence")
	limitBytes := uint64(preferredRPCPagePayload)
	if value, present, valid := optionalUint64(input, "limitBytes"); !valid || present && (value < 1 || value > 1<<20) {
		return nil, 0, errRPCInvalid
	} else if present {
		limitBytes = min(value, uint64(preferredRPCPagePayload))
	}
	limitLines := uint64(100)
	if value, present, valid := optionalUint64(input, "limitLines"); !valid || present && (value < 1 || value > 100) {
		return nil, 0, errRPCInvalid
	} else if present {
		limitLines = value
	}
	if !ok || !sequenceOK || !beforeOK || stream != "" && !validTaskV2LogStream(stream) || afterPresent && beforePresent {
		return nil, 0, errRPCInvalid
	}
	var page taskV2LogPage
	if beforePresent {
		page, err = store.ListLogsBefore(ctx, task.Definition.ID, stream, beforeSequence, limitLines)
	} else if !afterPresent && input["limitLines"] != nil {
		// A line-oriented request without an explicit before cursor starts at
		// the current tail. This is the default used by the task detail view.
		page, err = store.ListLogsBefore(ctx, task.Definition.ID, stream, 0, limitLines)
	} else {
		page, err = store.ListLogs(ctx, task.Definition.ID, stream, afterSequence, limitBytes)
	}
	if err != nil {
		return nil, 0, err
	}
	items := make([]taskV2LogRPCView, 0, len(page.Items))
	for _, entry := range page.Items {
		items = append(items, taskV2LogRPCProjection(entry))
	}
	reversePage := beforePresent || !afterPresent && input["limitLines"] != nil
	build := func(count int) any {
		acked := afterSequence
		selected := items[:count]
		nextBeforeSequence := page.NextBeforeSequence
		if reversePage {
			// Tail pagination must retain the newest suffix when the encrypted RPC
			// JSON budget is smaller than the requested line window. Returning the
			// oldest prefix while advertising the current high-watermark made task
			// terminals skip the most recent output under a dense burst.
			selected = items[len(items)-count:]
			if count < len(items) && count > 0 {
				nextBeforeSequence = selected[0].Sequence - 1
			}
		}
		if len(selected) > 0 {
			acked = selected[len(selected)-1].Sequence
		}
		lineCount := page.LineCount
		if reversePage {
			lineCount = uint64(len(selected))
		}
		return map[string]any{
			"items": selected, "ackedThroughSequence": acked, "highWatermark": page.HighWatermark,
			"minimumAvailableSequence": page.MinimumAvailableSequence,
			"nextBeforeSequence":       nextBeforeSequence, "lineCount": lineCount,
			"hasMore": page.HasMore || count < len(items), "resetRequired": page.ResetRequired,
		}
	}
	count, err := rpcPagePrefixLength(len(items), build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), page.HighWatermark, nil
}

func taskV2LogRPCProjection(entry taskV2Log) taskV2LogRPCView {
	view := taskV2LogRPCView{
		TaskID: entry.TaskID, RunID: entry.RunID, Sequence: entry.Sequence, Stream: entry.Stream,
		OccurredAt: entry.OccurredAt, RawAvailable: entry.RawAvailable,
	}
	// New entries carry a projection produced while their stream state was
	// available. Legacy rows keep empty metadata and are decoded deterministically
	// here, preserving the old UTF-8/base64 response contract.
	if entry.SourceEncoding != "" || entry.DisplayText != "" || entry.IsBinary || !entry.RawAvailable {
		view.SourceEncoding = entry.SourceEncoding
		if entry.IsBinary {
			view.Encoding = "base64"
			view.ContentBase64 = base64.StdEncoding.EncodeToString(entry.Content)
			view.DecodeWarning = "binary_output"
			return view
		}
		view.Encoding, view.Content = "utf-8", entry.DisplayText
		if entry.HadDecodeErrors {
			view.DecodeWarning = "decoded_with_replacements"
		}
		return view
	}
	decoder := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
	results := decoder.Feed(entry.Content)
	results = append(results, decoder.Flush()...)
	if len(results) == 0 || results[len(results)-1].IsBinary {
		view.Encoding = "base64"
		view.SourceEncoding = "binary"
		view.ContentBase64 = base64.StdEncoding.EncodeToString(entry.Content)
		view.DecodeWarning = "binary_output"
		return view
	}
	var text strings.Builder
	sourceEncoding, hadErrors := "utf-8", false
	for _, result := range results {
		text.WriteString(result.DisplayText)
		if result.SourceEncoding != "" {
			sourceEncoding = result.SourceEncoding
		}
		hadErrors = hadErrors || result.HadDecodeErrors
	}
	view.Encoding, view.Content, view.SourceEncoding = "utf-8", text.String(), sourceEncoding
	if hadErrors {
		view.DecodeWarning = "decoded_with_replacements"
	}
	return view
}

func taskV2InputHasOnly(input rpcInput, allowed ...string) bool {
	for key := range input {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

func taskV2ExpectedRevision(input rpcInput, key string) (uint64, bool) {
	value, present, ok := optionalUint64(input, key)
	return value, ok && present && value > 0
}

func taskV2OptionalRevisionPointer(input rpcInput, key string) (*uint64, error) {
	value, present, ok := optionalUint64(input, key)
	if !ok || present && value == 0 {
		return nil, errRPCInvalid
	}
	if !present {
		return nil, nil
	}
	return &value, nil
}
