package smtp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// fakeServer is a minimal plaintext SMTP server for exercising smtpSend. Like
// a real MTA it insists on a bare <addr-spec> in MAIL FROM and RCPT TO and
// answers 501 to anything else, which is what a display-name address produced.
type fakeServer struct {
	addr string
	// greeting replaces the 220 banner, to stand in for a service that is not
	// an SMTP server at all.
	greeting string

	mu       sync.Mutex
	envelope []string
}

func startFakeServer(t *testing.T, greeting string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &fakeServer{addr: ln.Addr().String(), greeting: greeting}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *fakeServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	reply := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }
	if s.greeting != "" {
		reply(s.greeting)
		return
	}
	reply("220 mail.example.test ESMTP")
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				reply("250 queued")
			}
			continue
		}
		verb := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(verb, "EHLO"), strings.HasPrefix(verb, "HELO"):
			reply("250 mail.example.test")
		case strings.HasPrefix(verb, "MAIL FROM:"), strings.HasPrefix(verb, "RCPT TO:"):
			arg := line[strings.Index(line, ":")+1:]
			if !strings.HasPrefix(arg, "<") || strings.Count(arg, "<") != 1 || strings.ContainsAny(arg, " \"") {
				reply("501 5.1.7 bad address syntax")
				continue
			}
			s.mu.Lock()
			s.envelope = append(s.envelope, line)
			s.mu.Unlock()
			reply("250 ok")
		case verb == "DATA":
			inData = true
			reply("354 go ahead")
		case verb == "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}

func (s *fakeServer) config(t *testing.T, from string, to ...string) json.RawMessage {
	t.Helper()
	host, port, _ := net.SplitHostPort(s.addr)
	p, _ := strconv.Atoi(port)
	raw, err := json.Marshal(Config{Host: host, Port: p, From: from, To: to, Encryption: encNone})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Validate accepts "Ops <ops@example.test>" — net/mail parses it — so the
// envelope has to use the bare address. Passing the configured string straight
// to MAIL FROM and RCPT TO made every delivery fail with a 501 on a channel the
// UI had just accepted.
func TestDisplayNameAddressesAreSentAsBareEnvelopeAddresses(t *testing.T) {
	srv := startFakeServer(t, "")
	cfg := srv.config(t, "Statwarden Alerts <alerts@example.test>", "On Call <oncall@example.test>", "dev@example.test")
	if err := New().Validate(cfg); err != nil {
		t.Fatalf("precondition: Validate rejected the config: %v", err)
	}
	if err := New().Send(context.Background(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "t"}); err != nil {
		t.Fatalf("Send error = %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	want := []string{"MAIL FROM:<alerts@example.test>", "RCPT TO:<oncall@example.test>", "RCPT TO:<dev@example.test>"}
	if strings.Join(srv.envelope, "|") != strings.Join(want, "|") {
		t.Fatalf("envelope = %q, want %q", srv.envelope, want)
	}
}

// The header keeps the display name: it is what the recipient sees.
func TestDisplayNameIsKeptInTheHeader(t *testing.T) {
	var got []byte
	n := New()
	n.send = func(_ context.Context, _ Config, msg []byte) error { got = msg; return nil }
	cfg := json.RawMessage(`{"host":"h","from":"Statwarden Alerts <alerts@example.test>","to":["ops@example.test"]}`)
	if err := n.Send(context.Background(), cfg, domain.NotificationEvent{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "From: Statwarden Alerts <alerts@example.test>\r\n") {
		t.Fatalf("From header lost the display name:\n%s", got)
	}
}

// Pointing a channel at something that is not an SMTP server — which any
// writer can do, at any host the server can reach — must not turn the test
// button into a way to read that service's banner.
func TestANonSMTPBannerIsNotReflected(t *testing.T) {
	for _, banner := range []string{
		"HTTP/1.1 400 Bad Request internal-build-host-42", // not SMTP at all
		"554 internal-build-host-42 refuses you",          // SMTP-shaped refusal
	} {
		srv := startFakeServer(t, banner)
		cfg := srv.config(t, "alerts@example.test", "ops@example.test")
		err := New().Send(context.Background(), cfg, domain.NotificationEvent{Kind: domain.EventKindTest, Title: "t"})
		if err == nil {
			t.Fatalf("greeting %q was accepted", banner)
		}
		if strings.Contains(err.Error(), "internal-build-host") {
			t.Errorf("the error reflects the remote banner: %v", err)
		}
	}
}
