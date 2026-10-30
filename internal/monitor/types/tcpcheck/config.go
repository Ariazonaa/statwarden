package tcpcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Config is the jsonb monitor configuration for the tcp type.
type Config struct {
	// Host is the target hostname or IP address. Required.
	Host string `json:"host"`
	// Port is the target TCP port (1-65535). Required.
	Port int `json:"port"`
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("tcp: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("tcp: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; it must not perform
// I/O.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("tcp: %q is required", "host")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("tcp: port %d out of range 1-65535", c.Port)
	}
	return nil
}
