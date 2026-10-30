// Package snmpcheck implements the SNMP OID monitor type: it reads a single
// OID from a network device over SNMP v2c or v3 (User Security Model, all
// authentication/privacy levels) and either confirms the OID resolves or
// asserts its value against an exact string or a numeric threshold. The SNMP
// session is created behind an injectable factory so tests exercise the value
// assertion and result mapping with fake PDUs and never touch the network.
package snmpcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "snmp"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// authProtocols maps config names to gosnmp authentication protocols.
var authProtocols = map[string]gosnmp.SnmpV3AuthProtocol{
	"MD5":    gosnmp.MD5,
	"SHA":    gosnmp.SHA,
	"SHA224": gosnmp.SHA224,
	"SHA256": gosnmp.SHA256,
	"SHA384": gosnmp.SHA384,
	"SHA512": gosnmp.SHA512,
}

// privProtocols maps config names to gosnmp privacy protocols.
var privProtocols = map[string]gosnmp.SnmpV3PrivProtocol{
	"DES":    gosnmp.DES,
	"AES":    gosnmp.AES,
	"AES192": gosnmp.AES192,
	"AES256": gosnmp.AES256,
}

// session is the minimal SNMP transport Check needs; the concrete gosnmp
// implementation is created by the factory, and tests inject a fake.
type session interface {
	Get(oids []string) (*gosnmp.SnmpPacket, error)
	Close() error
}

// sessionFactory opens a session for a config, honouring ctx for the timeout.
type sessionFactory func(ctx context.Context, c Config) (session, error)

// Checker implements registry.Checker for SNMP OID checks.
type Checker struct {
	// newSession overrides the SNMP transport in tests; nil uses gosnmp.
	newSession sessionFactory
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

// Check implements registry.Checker: it reads the OID and maps its presence
// and value (per the configured assertion) to UP/DOWN. Connection/read
// failures are DOWN results; only an unusable config returns an error.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	factory := ch.newSession
	if factory == nil {
		factory = dialSession
	}

	start := time.Now()
	sess, err := factory(ctx, c)
	if err != nil {
		return domain.CheckResult{Status: domain.StatusDown, Latency: time.Since(start), Message: err.Error()}, nil
	}
	defer func() { _ = sess.Close() }()

	packet, err := sess.Get([]string{normalizeOID(c.OID)})
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{Status: domain.StatusDown, Latency: latency, Message: err.Error()}, nil
	}
	if len(packet.Variables) == 0 {
		return domain.CheckResult{Status: domain.StatusDown, Latency: latency, Message: "snmp: empty response for " + c.OID}, nil
	}

	status, message, extra := assess(c, packet.Variables[0])
	return domain.CheckResult{Status: status, Latency: latency, Message: message, Extra: extra}, nil
}

// assess maps a single PDU (and the config's assertion) to a status, message
// and Extra payload.
func assess(c Config, pdu gosnmp.SnmpPDU) (domain.MonitorStatus, string, json.RawMessage) {
	switch pdu.Type {
	case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView, gosnmp.Null:
		return domain.StatusDown, fmt.Sprintf("snmp: OID %s not present on device (%s)", c.OID, pdu.Type), nil
	}

	value := pduString(pdu)
	extra := extraJSON(c.OID, value)

	switch {
	case c.ExpectedValue != "":
		if value != c.ExpectedValue {
			return domain.StatusDown, fmt.Sprintf("snmp: OID %s = %q, want %q", c.OID, value, c.ExpectedValue), extra
		}
		return domain.StatusUp, fmt.Sprintf("snmp: OID %s = %q as expected", c.OID, value), extra
	case c.Operator != "":
		num, ok := pduFloat(pdu)
		if !ok {
			return domain.StatusDown, fmt.Sprintf("snmp: OID %s value %q is not numeric for operator %s", c.OID, value, c.Operator), extra
		}
		if !compare(num, c.Operator, *c.Threshold) {
			return domain.StatusDown, fmt.Sprintf("snmp: OID %s = %s, assertion %s %g failed", c.OID, value, c.Operator, *c.Threshold), extra
		}
		return domain.StatusUp, fmt.Sprintf("snmp: OID %s = %s satisfies %s %g", c.OID, value, c.Operator, *c.Threshold), extra
	default:
		return domain.StatusUp, fmt.Sprintf("snmp: OID %s = %s", c.OID, value), extra
	}
}

// compare evaluates "a op b".
func compare(a float64, op string, b float64) bool {
	switch op {
	case OpEq:
		return a == b
	case OpNe:
		return a != b
	case OpGt:
		return a > b
	case OpGte:
		return a >= b
	case OpLt:
		return a < b
	case OpLte:
		return a <= b
	default:
		return false
	}
}

// pduString renders a PDU value as a comparable string.
func pduString(pdu gosnmp.SnmpPDU) string {
	switch v := pdu.Value.(type) {
	case []byte:
		return string(v)
	case string:
		return v
	case nil:
		return ""
	default:
		if bi := gosnmp.ToBigInt(pdu.Value); bi != nil {
			return bi.String()
		}
		return fmt.Sprint(v)
	}
}

// pduFloat renders a numeric PDU value as a float; ok is false for
// non-numeric values (e.g. an OctetString that is not a number).
func pduFloat(pdu gosnmp.SnmpPDU) (float64, bool) {
	switch v := pdu.Value.(type) {
	case []byte:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	case float32:
		return float64(v), true
	case float64:
		return v, true
	default:
		if bi := gosnmp.ToBigInt(pdu.Value); bi != nil {
			f := new(big.Float).SetInt(bi)
			out, _ := f.Float64()
			return out, true
		}
		return 0, false
	}
}

// normalizeOID ensures a leading dot, matching gosnmp's expectation.
func normalizeOID(oid string) string {
	oid = strings.TrimSpace(oid)
	if !strings.HasPrefix(oid, ".") {
		return "." + oid
	}
	return oid
}

// dialSession is the default gosnmp transport.
func dialSession(ctx context.Context, c Config) (session, error) {
	g := &gosnmp.GoSNMP{
		Target:    c.Host,
		Port:      uint16(c.port()), //nolint:gosec // validated to 0-65535
		Transport: "udp",
		Retries:   1,
		Timeout:   defaultTimeout,
	}
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline); d > 0 {
			g.Timeout = d
		}
	}

	switch c.version() {
	case Version2c:
		g.Version = gosnmp.Version2c
		g.Community = c.Community
	case Version3:
		g.Version = gosnmp.Version3
		g.SecurityModel = gosnmp.UserSecurityModel
		g.MsgFlags = msgFlags(c.SecurityLevel)
		g.ContextName = c.ContextName
		usm := &gosnmp.UsmSecurityParameters{UserName: c.Username}
		if c.SecurityLevel == SecAuthNoPriv || c.SecurityLevel == SecAuthPriv {
			usm.AuthenticationProtocol = authProtocols[c.AuthProtocol]
			usm.AuthenticationPassphrase = c.AuthPassword
		}
		if c.SecurityLevel == SecAuthPriv {
			usm.PrivacyProtocol = privProtocols[c.PrivProtocol]
			usm.PrivacyPassphrase = c.PrivPassword
		}
		g.SecurityParameters = usm
	}

	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("snmp: connect %s: %w", c.Host, err)
	}
	return g, nil
}

// msgFlags maps a security level to gosnmp message flags.
func msgFlags(level string) gosnmp.SnmpV3MsgFlags {
	switch level {
	case SecAuthNoPriv:
		return gosnmp.AuthNoPriv
	case SecAuthPriv:
		return gosnmp.AuthPriv
	default:
		return gosnmp.NoAuthNoPriv
	}
}

// extraJSON marshals the read OID/value for the heartbeat Extra column.
func extraJSON(oid, value string) json.RawMessage {
	data, err := json.Marshal(struct {
		OID   string `json:"oid"`
		Value string `json:"value"`
	}{OID: oid, Value: value})
	if err != nil {
		return nil
	}
	return data
}
