// Package certcheck implements the certificate-only monitor type: it opens a
// TLS connection to a host:port purely to inspect the presented leaf
// certificate and alerts on (impending) expiry. Unlike httpcheck it performs
// no content check — its whole purpose is watching TLS validity, so an expired
// or soon-to-expire certificate is a DOWN result with a days-remaining
// message. It reuses the shared sslcheck helpers for certificate extraction.
package certcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
	"github.com/Ariazonaa/statwarden/internal/monitor/sslcheck"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "cert"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// Checker implements registry.Checker for certificate-only checks.
type Checker struct {
	// rootCAs augments certificate verification in tests; nil uses the system
	// roots.
	rootCAs *x509.CertPool
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

// Check implements registry.Checker: it completes a TLS handshake, captures
// the leaf certificate and maps its remaining validity to UP/DOWN. Dial or
// handshake failures are DOWN results; only an unusable config errors.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.port()))
	serverName := c.ServerName
	if serverName == "" {
		serverName = c.Host
	}

	start := time.Now()
	down := func(msg string) (domain.CheckResult, error) {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: time.Since(start),
			Message: msg,
		}, nil
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return down(err.Error())
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return down(fmt.Sprintf("set deadline: %v", err))
		}
	}

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: c.IgnoreTLSErrors, //nolint:gosec // opt-in per monitor; expiry is still evaluated
		RootCAs:            ch.rootCAs,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return down(fmt.Sprintf("tls handshake: %v", err))
	}
	state := tlsConn.ConnectionState()
	latency := time.Since(start)

	now := time.Now()
	cert := sslcheck.FromTLSState(&state, now)
	if cert == nil {
		return down("no peer certificate presented")
	}

	result := domain.CheckResult{Latency: latency, Cert: cert}
	switch {
	case cert.DaysRemaining < 0:
		result.Status = domain.StatusDown
		result.Message = fmt.Sprintf("certificate expired %d day(s) ago (valid until %s)",
			-cert.DaysRemaining, cert.ValidTo.Format(time.RFC3339))
	case c.WarnDays > 0 && cert.DaysRemaining <= c.WarnDays:
		result.Status = domain.StatusDown
		result.Message = fmt.Sprintf("certificate expires in %d day(s), at or below the %d-day warning threshold",
			cert.DaysRemaining, c.WarnDays)
	default:
		result.Status = domain.StatusUp
		result.Message = fmt.Sprintf("certificate valid, %d day(s) remaining", cert.DaysRemaining)
	}
	return result, nil
}
