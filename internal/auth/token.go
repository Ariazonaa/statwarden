package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the entropy of an opaque token (256 bits).
const tokenBytes = 32

// NewToken returns a fresh URL-safe opaque token. It is shown to the client
// once (as a session cookie value or an API-key secret); only its HashToken
// digest is ever persisted, so a database leak cannot reconstruct live tokens.
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex-encoded SHA-256 of a token. Session and API-key
// lookups hash the presented token and compare against the stored digest; a
// plain hash (not argon2) is correct here because the input already has full
// cryptographic entropy, so it needs no slow KDF.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// APIKeyPrefix is the visible, non-secret prefix on API-key tokens. It lets
// operators recognise an Statwarden key at a glance and is stored in cleartext for
// display in the key list.
const APIKeyPrefix = "statwarden_"

// NewAPIKey returns a prefixed API-key secret and its short display prefix
// (the first 12 characters, safe to store and show).
func NewAPIKey() (secret, display string, err error) {
	tok, err := NewToken()
	if err != nil {
		return "", "", err
	}
	secret = APIKeyPrefix + tok
	display = secret[:12]
	return secret, display, nil
}

// sessionFingerprintDomain separates this digest from any other SHA-256 use in
// the codebase, so a fingerprint can never collide with or be mistaken for a
// stored token hash.
//
// It still carries the project's former name on purpose. The label is an input
// to every fingerprint ever computed and is never shown to anyone; it is a
// constant of the format, not branding, and changing it would silently change
// every fingerprint for no visible gain.
const sessionFingerprintDomain = "uptura-session-fingerprint:"

// SessionFingerprintLen is the hex length of a session fingerprint. 16 hex
// chars is 64 bits — far beyond collision range for the handful of sessions a
// single user holds, while staying short enough to read in a URL.
const SessionFingerprintLen = 16

// SessionFingerprint derives a stable, public identifier from a stored session
// hash. It exists so the UI can address one specific session — "log this device
// out" — without the server ever handing out the hash, which is the credential
// the session cookie must reproduce.
//
// Publishing it is safe: the input is already a SHA-256 of a 256-bit random
// token, and this digests it again behind a domain separator before truncating.
// Inverting the fingerprint would mean inverting SHA-256, and even knowing the
// stored hash would not let anyone forge a cookie, since the cookie is the
// pre-image of that hash and not the hash itself.
func SessionFingerprint(storedHash string) string {
	if storedHash == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionFingerprintDomain + storedHash))
	return hex.EncodeToString(sum[:])[:SessionFingerprintLen]
}
