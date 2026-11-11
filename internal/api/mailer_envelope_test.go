package api

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// SMTP_FROM is documented as an address, and "Statwarden <noreply@example.test>"
// is one — but handed verbatim to MAIL FROM it is "<Statwarden <noreply@…>>",
// which an MTA refuses with a 501. Every reset and subscriber mail then failed.
func TestSMTPMailerSendsABareEnvelopeSender(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var envelope []string
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		reply := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
		reply("220 mail.example.test ESMTP")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			upper := strings.ToUpper(line)
			switch {
			case inData:
				if line == "." {
					inData = false
					reply("250 queued")
				}
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				reply("250 mail.example.test")
			case strings.HasPrefix(upper, "MAIL FROM:"), strings.HasPrefix(upper, "RCPT TO:"):
				if arg := line[strings.Index(line, ":")+1:]; strings.Count(arg, "<") != 1 || strings.Contains(arg, " ") {
					reply("501 5.1.7 bad address syntax")
					continue
				}
				mu.Lock()
				envelope = append(envelope, line)
				mu.Unlock()
				reply("250 ok")
			case upper == "DATA":
				inData = true
				reply("354 go ahead")
			case upper == "QUIT":
				reply("221 bye")
				return
			default:
				reply("502 not implemented")
			}
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	m := NewSMTPMailer(SMTPSettings{
		Host: host, Port: p, Encryption: smtpEncNone,
		From: "Statwarden <noreply@example.test>",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Send(context.Background(), "Jane Doe <jane@example.test>", "Hello", "body", ""); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := "MAIL FROM:<noreply@example.test>|RCPT TO:<jane@example.test>"
	if got := strings.Join(envelope, "|"); got != want {
		t.Fatalf("envelope = %q, want %q", got, want)
	}
}
