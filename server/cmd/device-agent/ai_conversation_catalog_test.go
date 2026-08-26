package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func TestAIConversationCatalogV2ProjectsTerminalPreviewWithoutDeltaChanges(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	created, err := fixture.state.business.createAIConversation(
		t.Context(), fixture.project.ID, "", "Catalog projection", "readOnly", config, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID: fixture.project.ID,
		Limit:     30,
	})
	if err != nil || len(initial.Items) != 1 || initial.Items[0].ID != created.ID || initial.HighWatermark == 0 {
		t.Fatalf("initial catalog=%+v error=%v", initial, err)
	}

	turn, err := fixture.state.business.beginAIConversationTurn(
		t.Context(), fixture.project.ID, created.ID, uuid.NewString(), "start the catalog", "readOnly", nil, config,
		fixture.now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	afterInitial := initial.HighWatermark
	started, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID:     fixture.project.ID,
		Limit:         30,
		AfterRevision: &afterInitial,
	})
	if err != nil || len(started.Changes) != 1 || started.Changes[0].Operation != "upsert" ||
		started.Changes[0].Item == nil || started.Changes[0].Item.Status != "generating" ||
		started.Changes[0].Item.LatestMessage == nil || started.Changes[0].Item.LatestMessage.Preview != "start the catalog" {
		t.Fatalf("start changes=%+v error=%v", started, err)
	}

	body := strings.Repeat("🧪", maximumAIConversationPreviewCodePoints+80)
	if _, _, err := fixture.state.business.appendAIConversationTextDelta(
		t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, body,
		fixture.now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.state.business.appendAIConversationReasoningDelta(
		t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, "private reasoning",
		fixture.now.Add(2*time.Second+time.Millisecond),
	); err != nil {
		t.Fatal(err)
	}

	// Streaming deltas are for the visible chat only. The catalog journal must
	// still contain exactly the generation-start change at this point.
	afterDeltas, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID:     fixture.project.ID,
		Limit:         30,
		AfterRevision: &afterInitial,
	})
	if err != nil || len(afterDeltas.Changes) != 1 || afterDeltas.HighWatermark != started.HighWatermark {
		t.Fatalf("delta changed catalog journal=%+v error=%v", afterDeltas, err)
	}

	if _, _, _, err := fixture.state.business.finishAIConversationTurn(
		t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, chatUsage{}, chatProviderRun{},
		fixture.now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	completed, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID:     fixture.project.ID,
		Limit:         30,
		AfterRevision: &afterInitial,
	})
	if err != nil || len(completed.Changes) != 2 || completed.Changes[1].Sequence <= completed.Changes[0].Sequence {
		t.Fatalf("terminal catalog changes=%+v error=%v", completed, err)
	}
	terminal := completed.Changes[1].Item
	if completed.Changes[1].Operation != "upsert" || terminal == nil || terminal.Status != "completed" ||
		terminal.LastCompletedAssistantSequence != turn.Assistant.Sequence || terminal.LatestMessage == nil ||
		terminal.LatestMessage.Sequence != turn.Assistant.Sequence {
		t.Fatalf("terminal summary=%+v", completed.Changes[1])
	}
	preview := terminal.LatestMessage.Preview
	if !utf8.ValidString(preview) || utf8.RuneCountInString(preview) > maximumAIConversationPreviewCodePoints ||
		len(preview) > maximumAIConversationPreviewBytes || !strings.HasSuffix(preview, "\u2026") {
		t.Fatalf("unsafe or unbounded preview: %q (%d bytes, %d code points)", preview, len(preview), utf8.RuneCountInString(preview))
	}

	db, err := fixture.state.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var payload string
	if err := db.QueryRowContext(t.Context(), `SELECT payload_json FROM ai_conversation_changes
		WHERE project_id=? ORDER BY sequence DESC LIMIT 1`, fixture.project.ID.String()).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, body) || strings.Contains(payload, "modelBinding") || strings.Contains(payload, "reasoning") {
		t.Fatalf("catalog journal retained detail-only data: %s", payload)
	}
}

func TestAIConversationCatalogV2KeysetSnapshotUsesCatalogIndex(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	created := make([]conversationView, 0, 3)
	for index := 0; index < 3; index++ {
		value, err := fixture.state.business.createAIConversation(
			t.Context(), fixture.project.ID, "", "Keyset "+string(rune('A'+index)), "readOnly", config,
			fixture.now.Add(time.Duration(index)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, value)
	}
	first, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID: fixture.project.ID,
		Limit:     2,
	})
	if err != nil || len(first.Items) != 2 || first.NextCursor == nil || first.NextCursor.SnapshotRevision != first.HighWatermark {
		t.Fatalf("first keyset page=%+v error=%v", first, err)
	}

	newer, err := fixture.state.business.createAIConversation(
		t.Context(), fixture.project.ID, "", "Arrived after snapshot", "readOnly", config, fixture.now.Add(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID: fixture.project.ID,
		Cursor:    first.NextCursor,
		Limit:     2,
	})
	if err != nil || len(second.Items) != 1 || second.Items[0].ID == newer.ID {
		t.Fatalf("second keyset page=%+v error=%v", second, err)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, second.Items...) {
		if seen[item.ID] {
			t.Fatalf("duplicate keyset item %s", item.ID)
		}
		seen[item.ID] = true
	}
	for _, value := range created {
		if !seen[value.ID] {
			t.Fatalf("snapshot permanently omitted %s", value.ID)
		}
	}
	afterSnapshot := first.HighWatermark
	changes, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID:     fixture.project.ID,
		Limit:         30,
		AfterRevision: &afterSnapshot,
	})
	if err != nil || len(changes.Changes) != 1 || changes.Changes[0].ConversationID != newer.ID {
		t.Fatalf("post-snapshot changes=%+v error=%v", changes, err)
	}

	db, err := fixture.state.business.openReadDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+aiConversationCatalogSelect+`
		WHERE device_id=? AND project_id=? AND revision<=?
		ORDER BY updated_at_ms DESC,id DESC LIMIT ?`, fixture.state.DeviceID.String(), fixture.project.ID.String(), first.HighWatermark, 31)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, ignored int
		var detail string
		if err := rows.Scan(&id, &parent, &ignored, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.ToLower(strings.Join(details, "\n"))
	if !strings.Contains(plan, "ai_conversations_project_updated_idx") || strings.Contains(plan, "ai_messages") || strings.Contains(plan, "temp b-tree") {
		t.Fatalf("catalog query plan=%s", plan)
	}
}

func TestAIConversationCatalogV2FirstPageStaysBoundedAtTenThousandConversations(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	bindingJSON, err := marshalAIJSON(aiConversationBinding(config), 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := tx.PrepareContext(t.Context(), `INSERT INTO ai_conversation_changes(
		project_id,conversation_id,deleted,payload_json,occurred_at_ms
	) VALUES(?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	conversations, err := tx.PrepareContext(t.Context(), `INSERT INTO ai_conversations(
		id,device_id,project_id,revision,title,config_id,model_binding_json,workspace_mode,state,
		generation_id,active_assistant_id,last_message_sequence,message_count,created_at_ms,updated_at_ms
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = changes.Close()
		_ = tx.Rollback()
		t.Fatal(err)
	}
	startedAt := fixture.now.UTC().UnixMilli()
	for index := 0; index < 10_000; index++ {
		id := uuid.NewString()
		result, insertErr := changes.ExecContext(t.Context(), fixture.project.ID.String(), id, 0, `{}`, startedAt+int64(index))
		if insertErr != nil {
			_ = conversations.Close()
			_ = changes.Close()
			_ = tx.Rollback()
			t.Fatal(insertErr)
		}
		sequence, insertErr := result.LastInsertId()
		if insertErr != nil || sequence < 1 {
			_ = conversations.Close()
			_ = changes.Close()
			_ = tx.Rollback()
			t.Fatalf("catalog change sequence=%d error=%v", sequence, insertErr)
		}
		if _, insertErr = conversations.ExecContext(
			t.Context(), id, fixture.state.DeviceID.String(), fixture.project.ID.String(), sequence,
			fmt.Sprintf("Scale %05d", index), config.ID, bindingJSON, "readOnly", "idle", "", "", 0, 0,
			startedAt, startedAt+int64(index),
		); insertErr != nil {
			_ = conversations.Close()
			_ = changes.Close()
			_ = tx.Rollback()
			t.Fatal(insertErr)
		}
	}
	if err := conversations.Close(); err != nil {
		_ = changes.Close()
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := changes.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	page, err := fixture.state.business.listAIConversationCatalog(t.Context(), aiConversationCatalogListOptions{
		ProjectID: fixture.project.ID,
		Limit:     30,
	})
	if err != nil || len(page.Items) != 30 || page.NextCursor == nil || page.HighWatermark != 10_000 {
		t.Fatalf("10k first page=%+v error=%v", page, err)
	}
}

func TestAIConversationCatalogSummaryRPCIsCompact(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	created, err := fixture.state.business.createAIConversation(
		t.Context(), fixture.project.ID, "", "Summary RPC", "readOnly", config, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: fixture.state, requestProjectID: fixture.project.ID.String()}
	value, revision, err := dispatch.callAIConversationRPC(t.Context(), "conversation.summary.get", rpcInput{
		"conversationId": created.ID,
		"catalogVersion": float64(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("summary response type=%T", value)
	}
	item, ok := response["item"].(aiConversationListItem)
	if !ok || item.ID != created.ID || item.Revision != revision {
		t.Fatalf("summary response=%#v revision=%d", response, revision)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"modelBinding", "configId", "workspaceMode", "reasoning", "attachments"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("summary RPC leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestAIConversationCatalogV13MigrationBackfillsExistingProjection(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	created, err := fixture.state.business.createAIConversation(
		t.Context(), fixture.project.ID, "", "Migration projection", "readOnly", config, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := fixture.state.business.beginAIConversationTurn(
		t.Context(), fixture.project.ID, created.ID, uuid.NewString(), "historic prompt", "readOnly", nil, config,
		fixture.now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.state.business.appendAIConversationTextDelta(
		t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, "  historic\npreview  ",
		fixture.now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fixture.state.business.finishAIConversationTurn(
		t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, chatUsage{}, chatProviderRun{},
		fixture.now.Add(3*time.Second),
	); err != nil {
		t.Fatal(err)
	}

	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE ai_conversations SET
		latest_message_sequence=0, latest_message_role='', latest_message_status='', latest_message_preview='',
		latest_message_created_at_ms=0, last_completed_assistant_sequence=0, last_error_code='' WHERE id=?`, created.ID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		tx, err = db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrateAIConversationSchemaV13(t.Context(), tx); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	item, err := fixture.state.business.getAIConversationCatalogItem(t.Context(), fixture.project.ID, created.ID)
	if err != nil || item.LatestMessage == nil || item.LatestMessage.Preview != "historic preview" ||
		item.LastCompletedAssistantSequence != turn.Assistant.Sequence || item.Status != "completed" {
		t.Fatalf("backfilled catalog item=%+v error=%v", item, err)
	}

	rows, err := db.QueryContext(t.Context(), `PRAGMA table_info(ai_conversations)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&id, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"latest_message_sequence", "latest_message_role", "latest_message_status", "latest_message_preview",
		"latest_message_created_at_ms", "last_completed_assistant_sequence", "last_error_code",
	} {
		if !columns[column] {
			t.Fatalf("V13 column %q is missing", column)
		}
	}
}

func TestAIConversationCatalogResponseByteBudgetKeepsContinuations(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	items := make([]aiConversationListItem, 0, maximumAIConversationPage)
	changes := make([]aiConversationCatalogChange, 0, maximumAIConversationPage)
	for index := 0; index < maximumAIConversationPage; index++ {
		item := aiConversationListItem{
			ID: uuid.NewString(), Revision: uint64(index + 1), Title: strings.Repeat("t", maximumAIConversationTitleBytes),
			MessageCount: 2, Status: "completed", LastMessageSequence: 2, LastCompletedAssistantSequence: 2,
			LatestMessage: &aiConversationLatestMessage{
				Sequence: 2, Role: "assistant", Status: "complete",
				Preview: strings.Repeat("界", maximumAIConversationPreviewCodePoints), CreatedAt: now,
			},
			UpdatedAt: now.Add(-time.Duration(index) * time.Second),
		}
		items = append(items, item)
		copy := item
		changes = append(changes, aiConversationCatalogChange{
			Sequence: uint64(index + 1), Operation: "upsert", ConversationID: copy.ID, Item: &copy,
		})
	}

	snapshot, err := boundAIConversationCatalogSnapshotPage(aiConversationCatalogPage{
		Items: items, Changes: []aiConversationCatalogChange{}, AckedThroughSequence: 999, HighWatermark: 999,
		MinimumAvailableSequence: 1,
	})
	if err != nil || len(snapshot.Items) == 0 || len(snapshot.Items) >= len(items) || snapshot.NextCursor == nil {
		t.Fatalf("bounded snapshot=%+v error=%v", snapshot, err)
	}
	if fits, err := aiConversationCatalogResponseFits(snapshot); err != nil || !fits {
		t.Fatalf("bounded snapshot still exceeds response budget: fits=%v error=%v", fits, err)
	}
	last := snapshot.Items[len(snapshot.Items)-1]
	if snapshot.NextCursor.ID != last.ID || snapshot.NextCursor.SnapshotRevision != 999 {
		t.Fatalf("snapshot continuation=%+v last=%+v", snapshot.NextCursor, last)
	}

	incremental, err := boundAIConversationCatalogChangesPage(aiConversationCatalogPage{
		Items: []aiConversationListItem{}, Changes: changes, AckedThroughSequence: uint64(len(changes)),
		HighWatermark: uint64(len(changes)), MinimumAvailableSequence: 1,
	}, 0)
	if err != nil || len(incremental.Changes) == 0 || len(incremental.Changes) >= len(changes) || !incremental.HasMoreChanges {
		t.Fatalf("bounded changes=%+v error=%v", incremental, err)
	}
	if incremental.AckedThroughSequence != incremental.Changes[len(incremental.Changes)-1].Sequence {
		t.Fatalf("incremental ack advanced past emitted changes: %+v", incremental)
	}
	if fits, err := aiConversationCatalogResponseFits(incremental); err != nil || !fits {
		t.Fatalf("bounded changes still exceed response budget: fits=%v error=%v", fits, err)
	}
}

func TestAIConversationLegacyResponseByteBudgetKeepsOffsetContinuation(t *testing.T) {
	items := make([]conversationView, 0, 30)
	for index := range 30 {
		items = append(items, conversationView{
			ID: uuid.NewString(), ProjectID: uuid.NewString(), Revision: uint64(index + 1),
			Title: fmt.Sprintf("legacy-%02d", index), ConfigID: "default",
			ModelBinding:  aiModelBinding{ConfigID: "default", ConfigRevision: 1, Provider: "openai", Model: "model"},
			WorkspaceMode: "readOnly", CreatedAt: time.Unix(int64(index+1), 0).UTC(), UpdatedAt: time.Unix(int64(index+1), 0).UTC(),
			State: "idle", Todos: []aiTodoItem{
				{Content: strings.Repeat("x", 1000), Status: "pending"},
				{Content: strings.Repeat("y", 1000), Status: "pending"},
			},
		})
	}
	page := aiConversationListResult{
		Items: items, Changes: []aiConversationChange{}, NextOffset: len(items),
		HighWatermark: 91, LatestSequence: 91,
	}
	payload, err := boundLegacyAIConversationPage(page, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	selected := payload["items"].([]conversationView)
	if len(encoded) > preferredRPCPagePayload || len(selected) == 0 || len(selected) >= len(items) {
		t.Fatalf("bounded legacy page bytes=%d items=%d/%d", len(encoded), len(selected), len(items))
	}
	cursor, ok := payload["nextCursor"].(*string)
	if !ok || cursor == nil {
		t.Fatalf("bounded legacy page cursor=%#v", payload["nextCursor"])
	}
	next, err := decodeAIPageOffset(rpcInput{"cursor": *cursor})
	if err != nil || next != len(selected) {
		t.Fatalf("bounded legacy continuation=%d, want %d (error=%v)", next, len(selected), err)
	}
}

func TestAIConversationCatalogKeepsUserPreviewForEmptyFailureAndBackfill(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	config := installTestAIConfig(fixture.state)
	created, err := fixture.state.business.createAIConversation(
		t.Context(), fixture.project.ID, "", "Failure preview", "readOnly", config, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := fixture.state.business.beginAIConversationTurn(
		t.Context(), fixture.project.ID, created.ID, uuid.NewString(), "keep this preview", "readOnly", nil, config,
		fixture.now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := fixture.state.business.abortAIConversationTurn(
		t.Context(), fixture.project.ID, created.ID, turn.GenerationID, turn.Assistant.ID, "failed", "provider_unavailable",
		chatProviderRun{}, fixture.now.Add(2*time.Second),
	); err != nil {
		t.Fatal(err)
	}
	assertFailurePreview := func() {
		item, itemErr := fixture.state.business.getAIConversationCatalogItem(t.Context(), fixture.project.ID, created.ID)
		if itemErr != nil || item.Status != "error" || item.LatestMessage == nil ||
			item.LatestMessage.Sequence != turn.User.Sequence || item.LatestMessage.Role != "user" ||
			item.LatestMessage.Preview != "keep this preview" {
			t.Fatalf("failure catalog item=%+v error=%v", item, itemErr)
		}
		view, viewErr := fixture.state.business.getAIConversation(t.Context(), fixture.project.ID, created.ID)
		if viewErr != nil || view.Catalog.LastErrorCode != "provider_unavailable" {
			t.Fatalf("failure catalog projection=%+v error=%v", view.Catalog, viewErr)
		}
	}
	assertFailurePreview()

	db, err := fixture.state.business.openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE ai_conversations SET
		latest_message_sequence=0, latest_message_role='', latest_message_status='', latest_message_preview='',
		latest_message_created_at_ms=0, last_completed_assistant_sequence=0, last_error_code='' WHERE id=?`, created.ID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := migrateAIConversationSchemaV13(t.Context(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertFailurePreview()
}
