package importexport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMapKumaMonitors(t *testing.T) {
	raw := []byte(`{
		"version": "1.23.0",
		"monitorList": [
			{"name":"Web","type":"http","url":"https://example.com","interval":60,"maxretries":3,"active":true},
			{"name":"SSH","type":"port","hostname":"host.example","port":22,"interval":30},
			{"name":"Kw","type":"keyword","url":"https://x.test","keyword":"healthy","interval":45},
			{"name":"Cron","type":"push","interval":300},
			{"name":"Weird","type":"gamedig","interval":60}
		]
	}`)
	res, err := MapKuma(raw)
	if err != nil {
		t.Fatalf("MapKuma: %v", err)
	}
	if len(res.Monitors) != 4 {
		t.Fatalf("mapped monitors = %d, want 4: %+v", len(res.Monitors), res.Monitors)
	}
	// The unsupported gamedig monitor should produce a warning.
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, ";"), "gamedig") {
		t.Fatalf("expected a warning about gamedig, got %v", res.Warnings)
	}

	byName := map[string]MonitorSpec{}
	for _, m := range res.Monitors {
		byName[m.Name] = m
	}
	if byName["Web"].Type != "http" || byName["Web"].MaxRetries != 3 {
		t.Fatalf("Web mapped wrong: %+v", byName["Web"])
	}
	var webCfg map[string]any
	_ = json.Unmarshal(byName["Web"].Config, &webCfg)
	if webCfg["url"] != "https://example.com" {
		t.Fatalf("Web url not mapped: %v", webCfg)
	}
	if byName["SSH"].Type != "tcp" {
		t.Fatalf("SSH should map to tcp: %+v", byName["SSH"])
	}
	var sshCfg map[string]any
	_ = json.Unmarshal(byName["SSH"].Config, &sshCfg)
	if sshCfg["host"] != "host.example" || sshCfg["port"].(float64) != 22 {
		t.Fatalf("SSH config wrong: %v", sshCfg)
	}
	var kwCfg map[string]any
	_ = json.Unmarshal(byName["Kw"].Config, &kwCfg)
	if kwCfg["keyword"] != "healthy" {
		t.Fatalf("keyword not mapped: %v", kwCfg)
	}
	var pushCfg map[string]any
	_ = json.Unmarshal(byName["Cron"].Config, &pushCfg)
	if pushCfg["expected_interval_seconds"].(float64) != 300 {
		t.Fatalf("push interval not mapped: %v", pushCfg)
	}
}

func TestMapKumaNotifications(t *testing.T) {
	raw := []byte(`{
		"notificationList": [
			{"name":"Ops Slack","type":"slack","active":true},
			{"name":"Legacy","type":"octopush","active":true}
		]
	}`)
	res, err := MapKuma(raw)
	if err != nil {
		t.Fatalf("MapKuma: %v", err)
	}
	if len(res.Notifications) != 1 || res.Notifications[0].Type != "slack" {
		t.Fatalf("notifications = %+v", res.Notifications)
	}
	if len(res.Notifications[0].Unmapped) == 0 {
		t.Fatal("expected a note about re-entering credentials")
	}
	joined := strings.Join(res.Warnings, ";")
	if !strings.Contains(joined, "octopush") {
		t.Fatalf("expected octopush warning, got %v", res.Warnings)
	}
}

func TestMapKumaObjectKeyed(t *testing.T) {
	// Some Kuma exports key monitors by id in an object.
	raw := []byte(`{"monitors":{"1":{"name":"A","type":"ping","hostname":"1.1.1.1","interval":60}}}`)
	res, err := MapKuma(raw)
	if err != nil {
		t.Fatalf("MapKuma: %v", err)
	}
	if len(res.Monitors) != 1 || res.Monitors[0].Type != "ping" {
		t.Fatalf("object-keyed monitors not parsed: %+v", res.Monitors)
	}
}

func TestMapKumaInvalidJSON(t *testing.T) {
	if _, err := MapKuma([]byte("not json")); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}
