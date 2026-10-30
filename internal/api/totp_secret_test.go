package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

// enrollSecret starts TOTP enrolment and returns the plaintext secret the
// server handed the authenticator app.
func enrollSecret(t *testing.T, h *harness, sess *session) string {
	t.Helper()
	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/enroll", stepUp{Password: "supersecret"}, sess)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	mustJSON(t, rec, &body)
	if body["secret"] == "" {
		t.Fatal("enrolment returned no secret")
	}
	return body["secret"]
}

// storedTOTPSecret reads users.totp_secret exactly as the database holds it,
// bypassing the server's opener.
func storedTOTPSecret(t *testing.T, h *harness, userID int64) string {
	t.Helper()
	creds, err := h.store.GetCredentialsByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("GetCredentialsByID: %v", err)
	}
	return creds.TOTPSecret
}

// TestTOTPSecretSealedAtRest is the plaintext-second-factor regression.
//
// Backup codes were hashed from the start, but users.totp_secret sat in the
// column verbatim, so a database leak alone handed an attacker every account's
// second factor. It is now sealed with the same SecretsBox that seals
// monitors.config, and the whole enrol → verify → log-in path runs through the
// seal.
func TestTOTPSecretSealedAtRest(t *testing.T) {
	box, err := secrets.New("test-key")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	secret := enrollSecret(t, h, admin)

	stored := storedTOTPSecret(t, h, admin.userID)
	if !configEncrypted(json.RawMessage(stored)) {
		t.Fatalf("stored totp_secret is not a sealed envelope: %q", stored)
	}
	if strings.Contains(stored, secret) {
		t.Fatalf("the plaintext secret is present in the stored column: %q", stored)
	}

	// The pending secret is still usable: enrolment verifies through the seal.
	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/verify", totpVerifyRequest{Code: code}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify = %d, body %s", rec.Code, rec.Body.String())
	}
	if s := storedTOTPSecret(t, h, admin.userID); !configEncrypted(json.RawMessage(s)) {
		t.Fatalf("totp_secret is plaintext after verify: %q", s)
	}

	// And the LOGIN path opens it too — the read that is easiest to forget,
	// because it lives in auth_handlers.go rather than the enrolment handlers.
	fresh, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if _, c := h.login("admin@example.com", "supersecret", fresh); c != http.StatusOK {
		t.Fatalf("login with 2FA against a sealed secret = %d, want 200", c)
	}
	// A wrong code is still refused, so the seal did not turn verification into
	// a rubber stamp.
	if _, c := h.login("admin@example.com", "supersecret", "000000"); c != http.StatusUnauthorized {
		t.Fatalf("login with a bogus 2FA code = %d, want 401", c)
	}
}

// TestTOTPLegacyPlaintextSecretStillAuthenticates pins the rollout contract
// migration 0013 (and 0009 before it) documents: the encryption key lives only
// in the application, so no migration can rewrite existing rows. A secret that
// was written before the boundary existed must keep working on read, and must be
// re-sealed by the first write that touches it.
func TestTOTPLegacyPlaintextSecretStillAuthenticates(t *testing.T) {
	box, err := secrets.New("test-key")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	// A pre-migration row: the base32 secret written straight into the column,
	// deliberately bypassing the server's sealing wrapper.
	legacy, err := auth.NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	if err := h.store.SetUserTOTP(context.Background(), admin.userID, legacy, true, nil); err != nil {
		t.Fatalf("seed legacy secret: %v", err)
	}

	code, err := auth.TOTPCode(legacy, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if _, c := h.login("admin@example.com", "supersecret", code); c != http.StatusOK {
		t.Fatalf("login against a legacy plaintext secret = %d, want 200", c)
	}

	// The next write upgrades the row without changing the secret. Regenerating
	// backup codes takes the second factor away from whoever holds the old
	// ones, so it re-authenticates: with 2FA on, that means a code as well.
	stepUpCode, err := auth.TOTPCode(legacy, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/backup-codes",
		stepUp{Password: "supersecret", TOTPCode: stepUpCode}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate backup codes = %d, body %s", rec.Code, rec.Body.String())
	}
	stored := storedTOTPSecret(t, h, admin.userID)
	if !configEncrypted(json.RawMessage(stored)) {
		t.Fatalf("legacy row was not re-sealed on its next write: %q", stored)
	}
	if strings.Contains(stored, legacy) {
		t.Fatalf("the legacy plaintext survives in the column: %q", stored)
	}
	fresh, err := auth.TOTPCode(legacy, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if _, c := h.login("admin@example.com", "supersecret", fresh); c != http.StatusOK {
		t.Fatalf("login after the re-seal = %d, want 200 (the secret must be unchanged)", c)
	}
}

// TestTOTPSecretWriteFailsClosed proves a sealing failure aborts the write
// instead of falling back to storing the secret in the clear — the same
// fail-closed rule monitorInput applies to monitor configs.
func TestTOTPSecretWriteFailsClosed(t *testing.T) {
	h := newHarnessWithBox(t, failBox{})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/enroll", stepUp{Password: "supersecret"}, admin)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("enroll with a failing box = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if s := storedTOTPSecret(t, h, admin.userID); s != "" {
		t.Fatalf("a secret was persisted despite the seal failing: %q", s)
	}
}

// TestTOTPSecretPlaintextWithoutBox pins DEV_MODE: with no SecretsBox the column
// keeps the plaintext it always held and the flow is unaffected. A boundary that
// only worked with a key configured would make 2FA unusable in the project's own
// dev mode.
func TestTOTPSecretPlaintextWithoutBox(t *testing.T) {
	h := newHarness(t) // no box
	admin := h.setupAdmin()

	secret := enrollSecret(t, h, admin)
	if stored := storedTOTPSecret(t, h, admin.userID); stored != secret {
		t.Fatalf("stored secret = %q, want the plaintext %q", stored, secret)
	}
	code, err := auth.TOTPCode(secret, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/me/totp/verify", totpVerifyRequest{Code: code}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify without a box = %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestTOTPSecretOpenerNeverYieldsEmpty guards the sharp edge in auth.VerifyTOTP:
// base32 decodes "" successfully, so verification against an empty secret runs
// against an empty HMAC key whose codes anyone can compute. An envelope that
// cannot be opened — a changed ENCRYPTION_KEY, or a database restored into a
// DEV_MODE process — must therefore degrade to something that CANNOT verify,
// never to "".
func TestTOTPSecretOpenerNeverYieldsEmpty(t *testing.T) {
	box, err := secrets.New("the-real-key")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	sealer := &server{box: box}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	sealed, err := sealer.sealTOTPSecret(secret)
	if err != nil {
		t.Fatalf("sealTOTPSecret: %v", err)
	}
	if got := sealer.openTOTPSecret(sealed); got != secret {
		t.Fatalf("round trip = %q, want %q", got, secret)
	}

	wrongKey, err := secrets.New("a-different-key")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	for name, s := range map[string]*server{
		"wrong key": {box: wrongKey},
		"no box":    {},
	} {
		opened := s.openTOTPSecret(sealed)
		if opened == "" {
			t.Errorf("%s: opener returned the empty string, which VerifyTOTP would "+
				"verify against an empty HMAC key", name)
			continue
		}
		code, err := auth.TOTPCode(secret, time.Now())
		if err != nil {
			t.Fatalf("TOTPCode: %v", err)
		}
		if auth.VerifyTOTP(opened, code, time.Now()) {
			t.Errorf("%s: an unopenable secret still verified a code", name)
		}
		// Re-sealing what could not be opened must not bury it under a second
		// layer; the value is stored exactly as it was found.
		resealed, err := s.sealTOTPSecret(opened)
		if err != nil {
			t.Fatalf("%s: sealTOTPSecret: %v", name, err)
		}
		if resealed != sealed {
			t.Errorf("%s: re-sealed an unopenable envelope: %q", name, resealed)
		}
	}
}

// TestCredentialReadsOpenTheTOTPSecret is a source-level guard. The TOTP secret
// is read on the login path as well as the enrolment ones, and a handler that
// reaches for the store directly gets the SEALED value — which fails silently as
// "wrong code" rather than loudly as a compile error, because the whole test
// suite runs against MemStore. Routing every read through credentialsByID /
// credentialsByLogin and every write through setUserTOTP is what makes the rule
// hold, and this keeps it that way.
//
// GetCredentialsByEmail is deliberately NOT wrapped: that name is what
// oidc_test.go's S2 guard greps for, and hiding it behind a helper would blunt
// that guard. Its only caller never touches the TOTP secret.
func TestCredentialReadsOpenTheTOTPSecret(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob sources: %v", err)
	}
	if len(sources) == 0 {
		t.Fatal("no source files found; the guard would pass vacuously")
	}
	banned := []string{
		"store.GetCredentialsByID(",
		"store.GetCredentialsByLogin(",
		"store.SetUserTOTP(",
	}
	for _, name := range sources {
		// totp_secret.go holds the wrappers themselves; memstore.go and
		// pgstore.go DEFINE the methods rather than calling them.
		if name == "totp_secret.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := readSourceFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, call := range banned {
			if strings.Contains(raw, call) {
				t.Errorf("%s calls %s directly. Go through credentialsByID / "+
					"credentialsByLogin / setUserTOTP instead: users.totp_secret is "+
					"sealed at rest, and a direct call reads or writes the envelope.",
					name, call)
			}
		}
	}
}

// TestResealBackfillsSecretsNobodyTouches
//
// Migration 0013 could not encrypt existing rows and left each account's next
// write to re-seal its own secret. An account with 2FA enabled that nobody
// touches has no next write, so its secret would stay readable in a database
// dump for as long as the account exists — and those are precisely the accounts
// least likely to notice.
func TestResealBackfillsSecretsNobodyTouches(t *testing.T) {
	box, err := secrets.New("test-key-for-reseal")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	legacy, err := auth.NewTOTPSecret()
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	// A pre-migration row, written straight past the sealing wrapper.
	if err := h.store.SetUserTOTP(context.Background(), admin.userID, legacy, true, nil); err != nil {
		t.Fatalf("seed legacy secret: %v", err)
	}
	if configEncrypted(json.RawMessage(storedTOTPSecret(t, h, admin.userID))) {
		t.Fatal("the fixture is already sealed; this test would prove nothing")
	}

	n, err := ResealTOTPSecrets(context.Background(), h.store, box, nil)
	if err != nil {
		t.Fatalf("ResealTOTPSecrets: %v", err)
	}
	if n != 1 {
		t.Fatalf("sealed %d secrets, want 1", n)
	}
	stored := storedTOTPSecret(t, h, admin.userID)
	if !configEncrypted(json.RawMessage(stored)) {
		t.Fatalf("the legacy row is still plaintext: %q", stored)
	}
	if strings.Contains(stored, legacy) {
		t.Fatalf("the plaintext survives in the column: %q", stored)
	}

	// The account still works: re-sealing must not change what the secret IS.
	code, err := auth.TOTPCode(legacy, time.Now())
	if err != nil {
		t.Fatalf("TOTPCode: %v", err)
	}
	if _, c := h.login("admin@example.com", "supersecret", code); c != http.StatusOK {
		t.Fatalf("login after the re-seal = %d, want 200", c)
	}

	// Idempotent: a second pass finds nothing to do.
	again, err := ResealTOTPSecrets(context.Background(), h.store, box, nil)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if again != 0 {
		t.Fatalf("second pass sealed %d, want 0", again)
	}
}
