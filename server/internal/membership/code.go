package membership

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const (
	codePrefix       = "WZM"
	codeGroupCount   = 5
	codeGroupLength  = 4
	codeRandomLength = codeGroupCount * codeGroupLength
	codeAlphabet     = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	minimumHMACKey   = 32
)

var ErrInvalidCode = errors.New("invalid redemption code")
var ErrInvalidBatchQuantity = errors.New("redemption code batch quantity must be between 1 and 5000")

type IssuedCode struct {
	Plaintext  string
	Normalized string
	Digest     string
	Hint       string
}

type CodeCodec struct {
	random io.Reader
	key    []byte
}

func NewCodeCodec(hmacKey []byte) (*CodeCodec, error) {
	return newCodeCodec(hmacKey, rand.Reader)
}

func newCodeCodec(hmacKey []byte, random io.Reader) (*CodeCodec, error) {
	if len(hmacKey) < minimumHMACKey {
		return nil, fmt.Errorf("redemption code HMAC key must contain at least %d bytes", minimumHMACKey)
	}
	if random == nil {
		return nil, errors.New("redemption code random source is required")
	}
	keyCopy := append([]byte(nil), hmacKey...)
	return &CodeCodec{random: random, key: keyCopy}, nil
}

func (c *CodeCodec) Generate() (IssuedCode, error) {
	randomPart := make([]byte, codeRandomLength)
	buffer := make([]byte, codeRandomLength)
	if _, err := io.ReadFull(c.random, buffer); err != nil {
		return IssuedCode{}, fmt.Errorf("read secure randomness: %w", err)
	}
	for index, value := range buffer {
		randomPart[index] = codeAlphabet[int(value)&31]
	}

	normalized := codePrefix + string(randomPart)
	return IssuedCode{
		Plaintext:  formatNormalized(normalized),
		Normalized: normalized,
		Digest:     c.Digest(normalized),
		Hint:       normalized[len(normalized)-4:],
	}, nil
}

func (c *CodeCodec) GenerateBatch(quantity int) ([]IssuedCode, error) {
	if quantity < 1 || quantity > 5000 {
		return nil, ErrInvalidBatchQuantity
	}

	issued := make([]IssuedCode, 0, quantity)
	seen := make(map[string]struct{}, quantity)
	maxAttempts := quantity * 10
	for attempts := 0; len(issued) < quantity && attempts < maxAttempts; attempts++ {
		code, err := c.Generate()
		if err != nil {
			return nil, err
		}
		if _, exists := seen[code.Digest]; exists {
			continue
		}
		seen[code.Digest] = struct{}{}
		issued = append(issued, code)
	}
	if len(issued) != quantity {
		return nil, errors.New("secure random source produced too many duplicate redemption codes")
	}
	return issued, nil
}

func (c *CodeCodec) Inspect(raw string) (normalized, digest, hint string, err error) {
	normalized, err = NormalizeCode(raw)
	if err != nil {
		return "", "", "", err
	}
	return normalized, c.Digest(normalized), normalized[len(normalized)-4:], nil
}

func (c *CodeCodec) Digest(normalized string) string {
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write([]byte(normalized))
	return hex.EncodeToString(mac.Sum(nil))
}

func NormalizeCode(raw string) (string, error) {
	var builder strings.Builder
	builder.Grow(len(raw))
	for _, character := range strings.ToUpper(strings.TrimSpace(raw)) {
		switch {
		case character == '-' || unicode.IsSpace(character):
			continue
		case character >= 'A' && character <= 'Z':
			builder.WriteRune(character)
		case character >= '0' && character <= '9':
			builder.WriteRune(character)
		default:
			return "", ErrInvalidCode
		}
	}

	normalized := builder.String()
	if len(normalized) != len(codePrefix)+codeRandomLength || !strings.HasPrefix(normalized, codePrefix) {
		return "", ErrInvalidCode
	}
	for _, character := range normalized[len(codePrefix):] {
		if !strings.ContainsRune(codeAlphabet, character) {
			return "", ErrInvalidCode
		}
	}
	return normalized, nil
}

func formatNormalized(normalized string) string {
	randomPart := normalized[len(codePrefix):]
	groups := make([]string, 0, codeGroupCount+1)
	groups = append(groups, codePrefix)
	for start := 0; start < len(randomPart); start += codeGroupLength {
		groups = append(groups, randomPart[start:start+codeGroupLength])
	}
	return strings.Join(groups, "-")
}
