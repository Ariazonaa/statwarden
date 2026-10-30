package certcheck

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
		{"minimal valid", `{"host":"example.com"}`, false},
		{"with port", `{"host":"example.com","port":8443}`, false},
		{"server name override", `{"host":"1.2.3.4","server_name":"example.com"}`, false},
		{"warn days", `{"host":"example.com","warn_days":14}`, false},
		{"ignore tls errors", `{"host":"example.com","ignore_tls_errors":true}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"blank host", `{"host":"   "}`, true},
		{"port too high", `{"host":"h","port":70000}`, true},
		{"negative port", `{"host":"h","port":-1}`, true},
		{"negative warn days", `{"host":"h","warn_days":-1}`, true},
		{"unknown field", `{"host":"h","insecure":true}`, true},
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
	if got := (Config{Port: 8443}).port(); got != 8443 {
		t.Fatalf("explicit port = %d, want 8443", got)
	}
}
