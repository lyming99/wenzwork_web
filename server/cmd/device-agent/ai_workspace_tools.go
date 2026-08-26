package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumAIWorkspaceReadBytes      = int64(2 << 20)
	maximumAIWorkspaceWriteBytes     = 1 << 20
	maximumAIWorkspaceCommandBytes   = 12 << 10
	maximumAIWorkspaceCommandOutput  = 64 << 10
	maximumAIWorkspaceCommandVisible = 8 << 10
	maximumAIWorkspaceToolResult     = 32 << 10
	maximumAIWorkspaceSearchFiles    = 4_000
	maximumAIWorkspaceSearchBytes    = int64(1 << 20)
	maximumAIWorkspaceRollbackItems  = 50
	maximumAIToolResultArtifactBytes = 1 << 20
	maximumAIWorkspaceImageBytes     = 8 << 20
)

var aiWorkspaceImageMimeTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif",
}

var (
	aiWorkspaceIgnoredDirectories = map[string]struct{}{
		".dart_tool": {}, ".git": {}, ".gradle": {}, ".idea": {}, ".next": {}, ".nuxt": {}, ".vscode": {},
		"build": {}, "coverage": {}, "dist": {}, "node_modules": {}, "target": {},
	}
	aiWorkspaceSensitiveNames = map[string]struct{}{
		".env": {}, ".env.local": {}, ".env.production": {}, "id_dsa": {}, "id_ecdsa": {},
		"id_ed25519": {}, "id_rsa": {},
	}
	aiWorkspaceHostPattern = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))*$`)
)

// aiWorkspaceToolDefinition is the provider-neutral tool contract. InputSchema
// is deliberately plain JSON Schema so every provider adapter can translate it
// without coupling the file/process security layer to a vendor SDK.
type aiWorkspaceToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type aiWorkspaceToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type aiWorkspaceToolResult struct {
	Content  string         `json:"content"`
	Summary  string         `json:"summary"`
	IsError  bool           `json:"isError"`
	Metadata map[string]any `json:"metadata"`
	// Image carries a read_image payload to the provider prompt. It is
	// excluded from JSON (json:"-") so the durable content and audit hashes
	// never embed base64 blobs; integrity stays covered by content_hash.
	Image *aiPromptImage `json:"-"`
}

type aiWorkspaceApprovalPreview struct {
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	RelativePaths     []string `json:"relativePaths,omitempty"`
	Command           string   `json:"command,omitempty"`
	WorkingDirectory  string   `json:"workingDirectory,omitempty"`
	NetworkHosts      []string `json:"networkHosts,omitempty"`
	ArgumentsSHA256   string   `json:"argumentsSha256"`
	AllowForSession   bool     `json:"allowForSession"`
	Risk              string   `json:"risk"`
	SandboxStatus     string   `json:"sandboxStatus,omitempty"`
	EnvironmentPolicy string   `json:"environmentPolicy,omitempty"`
}

type aiWorkspaceToolPlan struct {
	Call                aiWorkspaceToolCall        `json:"call"`
	RequiresApproval    bool                       `json:"requiresApproval"`
	Preview             aiWorkspaceApprovalPreview `json:"preview"`
	resolvedPath        string
	commandLaunch       aiCommandSandboxLaunch
	terminalName        string
	terminalShell       string
	terminalDisplayCWD  string
	terminalSessionID   uuid.UUID
	terminalOwner       aiTerminalOwner
	auditID             string
	auditStartedAt      time.Time
	approvalScopeSHA256 string
	workspaceMode       string
	// escalation records a one-call permission upgrade. Every escalated call
	// requires its own user approval and never shares a session grant.
	escalation    *aiWorkspaceEscalation
	networkOrigin string
	// progress carries run_command heartbeat snapshots to the runtime, which
	// persists them as running-status tool run updates.
	progress chan string
}

// aiWorkspaceEscalation captures the session tier a one-call permission
// upgrade started from and the tier it executes under.
type aiWorkspaceEscalation struct {
	From string
	To   string
}

// aiWorkspaceToolSupportsEscalation reports tools that may request a one-call
// permission upgrade from the session tier.
func aiWorkspaceToolSupportsEscalation(name string) bool {
	return name == "write_file" || name == "replace_in_file" || name == "run_command" || name == "terminal_open"
}

// aiWorkspaceModeWider reports whether candidate is strictly wider than base.
func aiWorkspaceModeWider(candidate, base string) bool {
	rank := map[string]int{aiWorkspaceModeReadOnly: 0, aiWorkspaceModeWorkspaceWrite: 1, aiWorkspaceModeFullAccess: 2}
	return rank[candidate] > rank[base]
}

// aiWorkspaceCallMode resolves the effective execution tier for one call:
// the session tier, or an escalated tier when the model requests
// sandbox_permissions. Escalations must widen the session tier strictly and
// are rejected for tools that cannot escalate.
func aiWorkspaceCallMode(sessionMode, toolName string, arguments map[string]any) (string, *aiWorkspaceEscalation, error) {
	raw, present := arguments["sandbox_permissions"]
	if !present {
		if aiWorkspaceModeAllowsTool(sessionMode, toolName) {
			return sessionMode, nil, nil
		}
		return "", nil, errRPCForbidden
	}
	if !aiWorkspaceToolSupportsEscalation(toolName) {
		return "", nil, errRPCInvalid
	}
	// Some providers materialize optional tool arguments with null, an empty
	// string, or their own "use the current sandbox" sentinel. Those values do
	// not request more authority and are therefore equivalent to omission.
	if raw == nil {
		if aiWorkspaceModeAllowsTool(sessionMode, toolName) {
			return sessionMode, nil, nil
		}
		return "", nil, errRPCForbidden
	}
	tier, ok := raw.(string)
	if !ok {
		return "", nil, errRPCInvalid
	}
	tier = strings.TrimSpace(tier)
	if tier == "" || strings.EqualFold(tier, "none") || strings.EqualFold(tier, "use_default") {
		if aiWorkspaceModeAllowsTool(sessionMode, toolName) {
			return sessionMode, nil, nil
		}
		return "", nil, errRPCForbidden
	}
	// A full-access session has no wider tier. Ignore provider-specific string
	// spellings left by stale schemas (for example "disabled" or "read-only")
	// instead of rejecting an otherwise valid command before it can start.
	if sessionMode == aiWorkspaceModeFullAccess {
		if tier == aiWorkspaceModeWorkspaceWrite || tier == aiWorkspaceModeFullAccess {
			return "", nil, errRPCInvalid
		}
		return sessionMode, nil, nil
	}
	if tier != aiWorkspaceModeWorkspaceWrite && tier != aiWorkspaceModeFullAccess {
		return "", nil, errRPCInvalid
	}
	if !aiWorkspaceModeWider(tier, sessionMode) {
		return "", nil, errRPCInvalid
	}
	return tier, &aiWorkspaceEscalation{From: sessionMode, To: tier}, nil
}

type aiWorkspaceToolAuthorization struct {
	Approved        bool
	Decision        string
	AllowForSession bool
	FailureCode     string
}

type aiWorkspaceToolContext struct {
	Project        registeredProject
	ConversationID string
	GenerationID   string
	WorkspaceMode  string
	aiConfig       aiConfig
}

type aiWorkspaceRollbackRecord struct {
	ID               string
	ProjectID        uuid.UUID
	ConversationID   string
	WorkspaceMode    string
	RelativePath     string
	Path             string
	OriginalContents []byte
	OriginalAbsent   bool
	UpdatedHash      string
	CreatedAt        time.Time
}

type aiWorkspaceToolExecutor struct {
	state      *agentState
	supervisor *rawProcessSupervisor
	terminals  *aiPersistentTerminalManager
	now        func() time.Time
	sandbox    aiCommandSandboxPreparer
	web        aiWebToolService

	mu          sync.Mutex
	rollbacks   map[string]aiWorkspaceRollbackRecord
	policyCache sync.Map
}

func newAIWorkspaceToolExecutor(state *agentState, supervisor *rawProcessSupervisor, terminalSupervisor *processSupervisor) *aiWorkspaceToolExecutor {
	return &aiWorkspaceToolExecutor{
		state: state, supervisor: supervisor, terminals: newAIPersistentTerminalManager(terminalSupervisor), now: time.Now, sandbox: prepareAICommandSandbox,
		web: newDefaultAIWebToolService(), rollbacks: make(map[string]aiWorkspaceRollbackRecord),
	}
}

func aiWorkspaceToolDefinitions(mode string) []aiWorkspaceToolDefinition {
	// Escalation-capable tools expose only tiers wider than the current session.
	// Full-access schemas omit sandbox_permissions entirely so providers do not
	// invent empty/default values for an option that cannot have any effect.
	filePathDescription := "相对项目根目录的文件路径。"
	directoryPathDescription := "相对项目根目录的目录路径，默认为项目根目录。"
	workingDirectoryDescription := "相对项目根目录的工作目录。"
	fileScopeDescription := "当前项目内"
	if mode == aiWorkspaceModeFullAccess {
		filePathDescription = "文件路径；可使用绝对路径，或相对项目根目录且允许包含 .. 的路径。"
		directoryPathDescription = "目录路径；可使用绝对路径，或相对项目根目录且允许包含 .. 的路径；默认为项目根目录。"
		workingDirectoryDescription = "工作目录；可使用绝对路径，或相对项目根目录且允许包含 .. 的路径。"
		fileScopeDescription = "本机可访问范围内"
	}
	writeFileProperties := map[string]any{
		"path": aiStringSchema(filePathDescription), "content": aiStringSchema("完整文件内容。"),
		"expected_hash": aiStringSchema("已有文件的 SHA-256；新文件使用 absent 或省略。"),
	}
	aiWorkspaceAddSandboxPermissionSchema(writeFileProperties, mode, "可选的一次性权限升级；只能使用枚举中的精确值。不需要升级时请省略该字段。")
	writeFile := aiWorkspaceToolDefinition{Name: "write_file", Description: "新建或完整覆盖 UTF-8 文本文件；覆盖时必须提交最近一次读取所得 expected_hash。", InputSchema: aiObjectSchema(writeFileProperties, []string{"path", "content"})}
	replaceInFileProperties := map[string]any{
		"path": aiStringSchema(filePathDescription), "old_text": aiStringSchema("完全一致的旧文本。"),
		"new_text": aiStringSchema("替换后的文本。"), "replace_all": map[string]any{"type": "boolean"},
		"expected_hash": aiStringSchema("最近一次 read_file 返回的 SHA-256。"),
	}
	aiWorkspaceAddSandboxPermissionSchema(replaceInFileProperties, mode, "可选的一次性权限升级；只能使用枚举中的精确值。不需要升级时请省略该字段。")
	replaceInFile := aiWorkspaceToolDefinition{Name: "replace_in_file", Description: "在 UTF-8 文本文件中精确替换；默认要求 old_text 只出现一次。", InputSchema: aiObjectSchema(replaceInFileProperties, []string{"path", "old_text", "new_text", "expected_hash"})}
	runCommandProperties := map[string]any{
		"command": aiStringSchema("要执行的 shell 命令。"), "working_directory": aiStringSchema(workingDirectoryDescription),
		"timeout_seconds": aiIntegerSchema(1, 120), "allow_network": map[string]any{"type": "boolean"},
		"background":    map[string]any{"type": "boolean", "default": false, "description": "在后台运行并通过 job 工具跟踪；返回 job_id。"},
		"network_hosts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16},
	}
	aiWorkspaceAddSandboxPermissionSchema(runCommandProperties, mode, "可选的一次性权限升级；只能使用枚举中的精确值。不需要升级时请省略该字段。")
	runCommand := aiWorkspaceToolDefinition{Name: "run_command", Description: "在当前项目或其子目录中通过受管进程执行经用户审批的 shell 命令。", InputSchema: aiObjectSchema(runCommandProperties, []string{"command"})}
	read := []aiWorkspaceToolDefinition{
		{Name: "list_files", Description: "列出" + fileScopeDescription + "的文件和目录；可递归但不会跟随符号链接。", InputSchema: aiObjectSchema(map[string]any{
			"path":      aiStringSchema(directoryPathDescription),
			"recursive": map[string]any{"type": "boolean"}, "max_depth": aiIntegerSchema(1, 8), "limit": aiIntegerSchema(1, 1000),
		}, nil)},
		{Name: "search_files", Description: "在当前项目的文件名和普通文本内容中搜索，不读取依赖、构建产物、密钥文件或二进制文件。", InputSchema: aiObjectSchema(map[string]any{
			"query": aiStringSchema("要搜索的普通文本，不是正则表达式。"), "path": aiStringSchema(directoryPathDescription),
			"file_pattern": aiStringSchema("可选文件通配符，例如 *.go。"), "case_sensitive": map[string]any{"type": "boolean"},
			"max_results": aiIntegerSchema(1, 300),
		}, []string{"query"})},
		{Name: "read_file", Description: "读取" + fileScopeDescription + "的文本文件，返回带行号内容和 SHA-256；密钥文件不可读取。", InputSchema: aiObjectSchema(map[string]any{
			"path": aiStringSchema(filePathDescription), "start_line": aiIntegerSchema(1, 1<<30), "max_lines": aiIntegerSchema(1, 2000),
		}, []string{"path"})},
		{Name: "read_tool_result", Description: "读取此前因超出内联上限而保存为结果产物的工具结果片段。", InputSchema: aiObjectSchema(map[string]any{
			"artifact_id": aiStringSchema("工具结果中标注的 artifact_id。"), "offset": aiIntegerSchema(0, 1<<30), "max_bytes": aiIntegerSchema(1, 64<<10),
		}, []string{"artifact_id"})},
		{Name: "read_image", Description: "读取" + fileScopeDescription + "的 PNG/JPEG/WebP/GIF 图片并返回图像内容。", InputSchema: aiObjectSchema(map[string]any{
			"path": aiStringSchema(filePathDescription),
		}, []string{"path"})},
		{Name: "web_search", Description: "使用 DeepSeek 原生网页搜索查找当前或外部信息，返回可引用的来源 URL 与摘要。", InputSchema: aiObjectSchema(map[string]any{
			"query": aiStringSchema("具体、完整的网页搜索关键词。"),
		}, []string{"query"})},
		{Name: "web_fetch", Description: "匿名抓取一个公开 HTTP(S) 网页并转换为适合阅读的文本；不携带浏览器 Cookie。", InputSchema: aiObjectSchema(map[string]any{
			"url": aiStringSchema("要抓取的完整公开 HTTP(S) URL。"),
		}, []string{"url"})},
	}
	if mode == aiWorkspaceModeReadOnly {
		// Read-only sessions may request one-call write/command escalations;
		// every escalated call still requires a separate user approval.
		return append(read, writeFile, replaceInFile, runCommand)
	}
	if mode != aiWorkspaceModeWorkspaceWrite && mode != aiWorkspaceModeFullAccess {
		return nil
	}
	terminalOpenProperties := map[string]any{
		"type": map[string]any{"type": "string", "enum": []string{"shell"}, "description": "Terminal backend type; currently shell."},
		"name": aiStringSchema("Optional conversation-local display name."), "working_directory": aiStringSchema(workingDirectoryDescription),
		"allow_network": map[string]any{"type": "boolean"}, "network_hosts": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 16},
	}
	aiWorkspaceAddSandboxPermissionSchema(terminalOpenProperties, mode, "Optional one-call permission escalation. Use only an exact enum value and omit this field when no escalation is needed.")
	terminals := []aiWorkspaceToolDefinition{
		{Name: "terminal_open", Description: "Create an owner-isolated persistent PTY shell whose cwd, environment, REPL, and subprocess state survive across AI tool calls.", InputSchema: aiObjectSchema(terminalOpenProperties, []string{"type"})},
		{Name: "terminal_send", Description: "Send UTF-8 text to a persistent terminal. Inferred idle or timeout does not mean that the subprocess exited.", InputSchema: aiObjectSchema(map[string]any{
			"session_id": aiStringSchema("Session id returned by terminal_open or terminal_list."), "text": aiStringSchema("UTF-8 text to write."),
			"submit": map[string]any{"type": "boolean"}, "timeout_seconds": aiIntegerSchema(1, 120),
		}, []string{"session_id", "text"})},
		{Name: "terminal_read", Description: "Read a bounded newest-relative page of retained terminal output without sending input.", InputSchema: aiObjectSchema(map[string]any{
			"session_id": aiStringSchema("Terminal session id."), "offset": aiIntegerSchema(0, 1<<30), "count": aiIntegerSchema(1, 2000),
		}, []string{"session_id"})},
		{Name: "terminal_signal", Description: "Deliver SIGINT to foreground work or terminate the persistent shell with SIGTERM/SIGHUP.", InputSchema: aiObjectSchema(map[string]any{
			"session_id": aiStringSchema("Terminal session id."), "signal": map[string]any{"type": "string", "enum": []string{"SIGINT", "SIGTERM", "SIGHUP"}},
		}, []string{"session_id", "signal"})},
		{Name: "terminal_close", Description: "Close a persistent terminal and wait until its owned process tree has exited.", InputSchema: aiObjectSchema(map[string]any{
			"session_id": aiStringSchema("Terminal session id."),
		}, []string{"session_id"})},
		{Name: "terminal_list", Description: "List persistent terminals owned by this conversation, project, and permission mode.", InputSchema: aiObjectSchema(map[string]any{}, nil)},
	}
	return append(append(read, terminals...),
		writeFile, replaceInFile,
		aiWorkspaceToolDefinition{Name: "rollback_file_change", Description: "按 rollback_id 回滚一次 AI 文件修改；当前文件 hash 不一致时拒绝覆盖。", InputSchema: aiObjectSchema(map[string]any{
			"rollback_id":   aiStringSchema("write_file 或 replace_in_file 返回的回滚 ID。"),
			"expected_hash": aiStringSchema("当前文件 SHA-256。"),
		}, []string{"rollback_id", "expected_hash"})},
		runCommand,
	)
}

func aiObjectSchema(properties map[string]any, required []string) map[string]any {
	result := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		result["required"] = required
	}
	return result
}

func aiStringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func aiIntegerSchema(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

func aiWorkspaceAddSandboxPermissionSchema(properties map[string]any, mode, description string) {
	var tiers []string
	switch mode {
	case aiWorkspaceModeReadOnly:
		tiers = []string{aiWorkspaceModeWorkspaceWrite, aiWorkspaceModeFullAccess}
	case aiWorkspaceModeWorkspaceWrite:
		tiers = []string{aiWorkspaceModeFullAccess}
	}
	if len(tiers) == 0 {
		return
	}
	properties["sandbox_permissions"] = map[string]any{
		"type": "string", "enum": tiers, "description": description,
	}
}

func (executor *aiWorkspaceToolExecutor) Plan(ctx context.Context, workspace aiWorkspaceToolContext, call aiWorkspaceToolCall) (aiWorkspaceToolPlan, error) {
	if err := validateAIWorkspaceToolContext(workspace); err != nil || ctx.Err() != nil {
		return aiWorkspaceToolPlan{}, firstError(ctx.Err(), err)
	}
	if !validAIWorkspaceToolCall(call) {
		return aiWorkspaceToolPlan{}, errRPCForbidden
	}
	effectiveMode, escalation, err := aiWorkspaceCallMode(workspace.WorkspaceMode, call.Name, call.Arguments)
	if err != nil || !aiWorkspaceModeAllowsTool(effectiveMode, call.Name) {
		return aiWorkspaceToolPlan{}, firstError(err, errRPCForbidden)
	}
	argumentsHash, err := aiWorkspaceJSONHash(call.Arguments)
	if err != nil {
		return aiWorkspaceToolPlan{}, errRPCInvalid
	}
	plan := aiWorkspaceToolPlan{
		Call: call, Preview: aiWorkspaceApprovalPreview{ArgumentsSHA256: argumentsHash},
		auditID: uuid.NewString(), auditStartedAt: executor.now().UTC(), workspaceMode: effectiveMode, escalation: escalation,
	}
	switch call.Name {
	case "web_search":
		query, err := aiWorkspaceString(call.Arguments, "query", false, 16<<10)
		if err != nil || strings.TrimSpace(query) == "" || utf8.RuneCountInString(strings.TrimSpace(query)) > 4096 || executor.web == nil {
			return aiWorkspaceToolPlan{}, firstError(err, errRPCInvalid)
		}
		endpoint, err := executor.web.searchEndpoint(workspace.aiConfig)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.networkOrigin = aiWebOrigin(endpoint)
		plan.RequiresApproval = workspace.WorkspaceMode != aiWorkspaceModeFullAccess
		plan.Preview = aiWorkspaceApprovalPreview{
			Title: "搜索网页：" + aiWorkspaceShorten(query, 100), Description: "查询 DeepSeek 原生网页搜索服务，并把外部结果作为不可信数据返回。",
			NetworkHosts: []string{endpoint.Hostname()}, ArgumentsSHA256: argumentsHash, AllowForSession: true, Risk: "openWorld",
		}
	case "web_fetch":
		rawURL, err := aiWorkspaceString(call.Arguments, "url", false, aiWebFetchMaximumURLLength)
		if err != nil || executor.web == nil {
			return aiWorkspaceToolPlan{}, firstError(err, errRPCInvalid)
		}
		target, err := executor.web.validateFetchURL(rawURL)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.networkOrigin = aiWebOrigin(target)
		plan.RequiresApproval = workspace.WorkspaceMode != aiWorkspaceModeFullAccess
		plan.Preview = aiWorkspaceApprovalPreview{
			Title: "抓取网页：" + aiWorkspaceShorten(target.String(), 100), Description: "匿名访问公开网页；每次连接都会拒绝本机、内网及保留 IP，并且只跟随同源重定向。",
			NetworkHosts: []string{target.Hostname()}, ArgumentsSHA256: argumentsHash, AllowForSession: true, Risk: "openWorld",
		}
	case "read_tool_result":
		artifactID, err := aiWorkspaceString(call.Arguments, "artifact_id", false, 64)
		if err != nil || uuid.Validate(artifactID) != nil {
			return aiWorkspaceToolPlan{}, firstError(err, errRPCInvalid)
		}
		if _, err := aiWorkspaceInt(call.Arguments, "offset", 0, 0, 1<<30); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if _, err := aiWorkspaceInt(call.Arguments, "max_bytes", 16384, 1, 64<<10); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.Preview = aiWorkspaceApprovalPreview{
			Title: "读取工具结果产物 " + artifactID[:8], Description: "读取此前因超出内联上限而落盘保存的工具结果片段。",
			ArgumentsSHA256: argumentsHash, Risk: "readOnly",
		}
	case "list_files", "search_files", "read_file", "read_image", "write_file", "replace_in_file":
		path, err := aiWorkspaceString(call.Arguments, "path", call.Name == "list_files" || call.Name == "search_files", 4096)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		resolved, normalized, err := resolveAIWorkspaceToolPath(workspace.Project, path, call.Name == "write_file", plan.workspaceMode)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.resolvedPath = resolved
		plan.Preview.RelativePaths = []string{normalized}
		plan.Preview.Title = aiWorkspaceToolTitle(call.Name, normalized)
		external := filepath.IsAbs(filepath.FromSlash(normalized)) || filepath.VolumeName(filepath.FromSlash(normalized)) != ""
		if call.Name == "write_file" || call.Name == "replace_in_file" {
			plan.RequiresApproval = workspace.WorkspaceMode != aiWorkspaceModeFullAccess
			plan.Preview.Risk, plan.Preview.AllowForSession = "writesData", true
			plan.Preview.Description = "将修改当前项目内的文件；执行前会再次校验 SHA-256。"
			if external {
				plan.Preview.Description = "Full Access 将修改项目外的绝对路径文件；执行前会再次校验 SHA-256。"
			}
			if plan.escalation != nil {
				plan.Preview.AllowForSession = false
				plan.Preview.Description += " 该次调用已申请一次性权限升级至 " + plan.escalation.To + "，仅本次生效并需单独审批。"
			}
		} else {
			plan.Preview.Risk, plan.Preview.Description = "readOnly", "只读取当前项目内的内容。"
			if external {
				plan.Preview.Description = "Full Access 只读取项目外的绝对路径内容。"
			}
		}
	case "rollback_file_change":
		record, err := executor.rollbackFor(workspace, call.Arguments)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.resolvedPath = record.Path
		plan.RequiresApproval = workspace.WorkspaceMode != aiWorkspaceModeFullAccess
		plan.Preview = aiWorkspaceApprovalPreview{
			Title: "回滚 " + record.RelativePath, Description: "恢复该次 AI 修改前的文件版本。", RelativePaths: []string{record.RelativePath},
			ArgumentsSHA256: argumentsHash, AllowForSession: true, Risk: "writesData",
		}
	case "terminal_open":
		if executor.terminals == nil {
			return aiWorkspaceToolPlan{}, errRPCCapability
		}
		plan.terminalOwner = aiTerminalOwnerFor(workspace)
		terminalType, err := aiWorkspaceString(call.Arguments, "type", false, 16)
		if err != nil || terminalType != "shell" {
			return aiWorkspaceToolPlan{}, errRPCInvalid
		}
		name, err := aiWorkspaceString(call.Arguments, "name", true, 80)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		workingRelative, err := aiWorkspaceString(call.Arguments, "working_directory", true, 4096)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		workingDirectory, normalized, err := resolveAIWorkspaceToolPath(workspace.Project, workingRelative, false, plan.workspaceMode)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		info, err := os.Stat(workingDirectory)
		if err != nil || !info.IsDir() {
			return aiWorkspaceToolPlan{}, firstError(err, errRPCInvalid)
		}
		requestedNetwork, err := aiWorkspaceBool(call.Arguments, "allow_network", false)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		allowNetwork := requestedNetwork || plan.workspaceMode == aiWorkspaceModeFullAccess
		scopedNetworkHosts := aiWorkspaceNetworkHostsAreScoped(call.Arguments, requestedNetwork, plan.workspaceMode)
		hosts, err := aiWorkspaceNetworkHosts(call.Arguments, scopedNetworkHosts)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		argv, shell, err := terminalShellArgv("")
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if executor.sandbox == nil {
			return aiWorkspaceToolPlan{}, errAICommandSandboxUnavailable
		}
		sandboxRoot := workspace.Project.LocalPath
		if plan.workspaceMode == aiWorkspaceModeFullAccess {
			sandboxRoot = workingDirectory
		}
		launch, err := executor.sandbox(aiCommandSandboxRequest{
			Mode: plan.workspaceMode, WorkspaceRoot: sandboxRoot,
			WorkingDirectory: workingDirectory, Argv: argv, AllowNetwork: allowNetwork,
		})
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.commandLaunch, plan.terminalName, plan.terminalShell, plan.terminalDisplayCWD = launch, name, shell, normalized
		plan.RequiresApproval = workspace.WorkspaceMode != aiWorkspaceModeFullAccess
		description := "启动一个仅属于当前会话、项目和权限模式的持久 PTY shell。"
		if allowNetwork && plan.workspaceMode != aiWorkspaceModeFullAccess {
			description += " 获批网络将整体开放；终端输入中出现的主机名必须落在 network_hosts 白名单内。"
		}
		if plan.escalation != nil {
			description += " 该次调用已申请一次性权限升级至 " + plan.escalation.To + "，仅本次生效并需单独审批。"
		}
		plan.Preview = aiWorkspaceApprovalPreview{
			Title: "打开持久终端", Description: description, Command: strings.Join(argv, " "),
			WorkingDirectory: normalized, NetworkHosts: hosts, ArgumentsSHA256: argumentsHash,
			AllowForSession: false, Risk: "openWorld", SandboxStatus: launch.Status,
			EnvironmentPolicy: "只保留经审查的最小终端环境；不继承 Agent 密钥变量。",
		}
	case "terminal_send":
		if executor.terminals == nil {
			return aiWorkspaceToolPlan{}, errRPCCapability
		}
		plan.terminalOwner = aiTerminalOwnerFor(workspace)
		sessionID, err := aiWorkspaceTerminalSessionID(call.Arguments)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		text, err := aiWorkspaceTerminalText(call.Arguments)
		if err != nil || catastrophicAIWorkspaceCommand(text) {
			return aiWorkspaceToolPlan{}, errRPCInvalid
		}
		if _, err := aiWorkspaceBool(call.Arguments, "submit", true); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if _, err := aiWorkspaceInt(call.Arguments, "timeout_seconds", 10, 1, 120); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		view, err := executor.terminals.Inspect(aiTerminalOwnerFor(workspace), sessionID)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if view.Status["kind"] != "running" {
			return aiWorkspaceToolPlan{}, errRPCRevision
		}
		if !view.NetworkAllowed && looksLikeAIWorkspaceNetworkCommand(text) {
			return aiWorkspaceToolPlan{}, errRPCForbidden
		}
		if view.NetworkAllowed && len(view.NetworkHosts) > 0 && !enforceAIWorkspaceNetworkHosts(text, view.NetworkHosts) {
			return aiWorkspaceToolPlan{}, errRPCForbidden
		}
		plan.terminalSessionID = sessionID
		plan.RequiresApproval = workspace.WorkspaceMode != aiWorkspaceModeFullAccess
		plan.Preview = aiWorkspaceApprovalPreview{
			Title: "向持久终端输入", Description: "返回 inferred_idle 或 timeout 时，进程可能仍在运行。",
			Command: text, WorkingDirectory: view.CWD, NetworkHosts: append([]string(nil), view.NetworkHosts...), ArgumentsSHA256: argumentsHash,
			AllowForSession: !view.NetworkAllowed && !highRiskAIWorkspaceCommand(text), Risk: "openWorld", SandboxStatus: view.SandboxStatus,
		}
	case "terminal_read":
		if executor.terminals == nil {
			return aiWorkspaceToolPlan{}, errRPCCapability
		}
		plan.terminalOwner = aiTerminalOwnerFor(workspace)
		sessionID, err := aiWorkspaceTerminalSessionID(call.Arguments)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if _, err := aiWorkspaceInt(call.Arguments, "offset", 0, 0, 1<<30); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if _, err := aiWorkspaceInt(call.Arguments, "count", 500, 1, 2000); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if _, err := executor.terminals.Inspect(aiTerminalOwnerFor(workspace), sessionID); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.terminalSessionID = sessionID
		plan.Preview = aiWorkspaceApprovalPreview{Title: "读取持久终端", Description: "只读已保留的终端输出。", ArgumentsSHA256: argumentsHash, Risk: "readOnly"}
	case "terminal_list":
		if executor.terminals == nil {
			return aiWorkspaceToolPlan{}, errRPCCapability
		}
		plan.terminalOwner = aiTerminalOwnerFor(workspace)
		plan.Preview = aiWorkspaceApprovalPreview{Title: "列出持久终端", Description: "仅列出当前会话、项目与权限边界内的会话。", ArgumentsSHA256: argumentsHash, Risk: "readOnly"}
	case "terminal_signal", "terminal_close":
		if executor.terminals == nil {
			return aiWorkspaceToolPlan{}, errRPCCapability
		}
		plan.terminalOwner = aiTerminalOwnerFor(workspace)
		sessionID, err := aiWorkspaceTerminalSessionID(call.Arguments)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		view, err := executor.terminals.Inspect(aiTerminalOwnerFor(workspace), sessionID)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if call.Name == "terminal_signal" {
			signal, err := aiWorkspaceString(call.Arguments, "signal", false, 16)
			if err != nil || !slices.Contains([]string{"SIGINT", "SIGTERM", "SIGHUP"}, signal) {
				return aiWorkspaceToolPlan{}, errRPCInvalid
			}
		}
		plan.terminalSessionID = sessionID
		title, description := "发送终端信号", "向该持久终端发送受支持的信号。"
		if call.Name == "terminal_close" {
			title, description = "关闭持久终端", "终止并回收该终端的完整进程树。"
		}
		plan.Preview = aiWorkspaceApprovalPreview{Title: title, Description: description, WorkingDirectory: view.CWD, ArgumentsSHA256: argumentsHash, Risk: "destructive", SandboxStatus: view.SandboxStatus}
	case "run_command":
		command, err := aiWorkspaceString(call.Arguments, "command", false, maximumAIWorkspaceCommandBytes)
		if err != nil || strings.TrimSpace(command) == "" || strings.IndexByte(command, 0) >= 0 || catastrophicAIWorkspaceCommand(command) {
			return aiWorkspaceToolPlan{}, errRPCInvalid
		}
		if _, err := aiWorkspaceBool(call.Arguments, "background", false); err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		workingRelative, err := aiWorkspaceString(call.Arguments, "working_directory", true, 4096)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		workingDirectory, normalized, err := resolveAIWorkspaceToolPath(workspace.Project, workingRelative, false, plan.workspaceMode)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		info, err := os.Stat(workingDirectory)
		if err != nil || !info.IsDir() {
			return aiWorkspaceToolPlan{}, firstError(err, errRPCInvalid)
		}
		requestedNetwork, err := aiWorkspaceBool(call.Arguments, "allow_network", false)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		allowNetwork := requestedNetwork || plan.workspaceMode == aiWorkspaceModeFullAccess
		scopedNetworkHosts := aiWorkspaceNetworkHostsAreScoped(call.Arguments, requestedNetwork, plan.workspaceMode)
		hosts, err := aiWorkspaceNetworkHosts(call.Arguments, scopedNetworkHosts)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if plan.workspaceMode != aiWorkspaceModeFullAccess && !requestedNetwork && looksLikeAIWorkspaceNetworkCommand(command) {
			return aiWorkspaceToolPlan{}, errRPCForbidden
		}
		if requestedNetwork && plan.workspaceMode != aiWorkspaceModeFullAccess && !enforceAIWorkspaceNetworkHosts(command, hosts) {
			return aiWorkspaceToolPlan{}, errRPCForbidden
		}
		argv, err := aiWorkspaceCommandArgv(command)
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		if executor.sandbox == nil {
			return aiWorkspaceToolPlan{}, errAICommandSandboxUnavailable
		}
		sandboxRoot := workspace.Project.LocalPath
		if plan.workspaceMode == aiWorkspaceModeFullAccess {
			sandboxRoot = workingDirectory
		}
		launch, err := executor.sandbox(aiCommandSandboxRequest{
			Mode: plan.workspaceMode, WorkspaceRoot: sandboxRoot,
			WorkingDirectory: workingDirectory, Argv: argv, AllowNetwork: allowNetwork,
		})
		if err != nil {
			return aiWorkspaceToolPlan{}, err
		}
		plan.commandLaunch = launch
		plan.RequiresApproval = workspace.WorkspaceMode != aiWorkspaceModeFullAccess
		description := "命令将在清理后的最小环境中由受管进程和设备端权限沙箱执行。"
		if plan.workspaceMode == aiWorkspaceModeFullAccess {
			description = "Full Access 已关闭文件与网络沙箱；命令仍由受管进程执行并应用资源上限。"
		} else if allowNetwork {
			description += " 获批网络将整体开放；命令中出现的主机名必须落在 network_hosts 白名单内。"
		}
		allowForSession := !allowNetwork && !highRiskAIWorkspaceCommand(command)
		if plan.escalation != nil {
			description += " 该次调用已申请一次性权限升级至 " + plan.escalation.To + "，仅本次生效并需单独审批。"
			allowForSession = false
		}
		plan.Preview = aiWorkspaceApprovalPreview{
			Title: "执行命令", Description: description, Command: command,
			WorkingDirectory: normalized, NetworkHosts: hosts, ArgumentsSHA256: argumentsHash,
			AllowForSession: allowForSession, Risk: "openWorld",
			SandboxStatus: launch.Status, EnvironmentPolicy: "仅保留受审查的运行时环境变量；不继承 Agent 密钥变量。",
		}
	default:
		if strings.HasPrefix(call.Name, "mcp__") {
			if _, found := aiMCPToolForName(call.Name); !found {
				return aiWorkspaceToolPlan{}, errRPCNotFound
			}
			plan.Preview = aiWorkspaceApprovalPreview{
				Title: "调用外部工具 " + call.Name, Description: "调用本地 MCP 服务器工具；返回内容为不可信外部数据。",
				ArgumentsSHA256: argumentsHash, Risk: "readOnly",
			}
			break
		}
		return aiWorkspaceToolPlan{}, errRPCNotFound
	}
	plan.approvalScopeSHA256, err = aiWorkspaceApprovalScopeHash(plan)
	if err != nil {
		return aiWorkspaceToolPlan{}, err
	}
	return plan, nil
}

func aiWorkspaceApprovalScopeHash(plan aiWorkspaceToolPlan) (string, error) {
	scope := map[string]any{"tool": plan.Call.Name}
	if plan.escalation != nil {
		scope["escalation"] = map[string]any{"from": plan.escalation.From, "to": plan.escalation.To}
	}
	if plan.Call.Name == "web_search" || plan.Call.Name == "web_fetch" {
		scope["networkOrigin"] = plan.networkOrigin
		scope["networkHosts"] = plan.Preview.NetworkHosts
	}
	if len(plan.Preview.RelativePaths) > 0 {
		scope["relativePaths"] = plan.Preview.RelativePaths
		if plan.Call.Name == "write_file" || plan.Call.Name == "replace_in_file" || plan.Call.Name == "rollback_file_change" {
			scope["tool"] = "write_workspace"
		}
	}
	if plan.Call.Name == "run_command" {
		scope["commandHash"] = aiWorkspaceBytesHash([]byte(plan.Preview.Command))
		scope["workingDirectory"] = plan.Preview.WorkingDirectory
		scope["networkHosts"] = plan.Preview.NetworkHosts
		scope["networkAllowed"] = plan.commandLaunch.NetworkAllowed
		scope["sandboxMode"] = plan.commandLaunch.SandboxMode
	}
	if plan.Call.Name == "terminal_open" {
		scope["workingDirectory"] = plan.Preview.WorkingDirectory
		scope["name"] = plan.terminalName
		scope["networkHosts"] = plan.Preview.NetworkHosts
		scope["networkAllowed"] = plan.commandLaunch.NetworkAllowed
		scope["sandboxMode"] = plan.commandLaunch.SandboxMode
	}
	if strings.HasPrefix(plan.Call.Name, "terminal_") {
		scope["terminalOwner"] = map[string]any{
			"projectId": plan.terminalOwner.ProjectID.String(), "projectRoot": plan.terminalOwner.ProjectRoot,
			"conversationId": plan.terminalOwner.ConversationID, "workspaceMode": plan.terminalOwner.WorkspaceMode,
		}
	}
	if strings.HasPrefix(plan.Call.Name, "terminal_") && plan.terminalSessionID != uuid.Nil {
		scope["sessionId"] = plan.terminalSessionID.String()
	}
	if plan.Call.Name == "terminal_send" {
		scope["commandHash"] = aiWorkspaceBytesHash([]byte(plan.Preview.Command))
		scope["networkHosts"] = plan.Preview.NetworkHosts
	}
	return aiWorkspaceJSONHash(scope)
}

func (executor *aiWorkspaceToolExecutor) Execute(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan, approved bool) aiWorkspaceToolResult {
	decision := ""
	if plan.RequiresApproval {
		decision = "deny"
		if approved {
			decision = "allow_once"
		}
	}
	return executor.ExecuteAuthorized(ctx, workspace, plan, aiWorkspaceToolAuthorization{Approved: approved, Decision: decision})
}

func (executor *aiWorkspaceToolExecutor) ExecuteAuthorized(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan, authorization aiWorkspaceToolAuthorization) aiWorkspaceToolResult {
	if ctx.Err() != nil {
		return aiWorkspaceToolFailure("工具执行已取消。", "cancelled")
	}
	if validateAIWorkspaceToolContext(workspace) != nil || plan.workspaceMode != workspace.WorkspaceMode && (plan.escalation == nil || plan.escalation.From != workspace.WorkspaceMode || plan.escalation.To != plan.workspaceMode) {
		return aiWorkspaceToolFailure("工具计划与当前权限模式不匹配。", "permission_mode_mismatch")
	}
	argumentsHash, hashErr := aiWorkspaceJSONHash(plan.Call.Arguments)
	if hashErr != nil || argumentsHash != plan.Preview.ArgumentsSHA256 {
		return aiWorkspaceToolFailure("工具参数在计划生成后发生了变化。", "tool_plan_changed")
	}
	if plan.RequiresApproval && !authorization.Approved {
		result := aiWorkspaceToolFailure("操作需要用户审批。", "approval_required")
		outcome := "denied"
		if authorization.Decision == "cancelled" {
			outcome = "cancelled"
		}
		if err := executor.finishAudit(workspace, plan, result, outcome, firstNonEmpty(authorization.Decision, "deny"), false); err != nil {
			return aiWorkspaceToolFailure("本地审计不可用；高风险工具已拒绝执行。", "audit_unavailable")
		}
		return result
	}
	if err := executor.startAudit(workspace, plan, authorization); err != nil {
		return aiWorkspaceToolFailure("本地审计不可用；工具已拒绝执行。", "audit_unavailable")
	}
	var result aiWorkspaceToolResult
	var err error
	switch plan.Call.Name {
	case "web_search":
		result, err = executor.webSearch(ctx, workspace, plan)
	case "web_fetch":
		result, err = executor.webFetch(ctx, workspace, plan)
	case "list_files":
		result, err = executor.listFiles(ctx, workspace, plan)
	case "search_files":
		result, err = executor.searchFiles(ctx, workspace, plan)
	case "read_file":
		result, err = executor.readFile(ctx, workspace, plan)
	case "read_image":
		result, err = executor.readImage(ctx, workspace, plan)
	case "read_tool_result":
		result, err = executor.readToolResult(ctx, workspace, plan)
	case "write_file", "replace_in_file":
		result, err = executor.writeFile(ctx, workspace, plan)
	case "rollback_file_change":
		result, err = executor.rollbackFile(ctx, workspace, plan)
	case "terminal_open":
		result, err = executor.openTerminal(ctx, workspace, plan)
	case "terminal_send":
		result, err = executor.sendTerminal(ctx, workspace, plan)
	case "terminal_read":
		result, err = executor.readTerminal(workspace, plan)
	case "terminal_signal":
		result, err = executor.signalTerminal(workspace, plan)
	case "terminal_close":
		result, err = executor.closeTerminal(workspace, plan)
	case "terminal_list":
		result, err = executor.listTerminals(workspace)
	case "run_command":
		result, err = executor.runCommand(ctx, workspace, plan)
	default:
		if strings.HasPrefix(plan.Call.Name, "mcp__") {
			result, err = executor.callMCPTool(ctx, plan)
			break
		}
		err = errRPCNotFound
	}
	if err != nil {
		result = aiWorkspaceToolFailure(aiWorkspaceStableError(err), aiWorkspaceErrorCode(err))
		outcome := "failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = "cancelled"
		}
		_ = executor.finishAudit(workspace, plan, result, outcome, authorization.Decision, authorization.AllowForSession)
		return result
	}
	result = executor.boundAIWorkspaceToolResult(workspace, plan, result)
	_ = executor.finishAudit(workspace, plan, result, map[bool]string{true: "failed", false: "succeeded"}[result.IsError], authorization.Decision, authorization.AllowForSession)
	return result
}

func limitAIWorkspaceToolResult(result aiWorkspaceToolResult) aiWorkspaceToolResult {
	if len(result.Content) <= maximumAIWorkspaceToolResult {
		return result
	}
	limit := maximumAIWorkspaceToolResult - len("\n[工具结果已截断]")
	for limit > 0 && !utf8.ValidString(result.Content[:limit]) {
		limit--
	}
	result.Content = result.Content[:limit] + "\n[工具结果已截断]"
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["truncated"] = true
	return result
}

// aiWorkspaceToolSpillExempt reports tools whose results page natively and
// must not spill, preventing a read→spill→read loop (DSH spill-policy).
func aiWorkspaceToolSpillExempt(name string) bool {
	return name == "read_file" || name == "terminal_read" || name == "terminal_list"
}

// boundAIWorkspaceToolResult replaces the hard tail cut for oversized tool
// results: the full text is saved as a conversation-scoped artifact and the
// model receives a bounded head/tail preview plus the artifact locator. A
// storage failure falls back to the plain truncation and never turns a
// successful tool call into an error.
func (executor *aiWorkspaceToolExecutor) boundAIWorkspaceToolResult(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan, result aiWorkspaceToolResult) aiWorkspaceToolResult {
	if result.IsError || aiWorkspaceToolSpillExempt(plan.Call.Name) || len(result.Content) <= maximumAIWorkspaceToolResult {
		return limitAIWorkspaceToolResult(result)
	}
	if executor == nil || executor.state == nil || executor.state.business == nil {
		return limitAIWorkspaceToolResult(result)
	}
	artifactID, err := executor.state.business.saveAIToolResultArtifact(context.Background(), workspace.ConversationID, plan.Call.ID, []byte(result.Content))
	if err != nil {
		return limitAIWorkspaceToolResult(result)
	}
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["artifact_id"] = artifactID
	result.Metadata["truncated"] = true
	result.Content = aiWorkspaceSpillPreview(result.Content, artifactID)
	return result
}

// aiWorkspaceSpillPreview builds the bounded head/tail preview that replaces
// the full text in the model context. Total length stays within
// maximumAIWorkspaceToolResult.
func aiWorkspaceSpillPreview(content, artifactID string) string {
	marker := fmt.Sprintf("\n\n[工具结果共 %d 字节，已完整保存为结果产物 artifact_id=%q；使用 read_tool_result 分段读取中间内容]\n\n", len(content), artifactID)
	budget := maximumAIWorkspaceToolResult - len(marker)
	if budget < 512 {
		return truncateAIUTF8(content, maximumAIWorkspaceToolResult)
	}
	headBytes := budget / 2
	tailBytes := budget - headBytes
	return truncateAIUTF8(content, headBytes) + marker + aiWorkspaceUTF8Suffix(content, tailBytes)
}

func aiWorkspaceUTF8Suffix(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	start := len(value) - maximum
	for start < len(value) && !utf8.ValidString(value[start:]) {
		start++
	}
	return value[start:]
}

func (executor *aiWorkspaceToolExecutor) callMCPTool(ctx context.Context, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	entry, found := aiMCPToolForName(plan.Call.Name)
	if !found {
		return aiWorkspaceToolResult{}, errRPCNotFound
	}
	if ctx.Err() != nil {
		return aiWorkspaceToolResult{}, ctx.Err()
	}
	client := aiMCPEnsureClient(entry.ServerConfig)
	if client == nil {
		return aiWorkspaceToolResult{}, errRPCCapability
	}
	arguments := plan.Call.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	callContext, cancel := context.WithTimeout(ctx, aiMCPCallTimeout)
	defer cancel()
	text, isError, err := client.callTool(callContext, entry.Tool.Name, arguments)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	result := aiWorkspaceToolSuccess(text, "外部工具 "+entry.Tool.Name+" 已返回", map[string]any{
		"source_kind": "mcp", "untrusted": true, "mcp_server": entry.ServerConfig.Name, "mcp_tool": entry.Tool.Name,
	})
	if isError {
		result.IsError = true
		result.Summary = "外部工具报告错误"
	}
	return result, nil
}

func (executor *aiWorkspaceToolExecutor) readImage(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.sensitiveName(workspace.Project, filepath.Base(plan.resolvedPath)) {
		return aiWorkspaceToolResult{}, errRPCForbidden
	}
	mimeType, supported := aiWorkspaceImageMimeTypes[strings.ToLower(filepath.Ext(plan.resolvedPath))]
	if !supported {
		return aiWorkspaceToolResult{}, newAIWebError("仅支持 PNG、JPEG、WebP 和 GIF 图片。", "image_unsupported")
	}
	manager := fileRPCManagerFor(executor.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	info, err := os.Stat(plan.resolvedPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumAIWorkspaceImageBytes {
		return aiWorkspaceToolResult{}, firstError(err, errRPCInvalid)
	}
	data, err := readBoundedFile(plan.resolvedPath, maximumAIWorkspaceImageBytes)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return aiWorkspaceToolResult{}, err
	}
	hash := aiWorkspaceBytesHash(data)
	relative := plan.Preview.RelativePaths[0]
	encoded, _ := json.Marshal(map[string]any{
		"image": relative, "mime_type": mimeType, "bytes": len(data), "content_hash": hash,
	})
	result := aiWorkspaceToolSuccess(string(encoded), fmt.Sprintf("已读取图片 %s（%d 字节）", relative, len(data)), map[string]any{
		"image_path": relative, "mime_type": mimeType, "content_hash": hash, "image_bytes": len(data),
	})
	result.Image = &aiPromptImage{Name: filepath.Base(plan.resolvedPath), MimeType: mimeType, Base64Data: base64.StdEncoding.EncodeToString(data)}
	return result, nil
}

func (executor *aiWorkspaceToolExecutor) readToolResult(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	artifactID, err := aiWorkspaceString(plan.Call.Arguments, "artifact_id", false, 64)
	if err != nil || uuid.Validate(artifactID) != nil {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	offset, err := aiWorkspaceInt(plan.Call.Arguments, "offset", 0, 0, 1<<30)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	maxBytes, err := aiWorkspaceInt(plan.Call.Arguments, "max_bytes", 16384, 1, 64<<10)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	if ctx.Err() != nil {
		return aiWorkspaceToolResult{}, ctx.Err()
	}
	if executor == nil || executor.state == nil || executor.state.business == nil {
		return aiWorkspaceToolResult{}, errRPCCapability
	}
	content, totalBytes, err := executor.state.business.readAIToolResultArtifact(ctx, workspace.ConversationID, artifactID)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	start := min(offset, len(content))
	end := min(start+maxBytes, len(content))
	piece := content[start:end]
	for end < len(content) && !utf8.Valid(piece) {
		end++
		piece = content[start:end]
	}
	for start > 0 && !utf8.Valid(piece) {
		start--
		piece = content[start:end]
	}
	if !utf8.Valid(piece) {
		piece = []byte(truncateAIUTF8(string(piece), len(piece)))
	}
	var output strings.Builder
	fmt.Fprintf(&output, "[artifact_id=%s total_bytes=%d offset=%d next_offset=%d]\n", artifactID, totalBytes, start, end)
	output.Write(piece)
	if end < len(content) {
		fmt.Fprintf(&output, "\n[还有 %d 字节，使用 offset=%d 继续读取]", len(content)-end, end)
	}
	return aiWorkspaceToolSuccess(output.String(), fmt.Sprintf("已读取结果产物 %d 字节", end-start), map[string]any{
		"artifact_id": artifactID, "offset": start, "next_offset": end, "total_bytes": totalBytes,
	}), nil
}

func (executor *aiWorkspaceToolExecutor) startAudit(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan, authorization aiWorkspaceToolAuthorization) error {
	if executor == nil || executor.state == nil || executor.state.business == nil {
		return errRPCCapability
	}
	record, err := executor.auditRecord(workspace, plan)
	if err != nil {
		return err
	}
	record.Outcome = "running"
	record.ApprovalDecision = authorization.Decision
	record.AllowForSession = authorization.AllowForSession
	return executor.state.business.recordAIToolAudit(context.Background(), record)
}

func (executor *aiWorkspaceToolExecutor) finishAudit(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan, result aiWorkspaceToolResult, outcome, decision string, allowForSession bool) error {
	if executor == nil || executor.state == nil || executor.state.business == nil {
		return errRPCCapability
	}
	record, err := executor.auditRecord(workspace, plan)
	if err != nil {
		return err
	}
	resultHash, err := aiWorkspaceJSONHash(result)
	if err != nil {
		return err
	}
	finished := executor.now().UTC()
	if finished.Before(record.StartedAt) {
		finished = record.StartedAt
	}
	record.ResultSHA256, record.Outcome, record.ApprovalDecision = resultHash, outcome, decision
	record.AllowForSession, record.FinishedAt = allowForSession, &finished
	if code, ok := result.Metadata["error_code"].(string); ok {
		record.ErrorCode = code
	}
	return executor.state.business.recordAIToolAudit(context.Background(), record)
}

func (executor *aiWorkspaceToolExecutor) auditRecord(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiToolAuditRecord, error) {
	previewHash, err := aiWorkspaceJSONHash(plan.Preview)
	if err != nil || uuid.Validate(plan.auditID) != nil || plan.auditStartedAt.IsZero() {
		return aiToolAuditRecord{}, errRPCInvalid
	}
	return aiToolAuditRecord{
		ID: plan.auditID, ProjectID: workspace.Project.ID, ConversationID: workspace.ConversationID, GenerationID: workspace.GenerationID,
		ToolCallIDSHA256: aiWorkspaceBytesHash([]byte(plan.Call.ID)), ToolName: plan.Call.Name,
		ArgumentsSHA256: plan.Preview.ArgumentsSHA256, PreviewSHA256: previewHash, StartedAt: plan.auditStartedAt,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func validateAIWorkspaceToolContext(value aiWorkspaceToolContext) error {
	if value.Project.ID == uuid.Nil || value.Project.State != "available" || strings.TrimSpace(value.Project.LocalPath) == "" ||
		uuid.Validate(value.ConversationID) != nil || uuid.Validate(value.GenerationID) != nil || !validAIWorkspaceMode(value.WorkspaceMode) {
		return errRPCInvalid
	}
	return nil
}

func validAIWorkspaceToolCall(call aiWorkspaceToolCall) bool {
	if call.ID == "" || len(call.ID) > 256 || !utf8.ValidString(call.ID) || call.Arguments == nil {
		return false
	}
	known := slices.Contains([]string{
		"list_files", "search_files", "read_file", "read_tool_result", "read_image", "web_search", "web_fetch", "write_file", "replace_in_file", "rollback_file_change", "run_command",
		"terminal_open", "terminal_send", "terminal_read", "terminal_signal", "terminal_close", "terminal_list",
	}, call.Name)
	mcp := strings.HasPrefix(call.Name, "mcp__") && validAIProviderToolName(call.Name)
	return known || mcp
}

func aiWorkspaceModeAllowsTool(mode, name string) bool {
	if mode == aiWorkspaceModeReadOnly {
		if strings.HasPrefix(name, "mcp__") && validAIProviderToolName(name) {
			return true
		}
		return name == "list_files" || name == "search_files" || name == "read_file" || name == "read_tool_result" || name == "read_image" || name == "web_search" || name == "web_fetch"
	}
	return mode == aiWorkspaceModeWorkspaceWrite || mode == aiWorkspaceModeFullAccess
}

func resolveAIWorkspaceToolPath(project registeredProject, relative string, allowAbsent bool, mode string) (string, string, error) {
	if mode == aiWorkspaceModeFullAccess {
		return resolveAIFullAccessToolPath(project, relative, allowAbsent)
	}
	resolved, normalized, err := secureExistingProjectPath(project, relative)
	if err == nil || !allowAbsent || !errors.Is(err, errRPCNotFound) {
		return resolved, normalized, err
	}
	normalized, err = normalizeWorkspaceRelativePath(relative)
	if err != nil || normalized == "" {
		return "", "", firstError(err, errRPCForbidden)
	}
	parentRelative := filepath.ToSlash(filepath.Dir(normalized))
	if parentRelative == "." {
		parentRelative = ""
	}
	parent, _, err := secureExistingProjectPath(project, parentRelative)
	if err != nil {
		return "", "", err
	}
	resolved = filepath.Join(parent, filepath.Base(filepath.FromSlash(normalized)))
	if _, err := os.Lstat(resolved); !errors.Is(err, os.ErrNotExist) {
		return "", "", firstError(err, errRPCRevision)
	}
	return resolved, normalized, nil
}

// resolveAIFullAccessToolPath accepts native absolute paths and project-root
// relative paths (including ..) only for an effective Full Access call. The
// returned path is canonical. Missing write targets are anchored beneath a
// canonical existing parent so a symlink cannot redirect the later write.
func resolveAIFullAccessToolPath(project registeredProject, requested string, allowAbsent bool) (string, string, error) {
	if project.ID == uuid.Nil || strings.TrimSpace(project.LocalPath) == "" || project.State != "available" {
		return "", "", errRPCProject
	}
	if len(requested) > 4096 || !utf8.ValidString(requested) || strings.IndexByte(requested, 0) >= 0 {
		return "", "", errRPCForbidden
	}
	root, err := filepath.Abs(project.LocalPath)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", errRPCNotFound
	}
	if err != nil {
		return "", "", errRPCForbidden
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", "", firstError(err, errRPCProject)
	}

	input := filepath.FromSlash(requested)
	if filepath.VolumeName(input) != "" && !filepath.IsAbs(input) {
		return "", "", errRPCForbidden
	}
	candidate := input
	if candidate == "" {
		candidate = root
	} else if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		resolved, err = filepath.Abs(filepath.Clean(resolved))
		if err != nil {
			return "", "", err
		}
		return resolved, aiWorkspaceCanonicalDisplayPath(root, resolved), nil
	}
	if !allowAbsent || !errors.Is(err, os.ErrNotExist) {
		return "", "", firstError(err, errRPCNotFound)
	}
	base := filepath.Base(candidate)
	if base == "" || base == "." || base == ".." || runtime.GOOS == "windows" && !validFileName(base) {
		return "", "", errRPCForbidden
	}
	parent, parentErr := filepath.EvalSymlinks(filepath.Dir(candidate))
	if errors.Is(parentErr, os.ErrNotExist) {
		return "", "", errRPCNotFound
	}
	if parentErr != nil {
		return "", "", errRPCForbidden
	}
	parentInfo, parentErr := os.Stat(parent)
	if parentErr != nil || !parentInfo.IsDir() {
		return "", "", firstError(parentErr, errRPCInvalid)
	}
	resolved = filepath.Join(parent, base)
	if _, statErr := os.Lstat(resolved); !errors.Is(statErr, os.ErrNotExist) {
		return "", "", firstError(statErr, errRPCRevision)
	}
	return resolved, aiWorkspaceCanonicalDisplayPath(root, resolved), nil
}

func aiWorkspaceCanonicalDisplayPath(root, target string) string {
	root, target = filepath.Clean(root), filepath.Clean(target)
	if sameFilesystemPath(root, target) {
		return ""
	}
	if pathWithinRoot(root, target) {
		relative, err := filepath.Rel(root, target)
		if err == nil {
			return filepath.ToSlash(relative)
		}
	}
	return target
}

func aiWorkspaceChildDisplayPath(parent, name string) string {
	parentPath := filepath.FromSlash(parent)
	joined := filepath.Join(parentPath, name)
	if filepath.IsAbs(parentPath) || filepath.VolumeName(parentPath) != "" {
		return filepath.Clean(joined)
	}
	return filepath.ToSlash(joined)
}

// aiWorkspaceTraversalInfo rejects links, junction aliases, and irregular
// entries. Recursive tools therefore never escape their already-authorized
// starting directory and cannot enter a link cycle.
func aiWorkspaceTraversalInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, errRPCForbidden
	}
	evaluated, err := filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(evaluated, path) {
		return nil, firstError(err, errRPCForbidden)
	}
	return info, nil
}

func aiWorkspaceToolTitle(name, path string) string {
	switch name {
	case "list_files":
		return "列出 " + aiWorkspaceDisplayPath(path)
	case "search_files":
		return "搜索 " + aiWorkspaceDisplayPath(path)
	case "read_file":
		return "读取 " + aiWorkspaceDisplayPath(path)
	case "read_image":
		return "读取图片 " + aiWorkspaceDisplayPath(path)
	case "write_file":
		return "写入 " + aiWorkspaceDisplayPath(path)
	case "replace_in_file":
		return "修改 " + aiWorkspaceDisplayPath(path)
	default:
		return name
	}
}

func aiWorkspaceDisplayPath(path string) string {
	if path == "" {
		return "项目根目录"
	}
	return path
}

func (executor *aiWorkspaceToolExecutor) listFiles(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	info, err := os.Stat(plan.resolvedPath)
	if err != nil || !info.IsDir() {
		return aiWorkspaceToolResult{}, firstError(err, errRPCInvalid)
	}
	recursive, err := aiWorkspaceBool(plan.Call.Arguments, "recursive", false)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	maxDepth, err := aiWorkspaceInt(plan.Call.Arguments, "max_depth", 3, 1, 8)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	limit, err := aiWorkspaceInt(plan.Call.Arguments, "limit", 200, 1, 1000)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	manager := fileRPCManagerFor(executor.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entries := make([]string, 0, min(limit, 200))
	truncated := false
	var visit func(string, string, int) error
	visit = func(directory, relative string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		children, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		slices.SortFunc(children, func(left, right os.DirEntry) int {
			return strings.Compare(strings.ToLower(left.Name()), strings.ToLower(right.Name()))
		})
		for _, child := range children {
			if len(entries) >= limit {
				truncated = true
				return nil
			}
			resolved := filepath.Join(directory, child.Name())
			normalized := aiWorkspaceChildDisplayPath(relative, child.Name())
			childInfo, err := aiWorkspaceTraversalInfo(resolved)
			if err != nil {
				continue
			}
			if childInfo.IsDir() {
				if executor.ignoredDirectory(workspace.Project, child.Name()) {
					continue
				}
				entries = append(entries, normalized+"/")
				if recursive && depth < maxDepth {
					if err := visit(resolved, normalized, depth+1); err != nil {
						return err
					}
				}
			} else if childInfo.Mode().IsRegular() {
				entries = append(entries, normalized)
			}
		}
		return nil
	}
	baseRelative := ""
	if len(plan.Preview.RelativePaths) > 0 {
		baseRelative = plan.Preview.RelativePaths[0]
	}
	if err := visit(plan.resolvedPath, baseRelative, 0); err != nil {
		return aiWorkspaceToolResult{}, err
	}
	content := strings.Join(entries, "\n")
	if content == "" {
		content = "[目录为空]"
	}
	if truncated {
		content += fmt.Sprintf("\n\n[结果已截断，仅显示前 %d 项]", limit)
	}
	return aiWorkspaceToolSuccess(content, fmt.Sprintf("已列出 %d 项", len(entries)), map[string]any{"truncated": truncated, "count": len(entries)}), nil
}

func (executor *aiWorkspaceToolExecutor) searchFiles(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	query, err := aiWorkspaceString(plan.Call.Arguments, "query", false, 256)
	if err != nil || strings.TrimSpace(query) == "" {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	pattern, err := aiWorkspaceString(plan.Call.Arguments, "file_pattern", true, 255)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	caseSensitive, err := aiWorkspaceBool(plan.Call.Arguments, "case_sensitive", false)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	limit, err := aiWorkspaceInt(plan.Call.Arguments, "max_results", 80, 1, 300)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	if pattern != "" {
		if _, err := filepath.Match(pattern, "candidate"); err != nil {
			return aiWorkspaceToolResult{}, errRPCInvalid
		}
	}
	needle := query
	if !caseSensitive {
		needle = strings.ToLower(needle)
	}
	manager := fileRPCManagerFor(executor.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	results := make([]string, 0, min(limit, 80))
	matches := make([]map[string]any, 0, min(limit, 100))
	pending := []struct{ path, relative string }{{plan.resolvedPath, plan.Preview.RelativePaths[0]}}
	scanned, skipped := 0, 0
	for len(pending) > 0 && len(results) < limit && scanned < maximumAIWorkspaceSearchFiles {
		if err := ctx.Err(); err != nil {
			return aiWorkspaceToolResult{}, err
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		children, err := os.ReadDir(current.path)
		if err != nil {
			skipped++
			continue
		}
		for _, child := range children {
			resolved := filepath.Join(current.path, child.Name())
			normalized := aiWorkspaceChildDisplayPath(current.relative, child.Name())
			info, err := aiWorkspaceTraversalInfo(resolved)
			if err != nil {
				skipped++
				continue
			}
			if info.IsDir() {
				if !executor.ignoredDirectory(workspace.Project, child.Name()) {
					pending = append(pending, struct{ path, relative string }{resolved, normalized})
				}
				continue
			}
			if !info.Mode().IsRegular() || executor.sensitiveName(workspace.Project, child.Name()) || info.Size() < 0 || info.Size() > maximumAIWorkspaceSearchBytes {
				skipped++
				continue
			}
			if pattern != "" {
				matched, _ := filepath.Match(pattern, child.Name())
				if !matched {
					continue
				}
			}
			scanned++
			comparablePath := normalized
			if !caseSensitive {
				comparablePath = strings.ToLower(comparablePath)
			}
			if strings.Contains(comparablePath, needle) {
				results = append(results, normalized+":1: [文件路径匹配]")
				if len(results) >= limit {
					break
				}
			}
			data, err := readBoundedFile(resolved, maximumAIWorkspaceSearchBytes)
			if err != nil || looksBinaryText(data) || !utf8.Valid(data) {
				skipped++
				continue
			}
			for index, line := range strings.Split(string(data), "\n") {
				comparable := line
				if !caseSensitive {
					comparable = strings.ToLower(comparable)
				}
				if strings.Contains(comparable, needle) {
					shortened := aiWorkspaceShorten(strings.TrimSpace(line), 240)
					results = append(results, fmt.Sprintf("%s:%d: %s", normalized, index+1, shortened))
					if len(matches) < 100 {
						matches = append(matches, map[string]any{"file": normalized, "line": index + 1, "text": shortened})
					}
					if len(results) >= limit {
						break
					}
				}
			}
			if len(results) >= limit || scanned >= maximumAIWorkspaceSearchFiles {
				break
			}
		}
	}
	content := strings.Join(results, "\n")
	if content == "" {
		content = "[未找到匹配项]"
	}
	truncated := len(results) >= limit || scanned >= maximumAIWorkspaceSearchFiles
	if truncated {
		content += fmt.Sprintf("\n\n[结果已截断，仅显示前 %d 条]", limit)
	}
	if skipped > 0 {
		content += fmt.Sprintf("\n[跳过 %d 个过大、敏感、二进制、链接或不可读文件]", skipped)
	}
	result := aiWorkspaceToolSuccess(content, fmt.Sprintf("找到 %d 条匹配", len(results)), map[string]any{"count": len(results), "scannedFiles": scanned, "skippedFiles": skipped, "truncated": truncated})
	aiWorkspaceAttachView(&result, aiWorkspaceSearchView(matches, truncated))
	return result, nil
}

func (executor *aiWorkspaceToolExecutor) readFile(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.sensitiveName(workspace.Project, filepath.Base(plan.resolvedPath)) {
		return aiWorkspaceToolResult{}, errRPCForbidden
	}
	manager := fileRPCManagerFor(executor.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	info, err := os.Stat(plan.resolvedPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximumAIWorkspaceReadBytes {
		return aiWorkspaceToolResult{}, firstError(err, errRPCInvalid)
	}
	data, err := readBoundedFile(plan.resolvedPath, maximumAIWorkspaceReadBytes)
	if err != nil || looksBinaryText(data) || !utf8.Valid(data) {
		return aiWorkspaceToolResult{}, firstError(err, errRPCInvalid)
	}
	if err := ctx.Err(); err != nil {
		return aiWorkspaceToolResult{}, err
	}
	hash := aiWorkspaceBytesHash(data)
	lines := strings.Split(string(data), "\n")
	start, err := aiWorkspaceInt(plan.Call.Arguments, "start_line", 1, 1, 1<<30)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	count, err := aiWorkspaceInt(plan.Call.Arguments, "max_lines", 400, 1, 2000)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	startIndex := min(max(start-1, 0), len(lines))
	endIndex := min(startIndex+count, len(lines))
	relative := plan.Preview.RelativePaths[0]
	var output strings.Builder
	fmt.Fprintf(&output, "[source=workspace resourceId=%s version=%s content_hash=%s]\n", relative, hash, hash)
	for index := startIndex; index < endIndex; index++ {
		fmt.Fprintf(&output, "%d\t%s\n", index+1, lines[index])
	}
	if endIndex < len(lines) {
		fmt.Fprintf(&output, "\n[还有 %d 行，使用 start_line=%d 继续读取]", len(lines)-endIndex, endIndex+1)
	}
	result := aiWorkspaceToolSuccess(strings.TrimRight(output.String(), "\n"), fmt.Sprintf("已读取 %d 行", endIndex-startIndex), map[string]any{
		"content_hash": hash, "path": relative, "start_line": startIndex + 1, "end_line": endIndex,
	})
	aiWorkspaceAttachView(&result, aiWorkspaceReadView(relative, startIndex+1, endIndex, len(lines)))
	return result, nil
}

func (executor *aiWorkspaceToolExecutor) writeFile(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	manager := fileRPCManagerFor(executor.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	path, relative := plan.resolvedPath, plan.Preview.RelativePaths[0]
	original, absent, mode, err := readAIWorkspaceMutableFile(path)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	originalHash := "absent"
	if !absent {
		originalHash = aiWorkspaceBytesHash(original)
	}
	expected, err := aiWorkspaceString(plan.Call.Arguments, "expected_hash", true, 64)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	expected = strings.ToLower(strings.TrimSpace(expected))
	if absent {
		if expected != "" && expected != "absent" {
			return aiWorkspaceToolResult{}, errRPCRevision
		}
	} else if expected == "" || expected != originalHash {
		return aiWorkspaceToolResult{}, errRPCRevision
	}
	var updated []byte
	changedOccurrences := 1
	if plan.Call.Name == "write_file" {
		content, err := aiWorkspaceString(plan.Call.Arguments, "content", true, maximumAIWorkspaceWriteBytes)
		if err != nil {
			return aiWorkspaceToolResult{}, err
		}
		updated = []byte(content)
	} else {
		if absent {
			return aiWorkspaceToolResult{}, errRPCNotFound
		}
		oldText, err := aiWorkspaceString(plan.Call.Arguments, "old_text", false, maximumAIWorkspaceWriteBytes)
		if err != nil || oldText == "" {
			return aiWorkspaceToolResult{}, errRPCInvalid
		}
		newText, err := aiWorkspaceString(plan.Call.Arguments, "new_text", true, maximumAIWorkspaceWriteBytes)
		if err != nil {
			return aiWorkspaceToolResult{}, err
		}
		replaceAll, err := aiWorkspaceBool(plan.Call.Arguments, "replace_all", false)
		if err != nil {
			return aiWorkspaceToolResult{}, err
		}
		occurrences := bytes.Count(original, []byte(oldText))
		if occurrences == 0 || !replaceAll && occurrences != 1 {
			return aiWorkspaceToolResult{}, errRPCRevision
		}
		changedOccurrences = 1
		if replaceAll {
			changedOccurrences = occurrences
			updated = bytes.ReplaceAll(original, []byte(oldText), []byte(newText))
		} else {
			updated = bytes.Replace(original, []byte(oldText), []byte(newText), 1)
		}
	}
	if len(updated) > maximumAIWorkspaceWriteBytes || !utf8.Valid(updated) {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	if err := ctx.Err(); err != nil {
		return aiWorkspaceToolResult{}, err
	}
	if err := atomicWriteAIWorkspaceFile(path, relative, originalHash, updated, mode); err != nil {
		return aiWorkspaceToolResult{}, err
	}
	written, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(written, updated) {
		return aiWorkspaceToolResult{}, firstError(err, errRPCRevision)
	}
	updatedHash := aiWorkspaceBytesHash(written)
	rollbackID := uuid.NewString()
	executor.mu.Lock()
	executor.rollbacks[rollbackID] = aiWorkspaceRollbackRecord{
		ID: rollbackID, ProjectID: workspace.Project.ID, ConversationID: workspace.ConversationID, WorkspaceMode: plan.workspaceMode, RelativePath: relative,
		Path: path, OriginalContents: append([]byte(nil), original...), OriginalAbsent: absent, UpdatedHash: updatedHash, CreatedAt: executor.now().UTC(),
	}
	for len(executor.rollbacks) > maximumAIWorkspaceRollbackItems {
		var oldestID string
		var oldest time.Time
		for id, record := range executor.rollbacks {
			if oldestID == "" || record.CreatedAt.Before(oldest) {
				oldestID, oldest = id, record.CreatedAt
			}
		}
		delete(executor.rollbacks, oldestID)
	}
	executor.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"path": relative, "bytes": len(written), "content_hash": updatedHash, "rollback_id": rollbackID, "changed_occurrences": changedOccurrences})
	result := aiWorkspaceToolSuccess(string(payload), "已写入 "+relative, map[string]any{"content_hash": updatedHash, "rollback_id": rollbackID})
	if plan.Call.Name == "replace_in_file" {
		aiWorkspaceAttachView(&result, aiWorkspaceDiffView(original, written, relative))
	}
	return result, nil
}

func (executor *aiWorkspaceToolExecutor) rollbackFor(workspace aiWorkspaceToolContext, arguments map[string]any) (aiWorkspaceRollbackRecord, error) {
	id, err := aiWorkspaceString(arguments, "rollback_id", false, 64)
	if err != nil || uuid.Validate(id) != nil {
		return aiWorkspaceRollbackRecord{}, errRPCInvalid
	}
	executor.mu.Lock()
	record, found := executor.rollbacks[id]
	executor.mu.Unlock()
	if !found {
		return aiWorkspaceRollbackRecord{}, errRPCNotFound
	}
	if record.ProjectID != workspace.Project.ID || record.ConversationID != workspace.ConversationID {
		return aiWorkspaceRollbackRecord{}, errRPCForbidden
	}
	if record.WorkspaceMode != workspace.WorkspaceMode {
		return aiWorkspaceRollbackRecord{}, errRPCForbidden
	}
	pathInput := record.RelativePath
	if record.WorkspaceMode == aiWorkspaceModeFullAccess {
		pathInput = record.Path
	}
	resolved, normalized, err := resolveAIWorkspaceToolPath(workspace.Project, pathInput, false, record.WorkspaceMode)
	if err != nil || !sameFilesystemPath(resolved, record.Path) || normalized != record.RelativePath {
		return aiWorkspaceRollbackRecord{}, firstError(err, errRPCRevision)
	}
	return record, nil
}

func (executor *aiWorkspaceToolExecutor) rollbackFile(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	record, err := executor.rollbackFor(workspace, plan.Call.Arguments)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	expected, err := aiWorkspaceString(plan.Call.Arguments, "expected_hash", false, 64)
	if err != nil || strings.ToLower(strings.TrimSpace(expected)) != record.UpdatedHash {
		return aiWorkspaceToolResult{}, errRPCRevision
	}
	manager := fileRPCManagerFor(executor.state)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current, err := os.ReadFile(record.Path)
	if err != nil || aiWorkspaceBytesHash(current) != record.UpdatedHash {
		return aiWorkspaceToolResult{}, firstError(err, errRPCRevision)
	}
	if err := ctx.Err(); err != nil {
		return aiWorkspaceToolResult{}, err
	}
	restoredHash := "absent"
	if record.OriginalAbsent {
		tombstone := record.Path + ".wenzwork-rollback-" + record.ID
		if _, err := os.Lstat(tombstone); !errors.Is(err, os.ErrNotExist) {
			return aiWorkspaceToolResult{}, firstError(err, errRPCRevision)
		}
		if err := os.Rename(record.Path, tombstone); err != nil {
			return aiWorkspaceToolResult{}, err
		}
		if err := os.Remove(tombstone); err != nil {
			_ = os.Rename(tombstone, record.Path)
			return aiWorkspaceToolResult{}, err
		}
	} else {
		info, err := os.Stat(record.Path)
		if err != nil {
			return aiWorkspaceToolResult{}, err
		}
		if err := atomicWriteAIWorkspaceFile(record.Path, record.RelativePath, record.UpdatedHash, record.OriginalContents, info.Mode().Perm()); err != nil {
			return aiWorkspaceToolResult{}, err
		}
		restoredHash = aiWorkspaceBytesHash(record.OriginalContents)
	}
	executor.mu.Lock()
	delete(executor.rollbacks, record.ID)
	executor.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{"path": record.RelativePath, "restored_hash": restoredHash})
	return aiWorkspaceToolSuccess(string(payload), "已回滚 "+record.RelativePath, map[string]any{"content_hash": restoredHash}), nil
}

func (executor *aiWorkspaceToolExecutor) openTerminal(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.terminals == nil || !plan.terminalOwner.matches(aiTerminalOwnerFor(workspace)) || len(plan.commandLaunch.Argv) == 0 ||
		plan.commandLaunch.WorkingDirectory == "" || plan.commandLaunch.SandboxMode != plan.workspaceMode {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	view, motd, err := executor.terminals.Open(ctx, aiTerminalOpenRequest{
		Owner: plan.terminalOwner, Name: plan.terminalName, CWD: plan.commandLaunch.WorkingDirectory,
		DisplayCWD: plan.terminalDisplayCWD, Shell: plan.terminalShell, Launch: plan.commandLaunch,
		NetworkHosts: append([]string(nil), plan.Preview.NetworkHosts...),
	})
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	payload := map[string]any{
		"session_id": view.SessionID, "type": view.Type, "cwd": view.CWD, "pid": view.PID, "status": view.Status,
		"sandbox_status": view.SandboxStatus, "network_allowed": view.NetworkAllowed, "network_hosts": view.NetworkHosts, "motd": motd,
	}
	if view.Name != "" {
		payload["name"] = view.Name
	}
	content, err := marshalAITerminalResult(payload)
	if err != nil {
		_, _ = executor.terminals.CloseSession(plan.terminalOwner, uuid.MustParse(view.SessionID))
		return aiWorkspaceToolResult{}, err
	}
	return aiWorkspaceToolSuccess(content, "已打开持久终端 "+view.SessionID, map[string]any{
		"session_id": view.SessionID, "pid": view.PID, "network_allowed": view.NetworkAllowed, "sandbox_status": view.SandboxStatus,
	}), nil
}

func (executor *aiWorkspaceToolExecutor) sendTerminal(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.terminals == nil || !plan.terminalOwner.matches(aiTerminalOwnerFor(workspace)) {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	sessionID, err := aiWorkspaceTerminalSessionID(plan.Call.Arguments)
	if err != nil || sessionID != plan.terminalSessionID {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	text, err := aiWorkspaceTerminalText(plan.Call.Arguments)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	submit, err := aiWorkspaceBool(plan.Call.Arguments, "submit", true)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	timeoutSeconds, err := aiWorkspaceInt(plan.Call.Arguments, "timeout_seconds", 10, 1, 120)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	result, err := executor.terminals.Send(ctx, plan.terminalOwner, sessionID, text, submit, time.Duration(timeoutSeconds)*time.Second)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	payload := map[string]any{
		"session_id": sessionID.String(), "viewport": result.Viewport, "wait_reason": result.WaitReason,
		"session_status": result.SessionStatus, "truncated": result.Truncated,
	}
	content, err := marshalAITerminalResult(payload)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	summary := "终端已返回（" + result.WaitReason + "）"
	if result.SessionStatus["kind"] == "exited" {
		summary = "持久终端已退出"
	}
	return aiWorkspaceToolSuccess(content, summary, map[string]any{
		"session_id": sessionID.String(), "wait_reason": result.WaitReason, "truncated": result.Truncated,
	}), nil
}

func (executor *aiWorkspaceToolExecutor) readTerminal(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.terminals == nil || !plan.terminalOwner.matches(aiTerminalOwnerFor(workspace)) {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	sessionID, err := aiWorkspaceTerminalSessionID(plan.Call.Arguments)
	if err != nil || sessionID != plan.terminalSessionID {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	offset, err := aiWorkspaceInt(plan.Call.Arguments, "offset", 0, 0, 1<<30)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	count, err := aiWorkspaceInt(plan.Call.Arguments, "count", 500, 1, 2000)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	result, err := executor.terminals.Read(plan.terminalOwner, sessionID, offset, count)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	payload := map[string]any{"session_id": sessionID.String(), "text": result.Text, "total_lines": result.TotalLines, "line_begin": result.LineBegin, "line_end": result.LineEnd, "truncated": result.Truncated}
	content, err := marshalAITerminalResult(payload)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	return aiWorkspaceToolSuccess(content, "已读取持久终端 "+sessionID.String(), map[string]any{"session_id": sessionID.String(), "truncated": result.Truncated}), nil
}

func (executor *aiWorkspaceToolExecutor) signalTerminal(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.terminals == nil || !plan.terminalOwner.matches(aiTerminalOwnerFor(workspace)) {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	sessionID, err := aiWorkspaceTerminalSessionID(plan.Call.Arguments)
	if err != nil || sessionID != plan.terminalSessionID {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	signal, err := aiWorkspaceString(plan.Call.Arguments, "signal", false, 16)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	result, err := executor.terminals.Signal(plan.terminalOwner, sessionID, signal)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	content, err := marshalAITerminalResult(result)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	return aiWorkspaceToolSuccess(content, "已向持久终端发送 "+signal, result), nil
}

func (executor *aiWorkspaceToolExecutor) closeTerminal(workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.terminals == nil || !plan.terminalOwner.matches(aiTerminalOwnerFor(workspace)) {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	sessionID, err := aiWorkspaceTerminalSessionID(plan.Call.Arguments)
	if err != nil || sessionID != plan.terminalSessionID {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	started, err := executor.terminals.CloseSession(plan.terminalOwner, sessionID)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	payload := map[string]any{"session_id": sessionID.String(), "closed": true, "close_started": started}
	content, err := marshalAITerminalResult(payload)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	return aiWorkspaceToolSuccess(content, "已关闭持久终端 "+sessionID.String(), payload), nil
}

func (executor *aiWorkspaceToolExecutor) listTerminals(workspace aiWorkspaceToolContext) (aiWorkspaceToolResult, error) {
	if executor.terminals == nil {
		return aiWorkspaceToolResult{}, errRPCCapability
	}
	sessions, err := executor.terminals.List(aiTerminalOwnerFor(workspace))
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	content, err := marshalAITerminalResult(map[string]any{"sessions": sessions})
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	return aiWorkspaceToolSuccess(content, fmt.Sprintf("持久终端 %d 个", len(sessions)), map[string]any{"session_count": len(sessions)}), nil
}

func (executor *aiWorkspaceToolExecutor) Close() error {
	if executor == nil || executor.terminals == nil {
		return nil
	}
	return executor.terminals.Close()
}

func (executor *aiWorkspaceToolExecutor) ReconcileTerminals(workspace aiWorkspaceToolContext) error {
	if executor == nil || executor.terminals == nil || validateAIWorkspaceToolContext(workspace) != nil {
		return errRPCCapability
	}
	return executor.terminals.Reconcile(aiTerminalOwnerFor(workspace))
}

func (executor *aiWorkspaceToolExecutor) CloseProjectTerminals(projectID uuid.UUID, reason string) error {
	if executor == nil || executor.terminals == nil {
		return nil
	}
	return executor.terminals.CloseProject(projectID, reason)
}

func (executor *aiWorkspaceToolExecutor) runCommand(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	background, err := aiWorkspaceBool(plan.Call.Arguments, "background", false)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	if !background {
		return executor.runCommandForeground(ctx, workspace, plan)
	}
	if executor.state == nil {
		return aiWorkspaceToolResult{}, errRPCCapability
	}
	job, err := executor.state.registerAIJob(workspace.ConversationID, "command")
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	// The background command outlives the turn: run it under a detached,
	// killable context so the per-call budget and turn cancellation cannot
	// leak into the job.
	jobContext, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	go func() {
		result, runErr := executor.runCommandForeground(jobContext, workspace, plan)
		status, code := "succeeded", ""
		if runErr != nil || result.IsError {
			status = "failed"
			if errors.Is(runErr, context.Canceled) {
				status, code = "killed", "killed"
			} else if runErr != nil {
				code = aiWorkspaceErrorCode(runErr)
			} else if value, ok := result.Metadata["error_code"].(string); ok {
				code = value
			}
		}
		executor.state.finishAIJob(job, status, result.Content, code)
	}()
	content, _ := json.Marshal(map[string]any{"job_id": job.ID, "status": "running"})
	return aiWorkspaceToolSuccess(string(content), "命令已在后台启动。", map[string]any{"job_id": job.ID, "background": true}), nil
}

// aiCommandProgressInterval is the heartbeat cadence for foreground command
// progress snapshots.
const aiCommandProgressInterval = 500 * time.Millisecond

// aiCommandProgress tees raw command output into a bounded snapshot buffer and
// emits heartbeat texts on the plan's progress channel. Snapshot emission is
// best-effort (a slow consumer drops heartbeats, never blocks the command).
type aiCommandProgress struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	stop   chan struct{}
	done   sync.WaitGroup
	plan   aiWorkspaceToolPlan
}

func newAICommandProgress(plan *aiWorkspaceToolPlan) *aiCommandProgress {
	if plan == nil || plan.progress == nil {
		return nil
	}
	progress := &aiCommandProgress{stop: make(chan struct{}), plan: *plan}
	progress.done.Add(1)
	go func() {
		defer progress.done.Done()
		ticker := time.NewTicker(aiCommandProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				progress.mu.Lock()
				snapshot := progress.buffer.String()
				progress.mu.Unlock()
				if snapshot == "" {
					continue
				}
				text := truncateAIUTF8(strings.ToValidUTF8(snapshot, "\uFFFD"), maximumAIWorkspaceCommandVisible)
				select {
				case plan.progress <- text:
				default:
				}
			case <-progress.stop:
				return
			}
		}
	}()
	return progress
}

func (progress *aiCommandProgress) Write(chunk []byte) (int, error) {
	progress.mu.Lock()
	_, err := progress.buffer.Write(chunk)
	progress.mu.Unlock()
	return len(chunk), err
}

func (progress *aiCommandProgress) Close() {
	if progress == nil {
		return
	}
	close(progress.stop)
	progress.done.Wait()
	progress.mu.Lock()
	final := progress.buffer.String()
	progress.mu.Unlock()
	if text := truncateAIUTF8(strings.ToValidUTF8(final, "\uFFFD"), maximumAIWorkspaceCommandVisible); text != "" {
		select {
		case progress.plan.progress <- text:
		default:
		}
	}
	close(progress.plan.progress)
}

func (executor *aiWorkspaceToolExecutor) runCommandForeground(ctx context.Context, workspace aiWorkspaceToolContext, plan aiWorkspaceToolPlan) (aiWorkspaceToolResult, error) {
	if executor.supervisor == nil {
		return aiWorkspaceToolResult{}, errRPCCapability
	}
	timeoutSeconds, err := aiWorkspaceInt(plan.Call.Arguments, "timeout_seconds", 30, 1, 120)
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	launch := plan.commandLaunch
	if len(launch.Argv) == 0 || launch.WorkingDirectory == "" || launch.SandboxMode != plan.workspaceMode {
		return aiWorkspaceToolResult{}, errRPCInvalid
	}
	validationRoot := workspace.Project.LocalPath
	if plan.workspaceMode == aiWorkspaceModeFullAccess {
		validationRoot = launch.WorkingDirectory
	}
	process, err := executor.supervisor.Start(rawProcessLaunchSpec{
		ProjectID: workspace.Project.ID, ProjectRoot: validationRoot, WorkingDirectory: launch.WorkingDirectory,
		Argv: launch.Argv, Limits: processResourceLimits{
			MaximumLifetime: time.Duration(timeoutSeconds) * time.Second, MaximumMemoryBytes: 512 << 20,
			MaximumOutputBytes: 2 * maximumAIWorkspaceCommandOutput,
		},
	})
	if err != nil {
		return aiWorkspaceToolResult{}, err
	}
	defer process.release()

	progress := newAICommandProgress(&plan)
	defer progress.Close()
	stdoutReader := io.Reader(process.Stdout())
	stderrReader := io.Reader(process.Stderr())
	if progress != nil {
		stdoutReader = io.TeeReader(process.Stdout(), progress)
		stderrReader = io.TeeReader(process.Stderr(), progress)
	}

	type streamRead struct {
		name   string
		result aiWorkspaceCommandStreamOutput
		err    error
	}
	reads := make(chan streamRead, 2)
	go func() {
		result, readErr := readAIWorkspaceCommandStream(stdoutReader)
		reads <- streamRead{name: "stdout", result: result, err: readErr}
	}()
	go func() {
		result, readErr := readAIWorkspaceCommandStream(stderrReader)
		reads <- streamRead{name: "stderr", result: result, err: readErr}
	}()
	waitDone := make(chan int, 1)
	go func() { waitDone <- process.Wait() }()
	var exitCode int
	select {
	case exitCode = <-waitDone:
	case <-ctx.Done():
		_ = process.Close("client_close")
		exitCode = <-waitDone
	}
	_ = process.Close("process_exit")
	var stdout, stderr aiWorkspaceCommandStreamOutput
	for range 2 {
		read := <-reads
		if read.err != nil && !errors.Is(read.err, io.EOF) && process.reason() == "" {
			return aiWorkspaceToolResult{}, read.err
		}
		if read.name == "stdout" {
			stdout = read.result
		} else {
			stderr = read.result
		}
	}
	reason := process.reason()
	timedOut := reason == "lifetime_limit" || errors.Is(ctx.Err(), context.DeadlineExceeded)
	payload, marshalErr := json.Marshal(aiWorkspaceCommandResult{
		ExitCode: exitCode, Stdout: stdout, Stderr: stderr, BinaryOutput: stdout.Binary || stderr.Binary,
	})
	if marshalErr != nil || len(payload) > maximumAIWorkspaceToolResult {
		return aiWorkspaceToolResult{}, errRPCCapability
	}
	metadata := map[string]any{
		"exit_code": exitCode, "truncated": stdout.Truncated || stderr.Truncated, "network_allowed": launch.NetworkAllowed,
		"network_hosts": plan.Preview.NetworkHosts, "sandbox_status": plan.Preview.SandboxStatus,
		"sandbox_mode": launch.SandboxMode, "sandbox_backend": launch.Backend, "sandbox_enforcement": launch.Enforcement,
		"hard_network_isolation": launch.HardNetworkIsolation,
		"output_hash":            aiWorkspaceBytesHash(payload),
	}
	runnerFailed := !stderr.Binary && launch.runnerFailed(exitCode, stderr.Text)
	if runnerFailed {
		metadata["error_code"] = "sandbox_unavailable"
	}
	result := aiWorkspaceToolSuccess(string(payload), "命令执行成功", metadata)
	if runnerFailed {
		result.IsError, result.Summary = true, "命令沙箱不可用；目标命令未在未隔离状态下执行"
	} else if exitCode != 0 {
		result.IsError, result.Summary = true, fmt.Sprintf("命令退出码 %d", exitCode)
	}
	if timedOut {
		// Preserve whatever stdout/stderr was captured before the timeout so
		// the model sees the partial progress instead of a bare error.
		result.IsError, result.Summary = true, "命令执行超时并已终止；以下为超时前捕获的输出。"
		result.Metadata["error_code"] = "timeout"
		return result, nil
	}
	if ctx.Err() != nil {
		return aiWorkspaceToolResult{}, ctx.Err()
	}
	return result, nil
}

// aiWorkspaceCommandStreamOutput is deliberately stream-scoped. Do not merge
// stdout and stderr: their relative ordering is not observable from raw pipes.
type aiWorkspaceCommandStreamOutput struct {
	Text           string `json:"text,omitempty"`
	SourceEncoding string `json:"source_encoding"`
	Truncated      bool   `json:"truncated"`
	Binary         bool   `json:"binary,omitempty"`
	Bytes          int    `json:"bytes,omitempty"`
	DecodeWarning  bool   `json:"decode_warning,omitempty"`
}

type aiWorkspaceCommandResult struct {
	ExitCode     int                            `json:"exit_code"`
	Stdout       aiWorkspaceCommandStreamOutput `json:"stdout"`
	Stderr       aiWorkspaceCommandStreamOutput `json:"stderr"`
	BinaryOutput bool                           `json:"binary_output"`
}

func readAIWorkspaceCommandStream(reader io.Reader) (aiWorkspaceCommandStreamOutput, error) {
	result := aiWorkspaceCommandStreamOutput{SourceEncoding: "utf-8"}
	if reader == nil {
		return result, errRPCCapability
	}
	decoder := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
	remaining := maximumAIWorkspaceCommandOutput
	appendDecoded := func(decoded CommandTextDecodeResult) {
		if decoded.SourceEncoding != "" {
			result.SourceEncoding = decoded.SourceEncoding
		}
		result.DecodeWarning = result.DecodeWarning || decoded.HadDecodeErrors
		if decoded.IsBinary {
			result.Binary = true
			return
		}
		if result.Binary {
			return
		}
		result.Text, result.Truncated = appendAIWorkspaceVisibleText(result.Text, decoded.DisplayText, result.Truncated)
	}
	buffer := make([]byte, 8<<10)
	rawTruncated := false
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if result.Bytes <= int(^uint(0)>>1)-n {
				result.Bytes += n
			} else {
				result.Bytes = int(^uint(0) >> 1)
			}
			accepted := min(n, max(remaining, 0))
			if accepted > 0 {
				for _, decoded := range decoder.Feed(buffer[:accepted]) {
					appendDecoded(decoded)
				}
				remaining -= accepted
			}
			if accepted < n {
				result.Truncated = true
				rawTruncated = true
			}
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			return result, readErr
		}
		break
	}
	flush := decoder.Flush
	if rawTruncated {
		flush = decoder.FlushTruncatedTail
	}
	for _, decoded := range flush() {
		appendDecoded(decoded)
	}
	if result.Binary {
		result.Text = ""
	}
	return result, nil
}

func appendAIWorkspaceVisibleText(current, next string, alreadyTruncated bool) (string, bool) {
	if next == "" {
		return current, alreadyTruncated
	}
	remaining := maximumAIWorkspaceCommandVisible - len(current)
	if remaining <= 0 {
		return current, true
	}
	if len(next) <= remaining {
		return current + next, alreadyTruncated
	}
	truncated := next[:remaining]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return current + truncated, true
}

func readAIWorkspaceMutableFile(path string) ([]byte, bool, os.FileMode, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, 0o600, nil
	}
	if err != nil {
		return nil, false, 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || info.Size() > maximumAIWorkspaceWriteBytes {
		return nil, false, 0, errRPCForbidden
	}
	data, err := readBoundedFile(path, maximumAIWorkspaceWriteBytes)
	if err != nil || looksBinaryText(data) || !utf8.Valid(data) {
		return nil, false, 0, firstError(err, errRPCInvalid)
	}
	return data, false, info.Mode().Perm(), nil
}

func atomicWriteAIWorkspaceFile(path, relative, expectedHash string, contents []byte, mode os.FileMode) error {
	if len(contents) > maximumAIWorkspaceWriteBytes || !utf8.Valid(contents) || mode&0o777 == 0 {
		return errRPCInvalid
	}
	parent := filepath.Dir(path)
	temporary, err := os.CreateTemp(parent, ".wenzwork-ai-tool-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error { _ = temporary.Close(); return cause }
	if err := temporary.Chmod(mode); err != nil {
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
	current, err := os.Lstat(path)
	if expectedHash == "absent" {
		if !errors.Is(err, os.ErrNotExist) {
			return firstError(err, errRPCRevision)
		}
	} else {
		if err != nil || !current.Mode().IsRegular() || current.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || current.Size() > maximumAIWorkspaceWriteBytes {
			return firstError(err, errRPCRevision)
		}
		data, err := readBoundedFile(path, maximumAIWorkspaceWriteBytes)
		if err != nil || aiWorkspaceBytesHash(data) != expectedHash {
			return firstError(err, errRPCRevision)
		}
	}
	if expectedHash != "absent" && runtime.GOOS == "windows" {
		recovery := filepath.Join(parent, ".wenzwork-ai-recovery-"+uuid.NewString())
		if err := os.Rename(path, recovery); err != nil {
			return err
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			_ = os.Rename(recovery, path)
			return err
		}
		if err := os.Remove(recovery); err != nil {
			return err
		}
		return nil
	}
	_ = relative // relative is kept in the signature to make boundary-reviewed callers explicit.
	return os.Rename(temporaryPath, path)
}

func aiWorkspaceCommandArgv(command string) ([]string, error) {
	available := availableWorkspaceCommandShells()
	if len(available) == 0 {
		return nil, errRPCCapability
	}
	shell := available[0]
	executable, err := resolveSupervisedExecutable(shell)
	if err != nil {
		return nil, err
	}
	switch shell {
	case "pwsh", "powershell":
		return []string{executable, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsPowerShellCommand(command)}, nil
	case "cmd":
		return []string{executable, "/D", "/V:OFF", "/S", "/C", windowsCmdUTF8Bootstrap + " & (" + command + ")"}, nil
	default:
		return []string{executable, "-c", command}, nil
	}
}

func availableWorkspaceCommandShells() []string {
	candidates := []string{"bash", "zsh", "fish", "sh"}
	if runtime.GOOS == "windows" {
		candidates = []string{"pwsh", "powershell", "cmd"}
	}
	available := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, err := resolveSupervisedExecutable(candidate); err == nil {
			available = append(available, candidate)
		}
	}
	return available
}

func windowsPowerShellCommand(command string) string {
	// Run the user-approved command after establishing the child process's
	// encoding policy. Clear the code produced by `chcp` before invoking the
	// reviewed command: otherwise a failing cmdlet can accidentally inherit
	// chcp's successful native exit code. Native command exit codes are kept
	// only when the command itself failed.
	return windowsPowerShellUTF8Bootstrap + "; $global:LASTEXITCODE = $null; & { " + command +
		" }; $__wenzworkSuccess = $?; $__wenzworkExit = $global:LASTEXITCODE; " +
		"if (-not $__wenzworkSuccess) { if ($null -ne $__wenzworkExit -and $__wenzworkExit -ne 0) { exit [int]$__wenzworkExit }; exit 1 }; exit 0"
}

func aiTerminalOwnerFor(workspace aiWorkspaceToolContext) aiTerminalOwner {
	return aiTerminalOwner{
		ProjectID: workspace.Project.ID, ProjectRoot: filepath.Clean(workspace.Project.LocalPath),
		ConversationID: workspace.ConversationID, WorkspaceMode: workspace.WorkspaceMode,
	}
}

func aiWorkspaceTerminalSessionID(arguments map[string]any) (uuid.UUID, error) {
	value, err := aiWorkspaceString(arguments, "session_id", false, 80)
	if err != nil {
		return uuid.Nil, err
	}
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, errRPCInvalid
	}
	return parsed, nil
}

func aiWorkspaceTerminalText(arguments map[string]any) (string, error) {
	raw, exists := arguments["text"]
	text, ok := raw.(string)
	if !exists || !ok || len(text) > maximumAITerminalInputBytes || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return "", errRPCInvalid
	}
	return text, nil
}

func aiWorkspaceNetworkHosts(arguments map[string]any, allowNetwork bool) ([]string, error) {
	raw, exists := arguments["network_hosts"]
	if !exists || raw == nil {
		if allowNetwork {
			return nil, errRPCInvalid
		}
		return []string{}, nil
	}
	values, ok := raw.([]any)
	if !ok || len(values) > 16 || allowNetwork && len(values) == 0 || !allowNetwork && len(values) != 0 {
		return nil, errRPCInvalid
	}
	hosts := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		host, ok := value.(string)
		host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
		if !ok || host == "" || len(host) > 253 || !aiWorkspaceHostPattern.MatchString(host) {
			return nil, errRPCInvalid
		}
		if _, duplicate := seen[host]; duplicate {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	return hosts, nil
}

func aiWorkspaceNetworkHostsAreScoped(arguments map[string]any, requested bool, mode string) bool {
	if mode != aiWorkspaceModeFullAccess {
		return requested
	}
	raw, exists := arguments["network_hosts"]
	if !exists || raw == nil {
		return false
	}
	// Providers commonly materialize an omitted optional array as []. In a
	// full-access session that means no host scope, not an invalid empty scope.
	if values, ok := raw.([]any); ok && len(values) == 0 {
		return false
	}
	return true
}

func catastrophicAIWorkspaceCommand(command string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(command), " "))
	patterns := []string{"rm -rf /", "rm -fr /", "format c:", "format c ", "diskpart", "mkfs.", "shutdown ", "reboot", "stop-computer", "restart-computer"}
	for _, pattern := range patterns {
		if strings.Contains(normalized, pattern) {
			return true
		}
	}
	return false
}

func highRiskAIWorkspaceCommand(command string) bool {
	normalized := strings.ToLower(command)
	for _, marker := range []string{"sudo ", "runas ", "remove-item", " del ", " rm ", "git reset", "git clean", "chmod ", "chown ", "reg ", "sc.exe "} {
		if strings.Contains(" "+normalized, marker) {
			return true
		}
	}
	return false
}

func looksLikeAIWorkspaceNetworkCommand(command string) bool {
	normalized := strings.ToLower(command)
	for _, marker := range []string{"http://", "https://", "curl ", "wget ", "invoke-webrequest", "invoke-restmethod", "git clone", "git fetch", "git pull", "npm install", "pnpm install", "pip install", "go get ", "ssh ", "scp "} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func aiWorkspaceIgnoredDirectory(name string) bool {
	_, found := aiWorkspaceIgnoredDirectories[strings.ToLower(name)]
	return found
}

func aiWorkspaceSensitiveName(name string) bool {
	name = strings.ToLower(name)
	if _, found := aiWorkspaceSensitiveNames[name]; found {
		return true
	}
	return strings.HasPrefix(name, ".env.") || strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key")
}

func aiWorkspaceString(arguments map[string]any, key string, optional bool, maximum int) (string, error) {
	value, exists := arguments[key]
	if !exists || value == nil {
		if optional {
			return "", nil
		}
		return "", errRPCInvalid
	}
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) || len(text) > maximum || strings.IndexByte(text, 0) >= 0 {
		return "", errRPCInvalid
	}
	if !optional && text == "" {
		return "", errRPCInvalid
	}
	return text, nil
}

func aiWorkspaceBool(arguments map[string]any, key string, fallback bool) (bool, error) {
	value, exists := arguments[key]
	if !exists || value == nil {
		return fallback, nil
	}
	result, ok := value.(bool)
	if !ok {
		return false, errRPCInvalid
	}
	return result, nil
}

func aiWorkspaceInt(arguments map[string]any, key string, fallback, minimum, maximum int) (int, error) {
	value, exists := arguments[key]
	if !exists || value == nil {
		return fallback, nil
	}
	number, ok := value.(float64)
	if !ok || number != float64(int(number)) || number < float64(minimum) || number > float64(maximum) {
		return 0, errRPCInvalid
	}
	return int(number), nil
}

func aiWorkspaceJSONHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maximumRPCPayload {
		return "", errRPCInvalid
	}
	return aiWorkspaceBytesHash(encoded), nil
}

func aiWorkspaceBytesHash(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func aiWorkspaceToolSuccess(content, summary string, metadata map[string]any) aiWorkspaceToolResult {
	if metadata == nil {
		metadata = map[string]any{}
	}
	return aiWorkspaceToolResult{Content: content, Summary: summary, Metadata: metadata}
}

func aiWorkspaceToolFailure(content, code string) aiWorkspaceToolResult {
	return aiWorkspaceToolResult{Content: content, Summary: content, IsError: true, Metadata: map[string]any{"error_code": code}}
}

func aiWorkspaceStableError(err error) string {
	var webError *aiWebError
	if errors.As(err, &webError) {
		return webError.Message
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "工具执行已取消。"
	case errors.Is(err, context.DeadlineExceeded):
		return "工具执行超时并已终止。"
	case errors.Is(err, errAICommandSandboxUnavailable):
		return "当前设备缺少此权限模式所需的命令沙箱；命令已安全拒绝。"
	case errors.Is(err, errRPCForbidden):
		return "工具请求超出项目边界或权限范围。"
	case errors.Is(err, errRPCRevision):
		return "文件版本冲突；文件未修改，请重新读取后重试。"
	case errors.Is(err, errRPCNotFound):
		return "工具目标不存在或已经失效。"
	case errors.Is(err, errRPCCapability):
		return "当前设备无法安全执行该工具。"
	case errors.Is(err, errRPCBusy):
		return "设备资源已达到上限，请稍后重试。"
	default:
		return "工具参数无效或执行失败。"
	}
}

func aiWorkspaceErrorCode(err error) string {
	var webError *aiWebError
	if errors.As(err, &webError) {
		return webError.Code
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errAICommandSandboxUnavailable):
		return "sandbox_unavailable"
	case errors.Is(err, errRPCForbidden):
		return "forbidden"
	case errors.Is(err, errRPCRevision):
		return "revision_conflict"
	case errors.Is(err, errRPCNotFound):
		return "not_found"
	case errors.Is(err, errRPCCapability):
		return "capability_unavailable"
	case errors.Is(err, errRPCBusy):
		return "busy"
	default:
		return "invalid_or_failed"
	}
}

func aiWorkspaceShorten(value string, maximum int) string {
	value = strings.Join(strings.Fields(value), " ")
	if maximum <= 0 {
		return ""
	}
	if len(value) <= maximum {
		return value
	}
	const ellipsis = "…"
	if maximum <= len(ellipsis) {
		return truncateAIUTF8(value, maximum)
	}
	return truncateAIUTF8(value, maximum-len(ellipsis)) + ellipsis
}
