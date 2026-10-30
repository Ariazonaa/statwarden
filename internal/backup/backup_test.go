package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
}

// touch creates a finished dump file named for stamp.
func touch(t *testing.T, dir string, stamp time.Time) string {
	t.Helper()
	name := FilePrefix + stamp.UTC().Format(timeLayout) + fileExt
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestPruneKeepsTheFloorEvenWhenEverythingIsExpired is the rule that matters
// most, and the one whose failure is invisible until it is catastrophic.
//
// With age as the only rule, an instance whose pg_dump has been failing for
// longer than MaxAge prunes its way to ZERO backups and reports nothing wrong,
// because pruning itself succeeded. The dumps it deletes are, at that exact
// moment, the only copies that exist. Keep is a floor that age cannot argue
// away.
func TestPruneKeepsTheFloorEvenWhenEverythingIsExpired(t *testing.T) {
	dir := t.TempDir()
	now := at(2026, 8, 13, 12)
	// Five dumps, all far older than the 7-day window.
	for i := 1; i <= 5; i++ {
		touch(t, dir, now.AddDate(0, 0, -30*i))
	}
	r := New(Options{Dir: dir, Keep: 3, MaxAge: 7 * 24 * time.Hour, Now: func() time.Time { return now }})

	removed, err := r.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed %d, want 2", removed)
	}
	left := names(t, dir)
	if len(left) != 3 {
		t.Fatalf("kept %d files, want the 3 newest: %v", len(left), left)
	}
	// And they must be the NEWEST three, not any three.
	for _, n := range left {
		stamp, ok := parseStamp(n)
		if !ok {
			t.Fatalf("unparseable file survived: %s", n)
		}
		if stamp.Before(now.AddDate(0, 0, -95)) {
			t.Fatalf("an older dump was kept over a newer one: %s", n)
		}
	}
}

func TestPruneDeletesOnlyWhatIsBothSurplusAndExpired(t *testing.T) {
	dir := t.TempDir()
	now := at(2026, 8, 13, 12)
	fresh := touch(t, dir, now.Add(-time.Hour))
	alsoFresh := touch(t, dir, now.Add(-2*time.Hour))
	old := touch(t, dir, now.AddDate(0, 0, -30))

	r := New(Options{Dir: dir, Keep: 1, MaxAge: 7 * 24 * time.Hour, Now: func() time.Time { return now }})
	removed, err := r.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1 (only the expired surplus)", removed)
	}
	for _, p := range []string{fresh, alsoFresh} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("a dump inside the age window was deleted: %s", filepath.Base(p))
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("the expired surplus dump survived: %v", err)
	}
}

func TestPruneWithoutMaxAgeKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	now := at(2026, 8, 13, 12)
	for i := 1; i <= 4; i++ {
		touch(t, dir, now.AddDate(-i, 0, 0))
	}
	r := New(Options{Dir: dir, Keep: 1, MaxAge: 0, Now: func() time.Time { return now }})
	removed, err := r.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 0 || len(names(t, dir)) != 4 {
		t.Fatalf("MaxAge=0 must disable age pruning; removed=%d left=%d", removed, len(names(t, dir)))
	}
}

// TestPruneIgnoresForeignFiles pins that the directory is treated as a mounted
// volume that may hold things this job did not write — including its own
// in-progress dumps.
func TestPruneIgnoresForeignFiles(t *testing.T) {
	dir := t.TempDir()
	now := at(2026, 8, 13, 12)
	touch(t, dir, now.AddDate(0, 0, -365))
	touch(t, dir, now)

	foreign := []string{
		"notes.txt",
		"statwarden-nonsense.dump",
		FilePrefix + now.AddDate(0, 0, -400).UTC().Format(timeLayout) + fileExt + partialExt,
	}
	for _, n := range foreign {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	r := New(Options{Dir: dir, Keep: 1, MaxAge: time.Hour, Now: func() time.Time { return now }})
	if _, err := r.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, n := range foreign {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("prune deleted a file it does not own: %s", n)
		}
	}
}

// TestKeepIsAtLeastOne pins that a misconfigured floor cannot become "delete
// everything". Keep=0 with an age rule would otherwise empty the directory.
func TestKeepIsAtLeastOne(t *testing.T) {
	dir := t.TempDir()
	now := at(2026, 8, 13, 12)
	touch(t, dir, now.AddDate(-1, 0, 0))

	r := New(Options{Dir: dir, Keep: 0, MaxAge: time.Hour, Now: func() time.Time { return now }})
	if _, err := r.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(names(t, dir)) != 1 {
		t.Fatal("Keep=0 pruned the last remaining backup")
	}
}

func TestParseStamp(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{FilePrefix + "20260813T120000Z" + fileExt, true},
		{FilePrefix + "20260813T120000Z" + fileExt + partialExt, false},
		{FilePrefix + "not-a-time" + fileExt, false},
		{"other-20260813T120000Z" + fileExt, false},
		{FilePrefix + "20260813T120000Z.sql", false},
		{"", false},
		// Written before the rename from Uptura. Still a dump of this
		// instance, so retention must still see and prune it.
		{legacyFilePrefix + "20260813T120000Z" + fileExt, true},
		{legacyFilePrefix + "20260813T120000Z" + fileExt + partialExt, false},
	}
	for _, c := range cases {
		if _, ok := parseStamp(c.name); ok != c.ok {
			t.Errorf("parseStamp(%q) ok=%v, want %v", c.name, ok, c.ok)
		}
	}
}

// TestPreflightReportsAMissingTool pins the startup check. The point is the
// SENTINEL: Start logs and disables the job rather than aborting the process,
// so a caller that wants to treat it differently needs to be able to tell this
// apart from a permissions problem.
func TestPreflightReportsAMissingTool(t *testing.T) {
	r := New(Options{Dir: t.TempDir(), dumpCmd: "definitely-not-a-real-binary-xyzzy"})
	err := r.Preflight()
	if !errors.Is(err, ErrDumpToolMissing) {
		t.Fatalf("Preflight = %v, want ErrDumpToolMissing", err)
	}
}

// TestPreflightCreatesTheDirectory covers the ordinary first start, where the
// mounted volume exists but the backup subdirectory does not.
func TestPreflightCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "backups")
	// A tool that exists on every platform the tests run on.
	tool := "go"
	r := New(Options{Dir: dir, dumpCmd: tool})
	if err := r.Preflight(); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	// The writability probe must not be left behind.
	if n := names(t, dir); len(n) != 0 {
		t.Fatalf("Preflight left files behind: %v", n)
	}
}

// TestDumpLeavesNoPartialOnFailure pins the atomicity contract: a failed dump
// must not leave anything that could be mistaken for a restorable backup, and
// must not leave the .partial behind either.
func TestDumpLeavesNoPartialOnFailure(t *testing.T) {
	dir := t.TempDir()
	// `go env --nope` exits non-zero without writing the --file target.
	r := New(Options{Dir: dir, DatabaseURL: "postgres://x/y", dumpCmd: "go",
		Now: func() time.Time { return at(2026, 8, 13, 12) }})
	// Point at a command that fails: New keeps dumpCmd, and pg_dump's flags are
	// nonsense to `go`, so it exits non-zero.
	if _, err := r.Dump(context.Background()); err == nil {
		t.Fatal("Dump reported success for a failing command")
	}
	if n := names(t, dir); len(n) != 0 {
		t.Fatalf("a failed dump left files behind: %v", n)
	}
}
