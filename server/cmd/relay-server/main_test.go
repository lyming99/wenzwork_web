package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestTicketVerificationKeySetsAreCryptographicallyIndependent(t *testing.T) {
	connectionPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerPublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateIndependentTicketPublicKeys(
		map[string]ed25519.PublicKey{"connection-key": connectionPublicKey},
		map[string]ed25519.PublicKey{"peer-key": peerPublicKey},
	); err != nil {
		t.Fatalf("independent key sets rejected: %v", err)
	}
	if err := validateIndependentTicketPublicKeys(
		map[string]ed25519.PublicKey{"connection-key": connectionPublicKey},
		map[string]ed25519.PublicKey{"peer-key": connectionPublicKey},
	); err == nil {
		t.Fatal("identical key material was accepted across Ticket purposes")
	}
	if err := validateIndependentTicketPublicKeys(
		map[string]ed25519.PublicKey{"shared-key-id": connectionPublicKey},
		map[string]ed25519.PublicKey{"shared-key-id": peerPublicKey},
	); err == nil {
		t.Fatal("a shared Key ID was accepted across Ticket purposes")
	}
}

func TestEffectiveRelayVersionUsesReleaseBuildInjection(t *testing.T) {
	previous := version
	version = "1.2.3"
	t.Cleanup(func() { version = previous })
	if got := effectiveRelayVersion("0.0.0"); got != "1.2.3" {
		t.Fatalf("effectiveRelayVersion() = %q", got)
	}
	if got := effectiveRelayVersion("private-build"); got != "private-build" {
		t.Fatalf("explicit version was replaced with %q", got)
	}
}
