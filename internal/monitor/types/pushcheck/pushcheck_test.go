package pushcheck

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"ok minimal", `{"expected_interval_seconds":60}`, false},
		{"ok with grace", `{"expected_interval_seconds":60,"grace_period_seconds":30}`, false},
		{"empty", ``, true},
		{"zero interval", `{"expected_interval_seconds":0}`, true},
		{"negative grace", `{"expected_interval_seconds":60,"grace_period_seconds":-1}`, true},
		{"unknown field", `{"expected_interval_seconds":60,"nope":1}`, true},
	}
	var c Checker
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Validate(json.RawMessage(tt.cfg))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%q) err = %v, wantErr = %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

func TestIsPassive(t *testing.T) {
	var c registry.Checker = &Checker{}
	if _, ok := c.(registry.PassiveChecker); !ok {
		t.Fatal("push checker must satisfy registry.PassiveChecker")
	}
}

func TestRegisteredInDefault(t *testing.T) {
	if _, ok := registry.Get(TypeName); !ok {
		t.Fatalf("type %q not registered in the default registry", TypeName)
	}
}

func TestDeadline(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	got, ok := Deadline(json.RawMessage(`{"expected_interval_seconds":60,"grace_period_seconds":30}`), base)
	if !ok {
		t.Fatal("Deadline ok = false, want true")
	}
	want := base.Add(90 * time.Second)
	if !got.Equal(want) {
		t.Fatalf("Deadline = %v, want %v", got, want)
	}
	if _, ok := Deadline(json.RawMessage(`{}`), base); ok {
		t.Fatal("Deadline on invalid config should report ok = false")
	}
}
