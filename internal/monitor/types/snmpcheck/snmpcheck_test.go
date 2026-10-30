package snmpcheck

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/gosnmp/gosnmp"

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

func f64(v float64) *float64 { return &v }

// fakeSession returns canned PDUs (or a connect/get error) without any network.
type fakeSession struct {
	pdus   []gosnmp.SnmpPDU
	getErr error
}

func (s *fakeSession) Get([]string) (*gosnmp.SnmpPacket, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return &gosnmp.SnmpPacket{Variables: s.pdus}, nil
}
func (s *fakeSession) Close() error { return nil }

// withSession builds a Checker whose factory yields the given session.
func withSession(sess session, factoryErr error) *Checker {
	return &Checker{newSession: func(context.Context, Config) (session, error) {
		if factoryErr != nil {
			return nil, factoryErr
		}
		return sess, nil
	}}
}

func pdu(value any, typ gosnmp.Asn1BER) gosnmp.SnmpPDU {
	return gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.1.3.0", Type: typ, Value: value}
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("snmp checker not self-registered")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("snmp checker must provide a positive default timeout")
	}
}

func TestCheckExpectedValue(t *testing.T) {
	base := Config{Host: "dev", Community: "public", OID: "1.3.6.1.2.1.1.5.0"}

	t.Run("string match", func(t *testing.T) {
		base.ExpectedValue = "switch01"
		ch := withSession(&fakeSession{pdus: []gosnmp.SnmpPDU{pdu([]byte("switch01"), gosnmp.OctetString)}}, nil)
		res, _ := ch.Check(context.Background(), mustCfg(t, base))
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
		}
	})
	t.Run("string mismatch", func(t *testing.T) {
		base.ExpectedValue = "switch01"
		ch := withSession(&fakeSession{pdus: []gosnmp.SnmpPDU{pdu([]byte("router07"), gosnmp.OctetString)}}, nil)
		res, _ := ch.Check(context.Background(), mustCfg(t, base))
		if res.Status != domain.StatusDown {
			t.Fatalf("status = %s (%s), want down", res.Status, res.Message)
		}
	})
}

func TestCheckNumericThreshold(t *testing.T) {
	tests := []struct {
		name     string
		operator string
		thresh   float64
		value    any
		typ      gosnmp.Asn1BER
		want     domain.MonitorStatus
	}{
		{"gt pass", ">", 80, 42, gosnmp.Integer, domain.StatusDown},
		{"lt pass", "<", 80, uint(42), gosnmp.Gauge32, domain.StatusUp},
		{"gte boundary", ">=", 42, 42, gosnmp.Integer, domain.StatusUp},
		{"counter64 big int", "<", 1e9, big.NewInt(5), gosnmp.Counter64, domain.StatusUp},
		{"eq pass", "==", 1, 1, gosnmp.Integer, domain.StatusUp},
		{"ne pass", "!=", 0, 5, gosnmp.Integer, domain.StatusUp},
		{"numeric octet string", ">", 10, []byte("15"), gosnmp.OctetString, domain.StatusUp},
		{"non numeric octet string", ">", 10, []byte("up"), gosnmp.OctetString, domain.StatusDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Host: "dev", Community: "public", OID: "1.3.6.1.2.1.2.2.1.10.1", Operator: tt.operator, Threshold: f64(tt.thresh)}
			ch := withSession(&fakeSession{pdus: []gosnmp.SnmpPDU{pdu(tt.value, tt.typ)}}, nil)
			res, err := ch.Check(context.Background(), mustCfg(t, cfg))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if res.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Message, tt.want)
			}
			if len(res.Extra) == 0 {
				t.Fatal("OID value should be attached as Extra")
			}
		})
	}
}

func TestCheckPresenceOnly(t *testing.T) {
	cfg := Config{Host: "dev", Community: "public", OID: "1.3.6.1.2.1.1.3.0"}
	ch := withSession(&fakeSession{pdus: []gosnmp.SnmpPDU{pdu(uint32(12345), gosnmp.TimeTicks)}}, nil)
	res, _ := ch.Check(context.Background(), mustCfg(t, cfg))
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up when OID merely present", res.Status, res.Message)
	}
}

func TestCheckMissingOID(t *testing.T) {
	cfg := Config{Host: "dev", Community: "public", OID: "1.3.6.1.2.1.99.0"}
	ch := withSession(&fakeSession{pdus: []gosnmp.SnmpPDU{pdu(nil, gosnmp.NoSuchObject)}}, nil)
	res, _ := ch.Check(context.Background(), mustCfg(t, cfg))
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s (%s), want down for NoSuchObject", res.Status, res.Message)
	}
}

func TestCheckConnectAndGetErrors(t *testing.T) {
	cfg := Config{Host: "dev", Community: "public", OID: "1.3.6.1.2.1.1.3.0"}

	t.Run("connect failure", func(t *testing.T) {
		ch := withSession(nil, errors.New("snmp: connect dev: timeout"))
		res, err := ch.Check(context.Background(), mustCfg(t, cfg))
		if err != nil {
			t.Fatalf("connect failures must be DOWN results; got %v", err)
		}
		if res.Status != domain.StatusDown || res.Message == "" {
			t.Fatalf("res = %+v, want down with message", res)
		}
	})
	t.Run("get failure", func(t *testing.T) {
		ch := withSession(&fakeSession{getErr: errors.New("request timeout")}, nil)
		res, err := ch.Check(context.Background(), mustCfg(t, cfg))
		if err != nil {
			t.Fatalf("get failures must be DOWN results; got %v", err)
		}
		if res.Status != domain.StatusDown {
			t.Fatalf("res = %+v, want down", res)
		}
	})
}

func TestNormalizeOID(t *testing.T) {
	if got := normalizeOID("1.3.6.1"); got != ".1.3.6.1" {
		t.Fatalf("normalizeOID = %q, want leading dot", got)
	}
	if got := normalizeOID(".1.3.6.1"); got != ".1.3.6.1" {
		t.Fatalf("normalizeOID = %q, want unchanged", got)
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"h","oid":"1.2","version":"2c"}`)); err == nil {
		t.Fatal("Check must reject v2c config without community")
	}
}
