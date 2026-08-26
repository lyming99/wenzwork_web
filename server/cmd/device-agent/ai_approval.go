package main

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const defaultAIApprovalTimeout = 2 * time.Minute

type aiApprovalRequest struct {
	ID              string                     `json:"id"`
	ConversationID  string                     `json:"conversationId"`
	GenerationID    string                     `json:"generationId"`
	MessageID       string                     `json:"messageId"`
	ToolCallID      string                     `json:"toolCallId"`
	ToolName        string                     `json:"toolName"`
	Preview         aiWorkspaceApprovalPreview `json:"preview"`
	ExpiresAt       time.Time                  `json:"expiresAt"`
	AllowForSession bool                       `json:"allowForSession"`
	Reason          string                     `json:"reason,omitempty"`
}

type aiApprovalResolution struct {
	Decision        string
	Approved        bool
	AllowForSession bool
}

type pendingAIApproval struct {
	ProjectID   uuid.UUID
	Request     aiApprovalRequest
	ScopeSHA256 string
	decision    chan aiApprovalResolution
}

func (state *agentState) registerAIApproval(projectID uuid.UUID, request aiApprovalRequest, scopeSHA256 string) (*pendingAIApproval, error) {
	if state == nil || projectID == uuid.Nil || !validAIApprovalRequest(request) || !validAIWorkspaceDigest(scopeSHA256) {
		return nil, errRPCInvalid
	}
	pending := &pendingAIApproval{
		ProjectID: projectID, Request: request, ScopeSHA256: scopeSHA256, decision: make(chan aiApprovalResolution, 1),
	}
	state.aiApprovalMu.Lock()
	defer state.aiApprovalMu.Unlock()
	if state.aiApprovals == nil {
		state.aiApprovals = make(map[string]*pendingAIApproval)
	}
	if _, found := state.aiApprovals[request.ID]; found {
		return nil, errRPCRevision
	}
	state.aiApprovals[request.ID] = pending
	return pending, nil
}

func (state *agentState) waitAIApproval(ctx context.Context, pending *pendingAIApproval) aiApprovalResolution {
	if state == nil || pending == nil {
		return aiApprovalResolution{Decision: "cancelled"}
	}
	duration := time.Until(pending.Request.ExpiresAt)
	if duration < 0 {
		duration = 0
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case decision := <-pending.decision:
		if decision.AllowForSession {
			state.addAISessionGrant(pending.Request.GenerationID, pending.ScopeSHA256)
		}
		return decision
	case <-timer.C:
		state.removeAIApproval(pending.Request.ID, pending)
		return aiApprovalResolution{Decision: "timeout"}
	case <-ctx.Done():
		state.removeAIApproval(pending.Request.ID, pending)
		return aiApprovalResolution{Decision: "cancelled"}
	}
}

func (state *agentState) resolveAIApproval(projectID uuid.UUID, approvalID, conversationID, generationID, toolCallID, decision string) (aiApprovalResolution, error) {
	if state == nil || projectID == uuid.Nil || uuid.Validate(approvalID) != nil || uuid.Validate(conversationID) != nil ||
		uuid.Validate(generationID) != nil || toolCallID == "" || len(toolCallID) > 256 ||
		decision != "deny" && decision != "allowOnce" && decision != "allowForSession" {
		return aiApprovalResolution{}, errRPCInvalid
	}
	state.aiApprovalMu.Lock()
	pending := state.aiApprovals[approvalID]
	if pending == nil {
		state.aiApprovalMu.Unlock()
		return aiApprovalResolution{}, errRPCNotFound
	}
	request := pending.Request
	if pending.ProjectID != projectID || request.ConversationID != conversationID || request.GenerationID != generationID || request.ToolCallID != toolCallID {
		state.aiApprovalMu.Unlock()
		return aiApprovalResolution{}, errRPCRevision
	}
	if decision == "allowForSession" && !request.AllowForSession {
		state.aiApprovalMu.Unlock()
		return aiApprovalResolution{}, errRPCForbidden
	}
	delete(state.aiApprovals, approvalID)
	state.aiApprovalMu.Unlock()
	resolution := aiApprovalResolution{
		Decision: decision, Approved: decision == "allowOnce" || decision == "allowForSession", AllowForSession: decision == "allowForSession",
	}
	pending.decision <- resolution
	return resolution, nil
}

func (state *agentState) removeAIApproval(id string, expected *pendingAIApproval) {
	if state == nil {
		return
	}
	state.aiApprovalMu.Lock()
	if state.aiApprovals[id] == expected {
		delete(state.aiApprovals, id)
	}
	state.aiApprovalMu.Unlock()
}

// pendingAIApprovalForGeneration is a read-only recovery projection. Approval
// decisions remain in memory and are never reconstructed from an event log;
// a reconnecting client must receive the current, still-actionable request
// from the Agent before it can submit a decision.
func (state *agentState) pendingAIApprovalForGeneration(projectID uuid.UUID, conversationID, generationID string, now time.Time) *aiApprovalRequest {
	if state == nil || projectID == uuid.Nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil || now.IsZero() {
		return nil
	}
	state.aiApprovalMu.Lock()
	defer state.aiApprovalMu.Unlock()
	for _, pending := range state.aiApprovals {
		request := pending.Request
		if pending.ProjectID == projectID && request.ConversationID == conversationID && request.GenerationID == generationID && request.ExpiresAt.After(now) {
			return &request
		}
	}
	return nil
}

func (state *agentState) hasAISessionGrant(generationID, scopeSHA256 string) bool {
	if state == nil || uuid.Validate(generationID) != nil || !validAIWorkspaceDigest(scopeSHA256) {
		return false
	}
	state.aiApprovalMu.Lock()
	defer state.aiApprovalMu.Unlock()
	_, found := state.aiSessionGrants[generationID][scopeSHA256]
	return found
}

func (state *agentState) addAISessionGrant(generationID, scopeSHA256 string) {
	if state == nil || uuid.Validate(generationID) != nil || !validAIWorkspaceDigest(scopeSHA256) {
		return
	}
	state.aiApprovalMu.Lock()
	defer state.aiApprovalMu.Unlock()
	if state.aiSessionGrants == nil {
		state.aiSessionGrants = make(map[string]map[string]struct{})
	}
	if state.aiSessionGrants[generationID] == nil {
		state.aiSessionGrants[generationID] = make(map[string]struct{})
	}
	state.aiSessionGrants[generationID][scopeSHA256] = struct{}{}
}

func (state *agentState) clearAIGenerationApprovals(generationID string) {
	if state == nil {
		return
	}
	state.aiApprovalMu.Lock()
	delete(state.aiSessionGrants, generationID)
	for id, pending := range state.aiApprovals {
		if pending.Request.GenerationID == generationID {
			delete(state.aiApprovals, id)
			select {
			case pending.decision <- aiApprovalResolution{Decision: "cancelled"}:
			default:
			}
		}
	}
	state.aiApprovalMu.Unlock()
}

func validAIApprovalRequest(request aiApprovalRequest) bool {
	return uuid.Validate(request.ID) == nil && uuid.Validate(request.ConversationID) == nil && uuid.Validate(request.GenerationID) == nil &&
		uuid.Validate(request.MessageID) == nil && request.ToolCallID != "" && len(request.ToolCallID) <= 256 &&
		validAIProviderToolName(request.ToolName) && !request.ExpiresAt.IsZero() &&
		validAIWorkspaceDigest(request.Preview.ArgumentsSHA256) && len(request.Reason) <= 2048 && utf8.ValidString(request.Reason)
}

func (executor *aiWorkspaceToolExecutor) recordAwaitingApproval(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) error {
	if executor == nil || executor.state == nil || executor.state.business == nil || !plan.RequiresApproval {
		return errRPCInvalid
	}
	record, err := executor.auditRecord(workspace, plan)
	if err != nil {
		return err
	}
	record.Outcome = "awaiting_approval"
	return executor.state.business.recordAIToolAudit(context.Background(), record)
}

func approvalResolutionAuthorization(resolution aiApprovalResolution) aiWorkspaceToolAuthorization {
	decision := "deny"
	failureCode := ""
	switch resolution.Decision {
	case "allowOnce":
		decision = "allow_once"
	case "allowForSession":
		decision = "allow_for_session"
	case "timeout":
		decision = "timeout"
	case "cancelled":
		decision = "cancelled"
	case "unavailable":
		failureCode = "approval_unavailable"
	}
	return aiWorkspaceToolAuthorization{
		Approved: resolution.Approved, Decision: decision, AllowForSession: resolution.AllowForSession, FailureCode: failureCode,
	}
}
