package dnscheck

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
		{"minimal defaults to A", `{"host":"example.com"}`, false},
		{"explicit A", `{"host":"example.com","record_type":"A"}`, false},
		{"lowercase record type", `{"host":"example.com","record_type":"aaaa"}`, false},
		{"all types MX", `{"host":"example.com","record_type":"MX"}`, false},
		{"SOA", `{"host":"example.com","record_type":"SOA"}`, false},
		{"CAA", `{"host":"example.com","record_type":"CAA"}`, false},
		{"PTR ip", `{"host":"1.1.1.1","record_type":"PTR"}`, false},
		{"custom resolver host only", `{"host":"example.com","resolver":"9.9.9.9"}`, false},
		{"custom resolver host port", `{"host":"example.com","resolver":"9.9.9.9:5353"}`, false},
		{"expected values", `{"host":"example.com","expected":["1.2.3.4"]}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"blank host", `{"host":"  "}`, true},
		{"unknown record type", `{"host":"example.com","record_type":"WHAT"}`, true},
		{"blank expected", `{"host":"example.com","expected":["  "]}`, true},
		{"unknown field", `{"host":"example.com","recordtype":"A"}`, true},
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

func TestResolverAddr(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"9.9.9.9", "9.9.9.9:53"},
		{"9.9.9.9:5353", "9.9.9.9:5353"},
		{"  1.1.1.1  ", "1.1.1.1:53"},
	}
	for _, tt := range tests {
		if got := (Config{Resolver: tt.in}).resolverAddr(); got != tt.want {
			t.Fatalf("resolverAddr(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
