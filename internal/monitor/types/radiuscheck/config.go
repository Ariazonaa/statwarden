package radiuscheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// defaultPort is the RADIUS authentication UDP port assumed when the config
// omits one.
const defaultPort = 1812

// Config is the jsonb monitor configuration for the radius type.
type Config struct {
	// Host is the RADIUS server hostname or IP. Required.
	Host string `json:"host"`
	// Port is the RADIUS auth UDP port; 0 means the default of 1812.
	Port int `json:"port,omitempty"`
	// Secret is the shared secret between Statwarden and the server. Required.
	Secret string `json:"secret"`
	// Username is the User-Name to authenticate. Required.
	Username string `json:"username"`
	// Password is the User-Password to authenticate.
	Password string `json:"password,omitempty"`
	// NASIdentifier optionally sets the NAS-Identifier attribute some servers
	// require to select a policy.
	NASIdentifier string `json:"nas_identifier,omitempty"`
}

// Secrets lists the JSON field names in Config that carry credentials. The API
// redacts them from read responses and keeps a submitted redaction placeholder
// as "unchanged"; the SecretsBox seals the whole config at rest. Both the
// RADIUS shared secret and the User-Password are sensitive.
func (*Checker) Secrets() []string {
	return []string{"secret", "password"}
}

// port returns the configured port or the default.
func (c Config) port() int {
	if c.Port == 0 {
		return defaultPort
	}
	return c.Port
}

// parseConfig decodes the jsonb blob, rejecting unknown fields.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("radius: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("radius: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("radius: %q is required", "host")
	}
	if c.Secret == "" {
		return fmt.Errorf("radius: %q is required", "secret")
	}
	if strings.TrimSpace(c.Username) == "" {
		return fmt.Errorf("radius: %q is required", "username")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("radius: port %d out of range 0-65535", c.Port)
	}
	return nil
}
