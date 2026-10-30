package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"

	"github.com/Ariazonaa/statwarden/internal/safego"
)

type passwordResetRequest struct {
	Email string `json:"email"`
}

type passwordResetConfirmRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// resetAcceptedBody is the ONE response the request endpoint ever produces. It
// is a package-level value rather than a literal at each return so that a future
// edit cannot accidentally give one branch a different body.
func resetAcceptedBody() map[string]string {
	return map[string]string{"status": "if the address belongs to an account, a reset link has been sent"}
}

// handleRequestPasswordReset issues a reset token and mails it.
//
// The response is identical for every outcome: address known, address unknown,
// account deactivated, mailer unconfigured, token generation failed, store write
// failed. All of them answer 202 with the same body. Branching the status, the
// body, or even the shape of the work done would turn this endpoint into an
// account-existence oracle, which is exactly what it must not be.
//
// The lookup is performed unconditionally for the same reason handleLogin runs a
// dummy argon2 hash on an unknown address: skipping the work on the
// unknown-address path is observable in the timing.
func (s *server) handleRequestPasswordReset(w http.ResponseWriter, r *http.Request) {
	// Tighter than the 60/min the public group already applies: this endpoint is
	// an outbound-mail amplifier pointed at a third party's inbox, so it gets the
	// same 10/min brake the login endpoint uses.
	if !s.loginLimiter.allow("reset:" + clientIP(r)) {
		writeProblemDetail(w, http.StatusTooManyRequests, "too many reset requests, slow down")
		return
	}
	var req passwordResetRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	email := strings.TrimSpace(req.Email)
	if err := validateEmail(email); err != nil {
		// Even a malformed address gets the success answer. A 400 here would say
		// "that is not a real address", and by contrast a 202 would say "that IS
		// a well-formed address" — a weaker oracle, but an oracle.
		writeJSON(w, http.StatusAccepted, resetAcceptedBody())
		return
	}

	// Deliberately unconditional and deliberately ignoring the error: every
	// branch below converges on the same response.
	creds, err := s.store.GetCredentialsByEmail(r.Context(), email)
	if err == nil && creds.IsActive && s.resetLimiter.allow("reset-account:"+strings.ToLower(email)) {
		// Issued off the request path.
		//
		// The lookup above is unconditional, but issuing was not: minting a
		// token, writing it, writing an audit row and handing a message to the
		// SMTP server all happened only for an address that exists. Status and
		// body were identical, the RUNTIME was not, and a few hundred
		// milliseconds is a comfortable oracle over a few dozen requests.
		// Everything that distinguishes the two paths now happens after the
		// response, so the caller sees the same work either way.
		//
		// WithoutCancel because the request context dies with the response, and
		// this deliberately outlives it. 202 Accepted is exactly the promise
		// being made: taken for processing, outcome not reported.
		ctx := context.WithoutCancel(r.Context())
		s.resetInflight.Add(1)
		safego.Go(s.logger, "password reset issue", func() {
			s.issuePasswordResetAsync(ctx, r, creds.ID, email)
		})
	}
	writeJSON(w, http.StatusAccepted, resetAcceptedBody())
}

// issuePasswordResetAsync runs the issuing work off the request path. It exists
// as a named method so the goroutine has a stack frame worth reading in a panic
// trace, and so a test can call the synchronous half directly.
func (s *server) issuePasswordResetAsync(ctx context.Context, r *http.Request, userID int64, email string) {
	// Panic containment is the caller's, through safego.Go — one mechanism for
	// this rather than a hand-rolled recover here and a shared helper
	// everywhere else.
	defer s.resetInflight.Done()
	s.issuePasswordReset(r.WithContext(ctx), userID, email)
}

// issuePasswordReset mints, persists and mails a reset token. Every failure is
// swallowed into a log line: the caller must answer identically regardless.
func (s *server) issuePasswordReset(r *http.Request, userID int64, email string) {
	token, err := auth.NewToken()
	if err != nil {
		s.logger.Error("password reset token generation failed", "err", err)
		return
	}
	now := s.now()
	// Only the digest is persisted. The plaintext exists in exactly two places:
	// this variable and the mail body. It is never logged and never audited.
	_, err = s.store.CreatePasswordReset(r.Context(), PasswordReset{
		UserID:      userID,
		TokenHash:   auth.HashToken(token),
		ExpiresAt:   now.Add(passwordResetTTL),
		RequestedIP: clientIP(r),
		CreatedAt:   now,
	})
	if err != nil {
		s.logger.Error("password reset persist failed", "err", err)
		return
	}
	s.audit(r.Context(), &userID, "user.password_reset_request", "user", &userID, clientIP(r),
		map[string]string{"email": email})
	s.sendPasswordResetMail(r.Context(), email, token)
}

// sendPasswordResetMail delivers the reset link. The link is built from the
// configured public base URL and NEVER from the request Host header: it carries
// a credential, so a header-derived link would let anyone who can set a Host
// header redirect a victim's reset token to a domain they control.
func (s *server) sendPasswordResetMail(ctx context.Context, email, token string) {
	if s.mailer == nil {
		return
	}
	link := fmt.Sprintf("%s/reset-password?token=%s",
		strings.TrimRight(s.baseURL, "/"), url.QueryEscape(token))
	subject := "Reset your Statwarden password"
	text := fmt.Sprintf("A password reset was requested for your Statwarden account.\n\n"+
		"Open this link to choose a new password:\n\n%s\n\n"+
		"The link expires in %d hour(s) and can be used once. "+
		"If you did not request this, you can ignore this mail — your password stays unchanged.\n",
		link, int(passwordResetTTL.Hours()))
	html := fmt.Sprintf(`<p>A password reset was requested for your Statwarden account.</p>`+
		`<p><a href="%s">Choose a new password</a></p>`+
		`<p>The link expires in %d hour(s) and can be used once. If you did not request this, `+
		`you can ignore this mail — your password stays unchanged.</p>`,
		link, int(passwordResetTTL.Hours()))
	if err := s.mailer.Send(ctx, email, subject, text, html); err != nil {
		s.logger.Warn("send password reset mail", "error", err)
	}
}

// handleConfirmPasswordReset redeems a token and sets the new password.
//
// Every failure mode — unknown token, expired token, already-redeemed token,
// missing token — produces the identical 400. A distinct 410 for "expired"
// would leak that the token once existed. Only a password that fails the
// strength rule gets its own message, because that is a property of the input
// the caller just typed and reveals nothing about any token or account.
//
// On success the caller is NOT logged in. Issuing a session from a mailed link
// would mean that reading the mailbox is enough to be authenticated; requiring
// the new password to be used keeps the credential the thing that grants access.
func (s *server) handleConfirmPasswordReset(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimiter.allow("reset-confirm:" + clientIP(r)) {
		writeProblemDetail(w, http.StatusTooManyRequests, "too many attempts, slow down")
		return
	}
	var req passwordResetConfirmRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeProblemDetail(w, http.StatusBadRequest, invalidResetTokenDetail)
		return
	}
	// Password strength is checked BEFORE the token is redeemed, so a weak
	// password does not burn a single-use token.
	if err := validatePassword(req.NewPassword); err != nil {
		writeProblemDetail(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeProblemDetail(w, http.StatusInternalServerError, "hash password")
		return
	}

	// The store marks the token used and writes the password in one transaction,
	// so the token cannot be redeemed twice under concurrency, and invalidates
	// the user's other outstanding tokens so a stale mail cannot be replayed.
	reset, err := s.store.ConsumePasswordReset(r.Context(), auth.HashToken(token), hash, s.now())
	if err != nil {
		// Unknown, expired and already-used all arrive here as ErrNotFound and
		// are answered identically.
		writeProblemDetail(w, http.StatusBadRequest, invalidResetTokenDetail)
		return
	}

	// Empty exceptID excludes nothing: every session dies, including any the
	// attacker who prompted the reset may be holding.
	//
	// A failure here is reported rather than logged and passed over. The
	// password HAS changed at this point, so the answer cannot simply be an
	// error — it has to say which half happened, or the user will assume the
	// recovery worked and stop looking.
	if err := s.store.DeleteSessionsByUser(r.Context(), reset.UserID, ""); err != nil {
		s.logger.Error("password reset session invalidation failed", "user_id", reset.UserID, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError,
			"your password was changed, but existing sessions could not be signed out; "+
				"sign in and revoke them from your profile")
		return
	}
	// API keys outlive sessions by design, which is exactly why a recovery has
	// to take them with it. Killing the sessions and leaving the keys means the
	// account is still reachable by whoever prompted the reset, and the owner
	// has no reason to suspect it. Keys the owner wants back can be reissued;
	// a key the attacker keeps cannot be taken away later, because the owner
	// does not know it exists.
	n, err := s.store.DeleteAPIKeysByUser(r.Context(), reset.UserID)
	if err != nil {
		// Reported, not merely logged — the same treatment the session
		// revocation above gets, because the two failures are the same failure.
		// A surviving API key is arguably the worse of them: it outlives every
		// session by design and the owner cannot see it.
		s.logger.Error("password reset API key revocation failed",
			"user_id", reset.UserID, "err", err)
		writeProblemDetail(w, http.StatusInternalServerError,
			"your password was changed, but existing API keys could not be revoked; "+
				"sign in and revoke them from your profile")
		return
	}
	if n > 0 {
		s.logger.Info("password reset revoked API keys", "user_id", reset.UserID, "count", n)
	}
	s.audit(r.Context(), &reset.UserID, "user.password_reset", "user", &reset.UserID, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}

// Per-address reset budget.
//
// Small count, long window: a person who genuinely needs several resets in an
// hour is unusual, and a mailbox that has received three unrequested reset
// mails has already learned everything a fourth would tell it.
//
// The trade-off is real and worth stating rather than discovering. Capping per
// address means somebody who knows an address can spend its budget and keep the
// owner from getting a reset mail for the rest of the window. That is a denial
// of service on account recovery, bounded to an hour and self-healing. The
// alternative — no per-address cap — leaves this endpoint able to bury a third
// party's inbox from rotating clients, which the per-IP limiter cannot see.
// Neither is free; a delay that ends by itself is the better of the two.
const (
	resetPerAddressLimit  = 3
	resetPerAddressWindow = time.Hour
)

// invalidResetTokenDetail is the single message every token failure produces.
const invalidResetTokenDetail = "invalid or expired reset token"
