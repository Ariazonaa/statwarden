package mongocheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// defaultPort is the standard MongoDB TCP port.
const defaultPort = 27017

// defaultConnectTimeout is the connect timeout (seconds) when unset.
const defaultConnectTimeout = 10

// Config is the jsonb monitor configuration for the mongodb type.
type Config struct {
	// Host is the server hostname or IP. Required.
	Host string `json:"host"`
	// Port is the TCP port; 0 means the default 27017.
	Port int `json:"port,omitempty"`
	// User is the login user. Optional.
	User string `json:"user,omitempty"`
	// Password is the login password.
	Password string `json:"password,omitempty"`
	// Database is the authentication database / default database. Optional.
	Database string `json:"database,omitempty"`
	// TLS enables a TLS connection.
	TLS bool `json:"tls,omitempty"`
	// TLSSkipVerify disables certificate verification (self-signed certs).
	// Only meaningful when TLS is true.
	TLSSkipVerify bool `json:"tls_skip_verify,omitempty"`
	// ConnectTimeoutSeconds bounds the connection attempt; 0 means 10s.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds,omitempty"`
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

// connectTimeout returns the effective connect timeout.
func (c Config) connectTimeout() time.Duration {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout * time.Second
	}
	return time.Duration(c.ConnectTimeoutSeconds) * time.Second
}

// uri builds a mongodb:// connection URI from the config. It is pure (no I/O)
// so it can be unit-tested without a server.
func (c Config) uri() string {
	u := url.URL{
		Scheme: "mongodb",
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.port())),
		Path:   "/",
	}
	if c.Database != "" {
		u.Path = "/" + c.Database
	}
	if c.User != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}
	q := url.Values{}
	if c.TLS {
		q.Set("tls", "true")
		if c.TLSSkipVerify {
			q.Set("tlsInsecure", "true")
		}
	}
	ms := int(c.connectTimeout() / time.Millisecond)
	q.Set("connectTimeoutMS", strconv.Itoa(ms))
	q.Set("serverSelectionTimeoutMS", strconv.Itoa(ms))
	u.RawQuery = q.Encode()
	return u.String()
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("mongodb: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("mongodb: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if c.Host == "" {
		return fmt.Errorf("mongodb: %q is required", "host")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("mongodb: port %d out of range 1-65535", c.Port)
	}
	if c.Password != "" && c.User == "" {
		return fmt.Errorf("mongodb: password set but user is empty")
	}
	if c.TLSSkipVerify && !c.TLS {
		return fmt.Errorf("mongodb: tls_skip_verify has no effect unless tls is true")
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("mongodb: connect_timeout_seconds must not be negative")
	}
	return nil
}
