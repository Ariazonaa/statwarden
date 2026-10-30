package tcpcheck

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"testing"

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

// hostPort splits a 127.0.0.1:port listener address into its parts.
func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("tcp checker not self-registered")
	}
	if _, passive := c.(registry.PassiveChecker); passive {
		t.Fatal("tcp checker must not be passive")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("tcp checker must provide a positive default timeout")
	}
}

func TestCheckOpenPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	host, port := hostPort(t, ln.Addr().String())
	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
	if res.Latency <= 0 {
		t.Fatal("latency not measured")
	}
}

func TestCheckClosedPort(t *testing.T) {
	// Bind a listener, capture its address, then close it to guarantee the
	// port is refused without racing another process for a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := hostPort(t, ln.Addr().String())
	_ = ln.Close()

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port}))
	if err != nil {
		t.Fatalf("connection failures must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("Check(refused) = %+v, want down with message", res)
	}
}

func TestCheckHonoursContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, port := hostPort(t, ln.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ch Checker
	res, err := ch.Check(ctx, mustCfg(t, Config{Host: host, Port: port}))
	if err != nil {
		t.Fatalf("cancellation must be a DOWN result, not an error; got %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down for cancelled context", res.Status)
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"h","port":0}`)); err == nil {
		t.Fatal("Check must reject an out-of-range port")
	}
}
