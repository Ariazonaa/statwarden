package mqttcheck

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
		{"minimal tcp", `{"broker":"tcp://broker:1883"}`, false},
		{"with client id", `{"broker":"tcp://broker:1883","client_id":"statwarden"}`, false},
		{"with credentials", `{"broker":"tcp://broker:1883","username":"u","password":"p"}`, false},
		{"topic only", `{"broker":"tcp://broker:1883","topic":"health/#"}`, false},
		{"topic with expected", `{"broker":"tcp://broker:1883","topic":"health","expected_payload":"ok"}`, false},
		{"round trip", `{"broker":"tcp://broker:1883","topic":"health","publish_payload":"ping","expected_payload":"ping"}`, false},
		{"tls broker", `{"broker":"ssl://broker:8883"}`, false},
		{"tls with ignore errors", `{"broker":"ssl://broker:8883","ignore_tls_errors":true}`, false},
		{"websocket", `{"broker":"ws://broker:9001"}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing broker", `{"topic":"x"}`, true},
		{"bad scheme", `{"broker":"http://broker:1883"}`, true},
		{"no host", `{"broker":"tcp://"}`, true},
		{"password without user", `{"broker":"tcp://broker:1883","password":"p"}`, true},
		{"publish without topic", `{"broker":"tcp://broker:1883","publish_payload":"x"}`, true},
		{"expected without topic", `{"broker":"tcp://broker:1883","expected_payload":"x"}`, true},
		{"tls options on plain broker", `{"broker":"tcp://broker:1883","ignore_tls_errors":true}`, true},
		{"bad ca pem", `{"broker":"ssl://broker:8883","tls_ca":"nope"}`, true},
		{"negative timeout", `{"broker":"tcp://broker:1883","connect_timeout_seconds":-1}`, true},
		{"unknown field", `{"broker":"tcp://broker:1883","brokr":"x"}`, true},
		{"invalid json", `{"broker":`, true},
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

func TestClientOptions(t *testing.T) {
	c := Config{Broker: "tcp://broker:1883", ClientID: "statwarden", Username: "u", Password: "p"}
	opts, err := c.clientOptions()
	if err != nil {
		t.Fatalf("clientOptions() error = %v", err)
	}
	if len(opts.Servers) != 1 || opts.Servers[0].String() != "tcp://broker:1883" {
		t.Fatalf("broker not registered: %+v", opts.Servers)
	}
	if opts.ClientID != "statwarden" {
		t.Fatalf("client id = %q, want statwarden", opts.ClientID)
	}
	if opts.Username != "u" {
		t.Fatalf("username = %q, want u", opts.Username)
	}
}

func TestUsesTLS(t *testing.T) {
	tests := map[string]bool{
		"tcp://b:1883":  false,
		"ssl://b:8883":  true,
		"tls://b:8883":  true,
		"wss://b:443":   true,
		"ws://b:9001":   false,
		"mqtts://b:883": true,
	}
	for broker, want := range tests {
		if got := (Config{Broker: broker}).usesTLS(); got != want {
			t.Fatalf("usesTLS(%s) = %v, want %v", broker, got, want)
		}
	}
}
