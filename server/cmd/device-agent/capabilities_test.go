package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	peerv2 "github.com/wenzwork/wenzwork-web/server/internal/peerprotocol/v2"
)

func TestAgentCapabilitiesAdvertiseStableV2SurfaceAndSafeLimits(t *testing.T) {
	capabilities := agentCapabilities(nil)
	protocol, ok := capabilities["protocol"].(map[string]uint32)
	if !ok || protocol["minimum"] != 1 || protocol["maximum"] < protocol["minimum"] {
		t.Fatalf("protocol capabilities = %#v", capabilities["protocol"])
	}
	versions, ok := capabilities["featureVersions"].(map[string]uint32)
	if !ok || versions["agentEnvironment"] != 1 || versions["projects"] < 3 || versions["files"] < 2 || versions["terminal"] < 3 || versions["tasks"] < 2 || versions["taskLogs"] != 1 || versions["workflows"] < 2 || versions["ai"] < 10 {
		t.Fatalf("feature versions = %#v", capabilities["featureVersions"])
	}
	if capabilities["agentBuildId"] != version || capabilities["capabilityVersion"] != uint32(agentCapabilityContractVersion) || capabilities["connectionEpoch"] != uint64(0) {
		t.Fatalf("capability identity = build:%#v contract:%#v epoch:%#v", capabilities["agentBuildId"], capabilities["capabilityVersion"], capabilities["connectionEpoch"])
	}
	if outputVersion, ok := capabilities["executionOutputVersion"].(uint32); !ok || outputVersion != 2 {
		t.Fatalf("execution output version = %#v", capabilities["executionOutputVersion"])
	}
	resources, ok := capabilities["remoteV2Resources"].(v2AgentResourceSnapshot)
	if !ok || resources.LinkCount != 0 || resources.ActiveStreamCount != 0 ||
		resources.SequencerTombstoneHardLimit != peerv2.DefaultSequencerTombstoneLimit ||
		resources.SequencerActiveHardLimit != peerv2.DefaultSequencerActiveLimit ||
		resources.SequencerStreamHardLimit != peerv2.DefaultSequencerStreamLimit {
		t.Fatalf("remote/v2 resource diagnostics = %#v", capabilities["remoteV2Resources"])
	}
	features, ok := capabilities["features"].(map[string]bool)
	if !ok || !features["agent.environment"] || !features["project.v2"] || !features["project.remoteCreate"] || !features["project.remoteRoots"] || !features["project.remoteRemove"] ||
		!features["file.v2"] || !features["file.streamingDownload"] || !features["ai.v2"] || !features["ai.v3"] || !features["ai.v4"] || !features["ai.v5"] || !features["ai.v6"] || !features["ai.v7"] || !features["ai.v8"] || !features["ai.v9"] || !features["ai.v10"] ||
		!features["ai.planMode"] || !features["ai.todo"] || !features["ai.subagents"] || !features["ai.goal"] ||
		!features["ai.permissionModes"] || features["ai.commandSandbox"] != aiCommandSandboxRuntimeAvailable() || !features["ai.generationRecovery"] || !features["ai.conversationFork"] ||
		!features["terminal.attachLongPoll"] || features["terminal.duplexKeepAlive"] || !features["taskLogs.fileSeek"] || !features["taskLogs.bulkDownload"] ||
		features["terminal.interactive"] || features["tasks.v2"] || features["workflow.v2"] || features["ai.taskTools"] {
		t.Fatalf("feature flags = %#v", capabilities["features"])
	}
	limits, ok := capabilities["resourceLimits"].(map[string]uint64)
	if !ok || limits["rpcPayloadBytes"] != maximumRPCPayload || limits["taskRpcPayloadBytes"] != maximumRPCPayload ||
		limits["agentEnvironmentBytes"] != maximumSecretBytes || limits["agentEnvironmentVariables"] != maximumTaskEnvironmentVariables ||
		limits["taskDefinitionBytes"] == 0 || limits["taskLogSeekBytes"] != maximumTaskLogSeekBytes || limits["taskRunLogFileBytes"] != maximumTaskRunLogFileBytes ||
		limits["taskLogRetentionTargetBytesPerTask"] != maximumTaskLogBytesPerTask || limits["taskLogRetentionTargetBytesGlobal"] != maximumTaskLogBytesGlobal ||
		limits["taskLogDiskHardLimitBytes"] != maximumTaskLogDiskBytes || limits["taskLogDiskSafetyReserveBytes"] != minimumTaskLogDiskFreeBytes ||
		limits["taskLogBytesPerTask"] == 0 || limits["managedFileBytes"] != maximumManagedFileBytes || limits["fileTransferBytes"] != maximumManagedFileBytes || limits["fileTransferSeconds"] == 0 ||
		limits["taskConcurrentRuns"] != 0 || limits["taskProjectConcurrentRuns"] != 0 || limits["taskMaximumLifetimeSeconds"] == 0 ||
		limits["taskMaximumMemoryBytes"] == 0 || limits["taskMaximumOutputBytes"] == 0 ||
		limits["workflowMaximumNodes"] != maximumWorkflowV2Nodes || limits["workflowMaximumEdges"] != maximumWorkflowV2Edges ||
		limits["workflowMaximumParallelism"] != maximumWorkflowV2Parallelism ||
		limits["aiWorkspaceReadBytes"] != uint64(maximumAIWorkspaceReadBytes) || limits["aiWorkspaceWriteBytes"] != maximumAIWorkspaceWriteBytes ||
		limits["aiWorkspaceCommandOutputBytes"] != maximumAIWorkspaceCommandOutput || limits["aiApprovalTimeoutSeconds"] != uint64(defaultAIApprovalTimeout.Seconds()) ||
		limits["aiTerminalSessionCount"] != maximumAITerminalSessions || limits["aiTerminalScrollbackBytes"] != maximumAITerminalScrollbackBytes {
		t.Fatalf("resource limits = %#v", capabilities["resourceLimits"])
	}
	if limits["rpcInFlightPerLink"] != v2MaximumRPCsPerLink || limits["rpcInFlightPerDevice"] != v2MaximumRPCsPerDevice ||
		limits["operationCacheBytes"] != v2MaximumOperationBytes || limits["operationJournalDeviceBytes"] != v2OperationMaximumBytesPerDevice {
		t.Fatalf("remote/v2 operation limits = %#v", limits)
	}
	metrics, ok := capabilities["taskLogMetrics"].(map[string]any)
	if !ok || metrics["diskPressureCount"] != uint64(0) || metrics["lastDiskPressureReason"] != "" {
		t.Fatalf("task log metrics = %#v", capabilities["taskLogMetrics"])
	}
	if limits["aiConcurrentGenerations"] != maximumActiveAIGenerations {
		t.Fatalf("AI concurrency limit = %#v", limits["aiConcurrentGenerations"])
	}
	if limits["conversationStreamSubscriptionCount"] != maximumConversationStreamSubscriptions ||
		limits["conversationStreamConversationSubscriptionCount"] != maximumConversationStreamSubscriptionsPerConversation ||
		limits["conversationStreamQueueCount"] != maximumConversationStreamQueueCount ||
		limits["conversationStreamQueueBytes"] != maximumConversationStreamQueueBytes {
		t.Fatalf("conversation stream limits = %#v", limits)
	}
	runners, ok := capabilities["taskRunners"].([]string)
	details, detailsOK := capabilities["taskRunnerDetails"].([]taskRunnerCapability)
	if !ok || !detailsOK || !slices.Contains(runners, "script") || len(details) != len(taskRunnerKinds) {
		t.Fatalf("task runner capabilities = %#v, %#v", runners, details)
	}
	for _, detail := range details {
		if detail.Kind == "script" && (!detail.Available || detail.ProbeStatus != "ready") {
			t.Fatalf("script runner capability = %#v", detail)
		}
	}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > preferredRPCPagePayload {
		t.Fatalf("capability response bytes=%d, preferred page=%d", len(encoded), preferredRPCPagePayload)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"workspacepath", "localpath", "credential", "apikey", "secret"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("capability response contains forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestProjectCreationCapabilitiesIgnorePermissionSettings(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	disableDefaultProjectPolicy(t, state)
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-project.remoteCreate,-project.remoteRoots")
	features := agentFeatureFlags(state)
	if !features["project.remoteCreate"] || !features["project.remoteRoots"] {
		t.Fatalf("project creation was restricted by permission settings: %#v", features)
	}
}

func TestAICommandSandboxCapabilityCannotBeEnabledWithoutRuntime(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "ai.commandSandbox")
	if got := agentFeatureFlags(nil)["ai.commandSandbox"]; got != aiCommandSandboxRuntimeAvailable() {
		t.Fatalf("command sandbox capability=%v runtime=%v", got, aiCommandSandboxRuntimeAvailable())
	}
}

func TestTaskV2CapabilityRequiresProjectPolicyAndSupportsKillSwitch(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	disableDefaultProjectPolicy(t, state)
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "tasks.v2")
	if agentFeatureFlags(state)["tasks.v2"] || slices.Contains(agentRegistrationCapabilities(state), "remote.peer.task.control") {
		t.Fatal("positive environment override granted Task v2 without project policy")
	}
	if _, err := state.business.addProject(context.Background(), t.TempDir(), "Tasks", "", projectPolicy{AllowTaskExecution: true}); err != nil {
		t.Fatal(err)
	}
	if !agentFeatureFlags(state)["tasks.v2"] || !agentFeatureFlags(state)["workflow.v2"] || agentFeatureFlags(state)["ai.taskTools"] ||
		!slices.Contains(agentRegistrationCapabilities(state), "remote.peer.task.control") {
		t.Fatal("Task v2 was not advertised independently from AI task tools")
	}
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-ai.taskTools")
	if !agentFeatureFlags(state)["tasks.v2"] || agentFeatureFlags(state)["ai.taskTools"] {
		t.Fatal("AI task-tool kill switch disabled Task v2 or was ignored")
	}
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-tasks.v2")
	if agentFeatureFlags(state)["tasks.v2"] || agentFeatureFlags(state)["workflow.v2"] || agentFeatureFlags(state)["ai.taskTools"] ||
		slices.Contains(agentRegistrationCapabilities(state), "remote.peer.task.control") {
		t.Fatal("Task v2 rollout kill switch was ignored")
	}
}

func TestAIWorkspaceToolsCapabilityRequiresProjectPolicyAndSupportsKillSwitch(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	disableDefaultProjectPolicy(t, state)
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "ai.workspaceTools")
	if agentFeatureFlags(state)["ai.workspaceTools"] || slices.Contains(agentRegistrationCapabilities(state), "remote.peer.ai.tools") {
		t.Fatal("positive environment override granted AI tools without project policy")
	}
	if _, err := state.business.addProject(context.Background(), t.TempDir(), "AI tools", "", projectPolicy{AllowAIWorkspaceTools: true}); err != nil {
		t.Fatal(err)
	}
	if !agentFeatureFlags(state)["ai.workspaceTools"] || !slices.Contains(agentRegistrationCapabilities(state), "remote.peer.ai.tools") {
		t.Fatal("AI tools were not advertised for an enabled project")
	}
	if got := agentFeatureFlags(state)["ai.persistentTerminal"]; got != interactiveTerminalRuntimeAvailable() {
		t.Fatalf("persistent AI terminal capability=%v runtime=%v", got, interactiveTerminalRuntimeAvailable())
	}
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-ai.persistentTerminal")
	if !agentFeatureFlags(state)["ai.workspaceTools"] || agentFeatureFlags(state)["ai.persistentTerminal"] {
		t.Fatal("persistent AI terminal rollout kill switch was ignored or disabled unrelated tools")
	}
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-ai.workspaceTools")
	if agentFeatureFlags(state)["ai.workspaceTools"] || agentFeatureFlags(state)["ai.persistentTerminal"] || slices.Contains(agentRegistrationCapabilities(state), "remote.peer.ai.tools") {
		t.Fatal("AI tools rollout kill switch was ignored")
	}
}

func TestInteractiveTerminalCapabilityRequiresProjectPolicyAndRuntime(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_SECRET_STORE", "file")
	directory := t.TempDir()
	state, err := loadOrCreateAgentState(filepath.Join(directory, "state.json"), filepath.Join(directory, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.close()
	disableDefaultProjectPolicy(t, state)
	t.Cleanup(setInteractiveTerminalRuntimeProbe(func() bool { return true }))
	if agentFeatureFlags(state)["terminal.interactive"] || !agentFeatureFlags(state)["terminal.attachLongPoll"] {
		t.Fatal("interactive terminal was enabled without project policy")
	}
	if _, err := state.business.addProject(context.Background(), t.TempDir(), "PTY", "", projectPolicy{AllowInteractiveTerminal: true}); err != nil {
		t.Fatal(err)
	}
	if !agentFeatureFlags(state)["terminal.interactive"] || !agentFeatureFlags(state)["terminal.duplexStream"] || !agentFeatureFlags(state)["terminal.duplexKeepAlive"] ||
		!slices.Contains(agentRegistrationCapabilities(state), "remote.peer.terminal.interactive") {
		t.Fatal("interactive terminal was not advertised for an enabled project/runtime")
	}
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-terminal.interactive")
	if agentFeatureFlags(state)["terminal.interactive"] || !agentFeatureFlags(state)["terminal.attachLongPoll"] || slices.Contains(agentRegistrationCapabilities(state), "remote.peer.terminal.interactive") {
		t.Fatal("interactive terminal rollout kill switch was ignored")
	}
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-terminal.attachLongPoll")
	if agentFeatureFlags(state)["terminal.attachLongPoll"] || agentFeatureFlags(state)["terminal.interactive"] || slices.Contains(agentRegistrationCapabilities(state), "remote.peer.terminal.interactive") {
		t.Fatal("terminal long-poll contract kill switch was ignored")
	}
}

func disableDefaultProjectPolicy(t *testing.T, state *agentState) {
	t.Helper()
	projectID := stableProjectID(state.DeviceID, "")
	project, err := state.business.projectByID(t.Context(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.business.updateProject(t.Context(), projectID, nil, nil, &projectPolicy{}, &project.Revision); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCapabilitiesOnlyApplyKnownSafeFeatureOverrides(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_FEATURE_FLAGS", "-file.v2,terminal.interactive,tasks.v2,unknown.flag")
	features := agentFeatureFlags(nil)
	if features["file.v2"] || features["terminal.interactive"] || features["tasks.v2"] {
		t.Fatalf("feature overrides = %#v", features)
	}
	if _, found := features["unknown.flag"]; found {
		t.Fatalf("unknown feature was advertised: %#v", features)
	}
}
