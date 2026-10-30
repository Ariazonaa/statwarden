// Package auth provides Statwarden's authentication primitives: argon2id password
// hashing, opaque session/API-key tokens, TOTP two-factor codes with backup
// codes, and the role model for RBAC. Everything here is pure and
// dependency-free of the database so it is trivially unit-testable; the HTTP
// wiring lives in internal/api.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the argon2id cost parameters. The defaults follow the
// OWASP "second recommended" configuration (64 MiB, one iteration, four lanes)
// which is a good balance for an interactive login on modest self-hosted
// hardware.
type Argon2Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultArgon2Params is the parameter set used by HashPassword.
var DefaultArgon2Params = Argon2Params{
	Memory:      64 * 1024,
	Iterations:  1,
	Parallelism: 4,
	SaltLength:  16,
	KeyLength:   32,
}

// ErrInvalidHash is returned when a stored hash cannot be parsed.
var ErrInvalidHash = errors.New("auth: invalid password hash format")

// HashPassword hashes password with argon2id and encodes the result as a
// self-describing PHC string ($argon2id$v=19$m=…,t=…,p=…$salt$hash), so
// VerifyPassword can re-derive the parameters without external state.
func HashPassword(password string) (string, error) {
	return hashPasswordWith(password, DefaultArgon2Params)
}

func hashPasswordWith(password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the encoded argon2id hash in
// constant time. A malformed hash returns ErrInvalidHash.
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return true, nil
	}
	return false, nil
}

func decodeHash(encoded string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Argon2Params{}, nil, nil, ErrInvalidHash
	}
	// len() of a slice decoded from a stored PHC string: non-negative, and
	// bounded by the column that holds it long before uint32 could wrap.
	p.SaltLength = uint32(len(salt)) //nolint:gosec // G115: a hash string cannot be 4 GiB long
	p.KeyLength = uint32(len(key))   //nolint:gosec // G115: same
	return p, salt, key, nil
}
