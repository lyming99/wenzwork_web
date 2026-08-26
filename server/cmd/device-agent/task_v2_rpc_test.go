package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestTaskV2LogRPCProjectionKeepsEncodingAndBinaryMetadata(t *testing.T) {
	entry := taskV2Log{
		TaskID: uuid.New(), Sequence: 1, Stream: "stdout", Content: []byte{0xd6, 0xd0, 0xce, 0xc4},
		DisplayText: "中文", SourceEncoding: "gb18030", RawAvailable: true, OccurredAt: time.Now().UTC(),
	}
	view := taskV2LogRPCProjection(entry)
	if view.Encoding != "utf-8" || view.Content != "中文" || view.SourceEncoding != "gb18030" || !view.RawAvailable || view.DecodeWarning != "" {
		t.Fatalf("decoded RPC view = %+v", view)
	}
	entry.Sequence, entry.Stream, entry.Content, entry.DisplayText, entry.SourceEncoding, entry.IsBinary = 2, "stderr", []byte{0xff, 0x00}, "", "binary", true
	view = taskV2LogRPCProjection(entry)
	if view.Encoding != "base64" || view.ContentBase64 != base64.StdEncoding.EncodeToString(entry.Content) || view.DecodeWarning != "binary_output" {
		t.Fatalf("binary RPC view = %+v", view)
	}
}

func TestTaskV2LogRPCRejectsLegacySequenceContract(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 12; index++ {
		if _, err := fixture.store.AppendLog(
			t.Context(), created.Definition.ID, nil, "stdout", []byte(strings.Repeat("<", 1024)),
			fixture.now.Add(time.Duration(index+1)*time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
	}
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	legacy := dispatchTaskV2(t, dispatch, "task.logs", map[string]any{
		"taskId": created.Definition.ID, "afterSequence": 0, "limitBytes": 1 << 20,
	}, fixture.project.ID.String())
	if legacy.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE || legacy.GetError().GetSafeMessage() != "UPGRADE_REQUIRED" {
		t.Fatalf("legacy task log request error = %+v", legacy.GetError())
	}
}

func TestTaskV2LogRPCRejectsLegacyTailContract(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	created, err := fixture.store.Create(t.Context(), definition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 64; index++ {
		if _, err := fixture.store.AppendLog(
			t.Context(), created.Definition.ID, nil, "stdout", []byte(strings.Repeat("x", 1024)),
			fixture.now.Add(time.Duration(index+1)*time.Millisecond),
		); err != nil {
			t.Fatal(err)
		}
	}
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	response := dispatchTaskV2(t, dispatch, "task.logs", map[string]any{
		"taskId": created.Definition.ID, "beforeSequence": 0, "limitLines": 64,
	}, fixture.project.ID.String())
	if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE || response.GetError().GetSafeMessage() != "UPGRADE_REQUIRED" {
		t.Fatalf("legacy task log tail error = %+v", response.GetError())
	}
}

func TestTaskV2PeerRPCRequiresBoundPolicyAndSupportsLifecycle(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	if !agentFeatureFlags(fixture.state)["tasks.v2"] || !agentFeatureFlags(fixture.state)["workflow.v2"] ||
		!slices.Contains(agentRegistrationCapabilities(fixture.state), "remote.peer.task.control") {
		t.Fatal("Task v2 capability was not advertised for the enabled project")
	}
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())

	unbound := dispatchTaskV2(t, dispatch, "task.create", map[string]any{"definition": definition}, "")
	if unbound.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH {
		t.Fatalf("unbound create error = %+v", unbound.GetError())
	}
	createdResponse := dispatchTaskV2(t, dispatch, "task.create", map[string]any{"definition": definition}, fixture.project.ID.String())
	if createdResponse.GetError() != nil {
		t.Fatalf("create error = %+v", createdResponse.GetError())
	}
	var created taskV2Record
	if err := json.Unmarshal(createdResponse.GetJsonPayload(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Definition.ID != definition.ID || created.Status != "queued" || created.Revision != 1 ||
		created.LogAvailable || created.LogState != taskLogStateNone || created.LogGeneration != 0 || created.LogSizeBytes != 0 {
		t.Fatalf("created task = %+v", created)
	}

	listResponse := dispatchTaskV2(t, dispatch, "task.list", map[string]any{"limit": 10}, fixture.project.ID.String())
	if listResponse.GetError() != nil || !bytes.Contains(listResponse.GetJsonPayload(), []byte(definition.ID.String())) ||
		!bytes.Contains(listResponse.GetJsonPayload(), []byte("Review the current project")) {
		t.Fatalf("list response = %s, %+v", listResponse.GetJsonPayload(), listResponse.GetError())
	}
	deltaResponse := dispatchTaskV2(t, dispatch, "task.list", map[string]any{"afterRevision": 0, "limit": 10}, fixture.project.ID.String())
	if deltaResponse.GetError() != nil || !bytes.Contains(deltaResponse.GetJsonPayload(), []byte(`"operation":"upsert"`)) {
		t.Fatalf("delta response = %s, %+v", deltaResponse.GetJsonPayload(), deltaResponse.GetError())
	}

	updatedDefinition := definition
	updatedDefinition.Title = "Updated through Task v2"
	staleUpdate := dispatchTaskV2(t, dispatch, "task.update", map[string]any{
		"definition": updatedDefinition, "expectedRevision": created.Revision + 1,
	}, fixture.project.ID.String())
	if staleUpdate.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT {
		t.Fatalf("stale update error = %+v", staleUpdate.GetError())
	}
	updatedResponse := dispatchTaskV2(t, dispatch, "task.update", map[string]any{
		"definition": updatedDefinition, "expectedRevision": created.Revision,
	}, fixture.project.ID.String())
	if updatedResponse.GetError() != nil {
		t.Fatalf("update error = %+v", updatedResponse.GetError())
	}
	var updated taskV2Record
	if err := json.Unmarshal(updatedResponse.GetJsonPayload(), &updated); err != nil {
		t.Fatal(err)
	}

	startedResponse := dispatchTaskV2(t, dispatch, "task.start", map[string]any{
		"taskId": definition.ID, "expectedRevision": updated.Revision,
	}, fixture.project.ID.String())
	if startedResponse.GetError() != nil {
		t.Fatalf("start error = %+v", startedResponse.GetError())
	}
	var waiting taskV2Record
	if err := json.Unmarshal(startedResponse.GetJsonPayload(), &waiting); err != nil {
		t.Fatal(err)
	}
	running, run, err := fixture.state.tasksV2.StartRun(t.Context(), definition.ID, waiting.Revision, fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := fixture.state.tasksV2.OpenRunLogWriter(t.Context(), running, run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(t.Context(), "stdout", "runner output", nil, fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Append(t.Context(), "tool", "", []byte{0xff, 0xfe}, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.state.tasksV2.SealRunLog(t.Context(), running, run); err != nil {
		t.Fatal(err)
	}
	awaiting, _, err := fixture.state.tasksV2.FinishRun(t.Context(), definition.ID, running.Revision, "awaitingAcceptance", 0, "", "session-rpc", fixture.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	logsResponse := dispatchTaskV2(t, dispatch, "task.logs", map[string]any{
		"taskId": definition.ID, "runId": run.ID, "generation": run.LogGeneration, "tailBytes": maximumTaskLogSeekBytes,
	}, fixture.project.ID.String())
	if logsResponse.GetError() != nil || !bytes.Contains(logsResponse.GetJsonPayload(), []byte("runner output")) ||
		!bytes.Contains(logsResponse.GetJsonPayload(), []byte("binary output omitted")) {
		t.Fatalf("logs response = %s, %+v", logsResponse.GetJsonPayload(), logsResponse.GetError())
	}
	acceptedResponse := dispatchTaskV2(t, dispatch, "task.accept", map[string]any{
		"taskId": definition.ID, "expectedRevision": awaiting.Revision, "evidence": "tests passed",
	}, fixture.project.ID.String())
	if acceptedResponse.GetError() != nil {
		t.Fatalf("accept error = %+v", acceptedResponse.GetError())
	}
	var accepted taskV2Record
	if err := json.Unmarshal(acceptedResponse.GetJsonPayload(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Status != "completed" || accepted.ResultCode != "accepted" {
		t.Fatalf("accepted task = %+v", accepted)
	}

	undoResponse := dispatchTaskV2(t, dispatch, "task.undo-acceptance", map[string]any{
		"taskId": definition.ID, "expectedRevision": accepted.Revision,
	}, fixture.project.ID.String())
	if undoResponse.GetError() != nil {
		t.Fatalf("undo acceptance error = %+v", undoResponse.GetError())
	}
	var undone taskV2Record
	if err := json.Unmarshal(undoResponse.GetJsonPayload(), &undone); err != nil {
		t.Fatal(err)
	}
	if undone.Status != "awaitingAcceptance" || undone.ResultCode != "acceptance_undone" {
		t.Fatalf("undone task = %+v", undone)
	}
	reacceptedResponse := dispatchTaskV2(t, dispatch, "task.accept", map[string]any{
		"taskId": definition.ID, "expectedRevision": undone.Revision,
	}, fixture.project.ID.String())
	if reacceptedResponse.GetError() != nil {
		t.Fatalf("re-accept error = %+v", reacceptedResponse.GetError())
	}
	var reaccepted taskV2Record
	if err := json.Unmarshal(reacceptedResponse.GetJsonPayload(), &reaccepted); err != nil {
		t.Fatal(err)
	}

	retryResponse := dispatchTaskV2(t, dispatch, "task.retry", map[string]any{
		"taskId": definition.ID, "expectedRevision": reaccepted.Revision,
	}, fixture.project.ID.String())
	if retryResponse.GetError() != nil {
		t.Fatalf("retry error = %+v", retryResponse.GetError())
	}
	var retried taskV2Record
	if err := json.Unmarshal(retryResponse.GetJsonPayload(), &retried); err != nil {
		t.Fatal(err)
	}
	stoppedResponse := dispatchTaskV2(t, dispatch, "task.stop", map[string]any{
		"taskId": definition.ID, "expectedRevision": retried.Revision,
	}, fixture.project.ID.String())
	if stoppedResponse.GetError() != nil {
		t.Fatalf("stop error = %+v", stoppedResponse.GetError())
	}
	var stopped taskV2Record
	if err := json.Unmarshal(stoppedResponse.GetJsonPayload(), &stopped); err != nil {
		t.Fatal(err)
	}
	deletedResponse := dispatchTaskV2(t, dispatch, "task.delete", map[string]any{
		"taskId": definition.ID, "expectedRevision": stopped.Revision,
	}, fixture.project.ID.String())
	if deletedResponse.GetError() != nil || !bytes.Contains(deletedResponse.GetJsonPayload(), []byte(`"deleted":true`)) {
		t.Fatalf("delete response = %s, %+v", deletedResponse.GetJsonPayload(), deletedResponse.GetError())
	}
}

func TestTaskV2StartNowPromotesQueuedAndReadyWaitingTasksToParallel(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}

	queuedDefinition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	queued, err := fixture.store.Create(t.Context(), queuedDefinition, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	queuedResponse := dispatchTaskV2(t, dispatch, "task.start", map[string]any{
		"taskId": queued.Definition.ID, "expectedRevision": queued.Revision,
	}, fixture.project.ID.String())
	if queuedResponse.GetError() != nil {
		t.Fatalf("queued start error = %+v", queuedResponse.GetError())
	}
	var queuedStarted taskV2Record
	if err := json.Unmarshal(queuedResponse.GetJsonPayload(), &queuedStarted); err != nil {
		t.Fatal(err)
	}
	if queuedStarted.Status != "waiting" || queuedStarted.Definition.Execution.Mode != "parallel" ||
		!queuedStarted.Definition.Execution.RunImmediately || queuedStarted.Definition.Execution.ScheduledAt != nil ||
		queuedStarted.Revision != queued.Revision+1 || queuedStarted.DefinitionRevision != queued.DefinitionRevision+1 {
		t.Fatalf("queued run-now task = %+v", queuedStarted)
	}

	waitingDefinition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	waitingDefinition.Execution.ScheduledAt = nil
	waitingCreated, err := fixture.store.Create(t.Context(), waitingDefinition, fixture.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := fixture.store.Transition(t.Context(), waitingCreated.Definition.ID, waitingCreated.Revision, "waiting", "", fixture.now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	waitingResponse := dispatchTaskV2(t, dispatch, "task.start", map[string]any{
		"taskId": waiting.Definition.ID, "expectedRevision": waiting.Revision,
	}, fixture.project.ID.String())
	if waitingResponse.GetError() != nil {
		t.Fatalf("waiting start error = %+v", waitingResponse.GetError())
	}
	var waitingStarted taskV2Record
	if err := json.Unmarshal(waitingResponse.GetJsonPayload(), &waitingStarted); err != nil {
		t.Fatal(err)
	}
	if waitingStarted.Status != "waiting" || waitingStarted.Definition.Execution.Mode != "parallel" ||
		waitingStarted.Revision != waiting.Revision+1 || waitingStarted.DefinitionRevision != waiting.DefinitionRevision+1 {
		t.Fatalf("waiting concurrent task = %+v", waitingStarted)
	}
	stale := dispatchTaskV2(t, dispatch, "task.start", map[string]any{
		"taskId": waiting.Definition.ID, "expectedRevision": waiting.Revision,
	}, fixture.project.ID.String())
	if stale.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT {
		t.Fatalf("stale waiting start error = %+v", stale.GetError())
	}

	blockedDefinition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	blockedDefinition.Execution.ScheduledAt = nil
	blockedDefinition.Execution.RelatedTaskIDs = []uuid.UUID{queued.Definition.ID}
	blockedCreated, err := fixture.store.Create(t.Context(), blockedDefinition, fixture.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	blockedWaiting, err := fixture.store.Transition(t.Context(), blockedCreated.Definition.ID, blockedCreated.Revision, "waiting", "", fixture.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	blockedResponse := dispatchTaskV2(t, dispatch, "task.start", map[string]any{
		"taskId": blockedWaiting.Definition.ID, "expectedRevision": blockedWaiting.Revision,
	}, fixture.project.ID.String())
	if blockedResponse.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY {
		t.Fatalf("unsatisfied waiting start error = %+v", blockedResponse.GetError())
	}
	unchanged, err := fixture.store.Get(t.Context(), blockedWaiting.Definition.ID)
	if err != nil || unchanged.Revision != blockedWaiting.Revision || unchanged.Definition.Execution.Mode != "serial" {
		t.Fatalf("unsatisfied waiting task changed = %+v, %v", unchanged, err)
	}
}

func TestTaskV2PeerListUsesBoundedVersionedPages(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	for index := 0; index < 21; index++ {
		definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
		if _, err := fixture.store.Create(t.Context(), definition, fixture.now.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatalf("create task %d: %v", index, err)
		}
	}
	first := dispatchTaskV2(t, dispatch, "task.list", map[string]any{"limit": 20}, fixture.project.ID.String())
	if first.GetError() != nil {
		t.Fatalf("first task list error = %+v", first.GetError())
	}
	var firstPage struct {
		Items      []taskV2Record `json:"items"`
		NextCursor *string        `json:"nextCursor"`
		HighWater  uint64         `json:"highWatermark"`
	}
	if err := json.Unmarshal(first.GetJsonPayload(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 20 || firstPage.NextCursor == nil || *firstPage.NextCursor == "" || firstPage.HighWater == 0 {
		t.Fatalf("first task page = %+v", firstPage)
	}
	second := dispatchTaskV2(t, dispatch, "task.list", map[string]any{"limit": 20, "cursor": *firstPage.NextCursor}, fixture.project.ID.String())
	if second.GetError() != nil {
		t.Fatalf("second task list error = %+v", second.GetError())
	}
	var secondPage struct {
		Items      []taskV2Record `json:"items"`
		NextCursor *string        `json:"nextCursor"`
		HighWater  uint64         `json:"highWatermark"`
	}
	if err := json.Unmarshal(second.GetJsonPayload(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 1 || secondPage.NextCursor != nil || secondPage.HighWater != firstPage.HighWater ||
		secondPage.Items[0].Definition.ID == firstPage.Items[0].Definition.ID {
		t.Fatalf("second task page = %+v", secondPage)
	}
}

func TestTaskV2PeerRPCEnforcesImmediatePolicyRevocationAndLargerPrivatePayload(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	rootID := stableProjectID(fixture.state.DeviceID, "")
	root, err := fixture.state.business.projectByID(t.Context(), rootID)
	if err != nil {
		t.Fatal(err)
	}
	rootPolicy := root.Policy
	rootPolicy.AllowTaskExecution = false
	if _, err := fixture.state.business.updateProject(t.Context(), rootID, nil, nil, &rootPolicy, &root.Revision); err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	definition := normalizeTaskV2TestDefinition(t, fixture.project, uuid.New())
	largePrompt := strings.Repeat("p", maximumRPCPayload+1024)
	definition.Config = json.RawMessage(`{"promptSource":"customText","promptText":` + mustJSONQuote(t, largePrompt) + `,"attachedFilePaths":[]}`)
	if _, err := normalizeTaskConfig(fixture.project, definition.Kind, definition.Config); err != nil {
		t.Fatalf("normalize large task config: %v", err)
	}
	definition, err = normalizeTaskV2Definition(fixture.project, definition)
	if err != nil {
		t.Fatal(err)
	}
	largeInput := map[string]any{"definition": definition}
	payload, err := json.Marshal(largeInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newCallEnvelope(uuid.NewString(), "task.create", payload, time.Minute); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("large direct task request was not rejected: %v", err)
	}
	store, err := taskPayloadStoreFor(fixture.state)
	if err != nil {
		t.Fatal(err)
	}
	transferID := uuid.NewString()
	if _, err := store.prepare(fixture.project.ID.String(), rpcInput{
		"transferId": transferID, "targetMethod": "task.create", "totalBytes": float64(len(payload)), "sha256": sha256Hex(payload),
	}); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(payload); offset += taskPayloadChunkBytes {
		end := min(offset+taskPayloadChunkBytes, len(payload))
		chunk := payload[offset:end]
		if _, err := store.chunk(fixture.project.ID.String(), rpcInput{
			"transferId": transferID, "offset": float64(offset), "base64Data": base64.StdEncoding.EncodeToString(chunk), "chunkSha256": sha256Hex(chunk),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.commit(fixture.project.ID.String(), rpcInput{"transferId": transferID, "idempotencyKey": "large-task-create"}); err != nil {
		t.Fatal(err)
	}
	response := dispatchTaskV2(t, dispatch, "task.create", map[string]any{"payloadTransferId": transferID}, fixture.project.ID.String())
	if response.GetError() != nil {
		t.Fatalf("large chunked private create error = %+v", response.GetError())
	}

	disabledPolicy := fixture.project.Policy
	disabledPolicy.AllowTaskExecution = false
	expectedRevision := fixture.project.Revision
	if _, err := fixture.state.business.updateProject(t.Context(), fixture.project.ID, nil, nil, &disabledPolicy, &expectedRevision); err != nil {
		t.Fatal(err)
	}
	denied := dispatchTaskV2(t, dispatch, "task.get", map[string]any{"taskId": definition.ID}, fixture.project.ID.String())
	if denied.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE {
		t.Fatalf("revoked policy error = %+v", denied.GetError())
	}
	if agentFeatureFlags(fixture.state)["tasks.v2"] || slices.Contains(agentRegistrationCapabilities(fixture.state), "remote.peer.task.control") {
		t.Fatal("revoked Task v2 policy remained advertised")
	}

	forged := dispatch
	forged.ticketProjectID = uuid.NewString()
	forgedResponse := dispatchTaskV2(t, forged, "task.get", map[string]any{"taskId": definition.ID}, fixture.project.ID.String())
	if forgedResponse.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH {
		t.Fatalf("forged project error = %+v", forgedResponse.GetError())
	}
}

func TestTaskV2PeerRPCFollowUpIsAtomicAndRunHistoryIsPersisted(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	dispatch := dispatcher{
		state: fixture.state, now: func() time.Time { return time.Now().UTC() }, scope: "remote.peer.task.control",
		ticketProjectID: fixture.project.ID.String(), enforceProjectBinding: true,
	}
	makeAwaiting := func(id uuid.UUID, offset time.Duration) taskV2Record {
		definition := normalizeTaskV2TestDefinition(t, fixture.project, id)
		created, err := fixture.state.tasksV2.Create(t.Context(), definition, fixture.now.Add(offset))
		if err != nil {
			t.Fatal(err)
		}
		waiting, err := fixture.state.tasksV2.Transition(t.Context(), id, created.Revision, "waiting", "", fixture.now.Add(offset+time.Second))
		if err != nil {
			t.Fatal(err)
		}
		running, run, err := fixture.state.tasksV2.StartRun(t.Context(), id, waiting.Revision, fixture.now.Add(offset+2*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		awaiting, _, err := fixture.state.tasksV2.FinishRun(
			t.Context(), id, running.Revision, "awaitingAcceptance", 0, "", "follow-up-session", fixture.now.Add(offset+3*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if awaiting.CurrentRunID == nil || *awaiting.CurrentRunID != run.ID {
			t.Fatalf("awaiting run binding = %+v / %+v", awaiting, run)
		}
		return awaiting
	}

	source := makeAwaiting(uuid.New(), 0)
	runsResponse := dispatchTaskV2(t, dispatch, "task.runs", map[string]any{"taskId": source.Definition.ID}, fixture.project.ID.String())
	if runsResponse.GetError() != nil || !bytes.Contains(runsResponse.GetJsonPayload(), []byte("follow-up-session")) ||
		!bytes.Contains(runsResponse.GetJsonPayload(), []byte(`"attempt":0`)) {
		t.Fatalf("run history = %s, %+v", runsResponse.GetJsonPayload(), runsResponse.GetError())
	}
	followUpID := uuid.New()
	followUpResponse := dispatchTaskV2(t, dispatch, "task.follow-up", map[string]any{
		"sourceTaskId": source.Definition.ID, "taskId": followUpID, "expectedRevision": source.Revision,
		"feedback": "Address the remaining review finding and rerun tests.",
	}, fixture.project.ID.String())
	if followUpResponse.GetError() != nil {
		t.Fatalf("follow-up error = %+v", followUpResponse.GetError())
	}
	var result taskV2FollowUpResult
	if err := json.Unmarshal(followUpResponse.GetJsonPayload(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Source.Status != "changesRequested" || result.Source.ResultCode != "follow_up_created" ||
		result.FollowUp.Definition.ID != followUpID || result.FollowUp.Status != "waiting" ||
		result.FollowUp.LogAvailable || result.FollowUp.LogState != taskLogStateNone || result.FollowUp.LogGeneration != 0 ||
		result.FollowUp.Definition.ParentTaskID == nil || *result.FollowUp.Definition.ParentTaskID != source.Definition.ID ||
		result.FollowUp.Definition.RootTaskID == nil || *result.FollowUp.Definition.RootTaskID != source.Definition.ID ||
		result.FollowUp.Definition.AcceptanceFeedback == "" || result.HighWatermark == 0 {
		t.Fatalf("follow-up result = %+v", result)
	}
	var followUpConfig map[string]any
	if err := json.Unmarshal(result.FollowUp.Definition.Config, &followUpConfig); err != nil {
		t.Fatal(err)
	}
	if followUpConfig["promptSource"] != "customText" || !strings.Contains(followUpConfig["promptText"].(string), "remaining review finding") {
		t.Fatalf("follow-up config = %#v", followUpConfig)
	}
	duplicate := dispatchTaskV2(t, dispatch, "task.follow-up", map[string]any{
		"sourceTaskId": source.Definition.ID, "taskId": followUpID, "expectedRevision": source.Revision,
		"feedback": "must not duplicate",
	}, fixture.project.ID.String())
	if duplicate.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT {
		t.Fatalf("duplicate follow-up error = %+v", duplicate.GetError())
	}

	rollbackSource := makeAwaiting(uuid.New(), 10*time.Second)
	occupiedID := uuid.New()
	occupiedDefinition := normalizeTaskV2TestDefinition(t, fixture.project, occupiedID)
	if _, err := fixture.state.tasksV2.Create(t.Context(), occupiedDefinition, fixture.now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	conflict := dispatchTaskV2(t, dispatch, "task.follow-up", map[string]any{
		"sourceTaskId": rollbackSource.Definition.ID, "taskId": occupiedID, "expectedRevision": rollbackSource.Revision,
		"feedback": "this transaction must roll back",
	}, fixture.project.ID.String())
	if conflict.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT {
		t.Fatalf("occupied follow-up error = %+v", conflict.GetError())
	}
	rolledBack, err := fixture.state.tasksV2.Get(t.Context(), rollbackSource.Definition.ID)
	if err != nil || rolledBack.Status != "awaitingAcceptance" || rolledBack.Revision != rollbackSource.Revision {
		t.Fatalf("source after rolled-back follow-up = %+v, %v", rolledBack, err)
	}
	items, err := fixture.state.tasksV2.List(t.Context(), fixture.project.ID)
	if err != nil || len(items) != 4 {
		t.Fatalf("tasks after follow-up = %d, %v", len(items), err)
	}
}

func TestTaskV2MethodsKeepLegacyScopesCompatibleButRequirePeerScopeForNewOperations(t *testing.T) {
	if methodScope("task.list") != "remote.task.read" || methodScope("task.create") != "remote.task.write" {
		t.Fatal("legacy task method scopes changed")
	}
	for _, method := range []string{"task.list", "task.get", "task.logs", "task.create", "task.cancel"} {
		if !methodAllowsScope(method, "remote.peer.task.control") {
			t.Errorf("Task v2 peer scope does not allow %q", method)
		}
	}
	for _, method := range []string{"task.update", "task.start", "task.stop", "task.retry", "task.delete", "task.clear", "task.queue.start", "task.queue.stop", "task.accept", "task.undo-acceptance", "task.runs", "task.follow-up"} {
		if methodScope(method) != "remote.peer.task.control" || methodAllowsScope(method, "remote.task.write") || methodAllowsScope(method, "remote.task.read") {
			t.Errorf("new Task v2 method %q has unsafe scope mapping", method)
		}
	}
}

func dispatchTaskV2(t *testing.T, dispatch dispatcher, method string, input any, projectID string) *remotev1.RpcResponse {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := newCallEnvelope(uuid.NewString(), method, payload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	envelope.GetRequest().GetHeader().ProjectId = projectID
	return dispatch.dispatch(t.Context(), envelope).GetResponse()
}

func mustJSONQuote(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
