package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCodexJSONOutputFormatterStreamsEventsAcrossFrames(t *testing.T) {
	formatter := newCodexJSONOutputFormatter()
	events := []map[string]any{
		{"type": "thread.started", "thread_id": "019c-thread-123"},
		{"type": "turn.started"},
		{"type": "item.completed", "item": map[string]any{"type": "reasoning", "text": "先检查现状\n再执行测试"}},
		{"type": "item.started", "item": map[string]any{"type": "command_execution", "command": "go test ./..."}},
		{"type": "item.completed", "item": map[string]any{"type": "command_execution", "aggregated_output": "\x1b]0;title\x07\x1b[32mall good\x1b[0m\x1bPprivate\x1b\\", "exit_code": 0}},
		{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "完成"}},
		{"type": "turn.completed"},
	}
	var raw strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(encoded)
		raw.WriteByte('\n')
	}

	contents := raw.String()
	split := strings.Index(contents, "thread_id") + 3
	if split <= 3 || split >= len(contents) {
		t.Fatalf("unexpected test split for %q", contents)
	}
	if output := formatter.Feed([]byte(contents[:split])); len(output) != 0 {
		t.Fatalf("partial JSON produced output: %#v", output)
	}
	output := append([]string(nil), formatter.Feed([]byte(contents[split:]))...)
	output = append(output, formatter.Flush()...)
	rendered := strings.Join(output, "")

	if formatter.SessionID() != "019c-thread-123" {
		t.Fatalf("session ID = %q", formatter.SessionID())
	}
	for _, expected := range []string{"[状态] Codex 会话已连接", "[思考] 先检查现状", "[思考] 再执行测试", "[执行命令] go test ./...", "[命令输出] all good", "[命令结果] 退出码 0", "[回复] 完成", "任务执行完成"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("formatted output %q does not contain %q", rendered, expected)
		}
	}
	if strings.Contains(rendered, "\x1b") || strings.Contains(rendered, "title") || strings.Contains(rendered, "private") || strings.Contains(rendered, `{"type":`) {
		t.Fatalf("unformatted control or JSON output leaked: %q", rendered)
	}
}

func TestCodexJSONOutputFormatterKeepsPlainTextFallback(t *testing.T) {
	formatter := newCodexJSONOutputFormatter()
	output := formatter.Feed([]byte("a non-json diagnostic\n"))
	if got, want := strings.Join(output, ""), "[诊断] a non-json diagnostic\n"; got != want {
		t.Fatalf("fallback output = %q, want %q", got, want)
	}
}

func TestCodexJSONOutputFormatterOmitsFileContentsAndKeepsPaths(t *testing.T) {
	formatter := newCodexJSONOutputFormatter()
	events := []map[string]any{
		{"type": "item.started", "item": map[string]any{
			"id": "read-1", "type": "command_execution",
			"command": `Get-Content -Raw -LiteralPath 'docs/task.md'`,
		}},
		{"type": "item.completed", "item": map[string]any{
			"id": "read-1", "type": "command_execution", "aggregated_output": "private file body", "exit_code": 0,
		}},
		{"type": "item.started", "item": map[string]any{
			"id": "write-1", "type": "command_execution",
			"command": "apply_patch <<'PATCH'\n*** Begin Patch\n*** Update File: lib/app.dart\n+private patch body\n*** End Patch\nPATCH",
		}},
		{"type": "item.completed", "item": map[string]any{
			"id": "write-1", "type": "command_execution", "aggregated_output": "Done!", "exit_code": 0,
		}},
		{"type": "item.completed", "item": map[string]any{
			"type": "file_change", "changes": []any{
				map[string]any{"path": "lib/app.dart", "kind": "update"},
				map[string]any{"path": "obsolete.txt", "kind": "delete"},
			},
		}},
	}
	var rendered strings.Builder
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		rendered.WriteString(strings.Join(formatter.Feed(append(encoded, '\n')), ""))
	}
	got := rendered.String()
	for _, expected := range []string{"[读取文件] docs/task.md", "[写入文件] lib/app.dart", "[删除文件] obsolete.txt"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("formatted output %q does not contain %q", got, expected)
		}
	}
	if strings.Contains(got, "private file body") || strings.Contains(got, "private patch body") || strings.Contains(got, "Done!") {
		t.Fatalf("file contents leaked into formatted task log: %q", got)
	}
}

func TestCodexFormattedMessagesGainTimestampAndTypePrefixes(t *testing.T) {
	formatter := newCodexJSONOutputFormatter()
	event, err := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": "第一行\n第二行"},
	})
	if err != nil {
		t.Fatal(err)
	}
	formatted := strings.Join(formatter.Feed(append(event, '\n')), "")
	when := time.Date(2026, 8, 26, 1, 43, 12, 345000000, time.UTC)
	got := string(encodeTaskRunLogRecords("stdout", formatted, nil, when))
	want := "2026-08-26T01:43:12.345Z [stdout] [回复] 第一行\n" +
		"2026-08-26T01:43:12.345Z [stdout] [回复] 第二行\n"
	if got != want {
		t.Fatalf("persisted formatted log = %q, want %q", got, want)
	}
}

func TestCodexJSONOutputFormatterFlushesFinalEvent(t *testing.T) {
	formatter := newCodexJSONOutputFormatter()
	if output := formatter.Feed([]byte(`{"type":"turn.completed"}`)); len(output) != 0 {
		t.Fatalf("unterminated event produced output early: %#v", output)
	}
	if got := strings.Join(formatter.Flush(), ""); !strings.Contains(got, "执行完成") {
		t.Fatalf("final event was not flushed: %q", got)
	}
}
