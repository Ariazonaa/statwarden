package snmpcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// defaultPort is the SNMP UDP port assumed when the config omits one.
const defaultPort = 161

// SNMP protocol versions accepted in Config.Version.
const (
	Version2c = "2c"
	Version3  = "3"
)

// v3 security levels accepted in Config.SecurityLevel.
const (
	SecNoAuthNoPriv = "noAuthNoPriv"
	SecAuthNoPriv   = "authNoPriv"
	SecAuthPriv     = "authPriv"
)

// Numeric comparison operators accepted in Config.Operator.
const (
	OpEq  = "=="
	OpNe  = "!="
	OpGt  = ">"
	OpGte = ">="
	OpLt  = "<"
	OpLte = "<="
)

// Config is the jsonb monitor configuration for the snmp type.
type Config struct {
	// Host is the target device hostname or IP. Required.
	Host string `json:"host"`
	// Port is the SNMP UDP port; 0 means the default of 161.
	Port int `json:"port,omitempty"`
	// Version is "2c" (default) or "3".
	Version string `json:"version,omitempty"`
	// OID is the object identifier to read (dotted form, leading dot
	// optional). Required.
	OID string `json:"oid"`

	// Community is the v2c community string. Required for v2c.
	Community string `json:"community,omitempty"`

	// v3 User-Security-Model fields.
	Username      string `json:"username,omitempty"`
	SecurityLevel string `json:"security_level,omitempty"` // noAuthNoPriv|authNoPriv|authPriv
	AuthProtocol  string `json:"auth_protocol,omitempty"`  // MD5|SHA|SHA224|SHA256|SHA384|SHA512
	AuthPassword  string `json:"auth_password,omitempty"`
	PrivProtocol  string `json:"priv_protocol,omitempty"` // DES|AES|AES192|AES256
	PrivPassword  string `json:"priv_password,omitempty"`
	ContextName   string `json:"context_name,omitempty"`

	// ExpectedValue asserts the read value equals this string exactly
	// (numbers compared via their literal form). Mutually exclusive with the
	// numeric Operator/Threshold assertion.
	ExpectedValue string `json:"expected_value,omitempty"`
	// Operator + Threshold assert the read value numerically, e.g. Operator
	// ">" Threshold 80 marks DOWN when the value exceeds 80. Empty Operator
	// disables the numeric assertion.
	Operator  string   `json:"operator,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

// Secrets lists the JSON field names in Config that carry credentials. The API
// redacts them from read responses and keeps a submitted redaction placeholder
// as "unchanged"; the SecretsBox seals the whole config at rest. The v2c
// community string and the v3 auth/priv passwords are all shared secrets.
func (*Checker) Secrets() []string {
	return []string{"community", "auth_password", "priv_password"}
}

// port returns the configured port or the default.
func (c Config) port() int {
	if c.Port == 0 {
		return defaultPort
	}
	return c.Port
}

// version returns the configured version or the default.
func (c Config) version() string {
	if c.Version == "" {
		return Version2c
	}
	return c.Version
}

// parseConfig decodes the jsonb blob, rejecting unknown fields.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("snmp: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("snmp: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("snmp: %q is required", "host")
	}
	if strings.TrimSpace(c.OID) == "" {
		return fmt.Errorf("snmp: %q is required", "oid")
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("snmp: port %d out of range 0-65535", c.Port)
	}

	switch c.version() {
	case Version2c:
		if c.Community == "" {
			return fmt.Errorf("snmp: version 2c requires community")
		}
	case Version3:
		if err := c.validateV3(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("snmp: unsupported version %q (use %q or %q)", c.Version, Version2c, Version3)
	}

	return c.validateAssertion()
}

// validateV3 checks the User-Security-Model parameters for the chosen level.
func (c Config) validateV3() error {
	if c.Username == "" {
		return fmt.Errorf("snmp: version 3 requires username")
	}
	switch c.SecurityLevel {
	case SecNoAuthNoPriv:
	case SecAuthNoPriv:
		if err := c.requireAuth(); err != nil {
			return err
		}
	case SecAuthPriv:
		if err := c.requireAuth(); err != nil {
			return err
		}
		if _, ok := privProtocols[c.PrivProtocol]; !ok {
			return fmt.Errorf("snmp: security_level %q requires a valid priv_protocol (DES, AES, AES192, AES256)", SecAuthPriv)
		}
		if c.PrivPassword == "" {
			return fmt.Errorf("snmp: security_level %q requires priv_password", SecAuthPriv)
		}
	case "":
		return fmt.Errorf("snmp: version 3 requires security_level (noAuthNoPriv, authNoPriv or authPriv)")
	default:
		return fmt.Errorf("snmp: unknown security_level %q", c.SecurityLevel)
	}
	return nil
}

// requireAuth validates the authentication protocol/password pair.
func (c Config) requireAuth() error {
	if _, ok := authProtocols[c.AuthProtocol]; !ok {
		return fmt.Errorf("snmp: security_level %q requires a valid auth_protocol (MD5, SHA, SHA224, SHA256, SHA384, SHA512)", c.SecurityLevel)
	}
	if c.AuthPassword == "" {
		return fmt.Errorf("snmp: security_level %q requires auth_password", c.SecurityLevel)
	}
	return nil
}

// validateAssertion checks the value-assertion fields are coherent.
func (c Config) validateAssertion() error {
	if c.ExpectedValue != "" && c.Operator != "" {
		return fmt.Errorf("snmp: expected_value and operator are mutually exclusive")
	}
	if c.Operator != "" {
		if !validOperator(c.Operator) {
			return fmt.Errorf("snmp: unknown operator %q (use ==, !=, >, >=, < or <=)", c.Operator)
		}
		if c.Threshold == nil {
			return fmt.Errorf("snmp: operator %q requires threshold", c.Operator)
		}
	}
	if c.Threshold != nil && c.Operator == "" {
		return fmt.Errorf("snmp: threshold requires operator")
	}
	return nil
}

// validOperator reports whether op is a supported comparison operator.
func validOperator(op string) bool {
	switch op {
	case OpEq, OpNe, OpGt, OpGte, OpLt, OpLte:
		return true
	default:
		return false
	}
}
