package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestTaskRunnerRegistryProbesKindsIndependently(t *testing.T) {
	registry := newTaskRunnerRegistry()
	root := t.TempDir()
	registry.resolve = func(candidate string) (string, error) {
		if candidate == "hermes" {
			return "", errors.New("missing")
		}
		return filepath.Join(root, candidate+".exe"), nil
	}
	registry.runProbe = func(_ context.Context, _ string, arguments []string) ([]byte, error) {
		if slices.Contains(arguments, "model") {
			return []byte(`{"models":[{"id":"model-b"},{"id":"model-a"}]}`), nil
		}
		if slices.Contains(arguments, "--version") {
			return []byte("runner 1.2.3\n"), nil
		}
		return []byte(strings.Join([]string{
			"run resume one-shot afk", "--json --goal --ask-for-approval --sandbox",
			"--print --force --trust --output-format stream-json --stream-partial-output --resume",
			"-z --yolo --model --provider --usage-file --ndjson --quiet --no-update --no-selfdev",
			"--tool-profile --provider-profile --auto --format --session --prompt",
			"--dangerously-skip-permissions --allow-dangerously-skip-permissions --permission-mode",
			"--effort --thinking --session-id --approve --no-session",
		}, " ")), nil
	}

	registry.Refresh(t.Context())
	capabilities := registry.Capabilities()
	if len(capabilities) != len(taskRunnerKinds) {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	byKind := make(map[string]taskRunnerCapability, len(capabilities))
	for _, capability := range capabilities {
		byKind[capability.Kind] = capability
		if capability.executable != "" {
			t.Fatalf("capability leaked executable path: %#v", capability)
		}
	}
	for _, kind := range taskRunnerKinds {
		capability := byKind[kind]
		if kind == "hermes" {
			if capability.Available || capability.ErrorCode != "runner_not_installed" {
				t.Fatalf("Hermes capability = %#v", capability)
			}
			continue
		}
		if !capability.Available || capability.ProbeStatus != "ready" {
			t.Fatalf("%s capability = %#v", kind, capability)
		}
	}
	if got := byKind["jcode"].Models; !slices.Equal(got, []string{"model-a", "model-b"}) || !byKind["jcode"].ModelDiscovery {
		t.Fatalf("JCode models = %#v", byKind["jcode"])
	}
}

func TestTaskRunnerRegistryRecognizesCurrentCodexCLI(t *testing.T) {
	registry := newTaskRunnerRegistry()
	root := t.TempDir()
	registry.resolve = func(candidate string) (string, error) {
		if candidate != "codex" {
			return "", errors.New("missing")
		}
		return filepath.Join(root, "codex.exe"), nil
	}
	registry.runProbe = func(_ context.Context, _ string, arguments []string) ([]byte, error) {
		switch strings.Join(arguments, " ") {
		case "--version":
			return []byte("codex-cli 1.0\n"), nil
		case "exec --help":
			// Current Codex lists exec options here, but deliberately keeps
			// --ask-for-approval in the top-level command help.
			return []byte("--json --sandbox\n"), nil
		case "--help":
			return []byte("--ask-for-approval <APPROVAL_POLICY>\n"), nil
		case "features list":
			return []byte("goals                                stable             true\n"), nil
		default:
			return nil, errors.New("unexpected probe")
		}
	}

	registry.Refresh(t.Context())
	var codex taskRunnerCapability
	for _, capability := range registry.Capabilities() {
		if capability.Kind == "codex" {
			codex = capability
			break
		}
	}
	if !codex.Available || !codex.Features["approval"] || !codex.Features["sandbox"] ||
		!codex.Features["json"] || !codex.Features["goals"] {
		t.Fatalf("Codex capability = %#v", codex)
	}
}

func TestTaskRunnerRegistryPublishesCodexModelDetails(t *testing.T) {
	registry := newTaskRunnerRegistry()
	root := t.TempDir()
	registry.resolve = func(candidate string) (string, error) {
		if candidate != "codex" {
			return "", errors.New("missing")
		}
		return filepath.Join(root, "codex.exe"), nil
	}
	registry.runProbe = func(_ context.Context, _ string, arguments []string) ([]byte, error) {
		switch strings.Join(arguments, " ") {
		case "--version":
			return []byte("codex-cli 1.0\n"), nil
		case "exec --help":
			return []byte("--json --sandbox\n"), nil
		case "--help":
			return []byte("--ask-for-approval\n"), nil
		case "features list":
			return []byte("goals stable true\n"), nil
		default:
			return nil, errors.New("unexpected probe")
		}
	}
	registry.discoverModels = func(context.Context, string) ([]taskRunnerModel, error) {
		return []taskRunnerModel{{
			ID: "gpt-5.6", Model: "gpt-5.6", DisplayName: "GPT-5.6", IsDefault: true,
			DefaultReasoningEffort:    "high",
			SupportedReasoningEfforts: []taskRunnerReasoningEffort{{ReasoningEffort: "medium"}, {ReasoningEffort: "high"}},
		}}, nil
	}

	registry.Refresh(t.Context())
	var codex taskRunnerCapability
	for _, capability := range registry.Capabilities() {
		if capability.Kind == "codex" {
			codex = capability
			break
		}
	}
	if !codex.ModelDiscovery || !slices.Equal(codex.Models, []string{"gpt-5.6"}) || len(codex.ModelDetails) != 1 {
		t.Fatalf("Codex model details = %#v", codex)
	}
	if got := codex.ModelDetails[0].SupportedReasoningEfforts[1].ReasoningEffort; got != "high" {
		t.Fatalf("Codex reasoning efforts = %#v", codex.ModelDetails[0].SupportedReasoningEfforts)
	}
}

func TestParseCodexModelListResultBoundsAndDeduplicatesEfforts(t *testing.T) {
	models, err := parseCodexModelListResult(map[string]any{
		"data": []any{
			map[string]any{
				"id": "gpt-5.6", "model": "gpt-5.6", "displayName": "GPT-5.6",
				"description": "safe description", "isDefault": true,
				"supportedReasoningEfforts": []any{
					map[string]any{"reasoningEffort": "high", "description": "deep"},
					map[string]any{"reasoningEffort": "high"},
					map[string]any{"reasoningEffort": ""},
				},
			},
			map[string]any{"model": "hidden", "hidden": true},
			map[string]any{"id": "gpt-5.6"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Model != "gpt-5.6" || models[1].Model != "gpt-5.6" {
		// The parser deliberately keeps duplicate entries here; the discovery
		// layer de-duplicates across pages while preserving server ordering.
		t.Fatalf("parsed Codex models = %#v", models)
	}
	if got := len(models[0].SupportedReasoningEfforts); got != 1 {
		t.Fatalf("parsed reasoning efforts = %#v", models[0].SupportedReasoningEfforts)
	}
}

func TestTaskRunnerRegistryRetriesStartupMissAndAdaptsCodexGoalsFeature(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	root := t.TempDir()
	executable := filepath.Join(root, "codex.exe")
	registry := &taskRunnerRegistry{
		capabilities: map[string]taskRunnerCapability{
			"codex": {
				Kind: "codex", Available: false, ProbeStatus: "ready", Models: []string{},
				Features: map[string]bool{}, ErrorCode: "runner_not_installed",
			},
		},
		probeLocks: map[string]*sync.Mutex{"codex": new(sync.Mutex)},
		resolve: func(candidate string) (string, error) {
			if candidate != "codex" {
				return "", errors.New("unexpected runner")
			}
			return executable, nil
		},
		runProbe: func(_ context.Context, _ string, arguments []string) ([]byte, error) {
			switch strings.Join(arguments, " ") {
			case "--version":
				return []byte("codex-cli 0.147.0\n"), nil
			case "exec --help":
				return []byte("resume --json --sandbox\n"), nil
			case "--help":
				return []byte("--ask-for-approval\n"), nil
			case "features list":
				return []byte("goals stable true\n"), nil
			default:
				return nil, errors.New("unexpected probe")
			}
		},
	}
	config, err := json.Marshal(map[string]any{
		"promptSource": "customText", "promptText": "finish the work", "attachedFilePaths": []string{},
		"launchMode": "cli", "goalMode": true, "reasoningEffort": "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := taskV2Record{Definition: taskV2Definition{
		ID: uuid.New(), ProjectID: fixture.project.ID, Kind: "codex", CWD: ".", Config: config,
	}}
	invocation, err := registry.Prepare(t.Context(), fixture.project, task, filepath.Join(root, "private.prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Executable != executable || invocation.PromptPrefix != taskV2CodexGoalPromptPrefix {
		t.Fatalf("Goal-compatible invocation = %#v", invocation)
	}
	joined := strings.Join(invocation.Arguments, " ")
	if !strings.Contains(joined, "--enable goals") || strings.Contains(joined, "--goal") {
		t.Fatalf("Goal-compatible Codex arguments = %q", joined)
	}
	capability := registry.capabilities["codex"]
	if !capability.Available || !capability.Features["goals"] || capability.Features["goal"] {
		t.Fatalf("refreshed Codex capability = %#v", capability)
	}
}

func TestTaskRunnerRegistryReportsUnavailableExecutionContext(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	registry := &taskRunnerRegistry{
		capabilities: map[string]taskRunnerCapability{
			"codex": {
				Kind: "codex", Available: false, ProbeStatus: "ready", Models: []string{},
				Features: map[string]bool{}, ErrorCode: "runner_not_installed",
			},
		},
		probeLocks: map[string]*sync.Mutex{"codex": new(sync.Mutex)},
		resolve: func(string) (string, error) {
			return "", fmt.Errorf("cannot inspect desktop session: %w", errTaskExecutionContextUnavailable)
		},
		runProbe: runTaskRunnerProbe,
	}
	config, err := json.Marshal(map[string]any{
		"promptSource": "customText", "promptText": "finish the work", "attachedFilePaths": []string{},
		"launchMode": "cli", "goalMode": true, "reasoningEffort": "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	task := taskV2Record{Definition: taskV2Definition{
		ID: uuid.New(), ProjectID: fixture.project.ID, Kind: "codex", CWD: ".", Config: config,
	}}
	_, err = registry.Prepare(t.Context(), fixture.project, task, filepath.Join(t.TempDir(), "private.prompt.md"))
	var preparation taskRunnerPreparationError
	if !errors.As(err, &preparation) || preparation.code != "execution_context_unavailable" {
		t.Fatalf("execution-context preparation error = %#v", err)
	}
}

func TestPrepareCodexResumeDoesNotRequireGoalCapability(t *testing.T) {
	arguments, err := prepareCodexArguments(
		map[string]any{"goalMode": true, "reasoningEffort": "medium"},
		taskRunnerCapability{Features: map[string]bool{"json": true}},
		"019c-resume-session",
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	if strings.Contains(joined, "--goal") || strings.Contains(joined, "--enable goals") ||
		!strings.Contains(joined, "exec resume") {
		t.Fatalf("resume arguments = %q", joined)
	}
}

func TestPrepareCodexArgumentsUsesBatchSafeTOMLString(t *testing.T) {
	arguments, err := prepareCodexArguments(
		map[string]any{"reasoningEffort": "medium"},
		taskRunnerCapability{Features: map[string]bool{"json": true}},
		"",
		t.TempDir(),
	)
	if err != nil {
		t.Fatal(err)
	}
	configIndex := slices.Index(arguments, "-c")
	if configIndex < 0 || configIndex+1 >= len(arguments) {
		t.Fatalf("Codex arguments do not contain a config value: %#v", arguments)
	}
	if got, want := arguments[configIndex+1], "model_reasoning_effort='medium'"; got != want {
		t.Fatalf("Codex reasoning config = %q, want %q", got, want)
	}
}

func TestRealCodexRunnerProbe(t *testing.T) {
	if os.Getenv("WENZWORK_REAL_CODEX_TEST") != "1" {
		t.Skip("set WENZWORK_REAL_CODEX_TEST=1 to probe the installed Codex CLI")
	}
	registry := newTaskRunnerRegistry()
	capability, err := registry.probe(t.Context(), "codex", true)
	if err != nil {
		t.Fatal(err)
	}
	if !capability.Available || capability.Version == "" ||
		(!capability.Features["goal"] && !capability.Features["goals"]) {
		t.Fatalf("installed Codex capability = %#v", capability)
	}
}

func TestTaskRunnerAdaptersBuildUnattendedPrivateInvocations(t *testing.T) {
	fixture := newTaskV2StoreFixture(t)
	root := t.TempDir()
	registry := &taskRunnerRegistry{
		capabilities: make(map[string]taskRunnerCapability),
		probeLocks:   make(map[string]*sync.Mutex),
		resolve:      resolveSupervisedExecutable,
		runProbe:     runTaskRunnerProbe,
	}
	features := map[string]bool{
		"json": true, "goal": true, "approval": true, "sandbox": true,
		"print": true, "force": true, "trust": true, "output-format": true, "partialOutput": true,
		"oneShot": true, "yolo": true, "model": true, "provider": true, "usageFile": true,
		"run": true, "ndjson": true, "quiet": true, "no-update": true, "no-selfdev": true,
		"tool-profile": true, "provider-profile": true, "auto": true,
		"dangerous": true, "modernDangerous": true, "streamJson": true, "effort": true,
		"thinking": true, "sessionId": true, "prompt": true, "approve": true, "noSession": true,
	}
	for _, kind := range taskRunnerKinds {
		registry.probeLocks[kind] = new(sync.Mutex)
		registry.capabilities[kind] = taskRunnerCapability{
			Kind: kind, Available: true, ProbeStatus: "ready", Models: []string{}, SupportsResume: kind != "pi",
			Features: cloneTaskRunnerFeatures(features), executable: filepath.Join(root, kind+".exe"),
		}
	}
	promptPath := filepath.Join(root, "private prompt.md")
	baseConfig := map[string]any{
		"promptSource": "customText", "promptText": "private body", "attachedFilePaths": []string{},
	}
	tests := []struct {
		kind       string
		config     map[string]any
		want       []string
		stdin      bool
		promptArg  bool
		wantEnvKey string
		parseJSON  bool
	}{
		{kind: "codex", config: mergeTaskRunnerTestConfig(baseConfig, map[string]any{"model": "gpt-test", "goalMode": true, "reasoningEffort": "high"}), want: []string{"--ask-for-approval", "never", "exec", "--goal", "-"}, stdin: true, parseJSON: true},
		{kind: "cursor", config: mergeTaskRunnerTestConfig(baseConfig, map[string]any{"model": "cursor-test", "sandboxMode": "enabled"}), want: []string{"--print", "--force", "--trust", "stream-json"}, stdin: true},
		{kind: "hermes", config: mergeTaskRunnerTestConfig(baseConfig, map[string]any{"model": "hermes-test", "provider": "local"}), want: []string{"--yolo", "-z"}, promptArg: true},
		{kind: "jcode", config: mergeTaskRunnerTestConfig(baseConfig, map[string]any{"model": "jcode-test", "toolProfile": "full"}), want: []string{"--quiet", "run", "--ndjson"}, promptArg: true, wantEnvKey: "NO_COLOR=1"},
		{kind: "opencode", config: mergeTaskRunnerTestConfig(baseConfig, map[string]any{"autoMode": true}), want: []string{"run", "--auto", "json"}, stdin: true},
		{kind: "claude", config: mergeTaskRunnerTestConfig(baseConfig, map[string]any{"model": "claude-test", "reasoningEffort": "high", "apiBaseUrl": "https://example.invalid", "apiKey": "device-only-key"}), want: []string{"--print", "bypassPermissions", "--session-id", "stream-json"}, stdin: true, wantEnvKey: "ANTHROPIC_API_KEY=device-only-key"},
		{kind: "kimi", config: mergeTaskRunnerTestConfig(baseConfig, map[string]any{"autoMode": true}), want: []string{"--print", "stream-json"}, stdin: true},
		{kind: "pi", config: mergeTaskRunnerTestConfig(baseConfig, nil), want: []string{"--print", "--approve", "--no-session"}, stdin: true},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			encoded, err := json.Marshal(test.config)
			if err != nil {
				t.Fatal(err)
			}
			task := taskV2Record{Definition: taskV2Definition{
				ID: uuid.New(), ProjectID: fixture.project.ID, Kind: test.kind, CWD: ".", Config: encoded,
				Environment: map[string]string{"TASK_LABEL": "runner-test"},
			}}
			invocation, err := registry.Prepare(t.Context(), fixture.project, task, promptPath)
			if err != nil {
				t.Fatal(err)
			}
			if invocation.Executable == "" || invocation.UseStdinFile != test.stdin {
				t.Fatalf("invocation = %#v", invocation)
			}
			if invocation.ParseCodexJSON != test.parseJSON {
				t.Fatalf("ParseCodexJSON = %v, want %v", invocation.ParseCodexJSON, test.parseJSON)
			}
			joined := strings.Join(invocation.Arguments, " ")
			for _, expected := range test.want {
				if !strings.Contains(joined, expected) {
					t.Fatalf("arguments %q do not contain %q", joined, expected)
				}
			}
			if strings.Contains(joined, "private body") || strings.Contains(joined, "device-only-key") {
				t.Fatalf("private task content leaked into argv: %q", joined)
			}
			if strings.Contains(joined, promptPath) != test.promptArg {
				t.Fatalf("prompt path presence in %q = %v, want %v", joined, strings.Contains(joined, promptPath), test.promptArg)
			}
			if test.wantEnvKey != "" && !slices.Contains(invocation.Environment, test.wantEnvKey) {
				t.Fatalf("environment = %#v", invocation.Environment)
			}
			if test.kind == "claude" && uuid.Validate(invocation.CliSessionID) != nil {
				t.Fatalf("Claude session ID = %q", invocation.CliSessionID)
			}
		})
	}
	t.Run("script", func(t *testing.T) {
		encoded := json.RawMessage(`{"command":"echo script-private","cwdChoice":"workspace"}`)
		task := taskV2Record{Definition: taskV2Definition{
			ID: uuid.New(), ProjectID: fixture.project.ID, Kind: "script", CWD: ".", Config: encoded,
		}}
		invocation, err := registry.Prepare(t.Context(), fixture.project, task, "")
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(invocation.Executable) || !invocation.UseStdinFile || strings.Contains(strings.Join(invocation.Arguments, " "), "script-private") {
			t.Fatalf("script invocation = %#v", invocation)
		}
	})
}

func mergeTaskRunnerTestConfig(base, additions map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(additions))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range additions {
		result[key] = value
	}
	return result
}
