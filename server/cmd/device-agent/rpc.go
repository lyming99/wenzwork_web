package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maximumPeerRPCPlaintext = 60 << 10
	maximumRPCPayload       = 56 << 10
	preferredRPCPagePayload = 48 << 10
)

var (
	errRPCInvalid                         = errors.New("RPC request is invalid")
	errRPCNotFound                        = errors.New("RPC resource was not found")
	errRPCForbidden                       = errors.New("RPC path is outside the configured workspace")
	errRPCRevision                        = errors.New("RPC resource revision conflicts")
	errRPCIdempotency                     = errors.New("RPC idempotency key conflicts")
	errRPCProject                         = errors.New("RPC project binding does not match the ticket")
	errRPCCapability                      = errors.New("RPC capability is unavailable")
	errRPCBusy                            = errors.New("RPC resource limit is reached")
	errRPCProjectHasActiveAIConversations = errors.New("PROJECT_HAS_AI_CONVERSATIONS")
	errRPCProjectHasActiveTasks           = errors.New("PROJECT_HAS_TASKS")
	errRPCAIConfigRequired                = errors.New("AI_CONFIG_REQUIRED")
	errRPCAIConfigNotFound                = errors.New("AI_CONFIG_NOT_FOUND")
	errRPCAIConfigStorageUnavailable      = errors.New("AI_CONFIG_STORAGE_UNAVAILABLE")
	errRPCAIToolsScopeRequired            = errors.New("AI_TOOLS_SCOPE_REQUIRED")
	// AI replay and generation failures are deliberately distinguished from the
	// transport-level RPC enum.  RpcError.SafeMessage is the stable semantic
	// code for v3 clients; older clients still receive a valid enum code.
	errRPCConversationGenerationActive = errors.New("CONVERSATION_GENERATION_ACTIVE")
	errRPCAgentGenerationCapacity      = errors.New("AGENT_GENERATION_CAPACITY")
	errRPCResponsePageTooLarge         = errors.New("RESPONSE_PAGE_TOO_LARGE")
	errRPCEventItemTooLarge            = errors.New("EVENT_ITEM_TOO_LARGE")
	errRPCEventCursorExpired           = errors.New("EVENT_CURSOR_EXPIRED")
	errRPCPeerOffline                  = errors.New("PEER_OFFLINE")
	errRPCEventKindUnknown             = errors.New("rpc_event_kind_unknown")
	errTaskLogExpired                  = errors.New("LOG_EXPIRED")
	errTaskLogMigrating                = errors.New("LOG_MIGRATING")
	errTaskLogCorrupt                  = errors.New("LOG_CORRUPT")
	errTaskLogUpgradeRequired          = errors.New("UPGRADE_REQUIRED")
)

var aiConversationRPCEventKinds = map[string]remotev1.RpcEventKind{
	"chat.text.delta":         remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_DELTA,
	"chat.completed":          remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_COMPLETED,
	"chat.failed":             remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_FAILED,
	"chat.reasoning.delta":    remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_REASONING_DELTA,
	"chat.tool.status":        remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_TOOL_STATUS,
	"chat.approval.requested": remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_APPROVAL_REQUESTED,
	"chat.usage":              remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_USAGE,
	"chat.cancelled":          remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_CANCELLED,
	"chat.goal.changed":       remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_GOAL_CHANGED,
	"chat.plan_mode.changed":  remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_PLAN_MODE_CHANGED,
	"chat.todo.updated":       remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_TODO_UPDATED,
	"chat.subagent.started":   remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_SUBAGENT_STARTED,
	"chat.subagent.status":    remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_SUBAGENT_STATUS,
	"chat.subagent.message":   remotev1.RpcEventKind_RPC_EVENT_KIND_CHAT_SUBAGENT_MESSAGE,
}

type dispatcher struct {
	state         *agentState
	controlStore  *controlStateStore
	controlLoop   *deviceControlLoop
	tasks         TaskRepository
	now           func() time.Time
	scope         string
	ai            aiProvider
	chatEvent     func(aiConversationEvent) error
	eventContract rpcEventNegotiation
	// aiToolTimeout overrides per-tool execution budgets for tests; nil uses
	// aiToolExecutionBudget.
	aiToolTimeout func(string) time.Duration
	// aiWorkspaceToolsEnabled carries explicit, already-authorized turn intent
	// into autonomous inbox, Goal, and child-Agent continuations.
	aiWorkspaceToolsEnabled bool
	// Peer sessions set these from signed ticket claims. Direct stdio calls
	// intentionally leave enforcement disabled for backwards-compatible local
	// maintenance and test tooling.
	ticketProjectID       string
	requestProjectID      string
	enforceProjectBinding bool
	// peerSessionID binds opaque prepared sources to one E2EE Link. It is never
	// serialized and survives only Carrier replacement for that Link.
	peerSessionID string
}

func (d dispatcher) dispatchLive(ctx context.Context, envelope *remotev1.RpcEnvelope, emit func(*remotev1.RpcEnvelope) error) *remotev1.RpcEnvelope {
	if envelope == nil || envelope.GetRequest() == nil || envelope.GetRequest().GetHeader() == nil || emit == nil {
		return d.dispatch(ctx, envelope)
	}
	request := envelope.GetRequest()
	if request.GetMethod() == "event.subscribe" {
		return d.streamAgentEvents(ctx, envelope, emit)
	}
	if request.GetMethod() == "conversation.generation.attach" {
		return d.streamAIConversationGeneration(ctx, envelope, emit)
	}
	requestID := request.GetHeader().GetRequestId()
	if negotiation, err := rpcEventNegotiationForRequest(request); err == nil {
		d.eventContract = negotiation
	}
	liveAvailable := true
	d.chatEvent = func(event aiConversationEvent) error {
		if !d.eventContract.allows(event.Kind) {
			return nil
		}
		value, err := rpcEnvelopeForAIConversationEvent(event, requestID)
		if err != nil {
			d.recordRPCProtocolDiagnostic("rpcEnvelope", "rpc_event_kind_unknown", "channel", "outbound", request, len(event.Kind))
			return err
		}
		if !liveAvailable {
			return nil
		}
		if err := emit(value); err != nil {
			// The event is committed to the conversation store before this
			// callback runs. A lost Peer route therefore only disables best-
			// effort live delivery; it must never turn a healthy provider run
			// into a failed or cancelled device-side generation. The controller
			// replays these durable events after opening a fresh session.
			liveAvailable = false
		}
		return nil
	}
	response := d.dispatch(ctx, envelope)
	if response.GetResponse().GetError() == nil && request.GetMethod() == "terminal.attach" {
		terminalDispatcher := d
		terminalDispatcher.requestProjectID = strings.TrimSpace(request.GetHeader().GetProjectId())
		completion, err := terminalDispatcher.streamInteractiveTerminalAttach(ctx, envelope, emit)
		if err != nil {
			switch {
			case errors.Is(err, context.Canceled):
				setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED, "request cancelled", true)
			case errors.Is(err, context.DeadlineExceeded):
				setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_DEADLINE_EXCEEDED, "request deadline exceeded", true)
			case errors.Is(err, errRPCBusy):
				setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "terminal attach already active", true)
			default:
				setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "terminal stream interrupted", true)
			}
		} else {
			// Refresh the attach snapshot after emitting. Output may arrive while
			// the long-poll is open, so the final high watermark must never lag
			// behind events the client has already acknowledged.
			response = terminalDispatcher.dispatch(ctx, envelope)
			if response.GetResponse().GetError() == nil {
				var payload map[string]any
				if json.Unmarshal(response.GetResponse().GetJsonPayload(), &payload) != nil {
					setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "terminal response unavailable", true)
				} else {
					payload["completionReason"] = completion.Reason
					payload["heldMilliseconds"] = completion.Held.Milliseconds()
					payload["eventCount"] = completion.EventCount
					if completion.HighWatermark > 0 {
						payload["highWatermark"] = completion.HighWatermark
					}
					encoded, encodeErr := json.Marshal(payload)
					if encodeErr != nil || len(encoded) > maximumRPCPayloadForMethod("terminal.attach") {
						setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "terminal response unavailable", true)
					} else {
						response.GetResponse().JsonPayload = encoded
					}
				}
			}
		}
	}
	return response
}

type rpcInput map[string]any

func (d dispatcher) dispatch(ctx context.Context, envelope *remotev1.RpcEnvelope) *remotev1.RpcEnvelope {
	metricStartedAt := time.Now()
	var request *remotev1.RpcRequest
	if envelope != nil {
		request = envelope.GetRequest()
	}
	requestID := ""
	if request != nil && request.GetHeader() != nil {
		requestID = request.GetHeader().GetRequestId()
	}
	response := &remotev1.RpcResponse{Header: &remotev1.RpcResponseHeader{RequestId: requestID}}
	result := &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Response{Response: response}}
	compatibilityVersion := ""
	defer func() {
		d.recordCompatibilityMetricBestEffort(
			compatibilityVersion,
			compatibilityMetricErrorCode(response),
			time.Since(metricStartedAt),
		)
	}()
	if envelope == nil || request == nil || request.GetHeader() == nil {
		d.recordRPCProtocolDiagnostic("rpcEnvelope", "rpc_envelope_invalid", "channel", "inbound", request, 0)
		setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid request", false)
		return result
	}
	if envelope.GetProtocolVersion() != 1 {
		d.recordRPCProtocolDiagnostic("rpcEnvelope", "rpc_protocol_version_invalid", "session", "inbound", request, 0)
		setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid request", false)
		return result
	}
	if uuid.Validate(requestID) != nil || !validMethod(request.GetMethod()) ||
		(request.GetHeader().GetDeadline() != nil && !request.GetHeader().GetDeadline().AsTime().After(d.now())) {
		d.recordRPCProtocolDiagnostic("rpcModel", "rpc_model_invalid", "operation", "inbound", request, len(request.GetJsonPayload()))
		setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid request", false)
		return result
	}
	compatibilityVersion = compatibilityCapabilityVersion(d, request.GetMethod(), request.GetHeader().GetProjectId())
	if !rpcPayloadWithinLimit(request.GetMethod(), request.GetJsonPayload()) {
		d.recordRPCProtocolDiagnostic("rpcJson", "rpc_json_too_large", "operation", "inbound", request, len(request.GetJsonPayload()))
		setRPCPayloadTooLarge(response, len(request.GetJsonPayload()), maximumRPCPayloadForMethod(request.GetMethod()), request.GetMethod())
		return result
	}
	if !methodAllowsScope(request.GetMethod(), d.scope) {
		setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN, "method is outside the ticket scope", false)
		return result
	}
	var input rpcInput
	if len(request.GetJsonPayload()) == 0 {
		input = rpcInput{}
	} else if !utf8.Valid(request.GetJsonPayload()) {
		d.recordRPCProtocolDiagnostic("rpcJson", "rpc_json_invalid_utf8", "operation", "inbound", request, len(request.GetJsonPayload()))
		setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid JSON input", false)
		return result
	} else if err := json.Unmarshal(request.GetJsonPayload(), &input); err != nil {
		d.recordRPCProtocolDiagnostic("rpcJson", "rpc_json_invalid", "operation", "inbound", request, len(request.GetJsonPayload()))
		setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid JSON input", false)
		return result
	}
	requestProjectID := strings.TrimSpace(request.GetHeader().GetProjectId())
	callDispatcher := d
	callDispatcher.requestProjectID = requestProjectID
	if methodNegotiatesRPCEvents(request.GetMethod()) {
		negotiation, negotiationErr := parseRPCEventNegotiation(input)
		if negotiationErr != nil {
			d.recordRPCProtocolDiagnostic("rpcModel", "rpc_model_invalid", "operation", "inbound", request, len(request.GetJsonPayload()))
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid event negotiation", false)
			return result
		}
		callDispatcher.eventContract = negotiation
	}
	if err := callDispatcher.validateProjectBinding(request.GetMethod(), requestProjectID); err != nil {
		setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH, "project is not authorized for this session", false)
		return result
	}
	taskPayloadTransferID := ""
	if value, ok := input["payloadTransferId"].(string); ok {
		taskPayloadTransferID = strings.TrimSpace(value)
	}
	if !strings.HasPrefix(request.GetMethod(), "task.payload.") &&
		(strings.HasPrefix(request.GetMethod(), "task.") || strings.HasPrefix(request.GetMethod(), "workflow.")) {
		var resolveErr error
		input, resolveErr = callDispatcher.resolveTaskPayloadInput(request.GetMethod(), input)
		if resolveErr != nil {
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid task payload reference", false)
			return result
		}
	}
	output, revision, err := callDispatcher.call(ctx, request.GetMethod(), input)
	if err != nil {
		switch {
		case errors.Is(err, errRPCInvalid):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid input", false)
		case errors.Is(err, errRPCAIConfigRequired):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND, "AI_CONFIG_REQUIRED", false)
		case errors.Is(err, errRPCAIConfigNotFound):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND, "AI_CONFIG_NOT_FOUND", false)
		case errors.Is(err, errRPCAIConfigStorageUnavailable):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "AI_CONFIG_STORAGE_UNAVAILABLE", false)
		case errors.Is(err, errRPCAIToolsScopeRequired):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE, "AI_TOOLS_SCOPE_REQUIRED", false)
		case errors.Is(err, errRPCNotFound):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND, "resource not found", false)
		case errors.Is(err, errRPCForbidden):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN, "path is not allowed", false)
		case errors.Is(err, errRPCRevision):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT, "resource revision changed", false)
		case errors.Is(err, errRPCIdempotency):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_IDEMPOTENCY_CONFLICT, "idempotency key conflicts", false)
		case errors.Is(err, errRPCProject):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH, "project is not authorized for this session", false)
		case errors.Is(err, errRPCCapability):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE, "CAPABILITY_UNSUPPORTED", false)
		case errors.Is(err, errRPCConversationGenerationActive):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "CONVERSATION_GENERATION_ACTIVE", false)
		case errors.Is(err, errRPCProjectHasActiveAIConversations):
			// Keep the v3 wire codes stable; their narrowed meaning is documented
			// by the project.remove contract and the client-facing wording.
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "PROJECT_HAS_AI_CONVERSATIONS", false)
		case errors.Is(err, errRPCProjectHasActiveTasks):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "PROJECT_HAS_TASKS", false)
		case errors.Is(err, errRPCAgentGenerationCapacity):
			setRPCErrorWithRetryAfter(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "AGENT_GENERATION_CAPACITY", true, 2)
		case errors.Is(err, errRPCResponsePageTooLarge):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "RESPONSE_PAGE_TOO_LARGE", true)
		case errors.Is(err, errRPCEventItemTooLarge):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "EVENT_ITEM_TOO_LARGE", false)
		case errors.Is(err, errRPCEventCursorExpired):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT, "EVENT_CURSOR_EXPIRED", false)
		case errors.Is(err, errRPCPeerOffline):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "PEER_OFFLINE", true)
		case errors.Is(err, errTaskLogExpired):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND, "LOG_EXPIRED", false)
		case errors.Is(err, errTaskLogMigrating):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "LOG_MIGRATING", true)
		case errors.Is(err, errTaskLogCorrupt):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "LOG_CORRUPT", false)
		case errors.Is(err, errTaskLogUpgradeRequired):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_CAPABILITY_UNAVAILABLE, "UPGRADE_REQUIRED", false)
		case errors.Is(err, errRPCBusy):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "device resource limit reached", false)
		case errors.Is(err, errAIProviderStreamTruncated):
			// Partial output is durable, so automatically replaying the whole
			// request could duplicate visible text or tool side effects.
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "PROVIDER_STREAM_TRUNCATED", false)
		case errors.Is(err, errAIProviderRequestTimeout):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_DEADLINE_EXCEEDED, "PROVIDER_TIMEOUT", true)
		case errors.Is(err, context.DeadlineExceeded):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_DEADLINE_EXCEEDED, "request deadline exceeded", true)
		case errors.Is(err, context.Canceled):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED, "request cancelled", true)
		case errors.Is(err, errAIProvider):
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY, "AI provider is unavailable", true)
		default:
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "device operation failed", true)
		}
		return result
	}
	payload, err := json.Marshal(output)
	if err == nil && len(payload) > maximumRPCPayload && taskPayloadTransferID != "" {
		if compact, compactOK := compactTaskPayloadResult(request.GetMethod(), output, taskPayloadTransferID); compactOK {
			payload, err = json.Marshal(compact)
		}
	}
	if err != nil || len(payload) > maximumRPCPayloadForMethod(request.GetMethod()) {
		if err != nil {
			d.recordRPCProtocolDiagnostic("rpcModel", "rpc_model_invalid", "operation", "outbound", request, 0)
			setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "device response is unavailable", true)
		} else {
			d.recordRPCProtocolDiagnostic("rpcJson", "rpc_json_too_large", "operation", "outbound", request, len(payload))
			setRPCPayloadTooLarge(response, len(payload), maximumRPCPayloadForMethod(request.GetMethod()), request.GetMethod())
		}
		return result
	}
	response.Header.Revision = revision
	response.JsonPayload = payload
	return result
}

func (d dispatcher) validateProjectBinding(method, projectID string) error {
	if !d.enforceProjectBinding {
		return nil
	}
	projectScoped := method == "event.subscribe" || strings.HasPrefix(method, "terminal.") || strings.HasPrefix(method, "file.") ||
		strings.HasPrefix(method, "conversation.") || strings.HasPrefix(method, "chat.") || strings.HasPrefix(method, "task.") ||
		strings.HasPrefix(method, "workflow.")
	ticketProjectID := strings.TrimSpace(d.ticketProjectID)
	if !projectScoped {
		// Capabilities are a safe, device-local projection. A project ticket may
		// query them with its exact bound project ID, keeping capability
		// negotiation on the same encrypted session.
		if method == "agent.capabilities.get" && ticketProjectID != "" &&
			projectID == ticketProjectID && uuid.Validate(projectID) == nil {
			return nil
		}
		if projectID != "" || ticketProjectID != "" {
			return errRPCProject
		}
		return nil
	}
	// A ticket without a project is a pre-v2 session. Keep that legacy route
	// alive for old clients, but never let such a ticket smuggle a v2 header.
	// Fresh control-plane issuers always require a project for these methods.
	if ticketProjectID == "" {
		if projectID != "" {
			return errRPCProject
		}
		return nil
	}
	if projectID == "" || ticketProjectID == "" || projectID != ticketProjectID || uuid.Validate(projectID) != nil {
		return errRPCProject
	}
	return nil
}

// dispatchStream returns the final RPC response plus any request-bound events
// that must be delivered before that response. Keeping the event construction
// next to the dispatcher makes the same persisted messages visible to both the
// live stream and the later paginated history query.
func (d dispatcher) dispatchStream(ctx context.Context, envelope *remotev1.RpcEnvelope) (*remotev1.RpcEnvelope, []*remotev1.RpcEnvelope) {
	events := make([]*remotev1.RpcEnvelope, 0)
	requestID := ""
	var request *remotev1.RpcRequest
	if envelope != nil {
		request = envelope.GetRequest()
	}
	if request != nil && request.GetHeader() != nil {
		requestID = request.GetHeader().GetRequestId()
		if negotiation, err := rpcEventNegotiationForRequest(request); err == nil {
			d.eventContract = negotiation
		}
	}
	d.chatEvent = func(event aiConversationEvent) error {
		if !d.eventContract.allows(event.Kind) {
			return nil
		}
		value, err := rpcEnvelopeForAIConversationEvent(event, requestID)
		if err != nil {
			d.recordRPCProtocolDiagnostic("rpcEnvelope", "rpc_event_kind_unknown", "channel", "outbound", request, len(event.Kind))
		}
		if err == nil {
			events = append(events, value)
		}
		return err
	}
	response := d.dispatch(ctx, envelope)
	if request == nil || request.GetMethod() != "conversation.send" || response.GetResponse().GetError() != nil {
		return response, events
	}
	return response, events
}

func (d dispatcher) recordRPCProtocolDiagnostic(stage, reason, faultLevel, direction string, request *remotev1.RpcRequest, payloadBytes int) {
	method, requestID := "", ""
	if request != nil {
		method = request.GetMethod()
		if request.GetHeader() != nil {
			requestID = request.GetHeader().GetRequestId()
		}
	}
	d.state.recordProtocolDiagnostic(stage, reason, faultLevel, direction, method, d.scope, payloadBytes, requestID, "")
}

func rpcEnvelopeForAIConversationEvent(event aiConversationEvent, requestID string) (*remotev1.RpcEnvelope, error) {
	kind, ok := aiConversationRPCEventKinds[event.Kind]
	if !ok {
		return nil, errRPCEventKindUnknown
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Event{Event: &remotev1.RpcEvent{
			EventId: event.EventID, Kind: kind, RequestId: requestID, Sequence: event.Sequence,
			HighWatermark: event.Sequence, OccurredAt: timestamppb.New(event.OccurredAt), JsonPayload: payload,
		}},
	}, nil
}

func methodScope(method string) string {
	switch {
	case method == "event.subscribe":
		return "remote.peer.events"
	case strings.HasPrefix(method, "ai.config."):
		return "remote.peer.ai.config"
	case strings.HasPrefix(method, "agent.environment."):
		// Device-level command variables share the existing owner-only Agent
		// configuration scope and never accept a project binding.
		return "remote.peer.ai.config"
	case method == "project.list", method == "project.refresh", method == "project.sync":
		// Project discovery and refresh are device-local operations. They travel
		// through the encrypted Peer channel instead of a control-plane projection.
		return "remote.peer.query"
	case method == "project.create", method == "project.directory.list", method == "project.remove":
		return "remote.peer.query"
	case strings.HasPrefix(method, "task.payload."):
		return "remote.peer.task.control"
	case method == "task.list" || method == "task.get" || method == "task.logs" || method == "task.logs.download.prepare":
		return "remote.task.read"
	case method == "task.create" || method == "task.cancel":
		return "remote.task.write"
	case method == "task.update" || method == "task.start" || method == "task.stop" || method == "task.retry" ||
		method == "task.delete" || method == "task.clear" || method == "task.queue.start" || method == "task.accept" ||
		method == "task.undo-acceptance" || method == "task.queue.stop" || method == "task.runs" || method == "task.follow-up" || strings.HasPrefix(method, "workflow."):
		return "remote.peer.task.control"
	case method == "agent.identity.get" || method == "agent.capabilities.get":
		return "remote.peer.query"
	case method == "conversation.approval.respond":
		return "remote.peer.ai.tools"
	case method == "conversation.question.answer":
		return "remote.peer.ai.tools"
	case strings.HasPrefix(method, "conversation.") || strings.HasPrefix(method, "chat."):
		return "remote.peer.ai.chat"
	case method == "terminal.execute":
		return "remote.peer.terminal"
	case strings.HasPrefix(method, "terminal."):
		return "remote.peer.terminal.interactive"
	case method == "file.list" || method == "file.stat" || method == "file.details" || method == "file.search" || method == "file.read-text" || strings.HasPrefix(method, "file.download."):
		return "remote.peer.file.receive"
	case method == "file.write-text" || method == "file.create-text" || method == "file.mkdir" || method == "file.rename" || method == "file.move" || method == "file.delete" || method == "file.delete.prepare" || strings.HasPrefix(method, "file.upload."):
		return "remote.peer.file.send"
	default:
		return ""
	}
}

func methodAllowsScope(method, scope string) bool {
	required := methodScope(method)
	if required == "" {
		return false
	}
	// Capability discovery is read-only and contains no workspace paths,
	// credentials, or project content. Let a project-scoped Peer session use
	// it, so clients do not need a second unbound query session before their
	// first project operation.
	if method == "agent.capabilities.get" {
		switch scope {
		case "remote.peer.query", "remote.peer.ai.config", "remote.peer.ai.chat", "remote.peer.ai.tools",
			"remote.peer.file.send", "remote.peer.file.receive", "remote.peer.terminal", "remote.peer.terminal.interactive",
			"remote.peer.task.control", "remote.peer.events":
			return true
		default:
			return false
		}
	}
	if scope == required {
		return true
	}
	if scope == "remote.peer.ai.tools" && required == "remote.peer.ai.chat" {
		return true
	}
	if scope != "remote.peer.task.control" {
		return false
	}
	switch method {
	case "task.list", "task.get", "task.logs", "task.logs.download.prepare", "task.create", "task.cancel":
		return true
	default:
		return false
	}
}

func maximumRPCPayloadForMethod(method string) int {
	if method == "event.subscribe" {
		return maximumAgentEventPayloadBytes
	}
	return maximumRPCPayload
}

func rpcPayloadWithinLimit(method string, payload []byte) bool {
	return len(payload) <= maximumRPCPayloadForMethod(method)
}

func setRPCPayloadTooLarge(response *remotev1.RpcResponse, actualBytes, limitBytes int, method string) {
	setRPCError(response, remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED, "RPC_PAYLOAD_TOO_LARGE", false)
	payload, _ := json.Marshal(map[string]any{
		"limitBytes":          limitBytes,
		"sizeBucket":          rpcPayloadSizeBucket(actualBytes),
		"paginationAvailable": methodSupportsRPCPagination(method),
	})
	response.JsonPayload = payload
}

func methodSupportsRPCPagination(method string) bool {
	return strings.HasPrefix(method, "conversation.") || strings.HasSuffix(method, ".list") ||
		strings.HasSuffix(method, ".logs") || strings.HasSuffix(method, ".runs") ||
		strings.HasSuffix(method, ".revisions") || method == "file.search" || method == "ai.config.models"
}

func rpcPayloadSizeBucket(size int) string {
	switch {
	case size <= preferredRPCPagePayload:
		return "at_or_below_48KiB"
	case size <= maximumRPCPayload:
		return "48_to_56KiB"
	case size <= maximumPeerRPCPlaintext:
		return "56_to_60KiB"
	default:
		return "over_60KiB"
	}
}

func (d dispatcher) call(ctx context.Context, method string, input rpcInput) (any, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if strings.HasPrefix(method, "file.") {
		return d.callFileRPC(ctx, method, input)
	}
	if strings.HasPrefix(method, "task.payload.") {
		return d.callTaskPayloadRPC(method, input)
	}
	if strings.HasPrefix(method, "terminal.") && method != "terminal.execute" {
		return d.callInteractiveTerminalRPC(ctx, method, input)
	}
	if (strings.HasPrefix(method, "task.") || strings.HasPrefix(method, "workflow.")) &&
		(d.scope == "remote.peer.task.control" || d.scope == "remote.task.read" &&
			(method == "task.list" || method == "task.get" || method == "task.logs" || method == "task.logs.download.prepare")) {
		return d.callTaskV2RPC(ctx, method, input)
	}
	switch method {
	case "terminal.execute":
		return d.callTerminalExecute(ctx, input)
	case "agent.capabilities.get":
		return agentCapabilitiesWithContext(ctx, d.state), d.state.revisionValue(), nil
	case "agent.environment.get":
		if len(input) != 0 {
			return nil, 0, errRPCInvalid
		}
		variables, revision, stateRevision := d.state.agentEnvironmentSnapshot()
		return agentEnvironmentView(variables, revision), stateRevision, nil
	case "agent.environment.update":
		variables, expectedRevision, err := agentEnvironmentInput(input)
		if err != nil {
			return nil, 0, err
		}
		return d.state.replaceAgentEnvironment(ctx, variables, expectedRevision)
	case "project.create":
		return d.callProjectCreate(ctx, input)
	case "project.directory.list":
		return d.callProjectDirectoryList(input)
	case "project.remove":
		return d.callProjectRemove(ctx, input)
	case "ai.config.test":
		return d.callAIConfigTest(ctx, input)
	case "conversation.send", "conversation.chat.send", "chat.send":
		return d.callConversationSend(ctx, input)
	case "project.list":
		return d.callProjectList(input)
	case "project.refresh", "project.sync":
		return d.callProjectRefresh(ctx)
	case "task.list":
		return d.callTaskList(input)
	case "task.get":
		return d.callTaskGet(input)
	case "task.create":
		return d.callTaskCreate(input)
	case "task.cancel":
		return d.callTaskCancel(input)
	case "task.logs":
		return d.callTaskLogs(input)
	}
	if strings.HasPrefix(method, "ai.config.") {
		return d.callAIConfigRPC(ctx, method, input)
	}
	if strings.HasPrefix(method, "conversation.") || strings.HasPrefix(method, "chat.") {
		return d.callAIConversationRPC(ctx, method, input)
	}
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	switch method {
	case "agent.identity.get":
		return map[string]any{
			"deviceId": d.state.DeviceID, "identityAlgorithm": "Ed25519",
			"identityPublicKey": d.state.publicKey(), "keyVersion": d.state.KeyVersion,
		}, d.state.Revision, nil
	default:
		return nil, 0, errRPCNotFound
	}
}

func (d dispatcher) callAIConfigRPC(ctx context.Context, method string, input rpcInput) (any, uint64, error) {
	if d.state == nil || d.state.business == nil || d.state.secrets == nil {
		return nil, 0, errRPCAIConfigStorageUnavailable
	}
	switch method {
	case "ai.config.list":
		if !onlyInputFields(input, "cursor", "limit") {
			return nil, 0, errRPCInvalid
		}
		configs, err := d.state.business.listAIConfigs(ctx)
		if err != nil {
			return nil, 0, aiConfigStorageError(err)
		}
		items := make([]aiConfigView, 0, len(configs))
		var highWatermark uint64
		for _, config := range configs {
			items = append(items, config.view())
			if config.Revision > highWatermark {
				highWatermark = config.Revision
			}
		}
		pageWatermark, err := rpcPageSnapshotWatermark(map[string]any{
			"method": "ai.config.list", "highWatermark": highWatermark, "items": items,
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
				"items": items[start:end], "nextCursor": versionedPageCursor(pageWatermark, end, len(items)),
				"highWatermark": highWatermark, "snapshotWatermark": pageWatermark,
			}
		}
		count, err := rpcPagePrefixLength(requestedEnd-start, build)
		if err != nil {
			return nil, 0, err
		}
		return build(count), highWatermark, nil
	case "ai.config.get":
		id, ok := optionalInputString(input, "id", 80)
		if id == "" {
			id = "default"
		}
		if !ok || !validAIConfigID(id) {
			return nil, 0, errRPCInvalid
		}
		d.state.mu.RLock()
		config, found := d.state.AIConfigs[id]
		d.state.mu.RUnlock()
		if !found {
			value := defaultAIConfig()
			return value, value.Revision, nil
		}
		return config.view(), config.Revision, nil
	case "ai.config.models":
		return d.discoverAIConfigModels(ctx, input)
	case "ai.config.reasoning-efforts":
		return d.listAIConfigReasoningEfforts(input)
	case "ai.config.update":
		return d.updateAIConfig(ctx, input)
	case "ai.config.delete":
		return d.deleteAIConfig(ctx, input)
	default:
		return nil, 0, errRPCNotFound
	}
}

func (d dispatcher) listAIConfigReasoningEfforts(input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "id", "model") {
		return nil, 0, errRPCInvalid
	}
	id, idOK := optionalInputString(input, "id", 80)
	model, modelOK := inputString(input, "model", 120)
	if id == "" {
		id = "default"
	}
	if !idOK || !modelOK || !validAIConfigID(id) {
		return nil, 0, errRPCInvalid
	}
	config, found := d.state.aiConfigSnapshot(id)
	if !found {
		return nil, 0, errRPCAIConfigNotFound
	}
	// This is the Device Agent's adapter catalog rather than an inference in a
	// UI client. Every value below is accepted by config storage and normalized
	// by the concrete OpenAI/Anthropic/Google/DeepSeek/Ollama adapter. Keeping
	// the requested model in the response makes stale-selection substitution
	// detectable by all clients.
	items := []string{"automatic", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
	return map[string]any{
		"configId": id,
		"model":    model,
		"items":    items,
	}, config.Revision, nil
}

func (d dispatcher) discoverAIConfigModels(ctx context.Context, input rpcInput) (any, uint64, error) {
	if !onlyInputFields(input, "id", "refresh", "cursor", "limit") {
		return nil, 0, errRPCInvalid
	}
	id, ok := optionalInputString(input, "id", 80)
	if id == "" {
		id = "default"
	}
	refresh, refreshOK := optionalInputBool(input, "refresh", false)
	cursor, cursorOK := optionalInputString(input, "cursor", 96)
	if !ok || !refreshOK || !cursorOK || !validAIConfigID(id) || cursor != "" && refresh {
		return nil, 0, errRPCInvalid
	}
	config, found := d.state.aiConfigSnapshot(id)
	if !found {
		return nil, 0, errRPCAIConfigNotFound
	}
	if err := validateAIConfig(config); err != nil {
		return nil, 0, err
	}
	cached, discoveredAt, cacheErr := d.state.business.listAIModelDiscovery(ctx, id, config.Revision)
	if cacheErr != nil {
		return nil, 0, cacheErr
	}
	if cursor != "" {
		if len(cached) == 0 {
			return nil, 0, errRPCRevision
		}
		return aiConfigModelsPage(input, id, config.Revision, cached, true, false, discoveredAt)
	}
	if !refresh && len(cached) > 0 && d.now().Sub(discoveredAt) < 15*time.Minute {
		return aiConfigModelsPage(input, id, config.Revision, cached, true, false, discoveredAt)
	}
	provider := providerFor(d)
	discoverer, supported := provider.(aiModelDiscoverer)
	if !supported {
		return nil, 0, errRPCCapability
	}
	models, err := discoverer.DiscoverModels(ctx, config)
	if err != nil {
		if len(cached) > 0 {
			return aiConfigModelsPage(input, id, config.Revision, cached, true, true, discoveredAt)
		}
		return nil, 0, wrapAIError(err)
	}
	now := d.now().UTC()
	if err := d.state.business.replaceAIModelDiscovery(ctx, config, models, now); err != nil {
		return nil, 0, err
	}
	// Reload the persisted snapshot so the first page and every continuation
	// use the same deterministic model-id ordering and millisecond timestamp.
	models, discoveredAt, err = d.state.business.listAIModelDiscovery(ctx, id, config.Revision)
	if err != nil {
		return nil, 0, err
	}
	return aiConfigModelsPage(input, id, config.Revision, models, false, false, discoveredAt)
}

func aiConfigModelsPage(
	input rpcInput,
	configID string,
	configRevision uint64,
	models []aiModelDescriptor,
	cached bool,
	stale bool,
	discoveredAt time.Time,
) (any, uint64, error) {
	pageWatermark, err := rpcPageSnapshotWatermark(map[string]any{
		"method": "ai.config.models", "configId": configID, "configRevision": configRevision,
		"discoveredAt": discoveredAt, "items": models,
	})
	if err != nil {
		return nil, 0, err
	}
	start, requestedEnd, _, err := versionedPageWindow(input, len(models), pageWatermark)
	if err != nil {
		return nil, 0, err
	}
	build := func(count int) any {
		end := start + count
		return map[string]any{
			"configId": configID, "configRevision": configRevision, "items": models[start:end],
			"cached": cached, "stale": stale, "discoveredAt": discoveredAt,
			"nextCursor": versionedPageCursor(pageWatermark, end, len(models)), "highWatermark": pageWatermark,
		}
	}
	count, err := rpcPagePrefixLength(requestedEnd-start, build)
	if err != nil {
		return nil, 0, err
	}
	return build(count), configRevision, nil
}

func (d dispatcher) updateAIConfig(ctx context.Context, input rpcInput) (any, uint64, error) {
	id, idOK := optionalInputString(input, "id", 80)
	if id == "" {
		id = "default"
		input["id"] = id
	}
	expectedRevision, present, revisionOK := optionalUint64(input, "expectedRevision")
	if !idOK || !validAIConfigID(id) || !revisionOK || !present {
		return nil, 0, errRPCInvalid
	}
	config, err := configFromInput(input)
	if err != nil {
		return nil, 0, err
	}
	d.state.mu.RLock()
	current, exists := d.state.AIConfigs[id]
	d.state.mu.RUnlock()
	if (!exists && expectedRevision != 0) || (exists && expectedRevision != current.Revision) {
		return nil, 0, errRPCRevision
	}
	action, secret, err := aiSecretMutationFromInput(input)
	if err != nil {
		return nil, 0, err
	}
	config.Credential = current.Credential
	config.CredentialConfigured = exists && current.CredentialConfigured
	switch action {
	case "replace":
		config.Credential, config.CredentialConfigured = secret, true
	case "clear":
		config.Credential, config.CredentialConfigured = "", false
	}
	secretChanged := action != "keep"
	secretKey := aiCredentialSecretKey(id)
	if secretChanged {
		if err := beginV2SideEffect(ctx); err != nil {
			return nil, 0, err
		}
		if err := applyAISecretMutation(ctx, d.state.secrets, secretKey, config); err != nil {
			return nil, 0, aiConfigStorageError(err)
		}
	} else if err := completeV2WithoutSideEffect(ctx); err != nil {
		return nil, 0, err
	}
	stored, err := d.state.business.putAIConfig(ctx, config, expectedRevision)
	if err != nil {
		if secretChanged {
			if restoreAISecret(d.state.secrets, secretKey, current, exists) != nil {
				return nil, 0, aiConfigStorageError(errors.New("AI configuration transaction could not be rolled back"))
			}
			if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
				return nil, 0, aiConfigStorageError(errors.Join(err, sideEffectErr))
			}
		}
		return nil, 0, aiConfigStorageError(err)
	}
	stored.Credential = config.Credential
	stored.CredentialConfigured = config.CredentialConfigured
	d.state.mu.Lock()
	d.state.AIConfigs[id] = stored
	d.state.mu.Unlock()
	if secretChanged {
		if err := commitV2SideEffect(ctx); err != nil {
			return nil, 0, aiConfigStorageError(err)
		}
	}
	return stored.view(), stored.Revision, nil
}

func (d dispatcher) deleteAIConfig(ctx context.Context, input rpcInput) (any, uint64, error) {
	id, ok := inputStringAlias(input, []string{"id", "configId"}, 80)
	expected, present, revisionOK := optionalUint64(input, "expectedRevision")
	if !ok || !validAIConfigID(id) || !revisionOK {
		return nil, 0, errRPCInvalid
	}
	d.state.mu.RLock()
	current, exists := d.state.AIConfigs[id]
	d.state.mu.RUnlock()
	if !exists {
		return nil, 0, errRPCAIConfigNotFound
	}
	if present && expected != current.Revision {
		return nil, 0, errRPCRevision
	}
	secretKey := aiCredentialSecretKey(id)
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := d.state.secrets.Delete(ctx, secretKey); err != nil {
		return nil, 0, aiConfigStorageError(errors.New("delete AI credential from SecretStore"))
	}
	var expectedPointer *uint64
	if present {
		expectedPointer = &expected
	}
	deleted, err := d.state.business.deleteAIConfig(ctx, id, expectedPointer)
	if err != nil {
		if restoreAISecret(d.state.secrets, secretKey, current, true) != nil {
			return nil, 0, aiConfigStorageError(errors.New("AI configuration delete could not be rolled back"))
		}
		if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
			return nil, 0, aiConfigStorageError(errors.Join(err, sideEffectErr))
		}
		return nil, 0, aiConfigStorageError(err)
	}
	d.state.mu.Lock()
	delete(d.state.AIConfigs, id)
	d.state.mu.Unlock()
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, aiConfigStorageError(err)
	}
	return map[string]any{"deleted": true, "configId": id}, deleted.Revision, nil
}

func aiConfigStorageError(cause error) error {
	if cause == nil {
		return errRPCAIConfigStorageUnavailable
	}
	if errors.Is(cause, errRPCInvalid) || errors.Is(cause, errRPCNotFound) || errors.Is(cause, errRPCRevision) {
		return cause
	}
	return fmt.Errorf("%w: %v", errRPCAIConfigStorageUnavailable, cause)
}

func aiSecretMutationFromInput(input rpcInput) (string, string, error) {
	action, actionOK := optionalInputString(input, "secretAction", 16)
	secret, secretOK := optionalInputString(input, "secret", maximumSecretBytes)
	_, secretPresent := input["secret"]
	if !actionOK || !secretOK {
		return "", "", errRPCInvalid
	}
	if action == "" {
		if !secretPresent {
			return "keep", "", nil
		}
		if secret == "" {
			return "clear", "", nil
		}
		return "replace", secret, nil
	}
	switch action {
	case "keep":
		if secretPresent {
			return "", "", errRPCInvalid
		}
	case "replace":
		if !secretPresent || secret == "" {
			return "", "", errRPCInvalid
		}
	case "clear":
		if secretPresent && secret != "" {
			return "", "", errRPCInvalid
		}
	default:
		return "", "", errRPCInvalid
	}
	return action, secret, nil
}

func applyAISecretMutation(ctx context.Context, store secretStore, key string, config aiConfig) error {
	if config.CredentialConfigured {
		value := []byte(config.Credential)
		err := store.Put(ctx, key, value)
		zeroSecret(value)
		if err != nil {
			return errors.New("update AI credential in SecretStore")
		}
		return nil
	}
	if err := store.Delete(ctx, key); err != nil {
		return errors.New("update AI credential in SecretStore")
	}
	return nil
}

func restoreAISecret(store secretStore, key string, previous aiConfig, existed bool) error {
	if existed && previous.CredentialConfigured {
		value := []byte(previous.Credential)
		err := store.Put(context.Background(), key, value)
		zeroSecret(value)
		return err
	}
	return store.Delete(context.Background(), key)
}

func (d dispatcher) callAIConfigTest(ctx context.Context, input rpcInput) (any, uint64, error) {
	id, ok := inputStringAlias(input, []string{"id", "configId"}, 80)
	if !ok {
		return nil, 0, errRPCInvalid
	}
	config, found := d.state.aiConfigSnapshot(id)
	if !found {
		return nil, 0, errRPCAIConfigNotFound
	}
	if err := validateAIConfig(config); err != nil {
		return nil, 0, err
	}
	latency, err := providerFor(d).Test(ctx, config)
	if err != nil {
		return nil, 0, wrapAIError(err)
	}
	return map[string]any{"latencyMs": milliseconds(latency), "model": config.Model}, config.Revision, nil
}

func setRPCError(response *remotev1.RpcResponse, code remotev1.RpcErrorCode, message string, retryable bool) {
	setRPCErrorWithRetryAfter(response, code, message, retryable, 0)
}

func setRPCErrorWithRetryAfter(response *remotev1.RpcResponse, code remotev1.RpcErrorCode, message string, retryable bool, retryAfterSeconds uint32) {
	response.Error = &remotev1.RpcError{Code: code, SafeMessage: message, Retryable: retryable, RetryAfterSeconds: retryAfterSeconds}
}

func validMethod(value string) bool {
	if value == "" || len(value) > 80 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '.' && character != '-' {
			return false
		}
	}
	return true
}

func inputString(input rpcInput, key string, maximum int) (string, bool) {
	value, ok := input[key].(string)
	value = strings.TrimSpace(value)
	return value, ok && value != "" && utf8.ValidString(value) && len(value) <= maximum
}

func optionalInputString(input rpcInput, key string, maximum int) (string, bool) {
	value, exists := input[key]
	if !exists || value == nil {
		return "", true
	}
	text, ok := value.(string)
	return strings.TrimSpace(text), ok && utf8.ValidString(text) && len(text) <= maximum
}

func inputStringAlias(input rpcInput, keys []string, maximum int) (string, bool) {
	for _, key := range keys {
		if _, exists := input[key]; exists {
			return inputString(input, key, maximum)
		}
	}
	return "", false
}

func optionalUint64(input rpcInput, key string) (value uint64, present, ok bool) {
	raw, present := input[key]
	if !present || raw == nil {
		return 0, false, true
	}
	number, ok := raw.(float64)
	if !ok || number < 0 || number > float64(^uint64(0)) || number != float64(uint64(number)) {
		return 0, true, false
	}
	return uint64(number), true, true
}

func pageWindow(input rpcInput, total int) (start, end int, next *string, err error) {
	limit := 50
	if raw, exists := input["limit"]; exists {
		number, ok := raw.(float64)
		if !ok || number < 1 || number > 200 || number != float64(int(number)) {
			return 0, 0, nil, errRPCInvalid
		}
		limit = int(number)
	}
	if cursor, ok := optionalInputString(input, "cursor", 64); !ok {
		return 0, 0, nil, errRPCInvalid
	} else if cursor != "" {
		decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(cursor)
		value, parseErr := strconv.Atoi(string(decoded))
		if decodeErr != nil || parseErr != nil || value < 0 || value > total || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
			return 0, 0, nil, errRPCInvalid
		}
		start = value
	}
	end = start + limit
	if end > total {
		end = total
	}
	if end < total {
		value := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
		next = &value
	}
	return start, end, next, nil
}

// messagePageWindow interprets the cursor as the count already consumed from
// the newest end of the sequence. Each returned page stays in chronological
// order so clients can prepend older pages without reversing message content.
func messagePageWindow(input rpcInput, total int) (start, end int, next *string, err error) {
	consumed, consumedEnd, next, err := pageWindow(input, total)
	if err != nil {
		return 0, 0, nil, err
	}
	count := consumedEnd - consumed
	end = total - consumed
	start = end - count
	return start, end, next, nil
}

func syncPageState(input rpcInput, highWatermark uint64) (unchanged, resetRequired bool, err error) {
	cursor, ok := optionalInputString(input, "cursor", 64)
	if !ok {
		return false, false, errRPCInvalid
	}
	// A cursor is an explicit continuation of an already selected snapshot.
	// afterRevision applies only to a fresh first-page reconciliation.
	if cursor != "" {
		return false, false, nil
	}
	afterRevision, present, ok := optionalUint64(input, "afterRevision")
	if !ok {
		return false, false, errRPCInvalid
	}
	if !present {
		return false, false, nil
	}
	if afterRevision == highWatermark {
		return true, false, nil
	}
	return false, afterRevision > 0, nil
}

type fileEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"relativePath"`
	Directory  bool      `json:"directory"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

func (state *agentState) resolveWorkspacePath(relative string) (string, error) {
	relative = filepath.Clean(strings.TrimSpace(relative))
	if relative == "." {
		relative = ""
	}
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errRPCForbidden
	}
	root, err := filepath.Abs(state.Workspace)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.Abs(filepath.Join(root, relative))
	if err != nil || (resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator))) {
		return "", errRPCForbidden
	}
	return resolved, nil
}

func (state *agentState) listFiles(relative string) (any, uint64, error) {
	resolved, err := state.resolveWorkspacePath(relative)
	if err != nil {
		return nil, 0, err
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, errRPCNotFound
		}
		return nil, 0, err
	}
	result := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		path := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		result = append(result, fileEntry{Name: entry.Name(), Path: path, Directory: entry.IsDir(), Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
		if len(result) == 200 {
			break
		}
	}
	return map[string]any{"items": result}, state.revisionValue(), nil
}

func (state *agentState) statFile(relative string) (fileEntry, error) {
	resolved, err := state.resolveWorkspacePath(relative)
	if err != nil {
		return fileEntry{}, err
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return fileEntry{}, errRPCNotFound
	}
	if err != nil {
		return fileEntry{}, err
	}
	return fileEntry{Name: info.Name(), Path: filepath.ToSlash(filepath.Clean(relative)), Directory: info.IsDir(), Size: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
}

func (state *agentState) listProjects() (any, uint64, error) {
	value, _, err := state.listFiles("")
	if err != nil {
		return nil, 0, err
	}
	return value, state.revisionValue(), nil
}

func rpcDeadline(duration time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(time.Now().UTC().Add(duration))
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%v", err)
}
