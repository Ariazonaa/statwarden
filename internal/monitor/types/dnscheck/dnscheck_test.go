package dnscheck

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

func mustCfg(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// stubResolver starts a loopback UDP DNS server answering from a handler and
// returns its host:port address.
func stubResolver(t *testing.T, handler dns.HandlerFunc) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: handler}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// answer builds a handler that returns the given RRs (in the answer section)
// with NOERROR, or NXDOMAIN when rcode is set.
func answerHandler(rcode int, rrs ...dns.RR) dns.HandlerFunc {
	return func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		m.Rcode = rcode
		m.Answer = rrs
		_ = w.WriteMsg(m)
	}
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("dns checker not self-registered")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("dns checker must provide a positive default timeout")
	}
}

func TestCheckRecordTypes(t *testing.T) {
	mustRR := func(s string) dns.RR {
		rr, err := dns.NewRR(s)
		if err != nil {
			t.Fatalf("build RR %q: %v", s, err)
		}
		return rr
	}
	tests := []struct {
		name       string
		recordType string
		host       string
		rrs        []dns.RR
		wantValues string // substring expected in the UP message
	}{
		{"A", "A", "example.test.", []dns.RR{mustRR("example.test. 300 IN A 93.184.216.34")}, "93.184.216.34"},
		{"AAAA", "AAAA", "example.test.", []dns.RR{mustRR("example.test. 300 IN AAAA 2606:2800:220:1:248:1893:25c8:1946")}, "2606:2800:220:1:248:1893:25c8:1946"},
		{"CNAME", "CNAME", "www.example.test.", []dns.RR{mustRR("www.example.test. 300 IN CNAME example.test.")}, "example.test"},
		{"MX", "MX", "example.test.", []dns.RR{mustRR("example.test. 300 IN MX 10 mail.example.test.")}, "10 mail.example.test"},
		{"TXT", "TXT", "example.test.", []dns.RR{mustRR(`example.test. 300 IN TXT "v=spf1 -all"`)}, "v=spf1 -all"},
		{"NS", "NS", "example.test.", []dns.RR{mustRR("example.test. 300 IN NS ns1.example.test.")}, "ns1.example.test"},
		{"SOA", "SOA", "example.test.", []dns.RR{mustRR("example.test. 300 IN SOA ns1.example.test. admin.example.test. 2024010101 7200 3600 1209600 3600")}, "2024010101"},
		{"SRV", "SRV", "_sip._tcp.example.test.", []dns.RR{mustRR("_sip._tcp.example.test. 300 IN SRV 10 20 5060 sip.example.test.")}, "10 20 5060 sip.example.test"},
		{"CAA", "CAA", "example.test.", []dns.RR{mustRR(`example.test. 300 IN CAA 0 issue "letsencrypt.org"`)}, "issue"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := stubResolver(t, answerHandler(dns.RcodeSuccess, tt.rrs...))
			var ch Checker
			res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: tt.host, RecordType: tt.recordType, Resolver: addr}))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if res.Status != domain.StatusUp {
				t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
			}
			if !strings.Contains(res.Message, tt.wantValues) {
				t.Fatalf("message %q should contain %q", res.Message, tt.wantValues)
			}
			if len(res.Extra) == 0 {
				t.Fatal("resolved records should be attached as Extra")
			}
		})
	}
}

func TestCheckPTR(t *testing.T) {
	// PTR query for 1.1.1.1 must arrive as 1.1.1.1.in-addr.arpa. The query
	// name travels back over a channel so the assertion synchronises with the
	// server goroutine.
	nameCh := make(chan string, 1)
	addr := stubResolver(t, func(w dns.ResponseWriter, req *dns.Msg) {
		name := req.Question[0].Name
		select {
		case nameCh <- name:
		default:
		}
		m := new(dns.Msg)
		m.SetReply(req)
		ptr, _ := dns.NewRR(name + " 300 IN PTR one.example.test.")
		m.Answer = []dns.RR{ptr}
		_ = w.WriteMsg(m)
	})
	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: "1.1.1.1", RecordType: "PTR", Resolver: addr}))
	if err != nil {
		t.Fatal(err)
	}
	if gotName := <-nameCh; gotName != "1.1.1.1.in-addr.arpa." {
		t.Fatalf("PTR query name = %q, want reverse form", gotName)
	}
	if res.Status != domain.StatusUp || !strings.Contains(res.Message, "one.example.test") {
		t.Fatalf("res = %+v, want up with PTR target", res)
	}
}

func TestCheckExpectedAssertion(t *testing.T) {
	rr, _ := dns.NewRR("example.test. 300 IN A 93.184.216.34")
	addr := stubResolver(t, answerHandler(dns.RcodeSuccess, rr))
	var ch Checker

	t.Run("match", func(t *testing.T) {
		res, _ := ch.Check(context.Background(), mustCfg(t, Config{Host: "example.test.", RecordType: "A", Resolver: addr, Expected: []string{"93.184.216.34"}}))
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		res, _ := ch.Check(context.Background(), mustCfg(t, Config{Host: "example.test.", RecordType: "A", Resolver: addr, Expected: []string{"10.0.0.1"}}))
		if res.Status != domain.StatusDown || !strings.Contains(res.Message, "matched") {
			t.Fatalf("res = %+v, want down for expected mismatch", res)
		}
	})
}

func TestCheckNoRecordsAndNXDomain(t *testing.T) {
	var ch Checker
	t.Run("empty answer", func(t *testing.T) {
		addr := stubResolver(t, answerHandler(dns.RcodeSuccess))
		res, _ := ch.Check(context.Background(), mustCfg(t, Config{Host: "example.test.", RecordType: "A", Resolver: addr}))
		if res.Status != domain.StatusDown || !strings.Contains(res.Message, "no A records") {
			t.Fatalf("res = %+v, want down for empty answer", res)
		}
	})
	t.Run("nxdomain", func(t *testing.T) {
		addr := stubResolver(t, answerHandler(dns.RcodeNameError))
		res, _ := ch.Check(context.Background(), mustCfg(t, Config{Host: "missing.test.", RecordType: "A", Resolver: addr}))
		if res.Status != domain.StatusDown || !strings.Contains(res.Message, "NXDOMAIN") {
			t.Fatalf("res = %+v, want down for NXDOMAIN", res)
		}
	})
}

func TestCheckResolverUnreachable(t *testing.T) {
	// Bind then close a UDP socket to get a very likely dead resolver address.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // force the exchange to fail promptly regardless of retries

	var ch Checker
	res, err := ch.Check(ctx, mustCfg(t, Config{Host: "example.test.", RecordType: "A", Resolver: addr}))
	if err != nil {
		t.Fatalf("resolver failures must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("res = %+v, want down with message", res)
	}
}

func TestMatchesExpected(t *testing.T) {
	tests := []struct {
		name     string
		values   []string
		expected []string
		want     bool
	}{
		{"exact", []string{"1.2.3.4"}, []string{"1.2.3.4"}, true},
		{"trailing dot ignored", []string{"example.test"}, []string{"example.test."}, true},
		{"case insensitive", []string{"NS1.Example.Test"}, []string{"ns1.example.test"}, true},
		{"no match", []string{"1.2.3.4"}, []string{"5.6.7.8"}, false},
		{"one of many", []string{"a", "b", "c"}, []string{"z", "b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesExpected(tt.values, tt.expected); got != tt.want {
				t.Fatalf("matchesExpected(%v,%v) = %v, want %v", tt.values, tt.expected, got, tt.want)
			}
		})
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"x","record_type":"WHAT"}`)); err == nil {
		t.Fatal("Check must reject an unknown record type")
	}
}
