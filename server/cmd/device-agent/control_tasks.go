package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

const (
	maximumTaskReadmeBytes = 1 << 20
	maximumTaskOutputBytes = 2 << 20
	maximumInspectedFiles  = 100000
)

var (
	errTaskInput       = errors.New("task input is invalid")
	errTaskUnsupported = errors.New("task type is unsupported")
	errTaskUnavailable = errors.New("task dependency is unavailable")
)

type projectSyncCommandBody struct {
	ProjectID          *uuid.UUID `json:"projectId,omitempty"`
	AfterSequence      uint64     `json:"afterSequence"`
	KnownHighWatermark uint64     `json:"knownHighWatermark"`
}

type taskCreateCommandBody struct {
	TaskID           uuid.UUID       `json:"taskId"`
	ProjectID        *uuid.UUID      `json:"projectId,omitempty"`
	TaskType         string          `json:"taskType"`
	Title            string          `json:"title"`
	ExpectedRevision *uint64         `json:"expectedRevision,omitempty"`
	Input            json.RawMessage `json:"input"`
}

type taskCancelCommandBody struct {
	TaskID uuid.UUID `json:"taskId"`
}

type workspaceInspectInput struct {
	IncludeHidden bool `json:"includeHidden,omitempty"`
	MaxDepth      int  `json:"maxDepth,omitempty"`
}

type markdownRenderInput struct {
	Theme string `json:"theme,omitempty"`
}

type aiSummarizeInput struct {
	ConfigID      string `json:"configId,omitempty"`
	MaxCharacters int    `json:"maxCharacters,omitempty"`
}

type taskExecutionResult struct {
	Status     string
	ResultCode string
	Log        string
}

type taskAICompleter func(context.Context, aiConfig, string) (string, error)

func decodeProjectSyncCommand(raw json.RawMessage) (projectSyncCommandBody, error) {
	var body projectSyncCommandBody
	if err := decodeClosedCommandJSON(raw, &body); err != nil || body.ProjectID != nil && *body.ProjectID == uuid.Nil {
		return projectSyncCommandBody{}, errTaskInput
	}
	return body, nil
}

func decodeTaskCreateCommand(raw json.RawMessage, commandTaskID *uuid.UUID) (localTaskSpec, error) {
	var body taskCreateCommandBody
	if err := decodeClosedCommandJSON(raw, &body); err != nil || body.TaskID == uuid.Nil || commandTaskID == nil || *commandTaskID != body.TaskID ||
		body.ProjectID != nil && *body.ProjectID == uuid.Nil || strings.TrimSpace(body.Title) == "" || len(body.Title) > 200 || len(body.TaskType) > 80 {
		return localTaskSpec{}, errTaskInput
	}
	canonicalInput, err := validateTypedTaskInput(body.TaskType, body.Input)
	if err != nil {
		return localTaskSpec{}, err
	}
	return localTaskSpec{
		TaskID: body.TaskID, ProjectID: body.ProjectID, TaskType: body.TaskType, Title: strings.TrimSpace(body.Title),
		ExpectedRevision: body.ExpectedRevision, Input: canonicalInput,
	}, nil
}

func decodeTaskCancelCommand(raw json.RawMessage, commandTaskID *uuid.UUID) (uuid.UUID, error) {
	var body taskCancelCommandBody
	if err := decodeClosedCommandJSON(raw, &body); err != nil || body.TaskID == uuid.Nil || commandTaskID == nil || *commandTaskID != body.TaskID {
		return uuid.Nil, errTaskInput
	}
	return body.TaskID, nil
}

func decodeClosedCommandJSON(raw json.RawMessage, destination any) error {
	if len(raw) == 0 || len(raw) > 24<<10 || !json.Valid(raw) {
		return errTaskInput
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(new(any)) != io.EOF {
		return errTaskInput
	}
	return nil
}

func validateTypedTaskInput(taskType string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	switch taskType {
	case "workspace.inspect":
		var input workspaceInspectInput
		if decodeClosedCommandJSON(raw, &input) != nil || input.MaxDepth < 0 || input.MaxDepth > 8 {
			return nil, errTaskInput
		}
		if input.MaxDepth == 0 {
			input.MaxDepth = 4
		}
		return json.Marshal(input)
	case "markdown.render":
		var input markdownRenderInput
		if decodeClosedCommandJSON(raw, &input) != nil || input.Theme != "" && input.Theme != "light" && input.Theme != "dark" {
			return nil, errTaskInput
		}
		if input.Theme == "" {
			input.Theme = "light"
		}
		return json.Marshal(input)
	case "ai.summarize":
		var input aiSummarizeInput
		if decodeClosedCommandJSON(raw, &input) != nil || len(input.ConfigID) > 80 || input.MaxCharacters < 0 || input.MaxCharacters > 32768 {
			return nil, errTaskInput
		}
		if input.ConfigID == "" {
			input.ConfigID = "default"
		}
		if input.MaxCharacters == 0 {
			input.MaxCharacters = 16000
		}
		return json.Marshal(input)
	default:
		return nil, errTaskUnsupported
	}
}

func (loop *deviceControlLoop) executeTask(ctx context.Context, spec localTaskSpec) taskExecutionResult {
	root, err := loop.taskProjectRoot(spec.ProjectID, spec.ExpectedRevision)
	if err != nil {
		return taskExecutionResult{Status: "failed", ResultCode: "project_unavailable", Log: "Project is unavailable."}
	}
	switch spec.TaskType {
	case "workspace.inspect":
		var input workspaceInspectInput
		if json.Unmarshal(spec.Input, &input) != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "invalid_task_input", Log: "Task input is invalid."}
		}
		files, directories, total, err := inspectWorkspace(ctx, root, input)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return taskExecutionResult{Status: "cancelled", ResultCode: "cancelled", Log: "Task was cancelled."}
		}
		if err != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "inspection_failed", Log: "Workspace inspection failed."}
		}
		return taskExecutionResult{Status: "succeeded", ResultCode: "ok", Log: fmt.Sprintf("Inspected %d files and %d directories (%d bytes).", files, directories, total)}
	case "markdown.render":
		var input markdownRenderInput
		if json.Unmarshal(spec.Input, &input) != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "invalid_task_input", Log: "Task input is invalid."}
		}
		source, err := readProjectReadme(ctx, root, maximumTaskReadmeBytes)
		if err != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "readme_unavailable", Log: "README is unavailable."}
		}
		var rendered bytes.Buffer
		engine := goldmark.New(goldmark.WithExtensions(extension.GFM))
		if err := engine.Convert(source, &rendered); err != nil || rendered.Len() > maximumTaskOutputBytes {
			return taskExecutionResult{Status: "failed", ResultCode: "render_failed", Log: "Markdown rendering failed."}
		}
		page := []byte("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"color-scheme\" content=\"" + input.Theme + "\"></head><body>" + rendered.String() + "</body></html>\n")
		if err := atomicGeneratedTaskOutput(root, "readme.html", page); err != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "output_failed", Log: "Rendered output could not be stored."}
		}
		return taskExecutionResult{Status: "succeeded", ResultCode: "ok", Log: fmt.Sprintf("Rendered README to HTML (%d bytes).", len(page))}
	case "ai.summarize":
		var input aiSummarizeInput
		if json.Unmarshal(spec.Input, &input) != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "invalid_task_input", Log: "Task input is invalid."}
		}
		config, exists := loop.state.aiConfigSnapshot(input.ConfigID)
		if !exists || !config.Enabled || validateAIConfig(config) != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "ai_unavailable", Log: "AI configuration is unavailable."}
		}
		source, err := readProjectReadme(ctx, root, int64(input.MaxCharacters))
		if err != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "readme_unavailable", Log: "README is unavailable."}
		}
		complete := loop.aiComplete
		if complete == nil {
			complete = defaultTaskAICompletion(loop.taskHTTPClient)
		}
		summary, err := complete(ctx, config, string(source))
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return taskExecutionResult{Status: "cancelled", ResultCode: "cancelled", Log: "Task was cancelled."}
		}
		if err != nil || summary == "" || len(summary) > maximumTaskOutputBytes || !utf8.ValidString(summary) || strings.IndexByte(summary, 0) >= 0 {
			return taskExecutionResult{Status: "failed", ResultCode: "ai_failed", Log: "AI summary generation failed."}
		}
		if err := atomicGeneratedTaskOutput(root, "summary.md", []byte(summary+"\n")); err != nil {
			return taskExecutionResult{Status: "failed", ResultCode: "output_failed", Log: "Summary output could not be stored."}
		}
		return taskExecutionResult{Status: "succeeded", ResultCode: "ok", Log: fmt.Sprintf("Generated AI summary (%d characters).", utf8.RuneCountInString(summary))}
	default:
		return taskExecutionResult{Status: "rejected", ResultCode: "unsupported_task_type", Log: "Task type is unsupported."}
	}
}

func (loop *deviceControlLoop) taskProjectRoot(projectID *uuid.UUID, expectedRevision *uint64) (string, error) {
	if projectID == nil {
		return secureTaskRoot(loop.state, "")
	}
	snapshot, err := loop.store.snapshot()
	if err != nil {
		return "", err
	}
	project, exists := snapshot.Sync.Projects[projectID.String()]
	if !exists || project.State != "available" || expectedRevision != nil && project.Revision != *expectedRevision {
		return "", errTaskUnavailable
	}
	return secureTaskRoot(loop.state, project.RelativePath)
}

func secureTaskRoot(state *agentState, relative string) (string, error) {
	resolved, _, err := secureExistingWorkspacePath(state, relative)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return "", errTaskUnavailable
	}
	return resolved, nil
}

func inspectWorkspace(ctx context.Context, root string, input workspaceInspectInput) (files, directories int, total int64, resultErr error) {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		depth := 0
		if relative != "." {
			depth = strings.Count(filepath.ToSlash(relative), "/") + 1
		}
		if depth > input.MaxDepth {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if relative != "." && !input.IncludeHidden && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			if relative != "." {
				directories++
			}
			return nil
		}
		if info.Mode().IsRegular() {
			files++
			total += info.Size()
			if files > maximumInspectedFiles {
				return errTaskUnavailable
			}
		}
		return nil
	})
	return files, directories, total, err
}

func readProjectReadme(ctx context.Context, root string, maximum int64) ([]byte, error) {
	for _, name := range []string{"README.md", "README.markdown", "readme.md"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 || info.Size() > maximum {
			return nil, errTaskUnavailable
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(contents)) > maximum || !utf8.Valid(contents) || bytes.IndexByte(contents, 0) >= 0 {
			return nil, errTaskUnavailable
		}
		return contents, nil
	}
	return nil, errTaskUnavailable
}

func atomicGeneratedTaskOutput(root, name string, contents []byte) error {
	if !validFileName(name) || len(contents) > maximumTaskOutputBytes {
		return errTaskInput
	}
	directory := filepath.Join(root, ".wenzwork-output")
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(directory, 0o700); mkdirErr != nil && !os.IsExist(mkdirErr) {
			return mkdirErr
		}
		// Multiple tasks for the same project may create the output directory
		// concurrently. Re-inspect after creation (or EEXIST) so that the loser
		// accepts a real directory but still rejects a symlink/junction swap.
		info, err = os.Lstat(directory)
	}
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errTaskUnavailable
	}
	target := filepath.Join(directory, name)
	before, beforeErr := os.Lstat(target)
	if beforeErr == nil && (!before.Mode().IsRegular() || before.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0) {
		return errTaskUnavailable
	}
	if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
		return beforeErr
	}
	temporary, err := os.CreateTemp(directory, ".wenzwork-task-output-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) error { _ = temporary.Close(); return cause }
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
	current, currentErr := os.Lstat(target)
	if beforeErr == nil {
		if currentErr != nil || !os.SameFile(before, current) || current.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errRPCRevision
		}
		return os.Rename(temporaryPath, target)
	}
	if !errors.Is(currentErr, os.ErrNotExist) {
		return errRPCRevision
	}
	if err := os.Link(temporaryPath, target); err != nil {
		return err
	}
	return os.Remove(temporaryPath)
}

func defaultTaskAICompletion(_ *http.Client) taskAICompleter {
	return func(ctx context.Context, config aiConfig, source string) (string, error) {
		if validateAIConfig(config) != nil {
			return "", errTaskUnavailable
		}
		prompt := "Summarize this project README concisely in Markdown. Do not reproduce secrets or credentials.\n\n" + source
		result, err := (openAICompatibleProvider{}).Complete(ctx, config, nil, prompt)
		if err != nil {
			return "", errTaskUnavailable
		}
		return result, nil
	}
}
