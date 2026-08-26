package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
)

func TestCompatibilityCapabilityVersionUsesOnlyProtocolSemantics(t *testing.T) {
	dispatch := dispatcher{enforceProjectBinding: true, scope: "remote.peer.task.control"}
	projectID := uuid.NewString()
	tests := []struct {
		method    string
		projectID string
		want      string
	}{
		{method: "file.list", want: "files.v1"},
		{method: "file.list", projectID: projectID, want: "files.v2"},
		{method: "terminal.execute", projectID: projectID, want: "terminal.v1"},
		{method: "terminal.open", projectID: projectID, want: "terminal.v2"},
		{method: "task.list", projectID: projectID, want: "tasks.v2"},
		{method: "conversation.list", want: "ai.v1"},
		{method: "conversation.list", projectID: projectID, want: "ai.v2"},
		{method: "chat.send", projectID: projectID, want: "ai.v1"},
		{method: "ai.config.list", want: "ai.v2"},
		{method: "agent.capabilities.get", want: ""},
	}
	for _, test := range tests {
		if got := compatibilityCapabilityVersion(dispatch, test.method, test.projectID); got != test.want {
			t.Errorf("compatibilityCapabilityVersion(%q, %q) = %q, want %q", test.method, test.projectID, got, test.want)
		}
	}
	dispatch.enforceProjectBinding = false
	if got := compatibilityCapabilityVersion(dispatch, "terminal.execute", projectID); got != "" {
		t.Fatalf("direct/local call was classified as compatibility traffic: %q", got)
	}
}

func TestPeerRPCCompatibilityMetricsPersistV1UsageFailuresAndV2Denominator(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	workspace := filepath.Join(directory, "workspace")
	state, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	projectID := stableProjectID(state.DeviceID, "").String()

	legacyFile := dispatcher{
		state: state, now: time.Now, scope: "remote.peer.file.receive", enforceProjectBinding: true,
	}
	if response := dispatchEnvelope(t, legacyFile, "file.list", `{}`); response.GetError() != nil {
		t.Fatalf("legacy file.list error = %+v", response.GetError())
	}
	if response := dispatchEnvelope(t, legacyFile, "file.read-text", `{}`); response.GetError() == nil {
		t.Fatal("invalid legacy file.read-text unexpectedly succeeded")
	}

	v2File := legacyFile
	v2File.ticketProjectID = projectID
	v2Envelope, err := newCallEnvelope(uuid.NewString(), "file.list", []byte(`{}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v2Envelope.GetRequest().GetHeader().ProjectId = projectID
	if response := v2File.dispatch(t.Context(), v2Envelope).GetResponse(); response.GetError() != nil {
		t.Fatalf("v2 file.list error = %+v", response.GetError())
	}

	legacyTerminal := dispatcher{
		state: state, now: time.Now, scope: "remote.peer.terminal", ticketProjectID: projectID,
		enforceProjectBinding: true,
	}
	terminalEnvelope, err := newCallEnvelope(uuid.NewString(), "terminal.execute", []byte(`{}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	terminalEnvelope.GetRequest().GetHeader().ProjectId = projectID
	if response := legacyTerminal.dispatch(t.Context(), terminalEnvelope).GetResponse(); response.GetError() == nil {
		t.Fatal("invalid legacy terminal.execute unexpectedly succeeded")
	}

	legacyAI := dispatcher{
		state: state, now: time.Now, scope: "remote.peer.ai.chat", enforceProjectBinding: true,
	}
	if response := dispatchEnvelope(t, legacyAI, "conversation.list", `{}`); response.GetError() == nil {
		t.Fatal("unbound legacy conversation.list unexpectedly succeeded")
	}

	if err := state.business.waitForCompatibilityMetrics(t.Context()); err != nil {
		t.Fatalf("wait for queued compatibility metrics: %v", err)
	}
	metrics, err := state.business.listCompatibilityMetrics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byBucket := make(map[string]compatibilityMetricBucket, len(metrics))
	for _, metric := range metrics {
		byBucket[metric.CapabilityVersion+"|"+metric.ErrorCode] = metric
	}
	if byBucket["files.v1|ok"].CallCount != 1 ||
		byBucket["files.v1|RPC_ERROR_CODE_INVALID_ARGUMENT"].CallCount != 1 ||
		byBucket["files.v2|ok"].CallCount != 1 ||
		byBucket["terminal.v1|RPC_ERROR_CODE_INVALID_ARGUMENT"].CallCount != 1 ||
		byBucket["ai.v1|RPC_ERROR_CODE_PROJECT_MISMATCH"].CallCount != 1 {
		t.Fatalf("compatibility metric buckets = %#v", byBucket)
	}

	var fileV1Calls, fileV1Failures, fileV2Calls uint64
	for _, metric := range metrics {
		switch metric.CapabilityVersion {
		case "files.v1":
			fileV1Calls += metric.CallCount
			if metric.ErrorCode != "ok" {
				fileV1Failures += metric.CallCount
			}
		case "files.v2":
			fileV2Calls += metric.CallCount
		}
	}
	if fileV1Calls != 2 || fileV1Failures != 1 || fileV2Calls != 1 {
		t.Fatalf("file compatibility rates cannot be derived: v1=%d failures=%d v2=%d", fileV1Calls, fileV1Failures, fileV2Calls)
	}

	exposed, ok := agentCapabilities(state)["compatibilityMetrics"].([]compatibilityMetricBucket)
	if !ok || len(exposed) != len(metrics) {
		t.Fatalf("capability metric projection = %#v", exposed)
	}
	encoded, err := json.Marshal(exposed)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{workspace, projectID, "terminal.execute", "file.read-text", "conversation.list"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("compatibility metrics leaked sensitive or request-specific value %q: %s", forbidden, encoded)
		}
	}

	reloaded, err := loadOrCreateAgentState(statePath, workspace)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reloaded.business.listCompatibilityMetrics(t.Context())
	if err != nil || len(persisted) != len(metrics) {
		t.Fatalf("persisted compatibility metrics = %#v, %v", persisted, err)
	}
}

func TestCompatibilityMetricStoreRejectsUnboundedDimensions(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.business.recordCompatibilityMetric(
		t.Context(), "files.v3", "ok", time.Millisecond, time.Now().UTC(),
	); err == nil {
		t.Fatal("unrecognized capability version was recorded")
	}
	if err := state.business.recordCompatibilityMetric(
		t.Context(), "files.v1", "path=/private/project", time.Millisecond, time.Now().UTC(),
	); err == nil {
		t.Fatal("unbounded error dimension was recorded")
	}
}

func TestCompatibilityMetricWriteDoesNotDelayRPCResponse(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	projectID := stableProjectID(state.DeviceID, "").String()
	dispatch := dispatcher{
		state: state, now: time.Now, scope: "remote.peer.terminal", ticketProjectID: projectID,
		enforceProjectBinding: true,
	}
	envelope, err := newCallEnvelope(uuid.NewString(), "terminal.execute", []byte(`{}`), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	envelope.GetRequest().GetHeader().ProjectId = projectID

	state.business.mu.Lock()
	locked := true
	defer func() {
		if locked {
			state.business.mu.Unlock()
		}
	}()
	responses := make(chan *remotev1.RpcEnvelope, 1)
	go func() { responses <- dispatch.dispatch(context.Background(), envelope) }()
	select {
	case response := <-responses:
		if response.GetResponse().GetError() == nil {
			t.Fatal("invalid terminal request unexpectedly succeeded")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("RPC response waited for compatibility metric SQLite write")
	}
	state.business.mu.Unlock()
	locked = false
	if err := state.business.waitForCompatibilityMetrics(t.Context()); err != nil {
		t.Fatalf("wait for queued compatibility metrics: %v", err)
	}
}
