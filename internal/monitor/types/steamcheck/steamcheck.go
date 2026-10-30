// Package steamcheck implements the Steam game-server monitor type: it sends an
// A2S_INFO query (the Steam server-query protocol, implemented directly over UDP
// here) and reports UP when the server answers with a valid info response,
// surfacing the server name and player count. It handles the A2S challenge
// handshake. A missing response, a refused socket or a malformed reply map to a
// DOWN CheckResult rather than a Go error.
package steamcheck

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// TypeName is the registry name stored in monitors.type.
const TypeName = "steam"

// defaultTimeout is the per-type default when the monitor sets none.
const defaultTimeout = 5 * time.Second

// A2S protocol constants.
const (
	a2sInfoRequest   = 0x54 // 'T' — A2S_INFO request header
	a2sInfoResponse  = 0x49 // 'I' — A2S_INFO response header
	a2sChallenge     = 0x41 // 'A' — S2C_CHALLENGE response header
	a2sInfoPayload   = "Source Engine Query\x00"
	singlePacketFlag = 0xFFFFFFFF
	maxDatagram      = 1400
)

// serverInfo is the subset of an A2S_INFO response the check surfaces.
type serverInfo struct {
	Name       string `json:"name"`
	Map        string `json:"map"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"max_players"`
	Bots       int    `json:"bots"`
}

// Checker implements registry.Checker for the Steam game-server family.
type Checker struct {
	// query performs the live A2S_INFO exchange. nil uses the real UDP
	// implementation; tests inject a fake to exercise result mapping without a
	// server.
	query func(ctx context.Context, c Config) (serverInfo, error)
}

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

// Check implements registry.Checker. Query failures are DOWN results; only
// unusable configuration returns an error.
func (ch *Checker) Check(ctx context.Context, cfg json.RawMessage) (domain.CheckResult, error) {
	c, err := parseConfig(cfg)
	if err != nil {
		return domain.CheckResult{}, err
	}
	if err := c.validate(); err != nil {
		return domain.CheckResult{}, err
	}

	query := ch.query
	if query == nil {
		query = liveQuery
	}

	start := time.Now()
	info, err := query(ctx, c)
	latency := time.Since(start)
	if err != nil {
		return domain.CheckResult{
			Status:  domain.StatusDown,
			Latency: latency,
			Message: err.Error(),
		}, nil
	}

	extra, _ := json.Marshal(info)
	return domain.CheckResult{
		Status:  domain.StatusUp,
		Latency: latency,
		Message: fmt.Sprintf("%q online, %d/%d players on %s", info.Name, info.Players, info.MaxPlayers, info.Map),
		Extra:   extra,
	}, nil
}

// buildInfoRequest assembles an A2S_INFO request, appending the 4-byte
// challenge when the server demanded one.
func buildInfoRequest(challenge []byte) []byte {
	buf := make([]byte, 0, 5+len(a2sInfoPayload)+len(challenge))
	buf = binary.LittleEndian.AppendUint32(buf, singlePacketFlag)
	buf = append(buf, a2sInfoRequest)
	buf = append(buf, a2sInfoPayload...)
	buf = append(buf, challenge...)
	return buf
}

// liveQuery performs the real UDP A2S_INFO exchange, following one challenge
// round if the server requests it.
func liveQuery(ctx context.Context, c Config) (serverInfo, error) {
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return serverInfo{}, err
	}
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(time.Duration(c.connectTimeout()) * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return serverInfo{}, err
	}

	return exchange(conn, buildInfoRequest(nil), true)
}

// exchange writes a request, reads the reply and either parses an info
// response or (once) answers a challenge and retries.
func exchange(conn net.Conn, request []byte, allowChallenge bool) (serverInfo, error) {
	if _, err := conn.Write(request); err != nil {
		return serverInfo{}, err
	}
	buf := make([]byte, maxDatagram)
	n, err := conn.Read(buf)
	if err != nil {
		return serverInfo{}, err
	}
	typ, body, err := classify(buf[:n])
	if err != nil {
		return serverInfo{}, err
	}
	switch typ {
	case a2sInfoResponse:
		return parseInfo(body)
	case a2sChallenge:
		if !allowChallenge {
			return serverInfo{}, fmt.Errorf("server issued repeated challenge")
		}
		if len(body) < 4 {
			return serverInfo{}, fmt.Errorf("short challenge response")
		}
		return exchange(conn, buildInfoRequest(body[:4]), false)
	default:
		return serverInfo{}, fmt.Errorf("unexpected A2S response header 0x%02X", typ)
	}
}

// classify validates the single-packet header and returns the payload's type
// byte and the bytes that follow it.
func classify(pkt []byte) (byte, []byte, error) {
	if len(pkt) < 5 {
		return 0, nil, fmt.Errorf("A2S response too short (%d bytes)", len(pkt))
	}
	if binary.LittleEndian.Uint32(pkt[:4]) != singlePacketFlag {
		return 0, nil, fmt.Errorf("unsupported multi-packet A2S response")
	}
	return pkt[4], pkt[5:], nil
}

// parseInfo decodes an A2S_INFO response body (the bytes after the response
// header byte) into a serverInfo.
func parseInfo(body []byte) (serverInfo, error) {
	r := &reader{buf: body}
	r.skip(1) // protocol version
	name := r.cstring()
	mapName := r.cstring()
	r.cstring() // folder
	r.cstring() // game
	r.skip(2)   // app id (short)
	players := r.uint8()
	maxPlayers := r.uint8()
	bots := r.uint8()
	if r.err != nil {
		return serverInfo{}, fmt.Errorf("malformed A2S_INFO response: %w", r.err)
	}
	return serverInfo{
		Name:       name,
		Map:        mapName,
		Players:    int(players),
		MaxPlayers: int(maxPlayers),
		Bots:       int(bots),
	}, nil
}

// reader is a minimal little-endian byte reader with sticky-error semantics.
type reader struct {
	buf []byte
	pos int
	err error
}

func (r *reader) skip(n int) {
	if r.err != nil {
		return
	}
	if r.pos+n > len(r.buf) {
		r.err = fmt.Errorf("unexpected end of data")
		return
	}
	r.pos += n
}

func (r *reader) uint8() byte {
	if r.err != nil {
		return 0
	}
	if r.pos >= len(r.buf) {
		r.err = fmt.Errorf("unexpected end of data")
		return 0
	}
	b := r.buf[r.pos]
	r.pos++
	return b
}

func (r *reader) cstring() string {
	if r.err != nil {
		return ""
	}
	start := r.pos
	for r.pos < len(r.buf) {
		if r.buf[r.pos] == 0 {
			s := string(r.buf[start:r.pos])
			r.pos++
			return s
		}
		r.pos++
	}
	r.err = fmt.Errorf("unterminated string")
	return ""
}
