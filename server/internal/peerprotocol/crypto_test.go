package peerprotocol

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

func TestPeerCiphertextUsesAuthenticatedDirectionalSessionKeys(t *testing.T) {
	source, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	target, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sourceSecret, err := X25519SharedSecret(source, target.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	targetSecret, err := X25519SharedSecret(target, source.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sourceKeys, err := DeriveSessionKeys(sourceSecret, "ticket-1", "session-1", "source-1", "target-1")
	if err != nil {
		t.Fatal(err)
	}
	targetKeys, err := DeriveSessionKeys(targetSecret, "ticket-1", "session-1", "source-1", "target-1")
	if err != nil {
		t.Fatal(err)
	}
	metadata := CiphertextMetadata{
		FrameType: "PEER_QUERY", SessionID: "session-1", QueryID: "query-1", Generation: 1,
		MessageSequence: 1, Deadline: time.Date(2026, 8, 7, 12, 1, 0, 0, time.UTC), Direction: DirectionSourceToTarget,
	}
	ciphertext, err := Seal(sourceKeys.SourceToTarget, []byte("payload stays off the Relay"), metadata)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := Open(targetKeys.SourceToTarget, ciphertext, metadata)
	if err != nil || string(plaintext) != "payload stays off the Relay" {
		t.Fatalf("Open() = %q, %v", plaintext, err)
	}
	tampered := metadata
	tampered.MessageSequence++
	if _, err := Open(targetKeys.SourceToTarget, ciphertext, tampered); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered metadata error = %v", err)
	}
	if _, err := Open(targetKeys.TargetToSource, ciphertext, metadata); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong direction key error = %v", err)
	}
}

func TestCipherStateRejectsReplayGapWrongDirectionAndOldGeneration(t *testing.T) {
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	sealer, err := NewCipherState(key, DirectionSourceToTarget, CipherModeSeal, 4)
	if err != nil {
		t.Fatal(err)
	}
	opener, err := NewCipherState(key, DirectionSourceToTarget, CipherModeOpen, 4)
	if err != nil {
		t.Fatal(err)
	}
	metadata := CiphertextMetadata{
		FrameType: "PEER_QUERY", SessionID: "session", QueryID: "query", Generation: 4, MessageSequence: 1,
		Deadline: time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC), Direction: DirectionSourceToTarget,
	}
	ciphertext, err := sealer.SealNext([]byte("request"), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext, err := opener.OpenNext(ciphertext, metadata); err != nil || string(plaintext) != "request" {
		t.Fatalf("OpenNext() = %q, %v", plaintext, err)
	}
	if _, err := opener.OpenNext(ciphertext, metadata); !errors.Is(err, ErrSequence) {
		t.Fatalf("replay error = %v", err)
	}
	gap := metadata
	gap.MessageSequence = 3
	if _, err := sealer.SealNext([]byte("gap"), gap); !errors.Is(err, ErrSequence) {
		t.Fatalf("gap error = %v", err)
	}
	wrongDirection := metadata
	wrongDirection.MessageSequence = 2
	wrongDirection.Direction = DirectionTargetToSource
	if _, err := sealer.SealNext([]byte("wrong"), wrongDirection); !errors.Is(err, ErrSequence) {
		t.Fatalf("direction error = %v", err)
	}
	if err := sealer.BeginGeneration(5); err != nil {
		t.Fatal(err)
	}
	if _, err := sealer.SealNext([]byte("old"), metadata); !errors.Is(err, ErrSequence) {
		t.Fatalf("old generation error = %v", err)
	}
	if err := sealer.BeginGeneration(5); !errors.Is(err, ErrSequence) {
		t.Fatalf("generation replay error = %v", err)
	}
}
