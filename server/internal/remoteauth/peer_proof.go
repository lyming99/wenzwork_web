package remoteauth

import (
	"crypto/ed25519"
	"strings"
)

const peerEphemeralPublicKeySize = 32

// PeerOpenIdentityProof is signed by the source device identity key. The
// ticket jti and both device IDs prevent the ephemeral key proof from being
// replayed into another authorized Peer Session.
type PeerOpenIdentityProof struct {
	TicketJWTID        string
	SessionID          string
	SourceDeviceID     string
	TargetDeviceID     string
	EphemeralPublicKey []byte
}

// PeerReadyIdentityProof is signed by the target device identity key. It binds
// both ephemeral public keys so an intermediary cannot substitute either side
// of the end-to-end key agreement.
type PeerReadyIdentityProof struct {
	TicketJWTID              string
	SessionID                string
	SourceDeviceID           string
	TargetDeviceID           string
	SourceEphemeralPublicKey []byte
	TargetEphemeralPublicKey []byte
}

func SignPeerOpenIdentity(privateKey ed25519.PrivateKey, proof PeerOpenIdentityProof) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrProofInvalid
	}
	encoded, err := proof.canonicalBytes()
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, encoded), nil
}

func VerifyPeerOpenIdentity(publicKey ed25519.PublicKey, expectedThumbprint string, proof PeerOpenIdentityProof, signature []byte) error {
	if len(publicKey) != ed25519.PublicKeySize || PublicKeyThumbprint(publicKey) != expectedThumbprint || len(signature) != ed25519.SignatureSize {
		return ErrProofInvalid
	}
	encoded, err := proof.canonicalBytes()
	if err != nil || !ed25519.Verify(publicKey, encoded, signature) {
		return ErrProofInvalid
	}
	return nil
}

func SignPeerReadyIdentity(privateKey ed25519.PrivateKey, proof PeerReadyIdentityProof) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrProofInvalid
	}
	encoded, err := proof.canonicalBytes()
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, encoded), nil
}

func VerifyPeerReadyIdentity(publicKey ed25519.PublicKey, expectedThumbprint string, proof PeerReadyIdentityProof, signature []byte) error {
	if len(publicKey) != ed25519.PublicKeySize || PublicKeyThumbprint(publicKey) != expectedThumbprint || len(signature) != ed25519.SignatureSize {
		return ErrProofInvalid
	}
	encoded, err := proof.canonicalBytes()
	if err != nil || !ed25519.Verify(publicKey, encoded, signature) {
		return ErrProofInvalid
	}
	return nil
}

func (proof PeerOpenIdentityProof) canonicalBytes() ([]byte, error) {
	if !validPeerProofFields(proof.TicketJWTID, proof.SessionID, proof.SourceDeviceID, proof.TargetDeviceID) ||
		proof.SourceDeviceID == proof.TargetDeviceID || len(proof.EphemeralPublicKey) != peerEphemeralPublicKeySize {
		return nil, ErrProofInvalid
	}
	encoded := make([]byte, 0, 256)
	for _, field := range [][]byte{
		[]byte("wenzwork-relay-peer-open-v1"),
		[]byte(proof.TicketJWTID),
		[]byte(proof.SessionID),
		[]byte(proof.SourceDeviceID),
		[]byte(proof.TargetDeviceID),
		proof.EphemeralPublicKey,
	} {
		encoded = appendField(encoded, field)
	}
	return encoded, nil
}

func (proof PeerReadyIdentityProof) canonicalBytes() ([]byte, error) {
	if !validPeerProofFields(proof.TicketJWTID, proof.SessionID, proof.SourceDeviceID, proof.TargetDeviceID) ||
		proof.SourceDeviceID == proof.TargetDeviceID || len(proof.SourceEphemeralPublicKey) != peerEphemeralPublicKeySize ||
		len(proof.TargetEphemeralPublicKey) != peerEphemeralPublicKeySize {
		return nil, ErrProofInvalid
	}
	encoded := make([]byte, 0, 320)
	for _, field := range [][]byte{
		[]byte("wenzwork-relay-peer-ready-v1"),
		[]byte(proof.TicketJWTID),
		[]byte(proof.SessionID),
		[]byte(proof.SourceDeviceID),
		[]byte(proof.TargetDeviceID),
		proof.SourceEphemeralPublicKey,
		proof.TargetEphemeralPublicKey,
	} {
		encoded = appendField(encoded, field)
	}
	return encoded, nil
}

func validPeerProofFields(fields ...string) bool {
	for _, field := range fields {
		if field == "" || len(field) > 128 || strings.ContainsRune(field, '\x00') {
			return false
		}
	}
	return true
}
