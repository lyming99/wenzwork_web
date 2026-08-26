package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseAISkillFile(t *testing.T) {
	skill, err := parseAISkillFile("asr-subtitle", "---\nname: asr-subtitle\ndescription: 把视频或音频里的中文语音转成字幕。\nwhen_to_use: 用户说要识别字幕或转写音频时。\n---\n1. 调用模型 A\n2. 输出 SRT")
	if err != nil || skill.Name != "asr-subtitle" || !strings.Contains(skill.Description, "字幕") ||
		!strings.Contains(skill.WhenToUse, "转写") || !strings.Contains(skill.Instructions, "SRT") {
		t.Fatalf("skill = %+v error=%v", skill, err)
	}
	flat, err := parseAISkillFile("plain", "---\ndescription: 步骤说明。\n---\nstep one\nstep two")
	if err != nil || flat.Name != "plain" || !strings.Contains(flat.Instructions, "step two") {
		t.Fatalf("flat skill = %+v error=%v", flat, err)
	}
	if _, err := parseAISkillFile("bad", "---\ndescription: no body below\n---"); err == nil {
		t.Fatal("empty body must be rejected")
	}
	if _, err := parseAISkillFile("bad", "body only, no description"); err == nil {
		t.Fatal("missing description must be rejected")
	}
	if _, err := parseAISkillFile("bad name!", "body"); err == nil {
		t.Fatal("invalid name must be rejected")
	}
}

func TestAISkillCatalogAndLoad(t *testing.T) {
	root := t.TempDir()
	skills := filepath.Join(root, ".wenzwork", "skills")
	if err := os.MkdirAll(filepath.Join(skills, "asr-subtitle"), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(path, contents string) {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(skills, "asr-subtitle", "SKILL.md"), "---\ndescription: 转写字幕。\nwhen_to_use: 需要字幕时。\n---\n使用 FunASR 转写。")
	write(filepath.Join(skills, "deploy.md"), "---\ndescription: 部署流程。\n---\n执行发布步骤。")
	write(filepath.Join(skills, "broken.md"), "---\n---\nno body")
	catalog := loadAISkillCatalog(root)
	if len(catalog) != 2 {
		t.Fatalf("catalog = %+v", catalog)
	}
	names := []string{catalog[0].Name, catalog[1].Name}
	if !strings.Contains(strings.Join(names, ","), "asr-subtitle") || !strings.Contains(strings.Join(names, ","), "deploy") {
		t.Fatalf("catalog names = %v", names)
	}
	text := aiSkillCatalogText(catalog)
	if !strings.Contains(text, "<available_skills>") || !strings.Contains(text, "asr-subtitle: 转写字幕。") ||
		!strings.Contains(text, "skill tool") {
		t.Fatalf("catalog text = %q", text)
	}
	loaded, err := loadAISkill(root, "asr-subtitle")
	if err != nil || !strings.Contains(loaded.Instructions, "FunASR") {
		t.Fatalf("loaded = %+v error=%v", loaded, err)
	}
	flat, err := loadAISkill(root, "deploy")
	if err != nil || !strings.Contains(flat.Instructions, "发布步骤") {
		t.Fatalf("flat loaded = %+v error=%v", flat, err)
	}
	if _, err := loadAISkill(root, "missing"); err == nil {
		t.Fatal("missing skill must fail")
	}
	if aiSkillCatalogText(nil) != "" {
		t.Fatal("empty catalog must render empty")
	}
}

func TestAISkillLoadBudget(t *testing.T) {
	state := &agentState{}
	generation := uuid.NewString()
	for index := 0; index < maximumAISkillLoadsPerTurn; index++ {
		if !state.allowAISkillLoad(generation) {
			t.Fatalf("load %d must be allowed", index)
		}
	}
	if state.allowAISkillLoad(generation) {
		t.Fatal("load budget overflow must be rejected")
	}
	state.clearAISkillLoads(generation)
	if !state.allowAISkillLoad(generation) {
		t.Fatal("cleared budget must allow loads again")
	}
}

func TestAIConversationToolLoopLoadsSkill(t *testing.T) {
	provider := &scriptedConversationToolProvider{}
	provider.step = func(index int, _ aiProviderPrompt, onEvent func(aiProviderStreamEvent) error) error {
		switch index {
		case 0:
			arguments, _ := json.Marshal(map[string]any{"name": "asr-subtitle"})
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "tool_calls", ToolCalls: []aiProviderToolCall{{ID: "skill-call-1", Name: "skill", Arguments: arguments}}},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "tool_calls"},
			)
		case 1:
			return emitProviderEvents(onEvent,
				aiProviderStreamEvent{Kind: "text", Delta: "技能已加载。"},
				aiProviderStreamEvent{Kind: "completed", FinishReason: "stop"},
			)
		default:
			return errAIProvider
		}
	}
	fixture := newAIConversationToolTestFixture(t, "readOnly", provider)
	skills := filepath.Join(fixture.project.LocalPath, ".wenzwork", "skills", "asr-subtitle")
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "SKILL.md"), []byte("---\ndescription: 转写字幕。\n---\n使用 FunASR 转写并输出 SRT。"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.dispatch.callConversationSend(t.Context(), rpcInput{
		"conversationId": fixture.conversation.ID, "messageId": uuid.NewString(), "prompt": "加载技能",
	}); err != nil {
		t.Fatal(err)
	}
	_, prompts := provider.snapshot()
	result := prompts[1].ToolExchanges[0].Results[0]
	if result.IsError || !strings.Contains(result.Content, "FunASR") || !strings.Contains(result.Content, `\"name\":\"asr-subtitle\"`) {
		t.Fatalf("skill result = %+v", result)
	}
	// The loaded skill remains bounded well under the inline result budget.
	if len(result.Content) > maximumAIWorkspaceToolResult {
		t.Fatalf("skill result too large: %d", len(result.Content))
	}
}
