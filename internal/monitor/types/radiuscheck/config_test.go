package radiuscheck

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
		{"minimal valid", `{"host":"radius.local","secret":"s","username":"u"}`, false},
		{"with password", `{"host":"radius.local","secret":"s","username":"u","password":"p"}`, false},
		{"with port", `{"host":"radius.local","secret":"s","username":"u","port":1645}`, false},
		{"with nas identifier", `{"host":"radius.local","secret":"s","username":"u","nas_identifier":"statwarden"}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"secret":"s","username":"u"}`, true},
		{"missing secret", `{"host":"h","username":"u"}`, true},
		{"missing username", `{"host":"h","secret":"s"}`, true},
		{"blank username", `{"host":"h","secret":"s","username":"  "}`, true},
		{"port out of range", `{"host":"h","secret":"s","username":"u","port":70000}`, true},
		{"unknown field", `{"host":"h","secret":"s","username":"u","realm":"x"}`, true},
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

func TestConfigPortDefault(t *testing.T) {
	if got := (Config{}).port(); got != defaultPort {
		t.Fatalf("default port = %d, want %d", got, defaultPort)
	}
	if got := (Config{Port: 1645}).port(); got != 1645 {
		t.Fatalf("explicit port = %d, want 1645", got)
	}
}
