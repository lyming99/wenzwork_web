package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maximumCodexJSONLineBytes = 256 << 10
	codexLogTextLimit         = 2_000
	codexCommandTextLimit     = 300
	codexCommandOutputLimit   = 8_000
)

// codexJSONOutputFormatter converts the JSONL protocol written by
// `codex exec --json` into the concise, human-readable task log used by the
// device UI.  Frame boundaries are unrelated to JSONL line boundaries, so the
// formatter intentionally owns a small per-run pending buffer.
type codexJSONOutputFormatter struct {
	pending             []byte
	discardUntilNewline bool
	sessionID           string
	commands            map[string]codexCommandSummary
}

func newCodexJSONOutputFormatter() *codexJSONOutputFormatter {
	return &codexJSONOutputFormatter{commands: make(map[string]codexCommandSummary)}
}

func (formatter *codexJSONOutputFormatter) SessionID() string {
	if formatter == nil {
		return ""
	}
	return formatter.sessionID
}

func (formatter *codexJSONOutputFormatter) Feed(contents []byte) []string {
	if formatter == nil || len(contents) == 0 {
		return nil
	}
	var output []string
	for len(contents) > 0 {
		if formatter.discardUntilNewline {
			newline := bytes.IndexByte(contents, '\n')
			if newline < 0 {
				return output
			}
			formatter.discardUntilNewline = false
			contents = contents[newline+1:]
			continue
		}

		newline := bytes.IndexByte(contents, '\n')
		if newline < 0 {
			if len(formatter.pending)+len(contents) > maximumCodexJSONLineBytes {
				formatter.pending = nil
				formatter.discardUntilNewline = true
				return append(output, "[警告] Codex 结构化输出行过长，已跳过该行。\n")
			}
			formatter.pending = append(formatter.pending, contents...)
			return output
		}
		if len(formatter.pending)+newline > maximumCodexJSONLineBytes {
			formatter.pending = nil
			contents = contents[newline+1:]
			output = append(output, "[警告] Codex 结构化输出行过长，已跳过该行。\n")
			continue
		}
		formatter.pending = append(formatter.pending, contents[:newline]...)
		output = append(output, formatter.formatLine(formatter.pending)...)
		formatter.pending = nil
		contents = contents[newline+1:]
	}
	return output
}

func (formatter *codexJSONOutputFormatter) Flush() []string {
	if formatter == nil || formatter.discardUntilNewline || len(formatter.pending) == 0 {
		return nil
	}
	output := formatter.formatLine(formatter.pending)
	formatter.pending = nil
	return output
}

func (formatter *codexJSONOutputFormatter) formatLine(raw []byte) []string {
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		raw = raw[:len(raw)-1]
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return codexTypedLogText("诊断", string(raw), codexLogTextLimit)
	}
	event, ok := decoded.(map[string]any)
	if !ok {
		return nil
	}
	formatter.captureSession(event)
	return formatter.formatEvent(event)
}

func (formatter *codexJSONOutputFormatter) captureSession(event map[string]any) {
	if formatter == nil || formatter.sessionID != "" {
		return
	}
	for _, key := range []string{"thread_id", "threadId", "session_id", "sessionId"} {
		candidate, ok := event[key].(string)
		candidate = strings.TrimSpace(candidate)
		if ok && validTaskCliSessionID(candidate) {
			formatter.sessionID = candidate
			return
		}
	}
}

type codexCommandSummary struct {
	label string
	paths []string
}

func (formatter *codexJSONOutputFormatter) formatEvent(event map[string]any) []string {
	eventType, _ := event["type"].(string)
	switch eventType {
	case "thread.started":
		return codexTypedLogText("状态", "Codex 会话已连接。", codexLogTextLimit)
	case "turn.started":
		return codexTypedLogText("状态", "正在执行任务…", codexLogTextLimit)
	case "turn.completed":
		return codexTypedLogText("状态", "任务执行完成。", codexLogTextLimit)
	case "error", "turn.failed":
		message := codexJSONErrorMessage(event)
		if message == "" {
			message = "未知错误"
		}
		return codexTypedLogText("错误", "任务执行失败："+message, codexLogTextLimit)
	case "item.started", "item.completed":
		// Handled below.
	default:
		return nil
	}

	item, ok := event["item"].(map[string]any)
	if !ok {
		return nil
	}
	itemType, _ := item["type"].(string)
	switch itemType {
	case "agent_message":
		if eventType != "item.completed" {
			return nil
		}
		text, _ := item["text"].(string)
		return codexTypedLogText("回复", text, codexLogTextLimit)
	case "reasoning":
		if eventType != "item.completed" {
			return nil
		}
		text, _ := item["text"].(string)
		return codexTypedLogText("思考", text, codexLogTextLimit)
	case "command_execution":
		itemID, _ := item["id"].(string)
		if eventType == "item.started" {
			command, _ := item["command"].(string)
			summary := summarizeCodexCommand(command)
			if itemID != "" {
				formatter.commands[itemID] = summary
			}
			if summary.label != "" {
				return codexCommandSummaryLines(summary)
			}
			if command == "" {
				return codexTypedLogText("执行命令", "命令内容不可用。", codexCommandTextLimit)
			}
			return codexTypedLogText("执行命令", command, codexCommandTextLimit)
		}
		summary := formatter.commands[itemID]
		delete(formatter.commands, itemID)
		if summary.label == "" {
			command, _ := item["command"].(string)
			summary = summarizeCodexCommand(command)
		}
		exitCode, hasExitCode := item["exit_code"]
		if summary.label != "" {
			if hasExitCode && fmt.Sprint(exitCode) != "0" {
				return codexTypedLogText("错误", summary.label+"失败（退出码 "+safeCodexLogText(fmt.Sprint(exitCode), 64)+"）。", codexLogTextLimit)
			}
			// File contents and patch bodies are intentionally omitted. The start
			// event already recorded the useful operation and path.
			return nil
		}
		var output []string
		if contents, _ := item["aggregated_output"].(string); contents != "" {
			output = append(output, codexTypedLogText("命令输出", contents, codexCommandOutputLimit)...)
		}
		if hasExitCode && exitCode != nil {
			output = append(output, codexTypedLogText("命令结果", "退出码 "+safeCodexLogText(fmt.Sprint(exitCode), 64)+"。", 96)...)
		}
		return output
	case "file_change":
		if eventType == "item.completed" {
			return codexFileChangeLines(item)
		}
	case "mcp_tool_call":
		if eventType == "item.started" {
			server, _ := item["server"].(string)
			tool, _ := item["tool"].(string)
			name := strings.Trim(strings.TrimSpace(server)+"/"+strings.TrimSpace(tool), "/")
			if name == "" {
				name = "工具名称不可用"
			}
			return codexTypedLogText("调用工具", name, codexCommandTextLimit)
		}
	case "web_search":
		if eventType == "item.started" {
			query, _ := item["query"].(string)
			if strings.TrimSpace(query) == "" {
				query = "搜索内容不可用"
			}
			return codexTypedLogText("网络搜索", query, codexCommandTextLimit)
		}
	case "plan_update":
		if eventType == "item.completed" {
			return codexPlanLines(item)
		}
	}
	return nil
}

var (
	codexPowerShellPathPattern = regexp.MustCompile(`(?i)-(?:Literal)?Path\s+(?:"([^"]+)"|'([^']+)'|([^\s;|]+))`)
	codexShellReadPathPattern  = regexp.MustCompile(`(?i)\b(?:cat|head|tail)\s+(?:-[^\s]+\s+)*(?:"([^"]+)"|'([^']+)'|([^\s;|]+))`)
	codexRedirectPathPattern   = regexp.MustCompile(`(?:>>|>)\s*(?:"([^"]+)"|'([^']+)'|([^&\s;|]+))`)
	codexPatchPathPattern      = regexp.MustCompile(`(?m)^\*\*\*\s+(?:Add|Update|Delete) File:\s*(.+?)\s*$`)
)

func summarizeCodexCommand(command string) codexCommandSummary {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return codexCommandSummary{}
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "go test") || strings.Contains(lower, "flutter test") ||
		strings.Contains(lower, "dart test") || strings.Contains(lower, "npm test") ||
		strings.Contains(lower, "pnpm test") || strings.Contains(lower, "cargo test") ||
		strings.Contains(lower, "pytest") || strings.Contains(lower, "dotnet test") {
		return codexCommandSummary{}
	}
	if strings.Contains(lower, "*** begin patch") || strings.Contains(lower, "apply_patch") ||
		strings.Contains(lower, "set-content") || strings.Contains(lower, "add-content") ||
		strings.Contains(lower, "out-file") || codexRedirectPathPattern.MatchString(trimmed) {
		paths := codexCommandPaths(trimmed, codexPatchPathPattern, codexPowerShellPathPattern, codexRedirectPathPattern)
		return codexCommandSummary{label: "写入文件", paths: paths}
	}
	if strings.Contains(lower, "get-content") || strings.Contains(lower, "select-string") ||
		strings.HasPrefix(lower, "cat ") || strings.HasPrefix(lower, "head ") ||
		strings.HasPrefix(lower, "tail ") || strings.HasPrefix(lower, "type ") {
		paths := codexCommandPaths(trimmed, codexPowerShellPathPattern, codexShellReadPathPattern)
		return codexCommandSummary{label: "读取文件", paths: paths}
	}
	return codexCommandSummary{}
}

func codexCommandPaths(command string, patterns ...*regexp.Regexp) []string {
	seen := make(map[string]struct{})
	var paths []string
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatch(command, -1) {
			candidate := ""
			for index := 1; index < len(match); index++ {
				if strings.TrimSpace(match[index]) != "" {
					candidate = strings.TrimSpace(match[index])
					break
				}
			}
			candidate = strings.Trim(candidate, "\"'")
			if candidate == "" || strings.HasPrefix(candidate, "$") {
				continue
			}
			if _, exists := seen[candidate]; exists {
				continue
			}
			seen[candidate] = struct{}{}
			paths = append(paths, candidate)
		}
	}
	return paths
}

func codexCommandSummaryLines(summary codexCommandSummary) []string {
	if len(summary.paths) == 0 {
		return codexTypedLogText(summary.label, "文件路径不可用。", codexCommandTextLimit)
	}
	var output []string
	for _, path := range summary.paths {
		output = append(output, codexTypedLogText(summary.label, path, codexCommandTextLimit)...)
	}
	return output
}

func codexFileChangeLines(item map[string]any) []string {
	changes, _ := item["changes"].([]any)
	var output []string
	for _, raw := range changes {
		change, _ := raw.(map[string]any)
		path, _ := change["path"].(string)
		kind, _ := change["kind"].(string)
		label := "写入文件"
		if strings.EqualFold(kind, "delete") {
			label = "删除文件"
		}
		if strings.TrimSpace(path) != "" {
			output = append(output, codexTypedLogText(label, path, codexCommandTextLimit)...)
		}
	}
	if len(output) == 0 {
		return codexTypedLogText("写入文件", "文件路径不可用。", codexCommandTextLimit)
	}
	return output
}

func codexPlanLines(item map[string]any) []string {
	plan, _ := item["plan"].([]any)
	var output []string
	for _, raw := range plan {
		entry, _ := raw.(map[string]any)
		step, _ := entry["step"].(string)
		status, _ := entry["status"].(string)
		text := strings.TrimSpace(strings.TrimSpace(status) + " " + strings.TrimSpace(step))
		if text != "" {
			output = append(output, codexTypedLogText("计划", text, codexLogTextLimit)...)
		}
	}
	return output
}

func codexJSONErrorMessage(event map[string]any) string {
	if rawError, found := event["error"]; found {
		if structured, ok := rawError.(map[string]any); ok {
			if message, ok := structured["message"].(string); ok {
				return message
			}
		} else if message, ok := rawError.(string); ok {
			return message
		}
	}
	message, _ := event["message"].(string)
	return message
}

func codexTypedLogText(label, value string, maximumRunes int) []string {
	if value == "" {
		return nil
	}
	value = safeCodexLogText(value, maximumRunes)
	if value == "" {
		return nil
	}
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(strings.TrimSuffix(value, "\n"), "\n")
	output := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		output = append(output, "["+label+"] "+line+"\n")
	}
	return output
}

func safeCodexLogText(value string, maximumRunes int) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	// JSON is decoded before this point. Clean only its textual fields using
	// the same bounded state machine as other non-interactive output; regexes
	// cannot safely handle OSC/DCS strings split across process chunks.
	sanitizer := newVTTextSanitizer()
	value = sanitizer.Feed(value) + sanitizer.Flush()
	if maximumRunes <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maximumRunes {
		return value
	}
	return string(runes[:maximumRunes-1]) + "…"
}
