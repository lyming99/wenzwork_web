package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestAgentEnvironmentRPCPersistsEncryptedValuesAndInjectsCommands(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}

	initial := dispatchJSON(t, dispatch, "agent.environment.get", `{}`)
	if initial["revision"] != float64(1) || len(initial["variables"].(map[string]any)) != 0 {
		t.Fatalf("initial Agent environment = %#v", initial)
	}
	const privateMarker = "agent-environment-private-marker"
	updated := dispatchJSON(t, dispatch, "agent.environment.update", `{
		"expectedRevision": 1,
		"variables": {
			"OPENAI_BASE_URL": "https://gateway.example.test/v1",
			"WENZWORK_CONTROL_URL": "https://managed.example.test",
			"PRIVATE_TOKEN": "`+privateMarker+`",
			"EMPTY_VALUE": ""
		}
	}`)
	if updated["revision"] != float64(2) || updated["variables"].(map[string]any)["PRIVATE_TOKEN"] != privateMarker {
		t.Fatalf("updated Agent environment = %#v", updated)
	}
	for _, path := range []string{statePath, statePath + ".secrets.enc"} {
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(contents, []byte(privateMarker)) {
			t.Fatalf("%s contains plaintext Agent environment value", path)
		}
	}
	if err := state.close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reloaded.close() })
	variables, revision, _ := reloaded.agentEnvironmentSnapshot()
	if revision != 2 || variables["PRIVATE_TOKEN"] != privateMarker || variables["EMPTY_VALUE"] != "" ||
		variables["WENZWORK_CONTROL_URL"] != "https://managed.example.test" {
		t.Fatalf("reloaded Agent environment = %#v revision=%d", variables, revision)
	}

	starter := new(fakeRawStarter)
	supervisor := newRawProcessSupervisorWithDependencies(starter, func(int) (uint64, error) { return 0, nil }, 1)
	supervisor.environmentProvider = reloaded.agentEnvironmentList
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: workspace, WorkingDirectory: workspace,
		Argv: []string{"test-cli"}, Environment: []string{"PRIVATE_TOKEN=task-override"},
		Limits: processResourceLimits{MaximumLifetime: time.Minute, MaximumMemoryBytes: 1, MaximumOutputBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer process.release()
	specs, processes := fakeRawStarterSnapshot(starter)
	if len(specs) != 1 || !slices.Contains(specs[0].Environment, "OPENAI_BASE_URL=https://gateway.example.test/v1") ||
		!slices.Contains(specs[0].Environment, "WENZWORK_CONTROL_URL=https://managed.example.test") ||
		!slices.Contains(specs[0].Environment, "EMPTY_VALUE=") || !slices.Contains(specs[0].Environment, "PRIVATE_TOKEN=task-override") ||
		slices.Contains(specs[0].Environment, "PRIVATE_TOKEN="+privateMarker) {
		t.Fatalf("launched CLI environment = %#v", specs)
	}
	processes[0].finish(0)

	ptyStarter := new(fakePTYStarter)
	ptySupervisor := newProcessSupervisorWithDependencies(ptyStarter, func(int) (uint64, error) { return 0, nil }, 1)
	ptySupervisor.environmentProvider = reloaded.agentEnvironmentList
	terminal, err := ptySupervisor.Start(processLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: workspace, WorkingDirectory: workspace,
		Argv: []string{"test-shell"}, Rows: 24, Columns: 80,
		Limits: processResourceLimits{MaximumMemoryBytes: 1, MaximumOutputBytes: 1024},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ptyStarter.specs) != 1 || !slices.Contains(ptyStarter.specs[0].Environment, "PRIVATE_TOKEN="+privateMarker) ||
		!slices.Contains(ptyStarter.specs[0].Environment, "WENZWORK_CONTROL_URL=https://managed.example.test") {
		t.Fatalf("launched terminal environment = %#v", ptyStarter.specs)
	}
	ptyStarter.latest().finish(0)
	terminal.release()

	clearDispatch := dispatcher{state: reloaded, now: time.Now, scope: "remote.peer.ai.config"}
	cleared := dispatchJSON(t, clearDispatch, "agent.environment.update", `{"expectedRevision":2,"variables":{}}`)
	if cleared["revision"] != float64(3) || len(cleared["variables"].(map[string]any)) != 0 {
		t.Fatalf("cleared Agent environment = %#v", cleared)
	}
	if _, found, err := reloaded.secrets.Get(t.Context(), agentEnvironmentSecretKey); err != nil || found {
		t.Fatalf("cleared SecretStore item found=%v err=%v", found, err)
	}
}

func TestAgentEnvironmentRPCValidatesScopeInputAndRevision(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.close() })
	config := dispatcher{state: state, now: time.Now, scope: "remote.peer.ai.config"}

	for name, input := range map[string]string{
		"protected":       `{"expectedRevision":1,"variables":{"PATH":"untrusted"}}`,
		"caseDuplicate":   `{"expectedRevision":1,"variables":{"Token":"one","TOKEN":"two"}}`,
		"unknownField":    `{"expectedRevision":1,"variables":{},"replace":true}`,
		"missingRevision": `{"variables":{"TOKEN":"value"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := dispatchEnvelope(t, config, "agent.environment.update", input)
			if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT {
				t.Fatalf("invalid environment error = %+v", response.GetError())
			}
		})
	}

	dispatchJSON(t, config, "agent.environment.update", `{"expectedRevision":1,"variables":{"HTTPS_PROXY":"https://proxy.example.test"}}`)
	conflict := dispatchEnvelope(t, config, "agent.environment.update", `{"expectedRevision":1,"variables":{}}`)
	if conflict.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_REVISION_CONFLICT {
		t.Fatalf("stale environment revision error = %+v", conflict.GetError())
	}
	query := dispatcher{state: state, now: time.Now, scope: "remote.peer.query"}
	forbidden := dispatchEnvelope(t, query, "agent.environment.get", `{}`)
	if forbidden.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN {
		t.Fatalf("query-scope Agent environment error = %+v", forbidden.GetError())
	}
}
