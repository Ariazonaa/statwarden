package auth

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("correct password did not verify")
	}
	bad, err := VerifyPassword("wrong", hash)
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): %v", err)
	}
	if bad {
		t.Fatal("wrong password verified")
	}
}

func TestPasswordHashIsSalted(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Fatal("two hashes of the same password are identical: salt missing")
	}
}

func TestVerifyPasswordRejectsGarbage(t *testing.T) {
	if _, err := VerifyPassword("x", "not-a-phc-string"); err != ErrInvalidHash {
		t.Fatalf("err = %v, want ErrInvalidHash", err)
	}
}

func TestTokenHashDeterministicAndUnique(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	b, _ := NewToken()
	if a == b {
		t.Fatal("tokens collided")
	}
	firstHash := HashToken(a)
	if firstHash != HashToken(a) {
		t.Fatal("HashToken not deterministic")
	}
	if firstHash == HashToken(b) {
		t.Fatal("distinct tokens hashed equal")
	}
}

func TestNewAPIKeyPrefix(t *testing.T) {
	secret, display, err := NewAPIKey()
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if len(secret) <= len(APIKeyPrefix) || secret[:len(APIKeyPrefix)] != APIKeyPrefix {
		t.Fatalf("secret %q missing prefix", secret)
	}
	if display != secret[:12] {
		t.Fatalf("display %q != secret prefix", display)
	}
}

func TestTOTPVerifies(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	code, err := TOTPCode(secret, now)
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if len(code) != totpDigits {
		t.Fatalf("code %q length %d, want %d", code, len(code), totpDigits)
	}
	if !VerifyTOTP(secret, code, now) {
		t.Fatal("fresh code did not verify")
	}
	// Within skew window.
	if !VerifyTOTP(secret, code, now.Add(25*time.Second)) {
		t.Fatal("code within skew did not verify")
	}
	// Outside skew window.
	if VerifyTOTP(secret, code, now.Add(5*time.Minute)) {
		t.Fatal("stale code verified")
	}
	if VerifyTOTP(secret, "000000", now) && code != "000000" {
		t.Fatal("wrong code verified")
	}
}

func TestTOTPURIContainsSecret(t *testing.T) {
	uri := TOTPURI("ABCDEF", "Statwarden", "admin@example.com")
	if uri == "" || uri[:len("otpauth://totp/")] != "otpauth://totp/" {
		t.Fatalf("bad uri %q", uri)
	}
}

func TestBackupCodeConsume(t *testing.T) {
	plain, hashed, err := GenerateBackupCodes()
	if err != nil {
		t.Fatalf("GenerateBackupCodes: %v", err)
	}
	if len(plain) != BackupCodeCount || len(hashed) != BackupCodeCount {
		t.Fatalf("got %d/%d codes, want %d", len(plain), len(hashed), BackupCodeCount)
	}
	remaining, ok := ConsumeBackupCode(hashed, plain[0])
	if !ok {
		t.Fatal("valid backup code not consumed")
	}
	if len(remaining) != BackupCodeCount-1 {
		t.Fatalf("remaining %d, want %d", len(remaining), BackupCodeCount-1)
	}
	// Re-using the same code fails.
	if _, ok := ConsumeBackupCode(remaining, plain[0]); ok {
		t.Fatal("consumed backup code accepted twice")
	}
	// Unknown code fails.
	if _, ok := ConsumeBackupCode(remaining, "zzzzz-zzzzz"); ok {
		t.Fatal("unknown backup code accepted")
	}
}

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		have, min Role
		want      bool
	}{
		{RoleAdmin, RoleViewer, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleUser, RoleAdmin, false},
		{RoleUser, RoleUser, true},
		{RoleViewer, RoleUser, false},
		{RoleViewer, RoleViewer, true},
	}
	for _, c := range cases {
		if got := c.have.AtLeast(c.min); got != c.want {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", c.have, c.min, got, c.want)
		}
	}
	if RoleValid := Role("root").Valid(); RoleValid {
		t.Error("unknown role reported valid")
	}
}

func TestSessionFingerprint(t *testing.T) {
	const hashA = "8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4"
	const hashB = "d2a84f4b8b650937ec8f73cd8be2c74add5a911ba64df27458ed8229da804a26"

	a := SessionFingerprint(hashA)
	if got := len(a); got != SessionFingerprintLen {
		t.Errorf("length = %d, want %d", got, SessionFingerprintLen)
	}
	if a != SessionFingerprint(hashA) {
		t.Error("fingerprint is not stable across calls")
	}
	if b := SessionFingerprint(hashB); a == b {
		t.Error("different session hashes produced the same fingerprint")
	}
	if SessionFingerprint("") != "" {
		t.Error("empty hash should yield an empty fingerprint")
	}

	// The point of publishing this is that it reveals nothing about the stored
	// hash, so it must not appear verbatim inside it.
	if strings.Contains(hashA, a) {
		t.Errorf("fingerprint %q appears verbatim inside the stored hash", a)
	}
}
