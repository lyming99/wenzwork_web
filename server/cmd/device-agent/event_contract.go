package main

import (
	"encoding/json"
	"strings"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

const collaborationEventContractVersion = 1

var collaborationEventKinds = []string{
	"chat.goal.changed",
	"chat.plan_mode.changed",
	"chat.todo.updated",
	"chat.subagent.started",
	"chat.subagent.status",
	"chat.subagent.message",
}

type rpcEventNegotiation struct {
	version  uint64
	accepted map[string]struct{}
}

func parseRPCEventNegotiation(input rpcInput) (rpcEventNegotiation, error) {
	versionValue, versionCamel := input["eventContractVersion"]
	if snake, present := input["event_contract_version"]; present {
		if versionCamel {
			return rpcEventNegotiation{}, errRPCInvalid
		}
		versionValue, versionCamel = snake, true
	}
	acceptedValue, acceptedCamel := input["acceptedEventKinds"]
	if snake, present := input["accepted_event_kinds"]; present {
		if acceptedCamel {
			return rpcEventNegotiation{}, errRPCInvalid
		}
		acceptedValue, acceptedCamel = snake, true
	}
	if !versionCamel && !acceptedCamel {
		return rpcEventNegotiation{}, nil
	}
	version, ok := versionValue.(float64)
	if !versionCamel || !ok || version != collaborationEventContractVersion || !acceptedCamel {
		return rpcEventNegotiation{}, errRPCInvalid
	}
	values, ok := acceptedValue.([]any)
	if !ok || len(values) > len(aiConversationRPCEventKinds) {
		return rpcEventNegotiation{}, errRPCInvalid
	}
	known := make(map[string]struct{}, len(aiConversationRPCEventKinds))
	for kind := range aiConversationRPCEventKinds {
		known[kind] = struct{}{}
	}
	accepted := make(map[string]struct{}, len(values))
	for _, raw := range values {
		kind, ok := raw.(string)
		if !ok || strings.TrimSpace(kind) != kind {
			return rpcEventNegotiation{}, errRPCInvalid
		}
		if _, ok := known[kind]; !ok {
			return rpcEventNegotiation{}, errRPCInvalid
		}
		accepted[kind] = struct{}{}
	}
	return rpcEventNegotiation{version: uint64(version), accepted: accepted}, nil
}

func (negotiation rpcEventNegotiation) allows(kind string) bool {
	if !isCollaborationEventKind(kind) {
		return true
	}
	_, ok := negotiation.accepted[kind]
	return negotiation.version == collaborationEventContractVersion && ok
}

func (negotiation rpcEventNegotiation) supportsFullCollaborationContract() bool {
	if negotiation.version != collaborationEventContractVersion {
		return false
	}
	for _, kind := range collaborationEventKinds {
		if _, ok := negotiation.accepted[kind]; !ok {
			return false
		}
	}
	return true
}

func isCollaborationEventKind(kind string) bool {
	for _, candidate := range collaborationEventKinds {
		if kind == candidate {
			return true
		}
	}
	return false
}

func acceptedRPCEventKinds(negotiation rpcEventNegotiation) []string {
	result := make([]string, 0, len(negotiation.accepted))
	for _, kind := range collaborationEventKinds {
		if _, ok := negotiation.accepted[kind]; ok {
			result = append(result, kind)
		}
	}
	return result
}

func methodNegotiatesRPCEvents(method string) bool {
	switch method {
	case "event.subscribe", "conversation.events", "conversation.generation.attach", "conversation.send", "conversation.chat.send", "chat.send":
		return true
	default:
		return false
	}
}

func rpcEventNegotiationForRequest(request *remotev1.RpcRequest) (rpcEventNegotiation, error) {
	if request == nil || !methodNegotiatesRPCEvents(request.GetMethod()) {
		return rpcEventNegotiation{}, nil
	}
	var input rpcInput
	if len(request.GetJsonPayload()) == 0 || json.Unmarshal(request.GetJsonPayload(), &input) != nil {
		return rpcEventNegotiation{}, errRPCInvalid
	}
	return parseRPCEventNegotiation(input)
}
