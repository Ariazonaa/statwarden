// Package grpccheck implements the gRPC monitor type: it calls the standard
// grpc.health.v1.Health/Check RPC and reports UP when the target returns the
// expected serving status. With the default ExpectedStatus ("SERVING") it is a
// plain health check; with an explicit ExpectedStatus it is the keyword variant
// that asserts a specific status for a named service. Connection failures and an
// unexpected serving status map to a DOWN CheckResult rather than a Go error.
package grpccheck

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "grpc"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// Checker implements registry.Checker for the gRPC health family.
type Checker struct {
	// probe performs the live Health/Check RPC and returns the serving status
	// string. nil uses the real grpc-backed implementation; tests inject a fake
	// to exercise result mapping without a server.
	probe func(ctx context.Context, c Config) (string, error)
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

// Check implements registry.Checker. Connection failures and an unexpected
// serving status are DOWN results; only unusable configuration errors.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	probe := ch.probe
	if probe == nil {
		probe = liveCheck
	}

	start := time.Now()
	status, err := probe(ctx, c)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: err.Error(),
		}, nil
	}

	if status != c.expectedStatus() {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: fmt.Sprintf("serving status %q, want %q", status, c.expectedStatus()),
		}, nil
	}
	return domain.CheckResult{
		Status:  domain.StatusUp,
		Latency: latency,
		Message: fmt.Sprintf("health status %s", status),
	}, nil
}

// checkHealth issues the Health/Check RPC over an established connection and
// returns the serving status as its enum name. It is connection-agnostic so
// tests can drive it over an in-memory bufconn.
func checkHealth(ctx context.Context, conn grpc.ClientConnInterface, service string) (string, error) {
	client := grpc_health_v1.NewHealthClient(conn)
	resp, err := client.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: service})
	if err != nil {
		return "", err
	}
	return resp.GetStatus().String(), nil
}

// liveCheck dials the target and performs the real Health/Check RPC.
func liveCheck(ctx context.Context, c Config) (string, error) {
	dialCtx, cancel := context.WithTimeout(ctx, time.Duration(c.connectTimeout())*time.Second)
	defer cancel()

	creds := insecure.NewCredentials()
	if c.TLS {
		tlsCfg, err := c.tlsConfig()
		if err != nil {
			return "", err
		}
		creds = credentials.NewTLS(tlsCfg)
	}

	conn, err := grpc.NewClient(c.Target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	return checkHealth(dialCtx, conn, c.Service)
}
