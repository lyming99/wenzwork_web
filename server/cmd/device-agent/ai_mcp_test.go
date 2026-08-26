package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// fakeAIMCPProcess is an in-memory JSON-RPC stdio server for tests.
type fakeAIMCPProcess struct {
	stdinReader  *io.PipeReader
	stdinWriter  *io.PipeWriter
	stdoutReader *io.PipeReader
	stdoutWriter *io.PipeWriter
	once         sync.Once
}

func newFakeAIMCPProcess(t *testing.T) *fakeAIMCPProcess {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	process := &fakeAIMCPProcess{
		stdinReader: stdinReader, stdinWriter: stdinWriter,
		stdoutReader: stdoutReader, stdoutWriter: stdoutWriter,
	}
	go process.serve()
	t.Cleanup(func() { _ = process.Close() })
	return process
}

func (process *fakeAIMCPProcess) Stdin() io.WriteCloser  { return process.stdinWriter }
func (process *fakeAIMCPProcess) Stdout() io.ReadCloser { return process.stdoutReader }
func (process *fakeAIMCPProcess) Close() error {
	process.once.Do(func() {
		_ = process.stdinWriter.Close()
		_ = process.stdoutWriter.Close()
		_ = process.stdinReader.Close()
		_ = process.stdoutReader.Close()
	})
	return nil
}

func (process *fakeAIMCPProcess) serve() {
	scanner := bufio.NewScanner(process.stdinReader)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *uint64         `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		if request.ID == nil {
			continue
		}
		var response any
		switch request.Method {
		case "initialize":
			response = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			}
		case "tools/list":
			response = map[string]any{"tools": []any{
				map[string]any{
					"name": "add", "description": "Add two numbers.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
						"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"},
					}, "required": []string{"a", "b"}},
				},
				map[string]any{
					"name": "echo", "description": "Echo text.",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
						"text": map[string]any{"type": "string"},
					}},
				},
			}}
		case "tools/call":
			var call struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &call)
			switch call.Name {
			case "add":
				a, _ := call.Arguments["a"].(float64)
				b, _ := call.Arguments["b"].(float64)
				response = map[string]any{"content": []any{map[string]any{"type": "text", "text": jsonNumber(a + b)}}}
			case "echo":
				text, _ := call.Arguments["text"].(string)
				response = map[string]any{"content": []any{map[string]any{"type": "text", "text": "echo:" + text}}}
			default:
				response = map[string]any{"error": map[string]any{"code": -32601, "message": "unknown tool"}}
			}
		default:
			response = map[string]any{"error": map[string]any{"code": -32601, "message": "method not found"}}
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": response})
		_, _ = process.stdoutWriter.Write(append(encoded, '\n'))
	}
}

func jsonNumber(value float64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestAIMCPClientHandshakeListAndCall(t *testing.T) {
	fake := newFakeAIMCPProcess(t)
	previous := aiMCPProcessFactory
	aiMCPProcessFactory = func(aiMCPServerConfig) (aiMCPProcess, error) { return fake, nil }
	t.Cleanup(func() { aiMCPProcessFactory = previous })
	client, err := startAIMCPClient(aiMCPServerConfig{Name: "demo", Command: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.listTools(context.Background())
	if err != nil || len(tools) != 2 || tools[0].Name != "add" {
		t.Fatalf("tools=%+v error=%v", tools, err)
	}
	text, isError, err := client.callTool(context.Background(), "add", map[string]any{"a": float64(2), "b": float64(3)})
	if err != nil || isError || text != "5" {
		t.Fatalf("call text=%q isError=%v error=%v", text, isError, err)
	}
	text, isError, err = client.callTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil || isError || text != "echo:hi" {
		t.Fatalf("echo text=%q isError=%v error=%v", text, isError, err)
	}
}

func TestAIMCPToolDefinitionsAndExecutorCall(t *testing.T) {
	fake := newFakeAIMCPProcess(t)
	previous := aiMCPProcessFactory
	aiMCPProcessFactory = func(aiMCPServerConfig) (aiMCPProcess, error) { return fake, nil }
	t.Cleanup(func() { aiMCPProcessFactory = previous })
	t.Setenv("WENZWORK_AGENT_MCP_SERVERS", `[{"name":"demo","command":"fake"}]`)
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeReadOnly)
	definitions := aiMCPToolDefinitions(t.Context())
	if len(definitions) != 2 || definitions[0].Name != "mcp__demo__add" ||
		!strings.Contains(definitions[0].Description, "[MCP demo]") {
		t.Fatalf("definitions = %+v", definitions)
	}
	plan := planAIWorkspaceTool(t, fixture, "mcp__demo__add", map[string]any{"a": float64(4), "b": float64(5)})
	if plan.RequiresApproval || plan.Preview.Risk != "readOnly" {
		t.Fatalf("mcp plan = %+v", plan)
	}
	result := fixture.executor.Execute(t.Context(), fixture.context, plan, false)
	if result.IsError || !strings.Contains(result.Content, "9") || result.Metadata["source_kind"] != "mcp" || result.Metadata["untrusted"] != true {
		t.Fatalf("mcp result = %+v", result)
	}
	if !aiProviderToolResultUntrusted(result) {
		t.Fatal("mcp result must be untrusted")
	}
	// Unknown MCP tools fail closed at Plan.
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "mcp__demo__missing", Arguments: map[string]any{},
	}); err == nil {
		t.Fatal("unknown mcp tool plan must fail")
	}
}

func TestAIConversationToolLoopCallsMCPTool(t *testing.T) {
	fake := newFakeAIMCPProcess(t)
	previous := aiMCPProcessFactory
	aiMCPProcessFactory = func(aiMCPServerConfig) (aiMCPProcess, error) { return fake, nil }
	t.Cleanup(func() { aiMCPProcessFactory = previous })
	t.Setenv("WENZWORK_AGENT_MCP_SERVERS", `[{"name":"demo","command":"fake"}]`)
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{"text": "hello-mcp"})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "mcp-call-1", Name: "mcp__demo__echo", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "MCP 调用完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "调用 MCP 工具",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	if len(prompts) != 2 {
		t.Fatalf("provider calls = %d", len(prompts))
	}
	result := prompts[1].ToolExchanges[0].Results[0]
	if result.IsError || !strings.Contains(result.Content, "echo:hello-mcp") || !result.Untrusted {
		t.Fatalf("mcp loop result = %+v", result)
	}
}

func TestLoadAIMCPServerConfigsValidation(t *testing.T) {
	t.Setenv("WENZWORK_AGENT_MCP_SERVERS", `[
		{"name":"good","command":"tool"},
		{"name":"bad name","command":"tool"},
		{"name":"good","command":"dup"},
		{"name":"","command":"tool"},
		{"name":"empty-cmd","command":""}
	]`)
	configs := loadAIMCPServerConfigs()
	if len(configs) != 1 || configs[0].Name != "good" {
		t.Fatalf("configs = %+v", configs)
	}
	t.Setenv("WENZWORK_AGENT_MCP_SERVERS", "not json")
	if configs := loadAIMCPServerConfigs(); configs != nil {
		t.Fatalf("invalid env must yield no configs: %+v", configs)
	}
}
