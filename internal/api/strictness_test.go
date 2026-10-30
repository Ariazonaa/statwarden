package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/notify/secrets"
)

// TestAFailedSealIsRefusedNotSilentlyStoredInPlaintext
//
// A notification config carries SMTP passwords and webhook tokens, and is
// sealed at rest. When EncryptConfig failed the code fell through and stored
// the PLAINTEXT while answering 201, so the at-rest guarantee was gone and
// nothing said so. EncryptConfig fails when the system random source does —
// rare, and exactly the moment silence is worst.
func TestAFailedSealIsRefusedNotSilentlyStoredInPlaintext(t *testing.T) {
	h := newHarnessWithBox(t, failBox{})
	admin := h.setupAdmin()

	body := notificationRequest{
		Name: "smtp", Type: "fake",
		Config: json.RawMessage(`{"password":"s3cr3t"}`),
	}
	rec := h.requestWith(http.MethodPost, "/api/v1/notifications", body, admin)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("create with a failing sealer = %d, want 500; body %s",
			rec.Code, rec.Body.String())
	}

	// And nothing was written. A row that exists with the secret in the clear
	// is the failure; a refused request is merely an outage.
	list := h.requestWith(http.MethodGet, "/api/v1/notifications", nil, admin)
	if strings.Contains(list.Body.String(), "s3cr3t") {
		t.Fatalf("the plaintext secret reached the store: %s", list.Body.String())
	}
}

// The update path had the same hole.
func TestAFailedSealOnUpdateIsAlsoRefused(t *testing.T) {
	h := newHarnessWithBox(t, failBox{})
	admin := h.setupAdmin()
	rec := h.requestWith(http.MethodPut, "/api/v1/notifications/1", notificationRequest{
		Name: "smtp", Type: "fake", Config: json.RawMessage(`{"password":"s3cr3t"}`),
	}, admin)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("update with a failing sealer = %d, want 500; body %s",
			rec.Code, rec.Body.String())
	}
}

// TestTrailingJSONIsRejected
//
// encoding/json's Decode stops at the end of the first value and never looks
// further, so `{"a":1} {"a":2}` was accepted with the second object silently
// discarded — or the first, depending on which end another parser reads from.
// A proxy, a log pipeline and this server disagreeing about what was requested
// is exactly what the unknown-field strictness elsewhere exists to prevent.
func TestTrailingJSONIsRejected(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	for _, tc := range []struct{ name, body string }{
		{"a second object", `{"name":"a","type":"fake","interval_seconds":60}{"name":"b","type":"fake","interval_seconds":60}`},
		{"trailing garbage", `{"name":"a","type":"fake","interval_seconds":60}xyz`},
		{"trailing array", `{"name":"a","type":"fake","interval_seconds":60}[1,2]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.requestRaw(http.MethodPost, "/api/v1/monitors", tc.body, admin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s = %d, want 400; body %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// Whitespace after the value is not trailing data — rejecting it would break
// every client that pretty-prints its request bodies.
func TestTrailingWhitespaceIsFine(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()
	rec := h.requestRaw(http.MethodPost, "/api/v1/monitors",
		"{\"name\":\"a\",\"type\":\"fake\",\"interval_seconds\":60}\n\n  \t\n", admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("trailing whitespace = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}

// TestLockoutDoesNotRevealWhichAccountsExist
//
// The lockout was consulted only AFTER the account had been resolved, so five
// failures against a real address started answering 429 while an invented one
// kept answering 401. That sorts real accounts from imaginary ones without ever
// guessing a password — and the 401/429 split is far easier to read than any
// timing difference.
func TestLockoutDoesNotRevealWhichAccountsExist(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin() // admin@example.com exists

	// Each series gets its own client address. Sharing one would put both
	// inside the same per-IP request budget, which trips at 10/min and answers
	// 429 for everything after that — masking the very difference this test is
	// looking for. The first draft did exactly that and passed against the bug.
	attempt := func(email, ip string) int {
		return h.requestFrom(http.MethodPost, "/api/v1/auth/login",
			loginRequest{Email: email, Password: "definitely-wrong"}, ip).Code
	}

	// Past the lockout threshold, within the per-IP budget, and compared as
	// sequences rather than as any single response.
	var real, fake []int
	for range 8 {
		real = append(real, attempt("admin@example.com", "203.0.113.21:1000"))
		fake = append(fake, attempt("nobody@example.com", "203.0.113.22:1000"))
	}
	for i := range real {
		if real[i] != fake[i] {
			t.Fatalf("attempt %d: existing account answered %d, unknown answered %d — "+
				"the difference identifies which addresses are registered",
				i+1, real[i], fake[i])
		}
	}
	// And the lock did engage; otherwise this test proves only that nothing
	// happens at all.
	if real[len(real)-1] != http.StatusTooManyRequests {
		t.Fatalf("last attempt = %d, want 429: the lockout never triggered, so "+
			"the comparison above is vacuous", real[len(real)-1])
	}
}

// A locked bucket belongs to one client, not to the account. Otherwise anyone
// could lock any account they can name, which is a denial of service handed out
// for free.
func TestOneClientCannotLockAnAccountForEveryone(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	const victim = "admin@example.com"
	attackerIP := "203.0.113.7:1111"
	for range 8 {
		h.requestFrom(http.MethodPost, "/api/v1/auth/login",
			loginRequest{Email: victim, Password: "wrong"}, attackerIP)
	}
	if code := h.requestFrom(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: victim, Password: "wrong"}, attackerIP).Code; code != http.StatusTooManyRequests {
		t.Fatalf("the attacker's own bucket = %d, want 429", code)
	}

	// The owner, from their own address, is unaffected.
	rec := h.requestFrom(http.MethodPost, "/api/v1/auth/login",
		loginRequest{Email: victim, Password: "supersecret"}, "198.51.100.4:2222")
	if rec.Code != http.StatusOK {
		t.Fatalf("the owner's login = %d, want 200; body %s — a stranger locked "+
			"them out of their own account", rec.Code, rec.Body.String())
	}
}

// A mailbox belongs to a third party, so the per-IP limiter cannot bound what
// this endpoint sends to it. Exceeding the per-address budget still answers 202,
// because a limiter that announced itself would answer the very question the
// endpoint refuses to.
func TestResetMailIsBoundedPerAddress(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin()

	for i := range 6 {
		resp := h.requestFrom(http.MethodPost, "/api/v1/auth/password-reset",
			passwordResetRequest{Email: "admin@example.com"},
			// A fresh client each time, so only the per-address budget applies.
			"203.0.113."+itoa(i+1)+":5555")
		if resp.Code != http.StatusAccepted {
			t.Fatalf("request %d = %d, want 202 regardless of the budget", i+1, resp.Code)
		}
	}
	h.srv.resetInflight.Wait()
	if got := h.mails.count(); got > resetPerAddressLimit {
		t.Fatalf("sent %d mails to one address, want at most %d", got, resetPerAddressLimit)
	}
	if h.mails.count() == 0 {
		t.Fatal("no mail at all: the budget is not a budget, it is an off switch")
	}
}

// TestRenamingAChannelKeepsItsConfiguration
//
// A channel's config is hidden wholesale on read — unlike a monitor's, which is
// masked field by field and can be handed back — so an editor opened on an
// existing channel starts blank. Saving after changing only the name submitted
// that blank form as the new configuration. For most providers a required field
// is then missing and the write is rejected, which makes renaming impossible
// without re-typing the SMTP password; for a provider whose fields are all
// optional it went through and the credentials were simply gone, and the
// channel stopped delivering.
func TestRenamingAChannelKeepsItsConfiguration(t *testing.T) {
	box, err := secrets.New("test-key-for-channel-edit")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/notifications", notificationRequest{
		Name: "ops", Type: "fake",
		Config: json.RawMessage(`{"token":"s3cr3t"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &created)

	// A rename that says nothing about the configuration. The absent field is
	// the whole point: the client is stating that it did not change it.
	rec = h.requestRaw(http.MethodPut, "/api/v1/notifications/"+itoa64(created.ID),
		`{"name":"ops (renamed)","type":"fake"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	stored, err := h.store.GetNotification(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	plain, err := box.DecryptConfig(stored.Config)
	if err != nil {
		t.Fatalf("the stored config is not readable after a rename: %v", err)
	}
	if !strings.Contains(string(plain), "s3cr3t") {
		t.Fatalf("the rename wiped the channel's credentials: %s", plain)
	}
	if stored.Name != "ops (renamed)" {
		t.Fatalf("name = %q, want the new one", stored.Name)
	}
}

// An explicitly supplied config still replaces the stored one — otherwise
// changing a credential would be impossible.
func TestAnExplicitConfigStillReplaces(t *testing.T) {
	box, err := secrets.New("test-key-for-channel-replace")
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	h := newHarnessWithBox(t, box)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/notifications", notificationRequest{
		Name: "ops", Type: "fake", Config: json.RawMessage(`{"token":"old"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	mustJSON(t, rec, &created)

	rec = h.requestWith(http.MethodPut, "/api/v1/notifications/"+itoa64(created.ID),
		notificationRequest{Name: "ops", Type: "fake", Config: json.RawMessage(`{"token":"new"}`)}, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d, body %s", rec.Code, rec.Body.String())
	}
	stored, err := h.store.GetNotification(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("GetNotification: %v", err)
	}
	plain, err := box.DecryptConfig(stored.Config)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !strings.Contains(string(plain), "new") || strings.Contains(string(plain), "old") {
		t.Fatalf("the explicit config did not replace the stored one: %s", plain)
	}
}

// TestAMaskIsNotACredential
//
// Read responses mask secrets. Submitting a mask back is meaningful only on an
// UPDATE, where there is a stored value to keep — on a create there is nothing
// to restore, and the mask was stored as the credential itself. The read path
// then masked it again, so a monitor whose token is literally "__redacted__"
// looked exactly like one with a real secret while every check failed on a
// value the operator could not see was wrong. Copying an existing monitor's
// configuration out of the API is all it takes.
func TestAMaskIsNotACredential(t *testing.T) {
	h := newHarness(t)
	h.monReg.Register(secretChecker{})
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "copied", Type: "secretty", Interval: 60,
		Config: json.RawMessage(`{"url":"https://x.test","token":"` + monitorSecretPlaceholder + `"}`),
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with a masked token = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("the refusal does not name the offending field: %s", rec.Body.String())
	}

	// A real value is of course fine.
	rec = h.requestWith(http.MethodPost, "/api/v1/monitors", monitorRequest{
		Name: "real", Type: "secretty", Interval: 60,
		Config: json.RawMessage(`{"url":"https://x.test","token":"actual-secret"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a real token = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}

// The same for a channel: reads answer {"$encrypted":true}, and sending that
// back stored it AS the configuration — after which the channel had no usable
// settings and simply never delivered, while looking no different from a
// working one.
func TestTheChannelRedactionMarkerIsNotAConfiguration(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin()

	rec := h.requestWith(http.MethodPost, "/api/v1/notifications", notificationRequest{
		Name: "ops", Type: "fake", Config: json.RawMessage(`{"$encrypted":true}`),
	}, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create with the marker = %d, want 400; body %s", rec.Code, rec.Body.String())
	}

	// A genuine sealed envelope carries a base64 string, not a boolean, and is
	// not what this guard is looking for.
	rec = h.requestWith(http.MethodPost, "/api/v1/notifications", notificationRequest{
		Name: "ops", Type: "fake", Config: json.RawMessage(`{"token":"real"}`),
	}, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with a real config = %d, want 201; body %s", rec.Code, rec.Body.String())
	}
}
