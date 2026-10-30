package dockercheck

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
		{"minimal local", `{"container":"web"}`, false},
		{"explicit unix", `{"endpoint":"unix:///var/run/docker.sock","container":"web"}`, false},
		{"remote tcp", `{"endpoint":"tcp://dockerhost:2376","container":"web"}`, false},
		{"required running", `{"container":"web","required_state":"running"}`, false},
		{"required healthy", `{"container":"web","required_state":"healthy"}`, false},
		{"ignore tls", `{"endpoint":"tcp://h:2376","container":"web","ignore_tls_errors":true}`, false},
		{"connect timeout", `{"container":"web","connect_timeout_seconds":5}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing container", `{"endpoint":"unix:///var/run/docker.sock"}`, true},
		{"bad scheme", `{"endpoint":"ftp://h/x","container":"web"}`, true},
		{"unix without path", `{"endpoint":"unix://","container":"web"}`, true},
		{"tcp without host", `{"endpoint":"tcp://","container":"web"}`, true},
		{"unknown state", `{"container":"web","required_state":"green"}`, true},
		{"cert without key", `{"container":"web","tls_cert":"x"}`, true},
		{"key without cert", `{"container":"web","tls_key":"x"}`, true},
		{"bad ca pem", `{"container":"web","tls_ca":"not-a-pem"}`, true},
		{"negative timeout", `{"container":"web","connect_timeout_seconds":-1}`, true},
		{"unknown field", `{"container":"web","containr":"x"}`, true},
		{"invalid json", `{"container":`, true},
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

func TestResolveTransport(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		wantUnix   bool
		wantScheme string
		wantURL    string
	}{
		{
			"default local socket",
			Config{Container: "web"},
			true, "http",
			"http://docker/containers/web/json",
		},
		{
			"remote tcp plain",
			Config{Endpoint: "tcp://host:2375", Container: "api"},
			false, "http",
			"http://host:2375/containers/api/json",
		},
		{
			"remote tcp with tls material",
			Config{Endpoint: "tcp://host:2376", Container: "api", IgnoreTLSErrors: true},
			false, "https",
			"https://host:2376/containers/api/json",
		},
		{
			"https scheme",
			Config{Endpoint: "https://host:2376", Container: "db"},
			false, "https",
			"https://host:2376/containers/db/json",
		},
		{
			"container id path-escaped",
			Config{Endpoint: "tcp://host:2375", Container: "a/b"},
			false, "http",
			"http://host:2375/containers/a%2Fb/json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := tt.cfg.resolveTransport()
			if err != nil {
				t.Fatalf("resolveTransport() error = %v", err)
			}
			if tr.unix != tt.wantUnix {
				t.Fatalf("unix = %v, want %v", tr.unix, tt.wantUnix)
			}
			if tr.scheme != tt.wantScheme {
				t.Fatalf("scheme = %q, want %q", tr.scheme, tt.wantScheme)
			}
			if got := tr.inspectURL(tt.cfg.Container); got != tt.wantURL {
				t.Fatalf("inspectURL = %q, want %q", got, tt.wantURL)
			}
		})
	}
}
