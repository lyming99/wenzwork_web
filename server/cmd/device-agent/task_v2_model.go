package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	maximumTaskDefinitionBytes      = 512 << 10
	maximumTaskPromptBytes          = 256 << 10
	maximumTaskCommandBytes         = 64 << 10
	maximumTaskEnvironmentBytes     = 64 << 10
	maximumTaskEnvironmentVariables = 64
	maximumTaskAttachments          = 32
	maximumTaskRelationships        = 64
	maximumTaskLogEntryBytes        = 64 << 10
	maximumTaskRunLogRecordBytes    = 4096
	maximumTaskRunLogFileBytes      = 64 << 20
	maximumTaskLogSeekBytes         = 32 << 10
	maximumTaskLogBytesPerTask      = 16 << 20
	maximumTaskLogBytesGlobal       = 256 << 20
	maximumTaskChanges              = 4096
)

var (
	taskKinds    = []string{"codex", "cursor", "hermes", "jcode", "opencode", "claude", "kimi", "pi", "script", "workflow"}
	taskStatuses = []string{
		"queued", "waiting", "running", "awaitingAcceptance", "changesRequested", "completed", "failed", "blocked", "cancelled", "succeeded",
	}
	taskEnvironmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

type taskV2ExecutionOptions struct {
	Relation         string      `json:"relation"`
	Mode             string      `json:"mode"`
	RelatedTaskIDs   []uuid.UUID `json:"relatedTaskIds"`
	RunImmediately   bool        `json:"runImmediately"`
	ScheduledAt      *time.Time  `json:"scheduledAt,omitempty"`
	WorkflowID       *uuid.UUID  `json:"workflowId,omitempty"`
	CliSessionID     string      `json:"cliSessionId,omitempty"`
	ResumeCliSession bool        `json:"resumeCliSession"`
}

type taskV2Definition struct {
	ID                  uuid.UUID              `json:"id"`
	ProjectID           uuid.UUID              `json:"projectId"`
	Kind                string                 `json:"kind"`
	Title               string                 `json:"title"`
	CWD                 string                 `json:"cwd"`
	Plan                string                 `json:"plan,omitempty"`
	Config              json.RawMessage        `json:"config"`
	Execution           taskV2ExecutionOptions `json:"execution"`
	Scope               string                 `json:"scope"`
	OwnerWorkflowTaskID *uuid.UUID             `json:"ownerWorkflowTaskId,omitempty"`
	ParentTaskID        *uuid.UUID             `json:"parentTaskId,omitempty"`
	RootTaskID          *uuid.UUID             `json:"rootTaskId,omitempty"`
	AcceptanceFeedback  string                 `json:"acceptanceFeedback,omitempty"`
	Environment         map[string]string      `json:"environment,omitempty"`
}

type taskV2Record struct {
	Definition         taskV2Definition `json:"definition"`
	DefinitionRevision uint64           `json:"definitionRevision"`
	Status             string           `json:"status"`
	Revision           uint64           `json:"revision"`
	ChangeSequence     uint64           `json:"changeSequence"`
	CurrentRunID       *uuid.UUID       `json:"currentRunId,omitempty"`
	CreatedAt          time.Time        `json:"createdAt"`
	UpdatedAt          time.Time        `json:"updatedAt"`
	StartedAt          *time.Time       `json:"startedAt,omitempty"`
	FinishedAt         *time.Time       `json:"finishedAt,omitempty"`
	ExitCode           *int             `json:"exitCode,omitempty"`
	ResultCode         string           `json:"resultCode,omitempty"`
	LogAvailable       bool             `json:"logAvailable"`
	LogState           string           `json:"logState"`
	LogGeneration      uint64           `json:"logGeneration"`
	LogSizeBytes       uint64           `json:"logSizeBytes"`
	LogFormatVersion   uint32           `json:"-"`
	LogSHA256          string           `json:"-"`
	LogUpdatedAt       *time.Time       `json:"-"`
	LegacyLogPath      string           `json:"-"`
}

type taskV2Run struct {
	ID                      uuid.UUID  `json:"id"`
	TaskID                  uuid.UUID  `json:"taskId"`
	WorkflowRevisionID      *uuid.UUID `json:"workflowRevisionId,omitempty"`
	ParentWorkflowTaskRunID *uuid.UUID `json:"parentWorkflowTaskRunId,omitempty"`
	WorkflowNodeID          string     `json:"workflowNodeId,omitempty"`
	Status                  string     `json:"status"`
	Attempt                 uint32     `json:"attempt"`
	CreatedAt               time.Time  `json:"createdAt"`
	StartedAt               *time.Time `json:"startedAt,omitempty"`
	FinishedAt              *time.Time `json:"finishedAt,omitempty"`
	ExitCode                *int       `json:"exitCode,omitempty"`
	ResultCode              string     `json:"resultCode,omitempty"`
	CliSessionID            string     `json:"cliSessionId,omitempty"`
	LogAvailable            bool       `json:"logAvailable"`
	LogState                string     `json:"logState"`
	LogGeneration           uint64     `json:"logGeneration"`
	LogFormatVersion        uint32     `json:"logFormatVersion"`
	LogSizeBytes            uint64     `json:"logSizeBytes"`
	LogSHA256               string     `json:"-"`
	LogUpdatedAt            *time.Time `json:"-"`
	LegacyLogPath           string     `json:"-"`
}

const (
	taskLogStateNone      = "none"
	taskLogStateCreating  = "creating"
	taskLogStateActive    = "active"
	taskLogStateSealed    = "sealed"
	taskLogStateExpired   = "expired"
	taskLogStateMissing   = "missing"
	taskLogStateMigrating = "migrating"
)

func validTaskLogState(value string) bool {
	switch value {
	case taskLogStateNone, taskLogStateCreating, taskLogStateActive, taskLogStateSealed,
		taskLogStateExpired, taskLogStateMissing, taskLogStateMigrating:
		return true
	default:
		return false
	}
}

func taskLogAvailable(state string) bool {
	return state == taskLogStateActive || state == taskLogStateSealed
}

type taskV2Log struct {
	TaskID   uuid.UUID  `json:"taskId"`
	RunID    *uuid.UUID `json:"runId,omitempty"`
	Sequence uint64     `json:"sequence"`
	Stream   string     `json:"stream"`
	// Content always contains the original raw bytes. DisplayText is a safe
	// projection generated per stream for user-visible non-interactive logs.
	Content         []byte    `json:"-"`
	DisplayText     string    `json:"-"`
	SourceEncoding  string    `json:"-"`
	IsBinary        bool      `json:"-"`
	HadDecodeErrors bool      `json:"-"`
	RawAvailable    bool      `json:"-"`
	OccurredAt      time.Time `json:"occurredAt"`
}

func decodeTaskV2Definition(raw []byte) (taskV2Definition, error) {
	if len(raw) == 0 || len(raw) > maximumTaskDefinitionBytes {
		return taskV2Definition{}, errRPCInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var definition taskV2Definition
	if err := decoder.Decode(&definition); err != nil {
		return taskV2Definition{}, errRPCInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return taskV2Definition{}, errRPCInvalid
	}
	return definition, nil
}

func normalizeTaskV2Definition(project registeredProject, definition taskV2Definition) (taskV2Definition, error) {
	definition.Kind = strings.TrimSpace(definition.Kind)
	definition.Title = strings.TrimSpace(definition.Title)
	definition.Plan = strings.TrimSpace(definition.Plan)
	definition.Scope = strings.TrimSpace(definition.Scope)
	definition.AcceptanceFeedback = strings.TrimSpace(definition.AcceptanceFeedback)
	if definition.ID == uuid.Nil || definition.ProjectID != project.ID || !slices.Contains(taskKinds, definition.Kind) ||
		definition.Title == "" || len([]byte(definition.Title)) > 200 || len([]byte(definition.Plan)) > 64<<10 ||
		len([]byte(definition.AcceptanceFeedback)) > 64<<10 || strings.IndexByte(definition.AcceptanceFeedback, 0) >= 0 ||
		(definition.Scope != "topLevel" && definition.Scope != "workflowNode") {
		return taskV2Definition{}, errRPCInvalid
	}
	if definition.Scope == "topLevel" && definition.OwnerWorkflowTaskID != nil ||
		definition.Scope == "workflowNode" && definition.OwnerWorkflowTaskID == nil ||
		definition.Scope == "topLevel" && definition.Execution.WorkflowID != nil ||
		definition.Scope == "workflowNode" && (definition.Execution.WorkflowID == nil || *definition.Execution.WorkflowID != *definition.OwnerWorkflowTaskID) {
		return taskV2Definition{}, errRPCInvalid
	}
	cwd, err := normalizeTaskDirectory(project, definition.CWD)
	if err != nil {
		return taskV2Definition{}, err
	}
	definition.CWD = cwd
	if err := normalizeTaskExecution(&definition); err != nil {
		return taskV2Definition{}, err
	}
	if err := normalizeTaskEnvironment(&definition); err != nil {
		return taskV2Definition{}, err
	}
	config, err := normalizeTaskConfig(project, definition.Kind, definition.Config)
	if err != nil {
		return taskV2Definition{}, err
	}
	definition.Config = config
	encoded, err := json.Marshal(definition)
	if err != nil || len(encoded) > maximumTaskDefinitionBytes {
		return taskV2Definition{}, errRPCInvalid
	}
	return definition, nil
}

func normalizeTaskDirectory(project registeredProject, value string) (string, error) {
	absolute, normalized, err := secureExistingProjectPath(project, value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", errRPCNotFound
	}
	return normalized, nil
}

func normalizeTaskExecution(definition *taskV2Definition) error {
	execution := &definition.Execution
	if execution.Relation == "" {
		execution.Relation = "dependency"
	}
	if execution.Mode == "" {
		execution.Mode = "serial"
	}
	if (execution.Relation != "dependency" && execution.Relation != "sibling") ||
		(execution.Mode != "serial" && execution.Mode != "parallel") ||
		len(execution.RelatedTaskIDs) > maximumTaskRelationships ||
		execution.ResumeCliSession && execution.CliSessionID == "" ||
		len(execution.CliSessionID) > 512 || strings.IndexByte(execution.CliSessionID, 0) >= 0 {
		return errRPCInvalid
	}
	seen := make(map[uuid.UUID]struct{}, len(execution.RelatedTaskIDs))
	for _, related := range execution.RelatedTaskIDs {
		if related == uuid.Nil || related == definition.ID {
			return errRPCInvalid
		}
		if _, found := seen[related]; found {
			return errRPCInvalid
		}
		seen[related] = struct{}{}
	}
	if execution.ScheduledAt != nil {
		value := execution.ScheduledAt.UTC().Truncate(time.Millisecond)
		execution.ScheduledAt = &value
	}
	return nil
}

func normalizeTaskEnvironment(definition *taskV2Definition) error {
	if len(definition.Environment) > maximumTaskEnvironmentVariables {
		return errRPCInvalid
	}
	total := 0
	for name, value := range definition.Environment {
		upper := strings.ToUpper(name)
		total += len(name) + len(value)
		if !taskEnvironmentNamePattern.MatchString(name) || strings.IndexByte(value, 0) >= 0 || len(value) > 8<<10 ||
			strings.HasPrefix(upper, "WENZWORK_") {
			return errRPCInvalid
		}
		if interactiveCommandEnvironmentKey(upper) {
			return errRPCInvalid
		}
	}
	if total > maximumTaskEnvironmentBytes {
		return errRPCInvalid
	}
	return nil
}

func normalizeTaskConfig(project registeredProject, kind string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > maximumTaskDefinitionBytes {
		return nil, errRPCInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var config map[string]any
	if err := decoder.Decode(&config); err != nil || config == nil {
		return nil, errRPCInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errRPCInvalid
	}
	if kind == "script" {
		if err := normalizeScriptTaskConfig(project, config); err != nil {
			return nil, err
		}
	} else if kind == "workflow" {
		if !taskMapHasOnly(config, "revisionId", "revisionVersion") {
			return nil, errRPCInvalid
		}
	} else if err := normalizeCLITaskConfig(project, kind, config); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(config)
	if err != nil || len(encoded) > maximumTaskDefinitionBytes {
		return nil, errRPCInvalid
	}
	return encoded, nil
}

func normalizeCLITaskConfig(project registeredProject, kind string, config map[string]any) error {
	allowed := []string{"promptSource", "promptFilePath", "promptText", "attachedFilePaths"}
	switch kind {
	case "codex":
		allowed = append(allowed, "model", "launchMode", "goalMode", "reasoningEffort")
	case "cursor":
		allowed = append(allowed, "model", "sandboxMode")
	case "hermes":
		allowed = append(allowed, "model", "provider")
	case "jcode":
		allowed = append(allowed, "model", "provider", "providerProfile", "toolProfile")
	case "opencode", "kimi":
		allowed = append(allowed, "autoMode")
	case "claude":
		allowed = append(allowed, "model", "apiBaseUrl", "apiKey", "reasoningEffort")
	case "pi":
	default:
		return errRPCInvalid
	}
	if !taskMapHasOnly(config, allowed...) {
		return errRPCInvalid
	}
	source, ok := config["promptSource"].(string)
	if !ok || (source != "customText" && source != "currentMarkdownFile") {
		return errRPCInvalid
	}
	if source == "customText" {
		prompt, ok := config["promptText"].(string)
		if !ok || strings.TrimSpace(prompt) == "" || len([]byte(prompt)) > maximumTaskPromptBytes || strings.IndexByte(prompt, 0) >= 0 {
			return errRPCInvalid
		}
	} else {
		path, ok := taskOptionalString(config, "promptFilePath", 4096)
		if !ok || path == "" {
			return errRPCInvalid
		}
		normalized, err := normalizeTaskRegularFile(project, path)
		if err != nil {
			return err
		}
		config["promptFilePath"] = normalized
	}
	attachments, ok := taskStringList(config["attachedFilePaths"], maximumTaskAttachments, 4096)
	if !ok {
		return errRPCInvalid
	}
	for index, path := range attachments {
		normalized, err := normalizeTaskRegularFile(project, path)
		if err != nil {
			return err
		}
		attachments[index] = normalized
	}
	config["attachedFilePaths"] = attachments
	for _, key := range []string{"model", "provider", "providerProfile", "apiKey"} {
		if value, ok := taskOptionalString(config, key, 8192); !ok {
			return errRPCInvalid
		} else if value != "" {
			config[key] = value
		}
	}
	if value, ok := taskOptionalString(config, "apiBaseUrl", 2048); !ok || value != "" && !validTaskProviderURL(value) {
		return errRPCInvalid
	} else if value != "" {
		config["apiBaseUrl"] = value
	}
	if value, ok := taskOptionalString(config, "reasoningEffort", 16); !ok || value != "" && !slices.Contains([]string{"low", "medium", "high", "xhigh", "max", "ultra"}, value) {
		return errRPCInvalid
	}
	if value, found := config["launchMode"]; found && value != nil && value != "cli" && value != "windowsNativeExe" {
		return errRPCInvalid
	}
	if value, found := config["sandboxMode"]; found && value != nil && value != "enabled" && value != "disabled" {
		return errRPCInvalid
	}
	if value, found := config["toolProfile"]; found && value != nil && value != "minimal" && value != "full" {
		return errRPCInvalid
	}
	for _, key := range []string{"goalMode", "autoMode"} {
		if value, found := config[key]; found && value != nil {
			if _, ok := value.(bool); !ok {
				return errRPCInvalid
			}
		}
	}
	return nil
}

func normalizeScriptTaskConfig(project registeredProject, config map[string]any) error {
	if !taskMapHasOnly(config, "command", "cwdChoice", "customCwd") {
		return errRPCInvalid
	}
	command, ok := config["command"].(string)
	choice, choiceOK := config["cwdChoice"].(string)
	if !ok || strings.TrimSpace(command) == "" || len([]byte(command)) > maximumTaskCommandBytes || strings.IndexByte(command, 0) >= 0 ||
		!choiceOK || (choice != "workspace" && choice != "markdownDir" && choice != "custom") {
		return errRPCInvalid
	}
	if choice == "custom" {
		custom, ok := taskOptionalString(config, "customCwd", 4096)
		if !ok || custom == "" {
			return errRPCInvalid
		}
		normalized, err := normalizeTaskDirectory(project, custom)
		if err != nil {
			return err
		}
		config["customCwd"] = normalized
	}
	return nil
}

func normalizeTaskRegularFile(project registeredProject, value string) (string, error) {
	absolute, normalized, err := secureExistingProjectPath(project, value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() {
		return "", errRPCNotFound
	}
	return normalized, nil
}

func validTaskProviderURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "http" && isExplicitLoopbackHost(parsed.Hostname()))
}

func isExplicitLoopbackHost(value string) bool {
	if strings.EqualFold(value, "localhost") {
		return true
	}
	address := net.ParseIP(value)
	return address != nil && address.IsLoopback()
}

func taskMapHasOnly(value map[string]any, allowed ...string) bool {
	for key := range value {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

func taskOptionalString(value map[string]any, key string, maximum int) (string, bool) {
	raw, found := value[key]
	if !found || raw == nil {
		return "", true
	}
	text, ok := raw.(string)
	text = strings.TrimSpace(text)
	return text, ok && len([]byte(text)) <= maximum && strings.IndexByte(text, 0) < 0
}

func taskStringList(raw any, maximumItems, maximumBytes int) ([]string, bool) {
	if raw == nil {
		return []string{}, true
	}
	values, ok := raw.([]any)
	if !ok || len(values) > maximumItems {
		return nil, false
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, rawValue := range values {
		value, ok := rawValue.(string)
		value = strings.TrimSpace(value)
		if !ok || value == "" || len([]byte(value)) > maximumBytes || strings.IndexByte(value, 0) >= 0 {
			return nil, false
		}
		if _, found := seen[value]; found {
			return nil, false
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, true
}

func validTaskV2Status(value string) bool { return slices.Contains(taskStatuses, value) }

func taskV2TerminalStatus(value string) bool {
	switch value {
	case "completed", "failed", "blocked", "cancelled", "succeeded", "changesRequested":
		return true
	default:
		return false
	}
}

func validTaskV2Transition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case "queued":
		return to == "waiting" || to == "cancelled"
	case "waiting":
		return to == "running" || to == "cancelled" || to == "blocked"
	case "running":
		return to == "awaitingAcceptance" || to == "failed" || to == "cancelled" || to == "blocked"
	case "awaitingAcceptance":
		return to == "completed" || to == "changesRequested"
	case "changesRequested":
		return to == "completed" || to == "waiting"
	case "failed", "blocked", "cancelled":
		return to == "waiting"
	case "completed", "succeeded":
		return to == "waiting" || to == "awaitingAcceptance"
	default:
		return false
	}
}
