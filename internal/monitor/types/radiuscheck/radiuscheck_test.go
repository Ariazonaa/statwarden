package radiuscheck

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

func mustCfg(t *testing.T, c Config) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRegistration(t *testing.T) {
	c, ok := registry.Get(TypeName)
	if !ok {
		t.Fatal("radius checker not self-registered")
	}
	td, ok := c.(registry.TimeoutDefaulter)
	if !ok || td.DefaultTimeout() <= 0 {
		t.Fatal("radius checker must provide a positive default timeout")
	}
}

func TestCheckResponseMapping(t *testing.T) {
	tests := []struct {
		name    string
		code    radius.Code
		want    domain.MonitorStatus
		wantSub string
	}{
		{"access accept", radius.CodeAccessAccept, domain.StatusUp, "Accept"},
		{"access reject", radius.CodeAccessReject, domain.StatusDown, "Reject"},
		{"access challenge", radius.CodeAccessChallenge, domain.StatusDown, "Challenge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Checker{exchange: func(_ context.Context, req *radius.Packet, _ string) (*radius.Packet, error) {
				// The request must carry the configured username.
				if got := rfc2865.UserName_GetString(req); got != "alice" {
					t.Fatalf("request User-Name = %q, want alice", got)
				}
				return req.Response(tt.code), nil
			}}
			res, err := ch.Check(context.Background(), mustCfg(t, Config{
				Host: "radius.local", Secret: "s3cret", Username: "alice", Password: "pw",
			}))
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if res.Status != tt.want {
				t.Fatalf("status = %s (%s), want %s", res.Status, res.Message, tt.want)
			}
		})
	}
}

func TestCheckNASIdentifier(t *testing.T) {
	ch := &Checker{exchange: func(_ context.Context, req *radius.Packet, _ string) (*radius.Packet, error) {
		if got := rfc2865.NASIdentifier_GetString(req); got != "statwarden" {
			t.Fatalf("NAS-Identifier = %q, want statwarden", got)
		}
		return req.Response(radius.CodeAccessAccept), nil
	}}
	res, err := ch.Check(context.Background(), mustCfg(t, Config{
		Host: "radius.local", Secret: "s", Username: "u", Password: "p", NASIdentifier: "statwarden",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != domain.StatusUp {
		t.Fatalf("status = %s (%s), want up", res.Status, res.Message)
	}
}

func TestCheckExchangeError(t *testing.T) {
	ch := &Checker{exchange: func(context.Context, *radius.Packet, string) (*radius.Packet, error) {
		return nil, errors.New("i/o timeout")
	}}
	res, err := ch.Check(context.Background(), mustCfg(t, Config{Host: "h", Secret: "s", Username: "u"}))
	if err != nil {
		t.Fatalf("transport failures must be DOWN results, not errors; got %v", err)
	}
	if res.Status != domain.StatusDown || res.Message == "" {
		t.Fatalf("res = %+v, want down with message", res)
	}
}

func TestCheckRejectsInvalidConfig(t *testing.T) {
	var ch Checker
	if _, err := ch.Check(context.Background(), json.RawMessage(`{"host":"h","username":"u"}`)); err == nil {
		t.Fatal("Check must reject a config missing the shared secret")
	}
}
