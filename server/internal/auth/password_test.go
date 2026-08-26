package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordHashRoundTripAndUniqueSalt(t *testing.T) {
	params := DefaultArgon2Params()
	password := "正确的 horse battery staple 🔐"
	first, err := HashPassword(password, params)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	second, err := HashPassword(password, params)
	if err != nil {
		t.Fatalf("HashPassword() second error = %v", err)
	}
	if first == second {
		t.Fatal("two hashes use the same salt")
	}
	if !strings.HasPrefix(first, "$argon2id$v=19$m=65536,t=3,p=1$") {
		t.Fatalf("hash = %q, want PHC Argon2id parameters", first)
	}

	valid, err := VerifyPassword(first, password)
	if err != nil || !valid {
		t.Fatalf("VerifyPassword(correct) = %v, %v", valid, err)
	}
	valid, err = VerifyPassword(first, password+"wrong")
	if err != nil || valid {
		t.Fatalf("VerifyPassword(wrong) = %v, %v", valid, err)
	}
	if PasswordHashNeedsUpgrade(first, params) {
		t.Fatal("fresh hash unexpectedly needs upgrade")
	}
}

func TestPasswordValidationSupportsUnicodeAndBoundsWork(t *testing.T) {
	if err := ValidatePassword("中文口令可以包含足够多字符🔐"); err != nil {
		t.Fatalf("Unicode password rejected: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MinimumPasswordRunes)); err != nil {
		t.Fatalf("minimum-length password rejected: %v", err)
	}
	for _, value := range []string{strings.Repeat("a", MinimumPasswordRunes-1), strings.Repeat("a", MaximumPasswordRunes+1), strings.Repeat("界", 400)} {
		if err := ValidatePassword(value); !errors.Is(err, ErrInvalidPassword) {
			t.Fatalf("ValidatePassword(%d bytes) error = %v, want ErrInvalidPassword", len(value), err)
		}
	}
}

func TestPasswordHashParserRejectsUnsafeOrMalformedParameters(t *testing.T) {
	cases := []string{
		"not-a-phc-string",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=1,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=999999999,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for _, encoded := range cases {
		if _, err := VerifyPassword(encoded, "irrelevant-password"); !errors.Is(err, ErrInvalidHash) {
			t.Fatalf("VerifyPassword(%q) error = %v, want ErrInvalidHash", encoded, err)
		}
	}
}

func BenchmarkPasswordHash(b *testing.B) {
	params := DefaultArgon2Params()
	for range b.N {
		if _, err := HashPassword("benchmark-only-password", params); err != nil {
			b.Fatal(err)
		}
	}
}
