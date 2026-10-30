package mysqlcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
)

// defaultPort is the standard MySQL/MariaDB TCP port.
const defaultPort = 3306

// defaultConnectTimeout is the connect timeout (seconds) when unset.
const defaultConnectTimeout = 10

// TLS modes accepted in Config.TLS, mirroring go-sql-driver semantics.
const (
	TLSFalse      = "false"
	TLSTrue       = "true"
	TLSSkipVerify = "skip-verify"
	TLSPreferred  = "preferred"
)

// Config is the jsonb monitor configuration for the mysql type.
type Config struct {
	// Host is the server hostname or IP. Required.
	Host string `json:"host"`
	// Port is the TCP port; 0 means the default 3306.
	Port int `json:"port,omitempty"`
	// User is the login user. Optional.
	User string `json:"user,omitempty"`
	// Password is the login password.
	Password string `json:"password,omitempty"`
	// Database is the default schema to select.
	Database string `json:"database,omitempty"`
	// TLS selects TLS behaviour: false (default), true, skip-verify or
	// preferred.
	TLS string `json:"tls,omitempty"`
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

// tlsParam returns the effective go-sql-driver tls parameter.
func (c Config) tlsParam() string {
	if c.TLS == "" {
		return TLSFalse
	}
	return c.TLS
}

// connectTimeout returns the effective connect timeout.
func (c Config) connectTimeout() time.Duration {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout * time.Second
	}
	return time.Duration(c.ConnectTimeoutSeconds) * time.Second
}

// dsn builds a go-sql-driver DSN from the config. It is pure (no I/O) so it
// can be unit-tested without a server.
func (c Config) dsn() string {
	mc := mysql.NewConfig()
	mc.Net = "tcp"
	mc.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.port()))
	mc.User = c.User
	mc.Passwd = c.Password
	mc.DBName = c.Database
	mc.TLSConfig = c.tlsParam()
	mc.Timeout = c.connectTimeout()
	return mc.FormatDSN()
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("mysql: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("mysql: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if c.Host == "" {
		return fmt.Errorf("mysql: %q is required", "host")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("mysql: port %d out of range 1-65535", c.Port)
	}
	if c.Password != "" && c.User == "" {
		return fmt.Errorf("mysql: password set but user is empty")
	}
	switch c.TLS {
	case "", TLSFalse, TLSTrue, TLSSkipVerify, TLSPreferred:
	default:
		return fmt.Errorf("mysql: unknown tls %q (use false, true, skip-verify or preferred)", c.TLS)
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("mysql: connect_timeout_seconds must not be negative")
	}
	if c.ExpectedResult != "" && c.Query == "" {
		return fmt.Errorf("mysql: expected_result requires query")
	}
	return nil
}
