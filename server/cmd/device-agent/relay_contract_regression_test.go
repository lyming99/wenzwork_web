package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"github.com/wenzwork/wenzwork-web/server/internal/peerprotocol"
	"github.com/wenzwork/wenzwork-web/server/internal/remoteauth"
)

func TestRPCPayloadContractUsesUTF8ByteBoundaries(t *testing.T) {
	for _, size := range []int{maximumRPCPayload - 1, maximumRPCPayload} {
		if !rpcPayloadWithinLimit("task.create", make([]byte, size)) {
			t.Fatalf("%d-byte JSON payload was rejected", size)
		}
	}
	if rpcPayloadWithinLimit("task.create", make([]byte, maximumRPCPayload+1)) {
		t.Fatalf("%d-byte JSON payload was accepted", maximumRPCPayload+1)
	}
	if !peerRPCPlaintextWithinLimit(make([]byte, maximumPeerRPCPlaintext)) ||
		peerRPCPlaintextWithinLimit(make([]byte, maximumPeerRPCPlaintext+1)) {
		t.Fatal("Peer plaintext 60 KiB boundary is not exact")
	}

	unicodeJSON := []byte(`{"value":"` + strings.Repeat("界", 19112) + `"}`)
	if utf8.RuneCount(unicodeJSON) >= maximumRPCPayload || len(unicodeJSON) <= maximumRPCPayload ||
		rpcPayloadWithinLimit("task.create", unicodeJSON) {
		t.Fatalf("UTF-8 byte limit was not enforced: runes=%d bytes=%d", utf8.RuneCount(unicodeJSON), len(unicodeJSON))
	}
}

func TestRPCOversizeReturnsSmallStableErrorInsteadOfSealingPayload(t *testing.T) {
	dispatch := dispatcher{now: time.Now, scope: "remote.peer.query"}
	envelope, err := newCallEnvelope(uuid.NewString(), "agent.capabilities.get", []byte(`{}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	envelope.GetRequest().JsonPayload = make([]byte, 58_368)
	response := dispatch.dispatch(t.Context(), envelope)
	failure := response.GetResponse().GetError()
	if failure.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED ||
		failure.GetSafeMessage() != "RPC_PAYLOAD_TOO_LARGE" {
		t.Fatalf("oversize response = %+v", response.GetResponse())
	}
	if len(response.GetResponse().GetJsonPayload()) >= preferredRPCPagePayload {
		t.Fatalf("oversize error payload is not bounded: %d", len(response.GetResponse().GetJsonPayload()))
	}
	var details struct {
		LimitBytes          int    `json:"limitBytes"`
		SizeBucket          string `json:"sizeBucket"`
		PaginationAvailable bool   `json:"paginationAvailable"`
	}
	if err := json.Unmarshal(response.GetResponse().GetJsonPayload(), &details); err != nil ||
		details.LimitBytes != maximumRPCPayload || details.SizeBucket != "56_to_60KiB" {
		t.Fatalf("oversize details = %+v, error=%v", details, err)
	}
}

func Test58368ByteResponseCannotAdvancePeerSealer(t *testing.T) {
	sealer, err := peerprotocol.NewCipherState(make([]byte, 32), peerprotocol.DirectionTargetToSource, peerprotocol.CipherModeSeal, 1)
	if err != nil {
		t.Fatal(err)
	}
	state := &agentState{DeviceID: uuid.New(), ConnectionEpoch: 3}
	session := &targetPeerSession{
		state: state, sealer: sealer,
		claims: remoteauth.Claims{SessionID: uuid.NewString(), Scopes: []string{"remote.peer.query"}},
	}
	query := &remotev1.PeerCiphertext{SessionId: session.claims.SessionID, QueryId: uuid.NewString()}
	oversized := peerRPCSuccess(query.GetQueryId(), make([]byte, 58_368))
	if _, err := sealPeerRPCLocked(3, session, query, "PEER_COMPLETE", oversized); err == nil || !strings.Contains(err.Error(), "rpc_json_too_large") {
		t.Fatalf("58,368-byte response seal error = %v", err)
	}
	if generation, sequence, exhausted := sealer.NextSequence(); generation != 1 || sequence != 1 || exhausted {
		t.Fatalf("rejected response consumed sequence: generation=%d sequence=%d exhausted=%v", generation, sequence, exhausted)
	}
	small := peerRPCSuccess(query.GetQueryId(), nil)
	setRPCPayloadTooLarge(small.GetResponse(), 58_368, maximumRPCPayload, "agent.capabilities.get")
	if _, err := sealPeerRPCLocked(3, session, query, "PEER_COMPLETE", small); err != nil {
		t.Fatalf("small structured oversize error was not sealable: %v", err)
	}
	diagnostics := state.protocolDiagnosticSnapshot()
	if len(diagnostics) != 1 || diagnostics[0].Stage != "rpcJson" || diagnostics[0].Reason != "rpc_json_too_large" || diagnostics[0].PayloadSizeBucket != "56_to_60KiB" {
		t.Fatalf("oversize diagnostics = %#v", diagnostics)
	}
}

func TestAIConversationEventKindMappingIsCentralAndComplete(t *testing.T) {
	want := map[string]remotev1.RpcEventKind{
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
	if len(aiConversationRPCEventKinds) != len(want) {
		t.Fatalf("event mapping count=%d, want %d", len(aiConversationRPCEventKinds), len(want))
	}
	for name, kind := range want {
		event := aiConversationEvent{
			EventID: uuid.NewString(), ConversationID: uuid.NewString(), GenerationID: uuid.NewString(),
			MessageID: uuid.NewString(), Kind: name, Sequence: 1, Payload: map[string]any{}, OccurredAt: time.Now().UTC(),
		}
		envelope, err := rpcEnvelopeForAIConversationEvent(event, uuid.NewString())
		if err != nil || envelope.GetEvent().GetKind() != kind {
			t.Fatalf("event %q mapped to %v, error=%v", name, envelope.GetEvent().GetKind(), err)
		}
		var payload aiConversationEvent
		if err := json.Unmarshal(envelope.GetEvent().GetJsonPayload(), &payload); err != nil || payload.Kind != name {
			t.Fatalf("event %q outer/JSON mismatch: %+v, error=%v", name, payload, err)
		}
	}
	if _, err := rpcEnvelopeForAIConversationEvent(aiConversationEvent{Kind: "chat.future.unknown"}, uuid.NewString()); !errors.Is(err, errRPCEventKindUnknown) || err.Error() != "rpc_event_kind_unknown" {
		t.Fatalf("unknown event error = %v", err)
	}
}

func TestCollaborationEventsRequireExplicitFullNegotiation(t *testing.T) {
	legacy, err := parseRPCEventNegotiation(rpcInput{})
	if err != nil || !legacy.allows("chat.text.delta") || legacy.allows("chat.goal.changed") || legacy.supportsFullCollaborationContract() {
		t.Fatalf("legacy event negotiation = %+v, error=%v", legacy, err)
	}
	accepted := make([]any, 0, len(collaborationEventKinds))
	for _, kind := range collaborationEventKinds {
		accepted = append(accepted, kind)
	}
	current, err := parseRPCEventNegotiation(rpcInput{
		"eventContractVersion": float64(1),
		"acceptedEventKinds":   accepted,
	})
	if err != nil || !current.supportsFullCollaborationContract() {
		t.Fatalf("current event negotiation = %+v, error=%v", current, err)
	}
	for _, kind := range collaborationEventKinds {
		if !current.allows(kind) {
			t.Fatalf("negotiated event %q was filtered", kind)
		}
	}
	partial, err := parseRPCEventNegotiation(rpcInput{
		"event_contract_version": float64(1),
		"accepted_event_kinds":   accepted[:1],
	})
	if err != nil || partial.supportsFullCollaborationContract() || !partial.allows(collaborationEventKinds[0]) || partial.allows(collaborationEventKinds[1]) {
		t.Fatalf("partial event negotiation = %+v, error=%v", partial, err)
	}
}

func TestRPCEventNegotiationIsParsedFromEveryStreamingRequest(t *testing.T) {
	accepted := make([]string, len(collaborationEventKinds))
	copy(accepted, collaborationEventKinds)
	payload, err := json.Marshal(map[string]any{
		"eventContractVersion": collaborationEventContractVersion,
		"acceptedEventKinds":   accepted,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{
		"event.subscribe", "conversation.events", "conversation.generation.attach",
		"conversation.send", "conversation.chat.send", "chat.send",
	} {
		negotiation, err := rpcEventNegotiationForRequest(&remotev1.RpcRequest{Method: method, JsonPayload: payload})
		if err != nil || !negotiation.supportsFullCollaborationContract() {
			t.Fatalf("%s negotiation = %+v, error=%v", method, negotiation, err)
		}
	}
	legacy, err := rpcEventNegotiationForRequest(&remotev1.RpcRequest{Method: "conversation.events", JsonPayload: []byte(`{}`)})
	if err != nil || legacy.allows("chat.goal.changed") || !legacy.allows("chat.completed") {
		t.Fatalf("legacy request negotiation = %+v, error=%v", legacy, err)
	}
	if _, err := rpcEventNegotiationForRequest(&remotev1.RpcRequest{
		Method: "conversation.events", JsonPayload: []byte(`{"eventContractVersion":1}`),
	}); !errors.Is(err, errRPCInvalid) {
		t.Fatalf("partial negotiation error = %v", err)
	}
}
