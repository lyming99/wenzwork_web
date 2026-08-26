package peerprotocol

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	EphemeralPublicKeySize = 32
	MaximumPlaintextBytes  = 60 << 10
	CipherSuiteName        = "X25519-HKDF-SHA256-XCHACHA20-POLY1305"
)

var (
	ErrInvalidHandshake = errors.New("invalid Peer key agreement")
	ErrInvalidMetadata  = errors.New("invalid Peer ciphertext metadata")
	ErrAuthentication   = errors.New("Peer ciphertext authentication failed")
	ErrSequence         = errors.New("Peer ciphertext sequence is replayed or out of order")
)

type SessionKeys struct {
	SourceToTarget []byte
	TargetToSource []byte
}

type Direction uint32

const (
	DirectionSourceToTarget Direction = 1
	DirectionTargetToSource Direction = 2
)

type CiphertextMetadata struct {
	FrameType       string
	SessionID       string
	QueryID         string
	Generation      uint64
	MessageSequence uint64
	Deadline        time.Time
	Direction       Direction
}

type CipherMode uint8

const (
	CipherModeSeal CipherMode = 1
	CipherModeOpen CipherMode = 2
)

// CipherState makes the nonce uniqueness and replay invariant explicit. One
// instance is used for exactly one direction and one operation (seal or open).
// Sequence numbers start at one and must be contiguous within a generation.
type CipherState struct {
	mu         sync.Mutex
	key        []byte
	direction  Direction
	mode       CipherMode
	generation uint64
	next       uint64
	exhausted  bool
}

func NewCipherState(key []byte, direction Direction, mode CipherMode, generation uint64) (*CipherState, error) {
	if len(key) != chacha20poly1305.KeySize ||
		(direction != DirectionSourceToTarget && direction != DirectionTargetToSource) ||
		(mode != CipherModeSeal && mode != CipherModeOpen) || generation == 0 {
		return nil, ErrInvalidMetadata
	}
	return &CipherState{key: append([]byte(nil), key...), direction: direction, mode: mode, generation: generation, next: 1}, nil
}

func (state *CipherState) SealNext(plaintext []byte, metadata CiphertextMetadata) ([]byte, error) {
	if state == nil {
		return nil, ErrInvalidMetadata
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.mode != CipherModeSeal || !state.accepts(metadata) {
		return nil, ErrSequence
	}
	ciphertext, err := Seal(state.key, plaintext, metadata)
	if err != nil {
		return nil, err
	}
	state.advance()
	return ciphertext, nil
}

func (state *CipherState) OpenNext(ciphertext []byte, metadata CiphertextMetadata) ([]byte, error) {
	if state == nil {
		return nil, ErrInvalidMetadata
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.mode != CipherModeOpen || !state.accepts(metadata) {
		return nil, ErrSequence
	}
	plaintext, err := Open(state.key, ciphertext, metadata)
	if err != nil {
		return nil, err
	}
	state.advance()
	return plaintext, nil
}

func (state *CipherState) BeginGeneration(generation uint64) error {
	if state == nil || generation == 0 {
		return ErrInvalidMetadata
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if generation <= state.generation {
		return ErrSequence
	}
	state.generation, state.next, state.exhausted = generation, 1, false
	return nil
}

func (state *CipherState) NextSequence() (generation, sequence uint64, exhausted bool) {
	if state == nil {
		return 0, 0, true
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.generation, state.next, state.exhausted
}

func (state *CipherState) accepts(metadata CiphertextMetadata) bool {
	return !state.exhausted && metadata.Direction == state.direction && metadata.Generation == state.generation &&
		metadata.MessageSequence == state.next
}

func (state *CipherState) advance() {
	if state.next == math.MaxUint64 {
		state.exhausted = true
		return
	}
	state.next++
}

func X25519SharedSecret(privateKey *ecdh.PrivateKey, peerPublicKey []byte) ([]byte, error) {
	if privateKey == nil || len(peerPublicKey) != EphemeralPublicKeySize {
		return nil, ErrInvalidHandshake
	}
	publicKey, err := ecdh.X25519().NewPublicKey(peerPublicKey)
	if err != nil {
		return nil, ErrInvalidHandshake
	}
	secret, err := privateKey.ECDH(publicKey)
	if err != nil {
		return nil, ErrInvalidHandshake
	}
	return secret, nil
}

func DeriveSessionKeys(sharedSecret []byte, ticketJWTID, sessionID, sourceDeviceID, targetDeviceID string) (SessionKeys, error) {
	if len(sharedSecret) != 32 || !validField(ticketJWTID) || !validField(sessionID) || !validField(sourceDeviceID) ||
		!validField(targetDeviceID) || sourceDeviceID == targetDeviceID {
		return SessionKeys{}, ErrInvalidHandshake
	}
	saltInput := make([]byte, 0, 256)
	for _, field := range []string{"wenzwork-peer-salt-v1", ticketJWTID, sessionID, sourceDeviceID, targetDeviceID} {
		saltInput = appendField(saltInput, []byte(field))
	}
	salt := sha256.Sum256(saltInput)
	derive := func(label string) ([]byte, error) {
		info := make([]byte, 0, 160)
		for _, field := range []string{label, sessionID, sourceDeviceID, targetDeviceID} {
			info = appendField(info, []byte(field))
		}
		key := make([]byte, chacha20poly1305.KeySize)
		if _, err := io.ReadFull(hkdf.New(sha256.New, sharedSecret, salt[:], info), key); err != nil {
			return nil, fmt.Errorf("derive Peer key: %w", err)
		}
		return key, nil
	}
	sourceToTarget, err := derive("wenzwork-peer-source-to-target-v1")
	if err != nil {
		return SessionKeys{}, err
	}
	targetToSource, err := derive("wenzwork-peer-target-to-source-v1")
	if err != nil {
		return SessionKeys{}, err
	}
	return SessionKeys{SourceToTarget: sourceToTarget, TargetToSource: targetToSource}, nil
}

func Seal(key, plaintext []byte, metadata CiphertextMetadata) ([]byte, error) {
	if len(plaintext) == 0 || len(plaintext) > MaximumPlaintextBytes {
		return nil, ErrInvalidMetadata
	}
	aead, nonce, aad, err := cipherInputs(key, metadata)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

func Open(key, ciphertext []byte, metadata CiphertextMetadata) ([]byte, error) {
	if len(ciphertext) <= chacha20poly1305.Overhead || len(ciphertext) > MaximumPlaintextBytes+chacha20poly1305.Overhead {
		return nil, ErrInvalidMetadata
	}
	aead, nonce, aad, err := cipherInputs(key, metadata)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil || len(plaintext) == 0 || len(plaintext) > MaximumPlaintextBytes {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

func cipherInputs(key []byte, metadata CiphertextMetadata) (cipher.AEAD, []byte, []byte, error) {
	if len(key) != chacha20poly1305.KeySize || !validMetadata(metadata) {
		return nil, nil, nil, ErrInvalidMetadata
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, nil, nil, ErrInvalidMetadata
	}
	aad := canonicalMetadata(metadata)
	nonceDigest := sha256.Sum256(appendField(append([]byte(nil), aad...), []byte("nonce")))
	nonce := append([]byte(nil), nonceDigest[:chacha20poly1305.NonceSizeX]...)
	return aead, nonce, aad, nil
}

func validMetadata(metadata CiphertextMetadata) bool {
	validFrame := metadata.FrameType == "PEER_QUERY" || metadata.FrameType == "PEER_DELTA" ||
		metadata.FrameType == "PEER_COMPLETE" || metadata.FrameType == "PEER_CANCEL"
	if !validFrame || !validField(metadata.SessionID) || !validField(metadata.QueryID) || metadata.Generation == 0 ||
		metadata.MessageSequence == 0 || (metadata.Direction != DirectionSourceToTarget && metadata.Direction != DirectionTargetToSource) {
		return false
	}
	if metadata.FrameType == "PEER_QUERY" {
		return !metadata.Deadline.IsZero()
	}
	return metadata.Deadline.IsZero()
}

func canonicalMetadata(metadata CiphertextMetadata) []byte {
	encoded := make([]byte, 0, 192)
	encoded = appendField(encoded, []byte("wenzwork-peer-ciphertext-v1"))
	encoded = appendField(encoded, []byte(metadata.FrameType))
	encoded = appendField(encoded, []byte(metadata.SessionID))
	encoded = appendField(encoded, []byte(metadata.QueryID))
	encoded = binary.BigEndian.AppendUint64(encoded, metadata.Generation)
	encoded = binary.BigEndian.AppendUint64(encoded, metadata.MessageSequence)
	deadlineMillis := int64(0)
	if !metadata.Deadline.IsZero() {
		deadlineMillis = metadata.Deadline.UTC().UnixMilli()
	}
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(deadlineMillis))
	encoded = binary.BigEndian.AppendUint32(encoded, uint32(metadata.Direction))
	return encoded
}

func validField(value string) bool {
	return value != "" && len(value) <= 128 && !strings.ContainsRune(value, '\x00')
}

func appendField(destination, field []byte) []byte {
	destination = binary.BigEndian.AppendUint32(destination, uint32(len(field)))
	return append(destination, field...)
}
