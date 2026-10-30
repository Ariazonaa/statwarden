package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/auth"
)

// OIDC support here is the authorization-code flow with PKCE and nothing else.
// No implicit flow, no hybrid flow, no response_mode=form_post: an ID token is
// only ever accepted as the body of a token-endpoint response fetched by this
// server over TLS from the pinned issuer.
//
// That single restriction is what makes it correct to read the ID token's claims
// without verifying its JWT signature. OIDC Core §3.1.3.7 permits exactly this:
// when the token is received directly from the token endpoint over a
// TLS-authenticated channel, the server-authenticated connection stands in for
// signature validation. The condition holds only because there is no other way
// into parseIDToken — see the comment there before changing anything about how
// tokens arrive.

const (
	// oidcDiscoveryTTL bounds how long a discovery document is reused. Fetching
	// lazily and caching means an unreachable identity provider degrades SSO
	// instead of blocking startup.
	oidcDiscoveryTTL = time.Hour
	// oidcHTTPTimeout bounds every outbound call to the provider, matching the
	// convention in internal/outbound.
	oidcHTTPTimeout = 10 * time.Second
	// oidcMaxBody caps a discovery or token response, so a hostile or broken
	// endpoint cannot exhaust memory.
	oidcMaxBody = 1 << 20
	// oidcMaxSkew is the tolerance on the iat claim.
	oidcMaxSkew = 60 * time.Second
)

// OIDCProvider is one configured identity provider.
type OIDCProvider struct {
	// ID is the local, renameable label that appears in URLs.
	ID          string
	DisplayName string
	// Issuer is the provider's own identity and is pinned: the discovery
	// document and every ID token must claim exactly this value.
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	// AutoProvision permits creating a local account for an unknown subject. It
	// defaults to FALSE and should stay that way unless the identity provider is
	// the authoritative source of who may have an account here.
	AutoProvision bool
	// DefaultRole is the role a provisioned account receives. Least privilege.
	DefaultRole string
	// AllowedEmailDomains, when non-empty, restricts provisioning to these
	// e-mail domains.
	AllowedEmailDomains []string
	// AllowInsecureIssuer permits a plain-http issuer. DEV_MODE only.
	AllowInsecureIssuer bool

	HTTPClient *http.Client

	// mu guards the discovery cache below, and nothing else: everything above is
	// configuration written once before the provider is served and only read
	// afterwards. Any new mutable field either belongs under this mutex or does
	// not belong on a value shared by every request.
	mu      sync.Mutex
	meta    oidcMetadata
	metaAt  time.Time
	metaOK  bool
	nowFunc func() time.Time
}

// oidcMetadata is the subset of the discovery document this flow uses.
type oidcMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func (p *OIDCProvider) now() time.Time {
	if p.nowFunc != nil {
		return p.nowFunc()
	}
	return time.Now()
}

func (p *OIDCProvider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: oidcHTTPTimeout}
}

// scopeList returns the scopes to request, always including openid.
//
// Recomputed on every call rather than memoised. The memo was a data race: it
// read and wrote p.scopesOK with no lock — p.mu guards only the discovery cache —
// and the only caller is authorizeURL, reached from startOIDCFlow on a request
// goroutine. Two people clicking "sign in with SSO" at the same time raced on the
// field. Every writer stores the same value, so the logical outcome was never
// wrong, which is exactly why it would have sat there: the defect is a torn slice
// header, not a wrong scope list, and it only ever shows up as `go test -race`
// output or an implausible panic.
//
// Locking it would work; not caching it is better. This builds a three-element
// slice once per login attempt, which is nothing, and shared mutable state that
// buys nothing is worth removing rather than protecting.
func (p *OIDCProvider) scopeList() []string {
	out := []string{"openid"}
	for _, s := range p.Scopes {
		if s != "" && s != "openid" {
			out = append(out, s)
		}
	}
	return out
}

// metadata fetches and caches the discovery document.
//
// The document is REJECTED unless its issuer field equals the configured issuer
// byte for byte. That equality is the pin the rest of the flow rests on: without
// it, a provider that has been redirected or misconfigured could hand back
// endpoints belonging to somebody else, and every later check would then be
// performed against the wrong authority.
func (p *OIDCProvider) metadata(ctx context.Context) (oidcMetadata, error) {
	p.mu.Lock()
	if p.metaOK && p.now().Sub(p.metaAt) < oidcDiscoveryTTL {
		meta := p.meta
		p.mu.Unlock()
		return meta, nil
	}
	p.mu.Unlock()

	endpoint := strings.TrimRight(p.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return oidcMetadata{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return oidcMetadata{}, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode != http.StatusOK {
		return oidcMetadata{}, fmt.Errorf("oidc discovery: unexpected status %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oidcMaxBody))
	if err != nil {
		return oidcMetadata{}, fmt.Errorf("oidc discovery: %w", err)
	}
	var meta oidcMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return oidcMetadata{}, fmt.Errorf("oidc discovery: %w", err)
	}
	if meta.Issuer != p.Issuer {
		return oidcMetadata{}, fmt.Errorf("oidc discovery: issuer %q does not match the configured issuer %q",
			meta.Issuer, p.Issuer)
	}
	// Do not follow a discovery document down to a plaintext endpoint. The
	// token request carries the client secret and the code.
	for _, ep := range []string{meta.AuthorizationEndpoint, meta.TokenEndpoint} {
		if err := p.requireSecureEndpoint(ep); err != nil {
			return oidcMetadata{}, err
		}
	}

	p.mu.Lock()
	p.meta, p.metaAt, p.metaOK = meta, p.now(), true
	p.mu.Unlock()
	return meta, nil
}

func (p *OIDCProvider) requireSecureEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("oidc discovery: %q is not an absolute URL", raw)
	}
	if u.Scheme == "https" || (p.AllowInsecureIssuer && u.Scheme == "http") {
		return nil
	}
	return fmt.Errorf("oidc discovery: %q must be https", raw)
}

// newPKCE returns a verifier and its S256 challenge. The method is S256 only;
// "plain" is never emitted and never accepted, because a plain challenge offers
// no protection at all against an intercepted authorization code.
func newPKCE() (verifier, challenge string, err error) {
	verifier, err = auth.NewToken()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// authorizeURL builds the redirect the browser follows to the provider.
func (p *OIDCProvider) authorizeURL(meta oidcMetadata, redirectURI, state, nonce, challenge string) (string, error) {
	u, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code") // code only; see the file comment
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(p.scopeList(), " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// exchange trades the authorization code for an ID token at the token endpoint.
func (p *OIDCProvider) exchange(ctx context.Context, meta oidcMetadata, redirectURI, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.ClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic is what most providers prefer and what the spec
	// recommends; a provider that only accepts the secret in the body will
	// reject this, which is a configuration problem worth surfacing loudly
	// rather than papering over by sending the secret twice.
	req.SetBasicAuth(url.QueryEscape(p.ClientID), url.QueryEscape(p.ClientSecret))

	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("oidc token exchange: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	raw, err := io.ReadAll(io.LimitReader(resp.Body, oidcMaxBody))
	if err != nil {
		return "", fmt.Errorf("oidc token exchange: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The provider's error body may echo request detail; keep it out of the
		// returned error, which reaches a log.
		return "", fmt.Errorf("oidc token exchange: unexpected status %s", resp.Status)
	}
	var body struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("oidc token exchange: %w", err)
	}
	if body.IDToken == "" {
		return "", errors.New("oidc token exchange: response carried no id_token")
	}
	return body.IDToken, nil
}

// oidcClaims is the subset of the ID token this flow reads.
type oidcClaims struct {
	Iss               string          `json:"iss"`
	Sub               string          `json:"sub"`
	Aud               json.RawMessage `json:"aud"`
	Azp               string          `json:"azp"`
	Nonce             string          `json:"nonce"`
	Email             string          `json:"email"`
	EmailVerified     bool            `json:"email_verified"`
	Name              string          `json:"name"`
	PreferredUsername string          `json:"preferred_username"`
	Exp               int64           `json:"exp"`
	Iat               int64           `json:"iat"`
}

// audiences decodes the aud claim, which the spec allows to be either a single
// string or an array of strings.
func (c oidcClaims) audiences() []string {
	if len(c.Aud) == 0 {
		return nil
	}
	var one string
	if err := json.Unmarshal(c.Aud, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(c.Aud, &many); err == nil {
		return many
	}
	return nil
}

// parseIDToken decodes the claims out of an ID token WITHOUT verifying its
// signature.
//
// That is safe here and ONLY here. The token this function receives came
// directly from the pinned issuer's token endpoint, over a TLS connection this
// server opened and whose certificate it verified, in the body of a response to
// a request carrying the client secret. OIDC Core §3.1.3.7 explicitly allows the
// server-authenticated TLS channel to stand in for signature validation in that
// situation.
//
// If a token ever reaches this function from anywhere else — a query parameter,
// a fragment, a form post, a front-channel logout — that reasoning collapses and
// the token becomes attacker-chosen. Do not call this on anything but a token
// endpoint response.
func parseIDToken(raw string) (oidcClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return oidcClaims{}, errors.New("id token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return oidcClaims{}, fmt.Errorf("id token payload: %w", err)
	}
	var c oidcClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return oidcClaims{}, fmt.Errorf("id token payload: %w", err)
	}
	return c, nil
}

// validateClaims enforces every check the code flow requires. It fails on the
// first problem and its errors are for the log only: the caller answers with a
// coarse code and never echoes a claim value back to the browser.
func (p *OIDCProvider) validateClaims(c oidcClaims, wantNonce string, now time.Time) error {
	if c.Iss != p.Issuer {
		return fmt.Errorf("issuer mismatch: %q", c.Iss)
	}
	if strings.TrimSpace(c.Sub) == "" {
		return errors.New("subject is empty")
	}
	auds := c.audiences()
	if len(auds) == 0 {
		return errors.New("audience is missing")
	}
	found := false
	for _, a := range auds {
		if a == p.ClientID {
			found = true
			break
		}
	}
	if !found {
		return errors.New("audience does not contain the client id")
	}
	// With more than one audience the spec requires azp, and requires it to name
	// this client — otherwise the token was minted for somebody else and merely
	// mentions us.
	if len(auds) > 1 && c.Azp != p.ClientID {
		return errors.New("multiple audiences without a matching azp")
	}
	if c.Exp == 0 || now.After(time.Unix(c.Exp, 0)) {
		return errors.New("token is expired")
	}
	if c.Iat != 0 {
		if d := now.Sub(time.Unix(c.Iat, 0)); d < -oidcMaxSkew {
			return errors.New("token was issued in the future")
		}
	}
	// The nonce is the replay defence and is independent of state: state proves
	// the callback belongs to a flow this browser started, nonce proves the ID
	// TOKEN belongs to that same flow.
	if wantNonce == "" || c.Nonce != wantNonce {
		return errors.New("nonce mismatch")
	}
	return nil
}

// emailDomainAllowed reports whether an address may be auto-provisioned.
func (p *OIDCProvider) emailDomainAllowed(email string) bool {
	if len(p.AllowedEmailDomains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range p.AllowedEmailDomains {
		if strings.ToLower(strings.TrimSpace(d)) == domain {
			return true
		}
	}
	return false
}

// base64URL / decodeBase64URL wrap the flow cookie value. A cookie value may not
// carry arbitrary bytes, and the JSON contains none that need escaping, so the
// unpadded URL alphabet is the smallest safe encoding.
func base64URL(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func decodeBase64URL(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
