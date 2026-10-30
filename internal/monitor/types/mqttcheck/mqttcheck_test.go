package mqttcheck

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

// fakeProbe builds a probe returning the given result and error.
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
			"connect only ok",
			Config{Broker: "tcp://broker:1883"},
			probeResult{}, nil,
			domain.StatusUp,
		},
		{
			"connection refused is down",
			Config{Broker: "tcp://broker:1883"},
			probeResult{}, errors.New("connection refused"),
			domain.StatusDown,
		},
		{
			"auth failure is down",
			Config{Broker: "tcp://broker:1883", Username: "u", Password: "bad"},
			probeResult{}, errors.New("not authorized"),
			domain.StatusDown,
		},
		{
			"timeout is down",
			Config{Broker: "tcp://broker:1883"},
			probeResult{}, context.DeadlineExceeded,
			domain.StatusDown,
		},
		{
			"topic message received is up",
			Config{Broker: "tcp://broker:1883", Topic: "health"},
			probeResult{subscribed: true, gotMessage: true, payload: "ok"}, nil,
			domain.StatusUp,
		},
		{
			"no message is down",
			Config{Broker: "tcp://broker:1883", Topic: "health"},
			probeResult{subscribed: true, gotMessage: false}, nil,
			domain.StatusDown,
		},
		{
			"payload match is up",
			Config{Broker: "tcp://broker:1883", Topic: "health", ExpectedPayload: "ok"},
			probeResult{subscribed: true, gotMessage: true, payload: "ok"}, nil,
			domain.StatusUp,
		},
		{
			"payload mismatch is down",
			Config{Broker: "tcp://broker:1883", Topic: "health", ExpectedPayload: "ok"},
			probeResult{subscribed: true, gotMessage: true, payload: "bad"}, nil,
			domain.StatusDown,
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

// A failing assertion must not carry the OBSERVED value into the message.
//
// The message is served by GET /monitors/{id}/heartbeats to every authenticated
// reader, and a writer can aim a monitor at any address the process reaches — so
// an echo here is a read channel for internal services. What the operator needs
// for debugging is which assertion failed and what it EXPECTED, and that is
// their own text. See registry.Checker.Check for the policy.
func TestAFailedAssertionDoesNotEchoTheObservedValue(t *testing.T) {
	const secret = "s3cr3t-payload-from-an-internal-broker"

	c := Config{Topic: "sensors/1", ExpectedPayload: "online"}
	msg := c.assess(probeResult{subscribed: true, gotMessage: true, payload: secret})

	if msg == "" {
		t.Fatal("a mismatched assertion reported no failure at all")
	}
	if strings.Contains(msg, secret) {
		t.Errorf("the broker's payload reached the heartbeat message: %q", msg)
	}
	if !strings.Contains(msg, "online") {
		t.Errorf("the message dropped the operator's own expectation: %q", msg)
	}
}
