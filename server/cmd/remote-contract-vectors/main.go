// remote-contract-vectors generates deterministic, content-free Protobuf
// vectors consumed by the Go, Dart, and TypeScript protocol tests.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type encodedVector struct {
	Name            string `json:"name"`
	Layer           string `json:"layer"`
	Base64URL       string `json:"base64Url"`
	SHA256          string `json:"sha256"`
	EncodedBytes    int    `json:"encodedBytes"`
	ProtocolVersion uint32 `json:"protocolVersion"`
	MessageField    int    `json:"messageField"`
	EventKind       int32  `json:"eventKind,omitempty"`
	JSONKind        string `json:"jsonKind,omitempty"`
}

type vectorFixture struct {
	ContractVersion uint32          `json:"contractVersion"`
	GeneratedBy     string          `json:"generatedBy"`
	Relay           []encodedVector `json:"relay"`
	RPC             []encodedVector `json:"rpc"`
	Forward         []encodedVector `json:"forwardCompatibility"`
}

type canonicalContract struct {
	RPCEventKinds     map[string]int32 `json:"rpcEventKinds"`
	RequestID         string           `json:"requestId"`
	IdempotencyKey    string           `json:"idempotencyKey"`
	ProjectID         string           `json:"projectId"`
	ExpectedRevision  uint64           `json:"expectedRevision"`
	Deadline          string           `json:"deadline"`
	Method            string           `json:"method"`
	Input             map[string]any   `json:"input"`
	ExpectedBase64URL string           `json:"expectedBase64Url"`
}

func main() {
	output := flag.String("output", "../api/remote/v1/fixtures/protocol_golden_vectors.json", "output fixture")
	contractPath := flag.String("contract", "../api/remote/v1/fixtures/rpc_v2_contract.json", "canonical contract")
	flag.Parse()
	contents, err := os.ReadFile(*contractPath)
	if err != nil {
		panic(err)
	}
	var contract canonicalContract
	if json.Unmarshal(contents, &contract) != nil || len(contract.RPCEventKinds) != 20 {
		panic("canonical RPC event contract is invalid")
	}
	fixture := vectorFixture{
		ContractVersion: 1,
		GeneratedBy:     "server/cmd/remote-contract-vectors",
		Relay:           relayVectors(),
		RPC:             rpcVectors(contract),
	}
	known := fixture.RPC[1]
	knownBytes, err := base64.RawURLEncoding.DecodeString(known.Base64URL)
	if err != nil {
		panic(err)
	}
	withUnknownField := append([]byte(nil), knownBytes...)
	withUnknownField = protowire.AppendTag(withUnknownField, 99, protowire.BytesType)
	withUnknownField = protowire.AppendBytes(withUnknownField, []byte("future-field-v1"))
	fixture.Forward = []encodedVector{
		vector("rpc-response-unknown-field", "rpc", withUnknownField, 1, 11, 0, ""),
		unknownEventVector(),
	}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		panic(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(*output, encoded, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("generated %s\n", *output)
}

func relayVectors() []encodedVector {
	deadline := timestamppb.New(time.Date(2099, 1, 2, 3, 4, 5, 6_000_000, time.UTC))
	frameID := make([]byte, 16)
	for index := range frameID {
		frameID[index] = byte(index + 1)
	}
	key := make([]byte, 32)
	signature := make([]byte, 64)
	for index := range key {
		key[index] = byte(index + 1)
	}
	for index := range signature {
		signature[index] = byte(index + 65)
	}
	values := []struct {
		name  string
		field int
		value *remotev1.Envelope
	}{
		{"relay-challenge", 10, &remotev1.Envelope{ProtocolVersion: 1, FrameId: frameID, Frame: &remotev1.Envelope_AuthChallenge{AuthChallenge: &remotev1.AuthChallenge{Nonce: key, RelayNodeId: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RelayCellId: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Deadline: deadline}}}},
		{"relay-hello", 11, &remotev1.Envelope{ProtocolVersion: 1, FrameId: frameID, ConnectionEpoch: 7, Frame: &remotev1.Envelope_AuthProof{AuthProof: &remotev1.AuthProof{TicketJti: "ticket-golden-v1", ConnectionEpoch: 7, DeviceSignature: signature}}}},
		{"relay-ready", 12, &remotev1.Envelope{ProtocolVersion: 1, FrameId: frameID, ConnectionEpoch: 7, Frame: &remotev1.Envelope_Ready{Ready: &remotev1.Ready{ConnectionId: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", AcceptedConnectionEpoch: 7, HeartbeatIntervalSeconds: 25, ControlFrameLimitBytes: 4096, AbsoluteFrameLimitBytes: 1 << 20}}}},
		{"peer-open", 30, &remotev1.Envelope{ProtocolVersion: 1, FrameId: frameID, ConnectionEpoch: 7, Frame: &remotev1.Envelope_PeerOpen{PeerOpen: &remotev1.PeerOpen{SessionTicket: "fixture-ticket", SessionId: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", EphemeralPublicKey: key, IdentitySignature: signature}}}},
		{"peer-open-ack", 31, &remotev1.Envelope{ProtocolVersion: 1, FrameId: frameID, ConnectionEpoch: 7, Frame: &remotev1.Envelope_PeerReady{PeerReady: &remotev1.PeerReady{SessionId: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", EphemeralPublicKey: key, IdentitySignature: signature}}}},
		{"peer-close", 36, &remotev1.Envelope{ProtocolVersion: 1, FrameId: frameID, ConnectionEpoch: 7, Frame: &remotev1.Envelope_PeerError{PeerError: &remotev1.PeerError{SessionId: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", QueryId: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", Code: remotev1.ErrorCode_ERROR_CODE_PEER_INTERRUPTED, Retryable: true}}}},
	}
	result := make([]encodedVector, 0, len(values))
	for _, value := range values {
		result = append(result, vector(value.name, "relay", mustMarshal(value.value), 1, value.field, 0, ""))
	}
	return result
}

func rpcVectors(contract canonicalContract) []encodedVector {
	deadline, err := time.Parse(time.RFC3339Nano, contract.Deadline)
	if err != nil {
		panic(err)
	}
	requestPayload, err := json.Marshal(contract.Input)
	if err != nil {
		panic(err)
	}
	request := &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
		Header: &remotev1.RpcRequestHeader{
			RequestId: contract.RequestID, IdempotencyKey: contract.IdempotencyKey,
			ExpectedRevision: proto.Uint64(contract.ExpectedRevision), Deadline: timestamppb.New(deadline), ProjectId: contract.ProjectID,
		},
		Method: contract.Method, JsonPayload: requestPayload,
	}}}
	requestBytes := mustMarshal(request)
	if base64.RawURLEncoding.EncodeToString(requestBytes) != contract.ExpectedBase64URL {
		panic("canonical RpcRequest encoding drifted")
	}
	requestID := contract.RequestID
	response := &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Response{Response: &remotev1.RpcResponse{Header: &remotev1.RpcResponseHeader{RequestId: requestID, Revision: 7}, JsonPayload: []byte(`{"ok":true}`)}}}
	failure := &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Response{Response: &remotev1.RpcResponse{Header: &remotev1.RpcResponseHeader{RequestId: requestID}, Error: &remotev1.RpcError{Code: remotev1.RpcErrorCode_RPC_ERROR_CODE_RESOURCE_EXHAUSTED, SafeMessage: "RPC_PAYLOAD_TOO_LARGE"}, JsonPayload: []byte(`{"limitBytes":57344}`)}}}
	responseBytes := mustMarshal(response)
	result := []encodedVector{
		vector("rpc-request", "rpc", requestBytes, 1, 10, 0, ""),
		vector("rpc-response", "rpc", responseBytes, 1, 11, 0, ""),
		vector("rpc-error", "rpc", mustMarshal(failure), 1, 11, 0, ""),
		vector("rpc-envelope-exact-limit", "rpc", padUnknownFieldToSize(responseBytes, 60<<10), 1, 11, 0, ""),
	}
	type eventEntry struct {
		name string
		kind int32
	}
	events := make([]eventEntry, 0, len(contract.RPCEventKinds))
	for name, kind := range contract.RPCEventKinds {
		events = append(events, eventEntry{name, kind})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].kind < events[j].kind })
	for _, entry := range events {
		payload, _ := json.Marshal(map[string]any{"kind": entry.name})
		event := &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Event{Event: &remotev1.RpcEvent{
			EventId: "ffffffff-ffff-4fff-8fff-ffffffffffff", Kind: remotev1.RpcEventKind(entry.kind),
			RequestId: requestID, Sequence: uint64(entry.kind), HighWatermark: 20,
			OccurredAt: timestamppb.New(time.Date(2099, 1, 2, 3, 4, 5, int(entry.kind)*1_000_000, time.UTC)), JsonPayload: payload,
		}}}
		result = append(result, vector(fmt.Sprintf("rpc-event-%02d", entry.kind), "rpc", mustMarshal(event), 1, 12, entry.kind, entry.name))
	}
	return result
}

func unknownEventVector() encodedVector {
	payload := []byte(`{"kind":"future.event"}`)
	event := &remotev1.RpcEnvelope{ProtocolVersion: 1, Message: &remotev1.RpcEnvelope_Event{Event: &remotev1.RpcEvent{
		EventId: "ffffffff-ffff-4fff-8fff-ffffffffffff", Kind: remotev1.RpcEventKind(127),
		RequestId: "22222222-2222-4222-8222-222222222222", Sequence: 127, HighWatermark: 127, JsonPayload: payload,
	}}}
	return vector("rpc-event-unknown-enum", "rpc", mustMarshal(event), 1, 12, 127, "future.event")
}

func vector(name, layer string, value []byte, protocolVersion uint32, messageField int, eventKind int32, jsonKind string) encodedVector {
	digest := sha256.Sum256(value)
	return encodedVector{Name: name, Layer: layer, Base64URL: base64.RawURLEncoding.EncodeToString(value), SHA256: hex.EncodeToString(digest[:]), EncodedBytes: len(value), ProtocolVersion: protocolVersion, MessageField: messageField, EventKind: eventKind, JSONKind: jsonKind}
}

func padUnknownFieldToSize(value []byte, size int) []byte {
	for padding := 0; padding <= size-len(value); padding++ {
		candidate := append([]byte(nil), value...)
		candidate = protowire.AppendTag(candidate, 100, protowire.BytesType)
		candidate = protowire.AppendBytes(candidate, make([]byte, padding))
		if len(candidate) == size {
			return candidate
		}
	}
	panic("could not construct exact-size Protobuf vector")
}

func mustMarshal(value proto.Message) []byte {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
