package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const opaqueTokenBytes = 32

var ErrInvalidToken = errors.New("invalid token")

func NewOpaqueToken() (plaintext string, digest string, err error) {
	random := make([]byte, opaqueTokenBytes)
	if _, err := rand.Read(random); err != nil {
		return "", "", fmt.Errorf("generate opaque token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(random)
	digest = digestBytes(random)
	return plaintext, digest, nil
}

func DigestOpaqueToken(plaintext string) (string, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(plaintext)
	if err != nil || len(raw) != opaqueTokenBytes {
		return "", ErrInvalidToken
	}
	return digestBytes(raw), nil
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
