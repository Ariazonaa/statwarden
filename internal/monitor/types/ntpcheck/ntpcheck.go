// Package ntpcheck implements the NTP monitor type: one SNTP exchange (RFC 4330)
// with a time server, judged on three things — that it answers, that it is
// itself synchronised, and that its clock agrees with this host's.
//
// The last one is the point of the type. A clock that has run away breaks TOTP,
// certificate validation and log correlation at once, and nothing else in the
// product would notice. The offset is symmetric: it cannot say whether the
// server or this host is wrong, only that they disagree — and for the three
// things above, disagreement is the failure either way.
//
// It is a deliberately small client on the standard library: one request, no
// clock discipline, no dependency.
package ntpcheck

import (
	"context"
	"crypto/rand"
	"encoding/binary"
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
const TypeName = "ntp"

// defaultTimeout is the per-type default when the monitor sets none. One UDP
// round trip; a time server that needs longer is not one to set a clock by.
const defaultTimeout = 10 * time.Second

// packetLen is the size of an NTP header without extensions or MAC.
const packetLen = 48

// Leap indicator 3 is the "alarm condition": the server's clock is not
// synchronised. Stratum 16 says the same thing in the other field.
const (
	leapAlarm        = 3
	stratumUnsynced  = 16
	modeClient       = 3
	modeServer       = 4
	protocolVersion4 = 4
)

// ntpEpochOffset is the number of seconds between the NTP epoch (1900-01-01)
// and the Unix epoch (1970-01-01).
const ntpEpochOffset = 2208988800

// Checker implements registry.Checker for NTP.
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

// Check implements registry.Checker. An unreachable, unsynchronised or skewed
// server is a DOWN result, not an error; only an unusable config returns one.
func (*Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.port()))
	r, err := query(ctx, addr, time.Now)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: r.rtt,
			Message: fmt.Sprintf("ntp query to %s failed: %v", addr, err),
		}, nil
	}
	status, message := assess(c, addr, r)
	return domain.CheckResult{
		Status:  status,
		Latency: r.rtt,
		Message: message,
		Extra:   extraJSON(r),
	}, nil
}

// reply is what one exchange measured.
type reply struct {
	leap    uint8
	stratum uint8
	// refID is the 4-byte reference identifier; for stratum 0 it carries the
	// kiss code ("RATE", "DENY", …).
	refID [4]byte
	// offset is how far the server's clock is AHEAD of this host's; negative
	// when it is behind.
	offset time.Duration
	// rtt is the network round trip with the server's own processing time
	// taken out (RFC 4330 "delay"); it is what the heartbeat reports as latency.
	rtt time.Duration
}

// query performs one SNTP exchange. now is injectable so the arithmetic is
// testable against a server whose clock is deliberately wrong.
func query(ctx context.Context, addr string, now func() time.Time) (reply, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return reply{}, err
		}
	}
	// Read does not watch ctx; expiring the deadline is what unblocks it when
	// the engine cancels the check mid-wait.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	// The transmit timestamp is a random nonce, not our clock. The server
	// copies it into the reply's originate field, which is what ties the reply
	// to this request — an off-path spoofer has to guess 64 bits — and a nonce
	// tells the server nothing about this host's time. T1 is taken locally.
	var req [packetLen]byte
	req[0] = protocolVersion4<<3 | modeClient
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return reply{}, err
	}
	copy(req[40:48], nonce[:])

	t1 := now()
	start := time.Now() // monotonic, for the round trip alone
	if _, err := conn.Write(req[:]); err != nil {
		return reply{}, err
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	elapsed := time.Since(start)
	t4 := now()
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return reply{}, ctx.Err()
		}
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return reply{rtt: elapsed}, fmt.Errorf("no reply within %s", elapsed.Round(time.Millisecond))
		}
		return reply{rtt: elapsed}, err
	}
	return parseReply(buf[:n], nonce, t1, t4, elapsed)
}

// parseReply validates a server packet and computes offset and delay.
func parseReply(p []byte, nonce [8]byte, t1, t4 time.Time, elapsed time.Duration) (reply, error) {
	if len(p) < packetLen {
		return reply{rtt: elapsed}, fmt.Errorf("short reply (%d bytes)", len(p))
	}
	if mode := p[0] & 0x7; mode != modeServer {
		return reply{rtt: elapsed}, fmt.Errorf("reply has mode %d, want %d (server)", mode, modeServer)
	}
	var origin [8]byte
	copy(origin[:], p[24:32])
	if origin != nonce {
		// Either not the answer to this request or a forged one. Neither says
		// anything about the server's clock.
		return reply{rtt: elapsed}, errors.New("reply does not answer this request (originate timestamp mismatch)")
	}
	r := reply{
		leap:    p[0] >> 6,
		stratum: p[1],
	}
	copy(r.refID[:], p[12:16])
	t2 := ntpTime(p[32:40]) // server receive
	t3 := ntpTime(p[40:48]) // server transmit

	// RFC 4330 §5: offset = ((T2 − T1) + (T3 − T4)) / 2 and
	// delay = (T4 − T1) − (T3 − T2). The round trip uses the monotonic
	// measurement rather than T4 − T1, so a local clock step during the
	// exchange cannot produce a negative or absurd delay.
	r.offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
	r.rtt = max(elapsed-t3.Sub(t2), 0)
	return r, nil
}

// ntpTime decodes a 64-bit NTP timestamp (32.32 fixed point, seconds since
// 1900). The 32-bit seconds field wraps on 2036-02-07; per RFC 4330 §3 a value
// with the top bit CLEAR is read as the next era, which keeps this correct
// until 2104.
func ntpTime(b []byte) time.Time {
	secs := int64(binary.BigEndian.Uint32(b[0:4]))
	frac := int64(binary.BigEndian.Uint32(b[4:8]))
	if secs&0x80000000 == 0 {
		secs += 1 << 32
	}
	nanos := (frac * int64(time.Second)) >> 32
	return time.Unix(secs-ntpEpochOffset, nanos)
}

// assess maps a successful exchange to a status and a message.
func assess(c Config, addr string, r reply) (domain.MonitorStatus, string) {
	switch {
	case r.stratum == 0:
		// Kiss-o'-death: the server refuses to serve time to us.
		return domain.StatusDown, fmt.Sprintf("ntp server %s sent kiss-o'-death %s", addr, kissCode(r.refID))
	case r.leap == leapAlarm || r.stratum >= stratumUnsynced:
		return domain.StatusDown, fmt.Sprintf("ntp server %s is not synchronised (stratum %d)", addr, r.stratum)
	case c.MaxStratum > 0 && int(r.stratum) > c.MaxStratum:
		return domain.StatusDown, fmt.Sprintf("ntp server %s is at stratum %d, above the allowed %d",
			addr, r.stratum, c.MaxStratum)
	}
	off := r.offset.Round(time.Millisecond)
	if abs(r.offset) > c.maxOffset() {
		return domain.StatusDown, fmt.Sprintf("clock offset %s against %s exceeds the allowed ±%s "+
			"(stratum %d) — this host's clock or the server's has drifted",
			signed(off), addr, c.maxOffset(), r.stratum)
	}
	return domain.StatusUp, fmt.Sprintf("clock offset %s against %s, stratum %d",
		signed(off), addr, r.stratum)
}

// kissCode renders a kiss-o'-death reference id. The codes are four printable
// ASCII letters by definition (RFC 5905 §7.4); anything else is shown as hex
// rather than written into a message verbatim.
func kissCode(id [4]byte) string {
	for _, b := range id {
		if (b < 'A' || b > 'Z') && b != 0 {
			return fmt.Sprintf("0x%x", id[:])
		}
	}
	return fmt.Sprintf("%q", string(trimNUL(id[:])))
}

func trimNUL(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// signed prints a duration with an explicit sign, so "+12ms" (server ahead)
// and "-12ms" (server behind) read unambiguously.
func signed(d time.Duration) string {
	if d >= 0 {
		return "+" + d.String()
	}
	return d.String()
}

// stats is the per-check detail stored on the heartbeat.
type stats struct {
	OffsetMs float64 `json:"offset_ms"`
	DelayMs  float64 `json:"delay_ms"`
	Stratum  int     `json:"stratum"`
	Leap     int     `json:"leap"`
}

func extraJSON(r reply) json.RawMessage {
	data, err := json.Marshal(stats{
		OffsetMs: float64(r.offset) / float64(time.Millisecond),
		DelayMs:  float64(r.rtt) / float64(time.Millisecond),
		Stratum:  int(r.stratum),
		Leap:     int(r.leap),
	})
	if err != nil {
		return nil
	}
	return data
}
