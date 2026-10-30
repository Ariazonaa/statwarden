// Package secrets encrypts notification channel configs at rest.
//
// notifications.config carries provider secrets (SMTP passwords, webhook
// tokens, …). At the persistence boundary the whole config blob is sealed
// with AES-256-GCM under a key derived from the ENCRYPTION_KEY environment
// variable; registry, dispatcher and providers only ever see plaintext JSON.
//
// The key derivation is a single SHA-256 and cannot change without orphaning
// every row that is already encrypted — see the Box documentation.
//
// On disk the sealed blob stays valid jsonb by wrapping the base64 ciphertext
// in an envelope object: {"$encrypted": "<base64(nonce||ciphertext)>"}.
// Configs without the envelope pass through unchanged, so pre-existing or
// DEV_MODE plaintext rows keep working.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Sentinel errors.
var (
	// ErrEmptyKey is returned by New for an empty ENCRYPTION_KEY.
	ErrEmptyKey = errors.New("secrets: encryption key is empty")
	// ErrCiphertext is returned when a sealed blob cannot be decoded or
	// authenticated (wrong key, truncation, tampering).
	ErrCiphertext = errors.New("secrets: cannot decrypt: invalid ciphertext or wrong key")
)

// Box seals and opens small blobs with AES-256-GCM. The 32-byte AES key is the
// plain SHA-256 of the configured key string, so any non-empty ENCRYPTION_KEY
// produces a usable Box regardless of length or encoding.
//
// THE DERIVATION IS FROZEN. Every monitor and notification config already
// stored by any deployment was sealed under sha256(ENCRYPTION_KEY); introducing
// a salt, a work factor or any other KDF here would not strengthen those rows,
// it would make them permanently undecryptable. A single hash means the AES key
// is exactly as strong as the string an operator configured, and that is dealt
// with where it can be dealt with safely: internal/config rejects short,
// repetitive or placeholder ENCRYPTION_KEY values at startup (see
// config.MinSecretLength), before a weak key is ever used to encrypt anything.
//
// New itself stays permissive on purpose — DEV_MODE and the test suite hand it
// throwaway keys, and it is not the layer that knows about DEV_MODE.
type Box struct {
	aead cipher.AEAD
}

// New derives a Box from the ENCRYPTION_KEY string. Strength requirements on
// that string are enforced at configuration load time, not here; see Box.
func New(key string) (*Box, error) {
	if key == "" {
		return nil, ErrEmptyKey
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Encrypt seals plaintext with a fresh random nonce and returns
// base64(nonce || ciphertext).
func (b *Box) Encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secrets: nonce: %w", err)
	}
	sealed := b.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a base64(nonce || ciphertext) blob produced by Encrypt.
func (b *Box) Decrypt(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrCiphertext
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return nil, ErrCiphertext
	}
	plaintext, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return nil, ErrCiphertext
	}
	return plaintext, nil
}

// envelope is the jsonb wrapper stored in notifications.config.
type envelope struct {
	Encrypted string `json:"$encrypted"`
}

// IsEncrypted reports whether cfg is a sealed envelope.
func IsEncrypted(cfg json.RawMessage) bool {
	var env envelope
	if err := json.Unmarshal(cfg, &env); err != nil {
		return false
	}
	return env.Encrypted != ""
}

// EncryptConfig seals a plaintext config blob into an envelope. Sealing an
// already-sealed envelope is an error to prevent accidental double
// encryption.
func (b *Box) EncryptConfig(cfg json.RawMessage) (json.RawMessage, error) {
	if len(cfg) == 0 {
		cfg = json.RawMessage("{}")
	}
	if !json.Valid(cfg) {
		return nil, errors.New("secrets: config is not valid JSON")
	}
	if IsEncrypted(cfg) {
		return nil, errors.New("secrets: config is already encrypted")
	}
	sealed, err := b.Encrypt(cfg)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(envelope{Encrypted: sealed})
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return out, nil
}

// DecryptConfig opens an envelope back into plaintext JSON. A config without
// the envelope marker is returned unchanged (plaintext passthrough).
func (b *Box) DecryptConfig(cfg json.RawMessage) (json.RawMessage, error) {
	if len(cfg) == 0 {
		return json.RawMessage("{}"), nil
	}
	var env envelope
	if err := json.Unmarshal(cfg, &env); err != nil || env.Encrypted == "" {
		return cfg, nil
	}
	plaintext, err := b.Decrypt(env.Encrypted)
	if err != nil {
		return nil, err
	}
	if !json.Valid(plaintext) {
		return nil, ErrCiphertext
	}
	return plaintext, nil
}
