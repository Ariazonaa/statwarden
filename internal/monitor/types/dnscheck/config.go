package dnscheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/miekg/dns"
)

// Supported DNS record types (as stored in record_type). These map to the
// miekg/dns qtype constants in qtype().
const (
	RecordA     = "A"
	RecordAAAA  = "AAAA"
	RecordCNAME = "CNAME"
	RecordMX    = "MX"
	RecordTXT   = "TXT"
	RecordNS    = "NS"
	RecordSOA   = "SOA"
	RecordSRV   = "SRV"
	RecordPTR   = "PTR"
	RecordCAA   = "CAA"
)

// Config is the jsonb monitor configuration for the dns type.
type Config struct {
	// Host is the name to resolve. For PTR records it may be an IP address,
	// which is converted to its reverse (in-addr.arpa/ip6.arpa) form. Required.
	Host string `json:"host"`
	// RecordType is the queried record type; defaults to A. One of A, AAAA,
	// CNAME, MX, TXT, NS, SOA, SRV, PTR, CAA.
	RecordType string `json:"record_type,omitempty"`
	// Resolver is the DNS server to query as host or host:port; :53 is assumed
	// when the port is omitted. Empty uses the system resolver.
	Resolver string `json:"resolver,omitempty"`
	// Expected optionally asserts the answer set: at least one returned record
	// must match one of these values (case-insensitively, trailing dots
	// ignored). Empty means a non-empty answer is enough.
	Expected []string `json:"expected,omitempty"`
}

// recordType returns the configured record type or the default.
func (c Config) recordType() string {
	if c.RecordType == "" {
		return RecordA
	}
	return strings.ToUpper(c.RecordType)
}

// qtype maps the config record type to a miekg/dns query type. The bool is
// false for unknown types.
func qtype(recordType string) (uint16, bool) {
	switch strings.ToUpper(recordType) {
	case RecordA:
		return dns.TypeA, true
	case RecordAAAA:
		return dns.TypeAAAA, true
	case RecordCNAME:
		return dns.TypeCNAME, true
	case RecordMX:
		return dns.TypeMX, true
	case RecordTXT:
		return dns.TypeTXT, true
	case RecordNS:
		return dns.TypeNS, true
	case RecordSOA:
		return dns.TypeSOA, true
	case RecordSRV:
		return dns.TypeSRV, true
	case RecordPTR:
		return dns.TypePTR, true
	case RecordCAA:
		return dns.TypeCAA, true
	default:
		return 0, false
	}
}

// resolverAddr returns the resolver host:port, defaulting the port to 53.
func (c Config) resolverAddr() string {
	r := strings.TrimSpace(c.Resolver)
	if r == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(r); err != nil {
		return net.JoinHostPort(r, "53")
	}
	return r
}

// parseConfig decodes the jsonb blob, rejecting unknown fields.
func parseConfig(cfg json.RawMessage) (Config, error) {
	var c Config
	if len(cfg) == 0 {
		return c, fmt.Errorf("dns: empty config")
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("dns: invalid config: %w", err)
	}
	return c, nil
}

// validate performs the semantic checks behind Validate; no I/O.
func (c Config) validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("dns: %q is required", "host")
	}
	if _, ok := qtype(c.recordType()); !ok {
		return fmt.Errorf("dns: unsupported record_type %q (use one of A, AAAA, CNAME, MX, TXT, NS, SOA, SRV, PTR, CAA)", c.RecordType)
	}
	if c.Resolver != "" {
		if _, _, err := net.SplitHostPort(c.resolverAddr()); err != nil {
			return fmt.Errorf("dns: invalid resolver %q: %w", c.Resolver, err)
		}
	}
	for _, e := range c.Expected {
		if strings.TrimSpace(e) == "" {
			return fmt.Errorf("dns: expected values must not be blank")
		}
	}
	return nil
}
