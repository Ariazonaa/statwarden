package redischeck

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
		{"minimal valid", `{"host":"cache.local"}`, false},
		{"full connection", `{"host":"cache.local","port":6379,"username":"u","password":"p","database":2}`, false},
		{"tls", `{"host":"cache.local","tls":true}`, false},
		{"tls skip verify", `{"host":"cache.local","tls":true,"tls_skip_verify":true}`, false},
		{"connect timeout", `{"host":"cache.local","connect_timeout_seconds":5}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"port":6379}`, true},
		{"port too high", `{"host":"cache.local","port":70000}`, true},
		{"negative port", `{"host":"cache.local","port":-1}`, true},
		{"negative database", `{"host":"cache.local","database":-1}`, true},
		{"skip verify without tls", `{"host":"cache.local","tls_skip_verify":true}`, true},
		{"negative timeout", `{"host":"cache.local","connect_timeout_seconds":-1}`, true},
		{"unknown field", `{"host":"cache.local","databse":1}`, true},
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

func TestAddr(t *testing.T) {
	tests := []struct {
		cfg  Config
		want string
	}{
		{Config{Host: "cache.local"}, "cache.local:6379"},
		{Config{Host: "10.0.0.5", Port: 6380}, "10.0.0.5:6380"},
	}
	for _, tt := range tests {
		if got := tt.cfg.addr(); got != tt.want {
			t.Fatalf("addr() = %q, want %q", got, tt.want)
		}
	}
}
