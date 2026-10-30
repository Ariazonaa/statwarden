package ntpcheck

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// putNTP encodes t as a 64-bit NTP timestamp.
func putNTP(b []byte, t time.Time) {
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := (uint64(t.Nanosecond()) << 32) / uint64(time.Second)
	binary.BigEndian.PutUint32(b[0:4], uint32(secs))
	binary.BigEndian.PutUint32(b[4:8], uint32(frac))
}

// server describes how the fake answers.
type server struct {
	skew      time.Duration // added to the real clock
	stratum   byte
	leap      byte
	refID     string
	badOrigin bool
	mode      byte // 0 = server (4)
	silent    bool
}

// fakeNTP starts a UDP time server on loopback and returns its port.
func fakeNTP(t *testing.T, s server) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if s.silent || n < packetLen {
				continue
			}
			recv := time.Now().Add(s.skew)
			var out [packetLen]byte
			mode := s.mode
			if mode == 0 {
				mode = modeServer
			}
			out[0] = s.leap<<6 | protocolVersion4<<3 | mode
			out[1] = s.stratum
			copy(out[12:16], s.refID)
			copy(out[24:32], buf[40:48]) // originate = the client's transmit
			if s.badOrigin {
				out[24] ^= 0xff
			}
			putNTP(out[32:40], recv)
			putNTP(out[40:48], time.Now().Add(s.skew))
			_, _ = pc.WriteTo(out[:], from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

func check(t *testing.T, port int, extra string) domain.CheckResult {
	t.Helper()
	cfg := json.RawMessage(fmt.Sprintf(`{"host":"127.0.0.1","port":%d%s}`, port, extra))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := (&Checker{}).Check(ctx, cfg)
	if err != nil {
		t.Fatalf("Check returned an error for a runtime failure: %v", err)
	}
	return res
}

func TestASynchronisedServerIsUp(t *testing.T) {
	port := fakeNTP(t, server{stratum: 2, refID: "\x0a\x00\x00\x01"})
	res := check(t, port, "")
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "stratum 2") {
		t.Fatalf("message = %q, want the stratum named", res.Message)
	}
	var st stats
	if err := json.Unmarshal(res.Extra, &st); err != nil || st.Stratum != 2 {
		t.Fatalf("extra = %s (%v), want stratum 2", res.Extra, err)
	}
	if st.OffsetMs > 200 || st.OffsetMs < -200 {
		t.Fatalf("offset against a loopback server with the same clock = %vms", st.OffsetMs)
	}
}

// The reason the type exists: a clock that has run away is DOWN, and the offset
// is measured, not guessed — five seconds ahead reads as roughly +5s.
func TestASkewedClockIsDown(t *testing.T) {
	port := fakeNTP(t, server{stratum: 1, refID: "GPS", skew: 5 * time.Second})
	res := check(t, port, "")
	if res.Status != domain.StatusDown || !strings.Contains(res.Message, "exceeds the allowed ±1s") {
		t.Fatalf("status = %s (%s), want down for a 5s offset against the 1s default", res.Status, res.Message)
	}
	var st stats
	_ = json.Unmarshal(res.Extra, &st)
	if st.OffsetMs < 4800 || st.OffsetMs > 5200 {
		t.Fatalf("measured offset = %vms, want about +5000", st.OffsetMs)
	}

	// Behind is as wrong as ahead.
	port = fakeNTP(t, server{stratum: 1, skew: -3 * time.Second})
	if res := check(t, port, ""); res.Status != domain.StatusDown || !strings.Contains(res.Message, "-3") {
		t.Fatalf("server 3s behind = %s (%s), want down with a negative offset", res.Status, res.Message)
	}

	// And a wider allowance accepts it.
	port = fakeNTP(t, server{stratum: 1, skew: 5 * time.Second})
	if res := check(t, port, `,"max_offset_ms":10000`); res.Status != domain.StatusUp {
		t.Fatalf("5s offset with a 10s allowance = %s (%s), want up", res.Status, res.Message)
	}
}

func TestAnUnsynchronisedServerIsDown(t *testing.T) {
	for name, s := range map[string]server{
		"leap alarm": {stratum: 3, leap: leapAlarm},
		"stratum 16": {stratum: stratumUnsynced},
	} {
		port := fakeNTP(t, s)
		if res := check(t, port, ""); res.Status != domain.StatusDown || !strings.Contains(res.Message, "not synchronised") {
			t.Errorf("%s: status = %s (%s), want down, not synchronised", name, res.Status, res.Message)
		}
	}
}

func TestKissOfDeathIsDownAndNamed(t *testing.T) {
	port := fakeNTP(t, server{stratum: 0, refID: "RATE"})
	res := check(t, port, "")
	if res.Status != domain.StatusDown || !strings.Contains(res.Message, `"RATE"`) {
		t.Fatalf("status = %s (%s), want down naming the RATE kiss code", res.Status, res.Message)
	}
}

func TestMaxStratum(t *testing.T) {
	port := fakeNTP(t, server{stratum: 5})
	if res := check(t, port, `,"max_stratum":3`); res.Status != domain.StatusDown {
		t.Fatalf("stratum 5 against max 3 = %s (%s), want down", res.Status, res.Message)
	}
	if res := check(t, port, `,"max_stratum":5`); res.Status != domain.StatusUp {
		t.Fatalf("stratum 5 against max 5 = %s (%s), want up", res.Status, res.Message)
	}
}

// A reply that does not echo our nonce is not an answer to this request, and
// must not be believed — whatever time it claims.
func TestAReplyToSomeoneElseIsRejected(t *testing.T) {
	port := fakeNTP(t, server{stratum: 2, badOrigin: true})
	res := check(t, port, "")
	if res.Status != domain.StatusDown || !strings.Contains(res.Message, "originate") {
		t.Fatalf("status = %s (%s), want down for an originate mismatch", res.Status, res.Message)
	}
}

func TestAClientModeReplyIsRejected(t *testing.T) {
	port := fakeNTP(t, server{stratum: 2, mode: modeClient})
	if res := check(t, port, ""); res.Status != domain.StatusDown || !strings.Contains(res.Message, "mode") {
		t.Fatalf("status = %s (%s), want down for a non-server reply", res.Status, res.Message)
	}
}

func TestASilentServerIsDown(t *testing.T) {
	port := fakeNTP(t, server{silent: true})
	cfg := json.RawMessage(fmt.Sprintf(`{"host":"127.0.0.1","port":%d}`, port))
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := (&Checker{}).Check(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.StatusDown || !strings.Contains(res.Message, "no reply") {
		t.Fatalf("status = %s (%s), want down, no reply", res.Status, res.Message)
	}
}

// The seconds field wraps on 2036-02-07; a value with the top bit clear is the
// next era. Without that, every check after the wrap would compute an offset of
// about -136 years.
func TestNTPTimeCrossesThe2036Era(t *testing.T) {
	for _, want := range []time.Time{
		time.Date(2026, 9, 29, 12, 0, 0, 500_000_000, time.UTC),
		time.Date(2036, 2, 7, 6, 28, 15, 0, time.UTC), // last second of era 0
		time.Date(2036, 2, 7, 6, 28, 16, 0, time.UTC), // first of era 1
		time.Date(2050, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		var b [8]byte
		putNTP(b[:], want)
		if got := ntpTime(b[:]); got.Sub(want).Abs() > time.Microsecond {
			t.Errorf("ntpTime(%v) = %v", want, got.UTC())
		}
	}
}

// The RFC 4330 arithmetic, without a socket: a server 2s ahead, 40ms of wire in
// each direction and 5ms of processing reads as +2s offset and 80ms delay.
func TestOffsetAndDelayArithmetic(t *testing.T) {
	t1 := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(2*time.Second + 40*time.Millisecond)
	t3 := t2.Add(5 * time.Millisecond)
	t4 := t1.Add(85 * time.Millisecond)
	var p [packetLen]byte
	p[0] = protocolVersion4<<3 | modeServer
	p[1] = 2
	nonce := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	copy(p[24:32], nonce[:])
	putNTP(p[32:40], t2)
	putNTP(p[40:48], t3)
	r, err := parseReply(p[:], nonce, t1, t4, 85*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if (r.offset - 2*time.Second).Abs() > time.Millisecond {
		t.Errorf("offset = %v, want 2s", r.offset)
	}
	if (r.rtt - 80*time.Millisecond).Abs() > time.Millisecond {
		t.Errorf("delay = %v, want 80ms", r.rtt)
	}
}

func TestValidate(t *testing.T) {
	c := &Checker{}
	for cfg, ok := range map[string]bool{
		`{"host":"pool.ntp.org"}`: true,
		`{"host":"10.0.0.1","port":123,"max_offset_ms":50,"max_stratum":3}`: true,
		`{}`:                              false,
		`{"host":""}`:                     false,
		`{"host":"x","port":70000}`:       false,
		`{"host":"x","max_offset_ms":-1}`: false,
		`{"host":"x","max_stratum":16}`:   false,
		`{"host":"x","typo":1}`:           false,
	} {
		err := c.Validate(json.RawMessage(cfg))
		if ok && err != nil {
			t.Errorf("Validate(%s) = %v, want accepted", cfg, err)
		}
		if !ok && err == nil {
			t.Errorf("Validate(%s) accepted it", cfg)
		}
	}
}

func TestKissCodeNeverWritesRawBytes(t *testing.T) {
	if got := kissCode([4]byte{'D', 'E', 'N', 'Y'}); got != `"DENY"` {
		t.Errorf("kissCode(DENY) = %s", got)
	}
	if got := kissCode([4]byte{0x1b, '[', '2', 'J'}); !strings.HasPrefix(got, "0x") {
		t.Errorf("kissCode with control bytes = %q, want hex", got)
	}
}
