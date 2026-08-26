package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	MinimumPasswordRunes = 8
	MaximumPasswordRunes = 128
	MaximumPasswordBytes = 1024
)

var (
	ErrInvalidPassword = errors.New("invalid password")
	ErrInvalidHash     = errors.New("invalid password hash")
)

// Argon2Params stores values in the same units used by golang.org/x/crypto/argon2.
// Validation accepts the current OWASP minimum (19 MiB, t=2, p=1) so older
// hashes remain verifiable. New hashes default to a benchmarked stronger cost.
// https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
type Argon2Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

func DefaultArgon2Params() Argon2Params {
	return Argon2Params{
		MemoryKiB:   64 * 1024,
		Iterations:  3,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

func (p Argon2Params) Validate() error {
	if p.MemoryKiB < 19*1024 || p.MemoryKiB > 256*1024 {
		return errors.New("argon2 memory must be between 19456 and 262144 KiB")
	}
	if p.Iterations < 2 || p.Iterations > 10 {
		return errors.New("argon2 iterations must be between 2 and 10")
	}
	if p.Parallelism < 1 || p.Parallelism > 8 {
		return errors.New("argon2 parallelism must be between 1 and 8")
	}
	if p.SaltLength < 16 || p.SaltLength > 64 {
		return errors.New("argon2 salt length must be between 16 and 64 bytes")
	}
	if p.KeyLength < 32 || p.KeyLength > 64 {
		return errors.New("argon2 key length must be between 32 and 64 bytes")
	}
	return nil
}

func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return fmt.Errorf("%w: password must be valid UTF-8", ErrInvalidPassword)
	}
	if len(password) > MaximumPasswordBytes {
		return fmt.Errorf("%w: password exceeds byte limit", ErrInvalidPassword)
	}
	runes := utf8.RuneCountInString(password)
	if runes < MinimumPasswordRunes || runes > MaximumPasswordRunes {
		return fmt.Errorf("%w: password must contain between %d and %d characters", ErrInvalidPassword, MinimumPasswordRunes, MaximumPasswordRunes)
	}
	return nil
}

func HashPassword(password string, params Argon2Params) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	if err := params.Validate(); err != nil {
		return "", fmt.Errorf("invalid argon2 parameters: %w", err)
	}

	salt := make([]byte, params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKiB, params.Parallelism, params.KeyLength)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		params.MemoryKiB,
		params.Iterations,
		params.Parallelism,
		b64.EncodeToString(salt),
		b64.EncodeToString(hash),
	), nil
}

func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey([]byte(password), salt, params.Iterations, params.MemoryKiB, params.Parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func PasswordHashNeedsUpgrade(encoded string, current Argon2Params) bool {
	params, _, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return true
	}
	return params.MemoryKiB != current.MemoryKiB ||
		params.Iterations != current.Iterations ||
		params.Parallelism != current.Parallelism ||
		uint32(len(expected)) != current.KeyLength
}

func parsePasswordHash(encoded string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}

	paramParts := strings.Split(parts[3], ",")
	if len(paramParts) != 3 {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	memory, err := parseUintParameter(paramParts[0], "m=", 32)
	if err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	iterations, err := parseUintParameter(paramParts[1], "t=", 32)
	if err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	parallelism, err := parseUintParameter(paramParts[2], "p=", 8)
	if err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}

	b64 := base64.RawStdEncoding
	salt, err := b64.Strict().DecodeString(parts[4])
	if err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	hash, err := b64.Strict().DecodeString(parts[5])
	if err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	params := Argon2Params{
		MemoryKiB:   uint32(memory),
		Iterations:  uint32(iterations),
		Parallelism: uint8(parallelism),
		SaltLength:  uint32(len(salt)),
		KeyLength:   uint32(len(hash)),
	}
	if err := params.Validate(); err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	return params, salt, hash, nil
}

func parseUintParameter(raw, prefix string, bitSize int) (uint64, error) {
	if !strings.HasPrefix(raw, prefix) {
		return 0, ErrInvalidHash
	}
	return strconv.ParseUint(strings.TrimPrefix(raw, prefix), 10, bitSize)
}
