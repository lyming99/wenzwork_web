// Package peerv2 implements the cryptographic invariants of remote/v2.
//
// It deliberately has no dependency on a WebSocket, Relay, or protobuf
// runtime.  This lets the Client, Device Agent and Relay-facing adapters use
// the same transcript, HKDF and AEAD rules without accidentally turning a
// Carrier packet sequence into a business-message nonce.
package peerv2

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	ProtocolMajor         = 2
	RootKeySize           = chacha20poly1305.KeySize
	X25519PublicKeySize   = 32
	Ed25519SignatureSize  = ed25519.SignatureSize
	MaximumPlaintextBytes = 1 << 20
)

var (
	ErrInvalidHandshake = errors.New("remote/v2 handshake is invalid")
	ErrInvalidMetadata  = errors.New("remote/v2 encrypted record metadata is invalid")
	ErrAuthentication   = errors.New("remote/v2 encrypted record authentication failed")
)

// Direction is intentionally separate from any generated protobuf enum so
// cryptographic code can be tested and used by all language adapters.
type Direction uint8

const (
	DirectionClientToDevice Direction = 1
	DirectionDeviceToClient Direction = 2
)

// FrameType is authenticated with every encrypted record. Values match
// api/remote/v2/common.proto and are append-only.
type FrameType uint8

const (
	FrameChannelOpen        FrameType = 1
	FrameChannelAccept      FrameType = 2
	FrameChannelClose       FrameType = 3
	FrameStreamOpen         FrameType = 4
	FrameStreamData         FrameType = 5
	FrameStreamAck          FrameType = 6
	FrameStreamClose        FrameType = 7
	FrameRPCRequest         FrameType = 8
	FrameRPCResponse        FrameType = 9
	FrameRPCEvent           FrameType = 10
	FrameFileManifest       FrameType = 11
	FrameFileChunk          FrameType = 12
	FrameFileAck            FrameType = 13
	FrameFileCommit         FrameType = 14
	FrameEventSubscribe     FrameType = 15
	FrameEventAck           FrameType = 16
	FrameEventResume        FrameType = 17
	FrameEventResetRequired FrameType = 18
	FrameRekeyInit          FrameType = 19
	FrameRekeyAck           FrameType = 20
	FrameRekeyCommit        FrameType = 21
	FrameLinkConfirm        FrameType = 22
	FrameLinkReady          FrameType = 23
	FrameLinkLeaseRenew     FrameType = 24
	FrameLinkLeaseRenewed   FrameType = 25
)

// HandshakeBinding is the signed, protocol-stable identity binding. The
// Client signs CanonicalInitTranscript; the Device signs
// CanonicalHandshakeTranscript after it contributes its ephemeral key.
type HandshakeBinding struct {
	GrantID               string
	LinkID                string
	ClientID              string
	DeviceID              string
	RelayNodeID           string
	RelayCellID           string
	TargetConnectionEpoch uint64
	ClientIdentityVersion uint64
	DeviceIdentityVersion uint64
	ClientEphemeralPublic []byte
	DeviceEphemeralPublic []byte
	ClientChallenge       []byte
	DeviceChallenge       []byte
	ExpiresAtUnixMilli    int64
}

// CanonicalInitTranscript returns the exact bytes the Client identity signs.
func CanonicalInitTranscript(binding HandshakeBinding) ([]byte, error) {
	if !validHandshakeBinding(binding, false) {
		return nil, ErrInvalidHandshake
	}
	return canonicalHandshake(binding, false), nil
}

// CanonicalHandshakeTranscript returns the complete, signed transcript used
// both as the HKDF salt and for confirmation MACs.
func CanonicalHandshakeTranscript(binding HandshakeBinding) ([]byte, error) {
	if !validHandshakeBinding(binding, true) {
		return nil, ErrInvalidHandshake
	}
	return canonicalHandshake(binding, true), nil
}

func SignLinkInit(privateKey ed25519.PrivateKey, binding HandshakeBinding) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrInvalidHandshake
	}
	transcript, err := CanonicalInitTranscript(binding)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, transcript), nil
}

func VerifyLinkInit(publicKey ed25519.PublicKey, binding HandshakeBinding, signature []byte) error {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != Ed25519SignatureSize {
		return ErrInvalidHandshake
	}
	transcript, err := CanonicalInitTranscript(binding)
	if err != nil || !ed25519.Verify(publicKey, transcript, signature) {
		return ErrInvalidHandshake
	}
	return nil
}

func SignLinkAccept(privateKey ed25519.PrivateKey, binding HandshakeBinding) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrInvalidHandshake
	}
	transcript, err := CanonicalHandshakeTranscript(binding)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, transcript), nil
}

func VerifyLinkAccept(publicKey ed25519.PublicKey, binding HandshakeBinding, signature []byte) error {
	if len(publicKey) != ed25519.PublicKeySize || len(signature) != Ed25519SignatureSize {
		return ErrInvalidHandshake
	}
	transcript, err := CanonicalHandshakeTranscript(binding)
	if err != nil || !ed25519.Verify(publicKey, transcript, signature) {
		return ErrInvalidHandshake
	}
	return nil
}

func TranscriptHash(binding HandshakeBinding) ([sha256.Size]byte, error) {
	transcript, err := CanonicalHandshakeTranscript(binding)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(transcript), nil
}

// DeriveRootKey creates root_key[key_id=1] from X25519 output. The supplied
// transcript is full and signed, never a caller-selected partial transcript.
func DeriveRootKey(sharedSecret []byte, binding HandshakeBinding) ([]byte, error) {
	if len(sharedSecret) != X25519PublicKeySize {
		return nil, ErrInvalidHandshake
	}
	transcript, err := CanonicalHandshakeTranscript(binding)
	if err != nil {
		return nil, err
	}
	salt := sha256.Sum256(transcript)
	return hkdfBytes(sharedSecret, salt[:], appendFields(nil, "wenzwork-remote-v2/root", binding.LinkID, "1"), RootKeySize)
}

// LinkConfirmationMAC provides the encrypted LINK_CONFIRM proof after both
// identities have signed the complete transcript. It is deliberately derived
// from root_key[1], not a reusable stream key, and binds the canonical
// transcript hash so neither side can confuse confirmation from another Link.
func LinkConfirmationMAC(rootKey []byte, binding HandshakeBinding) ([]byte, error) {
	if len(rootKey) != RootKeySize {
		return nil, ErrInvalidHandshake
	}
	transcript, err := CanonicalHandshakeTranscript(binding)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(transcript)
	mac := hmac.New(sha256.New, rootKey)
	_, _ = mac.Write([]byte("wenzwork-remote-v2/link-confirm"))
	_, _ = mac.Write(digest[:])
	return mac.Sum(nil), nil
}

func VerifyLinkConfirmationMAC(rootKey []byte, binding HandshakeBinding, value []byte) error {
	expected, err := LinkConfirmationMAC(rootKey, binding)
	if err != nil || len(value) != len(expected) || !hmac.Equal(expected, value) {
		return ErrAuthentication
	}
	return nil
}

// DeriveControlKey derives the directional control key used only for Link
// control frames such as rekey. It is never reused for Stream payloads.
func DeriveControlKey(rootKey []byte, linkID string, keyID uint64, direction Direction) ([]byte, error) {
	return deriveKey(rootKey, linkID, keyID, direction, "control", "", "")
}

func DeriveChannelKey(rootKey []byte, linkID string, keyID uint64, direction Direction, channelID string) ([]byte, error) {
	return deriveKey(rootKey, linkID, keyID, direction, "channel", channelID, "")
}

func DeriveStreamKey(rootKey []byte, linkID string, keyID uint64, direction Direction, channelID, streamID string) ([]byte, error) {
	return deriveKey(rootKey, linkID, keyID, direction, "stream", channelID, streamID)
}

// RecordMetadata is included in both AEAD associated data and deterministic
// nonce construction. packet_sequence is deliberately absent.
type RecordMetadata struct {
	LinkID         string
	ChannelID      string
	StreamID       string
	KeyID          uint64
	Direction      Direction
	FrameType      FrameType
	StreamSequence uint64
}

func Seal(streamKey, plaintext []byte, metadata RecordMetadata) ([]byte, error) {
	if len(plaintext) == 0 || len(plaintext) > MaximumPlaintextBytes {
		return nil, ErrInvalidMetadata
	}
	aead, nonce, aad, err := recordInputs(streamKey, metadata)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

func Open(streamKey, ciphertext []byte, metadata RecordMetadata) ([]byte, error) {
	if len(ciphertext) <= chacha20poly1305.Overhead || len(ciphertext) > MaximumPlaintextBytes+chacha20poly1305.Overhead {
		return nil, ErrInvalidMetadata
	}
	aead, nonce, aad, err := recordInputs(streamKey, metadata)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil || len(plaintext) == 0 || len(plaintext) > MaximumPlaintextBytes {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

func AssociatedData(metadata RecordMetadata) ([]byte, error) {
	if !validRecordMetadata(metadata) {
		return nil, ErrInvalidMetadata
	}
	return canonicalRecordMetadata(metadata), nil
}

func Nonce(streamKey []byte, metadata RecordMetadata) ([]byte, error) {
	if len(streamKey) != chacha20poly1305.KeySize || !validRecordMetadata(metadata) {
		return nil, ErrInvalidMetadata
	}
	// The last eight bytes are the stream sequence, making reuse impossible
	// for a valid per-(key,direction,stream) sequence. The derived prefix keeps
	// nonces domain-separated between Stream keys.
	nonceKey, err := hkdfBytes(streamKey, nil, []byte("wenzwork-remote-v2/nonce-key"), sha256.Size)
	if err != nil {
		return nil, err
	}
	withoutSequence := metadata
	withoutSequence.StreamSequence = 0
	mac := hmac.New(sha256.New, nonceKey)
	_, _ = mac.Write(canonicalRecordMetadata(withoutSequence))
	digest := mac.Sum(nil)
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	copy(nonce[:16], digest[:16])
	binary.BigEndian.PutUint64(nonce[16:], metadata.StreamSequence)
	wipe(nonceKey)
	return nonce, nil
}

func recordInputs(streamKey []byte, metadata RecordMetadata) (ciphertextAEAD, []byte, []byte, error) {
	if len(streamKey) != chacha20poly1305.KeySize || !validRecordMetadata(metadata) {
		return nil, nil, nil, ErrInvalidMetadata
	}
	aead, err := chacha20poly1305.NewX(streamKey)
	if err != nil {
		return nil, nil, nil, ErrInvalidMetadata
	}
	nonce, err := Nonce(streamKey, metadata)
	if err != nil {
		return nil, nil, nil, err
	}
	return aead, nonce, canonicalRecordMetadata(metadata), nil
}

// ciphertextAEAD is a tiny local interface to keep the public API free of an
// implementation-specific cipher import.
type ciphertextAEAD interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

func deriveKey(rootKey []byte, linkID string, keyID uint64, direction Direction, layer, channelID, streamID string) ([]byte, error) {
	if len(rootKey) != RootKeySize || !validField(linkID) || keyID == 0 || !validDirection(direction) ||
		(layer != "control" && layer != "channel" && layer != "stream") ||
		(layer == "channel" && !validField(channelID)) ||
		(layer == "stream" && (!validField(channelID) || !validField(streamID))) {
		return nil, ErrInvalidHandshake
	}
	info := appendFields(nil, "wenzwork-remote-v2/key", layer, linkID, uint64Text(keyID), uint64Text(uint64(direction)), channelID, streamID)
	return hkdfBytes(rootKey, nil, info, RootKeySize)
}

func hkdfBytes(secret, salt, info []byte, size int) ([]byte, error) {
	if len(secret) == 0 || size <= 0 || size > 4096 {
		return nil, ErrInvalidHandshake
	}
	result := make([]byte, size)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, info), result); err != nil {
		return nil, fmt.Errorf("derive remote/v2 key: %w", err)
	}
	return result, nil
}

func validHandshakeBinding(binding HandshakeBinding, complete bool) bool {
	if !validField(binding.GrantID) || !validField(binding.LinkID) || !validField(binding.ClientID) || !validField(binding.DeviceID) ||
		binding.ClientID == binding.DeviceID || !validField(binding.RelayNodeID) || !validField(binding.RelayCellID) ||
		binding.TargetConnectionEpoch == 0 || binding.ClientIdentityVersion == 0 || binding.ExpiresAtUnixMilli <= 0 ||
		len(binding.ClientEphemeralPublic) != X25519PublicKeySize || len(binding.ClientChallenge) != 32 {
		return false
	}
	if !complete {
		return len(binding.DeviceEphemeralPublic) == 0 && len(binding.DeviceChallenge) == 0 && binding.DeviceIdentityVersion == 0
	}
	return binding.DeviceIdentityVersion > 0 && len(binding.DeviceEphemeralPublic) == X25519PublicKeySize && len(binding.DeviceChallenge) == 32
}

func validRecordMetadata(metadata RecordMetadata) bool {
	return validField(metadata.LinkID) && validField(metadata.ChannelID) && validField(metadata.StreamID) &&
		metadata.KeyID > 0 && metadata.StreamSequence > 0 && validDirection(metadata.Direction) && metadata.FrameType >= FrameChannelOpen && metadata.FrameType <= FrameLinkLeaseRenewed
}

func validDirection(direction Direction) bool {
	return direction == DirectionClientToDevice || direction == DirectionDeviceToClient
}

func canonicalHandshake(binding HandshakeBinding, complete bool) []byte {
	values := []string{
		"wenzwork-remote-v2/handshake", uint64Text(ProtocolMajor), binding.GrantID, binding.LinkID, binding.ClientID,
		binding.DeviceID, binding.RelayNodeID, binding.RelayCellID, uint64Text(binding.TargetConnectionEpoch),
		uint64Text(binding.ClientIdentityVersion), uint64Text(binding.DeviceIdentityVersion),
	}
	encoded := appendFields(nil, values...)
	encoded = appendBytes(encoded, binding.ClientEphemeralPublic)
	encoded = appendBytes(encoded, binding.DeviceEphemeralPublic)
	encoded = appendBytes(encoded, binding.ClientChallenge)
	encoded = appendBytes(encoded, binding.DeviceChallenge)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(binding.ExpiresAtUnixMilli))
	encoded = appendBytes(encoded, []byte{boolByte(complete)})
	return encoded
}

func canonicalRecordMetadata(metadata RecordMetadata) []byte {
	encoded := appendFields(nil,
		"wenzwork-remote-v2/record", metadata.LinkID, metadata.ChannelID, metadata.StreamID,
		uint64Text(metadata.KeyID), uint64Text(uint64(metadata.Direction)), uint64Text(uint64(metadata.FrameType)),
	)
	return binary.BigEndian.AppendUint64(encoded, metadata.StreamSequence)
}

func appendFields(destination []byte, values ...string) []byte {
	for _, value := range values {
		destination = appendBytes(destination, []byte(value))
	}
	return destination
}

func appendBytes(destination, value []byte) []byte {
	destination = binary.BigEndian.AppendUint32(destination, uint32(len(value)))
	return append(destination, value...)
}

func uint64Text(value uint64) string {
	return fmt.Sprintf("%d", value)
}

func boolByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}

func validField(value string) bool {
	return value == strings.TrimSpace(value) && value != "" && len(value) <= 256 && !strings.ContainsRune(value, '\x00')
}

func wipe(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
