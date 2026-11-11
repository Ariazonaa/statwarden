package netguard

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func withEnabled(t *testing.T, on bool) {
	t.Helper()
	prev := Enabled()
	SetEnabled(on)
	t.Cleanup(func() { SetEnabled(prev) })
}

func TestBlockedRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "::1",
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fd00::1", "fc00::1",
		"169.254.169.254", "fe80::1",
		"100.64.0.1", "100.127.255.255",
		"0.0.0.0", "0.1.2.3", "::",
		"224.0.0.1", "ff02::1", "255.255.255.255",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1",
		"64:ff9b::a9fe:a9fe", // NAT64 form of 169.254.169.254
	}
	for _, s := range blocked {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("Blocked(%s) = false, want true", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111", "172.32.0.1", "100.128.0.1", "64:ff9b::808:808"}
	for _, s := range allowed {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("Blocked(%s) = true, want false", s)
		}
	}
}

// Disabled is the default and must change nothing: monitoring internal
// services is what a self-hosted monitor is mostly for.
func TestDisabledAllowsEverything(t *testing.T) {
	withEnabled(t, false)
	if err := Control("tcp", "127.0.0.1:80", nil); err != nil {
		t.Fatalf("Control while disabled = %v", err)
	}
	if err := CheckConfig(context.Background(), json.RawMessage(`{"host":"127.0.0.1"}`)); err != nil {
		t.Fatalf("CheckConfig while disabled = %v", err)
	}
}

// Control runs on the address actually being dialled, after DNS — the only
// point where a rebinding name or an HTTP redirect cannot slip past.
func TestControlRefusesBlockedDialTargets(t *testing.T) {
	withEnabled(t, true)
	for _, addr := range []string{"127.0.0.1:80", "[::1]:443", "169.254.169.254:80", "[::ffff:10.0.0.1]:22"} {
		if err := Control("tcp", addr, nil); !errors.Is(err, ErrBlocked) {
			t.Errorf("Control(%s) = %v, want ErrBlocked", addr, err)
		}
	}
	if err := Control("unix", "/var/run/docker.sock", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("Control(unix) = %v, want ErrBlocked", err)
	}
	if err := Control("tcp", "1.1.1.1:443", nil); err != nil {
		t.Errorf("Control(public) = %v, want nil", err)
	}
}

// A dialer with Control refuses a loopback listener for real, not just in a
// unit of the predicate.
func TestDialerRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	withEnabled(t, true)
	d := Dialer(&net.Dialer{})
	conn, err := d.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial to a loopback listener succeeded with the guard on")
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("dial error = %v, want ErrBlocked", err)
	}
}

func TestCheckConfigFindsTargets(t *testing.T) {
	withEnabled(t, true)
	blocked := []string{
		`{"url":"http://127.0.0.1:8080/health"}`,
		`{"url":"https://[::1]/"}`,
		`{"host":"10.0.0.5","port":6379}`,
		`{"host":"192.168.1.10:5432"}`,
		`{"broker":"tcp://169.254.169.254:1883"}`,
		`{"url":"https://example.com","proxy_url":"http://127.0.0.1:3128"}`,
		`{"resolver":"10.0.0.53:53","hostname":"example.com"}`,
		`{"target":"unix:/run/app.sock"}`,
		`{"socket":"/var/run/docker.sock"}`,
		`{"url":"unix:///var/run/docker.sock"}`,
		`{"host":"localhost"}`,
	}
	for _, cfg := range blocked {
		if err := CheckConfig(context.Background(), json.RawMessage(cfg)); !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckConfig(%s) = %v, want ErrBlocked", cfg, err)
		}
	}
	allowed := []string{
		`{"url":"https://1.1.1.1/"}`,
		`{"host":"8.8.8.8","port":53}`,
		`{"keyword":"127.0.0.1"}`,
		`{}`,
	}
	for _, cfg := range allowed {
		if err := CheckConfig(context.Background(), json.RawMessage(cfg)); err != nil {
			t.Errorf("CheckConfig(%s) = %v, want nil", cfg, err)
		}
	}
}
