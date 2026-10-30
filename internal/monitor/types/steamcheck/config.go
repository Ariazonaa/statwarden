package steamcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// defaultConnectTimeout is the query timeout (seconds) when unset.
const defaultConnectTimeout = 5

// Config is the jsonb monitor configuration for the steam type. The check sends
// an A2S_INFO query (the Steam server-query protocol) and reports UP when the
// game server answers with a valid info response.
type Config struct {
	// Host is the game server hostname or IP. Required.
	Host string `json:"host"`
	// Port is the game server's query UDP port (commonly 27015). Required.
	Port int `json:"port"`
	// ConnectTimeoutSeconds bounds the UDP query; 0 means 5s.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds,omitempty"`
}

// connectTimeout returns the effective timeout in seconds.
func (c Config) connectTimeout() int {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout
	}
	return c.ConnectTimeoutSeconds
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("steam: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("steam: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no network I/O.
func (c Config) validate() error {
	if c.Host == "" {
		return fmt.Errorf("steam: %q is required", "host")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("steam: port %d out of range 1-65535", c.Port)
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("steam: connect_timeout_seconds must not be negative")
	}
	return nil
}
