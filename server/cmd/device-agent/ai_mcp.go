package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	maximumAIMCPServers     = 4
	maximumAIMCPToolsTotal  = 64
	maximumAIMCPResultBytes = 64 << 10
	aiMCPInitializeTimeout  = 10 * time.Second
	aiMCPCallTimeout        = 60 * time.Second
	aiMCPWriteTimeout       = 5 * time.Second
)

// aiMCPServerConfig describes one stdio MCP server from the operator
// configuration (WENZWORK_AGENT_MCP_SERVERS JSON array).
type aiMCPServerConfig struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func loadAIMCPServerConfigs() []aiMCPServerConfig {
	raw := strings.TrimSpace(os.Getenv("WENZWORK_AGENT_MCP_SERVERS"))
	if raw == "" {
		return nil
	}
	var configs []aiMCPServerConfig
	if json.Unmarshal([]byte(raw), &configs) != nil {
		return nil
	}
	result := make([]aiMCPServerConfig, 0, len(configs))
	seen := make(map[string]struct{})
	for _, config := range configs {
		if !validAIMCPName(config.Name) || strings.TrimSpace(config.Command) == "" || len(config.Args) > 32 ||
			len(result) >= maximumAIMCPServers {
			continue
		}
		if _, duplicate := seen[config.Name]; duplicate {
			continue
		}
		seen[config.Name] = struct{}{}
		result = append(result, config)
	}
	return result
}

func validAIMCPName(name string) bool {
	if len(name) < 1 || len(name) > 32 {
		return false
	}
	for _, character := range name {
		if character != '_' && character != '-' && (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func aiMCPToolFullName(server, tool string) string {
	return "mcp__" + server + "__" + tool
}

// aiMCPProcess abstracts the spawned stdio server so tests can inject fakes.
type aiMCPProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Close() error
}

type aiMCPCommandProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
}

func (process *aiMCPCommandProcess) Stdin() io.WriteCloser { return process.stdin }
func (process *aiMCPCommandProcess) Stdout() io.ReadCloser { return process.stdout }
func (process *aiMCPCommandProcess) Close() error {
	if process.command.Process != nil {
		_ = process.command.Process.Kill()
	}
	return process.command.Wait()
}

var aiMCPProcessFactory = func(config aiMCPServerConfig) (aiMCPProcess, error) {
	command, err := taskExecCommand(config.Command, config.Args)
	if err != nil {
		return nil, err
	}
	configureBackgroundProcess(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &aiMCPCommandProcess{command: command, stdin: stdin, stdout: stdout}, nil
}

type aiMCPResponse struct {
	Result json.RawMessage
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type aiMCPClient struct {
	name    string
	process aiMCPProcess
	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan aiMCPResponse
	closed  bool
}

func startAIMCPClient(config aiMCPServerConfig) (*aiMCPClient, error) {
	process, err := aiMCPProcessFactory(config)
	if err != nil {
		return nil, err
	}
	client := &aiMCPClient{name: config.Name, process: process, pending: make(map[uint64]chan aiMCPResponse)}
	go client.readLoop()
	ctx, cancel := context.WithTimeout(context.Background(), aiMCPInitializeTimeout)
	defer cancel()
	if _, err := client.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "wenzwork-device-agent", "version": "1"},
	}); err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize MCP server %s: %w", config.Name, err)
	}
	_ = client.notify("notifications/initialized")
	return client, nil
}

func (client *aiMCPClient) readLoop() {
	scanner := bufio.NewScanner(client.process.Stdout())
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			ID     *uint64         `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &envelope) != nil || envelope.ID == nil {
			continue
		}
		client.mu.Lock()
		pending := client.pending[*envelope.ID]
		delete(client.pending, *envelope.ID)
		client.mu.Unlock()
		if pending != nil {
			pending <- aiMCPResponse{Result: envelope.Result, Error: envelope.Error}
		}
	}
	client.mu.Lock()
	client.closed = true
	for id, pending := range client.pending {
		delete(client.pending, id)
		pending <- aiMCPResponse{Error: &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -32000, Message: "MCP server closed its output"}}
	}
	client.mu.Unlock()
}

func (client *aiMCPClient) send(message []byte) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return errors.New("MCP client is closed")
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.process.Stdin().Write(append(message, '\n'))
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(aiMCPWriteTimeout):
		return errors.New("MCP stdin write timed out")
	}
}

func (client *aiMCPClient) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return nil, errors.New("MCP client is closed")
	}
	client.nextID++
	id := client.nextID
	pending := make(chan aiMCPResponse, 1)
	client.pending[id] = pending
	client.mu.Unlock()
	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		client.mu.Lock()
		delete(client.pending, id)
		client.mu.Unlock()
		return nil, err
	}
	if err := client.send(message); err != nil {
		client.mu.Lock()
		delete(client.pending, id)
		client.mu.Unlock()
		return nil, err
	}
	select {
	case response := <-pending:
		if response.Error != nil {
			return nil, fmt.Errorf("MCP %s failed: %s", method, response.Error.Message)
		}
		return response.Result, nil
	case <-ctx.Done():
		client.mu.Lock()
		delete(client.pending, id)
		client.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (client *aiMCPClient) notify(method string) error {
	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	return client.send(message)
}

type aiMCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (client *aiMCPClient) listTools(ctx context.Context) ([]aiMCPTool, error) {
	result, err := client.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Tools []aiMCPTool `json:"tools"`
	}
	if json.Unmarshal(result, &envelope) != nil {
		return nil, errors.New("MCP tools/list returned an invalid payload")
	}
	return envelope.Tools, nil
}

// callTool invokes one tool and returns its text content plus the server-side
// isError flag. Text content is bounded.
func (client *aiMCPClient) callTool(ctx context.Context, name string, arguments map[string]any) (string, bool, error) {
	result, err := client.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return "", true, err
	}
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(result, &envelope) != nil {
		return "", true, errors.New("MCP tools/call returned an invalid payload")
	}
	var builder strings.Builder
	for _, part := range envelope.Content {
		if part.Type == "text" {
			builder.WriteString(part.Text)
		}
	}
	text := builder.String()
	if len(text) > maximumAIMCPResultBytes {
		text = truncateAIUTF8(text, maximumAIMCPResultBytes)
	}
	return text, envelope.IsError, nil
}

func (client *aiMCPClient) Close() {
	if client == nil {
		return
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return
	}
	client.closed = true
	process := client.process
	client.mu.Unlock()
	if process != nil {
		_ = process.Close()
	}
}

type aiMCPToolEntry struct {
	ServerConfig aiMCPServerConfig
	Tool         aiMCPTool
}

var aiMCPState struct {
	mu      sync.Mutex
	clients map[string]*aiMCPClient
	tools   map[string]aiMCPToolEntry
}

func (client *aiMCPClient) isClosed() bool {
	if client == nil {
		return true
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.closed
}

func aiMCPEnsureClient(config aiMCPServerConfig) *aiMCPClient {
	aiMCPState.mu.Lock()
	if aiMCPState.clients == nil {
		aiMCPState.clients = make(map[string]*aiMCPClient)
	}
	if client, found := aiMCPState.clients[config.Name]; found && !client.isClosed() {
		aiMCPState.mu.Unlock()
		return client
	}
	delete(aiMCPState.clients, config.Name)
	aiMCPState.mu.Unlock()
	client, err := startAIMCPClient(config)
	if err != nil {
		return nil
	}
	aiMCPState.mu.Lock()
	// Another goroutine may have won the race; keep the existing client.
	if existing, found := aiMCPState.clients[config.Name]; found && !existing.isClosed() {
		aiMCPState.mu.Unlock()
		client.Close()
		return existing
	}
	aiMCPState.clients[config.Name] = client
	aiMCPState.mu.Unlock()
	return client
}

func aiMCPToolForName(fullName string) (aiMCPToolEntry, bool) {
	aiMCPState.mu.Lock()
	defer aiMCPState.mu.Unlock()
	entry, found := aiMCPState.tools[fullName]
	return entry, found
}

// aiMCPToolDefinitions connects configured servers and renders their tools as
// mcp__<server>__<tool> definitions. Failures are per-server and silent: a
// broken server simply contributes no tools.
func aiMCPToolDefinitions(ctx context.Context) []aiWorkspaceToolDefinition {
	configs := loadAIMCPServerConfigs()
	if len(configs) == 0 {
		return nil
	}
	definitions := make([]aiWorkspaceToolDefinition, 0, maximumAIMCPToolsTotal)
	aiMCPState.mu.Lock()
	if aiMCPState.tools == nil {
		aiMCPState.tools = make(map[string]aiMCPToolEntry)
	}
	aiMCPState.mu.Unlock()
	for _, config := range configs {
		client := aiMCPEnsureClient(config)
		if client == nil {
			continue
		}
		tools, err := client.listTools(ctx)
		if err != nil {
			continue
		}
		for _, tool := range tools {
			if len(definitions) >= maximumAIMCPToolsTotal {
				return definitions
			}
			if !validAIMCPName(tool.Name) || len(tool.Description) > 2048 || tool.InputSchema == nil {
				continue
			}
			schema := tool.InputSchema
			if kind, _ := schema["type"].(string); kind != "object" {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			encoded, err := json.Marshal(schema)
			if err != nil || len(encoded) > 16<<10 {
				continue
			}
			fullName := aiMCPToolFullName(config.Name, tool.Name)
			aiMCPState.mu.Lock()
			aiMCPState.tools[fullName] = aiMCPToolEntry{ServerConfig: config, Tool: tool}
			aiMCPState.mu.Unlock()
			definitions = append(definitions, aiWorkspaceToolDefinition{
				Name:        fullName,
				Description: "[MCP " + config.Name + "] " + tool.Description,
				InputSchema: schema,
			})
		}
	}
	return definitions
}
