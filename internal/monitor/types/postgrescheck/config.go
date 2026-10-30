package postgrescheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// defaultPort is the standard PostgreSQL TCP port.
const defaultPort = 5432

// defaultConnectTimeout is the connect timeout (seconds) when unset.
const defaultConnectTimeout = 10

// SSL modes accepted in Config.SSLMode, mirroring libpq/pgx semantics.
const (
	SSLModeDisable    = "disable"
	SSLModeAllow      = "allow"
	SSLModePrefer     = "prefer"
	SSLModeRequire    = "require"
	SSLModeVerifyCA   = "verify-ca"
	SSLModeVerifyFull = "verify-full"
)

// Config is the jsonb monitor configuration for the postgres type.
type Config struct {
	// Host is the server hostname or IP. Required.
	Host string `json:"host"`
	// Port is the TCP port; 0 means the default 5432.
	Port int `json:"port,omitempty"`
	// User is the login role. Optional (some setups use peer/trust auth).
	User string `json:"user,omitempty"`
	// Password is the login password.
	Password string `json:"password,omitempty"`
	// Database is the database name to connect to.
	Database string `json:"database,omitempty"`
	// SSLMode selects TLS behaviour: disable, allow, prefer (default),
	// require, verify-ca or verify-full.
	SSLMode string `json:"ssl_mode,omitempty"`
	// ConnectTimeoutSeconds bounds the connection attempt; 0 means 10s.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds,omitempty"`

	// Query is an optional assertion query (e.g. "SELECT 1"). When empty the
	// check only verifies connectivity with a ping.
	Query string `json:"query,omitempty"`
	// ExpectedResult, when set, is compared as a string against the first
	// column of the first row returned by Query. Requires Query.
	ExpectedResult string `json:"expected_result,omitempty"`
}

// Secrets lists the JSON field names in Config that carry credentials. The API
// redacts them from read responses and keeps a submitted redaction placeholder
// as "unchanged"; the SecretsBox seals the whole config at rest.
func (*Checker) Secrets() []string {
	return []string{"password"}
}

// port returns the effective TCP port.
func (c Config) port() int {
	if c.Port == 0 {
		return defaultPort
	}
	return c.Port
}

// sslMode returns the effective SSL mode.
func (c Config) sslMode() string {
	if c.SSLMode == "" {
		return SSLModePrefer
	}
	return c.SSLMode
}

// connectTimeout returns the effective connect timeout in seconds.
func (c Config) connectTimeout() int {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout
	}
	return c.ConnectTimeoutSeconds
}

// dsn builds a libpq/pgx connection URL from the config. It is pure (no I/O)
// so it can be unit-tested without a server.
func (c Config) dsn() string {
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.port())),
		Path:   "/" + c.Database,
	}
	if c.User != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}
	q := url.Values{}
	q.Set("sslmode", c.sslMode())
	q.Set("connect_timeout", strconv.Itoa(c.connectTimeout()))
	u.RawQuery = q.Encode()
	return u.String()
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("postgres: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("postgres: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; it must not perform
// I/O.
func (c Config) validate() error {
	if c.Host == "" {
		return fmt.Errorf("postgres: %q is required", "host")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("postgres: port %d out of range 1-65535", c.Port)
	}
	if c.Password != "" && c.User == "" {
		return fmt.Errorf("postgres: password set but user is empty")
	}
	switch c.SSLMode {
	case "", SSLModeDisable, SSLModeAllow, SSLModePrefer, SSLModeRequire, SSLModeVerifyCA, SSLModeVerifyFull:
	default:
		return fmt.Errorf("postgres: unknown ssl_mode %q (use disable, allow, prefer, require, verify-ca or verify-full)", c.SSLMode)
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("postgres: connect_timeout_seconds must not be negative")
	}
	if c.ExpectedResult != "" && c.Query == "" {
		return fmt.Errorf("postgres: expected_result requires query")
	}
	return nil
}
