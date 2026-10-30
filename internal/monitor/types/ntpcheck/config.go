package ntpcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Defaults applied when the config leaves a field zero.
const (
	defaultPort = 123
	// defaultMaxOffsetMs is one second. A LAN server sits within a few
	// milliseconds and a public pool within tens; a second of disagreement is
	// already a clock that is running away, long before TOTP's 30-second steps
	// or a certificate's validity window notice.
	defaultMaxOffsetMs = 1000
)

// Config is the jsonb monitor configuration for the ntp type.
type Config struct {
	// Host is the time server's hostname or IP address. Required.
	Host string `json:"host"`
	// Port is the NTP port; 0 means 123.
	Port int `json:"port,omitempty"`
	// MaxOffsetMs is the largest acceptable difference, in either direction,
	// between the server's clock and this host's; 0 means 1000 ms. The check
	// cannot tell which of the two clocks is wrong — it reports that they
	// disagree, which is the failure either way.
	MaxOffsetMs int `json:"max_offset_ms,omitempty"`
	// MaxStratum, when set, turns a server further than this from its
	// reference clock DOWN. 0 accepts any synchronised stratum (1-15).
	MaxStratum int `json:"max_stratum,omitempty"`
}

func (c Config) port() int {
	if c.Port == 0 {
		return defaultPort
	}
	return c.Port
}

func (c Config) maxOffset() time.Duration {
	if c.MaxOffsetMs == 0 {
		return defaultMaxOffsetMs * time.Millisecond
	}
	return time.Duration(c.MaxOffsetMs) * time.Millisecond
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("ntp: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("ntp: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; it must not do I/O.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("ntp: %q is required", "host")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("ntp: port %d out of range 1-65535", c.Port)
	}
	if c.MaxOffsetMs < 0 {
		return fmt.Errorf("ntp: max_offset_ms must not be negative")
	}
	// Stratum 16 means "unsynchronised" and 0 is a kiss-o'-death; a limit
	// outside 1-15 could only ever accept nothing or everything.
	if c.MaxStratum < 0 || c.MaxStratum > 15 {
		return fmt.Errorf("ntp: max_stratum must be between 1 and 15 (0 = any)")
	}
	return nil
}
