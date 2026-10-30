package udpcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Accepted values for payload_encoding. They decode both the datagram that is
// sent and the bytes an operator expects back, so a binary probe is written
// once and asserted in the same alphabet.
const (
	encodingText   = "text"
	encodingHex    = "hex"
	encodingBase64 = "base64"
)

// defaultResponseTimeout is the wait for a reply (seconds) when unset.
const defaultResponseTimeout = 5

// Config is the jsonb monitor configuration for the udp type.
type Config struct {
	// Host is the target hostname or IP address. Required.
	Host string `json:"host"`
	// Port is the target UDP port (1-65535). Required.
	Port int `json:"port"`
	// Payload is the datagram to send, decoded per PayloadEncoding. Empty
	// sends a zero-length datagram: enough to draw an ICMP port unreachable
	// from a closed port, rarely enough to draw a reply from an open one.
	Payload string `json:"payload,omitempty"`
	// PayloadEncoding decodes Payload and Expect: "text" (default), "hex" or
	// "base64". Binary protocols need one of the latter two.
	PayloadEncoding string `json:"payload_encoding,omitempty"`
	// ExpectResponse requires a reply for UP; nil/absent means true.
	//
	// False is the only assertion available against a fire-and-forget service
	// — syslog, statsd, a metrics sink — that answers nothing by design: the
	// check then reports UP unless the socket reports an error, which on a
	// reachable host means an ICMP port unreachable. It is the weaker mode
	// and not the default, because a firewall that drops ICMP is
	// indistinguishable from a healthy listener.
	ExpectResponse *bool `json:"expect_response,omitempty"`
	// Expect must appear somewhere in the reply, decoded per
	// PayloadEncoding. It asserts on a reply and so implies one.
	Expect string `json:"expect,omitempty"`
	// ResponseTimeoutSeconds bounds the wait for a reply; 0 means 5s. The
	// monitor's own check timeout still caps it.
	ResponseTimeoutSeconds int `json:"response_timeout_seconds,omitempty"`
}

// encoding returns the effective payload encoding.
func (c Config) encoding() string {
	if c.PayloadEncoding == "" {
		return encodingText
	}
	return c.PayloadEncoding
}

// payloadBytes decodes the datagram to send.
func (c Config) payloadBytes() ([]byte, error) {
	return decodeBytes(c.encoding(), "payload", c.Payload)
}

// expectBytes decodes the assertion; empty means no assertion.
func (c Config) expectBytes() ([]byte, error) {
	return decodeBytes(c.encoding(), "expect", c.Expect)
}

// expectResponse reports whether a reply is required for UP.
func (c Config) expectResponse() bool {
	return c.ExpectResponse == nil || *c.ExpectResponse
}

// responseTimeout returns the effective wait for a reply.
func (c Config) responseTimeout() time.Duration {
	if c.ResponseTimeoutSeconds == 0 {
		return defaultResponseTimeout * time.Second
	}
	return time.Duration(c.ResponseTimeoutSeconds) * time.Second
}

// decodeBytes turns a configured string into the bytes it stands for. field
// names the config key so an error points at the one that failed.
func decodeBytes(encoding, field, value string) ([]byte, error) {
	switch encoding {
	case encodingText:
		return []byte(value), nil
	case encodingHex:
		// Operators write hex in groups ("00 01 ff") — strip the whitespace
		// rather than reject the readable spelling.
		b, err := hex.DecodeString(strings.Join(strings.Fields(value), ""))
		if err != nil {
			return nil, fmt.Errorf("udp: %q is not valid hex: %w", field, err)
		}
		return b, nil
	case encodingBase64:
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("udp: %q is not valid base64: %w", field, err)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("udp: payload_encoding %q must be one of %s, %s, %s",
			encoding, encodingText, encodingHex, encodingBase64)
	}
}

// parseConfig decodes the jsonb blob, rejecting unknown fields so typos in
// hand-written configs surface as errors.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("udp: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("udp: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; it must not perform
// I/O. Decoding the payload here is what keeps Check's own decode from ever
// being the thing that fails a probe.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("udp: %q is required", "host")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("udp: port %d out of range 1-65535", c.Port)
	}
	if c.ResponseTimeoutSeconds < 0 {
		return fmt.Errorf("udp: response_timeout_seconds must not be negative")
	}
	if _, err := c.payloadBytes(); err != nil {
		return err
	}
	if _, err := c.expectBytes(); err != nil {
		return err
	}
	if c.Expect != "" && !c.expectResponse() {
		return fmt.Errorf("udp: %q asserts on a reply and cannot be combined with expect_response=false", "expect")
	}
	return nil
}
