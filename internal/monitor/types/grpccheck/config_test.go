package grpccheck

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
		{"minimal", `{"target":"svc:50051"}`, false},
		{"with service", `{"target":"svc:50051","service":"pkg.Service"}`, false},
		{"expected serving", `{"target":"svc:50051","expected_status":"SERVING"}`, false},
		{"expected not_serving", `{"target":"svc:50051","expected_status":"NOT_SERVING"}`, false},
		{"lowercase status", `{"target":"svc:50051","expected_status":"serving"}`, false},
		{"tls", `{"target":"svc:50051","tls":true}`, false},
		{"tls with server name", `{"target":"svc:50051","tls":true,"server_name":"svc"}`, false},
		{"connect timeout", `{"target":"svc:50051","connect_timeout_seconds":5}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing target", `{"service":"x"}`, true},
		{"target with scheme", `{"target":"grpc://svc:50051"}`, true},
		{"unknown status", `{"target":"svc:50051","expected_status":"MAYBE"}`, true},
		{"tls option without tls", `{"target":"svc:50051","server_name":"svc"}`, true},
		{"ignore tls without tls", `{"target":"svc:50051","ignore_tls_errors":true}`, true},
		{"bad ca pem", `{"target":"svc:50051","tls":true,"tls_ca":"nope"}`, true},
		{"negative timeout", `{"target":"svc:50051","connect_timeout_seconds":-1}`, true},
		{"unknown field", `{"target":"svc:50051","targt":"x"}`, true},
		{"invalid json", `{"target":`, true},
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

func TestExpectedStatusDefaultAndNormalisation(t *testing.T) {
	if got := (Config{}).expectedStatus(); got != "SERVING" {
		t.Fatalf("default expectedStatus = %q, want SERVING", got)
	}
	if got := (Config{ExpectedStatus: "not_serving"}).expectedStatus(); got != "NOT_SERVING" {
		t.Fatalf("expectedStatus = %q, want NOT_SERVING", got)
	}
}
