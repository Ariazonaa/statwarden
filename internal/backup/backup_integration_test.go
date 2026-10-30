//go:build integration

// Live-PostgreSQL test for the backup job: it takes a real dump with pg_dump and
// RESTORES it into a fresh database with pg_restore, then checks the data came
// back.
//
// This is the only test that can say anything true about the feature. Everything
// in backup_test.go covers naming, pruning and atomicity — the bookkeeping around
// a dump — and none of it would notice if the dump itself were empty, truncated
// or unrestorable. A backup nobody has restored is a guess, and a test suite that
// only checks the guess is filed neatly is worse than none.
//
// Run: make test-integration
package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// requireTools skips loudly when pg_dump/pg_restore are absent. A skip rather
// than a failure because their absence is an environment problem, not a code
// one — but the message names exactly what went untested, so a green run with
// this line in it cannot be mistaken for a verified restore.
func requireTools(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("SKIPPING THE RESTORE TEST: %s is not on PATH, so the dump was never proved restorable", bin)
		}
	}
}

func baseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("DATABASE_URL")
	if u == "" {
		t.Fatal("DATABASE_URL is required for the integration suite (see `make test-integration`)")
	}
	return u
}

// withDatabase creates a throwaway database on the server named by DATABASE_URL
// and returns its DSN plus a drop function, so the test never touches whatever
// database the URL itself points at.
func withDatabase(t *testing.T, name string) (string, func()) {
	t.Helper()
	ctx := context.Background()
	base := baseURL(t)

	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)

	full := fmt.Sprintf("statwarden_backup_%s_%d", name, time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+full+`"`); err != nil {
		t.Fatalf("create database: %v", err)
	}
	dsn := replaceDBName(base, full)
	return dsn, func() {
		ctx := context.Background()
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		_, _ = c.Exec(ctx, `DROP DATABASE IF EXISTS "`+full+`" WITH (FORCE)`)
	}
}

// replaceDBName swaps the database name in a postgres:// URL.
func replaceDBName(url, name string) string {
	slash := strings.LastIndex(url, "/")
	if slash < 0 {
		return url
	}
	rest := ""
	if q := strings.Index(url[slash:], "?"); q >= 0 {
		rest = url[slash+q:]
	}
	return url[:slash+1] + name + rest
}

// TestDumpRestoresIntoAFreshDatabase is the whole point of the feature.
func TestDumpRestoresIntoAFreshDatabase(t *testing.T) {
	requireTools(t)
	ctx := context.Background()

	srcDSN, dropSrc := withDatabase(t, "src")
	defer dropSrc()

	src, err := pgx.Connect(ctx, srcDSN)
	if err != nil {
		t.Fatalf("connect source: %v", err)
	}
	// Deliberately more than one flat table: a sequence and a constraint are the
	// parts a hand-rolled COPY export gets wrong, and the reason this shells out
	// to pg_dump at all.
	if _, err := src.Exec(ctx, `
		CREATE TABLE widgets (
			id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			name  text NOT NULL UNIQUE,
			note  text
		);
		INSERT INTO widgets (name, note) VALUES ('alpha', 'first'), ('beta', NULL);`); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	src.Close(ctx)

	dir := t.TempDir()
	r := New(Options{DatabaseURL: srcDSN, Dir: dir, Keep: 1})
	path, err := r.Dump(ctx)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat dump: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("the dump is empty")
	}
	if strings.HasSuffix(path, partialExt) {
		t.Fatalf("a finished dump still carries the partial suffix: %s", path)
	}

	// Restore into a database that has never seen this schema.
	dstDSN, dropDst := withDatabase(t, "dst")
	defer dropDst()

	out, err := exec.CommandContext(ctx, "pg_restore",
		"--no-owner", "--no-acl", "--dbname="+dstDSN, path).CombinedOutput()
	if err != nil {
		t.Fatalf("pg_restore: %v\n%s", err, out)
	}

	dst, err := pgx.Connect(ctx, dstDSN)
	if err != nil {
		t.Fatalf("connect restored: %v", err)
	}
	defer dst.Close(ctx)

	var n int
	if err := dst.QueryRow(ctx, `SELECT count(*) FROM widgets`).Scan(&n); err != nil {
		t.Fatalf("the restored database has no widgets table: %v", err)
	}
	if n != 2 {
		t.Fatalf("restored row count = %d, want 2", n)
	}
	var note *string
	if err := dst.QueryRow(ctx, `SELECT note FROM widgets WHERE name = 'alpha'`).Scan(&note); err != nil {
		t.Fatalf("read restored row: %v", err)
	}
	if note == nil || *note != "first" {
		t.Fatalf("restored note = %v, want \"first\"", note)
	}
	// The identity sequence must have come across too, or the first insert after
	// a restore collides with an existing id — the classic half-restored
	// database that looks fine until it is written to.
	if _, err := dst.Exec(ctx, `INSERT INTO widgets (name) VALUES ('gamma')`); err != nil {
		t.Fatalf("insert after restore failed, the identity sequence did not survive: %v", err)
	}
}

// TestDumpFailsLoudlyOnABadURL pins that a broken connection is an error rather
// than an empty file that prunes a good backup out of the directory later.
func TestDumpFailsLoudlyOnABadURL(t *testing.T) {
	requireTools(t)
	dir := t.TempDir()
	r := New(Options{
		DatabaseURL: "postgres://nobody:nothing@127.0.0.1:1/nonexistent?sslmode=disable",
		Dir:         dir, Keep: 1,
	})
	if _, err := r.Dump(context.Background()); err == nil {
		t.Fatal("Dump reported success against an unreachable database")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed dump left files behind: %d", len(entries))
	}
}
