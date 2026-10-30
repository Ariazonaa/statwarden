package dockercheck

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// defaultEndpoint is the local Docker Engine socket used when Endpoint is unset.
const defaultEndpoint = "unix:///var/run/docker.sock"

// defaultConnectTimeout is the connect/request timeout (seconds) when unset.
const defaultConnectTimeout = 10

// Required container states. StateRunning only asserts the container is
// running; StateHealthy additionally requires a green health check.
const (
	StateRunning = "running"
	StateHealthy = "healthy"
)

// Config is the jsonb monitor configuration for the docker type. The same type
// serves the local-socket check (endpoint "unix://…") and the remote Docker API
// check (endpoint "tcp://host:2376" with optional TLS client credentials).
type Config struct {
	// Endpoint is the Docker Engine address: a unix socket
	// ("unix:///var/run/docker.sock", the default) or a TCP host
	// ("tcp://dockerhost:2376"). Required in practice; empty means the default
	// local socket.
	Endpoint string `json:"endpoint,omitempty"`
	// Container is the container name or ID to inspect. Required.
	Container string `json:"container"`
	// RequiredState is the assertion: "running" (default) or "healthy". The
	// latter requires the image to define a HEALTHCHECK.
	RequiredState string `json:"required_state,omitempty"`

	// TLSCA is a PEM-encoded CA bundle used to verify a remote TCP endpoint's
	// server certificate. Optional; when empty the system roots are used.
	TLSCA string `json:"tls_ca,omitempty"`
	// TLSCert and TLSKey are a PEM-encoded client certificate/key pair for
	// mutual TLS against a remote endpoint. Both or neither.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// IgnoreTLSErrors disables server certificate verification for remote
	// endpoints with self-signed certificates.
	IgnoreTLSErrors bool `json:"ignore_tls_errors,omitempty"`

	// ConnectTimeoutSeconds bounds the request; 0 means 10s.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds,omitempty"`
}

// Secrets implements the API's optional secretConfig interface: these fields
// are masked in read responses and preserved on write when the placeholder is
// submitted back.
//
// This type declared none, so every field of a Docker monitor's configuration
// was handed to anyone who could read the monitor — including tls_key, which is
// the PRIVATE half of an mTLS pair and grants whoever holds it control of the
// Docker daemon it authenticates to. tls_ca and tls_cert are deliberately not
// listed: both are public halves, and masking them would hide from an operator
// which CA and certificate a check is pinned to for no gain.
func (*Checker) Secrets() []string {
	return []string{"tls_key"}
}

// endpoint returns the effective endpoint string.
func (c Config) endpoint() string {
	if c.Endpoint == "" {
		return defaultEndpoint
	}
	return c.Endpoint
}

// requiredState returns the effective required state.
func (c Config) requiredState() string {
	if c.RequiredState == "" {
		return StateRunning
	}
	return c.RequiredState
}

// connectTimeout returns the effective timeout in seconds.
func (c Config) connectTimeout() int {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout
	}
	return c.ConnectTimeoutSeconds
}

// usesTLS reports whether the endpoint should be dialled over TLS: either the
// caller supplied TLS material, or the endpoint is an explicit https scheme.
func (c Config) usesTLS() bool {
	if c.TLSCA != "" || c.TLSCert != "" || c.IgnoreTLSErrors {
		return true
	}
	return strings.HasPrefix(c.endpoint(), "https://")
}

// transport describes how to reach the Engine API: whether the connection is a
// unix socket (with its path) or a TCP host, and the base URL host used when
// building request URLs.
type transport struct {
	unix     bool
	unixPath string
	network  string // "unix" or "tcp"
	address  string // socket path or host:port
	scheme   string // "http" or "https"
	host     string // Host component for the request URL
}

// resolveTransport parses the endpoint into a transport descriptor without
// performing any I/O, so URL construction is unit-testable.
func (c Config) resolveTransport() (transport, error) {
	ep := c.endpoint()
	u, err := url.Parse(ep)
	if err != nil {
		return transport{}, fmt.Errorf("docker: invalid endpoint %q: %w", ep, err)
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			path = u.Opaque
		}
		if path == "" {
			return transport{}, fmt.Errorf("docker: unix endpoint %q has no socket path", ep)
		}
		return transport{
			unix:     true,
			unixPath: path,
			network:  "unix",
			address:  path,
			scheme:   "http",
			host:     "docker",
		}, nil
	case "tcp", "http", "https":
		if u.Host == "" {
			return transport{}, fmt.Errorf("docker: endpoint %q has no host", ep)
		}
		scheme := "http"
		if u.Scheme == "https" || c.usesTLS() {
			scheme = "https"
		}
		return transport{
			network: "tcp",
			address: u.Host,
			scheme:  scheme,
			host:    u.Host,
		}, nil
	default:
		return transport{}, fmt.Errorf("docker: unsupported endpoint scheme %q (use unix, tcp, http or https)", u.Scheme)
	}
}

// inspectURL builds the container-inspect request URL for a resolved transport.
func (t transport) inspectURL(container string) string {
	return fmt.Sprintf("%s://%s/containers/%s/json", t.scheme, t.host, url.PathEscape(container))
}

// tlsConfig builds the client TLS configuration from the PEM material. It
// performs no I/O and is safe to call from validate().
func (c Config) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: c.IgnoreTLSErrors, //nolint:gosec // opt-in per monitor for self-signed remote daemons
	}
	if c.TLSCA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.TLSCA)) {
			return nil, fmt.Errorf("docker: tls_ca is not a valid PEM bundle")
		}
		cfg.RootCAs = pool
	}
	if c.TLSCert != "" || c.TLSKey != "" {
		pair, err := tls.X509KeyPair([]byte(c.TLSCert), []byte(c.TLSKey))
		if err != nil {
			return nil, fmt.Errorf("docker: invalid tls_cert/tls_key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return cfg, nil
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("docker: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("docker: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no network I/O.
func (c Config) validate() error {
	if c.Container == "" {
		return fmt.Errorf("docker: %q is required", "container")
	}
	if _, err := c.resolveTransport(); err != nil {
		return err
	}
	switch c.RequiredState {
	case "", StateRunning, StateHealthy:
	default:
		return fmt.Errorf("docker: unknown required_state %q (use running or healthy)", c.RequiredState)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("docker: tls_cert and tls_key must be set together")
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("docker: connect_timeout_seconds must not be negative")
	}
	// Fail fast on malformed TLS material even when the endpoint is local.
	if c.TLSCA != "" || c.TLSCert != "" {
		if _, err := c.tlsConfig(); err != nil {
			return err
		}
	}
	return nil
}
