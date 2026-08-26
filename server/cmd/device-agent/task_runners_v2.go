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
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	taskRunnerProbeCommandTimeout  = 5 * time.Second
	taskRunnerProbeSequenceTimeout = 20 * time.Second
	maximumTaskProbeBytes          = 256 << 10
)

var taskRunnerKinds = []string{"codex", "cursor", "hermes", "jcode", "opencode", "claude", "kimi", "pi", "script"}

type taskRunnerCapability struct {
	Kind           string            `json:"kind"`
	Available      bool              `json:"available"`
	ProbeStatus    string            `json:"probeStatus"`
	Version        string            `json:"version,omitempty"`
	Models         []string          `json:"models"`
	ModelDetails   []taskRunnerModel `json:"modelDetails,omitempty"`
	ModelDiscovery bool              `json:"modelDiscovery"`
	SupportsResume bool              `json:"supportsResume"`
	Features       map[string]bool   `json:"features"`
	ErrorCode      string            `json:"errorCode,omitempty"`

	executable string
}

// taskRunnerModel is the non-sensitive model metadata exposed to clients.
// It intentionally mirrors WenzMark's Codex model/list projection: model IDs
// and UI hints are safe to return, while credentials and provider state remain
// on the device.
type taskRunnerModel struct {
	ID                        string                      `json:"id"`
	Model                     string                      `json:"model"`
	DisplayName               string                      `json:"displayName"`
	Description               string                      `json:"description,omitempty"`
	DefaultReasoningEffort    string                      `json:"defaultReasoningEffort,omitempty"`
	SupportedReasoningEfforts []taskRunnerReasoningEffort `json:"supportedReasoningEfforts,omitempty"`
	Hidden                    bool                        `json:"hidden,omitempty"`
	IsDefault                 bool                        `json:"isDefault,omitempty"`
}

type taskRunnerReasoningEffort struct {
	ReasoningEffort string `json:"reasoningEffort"`
	Description     string `json:"description,omitempty"`
}

type taskRunnerInvocation struct {
	Executable     string
	Arguments      []string
	Environment    []string
	UseStdinFile   bool
	PromptPrefix   string
	CliSessionID   string
	ParseCodexJSON bool
}

type taskRunnerProvider interface {
	Prepare(context.Context, registeredProject, taskV2Record, string) (taskRunnerInvocation, error)
	Capabilities() []taskRunnerCapability
	Refresh(context.Context)
}

type taskRunnerRegistry struct {
	mu             sync.RWMutex
	capabilities   map[string]taskRunnerCapability
	probeLocks     map[string]*sync.Mutex
	resolve        func(string) (string, error)
	runProbe       func(context.Context, string, []string) ([]byte, error)
	discoverModels func(context.Context, string) ([]taskRunnerModel, error)
}

type taskRunnerPreparationError struct {
	code    string
	message string
}

func (err taskRunnerPreparationError) Error() string { return err.message }

func newTaskRunnerRegistry(environmentProvider ...func() []string) *taskRunnerRegistry {
	registry := &taskRunnerRegistry{
		capabilities:   make(map[string]taskRunnerCapability, len(taskRunnerKinds)),
		probeLocks:     make(map[string]*sync.Mutex, len(taskRunnerKinds)),
		resolve:        resolveSupervisedExecutable,
		runProbe:       runTaskRunnerProbe,
		discoverModels: discoverCodexModels,
	}
	if len(environmentProvider) > 0 && environmentProvider[0] != nil {
		provider := environmentProvider[0]
		registry.runProbe = func(ctx context.Context, executable string, arguments []string) ([]byte, error) {
			return runTaskRunnerProbeWithEnvironment(ctx, executable, arguments, provider())
		}
		registry.discoverModels = func(ctx context.Context, executable string) ([]taskRunnerModel, error) {
			return discoverCodexModelsWithEnvironment(ctx, executable, provider())
		}
	}
	for _, kind := range taskRunnerKinds {
		registry.probeLocks[kind] = new(sync.Mutex)
		capability := taskRunnerCapability{
			Kind: kind, ProbeStatus: "pending", Models: []string{}, Features: map[string]bool{},
		}
		if kind == "script" {
			capability.Available, capability.ProbeStatus, capability.Version = true, "ready", "system-shell"
		} else if executable, err := registry.resolveFirst(kind); err == nil {
			capability.Available, capability.executable = true, executable
		} else {
			capability.ProbeStatus, capability.ErrorCode = "ready", "runner_not_installed"
		}
		registry.capabilities[kind] = capability
	}
	return registry
}

func (registry *taskRunnerRegistry) Capabilities() []taskRunnerCapability {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	result := make([]taskRunnerCapability, 0, len(taskRunnerKinds))
	for _, kind := range taskRunnerKinds {
		capability := registry.capabilities[kind]
		capability.Models = append([]string(nil), capability.Models...)
		capability.ModelDetails = cloneTaskRunnerModels(capability.ModelDetails)
		capability.Features = cloneTaskRunnerFeatures(capability.Features)
		capability.executable = ""
		result = append(result, capability)
	}
	return result
}

func (registry *taskRunnerRegistry) Refresh(ctx context.Context) {
	if registry == nil {
		return
	}
	var group sync.WaitGroup
	for _, kind := range taskRunnerKinds {
		if kind == "script" {
			continue
		}
		group.Add(1)
		go func(kind string) {
			defer group.Done()
			_, _ = registry.probe(ctx, kind, true)
		}(kind)
	}
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (registry *taskRunnerRegistry) Prepare(
	ctx context.Context,
	project registeredProject,
	task taskV2Record,
	managedPromptPath string,
) (taskRunnerInvocation, error) {
	if registry == nil || task.Definition.ProjectID != project.ID || !slices.Contains(taskRunnerKinds, task.Definition.Kind) {
		return taskRunnerInvocation{}, taskRunnerPreparationError{code: "runner_unsupported", message: "Task runner is unsupported."}
	}
	if task.Definition.Kind == "script" {
		return prepareScriptTaskRunner(task)
	}
	capability, err := registry.probe(ctx, task.Definition.Kind, false)
	if err != nil || !capability.Available || capability.executable == "" {
		// A system service commonly starts before the first interactive login.
		// Do not let that startup-time miss poison the registry for the entire
		// boot: retry discovery when an actual task reaches the runner.
		capability, err = registry.probe(ctx, task.Definition.Kind, true)
		if err != nil || !capability.Available || capability.executable == "" {
			if errors.Is(err, errTaskExecutionContextUnavailable) {
				return taskRunnerInvocation{}, taskRunnerPreparationError{
					code:    "execution_context_unavailable",
					message: "A signed-in user is required to run tasks on this device.",
				}
			}
			return taskRunnerInvocation{}, taskRunnerPreparationError{code: "runner_unavailable", message: "The selected task runner is unavailable."}
		}
	}
	config, err := decodeTaskRunnerConfig(task.Definition.Config)
	if err != nil {
		return taskRunnerInvocation{}, err
	}
	promptRequired := task.Definition.Kind != "script"
	if promptRequired && managedPromptPath == "" {
		return taskRunnerInvocation{}, taskRunnerPreparationError{code: "prompt_unavailable", message: "The private task prompt is unavailable."}
	}

	resumeID := ""
	if task.Definition.Execution.ResumeCliSession {
		resumeID = strings.TrimSpace(task.Definition.Execution.CliSessionID)
		if !capability.SupportsResume || !validTaskCliSessionID(resumeID) {
			return taskRunnerInvocation{}, taskRunnerPreparationError{code: "resume_unsupported", message: "The task runner cannot resume this session."}
		}
	}
	environment := taskRunnerEnvironment(task.Definition.Environment)
	invocation := taskRunnerInvocation{Executable: capability.executable, Environment: environment}
	workingDirectory, _, err := secureExistingProjectPath(project, task.Definition.CWD)
	if err != nil {
		return taskRunnerInvocation{}, taskRunnerPreparationError{code: "working_directory_unavailable", message: "The task working directory is unavailable."}
	}

	switch task.Definition.Kind {
	case "codex":
		invocation.Executable, err = codexExecutableForLaunchMode(config, invocation.Executable)
		if err == nil {
			invocation.Arguments, err = prepareCodexArguments(config, capability, resumeID, workingDirectory)
		}
		if err == nil && resumeID == "" && taskConfigBool(config, "goalMode") &&
			!capability.Features["goal"] && capability.Features["goals"] {
			invocation.PromptPrefix = taskV2CodexGoalPromptPrefix
		}
		invocation.UseStdinFile = true
		// Codex emits a JSONL event stream only when --json was selected. Keep
		// that decision with the invocation so task execution can render the
		// protocol while older CLIs retain their plain-text output.
		invocation.ParseCodexJSON = capability.Features["json"]
		invocation.CliSessionID = resumeID
	case "cursor":
		invocation.Arguments, err = prepareCursorArguments(config, capability, resumeID)
		invocation.UseStdinFile = true
	case "hermes":
		invocation.Arguments, err = prepareHermesArguments(config, capability, managedPromptPath, resumeID)
	case "jcode":
		invocation.Arguments, err = prepareJcodeArguments(config, capability, managedPromptPath, resumeID)
		invocation.Environment = append(invocation.Environment, "NO_COLOR=1", "JCODE_NO_EMOJI=1")
	case "opencode":
		invocation.Arguments, err = prepareOpenCodeArguments(config, capability, resumeID)
		invocation.UseStdinFile = true
	case "claude":
		invocation.Arguments, invocation.CliSessionID, err = prepareClaudeArguments(config, capability, resumeID)
		invocation.Environment, err = mergeClaudeTaskEnvironment(invocation.Environment, config, err)
		invocation.UseStdinFile = true
	case "kimi":
		invocation.Arguments, invocation.UseStdinFile, err = prepareKimiArguments(config, capability, resumeID)
	case "pi":
		invocation.Arguments, err = preparePiArguments(capability)
		invocation.UseStdinFile = true
	default:
		err = taskRunnerPreparationError{code: "runner_unsupported", message: "Task runner is unsupported."}
	}
	if err != nil {
		return taskRunnerInvocation{}, err
	}
	return invocation, nil
}

func codexExecutableForLaunchMode(config map[string]any, cliExecutable string) (string, error) {
	switch taskConfigString(config, "launchMode") {
	case "", "cli":
		return cliExecutable, nil
	case "windowsNativeExe":
		executable, err := resolveWindowsNativeCodexExecutable(cliExecutable)
		if err != nil {
			return "", taskRunnerPreparationError{
				code:    "native_executable_unavailable",
				message: "The Windows native Codex executable is unavailable.",
			}
		}
		return executable, nil
	default:
		return "", taskRunnerPreparationError{code: "config_invalid", message: "Codex launch mode is invalid."}
	}
}

func (registry *taskRunnerRegistry) probe(ctx context.Context, kind string, refresh bool) (taskRunnerCapability, error) {
	if registry == nil || !slices.Contains(taskRunnerKinds, kind) {
		return taskRunnerCapability{}, errors.New("task runner kind is invalid")
	}
	if kind == "script" {
		registry.mu.RLock()
		defer registry.mu.RUnlock()
		return registry.capabilities[kind], nil
	}
	lock := registry.probeLocks[kind]
	lock.Lock()
	defer lock.Unlock()
	registry.mu.RLock()
	current := registry.capabilities[kind]
	registry.mu.RUnlock()
	if !refresh && current.ProbeStatus == "ready" {
		if current.Available {
			return current, nil
		}
		return current, errors.New(current.ErrorCode)
	}

	executable, err := registry.resolveFirst(kind)
	if err != nil {
		current = taskRunnerCapability{
			Kind: kind, Available: false, ProbeStatus: "ready", Models: []string{}, Features: map[string]bool{}, ErrorCode: "runner_not_installed",
		}
		registry.setCapability(kind, current)
		return current, err
	}
	probeContext, cancel := context.WithTimeout(ctx, taskRunnerProbeSequenceTimeout)
	defer cancel()
	versionOutput, versionErr := registry.runProbe(probeContext, executable, []string{"--version"})
	if versionErr != nil {
		current = taskRunnerCapability{
			Kind: kind, Available: false, ProbeStatus: "ready", Models: []string{}, Features: map[string]bool{}, ErrorCode: "runner_probe_failed",
		}
		registry.setCapability(kind, current)
		return current, versionErr
	}
	helpArguments := []string{"--help"}
	if kind == "codex" {
		helpArguments = []string{"exec", "--help"}
	} else if kind == "opencode" {
		helpArguments = []string{"run", "--help"}
	}
	helpOutput, helpErr := registry.runProbe(probeContext, executable, helpArguments)
	if helpErr != nil {
		current = taskRunnerCapability{
			Kind: kind, Available: false, ProbeStatus: "ready", Models: []string{}, Features: map[string]bool{}, ErrorCode: "runner_probe_failed",
		}
		registry.setCapability(kind, current)
		return current, helpErr
	}
	version := safeTaskRunnerVersion(versionOutput)
	help := string(helpOutput)
	if kind == "codex" {
		// Current Codex versions expose --ask-for-approval as a top-level
		// option, while exec-specific options such as --json and --goal are
		// documented under `codex exec --help`. Probe both surfaces so a
		// valid installed CLI is not incorrectly hidden from task clients.
		if globalHelpOutput, globalHelpErr := registry.runProbe(probeContext, executable, []string{"--help"}); globalHelpErr == nil {
			help += "\n" + string(globalHelpOutput)
		}
	}
	features, compatible, supportsResume := taskRunnerFeatures(kind, help)
	if kind == "codex" {
		// Recent Codex releases expose Goal mode as a stable feature rather
		// than an `exec --goal` flag. Match WenzMark's compatibility adapter:
		// presence in `features list` means the invocation may enable it.
		if featureOutput, featureErr := registry.runProbe(probeContext, executable, []string{"features", "list"}); featureErr == nil {
			features["goals"] = taskRunnerFeatureListed(featureOutput, "goals")
		}
	}
	current = taskRunnerCapability{
		Kind: kind, Available: compatible, ProbeStatus: "ready", Version: version, Models: []string{},
		SupportsResume: supportsResume, Features: features, executable: executable,
	}
	if !compatible {
		current.ErrorCode = "runner_incompatible"
	}
	if compatible && kind == "codex" && registry.discoverModels != nil {
		if models, modelErr := registry.discoverModels(probeContext, executable); modelErr == nil {
			current.ModelDetails = cloneTaskRunnerModels(models)
			current.Models = taskRunnerModelIDs(models)
			current.ModelDiscovery = true
		}
	}
	if compatible && kind == "jcode" {
		if models, modelErr := registry.discoverJcodeModels(probeContext, executable); modelErr == nil {
			current.Models, current.ModelDiscovery = models, true
		}
	}
	registry.setCapability(kind, current)
	if !compatible {
		return current, errors.New(current.ErrorCode)
	}
	return current, nil
}

func (registry *taskRunnerRegistry) setCapability(kind string, capability taskRunnerCapability) {
	registry.mu.Lock()
	registry.capabilities[kind] = capability
	registry.mu.Unlock()
}

func (registry *taskRunnerRegistry) resolveFirst(kind string) (string, error) {
	var resolveErr error
	for _, candidate := range taskRunnerExecutableCandidates(kind) {
		resolved, err := registry.resolve(candidate)
		if err == nil {
			return resolved, nil
		}
		resolveErr = errors.Join(resolveErr, err)
	}
	return "", errors.Join(errRPCCapability, resolveErr)
}

func taskRunnerExecutableCandidates(kind string) []string {
	if kind == "cursor" {
		return []string{"agent", "cursor-agent"}
	}
	return []string{kind}
}

func taskRunnerFeatures(kind, help string) (map[string]bool, bool, bool) {
	has := func(flag string) bool { return strings.Contains(help, flag) }
	features := map[string]bool{}
	compatible, resume := true, false
	switch kind {
	case "codex":
		features["json"] = has("--json")
		features["goal"] = has("--goal")
		features["approval"] = has("--ask-for-approval")
		features["sandbox"] = has("--sandbox")
		resume = strings.Contains(strings.ToLower(help), "resume")
		compatible = features["approval"] && features["sandbox"]
	case "cursor":
		for _, flag := range []string{"--print", "--force", "--trust", "--sandbox", "--output-format"} {
			features[strings.TrimPrefix(flag, "--")] = has(flag)
		}
		features["partialOutput"] = has("--stream-partial-output")
		resume = has("--resume")
		compatible = features["print"] && features["force"] && features["trust"] && features["sandbox"] && features["output-format"]
	case "hermes":
		features["oneShot"] = has("-z") || strings.Contains(strings.ToLower(help), "one-shot")
		features["yolo"] = has("--yolo")
		features["model"] = has("--model")
		features["provider"] = has("--provider")
		features["usageFile"] = has("--usage-file")
		resume = has("--resume")
		compatible = features["oneShot"] && features["yolo"]
	case "jcode":
		features["run"] = strings.Contains(strings.ToLower(help), "run")
		for _, flag := range []string{"--ndjson", "--quiet", "--no-update", "--no-selfdev", "--tool-profile", "--provider", "--provider-profile", "--model"} {
			features[strings.TrimPrefix(flag, "--")] = has(flag)
		}
		resume = has("--resume")
		compatible = features["run"] && features["ndjson"] && features["quiet"] && features["no-update"] && features["no-selfdev"] && features["tool-profile"]
	case "opencode":
		features["run"] = true
		features["auto"] = has("--auto")
		features["json"] = has("--format") && strings.Contains(strings.ToLower(help), "json")
		resume = has("--session")
		compatible = true
	case "claude":
		features["print"] = has("--print")
		features["dangerous"] = has("--dangerously-skip-permissions") || has("--allow-dangerously-skip-permissions") && has("--permission-mode")
		features["modernDangerous"] = has("--allow-dangerously-skip-permissions") && has("--permission-mode")
		features["streamJson"] = has("--output-format") && strings.Contains(strings.ToLower(help), "stream-json")
		features["effort"] = has("--effort")
		features["thinking"] = has("--thinking")
		features["sessionId"] = has("--session-id")
		resume = has("--resume")
		compatible = features["print"] && features["dangerous"]
	case "kimi":
		features["prompt"] = has("--prompt")
		features["print"] = has("--print")
		features["auto"] = has("--auto") || strings.Contains(strings.ToLower(help), "afk")
		features["streamJson"] = has("--output-format") && strings.Contains(strings.ToLower(help), "stream-json")
		resume = has("--session")
		compatible = features["prompt"] || features["print"]
	case "pi":
		features["print"] = has("--print")
		features["approve"] = has("--approve")
		features["noSession"] = has("--no-session")
		compatible = features["print"]
	default:
		compatible = false
	}
	return features, compatible, resume
}

func prepareScriptTaskRunner(task taskV2Record) (taskRunnerInvocation, error) {
	config, err := decodeTaskRunnerConfig(task.Definition.Config)
	if err != nil {
		return taskRunnerInvocation{}, err
	}
	command, ok := config["command"].(string)
	command = strings.TrimSpace(command)
	if !ok || command == "" {
		return taskRunnerInvocation{}, taskRunnerPreparationError{code: "config_invalid", message: "Script command is invalid."}
	}
	shellName := "sh"
	arguments := []string{"-l"}
	if runtime.GOOS == "windows" {
		shellName, arguments = "cmd", []string{"/d", "/q", "/v:off"}
	}
	shell, err := resolveSupervisedExecutable(shellName)
	if err != nil {
		return taskRunnerInvocation{}, taskRunnerPreparationError{code: "runner_unavailable", message: "The system shell is unavailable."}
	}
	return taskRunnerInvocation{
		Executable: shell, Arguments: arguments, Environment: taskRunnerEnvironment(task.Definition.Environment),
		UseStdinFile: true,
	}, nil
}

func prepareCodexArguments(config map[string]any, capability taskRunnerCapability, resumeID, workingDirectory string) ([]string, error) {
	reasoning := taskConfigString(config, "reasoningEffort")
	if reasoning == "" {
		reasoning = "medium"
	}
	if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max", "ultra"}, reasoning) {
		return nil, taskRunnerPreparationError{code: "config_invalid", message: "Codex reasoning effort is invalid."}
	}
	goal := resumeID == "" && taskConfigBool(config, "goalMode")
	if goal && !capability.Features["goal"] && !capability.Features["goals"] {
		return nil, taskRunnerPreparationError{code: "runner_incompatible", message: "Codex Goal mode is unavailable."}
	}
	arguments := []string{"--ask-for-approval", "never", "--sandbox", "danger-full-access"}
	if model := taskConfigString(config, "model"); model != "" {
		arguments = append(arguments, "--model", model)
	}
	if goal && !capability.Features["goal"] && capability.Features["goals"] {
		arguments = append(arguments, "--enable", "goals")
	}
	arguments = append(arguments, "exec")
	if resumeID != "" {
		arguments = append(arguments, "resume")
	}
	arguments = append(arguments, "--skip-git-repo-check")
	if resumeID == "" {
		arguments = append(arguments, "--cd", workingDirectory)
		if goal && capability.Features["goal"] {
			arguments = append(arguments, "--goal")
		}
	}
	// Keep the TOML string literal free of double quotes. On Windows the CLI
	// resolves to an npm .cmd shim, whose hardened launcher deliberately rejects
	// double quotes before invoking cmd.exe.
	arguments = append(arguments, "-c", fmt.Sprintf("model_reasoning_effort='%s'", reasoning))
	if capability.Features["json"] {
		arguments = append(arguments, "--json")
	}
	if resumeID != "" {
		arguments = append(arguments, resumeID)
	}
	return append(arguments, "-"), nil
}

func taskRunnerFeatureListed(output []byte, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func prepareCursorArguments(config map[string]any, capability taskRunnerCapability, resumeID string) ([]string, error) {
	if !capability.Available {
		return nil, taskRunnerPreparationError{code: "runner_incompatible", message: "Cursor agent mode is unavailable."}
	}
	sandbox := taskConfigString(config, "sandboxMode")
	if sandbox == "" {
		sandbox = "enabled"
	}
	if sandbox != "enabled" && sandbox != "disabled" {
		return nil, taskRunnerPreparationError{code: "config_invalid", message: "Cursor sandbox mode is invalid."}
	}
	arguments := []string{"--print", "--force", "--trust", "--sandbox", sandbox, "--output-format", "stream-json"}
	if capability.Features["partialOutput"] {
		arguments = append(arguments, "--stream-partial-output")
	}
	if resumeID != "" {
		arguments = append(arguments, "--resume", resumeID)
	}
	if model := taskConfigString(config, "model"); model != "" {
		arguments = append(arguments, "--model", model)
	}
	return arguments, nil
}

func prepareHermesArguments(config map[string]any, capability taskRunnerCapability, promptPath, resumeID string) ([]string, error) {
	model, provider := taskConfigString(config, "model"), taskConfigString(config, "provider")
	if provider != "" && model == "" || model != "" && !capability.Features["model"] || provider != "" && !capability.Features["provider"] {
		return nil, taskRunnerPreparationError{code: "runner_incompatible", message: "Hermes model/provider override is unavailable."}
	}
	arguments := []string{"--yolo"}
	if model != "" {
		arguments = append(arguments, "--model", model)
	}
	if provider != "" {
		arguments = append(arguments, "--provider", provider)
	}
	if resumeID != "" {
		arguments = append(arguments, "--resume", resumeID)
	}
	return append(arguments, "-z", "请读取并执行任务说明文件："+promptPath), nil
}

func prepareJcodeArguments(config map[string]any, capability taskRunnerCapability, promptPath, resumeID string) ([]string, error) {
	provider, profile, model := taskConfigString(config, "provider"), taskConfigString(config, "providerProfile"), taskConfigString(config, "model")
	if provider != "" && profile != "" || provider != "" && !capability.Features["provider"] || profile != "" && !capability.Features["provider-profile"] || model != "" && !capability.Features["model"] {
		return nil, taskRunnerPreparationError{code: "runner_incompatible", message: "Jcode task configuration is unsupported by the installed CLI."}
	}
	profileName := taskConfigString(config, "toolProfile")
	if profileName == "" {
		profileName = "full"
	}
	if profileName != "minimal" && profileName != "full" {
		return nil, taskRunnerPreparationError{code: "config_invalid", message: "Jcode tool profile is invalid."}
	}
	arguments := []string{"--quiet", "--no-update", "--no-selfdev", "--tool-profile", profileName}
	if provider != "" {
		arguments = append(arguments, "--provider", provider)
	}
	if profile != "" {
		arguments = append(arguments, "--provider-profile", profile)
	}
	if model != "" {
		arguments = append(arguments, "--model", model)
	}
	if resumeID != "" {
		arguments = append(arguments, "--resume", resumeID)
	}
	return append(arguments, "run", "--ndjson", "请读取并严格执行 WenzWork 托管的 UTF-8 Markdown 任务说明文件："+promptPath+"。完成必要修改和验证后汇总结果。"), nil
}

func prepareOpenCodeArguments(config map[string]any, capability taskRunnerCapability, resumeID string) ([]string, error) {
	auto := true
	if _, found := config["autoMode"]; found {
		auto = taskConfigBool(config, "autoMode")
	}
	if auto && !capability.Features["auto"] {
		return nil, taskRunnerPreparationError{code: "runner_incompatible", message: "OpenCode auto mode is unavailable."}
	}
	arguments := []string{"run"}
	if auto {
		arguments = append(arguments, "--auto")
	}
	if capability.Features["json"] {
		arguments = append(arguments, "--format", "json")
	}
	if resumeID != "" {
		arguments = append(arguments, "--session", resumeID)
	}
	return arguments, nil
}

func prepareClaudeArguments(config map[string]any, capability taskRunnerCapability, resumeID string) ([]string, string, error) {
	arguments := []string{"--print"}
	if capability.Features["modernDangerous"] {
		arguments = append(arguments, "--permission-mode", "bypassPermissions", "--allow-dangerously-skip-permissions")
	} else {
		arguments = append(arguments, "--dangerously-skip-permissions")
	}
	if model := taskConfigString(config, "model"); model != "" {
		arguments = append(arguments, "--model", model)
	}
	if effort := taskConfigString(config, "reasoningEffort"); effort != "" {
		if !slices.Contains([]string{"low", "medium", "high", "xhigh", "max", "ultra"}, effort) {
			return nil, "", taskRunnerPreparationError{code: "config_invalid", message: "Claude reasoning effort is invalid."}
		}
		if capability.Features["effort"] {
			arguments = append(arguments, "--effort", effort)
		} else if capability.Features["thinking"] {
			arguments = append(arguments, "--thinking", effort)
		} else {
			return nil, "", taskRunnerPreparationError{code: "runner_incompatible", message: "Claude reasoning effort is unavailable."}
		}
	}
	sessionID := resumeID
	if resumeID != "" {
		arguments = append(arguments, "--resume", resumeID)
	} else if capability.Features["sessionId"] {
		sessionID = uuid.NewString()
		arguments = append(arguments, "--session-id", sessionID)
	}
	if capability.Features["streamJson"] {
		arguments = append(arguments, "--verbose", "--output-format", "stream-json")
	}
	return arguments, sessionID, nil
}

func prepareKimiArguments(config map[string]any, capability taskRunnerCapability, resumeID string) ([]string, bool, error) {
	auto := true
	if _, found := config["autoMode"]; found {
		auto = taskConfigBool(config, "autoMode")
	}
	arguments := []string{}
	if resumeID != "" {
		arguments = append(arguments, "--session", resumeID)
	}
	if auto {
		if !capability.Features["auto"] || !capability.Features["print"] {
			return nil, false, taskRunnerPreparationError{code: "runner_incompatible", message: "Kimi unattended stdin mode is unavailable."}
		}
		arguments = append(arguments, "--print")
		if capability.Features["streamJson"] {
			arguments = append(arguments, "--output-format", "stream-json")
		}
	}
	return arguments, true, nil
}

func preparePiArguments(capability taskRunnerCapability) ([]string, error) {
	if !capability.Features["print"] {
		return nil, taskRunnerPreparationError{code: "runner_incompatible", message: "Pi print mode is unavailable."}
	}
	arguments := []string{"--print"}
	if capability.Features["approve"] {
		arguments = append(arguments, "--approve")
	}
	if capability.Features["noSession"] {
		arguments = append(arguments, "--no-session")
	}
	return arguments, nil
}

func mergeClaudeTaskEnvironment(environment []string, config map[string]any, prior error) ([]string, error) {
	if prior != nil {
		return nil, prior
	}
	if value := taskConfigString(config, "apiBaseUrl"); value != "" {
		environment = append(environment, "ANTHROPIC_BASE_URL="+value)
	}
	if value := taskConfigString(config, "apiKey"); value != "" {
		environment = append(environment, "ANTHROPIC_API_KEY="+value)
	}
	return environment, nil
}

func decodeTaskRunnerConfig(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var config map[string]any
	if err := decoder.Decode(&config); err != nil || config == nil {
		return nil, taskRunnerPreparationError{code: "config_invalid", message: "Task runner configuration is invalid."}
	}
	return config, nil
}

func taskConfigString(config map[string]any, key string) string {
	value, _ := config[key].(string)
	return strings.TrimSpace(value)
}

func taskConfigBool(config map[string]any, key string) bool {
	value, _ := config[key].(bool)
	return value
}

func taskRunnerEnvironment(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func validTaskCliSessionID(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.IsSpace(character) || character == '"' {
			return false
		}
	}
	return true
}

func safeTaskRunnerVersion(output []byte) string {
	line := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	if len(line) > 256 {
		line = line[:256]
	}
	line = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, line)
	if line == "" {
		return "unknown"
	}
	return line
}

func cloneTaskRunnerFeatures(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneTaskRunnerModels(source []taskRunnerModel) []taskRunnerModel {
	if len(source) == 0 {
		return []taskRunnerModel{}
	}
	result := make([]taskRunnerModel, len(source))
	for index, model := range source {
		result[index] = model
		result[index].SupportedReasoningEfforts = append([]taskRunnerReasoningEffort(nil), model.SupportedReasoningEfforts...)
	}
	return result
}

func taskRunnerModelIDs(models []taskRunnerModel) []string {
	result := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		identifier := strings.TrimSpace(model.Model)
		if identifier == "" {
			continue
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		result = append(result, identifier)
	}
	sort.Strings(result)
	return result
}

func runTaskRunnerProbe(ctx context.Context, executable string, arguments []string) ([]byte, error) {
	return runTaskRunnerProbeWithEnvironment(ctx, executable, arguments, nil)
}

func runTaskRunnerProbeWithEnvironment(ctx context.Context, executable string, arguments, environment []string) ([]byte, error) {
	if ctx == nil || !filepath.IsAbs(executable) {
		return nil, errors.New("task runner probe is invalid")
	}
	root := os.TempDir()
	supervisor := newRawProcessSupervisor()
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: append([]string{executable}, arguments...), Environment: environment,
		Limits: processResourceLimits{
			MaximumLifetime: taskRunnerProbeCommandTimeout, MaximumMemoryBytes: 256 << 20,
			MaximumOutputBytes: 2 * maximumTaskProbeBytes,
		},
	})
	if err != nil {
		return nil, err
	}
	defer process.release()
	stdout, stderr := newTaskProbeBuffer(maximumTaskProbeBytes), newTaskProbeBuffer(maximumTaskProbeBytes)
	reads := make(chan error, 2)
	go func() { _, readErr := io.Copy(stdout, process.Stdout()); reads <- readErr }()
	go func() { _, readErr := io.Copy(stderr, process.Stderr()); reads <- readErr }()
	done := make(chan int, 1)
	go func() { done <- process.Wait() }()
	var exitCode int
	select {
	case exitCode = <-done:
	case <-ctx.Done():
		_ = process.Close("client_close")
		<-done
		return nil, ctx.Err()
	}
	_ = process.Close("process_exit")
	readErr := errors.Join(<-reads, <-reads)
	output := append(stdout.Bytes(), stderr.Bytes()...)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return output, readErr
	}
	if process.reason() == "lifetime_limit" {
		return output, context.DeadlineExceeded
	}
	if exitCode != 0 {
		return output, fmt.Errorf("task runner probe exited with code %d", exitCode)
	}
	return output, nil
}

// discoverCodexModels follows WenzMark's app-server model/list handshake. The
// process is supervised like every other runner probe, and only bounded model
// metadata crosses the capability boundary; the Codex account and credentials
// remain inside the device-side CLI process.
func discoverCodexModels(ctx context.Context, executable string) ([]taskRunnerModel, error) {
	return discoverCodexModelsWithEnvironment(ctx, executable, nil)
}

func discoverCodexModelsWithEnvironment(ctx context.Context, executable string, environment []string) ([]taskRunnerModel, error) {
	if ctx == nil || !filepath.IsAbs(executable) {
		return nil, errors.New("Codex model discovery is invalid")
	}
	root := os.TempDir()
	supervisor := newRawProcessSupervisor()
	process, err := supervisor.Start(rawProcessLaunchSpec{
		ProjectID: uuid.New(), ProjectRoot: root, WorkingDirectory: root,
		Argv: []string{executable, "app-server", "--stdio"}, Environment: environment, InteractiveStdin: true,
		Limits: processResourceLimits{
			MaximumLifetime: taskRunnerProbeSequenceTimeout, MaximumMemoryBytes: 256 << 20,
			MaximumOutputBytes: 2 * maximumTaskProbeBytes,
		},
	})
	if err != nil {
		return nil, err
	}
	defer process.release()
	stdin := process.Stdin()
	if stdin == nil {
		return nil, errors.New("Codex model discovery stdin is unavailable")
	}
	stdout := bufio.NewReaderSize(process.Stdout(), 64<<10)
	// Drain stderr concurrently so a verbose CLI cannot block the protocol
	// stream while diagnostics fill the OS pipe.
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, process.Stderr())
		close(stderrDone)
	}()
	cancelDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = process.Close("client_close")
		case <-cancelDone:
		}
	}()
	defer close(cancelDone)
	defer func() {
		_ = stdin.Close()
		_ = process.Close("process_exit")
		_ = process.Wait()
		<-stderrDone
	}()

	writeMessage := func(message map[string]any) error {
		encoded, marshalErr := json.Marshal(message)
		if marshalErr != nil {
			return marshalErr
		}
		encoded = append(encoded, '\n')
		written, writeErr := stdin.Write(encoded)
		if writeErr != nil {
			return writeErr
		}
		if written != len(encoded) {
			return io.ErrShortWrite
		}
		return nil
	}

	readResponse := func(expectedID int64) (map[string]any, error) {
		for {
			line, readErr := stdout.ReadBytes('\n')
			if len(line) > maximumTaskProbeBytes {
				return nil, errors.New("Codex app-server response is too large")
			}
			if len(line) > 0 {
				var decoded map[string]any
				if json.Unmarshal(bytes.TrimSpace(line), &decoded) == nil {
					if id, ok := decoded["id"]; ok && fmt.Sprint(id) == fmt.Sprintf("%d", expectedID) {
						if responseErr := decoded["error"]; responseErr != nil {
							return nil, fmt.Errorf("Codex app-server request failed: %v", responseErr)
						}
						result, ok := decoded["result"].(map[string]any)
						if !ok {
							return nil, errors.New("Codex app-server response has no result object")
						}
						return result, nil
					}
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					return nil, errors.New("Codex app-server closed before responding")
				}
				return nil, readErr
			}
		}
	}

	if err := writeMessage(map[string]any{
		"method": "initialize", "id": int64(1),
		"params": map[string]any{"clientInfo": map[string]any{
			"name": "wenzwork", "title": "WenzWork", "version": "1",
		}},
	}); err != nil {
		return nil, err
	}
	if _, err := readResponse(1); err != nil {
		return nil, err
	}
	if err := writeMessage(map[string]any{
		"method": "initialized", "params": map[string]any{},
	}); err != nil {
		return nil, err
	}

	models := make([]taskRunnerModel, 0)
	seen := make(map[string]struct{})
	var cursor string
	for requestID, page := int64(2), 0; page < 32; requestID, page = requestID+1, page+1 {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := writeMessage(map[string]any{"method": "model/list", "id": requestID, "params": params}); err != nil {
			return nil, err
		}
		result, err := readResponse(requestID)
		if err != nil {
			return nil, err
		}
		pageModels, err := parseCodexModelListResult(result)
		if err != nil {
			return nil, err
		}
		for _, model := range pageModels {
			if _, exists := seen[model.Model]; exists {
				continue
			}
			seen[model.Model] = struct{}{}
			models = append(models, model)
		}
		next, _ := result["nextCursor"].(string)
		cursor = boundedTaskRunnerText(next, 256)
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		return nil, errors.New("Codex app-server returned too many model pages")
	}
	return models, nil
}

func parseCodexModelListResult(result map[string]any) ([]taskRunnerModel, error) {
	entries, ok := result["data"].([]any)
	if !ok {
		return nil, errors.New("Codex model/list response has no data array")
	}
	models := make([]taskRunnerModel, 0, len(entries))
	for _, entry := range entries {
		value, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		model := boundedTaskRunnerText(value["model"], 256)
		id := boundedTaskRunnerText(value["id"], 256)
		if model == "" {
			model = id
		}
		if id == "" {
			id = model
		}
		if model == "" || id == "" || value["hidden"] == true {
			continue
		}
		displayName := boundedTaskRunnerText(value["displayName"], 256)
		if displayName == "" {
			displayName = model
		}
		entryModel := taskRunnerModel{
			ID: id, Model: model, DisplayName: displayName,
			Description:            boundedTaskRunnerText(value["description"], 1024),
			DefaultReasoningEffort: boundedTaskRunnerText(value["defaultReasoningEffort"], 32),
			Hidden:                 false, IsDefault: value["isDefault"] == true,
		}
		if efforts, ok := value["supportedReasoningEfforts"].([]any); ok {
			seenEfforts := make(map[string]struct{}, len(efforts))
			for _, rawEffort := range efforts {
				effortMap, ok := rawEffort.(map[string]any)
				if !ok {
					continue
				}
				effort := boundedTaskRunnerText(effortMap["reasoningEffort"], 32)
				if effort == "" {
					continue
				}
				if _, exists := seenEfforts[effort]; exists {
					continue
				}
				seenEfforts[effort] = struct{}{}
				entryModel.SupportedReasoningEfforts = append(entryModel.SupportedReasoningEfforts, taskRunnerReasoningEffort{
					ReasoningEffort: effort,
					Description:     boundedTaskRunnerText(effortMap["description"], 512),
				})
			}
		}
		models = append(models, entryModel)
	}
	return models, nil
}

func boundedTaskRunnerText(value any, maximum int) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > maximum || strings.ContainsAny(text, "\x00\r\n") {
		return ""
	}
	return text
}

type taskProbeBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
}

func newTaskProbeBuffer(limit int) *taskProbeBuffer { return &taskProbeBuffer{remaining: limit} }

func (buffer *taskProbeBuffer) Write(contents []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	original := len(contents)
	if len(contents) > buffer.remaining {
		contents = contents[:buffer.remaining]
	}
	if len(contents) > 0 {
		_, _ = buffer.buffer.Write(contents)
		buffer.remaining -= len(contents)
	}
	return original, nil
}

func (buffer *taskProbeBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return append([]byte(nil), buffer.buffer.Bytes()...)
}

func (registry *taskRunnerRegistry) discoverJcodeModels(ctx context.Context, executable string) ([]string, error) {
	output, err := registry.runProbe(ctx, executable, []string{"--quiet", "--no-update", "--no-selfdev", "model", "list", "--json"})
	if err != nil {
		return nil, err
	}
	return parseTaskRunnerModelIDs(output)
}

func parseTaskRunnerModelIDs(raw []byte) ([]string, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	var entries []any
	switch value := decoded.(type) {
	case []any:
		entries = value
	case map[string]any:
		for _, key := range []string{"models", "items", "data"} {
			if items, ok := value[key].([]any); ok {
				entries = items
				break
			}
		}
	}
	models := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var identifier string
		switch value := entry.(type) {
		case string:
			identifier = strings.TrimSpace(value)
		case map[string]any:
			for _, key := range []string{"id", "model", "name"} {
				if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
					identifier = strings.TrimSpace(text)
					break
				}
			}
		}
		if identifier == "" || len(identifier) > 256 || strings.ContainsAny(identifier, "\x00\r\n") {
			continue
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		models = append(models, identifier)
	}
	sort.Strings(models)
	return models, nil
}
