package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	defaultAITurnOutputTokens       = 16000
	defaultAIActiveContextTokens    = 120000
	defaultAIAgentRounds            = 64
	defaultAIAgentToolCalls         = 100
	defaultAIAgentNoProgressRounds  = 8
	defaultAIRequestTimeoutSeconds  = 300
	defaultAIMaxRetries             = 2
	defaultAIRetryDelayMilliseconds = 350
)

var supportedAIProviders = []string{
	"openai", "anthropic", "google", "deepseek", "ollama", "openai-compatible",
}

func migrateAIConfigSchemaV6(ctx context.Context, tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ai_configs (
            id TEXT PRIMARY KEY,
            device_id TEXT NOT NULL,
            revision INTEGER NOT NULL,
            name TEXT NOT NULL,
            provider TEXT NOT NULL,
            base_url TEXT NOT NULL,
            non_secret_headers_json TEXT NOT NULL DEFAULT '{}',
            model TEXT NOT NULL,
            system_prompt TEXT NOT NULL DEFAULT '',
            temperature REAL NOT NULL DEFAULT 0.7,
            reasoning_effort TEXT NOT NULL DEFAULT 'automatic',
            max_turn_output_tokens INTEGER NOT NULL DEFAULT 16000,
            max_active_context_tokens INTEGER NOT NULL DEFAULT 120000,
            max_agent_rounds INTEGER NOT NULL DEFAULT 64,
            max_agent_tool_calls INTEGER NOT NULL DEFAULT 100,
            max_agent_no_progress_rounds INTEGER NOT NULL DEFAULT 8,
			request_timeout_seconds INTEGER NOT NULL DEFAULT 300,
            max_retries INTEGER NOT NULL DEFAULT 2,
            retry_base_delay_ms INTEGER NOT NULL DEFAULT 350,
            show_usage INTEGER NOT NULL DEFAULT 1,
            enabled INTEGER NOT NULL DEFAULT 1,
            secret_configured INTEGER NOT NULL DEFAULT 0,
            created_at_ms INTEGER NOT NULL,
            updated_at_ms INTEGER NOT NULL,
            CHECK(length(id) BETWEEN 1 AND 80),
            CHECK(revision > 0),
            CHECK(length(name) BETWEEN 1 AND 120),
            CHECK(provider IN ('openai','anthropic','google','deepseek','ollama','openai-compatible')),
            CHECK(length(base_url) <= 2048),
            CHECK(json_valid(non_secret_headers_json) AND json_type(non_secret_headers_json) = 'object' AND length(non_secret_headers_json) <= 16384),
            CHECK(length(model) BETWEEN 1 AND 120),
            CHECK(length(system_prompt) <= 32768),
            CHECK(temperature >= 0 AND temperature <= 2),
            CHECK(reasoning_effort IN ('automatic','none','minimal','low','medium','high','xhigh','max')),
            CHECK(max_turn_output_tokens BETWEEN 1 AND 1000000),
            CHECK(max_active_context_tokens BETWEEN 4096 AND 2000000),
            CHECK(max_agent_rounds BETWEEN 1 AND 200),
            CHECK(max_agent_tool_calls BETWEEN 1 AND 200),
            CHECK(max_agent_no_progress_rounds BETWEEN 1 AND 50),
            CHECK(request_timeout_seconds BETWEEN 1 AND 3600),
            CHECK(max_retries BETWEEN 0 AND 5),
            CHECK(retry_base_delay_ms BETWEEN 0 AND 60000),
            CHECK(show_usage IN (0,1)),
            CHECK(enabled IN (0,1)),
            CHECK(secret_configured IN (0,1))
        )`,
		`CREATE INDEX IF NOT EXISTS ai_configs_device_updated_idx ON ai_configs(device_id, updated_at_ms DESC, id)`,
		`CREATE TABLE IF NOT EXISTS ai_model_discovery (
            config_id TEXT NOT NULL REFERENCES ai_configs(id) ON DELETE CASCADE,
            config_revision INTEGER NOT NULL,
            model_id TEXT NOT NULL,
            display_name TEXT NOT NULL,
            capabilities_json TEXT NOT NULL DEFAULT '{}',
            max_input_tokens INTEGER,
            max_output_tokens INTEGER,
            discovered_at_ms INTEGER NOT NULL,
            PRIMARY KEY(config_id, model_id),
            CHECK(length(model_id) BETWEEN 1 AND 240),
            CHECK(length(display_name) BETWEEN 1 AND 240),
            CHECK(json_valid(capabilities_json) AND json_type(capabilities_json) = 'object'),
            CHECK(max_input_tokens IS NULL OR max_input_tokens > 0),
            CHECK(max_output_tokens IS NULL OR max_output_tokens > 0)
        )`,
		`CREATE INDEX IF NOT EXISTS ai_model_discovery_config_time_idx ON ai_model_discovery(config_id, discovered_at_ms DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create AI configuration schema: %w", err)
		}
	}
	return nil
}

func defaultAIConfigSettings(id string) aiConfig {
	return aiConfig{
		ID: id, Name: "Default", NonSecretHeaders: map[string]string{}, SystemPrompt: defaultAISystemPrompt,
		Temperature: 0.7, ReasoningEffort: "automatic", MaxTurnOutputTokens: defaultAITurnOutputTokens,
		MaxActiveContextTokens: defaultAIActiveContextTokens, MaxAgentRounds: defaultAIAgentRounds,
		MaxAgentToolCalls: defaultAIAgentToolCalls, MaxAgentNoProgressRounds: defaultAIAgentNoProgressRounds,
		RequestTimeoutSeconds: defaultAIRequestTimeoutSeconds, MaxRetries: defaultAIMaxRetries,
		RetryBaseDelayMilliseconds: defaultAIRetryDelayMilliseconds, ShowUsage: true,
	}
}

func applyLegacyAIConfigDefaults(config aiConfig) aiConfig {
	defaults := defaultAIConfigSettings(config.ID)
	config.Provider = canonicalAIProvider(config.Provider)
	config.NonSecretHeaders = cloneStringMap(config.NonSecretHeaders)
	if config.SystemPrompt == "" {
		config.SystemPrompt = defaults.SystemPrompt
	}
	if config.ReasoningEffort == "" {
		config.ReasoningEffort = defaults.ReasoningEffort
	}
	if config.Temperature == 0 {
		config.Temperature = defaults.Temperature
	}
	if config.MaxTurnOutputTokens == 0 {
		config.MaxTurnOutputTokens = defaults.MaxTurnOutputTokens
	}
	if config.MaxActiveContextTokens == 0 {
		config.MaxActiveContextTokens = defaults.MaxActiveContextTokens
	}
	if config.MaxAgentRounds == 0 {
		config.MaxAgentRounds = defaults.MaxAgentRounds
	}
	if config.MaxAgentToolCalls == 0 {
		config.MaxAgentToolCalls = defaults.MaxAgentToolCalls
	}
	if config.MaxAgentNoProgressRounds == 0 {
		config.MaxAgentNoProgressRounds = defaults.MaxAgentNoProgressRounds
	}
	if config.RequestTimeoutSeconds == 0 {
		config.RequestTimeoutSeconds = defaults.RequestTimeoutSeconds
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = defaults.MaxRetries
	}
	if config.RetryBaseDelayMilliseconds == 0 {
		config.RetryBaseDelayMilliseconds = defaults.RetryBaseDelayMilliseconds
	}
	// These fields did not exist in the v1 identity document.
	config.ShowUsage = true
	if config.Revision == 0 {
		config.Revision = 1
	}
	return config
}

func (store *businessStore) migrateLegacyAIConfigs(ctx context.Context, legacy map[string]aiConfig) error {
	if store == nil || len(legacy) == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin AI configuration migration: %w", err)
	}
	fail := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	ids := make([]string, 0, len(legacy))
	for id := range legacy {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		config := legacy[id]
		if config.ID == "" {
			config.ID = id
		}
		config = applyLegacyAIConfigDefaults(config)
		if config.ID != id || validateAIConfigForStorage(config, true) != nil {
			return fail(errors.New("legacy AI configuration is invalid"))
		}
		existing, _, found, err := queryAIConfig(ctx, tx, store.deviceID.String(), id)
		if err != nil {
			return fail(err)
		}
		if found {
			if !equalAIConfig(existing, config) {
				return fail(errors.New("legacy AI configuration conflicts with BusinessStore"))
			}
			continue
		}
		now := time.Now().UTC()
		if err := insertAIConfig(ctx, tx, store.deviceID.String(), config, now, now); err != nil {
			return fail(err)
		}
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return fmt.Errorf("commit AI configuration migration: %w", err)
	}
	return nil
}

func (store *businessStore) listAIConfigs(ctx context.Context) ([]aiConfig, error) {
	if store == nil {
		return nil, errors.New("AI configuration store is unavailable")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, aiConfigSelect+` WHERE device_id = ? ORDER BY id`, store.deviceID.String())
	if err != nil {
		return nil, fmt.Errorf("list AI configurations: %w", err)
	}
	defer rows.Close()
	result := make([]aiConfig, 0)
	for rows.Next() {
		config, _, err := scanAIConfig(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, config)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list AI configurations: %w", err)
	}
	return result, nil
}

func (store *businessStore) putAIConfig(ctx context.Context, config aiConfig, expectedRevision uint64) (aiConfig, error) {
	if store == nil || validateAIConfigForStorage(config, false) != nil {
		return aiConfig{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return aiConfig{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return aiConfig{}, fmt.Errorf("begin AI configuration update: %w", err)
	}
	fail := func(cause error) (aiConfig, error) {
		_ = tx.Rollback()
		return aiConfig{}, cause
	}
	current, createdAtMilliseconds, found, err := queryAIConfig(ctx, tx, store.deviceID.String(), config.ID)
	if err != nil {
		return fail(err)
	}
	if (!found && expectedRevision != 0) || (found && expectedRevision != current.Revision) {
		return fail(errRPCRevision)
	}
	if found && current.Revision == ^uint64(0) {
		return fail(errRPCRevision)
	}
	now := time.Now().UTC()
	createdAt := now
	config.Revision = 1
	if found {
		createdAt = time.UnixMilli(createdAtMilliseconds).UTC()
		config.Revision = current.Revision + 1
	}
	if err := insertAIConfig(ctx, tx, store.deviceID.String(), config, createdAt, now); err != nil {
		return fail(err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_model_discovery WHERE config_id = ? AND config_revision <> ?`, config.ID, config.Revision); err != nil {
		return fail(fmt.Errorf("invalidate AI model discovery: %w", err))
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiConfig{}, fmt.Errorf("commit AI configuration update: %w", err)
	}
	return config, nil
}

func (store *businessStore) deleteAIConfig(ctx context.Context, id string, expectedRevision *uint64) (aiConfig, error) {
	if store == nil || !validAIConfigID(id) {
		return aiConfig{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return aiConfig{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return aiConfig{}, fmt.Errorf("begin AI configuration delete: %w", err)
	}
	config, _, found, err := queryAIConfig(ctx, tx, store.deviceID.String(), id)
	if err != nil {
		_ = tx.Rollback()
		return aiConfig{}, err
	}
	if !found {
		_ = tx.Rollback()
		return aiConfig{}, errRPCNotFound
	}
	if expectedRevision != nil && *expectedRevision != config.Revision {
		_ = tx.Rollback()
		return aiConfig{}, errRPCRevision
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_configs WHERE id = ? AND device_id = ?`, id, store.deviceID.String()); err != nil {
		_ = tx.Rollback()
		return aiConfig{}, fmt.Errorf("delete AI configuration: %w", err)
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return aiConfig{}, fmt.Errorf("commit AI configuration delete: %w", err)
	}
	return config, nil
}

func (store *businessStore) replaceAIModelDiscovery(ctx context.Context, config aiConfig, models []aiModelDescriptor, discoveredAt time.Time) error {
	if store == nil || !validAIConfigID(config.ID) || config.Revision == 0 || len(models) == 0 || len(models) > 1000 || discoveredAt.IsZero() {
		return errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin AI model discovery update: %w", err)
	}
	fail := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	current, _, found, err := queryAIConfig(ctx, tx, store.deviceID.String(), config.ID)
	if err != nil {
		return fail(err)
	}
	if !found || current.Revision != config.Revision {
		return fail(errRPCRevision)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_model_discovery WHERE config_id = ?`, config.ID); err != nil {
		return fail(fmt.Errorf("clear AI model discovery: %w", err))
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if strings.TrimSpace(model.ID) == "" || len(model.ID) > 240 || strings.TrimSpace(model.DisplayName) == "" || len(model.DisplayName) > 240 {
			return fail(errRPCInvalid)
		}
		if model.MaxInputTokens != nil && (*model.MaxInputTokens == 0 || *model.MaxInputTokens > 2000000) ||
			model.MaxOutputTokens != nil && (*model.MaxOutputTokens == 0 || *model.MaxOutputTokens > 1000000) {
			return fail(errRPCInvalid)
		}
		if _, duplicate := seen[model.ID]; duplicate {
			return fail(errRPCInvalid)
		}
		seen[model.ID] = struct{}{}
		capabilities, err := json.Marshal(model.Capabilities)
		if err != nil || len(capabilities) > 16384 {
			return fail(errRPCInvalid)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ai_model_discovery(
            config_id, config_revision, model_id, display_name, capabilities_json,
            max_input_tokens, max_output_tokens, discovered_at_ms
        ) VALUES(?,?,?,?,?,?,?,?)`, config.ID, config.Revision, model.ID, model.DisplayName, string(capabilities),
			nullableAIUint(model.MaxInputTokens), nullableAIUint(model.MaxOutputTokens), discoveredAt.UTC().UnixMilli()); err != nil {
			return fail(fmt.Errorf("store AI model discovery: %w", err))
		}
	}
	if err := commitBusinessTransaction(ctx, tx); err != nil {
		return fmt.Errorf("commit AI model discovery update: %w", err)
	}
	return nil
}

func (store *businessStore) listAIModelDiscovery(ctx context.Context, configID string, revision uint64) ([]aiModelDescriptor, time.Time, error) {
	if store == nil || !validAIConfigID(configID) || revision == 0 {
		return nil, time.Time{}, errRPCInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	db, err := store.openDB()
	if err != nil {
		return nil, time.Time{}, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT model_id, display_name, capabilities_json,
        max_input_tokens, max_output_tokens, discovered_at_ms
        FROM ai_model_discovery WHERE config_id = ? AND config_revision = ? ORDER BY model_id`, configID, revision)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list AI model discovery: %w", err)
	}
	defer rows.Close()
	result := make([]aiModelDescriptor, 0)
	var discoveredAt time.Time
	for rows.Next() {
		var model aiModelDescriptor
		var capabilitiesJSON string
		var maxInput, maxOutput sql.NullInt64
		var discoveredAtMilliseconds int64
		if err := rows.Scan(&model.ID, &model.DisplayName, &capabilitiesJSON, &maxInput, &maxOutput, &discoveredAtMilliseconds); err != nil {
			return nil, time.Time{}, fmt.Errorf("scan AI model discovery: %w", err)
		}
		if json.Unmarshal([]byte(capabilitiesJSON), &model.Capabilities) != nil || model.Capabilities == nil || discoveredAtMilliseconds <= 0 {
			return nil, time.Time{}, errors.New("stored AI model discovery is invalid")
		}
		if maxInput.Valid {
			value := uint64(maxInput.Int64)
			model.MaxInputTokens = &value
		}
		if maxOutput.Valid {
			value := uint64(maxOutput.Int64)
			model.MaxOutputTokens = &value
		}
		observed := time.UnixMilli(discoveredAtMilliseconds).UTC()
		if discoveredAt.IsZero() || observed.Before(discoveredAt) {
			discoveredAt = observed
		}
		result = append(result, model)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("list AI model discovery: %w", err)
	}
	return result, discoveredAt, nil
}

func nullableAIUint(value *uint64) any {
	if value == nil {
		return nil
	}
	return *value
}

const aiConfigSelect = `SELECT id, revision, name, provider, base_url, non_secret_headers_json, model,
    system_prompt, temperature, reasoning_effort, max_turn_output_tokens, max_active_context_tokens,
    max_agent_rounds, max_agent_tool_calls, max_agent_no_progress_rounds, request_timeout_seconds,
    max_retries, retry_base_delay_ms, show_usage, enabled, secret_configured, created_at_ms
    FROM ai_configs`

type aiConfigScanner interface {
	Scan(...any) error
}

func scanAIConfig(scanner aiConfigScanner) (aiConfig, int64, error) {
	var config aiConfig
	var headersJSON string
	var showUsage, enabled, secretConfigured int
	var createdAtMilliseconds int64
	err := scanner.Scan(
		&config.ID, &config.Revision, &config.Name, &config.Provider, &config.BaseURL, &headersJSON, &config.Model,
		&config.SystemPrompt, &config.Temperature, &config.ReasoningEffort, &config.MaxTurnOutputTokens,
		&config.MaxActiveContextTokens, &config.MaxAgentRounds, &config.MaxAgentToolCalls,
		&config.MaxAgentNoProgressRounds, &config.RequestTimeoutSeconds, &config.MaxRetries,
		&config.RetryBaseDelayMilliseconds, &showUsage, &enabled, &secretConfigured, &createdAtMilliseconds,
	)
	if err != nil {
		return aiConfig{}, 0, fmt.Errorf("scan AI configuration: %w", err)
	}
	if json.Unmarshal([]byte(headersJSON), &config.NonSecretHeaders) != nil || config.NonSecretHeaders == nil ||
		(showUsage != 0 && showUsage != 1) || (enabled != 0 && enabled != 1) ||
		(secretConfigured != 0 && secretConfigured != 1) || createdAtMilliseconds <= 0 {
		return aiConfig{}, 0, errors.New("stored AI configuration is invalid")
	}
	config.ShowUsage, config.Enabled, config.CredentialConfigured = showUsage == 1, enabled == 1, secretConfigured == 1
	if validateAIConfigForStorage(config, true) != nil {
		return aiConfig{}, 0, errors.New("stored AI configuration is invalid")
	}
	return config, createdAtMilliseconds, nil
}

func queryAIConfig(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, deviceID, id string) (aiConfig, int64, bool, error) {
	config, createdAt, err := scanAIConfig(queryer.QueryRowContext(ctx, aiConfigSelect+` WHERE device_id = ? AND id = ?`, deviceID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return aiConfig{}, 0, false, nil
	}
	if err != nil {
		return aiConfig{}, 0, false, err
	}
	return config, createdAt, true, nil
}

func insertAIConfig(ctx context.Context, tx *sql.Tx, deviceID string, config aiConfig, createdAt, updatedAt time.Time) error {
	headers, err := json.Marshal(config.NonSecretHeaders)
	if err != nil || len(headers) > 16384 {
		return errRPCInvalid
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ai_configs(
        id, device_id, revision, name, provider, base_url, non_secret_headers_json, model,
        system_prompt, temperature, reasoning_effort, max_turn_output_tokens, max_active_context_tokens,
        max_agent_rounds, max_agent_tool_calls, max_agent_no_progress_rounds, request_timeout_seconds,
        max_retries, retry_base_delay_ms, show_usage, enabled, secret_configured, created_at_ms, updated_at_ms
    ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
    ON CONFLICT(id) DO UPDATE SET
        device_id=excluded.device_id, revision=excluded.revision, name=excluded.name, provider=excluded.provider,
        base_url=excluded.base_url, non_secret_headers_json=excluded.non_secret_headers_json, model=excluded.model,
        system_prompt=excluded.system_prompt, temperature=excluded.temperature, reasoning_effort=excluded.reasoning_effort,
        max_turn_output_tokens=excluded.max_turn_output_tokens, max_active_context_tokens=excluded.max_active_context_tokens,
        max_agent_rounds=excluded.max_agent_rounds, max_agent_tool_calls=excluded.max_agent_tool_calls,
        max_agent_no_progress_rounds=excluded.max_agent_no_progress_rounds, request_timeout_seconds=excluded.request_timeout_seconds,
        max_retries=excluded.max_retries, retry_base_delay_ms=excluded.retry_base_delay_ms,
        show_usage=excluded.show_usage, enabled=excluded.enabled, secret_configured=excluded.secret_configured,
        updated_at_ms=excluded.updated_at_ms`,
		config.ID, deviceID, config.Revision, config.Name, config.Provider, config.BaseURL, string(headers), config.Model,
		config.SystemPrompt, config.Temperature, config.ReasoningEffort, config.MaxTurnOutputTokens,
		config.MaxActiveContextTokens, config.MaxAgentRounds, config.MaxAgentToolCalls,
		config.MaxAgentNoProgressRounds, config.RequestTimeoutSeconds, config.MaxRetries,
		config.RetryBaseDelayMilliseconds, aiBoolInt(config.ShowUsage), aiBoolInt(config.Enabled),
		aiBoolInt(config.CredentialConfigured), createdAt.UTC().UnixMilli(), updatedAt.UTC().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("store AI configuration: %w", err)
	}
	return nil
}

func equalAIConfig(left, right aiConfig) bool {
	left.Credential, left.LegacyCredential = "", ""
	right.Credential, right.LegacyCredential = "", ""
	left.NonSecretHeaders = cloneStringMap(left.NonSecretHeaders)
	right.NonSecretHeaders = cloneStringMap(right.NonSecretHeaders)
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func cloneStringMap(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func aiBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func canonicalAIProvider(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.NewReplacer("_", "", "-", "", " ", "").Replace(normalized)
	switch normalized {
	case "openai":
		return "openai"
	case "anthropic", "claude":
		return "anthropic"
	case "google", "gemini", "googlegemini":
		return "google"
	case "deepseek":
		return "deepseek"
	case "ollama":
		return "ollama"
	case "openaicompatible", "compatible", "custom":
		return "openai-compatible"
	default:
		return ""
	}
}

func validAIConfigID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 80 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validateAIConfigForStorage(config aiConfig, allowEmptyBaseURL bool) error {
	if !validAIConfigID(config.ID) || strings.TrimSpace(config.Name) == "" || len(config.Name) > 120 ||
		!slices.Contains(supportedAIProviders, config.Provider) || len(config.BaseURL) > 2048 ||
		(!allowEmptyBaseURL && strings.TrimSpace(config.BaseURL) == "") || strings.TrimSpace(config.Model) == "" || len(config.Model) > 120 ||
		len(config.SystemPrompt) > 32768 || config.Temperature < 0 || config.Temperature > 2 ||
		!slices.Contains([]string{"automatic", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, config.ReasoningEffort) ||
		config.MaxTurnOutputTokens < 1 || config.MaxTurnOutputTokens > 1000000 ||
		config.MaxActiveContextTokens < 4096 || config.MaxActiveContextTokens > 2000000 ||
		config.MaxAgentRounds < 1 || config.MaxAgentRounds > 200 || config.MaxAgentToolCalls < 1 || config.MaxAgentToolCalls > 200 ||
		config.MaxAgentNoProgressRounds < 1 || config.MaxAgentNoProgressRounds > 50 ||
		config.RequestTimeoutSeconds < 1 || config.RequestTimeoutSeconds > 3600 || config.MaxRetries > 5 ||
		config.RetryBaseDelayMilliseconds > 60000 || validateNonSecretAIHeaders(config.NonSecretHeaders) != nil {
		return errRPCInvalid
	}
	return nil
}

func validateNonSecretAIHeaders(headers map[string]string) error {
	if len(headers) > 32 {
		return errRPCInvalid
	}
	for name, value := range headers {
		name = strings.TrimSpace(name)
		normalized := strings.ToLower(name)
		normalized = strings.Map(func(character rune) rune {
			if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
				return character
			}
			return -1
		}, normalized)
		if name == "" || len(name) > 128 || len(value) > 4096 || strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") ||
			strings.Contains(normalized, "authorization") || strings.Contains(normalized, "apikey") || strings.Contains(normalized, "token") ||
			strings.Contains(normalized, "secret") || strings.Contains(normalized, "cookie") {
			return errRPCInvalid
		}
	}
	return nil
}
