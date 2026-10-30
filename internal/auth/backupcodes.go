package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// BackupCodeCount is how many single-use recovery codes are generated per
// enrolment / regeneration.
const BackupCodeCount = 10

// backupCodeBytes is the random entropy per code: 10 bytes = 80 bits, encoded
// as 20 hex chars and displayed as five dash-separated groups of four.
//
// Storage form: codes are hashed with a single fast SHA-256 (via HashToken),
// not a slow password KDF. That is deliberate and safe here because the input
// is 80 bits of uniform randomness, not a user-chosen secret. An offline
// attacker who steals the hashes gains nothing from a slow hash: brute-forcing
// an 80-bit uniform value is computationally infeasible regardless of the
// per-guess cost, so there is no low-entropy dictionary to grind down. A fast
// hash keeps verification cheap while the entropy — not the KDF — provides the
// security margin. (Password hashes need a slow KDF precisely because passwords
// are low-entropy; backup codes are not.)
const backupCodeBytes = 10

// GenerateBackupCodes returns BackupCodeCount fresh plaintext recovery codes
// (shown to the user once) alongside their hashed forms (stored in
// users.backup_codes). A code is consumed by hashing the presented value and
// deleting the matching stored hash.
func GenerateBackupCodes() (plain []string, hashed []string, err error) {
	plain = make([]string, 0, BackupCodeCount)
	hashed = make([]string, 0, BackupCodeCount)
	for i := 0; i < BackupCodeCount; i++ {
		b := make([]byte, backupCodeBytes)
		if _, rerr := rand.Read(b); rerr != nil {
			return nil, nil, fmt.Errorf("auth: read backup code: %w", rerr)
		}
		code := formatBackupCode(hex.EncodeToString(b)) // 20 hex chars → 5×4 groups
		plain = append(plain, code)
		hashed = append(hashed, HashToken(code))
	}
	return plain, hashed, nil
}

// formatBackupCode groups a 20-char hex string into five dash-separated blocks
// of four, e.g. "1a2b-3c4d-5e6f-7081-92a3".
func formatBackupCode(hexCode string) string {
	var sb strings.Builder
	sb.Grow(len(hexCode) + len(hexCode)/4)
	for i := 0; i < len(hexCode); i += 4 {
		if i > 0 {
			sb.WriteByte('-')
		}
		end := i + 4
		if end > len(hexCode) {
			end = len(hexCode)
		}
		sb.WriteString(hexCode[i:end])
	}
	return sb.String()
}

// normalizeBackupCode reduces a presented code to the canonical stored spelling:
// hex digits only, lower case, regrouped into five blocks of four.
//
// The code is hashed before comparison, so the presented string had to match the
// generated one byte for byte — and three perfectly ordinary ways of typing the
// same code did not. A password manager or a person will hand back
// "1A2B-3C4D-…" (hex is conventionally case-insensitive), or the bare
// "1a2b3c4d5e6f708192a3" without the cosmetic dashes, or the right code with a
// trailing space from a copy. All three were refused.
//
// That matters more than it looks: a backup code is used at exactly the moment the
// authenticator is gone, so a rejected VALID code is a locked-out account with no
// self-service way back. Nothing is weakened by accepting the variants — they are
// the same code, the hash still has to match, and the 80 bits of entropy are
// untouched.
func normalizeBackupCode(code string) string {
	var hexOnly strings.Builder
	hexOnly.Grow(len(code))
	for i := 0; i < len(code); i++ {
		switch c := code[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
			hexOnly.WriteByte(c)
		case c >= 'A' && c <= 'F':
			hexOnly.WriteByte(c + ('a' - 'A'))
		}
	}
	return formatBackupCode(hexOnly.String())
}

// ConsumeBackupCode looks for code among the stored hashes. On a match it
// returns the remaining hashes (with the used one removed) and true; otherwise
// it returns stored unchanged and false. The scan is constant-time per entry.
//
// The presented code is normalised first (see normalizeBackupCode); the canonical
// form is its own fixed point, so hashes generated before this existed still match.
func ConsumeBackupCode(stored []string, code string) (remaining []string, ok bool) {
	want := HashToken(normalizeBackupCode(code))
	idx := -1
	for i, h := range stored {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			idx = i
		}
	}
	if idx < 0 {
		return stored, false
	}
	remaining = make([]string, 0, len(stored)-1)
	remaining = append(remaining, stored[:idx]...)
	remaining = append(remaining, stored[idx+1:]...)
	return remaining, true
}
