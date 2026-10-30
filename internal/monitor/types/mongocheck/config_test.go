package mongocheck

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
		{"full connection", `{"host":"db.local","port":27017,"user":"u","password":"p","database":"admin"}`, false},
		{"tls", `{"host":"db.local","tls":true}`, false},
		{"tls skip verify", `{"host":"db.local","tls":true,"tls_skip_verify":true}`, false},
		{"connect timeout", `{"host":"db.local","connect_timeout_seconds":5}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"port":27017}`, true},
		{"port too high", `{"host":"db.local","port":70000}`, true},
		{"negative port", `{"host":"db.local","port":-1}`, true},
		{"password without user", `{"host":"db.local","password":"p"}`, true},
		{"skip verify without tls", `{"host":"db.local","tls_skip_verify":true}`, true},
		{"negative timeout", `{"host":"db.local","connect_timeout_seconds":-1}`, true},
		{"unknown field", `{"host":"db.local","tlss":true}`, true},
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

func TestURI(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		contains []string
	}{
		{
			"defaults applied",
			Config{Host: "db.local"},
			[]string{"mongodb://db.local:27017/", "connectTimeoutMS=10000"},
		},
		{
			"full credentials and tls",
			Config{Host: "10.0.0.5", Port: 27018, User: "svc", Password: "s3cr3t", Database: "admin", TLS: true, TLSSkipVerify: true, ConnectTimeoutSeconds: 3},
			[]string{"mongodb://svc:s3cr3t@10.0.0.5:27018/admin", "tls=true", "tlsInsecure=true", "connectTimeoutMS=3000"},
		},
		{
			"tls off omits tls param",
			Config{Host: "db.local"},
			[]string{"mongodb://db.local:27017/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.uri()
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("uri() = %q, want it to contain %q", got, want)
				}
			}
			if !tt.cfg.TLS && strings.Contains(got, "tls=true") {
				t.Fatalf("uri() = %q, should not contain tls=true when TLS is off", got)
			}
		})
	}
}
