package main

import (
	"testing"

	"github.com/google/uuid"
	remotev2 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v2"
)

func TestV2RPCOperationDigestUsesStableAuthorizationBinding(t *testing.T) {
	controllerID := uuid.NewString()
	projectID := uuid.NewString()
	scope := "remote.peer.ai.config"
	revision := uint64(7)
	request := &remotev2.RpcRequest{
		OperationId:      uuid.NewString(),
		AttemptId:        uuid.NewString(),
		Method:           "ai.config.update",
		ExpectedRevision: &revision,
		Payload:          []byte(`{"id":"default","expectedRevision":7}`),
	}
	first := v2RPCOperationDigest(controllerID, projectID, scope, request)

	retryRevision := revision
	retry := &remotev2.RpcRequest{
		OperationId:      request.GetOperationId(),
		AttemptId:        uuid.NewString(),
		Method:           request.GetMethod(),
		ExpectedRevision: &retryRevision,
		Payload:          append([]byte(nil), request.GetPayload()...),
	}
	if got := v2RPCOperationDigest(controllerID, projectID, scope, retry); got != first {
		t.Fatal("operation digest changed across a new attempt and Channel recovery")
	}

	changedRevision := revision + 1
	bindings := map[string][32]byte{
		"controller": v2RPCOperationDigest(uuid.NewString(), projectID, scope, request),
		"project":    v2RPCOperationDigest(controllerID, uuid.NewString(), scope, request),
		"scope":      v2RPCOperationDigest(controllerID, projectID, "remote.peer.query", request),
		"method": v2RPCOperationDigest(controllerID, projectID, scope, &remotev2.RpcRequest{
			Method: request.GetMethod() + ".other", ExpectedRevision: &revision, Payload: request.GetPayload(),
		}),
		"payload": v2RPCOperationDigest(controllerID, projectID, scope, &remotev2.RpcRequest{
			Method: request.GetMethod(), ExpectedRevision: &revision, Payload: []byte(`{"id":"other"}`),
		}),
		"revision": v2RPCOperationDigest(controllerID, projectID, scope, &remotev2.RpcRequest{
			Method: request.GetMethod(), ExpectedRevision: &changedRevision, Payload: request.GetPayload(),
		}),
	}
	for field, digest := range bindings {
		if digest == first {
			t.Fatalf("operation digest did not bind %s", field)
		}
	}
}
