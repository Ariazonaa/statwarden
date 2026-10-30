package steamcheck

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// fakeQuery builds a query function returning the given info and error.
func fakeQuery(info serverInfo, err error) func(context.Context, Config) (serverInfo, error) {
	return func(context.Context, Config) (serverInfo, error) { return info, err }
}

func TestCheckResultMapping(t *testing.T) {
	tests := []struct {
		name       string
		info       serverInfo
		queryErr   error
		wantStatus domain.MonitorStatus
	}{
		{
			"answer is up",
			serverInfo{Name: "My Server", Map: "de_dust2", Players: 5, MaxPlayers: 16},
			nil,
			domain.StatusUp,
		},
		{
			"connection refused is down",
			serverInfo{},
			errors.New("connection refused"),
			domain.StatusDown,
		},
		{
			"timeout is down",
			serverInfo{},
			context.DeadlineExceeded,
			domain.StatusDown,
		},
		{
			"malformed reply is down",
			serverInfo{},
			errors.New("malformed A2S_INFO response"),
			domain.StatusDown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Checker{query: fakeQuery(tt.info, tt.queryErr)}
			raw, err := json.Marshal(Config{Host: "gs.local", Port: 27015})
			if err != nil {
				t.Fatal(err)
			}
			got, err := ch.Check(context.Background(), raw)
			if err != nil {
				t.Fatalf("Check() unexpected error: %v", err)
			}
			if got.Status != tt.wantStatus {
				t.Fatalf("Check() status = %q (%q), want %q", got.Status, got.Message, tt.wantStatus)
			}
			if tt.wantStatus == domain.StatusUp && len(got.Extra) == 0 {
				t.Fatal("expected server info in Extra")
			}
		})
	}
}

func TestCheckRejectsBadConfig(t *testing.T) {
	ch := &Checker{query: fakeQuery(serverInfo{}, nil)}
	if _, err := ch.Check(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Check() with invalid config = nil error, want error")
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := registry.Get(TypeName); !ok {
		t.Fatalf("type %q not registered in Default registry", TypeName)
	}
}

// buildInfoResponse assembles a single-packet A2S_INFO response for the fake
// server.
func buildInfoResponse(info serverInfo) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, singlePacketFlag)
	buf = append(buf, a2sInfoResponse)
	buf = append(buf, 0x11) // protocol
	appendCStr := func(s string) { buf = append(buf, []byte(s)...); buf = append(buf, 0) }
	appendCStr(info.Name)
	appendCStr(info.Map)
	appendCStr("folder")
	appendCStr("game")
	buf = binary.LittleEndian.AppendUint16(buf, 730) // app id
	buf = append(buf, byte(info.Players), byte(info.MaxPlayers), byte(info.Bots))
	return buf
}

// fakeA2SServer starts a UDP loopback server that answers A2S_INFO queries,
// first demanding a challenge (like a real modern Source server) and then
// returning the given info. Returns host and port.
func fakeA2SServer(t *testing.T, info serverInfo) (string, int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, maxDatagram)
		challenge := []byte{0xDE, 0xAD, 0xBE, 0xEF}
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pkt := make([]byte, n)
			copy(pkt, buf[:n])
			// A challenge is present when the request carries 4 trailing bytes
			// after the "Source Engine Query\0" payload.
			hasChallenge := n >= 5+len(a2sInfoPayload)+4
			if !hasChallenge {
				resp := binary.LittleEndian.AppendUint32(nil, singlePacketFlag)
				resp = append(resp, a2sChallenge)
				resp = append(resp, challenge...)
				_, _ = pc.WriteTo(resp, addr)
				continue
			}
			_, _ = pc.WriteTo(buildInfoResponse(info), addr)
		}
	}()

	host, portStr, err := net.SplitHostPort(pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// TestLiveQueryOverLoopback exercises the real UDP A2S exchange, including the
// challenge handshake, against a loopback server — no Steam server involved.
func TestLiveQueryOverLoopback(t *testing.T) {
	want := serverInfo{Name: "Statwarden Test", Map: "de_nuke", Players: 7, MaxPlayers: 24, Bots: 2}
	host, port := fakeA2SServer(t, want)

	got, err := liveQuery(context.Background(), Config{Host: host, Port: port})
	if err != nil {
		t.Fatalf("liveQuery() error = %v", err)
	}
	if got != want {
		t.Fatalf("liveQuery() = %+v, want %+v", got, want)
	}
}

func TestParseInfoMalformed(t *testing.T) {
	// A response header byte with a truncated body must yield an error, not a
	// panic.
	if _, err := parseInfo([]byte{0x11, 'a'}); err == nil {
		t.Fatal("parseInfo(truncated) = nil error, want error")
	}
}
