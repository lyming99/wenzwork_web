package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumTerminalCommandBytes = 512
	maximumTerminalOutputBytes  = 32 << 10
	terminalExecutionTimeout    = 10 * time.Second
)

// terminalCommand is deliberately not a shell command.  Parsing the small
// read-only vocabulary before creating a process keeps quoting, pipes,
// redirections and executable substitution out of the device Agent.
type terminalCommand struct {
	display string
	gitArgs []string
	list    bool
	pwd     bool
}

func parseTerminalCommand(value string) (terminalCommand, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximumTerminalCommandBytes || !utf8.ValidString(value) {
		return terminalCommand{}, errRPCInvalid
	}
	tokens := strings.Fields(value)
	if len(tokens) == 1 && tokens[0] == "pwd" {
		return terminalCommand{display: "pwd", pwd: true}, nil
	}
	if len(tokens) == 1 && (tokens[0] == "ls" || tokens[0] == "dir") {
		return terminalCommand{display: tokens[0], list: true}, nil
	}
	if slicesEqual(tokens, []string{"git", "status"}) {
		return terminalCommand{display: "git status", gitArgs: []string{"status"}}, nil
	}
	if slicesEqual(tokens, []string{"git", "diff", "--stat"}) {
		return terminalCommand{display: "git diff --stat", gitArgs: []string{"diff", "--stat"}}, nil
	}
	if slicesEqual(tokens, []string{"git", "log"}) {
		return terminalCommand{display: "git log", gitArgs: []string{"log", "-n", "10", "--oneline"}}, nil
	}
	if len(tokens) == 4 && tokens[0] == "git" && tokens[1] == "log" && tokens[2] == "-n" {
		count, err := strconv.Atoi(tokens[3])
		if err == nil && count >= 1 && count <= 50 {
			return terminalCommand{display: "git log -n " + strconv.Itoa(count), gitArgs: []string{"log", "-n", strconv.Itoa(count), "--oneline"}}, nil
		}
	}
	return terminalCommand{}, errRPCInvalid
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// limitedTerminalBuffer accepts all process output to avoid blocking a child
// process while retaining only the bounded response allowed by Peer RPC.
type limitedTerminalBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *limitedTerminalBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return len(value), nil
	}
	if len(value) > remaining {
		_, _ = buffer.Buffer.Write(value[:remaining])
		buffer.truncated = true
		return len(value), nil
	}
	_, _ = buffer.Buffer.Write(value)
	return len(value), nil
}

func (d dispatcher) callTerminalExecute(ctx context.Context, input rpcInput) (any, uint64, error) {
	commandText, ok := inputString(input, "command", maximumTerminalCommandBytes)
	if !ok {
		return nil, 0, errRPCInvalid
	}
	command, err := parseTerminalCommand(commandText)
	if err != nil {
		return nil, 0, err
	}
	workingDirectory, err := d.terminalWorkingDirectory(input)
	if err != nil {
		return nil, 0, err
	}
	if command.pwd {
		if err := completeV2WithoutSideEffect(ctx); err != nil {
			return nil, 0, err
		}
		return map[string]any{"command": command.display, "workingDirectory": workingDirectory, "output": workingDirectory, "exitCode": 0, "truncated": false}, d.state.revisionValue(), nil
	}
	if command.list {
		if err := completeV2WithoutSideEffect(ctx); err != nil {
			return nil, 0, err
		}
		return terminalList(workingDirectory, command.display, d.state.revisionValue())
	}

	executionContext, cancel := context.WithTimeout(ctx, terminalExecutionTimeout)
	defer cancel()
	result := exec.CommandContext(executionContext, "git", command.gitArgs...)
	configureBackgroundProcess(result)
	result.Dir = workingDirectory
	result.Stdin = strings.NewReader("")
	environment, err := reviewedProcessEnvironment(d.state.agentEnvironmentList())
	if err != nil {
		return nil, 0, err
	}
	result.Env = environment
	var stdout, stderr limitedTerminalBuffer
	stdout.limit, stderr.limit = maximumTerminalOutputBytes, maximumTerminalOutputBytes
	result.Stdout, result.Stderr = &stdout, &stderr
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	runErr := result.Run()
	exitCode := 0
	if runErr != nil {
		var processErr *exec.ExitError
		if errors.As(runErr, &processErr) {
			exitCode = processErr.ExitCode()
		} else if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(executionContext.Err(), context.DeadlineExceeded) {
			return nil, 0, context.DeadlineExceeded
		} else {
			return nil, 0, fmt.Errorf("run constrained git command: %w", runErr)
		}
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	output := strings.TrimSpace(stdout.String())
	if message := strings.TrimSpace(stderr.String()); message != "" {
		if output != "" {
			output += "\n"
		}
		output += message
	}
	return map[string]any{
		"command": command.display, "workingDirectory": workingDirectory, "output": output,
		"exitCode": exitCode, "truncated": stdout.truncated || stderr.truncated,
	}, d.state.revisionValue(), nil
}

func (d dispatcher) terminalWorkingDirectory(input rpcInput) (string, error) {
	// Project-bound Peer RPCs carry their authority in the signed ticket and
	// encrypted request header. Do not accept a second, caller-controlled
	// project ID in the JSON payload, which could otherwise drift from the
	// ticket binding. The old payload form remains only for v1 local/direct
	// callers that do not have a request header.
	if strings.TrimSpace(d.requestProjectID) != "" {
		if _, supplied := input["projectId"]; supplied {
			return "", errRPCInvalid
		}
		project, err := d.fileProject()
		if err != nil {
			return "", err
		}
		resolved, _, err := secureExistingProjectPath(project, "")
		if err != nil {
			return "", err
		}
		return resolved, nil
	}
	projectID, ok := optionalInputString(input, "projectId", 80)
	if !ok {
		return "", errRPCInvalid
	}
	relativePath := ""
	if projectID != "" {
		parsed, err := uuid.Parse(projectID)
		if err != nil || parsed == uuid.Nil {
			return "", errRPCInvalid
		}
		projects, err := scanWorkspaceProjects(context.Background(), d.state)
		if err != nil {
			return "", err
		}
		project, found := projects[parsed.String()]
		if !found || project.State != "available" {
			return "", errRPCNotFound
		}
		relativePath = project.RelativePath
	}
	resolved, _, err := secureExistingWorkspacePath(d.state, relativePath)
	if err != nil {
		if errors.Is(err, errRPCForbidden) {
			return "", err
		}
		return "", errRPCNotFound
	}
	return resolved, nil
}

func terminalList(workingDirectory, command string, revision uint64) (any, uint64, error) {
	entries, err := os.ReadDir(workingDirectory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, errRPCNotFound
		}
		return nil, 0, err
	}
	lines := make([]string, 0, len(entries))
	truncated := false
	for index, entry := range entries {
		if index >= 500 {
			truncated = true
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += string(filepath.Separator)
		}
		lines = append(lines, name)
	}
	output := strings.Join(lines, "\n")
	if len(output) > maximumTerminalOutputBytes {
		output = output[:maximumTerminalOutputBytes]
		truncated = true
	}
	return map[string]any{"command": command, "workingDirectory": workingDirectory, "output": output, "exitCode": 0, "truncated": truncated}, revision, nil
}

func minimalTerminalEnvironment() []string {
	// PowerShell uses PATHEXT to classify native commands as console
	// applications. If it is absent, a command such as `codex` can be handed
	// to the Windows default-terminal broker and escape the current ConPTY into
	// a visible Windows Terminal window. Keep the value inherited from the
	// Agent host; reviewedProcessEnvironment protects it from task overrides.
	keys := []string{"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "HOME", "USERPROFILE", "TMP", "TEMP"}
	values := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, found := os.LookupEnv(key); found {
			values = append(values, key+"="+value)
		}
	}
	return values
}
