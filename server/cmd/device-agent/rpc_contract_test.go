package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	remotev1 "github.com/wenzwork/wenzwork-web/server/internal/generated/remote/v1"
	peerv2 "github.com/wenzwork/wenzwork-web/server/internal/peerprotocol/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type rpcV2ContractFixture struct {
	FixtureVersion  uint32 `json:"fixtureVersion"`
	ContractVersion uint32 `json:"contractVersion"`
	ProtocolVersion uint32 `json:"protocolVersion"`
	Limits          struct {
		PeerPlaintextBytes         int `json:"peerPlaintextBytes"`
		RPCJSONBytes               int `json:"rpcJsonBytes"`
		AIEventReplayResponseBytes int `json:"aiEventReplayResponseBytes"`
		RPCPreferredPageBytes      int `json:"rpcPreferredPageBytes"`
	} `json:"limits"`
	ProtocolErrorCodes         []string         `json:"protocolErrorCodes"`
	ProtocolFailureReasons     []string         `json:"protocolFailureReasons"`
	RPCEventKinds              map[string]int32 `json:"rpcEventKinds"`
	CollaborationEventContract struct {
		Version                                  uint32   `json:"version"`
		FeatureFlag                              string   `json:"featureFlag"`
		RequestFields                            []string `json:"requestFields"`
		AcceptedEventKinds                       []string `json:"acceptedEventKinds"`
		LegacyDefaultExcludesCollaborationEvents bool     `json:"legacyDefaultExcludesCollaborationEvents"`
		ReplayUsesNegotiatedKinds                bool     `json:"replayUsesNegotiatedKinds"`
	} `json:"collaborationEventContract"`
	TaskPayloadV2Contract struct {
		Version                   uint32   `json:"version"`
		FeatureFlag               string   `json:"featureFlag"`
		Scope                     string   `json:"scope"`
		Methods                   []string `json:"methods"`
		InlineMaximumBytes        int      `json:"inlineMaximumBytes"`
		TotalMaximumBytes         int      `json:"totalMaximumBytes"`
		ChunkBytes                int      `json:"chunkBytes"`
		QuotaBytes                int64    `json:"quotaBytes"`
		MaximumTransfers          int      `json:"maximumTransfers"`
		TTLSeconds                int      `json:"ttlSeconds"`
		Hash                      string   `json:"hash"`
		SequentialOffsets         bool     `json:"sequentialOffsets"`
		CommitIdempotencyRequired bool     `json:"commitIdempotencyRequired"`
		ReconnectResume           bool     `json:"reconnectResume"`
	} `json:"taskPayloadV2Contract"`
	RequestID         string         `json:"requestId"`
	IdempotencyKey    string         `json:"idempotencyKey"`
	ProjectID         string         `json:"projectId"`
	ExpectedRevision  uint64         `json:"expectedRevision"`
	Deadline          string         `json:"deadline"`
	Method            string         `json:"method"`
	Input             map[string]any `json:"input"`
	ExpectedBase64URL string         `json:"expectedBase64Url"`
	ProjectMismatch   struct {
		Code        int32  `json:"code"`
		SafeMessage string `json:"safeMessage"`
		Retryable   bool   `json:"retryable"`
	} `json:"projectMismatch"`
	CapabilityContract struct {
		Method          string   `json:"method"`
		Scope           string   `json:"scope"`
		ProjectRequired bool     `json:"projectRequired"`
		RequiredFields  []string `json:"requiredFields"`
	} `json:"capabilityContract"`
	RemoteProjectCreate struct {
		Method                string   `json:"method"`
		Scope                 string   `json:"scope"`
		Channel               string   `json:"channel"`
		ProjectRequired       bool     `json:"projectRequired"`
		FeatureVersionKey     string   `json:"featureVersionKey"`
		MinimumFeatureVersion uint32   `json:"minimumFeatureVersion"`
		FeatureFlag           string   `json:"featureFlag"`
		AllowedInputFields    []string `json:"allowedInputFields"`
		ForbiddenInputFields  []string `json:"forbiddenInputFields"`
	} `json:"remoteProjectCreate"`
	RemoteProjectRemove struct {
		Method                   string   `json:"method"`
		Scope                    string   `json:"scope"`
		Channel                  string   `json:"channel"`
		ProjectRequired          bool     `json:"projectRequired"`
		FeatureVersionKey        string   `json:"featureVersionKey"`
		MinimumFeatureVersion    uint32   `json:"minimumFeatureVersion"`
		FeatureFlag              string   `json:"featureFlag"`
		AllowedInputFields       []string `json:"allowedInputFields"`
		ExpectedRevisionRequired bool     `json:"expectedRevisionRequired"`
		SoftDeleteOnly           bool     `json:"softDeleteOnly"`
		BlockingRelations        []string `json:"blockingRelations"`
	} `json:"remoteProjectRemove"`
	FileV2Contract struct {
		FeatureVersionKey       string   `json:"featureVersionKey"`
		MinimumFeatureVersion   uint32   `json:"minimumFeatureVersion"`
		FeatureFlag             string   `json:"featureFlag"`
		ProjectRequired         bool     `json:"projectRequired"`
		ReadScope               string   `json:"readScope"`
		WriteScope              string   `json:"writeScope"`
		ReadMethods             []string `json:"readMethods"`
		WriteMethods            []string `json:"writeMethods"`
		EntryRequiredFields     []string `json:"entryRequiredFields"`
		TextMaximumBytes        uint64   `json:"textMaximumBytes"`
		ManagedFileMaximumBytes uint64   `json:"managedFileMaximumBytes"`
		ChunkBytes              uint64   `json:"chunkBytes"`
		RecursiveDelete         struct {
			PrepareMethod                    string `json:"prepareMethod"`
			CommitMethod                     string `json:"commitMethod"`
			FeatureFlag                      string `json:"featureFlag"`
			ExpectedRevisionRequired         bool   `json:"expectedRevisionRequired"`
			OneTimeConfirmationTokenRequired bool   `json:"oneTimeConfirmationTokenRequired"`
		} `json:"recursiveDelete"`
		LegacyUnboundReadOnlyMethods []string `json:"legacyUnboundReadOnlyMethods"`
	} `json:"fileV2Contract"`
	TerminalV3Contract struct {
		FeatureVersionKey         string   `json:"featureVersionKey"`
		MinimumFeatureVersion     uint32   `json:"minimumFeatureVersion"`
		FeatureFlag               string   `json:"featureFlag"`
		LongPollFeatureFlag       string   `json:"longPollFeatureFlag"`
		DuplexMinimumFeature      uint32   `json:"duplexMinimumFeatureVersion"`
		DuplexFeatureFlag         string   `json:"duplexFeatureFlag"`
		DuplexKeepAliveFeature    string   `json:"duplexKeepAliveFeatureFlag"`
		DuplexKeepAliveSeconds    uint32   `json:"duplexKeepAliveSeconds"`
		DuplexKeepAliveSequence   uint64   `json:"duplexKeepAliveThroughSequence"`
		DuplexKeepAliveCredit     uint32   `json:"duplexKeepAliveCreditBytes"`
		DuplexStreamKind          string   `json:"duplexStreamKind"`
		DuplexFrameType           string   `json:"duplexFrameType"`
		DuplexInputWindowBytes    uint32   `json:"duplexInputWindowBytes"`
		DuplexOutputWindowBytes   uint32   `json:"duplexOutputWindowBytes"`
		DuplexRawBytes            bool     `json:"duplexRawBytes"`
		DuplexCumulativeInputAck  bool     `json:"duplexCumulativeInputAck"`
		DuplexOutputByteCredit    bool     `json:"duplexOutputByteCredit"`
		DuplexResumeFields        []string `json:"duplexResumeFields"`
		ProjectRequired           bool     `json:"projectRequired"`
		Scope                     string   `json:"scope"`
		LegacyMethod              string   `json:"legacyMethod"`
		LegacyScope               string   `json:"legacyScope"`
		Methods                   []string `json:"methods"`
		Events                    []string `json:"events"`
		OutputEncoding            string   `json:"outputEncoding"`
		MonotonicOutputSequence   bool     `json:"monotonicOutputSequence"`
		MonotonicInputSequence    bool     `json:"monotonicInputSequence"`
		ResetRequiredOnEviction   bool     `json:"resetRequiredOnEviction"`
		AttachWaitSeconds         uint64   `json:"attachWaitSeconds"`
		AttachMaximumWaitSeconds  uint64   `json:"attachMaximumWaitSeconds"`
		AttachMaximumPerMinute    int      `json:"attachMaximumPerMinute"`
		AttachBurst               int      `json:"attachBurst"`
		SingleAttachPerSession    bool     `json:"singleAttachPerSession"`
		AttachCompletionReasons   []string `json:"attachCompletionReasons"`
		AttachResponseDiagnostics []string `json:"attachResponseDiagnosticFields"`
		RingBytes                 uint64   `json:"ringBytes"`
		MaximumSessions           int      `json:"maximumSessions"`
		MaximumSessionsPerProject int      `json:"maximumSessionsPerProject"`
		DisconnectGraceSeconds    uint64   `json:"disconnectGraceSeconds"`
		IdleSeconds               uint64   `json:"idleSeconds"`
		LifetimeSeconds           uint64   `json:"lifetimeSeconds"`
		Signals                   []string `json:"signals"`
	} `json:"terminalV3Contract"`
	SequencerLifecycleContract struct {
		TombstoneLimit          int    `json:"tombstoneLimit"`
		MaximumStreamsPerLink   int    `json:"maximumStreamsPerLink"`
		MinimumTombstoneSeconds uint64 `json:"minimumTombstoneSeconds"`
		CloseRejectsLateFrames  bool   `json:"closeRejectsLateFrames"`
		RetireWithKey           bool   `json:"retireWithKey"`
		StreamIDReuseForbidden  bool   `json:"streamIdReuseForbidden"`
		CapacityBehavior        string `json:"capacityBehavior"`
	} `json:"sequencerLifecycleContract"`
	TaskV2Contract struct {
		FeatureVersionKey             string   `json:"featureVersionKey"`
		MinimumFeatureVersion         uint32   `json:"minimumFeatureVersion"`
		FeatureFlag                   string   `json:"featureFlag"`
		ProjectRequired               bool     `json:"projectRequired"`
		Scope                         string   `json:"scope"`
		Methods                       []string `json:"methods"`
		Kinds                         []string `json:"kinds"`
		Statuses                      []string `json:"statuses"`
		DefinitionMaximumBytes        uint64   `json:"definitionMaximumBytes"`
		LogEntryMaximumBytes          uint64   `json:"logEntryMaximumBytes"`
		LogBytesPerTask               uint64   `json:"logBytesPerTask"`
		LogBytesGlobal                uint64   `json:"logBytesGlobal"`
		LogEncodings                  []string `json:"logEncodings"`
		WorkflowFeatureVersionKey     string   `json:"workflowFeatureVersionKey"`
		WorkflowMinimumFeatureVersion uint32   `json:"workflowMinimumFeatureVersion"`
		WorkflowFeatureFlag           string   `json:"workflowFeatureFlag"`
		WorkflowMethods               []string `json:"workflowMethods"`
		WorkflowNodeTypes             []string `json:"workflowNodeTypes"`
		WorkflowEdgeTypes             []string `json:"workflowEdgeTypes"`
		WorkflowFailurePolicies       []string `json:"workflowFailurePolicies"`
		WorkflowNodeStatuses          []string `json:"workflowNodeStatuses"`
		WorkflowMaximumNodes          uint64   `json:"workflowMaximumNodes"`
		WorkflowMaximumEdges          uint64   `json:"workflowMaximumEdges"`
		WorkflowMaximumParallelism    uint64   `json:"workflowMaximumParallelism"`
		CompareAndSwapRequired        bool     `json:"compareAndSwapRequired"`
		ResetRequiredOnEviction       bool     `json:"resetRequiredOnEviction"`
		SensitiveBodyChannel          string   `json:"sensitiveBodyChannel"`
	} `json:"taskV2Contract"`
	TaskLogV1Contract struct {
		FeatureVersionKey       string   `json:"featureVersionKey"`
		MinimumFeatureVersion   uint32   `json:"minimumFeatureVersion"`
		SeekFeatureFlag         string   `json:"seekFeatureFlag"`
		BulkDownloadFeatureFlag string   `json:"bulkDownloadFeatureFlag"`
		ProjectRequired         bool     `json:"projectRequired"`
		LegacyReadScope         string   `json:"legacyReadScope"`
		PeerScope               string   `json:"peerScope"`
		RunProjectionFields     []string `json:"runProjectionFields"`
		Seek                    struct {
			Method                        string   `json:"method"`
			RequestFields                 []string `json:"requestFields"`
			MutuallyExclusiveCursorFields []string `json:"mutuallyExclusiveCursorFields"`
			ResponseFields                []string `json:"responseFields"`
			MaximumBytes                  uint64   `json:"maximumBytes"`
			ResponseMaximumBytes          uint64   `json:"responseMaximumBytes"`
			CursorUnit                    string   `json:"cursorUnit"`
			BoundedRead                   bool     `json:"boundedRead"`
			CompletePhysicalRecords       bool     `json:"completePhysicalRecords"`
		} `json:"seek"`
		Download struct {
			PrepareMethod           string   `json:"prepareMethod"`
			RequestFields           []string `json:"requestFields"`
			ResponseFields          []string `json:"responseFields"`
			SourceKind              string   `json:"sourceKind"`
			StreamKind              string   `json:"streamKind"`
			ChunkBytes              uint64   `json:"chunkBytes"`
			MaximumBytes            uint64   `json:"maximumBytes"`
			Hash                    string   `json:"hash"`
			ImmutablePreparedPrefix bool     `json:"immutablePreparedPrefix"`
			AcknowledgedResume      bool     `json:"acknowledgedResume"`
			PeerSessionBound        bool     `json:"peerSessionBound"`
		} `json:"download"`
		Event struct {
			Type                         string   `json:"type"`
			CursorKind                   string   `json:"cursorKind"`
			DataFields                   []string `json:"dataFields"`
			ContentFree                  bool     `json:"contentFree"`
			InvalidateOnGenerationChange bool     `json:"invalidateOnGenerationChange"`
		} `json:"event"`
		StableErrors          []string `json:"stableErrors"`
		AbsolutePathForbidden bool     `json:"absolutePathForbidden"`
		SQLiteBodyForbidden   bool     `json:"sqliteBodyForbidden"`
		WholeRunRetention     bool     `json:"wholeRunRetention"`
	} `json:"taskLogV1Contract"`
	AIV2Contract struct {
		FeatureVersionKey             string   `json:"featureVersionKey"`
		MinimumFeatureVersion         uint32   `json:"minimumFeatureVersion"`
		FeatureFlag                   string   `json:"featureFlag"`
		PermissionModesMinimumVersion uint32   `json:"permissionModesMinimumFeatureVersion"`
		PermissionModesFeatureFlag    string   `json:"permissionModesFeatureFlag"`
		CommandSandboxFeatureFlag     string   `json:"commandSandboxFeatureFlag"`
		PersistentTerminalFeatureFlag string   `json:"persistentTerminalFeatureFlag"`
		AgentLoopMinimumVersion       uint32   `json:"agentLoopMinimumFeatureVersion"`
		AgentLoopFeatureFlag          string   `json:"agentLoopFeatureFlag"`
		DurableInboxFeatureFlag       string   `json:"durableInboxFeatureFlag"`
		CollaborationMinimumVersion   uint32   `json:"collaborationMinimumFeatureVersion"`
		GoalMinimumVersion            uint32   `json:"goalMinimumFeatureVersion"`
		GoalFeatureFlag               string   `json:"goalFeatureFlag"`
		PlanModeFeatureFlag           string   `json:"planModeFeatureFlag"`
		TodoFeatureFlag               string   `json:"todoFeatureFlag"`
		SubagentsFeatureFlag          string   `json:"subagentsFeatureFlag"`
		LegacyWorkspaceWriteValues    []string `json:"legacyWorkspaceWriteValues"`
		ConfigProjectRequired         bool     `json:"configProjectRequired"`
		ConversationProjectRequired   bool     `json:"conversationProjectRequired"`
		ConfigScope                   string   `json:"configScope"`
		ChatScope                     string   `json:"chatScope"`
		ToolsScope                    string   `json:"toolsScope"`
		WorkspaceToolIntentField      string   `json:"workspaceToolIntentField"`
		WorkspaceToolIntentScope      string   `json:"workspaceToolIntentScope"`
		WorkspaceToolIntentDurable    bool     `json:"workspaceToolIntentDurable"`
		WorkspaceToolIntentMethods    []string `json:"workspaceToolIntentMethods"`
		ConfigMethods                 []string `json:"configMethods"`
		ChatMethods                   []string `json:"chatMethods"`
		ToolOnlyMethods               []string `json:"toolOnlyMethods"`
		InboxDestinations             []string `json:"inboxDestinations"`
		CancelSupportsKeepInbox       bool     `json:"cancelSupportsKeepInbox"`
		Providers                     []string `json:"providers"`
		SecretActions                 []string `json:"secretActions"`
		ConfigRequiredFields          []string `json:"configRequiredFields"`
		ConversationRequiredFields    []string `json:"conversationRequiredFields"`
		MessageRequiredFields         []string `json:"messageRequiredFields"`
		AttachmentReferenceFields     []string `json:"attachmentReferenceFields"`
		MessageStatuses               []string `json:"messageStatuses"`
		StreamEvents                  []string `json:"streamEvents"`
		Tools                         []string `json:"tools"`
		WorkspaceModes                []string `json:"workspaceModes"`
		ApprovalDecisions             []string `json:"approvalDecisions"`
		TodoStatuses                  []string `json:"todoStatuses"`
		SubagentStatuses              []string `json:"subagentStatuses"`
		GoalPhases                    []string `json:"goalPhases"`
		SubagentMaximumDepth          int      `json:"subagentMaximumDepth"`
		SubagentMaximumActiveChildren int      `json:"subagentMaximumActiveChildren"`
		MonotonicEventSequence        bool     `json:"monotonicEventSequence"`
		PersistedEventReplay          bool     `json:"persistedEventReplay"`
		CompareAndSwapRequired        bool     `json:"compareAndSwapRequired"`
		AbsolutePathForbidden         bool     `json:"absolutePathForbidden"`
		SensitiveBodyChannel          string   `json:"sensitiveBodyChannel"`
	} `json:"aiV2Contract"`
	AIV3GenerationRecoveryContract struct {
		FeatureVersionKey     string `json:"featureVersionKey"`
		MinimumFeatureVersion uint32 `json:"minimumFeatureVersion"`
		FeatureFlag           string `json:"featureFlag"`
		Snapshot              struct {
			Method                    string   `json:"method"`
			ProjectRequired           bool     `json:"projectRequired"`
			RequiredFields            []string `json:"requiredFields"`
			LegacyWatermarkField      string   `json:"legacyWatermarkField"`
			ConsistentReadTransaction bool     `json:"consistentReadTransaction"`
		} `json:"snapshot"`
		Attach struct {
			Method                                      string   `json:"method"`
			Scope                                       string   `json:"scope"`
			ProjectRequired                             bool     `json:"projectRequired"`
			RequestFields                               []string `json:"requestFields"`
			ResponseFields                              []string `json:"responseFields"`
			StreamEvents                                []string `json:"streamEvents"`
			Live                                        bool     `json:"live"`
			CacheReplayEvents                           bool     `json:"cacheReplayEvents"`
			DurableAfterRoute                           bool     `json:"durableAfterRoute"`
			Detach                                      string   `json:"detach"`
			ResetRequiredOnCursorExpiredOrQueueOverflow bool     `json:"resetRequiredOnCursorExpiredOrQueueOverflow"`
		} `json:"attach"`
		Regenerate struct {
			Method                      string   `json:"method"`
			Scope                       string   `json:"scope"`
			ProjectRequired             bool     `json:"projectRequired"`
			RequestFields               []string `json:"requestFields"`
			StableRequestIDRequired     bool     `json:"stableRequestIdRequired"`
			GenerationIDEqualsRequestID bool     `json:"generationIdEqualsRequestId"`
		} `json:"regenerate"`
		Limits struct {
			SubscriptionCount             int `json:"subscriptionCount"`
			ConversationSubscriptionCount int `json:"conversationSubscriptionCount"`
			QueueCount                    int `json:"queueCount"`
			QueueBytes                    int `json:"queueBytes"`
		} `json:"limits"`
		ProjectEventHints struct {
			AllowedTypes           []string `json:"allowedTypes"`
			ContentFree            bool     `json:"contentFree"`
			ForbiddenPayloadFields []string `json:"forbiddenPayloadFields"`
		} `json:"projectEventHints"`
	} `json:"aiV3GenerationRecoveryContract"`
	EventV1Contract struct {
		FeatureVersionKey     string   `json:"featureVersionKey"`
		MinimumFeatureVersion uint32   `json:"minimumFeatureVersion"`
		FeatureFlag           string   `json:"featureFlag"`
		Scope                 string   `json:"scope"`
		Method                string   `json:"method"`
		ProjectRequired       bool     `json:"projectRequired"`
		ReadOnlyMethods       []string `json:"readOnlyMethods"`
		RPCEventKinds         struct {
			SubscriptionControl int32 `json:"subscriptionControl"`
			AgentStateChanged   int32 `json:"agentStateChanged"`
		} `json:"rpcEventKinds"`
		Limits struct {
			SubscriptionCount        int    `json:"subscriptionCount"`
			ProjectSubscriptionCount int    `json:"projectSubscriptionCount"`
			QueueCount               int    `json:"queueCount"`
			QueueBytes               int    `json:"queueBytes"`
			ReplayCount              int    `json:"replayCount"`
			PayloadBytes             int    `json:"payloadBytes"`
			HeartbeatMinimumSeconds  uint64 `json:"heartbeatMinimumSeconds"`
			HeartbeatMaximumSeconds  uint64 `json:"heartbeatMaximumSeconds"`
		} `json:"limits"`
		SequenceRules struct {
			ControlSequence                        uint64 `json:"controlSequence"`
			GapRequiresReconcile                   bool   `json:"gapRequiresReconcile"`
			ResetRequiredOnRetentionOrSlowConsumer bool   `json:"resetRequiredOnRetentionOrSlowConsumer"`
			CursorPersistAfterSuccessfulDomainSync bool   `json:"cursorPersistAfterSuccessfulDomainSync"`
		} `json:"sequenceRules"`
		ForbiddenPayloadFields []string `json:"forbiddenPayloadFields"`
		ReplayCache            string   `json:"replayCache"`
		CompletionReasons      []string `json:"completionReasons"`
	} `json:"eventV1Contract"`
	DomainContracts []struct {
		Domain               string `json:"domain"`
		ContractVersion      uint32 `json:"contractVersion"`
		FeatureVersionKey    string `json:"featureVersionKey"`
		RepresentativeMethod string `json:"representativeMethod"`
		Scope                string `json:"scope"`
		Channel              string `json:"channel"`
		ProjectRequired      bool   `json:"projectRequired"`
		FeatureFlag          string `json:"featureFlag"`
	} `json:"domainContracts"`
	CompatibilityContract struct {
		Matrix []struct {
			ClientGeneration        string `json:"clientGeneration"`
			AgentGeneration         string `json:"agentGeneration"`
			ExpectedMode            string `json:"expectedMode"`
			CapabilityQueryRequired bool   `json:"capabilityQueryRequired"`
			NewMethodsAllowed       bool   `json:"newMethodsAllowed"`
			Fallback                string `json:"fallback"`
			MustNotCrash            bool   `json:"mustNotCrash"`
		} `json:"matrix"`
		Metrics struct {
			CapabilityVersions  []string `json:"capabilityVersions"`
			SuccessErrorCode    string   `json:"successErrorCode"`
			RequiredFields      []string `json:"requiredFields"`
			ForbiddenDimensions []string `json:"forbiddenDimensions"`
		} `json:"metrics"`
	} `json:"compatibilityContract"`
}

func loadRPCV2ContractFixture(t *testing.T) rpcV2ContractFixture {
	t.Helper()
	path := filepath.Join("..", "..", "..", "api", "remote", "v1", "fixtures", "rpc_v2_contract.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read RPC v2 fixture: %v", err)
	}
	var fixture rpcV2ContractFixture
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatalf("decode RPC v2 fixture: %v", err)
	}
	if fixture.FixtureVersion != 1 || fixture.ContractVersion != 6 || fixture.ProtocolVersion != 1 {
		t.Fatalf("unsupported RPC v2 fixture: %+v", fixture)
	}
	return fixture
}

func TestCanonicalRemoteContractMatchesRuntimeLimitsEventsAndTaskPayload(t *testing.T) {
	fixture := loadRPCV2ContractFixture(t)
	if fixture.Limits.PeerPlaintextBytes != maximumPeerRPCPlaintext ||
		fixture.Limits.RPCJSONBytes != maximumRPCPayload ||
		fixture.Limits.AIEventReplayResponseBytes != defaultAIEventResponseBytes ||
		fixture.Limits.RPCPreferredPageBytes != preferredRPCPagePayload {
		t.Fatalf("canonical limits = %+v", fixture.Limits)
	}
	if !slices.Contains(fixture.ProtocolErrorCodes, "incompatible_agent") ||
		!slices.Contains(fixture.ProtocolErrorCodes, "protocol_invalid") ||
		!slices.Contains(fixture.ProtocolFailureReasons, "rpc_event_kind_unknown") {
		t.Fatalf("canonical protocol errors are incomplete")
	}
	wantKinds := map[string]int32{
		"chat.text.delta": 1, "chat.completed": 2, "chat.failed": 3,
		"resource.changed": 4, "resource.tombstone": 5, "terminal.output": 6,
		"terminal.exit": 7, "chat.reasoning.delta": 8, "chat.tool.status": 9,
		"chat.approval.requested": 10, "chat.usage": 11, "chat.cancelled": 12,
		"event.subscription.control": 13, "agent.state.changed": 14,
		"chat.goal.changed": 15, "chat.plan_mode.changed": 16, "chat.todo.updated": 17,
		"chat.subagent.started": 18, "chat.subagent.status": 19, "chat.subagent.message": 20,
	}
	if len(fixture.RPCEventKinds) != len(wantKinds) {
		t.Fatalf("canonical event kinds = %#v", fixture.RPCEventKinds)
	}
	for kind, want := range wantKinds {
		if fixture.RPCEventKinds[kind] != want {
			t.Fatalf("canonical event kind %q = %d, want %d", kind, fixture.RPCEventKinds[kind], want)
		}
	}
	collaboration := fixture.CollaborationEventContract
	if collaboration.Version != 1 || collaboration.FeatureFlag != "events.collaboration.v1" ||
		!collaboration.LegacyDefaultExcludesCollaborationEvents || !collaboration.ReplayUsesNegotiatedKinds ||
		len(collaboration.AcceptedEventKinds) != 6 {
		t.Fatalf("collaboration contract = %+v", collaboration)
	}
	taskPayload := fixture.TaskPayloadV2Contract
	if taskPayload.Version != 2 || taskPayload.FeatureFlag != "taskPayload.v2" ||
		taskPayload.Scope != "remote.peer.task.control" || taskPayload.InlineMaximumBytes != maximumRPCPayload ||
		taskPayload.TotalMaximumBytes != maximumTaskDefinitionBytes || taskPayload.ChunkBytes != taskPayloadChunkBytes ||
		taskPayload.QuotaBytes != taskPayloadQuotaBytes || taskPayload.MaximumTransfers != maximumTaskPayloads ||
		taskPayload.TTLSeconds != int(taskPayloadTTL.Seconds()) || taskPayload.Hash != "sha256" ||
		!taskPayload.SequentialOffsets || !taskPayload.CommitIdempotencyRequired || !taskPayload.ReconnectResume {
		t.Fatalf("task payload contract = %+v", taskPayload)
	}
}

func TestRPCV2ContractFixtureMatchesGeneratedProtocol(t *testing.T) {
	fixture := loadRPCV2ContractFixture(t)
	deadline, err := time.Parse(time.RFC3339Nano, fixture.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(fixture.Input)
	if err != nil {
		t.Fatal(err)
	}
	revision := fixture.ExpectedRevision
	envelope := &remotev1.RpcEnvelope{
		ProtocolVersion: fixture.ProtocolVersion,
		Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
			Header: &remotev1.RpcRequestHeader{
				RequestId: fixture.RequestID, IdempotencyKey: fixture.IdempotencyKey,
				ExpectedRevision: &revision, Deadline: timestamppb.New(deadline), ProjectId: fixture.ProjectID,
			},
			Method: fixture.Method, JsonPayload: payload,
		}},
	}
	encoded, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.RawURLEncoding.EncodeToString(encoded); got != fixture.ExpectedBase64URL {
		t.Fatalf("encoded RPC fixture = %s, want %s", got, fixture.ExpectedBase64URL)
	}
}

func TestAgentEventContractFixtureMatchesProtocolAndRuntimeLimits(t *testing.T) {
	contract := loadRPCV2ContractFixture(t).EventV1Contract
	if contract.FeatureVersionKey != "events" || contract.MinimumFeatureVersion != 1 || contract.FeatureFlag != "events.v1" ||
		contract.Scope != "remote.peer.events" || contract.Method != "event.subscribe" || !contract.ProjectRequired ||
		!slices.Equal(contract.ReadOnlyMethods, []string{"event.subscribe", "agent.capabilities.get"}) {
		t.Fatalf("Agent event contract identity = %+v", contract)
	}
	if contract.RPCEventKinds.SubscriptionControl != int32(remotev1.RpcEventKind_RPC_EVENT_KIND_EVENT_SUBSCRIPTION_CONTROL) ||
		contract.RPCEventKinds.AgentStateChanged != int32(remotev1.RpcEventKind_RPC_EVENT_KIND_AGENT_STATE_CHANGED) {
		t.Fatalf("Agent event RPC kinds = %+v", contract.RPCEventKinds)
	}
	if contract.Limits.SubscriptionCount != maximumAgentEventSubscriptions ||
		contract.Limits.ProjectSubscriptionCount != maximumAgentEventSubscriptionsPerProject ||
		contract.Limits.QueueCount != maximumAgentEventQueueCount ||
		contract.Limits.QueueBytes != maximumAgentEventQueueBytes ||
		contract.Limits.ReplayCount != maximumAgentEventRetention ||
		contract.Limits.PayloadBytes != maximumAgentEventPayloadBytes ||
		contract.Limits.HeartbeatMinimumSeconds != minimumAgentEventHeartbeatSeconds ||
		contract.Limits.HeartbeatMaximumSeconds != maximumAgentEventHeartbeatSeconds {
		t.Fatalf("Agent event limits = %+v", contract.Limits)
	}
	if contract.SequenceRules.ControlSequence != 0 || !contract.SequenceRules.GapRequiresReconcile ||
		!contract.SequenceRules.ResetRequiredOnRetentionOrSlowConsumer ||
		!contract.SequenceRules.CursorPersistAfterSuccessfulDomainSync || contract.ReplayCache != "disabled" {
		t.Fatalf("Agent event replay rules = %+v", contract.SequenceRules)
	}
	for _, forbidden := range []string{"prompt", "content", "reasoning", "toolArguments", "toolOutput", "taskDefinition", "command", "logContent", "path", "accessToken", "ticket", "secret"} {
		if !slices.Contains(contract.ForbiddenPayloadFields, forbidden) {
			t.Fatalf("Agent event contract is missing forbidden field %q", forbidden)
		}
	}
	if !slices.Equal(contract.CompletionReasons, []string{"clientCancel", "sessionRenewal", "deadline", "reset", "agentShutdown", "policyRevoked"}) {
		t.Fatalf("Agent event completion reasons = %+v", contract.CompletionReasons)
	}
	if !methodAllowsScope(contract.Method, contract.Scope) || methodAllowsScope("task.list", contract.Scope) {
		t.Fatal("Agent event scope is not isolated to its read-only method set")
	}
}

func TestRPCV2ProjectMismatchUsesStableSafeError(t *testing.T) {
	fixture := loadRPCV2ContractFixture(t)
	deadline, err := time.Parse(time.RFC3339Nano, fixture.Deadline)
	if err != nil {
		t.Fatal(err)
	}
	envelope := &remotev1.RpcEnvelope{
		ProtocolVersion: fixture.ProtocolVersion,
		Message: &remotev1.RpcEnvelope_Request{Request: &remotev1.RpcRequest{
			Header: &remotev1.RpcRequestHeader{
				RequestId: fixture.RequestID, IdempotencyKey: fixture.IdempotencyKey,
				Deadline: timestamppb.New(deadline), ProjectId: uuid.NewString(),
			},
			Method: fixture.Method, JsonPayload: []byte(`{"path":"src"}`),
		}},
	}
	dispatch := dispatcher{
		now: time.Now, scope: "remote.peer.file.receive",
		ticketProjectID: fixture.ProjectID, enforceProjectBinding: true,
	}
	response := dispatch.dispatch(t.Context(), envelope).GetResponse().GetError()
	if response == nil || int32(response.GetCode()) != fixture.ProjectMismatch.Code ||
		response.GetSafeMessage() != fixture.ProjectMismatch.SafeMessage ||
		response.GetRetryable() != fixture.ProjectMismatch.Retryable {
		t.Fatalf("project mismatch error = %+v", response)
	}
}

func TestRPCV2ContractFixtureCoversEveryCapabilityDomain(t *testing.T) {
	fixture := loadRPCV2ContractFixture(t)
	if fixture.CapabilityContract.Method != "agent.capabilities.get" ||
		fixture.CapabilityContract.Scope != "remote.peer.query" || fixture.CapabilityContract.ProjectRequired ||
		!slices.Equal(fixture.CapabilityContract.RequiredFields, []string{
			"agentBuildId", "connectionEpoch", "capabilityVersion", "protocol.minimum", "protocol.maximum",
			"featureVersions", "features", "platform.os", "platform.arch", "shells", "taskRunners", "resourceLimits", "taskLogMetrics", "remoteV2Resources", "remoteOperationJournal",
		}) {
		t.Fatalf("capability contract = %+v", fixture.CapabilityContract)
	}
	create := fixture.RemoteProjectCreate
	if create.Method != "project.create" || create.Scope != "remote.peer.query" ||
		create.Channel != "peer-rpc" || create.ProjectRequired || create.FeatureVersionKey != "projects" ||
		create.MinimumFeatureVersion != 2 || create.FeatureFlag != "project.remoteCreate" ||
		len(create.AllowedInputFields) != 5 || create.AllowedInputFields[0] != "name" ||
		create.AllowedInputFields[1] != "displayName" || create.AllowedInputFields[2] != "gitUrl" ||
		create.AllowedInputFields[3] != "directoryId" || create.AllowedInputFields[4] != "parentDirectoryId" ||
		len(create.ForbiddenInputFields) != 3 || create.ForbiddenInputFields[0] != "path" ||
		create.ForbiddenInputFields[1] != "projectId" || create.ForbiddenInputFields[2] != "relativePath" ||
		methodScope(create.Method) != create.Scope {
		t.Fatalf("remote project-create contract = %+v", create)
	}
	remove := fixture.RemoteProjectRemove
	if remove.Method != "project.remove" || remove.Scope != "remote.peer.query" ||
		remove.Channel != "peer-rpc" || remove.ProjectRequired || remove.FeatureVersionKey != "projects" ||
		remove.MinimumFeatureVersion != 3 || remove.FeatureFlag != "project.remoteRemove" ||
		!slices.Equal(remove.AllowedInputFields, []string{"projectId", "expectedRevision"}) ||
		!remove.ExpectedRevisionRequired || !remove.SoftDeleteOnly ||
		!slices.Equal(remove.BlockingRelations, []string{"ai_conversations.generating", "tasks.non_terminal"}) ||
		methodScope(remove.Method) != remove.Scope {
		t.Fatalf("remote project-remove contract = %+v", remove)
	}
	file := fixture.FileV2Contract
	if file.FeatureVersionKey != "files" || file.MinimumFeatureVersion != 2 || file.FeatureFlag != "file.v2" ||
		!file.ProjectRequired || file.ReadScope != "remote.peer.file.receive" || file.WriteScope != "remote.peer.file.send" ||
		file.TextMaximumBytes != maximumTextReadBytes || file.ManagedFileMaximumBytes != 0 ||
		file.ChunkBytes != fileChunkBytes || len(file.EntryRequiredFields) != 11 ||
		file.RecursiveDelete.PrepareMethod != "file.delete.prepare" || file.RecursiveDelete.CommitMethod != "file.delete" ||
		file.RecursiveDelete.FeatureFlag != "recursiveDelete.confirmed" || !file.RecursiveDelete.ExpectedRevisionRequired ||
		!file.RecursiveDelete.OneTimeConfirmationTokenRequired {
		t.Fatalf("File v2 contract = %+v", file)
	}
	for _, method := range file.ReadMethods {
		if got := methodScope(method); got != file.ReadScope {
			t.Errorf("File v2 read scope for %q = %q", method, got)
		}
	}
	for _, method := range file.WriteMethods {
		if got := methodScope(method); got != file.WriteScope {
			t.Errorf("File v2 write scope for %q = %q", method, got)
		}
	}
	for _, method := range file.LegacyUnboundReadOnlyMethods {
		if !legacyReadOnlyFileMethod(method) || methodScope(method) != file.ReadScope {
			t.Errorf("legacy File v1 method %q is not read-only", method)
		}
	}
	terminal := fixture.TerminalV3Contract
	if terminal.FeatureVersionKey != "terminal" || terminal.MinimumFeatureVersion != 3 ||
		terminal.FeatureFlag != "terminal.interactive" || terminal.LongPollFeatureFlag != "terminal.attachLongPoll" || !terminal.ProjectRequired ||
		terminal.DuplexMinimumFeature != 4 || terminal.DuplexFeatureFlag != "terminal.duplexStream" ||
		terminal.DuplexKeepAliveFeature != "terminal.duplexKeepAlive" || terminal.DuplexKeepAliveSeconds != uint32(terminalDuplexKeepAliveInterval.Seconds()) ||
		terminal.DuplexKeepAliveSequence != 0 || terminal.DuplexKeepAliveCredit != 0 ||
		terminal.DuplexStreamKind != "terminal" || terminal.DuplexFrameType != "streamData" ||
		terminal.DuplexInputWindowBytes != maximumTerminalInputBytes || terminal.DuplexOutputWindowBytes != v2TerminalMaximumOutputCredit ||
		!terminal.DuplexRawBytes || !terminal.DuplexCumulativeInputAck || !terminal.DuplexOutputByteCredit ||
		!slices.Equal(terminal.DuplexResumeFields, []string{"afterOutputSequence", "afterInputSequence", "afterResizeSequence"}) ||
		terminal.Scope != "remote.peer.terminal.interactive" || terminal.LegacyMethod != "terminal.execute" ||
		terminal.LegacyScope != "remote.peer.terminal" || terminal.OutputEncoding != "base64url" ||
		!terminal.MonotonicOutputSequence || !terminal.MonotonicInputSequence || !terminal.ResetRequiredOnEviction ||
		terminal.RingBytes != maximumTerminalRingBytes || terminal.MaximumSessions != maximumTerminalSessions ||
		terminal.MaximumSessionsPerProject != maximumTerminalSessionsPerProject ||
		terminal.DisconnectGraceSeconds != uint64(terminalDisconnectGrace.Seconds()) ||
		terminal.IdleSeconds != uint64(terminalIdleTimeout.Seconds()) ||
		terminal.LifetimeSeconds != uint64(terminalMaximumLifetime.Seconds()) || len(terminal.Events) != 2 ||
		terminal.Events[0] != "terminal.output" || terminal.Events[1] != "terminal.exit" || len(terminal.Signals) != 3 ||
		terminal.AttachWaitSeconds != uint64(terminalDefaultAttachWait.Seconds()) || terminal.AttachMaximumWaitSeconds != uint64(terminalMaximumAttachWait.Seconds()) ||
		terminal.AttachMaximumPerMinute != terminalAttachTokensPerMinute || terminal.AttachBurst != terminalAttachTokenBurst || !terminal.SingleAttachPerSession ||
		!slices.Equal(terminal.AttachCompletionReasons, []string{"events", "timeout", "exit", "reset"}) ||
		!slices.Equal(terminal.AttachResponseDiagnostics, []string{"completionReason", "heldMilliseconds", "eventCount"}) {
		t.Fatalf("Terminal v2 contract = %+v", terminal)
	}
	sequencer := fixture.SequencerLifecycleContract
	if sequencer.TombstoneLimit != peerv2.DefaultSequencerTombstoneLimit || sequencer.MaximumStreamsPerLink != peerv2.DefaultSequencerStreamLimit ||
		sequencer.MinimumTombstoneSeconds != uint64(v2LinkRecoveryTTL.Seconds()) ||
		!sequencer.CloseRejectsLateFrames || !sequencer.RetireWithKey || !sequencer.StreamIDReuseForbidden || sequencer.CapacityBehavior != "rekey-or-close-link" {
		t.Fatalf("Sequencer lifecycle contract = %+v", sequencer)
	}
	if methodScope(terminal.LegacyMethod) != terminal.LegacyScope {
		t.Errorf("legacy terminal scope = %q", methodScope(terminal.LegacyMethod))
	}
	for _, method := range terminal.Methods {
		if got := methodScope(method); got != terminal.Scope {
			t.Errorf("Terminal v2 scope for %q = %q", method, got)
		}
	}
	task := fixture.TaskV2Contract
	if task.FeatureVersionKey != "tasks" || task.MinimumFeatureVersion != 2 || task.FeatureFlag != "tasks.v2" ||
		!task.ProjectRequired || task.Scope != "remote.peer.task.control" || task.SensitiveBodyChannel != "peer-rpc" ||
		!task.CompareAndSwapRequired || !task.ResetRequiredOnEviction ||
		task.DefinitionMaximumBytes != maximumTaskDefinitionBytes || task.LogEntryMaximumBytes != maximumTaskLogEntryBytes ||
		task.LogBytesPerTask != maximumTaskLogBytesPerTask || task.LogBytesGlobal != maximumTaskLogBytesGlobal ||
		!slices.Equal(task.Kinds, taskKinds) || !slices.Equal(task.Statuses, taskStatuses) ||
		!slices.Equal(task.LogEncodings, []string{"utf-8", "base64"}) || len(task.Methods) != 18 ||
		task.WorkflowFeatureVersionKey != "workflows" || task.WorkflowMinimumFeatureVersion != 2 || task.WorkflowFeatureFlag != "workflow.v2" ||
		!slices.Equal(task.WorkflowNodeTypes, workflowV2NodeTypes) || !slices.Equal(task.WorkflowEdgeTypes, workflowV2EdgeTypes) ||
		!slices.Equal(task.WorkflowFailurePolicies, workflowV2FailurePolicies) || !slices.Equal(task.WorkflowNodeStatuses, workflowV2NodeStatuses) ||
		task.WorkflowMaximumNodes != maximumWorkflowV2Nodes || task.WorkflowMaximumEdges != maximumWorkflowV2Edges ||
		task.WorkflowMaximumParallelism != maximumWorkflowV2Parallelism || len(task.WorkflowMethods) != 8 {
		t.Fatalf("Task v2 contract = %+v", task)
	}
	for _, method := range task.Methods {
		if !methodAllowsScope(method, task.Scope) {
			t.Errorf("Task v2 scope for %q does not allow %q", method, task.Scope)
		}
	}
	for _, method := range task.WorkflowMethods {
		if methodScope(method) != task.Scope || !methodAllowsScope(method, task.Scope) {
			t.Errorf("Workflow v2 scope for %q does not allow %q", method, task.Scope)
		}
	}
	taskLog := fixture.TaskLogV1Contract
	if taskLog.FeatureVersionKey != "taskLogs" || taskLog.MinimumFeatureVersion != 1 ||
		taskLog.SeekFeatureFlag != "taskLogs.fileSeek" || taskLog.BulkDownloadFeatureFlag != "taskLogs.bulkDownload" ||
		!taskLog.ProjectRequired || taskLog.LegacyReadScope != "remote.task.read" || taskLog.PeerScope != "remote.peer.task.control" ||
		!slices.Equal(taskLog.RunProjectionFields, []string{"logAvailable", "logState", "logGeneration", "logFormatVersion", "logSizeBytes"}) ||
		taskLog.Seek.Method != "task.logs" || taskLog.Seek.MaximumBytes != maximumTaskLogSeekBytes ||
		taskLog.Seek.ResponseMaximumBytes != preferredRPCPagePayload || taskLog.Seek.CursorUnit != "utf8-file-bytes" ||
		!taskLog.Seek.BoundedRead || !taskLog.Seek.CompletePhysicalRecords ||
		!slices.Equal(taskLog.Seek.MutuallyExclusiveCursorFields, []string{"offset", "tailBytes", "beforeOffset"}) ||
		taskLog.Download.PrepareMethod != "task.logs.download.prepare" || taskLog.Download.SourceKind != downloadSourceTaskLog ||
		taskLog.Download.StreamKind != "file" || taskLog.Download.ChunkBytes != fileChunkBytes ||
		taskLog.Download.MaximumBytes != maximumTaskRunLogFileBytes || taskLog.Download.Hash != "sha256" ||
		!taskLog.Download.ImmutablePreparedPrefix || !taskLog.Download.AcknowledgedResume || !taskLog.Download.PeerSessionBound ||
		taskLog.Event.Type != "task.logs.available" || taskLog.Event.CursorKind != "task_log_bytes" ||
		!slices.Equal(taskLog.Event.DataFields, []string{"runId", "generation", "highWatermark"}) ||
		!taskLog.Event.ContentFree || !taskLog.Event.InvalidateOnGenerationChange ||
		!taskLog.AbsolutePathForbidden || !taskLog.SQLiteBodyForbidden || !taskLog.WholeRunRetention ||
		!slices.Equal(taskLog.StableErrors, []string{"INVALID_ARGUMENT", "NOT_FOUND", "PROJECT_MISMATCH", "LOG_EXPIRED", "LOG_MIGRATING", "LOG_CORRUPT", "UPGRADE_REQUIRED"}) {
		t.Fatalf("Task log v1 contract = %+v", taskLog)
	}
	if methodScope(taskLog.Seek.Method) != taskLog.LegacyReadScope ||
		methodScope(taskLog.Download.PrepareMethod) != taskLog.LegacyReadScope ||
		!methodAllowsScope(taskLog.Seek.Method, taskLog.PeerScope) ||
		!methodAllowsScope(taskLog.Download.PrepareMethod, taskLog.PeerScope) {
		t.Fatalf("Task log v1 scope contract = %+v", taskLog)
	}
	ai := fixture.AIV2Contract
	if ai.FeatureVersionKey != "ai" || ai.MinimumFeatureVersion != 2 || ai.FeatureFlag != "ai.v2" ||
		ai.PermissionModesMinimumVersion != 4 || ai.PermissionModesFeatureFlag != "ai.permissionModes" ||
		ai.CommandSandboxFeatureFlag != "ai.commandSandbox" || ai.PersistentTerminalFeatureFlag != "ai.persistentTerminal" ||
		ai.AgentLoopMinimumVersion != 6 || ai.AgentLoopFeatureFlag != "ai.agentLoop" || ai.DurableInboxFeatureFlag != "ai.durableInbox" ||
		ai.CollaborationMinimumVersion != 7 || ai.GoalMinimumVersion != 8 || ai.GoalFeatureFlag != "ai.goal" || ai.PlanModeFeatureFlag != "ai.planMode" || ai.TodoFeatureFlag != "ai.todo" || ai.SubagentsFeatureFlag != "ai.subagents" ||
		!ai.CancelSupportsKeepInbox || !slices.Equal(ai.InboxDestinations, []string{"nextTurn", "nextStep"}) || !slices.Equal(ai.LegacyWorkspaceWriteValues, []string{"edit"}) ||
		ai.ConfigProjectRequired || !ai.ConversationProjectRequired || ai.ConfigScope != "remote.peer.ai.config" ||
		ai.ChatScope != "remote.peer.ai.chat" || ai.ToolsScope != "remote.peer.ai.tools" ||
		ai.WorkspaceToolIntentField != "enableWorkspaceTools" || ai.WorkspaceToolIntentScope != ai.ToolsScope || !ai.WorkspaceToolIntentDurable ||
		!slices.Equal(ai.WorkspaceToolIntentMethods, []string{"conversation.send", "conversation.regenerate", "conversation.goal.create", "conversation.goal.edit", "conversation.goal.pause", "conversation.goal.resume", "conversation.goal.clear", "conversation.subagent.message"}) ||
		ai.SensitiveBodyChannel != "peer-rpc" || !ai.MonotonicEventSequence || !ai.PersistedEventReplay ||
		!ai.CompareAndSwapRequired || !ai.AbsolutePathForbidden || len(ai.ConfigMethods) != 7 ||
		len(ai.ChatMethods) != 25 || len(ai.ToolOnlyMethods) != 2 || len(ai.ConfigRequiredFields) != 21 ||
		len(ai.ConversationRequiredFields) != 14 || len(ai.MessageRequiredFields) != 13 ||
		len(ai.AttachmentReferenceFields) != 7 ||
		!slices.Equal(ai.Providers, []string{"openai", "anthropic", "google", "deepseek", "ollama", "openai-compatible"}) ||
		!slices.Equal(ai.SecretActions, []string{"keep", "replace", "clear"}) ||
		!slices.Equal(ai.MessageStatuses, []string{"complete", "streaming", "stopped", "failed"}) ||
		!slices.Equal(ai.WorkspaceModes, []string{"readOnly", "workspaceWrite", "fullAccess"}) ||
		!slices.Equal(ai.ApprovalDecisions, []string{"deny", "allowOnce", "allowForSession"}) ||
		!slices.Equal(ai.Tools, []string{"list_files", "search_files", "read_file", "read_tool_result", "read_image", "web_search", "web_fetch", "terminal_open", "terminal_send", "terminal_read", "terminal_signal", "terminal_close", "terminal_list", "write_file", "replace_in_file", "rollback_file_change", "run_command", "get_goal", "create_goal", "update_goal", "todo_write", "exit_plan_mode", "ask_user_question", "skill", "spawn_agent", "subagent_fork", "list_agents", "send_message", "interrupt_agent", "job_list", "job_output", "job_kill"}) ||
		!slices.Equal(ai.StreamEvents, []string{"chat.text.delta", "chat.reasoning.delta", "chat.tool.status", "chat.approval.requested", "chat.usage", "chat.completed", "chat.failed", "chat.cancelled", "chat.goal.changed", "chat.plan_mode.changed", "chat.todo.updated", "chat.subagent.started", "chat.subagent.status", "chat.subagent.message"}) ||
		!slices.Equal(ai.TodoStatuses, []string{"pending", "in_progress", "completed"}) ||
		!slices.Equal(ai.SubagentStatuses, []string{"running", "ready", "completed", "failed", "interrupted"}) ||
		!slices.Equal(ai.GoalPhases, []string{"active", "paused", "blocked", "complete"}) ||
		ai.SubagentMaximumDepth != maximumAISubagentDepth || ai.SubagentMaximumActiveChildren != maximumAIActiveChildrenPerAgent {
		t.Fatalf("AI v2 contract = %+v", ai)
	}
	for _, method := range ai.ConfigMethods {
		if methodScope(method) != ai.ConfigScope || !methodAllowsScope(method, ai.ConfigScope) {
			t.Errorf("AI config scope for %q does not allow %q", method, ai.ConfigScope)
		}
	}
	for _, method := range ai.ChatMethods {
		if methodScope(method) != ai.ChatScope || !methodAllowsScope(method, ai.ChatScope) || !methodAllowsScope(method, ai.ToolsScope) {
			t.Errorf("AI chat method %q is not allowed by chat and tools scopes", method)
		}
	}
	for _, method := range ai.ToolOnlyMethods {
		if methodScope(method) != ai.ToolsScope || !methodAllowsScope(method, ai.ToolsScope) || methodAllowsScope(method, ai.ChatScope) {
			t.Errorf("AI tool-only method %q has an invalid scope mapping", method)
		}
	}
	recovery := fixture.AIV3GenerationRecoveryContract
	if recovery.FeatureVersionKey != "ai" || recovery.MinimumFeatureVersion != 3 || recovery.FeatureFlag != "ai.generationRecovery" ||
		recovery.Snapshot.Method != "conversation.get" || !recovery.Snapshot.ProjectRequired ||
		!recovery.Snapshot.ConsistentReadTransaction || recovery.Snapshot.LegacyWatermarkField != "eventHighWatermark" ||
		!slices.Equal(recovery.Snapshot.RequiredFields, []string{"conversation", "messages", "snapshotEventHighWatermark", "earliestAvailableEventSequence"}) ||
		recovery.Attach.Method != "conversation.generation.attach" || recovery.Attach.Scope != "remote.peer.ai.chat" || !recovery.Attach.ProjectRequired ||
		!slices.Equal(recovery.Attach.RequestFields, []string{"conversationId", "generationId", "afterSequence"}) ||
		len(recovery.Attach.ResponseFields) != 8 || !recovery.Attach.Live || recovery.Attach.CacheReplayEvents || recovery.Attach.DurableAfterRoute ||
		recovery.Attach.Detach != "peer-cancel-query-only" || !recovery.Attach.ResetRequiredOnCursorExpiredOrQueueOverflow ||
		recovery.Regenerate.Method != "conversation.regenerate" || recovery.Regenerate.Scope != "remote.peer.ai.chat" ||
		!recovery.Regenerate.ProjectRequired ||
		!slices.Equal(recovery.Regenerate.RequestFields, []string{"conversationId", "messageId", "regenerationRequestId", "workspaceMode", "enableWorkspaceTools"}) ||
		!recovery.Regenerate.StableRequestIDRequired || !recovery.Regenerate.GenerationIDEqualsRequestID ||
		recovery.Limits.SubscriptionCount != maximumConversationStreamSubscriptions ||
		recovery.Limits.ConversationSubscriptionCount != maximumConversationStreamSubscriptionsPerConversation ||
		recovery.Limits.QueueCount != maximumConversationStreamQueueCount || recovery.Limits.QueueBytes != maximumConversationStreamQueueBytes ||
		!slices.Equal(recovery.ProjectEventHints.AllowedTypes, []string{"conversation.changed", "conversation.events.available"}) ||
		!recovery.ProjectEventHints.ContentFree {
		t.Fatalf("AI v3 generation recovery contract = %+v", recovery)
	}
	for _, forbidden := range []string{"prompt", "content", "delta", "reasoning", "toolArguments", "toolOutput", "attachments"} {
		if !slices.Contains(recovery.ProjectEventHints.ForbiddenPayloadFields, forbidden) {
			t.Fatalf("AI v3 event hints are missing forbidden field %q", forbidden)
		}
	}
	traits := peerRPCMethodTraitsFor(recovery.Attach.Method)
	if methodScope(recovery.Attach.Method) != recovery.Attach.Scope || !methodAllowsScope(recovery.Attach.Method, recovery.Attach.Scope) ||
		!traits.live || traits.cacheReplayEvents || traits.durableAfterRoute {
		t.Fatalf("AI v3 attach scope/traits = %+v", traits)
	}
	wantProjectBinding := map[string]bool{
		"project":  false,
		"file":     true,
		"terminal": true,
		"task":     true,
		"ai":       true,
	}
	seen := make(map[string]bool, len(fixture.DomainContracts))
	for _, contract := range fixture.DomainContracts {
		wantBinding, ok := wantProjectBinding[contract.Domain]
		if !ok || seen[contract.Domain] {
			t.Fatalf("unexpected or duplicate domain contract %+v", contract)
		}
		seen[contract.Domain] = true
		if contract.ContractVersion == 0 || contract.FeatureVersionKey == "" ||
			contract.RepresentativeMethod == "" || contract.Scope == "" ||
			(contract.Channel != "peer-rpc" && contract.Channel != "device-control") ||
			contract.ProjectRequired != wantBinding {
			t.Fatalf("incomplete domain contract %+v", contract)
		}
		if got := methodScope(contract.RepresentativeMethod); got != contract.Scope {
			t.Errorf("methodScope(%q) = %q, fixture wants %q", contract.RepresentativeMethod, got, contract.Scope)
		}
	}
	if len(seen) != len(wantProjectBinding) {
		t.Fatalf("domain contracts = %#v", seen)
	}
	compatibility := fixture.CompatibilityContract
	wantMatrix := map[string]struct {
		mode, fallback              string
		capabilityQuery, newMethods bool
	}{
		"v1/v1": {mode: "legacy-only", fallback: "none"},
		"v1/v2": {mode: "legacy-on-v2-agent", fallback: "v1-retained"},
		"v2/v1": {mode: "safe-degrade", fallback: "legacy-authorized-only", capabilityQuery: true},
		"v2/v2": {mode: "capability-gated-v2", fallback: "per-capability", capabilityQuery: true, newMethods: true},
	}
	seenMatrix := make(map[string]bool, len(compatibility.Matrix))
	for _, combination := range compatibility.Matrix {
		key := combination.ClientGeneration + "/" + combination.AgentGeneration
		want, found := wantMatrix[key]
		if !found || seenMatrix[key] || !combination.MustNotCrash || combination.ExpectedMode != want.mode ||
			combination.Fallback != want.fallback || combination.CapabilityQueryRequired != want.capabilityQuery ||
			combination.NewMethodsAllowed != want.newMethods {
			t.Fatalf("invalid compatibility combination %+v", combination)
		}
		seenMatrix[key] = true
	}
	if len(seenMatrix) != len(wantMatrix) {
		t.Fatalf("compatibility matrix = %#v", seenMatrix)
	}
	metrics := compatibility.Metrics
	if metrics.SuccessErrorCode != "ok" ||
		!validCompatibilityMetricErrorCode(metrics.SuccessErrorCode) ||
		!slices.Equal(metrics.RequiredFields, []string{"capabilityVersion", "errorCode", "callCount", "totalDurationMilliseconds"}) ||
		len(metrics.ForbiddenDimensions) != 9 {
		t.Fatalf("compatibility metric contract = %+v", metrics)
	}
	seenVersions := make(map[string]bool, len(metrics.CapabilityVersions))
	for _, version := range metrics.CapabilityVersions {
		if seenVersions[version] || !validCompatibilityCapabilityVersion(version) {
			t.Fatalf("invalid compatibility metric version %q", version)
		}
		seenVersions[version] = true
	}
	if len(seenVersions) != 8 {
		t.Fatalf("compatibility metric versions = %#v", seenVersions)
	}
}
