package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	defaultAIGoalRounds           = uint64(256)
	maximumAIGoalRounds           = uint64(10000)
	maximumAIGoalObjectiveBytes   = 32 << 10
	maximumAIGoalBlockReasonBytes = 4096
)

type aiGoalBlockReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type aiGoalSnapshot struct {
	ID            string             `json:"id"`
	Revision      uint64             `json:"revision"`
	Objective     string             `json:"objective"`
	Phase         string             `json:"phase"`
	BlockedReason *aiGoalBlockReason `json:"blockedReason,omitempty"`
	RoundsStarted uint64             `json:"roundsStarted"`
	MaxGoalRounds uint64             `json:"maxGoalRounds"`
	CreatedAt     time.Time          `json:"createdAt"`
	UpdatedAt     time.Time          `json:"updatedAt"`
}

type aiGoalRoundSource struct {
	GoalID   string `json:"goalId"`
	Revision uint64 `json:"revision"`
	Round    uint64 `json:"round"`
}

func cloneAIGoalSnapshot(value *aiGoalSnapshot) *aiGoalSnapshot {
	if value == nil {
		return nil
	}
	copy := *value
	if value.BlockedReason != nil {
		reason := *value.BlockedReason
		copy.BlockedReason = &reason
	}
	return &copy
}

func validAIGoalCode(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	previousDash := false
	for _, character := range value {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-'
		if !valid || character == '-' && previousDash {
			return false
		}
		previousDash = character == '-'
	}
	return !previousDash
}

func validAIGoalSnapshot(value *aiGoalSnapshot) bool {
	if value == nil {
		return true
	}
	objective := strings.TrimSpace(value.Objective)
	if uuid.Validate(value.ID) != nil || value.Revision == 0 || objective == "" ||
		len(objective) > maximumAIGoalObjectiveBytes || !utf8.ValidString(objective) ||
		!slices.Contains([]string{"active", "paused", "blocked", "complete"}, value.Phase) ||
		value.RoundsStarted > maximumAIGoalRounds || value.MaxGoalRounds == 0 || value.MaxGoalRounds > maximumAIGoalRounds ||
		value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() || value.UpdatedAt.Before(value.CreatedAt) {
		return false
	}
	if value.Phase != "blocked" {
		return value.BlockedReason == nil
	}
	return value.BlockedReason != nil && validAIGoalCode(value.BlockedReason.Code) &&
		strings.TrimSpace(value.BlockedReason.Message) != "" && len(value.BlockedReason.Message) <= maximumAIGoalBlockReasonBytes &&
		utf8.ValidString(value.BlockedReason.Message)
}

func (state *agentState) armAIGoal(conversationID, goalID string) {
	if state == nil {
		return
	}
	state.aiGoalMu.Lock()
	if state.aiGoalArmed == nil {
		state.aiGoalArmed = map[string]string{}
	}
	state.aiGoalArmed[conversationID] = goalID
	state.aiGoalMu.Unlock()
}

func (state *agentState) disarmAIGoal(conversationID, goalID string) {
	if state == nil {
		return
	}
	state.aiGoalMu.Lock()
	if current, found := state.aiGoalArmed[conversationID]; found && (goalID == "" || current == goalID) {
		delete(state.aiGoalArmed, conversationID)
	}
	state.aiGoalMu.Unlock()
}

func (state *agentState) isAIGoalArmed(conversationID, goalID string) bool {
	if state == nil {
		return false
	}
	state.aiGoalMu.Lock()
	defer state.aiGoalMu.Unlock()
	return state.aiGoalArmed[conversationID] == goalID
}

func (state *agentState) requestAIGoalWake(conversationID string) bool {
	if state == nil {
		return false
	}
	state.aiGenerationMu.Lock()
	defer state.aiGenerationMu.Unlock()
	active, found := state.aiGenerations[conversationID]
	if !found {
		return false
	}
	active.WakeRequested = true
	state.aiGenerations[conversationID] = active
	return true
}

func (d dispatcher) scheduleAIGoal(projectID uuid.UUID, conversationID string) {
	if d.state == nil || d.state.requestAIGoalWake(conversationID) {
		return
	}
	go d.resumeAIAgentInbox(projectID, conversationID)
}

func (d dispatcher) withAIGoalActivation(value conversationView) conversationView {
	value.GoalArmed = value.Goal != nil && value.Goal.Phase == "active" && d.state.isAIGoalArmed(value.ID, value.Goal.ID)
	return value
}

func requireCurrentAIGoal(collaboration *aiConversationCollaboration, goalID string, revision uint64) (*aiGoalSnapshot, error) {
	if collaboration == nil || collaboration.Goal == nil {
		return nil, errRPCNotFound
	}
	if collaboration.Goal.ID != goalID || collaboration.Goal.Revision != revision {
		return nil, errRPCRevision
	}
	return collaboration.Goal, nil
}

func nextAIGoalUpdatedAt(goal *aiGoalSnapshot, now time.Time) time.Time {
	now = now.UTC()
	if now.Before(goal.UpdatedAt) {
		return goal.UpdatedAt
	}
	return now
}

func (d dispatcher) createAIGoal(
	ctx context.Context,
	projectID uuid.UUID,
	conversationID, objective string,
	maxGoalRounds uint64,
	generationID, messageID, source string,
) (conversationView, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" || len(objective) > maximumAIGoalObjectiveBytes || !utf8.ValidString(objective) ||
		maxGoalRounds == 0 || maxGoalRounds > maximumAIGoalRounds {
		return conversationView{}, errRPCInvalid
	}
	now := d.now().UTC()
	goal := &aiGoalSnapshot{
		ID: uuid.NewString(), Revision: 1, Objective: objective, Phase: "active",
		RoundsStarted: 0, MaxGoalRounds: maxGoalRounds, CreatedAt: now, UpdatedAt: now,
	}
	value, event, err := d.state.business.updateAIConversationCollaboration(ctx, projectID, conversationID,
		generationID, messageID, "chat.goal.changed", map[string]any{"operation": "create", "source": source, "goal": goal},
		func(collaboration *aiConversationCollaboration) error {
			if collaboration.Subagent != nil || collaboration.Goal != nil && collaboration.Goal.Phase != "complete" {
				return errRPCRevision
			}
			collaboration.Goal = cloneAIGoalSnapshot(goal)
			return nil
		}, now)
	if err != nil {
		return conversationView{}, err
	}
	if err := d.emitAIConversationEvent(event); err != nil {
		return conversationView{}, err
	}
	d.state.armAIGoal(conversationID, goal.ID)
	d.scheduleAIGoal(projectID, conversationID)
	return d.withAIGoalActivation(value), nil
}

func (d dispatcher) editAIGoal(
	ctx context.Context,
	projectID uuid.UUID,
	conversationID, goalID string,
	revision uint64,
	objective *string,
	maxGoalRounds *uint64,
	generationID, messageID, source string,
) (conversationView, error) {
	if objective == nil && maxGoalRounds == nil {
		return conversationView{}, errRPCInvalid
	}
	if objective != nil {
		normalized := strings.TrimSpace(*objective)
		if normalized == "" || len(normalized) > maximumAIGoalObjectiveBytes || !utf8.ValidString(normalized) {
			return conversationView{}, errRPCInvalid
		}
		*objective = normalized
	}
	if maxGoalRounds != nil && (*maxGoalRounds == 0 || *maxGoalRounds > maximumAIGoalRounds) {
		return conversationView{}, errRPCInvalid
	}
	now := d.now().UTC()
	value, event, err := d.state.business.updateAIConversationCollaboration(ctx, projectID, conversationID,
		generationID, messageID, "chat.goal.changed", map[string]any{"operation": "edit", "source": source},
		func(collaboration *aiConversationCollaboration) error {
			goal, err := requireCurrentAIGoal(collaboration, goalID, revision)
			if err != nil {
				return err
			}
			if objective != nil {
				goal.Objective = *objective
			}
			if maxGoalRounds != nil {
				goal.MaxGoalRounds = *maxGoalRounds
			}
			goal.Revision++
			goal.UpdatedAt = nextAIGoalUpdatedAt(goal, now)
			return nil
		}, now)
	if err != nil {
		return conversationView{}, err
	}
	if err := d.emitAIConversationEvent(event); err != nil {
		return conversationView{}, err
	}
	if value.Goal != nil && value.Goal.Phase == "active" && d.state.isAIGoalArmed(conversationID, value.Goal.ID) {
		d.scheduleAIGoal(projectID, conversationID)
	}
	return d.withAIGoalActivation(value), nil
}

func (d dispatcher) transitionAIGoal(
	ctx context.Context,
	projectID uuid.UUID,
	conversationID, goalID string,
	revision uint64,
	action string,
	reason *aiGoalBlockReason,
	generationID, messageID, source string,
) (conversationView, error) {
	allowed := map[string][]string{
		"pause":    {"active"},
		"resume":   {"active", "paused", "blocked"},
		"complete": {"active", "paused", "blocked"},
		"blocked":  {"active"},
	}
	if _, ok := allowed[action]; !ok || action == "blocked" &&
		(reason == nil || !validAIGoalCode(reason.Code) || strings.TrimSpace(reason.Message) == "" || len(reason.Message) > maximumAIGoalBlockReasonBytes || !utf8.ValidString(reason.Message)) {
		return conversationView{}, errRPCInvalid
	}
	if action == "resume" && d.state.isAIGoalArmed(conversationID, goalID) {
		return conversationView{}, errRPCRevision
	}
	now := d.now().UTC()
	value, event, err := d.state.business.updateAIConversationCollaboration(ctx, projectID, conversationID,
		generationID, messageID, "chat.goal.changed", map[string]any{"operation": action, "source": source, "reason": reason},
		func(collaboration *aiConversationCollaboration) error {
			goal, err := requireCurrentAIGoal(collaboration, goalID, revision)
			if err != nil {
				return err
			}
			if !slices.Contains(allowed[action], goal.Phase) {
				return errRPCRevision
			}
			if action == "resume" && goal.RoundsStarted >= goal.MaxGoalRounds {
				return errRPCRevision
			}
			goal.Revision++
			goal.UpdatedAt = nextAIGoalUpdatedAt(goal, now)
			goal.BlockedReason = nil
			switch action {
			case "pause":
				goal.Phase = "paused"
			case "resume":
				goal.Phase = "active"
			case "complete":
				goal.Phase = "complete"
			case "blocked":
				goal.Phase = "blocked"
				copy := *reason
				copy.Message = strings.TrimSpace(copy.Message)
				goal.BlockedReason = &copy
			}
			return nil
		}, now)
	if err != nil {
		return conversationView{}, err
	}
	if err := d.emitAIConversationEvent(event); err != nil {
		return conversationView{}, err
	}
	if action == "resume" {
		d.state.armAIGoal(conversationID, goalID)
		d.scheduleAIGoal(projectID, conversationID)
	} else {
		d.state.disarmAIGoal(conversationID, goalID)
	}
	return d.withAIGoalActivation(value), nil
}

func (d dispatcher) clearAIGoal(ctx context.Context, projectID uuid.UUID, conversationID, goalID string, revision uint64, source string) (conversationView, error) {
	now := d.now().UTC()
	value, event, err := d.state.business.updateAIConversationCollaboration(ctx, projectID, conversationID, "", "",
		"chat.goal.changed", map[string]any{"operation": "clear", "source": source},
		func(collaboration *aiConversationCollaboration) error {
			if _, err := requireCurrentAIGoal(collaboration, goalID, revision); err != nil {
				return err
			}
			collaboration.Goal = nil
			return nil
		}, now)
	if err != nil {
		return conversationView{}, err
	}
	if err := d.emitAIConversationEvent(event); err != nil {
		return conversationView{}, err
	}
	d.state.disarmAIGoal(conversationID, goalID)
	return d.withAIGoalActivation(value), nil
}

func (d dispatcher) createAIGoalRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.goal"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "objective", "maxGoalRounds", "enableWorkspaceTools") {
		return nil, 0, errRPCInvalid
	}
	workspaceToolsEnabled, toolsErr := d.aiWorkspaceToolsIntent(input)
	if toolsErr != nil {
		return nil, 0, toolsErr
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	objective, objectiveOK := inputString(input, "objective", maximumAIGoalObjectiveBytes)
	maximum, present, maximumOK := optionalUint64(input, "maxGoalRounds")
	if !present {
		maximum = defaultAIGoalRounds
	}
	if !conversationOK || !objectiveOK || !maximumOK || uuid.Validate(conversationID) != nil {
		return nil, 0, errRPCInvalid
	}
	value, err := d.createAIGoal(ctx, projectID, conversationID, objective, maximum, "", "", "user")
	if err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func (d dispatcher) editAIGoalRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.goal"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "goalId", "revision", "objective", "maxGoalRounds", "enableWorkspaceTools") {
		return nil, 0, errRPCInvalid
	}
	workspaceToolsEnabled, toolsErr := d.aiWorkspaceToolsIntent(input)
	if toolsErr != nil {
		return nil, 0, toolsErr
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	goalID, goalOK := inputString(input, "goalId", 80)
	revision, revisionPresent, revisionOK := optionalUint64(input, "revision")
	objectiveValue, objectiveOK := optionalInputString(input, "objective", maximumAIGoalObjectiveBytes)
	maximumValue, maximumPresent, maximumOK := optionalUint64(input, "maxGoalRounds")
	_, objectivePresent := input["objective"]
	var objective *string
	if objectivePresent {
		objective = &objectiveValue
	}
	var maximum *uint64
	if maximumPresent {
		maximum = &maximumValue
	}
	if !conversationOK || !goalOK || !revisionPresent || !revisionOK || !objectiveOK || !maximumOK ||
		uuid.Validate(conversationID) != nil || uuid.Validate(goalID) != nil {
		return nil, 0, errRPCInvalid
	}
	value, err := d.editAIGoal(ctx, projectID, conversationID, goalID, revision, objective, maximum, "", "", "user")
	if err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func (d dispatcher) transitionAIGoalRPC(ctx context.Context, projectID uuid.UUID, input rpcInput, action string) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.goal"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "goalId", "revision", "enableWorkspaceTools") {
		return nil, 0, errRPCInvalid
	}
	workspaceToolsEnabled, toolsErr := d.aiWorkspaceToolsIntent(input)
	if toolsErr != nil {
		return nil, 0, toolsErr
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	goalID, goalOK := inputString(input, "goalId", 80)
	revision, revisionPresent, revisionOK := optionalUint64(input, "revision")
	if !conversationOK || !goalOK || !revisionPresent || !revisionOK || uuid.Validate(conversationID) != nil || uuid.Validate(goalID) != nil {
		return nil, 0, errRPCInvalid
	}
	value, err := d.transitionAIGoal(ctx, projectID, conversationID, goalID, revision, action, nil, "", "", "user")
	if err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func (d dispatcher) clearAIGoalRPC(ctx context.Context, projectID uuid.UUID, input rpcInput) (any, uint64, error) {
	if !agentFeatureFlags(d.state)["ai.goal"] {
		return nil, 0, errRPCCapability
	}
	if !onlyInputFields(input, "conversationId", "goalId", "revision", "enableWorkspaceTools") {
		return nil, 0, errRPCInvalid
	}
	workspaceToolsEnabled, toolsErr := d.aiWorkspaceToolsIntent(input)
	if toolsErr != nil {
		return nil, 0, toolsErr
	}
	d.aiWorkspaceToolsEnabled = workspaceToolsEnabled
	conversationID, conversationOK := inputString(input, "conversationId", 80)
	goalID, goalOK := inputString(input, "goalId", 80)
	revision, revisionPresent, revisionOK := optionalUint64(input, "revision")
	if !conversationOK || !goalOK || !revisionPresent || !revisionOK || uuid.Validate(conversationID) != nil || uuid.Validate(goalID) != nil {
		return nil, 0, errRPCInvalid
	}
	value, err := d.clearAIGoal(ctx, projectID, conversationID, goalID, revision, "user")
	if err != nil {
		return nil, 0, err
	}
	return value, value.Revision, nil
}

func renderAIGoalRoundPrompt(goal *aiGoalSnapshot, round uint64) string {
	objective, _ := json.Marshal(goal.Objective)
	return fmt.Sprintf(`<goal_round>
Objective: %s
Round: %d/%d

Continue working toward the objective in this same session. Treat the current workspace, tool results, and durable conversation state as authoritative; inspect them instead of assuming earlier narration is still current. Make concrete progress and verify the result. Before claiming completion, gather evidence that the whole objective is achieved, call get_goal, and mark it complete with update_goal. If work remains, leave the Goal active for the next round. Report blocked only after the same condition persists for at least three consecutive Goal rounds.
</goal_round>`, objective, round, goal.MaxGoalRounds)
}

func (d dispatcher) admitNextAIGoalRound(ctx context.Context, projectID uuid.UUID, conversationID string) (conversationView, *aiGoalRoundSource, string, error) {
	current, err := d.state.business.getAIConversation(ctx, projectID, conversationID)
	if err != nil {
		return conversationView{}, nil, "", err
	}
	goal := current.Goal
	if goal == nil || goal.Phase != "active" || current.Subagent != nil || !d.state.isAIGoalArmed(conversationID, goal.ID) {
		return current, nil, "", nil
	}
	if goal.RoundsStarted >= goal.MaxGoalRounds {
		blocked, blockErr := d.transitionAIGoal(ctx, projectID, conversationID, goal.ID, goal.Revision, "blocked",
			&aiGoalBlockReason{Code: "round-limit", Message: fmt.Sprintf("Goal reached its configured limit of %d rounds.", goal.MaxGoalRounds)}, "", "", "scheduler")
		return blocked, nil, "", blockErr
	}
	nextRound := goal.RoundsStarted + 1
	source := &aiGoalRoundSource{GoalID: goal.ID, Revision: goal.Revision, Round: nextRound}
	now := d.now().UTC()
	value, event, err := d.state.business.updateAIConversationCollaboration(ctx, projectID, conversationID, "", "",
		"chat.goal.changed", map[string]any{"operation": "round", "source": "scheduler", "goalRound": source},
		func(collaboration *aiConversationCollaboration) error {
			admitted, goalErr := requireCurrentAIGoal(collaboration, goal.ID, goal.Revision)
			if goalErr != nil || admitted.Phase != "active" || admitted.RoundsStarted+1 != nextRound {
				return firstError(goalErr, errRPCRevision)
			}
			admitted.RoundsStarted = nextRound
			admitted.UpdatedAt = nextAIGoalUpdatedAt(admitted, now)
			return nil
		}, now)
	if err != nil {
		return conversationView{}, nil, "", err
	}
	if err := d.emitAIConversationEvent(event); err != nil {
		return conversationView{}, nil, "", err
	}
	return value, source, renderAIGoalRoundPrompt(value.Goal, nextRound), nil
}

func aiGoalToolValue(goal *aiGoalSnapshot, armed bool) map[string]any {
	if goal == nil {
		return map[string]any{"goal": nil}
	}
	value := map[string]any{
		"id":            goal.ID,
		"revision":      goal.Revision,
		"objective":     goal.Objective,
		"phase":         goal.Phase,
		"roundsStarted": goal.RoundsStarted,
		"maxGoalRounds": goal.MaxGoalRounds,
	}
	if goal.BlockedReason != nil {
		value["blockedReason"] = map[string]any{
			"code":    goal.BlockedReason.Code,
			"message": goal.BlockedReason.Message,
		}
	}
	activation := "disarmed"
	if armed && goal.Phase == "active" {
		activation = "armed"
	}
	return map[string]any{"goal": value, "activation": activation}
}
