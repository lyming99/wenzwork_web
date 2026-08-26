package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

type aiToolPreExecuteDecisionKind string

const (
	aiToolPreExecuteAllow aiToolPreExecuteDecisionKind = "allow"
	aiToolPreExecuteDeny  aiToolPreExecuteDecisionKind = "deny"
	aiToolPreExecuteAsk   aiToolPreExecuteDecisionKind = "ask"
)

type aiToolPreExecuteDecision struct {
	Kind   aiToolPreExecuteDecisionKind
	Reason string
}

type aiToolPreExecuteRequest struct {
	CallID               string
	ToolName             string
	ArgumentsJSON        string
	ArgumentsSHA256      string
	ConversationID       string
	GenerationID         string
	WorkspaceRoot        string
	WorkspaceMode        string
	Preview              aiWorkspaceApprovalPreview
	PlannedApprovalCount int
}

func (request aiToolPreExecuteRequest) requiresPlannedApproval() bool {
	return request.PlannedApprovalCount > 0
}

type aiToolPreExecuteNext func() (aiToolPreExecuteDecision, error)

type aiToolPreExecuteGate func(
	context.Context,
	aiToolPreExecuteRequest,
	aiToolPreExecuteNext,
) (aiToolPreExecuteDecision, error)

type registeredAIToolPreExecuteGate struct {
	id   uint64
	gate aiToolPreExecuteGate
}

// aiToolPreExecuteWaterfall is the device-side policy interception point. A
// gate claims a call by returning without invoking next; invoking next delegates
// to the immutable dispatch snapshot. The default, after every gate delegates,
// is allow. Gate failures and invalid decisions are normalized to deny.
type aiToolPreExecuteWaterfall struct {
	mu     sync.RWMutex
	nextID uint64
	gates  []registeredAIToolPreExecuteGate
}

func (waterfall *aiToolPreExecuteWaterfall) register(gate aiToolPreExecuteGate, prepend bool) func() {
	if waterfall == nil || gate == nil {
		return func() {}
	}
	waterfall.mu.Lock()
	waterfall.nextID++
	registration := registeredAIToolPreExecuteGate{id: waterfall.nextID, gate: gate}
	if prepend {
		waterfall.gates = append([]registeredAIToolPreExecuteGate{registration}, waterfall.gates...)
	} else {
		waterfall.gates = append(waterfall.gates, registration)
	}
	waterfall.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			waterfall.mu.Lock()
			defer waterfall.mu.Unlock()
			for index, candidate := range waterfall.gates {
				if candidate.id == registration.id {
					waterfall.gates = append(waterfall.gates[:index], waterfall.gates[index+1:]...)
					break
				}
			}
		})
	}
}

func (waterfall *aiToolPreExecuteWaterfall) decide(ctx context.Context, request aiToolPreExecuteRequest) aiToolPreExecuteDecision {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteDeny, Reason: "工具执行已取消。"}
	}
	if waterfall == nil || !validAIToolPreExecuteRequest(request) {
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteDeny, Reason: "执行前策略不可用，工具已拒绝执行。"}
	}
	waterfall.mu.RLock()
	snapshot := append([]registeredAIToolPreExecuteGate(nil), waterfall.gates...)
	waterfall.mu.RUnlock()

	decision, err := dispatchAIToolPreExecute(ctx, cloneAIToolPreExecuteRequest(request), snapshot, 0)
	if err != nil || !validAIToolPreExecuteDecision(decision) {
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteDeny, Reason: "执行前策略不可用，工具已拒绝执行。"}
	}
	return decision
}

func dispatchAIToolPreExecute(
	ctx context.Context,
	request aiToolPreExecuteRequest,
	gates []registeredAIToolPreExecuteGate,
	index int,
) (aiToolPreExecuteDecision, error) {
	if err := ctx.Err(); err != nil {
		return aiToolPreExecuteDecision{}, err
	}
	if index >= len(gates) {
		return aiToolPreExecuteDecision{Kind: aiToolPreExecuteAllow}, nil
	}

	var nextMu sync.Mutex
	nextCalled, nextFinished, nextViolation, handlerFinished := false, false, false, false
	var downstream aiToolPreExecuteDecision
	var downstreamErr error
	next := func() (aiToolPreExecuteDecision, error) {
		nextMu.Lock()
		if handlerFinished || nextCalled {
			nextViolation = true
			nextMu.Unlock()
			return aiToolPreExecuteDecision{}, errors.New("AI pre-execute gate called next more than once or after returning")
		}
		nextCalled = true
		nextMu.Unlock()

		value, err := dispatchAIToolPreExecute(ctx, cloneAIToolPreExecuteRequest(request), gates, index+1)
		nextMu.Lock()
		downstream, downstreamErr, nextFinished = value, err, true
		nextMu.Unlock()
		return value, err
	}

	decision, err := invokeAIToolPreExecuteGate(gates[index].gate, ctx, cloneAIToolPreExecuteRequest(request), next)
	nextMu.Lock()
	handlerFinished = true
	called, finished, violated := nextCalled, nextFinished, nextViolation
	delegated, delegatedErr := downstream, downstreamErr
	nextMu.Unlock()
	if err != nil || violated || called && !finished {
		return aiToolPreExecuteDecision{}, errors.New("AI pre-execute gate failed closed")
	}
	if called {
		return delegated, delegatedErr
	}
	return decision, nil
}

func invokeAIToolPreExecuteGate(
	gate aiToolPreExecuteGate,
	ctx context.Context,
	request aiToolPreExecuteRequest,
	next aiToolPreExecuteNext,
) (decision aiToolPreExecuteDecision, err error) {
	defer func() {
		if recover() != nil {
			decision = aiToolPreExecuteDecision{}
			err = errors.New("AI pre-execute gate panicked")
		}
	}()
	return gate(ctx, request, next)
}

func validAIToolPreExecuteDecision(decision aiToolPreExecuteDecision) bool {
	if len(decision.Reason) > 2048 || !utf8.ValidString(decision.Reason) {
		return false
	}
	switch decision.Kind {
	case aiToolPreExecuteAllow:
		return decision.Reason == ""
	case aiToolPreExecuteDeny:
		return strings.TrimSpace(decision.Reason) != ""
	case aiToolPreExecuteAsk:
		return true
	default:
		return false
	}
}

func validAIToolPreExecuteRequest(request aiToolPreExecuteRequest) bool {
	return request.CallID != "" && len(request.CallID) <= 256 && utf8.ValidString(request.CallID) &&
		validAIProviderToolName(request.ToolName) && json.Valid([]byte(request.ArgumentsJSON)) &&
		len(request.ArgumentsJSON) <= maximumRPCPayload && validAIWorkspaceDigest(request.ArgumentsSHA256) &&
		uuid.Validate(request.ConversationID) == nil && uuid.Validate(request.GenerationID) == nil && request.WorkspaceRoot != "" &&
		validAIWorkspaceMode(request.WorkspaceMode) && request.PlannedApprovalCount >= 0 && request.PlannedApprovalCount <= 1
}

func newAIToolPreExecuteRequest(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiToolPreExecuteRequest, error) {
	arguments, err := json.Marshal(plan.Call.Arguments)
	if err != nil || len(arguments) > maximumRPCPayload {
		return aiToolPreExecuteRequest{}, errRPCInvalid
	}
	plannedApprovalCount := 0
	if plan.RequiresApproval {
		plannedApprovalCount = 1
	}
	request := aiToolPreExecuteRequest{
		CallID: plan.Call.ID, ToolName: plan.Call.Name, ArgumentsJSON: string(arguments),
		ArgumentsSHA256: plan.Preview.ArgumentsSHA256, ConversationID: workspace.ConversationID,
		GenerationID: workspace.GenerationID, WorkspaceRoot: workspace.Project.LocalPath,
		WorkspaceMode: workspace.WorkspaceMode, Preview: cloneAIWorkspaceApprovalPreview(plan.Preview),
		PlannedApprovalCount: plannedApprovalCount,
	}
	if !validAIToolPreExecuteRequest(request) {
		return aiToolPreExecuteRequest{}, errRPCInvalid
	}
	return request, nil
}

func cloneAIToolPreExecuteRequest(request aiToolPreExecuteRequest) aiToolPreExecuteRequest {
	request.Preview = cloneAIWorkspaceApprovalPreview(request.Preview)
	return request
}

func cloneAIWorkspaceApprovalPreview(preview aiWorkspaceApprovalPreview) aiWorkspaceApprovalPreview {
	preview.RelativePaths = append([]string(nil), preview.RelativePaths...)
	preview.NetworkHosts = append([]string(nil), preview.NetworkHosts...)
	return preview
}

type aiApprovalNext func() (aiApprovalResolution, error)

type aiApprovalAnswerer func(context.Context, aiApprovalRequest, aiApprovalNext) (aiApprovalResolution, error)

type aiApprovalResponder func(context.Context, aiApprovalRequest) (aiApprovalResolution, error)

type registeredAIApprovalAnswerer struct {
	id       uint64
	answerer aiApprovalAnswerer
}

// aiApprovalWaterfall is independent from the pre-execute chain. An `ask`
// decision is resolved here, with the remote client responder as the terminal
// fallback. No answerer and no responder is an explicit unavailable denial.
type aiApprovalWaterfall struct {
	mu        sync.RWMutex
	nextID    uint64
	answerers []registeredAIApprovalAnswerer
}

func (waterfall *aiApprovalWaterfall) register(answerer aiApprovalAnswerer, prepend bool) func() {
	if waterfall == nil || answerer == nil {
		return func() {}
	}
	waterfall.mu.Lock()
	waterfall.nextID++
	registration := registeredAIApprovalAnswerer{id: waterfall.nextID, answerer: answerer}
	if prepend {
		waterfall.answerers = append([]registeredAIApprovalAnswerer{registration}, waterfall.answerers...)
	} else {
		waterfall.answerers = append(waterfall.answerers, registration)
	}
	waterfall.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			waterfall.mu.Lock()
			defer waterfall.mu.Unlock()
			for index, candidate := range waterfall.answerers {
				if candidate.id == registration.id {
					waterfall.answerers = append(waterfall.answerers[:index], waterfall.answerers[index+1:]...)
					break
				}
			}
		})
	}
}

func (waterfall *aiApprovalWaterfall) resolve(
	ctx context.Context,
	request aiApprovalRequest,
	responder aiApprovalResponder,
) aiApprovalResolution {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return aiApprovalResolution{Decision: "cancelled"}
	}
	if waterfall == nil || !validAIApprovalRequest(request) {
		return aiApprovalResolution{Decision: "unavailable"}
	}
	waterfall.mu.RLock()
	snapshot := append([]registeredAIApprovalAnswerer(nil), waterfall.answerers...)
	waterfall.mu.RUnlock()

	resolution, err := dispatchAIApproval(ctx, cloneAIApprovalRequest(request), snapshot, responder, 0)
	if err != nil || !validAIApprovalResolution(request, resolution) {
		return aiApprovalResolution{Decision: "unavailable"}
	}
	return resolution
}

func dispatchAIApproval(
	ctx context.Context,
	request aiApprovalRequest,
	answerers []registeredAIApprovalAnswerer,
	responder aiApprovalResponder,
	index int,
) (aiApprovalResolution, error) {
	if err := ctx.Err(); err != nil {
		return aiApprovalResolution{Decision: "cancelled"}, nil
	}
	if index >= len(answerers) {
		if responder == nil {
			return aiApprovalResolution{Decision: "unavailable"}, nil
		}
		return invokeAIApprovalResponder(responder, ctx, cloneAIApprovalRequest(request))
	}

	var nextMu sync.Mutex
	nextCalled, nextFinished, nextViolation, handlerFinished := false, false, false, false
	var downstream aiApprovalResolution
	var downstreamErr error
	next := func() (aiApprovalResolution, error) {
		nextMu.Lock()
		if handlerFinished || nextCalled {
			nextViolation = true
			nextMu.Unlock()
			return aiApprovalResolution{}, errors.New("AI approval answerer called next more than once or after returning")
		}
		nextCalled = true
		nextMu.Unlock()

		value, err := dispatchAIApproval(ctx, cloneAIApprovalRequest(request), answerers, responder, index+1)
		nextMu.Lock()
		downstream, downstreamErr, nextFinished = value, err, true
		nextMu.Unlock()
		return value, err
	}

	resolution, err := invokeAIApprovalAnswerer(answerers[index].answerer, ctx, cloneAIApprovalRequest(request), next)
	nextMu.Lock()
	handlerFinished = true
	called, finished, violated := nextCalled, nextFinished, nextViolation
	delegated, delegatedErr := downstream, downstreamErr
	nextMu.Unlock()
	if err != nil || violated || called && !finished {
		return aiApprovalResolution{}, errors.New("AI approval answerer failed closed")
	}
	if called {
		return delegated, delegatedErr
	}
	return resolution, nil
}

func invokeAIApprovalAnswerer(
	answerer aiApprovalAnswerer,
	ctx context.Context,
	request aiApprovalRequest,
	next aiApprovalNext,
) (resolution aiApprovalResolution, err error) {
	defer func() {
		if recover() != nil {
			resolution = aiApprovalResolution{}
			err = errors.New("AI approval answerer panicked")
		}
	}()
	return answerer(ctx, request, next)
}

func invokeAIApprovalResponder(
	responder aiApprovalResponder,
	ctx context.Context,
	request aiApprovalRequest,
) (resolution aiApprovalResolution, err error) {
	defer func() {
		if recover() != nil {
			resolution = aiApprovalResolution{}
			err = errors.New("AI approval responder panicked")
		}
	}()
	return responder(ctx, request)
}

func validAIApprovalResolution(request aiApprovalRequest, resolution aiApprovalResolution) bool {
	switch resolution.Decision {
	case "deny":
		return !resolution.Approved && !resolution.AllowForSession
	case "allowOnce":
		return resolution.Approved && !resolution.AllowForSession
	case "allowForSession":
		return request.AllowForSession && resolution.Approved && resolution.AllowForSession
	case "timeout", "cancelled", "unavailable":
		return !resolution.Approved && !resolution.AllowForSession
	default:
		return false
	}
}

func cloneAIApprovalRequest(request aiApprovalRequest) aiApprovalRequest {
	request.Preview = cloneAIWorkspaceApprovalPreview(request.Preview)
	return request
}

type aiToolPolicyRuntime struct {
	preExecute aiToolPreExecuteWaterfall
	approval   aiApprovalWaterfall
}

func newAIToolPolicyRuntime() *aiToolPolicyRuntime {
	return &aiToolPolicyRuntime{}
}
