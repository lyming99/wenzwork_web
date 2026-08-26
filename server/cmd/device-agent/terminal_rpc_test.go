package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestTerminalRPCUsesKnownProjectAndRestrictsCommandGrammar(t *testing.T) {
	directory := t.TempDir()
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), workspace)
	if err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(workspace, "project-a")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "readme.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.terminal"}
	projectID := stableProjectID(state.DeviceID, "project-a")

	pwd := dispatchJSON(t, dispatch, "terminal.execute", `{"projectId":"`+projectID.String()+`","command":"pwd"}`)
	if pwd["workingDirectory"] != projectPath || pwd["output"] != projectPath || pwd["exitCode"] != float64(0) {
		t.Fatalf("pwd result = %#v", pwd)
	}
	listed := dispatchJSON(t, dispatch, "terminal.execute", `{"projectId":"`+projectID.String()+`","command":"ls"}`)
	if output, _ := listed["output"].(string); !strings.Contains(output, "readme.txt") {
		t.Fatalf("list result = %#v", listed)
	}

	for _, command := range []string{"ls ..", "git status; whoami", "powershell Get-ChildItem", "git log -n 51"} {
		response := dispatchEnvelope(t, dispatch, "terminal.execute", `{"command":`+mustJSONTerminal(t, command)+`}`)
		if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_INVALID_ARGUMENT {
			t.Fatalf("command %q error = %+v", command, response.GetError())
		}
	}
	response := dispatchEnvelope(t, dispatch, "terminal.execute", `{"projectId":"`+uuid.NewString()+`","command":"pwd"}`)
	if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_NOT_FOUND {
		t.Fatalf("unknown project error = %+v", response.GetError())
	}
}

func TestTerminalRPCRequiresDedicatedTicketScope(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	dispatch := dispatcher{state: state, now: time.Now, scope: "remote.peer.query"}
	response := dispatchEnvelope(t, dispatch, "terminal.execute", `{"command":"pwd"}`)
	if response.GetError().GetCode() != remotev1.RpcErrorCode_RPC_ERROR_CODE_FORBIDDEN {
		t.Fatalf("scope error = %+v", response.GetError())
	}
}

func mustJSONTerminal(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
