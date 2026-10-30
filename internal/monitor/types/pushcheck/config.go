package pushcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Config is the jsonb monitor configuration for the push type. A push monitor
// is passive: the observed service calls Statwarden's push endpoint on a schedule,
// and a watchdog marks the monitor DOWN when a heartbeat fails to arrive within
// ExpectedInterval + GracePeriod.
type Config struct {
	// ExpectedIntervalSeconds is how often the service is expected to check
	// in. Required, >= 1.
	ExpectedIntervalSeconds int `json:"expected_interval_seconds"`
	// GracePeriodSeconds is the additional tolerance before a missing
	// heartbeat trips the monitor DOWN. 0 = no grace.
	GracePeriodSeconds int `json:"grace_period_seconds"`
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("push: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("push: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; it must not perform
// I/O.
func (c Config) validate() error {
	if c.ExpectedIntervalSeconds < 1 {
		return fmt.Errorf("push: expected_interval_seconds must be >= 1")
	}
	if c.GracePeriodSeconds < 0 {
		return fmt.Errorf("push: grace_period_seconds must be >= 0")
	}
	return nil
}
