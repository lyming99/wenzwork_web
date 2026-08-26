package auth

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHOTPMatchesRFC4226Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	want := []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"}
	for counter, expected := range want {
		if got := hotp(secret, uint64(counter), 6); got != expected {
			t.Fatalf("hotp(counter=%d) = %q, want %q", counter, got, expected)
		}
	}
}

func TestFindTOTPStepWindowAndInputValidation(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1_700_000_000, 0)
	current := now.Unix() / totpPeriodSeconds

	for _, offset := range []int64{-1, 0, 1} {
		code := hotp(secret, uint64(current+offset), 6)
		step, ok := findTOTPStep(secret, code, now)
		if !ok || step != current+offset {
			t.Fatalf("findTOTPStep(offset=%d) = (%d, %v)", offset, step, ok)
		}
	}
	for _, invalid := range []string{"12345", "1234567", "１２３４５６", "12A456"} {
		if _, ok := findTOTPStep(secret, invalid, now); ok {
			t.Fatalf("findTOTPStep(%q) accepted invalid input", invalid)
		}
	}
}

func TestMFASecretEncryptionUsesRandomNonceAndUserBinding(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	userID := uuid.New()
	secret := []byte("twenty-byte-secret!!")

	first, err := encryptMFASecret(key, userID, secret)
	if err != nil {
		t.Fatalf("encryptMFASecret() error = %v", err)
	}
	second, err := encryptMFASecret(key, userID, secret)
	if err != nil {
		t.Fatalf("encryptMFASecret() second error = %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("ciphertexts must differ because each enrollment uses a fresh nonce")
	}
	plaintext, err := decryptMFASecret(key, userID, first)
	if err != nil || !bytes.Equal(plaintext, secret) {
		t.Fatalf("decryptMFASecret() = %q, %v", plaintext, err)
	}
	if _, err := decryptMFASecret(key, uuid.New(), first); err == nil {
		t.Fatal("ciphertext was accepted for another user")
	}
	first[len(first)-1] ^= 1
	if _, err := decryptMFASecret(key, userID, first); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestRecoveryAlphabetProvidesFiveBitsPerCharacter(t *testing.T) {
	if len(recoveryAlphabet) != 32 {
		t.Fatalf("recovery alphabet length = %d, want 32", len(recoveryAlphabet))
	}
	for _, ambiguous := range "01IO" {
		if strings.ContainsRune(recoveryAlphabet, ambiguous) {
			t.Fatalf("recovery alphabet contains ambiguous character %q", ambiguous)
		}
	}
}
