package auth

import (
	"strings"
	"testing"
	"time"
)

// An empty or whitespace-only secret must never verify.
//
// base32 decodes "" successfully, so the key became zero-length and HOTP over a
// zero-length key is a fixed function of the time step — computable by anyone. Any
// account reaching VerifyTOTP in that state would accept an attacker's code as its
// second factor.
//
// No caller reaches it today (totp_enabled is only written together with a secret,
// and an unopenable envelope is handed back verbatim so it fails the base32
// decode), but users.totp_enabled has no constraint tying it to a non-empty
// users.totp_secret, so the state is representable in SQL. The primitive refuses it
// rather than relying on four separate arguments elsewhere.
func TestVerifyTOTPRefusesAnEmptySecret(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	for _, secret := range []string{"", " ", "\t\n"} {
		// The code an attacker would compute for a zero-length key.
		guess := hotp(nil, uint64(now.Unix())/30)
		if VerifyTOTP(secret, guess, now) {
			t.Errorf("secret %q verified the empty-key code %q", secret, guess)
		}
		// And nothing else may verify either.
		if VerifyTOTP(secret, "000000", now) {
			t.Errorf("secret %q verified an arbitrary code", secret)
		}
	}

	// A real secret still works, so the guard did not break verification.
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	code, err := TOTPCode(secret, now)
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if !VerifyTOTP(secret, code, now) {
		t.Fatal("a valid code for a real secret did not verify")
	}
}

// The three ordinary ways of typing a backup code all work.
//
// The code is hashed before comparison, so a presented value had to match the
// generated spelling byte for byte: uppercase hex, the bare form without the
// cosmetic dashes, and a copy with surrounding whitespace were all refused. A
// backup code is used exactly when the authenticator is gone, so a rejected VALID
// code is a locked-out account with no self-service way back.
func TestConsumeBackupCodeAcceptsTheObviousSpellings(t *testing.T) {
	plain, hashed, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	canonical := plain[0]

	variants := map[string]string{
		"canonical":       canonical,
		"upper case":      strings.ToUpper(canonical),
		"without dashes":  strings.ReplaceAll(canonical, "-", ""),
		"padded by paste": "  " + canonical + "\n",
		"upper, no dash":  strings.ToUpper(strings.ReplaceAll(canonical, "-", "")),
	}
	for name, variant := range variants {
		remaining, ok := ConsumeBackupCode(hashed, variant)
		if !ok {
			t.Errorf("%s (%q) was refused", name, variant)
			continue
		}
		if len(remaining) != len(hashed)-1 {
			t.Errorf("%s: remaining = %d, want %d", name, len(remaining), len(hashed)-1)
		}
	}

	// A wrong code is still wrong, and normalising did not widen what matches.
	if _, ok := ConsumeBackupCode(hashed, "ffff-ffff-ffff-ffff-ffff"); ok {
		t.Fatal("an unrelated code was accepted")
	}
	if _, ok := ConsumeBackupCode(hashed, ""); ok {
		t.Fatal("the empty string was accepted as a backup code")
	}
	// Truncating a real code must not match it.
	if _, ok := ConsumeBackupCode(hashed, canonical[:9]); ok {
		t.Fatal("a truncated code was accepted")
	}
}
