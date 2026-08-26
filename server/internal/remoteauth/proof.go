package remoteauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	ErrProofExpired = errors.New("device proof challenge expired")
	ErrProofInvalid = errors.New("device proof is invalid")
)

type Challenge struct {
	Nonce           []byte
	TicketJWTID     string
	CellID          string
	NodeID          string
	ConnectionEpoch uint64
	Deadline        time.Time
}

func NewChallenge(ticketJWTID, cellID, nodeID string, connectionEpoch uint64, now time.Time, ttl time.Duration) (Challenge, error) {
	return NewChallengeFrom(rand.Reader, ticketJWTID, cellID, nodeID, connectionEpoch, now, ttl)
}

func NewChallengeFrom(random io.Reader, ticketJWTID, cellID, nodeID string, connectionEpoch uint64, now time.Time, ttl time.Duration) (Challenge, error) {
	if ticketJWTID == "" || cellID == "" || nodeID == "" || connectionEpoch == 0 || ttl <= 0 {
		return Challenge{}, errors.New("challenge fields are required")
	}
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return Challenge{}, fmt.Errorf("generate challenge nonce: %w", err)
	}
	return Challenge{
		Nonce: nonce, TicketJWTID: ticketJWTID, CellID: cellID, NodeID: nodeID,
		ConnectionEpoch: connectionEpoch, Deadline: now.Add(ttl).UTC(),
	}, nil
}

func SignChallenge(privateKey ed25519.PrivateKey, challenge Challenge) ([]byte, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, ErrProofInvalid
	}
	encoded, err := challenge.canonicalBytes()
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(privateKey, encoded), nil
}

func VerifyChallenge(publicKey ed25519.PublicKey, expectedThumbprint string, challenge Challenge, signature []byte, now time.Time) error {
	if !now.Before(challenge.Deadline) {
		return ErrProofExpired
	}
	if len(publicKey) != ed25519.PublicKeySize || PublicKeyThumbprint(publicKey) != expectedThumbprint {
		return ErrProofInvalid
	}
	encoded, err := challenge.canonicalBytes()
	if err != nil || !ed25519.Verify(publicKey, encoded, signature) {
		return ErrProofInvalid
	}
	return nil
}

func (c Challenge) canonicalBytes() ([]byte, error) {
	if len(c.Nonce) != 32 || c.TicketJWTID == "" || c.CellID == "" || c.NodeID == "" || c.ConnectionEpoch == 0 || c.Deadline.IsZero() {
		return nil, ErrProofInvalid
	}
	encoded := make([]byte, 0, 128)
	encoded = appendField(encoded, []byte("wenzwork-relay-proof-v1"))
	encoded = appendField(encoded, c.Nonce)
	encoded = appendField(encoded, []byte(c.TicketJWTID))
	encoded = appendField(encoded, []byte(c.CellID))
	encoded = appendField(encoded, []byte(c.NodeID))
	encoded = binary.BigEndian.AppendUint64(encoded, c.ConnectionEpoch)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(c.Deadline.UnixMilli()))
	return encoded, nil
}

func appendField(destination, field []byte) []byte {
	destination = binary.BigEndian.AppendUint32(destination, uint32(len(field)))
	return append(destination, field...)
}
