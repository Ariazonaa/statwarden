// Package udpcheck implements the UDP monitor type: it sends one datagram to a
// host:port and judges what the socket says back. UDP has no handshake, so
// unlike tcpcheck there is nothing to succeed at by merely connecting — the
// check is only ever as strong as what it asks for. Requiring a reply (the
// default) is the honest mode: a service that answers is provably alive. The
// fire-and-forget mode is opt-in and weaker, because a firewall that drops ICMP
// looks exactly like a healthy listener.
package udpcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "udp"

// defaultTimeout is the per-type default when the monitor sets none. It leaves
// room for the type's own response wait, which defaults to 5s.
const defaultTimeout = 15 * time.Second

// maxDatagram sizes the read buffer. A UDP datagram tops out just under 64 KiB
// and the kernel DISCARDS whatever does not fit, so a smaller buffer would
// silently truncate a reply — and could turn a matching expect into a mismatch.
const maxDatagram = 65535

// outcome is what one datagram exchange produced.
type outcome int

const (
	// outcomeReply: the peer answered.
	outcomeReply outcome = iota
	// outcomeSilent: the wait elapsed with neither a reply nor an error.
	outcomeSilent
	// outcomeRejected: the socket reported an error. On a connected UDP
	// socket that is the kernel surfacing an ICMP error — port unreachable
	// above all — raised by the datagram just written.
	outcomeRejected
)

// probeResult carries an exchange over to assess, which does no I/O and is
// therefore testable without a socket.
type probeResult struct {
	outcome outcome
	reply   []byte
	latency time.Duration
	// window is how long the probe actually listened. It is the configured
	// response timeout unless the monitor's own check timeout cut it short,
	// and reporting it rather than the configured value keeps the message
	// from claiming a wait that never happened.
	window time.Duration
	err    error
}

// Checker implements registry.Checker for UDP datagram checks.
type Checker struct{}

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

// Check implements registry.Checker: it sends one datagram and maps the
// socket's answer against the configured assertions. An unreachable or silent
// target is a DOWN result, not an error; only an unusable config returns one.
func (*Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}
	// validate already decoded both, so neither error can fire here; handling
	// them keeps the decoding in one place instead of a must-style helper.
	payload, err := c.payloadBytes()
	if err != nil {
		return domain.CheckResult{}, err
	}
	expect, err := c.expectBytes()
	if err != nil {
		return domain.CheckResult{}, err
	}

	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	res := probe(ctx, addr, payload, c.responseTimeout())
	status, message := assess(c, addr, expect, res)

	return domain.CheckResult{
		Status:  status,
		Latency: res.latency,
		Message: message,
		Extra:   extraJSON(res),
	}, nil
}

// probe sends one datagram over a connected UDP socket and waits up to wait —
// or the context deadline, whichever comes first — for the socket to say
// something back.
func probe(ctx context.Context, addr string, payload []byte, wait time.Duration) probeResult {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return probeResult{outcome: outcomeRejected, err: err}
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(wait)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return probeResult{outcome: outcomeRejected, err: err}
	}
	// Read does not watch ctx. Expiring the deadline is what unblocks it when
	// the engine cancels the check mid-wait.
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stopWatch()

	start := time.Now()
	window := deadline.Sub(start).Round(time.Millisecond)
	if _, err := conn.Write(payload); err != nil {
		return probeResult{outcome: outcomeRejected, latency: time.Since(start), window: window, err: err}
	}
	written := time.Since(start)

	buf := make([]byte, maxDatagram)
	n, err := conn.Read(buf)
	switch {
	case err == nil:
		return probeResult{outcome: outcomeReply, reply: buf[:n], latency: time.Since(start), window: window}
	case errors.Is(ctx.Err(), context.Canceled):
		// Cancelled is not silence: the check was abandoned — the engine
		// shutting down, the monitor deleted mid-probe — and observed
		// nothing. Only an explicit cancel counts here. A ctx DEADLINE that
		// expires is the operator's own check timeout ending the listening
		// window, which is a real observation and falls through below.
		return probeResult{outcome: outcomeRejected, latency: time.Since(start), window: window, err: ctx.Err()}
	case isTimeout(err):
		// Nothing came back, which for a fire-and-forget probe is the
		// expected result — so the latency is the write, not the wait, and
		// an UP heartbeat does not plot a flat 5000 ms.
		return probeResult{outcome: outcomeSilent, latency: written, window: window, err: err}
	default:
		return probeResult{outcome: outcomeRejected, latency: time.Since(start), window: window, err: err}
	}
}

// isTimeout reports whether err is the read deadline expiring rather than the
// socket reporting a problem.
func isTimeout(err error) bool {
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}

// assess maps an exchange to a status and a message.
//
// Per the Checker contract the message never repeats what the peer sent: the
// reply is described by its size, and a failed assertion names the operator's
// own expected string, which is what debugging actually needs.
func assess(c Config, addr string, expect []byte, res probeResult) (domain.MonitorStatus, string) {
	switch res.outcome {
	case outcomeRejected:
		return domain.StatusDown, fmt.Sprintf("udp probe to %s failed: %v", addr, res.err)
	case outcomeSilent:
		if c.expectResponse() {
			return domain.StatusDown, fmt.Sprintf("no udp reply from %s within %s", addr, res.window)
		}
		return domain.StatusUp, fmt.Sprintf("datagram sent to %s, no error within %s (open or filtered)",
			addr, res.window)
	}

	// outcomeReply: something answered, so the assertion decides.
	switch {
	case len(expect) == 0:
		return domain.StatusUp, fmt.Sprintf("udp reply from %s (%d bytes)", addr, len(res.reply))
	case !bytes.Contains(res.reply, expect):
		return domain.StatusDown, fmt.Sprintf("udp reply from %s (%d bytes) does not contain the expected %q",
			addr, len(res.reply), c.Expect)
	default:
		return domain.StatusUp, fmt.Sprintf("udp reply from %s (%d bytes) contains the expected %q",
			addr, len(res.reply), c.Expect)
	}
}

// stats is the per-check detail stored on the heartbeat.
type stats struct {
	Responded     bool `json:"responded"`
	ResponseBytes int  `json:"response_bytes"`
}

// extraJSON marshals the exchange for the heartbeat Extra column.
func extraJSON(res probeResult) json.RawMessage {
	data, err := json.Marshal(stats{
		Responded:     res.outcome == outcomeReply,
		ResponseBytes: len(res.reply),
	})
	if err != nil {
		return nil
	}
	return data
}
