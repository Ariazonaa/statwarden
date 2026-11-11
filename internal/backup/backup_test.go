package backup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeDumpEnv turns this test binary into a stand-in for pg_dump: re-executed
// with it set, the process prints what it was given — argv and the password
// environment — to stdout and exits, so a test can see exactly what Dump hands
// the real tool without needing one installed.
const fakeDumpEnv = "STATWARDEN_BACKUP_FAKE_PG_DUMP"

type fakeDumpRecord struct {
	Args       []string `json:"args"`
	PGPassword string   `json:"pgpassword"`
}

func TestMain(m *testing.M) {
	if os.Getenv(fakeDumpEnv) == "hang" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if os.Getenv(fakeDumpEnv) == "1" {
		_ = json.NewEncoder(os.Stdout).Encode(fakeDumpRecord{
			Args:       os.Args[1:],
			PGPassword: os.Getenv("PGPASSWORD"),
		})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeDumpRunner returns a Runner whose pg_dump is this test binary.
func fakeDumpRunner(t *testing.T, dsn string) (*Runner, string) {
	t.Helper()
	t.Setenv(fakeDumpEnv, "1")
	t.Setenv("PGPASSWORD", "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	return New(Options{Dir: dir, DatabaseURL: dsn, dumpCmd: exe,
		Now: func() time.Time { return at(2026, 8, 13, 12) }}), dir
}

func readFakeDump(t *testing.T, path string) fakeDumpRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var rec fakeDumpRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("the dump file does not hold what pg_dump wrote to stdout: %v (%q)", err, b)
	}
	return rec
}

// TestDumpKeepsThePasswordOutOfArgv pins that the database password reaches
// pg_dump through PGPASSWORD, never the command line. argv is world-readable
// in /proc/<pid>/cmdline for as long as the dump runs, so the URL that used to
// be passed verbatim handed the database password to every local user and to
// any process-listing in a support bundle.
func TestDumpKeepsThePasswordOutOfArgv(t *testing.T) {
	r, _ := fakeDumpRunner(t,
		"postgres://statwarden:s3cr%40t-pw@db.example.test:5432/statwarden?sslmode=disable&pool_max_conns=8")
	path, err := r.Dump(context.Background())
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	rec := readFakeDump(t, path)
	for _, a := range rec.Args {
		if strings.Contains(a, "s3cr") {
			t.Fatalf("the password is on pg_dump's command line: %q", rec.Args)
		}
		// libpq rejects URI parameters it does not know, so a pgxpool knob in
		// DATABASE_URL used to fail every dump.
		if strings.Contains(a, "pool_max_conns") {
			t.Fatalf("a pgxpool-only parameter reached pg_dump: %q", rec.Args)
		}
	}
	if rec.PGPassword != "s3cr@t-pw" {
		t.Fatalf("PGPASSWORD = %q, want the decoded password from DATABASE_URL", rec.PGPassword)
	}
	joined := strings.Join(rec.Args, " ")
	if !strings.Contains(joined, "statwarden@db.example.test:5432/statwarden") {
		t.Fatalf("the connection target is missing from argv: %q", rec.Args)
	}
}

// A keyword/value DSN carries its password as password=…; it must be lifted
// out the same way.
func TestSplitPasswordKeywordForm(t *testing.T) {
	conn, pw := splitPassword("host=db.example.test user=sw password='a b\\'c' dbname=app")
	if pw != "a b'c" {
		t.Errorf("password = %q, want %q", pw, "a b'c")
	}
	if strings.Contains(conn, "password") || !strings.Contains(conn, "dbname=app") {
		t.Errorf("connection string = %q, want it without the password and with the rest", conn)
	}
	conn, pw = splitPassword("host=db.example.test password=plain user=sw")
	if pw != "plain" || strings.Contains(conn, "plain") {
		t.Errorf("unquoted password: conn=%q pw=%q", conn, pw)
	}
}

// TestFirstPassRespectsTheNewestDump pins that a restart does not dump again.
//
// The first pass used to run unconditionally at startup, so an instance that
// restarted a few times a day — a crash loop, a host doing updates, an operator
// iterating on config — wrote a dump per restart. All of them sat inside
// BACKUP_MAX_AGE_DAYS, so pruning removed none and the volume grew by one full
// dump per restart. The schedule now continues from the newest finished dump.
func TestFirstPassRespectsTheNewestDump(t *testing.T) {
	now := at(2026, 8, 13, 12)
	cases := []struct {
		name  string
		dumps []time.Time
		want  time.Duration
	}{
		{"no dump yet runs at once", nil, 0},
		{"recent dump waits for the rest of the interval", []time.Time{now.Add(-2 * time.Hour), now.Add(-30 * time.Hour)}, 22 * time.Hour},
		{"overdue dump runs at once", []time.Time{now.Add(-30 * time.Hour)}, 0},
		// A clock that went backwards must not postpone the next dump by more
		// than one interval.
		{"dump from the future waits one interval at most", []time.Time{now.Add(5 * time.Hour)}, 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, d := range tc.dumps {
				touch(t, dir, d)
			}
			r := New(Options{Dir: dir, Interval: 24 * time.Hour, Now: func() time.Time { return now }})
			if got := r.firstDelay(); got != tc.want {
				t.Fatalf("firstDelay = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestStalePartialsAreRemoved pins the cleanup of dumps that never finished. A
// process killed mid-dump (OOM, SIGKILL after the stop timeout, power loss)
// leaves its .partial behind; nothing listed those, so each one stayed on the
// volume forever. One still being written — younger than the dump timeout — is
// left alone, and so is anything this job did not name.
func TestStalePartialsAreRemoved(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	stale := filepath.Join(dir, FilePrefix+"20260101T000000Z"+fileExt+partialExt)
	fresh := filepath.Join(dir, FilePrefix+"20260813T115900Z"+fileExt+partialExt)
	foreign := filepath.Join(dir, "notes.partial")
	for _, p := range []string{stale, fresh, foreign} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-3 * time.Hour)
	for _, p := range []string{stale, foreign} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	r := New(Options{Dir: dir, Interval: time.Hour, Now: func() time.Time { return now }})
	if n := r.removeStalePartials(); n != 1 {
		t.Fatalf("removed %d partial dumps, want 1", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("the stale partial dump survived")
	}
	for _, p := range []string{fresh, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed: %v", filepath.Base(p), err)
		}
	}
}

// TestDumpIsBoundedByATimeout pins that a pg_dump that never returns — a lock
// it waits on forever, a server that stopped answering mid-stream — cannot
// stall the job for the life of the process. It is killed, and it leaves no
// .partial behind.
func TestDumpIsBoundedByATimeout(t *testing.T) {
	r, dir := fakeDumpRunner(t, "postgres://sw@db.example.test/app")
	t.Setenv(fakeDumpEnv, "hang")
	r.timeout = 300 * time.Millisecond
	start := time.Now()
	if _, err := r.Dump(context.Background()); err == nil {
		t.Fatal("a hanging pg_dump reported success")
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Fatalf("Dump returned after %s; the timeout did not bound it", took)
	}
	if n := names(t, dir); len(n) != 0 {
		t.Fatalf("a timed-out dump left files behind: %v", n)
	}
}

// TestDumpFileIsPrivate pins the dump's mode. pg_dump creates its --file with
// the process umask, which is 0644 almost everywhere, and a dump holds every
// password hash, session and API-key hash in the database. The file is now
// created by this package, 0600, and pg_dump only writes into it.
func TestDumpFileIsPrivate(t *testing.T) {
	r, _ := fakeDumpRunner(t, "postgres://sw@db.example.test/app")
	path, err := r.Dump(context.Background())
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	readFakeDump(t, path) // the content went through the file this package opened
	if runtime.GOOS == "windows" {
		// Windows has no group/other permission bits to assert on; the content
		// check above is what this platform can verify.
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("dump mode = %04o, want no group or other access", perm)
	}
}

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
