package grpccheck

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"strings"
)

// defaultConnectTimeout is the connect/RPC timeout (seconds) when unset.
const defaultConnectTimeout = 10

// defaultExpectedStatus is asserted when the config names none: a plain
// health check passes only when the target reports SERVING.
const defaultExpectedStatus = "SERVING"

// Serving statuses defined by grpc.health.v1.HealthCheckResponse.ServingStatus.
var servingStatuses = map[string]struct{}{
	"UNKNOWN":         {},
	"SERVING":         {},
	"NOT_SERVING":     {},
	"SERVICE_UNKNOWN": {},
}

// Config is the jsonb monitor configuration for the grpc type. It covers both
// the plain gRPC health-check (default ExpectedStatus "SERVING") and the
// keyword variant, where ExpectedStatus asserts a specific serving status
// returned for a named service.
type Config struct {
	// Target is the gRPC endpoint host:port. Required.
	Target string `json:"target"`
	// Service is the health service name passed to Health/Check; empty asks for
	// the server's overall status.
	Service string `json:"service,omitempty"`
	// ExpectedStatus is the serving status the check asserts: SERVING
	// (default), NOT_SERVING, UNKNOWN or SERVICE_UNKNOWN. This is the keyword
	// variant.
	ExpectedStatus string `json:"expected_status,omitempty"`

	// TLS enables transport security. When false the connection is plaintext
	// (h2c).
	TLS bool `json:"tls,omitempty"`
	// TLSCA is a PEM CA bundle used to verify the server certificate; empty
	// uses the system roots.
	TLSCA string `json:"tls_ca,omitempty"`
	// ServerName overrides the SNI/verification hostname; empty uses the host
	// from Target.
	ServerName string `json:"server_name,omitempty"`
	// IgnoreTLSErrors disables certificate verification for self-signed
	// endpoints. Requires TLS.
	IgnoreTLSErrors bool `json:"ignore_tls_errors,omitempty"`

	// ConnectTimeoutSeconds bounds the connect + RPC; 0 means 10s.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds,omitempty"`
}

// expectedStatus returns the effective expected serving status, upper-cased.
func (c Config) expectedStatus() string {
	if c.ExpectedStatus == "" {
		return defaultExpectedStatus
	}
	return strings.ToUpper(c.ExpectedStatus)
}

// connectTimeout returns the effective timeout in seconds.
func (c Config) connectTimeout() int {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout
	}
	return c.ConnectTimeoutSeconds
}

// tlsConfig builds the client TLS configuration from the config's PEM material.
// It performs no I/O and is safe to call from validate().
func (c Config) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.IgnoreTLSErrors, //nolint:gosec // opt-in per monitor for self-signed endpoints
	}
	if c.TLSCA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.TLSCA)) {
			return nil, fmt.Errorf("grpc: tls_ca is not a valid PEM bundle")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("grpc: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("grpc: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no network I/O.
func (c Config) validate() error {
	if c.Target == "" {
		return fmt.Errorf("grpc: %q is required", "target")
	}
	if strings.Contains(c.Target, "://") {
		return fmt.Errorf("grpc: target %q must be host:port without a scheme", c.Target)
	}
	if c.ExpectedStatus != "" {
		if _, ok := servingStatuses[strings.ToUpper(c.ExpectedStatus)]; !ok {
			return fmt.Errorf("grpc: unknown expected_status %q (use SERVING, NOT_SERVING, UNKNOWN or SERVICE_UNKNOWN)", c.ExpectedStatus)
		}
	}
	if !c.TLS && (c.TLSCA != "" || c.ServerName != "" || c.IgnoreTLSErrors) {
		return fmt.Errorf("grpc: TLS options set but tls is false")
	}
	if c.TLSCA != "" {
		if _, err := c.tlsConfig(); err != nil {
			return err
		}
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("grpc: connect_timeout_seconds must not be negative")
	}
	return nil
}
