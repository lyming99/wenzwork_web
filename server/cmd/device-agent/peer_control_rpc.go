package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wenzwork/wenzwork-web/server/internal/remotecontrol"
)

type peerProjectView struct {
	ID           uuid.UUID `json:"id"`
	DisplayName  string    `json:"displayName"`
	Revision     uint64    `json:"revision"`
	Capabilities []string  `json:"capabilities"`
	ObservedAt   time.Time `json:"observedAt"`
	State        string    `json:"state"`
}

type peerTaskView struct {
	ID         uuid.UUID            `json:"id"`
	DeviceID   uuid.UUID            `json:"deviceId"`
	ProjectID  *uuid.UUID           `json:"projectId"`
	TaskType   string               `json:"taskType"`
	Title      string               `json:"title"`
	Status     string               `json:"status"`
	Revision   uint64               `json:"revision"`
	CreatedAt  time.Time            `json:"createdAt"`
	StartedAt  *time.Time           `json:"startedAt"`
	FinishedAt *time.Time           `json:"finishedAt"`
	ResultCode *string              `json:"resultCode"`
	Kind       string               `json:"kind"`
	Runnable   bool                 `json:"runnable"`
	Run        peerTaskRunView      `json:"run"`
	Workflow   peerWorkflowLinkView `json:"workflow"`
}

type peerProjectChangeView struct {
	Sequence uint64          `json:"sequence"`
	Deleted  bool            `json:"deleted,omitempty"`
	Value    peerProjectView `json:"value"`
}

type peerTaskChangeView struct {
	Sequence uint64       `json:"sequence"`
	Deleted  bool         `json:"deleted,omitempty"`
	Value    peerTaskView `json:"value"`
}

type peerTaskRunView struct {
	ID                      string     `json:"id"`
	TaskID                  uuid.UUID  `json:"taskId"`
	Status                  string     `json:"status"`
	CreatedAt               time.Time  `json:"createdAt"`
	StartedAt               *time.Time `json:"startedAt"`
	FinishedAt              *time.Time `json:"finishedAt"`
	ResultCode              *string    `json:"resultCode"`
	Attempt                 uint32     `json:"attempt"`
	ParentWorkflowTaskRunID *string    `json:"parentWorkflowTaskRunId"`
	WorkflowNodeID          *string    `json:"workflowNodeId"`
}

type peerWorkflowLinkView struct {
	ParentTaskID  *string `json:"parentTaskId"`
	RootTaskID    *string `json:"rootTaskId"`
	WorkflowRunID *string `json:"workflowRunId"`
	NodeID        *string `json:"nodeId"`
}

func (d dispatcher) callProjectList(input rpcInput) (any, uint64, error) {
	if d.controlStore == nil {
		return nil, 0, errRPCNotFound
	}
	snapshot, err := d.controlStore.snapshot()
	if err != nil {
		return nil, 0, err
	}
	highWatermark := snapshot.ProjectHighWatermark
	if delta, revision, handled, err := d.projectDeltaPage(input, snapshot); handled || err != nil {
		return delta, revision, err
	}
	items := make([]peerProjectView, 0, len(snapshot.Sync.Projects))
	for _, project := range snapshot.Sync.Projects {
		observedAt := project.ObservedAt
		if observedAt.IsZero() {
			observedAt = d.now()
		}
		items = append(items, peerProjectView{
			ID: project.ID, DisplayName: project.DisplayName, Revision: project.Revision,
			Capabilities: append([]string(nil), projectCapabilities...), ObservedAt: observedAt.UTC(), State: project.State,
		})
	}
	slices.SortFunc(items, func(left, right peerProjectView) int {
		if result := strings.Compare(strings.ToLower(left.DisplayName), strings.ToLower(right.DisplayName)); result != 0 {
			return result
		}
		return strings.Compare(left.ID.String(), right.ID.String())
	})
	start, requestedEnd, _, err := versionedPageWindow(input, len(items), highWatermark)
	if err != nil {
		return nil, 0, err
	}
	observedAt := d.now()
	resetRequired := syncResetRequired(input, snapshot.ProjectMinimumAvailableSequence, highWatermark)
	build := func(count int) any {
		end := start + count
		return map[string]any{
			"items": items[start:end], "nextCursor": versionedPageCursor(highWatermark, end, len(items)), "highWatermark": highWatermark,
			"changes": []peerProjectChangeView{}, "resetRequired": resetRequired, "observedAt": observedAt,
		}
	}
	count, err := rpcPagePrefixLength(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), highWatermark, nil
}

func (d dispatcher) callProjectRefresh(ctx context.Context) (any, uint64, error) {
	if d.controlStore == nil {
		return nil, 0, errRPCNotFound
	}
	if _, err := reconcileWorkspaceProjects(ctx, d.state, d.controlStore, d.now(), false, nil); err != nil {
		return nil, 0, err
	}
	snapshot, err := d.controlStore.snapshot()
	if err != nil {
		return nil, 0, err
	}
	highWatermark := snapshot.ProjectHighWatermark
	return map[string]any{"refreshed": true, "count": len(snapshot.Sync.Projects), "highWatermark": highWatermark}, highWatermark, nil
}

func (d dispatcher) callTaskList(input rpcInput) (any, uint64, error) {
	repository := d.taskRepository()
	if repository == nil {
		return nil, 0, errRPCNotFound
	}
	snapshot, err := repository.Snapshot(context.Background())
	if err != nil {
		return nil, 0, err
	}
	highWatermark := snapshot.HighWatermark
	if delta, revision, handled, err := d.taskDeltaPage(input, snapshot); handled || err != nil {
		return delta, revision, err
	}
	items := make([]peerTaskView, 0, len(snapshot.Tasks))
	for _, task := range snapshot.Tasks {
		items = append(items, localTaskView(d.state.DeviceID, task))
	}
	slices.SortFunc(items, func(left, right peerTaskView) int {
		if result := right.CreatedAt.Compare(left.CreatedAt); result != 0 {
			return result
		}
		return strings.Compare(left.ID.String(), right.ID.String())
	})
	start, requestedEnd, _, err := versionedPageWindow(input, len(items), highWatermark)
	if err != nil {
		return nil, 0, err
	}
	resetRequired := syncResetRequired(input, snapshot.MinimumAvailableSequence, highWatermark)
	build := func(count int) any {
		end := start + count
		return map[string]any{
			"items": items[start:end], "changes": []peerTaskChangeView{},
			"nextCursor": versionedPageCursor(highWatermark, end, len(items)), "highWatermark": highWatermark,
			"resetRequired": resetRequired,
		}
	}
	count, err := rpcPagePrefixLength(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), highWatermark, nil
}

func (d dispatcher) callTaskGet(input rpcInput) (any, uint64, error) {
	taskID, ok := inputUUID(input, "taskId")
	repository := d.taskRepository()
	if !ok || repository == nil {
		return nil, 0, errRPCInvalid
	}
	task, err := repository.Get(context.Background(), taskID)
	if err != nil {
		return nil, 0, err
	}
	return localTaskView(d.state.DeviceID, task), task.Revision, nil
}

func (d dispatcher) callTaskCreate(input rpcInput) (any, uint64, error) {
	if d.controlLoop == nil {
		return nil, 0, errRPCNotFound
	}
	taskID := uuid.New()
	if value, exists := input["taskId"]; exists {
		text, ok := value.(string)
		parsed, err := uuid.Parse(strings.TrimSpace(text))
		if !ok || err != nil || parsed == uuid.Nil {
			return nil, 0, errRPCInvalid
		}
		taskID = parsed
	}
	var projectID *uuid.UUID
	if value, exists := input["projectId"]; exists && value != nil && value != "" {
		text, ok := value.(string)
		parsed, err := uuid.Parse(strings.TrimSpace(text))
		if !ok || err != nil || parsed == uuid.Nil {
			return nil, 0, errRPCInvalid
		}
		projectID = &parsed
	}
	taskType, taskTypeOK := inputString(input, "taskType", 80)
	title, titleOK := inputString(input, "title", 200)
	expectedRevision, expectedPresent, expectedOK := optionalUint64(input, "expectedRevision")
	if !taskTypeOK || !titleOK || !expectedOK {
		return nil, 0, errRPCInvalid
	}
	var expected *uint64
	if expectedPresent {
		expected = &expectedRevision
	}
	rawInput, exists := input["input"]
	if !exists {
		rawInput = map[string]any{}
	}
	encodedInput, err := json.Marshal(rawInput)
	if err != nil || len(encodedInput) > 16<<10 {
		return nil, 0, errRPCInvalid
	}
	body, err := json.Marshal(taskCreateCommandBody{
		TaskID: taskID, ProjectID: projectID, TaskType: taskType, Title: title,
		ExpectedRevision: expected, Input: encodedInput,
	})
	if err != nil {
		return nil, 0, err
	}
	spec, err := decodeTaskCreateCommand(body, &taskID)
	if err != nil {
		return nil, 0, errRPCInvalid
	}
	task, err := d.controlLoop.createPeerTask(spec, d.now())
	if err != nil {
		return nil, 0, err
	}
	return localTaskView(d.state.DeviceID, task), task.Revision, nil
}

func (d dispatcher) callTaskCancel(input rpcInput) (any, uint64, error) {
	taskID, ok := inputUUID(input, "taskId")
	if !ok || d.controlLoop == nil {
		return nil, 0, errRPCInvalid
	}
	task, err := d.controlLoop.cancelPeerTask(taskID, d.now())
	if err != nil {
		return nil, 0, err
	}
	return localTaskView(d.state.DeviceID, task), task.Revision, nil
}

func (d dispatcher) callTaskLogs(input rpcInput) (any, uint64, error) {
	taskID, ok := inputUUID(input, "taskId")
	repository := d.taskRepository()
	if !ok || repository == nil {
		return nil, 0, errRPCInvalid
	}
	stream, ok := optionalInputString(input, "stream", 16)
	if !ok || stream != "" && !validTaskLogStream(stream) {
		return nil, 0, errRPCInvalid
	}
	afterSequence, _, ok := optionalUint64(input, "afterSequence")
	if !ok {
		return nil, 0, errRPCInvalid
	}
	limitBytes := uint64(preferredRPCPagePayload)
	if value, present, valid := optionalUint64(input, "limitBytes"); !valid || present && (value < 1 || value > 1<<20) {
		return nil, 0, errRPCInvalid
	} else if present {
		limitBytes = min(value, uint64(preferredRPCPagePayload))
	}
	task, err := repository.Get(context.Background(), taskID)
	if err != nil {
		return nil, 0, err
	}
	persistedLogs, err := repository.ListLogs(context.Background(), taskID)
	if err != nil {
		return nil, 0, err
	}
	available := make([]remotecontrol.TaskLog, 0, len(persistedLogs))
	for _, entry := range persistedLogs {
		if stream == "" || entry.Stream == stream {
			available = append(available, entry)
		}
	}
	minimumAvailable := uint64(0)
	if len(available) > 0 {
		minimumAvailable = available[0].Sequence
	}
	items := make([]remotecontrol.TaskLog, 0)
	used := uint64(0)
	hasMore := false
	for _, entry := range available {
		if entry.Sequence <= afterSequence {
			continue
		}
		size := uint64(len([]byte(entry.Content)))
		if used+size > limitBytes && len(items) > 0 {
			hasMore = true
			break
		}
		items = append(items, entry)
		used += size
	}
	highWatermark := uint64(0)
	if task.NextLogSequence > 0 {
		highWatermark = task.NextLogSequence - 1
	}
	resetRequired := minimumAvailable > 0 && afterSequence+1 < minimumAvailable
	build := func(count int) any {
		acked := afterSequence
		if count > 0 {
			acked = items[count-1].Sequence
		}
		return map[string]any{
			"items": items[:count], "ackedThroughSequence": acked,
			"highWatermark": highWatermark, "hasMore": hasMore || count < len(items),
			"minimumAvailableSequence": minimumAvailable, "resetRequired": resetRequired,
		}
	}
	count, err := rpcPagePrefixLength(len(items), build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), task.Revision, nil
}

func localTaskView(deviceID uuid.UUID, task localTask) peerTaskView {
	var resultCode *string
	if task.TerminalResult != "" {
		value := task.TerminalResult
		resultCode = &value
	}
	status := wenzmarkTaskStatus(task.Status)
	return peerTaskView{
		ID: task.Spec.TaskID, DeviceID: deviceID, ProjectID: task.Spec.ProjectID, TaskType: task.Spec.TaskType,
		Title: task.Spec.Title, Status: task.Status, Revision: task.Revision, CreatedAt: task.CreatedAt.UTC(),
		StartedAt: task.StartedAt, FinishedAt: task.FinishedAt, ResultCode: resultCode,
		Kind: wenzmarkTaskKind(task.Spec.TaskType), Runnable: supportedTypedTask(task.Spec.TaskType),
		Run: peerTaskRunView{
			ID: stableTaskRunID(task.Spec.TaskID), TaskID: task.Spec.TaskID, Status: status, CreatedAt: task.CreatedAt.UTC(),
			StartedAt: task.StartedAt, FinishedAt: task.FinishedAt, ResultCode: resultCode, Attempt: 0,
		},
		Workflow: peerWorkflowLinkView{},
	}
}

func stableTaskRunID(taskID uuid.UUID) string {
	return uuid.NewSHA1(taskID, []byte("wenzwork-task-run:v1")).String()
}

func supportedTypedTask(taskType string) bool {
	switch taskType {
	case "workspace.inspect", "markdown.render", "ai.summarize":
		return true
	default:
		return false
	}
}

// WenzMark compatibility is deliberately representational. WenzWork executes
// only its three reviewed built-ins; it never treats WenzMark's script/CLI
// kinds as authority to start an arbitrary local process.
func wenzmarkTaskKind(taskType string) string {
	if taskType == "workflow" {
		return "workflow"
	}
	return "unsupported"
}

func wenzmarkTaskStatus(status string) string {
	switch status {
	case "queued", "dispatched", "accepted":
		return "waiting"
	case "running", "cancel_requested":
		return "running"
	case "succeeded":
		return "succeeded"
	case "cancelled":
		return "cancelled"
	case "failed", "rejected", "expired", "timed_out":
		return "failed"
	default:
		return "blocked"
	}
}

func (d dispatcher) projectDeltaPage(input rpcInput, snapshot controlPersistentState) (any, uint64, bool, error) {
	cursor, ok := optionalInputString(input, "cursor", 128)
	if !ok {
		return nil, 0, true, errRPCInvalid
	}
	after, target, deltaCursor, cursorErr := decodeDeltaCursor(cursor, "project")
	if cursorErr != nil {
		return nil, 0, true, cursorErr
	}
	if !deltaCursor {
		var present bool
		after, present, ok = inputSyncSequence(input)
		if !ok {
			return nil, 0, true, errRPCInvalid
		}
		if !present || after == 0 {
			return nil, 0, false, nil
		}
		target = snapshot.ProjectHighWatermark
	}
	if after > target || target > snapshot.ProjectHighWatermark || after+1 < snapshot.ProjectMinimumAvailableSequence {
		return nil, 0, false, nil
	}
	changes := make([]peerProjectChangeView, 0)
	for _, change := range snapshot.ProjectChanges {
		if change.Sequence > after && change.Sequence <= target {
			changes = append(changes, peerProjectChangeView{
				Sequence: change.Sequence, Deleted: change.Deleted, Value: localProjectView(change.Project, d.now()),
			})
		}
	}
	if after < target && (len(changes) == 0 || changes[0].Sequence != after+1) {
		return nil, 0, false, nil
	}
	limit, err := rpcPageLimit(input)
	if err != nil {
		return nil, 0, true, err
	}
	requestedCount := min(len(changes), limit)
	observedAt := d.now()
	build := func(count int) any {
		acked := target
		if count > 0 {
			acked = changes[count-1].Sequence
		}
		hasMore := count < len(changes)
		var next *string
		if hasMore {
			value := encodeDeltaCursor("project", target, acked)
			next = &value
		}
		return map[string]any{
			"items": []peerProjectView{}, "changes": changes[:count], "nextCursor": next, "highWatermark": acked,
			"snapshotHighWatermark": target, "minimumAvailableSequence": snapshot.ProjectMinimumAvailableSequence,
			"resetRequired": false, "observedAt": observedAt,
		}
	}
	count, err := rpcPagePrefixLength(requestedCount, build)
	if err != nil {
		return nil, 0, true, err
	}
	page := build(count).(map[string]any)
	return page, page["highWatermark"].(uint64), true, nil
}

func (d dispatcher) taskDeltaPage(input rpcInput, snapshot taskRepositorySnapshot) (any, uint64, bool, error) {
	cursor, ok := optionalInputString(input, "cursor", 128)
	if !ok {
		return nil, 0, true, errRPCInvalid
	}
	after, target, deltaCursor, cursorErr := decodeDeltaCursor(cursor, "task")
	if cursorErr != nil {
		return nil, 0, true, cursorErr
	}
	if !deltaCursor {
		var present bool
		after, present, ok = inputSyncSequence(input)
		if !ok {
			return nil, 0, true, errRPCInvalid
		}
		if !present || after == 0 {
			return nil, 0, false, nil
		}
		target = snapshot.HighWatermark
	}
	if after > target || target > snapshot.HighWatermark || after+1 < snapshot.MinimumAvailableSequence {
		return nil, 0, false, nil
	}
	changes := make([]peerTaskChangeView, 0)
	for _, change := range snapshot.Changes {
		if change.Sequence > after && change.Sequence <= target {
			changes = append(changes, peerTaskChangeView{
				Sequence: change.Sequence, Deleted: change.Deleted, Value: localTaskView(d.state.DeviceID, change.Task),
			})
		}
	}
	if after < target && (len(changes) == 0 || changes[0].Sequence != after+1) {
		return nil, 0, false, nil
	}
	limit, err := rpcPageLimit(input)
	if err != nil {
		return nil, 0, true, err
	}
	requestedCount := min(len(changes), limit)
	build := func(count int) any {
		acked := target
		if count > 0 {
			acked = changes[count-1].Sequence
		}
		hasMore := count < len(changes)
		var next *string
		if hasMore {
			value := encodeDeltaCursor("task", target, acked)
			next = &value
		}
		return map[string]any{
			"items": []peerTaskView{}, "changes": changes[:count], "nextCursor": next, "highWatermark": acked,
			"snapshotHighWatermark": target, "minimumAvailableSequence": snapshot.MinimumAvailableSequence,
			"resetRequired": false,
		}
	}
	count, err := rpcPagePrefixLength(requestedCount, build)
	if err != nil {
		return nil, 0, true, err
	}
	page := build(count).(map[string]any)
	return page, page["highWatermark"].(uint64), true, nil
}

func localProjectView(project localProject, fallback time.Time) peerProjectView {
	observedAt := project.ObservedAt
	if observedAt.IsZero() {
		observedAt = fallback
	}
	return peerProjectView{
		ID: project.ID, DisplayName: project.DisplayName, Revision: project.Revision,
		Capabilities: append([]string(nil), projectCapabilities...), ObservedAt: observedAt.UTC(), State: project.State,
	}
}

func inputSyncSequence(input rpcInput) (uint64, bool, bool) {
	if _, exists := input["afterSequence"]; exists {
		return optionalUint64(input, "afterSequence")
	}
	return optionalUint64(input, "afterRevision")
}

func syncResetRequired(input rpcInput, minimum, high uint64) bool {
	after, present, ok := inputSyncSequence(input)
	return ok && present && after > 0 && (after > high || after+1 < minimum)
}

func rpcPageLimit(input rpcInput) (int, error) {
	limit := 50
	if raw, exists := input["limit"]; exists {
		number, ok := raw.(float64)
		if !ok || number < 1 || number > 200 || number != float64(int(number)) {
			return 0, errRPCInvalid
		}
		limit = int(number)
	}
	return limit, nil
}

func encodeDeltaCursor(kind string, target, after uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("delta:%s:%d:%d", kind, target, after)))
}

func decodeDeltaCursor(cursor, wantKind string) (after, target uint64, handled bool, err error) {
	if cursor == "" {
		return 0, 0, false, nil
	}
	decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if decodeErr != nil || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
		return 0, 0, false, nil // It may be a full-snapshot cursor.
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) == 0 || parts[0] != "delta" {
		return 0, 0, false, nil
	}
	if len(parts) != 4 || parts[1] != wantKind {
		return 0, 0, true, errRPCInvalid
	}
	target, targetErr := strconv.ParseUint(parts[2], 10, 64)
	after, afterErr := strconv.ParseUint(parts[3], 10, 64)
	if targetErr != nil || afterErr != nil || after > target {
		return 0, 0, true, errRPCInvalid
	}
	return after, target, true, nil
}

func inputUUID(input rpcInput, key string) (uuid.UUID, bool) {
	value, ok := inputString(input, key, 64)
	parsed, err := uuid.Parse(value)
	return parsed, ok && err == nil && parsed != uuid.Nil
}

func (d dispatcher) taskRepository() TaskRepository {
	if d.tasks != nil {
		return d.tasks
	}
	return newEncryptedControlTaskRepository(d.controlStore)
}

// versionedPageWindow binds the opaque cursor to the selected device snapshot.
// A concurrent scan/task transition therefore fails with a revision conflict
// instead of silently skipping or duplicating records via a naked offset.
func versionedPageWindow(input rpcInput, total int, highWatermark uint64) (start, end int, next *string, err error) {
	limit := 50
	if raw, exists := input["limit"]; exists {
		number, ok := raw.(float64)
		if !ok || number < 1 || number > 200 || number != float64(int(number)) {
			return 0, 0, nil, errRPCInvalid
		}
		limit = int(number)
	}
	cursor, ok := optionalInputString(input, "cursor", 96)
	if !ok {
		return 0, 0, nil, errRPCInvalid
	}
	if cursor != "" {
		decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(cursor)
		parts := strings.Split(string(decoded), ":")
		cursorWatermark, watermarkErr := strconv.ParseUint(firstPart(parts), 10, 64)
		cursorOffset, offsetErr := strconv.Atoi(secondPart(parts))
		if decodeErr != nil || len(parts) != 2 || watermarkErr != nil || offsetErr != nil || cursorWatermark != highWatermark ||
			cursorOffset < 0 || cursorOffset > total || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
			if decodeErr == nil && watermarkErr == nil && cursorWatermark != highWatermark {
				return 0, 0, nil, errRPCRevision
			}
			return 0, 0, nil, errRPCInvalid
		}
		start = cursorOffset
	}
	end = min(total, start+limit)
	next = versionedPageCursor(highWatermark, end, total)
	return start, end, next, nil
}

func versionedPageCursor(highWatermark uint64, offset, total int) *string {
	if offset >= total {
		return nil
	}
	value := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", highWatermark, offset)))
	return &value
}

func firstPart(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func secondPart(values []string) string {
	if len(values) > 1 {
		return values[1]
	}
	return ""
}

func (loop *deviceControlLoop) createPeerTask(spec localTaskSpec, now time.Time) (localTask, error) {
	if loop == nil || loop.store == nil || spec.TaskID == uuid.Nil {
		return localTask{}, errRPCInvalid
	}
	if _, err := loop.taskProjectRoot(spec.ProjectID, spec.ExpectedRevision); err != nil {
		return localTask{}, errRPCNotFound
	}
	var result localTask
	created := false
	err := loop.store.update(func(state *controlPersistentState) error {
		if existing, exists := state.Tasks[spec.TaskID.String()]; exists {
			if !existing.PeerManaged || !sameLocalTaskSpec(existing.Spec, spec) {
				return errRPCRevision
			}
			result = existing
			return nil
		}
		result = localTask{
			Spec: spec, PeerManaged: true, Status: "running", Revision: taskRevisionAfter(0, now),
			CreatedAt: now.UTC(), StartedAt: timePointer(now), NextLogSequence: 1,
		}
		appendTaskLogEvent(state, &result, "system", "Task accepted.", now)
		appendTaskLogEvent(state, &result, "system", "Task started.", now)
		putLocalTask(state, &result)
		created = true
		return nil
	})
	if err != nil {
		return localTask{}, err
	}
	if created {
		loop.startTaskWorker(spec.TaskID)
	}
	return result, nil
}

func (loop *deviceControlLoop) cancelPeerTask(taskID uuid.UUID, now time.Time) (localTask, error) {
	if loop == nil || loop.store == nil || taskID == uuid.Nil {
		return localTask{}, errRPCInvalid
	}
	var result localTask
	err := loop.store.update(func(state *controlPersistentState) error {
		task, exists := state.Tasks[taskID.String()]
		if !exists {
			return errRPCNotFound
		}
		if task.Status == "succeeded" || task.Status == "failed" || task.Status == "cancelled" || task.Status == "rejected" {
			result = task
			return nil
		}
		task.CancelRequested, task.Status, task.Revision = true, "cancel_requested", taskRevisionAfter(task.Revision, now)
		state.CancelledTasks[taskID.String()] = true
		appendTaskLogEvent(state, &task, "system", "Cancellation requested.", now)
		putLocalTask(state, &task)
		result = task
		return nil
	})
	if err != nil {
		return localTask{}, err
	}
	loop.taskMu.Lock()
	cancel := loop.taskCancel[taskID.String()]
	loop.taskMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return result, nil
}
