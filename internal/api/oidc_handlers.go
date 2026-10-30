package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// oidcFlow is the transient per-flow state, carried in an HttpOnly cookie.
//
// It holds the PKCE verifier, the state and nonce it must later match, and — in
// link mode — the id of the user the resulting identity attaches to. Binding the
// user id into the cookie at link-start is what makes link mode safe: the
// callback never has to trust anything the identity provider says about who the
// caller is.
type oidcFlow struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	// LinkUserID is nil for a login flow.
	LinkUserID *int64 `json:"u,omitempty"`
}

// Coarse, stable error codes. The callback is a browser navigation, so failures
// redirect to the login page rather than rendering problem+json. The codes are
// deliberately blunt: a caller learns that SSO did not work, not which of a
// dozen checks rejected them.
const (
	oidcErrFailed     = "sso_failed"
	oidcErrNoAccount  = "sso_no_account"
	oidcErrEmailTaken = "sso_email_taken"
	oidcErrInactive   = "sso_inactive"
	oidcErrLinked     = "sso_already_linked"
)

// handleOIDCProviders lists the configured providers for the login page.
// It returns the local id and the display name and nothing else — never the
// issuer's client secret, and never anything that would describe the flow.
func (s *server) handleOIDCProviders(w http.ResponseWriter, r *http.Request) {
	type view struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	out := make([]view, 0, len(s.oidc))
	for _, p := range s.oidc {
		name := p.DisplayName
		if name == "" {
			name = p.ID
		}
		out = append(out, view{ID: p.ID, DisplayName: name})
	}
	writeJSON(w, http.StatusOK, out)
}

// oidcRedirectURI builds the callback URL the provider must redirect back to.
//
// It is DERIVED from the configured public base URL and never received. Building
// it from r.Host or X-Forwarded-Host would let anyone who can set a header point
// the authorization code — a credential — at a domain they control.
func (s *server) oidcRedirectURI(providerID string) string {
	return strings.TrimRight(s.baseURL, "/") + "/api/v1/auth/oidc/" + url.PathEscape(providerID) + "/callback"
}

// startOIDCFlow mints the flow state, stores it in the cookie and returns the
// authorize URL.
func (s *server) startOIDCFlow(w http.ResponseWriter, r *http.Request, p *OIDCProvider, linkUserID *int64) (string, error) {
	meta, err := p.metadata(r.Context())
	if err != nil {
		return "", err
	}
	state, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	nonce, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	verifier, challenge, err := newPKCE()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(oidcFlow{
		Provider: p.ID, State: state, Nonce: nonce, Verifier: verifier, LinkUserID: linkUserID,
	})
	if err != nil {
		return "", err
	}
	cookieValue := s.signFlow(base64URL(raw))
	authorizeURL, err := p.authorizeURL(meta, s.oidcRedirectURI(p.ID), state, nonce, challenge)
	if err != nil {
		return "", err
	}
	// The cookie is HttpOnly, so script on the page cannot read the verifier;
	// its Path is scoped to the OIDC routes; and its lifetime is the flow's, not
	// the session's.
	http.SetCookie(w, s.ceremonyCookie(oidcCookie,
		cookieValue, oidcCookiePath, int(oidcFlowTTL.Seconds())))
	return authorizeURL, nil
}

// signFlow appends an HMAC over the encoded flow payload, keyed on
// SESSION_SECRET.
//
// The cookie is otherwise plain JSON that its holder can rewrite, which is what
// made the link-mode user id forgeable. The callback also checks that id
// against the caller's live session, so this is the second of two independent
// gates rather than the only one — but an unsigned bearer of flow state is
// worth closing on its own, and it gives SESSION_SECRET an actual job: it was
// mandatory configuration that nothing outside the config parser ever read, so
// rotating it had no effect whatsoever.
//
// An empty secret (DEV_MODE) produces an unsigned value and verification then
// accepts one, so local development without the variable keeps working.
func (s *server) signFlow(payload string) string {
	if s.sessionSecret == "" {
		return payload
	}
	return payload + "." + flowSignature(s.sessionSecret, payload)
}

// verifyFlow strips and checks the signature, returning the payload.
func (s *server) verifyFlow(value string) (string, bool) {
	if s.sessionSecret == "" {
		return value, true
	}
	dot := strings.LastIndexByte(value, '.')
	if dot <= 0 {
		return "", false
	}
	payload, sig := value[:dot], value[dot+1:]
	want := flowSignature(s.sessionSecret, payload)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return "", false
	}
	return payload, true
}

func flowSignature(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// handleOIDCStart redirects the browser to the identity provider (login mode).
func (s *server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oidc[chi.URLParam(r, "provider")]
	if !ok {
		writeProblemDetail(w, http.StatusNotFound, "unknown identity provider")
		return
	}
	authorizeURL, err := s.startOIDCFlow(w, r, p, nil)
	if err != nil {
		s.logger.Error("oidc start failed", "provider", p.ID, "err", err)
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	// Not an open redirect: authorizeURL is built by OIDCProvider.authorizeURL
	// from the authorization_endpoint of the CONFIGURED issuer's discovery
	// document, which metadata() refuses unless its `issuer` field equals the
	// pinned OIDC_ISSUER and requireSecureEndpoint has accepted the scheme.
	// Nothing from this request selects the host.
	http.Redirect(w, r, authorizeURL, http.StatusFound) //nolint:gosec // G710: URL derives from the pinned issuer, not the request
}

// handleOIDCLinkStart begins a flow that attaches an external identity to the
// AUTHENTICATED caller.
//
// It is a CSRF-protected POST returning the authorize URL for the browser to
// navigate to, rather than a GET redirect, for a specific reason: linking is a
// mutation, and a top-level GET navigation cannot carry the X-CSRF-Token header
// the csrf middleware requires. A GET here would be a one-click link-injection.
func (s *server) handleOIDCLinkStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oidc[chi.URLParam(r, "provider")]
	if !ok {
		writeProblemDetail(w, http.StatusNotFound, "unknown identity provider")
		return
	}
	var req stepUp
	if !decodeJSON(w, r, &req) {
		return
	}
	// Attaching an identity adds a second way to log in — one the owner cannot
	// see the credentials for and that a password change does not revoke. From
	// a borrowed session it is the quietest way to keep an account.
	if !s.reauthenticate(w, r, req) {
		return
	}
	pr := principalFrom(r)
	userID := pr.userID
	authorizeURL, err := s.startOIDCFlow(w, r, p, &userID)
	if err != nil {
		s.logger.Error("oidc link start failed", "provider", p.ID, "err", err)
		writeProblemDetail(w, http.StatusBadGateway, "the identity provider could not be reached")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"authorize_url": authorizeURL})
}

// handleOIDCCallback completes both modes.
//
// It is mounted with authenticate (not requireAuth) so the anonymous login mode
// and the authenticated link mode can share one registered redirect URI — which
// matters, because the URI has to be whitelisted at the provider.
func (s *server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	// Cleared up front, not in a defer: every exit from this handler writes a
	// redirect, and a Set-Cookie added after the status line has been written is
	// dropped. Clearing here covers every outcome, success and failure alike,
	// because the flow is single-use either way.
	s.clearCeremonyCookie(w, oidcCookie, oidcCookiePath)

	p, ok := s.oidc[chi.URLParam(r, "provider")]
	if !ok {
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	flow, ok := s.readOIDCFlow(r)
	if !ok {
		// No cookie, an unreadable cookie, or one from another provider. This is
		// the login-CSRF defence: without it an attacker could complete a flow in
		// a victim's browser and silently sign them into the attacker's account.
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	if flow.Provider != p.ID {
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	// Constant-time, matching how the CSRF middleware compares its double-submit
	// pair. The comparison itself is the point; the timing property is cheap.
	gotState := r.URL.Query().Get("state")
	if subtle.ConstantTimeCompare([]byte(gotState), []byte(flow.State)) != 1 {
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	if errCode := r.URL.Query().Get("error"); errCode != "" {
		s.logger.Warn("oidc provider returned an error", "provider", p.ID, "error", errCode)
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}

	meta, err := p.metadata(r.Context())
	if err != nil {
		s.logger.Error("oidc discovery failed", "provider", p.ID, "err", err)
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	rawToken, err := p.exchange(r.Context(), meta, s.oidcRedirectURI(p.ID), code, flow.Verifier)
	if err != nil {
		s.logger.Warn("oidc token exchange failed", "provider", p.ID, "err", err)
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	claims, err := parseIDToken(rawToken)
	if err != nil {
		s.logger.Warn("oidc id token unparseable", "provider", p.ID, "err", err)
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	if err := p.validateClaims(claims, flow.Nonce, s.now()); err != nil {
		s.logger.Warn("oidc id token rejected", "provider", p.ID, "err", err)
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}

	if flow.LinkUserID != nil {
		// The flow cookie is JSON, base64-encoded, and NOT signed — its holder can
		// rewrite it. Trusting the user id in it was an account takeover: start an
		// ordinary login, add "u":1 to the cookie, finish the flow with your own
		// identity provider account, and your external subject is now linked to
		// local user 1. The next SSO login signs you in as that user.
		//
		// Link mode therefore requires a live session for exactly that user. The
		// route runs `authenticate`, so the caller's own session cookie is already
		// resolved here, and a forged id no longer matches anything the attacker
		// can present. That is also the honest model: linking an identity to an
		// account is a mutation on that account, so it must be proven, not asserted.
		pr := principalFrom(r)
		if pr == nil || pr.userID != *flow.LinkUserID {
			s.logger.Warn("oidc link rejected: flow user id does not match the caller",
				"provider", p.ID, "flow_user_id", *flow.LinkUserID, "authenticated", pr != nil)
			s.redirectLoginError(w, r, oidcErrFailed)
			return
		}
		s.completeOIDCLink(w, r, p, claims, *flow.LinkUserID)
		return
	}
	s.completeOIDCLogin(w, r, p, claims)
}

// completeOIDCLogin resolves the assertion to a local account and signs in.
//
// The ONLY join key is (issuer, subject). There is deliberately no lookup by
// e-mail anywhere on this path, and GetCredentialsByEmail is deliberately never
// called from this file: an IdP-asserted address is display data, and matching
// on it would let a hostile or compromised provider take over any local account
// simply by asserting its address. A reviewer should be able to grep this file
// for that method and find nothing.
func (s *server) completeOIDCLogin(w http.ResponseWriter, r *http.Request, p *OIDCProvider, claims oidcClaims) {
	identity, err := s.store.GetIdentity(r.Context(), claims.Iss, claims.Sub)
	switch {
	case err == nil:
		u, err := s.store.GetUserByID(r.Context(), identity.UserID)
		if err != nil {
			s.redirectLoginError(w, r, oidcErrFailed)
			return
		}
		if !u.IsActive {
			s.redirectLoginError(w, r, oidcErrInactive)
			return
		}
		_ = s.store.TouchIdentity(r.Context(), identity.ID, s.now(), claims.Email, claims.Name)
		s.finishOIDCSession(w, r, p, u)
		return

	case errors.Is(err, ErrNotFound):
		s.provisionOIDCUser(w, r, p, claims)
		return

	default:
		s.logger.Error("oidc identity lookup failed", "provider", p.ID, "err", err)
		s.redirectLoginError(w, r, oidcErrFailed)
	}
}

// provisionOIDCUser handles an assertion for a subject nobody has linked.
//
// With auto-provisioning off — the default — this creates nothing and refuses.
// That default is the point: an identity provider that can mint accounts here is
// an identity provider that can grant itself access, so turning it on has to be
// a deliberate act by whoever runs the instance.
func (s *server) provisionOIDCUser(w http.ResponseWriter, r *http.Request, p *OIDCProvider, claims oidcClaims) {
	if !p.AutoProvision {
		s.redirectLoginError(w, r, oidcErrNoAccount)
		return
	}
	email := strings.TrimSpace(strings.ToLower(claims.Email))
	// An unverified address is one the provider has not itself confirmed. It must
	// never become a local account's identity.
	if email == "" || !claims.EmailVerified || validateEmail(email) != nil {
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	if !p.emailDomainAllowed(email) {
		s.redirectLoginError(w, r, oidcErrNoAccount)
		return
	}
	role := p.DefaultRole
	if role == "" {
		role = string(auth.RoleViewer)
	}
	displayName := claims.Name
	if displayName == "" {
		displayName = claims.PreferredUsername
	}

	// Provisioned accounts have NO local password. The empty hash is not a
	// weak password: auth.VerifyPassword rejects any string that is not a
	// well-formed argon2id value, so a password login against such an account
	// gets the same generic 401 as a wrong password.
	// The provisioned account needs a login username too; derive one from the
	// verified address. The unique index still guards it — a collision surfaces
	// as ErrConflict and the same refusal as a taken address.
	//
	// The user row and the identity are written together. As two calls, a
	// failure or a crash between them left an account with no password and no
	// identity — impossible to log into, yet holding the address and username,
	// so every later attempt by the same person was refused as "address already
	// taken" and nothing pointed at why.
	u, err := s.store.CreateUserWithIdentity(r.Context(), email, deriveUsername(email),
		displayName, role, ExternalIdentity{
			Provider: p.ID, Issuer: claims.Iss, Subject: claims.Sub,
			Email: claims.Email, DisplayName: claims.Name,
		})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			// The address already belongs to a local account. This is exactly the
			// case where the tempting behaviour — attach the identity to the
			// account that owns the address — is the vulnerability. The unique
			// index on lower(email) is what surfaced it, and the answer is to
			// refuse and tell the user to link it themselves from a session they
			// have already proven they own.
			s.redirectLoginError(w, r, oidcErrEmailTaken)
			return
		}
		s.logger.Error("oidc provisioning failed", "provider", p.ID, "err", err)
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	s.audit(r.Context(), &u.ID, "user.provision.oidc", "user", &u.ID, clientIP(r),
		map[string]string{"provider": p.ID, "subject": claims.Sub})
	s.finishOIDCSession(w, r, p, u)
}

// completeOIDCLink attaches the asserted identity to the user the flow cookie
// names — never to a user resolved from the assertion.
func (s *server) completeOIDCLink(w http.ResponseWriter, r *http.Request, p *OIDCProvider, claims oidcClaims, userID int64) {
	existing, err := s.store.GetIdentity(r.Context(), claims.Iss, claims.Sub)
	switch {
	case err == nil:
		if existing.UserID != userID {
			// Already linked elsewhere. Moving it would let whoever controls the
			// external account detach it from its owner.
			s.redirectProfile(w, r, oidcErrLinked)
			return
		}
		s.redirectProfile(w, r, "") // already linked to this user: idempotent
		return
	case !errors.Is(err, ErrNotFound):
		s.logger.Error("oidc identity lookup failed", "provider", p.ID, "err", err)
		s.redirectProfile(w, r, oidcErrFailed)
		return
	}

	now := s.now()
	if _, err := s.store.CreateIdentity(r.Context(), ExternalIdentity{
		UserID: userID, Provider: p.ID, Issuer: claims.Iss, Subject: claims.Sub,
		Email: claims.Email, DisplayName: claims.Name, LastLoginAt: &now,
	}); err != nil {
		s.logger.Error("oidc link failed", "provider", p.ID, "err", err)
		s.redirectProfile(w, r, oidcErrFailed)
		return
	}
	s.audit(r.Context(), &userID, "user.identity.link", "user", &userID, clientIP(r),
		map[string]string{"provider": p.ID, "subject": claims.Sub})
	s.redirectProfile(w, r, "")
}

// finishOIDCSession issues the session and lands the browser on the dashboard.
func (s *server) finishOIDCSession(w http.ResponseWriter, r *http.Request, p *OIDCProvider, u User) {
	// A successful single sign-on clears the password-login bucket for the same
	// address from this address's client, so someone who fumbled their password
	// and then signed in with SSO is not left locked out.
	s.lockout.reset(loginKey(u.Email, clientIP(r)))
	if _, err := s.issueSession(w, r, u.ID); err != nil {
		s.redirectLoginError(w, r, oidcErrFailed)
		return
	}
	s.audit(r.Context(), &u.ID, "user.login.oidc", "user", &u.ID, clientIP(r),
		map[string]string{"provider": p.ID})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *server) redirectLoginError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, "/login?error="+url.QueryEscape(code), http.StatusFound)
}

func (s *server) redirectProfile(w http.ResponseWriter, r *http.Request, code string) {
	target := "/profile"
	if code != "" {
		target += "?error=" + url.QueryEscape(code)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// readOIDCFlow decodes the flow cookie.
func (s *server) readOIDCFlow(r *http.Request) (oidcFlow, bool) {
	c, err := r.Cookie(oidcCookie)
	if err != nil || c.Value == "" {
		return oidcFlow{}, false
	}
	payload, ok := s.verifyFlow(c.Value)
	if !ok {
		return oidcFlow{}, false
	}
	raw, err := decodeBase64URL(payload)
	if err != nil {
		return oidcFlow{}, false
	}
	var f oidcFlow
	if err := json.Unmarshal(raw, &f); err != nil {
		return oidcFlow{}, false
	}
	if f.State == "" || f.Nonce == "" || f.Verifier == "" {
		return oidcFlow{}, false
	}
	return f, true
}

// handleListIdentities lists the caller's linked external accounts.
func (s *server) handleListIdentities(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	ids, err := s.store.ListIdentitiesByUser(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	if ids == nil {
		ids = []ExternalIdentity{}
	}
	writeJSON(w, http.StatusOK, ids)
}

// handleUnlinkIdentity removes one of the caller's links.
//
// The delete is scoped by the caller's user id in the statement itself, so an id
// belonging to somebody else matches nothing. It also refuses to remove the LAST
// identity of an account that has no local password, because doing so would
// leave that account with no way to authenticate at all — an irrecoverable
// self-lockout the user cannot undo.
func (s *server) handleUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r)
	id, ok := idParam(r, "id")
	if !ok {
		writeProblemDetail(w, http.StatusBadRequest, "invalid id")
		return
	}
	creds, err := s.credentialsByID(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	ids, err := s.store.ListIdentitiesByUser(r.Context(), p.userID)
	if storeError(w, err) {
		return
	}
	// PasswordHash == "" is the canonical "no local password" predicate: it is
	// what provisioning writes and what auth.VerifyPassword rejects outright.
	if creds.PasswordHash == "" && len(ids) <= 1 {
		writeProblemDetail(w, http.StatusConflict,
			"set a password before unlinking your last single sign-on account, or you will not be able to sign in")
		return
	}
	if err := s.store.DeleteIdentity(r.Context(), id, p.userID); storeError(w, err) {
		return
	}
	s.audit(r.Context(), &p.userID, "user.identity.unlink", "user", &p.userID, clientIP(r), nil)
	w.WriteHeader(http.StatusNoContent)
}
