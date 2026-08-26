package main

import (
	"encoding/json"
	"strings"
)

// maximumAIToolViewBytes bounds the declarative presentation view attached to
// a tool run. Views serve the UI only; the model never receives them.
const maximumAIToolViewBytes = 4 << 10

// aiWorkspaceAttachView attaches a declarative presentation view to a tool
// result. Oversized or unmarshalable views are dropped silently; the text
// result stays authoritative.
func aiWorkspaceAttachView(result *aiWorkspaceToolResult, view map[string]any) {
	if result == nil || view == nil {
		return
	}
	encoded, err := json.Marshal(view)
	if err != nil || len(encoded) > maximumAIToolViewBytes {
		return
	}
	if result.Metadata == nil {
		result.Metadata = map[string]any{}
	}
	result.Metadata["view"] = view
}

// aiWorkspaceDiffView renders a single-hunk unified-style diff view for
// replace_in_file with two lines of context on each side.
func aiWorkspaceDiffView(original, updated []byte, path string) map[string]any {
	oldLines := strings.Split(string(original), "\n")
	newLines := strings.Split(string(updated), "\n")
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}
	context := 2
	oldStart := max(0, prefix-context)
	oldEnd := min(len(oldLines), len(oldLines)-suffix+context)
	newStart := max(0, prefix-context)
	newEnd := min(len(newLines), len(newLines)-suffix+context)
	lines := make([]map[string]any, 0, (oldEnd-oldStart)+(newEnd-newStart))
	for index := oldStart; index < oldEnd; index++ {
		kind := " "
		if index >= prefix && index < len(oldLines)-suffix {
			kind = "-"
		}
		lines = append(lines, map[string]any{"type": kind, "text": oldLines[index]})
	}
	for index := newStart; index < newEnd; index++ {
		kind := " "
		if index >= prefix && index < len(newLines)-suffix {
			kind = "+"
		}
		if kind != "+" {
			continue
		}
		lines = append(lines, map[string]any{"type": kind, "text": newLines[index]})
	}
	return map[string]any{
		"kind": "diff", "path": path,
		"hunks": []map[string]any{{
			"oldStart": oldStart + 1, "oldCount": oldEnd - oldStart,
			"newStart": newStart + 1, "newCount": newEnd - newStart,
			"lines": lines,
		}},
	}
}

// aiWorkspaceSearchView renders the structured match list for search_files.
func aiWorkspaceSearchView(matches []map[string]any, truncated bool) map[string]any {
	return map[string]any{"kind": "search", "truncated": truncated, "matches": matches}
}

// aiWorkspaceWebView renders source cards for web_search.
func aiWorkspaceWebView(sources []map[string]any, truncated bool) map[string]any {
	return map[string]any{"kind": "web", "truncated": truncated, "sources": sources}
}

// aiWorkspaceReadView renders the line-range anchor for read_file.
func aiWorkspaceReadView(path string, startLine, endLine, totalLines int) map[string]any {
	return map[string]any{"kind": "read", "path": path, "startLine": startLine, "endLine": endLine, "totalLines": totalLines}
}
