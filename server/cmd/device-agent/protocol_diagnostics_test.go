package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestDeviceProtocolDiagnosticsRetainOnlyContentFreeRing(t *testing.T) {
	state := &agentState{DeviceID: uuid.New(), ConnectionEpoch: 9}
	requestID, sessionID := uuid.NewString(), uuid.NewString()
	for index := 0; index < maximumDeviceProtocolDiagnostics+12; index++ {
		state.recordProtocolDiagnostic(
			"rpcJson", "rpc_json_invalid", "operation", "inbound", "conversation.send",
			"remote.peer.ai.chat", maximumRPCPayload+index, requestID, sessionID,
		)
	}
	diagnostics := state.protocolDiagnosticSnapshot()
	if len(diagnostics) != maximumDeviceProtocolDiagnostics {
		t.Fatalf("diagnostic count = %d", len(diagnostics))
	}
	last := diagnostics[len(diagnostics)-1]
	if last.Stage != "rpcJson" || last.Reason != "rpc_json_invalid" || last.FaultLevel != "operation" ||
		last.MethodClass != "conversation" || last.Scope != "remote.peer.ai.chat" || last.ConnectionEpoch != 9 ||
		last.RequestHash == "" || last.SessionHash == "" || last.RootFailureID == "" ||
		last.RequestHash == requestID || last.SessionHash == sessionID {
		t.Fatalf("last diagnostic = %#v", last)
	}
	encoded, err := json.Marshal(diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{requestID, sessionID, "conversation.send", "prompt", "ciphertext", "ticket"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("diagnostics leaked %q", forbidden)
		}
	}
}

func TestDispatcherDiagnosesInvalidUTF8WithoutRetainingPayload(t *testing.T) {
	state := &agentState{DeviceID: uuid.New(), ConnectionEpoch: 4}
	requestID := uuid.NewString()
	payload := append([]byte(`{"secret":"never-store"}`), 0xff)
	d := dispatcher{state: state, scope: "remote.peer.query", now: func() time.Time { return time.Now().UTC() }}
	response := d.dispatch(context.Background(), &remotev1.RpcEnvelope{
		ProtocolVersion: 1,
		Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
			Header: &remotev1.RpcRequestHeader{
				RequestId: requestID, IdempotencyKey: "diagnostic-test", Deadline: timestamppb.New(time.Now().Add(time.Minute)),
			},
			Method: "agent.capabilities.get", JsonPayload: payload,
		}},
	})
	if response.GetResponse().GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("response = %+v", response)
	}
	diagnostics := state.protocolDiagnosticSnapshot()
	if len(diagnostics) != 1 || diagnostics[0].Reason != "rpc_json_invalid_utf8" || diagnostics[0].PayloadSizeBucket == "empty" {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	encoded, _ := json.Marshal(diagnostics)
	if strings.Contains(string(encoded), "never-store") {
		t.Fatal("diagnostics retained RPC JSON")
	}
}
