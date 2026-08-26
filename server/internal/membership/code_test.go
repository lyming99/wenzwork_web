package membership

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCodeCodecGenerateCreatesExpectedFormatAndDigest(t *testing.T) {
	codec, err := newCodeCodec([]byte(strings.Repeat("k", 32)), bytes.NewReader(make([]byte, codeRandomLength)))
	if err != nil {
		t.Fatalf("newCodeCodec() error = %v", err)
	}

	issued, err := codec.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if issued.Plaintext != "WZM-2222-2222-2222-2222-2222" {
		t.Fatalf("Plaintext = %q", issued.Plaintext)
	}
	if issued.Normalized != "WZM22222222222222222222" {
		t.Fatalf("Normalized = %q", issued.Normalized)
	}
	if len(issued.Digest) != 64 {
		t.Fatalf("Digest length = %d, want 64", len(issued.Digest))
	}
	if issued.Hint != "2222" {
		t.Fatalf("Hint = %q, want 2222", issued.Hint)
	}
}

func TestNormalizeCodeAllowsCaseSpacesAndHyphens(t *testing.T) {
	got, err := NormalizeCode("  wzm-2345 6789-abcd-efgh-jkmn  ")
	if err != nil {
		t.Fatalf("NormalizeCode() error = %v", err)
	}
	if got != "WZM23456789ABCDEFGHJKMN" {
		t.Fatalf("NormalizeCode() = %q", got)
	}
}

func TestNormalizeCodeRejectsAmbiguousOrUnexpectedCharacters(t *testing.T) {
	tests := []string{
		"WZM-0000-2222-2222-2222-2222",
		"WZM-OOOO-2222-2222-2222-2222",
		"WZM-2222-2222-2222-2222-22$2",
		"OTHER-2222-2222-2222-2222-2222",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			_, err := NormalizeCode(input)
			if !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("NormalizeCode() error = %v, want ErrInvalidCode", err)
			}
		})
	}
}

func TestCodeCodecUsesIndependentHMACKeys(t *testing.T) {
	first, _ := newCodeCodec([]byte(strings.Repeat("a", 32)), bytes.NewReader(make([]byte, codeRandomLength)))
	second, _ := newCodeCodec([]byte(strings.Repeat("b", 32)), bytes.NewReader(make([]byte, codeRandomLength)))
	const normalized = "WZM22222222222222222222"
	if first.Digest(normalized) == second.Digest(normalized) {
		t.Fatal("different HMAC keys produced the same digest")
	}
}

func TestCodeCodecRejectsShortHMACKey(t *testing.T) {
	_, err := NewCodeCodec([]byte("too-short"))
	if err == nil {
		t.Fatal("NewCodeCodec() error = nil, want validation error")
	}
}

func TestCodeCodecGenerateBatchRejectsOutOfRangeQuantity(t *testing.T) {
	codec, _ := NewCodeCodec([]byte(strings.Repeat("k", 32)))
	for _, quantity := range []int{0, 5001} {
		_, err := codec.GenerateBatch(quantity)
		if !errors.Is(err, ErrInvalidBatchQuantity) {
			t.Fatalf("GenerateBatch(%d) error = %v, want ErrInvalidBatchQuantity", quantity, err)
		}
	}
}

func TestCodeCodecGenerateBatchReturnsUniqueCodes(t *testing.T) {
	codec, _ := NewCodeCodec([]byte(strings.Repeat("k", 32)))
	codes, err := codec.GenerateBatch(128)
	if err != nil {
		t.Fatalf("GenerateBatch() error = %v", err)
	}
	seen := make(map[string]struct{}, len(codes))
	for _, code := range codes {
		if _, exists := seen[code.Digest]; exists {
			t.Fatalf("duplicate digest generated: %s", code.Digest)
		}
		seen[code.Digest] = struct{}{}
	}
}
