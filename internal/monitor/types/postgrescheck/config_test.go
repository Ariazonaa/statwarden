package postgrescheck

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"minimal valid", `{"host":"db.local"}`, false},
		{"full connection", `{"host":"db.local","port":5432,"user":"u","password":"p","database":"app"}`, false},
		{"ssl require", `{"host":"db.local","ssl_mode":"require"}`, false},
		{"ssl verify-full", `{"host":"db.local","ssl_mode":"verify-full"}`, false},
		{"connect timeout", `{"host":"db.local","connect_timeout_seconds":5}`, false},
		{"query only", `{"host":"db.local","query":"SELECT 1"}`, false},
		{"query with expected", `{"host":"db.local","query":"SELECT 1","expected_result":"1"}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"port":5432}`, true},
		{"port too high", `{"host":"db.local","port":70000}`, true},
		{"negative port", `{"host":"db.local","port":-1}`, true},
		{"password without user", `{"host":"db.local","password":"p"}`, true},
		{"unknown ssl mode", `{"host":"db.local","ssl_mode":"maybe"}`, true},
		{"negative timeout", `{"host":"db.local","connect_timeout_seconds":-1}`, true},
		{"expected without query", `{"host":"db.local","expected_result":"1"}`, true},
		{"unknown field", `{"host":"db.local","porrt":5432}`, true},
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

func TestDSN(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		contains []string
	}{
		{
			"defaults applied",
			Config{Host: "db.local"},
			[]string{"postgres://db.local:5432/", "sslmode=prefer", "connect_timeout=10"},
		},
		{
			"full credentials",
			Config{Host: "10.0.0.5", Port: 6432, User: "svc", Password: "s3cr3t", Database: "app", SSLMode: "require", ConnectTimeoutSeconds: 3},
			[]string{"postgres://svc:s3cr3t@10.0.0.5:6432/app", "sslmode=require", "connect_timeout=3"},
		},
		{
			"special chars escaped",
			Config{Host: "db.local", User: "a@b", Password: "p:w/d"},
			[]string{"a%40b", "p%3Aw%2Fd"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.dsn()
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("dsn() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}
