package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// defaultAIEventResponseBytes leaves headroom below the 60 KiB protocol frame
// for envelope metadata and future response fields. It is deliberately used
// even when a legacy client does not advertise a byte budget.
const defaultAIEventResponseBytes = 48 << 10

const maximumAIMessageWireToolRuns = 8
const maximumAIContextSummaryWireBytes = 4 << 10

func (d dispatcher) aiConversationProjectID() (uuid.UUID, error) {
	value := strings.TrimSpace(d.requestProjectID)
	if value == "" && !d.enforceProjectBinding && d.state != nil {
		return stableProjectID(d.state.DeviceID, ""), nil
	}
	projectID, err := uuid.Parse(value)
	if err != nil || projectID == uuid.Nil {
		return uuid.Nil, errRPCProject
	}
	return projectID, nil
}

func normalizeAIWorkspaceMode(value string) string {
	switch strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.TrimSpace(value))) {
	case "readonly", "read":
		return aiWorkspaceModeReadOnly
	case "workspacewrite", "edit":
		return aiWorkspaceModeWorkspaceWrite
	case "fullaccess", "dangerfullaccess", "allpermissions", "full":
		return aiWorkspaceModeFullAccess
	default:
		return ""
	}
}

func (d dispatcher) conversationAIConfig(configID, model string) (aiConfig, error) {
	if d.state == nil {
		return aiConfig{}, errRPCNotFound
	}
	d.state.mu.RLock()
	defer d.state.mu.RUnlock()
	configID, model = strings.TrimSpace(configID), strings.TrimSpace(model)
	if configID != "" {
		config, found := d.state.AIConfigs[configID]
		if !found || !config.Enabled || validateAIConfig(config) != nil {
			return aiConfig{}, errRPCAIConfigNotFound
		}
		return config, nil
	}
	config, err := selectAIConfigLocked(d.state, model)
	if errors.Is(err, errRPCNotFound) {
		if model == "" {
			return aiConfig{}, errRPCAIConfigRequired
		}
		return aiConfig{}, errRPCAIConfigNotFound
	}
	return config, err
}

func decodeAIPageOffset(input rpcInput) (int, error) {
	cursor, ok := optionalInputString(input, "cursor", 64)
	if !ok {
		return 0, errRPCInvalid
	}
	if cursor == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
		return 0, errRPCInvalid
	}
	offset, err := strconv.Atoi(string(decoded))
	if err != nil || offset < 0 {
		return 0, errRPCInvalid
	}
	return offset, nil
}

func encodeAIPageOffset(offset int) *string {
	if offset <= 0 {
		return nil
	}
	value := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
	return &value
}

func conversationCatalogVersion(input rpcInput) (uint64, error) {
	version, present, ok := optionalUint64(input, "catalogVersion")
	if !ok {
		return 0, errRPCInvalid
	}
	if !present {
		return 1, nil
	}
	if version != 2 {
		return 0, errRPCInvalid
	}
	return version, nil
}

func decodeAIConversationCatalogCursor(input rpcInput) (*aiConversationCatalogCursor, error) {
	cursor, ok := optionalInputString(input, "cursor", 512)
	if !ok {
		return nil, errRPCInvalid
	}
	if cursor == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
		return nil, errRPCInvalid
	}
	var value aiConversationCatalogCursor
	if json.Unmarshal(decoded, &value) != nil || value.Version != 2 || value.SnapshotRevision == 0 ||
		value.UpdatedAtMS <= 0 || uuid.Validate(value.ID) != nil {
		return nil, errRPCInvalid
	}
	return &value, nil
}

func encodeAIConversationCatalogCursor(value *aiConversationCatalogCursor) *string {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	result := base64.RawURLEncoding.EncodeToString(encoded)
	return &result
}

func aiConversationCatalogResponsePayload(page aiConversationCatalogPage) map[string]any {
	return map[string]any{
		"items": page.Items, "changes": page.Changes, "nextCursor": encodeAIConversationCatalogCursor(page.NextCursor),
		"ackedThroughSequence": page.AckedThroughSequence, "highWatermark": page.HighWatermark,
		"latestSequence": page.HighWatermark, "minimumAvailableSequence": page.MinimumAvailableSequence,
		"hasMore": page.HasMoreChanges, "hasMoreChanges": page.HasMoreChanges, "resetRequired": page.ResetRequired,
	}
}

func aiConversationCatalogResponseFits(page aiConversationCatalogPage) (bool, error) {
	encoded, err := json.Marshal(aiConversationCatalogResponsePayload(page))
	if err != nil {
		return false, err
	}
	return len(encoded) <= preferredRPCPagePayload, nil
}

func aiConversationCatalogCursorForPage(page aiConversationCatalogPage, item aiConversationListItem) *aiConversationCatalogCursor {
	return &aiConversationCatalogCursor{
		Version: 2, SnapshotRevision: page.AckedThroughSequence,
		UpdatedAtMS: item.UpdatedAt.UTC().UnixMilli(), ID: item.ID,
	}
}

// boundAIConversationCatalogSnapshotPage enforces the envelope's actual JSON
// budget rather than assuming a requested row count will fit. The continuation
// cursor is rebuilt from the final emitted row, so byte-based truncation still
// preserves the stable keyset snapshot.
func boundAIConversationCatalogSnapshotPage(page aiConversationCatalogPage) (aiConversationCatalogPage, error) {
	all := page.Items
	originalHasMore := page.NextCursor != nil
	page.Items = make([]aiConversationListItem, 0, len(all))
	page.NextCursor = nil
	for index, item := range all {
		page.Items = append(page.Items, item)
		page.NextCursor = nil
		if index < len(all)-1 || originalHasMore {
			page.NextCursor = aiConversationCatalogCursorForPage(page, item)
		}
		fits, err := aiConversationCatalogResponseFits(page)
		if err != nil {
			return aiConversationCatalogPage{}, err
		}
		if fits {
			continue
		}
		page.Items = page.Items[:len(page.Items)-1]
		if len(page.Items) == 0 {
			return aiConversationCatalogPage{}, errRPCResponsePageTooLarge
		}
		page.NextCursor = aiConversationCatalogCursorForPage(page, page.Items[len(page.Items)-1])
		return page, nil
	}
	return page, nil
}

// boundAIConversationCatalogChangesPage applies the same actual-byte budget
// to the journal path. Ack advances only through the final emitted change;
// an omitted item is always advertised through hasMoreChanges.
func boundAIConversationCatalogChangesPage(page aiConversationCatalogPage, after uint64) (aiConversationCatalogPage, error) {
	all := page.Changes
	originalHasMore := page.HasMoreChanges
	page.Changes = make([]aiConversationCatalogChange, 0, len(all))
	page.AckedThroughSequence = after
	page.HasMoreChanges = originalHasMore
	for index, change := range all {
		page.Changes = append(page.Changes, change)
		page.AckedThroughSequence = change.Sequence
		page.HasMoreChanges = index < len(all)-1 || originalHasMore
		fits, err := aiConversationCatalogResponseFits(page)
		if err != nil {
			return aiConversationCatalogPage{}, err
		}
		if fits {
			continue
		}
		page.Changes = page.Changes[:len(page.Changes)-1]
		if len(page.Changes) == 0 {
			return aiConversationCatalogPage{}, errRPCResponsePageTooLarge
		}
		page.AckedThroughSequence = page.Changes[len(page.Changes)-1].Sequence
		page.HasMoreChanges = true
		return page, nil
	}
	return page, nil
}

// boundLegacyAIConversationPage keeps pre-catalog-v2 clients operational when
// a count-bounded page contains enough Goal/todo/collaboration metadata to
// cross the RPC JSON ceiling. The legacy cursor remains an item offset, so a
// byte-truncated snapshot resumes immediately after the final emitted item.
// Change pages advance their acknowledgement only through the final emitted
// journal entry and explicitly retain hasMoreChanges.
func boundLegacyAIConversationPage(page aiConversationListResult, offset int, afterRevision *uint64) (map[string]any, error) {
	response := func(items []conversationView, changes []aiConversationChange, nextOffset int, highWatermark uint64, hasMoreChanges bool) map[string]any {
		return map[string]any{
			"items": items, "changes": changes, "nextCursor": encodeAIPageOffset(nextOffset),
			"highWatermark": highWatermark, "latestSequence": page.LatestSequence,
			"resetRequired": page.ResetRequired, "hasMoreChanges": hasMoreChanges,
		}
	}
	if afterRevision == nil || page.ResetRequired || len(page.Items) > 0 {
		build := func(count int) any {
			nextOffset := 0
			if count < len(page.Items) || page.NextOffset != 0 {
				nextOffset = offset + count
			}
			return response(page.Items[:count], page.Changes, nextOffset, page.HighWatermark, page.HasMoreChanges)
		}
		count, err := rpcPagePrefixLength(len(page.Items), build)
		if err != nil {
			return nil, err
		}
		return build(count).(map[string]any), nil
	}
	build := func(count int) any {
		acked := page.HighWatermark
		hasMore := page.HasMoreChanges
		if count < len(page.Changes) {
			acked = *afterRevision
			if count > 0 {
				acked = page.Changes[count-1].Sequence
			}
			hasMore = true
		}
		return response(page.Items, page.Changes[:count], page.NextOffset, acked, hasMore)
	}
	count, err := rpcPagePrefixLength(len(page.Changes), build)
	if err != nil {
		return nil, err
	}
	return build(count).(map[string]any), nil
}

func decodeAIMessageCursor(input rpcInput) (uint64, error) {
	before, present, ok := optionalUint64(input, "beforeSequence")
	if !ok {
		return 0, errRPCInvalid
	}
	cursor, cursorOK := optionalInputString(input, "cursor", 64)
	if !cursorOK || present && cursor != "" {
		return 0, errRPCInvalid
	}
	if cursor == "" {
		return before, nil
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
		return 0, errRPCInvalid
	}
	value, err := strconv.ParseUint(string(decoded), 10, 64)
	if err != nil || value == 0 {
		return 0, errRPCInvalid
	}
	return value, nil
}

func encodeAIMessageCursor(before uint64) *string {
	if before == 0 {
		return nil
	}
	value := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(before, 10)))
	return &value
}

func conversationLimit(input rpcInput, fallback, maximum int) (int, error) {
	value := float64(fallback)
	if raw, found := input["limit"]; found {
		var ok bool
		value, ok = raw.(float64)
		if !ok {
			return 0, errRPCInvalid
		}
	}
	if value < 1 || value > float64(maximum) || value != float64(int(value)) {
		return 0, errRPCInvalid
	}
	return int(value), nil
}

func conversationEventResponseBudget(input rpcInput) (int, error) {
	requested, present, ok := optionalUint64(input, "maxResponseBytes")
	if !ok {
		return 0, errRPCInvalid
	}
	if !present {
		return defaultAIEventResponseBytes, nil
	}
	// A client can ask for a smaller page but never relax the Agent's own
	// safety boundary. Tiny budgets are allowed so the caller receives the
	// explicit RESPONSE_PAGE_TOO_LARGE semantic instead of an ambiguous input
	// failure when even response metadata cannot fit.
	if requested == 0 || requested > maximumRPCPayload {
		return 0, errRPCInvalid
	}
	return min(int(requested), defaultAIEventResponseBytes), nil
}

func summaryAIConversationToolRunForWire(run chatToolRun) chatToolRun {
	result := compactAIConversationToolRunForEvent(run)
	encoded, err := json.Marshal(result)
	if err == nil && len(encoded) <= 2<<10 {
		return result
	}
	result.Arguments = map[string]any{"truncated": true, "source": "messageSnapshot"}
	result.Result = map[string]any{"truncated": true, "source": "messageSnapshot"}
	result.Output = truncateAIConversationUTF8(result.Output, 512)
	return result
}

// aiConversationMessageWire keeps a normal message page bounded without
// shortening the authoritative message. Large body fields expose an explicit
// reference that callers resolve through conversation.message.content.
func aiConversationMessageWire(message chatMessage) map[string]any {
	content, reasoning := message.Content, message.Reasoning
	var contentRef, reasoningRef map[string]any
	if len(content) > maximumAIMessageContentChunkBytes {
		content = truncateAIConversationUTF8(content, maximumAIMessageContentChunkBytes)
		contentRef = map[string]any{"field": "content", "totalBytes": len(message.Content)}
	}
	if len(reasoning) > maximumAIMessageContentChunkBytes {
		reasoning = truncateAIConversationUTF8(reasoning, maximumAIMessageContentChunkBytes)
		reasoningRef = map[string]any{"field": "reasoning", "totalBytes": len(message.Reasoning)}
	}
	start := max(0, len(message.ToolRuns)-maximumAIMessageWireToolRuns)
	toolRuns := make([]chatToolRun, 0, len(message.ToolRuns)-start)
	for _, run := range message.ToolRuns[start:] {
		toolRuns = append(toolRuns, summaryAIConversationToolRunForWire(run))
	}
	result := map[string]any{
		"id":          message.ID,
		"revision":    message.Revision,
		"sequence":    message.Sequence,
		"role":        message.Role,
		"content":     content,
		"status":      message.Status,
		"errorCode":   message.ErrorCode,
		"attachments": message.Attachments,
		"reasoning":   reasoning,
		"toolRuns":    toolRuns,
		"usage":       message.Usage,
		"providerRun": message.ProviderRun,
		"createdAt":   message.CreatedAt,
	}
	if message.GenerationID != "" {
		result["generationId"] = message.GenerationID
	}
	if contentRef != nil {
		result["contentRef"] = contentRef
	}
	if reasoningRef != nil {
		result["reasoningRef"] = reasoningRef
	}
	if start > 0 {
		result["toolRunsRef"] = map[string]any{"source": "messageSnapshot", "total": len(message.ToolRuns)}
	}
	return result
}

func aiContextSummaryWire(summary *aiContextSummary) *aiContextSummary {
	if summary == nil || len(summary.Content) <= maximumAIContextSummaryWireBytes {
		return summary
	}
	copy := *summary
	copy.Content = truncateAIConversationUTF8(copy.Content, maximumAIContextSummaryWireBytes)
	return &copy
}

func boundedAIConversationMessagePage(items []chatMessage, originalNext uint64, base func([]map[string]any, uint64) map[string]any) ([]map[string]any, uint64, error) {
	selected := make([]map[string]any, 0, len(items))
	next := originalNext
	for index := len(items) - 1; index >= 0; index-- {
		candidate := append([]map[string]any{aiConversationMessageWire(items[index])}, selected...)
		candidateNext := originalNext
		if index > 0 {
			candidateNext = items[index].Sequence
		}
		encoded, err := json.Marshal(base(candidate, candidateNext))
		if err != nil {
			return nil, 0, err
		}
		if len(encoded) > defaultAIEventResponseBytes {
			if len(selected) == 0 {
				return nil, 0, errRPCResponsePageTooLarge
			}
			break
		}
		selected, next = candidate, candidateNext
	}
	if len(items) == 0 {
		encoded, err := json.Marshal(base(selected, originalNext))
		if err != nil {
			return nil, 0, err
		}
		if len(encoded) > defaultAIEventResponseBytes {
			return nil, 0, errRPCResponsePageTooLarge
		}
	}
	return selected, next, nil
}

func (d dispatcher) callAIConversationRPC(ctx context.Context, method string, input rpcInput) (any, uint64, error) {
	if d.state == nil || d.state.business == nil {
		return nil, 0, errRPCCapability
	}
	projectID, err := d.aiConversationProjectID()
	if err != nil {
		return nil, 0, err
	}
	switch method {
	case "conversation.list":
		return d.listAIConversationRPC(ctx, projectID, input)
	case "conversation.summary.get":
		return d.getAIConversationCatalogSummaryRPC(ctx, projectID, input)
	case "conversation.get":
		return d.getAIConversationRPC(ctx, projectID, input)
	case "conversation.create":
		return d.createAIConversationRPC(ctx, projectID, input)
	case "conversation.fork":
		return d.forkAIConversationRPC(ctx, projectID, input)
	case "conversation.rename":
		return d.renameAIConversationRPC(ctx, projectID, input)
	case "conversation.delete":
		return d.deleteAIConversationRPC(ctx, projectID, input)
	case "conversation.search":
		return d.searchAIConversationRPC(ctx, projectID, input)
	case "conversation.messages.before":
		return d.listAIConversationMessagesRPC(ctx, projectID, input)
	case "conversation.message.content":
		return d.getAIConversationMessageContentRPC(ctx, projectID, input)
	case "conversation.cancel":
		return d.cancelAIConversationRPC(ctx, projectID, input)
	case "conversation.inbox.list":
		return d.listAIAgentInboxRPC(ctx, projectID, input)
	case "conversation.inbox.replace":
		return d.replaceAIAgentInboxRPC(ctx, projectID, input)
	case "conversation.inbox.remove":
		return d.removeAIAgentInboxRPC(ctx, projectID, input)
	case "conversation.inbox.clear":
		return d.clearAIAgentInboxRPC(ctx, projectID, input)
	case "conversation.plan.set":
		return d.setAIConversationPlanModeRPC(ctx, projectID, input)
	case "conversation.goal.create":
		return d.createAIGoalRPC(ctx, projectID, input)
	case "conversation.goal.edit":
		return d.editAIGoalRPC(ctx, projectID, input)
	case "conversation.goal.pause":
		return d.transitionAIGoalRPC(ctx, projectID, input, "pause")
	case "conversation.goal.resume":
		return d.transitionAIGoalRPC(ctx, projectID, input, "resume")
	case "conversation.goal.clear":
		return d.clearAIGoalRPC(ctx, projectID, input)
	case "conversation.subagents.list":
		return d.listAISubagentsRPC(ctx, projectID, input)
	case "conversation.subagent.message":
		return d.sendAISubagentMessageRPC(ctx, projectID, input)
	case "conversation.subagent.interrupt":
		return d.interruptAISubagentRPC(ctx, projectID, input)
	case "conversation.events":
		return d.listAIConversationEventsRPC(ctx, projectID, input)
	case "conversation.generation.get":
		return d.getAIConversationGenerationRPC(ctx, projectID, input)
	case "conversation.generation.attach":
		// Attach is an explicitly Relay-bound long-lived request. Standard
		// dispatch/stdio has no query-specific cancellation channel, so reject
		// it instead of accidentally leaking a subscription.
		return nil, 0, errRPCCapability
	case "conversation.regenerate":
		return d.regenerateAIConversationRPC(ctx, projectID, input)
	case "conversation.approval.respond":
		return d.respondAIConversationApprovalRPC(ctx, projectID, input)
	case "conversation.question.answer":
		return d.answerAIConversationQuestionRPC(ctx, projectID, input)
	default:
		return nil, 0, errRPCNotFound
	}
}

func (d dispatcher) answerAIConversationQuestionRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "generationId", "toolCallId", "answers") {
		return nil, 0, errRPCInvalid
	}
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	generationID, generationOK := inputString(input, "generationId", 80)
	toolCallID, toolCallOK := inputString(input, "toolCallId", 256)
	rawAnswers, answersOK := input["answers"].([]any)
	if !conversationOK || !generationOK || !toolCallOK || !answersOK || len(rawAnswers) == 0 || len(rawAnswers) > maximumAIQuestionsPerCall {
		return nil, 0, errRPCInvalid
	}
	answers := make([]aiQuestionAnswer, 0, len(rawAnswers))
	for _, raw := range rawAnswers {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, 0, errRPCInvalid
		}
		var answer aiQuestionAnswer
		if json.Unmarshal(encoded, &answer) != nil || !validAIQuestionAnswer(answer) {
			return nil, 0, errRPCInvalid
		}
		answers = append(answers, answer)
	}
	if err := d.state.resolveAIQuestion(projectID, conversationID, generationID, toolCallID, answers); err != nil {
		return nil, 0, err
	}
	conversation, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"accepted": true, "conversationId": conversationID, "generationId": generationID,
		"toolCallId": toolCallID, "answers": answers,
	}, conversation.Revision, nil
}

func (d dispatcher) setAIConversationPlanModeRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.planMode"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "active") {
		return nil, 0, errRPCInvalid
	}
	conversationID, ok := inputString(input, "conversationId", 80)
	active, activeOK := input["active"].(bool)
	if !ok || !activeOK || uuid.Validate(conversationID) != nil {
		return nil, 0, errRPCInvalid
	}
	current, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	if current.PlanModeActive == active {
		return current, current.Revision, nil
	}
	value, event, err := d.state.business.updateAIConversationCollaboration(ctx, projectID, conversationID, "", "",
		"chat.plan_mode.changed", map[string]any{"active": active, "source": "user"},
		func(collaboration *aiConversationCollaboration) error {
			collaboration.PlanModeActive = active
			return nil
		}, d.now().UTC())
	if err != nil {
		return nil, 0, err
	}
	if err := d.emitAIConversationEvent(event); err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func (d dispatcher) listAISubagentsRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.subagents"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "cursor", "limit") {
		return nil, 0, errRPCInvalid
	}
	conversationID, ok := inputString(input, "conversationId", 80)
	if !ok || uuid.Validate(conversationID) != nil {
		return nil, 0, errRPCInvalid
	}
	parent, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	children, err := d.state.business.listAISubagents(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	pageWatermark, err := rpcPageSnapshotWatermark(map[string]any{
		"method": "conversation.subagents.list", "conversationId": conversationID,
		"parentRevision": parent.Revision, "items": children,
	})
	if err != nil {
		return nil, 0, err
	}
	start, requestedEnd, _, err := versionedPageWindow(input, len(children), pageWatermark)
	if err != nil {
		return nil, 0, err
	}
	build := func(count int) any {
		end := start + count
		return map[string]any{
			"conversationId": conversationID, "items": children[start:end],
			"nextCursor":    versionedPageCursor(pageWatermark, end, len(children)),
			"highWatermark": pageWatermark, "parentRevision": parent.Revision,
		}
	}
	count, err := rpcPagePrefixLength(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), parent.Revision, nil
}

func (d dispatcher) sendAISubagentMessageRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.subagents"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "agentId", "message", "enableWorkspaceTools") {
		return nil, 0, errRPCInvalid
	}
	workspaceToolsEnabled, toolsErr := d.aiWorkspaceToolsIntent(input)
	if toolsErr != nil {
		return nil, 0, toolsErr
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	agentID, agentOK := inputString(input, "agentId", 80)
	message, messageOK := inputString(input, "message", 32<<10)
	message = strings.TrimSpace(message)
	if !conversationOK || !agentOK || !messageOK || uuid.Validate(conversationID) != nil || uuid.Validate(agentID) != nil || message == "" {
		return nil, 0, errRPCInvalid
	}
	if err := d.sendAISubagentMessage(ctx, projectID, conversationID, agentID, message); err != nil {
		return nil, 0, err
	}
	child, err := d.state.business.getAIConversation(ctx, projectID, agentID)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"accepted": true, "conversation": child}, child.Revision, nil
}

func (d dispatcher) interruptAISubagentRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.subagents"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "agentId") {
		return nil, 0, errRPCInvalid
	}
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	agentID, agentOK := inputString(input, "agentId", 80)
	if !conversationOK || !agentOK || uuid.Validate(conversationID) != nil || uuid.Validate(agentID) != nil {
		return nil, 0, errRPCInvalid
	}
	interrupted, err := d.interruptAISubagent(ctx, projectID, conversationID, agentID)
	if err != nil {
		return nil, 0, err
	}
	child, err := d.state.business.getAIConversation(ctx, projectID, agentID)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"interrupted": interrupted, "conversation": child}, child.Revision, nil
}

func (d dispatcher) respondAIConversationApprovalRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "approvalId", "conversationId", "generationId", "toolCallId", "decision") {
		return nil, 0, errRPCInvalid
	}
	approvalID, approvalOK := inputString(input, "approvalId", 80)
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	generationID, generationOK := inputString(input, "generationId", 80)
	toolCallID, toolCallOK := inputString(input, "toolCallId", 256)
	decision, decisionOK := inputString(input, "decision", 32)
	if !approvalOK || !conversationOK || !generationOK || !toolCallOK || !decisionOK {
		return nil, 0, errRPCInvalid
	}
	resolution, err := d.state.resolveAIApproval(projectID, approvalID, conversationID, generationID, toolCallID, decision)
	if err != nil {
		return nil, 0, err
	}
	conversation, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"accepted": true, "approvalId": approvalID, "conversationId": conversationID,
		"generationId": generationID, "toolCallId": toolCallID, "decision": resolution.Decision,
	}, conversation.Revision, nil
}

func (d dispatcher) listAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	// Conversation list is a read-only snapshot. Bound the local SQLite wait so
	// an active generation cannot turn a LAN page load into a multi-second UI
	// spinner; callers retain their last page and can retry on contention.
	ctx, cancel := context.WithTimeout(ctx, localListReadBudget)
	defer cancel()
	catalogVersion, err := conversationCatalogVersion(input)
	if err != nil {
		return nil, 0, err
	}
	fallbackLimit := 50
	if catalogVersion == 2 {
		fallbackLimit = 30
	}
	limit, err := conversationLimit(input, fallbackLimit, maximumAIConversationPage)
	if err != nil {
		return nil, 0, err
	}
	after, present, ok := optionalUint64(input, "afterRevision")
	if !ok {
		return nil, 0, errRPCInvalid
	}
	var afterPointer *uint64
	if present {
		afterPointer = &after
	}
	if catalogVersion == 2 {
		cursor, cursorErr := decodeAIConversationCatalogCursor(input)
		if cursorErr != nil || present && cursor != nil {
			return nil, 0, firstError(cursorErr, errRPCInvalid)
		}
		page, listErr := d.state.business.listAIConversationCatalog(ctx, aiConversationCatalogListOptions{
			ProjectID: projectID, Cursor: cursor, Limit: limit, AfterRevision: afterPointer,
		})
		if listErr != nil {
			return nil, 0, listErr
		}
		if afterPointer == nil {
			page, listErr = boundAIConversationCatalogSnapshotPage(page)
		} else {
			page, listErr = boundAIConversationCatalogChangesPage(page, *afterPointer)
		}
		if listErr != nil {
			return nil, 0, listErr
		}
		return aiConversationCatalogResponsePayload(page), page.HighWatermark, nil
	}
	offset, err := decodeAIPageOffset(input)
	if err != nil || present && offset != 0 {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	page, err := d.state.business.listAIConversations(ctx, aiConversationListOptions{
		ProjectID: projectID, Offset: offset, Limit: limit, AfterRevision: afterPointer,
	})
	if err != nil {
		return nil, 0, err
	}
	for index := range page.Items {
		page.Items[index] = d.withAIGoalActivation(page.Items[index])
	}
	for index := range page.Changes {
		if !page.Changes[index].Deleted {
			page.Changes[index].Value = d.withAIGoalActivation(page.Changes[index].Value)
		}
	}
	payload, err := boundLegacyAIConversationPage(page, offset, afterPointer)
	if err != nil {
		return nil, 0, err
	}
	return payload, page.HighWatermark, nil
}

func (d dispatcher) getAIConversationCatalogSummaryRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "catalogVersion") {
		return nil, 0, errRPCInvalid
	}
	if _, err := conversationCatalogVersion(input); err != nil {
		return nil, 0, err
	}
	conversationID, ok := inputString(input, "conversationId", 80)
	if !ok || uuid.Validate(conversationID) != nil {
		return nil, 0, errRPCInvalid
	}
	item, err := d.state.business.getAIConversationCatalogItem(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"item": item}, item.Revision, nil
}

func (d dispatcher) getAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	id, ok := inputString(input, "conversationId", 80)
	if !ok || uuid.Validate(id) != nil {
		return nil, 0, errRPCInvalid
	}
	limit, err := conversationLimit(input, 50, maximumAIConversationPage)
	if err != nil {
		return nil, 0, err
	}
	before, err := decodeAIMessageCursor(input)
	if err != nil {
		return nil, 0, err
	}
	snapshot, err := d.state.business.getAIConversationSnapshot(ctx, projectID, id, before, limit)
	if err != nil {
		return nil, 0, err
	}
	snapshot.Conversation = d.withAIGoalActivation(snapshot.Conversation)
	summary := aiContextSummaryWire(snapshot.ContextSummary)
	messageItems, nextBefore, err := boundedAIConversationMessagePage(
		snapshot.Messages.Items,
		snapshot.Messages.NextBefore,
		func(items []map[string]any, next uint64) map[string]any {
			return map[string]any{
				"conversation": snapshot.Conversation, "messages": items, "nextCursor": encodeAIMessageCursor(next),
				"highWatermark": snapshot.Messages.HighWatermark, "resetRequired": snapshot.Messages.ResetRequired, "contextSummary": summary,
				"snapshotEventHighWatermark": snapshot.EventHighWatermark, "eventHighWatermark": snapshot.EventHighWatermark,
				"earliestAvailableEventSequence": snapshot.EarliestAvailableEventSequence,
			}
		},
	)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"conversation": snapshot.Conversation, "messages": messageItems, "nextCursor": encodeAIMessageCursor(nextBefore),
		"highWatermark": snapshot.Messages.HighWatermark, "resetRequired": snapshot.Messages.ResetRequired, "contextSummary": summary,
		"snapshotEventHighWatermark": snapshot.EventHighWatermark, "eventHighWatermark": snapshot.EventHighWatermark,
		"earliestAvailableEventSequence": snapshot.EarliestAvailableEventSequence,
	}, snapshot.Conversation.Revision, nil
}

// getAIConversationGenerationRPC is intentionally lightweight: the mobile
// client calls it before sending whenever transport or replay state is
// uncertain. Supplying generationId also lets a reconnect verify a terminal
// generation after the conversation has already returned to idle.
func (d dispatcher) getAIConversationGenerationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "generationId") {
		return nil, 0, errRPCInvalid
	}
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	generationID, generationOK := optionalInputString(input, "generationId", 80)
	if !conversationOK || !generationOK || uuid.Validate(conversationID) != nil || generationID != "" && uuid.Validate(generationID) != nil {
		return nil, 0, errRPCInvalid
	}
	state, revision, err := d.state.business.getAIConversationGenerationState(ctx, projectID, conversationID, generationID)
	if err != nil {
		return nil, 0, err
	}
	pendingApproval := (*aiApprovalRequest)(nil)
	if state.GenerationID != "" {
		pendingApproval = d.state.pendingAIApprovalForGeneration(projectID, conversationID, state.GenerationID, d.now().UTC())
	}
	return map[string]any{
		"conversationId":    state.ConversationID,
		"generationId":      state.GenerationID,
		"status":            state.Status,
		"startedAt":         state.StartedAt,
		"updatedAt":         state.UpdatedAt,
		"lastEventSequence": state.LastEventSequence,
		"highWatermark":     state.LastEventSequence,
		"canAcceptNewTurn":  state.CanAcceptNewTurn,
		"errorCode":         state.ErrorCode,
		"pendingApproval":   pendingApproval,
	}, revision, nil
}

func (d dispatcher) createAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	title, ok := inputString(input, "title", maximumAIConversationTitleBytes)
	configID, configOK := optionalInputString(input, "configId", 80)
	model, modelOK := optionalInputString(input, "model", 120)
	workspaceMode, modeOK := optionalInputString(input, "workspaceMode", 32)
	id, idOK := optionalInputString(input, "conversationId", 80)
	if workspaceMode == "" {
		workspaceMode = "readOnly"
	} else {
		workspaceMode = normalizeAIWorkspaceMode(workspaceMode)
	}
	if !ok || !configOK || !modelOK || !modeOK || !idOK || !validAIWorkspaceMode(workspaceMode) || id != "" && uuid.Validate(id) != nil {
		return nil, 0, errRPCInvalid
	}
	config, err := d.conversationAIConfig(configID, model)
	if err != nil {
		return nil, 0, err
	}
	value, err := d.state.business.createAIConversation(ctx, projectID, id, title, workspaceMode, config, d.now().UTC())
	if err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func (d dispatcher) forkAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.conversationFork"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "sourceConversationId", "conversationId", "messageId", "messageSequence", "throughMessageId", "throughMessageSequence", "expectedRevision") {
		return nil, 0, errRPCInvalid
	}
	sourceID, sourceOK := inputString(input, "sourceConversationId", 80)
	childID, childOK := inputString(input, "conversationId", 80)
	messageID, messageOK := optionalInputString(input, "messageId", 80)
	messageSequence, sequencePresent, sequenceOK := optionalUint64(input, "messageSequence")
	throughMessageID, throughMessageOK := optionalInputString(input, "throughMessageId", 80)
	throughMessageSequence, throughSequencePresent, throughSequenceOK := optionalUint64(input, "throughMessageSequence")
	revision, err := expectedConversationRevision(input, d.enforceProjectBinding)
	legacyBoundary := messageID != "" || sequencePresent
	inclusiveBoundary := throughMessageID != "" || throughSequencePresent
	if !sourceOK || !childOK || !messageOK || !sequenceOK || !throughMessageOK || !throughSequenceOK ||
		legacyBoundary == inclusiveBoundary ||
		legacyBoundary && (messageID == "" || !sequencePresent || messageSequence == 0 || uuid.Validate(messageID) != nil) ||
		inclusiveBoundary && (throughMessageID == "" || !throughSequencePresent || throughMessageSequence == 0 || uuid.Validate(throughMessageID) != nil) ||
		uuid.Validate(sourceID) != nil || uuid.Validate(childID) != nil || err != nil {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	includeBoundary := inclusiveBoundary
	if inclusiveBoundary {
		messageID = throughMessageID
		messageSequence = throughMessageSequence
	}
	if revision == 0 {
		current, loadErr := d.state.business.getAIConversation(ctx, projectID, sourceID)
		if loadErr != nil {
			return nil, 0, loadErr
		}
		revision = current.Revision
	}
	value, err := d.state.business.forkAIConversation(
		ctx,
		projectID,
		sourceID,
		childID,
		messageID,
		messageSequence,
		revision,
		includeBoundary,
		d.now().UTC(),
	)
	if err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func expectedConversationRevision(input rpcInput, required bool) (uint64, error) {
	revision, present, ok := optionalUint64(input, "expectedRevision")
	if !ok || required && !present || present && revision == 0 {
		return 0, errRPCInvalid
	}
	return revision, nil
}

func (d dispatcher) renameAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	id, idOK := inputString(input, "conversationId", 80)
	title, titleOK := inputString(input, "title", maximumAIConversationTitleBytes)
	revision, err := expectedConversationRevision(input, d.enforceProjectBinding)
	if !idOK || !titleOK || uuid.Validate(id) != nil || err != nil {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	if revision == 0 {
		current, err := d.state.business.getAIConversation(ctx, projectID, id)
		if err != nil {
			return nil, 0, err
		}
		revision = current.Revision
	}
	value, err := d.state.business.renameAIConversation(ctx, projectID, id, title, revision, d.now().UTC())
	if err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func (d dispatcher) deleteAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	id, idOK := inputString(input, "conversationId", 80)
	revision, err := expectedConversationRevision(input, d.enforceProjectBinding)
	if !idOK || uuid.Validate(id) != nil || err != nil {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	if revision == 0 {
		current, err := d.state.business.getAIConversation(ctx, projectID, id)
		if err != nil {
			return nil, 0, err
		}
		revision = current.Revision
	}
	descendants, err := d.state.business.listAISubagentDescendants(ctx, projectID, id)
	if err != nil {
		return nil, 0, err
	}
	for _, child := range descendants {
		d.state.cancelAIGeneration(child.ID, "")
	}
	value, err := d.state.business.deleteAIConversation(ctx, projectID, id, revision, d.now().UTC())
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"deleted": true, "conversation": value, "conversationId": id}, value.Revision, nil
}

func (d dispatcher) listAIConversationMessagesRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	id, ok := inputString(input, "conversationId", 80)
	limit, err := conversationLimit(input, 50, maximumAIConversationPage)
	if !ok || uuid.Validate(id) != nil || err != nil {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	before, err := decodeAIMessageCursor(input)
	if err != nil {
		return nil, 0, err
	}
	page, err := d.state.business.listAIConversationMessages(ctx, projectID, id, before, limit)
	if err != nil {
		return nil, 0, err
	}
	if after, present, ok := optionalUint64(input, "afterRevision"); !ok {
		return nil, 0, errRPCInvalid
	} else if present && after == page.HighWatermark {
		page.Items, page.NextBefore = []chatMessage{}, 0
	}
	messageItems, nextBefore, err := boundedAIConversationMessagePage(
		page.Items,
		page.NextBefore,
		func(items []map[string]any, next uint64) map[string]any {
			return map[string]any{
				"items": items, "nextCursor": encodeAIMessageCursor(next), "nextBeforeSequence": next,
				"highWatermark": page.HighWatermark, "resetRequired": page.ResetRequired,
			}
		},
	)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"items": messageItems, "nextCursor": encodeAIMessageCursor(nextBefore), "nextBeforeSequence": nextBefore,
		"highWatermark": page.HighWatermark, "resetRequired": page.ResetRequired,
	}, page.HighWatermark, nil
}

// getAIConversationMessageContentRPC provides a byte-addressed, UTF-8 safe
// read for message bodies that are too large to embed in a normal page. It is
// intentionally independent from replay events: events are a recovery cache,
// while this reads the authoritative message record.
func (d dispatcher) getAIConversationMessageContentRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "messageId", "field", "offset", "maxBytes") {
		return nil, 0, errRPCInvalid
	}
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	messageID, messageOK := inputString(input, "messageId", 80)
	field, fieldOK := inputString(input, "field", 16)
	offset, _, offsetOK := optionalUint64(input, "offset")
	requestedBytes, present, bytesOK := optionalUint64(input, "maxBytes")
	if !conversationOK || !messageOK || !fieldOK || !offsetOK || !bytesOK || uuid.Validate(conversationID) != nil || uuid.Validate(messageID) != nil ||
		field != "content" && field != "reasoning" {
		return nil, 0, errRPCInvalid
	}
	maximumBytes := maximumAIMessageContentChunkBytes
	if present {
		if requestedBytes == 0 || requestedBytes > maximumAIMessageContentChunkBytes {
			return nil, 0, errRPCInvalid
		}
		maximumBytes = int(requestedBytes)
	}
	chunk, err := d.state.business.readAIConversationMessageContent(ctx, projectID, conversationID, messageID, field, offset, maximumBytes)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{
		"conversationId": conversationID,
		"messageId":      messageID,
		"field":          field,
		"offset":         chunk.Offset,
		"nextOffset":     chunk.NextOffset,
		"totalBytes":     chunk.TotalBytes,
		"hasMore":        chunk.HasMore,
		"revision":       chunk.Revision,
		"content":        chunk.Content,
	}, chunk.Revision, nil
}

func (d dispatcher) searchAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	query, ok := inputString(input, "query", 200)
	limit, err := conversationLimit(input, 50, maximumAIConversationPage)
	if !ok || err != nil {
		return nil, 0, firstError(err, errRPCInvalid)
	}
	offset, err := decodeAIPageOffset(input)
	if err != nil {
		return nil, 0, err
	}
	items, total, err := d.state.business.searchAIConversations(ctx, projectID, query, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	build := func(count int) any {
		next := 0
		if offset+count < total {
			next = offset + count
		}
		return map[string]any{"items": items[:count], "nextCursor": encodeAIPageOffset(next), "total": total}
	}
	count, err := rpcPagePrefixLength(len(items), build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), uint64(total), nil
}

func (d dispatcher) cancelAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "generationId", "keepInbox") {
		return nil, 0, errRPCInvalid
	}
	id, ok := inputString(input, "conversationId", 80)
	generationID, generationOK := optionalInputString(input, "generationId", 80)
	keepInbox, keepInboxOK := optionalInputBool(input, "keepInbox", false)
	if !ok || !generationOK || !keepInboxOK || uuid.Validate(id) != nil || generationID != "" && uuid.Validate(generationID) != nil {
		return nil, 0, errRPCInvalid
	}
	value, assistantID, err := d.state.business.activeAIConversationGeneration(ctx, projectID, id)
	if err != nil {
		return nil, 0, err
	}
	if value.Goal != nil && value.Goal.Phase == "active" && d.state.isAIGoalArmed(id, value.Goal.ID) {
		if paused, pauseErr := d.transitionAIGoal(ctx, projectID, id, value.Goal.ID, value.Goal.Revision,
			"pause", nil, "", "", "user-cancel"); pauseErr == nil {
			value = paused
		} else if !errors.Is(pauseErr, errRPCRevision) {
			return nil, 0, pauseErr
		}
	}
	if value.State == "generating" {
		if generationID != "" && generationID != value.GenerationID {
			return nil, 0, errRPCRevision
		}
		generationID = value.GenerationID
	}
	var cancelGeneration context.CancelFunc
	var active activeAIGeneration
	var registered bool
	d.state.aiGenerationMu.Lock()
	active, registered = d.state.aiGenerations[id]
	if registered && generationID != "" && active.GenerationID != generationID {
		d.state.aiGenerationMu.Unlock()
		return nil, 0, errRPCRevision
	}
	firstCancellation := registered && active.Phase != aiAgentPhaseStopping
	if firstCancellation {
		active.Phase = aiAgentPhaseStopping
		active.StepWindowOpen = false
		if active.CancelCause == "" {
			active.CancelCause = "cancelled_by_user"
		}
		if !keepInbox {
			if _, clearErr := d.state.business.clearAIAgentInbox(ctx, projectID, id, d.now().UTC()); clearErr != nil {
				d.state.aiGenerationMu.Unlock()
				return nil, 0, clearErr
			}
		}
		d.state.aiGenerations[id] = active
		cancelGeneration = active.Cancel
	} else if !registered && !keepInbox {
		if _, clearErr := d.state.business.clearAIAgentInbox(ctx, projectID, id, d.now().UTC()); clearErr != nil {
			d.state.aiGenerationMu.Unlock()
			return nil, 0, clearErr
		}
	}
	d.state.aiGenerationMu.Unlock()

	// Publish the scoped descendant cutoff before the root cancellation can
	// cause a child to settle and enqueue work back into this conversation.
	cancellation, prepareErr := d.prepareAISubagentCancellation(ctx, projectID, id)
	if cancelGeneration != nil {
		cancelGeneration()
	}
	var descendantErr error
	if prepareErr == nil {
		descendantErr = d.cancelAISubagentDescendants(projectID, cancellation)
	}
	treeCancellationErr := errors.Join(prepareErr, descendantErr)
	if value.State != "generating" {
		if treeCancellationErr != nil {
			return nil, 0, treeCancellationErr
		}
		if registered {
			return map[string]any{
				"cancelled": true, "conversationId": id, "generationId": active.GenerationID,
				"keepInbox": keepInbox, "firstCancellation": firstCancellation,
			}, value.Revision, nil
		}
		return map[string]any{"cancelled": false, "conversationId": id}, value.Revision, nil
	}
	_, updated, events, err := d.state.business.abortAIConversationTurn(context.Background(), projectID, id, generationID, assistantID,
		"stopped", "cancelled_by_user", chatProviderRun{Provider: value.ModelBinding.Provider, Model: value.ModelBinding.Model, FinishReason: "cancelled", AttemptCount: 1}, d.now().UTC())
	if err != nil && !errors.Is(err, errRPCRevision) {
		return nil, 0, err
	}
	if err == nil {
		d.emitAIConversationEvents(events)
	}
	if errors.Is(err, errRPCRevision) {
		updated, err = d.state.business.getAIConversation(ctx, projectID, id)
		if err != nil {
			return nil, 0, err
		}
	}
	if treeCancellationErr != nil {
		return nil, 0, treeCancellationErr
	}
	return map[string]any{
		"cancelled": true, "conversationId": id, "generationId": generationID,
		"keepInbox": keepInbox, "firstCancellation": firstCancellation,
	}, updated.Revision, nil
}

func (d dispatcher) listAIConversationEventsRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "conversationId", "afterSequence", "limit", "maxResponseBytes", "eventContractVersion", "acceptedEventKinds", "event_contract_version", "accepted_event_kinds") {
		return nil, 0, errRPCInvalid
	}
	id, ok := inputString(input, "conversationId", 80)
	after, _, afterOK := optionalUint64(input, "afterSequence")
	limit, err := conversationLimit(input, 200, maximumAIEventPage)
	responseBytes, budgetErr := conversationEventResponseBudget(input)
	if !ok || !afterOK || uuid.Validate(id) != nil || err != nil || budgetErr != nil {
		return nil, 0, firstError(firstError(err, budgetErr), errRPCInvalid)
	}
	negotiation, err := parseRPCEventNegotiation(input)
	if err != nil {
		return nil, 0, err
	}
	page, err := d.state.business.listAIConversationEventsPage(ctx, projectID, id, after, limit, responseBytes)
	if err != nil {
		return nil, 0, err
	}
	filtered := page.Items[:0]
	for _, event := range page.Items {
		if negotiation.allows(event.Kind) {
			filtered = append(filtered, event)
		}
	}
	page.Items = filtered
	payload := page.responsePayload()
	payload["eventContractVersion"] = negotiation.version
	payload["collaborationEvents"] = negotiation.supportsFullCollaborationContract()
	return payload, page.HighWatermark, nil
}

func (d dispatcher) regenerateAIConversationRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	messageID, messageOK := inputString(input, "messageId", 80)
	regenerationRequestID, requestOK := inputString(input, "regenerationRequestId", 80)
	workspaceMode, modeOK := optionalInputString(input, "workspaceMode", 32)
	workspaceToolsEnabled, toolsErr := d.aiWorkspaceToolsIntent(input)
	if !conversationOK || !messageOK || !requestOK || !modeOK || toolsErr != nil || uuid.Validate(conversationID) != nil ||
		uuid.Validate(messageID) != nil || uuid.Validate(regenerationRequestID) != nil {
		if toolsErr != nil {
			return nil, 0, toolsErr
		}
		return nil, 0, errRPCInvalid
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	current, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	if workspaceMode == "" {
		workspaceMode = current.WorkspaceMode
	} else {
		workspaceMode = normalizeAIWorkspaceMode(workspaceMode)
	}
	if !validAIWorkspaceMode(workspaceMode) {
		return nil, 0, errRPCInvalid
	}
	config, err := d.conversationAIConfig(current.ConfigID, current.ModelBinding.Model)
	if err != nil {
		return nil, 0, err
	}
	unlockDriver := d.state.aiConversationDriverLock(conversationID)
	defer unlockDriver()
	// The request ID is also the durable generation ID. A retry after client
	// restart can therefore discover the already-running or terminal assistant
	// message even when the in-memory operation-id bridge no longer exists.
	if existing, revision, existingErr := d.state.business.getAIConversationGenerationState(ctx, projectID, conversationID, regenerationRequestID); existingErr == nil {
		return map[string]any{
			"accepted": true, "replayed": true, "regeneratedFromMessageId": messageID, "regenerationRequestId": regenerationRequestID,
			"conversationId": conversationID, "generationId": existing.GenerationID, "revision": revision, "acceptedRevision": revision,
		}, revision, nil
	} else if !errors.Is(existingErr, errRPCRevision) {
		return nil, 0, existingErr
	}
	generationContext, releaseGeneration := contextWithoutPeerTransportCancellation(ctx)
	defer releaseGeneration()
	generationID := regenerationRequestID
	reservedContext, cancelReserved := context.WithCancel(generationContext)
	if reserveErr := d.state.reserveAIGeneration(conversationID, generationID, cancelReserved); reserveErr != nil {
		cancelReserved()
		return nil, 0, reserveErr
	}
	turn, err := d.state.business.beginAIConversationRegenerationWithGeneration(reservedContext, projectID, conversationID,
		messageID, generationID, workspaceMode, config, d.now().UTC())
	if err != nil {
		cancelReserved()
		if d.state.unregisterAIGeneration(conversationID, generationID) {
			go d.resumeAIAgentInbox(projectID, conversationID)
		}
		return nil, 0, err
	}
	turn.WorkspaceToolsEnabled = &workspaceToolsEnabled
	completed, err := d.executeAIConversationTurn(reservedContext, projectID, turn, turn.User.Content, turn.User.Attachments, config)
	cancelReserved()
	if err != nil {
		return nil, 0, err
	}
	if drained, ran, drainErr := d.drainAIAgentInbox(generationContext, projectID, conversationID); drainErr != nil {
		return nil, 0, drainErr
	} else if ran {
		completed = drained
	}
	return map[string]any{
		"accepted": true, "regeneratedFromMessageId": messageID, "regenerationRequestId": regenerationRequestID, "conversationId": conversationID,
		"generationId": turn.GenerationID, "revision": completed.Revision, "acceptedRevision": turn.Conversation.Revision,
	}, completed.Revision, nil
}

func (d dispatcher) validateAIConversationAttachments(ctx context.Context, projectID uuid.UUID, input rpcInput) ([]chatAttachmentReference, error) {
	raw, present := input["attachments"]
	if !present || raw == nil {
		return []chatAttachmentReference{}, nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) > 8 {
		return nil, errRPCInvalid
	}
	project, err := d.state.business.projectByID(ctx, projectID)
	if err != nil {
		return nil, err
	}
	result := make([]chatAttachmentReference, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	var totalSize uint64
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return nil, errRPCInvalid
		}
		id, idOK := inputString(rpcInput(item), "id", 80)
		relativePath, pathOK := inputString(rpcInput(item), "relativePath", 4096)
		name, nameOK := inputString(rpcInput(item), "name", 255)
		mimeType, mimeOK := inputString(rpcInput(item), "mimeType", 120)
		mediaType, _, mediaTypeErr := mime.ParseMediaType(mimeType)
		mimeType = strings.ToLower(mediaType)
		digest, digestOK := digestInput(rpcInput(item), "sha256")
		size, sizePresent, sizeOK := optionalUint64(rpcInput(item), "size")
		revision, revisionPresent, revisionOK := optionalUint64(rpcInput(item), "revision")
		if !idOK || uuid.Validate(id) != nil || !pathOK || !nameOK || !mimeOK || mediaTypeErr != nil || mimeType == "" || !digestOK ||
			!sizeOK || !sizePresent || size > 10<<20 || !revisionOK || !revisionPresent || revision == 0 ||
			strings.ContainsAny(mimeType, "\r\n") {
			return nil, errRPCInvalid
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, errRPCInvalid
		}
		resolved, normalized, err := secureExistingProjectPath(project, relativePath)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || uint64(info.Size()) != size || workspaceFileRevision(normalized, info) != revision || filepath.Base(normalized) != name {
			return nil, errRPCRevision
		}
		contents, err := readBoundedFile(resolved, 10<<20)
		if err != nil {
			return nil, err
		}
		actualDigest := sha256.Sum256(contents)
		if base64.RawURLEncoding.EncodeToString(actualDigest[:]) != digest {
			return nil, errRPCRevision
		}
		if strings.HasPrefix(mimeType, "image/") {
			if size == 0 || !slices.Contains([]string{"image/png", "image/jpeg", "image/webp", "image/gif"}, mimeType) ||
				http.DetectContentType(contents) != mimeType {
				return nil, errRPCInvalid
			}
		} else if _, _, err := decodeWorkspaceText(name, contents); err != nil {
			return nil, errRPCInvalid
		}
		after, err := os.Stat(resolved)
		if err != nil || workspaceFileRevision(normalized, after) != revision {
			return nil, firstError(err, errRPCRevision)
		}
		totalSize += size
		if totalSize > 32<<20 {
			return nil, errRPCInvalid
		}
		seen[id] = struct{}{}
		result = append(result, chatAttachmentReference{
			ID: id, RelativePath: normalized, Name: name, MimeType: mimeType, Size: size, SHA256: digest, Revision: revision,
		})
	}
	return result, nil
}

func (d dispatcher) aiPromptWithAttachments(ctx context.Context, projectID uuid.UUID, prompt string, attachments []chatAttachmentReference) (aiProviderPrompt, error) {
	if len(attachments) == 0 {
		return aiProviderPrompt{Text: prompt, Images: []aiPromptImage{}}, nil
	}
	project, err := d.state.business.projectByID(ctx, projectID)
	if err != nil {
		return aiProviderPrompt{}, err
	}
	result := aiProviderPrompt{Images: make([]aiPromptImage, 0, len(attachments))}
	var builder strings.Builder
	builder.WriteString(prompt)
	for _, attachment := range attachments {
		resolved, normalized, err := secureExistingProjectPath(project, attachment.RelativePath)
		if err != nil {
			return aiProviderPrompt{}, err
		}
		info, err := os.Stat(resolved)
		if err != nil || workspaceFileRevision(normalized, info) != attachment.Revision || uint64(info.Size()) != attachment.Size {
			return aiProviderPrompt{}, firstError(err, errRPCRevision)
		}
		data, err := readBoundedFile(resolved, 10<<20)
		if err != nil || uint64(len(data)) != attachment.Size {
			return aiProviderPrompt{}, firstError(err, errRPCRevision)
		}
		digest := sha256.Sum256(data)
		if base64.RawURLEncoding.EncodeToString(digest[:]) != attachment.SHA256 {
			return aiProviderPrompt{}, errRPCRevision
		}
		after, err := os.Stat(resolved)
		if err != nil || workspaceFileRevision(normalized, after) != attachment.Revision {
			return aiProviderPrompt{}, firstError(err, errRPCRevision)
		}
		name, path, mediaType := html.EscapeString(attachment.Name), html.EscapeString(attachment.RelativePath), html.EscapeString(attachment.MimeType)
		if strings.HasPrefix(attachment.MimeType, "image/") {
			builder.WriteString("\n\n<attachment_ref type=\"image\" name=\"")
			builder.WriteString(name)
			builder.WriteString("\" path=\"")
			builder.WriteString(path)
			builder.WriteString("\" mime=\"")
			builder.WriteString(mediaType)
			builder.WriteString("\" version=\"")
			builder.WriteString(attachment.SHA256)
			builder.WriteString("\" content=\"supplied-separately\" />")
			result.Images = append(result.Images, aiPromptImage{
				Name: attachment.Name, MimeType: attachment.MimeType, Base64Data: base64.StdEncoding.EncodeToString(data),
			})
			if builder.Len() > maximumAIHistoryBytes {
				return aiProviderPrompt{}, errRPCInvalid
			}
			continue
		}
		builder.WriteString("\n\n<attachment name=\"")
		builder.WriteString(name)
		builder.WriteString("\" path=\"")
		builder.WriteString(path)
		builder.WriteString("\" mime=\"")
		builder.WriteString(mediaType)
		builder.WriteString("\" version=\"")
		builder.WriteString(attachment.SHA256)
		builder.WriteString("\" trusted=\"false\">\n")
		content, _, err := decodeWorkspaceText(info.Name(), data)
		if err != nil {
			return aiProviderPrompt{}, errRPCInvalid
		} else {
			runes := []rune(content)
			if len(runes) > 40000 {
				content = string(runes[:40000]) + "\n[truncated]"
			}
			builder.WriteString(content)
		}
		builder.WriteString("\n</attachment>")
		if builder.Len() > maximumAIHistoryBytes {
			return aiProviderPrompt{}, errRPCInvalid
		}
	}
	result.Text = builder.String()
	if err := validateAIProviderPrompt(result); err != nil {
		return aiProviderPrompt{}, err
	}
	return result, nil
}

func aiHistoryWithAttachmentReferences(history []chatMessage) []chatMessage {
	result := make([]chatMessage, len(history))
	copy(result, history)
	for index := range result {
		message := &result[index]
		if message.Role != "user" || len(message.Attachments) == 0 {
			continue
		}
		var builder strings.Builder
		builder.WriteString(message.Content)
		builder.WriteString("\n\n<attachments>")
		for _, attachment := range message.Attachments {
			builder.WriteString("\n<attachment_ref name=\"")
			builder.WriteString(html.EscapeString(attachment.Name))
			builder.WriteString("\" path=\"")
			builder.WriteString(html.EscapeString(attachment.RelativePath))
			builder.WriteString("\" mime=\"")
			builder.WriteString(html.EscapeString(attachment.MimeType))
			builder.WriteString("\" version=\"")
			builder.WriteString(attachment.SHA256)
			builder.WriteString("\" content=\"omitted-from-active-context\" />")
		}
		builder.WriteString("\n</attachments>")
		message.Content = builder.String()
	}
	return result
}

func aiConversationPromptInput(input rpcInput) (string, bool) {
	for _, key := range []string{"prompt", "content"} {
		raw, exists := input[key]
		if !exists {
			continue
		}
		value, ok := raw.(string)
		value = strings.TrimSpace(value)
		return value, ok && utf8.ValidString(value) && len(value) <= 32<<10
	}
	return "", false
}

func estimateAITextTokens(value string) uint64 {
	if value == "" {
		return 0
	}
	var ascii, nonASCII uint64
	for _, point := range value {
		if point < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
	}
	// Most CJK code points consume about one token while Latin prose/code is
	// closer to four characters per token. This remains provider-independent,
	// but avoids the old 4x under-estimate for Chinese conversations.
	return max(uint64(1), nonASCII+(ascii+3)/4)
}

func (d dispatcher) emitAIConversationEvent(event aiConversationEvent) error {
	projectID, err := d.aiConversationProjectID()
	if err == nil && d.state != nil {
		d.state.publishAIConversationEvent(projectID, event)
	}
	if d.chatEvent != nil {
		return d.chatEvent(event)
	}
	return nil
}

func (d dispatcher) emitAIConversationEvents(events []aiConversationEvent) {
	for _, event := range events {
		if d.emitAIConversationEvent(event) != nil {
			return
		}
	}
}

func (d dispatcher) callConversationSend(ctx context.Context, input rpcInput) (any, uint64, error) {
	if d.state == nil || d.state.business == nil {
		return nil, 0, errRPCCapability
	}
	projectID, err := d.aiConversationProjectID()
	if err != nil {
		return nil, 0, err
	}
	conversationID, idOK := inputString(input, "conversationId", 80)
	prompt, promptOK := aiConversationPromptInput(input)
	messageID, messageOK := optionalInputString(input, "messageId", 80)
	workspaceMode, modeOK := optionalInputString(input, "workspaceMode", 32)
	destination, destinationOK := optionalInputString(input, "destination", 32)
	workspaceToolsEnabled, toolsErr := d.aiWorkspaceToolsIntent(input)
	if destination == "" {
		destination = aiInboxNextTurn
	}
	if !idOK || uuid.Validate(conversationID) != nil || !promptOK || !messageOK || !modeOK || !destinationOK || toolsErr != nil ||
		!validAIInboxDestination(destination) || messageID != "" && uuid.Validate(messageID) != nil {
		if toolsErr != nil {
			return nil, 0, toolsErr
		}
		return nil, 0, errRPCInvalid
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	if messageID == "" {
		messageID = uuid.NewString()
	}
	current, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	if workspaceMode == "" {
		workspaceMode = current.WorkspaceMode
	} else {
		workspaceMode = normalizeAIWorkspaceMode(workspaceMode)
	}
	if !validAIWorkspaceMode(workspaceMode) {
		return nil, 0, errRPCInvalid
	}
	config, err := d.conversationAIConfig(current.ConfigID, current.ModelBinding.Model)
	if err != nil {
		return nil, 0, err
	}
	attachments, err := d.validateAIConversationAttachments(ctx, projectID, input)
	if err != nil {
		return nil, 0, err
	}
	if prompt == "" && len(attachments) == 0 {
		return nil, 0, errRPCInvalid
	}
	now := d.now().UTC()
	if existing, found, findErr := d.state.business.findAIConversationUserMessage(ctx, projectID, conversationID, messageID); findErr != nil {
		return nil, 0, findErr
	} else if found {
		if existing.Content != prompt || !slices.Equal(existing.Attachments, attachments) {
			return nil, 0, errRPCRevision
		}
		return map[string]any{
			"accepted": true, "queued": false, "replayed": true, "conversationId": conversationID,
			"generationId": existing.GenerationID, "messageId": existing.ID, "destination": aiInboxNextTurn,
			"disposition": aiInboxNextTurn, "revision": current.Revision,
		}, current.Revision, nil
	}
	queuedInput := aiAgentInboxItem{
		ID: messageID, ConversationID: conversationID, Destination: destination, Prompt: prompt,
		Attachments: attachments, WorkspaceMode: workspaceMode, WorkspaceToolsEnabled: workspaceToolsEnabled, CreatedAt: now,
	}
	if queued, active, found, offerErr := d.enqueueForActiveAIGeneration(ctx, projectID, queuedInput); found {
		if offerErr != nil {
			return nil, 0, offerErr
		}
		return map[string]any{
			"accepted": true, "queued": true, "replayed": queued.Replayed, "conversationId": conversationID,
			"generationId": active.GenerationID, "messageId": queued.ID, "destination": queued.Destination,
			"disposition": queued.Destination, "revision": current.Revision,
		}, current.Revision, nil
	}
	unlockDriver := d.state.aiConversationDriverLock(conversationID)
	defer unlockDriver()
	// A driver may have started while this call waited for the conversation
	// lock. Recheck and join it through the same durable inbox path.
	if queued, active, found, offerErr := d.enqueueForActiveAIGeneration(ctx, projectID, queuedInput); found {
		if offerErr != nil {
			return nil, 0, offerErr
		}
		return map[string]any{
			"accepted": true, "queued": true, "replayed": queued.Replayed, "conversationId": conversationID,
			"generationId": active.GenerationID, "messageId": queued.ID, "destination": queued.Destination,
			"disposition": queued.Destination, "revision": current.Revision,
		}, current.Revision, nil
	}
	// A previous driver can leave durable work parked after a keep-inbox
	// cancellation, provider failure, or process restart. An idle delivery must
	// join that FIFO instead of bypassing older work with a direct turn.
	pending, err := d.state.business.listAIAgentInbox(ctx, projectID, conversationID)
	if err != nil {
		return nil, 0, err
	}
	if len(pending) > 0 {
		if queuedInput.Destination == aiInboxNextStep {
			queuedInput.Destination = aiInboxNextTurn
		}
		queued, enqueueErr := d.state.business.enqueueAIAgentInboxItem(ctx, projectID, queuedInput)
		if enqueueErr != nil {
			return nil, 0, enqueueErr
		}
		generationContext, releaseGeneration := contextWithoutPeerTransportCancellation(ctx)
		defer releaseGeneration()
		completed, ran, drainErr := d.drainAIAgentInbox(generationContext, projectID, conversationID)
		if drainErr != nil {
			return nil, 0, drainErr
		}
		if !ran {
			return nil, 0, errRPCRevision
		}
		delivered, found, findErr := d.state.business.findAIConversationUserMessage(
			ctx, projectID, conversationID, queued.ID,
		)
		if findErr != nil {
			return nil, 0, findErr
		}
		if !found {
			return nil, 0, errRPCRevision
		}
		return map[string]any{
			"accepted": true, "queued": true, "replayed": queued.Replayed, "conversationId": conversationID,
			"generationId": delivered.GenerationID, "messageId": queued.ID, "destination": queued.Destination,
			"disposition": queued.Destination, "revision": completed.Revision, "acceptedRevision": current.Revision,
		}, completed.Revision, nil
	}
	// Reserve the conversation before the durable begin. This closes the
	// cancellation window between inserting the streaming assistant row and
	// registering its driver.
	generationContext, releaseGeneration := contextWithoutPeerTransportCancellation(ctx)
	defer releaseGeneration()
	generationID := uuid.NewString()
	reservedContext, cancelReserved := context.WithCancel(generationContext)
	if reserveErr := d.state.reserveAIGeneration(conversationID, generationID, cancelReserved); reserveErr != nil {
		cancelReserved()
		return nil, 0, reserveErr
	}
	turn, err := d.state.business.beginAIConversationTurnWithGeneration(reservedContext, projectID, conversationID, messageID,
		generationID, prompt, workspaceMode, attachments, config, now)
	if err != nil {
		cancelReserved()
		if d.state.unregisterAIGeneration(conversationID, generationID) {
			go d.resumeAIAgentInbox(projectID, conversationID)
		}
		return nil, 0, err
	}
	turn.WorkspaceToolsEnabled = &workspaceToolsEnabled
	if turn.Replayed {
		cancelReserved()
		if d.state.unregisterAIGeneration(conversationID, generationID) {
			go d.resumeAIAgentInbox(projectID, conversationID)
		}
		return map[string]any{
			"accepted": true, "queued": false, "replayed": true, "conversationId": conversationID,
			"generationId": turn.GenerationID, "messageId": messageID, "destination": aiInboxNextTurn,
			"disposition": aiInboxNextTurn, "revision": turn.Conversation.Revision,
		}, turn.Conversation.Revision, nil
	}
	completed, err := d.executeAIConversationTurn(reservedContext, projectID, turn, prompt, attachments, config)
	cancelReserved()
	if err != nil {
		return nil, 0, err
	}
	if drained, ran, drainErr := d.drainAIAgentInbox(generationContext, projectID, conversationID); drainErr != nil {
		return nil, 0, drainErr
	} else if ran {
		completed = drained
	}
	return map[string]any{
		"accepted": true, "queued": false, "replayed": false, "conversationId": conversationID,
		"generationId": turn.GenerationID, "messageId": messageID, "destination": aiInboxNextTurn,
		"disposition": aiInboxNextTurn, "revision": completed.Revision, "acceptedRevision": turn.Conversation.Revision,
	}, completed.Revision, nil
}

func (d dispatcher) aiWorkspaceToolsIntent(input rpcInput) (bool, error) {
	requested, valid := optionalInputBool(input, "enableWorkspaceTools", false)
	_, present := input["enableWorkspaceTools"]
	if !valid {
		return false, errRPCInvalid
	}
	if present && requested && d.scope != "remote.peer.ai.tools" {
		return false, errRPCAIToolsScopeRequired
	}
	if present {
		return requested, nil
	}
	// Legacy tools-scope callers predate the explicit intent field. Preserve
	// their behavior while every new browser request sends the boolean.
	return d.scope == "remote.peer.ai.tools", nil
}

func splitAIConversationUTF8(value string, maximumBytes int) []string {
	if value == "" || maximumBytes < 1 || !utf8.ValidString(value) {
		return nil
	}
	if len(value) <= maximumBytes {
		return []string{value}
	}
	result := make([]string, 0, (len(value)+maximumBytes-1)/maximumBytes)
	for start := 0; start < len(value); {
		end := min(start+maximumBytes, len(value))
		for end > start && !utf8.ValidString(value[start:end]) {
			end--
		}
		if end == start {
			return nil
		}
		result = append(result, value[start:end])
		start = end
	}
	return result
}

func (d dispatcher) executeAIConversationTurn(ctx context.Context, projectID uuid.UUID, turn aiConversationTurn, prompt string, attachments []chatAttachmentReference, config aiConfig) (conversationView, error) {
	conversationID := turn.Conversation.ID
	workspaceToolsEnabled := d.aiWorkspaceToolsEnabled || d.scope == "remote.peer.ai.tools"
	if turn.WorkspaceToolsEnabled != nil {
		workspaceToolsEnabled = *turn.WorkspaceToolsEnabled
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	generationContext, cancelGeneration := context.WithCancel(ctx)
	if registrationErr := d.state.tryRegisterAIGeneration(conversationID, turn.GenerationID, cancelGeneration); registrationErr != nil {
		cancelGeneration()
		failureCode := "generation_registry_busy"
		if errors.Is(registrationErr, errRPCConversationGenerationActive) {
			failureCode = "generation_active"
		} else if errors.Is(registrationErr, errRPCAgentGenerationCapacity) {
			failureCode = "generation_capacity"
		}
		_, _, _, _ = d.state.business.abortAIConversationTurn(context.Background(), projectID, conversationID, turn.GenerationID,
			turn.Assistant.ID, "failed", failureCode, chatProviderRun{Provider: config.Provider, Model: config.Model, FinishReason: "error", AttemptCount: 1}, d.now().UTC())
		return conversationView{}, registrationErr
	}
	d.state.aiGenerationMu.Lock()
	if active, found := d.state.aiGenerations[conversationID]; found && active.GenerationID == turn.GenerationID {
		active.WorkspaceToolsEnabled = workspaceToolsEnabled
		d.state.aiGenerations[conversationID] = active
	}
	d.state.aiGenerationMu.Unlock()
	defer func() {
		cancelGeneration()
		if d.state.unregisterAIGeneration(conversationID, turn.GenerationID) {
			go d.resumeAIAgentInbox(projectID, conversationID)
		}
	}()

	providerPrompt, err := d.aiPromptWithAttachments(generationContext, projectID, prompt, attachments)
	if err != nil {
		_, _, events, _ := d.state.business.abortAIConversationTurn(context.Background(), projectID, conversationID, turn.GenerationID,
			turn.Assistant.ID, "failed", "attachment_changed", chatProviderRun{Provider: config.Provider, Model: config.Model, FinishReason: "error", AttemptCount: 1}, d.now().UTC())
		d.emitAIConversationEvents(events)
		return conversationView{}, err
	}
	effective := effectiveAIConfig(config)
	baseSystemPrompt := effective.SystemPrompt
	history := aiHistoryWithAttachmentReferences(turn.History)
	persistedSummary, err := d.state.business.loadAIContextSummary(generationContext, projectID, conversationID)
	if err != nil {
		_, _, events, _ := d.state.business.abortAIConversationTurn(context.Background(), projectID, conversationID, turn.GenerationID,
			turn.Assistant.ID, "failed", "context_summary_load_failed", chatProviderRun{Provider: config.Provider, Model: config.Model, FinishReason: "error", AttemptCount: 1}, d.now().UTC())
		d.emitAIConversationEvents(events)
		return conversationView{}, err
	}
	history = applyPersistedAIContextSummary(history, persistedSummary, d.now().UTC())
	provider := providerFor(d)
	toolRuntime, err := d.conversationToolRuntime(generationContext, projectID, turn, effective)
	if _, supportsToolPrompt := provider.(promptEventStreamingAIProvider); !supportsToolPrompt {
		// Text-only provider shims (including older compatible adapters) cannot
		// receive tool schemas or return structured calls. Keep normal chat
		// available and expose collaboration as soon as that provider implements
		// the prompt/tool contract.
		toolRuntime = nil
	}
	if err == nil && toolRuntime != nil {
		providerPrompt.Tools = toolRuntime.definitions
		hasWebTools, hasTaskTools := false, false
		for _, definition := range toolRuntime.definitions {
			if definition.Name == "terminal_open" {
				effective.SystemPrompt = strings.TrimSpace(effective.SystemPrompt + "\n\n" + aiPersistentTerminalSystemGuidance)
			}
			if definition.Name == "web_search" {
				hasWebTools = true
			}
			if definition.Name == "task_list" {
				hasTaskTools = true
			}
		}
		if hasWebTools {
			effective.SystemPrompt = strings.TrimSpace(effective.SystemPrompt + "\n\n" + aiWebSystemGuidance)
		}
		if hasTaskTools {
			effective.SystemPrompt = strings.TrimSpace(effective.SystemPrompt + "\n\n" + aiTaskSystemGuidance)
		}
	}
	baseSystemPrompt = effective.SystemPrompt
	effective.SystemPrompt = strings.TrimSpace(baseSystemPrompt + "\n\n" + aiCollaborationSystemGuidance(turn.Conversation, turn.GoalRound))
	if skillCatalog := d.aiSkillCatalogForTurn(generationContext, projectID); skillCatalog != "" {
		effective.SystemPrompt = strings.TrimSpace(effective.SystemPrompt + "\n\n" + skillCatalog)
	}
	// The per-round refresh below must keep the skill catalog, so capture the
	// base prompt only after every guidance block is in place.
	baseSystemPrompt = effective.SystemPrompt
	providerRun := chatProviderRun{Provider: config.Provider, Model: config.Model}
	contentOffset := 0
	trailingTextLineBreaks := 0
	const persistentDeltaFlushInterval = 80 * time.Millisecond
	// Provider streams often arrive token-by-token. Persisting each token makes
	// replay grow with model chunking rather than user-visible output, so merge
	// adjacent fields into bounded durable deltas. Tool/approval/terminal paths
	// explicitly flush below to preserve ordering.
	var pendingDelta strings.Builder
	pendingField := ""
	pendingStartedAt := time.Time{}
	flushDeltas := func() error {
		if pendingField == "" || pendingDelta.Len() == 0 {
			pendingField, pendingStartedAt = "", time.Time{}
			pendingDelta.Reset()
			return nil
		}
		chunk := pendingDelta.String()
		field := pendingField
		pendingField, pendingStartedAt = "", time.Time{}
		pendingDelta.Reset()
		var event aiConversationEvent
		var err error
		if field == "text" {
			_, event, err = d.state.business.appendAIConversationTextDelta(generationContext, projectID, conversationID,
				turn.GenerationID, turn.Assistant.ID, chunk, d.now().UTC())
		} else {
			_, event, err = d.state.business.appendAIConversationReasoningDelta(generationContext, projectID, conversationID,
				turn.GenerationID, turn.Assistant.ID, chunk, d.now().UTC())
		}
		if err != nil {
			return err
		}
		return d.emitAIConversationEvent(event)
	}
	appendPersistentDelta := func(field, value string) error {
		pieces := splitAIConversationUTF8(value, maximumAIPersistentDeltaBytes)
		if len(pieces) == 0 {
			return errAIProvider
		}
		for _, piece := range pieces {
			now := d.now().UTC()
			if pendingField != "" && (pendingField != field || pendingDelta.Len()+len(piece) > maximumAIPersistentDeltaBytes ||
				!pendingStartedAt.IsZero() && now.Sub(pendingStartedAt) >= persistentDeltaFlushInterval) {
				if err := flushDeltas(); err != nil {
					return err
				}
			}
			if pendingField == "" {
				pendingField, pendingStartedAt = field, now
			}
			pendingDelta.WriteString(piece)
			if pendingDelta.Len() >= maximumAIPersistentDeltaBytes {
				if err := flushDeltas(); err != nil {
					return err
				}
			}
		}
		return nil
	}
	appendTextDelta := func(chunk string) error {
		if err := appendPersistentDelta("text", chunk); err != nil {
			return err
		}
		contentOffset += aiUTF16Length(chunk)
		trailingTextLineBreaks = aiConversationTrailingLineBreaks(trailingTextLineBreaks, chunk)
		return nil
	}
	appendReasoningDelta := func(chunk string) error {
		return appendPersistentDelta("reasoning", chunk)
	}
	eventStreaming, canStreamEvents := provider.(eventStreamingAIProvider)
	streaming, canStream := provider.(streamingAIProvider)
	promptStreaming, canStreamPrompt := provider.(promptEventStreamingAIProvider)
	providerUsage := chatUsage{}
	toolExchanges := make([]aiProviderToolExchange, 0)
	seenToolCallIDs := make(map[string]struct{})
	var noProgressRounds uint32
	lastToolFingerprint := ""
	failureCode := "provider_unavailable"
	contextOverflowRecoveryUsed := false
	applyContextCompaction := func(trigger aiContextCompactionTrigger) (bool, error) {
		before := estimateAIProviderRequestTokens(effective, history, providerPrompt)
		compacted, compactErr := compactAIProviderContext(
			generationContext,
			provider,
			effective,
			history,
			providerPrompt,
			conversationID,
			d.now().UTC(),
			trigger,
		)
		if compactErr != nil {
			return false, compactErr
		}
		if compacted.Summary != nil {
			if saveErr := d.state.business.saveAIContextSummary(generationContext, projectID, *compacted.Summary); saveErr != nil {
				return false, saveErr
			}
		}
		history = compacted.History
		providerPrompt = compacted.Prompt
		toolExchanges = providerPrompt.ToolExchanges
		return compacted.Changed && compacted.EstimatedTokens < before, nil
	}
	// Match deepseek-harness's continuation model: provider/tool counts are
	// observability fields, not arbitrary terminal budgets. Real safety
	// boundaries remain cancellation, context/output limits, per-tool timeouts,
	// and repeated no-progress detection.
	for round := uint32(1); err == nil; round++ {
		steering, claimErr := d.claimAIAgentStep(generationContext, projectID, conversationID, turn.GenerationID, false)
		if claimErr != nil {
			err, failureCode = claimErr, "inbox_claim_failed"
			break
		}
		if len(steering) > 0 {
			steeringText, steeringImages, steeringErr := d.steeringProviderPrompt(generationContext, projectID, steering)
			if steeringErr != nil {
				err, failureCode = steeringErr, "inbox_attachment_changed"
				break
			}
			providerPrompt.Text = strings.TrimSpace(providerPrompt.Text + "\n\n" + steeringText)
			providerPrompt.Images = append(providerPrompt.Images, steeringImages...)
		}
		providerRun.AttemptCount = round
		providerPrompt.ToolExchanges = toolExchanges
		if _, compactErr := applyContextCompaction(aiContextPressure); compactErr != nil {
			err, failureCode = compactErr, "context_summary_failed"
			break
		}
		providerRun.FinishReason = ""
		var roundText, roundReasoning strings.Builder
		roundUsage := chatUsage{}
		roundToolCalls := make([]aiProviderToolCall, 0)
		roundToolOffsets := make(map[string]int)
		// Some OpenAI-compatible coding gateways expose several committed
		// assistant updates through one completion stream. Their reasoning/text
		// transitions retain the update boundary, but concatenating only the text
		// deltas used to turn the whole run into one unbroken paragraph. Preserve
		// that boundary in the durable visible body while keeping roundText equal
		// to the provider's original bytes for tool follow-up context.
		textBoundaryPending := contentOffset > 0
		appendRoundText := func(chunk string) error {
			visibleChunk := chunk
			if textBoundaryPending && contentOffset > 0 {
				visibleChunk = ensureAIConversationParagraphBoundary(trailingTextLineBreaks, chunk)
			}
			textBoundaryPending = false
			if err := appendTextDelta(visibleChunk); err != nil {
				return err
			}
			roundText.WriteString(chunk)
			return nil
		}
		handleProviderEvent := func(event aiProviderStreamEvent) error {
			if len(event.ProviderRequestID) > 256 || !utf8.ValidString(event.ProviderRequestID) ||
				len(event.FinishReason) > 80 || !utf8.ValidString(event.FinishReason) {
				return errAIProvider
			}
			if event.ProviderRequestID != "" {
				providerRun.ProviderRequestID = event.ProviderRequestID
			}
			if event.FinishReason != "" {
				providerRun.FinishReason = event.FinishReason
			}
			switch event.Kind {
			case "text":
				if event.Delta == "" {
					return nil
				}
				return appendRoundText(event.Delta)
			case "reasoning":
				if event.Delta == "" {
					return nil
				}
				if roundText.Len() > 0 {
					textBoundaryPending = true
				}
				if err := appendReasoningDelta(event.Delta); err != nil {
					return err
				}
				roundReasoning.WriteString(event.Delta)
				return nil
			case "tool_calls":
				if err := flushDeltas(); err != nil {
					return err
				}
				if toolRuntime == nil || len(event.ToolCalls) == 0 {
					return errAIProvider
				}
				for _, call := range event.ToolCalls {
					if !validAIProviderToolCall(call) {
						return errAIProvider
					}
					if _, duplicate := seenToolCallIDs[call.ID]; duplicate {
						return errAIProvider
					}
					seenToolCallIDs[call.ID] = struct{}{}
					roundToolOffsets[call.ID] = contentOffset
					roundToolCalls = append(roundToolCalls, call)
				}
				return nil
			case "usage":
				roundUsage = mergeAIUsage(roundUsage, event.Usage)
				return nil
			case "completed":
				return nil
			default:
				return errAIProvider
			}
		}
		invokeProvider := func() error {
			if (len(providerPrompt.Images) > 0 || len(providerPrompt.Tools) > 0 || len(providerPrompt.ToolExchanges) > 0) && !canStreamPrompt {
				return errAIProvider
			}
			if canStreamPrompt {
				return promptStreaming.CompletePromptEventStream(generationContext, effective, history, providerPrompt, handleProviderEvent)
			}
			if canStreamEvents {
				return eventStreaming.CompleteEventStream(generationContext, effective, history, providerPrompt.Text, handleProviderEvent)
			}
			if canStream {
				return streaming.CompleteStream(generationContext, effective, history, providerPrompt.Text, func(chunk string) error {
					return appendRoundText(chunk)
				})
			}
			answer, completeErr := provider.Complete(generationContext, effective, history, providerPrompt.Text)
			if completeErr != nil {
				return completeErr
			}
			return appendRoundText(answer)
		}
		err = invokeProvider()
		contextOverflow := isAIContextOverflowError(err) || err == nil && isAIContextOverflowFinishReason(providerRun.FinishReason)
		if contextOverflow && !contextOverflowRecoveryUsed && roundText.Len() == 0 && roundReasoning.Len() == 0 && len(roundToolCalls) == 0 {
			progressed, compactErr := applyContextCompaction(aiContextOverflow)
			if compactErr != nil {
				err, failureCode = compactErr, "context_summary_failed"
			} else if progressed {
				contextOverflowRecoveryUsed = true
				providerRun.FinishReason = ""
				roundUsage = chatUsage{}
				err = invokeProvider()
				contextOverflow = isAIContextOverflowError(err) || err == nil && isAIContextOverflowFinishReason(providerRun.FinishReason)
			}
		}
		if contextOverflow {
			err, failureCode = errAIContextLimit, "context_limit"
		}
		if err != nil {
			switch {
			case errors.Is(err, errAIProviderStreamTruncated):
				failureCode = "provider_stream_truncated"
			case errors.Is(err, errAIProviderRequestTimeout), errors.Is(err, context.DeadlineExceeded):
				failureCode = "provider_timeout"
			}
		}
		// Flush a final partial coalescing window before changing rounds or
		// recording a terminal state. This also preserves partial output when a
		// provider ends with an error after emitting useful text.
		if flushErr := flushDeltas(); flushErr != nil && err == nil {
			err = flushErr
		}
		if err != nil {
			break
		}
		providerUsage = addAIUsage(providerUsage, roundUsage)
		if len(roundToolCalls) == 0 {
			stoppingSteering, stoppingErr := d.claimAIAgentStep(generationContext, projectID, conversationID, turn.GenerationID, true)
			if stoppingErr != nil {
				err, failureCode = stoppingErr, "inbox_claim_failed"
				break
			}
			if len(stoppingSteering) > 0 {
				steeringText, steeringImages, steeringErr := d.steeringProviderPrompt(generationContext, projectID, stoppingSteering)
				if steeringErr != nil {
					err, failureCode = steeringErr, "inbox_attachment_changed"
					break
				}
				if len(toolExchanges) == 0 {
					history = append(history, chatMessage{Role: "assistant", Content: roundText.String()})
					providerPrompt.Text = steeringText
					providerPrompt.Images = steeringImages
				} else {
					providerPrompt.Text = strings.TrimSpace(providerPrompt.Text + "\n\n" + steeringText)
					providerPrompt.Images = append(providerPrompt.Images, steeringImages...)
				}
				continue
			}
			break
		}
		// Persist every tool-run record in model order first so the stored
		// tool_runs array stays deterministic, then execute with barrier
		// scheduling: parallel-classified reads run concurrently inside a
		// bounded pool, writes and terminal input stay exclusive.
		runs := make([]chatToolRun, len(roundToolCalls))
		startedAts := make([]time.Time, len(roundToolCalls))
		for index, call := range roundToolCalls {
			run, startedAt, startErr := toolRuntime.startCall(generationContext, d, turn, call, roundToolOffsets[call.ID])
			if startErr != nil {
				err, failureCode = startErr, "tool_execution_failed"
				break
			}
			runs[index], startedAts[index] = run, startedAt
		}
		if err != nil {
			break
		}
		results, toolErr := executeAIToolCallRound(generationContext, toolRuntime, d, turn, roundToolCalls, runs, startedAts)
		if toolErr != nil {
			err, failureCode = toolErr, "tool_execution_failed"
			break
		}
		fingerprint, fingerprintErr := aiToolExchangeFingerprint(roundToolCalls, results)
		if fingerprintErr != nil {
			err, failureCode = fingerprintErr, "tool_execution_failed"
			break
		}
		if fingerprint == lastToolFingerprint {
			noProgressRounds++
		} else {
			lastToolFingerprint, noProgressRounds = fingerprint, 0
		}
		toolExchanges = append(toolExchanges, aiProviderToolExchange{
			AssistantText: roundText.String(), AssistantReasoning: roundReasoning.String(), Calls: roundToolCalls, Results: results,
		})
		if currentConversation, refreshErr := d.state.business.getAIConversation(generationContext, projectID, conversationID); refreshErr == nil {
			effective.SystemPrompt = strings.TrimSpace(baseSystemPrompt + "\n\n" + aiCollaborationSystemGuidance(currentConversation, turn.GoalRound))
		}
		if noProgressRounds >= maximumAIAgentNoProgressRounds {
			err, failureCode = aiToolExecutionFailure("agent_no_progress"), "agent_no_progress"
			break
		}
		// Advisory self-correction chance before the hard limit ends the turn.
		if reminder := aiToolRepeatReminder(roundToolCalls, noProgressRounds); reminder != "" {
			providerPrompt.Text = strings.TrimSpace(providerPrompt.Text + "\n\n" + reminder)
		}
	}
	if err != nil || generationContext.Err() != nil {
		status, errorCode := "failed", failureCode
		if generationContext.Err() != nil {
			status, errorCode = "stopped", "cancelled"
		}
		providerRun.FinishReason = "error"
		if status == "stopped" {
			providerRun.FinishReason = "cancelled"
		}
		_, _, events, abortErr := d.state.business.abortAIConversationTurn(context.Background(), projectID, conversationID,
			turn.GenerationID, turn.Assistant.ID, status, errorCode, providerRun, d.now().UTC())
		if abortErr == nil {
			d.emitAIConversationEvents(events)
		} else if !errors.Is(abortErr, errRPCRevision) {
			return conversationView{}, abortErr
		}
		if generationContext.Err() != nil {
			return conversationView{}, generationContext.Err()
		}
		if isAIConversationToolLimit(err) {
			return conversationView{}, err
		}
		return conversationView{}, wrapAIError(err)
	}
	if providerRun.FinishReason == "" {
		providerRun.FinishReason = "stop"
	}
	inputTokens := estimateAITextTokens(providerPrompt.Text)
	for _, message := range history {
		inputTokens += estimateAITextTokens(message.Content)
	}
	messagePage, err := d.state.business.listAIConversationMessages(generationContext, projectID, conversationID, 0, 1)
	if err != nil || len(messagePage.Items) != 1 {
		return conversationView{}, firstError(err, errRPCRevision)
	}
	outputTokens := estimateAITextTokens(messagePage.Items[0].Content)
	usage := providerUsage
	if usage.TotalTokens == 0 {
		usage = chatUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens}
	}
	_, completed, events, err := d.state.business.finishAIConversationTurn(generationContext, projectID, conversationID,
		turn.GenerationID, turn.Assistant.ID, usage, providerRun, d.now().UTC())
	if err != nil {
		return conversationView{}, err
	}
	d.emitAIConversationEvents(events)
	return completed, nil
}

func mergeAIUsage(current, next chatUsage) chatUsage {
	current.InputTokens = max(current.InputTokens, next.InputTokens)
	current.OutputTokens = max(current.OutputTokens, next.OutputTokens)
	current.ReasoningTokens = max(current.ReasoningTokens, next.ReasoningTokens)
	current.CachedInputTokens = max(current.CachedInputTokens, next.CachedInputTokens)
	current.TotalTokens = max(current.TotalTokens, next.TotalTokens, current.InputTokens+current.OutputTokens)
	return current
}

// ensureAIConversationParagraphBoundary adds only the line breaks missing at
// the join. Providers that already delimit their next text block therefore
// keep their exact spacing, while merged phase transitions gain one Markdown
// paragraph boundary.
func ensureAIConversationParagraphBoundary(trailingLineBreaks int, next string) string {
	missing := 2 - min(2, trailingLineBreaks+aiConversationLeadingLineBreaks(next))
	if missing <= 0 {
		return next
	}
	return strings.Repeat("\n", missing) + next
}

func aiConversationLeadingLineBreaks(value string) int {
	count := 0
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\n':
			count++
			if count == 2 {
				return count
			}
		case '\r':
			// CR belongs to the same logical line break as a following LF.
		default:
			return count
		}
	}
	return count
}

func aiConversationTrailingLineBreaks(previous int, chunk string) int {
	count := 0
	onlyLineBreaks := true
	for index := len(chunk) - 1; index >= 0; index-- {
		switch chunk[index] {
		case '\n':
			count++
			if count == 2 {
				return count
			}
		case '\r':
			// Ignore the CR half of CRLF while counting logical line breaks.
		default:
			onlyLineBreaks = false
			index = -1
		}
	}
	if onlyLineBreaks {
		count += previous
	}
	return min(2, count)
}
