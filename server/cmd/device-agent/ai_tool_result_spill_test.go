package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func TestAIWorkspaceSpillPreviewBoundsAndMarkers(t *testing.T) {
	content := "HEAD-MARKER " + strings.Repeat("m", 100_000) + " TAIL-MARKER"
	preview := aiWorkspaceSpillPreview(content, uuid.NewString())
	if len(preview) > maximumAIWorkspaceToolResult {
		t.Fatalf("preview exceeds budget: %d", len(preview))
	}
	if !strings.Contains(preview, "HEAD-MARKER") || !strings.Contains(preview, "TAIL-MARKER") ||
		!strings.Contains(preview, "read_tool_result") || !utf8.ValidString(preview) {
		t.Fatalf("preview = %q", preview)
	}
	suffix := aiWorkspaceUTF8Suffix(strings.Repeat("中", 30000), 100)
	if len(suffix) > 100 || !utf8.ValidString(suffix) {
		t.Fatalf("suffix invalid: len=%d valid=%v", len(suffix), utf8.ValidString(suffix))
	}
}

func TestAIWorkspaceToolSpillSavesAndPagesArtifact(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	config := installTestAIConfig(fixture.state)
	conversation, err := fixture.state.business.createAIConversation(t.Context(), fixture.project.ID, "", "spill", aiWorkspaceModeWorkspaceWrite, config, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	fixture.context.ConversationID = conversation.ID
	var builder strings.Builder
	for index := 0; index < 400; index++ {
		fmt.Fprintf(&builder, "needle marker-%03d %s\n", index, strings.Repeat("z", 110))
	}
	full := builder.String()
	plan := aiWorkspaceToolPlan{Call: aiWorkspaceToolCall{ID: "spill-call-1", Name: "search_files"}}
	result := fixture.executor.boundAIWorkspaceToolResult(fixture.context, plan, aiWorkspaceToolSuccess(full, "找到 400 条匹配", map[string]any{"count": 400}))
	if len(result.Content) > maximumAIWorkspaceToolResult {
		t.Fatalf("preview exceeds budget: %d", len(result.Content))
	}
	artifactID, _ := result.Metadata["artifact_id"].(string)
	if artifactID == "" || !strings.Contains(result.Content, "read_tool_result") ||
		!strings.Contains(result.Content, "marker-000") || !strings.Contains(result.Content, "marker-399") ||
		strings.Contains(result.Content, "marker-200") {
		t.Fatalf("spill preview = %+v", result)
	}
	stored, total, err := fixture.state.business.readAIToolResultArtifact(t.Context(), fixture.context.ConversationID, artifactID)
	if err != nil || int64(len(full)) != total || string(stored) != full {
		t.Fatalf("stored artifact total=%d bytes=%d error=%v", total, len(stored), err)
	}
	middle := strings.Index(full, "marker-200")
	readPlan := planAIWorkspaceTool(t, fixture, "read_tool_result", map[string]any{
		"artifact_id": artifactID, "offset": float64(middle - 100), "max_bytes": float64(500),
	})
	read := fixture.executor.Execute(t.Context(), fixture.context, readPlan, false)
	if read.IsError || !strings.Contains(read.Content, "marker-200") || read.Metadata["artifact_id"] != artifactID {
		t.Fatalf("paged read = %+v", read)
	}
	// The read-only permission mode also allows reading spilled artifacts.
	readOnly := newAIWorkspaceToolFixture(t, aiWorkspaceModeReadOnly)
	if _, err := readOnly.executor.Plan(t.Context(), readOnly.context, aiWorkspaceToolCall{
		ID: uuid.NewString(), Name: "read_tool_result", Arguments: map[string]any{"artifact_id": artifactID},
	}); err != nil {
		t.Fatalf("readOnly read_tool_result plan: %v", err)
	}
	// Cross-conversation artifact reads fail closed.
	if _, _, err := fixture.state.business.readAIToolResultArtifact(t.Context(), readOnly.context.ConversationID, artifactID); !errors.Is(err, errRPCNotFound) {
		t.Fatalf("cross-conversation read error = %v", err)
	}
}

func TestAIConversationToolLoopSpillsAndReadsSearchResult(t *testing.T) {
	var fixture aiConversationToolTestFixture
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, prompt aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "search-call-1", Name: "search_files", Arguments: json.RawMessage(`{"query":"needle","max_results":300}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			var spilled struct {
				Content  string `json:"content"`
				Metadata struct {
					ArtifactID string `json:"artifact_id"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(prompt.ToolExchanges[0].Results[0].Content), &spilled); err != nil || spilled.Metadata.ArtifactID == "" {
				return fmt.Errorf("decode spill result: %w", err)
			}
			stored, _, err := fixture.state.business.readAIToolResultArtifact(t.Context(), fixture.conversation.ID, spilled.Metadata.ArtifactID)
			if err != nil {
				return err
			}
			middle := bytes.Index(stored, []byte("marker-175"))
			if middle < 0 {
				return fmt.Errorf("marker-175 not in stored artifact")
			}
			arguments, _ := json.Marshal(map[string]any{
				"artifact_id": spilled.Metadata.ArtifactID, "offset": middle - 250, "max_bytes": 1500,
			})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "read-artifact-1", Name: "read_tool_result", Arguments: arguments,
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 2:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "产物读取完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture = newAIConversationToolTestFixture(t, "readOnly", provider)
	var builder strings.Builder
	for line := 0; line < 300; line++ {
		fmt.Fprintf(&builder, "needle marker-%03d %s\n", line, strings.Repeat("y", 110))
	}
	if err := os.WriteFile(filepath.Join(fixture.project.LocalPath, "big.txt"), []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "搜索大结果并分页读取",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	if len(prompts) != 3 {
		t.Fatalf("provider calls = %d", len(prompts))
	}
	spilled := prompts[1].ToolExchanges[0].Results[0]
	var decoded struct {
		Content  string `json:"content"`
		IsError  bool   `json:"isError"`
		Metadata struct {
			ArtifactID string `json:"artifact_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(spilled.Content), &decoded); err != nil {
		t.Fatalf("decode spilled envelope: %v", err)
	}
	if decoded.IsError || len(decoded.Content) > maximumAIWorkspaceToolResult || decoded.Metadata.ArtifactID == "" {
		t.Fatalf("spilled result = %+v", spilled)
	}
	if !strings.Contains(decoded.Content, "marker-000") || !strings.Contains(decoded.Content, "marker-299") ||
		strings.Contains(decoded.Content, "marker-175") || !strings.Contains(decoded.Content, "artifact_id") {
		t.Fatalf("spill preview = %+v", spilled)
	}
	paged := prompts[2].ToolExchanges[1].Results[0]
	if paged.IsError || !strings.Contains(paged.Content, "marker-175") || !strings.Contains(paged.Content, "next_offset") {
		t.Fatalf("paged read = %+v", paged)
	}
}
