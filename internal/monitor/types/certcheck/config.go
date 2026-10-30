package certcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// defaultPort is the TLS port assumed when the config omits one.
const defaultPort = 443

// Config is the jsonb monitor configuration for the cert type. It watches the
// TLS certificate of a host:port for expiry without inspecting any payload.
type Config struct {
	// Host is the target hostname or IP address. Required.
	Host string `json:"host"`
	// Port is the target TLS port; 0 means the default of 443.
	Port int `json:"port,omitempty"`
	// ServerName overrides the SNI/verification name sent in the handshake;
	// empty uses Host.
	ServerName string `json:"server_name,omitempty"`
	// WarnDays marks the monitor DOWN once the certificate has this many days
	// or fewer remaining, even though it is still technically valid. 0
	// disables the early warning (only an expired certificate is DOWN).
	WarnDays int `json:"warn_days,omitempty"`
	// IgnoreTLSErrors skips chain/name verification so the certificate can
	// still be inspected for expiry on self-signed or otherwise untrusted
	// targets. Expiry is always evaluated by Statwarden regardless of this flag.
	IgnoreTLSErrors bool `json:"ignore_tls_errors,omitempty"`
}

// port returns the configured port or the default.
func (c Config) port() int {
	if c.Port == 0 {
		return defaultPort
	}
	return c.Port
}

// parseConfig decodes the jsonb blob, rejecting unknown fields.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("cert: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("cert: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("cert: %q is required", "host")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("cert: port %d out of range 0-65535", c.Port)
	}
	if c.WarnDays < 0 {
		return fmt.Errorf("cert: warn_days must not be negative")
	}
	return nil
}
