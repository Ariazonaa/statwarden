package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// hostile is a monitor name a user with write access can store: an OSC
// sequence that retitles the terminal, a clear-screen, a bidi override that
// reorders what follows, a tab that shifts the table, and a newline that forges
// an extra row.
const hostile = "web\x1b]0;owned\x07\x1b[2J\u202eevil\tcol\nID 99 fake"

func assertTerminalSafe(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{"\x1b", "\x07", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Fatalf("output carries %q from a server string: %q", bad, out)
		}
	}
	if strings.Contains(out, "\nID 99 fake") {
		t.Fatalf("a newline in a server string forged an output line: %q", out)
	}
}

// TestTableOutputNeutralisesControlCharacters pins that strings from the server
// never reach the terminal raw. Monitor names, URLs and descriptions are
// written by any user with the writer role, and printing them verbatim let one
// of them drive the terminal of whoever ran `statwarden-cli monitor list` —
// retitle it, clear it, or hide text behind a bidi override.
func TestTableOutputNeutralisesControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	if err := writeMonitorTable(&buf, []MonitorView{{ID: 1, Name: hostile, Type: "http", URL: "https://example.test/\x1b[31m", Active: true, Status: "up"}}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	assertTerminalSafe(t, out)
	if lines := strings.Count(out, "\n"); lines != 2 {
		t.Fatalf("table has %d lines, want header + one row: %q", lines, out)
	}
	if !strings.Contains(out, "web") {
		t.Fatalf("the printable part of the name was lost: %q", out)
	}
}

func TestDetailOutputNeutralisesControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	m := MonitorView{ID: 1, Name: hostile, Type: "http", Description: hostile, LinkURL: hostile,
		Config: json.RawMessage(`{"url":"https://example.test/\u001b[2J"}`), CreatedAt: time.Now()}
	if err := writeMonitorDetail(&buf, m); err != nil {
		t.Fatal(err)
	}
	assertTerminalSafe(t, buf.String())
}

// The problem+json detail of an error response is server text too, and it is
// printed to stderr on every failure.
func TestAPIErrorNeutralisesControlCharacters(t *testing.T) {
	err := &APIError{Status: 400, Title: "Bad Request", Detail: hostile}
	assertTerminalSafe(t, err.Error())
}

func TestTerminalSafeKeepsOrdinaryText(t *testing.T) {
	for _, s := range []string{"Web — Frankfurt", "Prüfung 1/2", "日本語", "a  b"} {
		if got := terminalSafe(s); got != s {
			t.Errorf("terminalSafe(%q) = %q, want it unchanged", s, got)
		}
	}
}
