package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNewEmptyKey(t *testing.T) {
	if _, err := New(""); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("New(\"\") error = %v, want ErrEmptyKey", err)
	}
}

// TestKeyDerivationIsFrozen pins the derivation itself: the AES key is the
// plain SHA-256 of the configured string, with no salt and no work factor.
//
// This looks like exactly the thing a security review would want replaced by a
// KDF, and it must not be. Every monitor and notification config already in a
// production database was sealed under sha256(ENCRYPTION_KEY); a salted or
// stretched derivation would not make those rows stronger, it would make them
// unreadable for good. The weakness a single hash leaves — the AES key is only
// as strong as the operator's string — is handled at the other end, by
// internal/config refusing short, repetitive or placeholder keys at startup.
//
// The test builds the cipher independently instead of calling New, so changing
// New's derivation fails here rather than silently at the next deployment's
// first decrypt.
func TestKeyDerivationIsFrozen(t *testing.T) {
	const key = "Zt4Nw8Rk1Vs6Mc3Xy9Bp2Ld7Qh5Gf0Jut"
	plaintext := []byte(`{"password":"hunter2"}`)

	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	sealed := base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plaintext, nil))

	box, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Decrypt(sealed)
	if err != nil {
		t.Fatalf("Decrypt of a blob sealed under sha256(key) failed: %v — the key derivation "+
			"changed, which orphans every config encrypted by an existing deployment", err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("round trip = %q, want %q", got, plaintext)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	box, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	tests := []string{
		"",
		"hello",
		`{"password":"hunter2","url":"https://example.com"}`,
		strings.Repeat("x", 64*1024),
	}
	for _, plaintext := range tests {
		sealed, err := box.Encrypt([]byte(plaintext))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		got, err := box.Decrypt(sealed)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if string(got) != plaintext {
			t.Fatalf("round trip = %q, want %q", got, plaintext)
		}
	}
}

func TestEncryptNonceRandomness(t *testing.T) {
	box, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := box.Encrypt([]byte("same"))
	b, _ := box.Encrypt([]byte("same"))
	if a == b {
		t.Fatal("two encryptions of the same plaintext must differ (random nonce)")
	}
}

func TestDecryptFailures(t *testing.T) {
	box, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("wrong key", func(t *testing.T) {
		other, _ := New("other-key")
		if _, err := other.Decrypt(sealed); !errors.Is(err, ErrCiphertext) {
			t.Fatalf("error = %v, want ErrCiphertext", err)
		}
	})
	t.Run("tampered", func(t *testing.T) {
		raw, _ := base64.StdEncoding.DecodeString(sealed)
		raw[len(raw)-1] ^= 0xff
		tampered := base64.StdEncoding.EncodeToString(raw)
		if _, err := box.Decrypt(tampered); !errors.Is(err, ErrCiphertext) {
			t.Fatalf("error = %v, want ErrCiphertext", err)
		}
	})
	t.Run("not base64", func(t *testing.T) {
		if _, err := box.Decrypt("%%%"); !errors.Is(err, ErrCiphertext) {
			t.Fatalf("error = %v, want ErrCiphertext", err)
		}
	})
	t.Run("too short", func(t *testing.T) {
		if _, err := box.Decrypt(base64.StdEncoding.EncodeToString([]byte("ab"))); !errors.Is(err, ErrCiphertext) {
			t.Fatalf("error = %v, want ErrCiphertext", err)
		}
	})
}

func TestConfigEnvelopeRoundTrip(t *testing.T) {
	box, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	plaintext := json.RawMessage(`{"url":"https://hooks.example.com/x","token":"s3cret"}`)

	sealed, err := box.EncryptConfig(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(sealed) {
		t.Fatal("sealed config must stay valid JSON (jsonb column)")
	}
	if !IsEncrypted(sealed) {
		t.Fatal("IsEncrypted(sealed) = false")
	}
	if strings.Contains(string(sealed), "s3cret") {
		t.Fatal("sealed config leaks plaintext")
	}

	got, err := box.DecryptConfig(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plaintext) {
		t.Fatalf("DecryptConfig = %s, want %s", got, plaintext)
	}
}

func TestDecryptConfigPassthrough(t *testing.T) {
	box, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plaintext object", `{"url":"https://example.com"}`, `{"url":"https://example.com"}`},
		{"empty", "", "{}"},
		{"empty object", "{}", "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := box.DecryptConfig(json.RawMessage(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("DecryptConfig(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestEncryptConfigRejects(t *testing.T) {
	box, err := New("test-key")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("invalid json", func(t *testing.T) {
		if _, err := box.EncryptConfig(json.RawMessage(`{"broken`)); err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
	t.Run("double encryption", func(t *testing.T) {
		sealed, err := box.EncryptConfig(json.RawMessage(`{"a":1}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := box.EncryptConfig(sealed); err == nil {
			t.Fatal("expected error for already-encrypted config")
		}
	})
}
