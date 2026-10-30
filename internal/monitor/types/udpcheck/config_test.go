package udpcheck

import (
	"encoding/json"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"minimal valid", `{"host":"example.com","port":53}`, false},
		{"ip host", `{"host":"127.0.0.1","port":123}`, false},
		{"lowest port", `{"host":"h","port":1}`, false},
		{"highest port", `{"host":"h","port":65535}`, false},
		{"text payload", `{"host":"h","port":53,"payload":"ping"}`, false},
		{"explicit text encoding", `{"host":"h","port":53,"payload":"ping","payload_encoding":"text"}`, false},
		{"hex payload", `{"host":"h","port":53,"payload":"00 01 ff","payload_encoding":"hex"}`, false},
		{"base64 payload", `{"host":"h","port":53,"payload":"cGluZw==","payload_encoding":"base64"}`, false},
		{"expect with implicit response", `{"host":"h","port":53,"expect":"pong"}`, false},
		{"expect with explicit response", `{"host":"h","port":53,"expect":"pong","expect_response":true}`, false},
		{"fire and forget", `{"host":"h","port":514,"payload":"x","expect_response":false}`, false},
		{"response timeout", `{"host":"h","port":53,"response_timeout_seconds":2}`, false},
		{"zero response timeout takes the default", `{"host":"h","port":53,"response_timeout_seconds":0}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"port":53}`, true},
		{"blank host", `{"host":"  ","port":53}`, true},
		{"missing port", `{"host":"example.com"}`, true},
		{"port zero", `{"host":"h","port":0}`, true},
		{"port too high", `{"host":"h","port":70000}`, true},
		{"negative port", `{"host":"h","port":-1}`, true},
		{"negative response timeout", `{"host":"h","port":53,"response_timeout_seconds":-1}`, true},
		{"unknown encoding", `{"host":"h","port":53,"payload_encoding":"ascii"}`, true},
		{"odd-length hex", `{"host":"h","port":53,"payload":"abc","payload_encoding":"hex"}`, true},
		{"non-hex payload", `{"host":"h","port":53,"payload":"zz","payload_encoding":"hex"}`, true},
		{"non-base64 payload", `{"host":"h","port":53,"payload":"!!!","payload_encoding":"base64"}`, true},
		{"non-hex expect", `{"host":"h","port":53,"expect":"zz","payload_encoding":"hex"}`, true},
		// An assertion on a reply the check is told not to wait for can only
		// ever fail, so it is rejected at save time rather than at 3am.
		{"expect without a required response", `{"host":"h","port":53,"expect":"pong","expect_response":false}`, true},
		{"unknown field", `{"host":"h","port":53,"protocol":"dns"}`, true},
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

func TestExpectResponseDefaultsToTrue(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"absent", Config{}, true},
		{"explicit true", Config{ExpectResponse: &yes}, true},
		{"explicit false", Config{ExpectResponse: &no}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.expectResponse(); got != tt.want {
				t.Fatalf("expectResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResponseTimeout(t *testing.T) {
	t.Parallel()
	if got := (Config{}).responseTimeout(); got != defaultResponseTimeout*time.Second {
		t.Fatalf("responseTimeout() = %s, want the %ds default", got, defaultResponseTimeout)
	}
	if got := (Config{ResponseTimeoutSeconds: 3}).responseTimeout(); got != 3*time.Second {
		t.Fatalf("responseTimeout() = %s, want 3s", got)
	}
}

func TestPayloadDecoding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"text is taken verbatim", Config{Payload: "STATUS\n"}, "STATUS\n"},
		{"empty payload is a zero-length datagram", Config{}, ""},
		{"hex", Config{Payload: "70696e67", PayloadEncoding: encodingHex}, "ping"},
		{"grouped hex ignores whitespace", Config{Payload: "70 69 6e 67", PayloadEncoding: encodingHex}, "ping"},
		{"base64", Config{Payload: "cGluZw==", PayloadEncoding: encodingBase64}, "ping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.payloadBytes()
			if err != nil {
				t.Fatalf("payloadBytes() error = %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("payloadBytes() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The encoding governs the assertion too — a hex probe asserted in text would
// never match, and the mismatch would look like an outage.
func TestExpectUsesThePayloadEncoding(t *testing.T) {
	t.Parallel()
	got, err := Config{Expect: "706f6e67", PayloadEncoding: encodingHex}.expectBytes()
	if err != nil {
		t.Fatalf("expectBytes() error = %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("expectBytes() = %q, want %q", got, "pong")
	}
}
