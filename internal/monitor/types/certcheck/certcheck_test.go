package certcheck

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

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

// newTLSServer starts a loopback TLS listener presenting a self-signed
// certificate for "localhost" valid until notAfter. It returns the host, port
// and a cert pool trusting the certificate.
func newTLSServer(t *testing.T, notAfter time.Time) (host string, port int, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(leaf)

	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Drive the handshake, then close.
			if tc, ok := conn.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			_ = conn.Close()
		}
	}()

	h, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return h, p, pool
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("cert checker not self-registered")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("cert checker must provide a positive default timeout")
	}
}

func TestCheckValidCertificate(t *testing.T) {
	host, port, pool := newTLSServer(t, time.Now().Add(60*24*time.Hour))
	ch := Checker{rootCAs: pool}

	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port, ServerName: "localhost"}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
	if res.Cert == nil || res.Cert.FingerprintSHA256 == "" {
		t.Fatalf("certificate not captured: %+v", res.Cert)
	}
	if !strings.Contains(res.Message, "remaining") {
		t.Fatalf("message %q should mention remaining days", res.Message)
	}
	if res.Latency <= 0 {
		t.Fatal("latency not measured")
	}
}

func TestCheckWarnThreshold(t *testing.T) {
	host, port, pool := newTLSServer(t, time.Now().Add(5*24*time.Hour))
	ch := Checker{rootCAs: pool}

	t.Run("within warn window is down", func(t *testing.T) {
		res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port, ServerName: "localhost", WarnDays: 7}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != domain.StatusDown {
			t.Fatalf("status = %s (%s), want down within warn window", res.Status, res.Message)
		}
		if !strings.Contains(res.Message, "threshold") {
			t.Fatalf("message %q should mention the threshold", res.Message)
		}
	})

	t.Run("outside warn window is up", func(t *testing.T) {
		res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port, ServerName: "localhost", WarnDays: 2}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != domain.StatusUp {
			t.Fatalf("status = %s (%s), want up outside warn window", res.Status, res.Message)
		}
	})
}

func TestCheckExpiredCertificate(t *testing.T) {
	// An expired certificate fails verification, so ignore_tls_errors lets us
	// still complete the handshake and inspect expiry ourselves.
	host, port, _ := newTLSServer(t, time.Now().Add(-3*24*time.Hour))
	var ch Checker

	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port, ServerName: "localhost", IgnoreTLSErrors: true}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s (%s), want down for expired cert", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "expired") {
		t.Fatalf("message %q should mention expiry", res.Message)
	}
}

func TestCheckUntrustedCertificate(t *testing.T) {
	host, port, _ := newTLSServer(t, time.Now().Add(60*24*time.Hour))
	var ch Checker // no rootCAs: the self-signed cert is untrusted

	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port, ServerName: "localhost"}))
	if err != nil {
		t.Fatalf("handshake failures must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down for untrusted certificate", res.Status)
	}
}

func TestCheckDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, portStr, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: h, Port: p}))
	if err != nil {
		t.Fatalf("dial failures must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("Check(refused) = %+v, want down with message", res)
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"","port":443}`)); err == nil {
		t.Fatal("Check must reject a missing host")
	}
}
