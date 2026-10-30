// Package radiuscheck implements the RADIUS authentication monitor type: it
// sends an Access-Request (User-Name + User-Password, optional NAS-Identifier)
// to a RADIUS server and reports UP when the server answers with
// Access-Accept. The packet exchange is injectable so tests exercise the
// response mapping with fake replies and never touch the network.
package radiuscheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "radius"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// exchangeFunc sends a request packet to addr and returns the reply; injected
// in tests.
type exchangeFunc func(ctx context.Context, packet *radius.Packet, addr string) (*radius.Packet, error)

// Checker implements registry.Checker for RADIUS authentication checks.
type Checker struct {
	// exchange overrides the RADIUS transport in tests; nil uses radius.Exchange.
	exchange exchangeFunc
}

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

// Check implements registry.Checker: it performs an Access-Request and maps the
// reply code to UP (Access-Accept) or DOWN (reject/challenge/no answer).
// Transport failures are DOWN results; only an unusable config returns an
// error.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	packet := radius.New(radius.CodeAccessRequest, []byte(c.Secret))
	if err := rfc2865.UserName_SetString(packet, c.Username); err != nil {
		return domain.CheckResult{}, fmt.Errorf("radius: set User-Name: %w", err)
	}
	if err := rfc2865.UserPassword_SetString(packet, c.Password); err != nil {
		return domain.CheckResult{}, fmt.Errorf("radius: set User-Password: %w", err)
	}
	if c.NASIdentifier != "" {
		if err := rfc2865.NASIdentifier_SetString(packet, c.NASIdentifier); err != nil {
			return domain.CheckResult{}, fmt.Errorf("radius: set NAS-Identifier: %w", err)
		}
	}

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.port()))
	exchange := ch.exchange
	if exchange == nil {
		exchange = radius.Exchange
	}

	start := time.Now()
	reply, err := exchange(ctx, packet, addr)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{Status: domain.StatusDown, Latency: latency, Message: err.Error()}, nil
	}

	if reply.Code == radius.CodeAccessAccept {
		return domain.CheckResult{
			Status:  domain.StatusUp,
			Latency: latency,
			Message: "radius: Access-Accept",
		}, nil
	}
	return domain.CheckResult{
		Status:  domain.StatusDown,
		Latency: latency,
		Message: fmt.Sprintf("radius: expected Access-Accept, got %s", reply.Code),
	}, nil
}
