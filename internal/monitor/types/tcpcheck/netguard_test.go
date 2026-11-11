package tcpcheck

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/netguard"
)

func TestGuardRefusesALoopbackDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	netguard.SetEnabled(true)
	t.Cleanup(func() { netguard.SetEnabled(false) })

	host, port := hostPort(t, ln.Addr().String())
	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port}))
	msg := res.Message
	if err != nil {
		msg = err.Error()
	}
	if err == nil && res.Status != domain.StatusDown {
		t.Fatalf("a loopback port was reached with the guard on: %s", res.Status)
	}
	if !strings.Contains(msg, "BLOCK_PRIVATE_TARGETS") {
		t.Fatalf("refusal does not name the guard: %q", msg)
	}
}
