package mysqlcheck

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

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
		{"connectivity ok", Config{Host: "db.local"}, probeResult{}, nil, domain.StatusUp},
		{"connection refused is down", Config{Host: "db.local"}, probeResult{}, errors.New("connection refused"), domain.StatusDown},
		{"auth failure is down", Config{Host: "db.local", User: "u", Password: "bad"}, probeResult{}, errors.New("Access denied for user"), domain.StatusDown},
		{"timeout is down", Config{Host: "db.local"}, probeResult{}, context.DeadlineExceeded, domain.StatusDown},
		{"query match is up", Config{Host: "db.local", Query: "SELECT 1", ExpectedResult: "1"}, probeResult{value: "1", hasValue: true}, nil, domain.StatusUp},
		{"query mismatch is down", Config{Host: "db.local", Query: "SELECT 1", ExpectedResult: "1"}, probeResult{value: "0", hasValue: true}, nil, domain.StatusDown},
		{"expected but no rows is down", Config{Host: "db.local", Query: "SELECT x", ExpectedResult: "1"}, probeResult{}, nil, domain.StatusDown},
		{"query without expectation is up", Config{Host: "db.local", Query: "SELECT 1"}, probeResult{}, nil, domain.StatusUp},
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
		{int64(42), "42"},
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
