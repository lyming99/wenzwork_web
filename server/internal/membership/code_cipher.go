package membership

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

type redemptionCodeCipher struct {
	aead cipher.AEAD
}

func newRedemptionCodeCipher(encryptionKey, purpose string) (*redemptionCodeCipher, error) {
	if len(encryptionKey) < 32 {
		return nil, errors.New("redemption code encryption key must contain at least 32 bytes")
	}
	if purpose == "" {
		return nil, errors.New("redemption code encryption purpose is required")
	}

	derivation := hmac.New(sha256.New, []byte(encryptionKey))
	_, _ = derivation.Write([]byte(purpose))
	block, err := aes.NewCipher(derivation.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create redemption code cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create redemption code AEAD: %w", err)
	}
	return &redemptionCodeCipher{aead: aead}, nil
}

func (c *redemptionCodeCipher) Encrypt(recordID uuid.UUID, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate redemption code nonce: %w", err)
	}
	result := make([]byte, 1, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	result[0] = 1
	result = append(result, nonce...)
	return c.aead.Seal(result, nonce, plaintext, recordID[:]), nil
}

func (c *redemptionCodeCipher) Decrypt(recordID uuid.UUID, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < 1+c.aead.NonceSize()+c.aead.Overhead() || ciphertext[0] != 1 {
		return nil, errors.New("redemption code ciphertext is invalid")
	}
	nonceEnd := 1 + c.aead.NonceSize()
	plaintext, err := c.aead.Open(nil, ciphertext[1:nonceEnd], ciphertext[nonceEnd:], recordID[:])
	if err != nil {
		return nil, errors.New("redemption code authentication failed")
	}
	return plaintext, nil
}
