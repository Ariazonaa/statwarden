package udpcheck

import (
	"context"
	"encoding/json"
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

// udpServer starts a loopback UDP socket that answers each datagram with
// reply(request). A nil reply — or a nil return from it — leaves the request
// unanswered, which is the silent-listener case UDP monitoring turns on.
func udpServer(t *testing.T, reply func(req []byte) []byte) (string, int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return // the socket closed with the test
			}
			if reply == nil {
				continue
			}
			if out := reply(buf[:n]); out != nil {
				_, _ = pc.WriteTo(out, addr)
			}
		}
	}()
	return hostPort(t, pc.LocalAddr().String())
}

// echo answers every datagram with its own bytes.
func echo(req []byte) []byte { return append([]byte(nil), req...) }

// shortCtx bounds a probe against a silent server so the suite does not sit
// out the type's own multi-second response timeout.
func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("udp checker not self-registered")
	}
	if _, passive := c.(registry.PassiveChecker); passive {
		t.Fatal("udp checker must not be passive")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("udp checker must provide a positive default timeout")
	}
	// The check waits for a reply inside the engine's per-check deadline, so
	// a default timeout below that wait would cancel every silent probe
	// before its own timeout ever expired.
	if td.DefaultTimeout() < defaultResponseTimeout*time.Second {
		t.Fatalf("DefaultTimeout() = %s, must leave room for the %ds response wait",
			td.DefaultTimeout(), defaultResponseTimeout)
	}
}

func TestCheckReplyingServerIsUp(t *testing.T) {
	// The reply is deliberately slow. An instant loopback round trip measures
	// as zero on a coarse clock, so "latency is non-zero" would be flaky in
	// one direction and vacuous in the other; a delay the check has to sit
	// through makes the assertion mean the round trip was actually timed.
	const replyDelay = 20 * time.Millisecond
	host, port := udpServer(t, func(req []byte) []byte {
		time.Sleep(replyDelay)
		return echo(req)
	})

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: host, Port: port, Payload: "ping"}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
	if res.Latency < replyDelay/4 {
		t.Fatalf("latency = %s, want the %s round trip to be measured", res.Latency, replyDelay)
	}
	if !strings.Contains(string(res.Extra), `"responded":true`) {
		t.Fatalf("Extra = %s, want responded true", res.Extra)
	}
}

func TestCheckExpectMatch(t *testing.T) {
	host, port := udpServer(t, func([]byte) []byte { return []byte("PONG v2") })

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{
		Host: host, Port: port, Payload: "PING", Expect: "PONG",
	}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
}

// A failed assertion must name what the operator expected and nothing the peer
// wrote — the message is served to every reader, including read-only API keys,
// so echoing the reply would turn the check into a read channel.
func TestCheckExpectMismatchDoesNotEchoTheReply(t *testing.T) {
	const secret = "internal-service-banner"
	host, port := udpServer(t, func([]byte) []byte { return []byte(secret) })

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{
		Host: host, Port: port, Payload: "PING", Expect: "PONG",
	}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down for a mismatched reply", res.Status)
	}
	if strings.Contains(res.Message, secret) {
		t.Fatalf("message leaks the peer's reply: %q", res.Message)
	}
	if !strings.Contains(res.Message, "PONG") {
		t.Fatalf("message = %q, want the configured expectation named", res.Message)
	}
}

func TestCheckSilentServerIsDownWhenAReplyIsRequired(t *testing.T) {
	host, port := udpServer(t, nil)

	var ch Checker
	res, err := ch.Check(shortCtx(t), mustCfg(t, Config{Host: host, Port: port, Payload: "ping"}))
	if err != nil {
		t.Fatalf("a silent target must be a DOWN result, not an error; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("Check(silent) = %+v, want down with message", res)
	}
	if !strings.Contains(string(res.Extra), `"responded":false`) {
		t.Fatalf("Extra = %s, want responded false", res.Extra)
	}
}

// The fire-and-forget mode is the whole point of expect_response=false: a
// syslog or statsd sink answers nothing and is still healthy.
func TestCheckSilentServerIsUpWhenNoReplyIsRequired(t *testing.T) {
	host, port := udpServer(t, nil)
	no := false

	var ch Checker
	res, err := ch.Check(shortCtx(t), mustCfg(t, Config{
		Host: host, Port: port, Payload: "<14>test", ExpectResponse: &no,
	}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
	// The wait is an observation window, not a round trip: reporting it as
	// latency would plot a flat multi-second bar on every healthy check.
	if res.Latency >= 100*time.Millisecond {
		t.Fatalf("latency = %s, want the write cost rather than the wait", res.Latency)
	}
}

func TestCheckClosedPortIsDown(t *testing.T) {
	// Bind a socket, capture its address, then close it to guarantee the port
	// is unused without racing another process for a free one. A reachable
	// closed port answers with an ICMP port unreachable; where ICMP is
	// suppressed the probe simply falls through to its response timeout.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port := hostPort(t, pc.LocalAddr().String())
	_ = pc.Close()

	var ch Checker
	res, err := ch.Check(shortCtx(t), mustCfg(t, Config{Host: host, Port: port, Payload: "ping"}))
	if err != nil {
		t.Fatalf("an unreachable port must be a DOWN result, not an error; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("Check(closed) = %+v, want down with message", res)
	}
}

func TestCheckHonoursContextCancel(t *testing.T) {
	host, port := udpServer(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var ch Checker
	res, err := ch.Check(ctx, mustCfg(t, Config{Host: host, Port: port, Payload: "ping"}))
	if err != nil {
		t.Fatalf("cancellation must be a DOWN result, not an error; got %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down for a cancelled context", res.Status)
	}
}

// A cancel landing mid-wait must not be mistaken for the quiet socket that
// expect_response=false treats as healthy.
func TestCheckCancelMidWaitIsNotAHealthySilence(t *testing.T) {
	host, port := udpServer(t, nil)
	no := false

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	var ch Checker
	res, err := ch.Check(ctx, mustCfg(t, Config{
		Host: host, Port: port, Payload: "ping", ExpectResponse: &no,
	}))
	if err != nil {
		t.Fatalf("cancellation must be a DOWN result, not an error; got %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s, want down for a cancelled context", res.Status)
	}
}

func TestCheckBinaryPayloadRoundTrip(t *testing.T) {
	host, port := udpServer(t, echo)

	var ch Checker
	res, err := ch.Check(context.Background(), mustCfg(t, Config{
		Host:            host,
		Port:            port,
		Payload:         "00 01 ff",
		Expect:          "01ff",
		PayloadEncoding: encodingHex,
	}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"h","port":0}`)); err == nil {
		t.Fatal("Check must reject an out-of-range port")
	}
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"h","port":53,"payload":"zz","payload_encoding":"hex"}`)); err == nil {
		t.Fatal("Check must reject an undecodable payload")
	}
}
