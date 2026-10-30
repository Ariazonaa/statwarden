package mqttcheck

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// defaultConnectTimeout is the connect/message-wait timeout (seconds) when unset.
const defaultConnectTimeout = 10

// Broker URL schemes paho understands.
var brokerSchemes = map[string]struct{}{
	"tcp":   {},
	"mqtt":  {},
	"ssl":   {},
	"tls":   {},
	"mqtts": {},
	"ws":    {},
	"wss":   {},
}

// tlsSchemes are the broker schemes that imply transport security.
var tlsSchemes = map[string]struct{}{
	"ssl":   {},
	"tls":   {},
	"mqtts": {},
	"wss":   {},
}

// Config is the jsonb monitor configuration for the mqtt type. The check
// connects to the broker and, when a Topic is given, asserts that a message
// arrives (optionally with an expected payload). PublishPayload turns the check
// into a self-contained publish/subscribe round-trip.
type Config struct {
	// Broker is the broker URL, e.g. "tcp://broker:1883" or
	// "ssl://broker:8883". Required.
	Broker string `json:"broker"`
	// ClientID is the MQTT client identifier; empty lets the library generate
	// one.
	ClientID string `json:"client_id,omitempty"`
	// Username and Password authenticate to the broker. Optional.
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`

	// Topic, when set, is subscribed to; the check then waits for a message.
	Topic string `json:"topic,omitempty"`
	// PublishPayload, when set (requires Topic), is published to Topic after
	// subscribing, making the check a self-contained round-trip.
	PublishPayload string `json:"publish_payload,omitempty"`
	// ExpectedPayload, when set (requires Topic), must equal the payload of the
	// received message.
	ExpectedPayload string `json:"expected_payload,omitempty"`

	// TLSCA is a PEM CA bundle to verify the broker certificate for TLS
	// schemes; empty uses the system roots.
	TLSCA string `json:"tls_ca,omitempty"`
	// ServerName overrides the TLS verification hostname; empty uses the broker
	// host.
	ServerName string `json:"server_name,omitempty"`
	// IgnoreTLSErrors disables certificate verification for self-signed
	// brokers.
	IgnoreTLSErrors bool `json:"ignore_tls_errors,omitempty"`

	// ConnectTimeoutSeconds bounds connect and message wait; 0 means 10s.
	ConnectTimeoutSeconds int `json:"connect_timeout_seconds,omitempty"`
}

// Secrets lists the JSON field names in Config that carry credentials. The API
// redacts them from read responses and keeps a submitted redaction placeholder
// as "unchanged"; the SecretsBox seals the whole config at rest.
func (*Checker) Secrets() []string {
	return []string{"password"}
}

// connectTimeout returns the effective timeout in seconds.
func (c Config) connectTimeout() int {
	if c.ConnectTimeoutSeconds == 0 {
		return defaultConnectTimeout
	}
	return c.ConnectTimeoutSeconds
}

// brokerScheme returns the broker URL scheme, or "" when unparseable.
func (c Config) brokerScheme() string {
	u, err := url.Parse(c.Broker)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme)
}

// usesTLS reports whether the broker scheme implies transport security.
func (c Config) usesTLS() bool {
	_, ok := tlsSchemes[c.brokerScheme()]
	return ok
}

// tlsConfig builds the client TLS configuration from the config's PEM material.
// It performs no I/O and is safe to call from validate().
func (c Config) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         c.ServerName,
		InsecureSkipVerify: c.IgnoreTLSErrors, //nolint:gosec // opt-in per monitor for self-signed brokers
	}
	if c.TLSCA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(c.TLSCA)) {
			return nil, fmt.Errorf("mqtt: tls_ca is not a valid PEM bundle")
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
		return c, fmt.Errorf("mqtt: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("mqtt: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no network I/O.
func (c Config) validate() error {
	if c.Broker == "" {
		return fmt.Errorf("mqtt: %q is required", "broker")
	}
	u, err := url.Parse(c.Broker)
	if err != nil {
		return fmt.Errorf("mqtt: invalid broker %q: %w", c.Broker, err)
	}
	if _, ok := brokerSchemes[strings.ToLower(u.Scheme)]; !ok {
		return fmt.Errorf("mqtt: unsupported broker scheme %q (use tcp, ssl, tls, ws or wss)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("mqtt: broker %q has no host", c.Broker)
	}
	if c.Password != "" && c.Username == "" {
		return fmt.Errorf("mqtt: password set but username is empty")
	}
	if c.PublishPayload != "" && c.Topic == "" {
		return fmt.Errorf("mqtt: publish_payload requires topic")
	}
	if c.ExpectedPayload != "" && c.Topic == "" {
		return fmt.Errorf("mqtt: expected_payload requires topic")
	}
	if !c.usesTLS() && (c.TLSCA != "" || c.ServerName != "" || c.IgnoreTLSErrors) {
		return fmt.Errorf("mqtt: TLS options set but broker scheme is not TLS")
	}
	if c.TLSCA != "" {
		if _, err := c.tlsConfig(); err != nil {
			return err
		}
	}
	if c.ConnectTimeoutSeconds < 0 {
		return fmt.Errorf("mqtt: connect_timeout_seconds must not be negative")
	}
	return nil
}
