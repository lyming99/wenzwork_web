package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	aiInboxNextTurn = "nextTurn"
	aiInboxNextStep = "nextStep"

	maximumAIAgentInboxItems = 100
	maximumAIAgentInboxBytes = 256 << 10
)

type aiAgentPhase string

const (
	aiAgentPhaseMaintenance aiAgentPhase = "maintenance"
	aiAgentPhaseRunning     aiAgentPhase = "running"
	aiAgentPhaseStopping    aiAgentPhase = "stopping"
)

type aiAgentInboxItem struct {
	ID                    string                    `json:"id"`
	ConversationID        string                    `json:"conversationId"`
	Destination           string                    `json:"destination"`
	Prompt                string                    `json:"prompt"`
	Attachments           []chatAttachmentReference `json:"attachments"`
	WorkspaceMode         string                    `json:"workspaceMode"`
	WorkspaceToolsEnabled bool                      `json:"enableWorkspaceTools"`
	State                 string                    `json:"state"`
	ClaimedGenerationID   string                    `json:"claimedGenerationId,omitempty"`
	Sequence              uint64                    `json:"sequence"`
	CreatedAt             time.Time                 `json:"createdAt"`
	UpdatedAt             time.Time                 `json:"updatedAt"`
	Replayed              bool                      `json:"replayed,omitempty"`
}

func validAIInboxDestination(value string) bool {
	return value == aiInboxNextTurn || value == aiInboxNextStep
}

func validateAIAgentInboxItem(item aiAgentInboxItem) error {
	if uuid.Validate(item.ID) != nil || uuid.Validate(item.ConversationID) != nil ||
		!validAIInboxDestination(item.Destination) || !validAIWorkspaceMode(item.WorkspaceMode) ||
		(strings.TrimSpace(item.Prompt) == "" && len(item.Attachments) == 0) ||
		len(item.Prompt) > 32<<10 || !utf8.ValidString(item.Prompt) || len(item.Attachments) > 8 ||
		item.CreatedAt.IsZero() {
		return errRPCInvalid
	}
	encoded, err := json.Marshal(item.Attachments)
	if err != nil || len(encoded)+len(item.Prompt) > maximumAIAgentInboxBytes {
		return errRPCInvalid
	}
	return nil
}

func (store *businessStore) enqueueAIAgentInboxItem(ctx context.Context, projectID uuid.UUID, item aiAgentInboxItem) (aiAgentInboxItem, error) {
	if store == nil || projectID == uuid.Nil || validateAIAgentInboxItem(item) != nil {
		return aiAgentInboxItem{}, errRPCInvalid
	}
	attachmentsJSON, err := json.Marshal(item.Attachments)
	if err != nil {
		return aiAgentInboxItem{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return aiAgentInboxItem{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return aiAgentInboxItem{}, err
	}
	defer tx.Rollback()
	existing, found, err := queryAIAgentInboxItem(ctx, tx, item.ID)
	if err != nil {
		return aiAgentInboxItem{}, err
	}
	if found {
		if existing.ConversationID != item.ConversationID || existing.Destination != item.Destination ||
			existing.Prompt != item.Prompt || existing.WorkspaceMode != item.WorkspaceMode ||
			existing.WorkspaceToolsEnabled != item.WorkspaceToolsEnabled ||
			!slices.Equal(existing.Attachments, item.Attachments) {
			return aiAgentInboxItem{}, errRPCRevision
		}
		existing.Replayed = true
		return existing, nil
	}
	var conversationExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_conversations
        WHERE id=? AND device_id=? AND project_id=?`, item.ConversationID, store.deviceID.String(), projectID.String()).Scan(&conversationExists); err != nil {
		return aiAgentInboxItem{}, err
	}
	if conversationExists != 1 {
		return aiAgentInboxItem{}, errRPCNotFound
	}
	var count int
	var pendingBytes int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(length(prompt)+length(attachments_json)),0) FROM ai_agent_inbox
        WHERE device_id=? AND project_id=? AND conversation_id=? AND state='pending'`,
		store.deviceID.String(), projectID.String(), item.ConversationID).Scan(&count, &pendingBytes); err != nil {
		return aiAgentInboxItem{}, err
	}
	if count >= maximumAIAgentInboxItems || pendingBytes+len(item.Prompt)+len(attachmentsJSON) > maximumAIAgentInboxBytes {
		return aiAgentInboxItem{}, errRPCBusy
	}
	now := item.CreatedAt.UTC()
	result, err := tx.ExecContext(ctx, `INSERT INTO ai_agent_inbox(
		item_id,device_id,project_id,conversation_id,destination,prompt,attachments_json,workspace_mode,workspace_tools_enabled,state,
		claimed_generation_id,created_at_ms,updated_at_ms) VALUES(?,?,?,?,?,?,?,?,?, 'pending','',?,?)`,
		item.ID, store.deviceID.String(), projectID.String(), item.ConversationID, item.Destination, item.Prompt,
		string(attachmentsJSON), aiWorkspaceModeForStorage(item.WorkspaceMode), aiBoolInt(item.WorkspaceToolsEnabled), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return aiAgentInboxItem{}, err
	}
	sequence, err := result.LastInsertId()
	if err != nil || sequence < 1 {
		return aiAgentInboxItem{}, firstError(err, errRPCRevision)
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiAgentInboxItem{}, err
	}
	item.Sequence = uint64(sequence)
	item.State = "pending"
	item.CreatedAt, item.UpdatedAt = now, now
	item.Attachments = append([]chatAttachmentReference(nil), item.Attachments...)
	return item, nil
}

func (store *businessStore) listAIAgentInbox(ctx context.Context, projectID uuid.UUID, conversationID string) ([]aiAgentInboxItem, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil {
		return nil, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT sequence,item_id,conversation_id,destination,prompt,attachments_json,
		workspace_mode,workspace_tools_enabled,state,claimed_generation_id,created_at_ms,updated_at_ms FROM ai_agent_inbox
        WHERE device_id=? AND project_id=? AND conversation_id=? AND state='pending' ORDER BY sequence LIMIT ?`,
		store.deviceID.String(), projectID.String(), conversationID, maximumAIAgentInboxItems)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAIAgentInboxRows(rows)
}

func (store *businessStore) findAIConversationUserMessage(ctx context.Context, projectID uuid.UUID, conversationID, messageID string) (chatMessage, bool, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(messageID) != nil {
		return chatMessage{}, false, errRPCInvalid
	}
	db, err := store.openReadDB()
	if err != nil {
		return chatMessage{}, false, err
	}
	defer db.Close()
	message, err := scanAIMessage(db.QueryRowContext(ctx, `SELECT m.id,m.revision,m.sequence,m.role,m.content,m.status,m.error_code,
        m.attachments_json,m.reasoning,m.tool_runs_json,m.usage_json,m.provider_run_json,m.generation_id,m.created_at_ms
        FROM ai_messages m JOIN ai_conversations c ON c.id=m.conversation_id
        WHERE m.conversation_id=? AND m.id=? AND c.device_id=? AND c.project_id=? AND m.role='user'`,
		conversationID, messageID, store.deviceID.String(), projectID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return chatMessage{}, false, nil
	}
	return message, err == nil, err
}

func (store *businessStore) claimAIAgentInbox(ctx context.Context, projectID uuid.UUID, conversationID, boundary, generationID string, now time.Time) ([]aiAgentInboxItem, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil ||
		!validAIInboxDestination(boundary) || now.IsZero() {
		return nil, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	items, err := selectPendingAIAgentInbox(ctx, tx, store.deviceID.String(), projectID.String(), conversationID, aiInboxNextStep, maximumAIAgentInboxItems)
	if err != nil {
		return nil, err
	}
	if boundary == aiInboxNextTurn {
		turns, selectErr := selectPendingAIAgentInbox(ctx, tx, store.deviceID.String(), projectID.String(), conversationID, aiInboxNextTurn, 1)
		if selectErr != nil {
			return nil, selectErr
		}
		items = append(items, turns...)
	}
	for index := range items {
		result, updateErr := tx.ExecContext(ctx, `UPDATE ai_agent_inbox SET state='claimed',claimed_generation_id=?,updated_at_ms=?
            WHERE sequence=? AND state='pending'`, generationID, now.UTC().UnixMilli(), items[index].Sequence)
		if updateErr != nil {
			return nil, updateErr
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return nil, errRPCRevision
		}
		items[index].State = "claimed"
		items[index].ClaimedGenerationID = generationID
		items[index].UpdatedAt = now.UTC()
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return nil, err
	}
	return items, nil
}

func (store *businessStore) replaceAIAgentInboxItem(ctx context.Context, projectID uuid.UUID, item aiAgentInboxItem, now time.Time) (aiAgentInboxItem, error) {
	if store == nil || projectID == uuid.Nil || validateAIAgentInboxItem(item) != nil || now.IsZero() {
		return aiAgentInboxItem{}, errRPCInvalid
	}
	attachmentsJSON, err := json.Marshal(item.Attachments)
	if err != nil {
		return aiAgentInboxItem{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return aiAgentInboxItem{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return aiAgentInboxItem{}, err
	}
	defer tx.Rollback()
	var otherBytes int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(prompt)+length(attachments_json)),0) FROM ai_agent_inbox
        WHERE device_id=? AND project_id=? AND conversation_id=? AND state='pending' AND item_id<>?`, store.deviceID.String(),
		projectID.String(), item.ConversationID, item.ID).Scan(&otherBytes); err != nil {
		return aiAgentInboxItem{}, err
	}
	if otherBytes+len(item.Prompt)+len(attachmentsJSON) > maximumAIAgentInboxBytes {
		return aiAgentInboxItem{}, errRPCBusy
	}
	result, err := tx.ExecContext(ctx, `UPDATE ai_agent_inbox SET destination=?,prompt=?,attachments_json=?,workspace_mode=?,workspace_tools_enabled=?,updated_at_ms=?
		WHERE item_id=? AND device_id=? AND project_id=? AND conversation_id=? AND state='pending'`, item.Destination,
		item.Prompt, string(attachmentsJSON), aiWorkspaceModeForStorage(item.WorkspaceMode), aiBoolInt(item.WorkspaceToolsEnabled), now.UTC().UnixMilli(), item.ID,
		store.deviceID.String(), projectID.String(), item.ConversationID)
	if err != nil {
		return aiAgentInboxItem{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return aiAgentInboxItem{}, errRPCNotFound
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiAgentInboxItem{}, err
	}
	item.State, item.UpdatedAt = "pending", now.UTC()
	return item, nil
}

func (store *businessStore) removeAIAgentInboxItem(ctx context.Context, projectID uuid.UUID, conversationID, itemID string, now time.Time) (bool, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(itemID) != nil || now.IsZero() {
		return false, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return false, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE ai_agent_inbox SET state='cancelled',updated_at_ms=?
        WHERE item_id=? AND device_id=? AND project_id=? AND conversation_id=? AND state='pending'`, now.UTC().UnixMilli(),
		itemID, store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return false, err
	}
	return affected == 1, nil
}

func (store *businessStore) clearAIAgentInbox(ctx context.Context, projectID uuid.UUID, conversationID string, now time.Time) (uint64, error) {
	if store == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || now.IsZero() {
		return 0, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE ai_agent_inbox SET state='cancelled',updated_at_ms=?
        WHERE device_id=? AND project_id=? AND conversation_id=? AND state='pending'`, now.UTC().UnixMilli(),
		store.deviceID.String(), projectID.String(), conversationID)
	if err != nil {
		return 0, err
	}
	affected, _ := result.RowsAffected()
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return 0, err
	}
	return uint64(max(int64(0), affected)), nil
}

func queryAIAgentInboxItem(ctx context.Context, tx *sql.Tx, itemID string) (aiAgentInboxItem, bool, error) {
	item, err := scanAIAgentInboxRow(tx.QueryRowContext(ctx, `SELECT sequence,item_id,conversation_id,destination,prompt,
		attachments_json,workspace_mode,workspace_tools_enabled,state,claimed_generation_id,created_at_ms,updated_at_ms
        FROM ai_agent_inbox WHERE item_id=?`, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return aiAgentInboxItem{}, false, nil
	}
	return item, err == nil, err
}

type aiInboxScanner interface {
	Scan(...any) error
}

func scanAIAgentInboxRow(scanner aiInboxScanner) (aiAgentInboxItem, error) {
	var item aiAgentInboxItem
	var attachmentsJSON string
	var workspaceToolsEnabled int
	var createdAt, updatedAt int64
	if err := scanner.Scan(&item.Sequence, &item.ID, &item.ConversationID, &item.Destination, &item.Prompt,
		&attachmentsJSON, &item.WorkspaceMode, &workspaceToolsEnabled, &item.State, &item.ClaimedGenerationID, &createdAt, &updatedAt); err != nil {
		return aiAgentInboxItem{}, err
	}
	if err := json.Unmarshal([]byte(attachmentsJSON), &item.Attachments); err != nil {
		return aiAgentInboxItem{}, fmt.Errorf("decode AI Agent inbox attachments: %w", err)
	}
	if item.Attachments == nil {
		item.Attachments = []chatAttachmentReference{}
	}
	item.WorkspaceMode = normalizeAIWorkspaceMode(item.WorkspaceMode)
	item.WorkspaceToolsEnabled = workspaceToolsEnabled == 1
	item.CreatedAt, item.UpdatedAt = time.UnixMilli(createdAt).UTC(), time.UnixMilli(updatedAt).UTC()
	return item, nil
}

func scanAIAgentInboxRows(rows *sql.Rows) ([]aiAgentInboxItem, error) {
	items := make([]aiAgentInboxItem, 0)
	for rows.Next() {
		item, err := scanAIAgentInboxRow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func selectPendingAIAgentInbox(ctx context.Context, tx *sql.Tx, deviceID, projectID, conversationID, destination string, limit int) ([]aiAgentInboxItem, error) {
	rows, err := tx.QueryContext(ctx, `SELECT sequence,item_id,conversation_id,destination,prompt,attachments_json,
		workspace_mode,workspace_tools_enabled,state,claimed_generation_id,created_at_ms,updated_at_ms FROM ai_agent_inbox
        WHERE device_id=? AND project_id=? AND conversation_id=? AND state='pending' AND destination=?
        ORDER BY sequence LIMIT ?`, deviceID, projectID, conversationID, destination, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAIAgentInboxRows(rows)
}

func (state *agentState) aiConversationDriverLock(conversationID string) func() {
	if state == nil {
		return func() {}
	}
	value, _ := state.aiDriverLocks.LoadOrStore(conversationID, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// enqueueForActiveAIGeneration serializes the in-memory phase transition with
// the durable insert. A cancellation that clears the inbox therefore happens
// wholly before or wholly after this offer; input arriving after stopping has
// begun is retargeted and latches exactly one replacement wake-up.
func (d dispatcher) enqueueForActiveAIGeneration(ctx context.Context, projectID uuid.UUID, item aiAgentInboxItem) (aiAgentInboxItem, activeAIGeneration, bool, error) {
	if d.state == nil || d.state.business == nil {
		return aiAgentInboxItem{}, activeAIGeneration{}, false, errRPCCapability
	}
	d.state.aiGenerationMu.Lock()
	defer d.state.aiGenerationMu.Unlock()
	if d.state.aiSubagentStopping[item.ConversationID] > 0 {
		return aiAgentInboxItem{}, activeAIGeneration{}, true, context.Canceled
	}
	active, found := d.state.aiGenerations[item.ConversationID]
	if !found {
		return aiAgentInboxItem{}, activeAIGeneration{}, false, nil
	}
	if active.Phase == aiAgentPhaseStopping || item.Destination == aiInboxNextStep &&
		(!active.StepWindowOpen || active.WorkspaceToolsEnabled != item.WorkspaceToolsEnabled) {
		item.Destination = aiInboxNextTurn
	}
	if active.Phase == aiAgentPhaseStopping {
		active.WakeRequested = true
	}
	stored, err := d.state.business.enqueueAIAgentInboxItem(ctx, projectID, item)
	if err != nil {
		return aiAgentInboxItem{}, activeAIGeneration{}, true, err
	}
	d.state.aiGenerations[item.ConversationID] = active
	return stored, active, true, nil
}

func (d dispatcher) claimAIAgentStep(ctx context.Context, projectID uuid.UUID, conversationID, generationID string, closeWindow bool) ([]aiAgentInboxItem, error) {
	d.state.aiGenerationMu.Lock()
	defer d.state.aiGenerationMu.Unlock()
	active, found := d.state.aiGenerations[conversationID]
	if !found || active.GenerationID != generationID {
		return nil, errRPCRevision
	}
	if active.Phase == aiAgentPhaseStopping {
		return nil, context.Canceled
	}
	if closeWindow {
		active.StepWindowOpen = false
		d.state.aiGenerations[conversationID] = active
	}
	items, err := d.state.business.claimAIAgentInbox(ctx, projectID, conversationID, aiInboxNextStep, generationID, d.now().UTC())
	if err != nil {
		return nil, err
	}
	if len(items) > 0 {
		active.StepWindowOpen = true
		d.state.aiGenerations[conversationID] = active
	}
	return items, nil
}

func combineAIAgentInboxItems(items []aiAgentInboxItem) (string, []chatAttachmentReference, string, string, bool, error) {
	if len(items) == 0 {
		return "", nil, "", "", false, errRPCInvalid
	}
	primary := items[0]
	for _, item := range items {
		if item.Destination == aiInboxNextTurn {
			primary = item
			break
		}
	}
	var prompt strings.Builder
	attachments := make([]chatAttachmentReference, 0)
	seenAttachments := make(map[string]struct{})
	for index, item := range items {
		if len(items) == 1 {
			prompt.WriteString(item.Prompt)
		} else {
			if index > 0 {
				prompt.WriteString("\n\n")
			}
			if item.Destination == aiInboxNextStep {
				prompt.WriteString("[User steering]\n")
			} else {
				prompt.WriteString("[Queued user message]\n")
			}
			prompt.WriteString(item.Prompt)
		}
		for _, attachment := range item.Attachments {
			if _, duplicate := seenAttachments[attachment.ID]; duplicate {
				continue
			}
			seenAttachments[attachment.ID] = struct{}{}
			attachments = append(attachments, attachment)
		}
	}
	if len(attachments) > 8 || prompt.Len() > 32<<10 {
		return "", nil, "", "", false, errRPCInvalid
	}
	return prompt.String(), attachments, primary.ID, primary.WorkspaceMode, primary.WorkspaceToolsEnabled, nil
}

func (d dispatcher) steeringProviderPrompt(ctx context.Context, projectID uuid.UUID, items []aiAgentInboxItem) (string, []aiPromptImage, error) {
	var text strings.Builder
	images := make([]aiPromptImage, 0)
	for index, item := range items {
		steering, err := d.aiPromptWithAttachments(ctx, projectID, item.Prompt, item.Attachments)
		if err != nil {
			return "", nil, err
		}
		if index > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString("[User steering]\n")
		text.WriteString(steering.Text)
		images = append(images, steering.Images...)
	}
	if text.Len() > maximumAIHistoryBytes || len(images) > 8 {
		return "", nil, errRPCInvalid
	}
	return text.String(), images, nil
}

func (d dispatcher) drainAIAgentInbox(ctx context.Context, projectID uuid.UUID, conversationID string) (conversationView, bool, error) {
	var latest conversationView
	ran := false
	for {
		pending, err := d.state.business.listAIAgentInbox(ctx, projectID, conversationID)
		if err != nil {
			return latest, ran, err
		}
		boundary := make([]aiAgentInboxItem, 0)
		for _, item := range pending {
			if item.Destination == aiInboxNextStep {
				boundary = append(boundary, item)
			}
		}
		for _, item := range pending {
			if item.Destination == aiInboxNextTurn {
				boundary = append(boundary, item)
				break
			}
		}
		var prompt, messageID, workspaceMode string
		workspaceToolsEnabled := d.aiWorkspaceToolsEnabled || d.scope == "remote.peer.ai.tools"
		var attachments []chatAttachmentReference
		var goalRound *aiGoalRoundSource
		var current conversationView
		if len(boundary) == 0 {
			current, goalRound, prompt, err = d.admitNextAIGoalRound(ctx, projectID, conversationID)
			if err != nil {
				return latest, ran, err
			}
			if goalRound == nil {
				return latest, ran, nil
			}
			messageID, workspaceMode = uuid.NewString(), current.WorkspaceMode
		} else {
			prompt, attachments, messageID, workspaceMode, workspaceToolsEnabled, err = combineAIAgentInboxItems(boundary)
			if err != nil {
				return latest, ran, err
			}
			current, err = d.state.business.getAIConversation(ctx, projectID, conversationID)
			if err != nil {
				return latest, ran, err
			}
		}
		config, err := d.conversationAIConfig(current.ConfigID, current.ModelBinding.Model)
		if err != nil {
			return latest, ran, err
		}
		generationID := uuid.NewString()
		turnContext, cancelTurn := context.WithCancel(ctx)
		if err := d.state.reserveAIGeneration(conversationID, generationID, cancelTurn); err != nil {
			cancelTurn()
			return latest, ran, err
		}
		turn, err := d.state.business.beginAIConversationTurnWithGeneration(turnContext, projectID, conversationID, messageID,
			generationID, prompt, workspaceMode, attachments, config, d.now().UTC())
		if err != nil {
			if goalRound != nil {
				d.state.disarmAIGoal(conversationID, goalRound.GoalID)
			}
			cancelTurn()
			if d.state.unregisterAIGeneration(conversationID, generationID) {
				go d.resumeAIAgentInbox(projectID, conversationID)
			}
			return latest, ran, err
		}
		turn.GoalRound = goalRound
		turn.WorkspaceToolsEnabled = &workspaceToolsEnabled
		d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
		var claimed []aiAgentInboxItem
		if len(boundary) > 0 {
			claimed, err = d.state.business.claimAIAgentInbox(ctx, projectID, conversationID, aiInboxNextTurn, turn.GenerationID, d.now().UTC())
		}
		if err != nil {
			_, _, events, _ := d.state.business.abortAIConversationTurn(context.Background(), projectID, conversationID, turn.GenerationID,
				turn.Assistant.ID, "failed", "inbox_claim_failed", chatProviderRun{Provider: config.Provider, Model: config.Model, FinishReason: "error", AttemptCount: 1}, d.now().UTC())
			d.emitAIConversationEvents(events)
			cancelTurn()
			if d.state.unregisterAIGeneration(conversationID, generationID) {
				go d.resumeAIAgentInbox(projectID, conversationID)
			}
			return latest, ran, err
		}
		if len(boundary) > 0 && len(claimed) == 0 {
			_, _, events, _ := d.state.business.abortAIConversationTurn(context.Background(), projectID, conversationID, turn.GenerationID,
				turn.Assistant.ID, "failed", "inbox_claim_empty", chatProviderRun{Provider: config.Provider, Model: config.Model, FinishReason: "error", AttemptCount: 1}, d.now().UTC())
			d.emitAIConversationEvents(events)
			cancelTurn()
			if d.state.unregisterAIGeneration(conversationID, generationID) {
				go d.resumeAIAgentInbox(projectID, conversationID)
			}
			return latest, ran, errRPCRevision
		}
		latest, err = d.executeAIConversationTurn(turnContext, projectID, turn, prompt, attachments, config)
		cancelTurn()
		ran = true
		if err != nil {
			if goalRound != nil {
				d.state.disarmAIGoal(conversationID, goalRound.GoalID)
			}
			return latest, ran, err
		}
	}
}

func (d dispatcher) resumeAIAgentInbox(projectID uuid.UUID, conversationID string) {
	// Inbox drivers start autonomous generations after the request that queued
	// their work. A copied chatEvent remains bound to that caller's RPC Stream;
	// retaining it would relabel this conversation's events with the old request
	// ID and leak them into another conversation. Durable/project events are
	// still published by emitAIConversationEvent when chatEvent is nil.
	d.chatEvent = nil
	if d.state == nil || d.state.business == nil {
		return
	}
	unlock := d.state.aiConversationDriverLock(conversationID)
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	conversation, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return
	}
	pending, err := d.state.business.listAIAgentInbox(ctx, projectID, conversationID)
	if err != nil {
		return
	}
	driveableGoal := conversation.Goal != nil && conversation.Goal.Phase == "active" &&
		d.state.isAIGoalArmed(conversationID, conversation.Goal.ID)
	if len(pending) == 0 && !driveableGoal {
		return
	}
	child := conversation.Subagent != nil
	if child {
		finishActivity, admitted := d.state.beginAISubagentActivity(conversationID)
		if !admitted {
			return
		}
		defer finishActivity()
		conversation, err = d.markAISubagentRunning(ctx, projectID, conversation)
		if err != nil {
			return
		}
	}
	_, ran, drainErr := d.drainAIAgentInbox(ctx, projectID, conversationID)
	if !child || !ran && drainErr == nil {
		return
	}
	status, errorText := "completed", ""
	if drainErr != nil {
		status, errorText = "failed", stableAISubagentError(drainErr)
		if errors.Is(drainErr, context.Canceled) {
			status, errorText = "interrupted", "Child agent was interrupted."
		}
	}
	summary := ""
	if page, pageErr := d.state.business.listAIConversationMessages(context.Background(), projectID, conversationID, 0, 1); pageErr == nil && len(page.Items) == 1 && page.Items[0].Role == "assistant" {
		summary = page.Items[0].Content
	}
	_ = d.settleAISubagent(projectID, conversation, status, summary, errorText)
}

func (d dispatcher) listAIAgentInboxRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "cursor", "limit") {
		return nil, 0, errRPCInvalid
	}
	conversationID, ok := inputString(input, "conversationId", 80)
	if !ok || uuid.Validate(conversationID) != nil {
		return nil, 0, errRPCInvalid
	}
	conversation, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	items, err := d.state.business.listAIAgentInbox(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	pageWatermark, err := rpcPageSnapshotWatermark(map[string]any{
		"method": "conversation.inbox.list", "conversationId": conversationID,
		"conversationRevision": conversation.Revision, "items": items,
	})
	if err != nil {
		return nil, 0, err
	}
	start, requestedEnd, _, err := versionedPageWindow(input, len(items), pageWatermark)
	if err != nil {
		return nil, 0, err
	}
	build := func(count int) any {
		end := start + count
		return map[string]any{
			"conversationId": conversationID, "items": items[start:end],
			"nextCursor":    versionedPageCursor(pageWatermark, end, len(items)),
			"highWatermark": pageWatermark, "conversationRevision": conversation.Revision,
		}
	}
	count, err := rpcPagePrefixLength(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), conversation.Revision, nil
}

func (d dispatcher) replaceAIAgentInboxRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	itemID, itemOK := inputString(input, "itemId", 80)
	prompt, promptOK := aiConversationPromptInput(input)
	destination, destinationOK := optionalInputString(input, "destination", 32)
	workspaceMode, workspaceOK := optionalInputString(input, "workspaceMode", 32)
	if !conversationOK || !itemOK || !promptOK || !destinationOK || !workspaceOK ||
		uuid.Validate(conversationID) != nil || uuid.Validate(itemID) != nil {
		return nil, 0, errRPCInvalid
	}
	conversation, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	pending, err := d.state.business.listAIAgentInbox(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	var existing *aiAgentInboxItem
	for index := range pending {
		if pending[index].ID == itemID {
			existing = &pending[index]
			break
		}
	}
	if existing == nil {
		return nil, 0, errRPCNotFound
	}
	if destination == "" {
		destination = existing.Destination
	}
	if workspaceMode == "" {
		workspaceMode = existing.WorkspaceMode
	} else {
		workspaceMode = normalizeAIWorkspaceMode(workspaceMode)
	}
	if !validAIInboxDestination(destination) || !validAIWorkspaceMode(workspaceMode) {
		return nil, 0, errRPCInvalid
	}
	attachments, err := d.validateAIConversationAttachments(ctx, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	replacement := aiAgentInboxItem{
		ID: itemID, ConversationID: conversationID, Destination: destination, Prompt: prompt,
		Attachments: attachments, WorkspaceMode: workspaceMode, WorkspaceToolsEnabled: existing.WorkspaceToolsEnabled, CreatedAt: existing.CreatedAt,
	}
	d.state.aiGenerationMu.Lock()
	active, activeFound := d.state.aiGenerations[conversationID]
	if activeFound && (active.Phase == aiAgentPhaseStopping || destination == aiInboxNextStep && !active.StepWindowOpen) {
		replacement.Destination = aiInboxNextTurn
	}
	if activeFound && active.Phase == aiAgentPhaseStopping {
		active.WakeRequested = true
		d.state.aiGenerations[conversationID] = active
	}
	replaced, err := d.state.business.replaceAIAgentInboxItem(ctx, projectID, replacement, d.now().UTC())
	d.state.aiGenerationMu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"accepted": true, "item": replaced}, conversation.Revision, nil
}

func (d dispatcher) removeAIAgentInboxRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "itemId") {
		return nil, 0, errRPCInvalid
	}
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	itemID, itemOK := inputString(input, "itemId", 80)
	if !conversationOK || !itemOK || uuid.Validate(conversationID) != nil || uuid.Validate(itemID) != nil {
		return nil, 0, errRPCInvalid
	}
	conversation, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	removed, err := d.state.business.removeAIAgentInboxItem(ctx, projectID, conversationID, itemID, d.now().UTC())
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"removed": removed, "conversationId": conversationID, "itemId": itemID}, conversation.Revision, nil
}

func (d dispatcher) clearAIAgentInboxRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId") {
		return nil, 0, errRPCInvalid
	}
	conversationID, ok := inputString(input, "conversationId", 80)
	if !ok || uuid.Validate(conversationID) != nil {
		return nil, 0, errRPCInvalid
	}
	conversation, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	cleared, err := d.state.business.clearAIAgentInbox(ctx, projectID, conversationID, d.now().UTC())
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"cleared": cleared, "conversationId": conversationID}, conversation.Revision, nil
}
