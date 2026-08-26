package relaymanagement

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestAccessKeyDigestValidatesFormatAndHashesPlaintext(t *testing.T) {
	plaintext := accessKeyPrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	wantBytes := sha256.Sum256([]byte(plaintext))
	want := hex.EncodeToString(wantBytes[:])
	if got, ok := accessKeyDigest(plaintext); !ok || got != want {
		t.Fatalf("accessKeyDigest(valid) = %q, %t, want %q, true", got, ok, want)
	}

	for _, invalid := range []string{
		"", "relay_short", "other_" + strings.Repeat("a", 43),
		"relay_" + strings.Repeat("!", 43), "relay_" + strings.Repeat("a", 44),
	} {
		if _, ok := accessKeyDigest(invalid); ok {
			t.Fatalf("accessKeyDigest accepted %q", invalid)
		}
	}
}

func TestListenerAddressUsesConfiguredPort(t *testing.T) {
	if got := listenerAddress(18443); got != ":18443" {
		t.Fatalf("listenerAddress(18443) = %q, want :18443", got)
	}
	for _, port := range []int{0, -1, 65_536} {
		if validListenerPort(port) {
			t.Fatalf("validListenerPort(%d) = true", port)
		}
	}
}

func TestAgentRuntimeKeepsWSSURLIndependentFromPlaintextListener(t *testing.T) {
	store := &Store{agentConfig: AgentRuntimeConfiguration{ListenAddress: ":8443"}}
	configuration, err := store.agentRuntimeConfiguration(
		installationRow{
			PublicEndpoint: "wss://relay.example.test/v2/connect",
			ListenerPort:   18443,
		},
		cellRow{ConnectionHardLimit: 2_000},
	)
	if err != nil {
		t.Fatalf("agentRuntimeConfiguration() error = %v", err)
	}
	if configuration.PublicEndpoint != "wss://relay.example.test/v2/connect" || configuration.ListenAddress != ":18443" {
		t.Fatalf("agentRuntimeConfiguration() = %+v", configuration)
	}
}
