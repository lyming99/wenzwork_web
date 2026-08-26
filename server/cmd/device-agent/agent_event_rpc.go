package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type agentEventSubscribeInput struct {
	AfterSequence    *uint64
	HeartbeatSeconds uint64
	EventContract    rpcEventNegotiation
}

func (d dispatcher) streamAgentEvents(ctx context.Context, envelope *remotev1.RpcEnvelope, emit func(*remotev1.RpcEnvelope) error) *remotev1.RpcEnvelope {
	request := envelope.GetRequest()
	requestID := ""
	if request != nil && request.GetHeader() != nil {
		requestID = request.GetHeader().GetRequestId()
	}
	response := newAgentEventRPCResponse(requestID)
	if envelope == nil || envelope.GetProtocolVersion() != 1 || request == nil || request.GetHeader() == nil ||
		request.GetMethod() != "event.subscribe" || uuid.Validate(requestID) != nil ||
		!methodAllowsScope(request.GetMethod(), d.scope) || len(request.GetJsonPayload()) > maximumAgentEventPayloadBytes {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid event subscription", false)
		return response
	}
	projectID, err := uuid.Parse(strings.TrimSpace(request.GetHeader().GetProjectId()))
	if err != nil || projectID == uuid.Nil || d.validateProjectBinding(request.GetMethod(), projectID.String()) != nil || d.state == nil || d.state.business == nil || d.state.eventHub == nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_PROJECT_MISMATCH, "project is not authorized for this session", false)
		return response
	}
	input, err := parseAgentEventSubscribeInput(request.GetJsonPayload())
	if err != nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT, "invalid event subscription", false)
		return response
	}
	subscriber, err := d.state.eventHub.subscribe(projectID)
	if err != nil {
		code := remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL
		if errors.Is(err, errRPCBusy) {
			code = remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY
		}
		setRPCError(response.GetResponse(), code, "event subscription is unavailable", code == remotev1.RpcErrorCode_RPC_ERROR_CODE_BUSY)
		return response
	}
	defer subscriber.close()

	info, err := d.state.business.agentEventStreamInfo(ctx, projectID)
	if err != nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "event subscription is unavailable", true)
		return response
	}
	// The queue is registered before the first watermark read. Arm it at that
	// point, then capture a second watermark: changes between the two reads are
	// covered by the direct replay and changes after arming are queued. This is
	// per-subscriber suppression only, so a new subscription never advances the
	// pump watermark past events that older subscribers still need.
	subscriber.beginLiveAt(info.HighWatermark)
	info, err = d.state.business.agentEventStreamInfo(ctx, projectID)
	if err != nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "event subscription is unavailable", true)
		return response
	}
	resetReason := ""
	resetRequired := false
	if input.AfterSequence == nil {
		resetRequired, resetReason = true, "bootstrap"
	} else if *input.AfterSequence > info.HighWatermark {
		resetRequired, resetReason = true, "sequenceGap"
	} else if *input.AfterSequence+1 < info.MinimumAvailableSequence {
		resetRequired, resetReason = true, "retention"
	}
	if err := emit(agentEventControlEnvelope(requestID, projectID, "subscription.ready", info.HighWatermark, map[string]any{
		"schemaVersion":            1,
		"type":                     "subscription.ready",
		"projectId":                projectID.String(),
		"minimumAvailableSequence": info.MinimumAvailableSequence,
		"highWatermark":            info.HighWatermark,
		"resetRequired":            resetRequired,
		"resetReason":              resetReason,
		"heartbeatSeconds":         input.HeartbeatSeconds,
		"supportedTopics":          []string{"agent", "capabilities", "conversation", "message", "task", "taskLog", "workflow"},
		"eventContractVersion":     input.EventContract.version,
		"acceptedEventKinds":       acceptedRPCEventKinds(input.EventContract),
		"collaborationEvents":      input.EventContract.supportsFullCollaborationContract(),
		"agent":                    map[string]any{"status": "ready", "capabilitiesRevision": d.state.revisionValue()},
	})); err != nil {
		return agentEventClosedResponse(requestID, "agentShutdown", 0, false)
	}
	if resetRequired {
		_ = emit(agentEventControlEnvelope(requestID, projectID, "subscription.resetRequired", info.HighWatermark, map[string]any{
			"schemaVersion": 1, "type": "subscription.resetRequired", "projectId": projectID.String(),
			"highWatermark": info.HighWatermark, "resetRequired": true, "resetReason": resetReason,
		}))
		return agentEventClosedResponse(requestID, "reset", 0, true)
	}
	lastEmitted := uint64(0)
	if input.AfterSequence != nil {
		lastEmitted = *input.AfterSequence
	}
	if lastEmitted < info.HighWatermark {
		events, err := d.state.business.listAgentEvents(ctx, projectID, lastEmitted, info.HighWatermark)
		if err != nil || !agentEventReplayIsContiguous(events, lastEmitted, info.HighWatermark) {
			_ = emit(agentEventControlEnvelope(requestID, projectID, "subscription.resetRequired", info.HighWatermark, map[string]any{
				"schemaVersion": 1, "type": "subscription.resetRequired", "projectId": projectID.String(),
				"highWatermark": info.HighWatermark, "resetRequired": true, "resetReason": "sequenceGap",
			}))
			return agentEventClosedResponse(requestID, "reset", lastEmitted, true)
		}
		for _, event := range events {
			if err := emit(agentEventEnvelope(event, requestID, info.HighWatermark)); err != nil {
				return agentEventClosedResponse(requestID, "agentShutdown", lastEmitted, false)
			}
			lastEmitted = event.Sequence
		}
	}

	heartbeat := time.NewTicker(time.Duration(input.HeartbeatSeconds) * time.Second)
	defer heartbeat.Stop()
	for {
		if reason := subscriber.resetReasonValue(); reason != "" {
			if !validAgentEventResetReason(reason) {
				return agentEventClosedResponse(requestID, "agentShutdown", lastEmitted, false)
			}
			_ = emit(agentEventControlEnvelope(requestID, projectID, "subscription.resetRequired", d.state.eventHub.publishedSequence(projectID), map[string]any{
				"schemaVersion": 1, "type": "subscription.resetRequired", "projectId": projectID.String(),
				"highWatermark": d.state.eventHub.publishedSequence(projectID), "resetRequired": true, "resetReason": reason,
			}))
			return agentEventClosedResponse(requestID, "reset", lastEmitted, true)
		}
		select {
		case <-ctx.Done():
			reason := "clientCancel"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = "deadline"
			}
			return agentEventClosedResponse(requestID, reason, lastEmitted, false)
		case <-heartbeat.C:
			info, err := d.state.business.agentEventStreamInfo(ctx, projectID)
			if err != nil {
				continue
			}
			if err := emit(agentEventControlEnvelope(requestID, projectID, "subscription.heartbeat", info.HighWatermark, map[string]any{
				"schemaVersion": 1, "type": "subscription.heartbeat", "projectId": projectID.String(),
				"highWatermark": info.HighWatermark, "serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})); err != nil {
				return agentEventClosedResponse(requestID, "agentShutdown", lastEmitted, false)
			}
		case event := <-subscriber.events:
			if !subscriber.consume(event) {
				return agentEventClosedResponse(requestID, "agentShutdown", lastEmitted, false)
			}
			if event.Sequence <= lastEmitted {
				continue
			}
			if event.Sequence != lastEmitted+1 {
				_ = emit(agentEventControlEnvelope(requestID, projectID, "subscription.resetRequired", d.state.eventHub.publishedSequence(projectID), map[string]any{
					"schemaVersion": 1, "type": "subscription.resetRequired", "projectId": projectID.String(),
					"highWatermark": d.state.eventHub.publishedSequence(projectID), "resetRequired": true, "resetReason": "sequenceGap",
				}))
				return agentEventClosedResponse(requestID, "reset", lastEmitted, true)
			}
			if err := emit(agentEventEnvelope(event, requestID, d.state.eventHub.publishedSequence(projectID))); err != nil {
				return agentEventClosedResponse(requestID, "agentShutdown", lastEmitted, false)
			}
			lastEmitted = event.Sequence
		}
	}
}

func validAgentEventResetReason(value string) bool {
	switch value {
	case "bootstrap", "retention", "sequenceGap", "slowConsumer", "schemaChanged":
		return true
	default:
		return false
	}
}

func parseAgentEventSubscribeInput(payload []byte) (agentEventSubscribeInput, error) {
	result := agentEventSubscribeInput{HeartbeatSeconds: 20}
	if len(payload) == 0 {
		return result, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return result, errRPCInvalid
	}
	for key := range raw {
		if key != "afterSequence" && key != "heartbeatSeconds" && key != "eventContractVersion" &&
			key != "acceptedEventKinds" && key != "event_contract_version" && key != "accepted_event_kinds" {
			return result, errRPCInvalid
		}
	}
	if value, ok := raw["afterSequence"]; ok {
		sequence, err := parseAgentEventUint(value)
		if err != nil {
			return result, err
		}
		result.AfterSequence = &sequence
	}
	if value, ok := raw["heartbeatSeconds"]; ok {
		heartbeat, err := parseAgentEventUint(value)
		if err != nil || heartbeat < minimumAgentEventHeartbeatSeconds || heartbeat > maximumAgentEventHeartbeatSeconds {
			return result, errRPCInvalid
		}
		result.HeartbeatSeconds = heartbeat
	}
	var negotiationInput rpcInput
	if err := json.Unmarshal(payload, &negotiationInput); err != nil {
		return result, errRPCInvalid
	}
	negotiation, err := parseRPCEventNegotiation(negotiationInput)
	if err != nil {
		return result, err
	}
	result.EventContract = negotiation
	return result, nil
}

func parseAgentEventUint(value json.RawMessage) (uint64, error) {
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, errRPCInvalid
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 0 || uint64(parsed) > maxSafeJSONInteger {
		return 0, errRPCInvalid
	}
	return uint64(parsed), nil
}

func agentEventReplayIsContiguous(events []agentEventRecord, after, through uint64) bool {
	if after == through {
		return len(events) == 0
	}
	if len(events) == 0 {
		return false
	}
	expected := after + 1
	for _, event := range events {
		if event.Sequence != expected {
			return false
		}
		expected++
	}
	return expected == through+1
}

func newAgentEventRPCResponse(requestID string) *remotev1.RpcEnvelope {
	return &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Response{Response: &remotev1.RpcResponse{
		Header: &remotev1.RpcResponseHeader{RequestId: requestID},
	}}}
}

func agentEventClosedResponse(requestID, reason string, lastEmitted uint64, resetRequired bool) *remotev1.RpcEnvelope {
	response := newAgentEventRPCResponse(requestID)
	payload, err := json.Marshal(map[string]any{
		"closed": true, "reason": reason, "lastEmittedSequence": lastEmitted, "resetRequired": resetRequired,
	})
	if err != nil {
		setRPCError(response.GetResponse(), remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL, "event subscription could not close", true)
		return response
	}
	response.GetResponse().JsonPayload = payload
	return response
}

func agentEventEnvelope(event agentEventRecord, requestID string, highWatermark uint64) *remotev1.RpcEnvelope {
	if highWatermark < event.Sequence {
		highWatermark = event.Sequence
	}
	return &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Event{Event: &remotev1.RpcEvent{
		EventId: event.EventID, Kind: remotev1.RpcEventKind_RPC_EVENT_KIND_AGENT_STATE_CHANGED,
		RequestId: requestID, Sequence: event.Sequence, HighWatermark: highWatermark,
		OccurredAt: timestamppb.New(event.OccurredAt), JsonPayload: append([]byte(nil), event.SafePayloadJSON...),
	}}}
}

func agentEventControlEnvelope(requestID string, projectID uuid.UUID, eventType string, highWatermark uint64, payload map[string]any) *remotev1.RpcEnvelope {
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maximumAgentEventPayloadBytes {
		encoded = []byte(`{"schemaVersion":1,"type":"subscription.closing"}`)
	}
	return &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Event{Event: &remotev1.RpcEvent{
		EventId: uuid.NewString(), Kind: remotev1.RpcEventKind_RPC_EVENT_KIND_EVENT_SUBSCRIPTION_CONTROL,
		RequestId: requestID, Sequence: 0, HighWatermark: highWatermark, OccurredAt: timestamppb.Now(), JsonPayload: encoded,
	}}}
}
