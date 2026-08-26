package main

import (
	"strings"
	"testing"
)

func collectCommandTextResults(results []CommandTextDecodeResult, text *strings.Builder, last *CommandTextDecodeResult) {
	for _, result := range results {
		text.WriteString(result.DisplayText)
		*last = result
	}
}

func TestCommandTextDecoderKeepsSplitUTF8AndSanitizesVT(t *testing.T) {
	decoder := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
	var text strings.Builder
	var last CommandTextDecodeResult
	collectCommandTextResults(decoder.Feed([]byte("before "+"\x1b]0;title")), &text, &last)
	// Split the three-byte Chinese rune and the OSC terminator across reads.
	collectCommandTextResults(decoder.Feed([]byte("\x07"+"你"[:1])), &text, &last)
	collectCommandTextResults(decoder.Feed([]byte("你"[1:]+"\x1b[?25h after\r\n")), &text, &last)
	collectCommandTextResults(decoder.Flush(), &text, &last)
	if got, want := text.String(), "before 你 after\n"; got != want {
		t.Fatalf("display text = %q, want %q", got, want)
	}
	if last.SourceEncoding != "utf-8" || last.IsBinary || last.HadDecodeErrors {
		t.Fatalf("metadata = %+v", last)
	}
}

func TestCommandTextDecoderRecognizesUTF16AndGB18030(t *testing.T) {
	utf16 := newCommandTextDecoder(commandTextDecoderOptions{SanitizeVT: true})
	var text strings.Builder
	var last CommandTextDecodeResult
	collectCommandTextResults(utf16.Feed([]byte{0xff}), &text, &last)
	collectCommandTextResults(utf16.Feed([]byte{0xfe, 0x2d, 0x4e, 0x87}), &text, &last)
	collectCommandTextResults(utf16.Feed([]byte{0x65}), &text, &last)
	collectCommandTextResults(utf16.Flush(), &text, &last)
	if got, want := text.String(), "中文"; got != want || last.SourceEncoding != "utf-16le" || last.IsBinary {
		t.Fatalf("utf16 display/metadata = %q %+v", got, last)
	}

	emoji := newCommandTextDecoder(commandTextDecoderOptions{})
	text.Reset()
	last = CommandTextDecodeResult{}
	collectCommandTextResults(emoji.Feed([]byte{0xff, 0xfe, 0x3d, 0xd8}), &text, &last)
	collectCommandTextResults(emoji.Feed([]byte{0x00, 0xde}), &text, &last)
	collectCommandTextResults(emoji.Flush(), &text, &last)
	if got, want := text.String(), "😀"; got != want || last.HadDecodeErrors {
		t.Fatalf("split utf16 surrogate = %q %+v", got, last)
	}

	gb18030 := newCommandTextDecoder(commandTextDecoderOptions{FallbackEncoding: "gb18030", SanitizeVT: true})
	text.Reset()
	last = CommandTextDecodeResult{}
	collectCommandTextResults(gb18030.Feed([]byte{0xd6, 0xd0, 0xce}), &text, &last)
	collectCommandTextResults(gb18030.Feed([]byte{0xc4}), &text, &last)
	collectCommandTextResults(gb18030.Flush(), &text, &last)
	if got, want := text.String(), "中文"; got != want || last.SourceEncoding != "gb18030" || last.IsBinary {
		t.Fatalf("gb18030 display/metadata = %q %+v", got, last)
	}
}

func TestCommandTextDecoderKeepsBinaryAsBinary(t *testing.T) {
	decoder := newCommandTextDecoder(commandTextDecoderOptions{FallbackEncoding: "gb18030", SanitizeVT: true})
	results := append([]CommandTextDecodeResult(nil), decoder.Feed([]byte{0xff, 0x00, 0x80})...)
	results = append(results, decoder.Flush()...)
	if len(results) != 1 || !results[0].IsBinary || results[0].SourceEncoding != "binary" || len(results[0].DisplayText) != 0 {
		t.Fatalf("binary results = %#v", results)
	}
}

func TestCommandTextDecoderTruncatedTailKeepsCompleteUTF8AndUTF16Text(t *testing.T) {
	utf8Decoder := newCommandTextDecoder(commandTextDecoderOptions{})
	var text strings.Builder
	var last CommandTextDecodeResult
	collectCommandTextResults(utf8Decoder.Feed([]byte("alpha \xe4\xb8")), &text, &last)
	collectCommandTextResults(utf8Decoder.FlushTruncatedTail(), &text, &last)
	if got, want := text.String(), "alpha "; got != want || last.IsBinary || last.SourceEncoding != "utf-8" {
		t.Fatalf("truncated UTF-8 = %q %+v", got, last)
	}

	utf16Decoder := newCommandTextDecoder(commandTextDecoderOptions{})
	text.Reset()
	last = CommandTextDecodeResult{}
	collectCommandTextResults(utf16Decoder.Feed([]byte{0xff, 0xfe, 0x2d, 0x4e, 0x3d, 0xd8}), &text, &last)
	collectCommandTextResults(utf16Decoder.FlushTruncatedTail(), &text, &last)
	if got, want := text.String(), "中"; got != want || last.IsBinary || last.SourceEncoding != "utf-16le" {
		t.Fatalf("truncated UTF-16 = %q %+v", got, last)
	}
}

func TestVTTextSanitizerHandlesStringControlsAcrossChunks(t *testing.T) {
	sanitizer := newVTTextSanitizer()
	got := sanitizer.Feed("A\x1b]8;;https://example")
	got += sanitizer.Feed(".test\x1b\\B\x1bPpayload\x1b\\C\x1b[31mD\x1b[0m\x01")
	got += sanitizer.Flush()
	if want := "ABCD"; got != want {
		t.Fatalf("sanitized text = %q, want %q", got, want)
	}
	counts := sanitizer.RemovedSequenceCounts()
	if counts["osc"] != 1 || counts["string"] != 1 || counts["csi"] != 2 || counts["control"] != 1 {
		t.Fatalf("removed sequence counts = %#v", counts)
	}
}
