package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	agentEnvironmentSecretKey     = "agent-environment:v1"
	agentEnvironmentSecretVersion = 1
)

type agentEnvironmentSecret struct {
	Version   int               `json:"version"`
	Variables map[string]string `json:"variables"`
}

func (state *agentState) loadAgentEnvironment(ctx context.Context) error {
	if state == nil || state.secrets == nil {
		return errors.New("Agent environment SecretStore is unavailable")
	}
	raw, found, err := state.secrets.Get(ctx, agentEnvironmentSecretKey)
	if err != nil {
		return errors.New("read Agent environment from SecretStore")
	}
	if !found {
		state.agentEnvironment = map[string]string{}
		return nil
	}
	defer zeroSecret(raw)
	if len(raw) == 0 || len(raw) > maximumSecretBytes {
		return errors.New("Agent environment SecretStore item is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var stored agentEnvironmentSecret
	if err := decoder.Decode(&stored); err != nil || stored.Version != agentEnvironmentSecretVersion || stored.Variables == nil {
		return errors.New("Agent environment SecretStore item is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("Agent environment SecretStore item contains trailing data")
	}
	variables, encoded, err := normalizeAgentEnvironment(stored.Variables)
	zeroSecret(encoded)
	if err != nil {
		return errors.New("Agent environment SecretStore item is invalid")
	}
	state.agentEnvironment = variables
	return nil
}

func (state *agentState) agentEnvironmentSnapshot() (map[string]string, uint64, uint64) {
	if state == nil {
		return map[string]string{}, 1, 0
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	revision := state.AgentEnvironmentRevision
	if revision == 0 {
		revision = 1
	}
	return cloneAgentEnvironment(state.agentEnvironment), revision, state.Revision
}

// agentEnvironmentList is evaluated for every launch. Existing processes keep
// their original environment, while the next CLI/PTY command sees the latest
// committed device-level configuration without restarting the Agent.
func (state *agentState) agentEnvironmentList() []string {
	variables, _, _ := state.agentEnvironmentSnapshot()
	keys := make([]string, 0, len(variables))
	for key := range variables {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+variables[key])
	}
	return result
}

func (state *agentState) replaceAgentEnvironment(ctx context.Context, variables map[string]string, expectedRevision *uint64) (map[string]any, uint64, error) {
	normalized, encoded, err := normalizeAgentEnvironment(variables)
	if err != nil {
		return nil, 0, err
	}
	defer zeroSecret(encoded)
	if state == nil || state.secrets == nil {
		return nil, 0, errRPCCapability
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	currentRevision := state.AgentEnvironmentRevision
	if currentRevision == 0 {
		currentRevision = 1
	}
	if expectedRevision != nil && *expectedRevision != currentRevision {
		return nil, 0, errRPCRevision
	}
	if equalAgentEnvironment(state.agentEnvironment, normalized) {
		if err := completeV2WithoutSideEffect(ctx); err != nil {
			return nil, 0, err
		}
		return agentEnvironmentView(normalized, currentRevision), state.Revision, nil
	}
	if currentRevision == ^uint64(0) {
		return nil, 0, errors.New("Agent environment revision is exhausted")
	}

	previous := cloneAgentEnvironment(state.agentEnvironment)
	previousRevision := state.AgentEnvironmentRevision
	if err := beginV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	if err := writeAgentEnvironmentSecret(ctx, state.secrets, normalized, encoded); err != nil {
		return nil, 0, err
	}
	state.agentEnvironment = normalized
	state.AgentEnvironmentRevision = currentRevision + 1
	if err := state.persistMutationLocked(); err != nil {
		state.agentEnvironment = previous
		state.AgentEnvironmentRevision = previousRevision
		previousEncoded, encodeErr := encodeAgentEnvironment(previous)
		if encodeErr == nil {
			restoreErr := writeAgentEnvironmentSecret(context.WithoutCancel(ctx), state.secrets, previous, previousEncoded)
			zeroSecret(previousEncoded)
			if restoreErr != nil {
				return nil, 0, errors.Join(err, errors.New("restore Agent environment SecretStore item"))
			}
			if sideEffectErr := rollbackV2SideEffect(ctx); sideEffectErr != nil {
				return nil, 0, errors.Join(err, sideEffectErr)
			}
		}
		return nil, 0, err
	}
	if err := commitV2SideEffect(ctx); err != nil {
		return nil, 0, err
	}
	return agentEnvironmentView(normalized, state.AgentEnvironmentRevision), state.Revision, nil
}

func normalizeAgentEnvironment(variables map[string]string) (map[string]string, []byte, error) {
	if variables == nil || len(variables) > maximumTaskEnvironmentVariables {
		return nil, nil, errRPCInvalid
	}
	normalized := make(map[string]string, len(variables))
	seen := make(map[string]struct{}, len(variables))
	for name, value := range variables {
		upper := strings.ToUpper(name)
		if !taskEnvironmentNamePattern.MatchString(name) || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 ||
			len(value) > 8<<10 || protectedProcessEnvironmentName(upper) {
			return nil, nil, errRPCInvalid
		}
		if _, duplicate := seen[upper]; duplicate {
			return nil, nil, errRPCInvalid
		}
		seen[upper] = struct{}{}
		normalized[name] = value
	}
	encoded, err := encodeAgentEnvironment(normalized)
	if err != nil || len(encoded) > maximumSecretBytes {
		zeroSecret(encoded)
		return nil, nil, errRPCInvalid
	}
	return normalized, encoded, nil
}

func encodeAgentEnvironment(variables map[string]string) ([]byte, error) {
	return json.Marshal(agentEnvironmentSecret{
		Version:   agentEnvironmentSecretVersion,
		Variables: cloneAgentEnvironment(variables),
	})
}

func writeAgentEnvironmentSecret(ctx context.Context, store secretStore, variables map[string]string, encoded []byte) error {
	if len(variables) == 0 {
		if err := store.Delete(ctx, agentEnvironmentSecretKey); err != nil {
			return errors.New("delete Agent environment from SecretStore")
		}
		return nil
	}
	if err := store.Put(ctx, agentEnvironmentSecretKey, encoded); err != nil {
		return errors.New("write Agent environment to SecretStore")
	}
	return nil
}

func agentEnvironmentInput(input rpcInput) (map[string]string, *uint64, error) {
	if len(input) == 0 || len(input) > 2 {
		return nil, nil, errRPCInvalid
	}
	for key := range input {
		if key != "variables" && key != "expectedRevision" {
			return nil, nil, errRPCInvalid
		}
	}
	raw, found := input["variables"]
	object, ok := raw.(map[string]any)
	if !found || !ok {
		return nil, nil, errRPCInvalid
	}
	variables := make(map[string]string, len(object))
	for name, rawValue := range object {
		value, ok := rawValue.(string)
		if !ok {
			return nil, nil, errRPCInvalid
		}
		variables[name] = value
	}
	expected, present, ok := optionalUint64(input, "expectedRevision")
	if !ok || !present || expected == 0 {
		return nil, nil, errRPCInvalid
	}
	return variables, &expected, nil
}

func agentEnvironmentView(variables map[string]string, revision uint64) map[string]any {
	return map[string]any{
		"variables": cloneAgentEnvironment(variables),
		"revision":  revision,
	}
}

func cloneAgentEnvironment(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func equalAgentEnvironment(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, found := right[key]
		if !found || other != value {
			return false
		}
	}
	return true
}

func protectedProcessEnvironmentName(name string) bool {
	return interactiveCommandEnvironmentKey(name)
}
