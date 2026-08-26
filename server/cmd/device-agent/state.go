package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	agentStateVersion          = 1
	maximumActiveAIGenerations = 8
)

const defaultAISystemPrompt = "你是 WenzMark 编辑器内的 AI 助手。请基于用户提供的 Markdown 文档、选中文本和附件准确回答；信息不足时明确说明，不要编造文档中不存在的内容。"

type aiConfig struct {
	ID                     string            `json:"id"`
	Name                   string            `json:"name"`
	Provider               string            `json:"provider"`
	BaseURL                string            `json:"baseUrl"`
	NonSecretHeaders       map[string]string `json:"nonSecretHeaders,omitempty"`
	Model                  string            `json:"model"`
	SystemPrompt           string            `json:"systemPrompt,omitempty"`
	Temperature            float64           `json:"temperature,omitempty"`
	ReasoningEffort        string            `json:"reasoningEffort,omitempty"`
	MaxTurnOutputTokens    uint32            `json:"maxTurnOutputTokens,omitempty"`
	MaxActiveContextTokens uint32            `json:"maxActiveContextTokens,omitempty"`
	// Legacy loop-count fields remain readable on disk and wire for rolling
	// upgrades. The agent loop no longer treats them as terminal budgets.
	MaxAgentRounds             uint32 `json:"maxAgentRounds,omitempty"`
	MaxAgentToolCalls          uint32 `json:"maxAgentToolCalls,omitempty"`
	MaxAgentNoProgressRounds   uint32 `json:"maxAgentNoProgressRounds,omitempty"`
	RequestTimeoutSeconds      uint32 `json:"requestTimeoutSeconds,omitempty"`
	MaxRetries                 uint32 `json:"maxRetries,omitempty"`
	RetryBaseDelayMilliseconds uint32 `json:"retryBaseDelayMilliseconds,omitempty"`
	ShowUsage                  bool   `json:"showUsage,omitempty"`
	Enabled                    bool   `json:"enabled"`
	CredentialConfigured       bool   `json:"secretConfigured,omitempty"`
	Revision                   uint64 `json:"revision"`

	// Credential is populated from SecretStore and is never serialized.
	// LegacyCredential exists only so version-1 state can be migrated; it is
	// cleared before any newly encoded state is installed.
	Credential       string `json:"-"`
	LegacyCredential string `json:"credential,omitempty"`
}

type aiConfigView struct {
	ID                         string            `json:"id"`
	Revision                   uint64            `json:"revision"`
	Name                       string            `json:"name"`
	Provider                   string            `json:"provider"`
	BaseURL                    string            `json:"baseUrl"`
	NonSecretHeaders           map[string]string `json:"nonSecretHeaders"`
	Model                      string            `json:"model"`
	SystemPrompt               string            `json:"systemPrompt"`
	Temperature                float64           `json:"temperature"`
	ReasoningEffort            string            `json:"reasoningEffort"`
	MaxTurnOutputTokens        uint32            `json:"maxTurnOutputTokens"`
	MaxActiveContextTokens     uint32            `json:"maxActiveContextTokens"`
	MaxAgentRounds             uint32            `json:"maxAgentRounds"`
	MaxAgentToolCalls          uint32            `json:"maxAgentToolCalls"`
	MaxAgentNoProgressRounds   uint32            `json:"maxAgentNoProgressRounds"`
	RequestTimeoutSeconds      uint32            `json:"requestTimeoutSeconds"`
	MaxRetries                 uint32            `json:"maxRetries"`
	RetryBaseDelayMilliseconds uint32            `json:"retryBaseDelayMilliseconds"`
	ShowUsage                  bool              `json:"showUsage"`
	SecretConfigured           bool              `json:"secretConfigured"`
	Enabled                    bool              `json:"enabled"`
}

func (config aiConfig) view() aiConfigView {
	return aiConfigView{
		ID: config.ID, Revision: config.Revision, Name: config.Name, Provider: config.Provider,
		BaseURL: config.BaseURL, NonSecretHeaders: cloneStringMap(config.NonSecretHeaders), Model: config.Model,
		SystemPrompt: config.SystemPrompt, Temperature: config.Temperature, ReasoningEffort: config.ReasoningEffort,
		MaxTurnOutputTokens: config.MaxTurnOutputTokens, MaxActiveContextTokens: config.MaxActiveContextTokens,
		MaxAgentRounds: config.MaxAgentRounds, MaxAgentToolCalls: config.MaxAgentToolCalls,
		MaxAgentNoProgressRounds: config.MaxAgentNoProgressRounds, RequestTimeoutSeconds: config.RequestTimeoutSeconds,
		MaxRetries: config.MaxRetries, RetryBaseDelayMilliseconds: config.RetryBaseDelayMilliseconds, ShowUsage: config.ShowUsage,
		SecretConfigured: config.CredentialConfigured || config.Credential != "", Enabled: config.Enabled,
	}
}

func defaultAIConfig() aiConfigView {
	return defaultAIConfigSettings("default").view()
}

type chatMessage struct {
	ID           string                    `json:"id"`
	Revision     uint64                    `json:"revision"`
	Sequence     uint64                    `json:"sequence"`
	Role         string                    `json:"role"`
	Content      string                    `json:"content"`
	Status       string                    `json:"status"`
	ErrorCode    string                    `json:"errorCode"`
	Attachments  []chatAttachmentReference `json:"attachments"`
	Reasoning    string                    `json:"reasoning"`
	ToolRuns     []chatToolRun             `json:"toolRuns"`
	Usage        chatUsage                 `json:"usage"`
	ProviderRun  chatProviderRun           `json:"providerRun"`
	CreatedAt    time.Time                 `json:"createdAt"`
	GenerationID string                    `json:"generationId,omitempty"`
}

type chatAttachmentReference struct {
	ID           string `json:"id"`
	RelativePath string `json:"relativePath"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         uint64 `json:"size"`
	SHA256       string `json:"sha256"`
	Revision     uint64 `json:"revision"`
}

type chatToolRun struct {
	ID            string     `json:"id"`
	Tool          string     `json:"tool"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Status        string     `json:"status"`
	Arguments     any        `json:"arguments"`
	Result        any        `json:"result"`
	Output        string     `json:"output,omitempty"`
	ErrorCode     string     `json:"errorCode"`
	View          any        `json:"view,omitempty"`
	ContentOffset *int       `json:"contentOffset,omitempty"`
	StartedAt     time.Time  `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

type chatUsage struct {
	InputTokens       uint64 `json:"inputTokens"`
	OutputTokens      uint64 `json:"outputTokens"`
	ReasoningTokens   uint64 `json:"reasoningTokens"`
	CachedInputTokens uint64 `json:"cachedInputTokens"`
	TotalTokens       uint64 `json:"totalTokens"`
}

type chatProviderRun struct {
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	ProviderRequestID string `json:"providerRequestId"`
	FinishReason      string `json:"finishReason"`
	AttemptCount      uint32 `json:"attemptCount"`
}

type aiModelBinding struct {
	ConfigID       string `json:"configId"`
	ConfigRevision uint64 `json:"configRevision"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
}

type conversation struct {
	ID                  string                `json:"id"`
	ProjectID           string                `json:"projectId,omitempty"`
	Revision            uint64                `json:"revision"`
	Title               string                `json:"title"`
	ConfigID            string                `json:"configId,omitempty"`
	ModelBinding        aiModelBinding        `json:"modelBinding,omitempty"`
	WorkspaceMode       string                `json:"workspaceMode,omitempty"`
	LastMessageSequence uint64                `json:"lastMessageSequence,omitempty"`
	CreatedAt           time.Time             `json:"createdAt,omitempty"`
	UpdatedAt           time.Time             `json:"updatedAt"`
	State               string                `json:"state"`
	GenerationID        string                `json:"generationId,omitempty"`
	ActiveAssistantID   string                `json:"activeAssistantId,omitempty"`
	PlanModeActive      bool                  `json:"planModeActive,omitempty"`
	Todos               []aiTodoItem          `json:"todos,omitempty"`
	Subagent            *aiSubagentDescriptor `json:"subagent,omitempty"`
	Goal                *aiGoalSnapshot       `json:"goal,omitempty"`
	// Model remains only for decoding the v1 identity document.
	Model    string        `json:"model,omitempty"`
	Messages []chatMessage `json:"messages,omitempty"`
}

type conversationView struct {
	ID                  string                          `json:"id"`
	ProjectID           string                          `json:"projectId"`
	Revision            uint64                          `json:"revision"`
	Title               string                          `json:"title"`
	ConfigID            string                          `json:"configId"`
	ModelBinding        aiModelBinding                  `json:"modelBinding"`
	Model               string                          `json:"model,omitempty"`
	WorkspaceMode       string                          `json:"workspaceMode"`
	LastMessageSequence uint64                          `json:"lastMessageSequence"`
	CreatedAt           time.Time                       `json:"createdAt"`
	UpdatedAt           time.Time                       `json:"updatedAt"`
	MessageCount        int                             `json:"messageCount"`
	State               string                          `json:"state"`
	GenerationID        string                          `json:"generationId,omitempty"`
	PlanModeActive      bool                            `json:"planModeActive"`
	Todos               []aiTodoItem                    `json:"todos"`
	Subagent            *aiSubagentDescriptor           `json:"subagent,omitempty"`
	Goal                *aiGoalSnapshot                 `json:"goal"`
	GoalArmed           bool                            `json:"goalArmed"`
	Catalog             aiConversationCatalogProjection `json:"-"`
}

func (value conversation) view() conversationView {
	if value.CreatedAt.IsZero() {
		value.CreatedAt = value.UpdatedAt
	}
	if value.WorkspaceMode == "" {
		value.WorkspaceMode = aiWorkspaceModeReadOnly
	}
	value.WorkspaceMode = aiWorkspaceModeFromStorage(value.WorkspaceMode)
	if value.ModelBinding.Model == "" {
		value.ModelBinding = aiModelBinding{ConfigID: value.ConfigID, Provider: "openai-compatible", Model: value.Model}
	}
	if value.ConfigID == "" {
		value.ConfigID = value.ModelBinding.ConfigID
	}
	if value.LastMessageSequence == 0 && len(value.Messages) > 0 {
		value.LastMessageSequence = value.Messages[len(value.Messages)-1].Sequence
	}
	return conversationView{
		ID: value.ID, ProjectID: value.ProjectID, Revision: value.Revision, Title: value.Title,
		ConfigID: value.ConfigID, ModelBinding: value.ModelBinding, WorkspaceMode: value.WorkspaceMode,
		Model:               value.ModelBinding.Model,
		LastMessageSequence: value.LastMessageSequence, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
		MessageCount: len(value.Messages), State: value.State, GenerationID: value.GenerationID,
		PlanModeActive: value.PlanModeActive, Todos: append([]aiTodoItem(nil), value.Todos...), Subagent: value.Subagent,
		Goal: cloneAIGoalSnapshot(value.Goal),
	}
}

type agentState struct {
	mu              sync.RWMutex `json:"-"`
	SchemaVersion   int          `json:"schemaVersion"`
	DeviceID        uuid.UUID    `json:"deviceId"`
	PrivateKey      string       `json:"identityPrivateKey"`
	KeyVersion      uint64       `json:"keyVersion"`
	Revision        uint64       `json:"revision"`
	SessionID       uuid.UUID    `json:"sessionId,omitempty"`
	ConnectionEpoch uint64       `json:"connectionEpoch"`
	Workspace       string       `json:"workspace"`
	// AgentEnvironmentRevision is persisted with the device identity, while
	// the environment values themselves stay in the platform SecretStore.
	AgentEnvironmentRevision uint64 `json:"agentEnvironmentRevision,omitempty"`
	// LegacyAIConfigs is decoded only to perform the one-way v1 JSON-to-SQLite
	// migration. Runtime configurations never serialize into the identity file.
	LegacyAIConfigs map[string]aiConfig `json:"aiConfigs,omitempty"`
	AIConfigs       map[string]aiConfig `json:"-"`
	// LegacyConversations is decoded only for the one-way identity-to-
	// BusinessStore migration. Conversation bodies never return to identity JSON.
	LegacyConversations map[string]conversation `json:"conversations,omitempty"`
	Conversations       map[string]conversation `json:"-"`
	agentEnvironment    map[string]string       `json:"-"`

	path         string
	identity     ed25519.PrivateKey
	business     *businessStore
	secrets      secretStore
	controlStore *controlStateStore
	controlLoop  *deviceControlLoop

	servicesMu sync.Mutex
	// processes owns ConPTY/PTY sessions only. rawProcesses owns background
	// commands with independent stdout/stderr pipes.
	processes              *processSupervisor
	rawProcesses           *rawProcessSupervisor
	terminals              *terminalService
	tasksV2                *taskV2Store
	taskPayloads           *taskPayloadStore
	taskRunners            *taskRunnerRegistry
	taskEngine             *taskEngine
	aiTools                *aiWorkspaceToolExecutor
	eventHub               *agentEventHub
	eventPump              *agentEventPump
	conversationStreamHub  *conversationStreamHub
	conversationStreamPump *conversationStreamPump
	aiToolPolicy           *aiToolPolicyRuntime
	v2Links                *v2AgentLinkRegistry
	v2OperationMu          sync.Mutex
	v2Operations           map[string]*v2InFlightOperation

	aiGenerationMu           sync.Mutex
	aiGenerations            map[string]activeAIGeneration
	aiSubagentClosingMembers map[string]int
	aiSubagentStopping       map[string]int
	aiSubagentMu             sync.Mutex
	aiSubagentActivities     map[string]*aiSubagentActivity
	aiDriverLocks            sync.Map
	aiGoalMu                 sync.Mutex
	aiGoalArmed              map[string]string
	aiApprovalMu             sync.Mutex
	aiApprovals              map[string]*pendingAIApproval
	aiSessionGrants          map[string]map[string]struct{}
	aiQuestionMu             sync.Mutex
	aiQuestions              map[string]*pendingAIQuestion
	skillLoadMu              sync.Mutex
	skillLoads               map[string]int
	aiJobMu                  sync.Mutex
	aiJobs                   map[string]*aiJobRecord

	protocolDiagnosticsMu  sync.Mutex
	protocolDiagnosticSalt [32]byte
	protocolDiagnostics    []deviceProtocolDiagnostic
	protocolDiagnosticSink func(deviceProtocolDiagnostic)

	connectionDiagnosticMu   sync.Mutex
	connectionDiagnosticSink func(deviceConnectionDiagnostic)
}

type activeAIGeneration struct {
	GenerationID          string
	Cancel                context.CancelFunc
	Done                  chan struct{}
	Phase                 aiAgentPhase
	StepWindowOpen        bool
	WorkspaceToolsEnabled bool
	WakeRequested         bool
	CancelCause           string
}

func loadOrCreateAgentState(path, workspace string) (*agentState, error) {
	var err error
	path, err = absoluteAgentPath(path)
	if err != nil {
		return nil, errors.New("state and workspace paths are required")
	}
	workspace, err = absoluteAgentPath(workspace)
	if err != nil {
		return nil, errors.New("state and workspace paths are required")
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(workspace, 0o700); err != nil {
			return nil, fmt.Errorf("create workspace: %w", err)
		}
		_, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		state := &agentState{
			SchemaVersion: agentStateVersion, DeviceID: uuid.New(), PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
			KeyVersion: 1, Revision: 1, Workspace: workspace, AgentEnvironmentRevision: 1, AIConfigs: map[string]aiConfig{},
			Conversations: map[string]conversation{}, aiGenerations: map[string]activeAIGeneration{}, aiGoalArmed: map[string]string{}, path: path, identity: privateKey,
		}
		if err := state.write(); err != nil {
			return nil, err
		}
		store, err := openBusinessStore(state)
		if err != nil {
			return nil, err
		}
		state.business = store
		state.startAgentEventRuntime()
		state.tasksV2 = newTaskV2Store(store)
		state.taskRunners = newTaskRunnerRegistry(state.agentEnvironmentList)
		if _, err := state.tasksV2.RecoverInterrupted(context.Background(), time.Now().UTC()); err != nil {
			_ = store.close()
			return nil, fmt.Errorf("recover interrupted Task v2 runs: %w", err)
		}
		if err := state.initializeTaskLogFiles(); err != nil {
			_ = store.close()
			return nil, err
		}
		secrets, err := openSecretStore(state.path, state.DeviceID, state.identity)
		if err != nil {
			_ = store.close()
			return nil, err
		}
		state.secrets = secrets
		if err := state.loadAgentEnvironment(context.Background()); err != nil {
			_ = store.close()
			return nil, err
		}
		if err := state.reloadAIConfigsFromBusiness(context.Background()); err != nil {
			_ = store.close()
			return nil, err
		}
		if _, err := state.business.recoverInterruptedAIConversations(context.Background(), time.Now().UTC()); err != nil {
			_ = store.close()
			return nil, fmt.Errorf("recover interrupted AI conversations: %w", err)
		}
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent state: %w", err)
	}
	if len(contents) == 0 || len(contents) > 4<<20 {
		return nil, errors.New("agent state size is invalid")
	}
	if err := verifyStateFileSecurity(path); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	state := new(agentState)
	if err := decoder.Decode(state); err != nil {
		return nil, errors.New("agent state JSON is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("agent state contains trailing data")
	}
	privateKey, err := base64.RawURLEncoding.Strict().DecodeString(state.PrivateKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize || state.SchemaVersion != agentStateVersion ||
		state.DeviceID == uuid.Nil || state.KeyVersion == 0 || state.Revision == 0 || state.Workspace == "" {
		return nil, errors.New("agent state identity is invalid")
	}
	state.AIConfigs = state.LegacyAIConfigs
	if state.AIConfigs == nil {
		state.AIConfigs = map[string]aiConfig{}
	}
	state.Conversations = state.LegacyConversations
	if state.Conversations == nil {
		state.Conversations = map[string]conversation{}
	}
	stateWorkspace, err := absoluteAgentPath(state.Workspace)
	if err != nil {
		return nil, errors.New("agent state identity is invalid")
	}
	state.aiGenerations = map[string]activeAIGeneration{}
	state.aiGoalArmed = map[string]string{}
	state.agentEnvironment = map[string]string{}
	if state.AgentEnvironmentRevision == 0 {
		state.AgentEnvironmentRevision = 1
	}
	state.path, state.Workspace, state.identity = path, stateWorkspace, ed25519.PrivateKey(privateKey)
	store, err := openBusinessStore(state)
	if err != nil {
		return nil, err
	}
	state.business = store
	state.startAgentEventRuntime()
	state.tasksV2 = newTaskV2Store(store)
	state.taskRunners = newTaskRunnerRegistry(state.agentEnvironmentList)
	if _, err := state.tasksV2.RecoverInterrupted(context.Background(), time.Now().UTC()); err != nil {
		_ = store.close()
		return nil, fmt.Errorf("recover interrupted Task v2 runs: %w", err)
	}
	if err := state.initializeTaskLogFiles(); err != nil {
		_ = store.close()
		return nil, err
	}
	secrets, err := openSecretStore(state.path, state.DeviceID, state.identity)
	if err != nil {
		_ = store.close()
		return nil, err
	}
	state.secrets = secrets
	if err := state.loadAgentEnvironment(context.Background()); err != nil {
		_ = store.close()
		return nil, err
	}
	if err := state.migrateLegacyAIConfigCredentials(context.Background()); err != nil {
		_ = store.close()
		return nil, err
	}
	if err := state.business.migrateLegacyAIConfigs(context.Background(), state.AIConfigs); err != nil {
		_ = store.close()
		return nil, err
	}
	if err := state.removeLegacyAIConfigsFromIdentity(); err != nil {
		_ = store.close()
		return nil, err
	}
	if err := state.reloadAIConfigsFromBusiness(context.Background()); err != nil {
		_ = store.close()
		return nil, err
	}
	if err := state.loadAIConfigCredentials(context.Background()); err != nil {
		_ = store.close()
		return nil, err
	}
	legacyProjectID := stableProjectID(state.DeviceID, "")
	if err := state.business.migrateLegacyAIConversations(context.Background(), legacyProjectID, state.Conversations, state.AIConfigs); err != nil {
		_ = store.close()
		return nil, err
	}
	if err := state.removeLegacyAIConversationsFromIdentity(); err != nil {
		_ = store.close()
		return nil, err
	}
	state.Conversations = map[string]conversation{}
	if _, err := state.business.recoverInterruptedAIConversations(context.Background(), time.Now().UTC()); err != nil {
		_ = store.close()
		return nil, fmt.Errorf("recover interrupted AI conversations: %w", err)
	}
	return state, nil
}

func (state *agentState) initializeTaskLogFiles() error {
	if state == nil || state.tasksV2 == nil {
		return errors.New("task log store is unavailable")
	}
	if err := state.tasksV2.ReconcileTaskLogFiles(context.Background()); err != nil {
		return fmt.Errorf("reconcile task log files: %w", err)
	}
	return nil
}

// absoluteAgentPath keeps all private state-adjacent stores on one stable
// absolute path. The task engine and conversation store deliberately reject
// relative paths before applying their private-directory protections; serving
// an Agent from a relative state-file setting must therefore be normalized at
// the boundary rather than fail only when the first task starts.
func absoluteAgentPath(value string) (string, error) {
	value = filepath.Clean(strings.TrimSpace(value))
	if value == "" || value == "." {
		return "", errRPCInvalid
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func (state *agentState) removeLegacyAIConfigsFromIdentity() error {
	if state == nil || state.LegacyAIConfigs == nil {
		return nil
	}
	legacy := state.LegacyAIConfigs
	state.LegacyAIConfigs = nil
	if err := state.write(); err != nil {
		state.LegacyAIConfigs = legacy
		return errors.New("commit AI configuration BusinessStore migration")
	}
	return nil
}

func (state *agentState) removeLegacyAIConversationsFromIdentity() error {
	if state == nil || state.LegacyConversations == nil {
		return nil
	}
	legacy := state.LegacyConversations
	state.LegacyConversations = nil
	if err := state.write(); err != nil {
		state.LegacyConversations = legacy
		return errors.New("commit AI conversation BusinessStore migration")
	}
	return nil
}

func (state *agentState) reloadAIConfigsFromBusiness(ctx context.Context) error {
	if state == nil || state.business == nil {
		return errors.New("Agent BusinessStore is unavailable")
	}
	configs, err := state.business.listAIConfigs(ctx)
	if err != nil {
		return err
	}
	state.AIConfigs = make(map[string]aiConfig, len(configs))
	for _, config := range configs {
		state.AIConfigs[config.ID] = config
	}
	return nil
}

func (state *agentState) close() error {
	if state == nil {
		return nil
	}
	state.servicesMu.Lock()
	engine := state.taskEngine
	aiTools := state.aiTools
	terminals := state.terminals
	tasksV2 := state.tasksV2
	processes := state.processes
	rawProcesses := state.rawProcesses
	state.taskEngine = nil
	state.taskRunners = nil
	state.aiTools = nil
	state.terminals = nil
	state.processes = nil
	state.rawProcesses = nil
	state.tasksV2 = nil
	eventPump := state.eventPump
	eventHub := state.eventHub
	conversationStreamPump := state.conversationStreamPump
	conversationStreamHub := state.conversationStreamHub
	state.eventPump = nil
	state.eventHub = nil
	state.conversationStreamPump = nil
	state.conversationStreamHub = nil
	state.aiToolPolicy = nil
	state.servicesMu.Unlock()
	var result error
	if engine != nil {
		result = errors.Join(result, engine.Close())
	}
	closeFileRPCManager(state)
	if tasksV2 != nil {
		tasksV2.closeTaskLogRuntime()
	}
	if aiTools != nil {
		result = errors.Join(result, aiTools.Close())
	}
	if terminals != nil {
		result = errors.Join(result, terminals.Close())
	} else if processes != nil {
		result = errors.Join(result, processes.Close())
	}
	if rawProcesses != nil {
		result = errors.Join(result, rawProcesses.Close())
	}
	if eventHub != nil {
		eventHub.close()
	}
	if eventPump != nil {
		eventPump.close()
	}
	if conversationStreamPump != nil {
		conversationStreamPump.close()
	}
	if conversationStreamHub != nil {
		conversationStreamHub.close()
	}
	if state.business != nil {
		result = errors.Join(result, state.business.close())
	}
	return result
}

func (state *agentState) startAgentEventRuntime() {
	if state == nil || state.business == nil {
		return
	}
	if state.eventHub == nil && state.eventPump == nil {
		hub := newAgentEventHub()
		pump := newAgentEventPump(state.business, hub)
		state.eventHub = hub
		state.eventPump = pump
		pump.start()
	}
	if state.conversationStreamHub == nil && state.conversationStreamPump == nil {
		hub := newConversationStreamHub()
		pump := newConversationStreamPump(state.business, hub)
		state.conversationStreamHub = hub
		state.conversationStreamPump = pump
		pump.start()
	}
}

// publishAIConversationEvent is deliberately called only after the SQLite
// mutation returned successfully. The stream hub is best-effort latency
// acceleration; ai_conversation_events remains the recovery source.
func (state *agentState) publishAIConversationEvent(projectID uuid.UUID, event aiConversationEvent) {
	if state == nil || projectID == uuid.Nil || event.ConversationID == "" || event.Sequence == 0 {
		return
	}
	state.servicesMu.Lock()
	hub := state.conversationStreamHub
	state.servicesMu.Unlock()
	if hub != nil {
		hub.publishFor(projectID, event)
	}
}

func (state *agentState) startTaskEngine() error {
	if state == nil || state.business == nil || state.tasksV2 == nil {
		return errRPCCapability
	}
	state.servicesMu.Lock()
	defer state.servicesMu.Unlock()
	if state.taskEngine != nil {
		return nil
	}
	if state.rawProcesses == nil {
		state.rawProcesses = newRawProcessSupervisor(state.agentEnvironmentList)
	}
	if state.taskRunners == nil {
		state.taskRunners = newTaskRunnerRegistry(state.agentEnvironmentList)
	}
	engine := newTaskEngine(state, state.rawProcesses, state.taskRunners)
	state.tasksV2.startTaskLogMaintenance()
	if err := engine.Start(); err != nil {
		state.tasksV2.closeTaskLogMaintenance()
		return err
	}
	state.taskEngine = engine
	return nil
}

func (state *agentState) currentTaskEngine() *taskEngine {
	if state == nil {
		return nil
	}
	state.servicesMu.Lock()
	defer state.servicesMu.Unlock()
	return state.taskEngine
}

func (state *agentState) wakeTaskEngine() {
	if engine := state.currentTaskEngine(); engine != nil {
		engine.Wake()
	}
}

func (state *agentState) taskRunnerCapabilitySnapshot() []taskRunnerCapability {
	if state == nil {
		return nil
	}
	state.servicesMu.Lock()
	runners := state.taskRunners
	state.servicesMu.Unlock()
	if runners == nil {
		return nil
	}
	return runners.Capabilities()
}

func (state *agentState) terminalService() (*terminalService, error) {
	if state == nil || state.business == nil {
		return nil, errRPCCapability
	}
	state.servicesMu.Lock()
	defer state.servicesMu.Unlock()
	if state.terminals != nil {
		return state.terminals, nil
	}
	if state.processes == nil {
		state.processes = newProcessSupervisor(state.agentEnvironmentList)
	}
	state.terminals = newTerminalService(state, state.processes)
	return state.terminals, nil
}

func (state *agentState) aiWorkspaceTools() (*aiWorkspaceToolExecutor, error) {
	if state == nil || state.business == nil {
		return nil, errRPCCapability
	}
	state.servicesMu.Lock()
	defer state.servicesMu.Unlock()
	if state.aiTools != nil {
		return state.aiTools, nil
	}
	if state.rawProcesses == nil {
		state.rawProcesses = newRawProcessSupervisor(state.agentEnvironmentList)
	}
	if state.processes == nil {
		state.processes = newProcessSupervisor(state.agentEnvironmentList)
	}
	state.aiTools = newAIWorkspaceToolExecutor(state, state.rawProcesses, state.processes)
	return state.aiTools, nil
}

func (state *agentState) existingAIWorkspaceTools() *aiWorkspaceToolExecutor {
	if state == nil {
		return nil
	}
	state.servicesMu.Lock()
	defer state.servicesMu.Unlock()
	return state.aiTools
}

func (state *agentState) aiToolPolicyRuntime() *aiToolPolicyRuntime {
	if state == nil {
		return nil
	}
	state.servicesMu.Lock()
	defer state.servicesMu.Unlock()
	if state.aiToolPolicy == nil {
		state.aiToolPolicy = newAIToolPolicyRuntime()
	}
	return state.aiToolPolicy
}

func (state *agentState) publicKey() string {
	return base64.RawURLEncoding.EncodeToString(state.identity.Public().(ed25519.PublicKey))
}

func (state *agentState) persistMutation() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.persistMutationLocked()
}

func (state *agentState) persistMutationLocked() error {
	if state.Revision == ^uint64(0) {
		return errors.New("agent revision is exhausted")
	}
	previous := state.Revision
	state.Revision++
	if err := state.writeLocked(); err != nil {
		state.Revision = previous
		return err
	}
	return nil
}

func (state *agentState) write() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.writeLocked()
}

func (state *agentState) writeLocked() error {
	if state == nil || state.path == "" || len(state.identity) != ed25519.PrivateKeySize {
		return errors.New("agent state is invalid")
	}
	state.PrivateKey = base64.RawURLEncoding.EncodeToString(state.identity)
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	parent := filepath.Dir(state.path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".device-agent-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error {
		_ = temporary.Close()
		return cause
	}
	if err := temporary.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fail(err)
	}
	if err := temporary.Sync(); err != nil {
		return fail(err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := secureStateFile(temporaryPath); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, state.path); err != nil {
		return err
	}
	return secureStateFile(state.path)
}

func (state *agentState) revisionValue() uint64 {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.Revision
}

func (state *agentState) setSessionID(sessionID uuid.UUID) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	previous := state.SessionID
	state.SessionID = sessionID
	if err := state.writeLocked(); err != nil {
		state.SessionID = previous
		return err
	}
	return nil
}

func (state *agentState) advanceConnectionEpoch() (uint64, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ConnectionEpoch == ^uint64(0) {
		return 0, errors.New("connection epoch is exhausted")
	}
	previous := state.ConnectionEpoch
	state.ConnectionEpoch++
	if err := state.writeLocked(); err != nil {
		state.ConnectionEpoch = previous
		return 0, err
	}
	return state.ConnectionEpoch, nil
}

func (state *agentState) connectionEpochValue() uint64 {
	if state == nil {
		return 0
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.ConnectionEpoch
}

func (state *agentState) aiConfigSnapshot(id string) (aiConfig, bool) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	config, found := state.AIConfigs[id]
	return config, found
}

func (state *agentState) registerAIGeneration(conversationID, generationID string, cancel context.CancelFunc) bool {
	return state.tryRegisterAIGeneration(conversationID, generationID, cancel) == nil
}

func (state *agentState) reserveAIGeneration(conversationID, generationID string, cancel context.CancelFunc) error {
	if state == nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil || cancel == nil {
		return errRPCInvalid
	}
	state.aiGenerationMu.Lock()
	defer state.aiGenerationMu.Unlock()
	if state.aiGenerations == nil {
		state.aiGenerations = map[string]activeAIGeneration{}
	}
	if state.aiSubagentStopping[conversationID] > 0 {
		return context.Canceled
	}
	if _, active := state.aiGenerations[conversationID]; active {
		return errRPCConversationGenerationActive
	}
	if len(state.aiGenerations) >= maximumActiveAIGenerations {
		return errRPCAgentGenerationCapacity
	}
	state.aiGenerations[conversationID] = activeAIGeneration{
		GenerationID: generationID, Cancel: cancel, Done: make(chan struct{}), Phase: aiAgentPhaseMaintenance,
	}
	return nil
}

// tryRegisterAIGeneration preserves the old bool helper for callers that only
// need a guard, while allowing the RPC layer to distinguish a per-conversation
// conflict from global device capacity.
func (state *agentState) tryRegisterAIGeneration(conversationID, generationID string, cancel context.CancelFunc) error {
	if state == nil || uuid.Validate(conversationID) != nil || uuid.Validate(generationID) != nil || cancel == nil {
		return errRPCInvalid
	}
	state.aiGenerationMu.Lock()
	defer state.aiGenerationMu.Unlock()
	if state.aiGenerations == nil {
		state.aiGenerations = map[string]activeAIGeneration{}
	}
	if active, found := state.aiGenerations[conversationID]; found {
		if active.GenerationID == generationID && active.Phase == aiAgentPhaseMaintenance {
			active.Cancel = cancel
			active.Phase = aiAgentPhaseRunning
			active.StepWindowOpen = true
			state.aiGenerations[conversationID] = active
			return nil
		}
		if active.GenerationID == generationID && active.Phase == aiAgentPhaseStopping {
			cancel()
			return nil
		}
		return errRPCConversationGenerationActive
	}
	if state.aiSubagentStopping[conversationID] > 0 {
		return context.Canceled
	}
	if len(state.aiGenerations) >= maximumActiveAIGenerations {
		return errRPCAgentGenerationCapacity
	}
	state.aiGenerations[conversationID] = activeAIGeneration{
		GenerationID: generationID, Cancel: cancel, Done: make(chan struct{}), Phase: aiAgentPhaseRunning, StepWindowOpen: true,
	}
	return nil
}

func (state *agentState) unregisterAIGeneration(conversationID, generationID string) bool {
	if state == nil {
		return false
	}
	wakeRequested := false
	var done chan struct{}
	state.aiGenerationMu.Lock()
	if active, found := state.aiGenerations[conversationID]; found && active.GenerationID == generationID {
		wakeRequested = active.WakeRequested
		done = active.Done
		delete(state.aiGenerations, conversationID)
	}
	state.aiGenerationMu.Unlock()
	state.clearAIGenerationApprovals(generationID)
	state.clearAIGenerationQuestions(generationID)
	state.clearAISkillLoads(generationID)
	if done != nil {
		close(done)
	}
	return wakeRequested
}

func (state *agentState) cancelAIGeneration(conversationID, generationID string) bool {
	if state == nil {
		return false
	}
	state.aiGenerationMu.Lock()
	active, found := state.aiGenerations[conversationID]
	if !found || generationID != "" && active.GenerationID != generationID {
		state.aiGenerationMu.Unlock()
		return false
	}
	if active.Phase != aiAgentPhaseStopping {
		active.Phase = aiAgentPhaseStopping
		active.StepWindowOpen = false
		if active.CancelCause == "" {
			active.CancelCause = "cancelled"
		}
		state.aiGenerations[conversationID] = active
	}
	state.aiGenerationMu.Unlock()
	active.Cancel()
	return true
}

func (state *agentState) migrateLegacyAIConfigCredentials(ctx context.Context) error {
	if state == nil || state.secrets == nil {
		return errors.New("Agent SecretStore is unavailable")
	}
	type addedSecret struct{ key string }
	added := make([]addedSecret, 0)
	rollback := func() {
		for index := len(added) - 1; index >= 0; index-- {
			_ = state.secrets.Delete(context.Background(), added[index].key)
		}
	}
	changed := false
	for id, config := range state.AIConfigs {
		if config.LegacyCredential == "" {
			continue
		}
		key := aiCredentialSecretKey(id)
		existing, found, err := state.secrets.Get(ctx, key)
		if err != nil {
			rollback()
			return errors.New("migrate AI credential to SecretStore")
		}
		if found {
			matches := subtle.ConstantTimeCompare(existing, []byte(config.LegacyCredential)) == 1
			zeroSecret(existing)
			if !matches {
				rollback()
				return errors.New("migrate AI credential: SecretStore item conflicts with legacy state")
			}
		} else {
			value := []byte(config.LegacyCredential)
			err = state.secrets.Put(ctx, key, value)
			zeroSecret(value)
			if err != nil {
				rollback()
				return errors.New("migrate AI credential to SecretStore")
			}
			added = append(added, addedSecret{key: key})
		}
		config.Credential = config.LegacyCredential
		config.LegacyCredential = ""
		config.CredentialConfigured = true
		state.AIConfigs[id] = config
		changed = true
	}
	if !changed {
		return nil
	}
	if err := state.write(); err != nil {
		rollback()
		return errors.New("commit AI credential SecretStore migration")
	}
	return nil
}

func (state *agentState) loadAIConfigCredentials(ctx context.Context) error {
	if state == nil || state.secrets == nil {
		return errors.New("Agent SecretStore is unavailable")
	}
	for id, config := range state.AIConfigs {
		if !config.CredentialConfigured {
			config.Credential = ""
			state.AIConfigs[id] = config
			continue
		}
		value, found, err := state.secrets.Get(ctx, aiCredentialSecretKey(id))
		if err != nil || !found {
			zeroSecret(value)
			return errors.New("load configured AI credential from SecretStore")
		}
		config.Credential = string(value)
		zeroSecret(value)
		state.AIConfigs[id] = config
	}
	return nil
}

func (state *agentState) conversationMessagesAtRevision(id string, revision uint64) ([]chatMessage, bool) {
	state.mu.RLock()
	defer state.mu.RUnlock()
	value, found := state.Conversations[id]
	if !found {
		return nil, false
	}
	messages := make([]chatMessage, 0, 2)
	for _, message := range value.Messages {
		if message.Revision == revision {
			messages = append(messages, message)
		}
	}
	return messages, true
}

func cloneConversation(value conversation) conversation {
	value.Messages = append([]chatMessage(nil), value.Messages...)
	return value
}

func configFromInput(input rpcInput) (aiConfig, error) {
	id, okID := optionalInputString(input, "id", 80)
	if id == "" {
		id = "default"
	}
	name, okName := inputString(input, "name", 120)
	provider, okProvider := inputString(input, "provider", 80)
	provider = canonicalAIProvider(provider)
	baseURL, okBaseURL := optionalInputString(input, "baseUrl", 2048)
	if baseURL == "" {
		baseURL = defaultAIProviderBaseURL(provider)
	}
	model, okModel := inputString(input, "model", 120)
	enabled, okEnabled := input["enabled"].(bool)
	defaults := defaultAIConfigSettings(id)
	headers, okHeaders := optionalStringMap(input, "nonSecretHeaders")
	systemPrompt, okSystemPrompt := optionalInputString(input, "systemPrompt", 32768)
	if _, present := input["systemPrompt"]; !present {
		systemPrompt = defaults.SystemPrompt
	}
	temperature, okTemperature := optionalInputFloat(input, "temperature", defaults.Temperature)
	reasoningEffort, okReasoning := optionalInputString(input, "reasoningEffort", 16)
	if reasoningEffort == "" {
		reasoningEffort = defaults.ReasoningEffort
	}
	maxTurnOutputTokens, okTurnOutput := optionalInputUint32(input, "maxTurnOutputTokens", defaults.MaxTurnOutputTokens)
	maxActiveContextTokens, okActiveContext := optionalInputUint32(input, "maxActiveContextTokens", defaults.MaxActiveContextTokens)
	maxAgentRounds, okAgentRounds := optionalInputUint32(input, "maxAgentRounds", defaults.MaxAgentRounds)
	maxAgentToolCalls, okToolCalls := optionalInputUint32(input, "maxAgentToolCalls", defaults.MaxAgentToolCalls)
	maxNoProgress, okNoProgress := optionalInputUint32(input, "maxAgentNoProgressRounds", defaults.MaxAgentNoProgressRounds)
	requestTimeout, okTimeout := optionalInputUint32(input, "requestTimeoutSeconds", defaults.RequestTimeoutSeconds)
	maxRetries, okRetries := optionalInputUint32(input, "maxRetries", defaults.MaxRetries)
	retryDelay, okRetryDelay := optionalInputUint32(input, "retryBaseDelayMilliseconds", defaults.RetryBaseDelayMilliseconds)
	showUsage, okShowUsage := optionalInputBool(input, "showUsage", true)
	if !okID || !okName || !okProvider || provider == "" || !okBaseURL || !okModel || !okEnabled || !okHeaders ||
		!okSystemPrompt || !okTemperature || !okReasoning || !okTurnOutput || !okActiveContext || !okAgentRounds ||
		!okToolCalls || !okNoProgress || !okTimeout || !okRetries || !okRetryDelay || !okShowUsage {
		return aiConfig{}, errRPCInvalid
	}
	config := aiConfig{
		ID: id, Name: name, Provider: provider, BaseURL: baseURL, NonSecretHeaders: headers, Model: model,
		SystemPrompt: systemPrompt, Temperature: temperature, ReasoningEffort: reasoningEffort,
		MaxTurnOutputTokens: maxTurnOutputTokens, MaxActiveContextTokens: maxActiveContextTokens,
		MaxAgentRounds: maxAgentRounds, MaxAgentToolCalls: maxAgentToolCalls, MaxAgentNoProgressRounds: maxNoProgress,
		RequestTimeoutSeconds: requestTimeout, MaxRetries: maxRetries, RetryBaseDelayMilliseconds: retryDelay,
		ShowUsage: showUsage, Enabled: enabled,
	}
	if err := validateAIConfigForStorage(config, false); err != nil {
		return aiConfig{}, err
	}
	if _, _, err := parseAIBaseURL(config.BaseURL); err != nil {
		// Base URL syntax and endpoint-policy failures are configuration input
		// errors. They happen before provider I/O and must not be surfaced as a
		// retryable provider outage (which makes clients reconnect needlessly).
		return aiConfig{}, errRPCInvalid
	}
	return config, nil
}

func defaultAIProviderBaseURL(provider string) string {
	switch provider {
	case "openai":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "google":
		return "https://generativelanguage.googleapis.com/v1beta"
	case "deepseek":
		return "https://api.deepseek.com/v1"
	case "ollama":
		return "http://localhost:11434"
	default:
		return ""
	}
}

func optionalStringMap(input rpcInput, key string) (map[string]string, bool) {
	raw, exists := input[key]
	if !exists || raw == nil {
		return map[string]string{}, true
	}
	value, ok := raw.(map[string]any)
	if !ok || len(value) > 32 {
		return nil, false
	}
	result := make(map[string]string, len(value))
	for name, rawValue := range value {
		text, ok := rawValue.(string)
		if !ok {
			return nil, false
		}
		result[name] = text
	}
	return result, validateNonSecretAIHeaders(result) == nil
}

func optionalInputFloat(input rpcInput, key string, fallback float64) (float64, bool) {
	raw, exists := input[key]
	if !exists || raw == nil {
		return fallback, true
	}
	value, ok := raw.(float64)
	return value, ok
}

func optionalInputUint32(input rpcInput, key string, fallback uint32) (uint32, bool) {
	raw, exists := input[key]
	if !exists || raw == nil {
		return fallback, true
	}
	value, ok := raw.(float64)
	if !ok || value < 0 || value > float64(^uint32(0)) || value != float64(uint32(value)) {
		return 0, false
	}
	return uint32(value), true
}

func optionalInputBool(input rpcInput, key string, fallback bool) (bool, bool) {
	raw, exists := input[key]
	if !exists || raw == nil {
		return fallback, true
	}
	value, ok := raw.(bool)
	return value, ok
}
