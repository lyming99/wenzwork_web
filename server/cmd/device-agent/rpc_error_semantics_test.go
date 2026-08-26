package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestDispatcherDistinguishesDeadlineFromExplicitCancellation(t *testing.T) {
	state, err := loadOrCreateAgentState(filepath.Join(t.TempDir(), "state.json"), filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.query"}
	request, err := newCallEnvelope(uuid.NewString(), "agent.capabilities.get", []byte(`{}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	deadlineContext, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	deadlineError := dispatch.dispatch(deadlineContext, request).GetResponse().GetError()
	if deadlineError.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_DEADLINE_EXCEEDED || !deadlineError.GetRetryable() {
		t.Fatalf("deadline error = %+v", deadlineError)
	}

	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	cancelledError := dispatch.dispatch(cancelledContext, request).GetResponse().GetError()
	if cancelledError.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_CANCELLED || !cancelledError.GetRetryable() {
		t.Fatalf("cancelled error = %+v", cancelledError)
	}
}
