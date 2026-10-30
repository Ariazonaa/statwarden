// Package pushcheck implements the push (heartbeat) monitor type. It is a
// passive type: Statwarden never probes the target. Instead the observed service
// (a cron job, backup script, …) POSTs to /api/v1/push/{token} on a schedule;
// each call records a heartbeat and resets the monitor's last-seen. A watchdog
// (see internal/monitor/push) marks the monitor DOWN when no heartbeat arrives
// within expected_interval + grace_period.
package pushcheck

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "push"

// Checker implements registry.PassiveChecker for push/heartbeat monitors.
type Checker struct{}

func init() {
	registry.Register(&Checker{})
}

// Type implements registry.Checker.
func (*Checker) Type() string { return TypeName }

// Passive implements registry.PassiveChecker: it marks this type as fed by the
// push endpoint and its watchdog rather than by the scheduler's timers.
func (*Checker) Passive() {}

// Validate implements registry.Checker.
func (*Checker) Validate(cfg json.RawMessage) error {
	c, err := parseConfig(cfg)
	if err != nil {
		return err
	}
	return c.validate()
}

// Check implements registry.Checker. The engine never probes a passive type,
// so this is only reachable via an explicit manual trigger; it reports the
// monitor as awaiting heartbeats rather than performing any I/O.
func (*Checker) Check(_ context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	if _, err := parseConfig(cfg); err != nil {
		return domain.CheckResult{}, err
	}
	return domain.CheckResult{
		Status:  domain.StatusUp,
		Message: "push monitor is passive; status is driven by incoming heartbeats",
	}, nil
}

// Deadline returns the instant by which the next heartbeat must arrive, given
// the time of the last heartbeat. The watchdog trips the monitor DOWN once the
// clock passes this deadline. A config error yields a zero time.
func Deadline(cfg json.RawMessage, lastSeen time.Time) (time.Time, bool) {
	c, err := parseConfig(cfg)
	if err != nil || c.validate() != nil {
		return time.Time{}, false
	}
	window := time.Duration(c.ExpectedIntervalSeconds+c.GracePeriodSeconds) * time.Second
	return lastSeen.Add(window), true
}
