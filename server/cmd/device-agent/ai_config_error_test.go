package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

type rejectingAIConfigSecretStore struct{}

func (rejectingAIConfigSecretStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (rejectingAIConfigSecretStore) Put(context.Context, string, []byte) error {
	return errors.New("injected SecretStore failure")
}

func (rejectingAIConfigSecretStore) Delete(context.Context, string) error {
	return nil
}

func TestAIConfigStorageFailureReturnsStableNonRetryableCode(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	state, err := loadOrCreateAgentState(
		filepath.Join(t.TempDir(), "agent-state.json"),
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	state.secrets = rejectingAIConfigSecretStore{}

	response := dispatchEnvelope(t, dispatcher{
		state: state,
		now:   time.Now,
		scope: "remote.peer.ai.config",
	}, "ai.config.update", `{
		"id":"default","expectedRevision":0,"name":"Default","provider":"openai-compatible",
		"baseUrl":"https://api.example.test/v1","model":"gpt-test","enabled":true,
		"secretAction":"replace","secret":"private-test-marker"
	}`)
	failure := response.GetError()
	if failure == nil || failure.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INTERNAL ||
		failure.GetSafeMessage() != "AI_CONFIG_STORAGE_UNAVAILABLE" || failure.GetRetryable() {
		t.Fatalf("AI config storage error = %+v", failure)
	}
}

func TestMissingAIConfigOperationsReturnStableSemanticCode(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	state, err := loadOrCreateAgentState(
		filepath.Join(t.TempDir(), "agent-state.json"),
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}

	for _, test := range []struct {
		method string
		input  string
	}{
		{method: "ai.config.models", input: `{"id":"missing"}`},
		{method: "ai.config.reasoning-efforts", input: `{"id":"missing","model":"gpt-test"}`},
		{method: "ai.config.test", input: `{"id":"missing"}`},
		{method: "ai.config.delete", input: `{"id":"missing","expectedRevision":1}`},
	} {
		t.Run(test.method, func(t *testing.T) {
			failure := dispatchEnvelope(t, dispatch, test.method, test.input).GetError()
			if failure == nil || failure.GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND ||
				failure.GetSafeMessage() != "AI_CONFIG_NOT_FOUND" || failure.GetRetryable() {
				t.Fatalf("%s missing config error = %+v", test.method, failure)
			}
		})
	}
}
