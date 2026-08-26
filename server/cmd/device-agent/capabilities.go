package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
)

const agentCapabilityContractVersion = 3

// agentCapabilities is intentionally device-local and non-sensitive. It is
// returned only through the E2EE RPC channel so the control plane continues to
// store a small, reviewed projection rather than OS details or paths.
func agentCapabilities(state *agentState) map[string]any {
	return agentCapabilitiesWithContext(context.Background(), state)
}

func agentCapabilitiesWithContext(ctx context.Context, state *agentState) map[string]any {
	flags := agentFeatureFlagsWithContext(ctx, state)
	runnerDetails := agentTaskRunnerCapabilities(state)
	compatibilityMetrics := make([]compatibilityMetricBucket, 0)
	if state != nil && state.business != nil {
		if metrics, err := state.business.listCompatibilityMetrics(ctx); err == nil {
			compatibilityMetrics = metrics
		}
	}
	connectionEpoch := uint64(0)
	if state != nil {
		connectionEpoch = state.connectionEpochValue()
	}
	return map[string]any{
		"agentBuildId":      version,
		"connectionEpoch":   connectionEpoch,
		"capabilityVersion": uint32(agentCapabilityContractVersion),
		"protocol":          map[string]uint32{"minimum": 1, "maximum": 1},
		"featureVersions": map[string]uint32{
			"agentEnvironment": 1,
			"projects":         3,
			"files":            2,
			"terminal":         4,
			"tasks":            2,
			"taskLogs":         1,
			"workflows":        2,
			"ai":               10,
			"events":           1,
		},
		"features": flags,
		"platform": map[string]string{
			"os":   runtime.GOOS,
			"arch": runtime.GOARCH,
		},
		"shells":            availableShells(),
		"taskRunners":       availableTaskRunners(runnerDetails),
		"taskRunnerDetails": runnerDetails,
		"resourceLimits":    agentResourceLimits(),
		"eventContract": map[string]any{
			"version": uint32(1),
			"kinds": []string{
				"chat.goal.changed", "chat.plan_mode.changed", "chat.todo.updated",
				"chat.subagent.started", "chat.subagent.status", "chat.subagent.message",
			},
		},
		"executionOutputVersion": uint32(2),
		"compatibilityMetrics":   compatibilityMetrics,
		"taskLogMetrics":         stateTaskLogMetrics(state),
		"remoteV2Resources":      state.v2ResourceSnapshot(),
		"remoteOperationJournal": stateV2OperationJournalSnapshot(ctx, state),
	}
}

func stateV2OperationJournalSnapshot(ctx context.Context, state *agentState) v2OperationJournalSnapshot {
	if state == nil || state.business == nil {
		return v2OperationJournalSnapshot{}
	}
	return state.business.v2OperationJournalSnapshot(ctx)
}

func stateTaskLogMetrics(state *agentState) map[string]any {
	if state == nil || state.tasksV2 == nil {
		return map[string]any{"diskPressureCount": uint64(0), "lastDiskPressureReason": ""}
	}
	return state.tasksV2.taskLogMetricSnapshot()
}

func agentFeatureFlags(state *agentState) map[string]bool {
	return agentFeatureFlagsWithContext(context.Background(), state)
}

func agentFeatureFlagsWithContext(ctx context.Context, state *agentState) map[string]bool {
	// Stable project/file v2 is on by default for a new Agent. Higher-risk
	// capabilities stay disabled until their policy implementation is present.
	flags := map[string]bool{
		"project.v2":                        true,
		"file.v2":                           true,
		"file.streamingDownload":            true,
		"terminal.interactive":              false,
		"terminal.attachLongPoll":           true,
		"terminal.duplexStream":             true,
		"terminal.duplexKeepAlive":          true,
		"tasks.v2":                          false,
		"workflow.v2":                       false,
		"ai.v2":                             true,
		"ai.v3":                             true,
		"ai.v4":                             true,
		"ai.v5":                             true,
		"ai.v6":                             true,
		"ai.v7":                             true,
		"ai.v8":                             true,
		"ai.v9":                             true,
		"ai.v10":                            true,
		"ai.agentLoop":                      true,
		"ai.durableInbox":                   true,
		"ai.planMode":                       true,
		"ai.todo":                           true,
		"ai.questions":                      true,
		"ai.skills":                         true,
		"ai.jobs":                           true,
		"ai.subagents":                      true,
		"ai.goal":                           true,
		"ai.permissionModes":                true,
		"ai.commandSandbox":                 aiCommandSandboxRuntimeAvailable(),
		"ai.generationRecovery":             true,
		"ai.conversationFork":               true,
		"aiEventReplayBytePaging":           true,
		"aiEventReplaySnapshotReset":        true,
		"aiEventDeltaCoalescing":            true,
		"aiGenerationState":                 true,
		"aiChunkedMessageContent":           true,
		"aiConversationCatalogSummary":      true,
		"aiConversationCatalogKeysetPaging": true,
		"aiConversationCatalogChanges":      true,
		"ai.workspaceTools":                 false,
		"ai.taskTools":                      false,
		"ai.persistentTerminal":             false,
		"events.v1":                         true,
		"events.collaboration.v1":           true,
		"taskPayload.v2":                    true,
		"taskLogs.fileSeek":                 true,
		"taskLogs.bulkDownload":             true,
		"remoteProtocolDiagnostics.v2":      true,
		"strictRpcOutboundLimit":            true,
		"project.remoteCreate":              true,
		"project.remoteRoots":               true,
		"project.remoteRemove":              true,
		"agent.environment":                 true,
		"recursiveDelete.confirmed":         false,
	}
	if state != nil && state.business != nil {
		if projects, err := state.business.listProjects(ctx, false); err == nil {
			for _, project := range projects {
				if project.State != "available" {
					continue
				}
				flags["recursiveDelete.confirmed"] = flags["recursiveDelete.confirmed"] || project.Policy.AllowRecursiveDelete
				flags["terminal.interactive"] = flags["terminal.interactive"] || project.Policy.AllowInteractiveTerminal
				flags["tasks.v2"] = flags["tasks.v2"] || project.Policy.AllowTaskExecution
				flags["workflow.v2"] = flags["workflow.v2"] || project.Policy.AllowTaskExecution
				flags["ai.workspaceTools"] = flags["ai.workspaceTools"] || project.Policy.AllowAIWorkspaceTools
				flags["ai.taskTools"] = flags["ai.taskTools"] ||
					project.Policy.AllowAIWorkspaceTools && project.Policy.AllowTaskExecution
			}
		}
	}
	persistentTerminalDisabled := false
	for _, raw := range strings.Split(os.Getenv("WENZWORK_AGENT_FEATURE_FLAGS"), ",") {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		enabled := true
		if strings.HasPrefix(value, "-") {
			enabled, value = false, strings.TrimPrefix(value, "-")
		}
		if _, known := flags[value]; known {
			// A positive environment override must not grant the high-risk PTY
			// feature without an explicit project policy. A negative override is
			// still useful as a rollout kill switch.
			if value != "terminal.interactive" && value != "terminal.attachLongPoll" && value != "terminal.duplexStream" && value != "tasks.v2" && value != "workflow.v2" && value != "ai.workspaceTools" && value != "ai.persistentTerminal" || !enabled {
				flags[value] = enabled
				persistentTerminalDisabled = persistentTerminalDisabled || value == "ai.persistentTerminal" && !enabled
			}
		}
	}
	// Project registration and directory selection are owner-authenticated,
	// device-level operations. They are protocol declarations rather than
	// policy switches and cannot be disabled by per-project or environment
	// permission settings.
	flags["project.remoteCreate"] = true
	flags["project.remoteRoots"] = true
	flags["project.remoteRemove"] = true
	flags["workflow.v2"] = flags["workflow.v2"] && flags["tasks.v2"]
	flags["ai.workspaceTools"] = flags["ai.workspaceTools"] && flags["ai.v2"]
	flags["ai.taskTools"] = flags["ai.taskTools"] && flags["ai.v9"] && flags["tasks.v2"] && flags["ai.workspaceTools"]
	flags["ai.persistentTerminal"] = flags["ai.workspaceTools"] && flags["ai.v5"] && !persistentTerminalDisabled && interactiveTerminalRuntimeAvailable()
	flags["ai.permissionModes"] = flags["ai.permissionModes"] && flags["ai.v2"] && flags["ai.v4"]
	flags["ai.commandSandbox"] = flags["ai.commandSandbox"] && flags["ai.permissionModes"] && aiCommandSandboxRuntimeAvailable()
	flags["ai.planMode"] = flags["ai.planMode"] && flags["ai.v7"]
	flags["ai.todo"] = flags["ai.todo"] && flags["ai.v7"]
	flags["ai.subagents"] = flags["ai.subagents"] && flags["ai.v7"]
	flags["ai.goal"] = flags["ai.goal"] && flags["ai.v8"]
	flags["ai.conversationFork"] = flags["ai.conversationFork"] && flags["ai.v10"]
	flags["terminal.interactive"] = flags["terminal.interactive"] && interactiveTerminalRuntimeAvailable()
	// v3 advertises the strict bounded long-poll contract separately from the
	// project/runtime permission bit. Keeping both flags mandatory lets new
	// clients safely downgrade an older build that also called its PTY surface
	// "terminal.interactive" but returned empty attach snapshots immediately.
	// terminal.attachLongPoll is deliberately left independent from the
	// project authorization bit so clients can distinguish "upgrade required"
	// from "device policy disabled".
	if !flags["terminal.attachLongPoll"] {
		flags["terminal.interactive"] = false
	}
	flags["terminal.duplexStream"] = flags["terminal.duplexStream"] && flags["terminal.interactive"]
	flags["terminal.duplexKeepAlive"] = flags["terminal.duplexKeepAlive"] && flags["terminal.duplexStream"]
	return flags
}

func interactiveTerminalRuntimeAvailable() bool {
	probe, _ := interactiveTerminalRuntimeProbe.Load().(func() bool)
	return probe != nil && probe()
}

var interactiveTerminalRuntimeProbe atomic.Value

func init() {
	interactiveTerminalRuntimeProbe.Store(func() bool {
		if len(availableShells()) == 0 {
			return false
		}
		return platformInteractiveTerminalRuntimeAvailable()
	})
}

func setInteractiveTerminalRuntimeProbe(probe func() bool) func() {
	previous, _ := interactiveTerminalRuntimeProbe.Load().(func() bool)
	if probe == nil {
		probe = func() bool { return false }
	}
	interactiveTerminalRuntimeProbe.Store(probe)
	return func() { interactiveTerminalRuntimeProbe.Store(previous) }
}

func availableShells() []string {
	return availableShellsForOS(runtime.GOOS, exec.LookPath)
}

func availableShellsForOS(operatingSystem string, lookPath func(string) (string, error)) []string {
	if lookPath == nil {
		return nil
	}
	candidates := interactiveShellCandidatesForOS(operatingSystem)
	available := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, err := lookPath(candidate); err == nil {
			available = append(available, candidate)
		}
	}
	return available
}

func interactiveShellCandidatesForOS(operatingSystem string) []string {
	switch operatingSystem {
	case "windows":
		return []string{"pwsh", "powershell", "cmd"}
	case "darwin":
		// Modern macOS uses zsh as its default interactive shell. Starting the
		// system Bash first writes Apple's migration banner into every new PTY,
		// before the user has entered a command.
		return []string{"zsh", "bash", "fish", "sh"}
	default:
		return []string{"bash", "zsh", "fish", "sh"}
	}
}

func agentTaskRunnerCapabilities(state *agentState) []taskRunnerCapability {
	if capabilities := state.taskRunnerCapabilitySnapshot(); len(capabilities) > 0 {
		return capabilities
	}
	return newTaskRunnerRegistry().Capabilities()
}

func availableTaskRunners(capabilities []taskRunnerCapability) []string {
	available := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		if capability.Available {
			available = append(available, capability.Kind)
		}
	}
	slices.Sort(available)
	return available
}

func agentResourceLimits() map[string]uint64 {
	return map[string]uint64{
		"rpcPayloadBytes":                                 maximumRPCPayload,
		"taskRpcPayloadBytes":                             maximumRPCPayload,
		"peerPlaintextBytes":                              maximumPeerRPCPlaintext,
		"rpcJsonBytes":                                    maximumRPCPayload,
		"rpcPreferredPageBytes":                           preferredRPCPagePayload,
		"taskPayloadTotalBytes":                           maximumTaskDefinitionBytes,
		"taskPayloadChunkBytes":                           taskPayloadChunkBytes,
		"taskPayloadQuotaBytes":                           taskPayloadQuotaBytes,
		"taskPayloadTransferCount":                        maximumTaskPayloads,
		"taskPayloadTTLSeconds":                           uint64(taskPayloadTTL.Seconds()),
		"managedFileBytes":                                maximumManagedFileBytes,
		"textReadBytes":                                   maximumTextReadBytes,
		"fileChunkBytes":                                  fileChunkBytes,
		"fileTransferSeconds":                             uint64(fileTransferTTL.Seconds()),
		"fileTransferCount":                               maximumActiveFileTransfers,
		"fileTransferBytes":                               maximumManagedFileBytes,
		"rpcInFlightPerLink":                              v2MaximumRPCsPerLink,
		"rpcInFlightPerController":                        v2MaximumRPCsPerController,
		"rpcInFlightPerDevice":                            v2MaximumRPCsPerDevice,
		"operationInFlight":                               v2MaximumInFlightOperations,
		"operationCacheEntries":                           v2MaximumCachedOperations,
		"operationCacheBytes":                             v2MaximumOperationBytes,
		"operationJournalControllerRows":                  v2OperationMaximumRowsPerController,
		"operationJournalControllerBytes":                 v2OperationMaximumBytesPerController,
		"operationJournalDeviceRows":                      v2OperationMaximumRowsPerDevice,
		"operationJournalDeviceBytes":                     v2OperationMaximumBytesPerDevice,
		"recursiveDeleteItems":                            maximumRecursiveDeleteEntries,
		"terminalSessionCount":                            maximumTerminalSessions,
		"terminalProjectSessionCount":                     maximumTerminalSessionsPerProject,
		"terminalIdleSeconds":                             uint64(terminalIdleTimeout.Seconds()),
		"terminalLifetimeSeconds":                         uint64(terminalMaximumLifetime.Seconds()),
		"terminalDisconnectGraceSeconds":                  uint64(terminalDisconnectGrace.Seconds()),
		"terminalRingBytes":                               maximumTerminalRingBytes,
		"terminalInputChunkBytes":                         maximumTerminalInputBytes,
		"terminalInputBytesPerSecond":                     maximumTerminalInputRateBytes,
		"terminalOutputBytesPerSecond":                    maximumTerminalOutputRateBytes,
		"terminalOutputBytes":                             maximumInteractiveTerminalOutputBytes,
		"terminalMemoryBytes":                             maximumTerminalMemoryBytes,
		"taskDefinitionBytes":                             maximumTaskDefinitionBytes,
		"taskPromptBytes":                                 maximumTaskPromptBytes,
		"taskCommandBytes":                                maximumTaskCommandBytes,
		"taskEnvironmentBytes":                            maximumTaskEnvironmentBytes,
		"taskEnvironmentVariables":                        maximumTaskEnvironmentVariables,
		"agentEnvironmentBytes":                           maximumSecretBytes,
		"agentEnvironmentVariables":                       maximumTaskEnvironmentVariables,
		"taskAttachments":                                 maximumTaskAttachments,
		"taskRelationships":                               maximumTaskRelationships,
		"taskLogEntryBytes":                               maximumTaskLogEntryBytes,
		"taskLogSeekBytes":                                maximumTaskLogSeekBytes,
		"taskRunLogFileBytes":                             maximumTaskRunLogFileBytes,
		"taskLogBytesPerTask":                             maximumTaskLogBytesPerTask,
		"taskLogBytesGlobal":                              maximumTaskLogBytesGlobal,
		"taskLogRetentionTargetBytesPerTask":              maximumTaskLogBytesPerTask,
		"taskLogRetentionTargetBytesGlobal":               maximumTaskLogBytesGlobal,
		"taskLogDiskHardLimitBytes":                       maximumTaskLogDiskBytes,
		"taskLogDiskSafetyReserveBytes":                   minimumTaskLogDiskFreeBytes,
		"taskMaximumLifetimeSeconds":                      uint64(defaultTaskMaximumLifetime.Seconds()),
		"taskMaximumMemoryBytes":                          defaultTaskMaximumMemoryBytes,
		"taskMaximumOutputBytes":                          defaultTaskMaximumOutputBytes,
		"workflowMaximumNodes":                            maximumWorkflowV2Nodes,
		"workflowMaximumEdges":                            maximumWorkflowV2Edges,
		"workflowMaximumParallelism":                      maximumWorkflowV2Parallelism,
		"aiWorkspaceReadBytes":                            uint64(maximumAIWorkspaceReadBytes),
		"aiWorkspaceWriteBytes":                           maximumAIWorkspaceWriteBytes,
		"aiWorkspaceCommandBytes":                         maximumAIWorkspaceCommandBytes,
		"aiWorkspaceCommandOutputBytes":                   maximumAIWorkspaceCommandOutput,
		"aiWorkspaceToolResultBytes":                      maximumAIWorkspaceToolResult,
		"aiWorkspaceRollbackItems":                        maximumAIWorkspaceRollbackItems,
		"aiWebSearchResults":                              aiWebSearchMaximumResults,
		"aiWebFetchURLBytes":                              aiWebFetchMaximumURLLength,
		"aiWebFetchResponseBytes":                         aiWebFetchMaximumBytes,
		"aiWebFetchBodyCharacters":                        aiWebFetchMaximumCharacters,
		"aiWebFetchRedirects":                             aiWebMaximumRedirects,
		"aiTerminalSessionCount":                          maximumAITerminalSessions,
		"aiTerminalOwnerSessionCount":                     maximumAITerminalSessionsPerOwner,
		"aiTerminalInputBytes":                            maximumAITerminalInputBytes,
		"aiTerminalScrollbackBytes":                       maximumAITerminalScrollbackBytes,
		"aiTerminalReadBytes":                             maximumAITerminalReadBytes,
		"aiTerminalLifetimeSeconds":                       uint64(aiTerminalMaximumLifetime.Seconds()),
		"aiApprovalTimeoutSeconds":                        uint64(defaultAIApprovalTimeout.Seconds()),
		"aiDefaultAgentRounds":                            defaultAIAgentRounds,
		"aiDefaultAgentToolCalls":                         defaultAIAgentToolCalls,
		"aiDefaultNoProgressRounds":                       defaultAIAgentNoProgressRounds,
		"aiParallelToolCalls":                             maximumAIParallelToolCalls,
		"aiConcurrentGenerations":                         maximumActiveAIGenerations,
		"aiAgentInboxItems":                               maximumAIAgentInboxItems,
		"aiAgentInboxBytes":                               maximumAIAgentInboxBytes,
		"aiTodoItems":                                     maximumAITodos,
		"aiSubagentDepth":                                 maximumAISubagentDepth,
		"aiSubagentActiveChildren":                        maximumAIActiveChildrenPerAgent,
		"aiEventReplayResponseBytes":                      defaultAIEventResponseBytes,
		"aiEventReplayItemBytes":                          maximumAIPersistentEventPayload,
		"aiPersistentDeltaBytes":                          maximumAIPersistentDeltaBytes,
		"aiMessageContentChunkBytes":                      maximumAIMessageContentChunkBytes,
		"conversationStreamSubscriptionCount":             maximumConversationStreamSubscriptions,
		"conversationStreamConversationSubscriptionCount": maximumConversationStreamSubscriptionsPerConversation,
		"conversationStreamQueueCount":                    maximumConversationStreamQueueCount,
		"conversationStreamQueueBytes":                    maximumConversationStreamQueueBytes,
		"peerTicketMaxBytes":                              16 << 20,
		"peerTicketMaxSeconds":                            uint64(maximumTargetPeerSessionDuration.Seconds()),
		"eventSubscriptionCount":                          maximumAgentEventSubscriptions,
		"eventProjectSubscriptionCount":                   maximumAgentEventSubscriptionsPerProject,
		"eventQueueCount":                                 maximumAgentEventQueueCount,
		"eventQueueBytes":                                 maximumAgentEventQueueBytes,
		"eventReplayCount":                                maximumAgentEventRetention,
		"eventPayloadBytes":                               maximumAgentEventPayloadBytes,
		"eventHeartbeatMinimumSeconds":                    minimumAgentEventHeartbeatSeconds,
		"eventHeartbeatMaximumSeconds":                    maximumAgentEventHeartbeatSeconds,
	}
}
