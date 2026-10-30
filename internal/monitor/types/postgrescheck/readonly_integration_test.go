//go:build integration

package postgrescheck

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Ariazonaa/statwarden/internal/domain"
)

// TestAssertionQueryCannotWrite is the only place the read-only guarantee can
// actually be demonstrated.
//
// The assertion query is operator-supplied SQL, executed with the monitor's own
// stored credentials — routinely a full application account, not a reporting
// role. Nothing constrained the statement, so anyone who could edit a monitor
// could make the monitoring system issue UPDATE or DELETE against the database
// it is supposed to be watching, on a timer.
//
// A unit test cannot see this: there is no database, and the fix is a property
// of the server session rather than of any Go code that could be faked. So it
// lives here, behind the integration tag, against a real PostgreSQL.
func TestAssertionQueryCannotWrite(t *testing.T) {
	// Unlike the internal/api suite, this connects to the server named by
	// DATABASE_URL directly rather than provisioning a throwaway database: the
	// checker takes discrete connection fields and the point is to drive it the
	// way a real monitor is driven. It creates and drops one distinctively
	// named table and touches nothing else.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is required for the integration suite (see `make test-integration`)")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	const table = "readonly_probe_target"
	for _, stmt := range []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table),
		fmt.Sprintf(`CREATE TABLE %s (n integer NOT NULL)`, table),
		fmt.Sprintf(`INSERT INTO %s (n) VALUES (1)`, table),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table))
	})

	// The checker takes discrete connection fields, not a DSN, so split the
	// suite's URL into them.
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		host, portStr = u.Host, "5432"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	pw, _ := u.User.Password()
	base := map[string]any{
		"host":     host,
		"port":     port,
		"user":     u.User.Username(),
		"password": pw,
		"database": strings.TrimPrefix(u.Path, "/"),
		"ssl_mode": u.Query().Get("sslmode"),
	}

	// Check reports a rejected query as a DOWN RESULT, not as an error — an
	// error return is reserved for unusable configuration. The first version of
	// this test looked at the error and therefore saw every write "accepted"
	// while the writes were in fact being blocked; it failed against a working
	// fix. Read the result.
	check := func(t *testing.T, query string) domain.CheckResult {
		t.Helper()
		cfg := make(map[string]any, len(base)+1)
		maps.Copy(cfg, base)
		cfg["query"] = query
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if verr := (&Checker{}).Validate(raw); verr != nil {
			t.Fatalf("config rejected before it could run: %v", verr)
		}
		res, cerr := (&Checker{}).Check(ctx, raw)
		if cerr != nil {
			t.Fatalf("Check returned a config error: %v", cerr)
		}
		return res
	}

	// A plain read still works — the guard must not break the feature.
	if res := check(t, fmt.Sprintf(`SELECT n FROM %s`, table)); res.Status != domain.StatusUp {
		t.Fatalf("a read-only assertion query reported %s: %s", res.Status, res.Message)
	}

	for _, tc := range []struct{ name, query string }{
		{"update", fmt.Sprintf(`UPDATE %s SET n = 99`, table)},
		{"update returning", fmt.Sprintf(`UPDATE %s SET n = 99 RETURNING n`, table)},
		{"delete", fmt.Sprintf(`DELETE FROM %s`, table)},
		{"insert", fmt.Sprintf(`INSERT INTO %s (n) VALUES (42)`, table)},
		{"writing cte", fmt.Sprintf(
			`WITH w AS (UPDATE %s SET n = 99 RETURNING n) SELECT count(*) FROM w`, table)},
		{"ddl", `CREATE TABLE readonly_probe_should_not_exist (n integer)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := check(t, tc.query)
			if res.Status != domain.StatusDown {
				t.Fatalf("a writing assertion query was accepted: %s", tc.query)
			}
			// PostgreSQL's own refusal, not some unrelated failure.
			if !strings.Contains(res.Message, "read-only transaction") {
				t.Fatalf("rejected for the wrong reason: %s", res.Message)
			}
		})
	}

	// The decisive assertion: the row is untouched. An error return would also
	// be produced by a syntax error or a lost connection, so proving the write
	// did not happen is what actually establishes the guarantee.
	var n int
	if err := admin.QueryRow(ctx, fmt.Sprintf(`SELECT n FROM %s`, table)).Scan(&n); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n != 1 {
		t.Fatalf("the probe wrote to the monitored database: n = %d, want 1", n)
	}
	var rows int
	if err := admin.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("row count = %d, want 1 — the probe inserted or deleted", rows)
	}
}
