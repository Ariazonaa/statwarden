package snmpcheck

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
		{"v2c minimal", `{"host":"dev","community":"public","oid":"1.3.6.1.2.1.1.3.0"}`, false},
		{"v2c explicit", `{"host":"dev","version":"2c","community":"public","oid":"1.3.6"}`, false},
		{"v2c with port", `{"host":"dev","community":"public","oid":"1.3.6","port":1161}`, false},
		{"v2c expected value", `{"host":"dev","community":"public","oid":"1.3.6","expected_value":"up"}`, false},
		{"v2c numeric assertion", `{"host":"dev","community":"public","oid":"1.3.6","operator":">","threshold":80}`, false},
		{"v3 noAuthNoPriv", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"noAuthNoPriv"}`, false},
		{"v3 authNoPriv", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"authNoPriv","auth_protocol":"SHA","auth_password":"secret12"}`, false},
		{"v3 authPriv", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"authPriv","auth_protocol":"SHA256","auth_password":"secret12","priv_protocol":"AES","priv_password":"privpass1"}`, false},

		{"empty config", ``, true},
		{"empty object", `{}`, true},
		{"missing host", `{"community":"public","oid":"1.3.6"}`, true},
		{"missing oid", `{"host":"dev","community":"public"}`, true},
		{"v2c without community", `{"host":"dev","oid":"1.3.6"}`, true},
		{"unknown version", `{"host":"dev","version":"1","community":"public","oid":"1.3.6"}`, true},
		{"port out of range", `{"host":"dev","community":"public","oid":"1.3.6","port":70000}`, true},

		{"v3 without username", `{"host":"dev","version":"3","oid":"1.3.6","security_level":"noAuthNoPriv"}`, true},
		{"v3 without security level", `{"host":"dev","version":"3","oid":"1.3.6","username":"u"}`, true},
		{"v3 unknown security level", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"bogus"}`, true},
		{"v3 authNoPriv missing auth proto", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"authNoPriv","auth_password":"x"}`, true},
		{"v3 authNoPriv bad auth proto", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"authNoPriv","auth_protocol":"CRC","auth_password":"x"}`, true},
		{"v3 authNoPriv missing password", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"authNoPriv","auth_protocol":"SHA"}`, true},
		{"v3 authPriv missing priv proto", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"authPriv","auth_protocol":"SHA","auth_password":"x"}`, true},
		{"v3 authPriv missing priv password", `{"host":"dev","version":"3","oid":"1.3.6","username":"u","security_level":"authPriv","auth_protocol":"SHA","auth_password":"x","priv_protocol":"AES"}`, true},

		{"expected and operator together", `{"host":"dev","community":"public","oid":"1.3.6","expected_value":"5","operator":">","threshold":1}`, true},
		{"operator without threshold", `{"host":"dev","community":"public","oid":"1.3.6","operator":">"}`, true},
		{"threshold without operator", `{"host":"dev","community":"public","oid":"1.3.6","threshold":5}`, true},
		{"unknown operator", `{"host":"dev","community":"public","oid":"1.3.6","operator":"~","threshold":5}`, true},

		{"unknown field", `{"host":"dev","community":"public","oid":"1.3.6","retries":2}`, true},
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
