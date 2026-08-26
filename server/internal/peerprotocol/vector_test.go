package peerprotocol

import (
	"crypto/ecdh"
	"encoding/hex"
	"testing"
	"time"
)

func TestPeerCipherV1InteropVector(t *testing.T) {
	sourcePrivateBytes, targetPrivateBytes := make([]byte, 32), make([]byte, 32)
	for index := range sourcePrivateBytes {
		sourcePrivateBytes[index] = byte(index + 1)
		targetPrivateBytes[index] = byte(index + 33)
	}
	sourcePrivate, err := ecdh.X25519().NewPrivateKey(sourcePrivateBytes)
	if err != nil {
		t.Fatal(err)
	}
	targetPrivate, err := ecdh.X25519().NewPrivateKey(targetPrivateBytes)
	if err != nil {
		t.Fatal(err)
	}
	sharedSecret, err := X25519SharedSecret(sourcePrivate, targetPrivate.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := DeriveSessionKeys(sharedSecret, "ticket-vector-1", "session-vector-1", "source-vector-1", "target-vector-1")
	if err != nil {
		t.Fatal(err)
	}
	metadata := CiphertextMetadata{
		FrameType: "PEER_QUERY", SessionID: "session-vector-1", QueryID: "query-vector-1", Generation: 7,
		MessageSequence: 9, Deadline: time.UnixMilli(1786150923456).UTC(), Direction: DirectionSourceToTarget,
	}
	ciphertext, err := Seal(keys.SourceToTarget, []byte(`{"method":"ai.config.get"}`), metadata)
	if err != nil {
		t.Fatal(err)
	}
	for name, vector := range map[string]struct{ got, want string }{
		"sourcePublic":   {hex.EncodeToString(sourcePrivate.PublicKey().Bytes()), "07a37cbc142093c8b755dc1b10e86cb426374ad16aa853ed0bdfc0b2b86d1c7c"},
		"targetPublic":   {hex.EncodeToString(targetPrivate.PublicKey().Bytes()), "5869aff450549732cbaaed5e5df9b30a6da31cb0e5742bad5ad4a1a768f1a67b"},
		"sharedSecret":   {hex.EncodeToString(sharedSecret), "a84dc7c3c8f058b1b2dc4cd1e9b5dc0a7987f88b6a9564cde3391fc421159e77"},
		"sourceToTarget": {hex.EncodeToString(keys.SourceToTarget), "a7b20f163243e612a76f78e71bc70ebcf73d0f7e51a730d6b733a6b2fe74e7eb"},
		"targetToSource": {hex.EncodeToString(keys.TargetToSource), "532e00407a550d6bb098a12ba09df7ec08a4f34511921b3e91fdc917abfc99ad"},
		"aad":            {hex.EncodeToString(canonicalMetadata(metadata)), "0000001b77656e7a776f726b2d706565722d636970686572746578742d76310000000a504545525f51554552590000001073657373696f6e2d766563746f722d310000000e71756572792d766563746f722d31000000000000000700000000000000090000019fdee42cc000000001"},
		"ciphertext":     {hex.EncodeToString(ciphertext), "dd52161399e05e6083ba1edbd8e76176c9633f44ba6bab432e9a1a26398d0a6b641ac48c55ee869d2a59"},
	} {
		t.Run(name, func(t *testing.T) {
			if vector.got != vector.want {
				t.Fatalf("vector mismatch\n got: %s\nwant: %s", vector.got, vector.want)
			}
		})
	}
}
