package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	taskV2ResumePrompt = "请恢复并继续执行上次中断的任务。先检查当前工作区和会话中的已有进展，从中断处继续完成剩余要求并验证结果；不要重复已经完成的工作。"
	// Codex versions that expose the stable `goals` feature but no longer
	// expose `exec --goal` need the same explicit goal instruction WenzMark
	// uses. The prefix is written to private stdin, never argv or task logs.
	taskV2CodexGoalPromptPrefix = "请把下面的内容作为持续目标，创建目标并持续工作，直到需求完整实现和验证：\n\n"
)

type managedTaskPrompt struct {
	Path string
}

func buildTaskV2Prompt(project registeredProject, task taskV2Record) ([]byte, error) {
	if task.Definition.ProjectID != project.ID || task.Definition.Kind == "script" || task.Definition.Kind == "workflow" {
		return nil, errRPCInvalid
	}
	config, err := decodeTaskRunnerConfig(task.Definition.Config)
	if err != nil {
		return nil, err
	}
	var prompt string
	if task.Definition.Execution.ResumeCliSession {
		prompt = taskV2ResumePrompt
	} else {
		source := taskConfigString(config, "promptSource")
		switch source {
		case "customText":
			prompt = taskConfigString(config, "promptText")
		case "currentMarkdownFile":
			relative := taskConfigString(config, "promptFilePath")
			absolute, _, pathErr := secureExistingProjectPath(project, relative)
			if pathErr != nil {
				return nil, taskRunnerPreparationError{code: "prompt_file_unavailable", message: "The task prompt file is unavailable."}
			}
			info, statErr := os.Stat(absolute)
			if statErr != nil || !info.Mode().IsRegular() {
				return nil, taskRunnerPreparationError{code: "prompt_file_unavailable", message: "The task prompt file is unavailable."}
			}
			prompt = "请读取并执行以下 Markdown 文件中的全部任务要求，完成必要修改并验证结果：\n" + absolute
		default:
			return nil, taskRunnerPreparationError{code: "config_invalid", message: "The task prompt source is invalid."}
		}
		if strings.TrimSpace(prompt) == "" {
			return nil, taskRunnerPreparationError{code: "prompt_empty", message: "The task prompt is empty."}
		}
		if gitURL := strings.TrimSpace(project.GitURL); gitURL != "" {
			prompt += "\n\nThis project is configured with Git remote: " + gitURL +
				"\nIf the requested work needs project initialization or repository sync, use this remote in the current project directory."
		}
		if plan := strings.TrimSpace(task.Definition.Plan); plan != "" {
			prompt += "\n\n执行方案（请按此方案实施并完成验证）：\n" + plan
		}
		attachments, ok := config["attachedFilePaths"].([]any)
		if !ok && config["attachedFilePaths"] != nil {
			return nil, taskRunnerPreparationError{code: "config_invalid", message: "Task attachments are invalid."}
		}
		resolved := make([]string, 0, len(attachments))
		for _, raw := range attachments {
			relative, ok := raw.(string)
			if !ok {
				return nil, taskRunnerPreparationError{code: "config_invalid", message: "Task attachments are invalid."}
			}
			absolute, _, pathErr := secureExistingProjectPath(project, relative)
			if pathErr != nil {
				return nil, taskRunnerPreparationError{code: "attachment_unavailable", message: "A task attachment is unavailable."}
			}
			info, statErr := os.Stat(absolute)
			if statErr != nil || !info.Mode().IsRegular() {
				return nil, taskRunnerPreparationError{code: "attachment_unavailable", message: "A task attachment is unavailable."}
			}
			resolved = append(resolved, absolute)
		}
		if len(resolved) > 0 {
			prompt += "\n\n关联文件（请按需读取）：\n- " + strings.Join(resolved, "\n- ")
		}
	}
	contents := []byte(prompt)
	if len(contents) == 0 || len(contents) > maximumTaskDefinitionBytes || strings.IndexByte(prompt, 0) >= 0 {
		return nil, taskRunnerPreparationError{code: "prompt_too_large", message: "The expanded task prompt is too large."}
	}
	return contents, nil
}

func buildTaskV2ScriptInput(task taskV2Record) ([]byte, error) {
	if task.Definition.Kind != "script" {
		return nil, errRPCInvalid
	}
	config, err := decodeTaskRunnerConfig(task.Definition.Config)
	if err != nil {
		return nil, err
	}
	command, ok := config["command"].(string)
	if !ok || strings.TrimSpace(command) == "" || len([]byte(command)) > maximumTaskCommandBytes || strings.IndexByte(command, 0) >= 0 {
		return nil, taskRunnerPreparationError{code: "config_invalid", message: "Script command is invalid."}
	}
	if !strings.HasSuffix(command, "\n") {
		command += "\n"
	}
	return []byte(command), nil
}

func createManagedTaskPrompt(statePath string, taskID, runID uuid.UUID, contents []byte) (managedTaskPrompt, error) {
	if strings.TrimSpace(statePath) == "" || taskID == uuid.Nil || runID == uuid.Nil || len(contents) == 0 || len(contents) > maximumTaskDefinitionBytes {
		return managedTaskPrompt{}, errRPCInvalid
	}
	path, err := managedTaskPromptPath(statePath, taskID, runID)
	if err != nil {
		return managedTaskPrompt{}, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return managedTaskPrompt{}, fmt.Errorf("create private task prompt: %w", err)
	}
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		return managedTaskPrompt{}, fmt.Errorf("write private task prompt: %w", err)
	}
	if err := file.Sync(); err != nil {
		return managedTaskPrompt{}, fmt.Errorf("sync private task prompt: %w", err)
	}
	if err := file.Close(); err != nil {
		return managedTaskPrompt{}, fmt.Errorf("close private task prompt: %w", err)
	}
	if err := secureStateFile(path); err != nil {
		return managedTaskPrompt{}, fmt.Errorf("protect private task prompt: %w", err)
	}
	failed = false
	return managedTaskPrompt{Path: path}, nil
}

// managedTaskPromptPath reserves the deterministic private path passed to a
// runner adapter before the file is published. Some CLIs consume the path as
// an argv reference, while stdin-based runners receive its bounded contents
// through a supervised pipe. The file itself is created only after the adapter
// has selected any prompt compatibility prefix.
func managedTaskPromptPath(statePath string, taskID, runID uuid.UUID) (string, error) {
	if strings.TrimSpace(statePath) == "" || taskID == uuid.Nil || runID == uuid.Nil {
		return "", errRPCInvalid
	}
	directory := statePath + ".task-runtime"
	if err := preparePrivateConversationDirectory(directory); err != nil {
		return "", fmt.Errorf("create private task runtime directory: %w", err)
	}
	return filepath.Join(directory, taskID.String()+"-"+runID.String()+".prompt.md"), nil
}

func applyTaskRunnerPromptPrefix(contents []byte, prefix string) ([]byte, error) {
	if prefix == "" {
		return contents, nil
	}
	if strings.IndexByte(prefix, 0) >= 0 || len(prefix)+len(contents) > maximumTaskDefinitionBytes {
		return nil, taskRunnerPreparationError{code: "prompt_too_large", message: "The expanded task prompt is too large."}
	}
	result := make([]byte, 0, len(prefix)+len(contents))
	result = append(result, prefix...)
	result = append(result, contents...)
	return result, nil
}

func (prompt managedTaskPrompt) Cleanup() error {
	if prompt.Path == "" {
		return nil
	}
	return os.Remove(prompt.Path)
}

func cleanupManagedTaskPrompts(statePath string) error {
	if strings.TrimSpace(statePath) == "" {
		return errRPCInvalid
	}
	directory := statePath + ".task-runtime"
	_, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := preparePrivateConversationDirectory(directory); err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".prompt.md") {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
