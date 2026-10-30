package pingcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Defaults applied when the config leaves a field unset.
const (
	defaultCount         = 4
	defaultLossThreshold = 100.0
)

// Config is the jsonb monitor configuration for the ping type.
type Config struct {
	// Host is the target hostname or IP address. Required.
	Host string `json:"host"`
	// Count is how many echo requests to send; 0 means the default of 4.
	Count int `json:"count,omitempty"`
	// PacketLossThresholdPercent marks the monitor DOWN when the measured loss
	// percentage exceeds this value. 0 means the default of 100 (DOWN only on
	// total loss); set e.g. 20 to alert once more than 20% of packets are lost.
	PacketLossThresholdPercent float64 `json:"packet_loss_threshold_percent,omitempty"`
}

// count returns the configured probe count or the default.
func (c Config) count() int {
	if c.Count == 0 {
		return defaultCount
	}
	return c.Count
}

// lossThreshold returns the configured loss threshold or the default.
func (c Config) lossThreshold() float64 {
	if c.PacketLossThresholdPercent == 0 {
		return defaultLossThreshold
	}
	return c.PacketLossThresholdPercent
}

// parseConfig decodes the jsonb blob, rejecting unknown fields.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("ping: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("ping: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("ping: %q is required", "host")
	}
	if c.Count < 0 {
		return fmt.Errorf("ping: count must not be negative")
	}
	if c.Count > 100 {
		return fmt.Errorf("ping: count %d exceeds the maximum of 100", c.Count)
	}
	if c.PacketLossThresholdPercent < 0 || c.PacketLossThresholdPercent > 100 {
		return fmt.Errorf("ping: packet_loss_threshold_percent %.1f out of range 0-100", c.PacketLossThresholdPercent)
	}
	return nil
}
