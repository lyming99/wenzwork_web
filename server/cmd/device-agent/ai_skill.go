package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maximumAISkills             = 64
	maximumAISkillBytes         = 24 << 10
	maximumAISkillDescription   = 500
	maximumAISkillLoadsPerTurn  = 8
)

// aiSkill is one loadable instruction bundle: a SKILL.md under
// .wenzwork/skills/<name>/ with an optional YAML-ish frontmatter carrying
// name/description/when_to_use.
type aiSkill struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	WhenToUse    string `json:"whenToUse,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

func validAISkillName(name string) bool {
	if len(name) < 1 || len(name) > 64 || !utf8.ValidString(name) {
		return false
	}
	for _, character := range name {
		if character != '_' && character != '-' && (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// parseAISkillFile splits an optional leading frontmatter (--- ... ---) from
// the instruction body and normalizes the known keys.
func parseAISkillFile(name, contents string) (*aiSkill, error) {
	description, whenToUse := "", ""
	body := contents
	if strings.HasPrefix(contents, "---\n") {
		rest := contents[4:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			frontmatter := rest[:end]
			body = rest[end+len("\n---"):]
			for _, line := range strings.Split(frontmatter, "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
				if !ok {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "name":
					candidate := strings.TrimSpace(value)
					if validAISkillName(candidate) {
						name = candidate
					}
				case "description":
					description = strings.TrimSpace(value)
				case "when_to_use", "when-to-use":
					whenToUse = strings.TrimSpace(value)
				}
			}
		}
	}
	body = strings.TrimSpace(body)
	if !validAISkillName(name) || description == "" || len(description) > maximumAISkillDescription ||
		body == "" || len(body) > maximumAISkillBytes || !utf8.ValidString(body) {
		return nil, errRPCInvalid
	}
	return &aiSkill{Name: name, Description: description, WhenToUse: whenToUse, Instructions: body}, nil
}

// loadAISkillCatalog scans <root>/.wenzwork/skills for skill bundles. Both
// <name>/SKILL.md directories and flat <name>.md files are accepted; invalid
// entries are skipped and the catalog stays bounded.
func loadAISkillCatalog(root string) []aiSkill {
	directory := filepath.Join(root, ".wenzwork", "skills")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	skills := make([]aiSkill, 0, min(len(entries), maximumAISkills))
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if len(skills) >= maximumAISkills {
			break
		}
		name := entry.Name()
		var path string
		switch {
		case entry.IsDir():
			path = filepath.Join(directory, name, "SKILL.md")
			if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() {
				continue
			}
		case strings.HasSuffix(strings.ToLower(name), ".md") && !entry.IsDir():
			path = filepath.Join(directory, name)
			name = strings.TrimSuffix(name, filepath.Ext(name))
		default:
			continue
		}
		if !validAISkillName(name) {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		contents, readErr := readBoundedFile(path, maximumAISkillBytes+4096)
		if readErr != nil {
			continue
		}
		skill, parseErr := parseAISkillFile(name, string(contents))
		if parseErr != nil {
			continue
		}
		seen[name] = struct{}{}
		skills = append(skills, aiSkill{Name: skill.Name, Description: skill.Description, WhenToUse: skill.WhenToUse})
	}
	return skills
}

// loadAISkill returns the full instructions of one named skill.
func loadAISkill(root, name string) (*aiSkill, error) {
	if !validAISkillName(name) {
		return nil, errRPCInvalid
	}
	directory := filepath.Join(root, ".wenzwork", "skills")
	candidates := []string{
		filepath.Join(directory, name, "SKILL.md"),
		filepath.Join(directory, name+".md"),
	}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		contents, readErr := readBoundedFile(path, maximumAISkillBytes+4096)
		if readErr != nil {
			return nil, readErr
		}
		return parseAISkillFile(name, string(contents))
	}
	return nil, errRPCNotFound
}

// aiSkillCatalogText renders the model-facing catalog block injected into the
// system prompt.
func aiSkillCatalogText(skills []aiSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("<available_skills>\n")
	for _, skill := range skills {
		fmt.Fprintf(&builder, "- %s: %s", skill.Name, skill.Description)
		if skill.WhenToUse != "" {
			fmt.Fprintf(&builder, " (when to use: %s)", skill.WhenToUse)
		}
		builder.WriteString("\n")
	}
	builder.WriteString("</available_skills>\n")
	builder.WriteString("Before acting on a task that matches a listed skill, call the skill tool with its exact name to load the full instructions.")
	return builder.String()
}

// allowAISkillLoad enforces the per-generation skill load budget.
func (state *agentState) allowAISkillLoad(generationID string) bool {
	if state == nil || uuid.Validate(generationID) != nil {
		return false
	}
	state.skillLoadMu.Lock()
	defer state.skillLoadMu.Unlock()
	if state.skillLoads == nil {
		state.skillLoads = make(map[string]int)
	}
	if state.skillLoads[generationID] >= maximumAISkillLoadsPerTurn {
		return false
	}
	state.skillLoads[generationID]++
	return true
}

func (state *agentState) clearAISkillLoads(generationID string) {
	if state == nil {
		return
	}
	state.skillLoadMu.Lock()
	delete(state.skillLoads, generationID)
	state.skillLoadMu.Unlock()
}

func (d dispatcher) aiSkillCatalogForTurn(ctx context.Context, projectID uuid.UUID) string {
	if d.state == nil || d.state.business == nil {
		return ""
	}
	project, err := d.state.business.projectByID(ctx, projectID)
	if err != nil {
		return ""
	}
	return aiSkillCatalogText(loadAISkillCatalog(project.LocalPath))
}
