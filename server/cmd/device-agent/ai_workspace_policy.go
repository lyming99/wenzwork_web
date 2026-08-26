package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumAIWorkspacePolicyEntries     = 64
	maximumAIWorkspacePolicyEntryBytes  = 128
	maximumAIWorkspacePolicyFileBytes   = 16 << 10
)

// aiWorkspaceProjectPolicy is the project-level override for the built-in
// ignore and sensitive rules, loaded from .wenzwork/ai-tool-policy.json.
type aiWorkspaceProjectPolicy struct {
	IgnoredDirectories []string `json:"ignoredDirectories"`
	SensitiveNames     []string `json:"sensitiveNames"`
}

func loadAIWorkspaceProjectPolicy(path string) (*aiWorkspaceProjectPolicy, error) {
	data, err := readBoundedFile(path, maximumAIWorkspacePolicyFileBytes)
	if err != nil || len(data) == 0 {
		return nil, errRPCInvalid
	}
	var policy aiWorkspaceProjectPolicy
	if json.Unmarshal(data, &policy) != nil || !validAIWorkspaceProjectPolicy(policy) {
		return nil, errRPCInvalid
	}
	normalized := aiWorkspaceProjectPolicy{
		IgnoredDirectories: make([]string, 0, len(policy.IgnoredDirectories)),
		SensitiveNames:     make([]string, 0, len(policy.SensitiveNames)),
	}
	for _, entry := range policy.IgnoredDirectories {
		normalized.IgnoredDirectories = append(normalized.IgnoredDirectories, strings.ToLower(strings.TrimSpace(entry)))
	}
	for _, entry := range policy.SensitiveNames {
		normalized.SensitiveNames = append(normalized.SensitiveNames, strings.ToLower(strings.TrimSpace(entry)))
	}
	return &normalized, nil
}

func validAIWorkspaceProjectPolicy(policy aiWorkspaceProjectPolicy) bool {
	if len(policy.IgnoredDirectories) > maximumAIWorkspacePolicyEntries || len(policy.SensitiveNames) > maximumAIWorkspacePolicyEntries {
		return false
	}
	for _, entry := range policy.IgnoredDirectories {
		entry = strings.TrimSpace(entry)
		if entry == "" || len(entry) > maximumAIWorkspacePolicyEntryBytes || !utf8.ValidString(entry) ||
			strings.ContainsAny(entry, `/\`) {
			return false
		}
	}
	for _, entry := range policy.SensitiveNames {
		entry = strings.TrimSpace(entry)
		if entry == "" || len(entry) > maximumAIWorkspacePolicyEntryBytes || !utf8.ValidString(entry) {
			return false
		}
		if _, err := filepath.Match(entry, "candidate"); err != nil {
			return false
		}
	}
	return true
}

type aiWorkspacePolicyEntry struct {
	modTime time.Time
	size    int64
	policy  *aiWorkspaceProjectPolicy
}

// projectPolicy resolves project-level overrides with a stat-validated cache.
// A missing or invalid file fails closed to the built-in defaults and never
// widens the policy surface.
func (executor *aiWorkspaceToolExecutor) projectPolicy(projectID uuid.UUID, root string) *aiWorkspaceProjectPolicy {
	if executor == nil || projectID == uuid.Nil || root == "" {
		return nil
	}
	path := filepath.Join(root, ".wenzwork", "ai-tool-policy.json")
	info, err := os.Stat(path)
	if err != nil {
		executor.policyCache.Store(projectID, aiWorkspacePolicyEntry{})
		return nil
	}
	if value, found := executor.policyCache.Load(projectID); found {
		entry := value.(aiWorkspacePolicyEntry)
		if entry.modTime.Equal(info.ModTime()) && entry.size == info.Size() {
			return entry.policy
		}
	}
	policy, err := loadAIWorkspaceProjectPolicy(path)
	if err != nil {
		executor.policyCache.Store(projectID, aiWorkspacePolicyEntry{})
		return nil
	}
	executor.policyCache.Store(projectID, aiWorkspacePolicyEntry{modTime: info.ModTime(), size: info.Size(), policy: policy})
	return policy
}

func (executor *aiWorkspaceToolExecutor) ignoredDirectory(project registeredProject, name string) bool {
	if aiWorkspaceIgnoredDirectory(name) {
		return true
	}
	policy := executor.projectPolicy(project.ID, project.LocalPath)
	if policy == nil {
		return false
	}
	return slices.Contains(policy.IgnoredDirectories, strings.ToLower(name))
}

func (executor *aiWorkspaceToolExecutor) sensitiveName(project registeredProject, name string) bool {
	if aiWorkspaceSensitiveName(name) {
		return true
	}
	policy := executor.projectPolicy(project.ID, project.LocalPath)
	if policy == nil {
		return false
	}
	lower := strings.ToLower(name)
	for _, entry := range policy.SensitiveNames {
		if entry == lower {
			return true
		}
		if matched, err := filepath.Match(entry, lower); err == nil && matched {
			return true
		}
	}
	return false
}

// aiWorkspaceHostTokenPattern extracts hostname-like tokens from command
// text. It is a containment heuristic used only when the model explicitly
// declared a network_hosts whitelist.
var aiWorkspaceHostTokenPattern = regexp.MustCompile(`[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+`)

// aiWorkspaceCommandHosts returns the hostname-like tokens in a command.
// Tokens whose final label is purely numeric (version-looking strings) are
// dropped, and tokens that are path/file continuations are skipped while URL
// hosts ("://host") and user hosts ("user@host") are kept.
func aiWorkspaceCommandHosts(command string) []string {
	lower := strings.ToLower(command)
	seen := make(map[string]struct{})
	hosts := make([]string, 0, 4)
	for _, span := range aiWorkspaceHostTokenPattern.FindAllStringIndex(lower, -1) {
		start, end := span[0], span[1]
		token := lower[start:end]
		labels := strings.Split(token, ".")
		if !strings.ContainsAny(labels[len(labels)-1], "abcdefghijklmnopqrstuvwxyz") {
			continue
		}
		if start > 0 {
			prefix := lower[start-1]
			urlPrefix := start >= 3 && lower[start-3:start-1] == ":/"
			if prefix == '.' || prefix == '-' || prefix >= 'a' && prefix <= 'z' || prefix >= '0' && prefix <= '9' ||
				prefix == '/' && !urlPrefix {
				continue
			}
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		hosts = append(hosts, token)
	}
	return hosts
}

func aiWorkspaceHostAllowed(host string, whitelist []string) bool {
	for _, entry := range whitelist {
		if strings.ToLower(strings.TrimSpace(entry)) == host {
			return true
		}
	}
	return false
}

// enforceAIWorkspaceNetworkHosts reports whether every hostname-like token in
// the command is covered by the declared whitelist.
func enforceAIWorkspaceNetworkHosts(command string, hosts []string) bool {
	for _, host := range aiWorkspaceCommandHosts(command) {
		if !aiWorkspaceHostAllowed(host, hosts) {
			return false
		}
	}
	return true
}
