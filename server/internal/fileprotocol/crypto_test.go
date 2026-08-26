package fileprotocol

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

type fileVector struct {
	ProtocolVersion           uint32             `json:"protocolVersion"`
	TicketJWTID               string             `json:"ticketJti"`
	TransferID                string             `json:"transferId"`
	Generation                uint64             `json:"generation"`
	SourceDeviceID            string             `json:"sourceDeviceId"`
	TargetDeviceID            string             `json:"targetDeviceId"`
	SourceIdentitySeed        string             `json:"sourceIdentitySeed"`
	TargetIdentitySeed        string             `json:"targetIdentitySeed"`
	SourceEphemeralPrivateKey string             `json:"sourceEphemeralPrivateKey"`
	TargetEphemeralPrivateKey string             `json:"targetEphemeralPrivateKey"`
	DeclaredTotalBytes        uint64             `json:"declaredTotalBytes"`
	DeclaredFileCount         uint32             `json:"declaredFileCount"`
	ChunkSize                 uint32             `json:"chunkSize"`
	ReceiveWindow             uint32             `json:"receiveWindow"`
	FileID                    string             `json:"fileId"`
	NoncePrefix               string             `json:"noncePrefix"`
	ChunkIndex                uint64             `json:"chunkIndex"`
	Plaintext                 string             `json:"plaintext"`
	ManifestPlaintext         string             `json:"manifestPlaintext"`
	Expected                  fileVectorExpected `json:"expected"`
}

type fileVectorExpected struct {
	SourceEphemeralPublicKey string `json:"sourceEphemeralPublicKey"`
	TargetEphemeralPublicKey string `json:"targetEphemeralPublicKey"`
	OpenSignature            string `json:"openSignature"`
	AcceptSignature          string `json:"acceptSignature"`
	TranscriptHash           string `json:"transcriptHash"`
	SharedSecret             string `json:"sharedSecret"`
	ManifestHash             string `json:"manifestHash"`
	ManifestKey              string `json:"manifestKey"`
	FileMasterKey            string `json:"fileMasterKey"`
	ControlAtoB              string `json:"controlAtoB"`
	ControlBtoA              string `json:"controlBtoA"`
	FileKey                  string `json:"fileKey"`
	Nonce                    string `json:"nonce"`
	Ciphertext               string `json:"ciphertext"`
}

func TestFileV1FixedVector(t *testing.T) {
	contents, err := os.ReadFile("testdata/file_v1_vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector fileVector
	if err := json.Unmarshal(contents, &vector); err != nil {
		t.Fatal(err)
	}
	sourceIdentitySeed := mustHex(t, vector.SourceIdentitySeed)
	targetIdentitySeed := mustHex(t, vector.TargetIdentitySeed)
	sourceIdentityPrivate := ed25519.NewKeyFromSeed(sourceIdentitySeed)
	targetIdentityPrivate := ed25519.NewKeyFromSeed(targetIdentitySeed)
	sourceEphemeralPrivate, err := ecdh.X25519().NewPrivateKey(mustHex(t, vector.SourceEphemeralPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	targetEphemeralPrivate, err := ecdh.X25519().NewPrivateKey(mustHex(t, vector.TargetEphemeralPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	open := OpenTranscript{
		TicketJWTID: vector.TicketJWTID, TransferID: vector.TransferID, Generation: vector.Generation,
		SourceDeviceID: vector.SourceDeviceID, TargetDeviceID: vector.TargetDeviceID,
		SourceEphemeralPublicKey: sourceEphemeralPrivate.PublicKey().Bytes(),
		DeclaredTotalBytes:       vector.DeclaredTotalBytes, DeclaredFileCount: vector.DeclaredFileCount,
	}
	openSignature, err := SignOpen(sourceIdentityPrivate, open)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyOpen(sourceIdentityPrivate.Public().(ed25519.PublicKey), open, openSignature); err != nil {
		t.Fatal(err)
	}
	accept := AcceptTranscript{
		TargetEphemeralPublicKey: targetEphemeralPrivate.PublicKey().Bytes(), CipherSuite: XChaCha20Poly1305,
		ChunkSize: vector.ChunkSize, ReceiveWindow: vector.ReceiveWindow,
	}
	acceptSignature, err := SignAccept(targetIdentityPrivate, open, openSignature, accept)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAccept(targetIdentityPrivate.Public().(ed25519.PublicKey), open, openSignature, accept, acceptSignature); err != nil {
		t.Fatal(err)
	}
	handshake := Handshake{Open: open, OpenSignature: openSignature, Accept: accept, AcceptSignature: acceptSignature}
	transcriptHash, err := handshake.TranscriptHash()
	if err != nil {
		t.Fatal(err)
	}
	sourceSecret, err := X25519SharedSecret(sourceEphemeralPrivate, accept.TargetEphemeralPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	targetSecret, err := X25519SharedSecret(targetEphemeralPrivate, open.SourceEphemeralPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceSecret, targetSecret) {
		t.Fatal("X25519 shared secrets differ")
	}
	keys, err := DeriveSessionKeys(sourceSecret, vector.TicketJWTID, vector.TransferID, vector.Generation, transcriptHash)
	if err != nil {
		t.Fatal(err)
	}
	fileID := mustHex(t, vector.FileID)
	fileKey, err := DeriveFileKey(keys.FileMasterKey, fileID)
	if err != nil {
		t.Fatal(err)
	}
	manifestHash := sha256.Sum256([]byte(vector.ManifestPlaintext))
	metadata := ChunkMetadata{
		ProtocolVersion: vector.ProtocolVersion, TransferID: vector.TransferID, Generation: vector.Generation,
		FileID: fileID, ChunkIndex: vector.ChunkIndex, Offset: 0, PlaintextSize: uint32(len(vector.Plaintext)),
		TotalFileSize: uint64(len(vector.Plaintext)), Direction: DirectionSenderToReceiver, ManifestHash: manifestHash[:],
	}
	noncePrefix := mustHex(t, vector.NoncePrefix)
	ciphertext, err := SealChunk(fileKey, noncePrefix, []byte(vector.Plaintext), metadata)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := OpenChunk(fileKey, noncePrefix, ciphertext, metadata)
	if err != nil || string(plaintext) != vector.Plaintext {
		t.Fatalf("OpenChunk() = %q, %v", plaintext, err)
	}
	nonce := append(append([]byte(nil), noncePrefix...), make([]byte, 8)...)
	actual := fileVectorExpected{
		SourceEphemeralPublicKey: hex.EncodeToString(open.SourceEphemeralPublicKey),
		TargetEphemeralPublicKey: hex.EncodeToString(accept.TargetEphemeralPublicKey),
		OpenSignature:            hex.EncodeToString(openSignature), AcceptSignature: hex.EncodeToString(acceptSignature),
		TranscriptHash: hex.EncodeToString(transcriptHash), SharedSecret: hex.EncodeToString(sourceSecret),
		ManifestHash: hex.EncodeToString(manifestHash[:]), ManifestKey: hex.EncodeToString(keys.ManifestKey),
		FileMasterKey: hex.EncodeToString(keys.FileMasterKey), ControlAtoB: hex.EncodeToString(keys.ControlAtoB),
		ControlBtoA: hex.EncodeToString(keys.ControlBtoA), FileKey: hex.EncodeToString(fileKey),
		Nonce: hex.EncodeToString(nonce), Ciphertext: hex.EncodeToString(ciphertext),
	}
	if actual != vector.Expected {
		formatted, _ := json.MarshalIndent(actual, "", "  ")
		t.Fatalf("fixed vector mismatch; replace expected with:\n%s", formatted)
	}

	tampered := metadata
	tampered.ManifestHash = append([]byte(nil), metadata.ManifestHash...)
	tampered.ManifestHash[0] ^= 0xff
	if _, err := OpenChunk(fileKey, noncePrefix, ciphertext, tampered); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("tampered AAD error = %v", err)
	}
}

func TestResumeStateOnlyReturnsMissingDurableChunks(t *testing.T) {
	state := NewResumeState(1)
	for _, index := range []uint64{0, 1, 4, 5, 9} {
		if err := state.MarkDurable(1, index); err != nil {
			t.Fatal(err)
		}
	}
	missing, more, err := state.Missing(1, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	expected := []ChunkRange{{Start: 2, End: 4}, {Start: 6, End: 9}}
	if more || !equalRanges(missing, expected) {
		t.Fatalf("Missing() = %+v, more=%v", missing, more)
	}
	if err := state.FenceGeneration(2); err != nil {
		t.Fatal(err)
	}
	if err := state.MarkDurable(1, 2); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("old generation error = %v", err)
	}
	missing, _, err = state.Missing(2, 10, 10)
	if err != nil || !equalRanges(missing, expected) {
		t.Fatalf("resumed Missing() = %+v, %v", missing, err)
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func equalRanges(left, right []ChunkRange) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
