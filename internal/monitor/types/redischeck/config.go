package redischeck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"
)

// defaultPort is the standard Redis TCP port.
const defaultPort = 6379

// defaultConnectTimeout is the connect timeout (seconds) when unset.
const defaultConnectTimeout = 10

// Config is the jsonb monitor configuration for the redis type.
type Config struct {
	// Host is the server hostname or IP. Required.
	Host string `json:"host"`
	// Port is the TCP port; 0 means the default 6379.
	Port int `json:"port,omitempty"`
	// Username is the ACL username (Redis 6+). Optional.
	Username string `json:"username,omitempty"`
	// Password is the AUTH password. Optional.
	Password string `json:"password,omitempty"`
	// Database is the numeric database index selected after connect.
	Database int `json:"database,omitempty"`
	// TLS enables a TLS (rediss) connection.
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

// addr returns the host:port dial address. Pure (no I/O), unit-testable.
func (c Config) addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.port()))
}

// connectTimeout returns the effective connect timeout.
func (c Config) connectTimeout() time.Duration {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout * time.Second
	}
	return time.Duration(c.ConnectTimeoutSeconds) * time.Second
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("redis: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("redis: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if c.Host == "" {
		return fmt.Errorf("redis: %q is required", "host")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("redis: port %d out of range 1-65535", c.Port)
	}
	if c.Database < 0 {
		return fmt.Errorf("redis: database index must not be negative")
	}
	if c.TLSSkipVerify && !c.TLS {
		return fmt.Errorf("redis: tls_skip_verify has no effect unless tls is true")
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("redis: connect_timeout_seconds must not be negative")
	}
	return nil
}
