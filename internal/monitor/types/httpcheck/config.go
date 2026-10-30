package httpcheck

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Body encodings accepted in Config.BodyEncoding.
const (
	BodyEncodingRaw  = "raw"
	BodyEncodingJSON = "json"
	BodyEncodingForm = "form"
)

// Auth methods accepted in Config.AuthMethod. mTLS is configured separately
// via the tls_client_* fields because a client certificate can be combined
// with basic or bearer auth.
const (
	AuthNone   = "none"
	AuthBasic  = "basic"
	AuthBearer = "bearer"
)

// Config is the jsonb monitor configuration for the http type.
type Config struct {
	// URL is the http(s) target. Required.
	URL string `json:"url"`
	// Method defaults to GET. One of GET/HEAD/POST/PUT/PATCH/DELETE/OPTIONS.
	Method string `json:"method,omitempty"`
	// AcceptedStatusCodes lists response codes counting as UP. Elements are
	// numbers (418) or range strings ("200-299", "301", "200-299,301").
	// Empty means any 2xx.
	AcceptedStatusCodes StatusCodeSet `json:"accepted_status_codes,omitempty"`

	// Headers are extra request headers (API keys, Accept, …).
	Headers map[string]string `json:"headers,omitempty"`
	// UserAgent overrides the default Statwarden User-Agent. It wins over a
	// User-Agent entry in Headers.
	UserAgent string `json:"user_agent,omitempty"`
	// Body is the raw request body.
	Body string `json:"body,omitempty"`
	// BodyEncoding is "raw" (default, no Content-Type), "json"
	// (application/json, body must be valid JSON) or "form"
	// (application/x-www-form-urlencoded, body must be a valid query string).
	BodyEncoding string `json:"body_encoding,omitempty"`

	// AuthMethod is "none" (default), "basic" or "bearer".
	AuthMethod string `json:"auth_method,omitempty"`
	// BasicUsername/BasicPassword are used when AuthMethod is "basic".
	BasicUsername string `json:"basic_username,omitempty"`
	BasicPassword string `json:"basic_password,omitempty"`
	// BearerToken is sent as "Authorization: Bearer …" when AuthMethod is
	// "bearer".
	BearerToken string `json:"bearer_token,omitempty"`

	// TLSClientCertPEM/TLSClientKeyPEM hold a PEM client certificate and key
	// for mTLS. Both or neither must be set.
	TLSClientCertPEM string `json:"tls_client_cert_pem,omitempty"`
	TLSClientKeyPEM  string `json:"tls_client_key_pem,omitempty"`

	// FollowRedirects toggles following 3xx responses; nil/absent means
	// true. When false the redirect response itself is evaluated (combine
	// with accepted_status_codes like "301-302").
	FollowRedirects *bool `json:"follow_redirects,omitempty"`
	// MaxRedirects caps followed redirects; 0 means the default of 10.
	MaxRedirects int `json:"max_redirects,omitempty"`
	// IgnoreTLSErrors skips certificate verification (self-signed certs).
	IgnoreTLSErrors bool `json:"ignore_tls_errors,omitempty"`
	// ProxyURL routes the check through an http://, https://, socks5:// or
	// socks5h:// proxy, with optional user:pass@ userinfo.
	ProxyURL string `json:"proxy_url,omitempty"`

	// Keyword must appear in the response body for the check to pass.
	Keyword string `json:"keyword,omitempty"`
	// KeywordAbsent must NOT appear in the response body.
	KeywordAbsent string `json:"keyword_absent,omitempty"`
	// JSONPath is a gjson dot/bracket path evaluated against a JSON body
	// (e.g. "status" or "checks.0.state"). The path must exist.
	JSONPath string `json:"json_path,omitempty"`
	// JSONExpected is the expected string form of the value at JSONPath
	// (numbers/bools compare via their literal, e.g. "42", "true"). Empty
	// means existence of the path is enough.
	JSONExpected string `json:"json_expected,omitempty"`
	// HeaderName names a response header that must be present.
	HeaderName string `json:"header_name,omitempty"`
	// HeaderValue is the exact required value of HeaderName.
	HeaderValue string `json:"header_value,omitempty"`
	// HeaderValueRegex is a regular expression the value of HeaderName must
	// match. Mutually exclusive with HeaderValue.
	HeaderValueRegex string `json:"header_value_regex,omitempty"`
	// MaxResponseTimeMS marks the check DOWN (degraded) when the response
	// takes longer than this many milliseconds. 0 disables the threshold.
	MaxResponseTimeMS int `json:"max_response_time_ms,omitempty"`
}

// Secrets lists the JSON field names in Config that carry credentials. The API
// redacts them from read responses and keeps a submitted redaction placeholder
// as "unchanged"; the SecretsBox seals the whole config at rest. Request
// headers are included wholesale because they routinely carry API keys and
// cookies, and tls_client_key_pem is the mTLS private key.
func (*Checker) Secrets() []string {
	return []string{
		"headers", "basic_password", "bearer_token", "tls_client_key_pem",
		// proxy_url is a URL, and a proxy URL is one of the few that routinely
		// carries its credentials inline as user:password@host — which is why
		// the validator accepts userinfo at all. Redacted whole rather than
		// stripped down to the host: a partially masked URL still tells a
		// reader the username, and the field is not worth a second parser.
		"proxy_url",
	}
}

// StatusRange is one inclusive range of HTTP status codes; single codes have
// Lo == Hi.
type StatusRange struct {
	Lo, Hi int
}

// String renders the range in config syntax ("418" or "200-299").
func (r StatusRange) String() string {
	if r.Lo == r.Hi {
		return strconv.Itoa(r.Lo)
	}
	return fmt.Sprintf("%d-%d", r.Lo, r.Hi)
}

// StatusCodeSet is the decoded accepted_status_codes list. Its JSON form is
// an array whose elements are numbers or (comma-separated) range strings.
type StatusCodeSet []StatusRange

// UnmarshalJSON implements json.Unmarshaler.
func (s *StatusCodeSet) UnmarshalJSON(data []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("accepted_status_codes must be an array of codes or ranges: %w", err)
	}
	set := make(StatusCodeSet, 0, len(items))
	for _, item := range items {
		var code int
		if err := json.Unmarshal(item, &code); err == nil {
			set = append(set, StatusRange{Lo: code, Hi: code})
			continue
		}
		var str string
		if err := json.Unmarshal(item, &str); err != nil {
			return fmt.Errorf("accepted_status_codes entry %s must be a number or a range string like \"200-299\"", item)
		}
		for token := range strings.SplitSeq(str, ",") {
			r, err := parseStatusToken(strings.TrimSpace(token))
			if err != nil {
				return err
			}
			set = append(set, r)
		}
	}
	*s = set
	return nil
}

// MarshalJSON implements json.Marshaler, emitting numbers for single codes
// and "lo-hi" strings for ranges.
func (s StatusCodeSet) MarshalJSON() ([]byte, error) {
	items := make([]any, len(s))
	for i, r := range s {
		if r.Lo == r.Hi {
			items[i] = r.Lo
		} else {
			items[i] = r.String()
		}
	}
	return json.Marshal(items)
}

// parseStatusToken parses "418" or "200-299".
func parseStatusToken(token string) (StatusRange, error) {
	lo, hi, isRange := strings.Cut(token, "-")
	loCode, err := strconv.Atoi(strings.TrimSpace(lo))
	if err != nil {
		return StatusRange{}, fmt.Errorf("invalid accepted status code %q", token)
	}
	if !isRange {
		return StatusRange{Lo: loCode, Hi: loCode}, nil
	}
	hiCode, err := strconv.Atoi(strings.TrimSpace(hi))
	if err != nil {
		return StatusRange{}, fmt.Errorf("invalid accepted status range %q", token)
	}
	return StatusRange{Lo: loCode, Hi: hiCode}, nil
}

// Match reports whether code counts as UP; an empty set means any 2xx.
func (s StatusCodeSet) Match(code int) bool {
	if len(s) == 0 {
		return code >= 200 && code <= 299
	}
	for _, r := range s {
		if code >= r.Lo && code <= r.Hi {
			return true
		}
	}
	return false
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("http: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("http: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; it must not perform
// I/O.
func (c Config) validate() error {
	if c.URL == "" {
		return fmt.Errorf("http: %q is required", "url")
	}
	u, err := url.Parse(c.URL)
	switch {
	case err != nil:
		return fmt.Errorf("http: invalid url: %w", err)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("http: url must start with http:// or https://, got %q", c.URL)
	case u.Host == "":
		return fmt.Errorf("http: url %q has no host", c.URL)
	case u.User != nil:
		// Credentials in the URL are a credential the API cannot protect. url is
		// the primary display field for an HTTP monitor — it is returned as
		// config.url and again as the derived view.url — so unlike proxy_url it
		// cannot simply be redacted whole. The dedicated fields carry the same
		// thing where redaction and the SecretsBox already cover it, so the
		// credential is refused here rather than stored somewhere it leaks.
		//
		// Rejected whether or not a password is present: Go's http client sends
		// basic auth for a bare "user@host" too, so username-only userinfo is
		// still authentication in the wrong place.
		return fmt.Errorf("http: url must not carry credentials; use auth_method %q with basic_username/basic_password", AuthBasic)
	}

	if c.Method != "" {
		switch c.Method {
		case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
			http.MethodPatch, http.MethodDelete, http.MethodOptions:
		default:
			return fmt.Errorf("http: unsupported method %q (use an uppercase standard method)", c.Method)
		}
	}

	for _, r := range c.AcceptedStatusCodes {
		if r.Lo > r.Hi {
			return fmt.Errorf("http: accepted status range %q is inverted (start > end)", r)
		}
		if r.Lo < 100 || r.Hi > 599 {
			return fmt.Errorf("http: accepted status range %q out of range 100-599", r)
		}
	}

	for name := range c.Headers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("http: request header with empty name")
		}
	}

	switch c.BodyEncoding {
	case "", BodyEncodingRaw:
	case BodyEncodingJSON:
		if !json.Valid([]byte(c.Body)) {
			return fmt.Errorf("http: body_encoding is %q but body is not valid JSON", BodyEncodingJSON)
		}
	case BodyEncodingForm:
		if _, err := url.ParseQuery(c.Body); err != nil {
			return fmt.Errorf("http: body_encoding is %q but body is not a valid query string: %w", BodyEncodingForm, err)
		}
	default:
		return fmt.Errorf("http: unknown body_encoding %q (use %q, %q or %q)",
			c.BodyEncoding, BodyEncodingRaw, BodyEncodingJSON, BodyEncodingForm)
	}

	switch c.AuthMethod {
	case "", AuthNone:
		if c.BasicUsername != "" || c.BasicPassword != "" {
			return fmt.Errorf("http: basic credentials set but auth_method is not %q", AuthBasic)
		}
		if c.BearerToken != "" {
			return fmt.Errorf("http: bearer_token set but auth_method is not %q", AuthBearer)
		}
	case AuthBasic:
		if c.BasicUsername == "" {
			return fmt.Errorf("http: auth_method %q requires basic_username", AuthBasic)
		}
		if c.BearerToken != "" {
			return fmt.Errorf("http: bearer_token set but auth_method is %q", AuthBasic)
		}
	case AuthBearer:
		if c.BearerToken == "" {
			return fmt.Errorf("http: auth_method %q requires bearer_token", AuthBearer)
		}
		if c.BasicUsername != "" || c.BasicPassword != "" {
			return fmt.Errorf("http: basic credentials set but auth_method is %q", AuthBearer)
		}
	default:
		return fmt.Errorf("http: unknown auth_method %q (use %q, %q or %q)",
			c.AuthMethod, AuthNone, AuthBasic, AuthBearer)
	}

	if (c.TLSClientCertPEM == "") != (c.TLSClientKeyPEM == "") {
		return fmt.Errorf("http: tls_client_cert_pem and tls_client_key_pem must be set together")
	}
	if c.TLSClientCertPEM != "" {
		if _, err := tls.X509KeyPair([]byte(c.TLSClientCertPEM), []byte(c.TLSClientKeyPEM)); err != nil {
			return fmt.Errorf("http: invalid TLS client certificate/key: %w", err)
		}
	}

	if c.MaxRedirects < 0 {
		return fmt.Errorf("http: max_redirects must not be negative")
	}
	if c.MaxResponseTimeMS < 0 {
		return fmt.Errorf("http: max_response_time_ms must not be negative")
	}

	if c.ProxyURL != "" {
		p, err := url.Parse(c.ProxyURL)
		if err != nil {
			return fmt.Errorf("http: invalid proxy_url: %w", err)
		}
		switch p.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("http: proxy_url scheme %q not supported (use http, https, socks5 or socks5h)", p.Scheme)
		}
		if p.Host == "" {
			return fmt.Errorf("http: proxy_url %q has no host", c.ProxyURL)
		}
	}

	if c.Keyword != "" && c.Keyword == c.KeywordAbsent {
		return fmt.Errorf("http: keyword and keyword_absent are identical — the check could never pass")
	}
	if c.JSONExpected != "" && c.JSONPath == "" {
		return fmt.Errorf("http: json_expected requires json_path")
	}
	if c.HeaderName == "" && (c.HeaderValue != "" || c.HeaderValueRegex != "") {
		return fmt.Errorf("http: header_value/header_value_regex require header_name")
	}
	if c.HeaderValue != "" && c.HeaderValueRegex != "" {
		return fmt.Errorf("http: header_value and header_value_regex are mutually exclusive")
	}
	if c.HeaderValueRegex != "" {
		if _, err := regexp.Compile(c.HeaderValueRegex); err != nil {
			return fmt.Errorf("http: invalid header_value_regex: %w", err)
		}
	}
	return nil
}

// needsBody reports whether any configured assertion reads the response body.
func (c Config) needsBody() bool {
	return c.Keyword != "" || c.KeywordAbsent != "" || c.JSONPath != ""
}
