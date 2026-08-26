package catalog

import (
	"bytes"
	"strings"
	"testing"
)

func TestValidGitHubRepository(t *testing.T) {
	tests := []struct {
		repository string
		valid      bool
	}{
		{"lyming99/wenzwork_web", true},
		{" acme/wenzwork ", true},
		{"", false},
		{"acme", false},
		{"acme/wenzwork/releases", false},
		{"acme/../wenzwork", false},
		{"acme/wenz work", false},
		{string(make([]byte, 201)), false},
	}
	for _, test := range tests {
		if got := validGitHubRepository(test.repository); got != test.valid {
			t.Errorf("validGitHubRepository(%q) = %v, want %v", test.repository, got, test.valid)
		}
	}
}

func TestNormalizeReleaseMirrorBaseURL(t *testing.T) {
	for _, test := range []struct {
		value, normalized string
		valid             bool
	}{
		{"", "", true},
		{" https://mirror.example.com/ ", "https://mirror.example.com", true},
		{"http://mirror.example.com:8080/wenzwork", "http://mirror.example.com:8080/wenzwork", true},
		{"https://user:password@mirror.example.com", "", false},
		{"https://mirror.example.com?token=secret", "", false},
		{"https://mirror.example.com/#fragment", "", false},
		{"ftp://mirror.example.com", "", false},
		{"mirror.example.com", "", false},
		{"https://mirror.example.com/../admin", "", false},
		{"https://mirror.example.com/" + strings.Repeat("a", maxReleaseMirrorBaseURLLength), "", false},
	} {
		got, valid := normalizeReleaseMirrorBaseURL(test.value)
		if valid != test.valid || got != test.normalized {
			t.Errorf("normalizeReleaseMirrorBaseURL(%q) = %q, %v, want %q, %v", test.value, got, valid, test.normalized, test.valid)
		}
	}
}

func TestValidGitHubToken(t *testing.T) {
	for _, test := range []struct {
		token string
		valid bool
	}{
		{"github_pat_example_123", true},
		{"ghp-example-token", true},
		{"", false},
		{"token with spaces", false},
		{"token\nwith-newline", false},
		{string(make([]byte, maxGitHubTokenLength+1)), false},
	} {
		if got := validGitHubToken(test.token); got != test.valid {
			t.Errorf("validGitHubToken(%q) = %v, want %v", test.token, got, test.valid)
		}
	}
}

func TestReleaseSourceTokenCipherUsesRandomAuthenticatedEncryption(t *testing.T) {
	codec, err := newReleaseSourceTokenCipher("test-master-key-that-is-at-least-32-bytes-long")
	if err != nil {
		t.Fatalf("newReleaseSourceTokenCipher() error = %v", err)
	}
	plaintext := []byte("github_pat_private_repository_token")
	first, err := codec.encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt(first) error = %v", err)
	}
	second, err := codec.encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt(second) error = %v", err)
	}
	if bytes.Equal(first, second) || bytes.Contains(first, plaintext) {
		t.Fatal("ciphertext is deterministic or contains plaintext")
	}
	decrypted, err := codec.decrypt(first)
	if err != nil || !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypt() = %q, %v", decrypted, err)
	}
	first[len(first)-1] ^= 1
	if _, err := codec.decrypt(first); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	otherCodec, err := newReleaseSourceTokenCipher("different-master-key-that-is-at-least-32-bytes")
	if err != nil {
		t.Fatalf("newReleaseSourceTokenCipher(other) error = %v", err)
	}
	if _, err := otherCodec.decrypt(second); err == nil {
		t.Fatal("ciphertext was accepted with a different master key")
	}
}
