package httpcheck

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStatusCodeSetUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    StatusCodeSet
		wantErr bool
	}{
		{"numbers", `[200, 418]`, StatusCodeSet{{200, 200}, {418, 418}}, false},
		{"range string", `["200-299"]`, StatusCodeSet{{200, 299}}, false},
		{"single code string", `["301"]`, StatusCodeSet{{301, 301}}, false},
		{"comma list in one string", `["200-299,301,418"]`, StatusCodeSet{{200, 299}, {301, 301}, {418, 418}}, false},
		{"mixed with spaces", `[418, "200-299, 301"]`, StatusCodeSet{{418, 418}, {200, 299}, {301, 301}}, false},
		{"empty array", `[]`, StatusCodeSet{}, false},
		{"not an array", `"200"`, nil, true},
		{"boolean element", `[true]`, nil, true},
		{"float element", `[200.5]`, nil, true},
		{"garbage range", `["abc"]`, nil, true},
		{"open-ended range", `["200-"]`, nil, true},
		{"empty string element", `[""]`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got StatusCodeSet
			err := json.Unmarshal([]byte(tt.json), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Unmarshal(%s) error = %v, wantErr %v", tt.json, err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Unmarshal(%s) = %v, want %v", tt.json, got, tt.want)
			}
		})
	}
}

func TestStatusCodeSetMarshalRoundTrip(t *testing.T) {
	in := StatusCodeSet{{200, 299}, {301, 301}, {418, 418}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out StatusCodeSet
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: %v -> %s -> %v", in, data, out)
	}
}

func TestStatusCodeSetMatch(t *testing.T) {
	tests := []struct {
		name string
		set  StatusCodeSet
		code int
		want bool
	}{
		{"empty set accepts 200", nil, 200, true},
		{"empty set accepts 299", nil, 299, true},
		{"empty set rejects 301", nil, 301, false},
		{"empty set rejects 500", nil, 500, false},
		{"range lower bound", StatusCodeSet{{200, 299}}, 200, true},
		{"range upper bound", StatusCodeSet{{200, 299}}, 299, true},
		{"outside range", StatusCodeSet{{200, 299}}, 300, false},
		{"single code hit", StatusCodeSet{{418, 418}}, 418, true},
		{"multi-range hit", StatusCodeSet{{200, 299}, {401, 401}}, 401, true},
		{"explicit set rejects unlisted 200s", StatusCodeSet{{301, 302}}, 200, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.Match(tt.code); got != tt.want {
				t.Fatalf("%v.Match(%d) = %v, want %v", tt.set, tt.code, got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"minimal valid", `{"url":"https://example.com"}`, false},
		{"http scheme", `{"url":"http://example.com/health"}`, false},
		{"method and codes", `{"url":"https://example.com","method":"HEAD","accepted_status_codes":[200,401]}`, false},
		{"code ranges", `{"url":"https://example.com","accepted_status_codes":["200-299,301",418]}`, false},
		{"keyword pair", `{"url":"https://example.com","keyword":"ok","keyword_absent":"error"}`, false},
		{"json assertion", `{"url":"https://example.com","json_path":"status","json_expected":"healthy"}`, false},
		{"json existence only", `{"url":"https://example.com","json_path":"checks.0.state"}`, false},
		{"header exact", `{"url":"https://example.com","header_name":"X-Ok","header_value":"yes"}`, false},
		{"header regex", `{"url":"https://example.com","header_name":"X-Ok","header_value_regex":"^ye+s$"}`, false},
		{"header presence only", `{"url":"https://example.com","header_name":"X-Ok"}`, false},
		{"response time threshold", `{"url":"https://example.com","max_response_time_ms":500}`, false},
		{"request shaping", `{"url":"https://example.com","method":"POST","body":"{\"a\":1}","body_encoding":"json","headers":{"X-Key":"v"},"user_agent":"custom/1.0"}`, false},
		{"form body", `{"url":"https://example.com","method":"POST","body":"a=1&b=2","body_encoding":"form"}`, false},
		{"raw body", `{"url":"https://example.com","method":"PUT","body":"plain","body_encoding":"raw"}`, false},
		{"basic auth", `{"url":"https://example.com","auth_method":"basic","basic_username":"u","basic_password":"p"}`, false},
		{"basic auth empty password", `{"url":"https://example.com","auth_method":"basic","basic_username":"u"}`, false},
		{"bearer auth", `{"url":"https://example.com","auth_method":"bearer","bearer_token":"tok"}`, false},
		{"redirect toggle", `{"url":"https://example.com","follow_redirects":false}`, false},
		{"max redirects", `{"url":"https://example.com","max_redirects":3}`, false},
		{"ignore tls errors", `{"url":"https://example.com","ignore_tls_errors":true}`, false},
		{"http proxy", `{"url":"https://example.com","proxy_url":"http://proxy.local:3128"}`, false},
		{"socks5 proxy with auth", `{"url":"https://example.com","proxy_url":"socks5://user:pass@proxy.local:1080"}`, false},
		{"socks5h proxy", `{"url":"https://example.com","proxy_url":"socks5h://proxy.local:1080"}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing scheme", `{"url":"example.com"}`, true},
		{"unsupported scheme", `{"url":"ftp://example.com"}`, true},
		{"no host", `{"url":"https://"}`, true},
		// url is the monitor's display target and cannot be redacted whole the
		// way proxy_url is, so a credential inside it is refused rather than
		// stored somewhere the read path has to defend. Username-only userinfo
		// counts: Go's client sends basic auth for that too.
		{"credentials in url", `{"url":"https://user:pass@example.com/health"}`, true},
		{"username in url", `{"url":"https://user@example.com/health"}`, true},
		{"empty password in url", `{"url":"https://user:@example.com"}`, true},
		{"bad method", `{"url":"https://example.com","method":"BREW"}`, true},
		{"lowercase method", `{"url":"https://example.com","method":"get"}`, true},
		{"status code too low", `{"url":"https://example.com","accepted_status_codes":[99]}`, true},
		{"status code too high", `{"url":"https://example.com","accepted_status_codes":[600]}`, true},
		{"range out of bounds", `{"url":"https://example.com","accepted_status_codes":["50-99"]}`, true},
		{"inverted range", `{"url":"https://example.com","accepted_status_codes":["299-200"]}`, true},
		{"garbage range", `{"url":"https://example.com","accepted_status_codes":["abc"]}`, true},
		{"empty header name", `{"url":"https://example.com","headers":{" ":"v"}}`, true},
		{"unknown body encoding", `{"url":"https://example.com","body_encoding":"xml"}`, true},
		{"json encoding invalid body", `{"url":"https://example.com","body":"not json","body_encoding":"json"}`, true},
		{"form encoding invalid body", `{"url":"https://example.com","body":"a=%zz","body_encoding":"form"}`, true},
		{"unknown auth method", `{"url":"https://example.com","auth_method":"digest"}`, true},
		{"basic auth without username", `{"url":"https://example.com","auth_method":"basic"}`, true},
		{"bearer auth without token", `{"url":"https://example.com","auth_method":"bearer"}`, true},
		{"credentials without auth method", `{"url":"https://example.com","basic_username":"u"}`, true},
		{"bearer token without auth method", `{"url":"https://example.com","bearer_token":"tok"}`, true},
		{"mixed auth fields", `{"url":"https://example.com","auth_method":"basic","basic_username":"u","bearer_token":"tok"}`, true},
		{"client cert without key", `{"url":"https://example.com","tls_client_cert_pem":"x"}`, true},
		{"client key without cert", `{"url":"https://example.com","tls_client_key_pem":"x"}`, true},
		{"garbage client cert pair", `{"url":"https://example.com","tls_client_cert_pem":"x","tls_client_key_pem":"y"}`, true},
		{"negative max redirects", `{"url":"https://example.com","max_redirects":-1}`, true},
		{"negative response time", `{"url":"https://example.com","max_response_time_ms":-1}`, true},
		{"proxy bad scheme", `{"url":"https://example.com","proxy_url":"ftp://proxy.local"}`, true},
		{"proxy without host", `{"url":"https://example.com","proxy_url":"socks5://"}`, true},
		{"proxy unparsable", `{"url":"https://example.com","proxy_url":"://nope"}`, true},
		{"identical keywords", `{"url":"https://example.com","keyword":"ok","keyword_absent":"ok"}`, true},
		{"json expected without path", `{"url":"https://example.com","json_expected":"healthy"}`, true},
		{"header value without name", `{"url":"https://example.com","header_value":"yes"}`, true},
		{"header value and regex together", `{"url":"https://example.com","header_name":"X","header_value":"a","header_value_regex":"a"}`, true},
		{"invalid header regex", `{"url":"https://example.com","header_name":"X","header_value_regex":"["}`, true},
		{"unknown field", `{"url":"https://example.com","keywrod":"ok"}`, true},
		{"invalid json", `{"url":`, true},
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

// TestValidateMTLS uses a generated, syntactically valid client certificate.
func TestValidateMTLS(t *testing.T) {
	certPEM, keyPEM, _ := generateClientCert(t)
	cfg, err := json.Marshal(Config{
		URL:              "https://example.com",
		TLSClientCertPEM: certPEM,
		TLSClientKeyPEM:  keyPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ch Checker
	if err := ch.Validate(cfg); err != nil {
		t.Fatalf("Validate(valid mTLS config) = %v, want nil", err)
	}
}
