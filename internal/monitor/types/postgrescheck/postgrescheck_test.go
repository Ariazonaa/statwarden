package postgrescheck

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// fakeProbe builds a probe function returning the given result and error.
func fakeProbe(res probeResult, err error) func(context.Context, Config) (probeResult, error) {
	return func(context.Context, Config) (probeResult, error) { return res, err }
}

func TestCheckResultMapping(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		res        probeResult
		probeErr   error
		wantStatus domain.MonitorStatus
	}{
		{
			"connectivity ok",
			Config{Host: "db.local"},
			probeResult{}, nil,
			domain.StatusUp,
		},
		{
			"connection refused is down",
			Config{Host: "db.local"},
			probeResult{}, errors.New("connection refused"),
			domain.StatusDown,
		},
		{
			"auth failure is down",
			Config{Host: "db.local", User: "u", Password: "bad"},
			probeResult{}, errors.New("password authentication failed"),
			domain.StatusDown,
		},
		{
			"timeout is down",
			Config{Host: "db.local"},
			probeResult{}, context.DeadlineExceeded,
			domain.StatusDown,
		},
		{
			"query match is up",
			Config{Host: "db.local", Query: "SELECT 1", ExpectedResult: "1"},
			probeResult{value: "1", hasValue: true}, nil,
			domain.StatusUp,
		},
		{
			"query mismatch is down",
			Config{Host: "db.local", Query: "SELECT 1", ExpectedResult: "1"},
			probeResult{value: "0", hasValue: true}, nil,
			domain.StatusDown,
		},
		{
			"expected result but no rows is down",
			Config{Host: "db.local", Query: "SELECT x", ExpectedResult: "1"},
			probeResult{}, nil,
			domain.StatusDown,
		},
		{
			"query without expectation is up",
			Config{Host: "db.local", Query: "SELECT 1"},
			probeResult{}, nil,
			domain.StatusUp,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Checker{probe: fakeProbe(tt.res, tt.probeErr)}
			raw, err := json.Marshal(tt.cfg)
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
		})
	}
}

func TestCheckRejectsBadConfig(t *testing.T) {
	ch := &Checker{probe: fakeProbe(probeResult{}, nil)}
	if _, err := ch.Check(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Check() with invalid config = nil error, want error")
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := registry.Get(TypeName); !ok {
		t.Fatalf("type %q not registered in Default registry", TypeName)
	}
}

func TestStringify(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"text", "text"},
		{[]byte("bytes"), "bytes"},
		{int32(42), "42"},
		{true, "true"},
	}
	for _, tt := range tests {
		if got := stringify(tt.in); got != tt.want {
			t.Fatalf("stringify(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A failing assertion must not carry the OBSERVED value into the message.
//
// The message is served by GET /monitors/{id}/heartbeats to every authenticated
// reader, and a writer can aim a monitor at any address the process reaches — so
// an echo here is a read channel for internal services. What the operator needs
// for debugging is which assertion failed and what it EXPECTED, and that is
// their own text. See registry.Checker.Check for the policy.
func TestAFailedAssertionDoesNotEchoTheObservedValue(t *testing.T) {
	const secret = "s3cr3t-row-from-an-internal-database"

	c := Config{ExpectedResult: "1"}
	msg := c.assess(probeResult{value: secret, hasValue: true})

	if msg == "" {
		t.Fatal("a mismatched assertion reported no failure at all")
	}
	if strings.Contains(msg, secret) {
		t.Errorf("the observed value reached the heartbeat message: %q", msg)
	}
	if !strings.Contains(msg, "1") {
		t.Errorf("the message dropped the operator's own expectation: %q", msg)
	}
}

// A failing assertion query must not put the server's error text into the
// message either: PostgreSQL quotes the offending value — a cast that fails
// names the row's content — and the message is readable by every user. The
// SQLSTATE says what kind of failure it was without saying what was in the row.
func TestAFailedAssertionQueryDoesNotEchoTheServerError(t *testing.T) {
	const secret = "s3cr3t-row-from-an-internal-database"
	old := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond // the fake never answers ROLLBACK
	t.Cleanup(func() { cleanupTimeout = old })
	srv := startFakeServer(t, &pgproto3.ErrorResponse{
		Severity: "ERROR", Code: "22P02",
		Message: `invalid input syntax for type integer: "` + secret + `"`,
		Detail:  "Failing row contains (" + secret + ").",
	})

	var ch Checker
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ch.Check(ctx, mustCfg(t, Config{
		Host: srv.host, Port: srv.port, User: "monitor", Database: "app",
		SSLMode: SSLModeDisable, Query: "SELECT secret::int FROM t",
	}))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if res.Status != domain.StatusDown {
		t.Fatalf("status = %s (%s), want down", res.Status, res.Message)
	}
	if strings.Contains(res.Message, secret) {
		t.Errorf("the server's error text reached the heartbeat message: %q", res.Message)
	}
	if !strings.Contains(res.Message, "22P02") {
		t.Errorf("message %q does not carry the SQLSTATE", res.Message)
	}
}

// The rollback after the assertion query, and the close, ran on
// context.Background(): a server that stopped answering at that point held the
// check forever, and the engine never schedules a monitor whose check has not
// returned.
func TestCleanupAfterTheQueryIsBounded(t *testing.T) {
	old := cleanupTimeout
	cleanupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { cleanupTimeout = old })

	srv := startFakeServer(t, &pgproto3.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "syntax error"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = livePostgres(context.Background(), Config{
			Host: srv.host, Port: srv.port, User: "monitor", Database: "app",
			SSLMode: SSLModeDisable, Query: "SELEC 1",
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("livePostgres still waiting for a ROLLBACK the server never answers")
	}
}
