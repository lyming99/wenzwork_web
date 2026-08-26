package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAIWorkspaceDiffView(t *testing.T) {
	original := []byte("alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\n")
	updated := []byte("alpha\nbeta\nGAMMA\ndelta\nepsilon\nzeta\n")
	view := aiWorkspaceDiffView(original, updated, "notes.txt")
	if view["kind"] != "diff" || view["path"] != "notes.txt" {
		t.Fatalf("diff view = %+v", view)
	}
	hunks, _ := view["hunks"].([]map[string]any)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %+v", hunks)
	}
	var removed, added bool
	for _, line := range hunks[0]["lines"].([]map[string]any) {
		switch line["type"] {
		case "-":
			removed = removed || line["text"] == "gamma"
		case "+":
			added = added || line["text"] == "GAMMA"
		}
	}
	if !removed || !added {
		t.Fatalf("diff lines = %+v", hunks[0]["lines"])
	}
}

func TestAIWorkspaceToolViewsAttachAndPersist(t *testing.T) {
	fixture := newAIWorkspaceToolFixture(t, aiWorkspaceModeWorkspaceWrite)
	root := fixture.project.LocalPath
	if err := writeTestWorkspaceFile(t, root, "sample.txt", "alpha\nneedle one\nomega\n"); err != nil {
		t.Fatal(err)
	}
	read := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "read_file", map[string]any{"path": "sample.txt"}), false)
	readView, _ := read.Metadata["view"].(map[string]any)
	if readView["kind"] != "read" || readView["path"] != "sample.txt" || readView["endLine"] != 4 {
		t.Fatalf("read view = %+v", readView)
	}
	search := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "search_files", map[string]any{"query": "needle"}), false)
	searchView, _ := search.Metadata["view"].(map[string]any)
	matches, _ := searchView["matches"].([]map[string]any)
	if searchView["kind"] != "search" || len(matches) != 1 || matches[0]["file"] != "sample.txt" || matches[0]["line"] != 2 {
		t.Fatalf("search view = %+v", searchView)
	}
	replace := fixture.executor.Execute(t.Context(), fixture.context, planAIWorkspaceTool(t, fixture, "replace_in_file", map[string]any{
		"path": "sample.txt", "old_text": "omega", "new_text": "OMEGA",
		"expected_hash": read.Metadata["content_hash"],
	}), true)
	if replace.IsError {
		t.Fatalf("replace = %+v", replace)
	}
	diffView, _ := replace.Metadata["view"].(map[string]any)
	if diffView["kind"] != "diff" {
		t.Fatalf("diff view = %+v", diffView)
	}
}

func TestAIConversationToolLoopPersistsReadViewAndKeepsModelResultClean(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{
					ID: "read-view-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"view.txt"}`),
				}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "视图读取完成。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	if err := writeTestWorkspaceFile(t, fixture.project.LocalPath, "view.txt", "line one\nline two\n"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "读取并展示视图",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	if strings.Contains(prompts[1].ToolExchanges[0].Results[0].Content, `"view"`) {
		t.Fatal("model-facing tool result must not carry the presentation view")
	}
	page, err := fixture.state.business.listAIConversationMessages(t.Context(), fixture.project.ID, fixture.conversation.ID, 0, 10)
	if err != nil || len(page.Items) != 2 || len(page.Items[1].ToolRuns) != 1 {
		t.Fatalf("messages=%+v error=%v", page.Items, err)
	}
	run := page.Items[1].ToolRuns[0]
	view, ok := run.View.(map[string]any)
	if !ok || view["kind"] != "read" || view["path"] != "view.txt" {
		t.Fatalf("persisted view = %+v", run.View)
	}
}

func writeTestWorkspaceFile(t *testing.T, root, name, contents string) error {
	t.Helper()
	return os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600)
}
