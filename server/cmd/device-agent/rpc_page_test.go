package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRPCPagePrefixMeasuresCompleteEncodedJSONAndAdvancesCursor(t *testing.T) {
	items := make([]string, 12)
	for index := range items {
		// encoding/json escapes '<' as six bytes. This catches implementations
		// that budget raw strings or item bodies instead of the final response.
		items[index] = strings.Repeat("<", 1024)
	}
	build := func(count int) any {
		return map[string]any{
			"items": items[:count], "nextCursor": versionedPageCursor(7, count, len(items)),
			"highWatermark": uint64(7), "resetRequired": false,
		}
	}
	count, err := rpcPagePrefixLength(len(items), build)
	if err != nil || count < 1 || count >= len(items) {
		t.Fatalf("bounded prefix count=%d error=%v", count, err)
	}
	encoded, err := json.Marshal(build(count))
	if err != nil || len(encoded) > preferredRPCPagePayload {
		t.Fatalf("bounded page bytes=%d error=%v", len(encoded), err)
	}
	nextEncoded, err := json.Marshal(build(count + 1))
	if err != nil || len(nextEncoded) <= preferredRPCPagePayload {
		t.Fatalf("larger page unexpectedly fits: bytes=%d error=%v", len(nextEncoded), err)
	}

	cursor := versionedPageCursor(7, count, len(items))
	start, _, _, err := versionedPageWindow(rpcInput{"cursor": *cursor}, len(items), 7)
	if err != nil || start != count {
		t.Fatalf("next cursor resumes at %d, want %d (error=%v)", start, count, err)
	}
}

func TestRPCPagePrefixRejectsOversizedAtomicItem(t *testing.T) {
	item := strings.Repeat("<", preferredRPCPagePayload)
	_, err := rpcPagePrefixLength(1, func(count int) any {
		items := []string{}
		if count == 1 {
			items = []string{item}
		}
		return map[string]any{"items": items, "nextCursor": "resume"}
	})
	if !errors.Is(err, errRPCResponsePageTooLarge) {
		t.Fatalf("oversized atomic item error=%v", err)
	}
}

func TestRPCPageSnapshotWatermarkBindsResourceAndOrderedContent(t *testing.T) {
	first, err := rpcPageSnapshotWatermark(map[string]any{"resource": "a", "items": []string{"one", "two"}})
	if err != nil || first == 0 {
		t.Fatalf("first snapshot watermark=%d error=%v", first, err)
	}
	same, err := rpcPageSnapshotWatermark(map[string]any{"resource": "a", "items": []string{"one", "two"}})
	if err != nil || same != first {
		t.Fatalf("stable snapshot watermark=%d, want %d (error=%v)", same, first, err)
	}
	otherResource, _ := rpcPageSnapshotWatermark(map[string]any{"resource": "b", "items": []string{"one", "two"}})
	otherOrder, _ := rpcPageSnapshotWatermark(map[string]any{"resource": "a", "items": []string{"two", "one"}})
	if otherResource == first || otherOrder == first {
		t.Fatalf("snapshot cursor was not bound: first=%d resource=%d order=%d", first, otherResource, otherOrder)
	}
}

func TestRPCPageSnapshotWatermarkStaysWithinSafeJSONIntegerRange(t *testing.T) {
	for raw, want := range map[uint64]uint64{
		0:                  1,
		1:                  1,
		maxSafeJSONInteger: maxSafeJSONInteger,
		^uint64(0):         maxSafeJSONInteger,
	} {
		if got := safeRPCPageWatermark(raw); got != want {
			t.Fatalf("safe page watermark(%d) = %d, want %d", raw, got, want)
		}
		if got := safeRPCPageWatermark(raw); got > maxSafeJSONInteger {
			t.Fatalf("safe page watermark(%d) exceeds JSON range: %d", raw, got)
		}
	}
}

func TestRPCPaginationCapabilityCoversGrowableMethods(t *testing.T) {
	for _, method := range []string{
		"project.list", "project.directory.list", "task.list", "task.logs", "task.runs",
		"workflow.revisions", "file.list", "file.search", "conversation.search", "conversation.subagents.list",
		"ai.config.list", "ai.config.models",
	} {
		if !methodSupportsRPCPagination(method) {
			t.Errorf("method %q is not marked paginatable", method)
		}
	}
	if fileResponseBudget != preferredRPCPagePayload {
		t.Fatalf("file response budget=%d, preferred page=%d", fileResponseBudget, preferredRPCPagePayload)
	}
}
