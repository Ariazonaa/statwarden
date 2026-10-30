// Package dnscheck implements the DNS-record monitor type: it resolves a name
// for a chosen record type (A, AAAA, CNAME, MX, TXT, NS, SOA, SRV, PTR, CAA),
// optionally against a per-monitor resolver, and optionally asserts that the
// answer set contains an expected value. A missing/empty answer, a resolver
// failure, or a failed expected-value assertion is a DOWN result; the resolved
// records are attached to the heartbeat as Extra data.
package dnscheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "dns"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 10 * time.Second

// exchangeFunc performs one DNS query. It is a field on Checker so tests can
// inject canned responses; the default uses a real UDP dns.Client.
type exchangeFunc func(ctx context.Context, msg *dns.Msg, addr string) (*dns.Msg, error)

// Checker implements registry.Checker for DNS-record checks.
type Checker struct {
	// exchange overrides the query transport in tests; nil uses a real client.
	exchange exchangeFunc
	// resolvConf is the resolv.conf path used to find the system resolver when
	// a monitor sets none; empty uses the OS default. Overridable in tests.
	resolvConf string
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

// Check implements registry.Checker: it queries the resolver and maps the
// answer (and any expected-value assertion) to UP/DOWN. Resolver/network
// failures are DOWN results; only an unusable config or an unresolvable system
// resolver returns an error.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	qname, qt, err := c.query()
	if err != nil {
		return domain.CheckResult{}, err
	}
	addr, err := ch.resolver(c)
	if err != nil {
		return domain.CheckResult{}, err
	}

	msg := new(dns.Msg)
	msg.SetQuestion(qname, qt)
	msg.RecursionDesired = true

	start := time.Now()
	resp, err := ch.doExchange(ctx, msg, addr)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{Status: domain.StatusDown, Latency: latency, Message: err.Error()}, nil
	}
	if resp.Rcode != dns.RcodeSuccess {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: fmt.Sprintf("resolver returned %s for %s %s", dns.RcodeToString[resp.Rcode], c.recordType(), c.Host),
		}, nil
	}

	values := extractValues(resp.Answer, qt)
	result := domain.CheckResult{Latency: latency, Extra: extraJSON(c, values)}
	if len(values) == 0 {
		result.Status = domain.StatusDown
		result.Message = fmt.Sprintf("no %s records for %s", c.recordType(), c.Host)
		return result, nil
	}
	if len(c.Expected) > 0 {
		if !matchesExpected(values, c.Expected) {
			result.Status = domain.StatusDown
			result.Message = fmt.Sprintf("no %s record for %s matched %v (got %v)", c.recordType(), c.Host, c.Expected, values)
			return result, nil
		}
	}
	result.Status = domain.StatusUp
	result.Message = fmt.Sprintf("%d %s record(s): %s", len(values), c.recordType(), strings.Join(values, ", "))
	return result, nil
}

// query returns the FQDN and qtype for the config, converting PTR IP targets
// to their reverse form.
func (c Config) query() (string, uint16, error) {
	qt, ok := qtype(c.recordType())
	if !ok {
		return "", 0, fmt.Errorf("dns: unsupported record_type %q", c.RecordType)
	}
	name := c.Host
	if qt == dns.TypePTR {
		if arpa, err := dns.ReverseAddr(c.Host); err == nil {
			name = arpa
		}
	}
	return dns.Fqdn(name), qt, nil
}

// resolver returns the resolver address to query, falling back to the system
// resolver when the monitor sets none.
func (ch *Checker) resolver(c Config) (string, error) {
	if addr := c.resolverAddr(); addr != "" {
		return addr, nil
	}
	path := ch.resolvConf
	if path == "" {
		path = "/etc/resolv.conf"
	}
	conf, err := dns.ClientConfigFromFile(path)
	if err != nil || len(conf.Servers) == 0 {
		return "", fmt.Errorf("dns: no resolver configured and system resolver unavailable: %w", err)
	}
	port := conf.Port
	if port == "" {
		port = "53"
	}
	return net.JoinHostPort(conf.Servers[0], port), nil
}

// doExchange runs the injected or default DNS transport.
func (ch *Checker) doExchange(ctx context.Context, msg *dns.Msg, addr string) (*dns.Msg, error) {
	if ch.exchange != nil {
		return ch.exchange(ctx, msg, addr)
	}
	client := &dns.Client{Net: "udp"}
	resp, _, err := client.ExchangeContext(ctx, msg, addr)
	return resp, err
}

// extractValues renders each answer RR of the queried type into a canonical
// comparable string.
func extractValues(answers []dns.RR, qt uint16) []string {
	var out []string
	for _, rr := range answers {
		if rr.Header().Rrtype != qt {
			continue // ignore CNAME chains etc. in the answer section
		}
		out = append(out, rrValue(rr))
	}
	return out
}

// rrValue renders the value portion of a resource record.
func rrValue(rr dns.RR) string {
	switch v := rr.(type) {
	case *dns.A:
		return v.A.String()
	case *dns.AAAA:
		return v.AAAA.String()
	case *dns.CNAME:
		return strings.TrimSuffix(v.Target, ".")
	case *dns.MX:
		return fmt.Sprintf("%d %s", v.Preference, strings.TrimSuffix(v.Mx, "."))
	case *dns.TXT:
		return strings.Join(v.Txt, "")
	case *dns.NS:
		return strings.TrimSuffix(v.Ns, ".")
	case *dns.SOA:
		return fmt.Sprintf("%s %s %d", strings.TrimSuffix(v.Ns, "."), strings.TrimSuffix(v.Mbox, "."), v.Serial)
	case *dns.SRV:
		return fmt.Sprintf("%d %d %d %s", v.Priority, v.Weight, v.Port, strings.TrimSuffix(v.Target, "."))
	case *dns.PTR:
		return strings.TrimSuffix(v.Ptr, ".")
	case *dns.CAA:
		return fmt.Sprintf("%d %s %s", v.Flag, v.Tag, v.Value)
	default:
		// Fall back to the RDATA portion of the record's text form.
		parts := strings.SplitN(rr.String(), "\t", 5)
		return parts[len(parts)-1]
	}
}

// matchesExpected reports whether any value matches any expected entry,
// case-insensitively and ignoring trailing dots.
func matchesExpected(values, expected []string) bool {
	norm := func(s string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), ".")) }
	want := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		want[norm(e)] = struct{}{}
	}
	for _, v := range values {
		if _, ok := want[norm(v)]; ok {
			return true
		}
	}
	return false
}

// extraJSON marshals the resolved records for the heartbeat Extra column.
func extraJSON(c Config, values []string) json.RawMessage {
	payload := struct {
		RecordType string   `json:"record_type"`
		Name       string   `json:"name"`
		Records    []string `json:"records"`
	}{RecordType: c.recordType(), Name: c.Host, Records: values}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return data
}
