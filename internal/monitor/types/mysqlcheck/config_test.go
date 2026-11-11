package mysqlcheck

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"minimal valid", `{"host":"db.local"}`, false},
		{"full connection", `{"host":"db.local","port":3306,"user":"u","password":"p","database":"app"}`, false},
		{"tls true", `{"host":"db.local","tls":"true"}`, false},
		{"tls skip-verify", `{"host":"db.local","tls":"skip-verify"}`, false},
		{"connect timeout", `{"host":"db.local","connect_timeout_seconds":5}`, false},
		{"query only", `{"host":"db.local","query":"SELECT 1"}`, false},
		{"query with expected", `{"host":"db.local","query":"SELECT 1","expected_result":"1"}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"port":3306}`, true},
		{"port too high", `{"host":"db.local","port":70000}`, true},
		{"negative port", `{"host":"db.local","port":-1}`, true},
		{"password without user", `{"host":"db.local","password":"p"}`, true},
		{"unknown tls", `{"host":"db.local","tls":"maybe"}`, true},
		{"negative timeout", `{"host":"db.local","connect_timeout_seconds":-1}`, true},
		{"expected without query", `{"host":"db.local","expected_result":"1"}`, true},
		{"unknown field", `{"host":"db.local","databse":"app"}`, true},
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
			[]string{"tcp(db.local:3306)", "timeout=10s"},
		},
		{
			"full credentials",
			Config{Host: "10.0.0.5", Port: 3307, User: "svc", Password: "s3cr3t", Database: "app", TLS: "skip-verify", ConnectTimeoutSeconds: 3},
			[]string{"svc:s3cr3t@tcp(10.0.0.5:3307)/app", "tls=skip-verify", "timeout=3s"},
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

// database/sql's Tx.Rollback takes no context, so after the assertion query the
// ROLLBACK was bounded by nothing: a server that stopped answering held the
// check — and the monitor, which is not rescheduled until it returns — forever.
// I/O timeouts on the connection are what bound it.
func TestDSNBoundsEveryRead(t *testing.T) {
	mc, err := mysql.ParseDSN(Config{Host: "db.example.test", ConnectTimeoutSeconds: 3}.dsn())
	if err != nil {
		t.Fatal(err)
	}
	if mc.ReadTimeout != 3*time.Second || mc.WriteTimeout != 3*time.Second {
		t.Fatalf("read/write timeout = %s/%s, want the 3s connect timeout", mc.ReadTimeout, mc.WriteTimeout)
	}
}
