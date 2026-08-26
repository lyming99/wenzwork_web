package deviceaccesskey

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDeviceAccessKeyIsCanonicalAndOnlyDigestIsDerived(t *testing.T) {
	plaintext, digest, err := newKey(bytes.NewReader(make([]byte, secretBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, "device_") || len(plaintext) != len("device_")+43 {
		t.Fatalf("plaintext = %q", plaintext)
	}
	want := sha256.Sum256([]byte(plaintext))
	if digest != hex.EncodeToString(want[:]) {
		t.Fatalf("digest = %q", digest)
	}
	if parsed, ok := keyDigest(plaintext); !ok || parsed != digest {
		t.Fatalf("keyDigest() = %q, %t", parsed, ok)
	}
	for _, invalid := range []string{plaintext + "=", " " + plaintext, strings.ToUpper(plaintext), "relay_" + plaintext} {
		if _, ok := keyDigest(invalid); ok {
			t.Fatalf("accepted invalid key %q", invalid)
		}
	}
}

func TestAccessKeyScopesAlwaysUseFullDeviceProfile(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	want := strings.Join(FullAccessScopes(), ",")
	for _, requested := range [][]string{nil, {"remote.peer.query"}, {"remote.connect", "admin"}, {"remote.connect", "remote.connect"}} {
		input, _, err := normalizeCreate(CreateInput{
			UserID: uuid.New(), Label: " Agent key ", Scopes: requested,
		}, now)
		if err != nil {
			t.Fatalf("requested scopes %v: %v", requested, err)
		}
		if input.Label != "Agent key" || strings.Join(input.Scopes, ",") != want {
			t.Fatalf("requested scopes %v normalized = %+v", requested, input)
		}
	}
}

func TestIdempotencyKeyRequestDigestAndCipherAreCanonicalAndBound(t *testing.T) {
	for _, valid := range []string{"device-key-1", "ABC_def.123:xyz"} {
		if parsed, ok := ParseIdempotencyKey(valid); !ok || parsed != valid {
			t.Fatalf("ParseIdempotencyKey(%q) = %q, %t", valid, parsed, ok)
		}
	}
	for _, invalid := range []string{"", "short", " device-key-1", "device key 1", strings.Repeat("a", 129)} {
		if _, ok := ParseIdempotencyKey(invalid); ok {
			t.Fatalf("accepted invalid Idempotency-Key %q", invalid)
		}
	}

	userID := uuid.New()
	expiresAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	left, _, err := normalizeCreateRequest(CreateInput{
		UserID: userID, Label: "Desktop", Scopes: []string{"remote.peer.ai.config", "remote.connect"}, ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	right, _, err := normalizeCreateRequest(CreateInput{
		UserID: userID, Label: "Desktop", Scopes: []string{"remote.connect", "remote.peer.ai.config"}, ExpiresAt: &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if createRequestDigest(left) != createRequestDigest(right) {
		t.Fatal("equivalent create requests produced different digests")
	}
	right.Label = "Different"
	if createRequestDigest(left) == createRequestDigest(right) {
		t.Fatal("different create requests produced the same digest")
	}

	aead, err := newIdempotencyAEAD(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	nonceMaterial := make([]byte, aead.NonceSize()*2)
	for index := range nonceMaterial {
		nonceMaterial[index] = byte(index)
	}
	store := &Store{idempotencyAEAD: aead, random: bytes.NewReader(nonceMaterial)}
	aad := idempotencyAAD(userID, "create", userID, "device-key-1", createRequestDigest(left))
	plaintext := []byte(`{"key":"device_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`)
	first, err := store.sealIdempotentResult(plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.sealIdempotentResult(plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || bytes.Contains(first, plaintext) || bytes.Contains(second, plaintext) {
		t.Fatal("idempotency ciphertext reused a nonce or exposed plaintext")
	}
	opened, err := store.openIdempotentResult(first, aad)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("openIdempotentResult() = %q, %v", opened, err)
	}
	wrongAAD := idempotencyAAD(userID, "rotate", userID, "device-key-1", createRequestDigest(left))
	if _, err := store.openIdempotentResult(first, wrongAAD); err == nil {
		t.Fatal("ciphertext authenticated for a different operation")
	}
	if _, err := newIdempotencyAEAD(bytes.Repeat([]byte("x"), 31)); err == nil {
		t.Fatal("short idempotency encryption key was accepted")
	}
}
