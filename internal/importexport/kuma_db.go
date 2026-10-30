package importexport

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite" (no cgo)
)

// MapKumaDB maps a Uptime-Kuma SQLite database (the raw bytes of its kuma.db)
// onto Statwarden specs. Kuma removed the JSON backup export in v1.23, so the
// database is the reliable migration source. Only the monitor, notification and
// tag tables are read; heartbeat history is ignored. Field translation reuses
// the same code as the JSON export path (mapKumaMonitorList / kumaMonitorConfig).
func MapKumaDB(raw []byte) (KumaResult, error) {
	// modernc's driver needs a file path, so stage the upload in a temp file.
	f, err := os.CreateTemp("", "kuma-import-*.db")
	if err != nil {
		return KumaResult{}, fmt.Errorf("kuma db import: temp file: %w", err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, werr := f.Write(raw); werr != nil {
		_ = f.Close()
		return KumaResult{}, fmt.Errorf("kuma db import: write temp file: %w", werr)
	}
	if cerr := f.Close(); cerr != nil {
		return KumaResult{}, fmt.Errorf("kuma db import: close temp file: %w", cerr)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return KumaResult{}, fmt.Errorf("kuma db import: open database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("SELECT 1 FROM monitor LIMIT 1"); err != nil {
		return KumaResult{}, fmt.Errorf("kuma db import: not an Uptime-Kuma database (no monitor table)")
	}

	tags, tagName, err := readKumaTags(db)
	if err != nil {
		return KumaResult{}, err
	}
	monTags, err := readKumaMonitorTags(db, tagName)
	if err != nil {
		return KumaResult{}, err
	}
	// Notifications first: their id→name lookup lets us attach each monitor's
	// channel assignments before the monitors are built.
	notifs, notifName, nwarns, err := readKumaNotifications(db)
	if err != nil {
		return KumaResult{}, err
	}
	monNotifs, err := readKumaMonitorNotifs(db, notifName)
	if err != nil {
		return KumaResult{}, err
	}
	mons, warns, err := readKumaMonitors(db, monTags, monNotifs)
	if err != nil {
		return KumaResult{}, err
	}

	res := KumaResult{Monitors: mons, Notifications: notifs, Tags: tags}
	res.Warnings = append(res.Warnings, warns...)
	res.Warnings = append(res.Warnings, nwarns...)
	return res, nil
}

// tableColumns returns the column names present in a table, so a projection can
// be built from the schema that is actually there.
//
// Uptime-Kuma's schema has grown over years of releases, and an import is by
// definition reading a database written by some OTHER version. A fixed column list
// therefore has to be treated as a guess: SQLite resolves column names when it
// prepares the statement, so naming one the table does not have is an error before
// a single row is read.
//
// An unreadable table is reported as "no columns" rather than as an error: every
// caller already treats an absent table as a non-fatal condition, and a table that
// cannot even be introspected is indistinguishable from an absent one.
func tableColumns(db *sql.DB, table string) map[string]bool {
	have := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return have
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return map[string]bool{}
		}
		have[name] = true
	}
	// A half-read column list is worse than none: it would report a column that
	// exists as absent, and the projection would then quietly substitute a constant
	// for real data. Discard a partial answer so the caller's own "does this look
	// like a Kuma table" check fails loudly instead.
	if err := rows.Err(); err != nil {
		return map[string]bool{}
	}
	return have
}

// projection renders one column of a SELECT: COALESCE(col, fallback) when the
// schema has it, and the bare fallback when it does not, so the Scan order stays
// fixed whatever the source schema looks like. The names come from a literal list
// in this file, never from the database, so there is nothing to inject.
func projection(have map[string]bool, name, fallback string) string {
	if have[name] {
		return "COALESCE(" + name + ", " + fallback + ")"
	}
	return fallback
}

// readKumaMonitorNotifs maps each monitor id to the names of the notification
// channels assigned to it (Kuma's monitor_notification join table).
func readKumaMonitorNotifs(db *sql.DB, notifName map[int64]string) (map[int64][]string, error) {
	out := map[int64][]string{}
	rows, err := db.Query(`SELECT monitor_id, notification_id FROM monitor_notification`)
	if err != nil {
		return out, nil //nolint:nilerr // absent join table is fine
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mid, nid int64
		if err := rows.Scan(&mid, &nid); err != nil {
			return nil, fmt.Errorf("kuma db import: scan monitor_notification: %w", err)
		}
		if name, ok := notifName[nid]; ok && name != "" {
			out[mid] = append(out[mid], name)
		}
	}
	return out, rows.Err()
}

// readKumaTags returns the tag specs and an id→name lookup.
func readKumaTags(db *sql.DB) ([]TagSpec, map[int64]string, error) {
	// Same reason as readKumaMonitors: `color` is not in every Kuma schema, and a
	// fixed projection naming it turned "this database has no colours" into "this
	// database has no tags" — silently, because the query error is swallowed below
	// on the assumption that it means the TABLE is absent.
	have := tableColumns(db, "tag")
	if !have["id"] || !have["name"] {
		return nil, map[int64]string{}, nil // no tag table (or not one we can read)
	}
	// Literals only, as in readKumaMonitors and readKumaNotifications.
	rows, err := db.Query(`SELECT id, name, ` + projection(have, "color", "''") + ` FROM tag`) //nolint:gosec // G202: composed only of literals
	if err != nil {
		// Very old Kuma has no tag table; that is not fatal.
		return nil, map[int64]string{}, nil //nolint:nilerr // absent tag table is fine
	}
	defer func() { _ = rows.Close() }()
	var tags []TagSpec
	byID := map[int64]string{}
	for rows.Next() {
		var id int64
		var name, color string
		if err := rows.Scan(&id, &name, &color); err != nil {
			return nil, nil, fmt.Errorf("kuma db import: scan tag: %w", err)
		}
		byID[id] = name
		tags = append(tags, TagSpec{Name: name, Color: color})
	}
	return tags, byID, rows.Err()
}

// readKumaMonitorTags maps each monitor id to the names of its tags.
func readKumaMonitorTags(db *sql.DB, tagName map[int64]string) (map[int64][]string, error) {
	out := map[int64][]string{}
	rows, err := db.Query(`SELECT monitor_id, tag_id FROM monitor_tag`)
	if err != nil {
		return out, nil //nolint:nilerr // absent join table is fine
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var mid, tid int64
		if err := rows.Scan(&mid, &tid); err != nil {
			return nil, fmt.Errorf("kuma db import: scan monitor_tag: %w", err)
		}
		if name, ok := tagName[tid]; ok && name != "" {
			out[mid] = append(out[mid], name)
		}
	}
	return out, rows.Err()
}

// readKumaMonitors reads the monitor table into Kuma structs and maps them via
// the shared mapKumaMonitorList. The projection is built from the columns the
// source schema actually has, so a Kuma old enough to be missing some of them
// still imports — see the comment inside.
func readKumaMonitors(db *sql.DB, monTags, monNotifs map[int64][]string) ([]MonitorSpec, []string, error) {
	// The projection is built from the columns this database actually has.
	//
	// It used to be a fixed list, with a comment claiming that "columns absent in
	// very old schemas are read through COALESCE so a missing value never aborts
	// the import". COALESCE does no such thing: it substitutes for a NULL VALUE, and
	// says nothing about a column that is not there. SQLite resolves names when it
	// prepares the statement, so an older Kuma schema failed the whole import with
	// `no such column: dns_resolve_type` before reading a row — the exact case the
	// comment promised to handle, in a feature whose entire job is to read a
	// database written by some other version.
	have := tableColumns(db, "monitor")
	for _, required := range []string{"id", "name", "type"} {
		if !have[required] {
			return nil, nil, fmt.Errorf(
				"kuma db import: the monitor table has no %q column; this does not look "+
					"like an Uptime-Kuma database", required)
		}
	}
	// Concatenated for the same reason, and safe for the same one: every
	// projection argument below is a literal, and `have` only selects between two
	// constants. See readKumaNotifications for the invariant in full.
	q := `SELECT id, name, type, ` + strings.Join([]string{ //nolint:gosec // G202: composed only of literals
		projection(have, "url", "''"),
		projection(have, "hostname", "''"),
		projection(have, "port", "0"),
		projection(have, "interval", "60"),
		projection(have, "maxretries", "0"),
		projection(have, "method", "''"),
		projection(have, "keyword", "''"),
		projection(have, "active", "1"),
		projection(have, "upside_down", "0"),
		projection(have, "dns_resolve_type", "''"),
		projection(have, "push_token", "''"),
		projection(have, "accepted_statuscodes_json", "''"),
		projection(have, "ignore_tls", "0"),
	}, ", ") + ` FROM monitor ORDER BY id`
	rows, err := db.Query(q)
	if err != nil {
		return nil, nil, fmt.Errorf("kuma db import: query monitors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var list []kumaMonitor
	var warnings []string
	for rows.Next() {
		var (
			id                         int64
			name, typ, url, hostname   string
			port, interval, maxretries int
			method, keyword            string
			active, upsideDown         int
			dnsType, pushToken         string
			acceptedJSON               string
			ignoreTLS                  int
		)
		if err := rows.Scan(&id, &name, &typ, &url, &hostname, &port, &interval,
			&maxretries, &method, &keyword, &active, &upsideDown, &dnsType,
			&pushToken, &acceptedJSON, &ignoreTLS); err != nil {
			return nil, nil, fmt.Errorf("kuma db import: scan monitor: %w", err)
		}
		act := active != 0
		km := kumaMonitor{
			Name:           name,
			Type:           typ,
			URL:            url,
			Hostname:       hostname,
			Port:           port,
			Interval:       interval,
			Retries:        maxretries,
			Method:         method,
			Keyword:        keyword,
			Active:         &act,
			UpsideDown:     upsideDown != 0,
			DNSResolveType: dnsType,
			IgnoreTLS:      ignoreTLS != 0,
			PushToken:      pushToken,
			Tags:           monTags[id],
			Notifications:  monNotifs[id],
		}
		// accepted_statuscodes_json is a JSON array of codes/ranges, e.g.
		// ["200-299","401"]. Best-effort: ignore if it does not parse.
		if acceptedJSON != "" {
			var codes []string
			if json.Unmarshal([]byte(acceptedJSON), &codes) == nil {
				km.AcceptedStatusCodes = codes
			}
		}
		if km.Type == "push" && km.PushToken != "" {
			warnings = append(warnings, fmt.Sprintf("monitor %q: Statwarden issues a new push token on create; update your push source with the new URL", km.Name))
		}
		list = append(list, km)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("kuma db import: iterate monitors: %w", err)
	}

	specs, mapWarn := mapKumaMonitorList(list)
	warnings = append(warnings, mapWarn...)
	return specs, warnings, nil
}

// readKumaNotifications reads the notification table. Kuma stores the provider
// type and its secrets inside the JSON `config` column; we read the type and
// create a matching, inactive-config stub, asking the operator to re-enter
// credentials (mirroring the JSON import).
func readKumaNotifications(db *sql.DB) ([]NotificationSpec, map[int64]string, []string, error) {
	nameByID := map[int64]string{}
	// Built from the schema present, for the reason readKumaMonitors gives. Here the
	// old behaviour was the quietest of the three: a missing `active` or `config`
	// column failed the query, the error was swallowed as "no notification table",
	// and every channel plus every monitor's alert routing disappeared from the
	// import without a warning.
	have := tableColumns(db, "notification")
	if !have["id"] || !have["name"] {
		return nil, nameByID, nil, nil // no notification table (or not one we can read)
	}
	// Concatenated, and safe because of an invariant worth stating: projection's
	// `name` and `fallback` are compile-time literals at all sixteen call sites,
	// and `have` — the only value derived from the UPLOADED database — is read as
	// a membership test alone. A foreign column name never reaches the string;
	// it only decides which of two constants is chosen. Pass a dynamic name to
	// projection and this stops being true.
	rows, err := db.Query(`SELECT id, name, ` + //nolint:gosec // G202: composed only of literals, see above
		projection(have, "config", "''") + `, ` +
		projection(have, "active", "1") + ` FROM notification`)
	if err != nil {
		return nil, nameByID, nil, nil //nolint:nilerr // absent notification table is fine
	}
	defer func() { _ = rows.Close() }()
	var out []NotificationSpec
	var warnings []string
	for rows.Next() {
		var id int64
		var name, config string
		var active int
		if err := rows.Scan(&id, &name, &config, &active); err != nil {
			return nil, nil, nil, fmt.Errorf("kuma db import: scan notification: %w", err)
		}
		var cfg struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(config), &cfg)
		utype, ok := notificationTypeMap[cfg.Type]
		if !ok || utype == "" {
			warnings = append(warnings, fmt.Sprintf("notification %q: unsupported Kuma provider %q, skipped", name, cfg.Type))
			continue
		}
		// Record the id→name only for channels we actually create, so a monitor
		// never ends up referencing a channel that was skipped.
		nameByID[id] = name
		out = append(out, NotificationSpec{
			Name:     name,
			Type:     utype,
			Active:   active != 0,
			Config:   json.RawMessage(`{}`),
			Unmapped: []string{"config: Kuma stores channel secrets separately; re-enter credentials"},
		})
	}
	return out, nameByID, warnings, rows.Err()
}
