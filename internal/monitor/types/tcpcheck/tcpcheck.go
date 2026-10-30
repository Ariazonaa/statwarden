// Package tcpcheck implements the TCP port-connect monitor type: it opens a
// TCP connection to a host:port and reports UP when the connection is
// accepted (e.g. SSH on 22, SMTP on 25). It performs no protocol negotiation
// beyond the TCP handshake.
package tcpcheck

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "tcp"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// Checker implements registry.Checker for TCP port-connect checks.
type Checker struct{}

func init() {
	registry.Register(&Checker{})
}

// Type implements registry.Checker.
func (*Checker) Type() string { return TypeName }

// DefaultTimeout implements registry.TimeoutDefaulter.
func (*Checker) DefaultTimeout() time.Duration { return defaultTimeout }

// Validate implements registry.Checker.
func (*Checker) Validate(cfg json.RawMessage) error {
	c, err := parseConfig(cfg)
	if err != nil {
		return err
	}
	return c.validate()
}

// Check implements registry.Checker: it dials the TCP endpoint and reports UP
// when the handshake completes. A refused/unreachable target is a DOWN
// result, not an error; only an unusable config returns an error.
func (*Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	var dialer net.Dialer
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: err.Error(),
		}, nil
	}
	_ = conn.Close()

	return domain.CheckResult{
		Status:  domain.StatusUp,
		Latency: latency,
		Message: "tcp connection to " + addr + " accepted",
	}, nil
}
