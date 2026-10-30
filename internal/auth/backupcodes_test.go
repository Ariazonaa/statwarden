package auth

import (
	"regexp"
	"strings"
	"testing"
)

// backupCodePattern is the required plaintext format: five dash-separated
// groups of four lowercase hex chars (20 hex chars = 80 bits of entropy).
var backupCodePattern = regexp.MustCompile(`^[0-9a-f]{4}(-[0-9a-f]{4}){4}$`)

func TestGenerateBackupCodesEntropyAndFormat(t *testing.T) {
	plain, hashed, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	if len(plain) != BackupCodeCount || len(hashed) != BackupCodeCount {
		t.Fatalf("got %d plain / %d hashed codes, want %d each", len(plain), len(hashed), BackupCodeCount)
	}

	for _, code := range plain {
		// 80-bit entropy: exactly 20 hex chars (10 random bytes). A 40-bit
		// (5-byte / 10-hex) code fails this — the pre-fix "xxxxx-xxxxx" form
		// is only 10 hex chars and does not match the five-group pattern.
		hexOnly := strings.ReplaceAll(code, "-", "")
		if len(hexOnly) != 2*backupCodeBytes {
			t.Errorf("code %q has %d hex chars, want %d (%d bits)", code, len(hexOnly), 2*backupCodeBytes, 8*backupCodeBytes)
		}
		if backupCodeBytes < 10 {
			t.Errorf("backupCodeBytes = %d, want >= 10 (80 bits)", backupCodeBytes)
		}
		if !backupCodePattern.MatchString(code) {
			t.Errorf("code %q does not match required format %s", code, backupCodePattern)
		}
	}
}

func TestGenerateBackupCodesAreUnique(t *testing.T) {
	plain, _, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	seen := make(map[string]bool, len(plain))
	for _, code := range plain {
		if seen[code] {
			t.Errorf("duplicate backup code generated: %q", code)
		}
		seen[code] = true
	}
}

func TestGenerateBackupCodesHashMatchesPlain(t *testing.T) {
	plain, hashed, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	for i, code := range plain {
		if HashToken(code) != hashed[i] {
			t.Errorf("hashed[%d] does not match HashToken(plain[%d])", i, i)
		}
	}
}

func TestConsumeBackupCode(t *testing.T) {
	plain, hashed, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}

	// A valid code is consumed and removed exactly once.
	remaining, ok := ConsumeBackupCode(hashed, plain[0])
	if !ok {
		t.Fatal("valid backup code was not accepted")
	}
	if len(remaining) != len(hashed)-1 {
		t.Fatalf("remaining has %d codes, want %d", len(remaining), len(hashed)-1)
	}

	// Single use: the same code no longer works against the remaining set.
	if _, ok := ConsumeBackupCode(remaining, plain[0]); ok {
		t.Error("consumed backup code was accepted a second time")
	}

	// An unknown code leaves the set untouched.
	after, ok := ConsumeBackupCode(remaining, "0000-0000-0000-0000-0000")
	if ok {
		t.Error("unknown backup code was accepted")
	}
	if len(after) != len(remaining) {
		t.Error("stored set changed on a non-matching code")
	}
}
