package fileprotocol

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	ProtocolVersion        uint32 = 1
	XChaCha20Poly1305      uint32 = 1
	DefaultChunkSize              = 64 << 10
	MaximumChunkSize              = 64 << 10
	EphemeralPublicKeySize        = 32
	FileIDSize                    = 16
	ManifestHashSize              = sha256.Size
)

var (
	ErrInvalidHandshake = errors.New("invalid file handshake")
	ErrInvalidMetadata  = errors.New("invalid encrypted file metadata")
	ErrAuthentication   = errors.New("file payload authentication failed")
)

type OpenTranscript struct {
	TicketJWTID              string
	TransferID               string
	Generation               uint64
	SourceDeviceID           string
	TargetDeviceID           string
	SourceEphemeralPublicKey []byte
	DeclaredTotalBytes       uint64
	DeclaredFileCount        uint32
}

type AcceptTranscript struct {
	TargetEphemeralPublicKey []byte
	CipherSuite              uint32
	ChunkSize                uint32
	ReceiveWindow            uint32
}

type Handshake struct {
	Open            OpenTranscript
	OpenSignature   []byte
	Accept          AcceptTranscript
	AcceptSignature []byte
}

func SignOpen(privateKey ed25519.PrivateKey, open OpenTranscript) ([]byte, error) {
	message, err := openSigningMessage(open)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrInvalidHandshake
	}
	return ed25519.Sign(privateKey, message), nil
}

func VerifyOpen(publicKey ed25519.PublicKey, open OpenTranscript, signature []byte) error {
	message, err := openSigningMessage(open)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, message, signature) {
		return ErrInvalidHandshake
	}
	return nil
}

func SignAccept(privateKey ed25519.PrivateKey, open OpenTranscript, openSignature []byte, accept AcceptTranscript) ([]byte, error) {
	message, err := acceptSigningMessage(open, openSignature, accept)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrInvalidHandshake
	}
	return ed25519.Sign(privateKey, message), nil
}

func VerifyAccept(publicKey ed25519.PublicKey, open OpenTranscript, openSignature []byte, accept AcceptTranscript, signature []byte) error {
	message, err := acceptSigningMessage(open, openSignature, accept)
	if err != nil || len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, message, signature) {
		return ErrInvalidHandshake
	}
	return nil
}

func (h Handshake) TranscriptHash() ([]byte, error) {
	if err := validateOpen(h.Open); err != nil || validateAccept(h.Accept) != nil || len(h.OpenSignature) != ed25519.SignatureSize || len(h.AcceptSignature) != ed25519.SignatureSize {
		return nil, ErrInvalidHandshake
	}
	encoded := canonicalOpen(h.Open, "wenzwork-file-transcript-v1")
	encoded = appendField(encoded, h.OpenSignature)
	encoded = appendAccept(encoded, h.Accept)
	encoded = appendField(encoded, h.AcceptSignature)
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func openSigningMessage(open OpenTranscript) ([]byte, error) {
	if err := validateOpen(open); err != nil {
		return nil, err
	}
	return canonicalOpen(open, "wenzwork-file-open-v1"), nil
}

func acceptSigningMessage(open OpenTranscript, openSignature []byte, accept AcceptTranscript) ([]byte, error) {
	if err := validateOpen(open); err != nil || validateAccept(accept) != nil || len(openSignature) != ed25519.SignatureSize {
		return nil, ErrInvalidHandshake
	}
	encoded := canonicalOpen(open, "wenzwork-file-accept-v1")
	signatureDigest := sha256.Sum256(openSignature)
	encoded = appendField(encoded, signatureDigest[:])
	return appendAccept(encoded, accept), nil
}

func canonicalOpen(open OpenTranscript, domain string) []byte {
	encoded := make([]byte, 0, 256)
	encoded = appendField(encoded, []byte(domain))
	encoded = appendField(encoded, []byte(open.TicketJWTID))
	encoded = appendField(encoded, []byte(open.TransferID))
	encoded = binary.BigEndian.AppendUint64(encoded, open.Generation)
	encoded = appendField(encoded, []byte(open.SourceDeviceID))
	encoded = appendField(encoded, []byte(open.TargetDeviceID))
	encoded = appendField(encoded, open.SourceEphemeralPublicKey)
	encoded = binary.BigEndian.AppendUint64(encoded, open.DeclaredTotalBytes)
	encoded = binary.BigEndian.AppendUint32(encoded, open.DeclaredFileCount)
	return encoded
}

func appendAccept(encoded []byte, accept AcceptTranscript) []byte {
	encoded = appendField(encoded, accept.TargetEphemeralPublicKey)
	encoded = binary.BigEndian.AppendUint32(encoded, accept.CipherSuite)
	encoded = binary.BigEndian.AppendUint32(encoded, accept.ChunkSize)
	encoded = binary.BigEndian.AppendUint32(encoded, accept.ReceiveWindow)
	return encoded
}

func validateOpen(open OpenTranscript) error {
	if open.TicketJWTID == "" || open.TransferID == "" || open.Generation == 0 || open.SourceDeviceID == "" || open.TargetDeviceID == "" ||
		len(open.SourceEphemeralPublicKey) != EphemeralPublicKeySize || open.DeclaredFileCount == 0 {
		return ErrInvalidHandshake
	}
	return nil
}

func validateAccept(accept AcceptTranscript) error {
	if len(accept.TargetEphemeralPublicKey) != EphemeralPublicKeySize || accept.CipherSuite != XChaCha20Poly1305 ||
		accept.ChunkSize == 0 || accept.ChunkSize > MaximumChunkSize || accept.ReceiveWindow < accept.ChunkSize || accept.ReceiveWindow > 4<<20 {
		return ErrInvalidHandshake
	}
	return nil
}

type SessionKeys struct {
	ManifestKey   []byte
	FileMasterKey []byte
	ControlAtoB   []byte
	ControlBtoA   []byte
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

func DeriveSessionKeys(sharedSecret []byte, ticketJWTID, transferID string, generation uint64, transcriptHash []byte) (SessionKeys, error) {
	if len(sharedSecret) != 32 || ticketJWTID == "" || transferID == "" || generation == 0 || len(transcriptHash) != sha256.Size {
		return SessionKeys{}, ErrInvalidHandshake
	}
	saltInput := make([]byte, 0, 128)
	saltInput = appendField(saltInput, []byte("wenzwork-file-salt-v1"))
	saltInput = appendField(saltInput, []byte(ticketJWTID))
	saltInput = appendField(saltInput, []byte(transferID))
	saltInput = appendField(saltInput, transcriptHash)
	salt := sha256.Sum256(saltInput)
	derive := func(label string) ([]byte, error) {
		info := make([]byte, 0, 96)
		info = appendField(info, []byte(label))
		info = appendField(info, []byte(transferID))
		info = binary.BigEndian.AppendUint64(info, generation)
		key := make([]byte, chacha20poly1305.KeySize)
		if _, err := io.ReadFull(hkdf.New(sha256.New, sharedSecret, salt[:], info), key); err != nil {
			return nil, fmt.Errorf("derive %s: %w", label, err)
		}
		return key, nil
	}
	manifestKey, err := derive("ww-file-manifest-v1")
	if err != nil {
		return SessionKeys{}, err
	}
	fileMasterKey, err := derive("ww-file-master-v1")
	if err != nil {
		return SessionKeys{}, err
	}
	controlAtoB, err := derive("ww-file-control-a2b-v1")
	if err != nil {
		return SessionKeys{}, err
	}
	controlBtoA, err := derive("ww-file-control-b2a-v1")
	if err != nil {
		return SessionKeys{}, err
	}
	return SessionKeys{ManifestKey: manifestKey, FileMasterKey: fileMasterKey, ControlAtoB: controlAtoB, ControlBtoA: controlBtoA}, nil
}

func DeriveFileKey(fileMasterKey, fileID []byte) ([]byte, error) {
	if len(fileMasterKey) != chacha20poly1305.KeySize || len(fileID) != FileIDSize {
		return nil, ErrInvalidMetadata
	}
	info := make([]byte, 0, 48)
	info = appendField(info, []byte("ww-file-v1"))
	info = appendField(info, fileID)
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, fileMasterKey, nil, info), key); err != nil {
		return nil, fmt.Errorf("derive file key: %w", err)
	}
	return key, nil
}

type Direction uint32

const (
	DirectionSenderToReceiver Direction = 1
	DirectionReceiverToSender Direction = 2
)

type ChunkMetadata struct {
	ProtocolVersion uint32
	TransferID      string
	Generation      uint64
	FileID          []byte
	ChunkIndex      uint64
	Offset          uint64
	PlaintextSize   uint32
	TotalFileSize   uint64
	Direction       Direction
	ManifestHash    []byte
}

func SealChunk(fileKey, noncePrefix, plaintext []byte, metadata ChunkMetadata) ([]byte, error) {
	if uint32(len(plaintext)) != metadata.PlaintextSize || len(plaintext) > MaximumChunkSize {
		return nil, ErrInvalidMetadata
	}
	aead, nonce, aad, err := chunkCipherInputs(fileKey, noncePrefix, metadata)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

func OpenChunk(fileKey, noncePrefix, ciphertext []byte, metadata ChunkMetadata) ([]byte, error) {
	aead, nonce, aad, err := chunkCipherInputs(fileKey, noncePrefix, metadata)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil || uint32(len(plaintext)) != metadata.PlaintextSize {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

func chunkCipherInputs(fileKey, noncePrefix []byte, metadata ChunkMetadata) (cipher.AEAD, []byte, []byte, error) {
	if len(fileKey) != chacha20poly1305.KeySize || len(noncePrefix) != 16 || metadata.ProtocolVersion != ProtocolVersion ||
		metadata.TransferID == "" || metadata.Generation == 0 || len(metadata.FileID) != FileIDSize ||
		metadata.PlaintextSize > MaximumChunkSize || metadata.Offset > metadata.TotalFileSize ||
		uint64(metadata.PlaintextSize) > metadata.TotalFileSize-metadata.Offset ||
		(metadata.Direction != DirectionSenderToReceiver && metadata.Direction != DirectionReceiverToSender) || len(metadata.ManifestHash) != ManifestHashSize {
		return nil, nil, nil, ErrInvalidMetadata
	}
	aead, err := chacha20poly1305.NewX(fileKey)
	if err != nil {
		return nil, nil, nil, ErrInvalidMetadata
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	copy(nonce, noncePrefix)
	binary.BigEndian.PutUint64(nonce[16:], metadata.ChunkIndex)
	aad := make([]byte, 0, 192)
	aad = appendField(aad, []byte("ww-file-chunk-aad-v1"))
	aad = binary.BigEndian.AppendUint32(aad, metadata.ProtocolVersion)
	aad = appendField(aad, []byte(metadata.TransferID))
	aad = binary.BigEndian.AppendUint64(aad, metadata.Generation)
	aad = appendField(aad, metadata.FileID)
	aad = binary.BigEndian.AppendUint64(aad, metadata.ChunkIndex)
	aad = binary.BigEndian.AppendUint64(aad, metadata.Offset)
	aad = binary.BigEndian.AppendUint32(aad, metadata.PlaintextSize)
	aad = binary.BigEndian.AppendUint64(aad, metadata.TotalFileSize)
	aad = binary.BigEndian.AppendUint32(aad, uint32(metadata.Direction))
	aad = appendField(aad, metadata.ManifestHash)
	return aead, nonce, aad, nil
}

func appendField(destination, field []byte) []byte {
	destination = binary.BigEndian.AppendUint32(destination, uint32(len(field)))
	return append(destination, field...)
}
