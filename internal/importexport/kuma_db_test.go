package importexport

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	_ "modernc.org/sqlite"
)

// buildKumaDB writes a minimal Uptime-Kuma-shaped SQLite database to a temp file
// and returns its bytes. It mirrors the column subset MapKumaDB reads.
func buildKumaDB(t *testing.T) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "kuma-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE monitor (id INTEGER PRIMARY KEY, name TEXT, type TEXT, url TEXT,
			hostname TEXT, port INTEGER, interval INTEGER, maxretries INTEGER, method TEXT,
			keyword TEXT, active INTEGER, upside_down INTEGER, dns_resolve_type TEXT,
			push_token TEXT, accepted_statuscodes_json TEXT, ignore_tls INTEGER)`,
		`CREATE TABLE tag (id INTEGER PRIMARY KEY, name TEXT, color TEXT)`,
		`CREATE TABLE monitor_tag (id INTEGER PRIMARY KEY, monitor_id INTEGER, tag_id INTEGER)`,
		`CREATE TABLE notification (id INTEGER PRIMARY KEY, name TEXT, config TEXT, active INTEGER)`,
		`CREATE TABLE monitor_notification (id INTEGER PRIMARY KEY, monitor_id INTEGER, notification_id INTEGER)`,
		`INSERT INTO tag (id,name,color) VALUES (1,'Web','#DB2777'),(2,'Docker','#4B5563')`,
		`INSERT INTO monitor (id,name,type,url,active,interval,maxretries,method,keyword,accepted_statuscodes_json,ignore_tls)
			VALUES (1,'Site','http','https://example.com',1,60,2,'GET','ok','["200-299","401"]',1)`,
		`INSERT INTO monitor (id,name,type,hostname,port,active,interval,maxretries)
			VALUES (2,'SSH','port','10.0.0.1',22,1,30,0)`,
		`INSERT INTO monitor (id,name,type,hostname,active,interval) VALUES (3,'Gw','ping','10.0.0.254',0,20)`,
		`INSERT INTO monitor (id,name,type,active,interval,push_token) VALUES (4,'Cron','push',1,120,'tok123')`,
		`INSERT INTO monitor (id,name,type,active) VALUES (5,'Weird','made-up-type',1)`,
		`INSERT INTO monitor_tag (monitor_id,tag_id) VALUES (1,1),(1,2)`,
		`INSERT INTO notification (id,name,config,active) VALUES (1,'Alerts','{"type":"pushover","x":"y"}',1)`,
		// Site (1) and SSH (2) alert via the Alerts channel; ping Gw (3) has none.
		`INSERT INTO monitor_notification (monitor_id,notification_id) VALUES (1,1),(2,1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	db.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMapKumaDB(t *testing.T) {
	res, err := MapKumaDB(buildKumaDB(t))
	if err != nil {
		t.Fatalf("MapKumaDB: %v", err)
	}

	// The made-up type is skipped with a warning; the other four map.
	if len(res.Monitors) != 4 {
		t.Fatalf("monitors = %d, want 4", len(res.Monitors))
	}
	byName := map[string]MonitorSpec{}
	for _, m := range res.Monitors {
		byName[m.Name] = m
	}

	// HTTP: type, url, keyword, accepted status codes, ignore-tls, and tags.
	site := byName["Site"]
	if site.Type != "http" || site.IntervalSeconds != 60 || site.MaxRetries != 2 || !site.Active {
		t.Fatalf("Site fields wrong: %+v", site)
	}
	var cfg map[string]any
	if err := json.Unmarshal(site.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["url"] != "https://example.com" || cfg["keyword"] != "ok" || cfg["ignore_tls_errors"] != true {
		t.Fatalf("Site config wrong: %v", cfg)
	}
	if codes, _ := cfg["accepted_status_codes"].([]any); len(codes) != 2 || codes[0] != "200-299" {
		t.Fatalf("accepted_status_codes wrong: %v", cfg["accepted_status_codes"])
	}
	if len(site.Tags) != 2 {
		t.Fatalf("Site tags = %v, want 2 (Web, Docker)", site.Tags)
	}
	// Channel assignment carried from monitor_notification.
	if len(site.Notifications) != 1 || site.Notifications[0] != "Alerts" {
		t.Fatalf("Site notifications = %v, want [Alerts]", site.Notifications)
	}
	if gw := byName["Gw"]; len(gw.Notifications) != 0 {
		t.Fatalf("Gw notifications = %v, want none", gw.Notifications)
	}

	// port -> tcp with host+port; ping -> ping host; the inactive ping keeps active=false.
	if ssh := byName["SSH"]; ssh.Type != "tcp" {
		t.Fatalf("SSH type = %s, want tcp", ssh.Type)
	}
	if gw := byName["Gw"]; gw.Type != "ping" || gw.Active {
		t.Fatalf("Gw wrong: %+v (want ping, inactive)", gw)
	}
	if cron := byName["Cron"]; cron.Type != "push" {
		t.Fatalf("Cron type = %s, want push", cron.Type)
	}

	// Tags carried over (both source tags).
	if len(res.Tags) != 2 {
		t.Fatalf("tags = %d, want 2", len(res.Tags))
	}
	// Pushover notification mapped.
	if len(res.Notifications) != 1 || res.Notifications[0].Type != "pushover" {
		t.Fatalf("notifications = %+v, want one pushover", res.Notifications)
	}
	// The unsupported type produced a warning.
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning for the unsupported monitor type")
	}
}

func TestMapKumaDBRejectsNonKuma(t *testing.T) {
	if _, err := MapKumaDB([]byte("this is not a database")); err == nil {
		t.Fatal("expected an error for a non-SQLite payload")
	}
}

// buildOldKumaDB writes a database shaped like an OLDER Uptime-Kuma: the monitor
// table lacks the columns later releases added, the tag table has no `color`, and
// the notification table has no `active`.
func buildOldKumaDB(t *testing.T) []byte {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "kuma-old-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		// No dns_resolve_type, accepted_statuscodes_json or ignore_tls.
		`CREATE TABLE monitor (id INTEGER PRIMARY KEY, name TEXT, type TEXT, url TEXT,
			hostname TEXT, port INTEGER, interval INTEGER, maxretries INTEGER,
			method TEXT, keyword TEXT, active INTEGER, upside_down INTEGER, push_token TEXT)`,
		`INSERT INTO monitor (id,name,type,url,interval,active,maxretries)
			VALUES (1,'legacy web','http','https://example.com',120,1,2)`,
		// No color column.
		`CREATE TABLE tag (id INTEGER PRIMARY KEY, name TEXT)`,
		`INSERT INTO tag (id,name) VALUES (1,'prod')`,
		`CREATE TABLE monitor_tag (monitor_id INTEGER, tag_id INTEGER)`,
		`INSERT INTO monitor_tag VALUES (1,1)`,
		// No active column.
		`CREATE TABLE notification (id INTEGER PRIMARY KEY, name TEXT, config TEXT)`,
		`INSERT INTO notification (id,name,config) VALUES (1,'ops','{"type":"slack"}')`,
		`CREATE TABLE monitor_notification (monitor_id INTEGER, notification_id INTEGER)`,
		`INSERT INTO monitor_notification VALUES (1,1)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// An older Kuma schema imports instead of aborting the whole upload.
//
// The projection used to be a fixed column list, with a comment claiming COALESCE
// made it tolerant of absent columns. COALESCE substitutes for a NULL VALUE and says
// nothing about a column that is not there: SQLite resolves names at prepare time,
// so the import died with `no such column: dns_resolve_type` before reading a row —
// in a feature whose entire job is to read a database written by another version.
//
// The tag and notification readers failed the same way but more quietly: their query
// error is swallowed as "the table is absent", so a missing `color` silently meant
// "no tags" and a missing `active` silently meant no channels at all — taking every
// monitor's alert routing with them, with no warning.
func TestMapKumaDBReadsAnOlderSchema(t *testing.T) {
	res, err := MapKumaDB(buildOldKumaDB(t))
	if err != nil {
		t.Fatalf("MapKumaDB on an older schema: %v", err)
	}

	if len(res.Monitors) != 1 {
		t.Fatalf("monitors = %d, want 1: %+v", len(res.Monitors), res.Monitors)
	}
	m := res.Monitors[0]
	if m.Name != "legacy web" || m.Type != "http" {
		t.Fatalf("monitor = %+v, want the legacy http monitor", m)
	}
	// Values that DO exist in the old schema must survive, not be defaulted away.
	if m.IntervalSeconds != 120 {
		t.Errorf("interval = %d, want 120 from the source row", m.IntervalSeconds)
	}
	if m.MaxRetries != 2 {
		t.Errorf("max_retries = %d, want 2 from the source row", m.MaxRetries)
	}

	// The tag came through despite the table having no colour.
	if len(res.Tags) != 1 || res.Tags[0].Name != "prod" {
		t.Fatalf("tags = %+v, want one named prod", res.Tags)
	}
	if len(m.Tags) != 1 || m.Tags[0] != "prod" {
		t.Errorf("monitor tags = %v, want [prod]", m.Tags)
	}

	// And the channel, despite the table having no active column — this is the one
	// whose loss was silent and took the monitor's alert routing with it.
	if len(res.Notifications) != 1 || res.Notifications[0].Name != "ops" {
		t.Fatalf("notifications = %+v, want one named ops", res.Notifications)
	}
	if len(m.Notifications) != 1 || m.Notifications[0] != "ops" {
		t.Errorf("monitor channels = %v, want [ops] — the alert routing was dropped", m.Notifications)
	}
}

// A table that is not a monitor table at all is still refused, so widening the
// projection did not turn "wrong file" into a silent empty import.
func TestMapKumaDBStillRejectsAMonitorTableWithoutTheBasics(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "kuma-bad-*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A table called monitor, but nothing that identifies a monitor.
	if _, err := db.Exec(`CREATE TABLE monitor (something TEXT)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := MapKumaDB(raw); err == nil {
		t.Fatal("a monitor table with no id/name/type was accepted")
	}
}
