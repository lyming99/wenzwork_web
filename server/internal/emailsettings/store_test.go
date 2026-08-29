package emailsettings

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestPasswordCipherUsesRandomAuthenticatedEncryption(t *testing.T) {
	codec, err := newPasswordCipher(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	first, err := codec.encrypt([]byte("smtp-secret"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := codec.encrypt([]byte("smtp-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("SMTP password encryption reused a nonce")
	}
	plaintext, err := codec.decrypt(first)
	if err != nil || string(plaintext) != "smtp-secret" {
		t.Fatalf("decrypt() = %q, %v", plaintext, err)
	}
	first[len(first)-1] ^= 1
	if _, err := codec.decrypt(first); err == nil {
		t.Fatal("decrypt() accepted tampered ciphertext")
	}
}

func TestDatabaseConfigurationOverridesLocalFallback(t *testing.T) {
	codec, err := newPasswordCipher(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := codec.encrypt([]byte("database-secret"))
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{fallback: Config{
		Host: "local.example.test", Port: 1025, From: "local@example.test", Timeout: time.Second,
	}, cipher: codec}
	config, source, err := store.configFromRow(settingsRow{
		OverrideEnabled: true, SMTPHost: "database.example.test", SMTPPort: 587,
		SMTPUser: "mailer", SMTPPasswordCiphertext: ciphertext, MailFrom: "database@example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceDatabase || config.Host != "database.example.test" || config.Password != "database-secret" {
		t.Fatalf("resolved database config = source %q config %+v", source, config)
	}
}

func TestDeliveryFallsBackToLocalWhenDatabaseIsUnavailable(t *testing.T) {
	store := &Store{fallback: Config{
		Host: "local.example.test", Port: 1025, From: "local@example.test", Timeout: time.Second,
	}}
	config, err := store.resolveForDelivery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "local.example.test" {
		t.Fatalf("fallback config = %+v", config)
	}
}

func TestCandidateRequiresCompleteConfigurationButAllowsPasswordlessSMTP(t *testing.T) {
	candidate, err := candidateConfig(Config{}, "smtp.example.test", 25, "", nil, false, "noreply@example.test")
	if err != nil || candidate.Host != "smtp.example.test" {
		t.Fatalf("passwordless candidate = %+v, %v", candidate, err)
	}
	if _, err := candidateConfig(Config{}, "smtp.example.test", 587, "mailer", nil, false, "noreply@example.test"); err == nil {
		t.Fatal("candidateConfig() accepted an SMTP username without a password")
	}
}
