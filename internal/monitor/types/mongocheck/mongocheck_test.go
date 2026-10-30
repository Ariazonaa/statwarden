package mongocheck

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

func fakeProbe(err error) func(context.Context, Config) error {
	return func(context.Context, Config) error { return err }
}

func TestCheckResultMapping(t *testing.T) {
	tests := []struct {
		name       string
		probeErr   error
		wantStatus domain.MonitorStatus
	}{
		{"ping ok is up", nil, domain.StatusUp},
		{"connection refused is down", errors.New("connection refused"), domain.StatusDown},
		{"auth failure is down", errors.New("Authentication failed"), domain.StatusDown},
		{"timeout is down", context.DeadlineExceeded, domain.StatusDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Checker{probe: fakeProbe(tt.probeErr)}
			got, err := ch.Check(context.Background(), json.RawMessage(`{"host":"db.local"}`))
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
	ch := &Checker{probe: fakeProbe(nil)}
	if _, err := ch.Check(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Check() with invalid config = nil error, want error")
	}
}

func TestRegistered(t *testing.T) {
	if _, ok := registry.Get(TypeName); !ok {
		t.Fatalf("type %q not registered in Default registry", TypeName)
	}
}
