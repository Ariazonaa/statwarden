package migrations

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.(up|down)\.sql$`)

// coreTables is the canonical table list from docs/architecture.md.
var coreTables = []string{
	"users",
	"sessions",
	"monitors",
	"tags",
	"monitor_tags",
	"heartbeats",
	"cert_info",
	"notifications",
	"monitor_notifications",
	"maintenance_windows",
	"maintenance_monitors",
	"status_pages",
	"status_page_groups",
	"status_page_monitors",
	"incidents",
	"incident_updates",
	"status_page_subscribers",
	"settings",
	"audit_log",
}

func readAll(t *testing.T) map[string]string {
	t.Helper()
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := fs.ReadFile(files, e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

func TestMigrationNamingAndPairing(t *testing.T) {
	all := readAll(t)
	if len(all) == 0 {
		t.Fatal("no embedded migration files found")
	}

	ups := map[string]string{}   // version -> name
	downs := map[string]string{} // version -> name
	for name := range all {
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("migration %q does not match NNNN_name.(up|down).sql", name)
			continue
		}
		switch m[3] {
		case "up":
			ups[m[1]] = name
		case "down":
			downs[m[1]] = name
		}
	}

	for v, name := range ups {
		if _, ok := downs[v]; !ok {
			t.Errorf("up migration %q has no matching down migration", name)
		}
	}
	for v, name := range downs {
		if _, ok := ups[v]; !ok {
			t.Errorf("down migration %q has no matching up migration", name)
		}
	}

	// Versions must be sequential starting at 0001.
	versions := make([]string, 0, len(ups))
	for v := range ups {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	for i, v := range versions {
		if want := fmt.Sprintf("%04d", i+1); v != want {
			t.Errorf("migration versions not sequential: got %s at position %d, want %s", v, i, want)
		}
	}

	for name, content := range all {
		if strings.TrimSpace(content) == "" {
			t.Errorf("migration %q is empty", name)
		}
	}
}

func TestMigrationSourceLoadsWithIofs(t *testing.T) {
	src, err := iofs.New(files, ".")
	if err != nil {
		t.Fatalf("iofs.New: %v", err)
	}
	defer src.Close()

	first, err := src.First()
	if err != nil {
		t.Fatalf("source has no first migration: %v", err)
	}
	if first != 1 {
		t.Errorf("first migration version = %d, want 1", first)
	}

	// Walk the whole chain; every version must expose readable up + down.
	for v := first; ; {
		if r, _, err := src.ReadUp(v); err != nil {
			t.Errorf("ReadUp(%d): %v", v, err)
		} else {
			_ = r.Close()
		}
		if r, _, err := src.ReadDown(v); err != nil {
			t.Errorf("ReadDown(%d): %v", v, err)
		} else {
			_ = r.Close()
		}
		next, err := src.Next(v)
		if err != nil {
			break // end of chain
		}
		v = next
	}
}

func TestInitMigrationCoversCoreSchema(t *testing.T) {
	all := readAll(t)
	up, ok := all["0001_init.up.sql"]
	if !ok {
		t.Fatal("0001_init.up.sql not embedded")
	}
	down, ok := all["0001_init.down.sql"]
	if !ok {
		t.Fatal("0001_init.down.sql not embedded")
	}

	for _, table := range coreTables {
		if !strings.Contains(up, "CREATE TABLE "+table+" (") {
			t.Errorf("up migration does not create table %q", table)
		}
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+table+";") {
			t.Errorf("down migration does not drop table %q", table)
		}
	}

	// The architecture mandates this heartbeat index for history queries.
	if !strings.Contains(up, "ON heartbeats (monitor_id, checked_at") {
		t.Error("up migration is missing the (monitor_id, checked_at) index on heartbeats")
	}

	// Every up-created table must be dropped exactly once; guard against
	// tables sneaking in without a down counterpart.
	creates := regexp.MustCompile(`(?m)^CREATE TABLE (\w+) \(`).FindAllStringSubmatch(up, -1)
	for _, m := range creates {
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+m[1]+";") {
			t.Errorf("table %q is created but never dropped in the down migration", m[1])
		}
	}
}
