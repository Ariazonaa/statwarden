package pingcheck

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
		{"ip host", `{"host":"1.1.1.1"}`, false},
		{"with count", `{"host":"h","count":10}`, false},
		{"with threshold", `{"host":"h","packet_loss_threshold_percent":20}`, false},
		{"max count", `{"host":"h","count":100}`, false},
		{"threshold 100", `{"host":"h","packet_loss_threshold_percent":100}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"blank host", `{"host":"  "}`, true},
		{"negative count", `{"host":"h","count":-1}`, true},
		{"count too high", `{"host":"h","count":1000}`, true},
		{"negative threshold", `{"host":"h","packet_loss_threshold_percent":-5}`, true},
		{"threshold over 100", `{"host":"h","packet_loss_threshold_percent":150}`, true},
		{"unknown field", `{"host":"h","packets":4}`, true},
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

func TestConfigDefaults(t *testing.T) {
	if got := (Config{}).count(); got != defaultCount {
		t.Fatalf("default count = %d, want %d", got, defaultCount)
	}
	if got := (Config{Count: 7}).count(); got != 7 {
		t.Fatalf("count = %d, want 7", got)
	}
	if got := (Config{}).lossThreshold(); got != defaultLossThreshold {
		t.Fatalf("default loss threshold = %.1f, want %.1f", got, defaultLossThreshold)
	}
	if got := (Config{PacketLossThresholdPercent: 20}).lossThreshold(); got != 20 {
		t.Fatalf("loss threshold = %.1f, want 20", got)
	}
}
