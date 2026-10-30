package steamcheck

import (
	"encoding/json"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"minimal", `{"host":"gs.local","port":27015}`, false},
		{"with timeout", `{"host":"gs.local","port":27015,"connect_timeout_seconds":3}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"port":27015}`, true},
		{"missing port", `{"host":"gs.local"}`, true},
		{"port too high", `{"host":"gs.local","port":70000}`, true},
		{"negative port", `{"host":"gs.local","port":-1}`, true},
		{"negative timeout", `{"host":"gs.local","port":27015,"connect_timeout_seconds":-1}`, true},
		{"unknown field", `{"host":"gs.local","port":27015,"prt":1}`, true},
		{"invalid json", `{"host":`, true},
	}
	var ch Checker
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ch.Validate(json.RawMessage(tt.cfg))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate(%s) error = %v, wantErr %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}
