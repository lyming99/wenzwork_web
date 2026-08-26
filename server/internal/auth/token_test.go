package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestOpaqueTokenRoundTripAndUniqueness(t *testing.T) {
	first, firstDigest, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() error = %v", err)
	}
	second, secondDigest, err := NewOpaqueToken()
	if err != nil {
		t.Fatalf("NewOpaqueToken() second error = %v", err)
	}
	if first == second || firstDigest == secondDigest {
		t.Fatal("opaque tokens are not unique")
	}
	if strings.Contains(first, "+") || strings.Contains(first, "/") || strings.Contains(first, "=") {
		t.Fatalf("token %q is not raw URL-safe base64", first)
	}
	got, err := DigestOpaqueToken(first)
	if err != nil || got != firstDigest {
		t.Fatalf("DigestOpaqueToken() = %q, %v; want %q", got, err, firstDigest)
	}
}

func TestOpaqueTokenRejectsMalformedOrWrongLength(t *testing.T) {
	for _, token := range []string{"", "not base64!", "c2hvcnQ"} {
		if _, err := DigestOpaqueToken(token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("DigestOpaqueToken(%q) error = %v, want ErrInvalidToken", token, err)
		}
	}
}
