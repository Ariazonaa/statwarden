package tcpcheck

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
		{"minimal valid", `{"host":"example.com","port":22}`, false},
		{"ip host", `{"host":"127.0.0.1","port":443}`, false},
		{"lowest port", `{"host":"h","port":1}`, false},
		{"highest port", `{"host":"h","port":65535}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"port":22}`, true},
		{"blank host", `{"host":"  ","port":22}`, true},
		{"missing port", `{"host":"example.com"}`, true},
		{"port zero", `{"host":"h","port":0}`, true},
		{"port too high", `{"host":"h","port":70000}`, true},
		{"negative port", `{"host":"h","port":-1}`, true},
		{"unknown field", `{"host":"h","port":22,"protocol":"ssh"}`, true},
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
