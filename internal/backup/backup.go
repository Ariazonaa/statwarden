// Package backup takes scheduled logical dumps of the PostgreSQL database to a
// local directory and prunes old ones.
//
// It shells out to pg_dump rather than writing the dump itself. A hand-rolled
// export over COPY looks simple until it has to reproduce schema, sequences,
// constraints, extensions and ownership in a restorable order — which is the
// part a backup exists for, and the part that fails silently when it is wrong.
// pg_dump is the tool that already knows all of that, and pg_restore is the
// counterpart an operator will actually reach for.
//
// Scope is deliberately one destination: a directory. Uploads already live under
// a mounted volume (UPLOADS_DIR), so a sibling directory needs no new concept
// and no new dependency. Shipping to object storage would add an SDK and a
// credential surface; it is a separate decision, not a flag on this one.
//
// What this package does NOT claim: it is not a substitute for a managed
// database's own backups or for volume snapshots, and a dump nobody has ever
// restored is a guess. docs/deployment.md documents the restore, and an
// integration test performs one.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Ariazonaa/statwarden/internal/metrics"
	"github.com/Ariazonaa/statwarden/internal/safego"
)

// JobName is the label this job records its runs under in internal/metrics.
const JobName = "backup"

// FilePrefix and fileExt bracket a dump file's name. The timestamp between them
// is RFC 3339 basic format in UTC, which sorts lexicographically in the same
// order as chronologically — so pruning can order by name and never has to
// trust a filesystem mtime that a copy or a restore would have rewritten.
const (
	FilePrefix = "statwarden-"
	fileExt    = ".dump"
	// partialExt marks a dump still being written. The final name appears only
	// via rename, which is atomic within a directory, so a crashed or killed
	// dump leaves an obviously-unfinished file rather than a truncated one that
	// looks restorable.
	partialExt = ".partial"
	// timeLayout is the timestamp in a dump's file name.
	timeLayout = "20060102T150405Z"
)

// Options configures a Runner.
type Options struct {
	// DatabaseURL is the connection string handed to pg_dump.
	DatabaseURL string
	// Dir is the directory dumps are written to. Created if missing.
	Dir string
	// Interval is the time between dumps.
	Interval time.Duration
	// Keep is how many of the newest dumps are always retained, regardless of
	// age. It is a floor, not a target: without it, an instance left running
	// past MaxAge with a failing pg_dump would prune its way to zero backups
	// and report nothing unusual, because pruning succeeded.
	Keep int
	// MaxAge deletes dumps older than this. Zero disables age-based pruning and
	// leaves Keep as the only rule.
	MaxAge time.Duration
	// Logger is required in practice; nil falls back to slog.Default.
	Logger *slog.Logger
	// Now defaults to time.Now, and exists so tests can name files
	// deterministically.
	Now func() time.Time
	// dumpCmd is the pg_dump binary, overridable in tests.
	dumpCmd string
}

// Runner performs and prunes dumps.
type Runner struct {
	databaseURL string
	dir         string
	interval    time.Duration
	keep        int
	maxAge      time.Duration
	log         *slog.Logger
	now         func() time.Time
	dumpCmd     string
}

// New builds a Runner. It does not touch the filesystem.
func New(o Options) *Runner {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.dumpCmd == "" {
		o.dumpCmd = "pg_dump"
	}
	if o.Keep < 1 {
		o.Keep = 1
	}
	return &Runner{
		databaseURL: o.DatabaseURL,
		dir:         o.Dir,
		interval:    o.Interval,
		keep:        o.Keep,
		maxAge:      o.MaxAge,
		log:         o.Logger.With("job", JobName),
		now:         o.Now,
		dumpCmd:     o.dumpCmd,
	}
}

// ErrDumpToolMissing means pg_dump was not found on PATH.
var ErrDumpToolMissing = errors.New("backup: pg_dump not found on PATH")

// Preflight reports whether the runner can work at all: pg_dump must exist and
// the target directory must be creatable and writable.
//
// Called before the loop starts so the failure appears at deploy time, next to
// the change that caused it, rather than one interval later in a log nobody is
// reading.
func (r *Runner) Preflight() error {
	if _, err := exec.LookPath(r.dumpCmd); err != nil {
		return fmt.Errorf("%w (looked for %q)", ErrDumpToolMissing, r.dumpCmd)
	}
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		return fmt.Errorf("backup: create %s: %w", r.dir, err)
	}
	probe := filepath.Join(r.dir, ".writable")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return fmt.Errorf("backup: %s is not writable: %w", r.dir, err)
	}
	return os.Remove(probe)
}

// Start launches the backup loop and returns immediately.
//
// A failed preflight disables the job and is logged at ERROR — it does not stop
// the process. The trade-off is deliberate and worth stating: aborting startup
// would make a broken backup configuration take MONITORING down with it, and
// this product's job is to be the thing that still works when something else
// does not. The failure is instead made loud and observable — the error at
// startup, and no successful run ever recorded under the backup job in
// internal/metrics, which is what an alert should watch.
func (r *Runner) Start(ctx context.Context) {
	if err := r.Preflight(); err != nil {
		r.log.Error("backups are enabled but cannot run; no dumps will be taken", "error", err)
		metrics.ObserveJobRun(JobName, 0, 0, err)
		return
	}
	safego.Go(r.log, "backup job", func() { r.loop(ctx) })
}

func (r *Runner) loop(ctx context.Context) {
	r.log.Info("backup job started",
		"dir", r.dir, "interval", r.interval.String(), "keep", r.keep, "max_age", r.maxAge.String())

	// One pass immediately, matching the retention pruner and the rollup job: an
	// instance that restarts often would otherwise never reach its first dump.
	_ = safego.Do(r.log, "backup pass", func() { r.runOnce(ctx) })

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("backup job stopped")
			return
		case <-ticker.C:
			// Contained per ITERATION, not around the loop: safego.Go recovers a
			// panic and then lets the goroutine end, so wrapping the loop would
			// buy one log line and silently stop backing up for the life of the
			// process. TestPeriodicLoopsContainTheirIterations enforces this.
			_ = safego.Do(r.log, "backup pass", func() { r.runOnce(ctx) })
		}
	}
}

// runOnce takes one dump and prunes, reporting the pass to metrics either way.
func (r *Runner) runOnce(ctx context.Context) {
	start := time.Now()
	path, err := r.Dump(ctx)
	dur := time.Since(start)
	if err != nil {
		r.log.Error("backup failed", "error", err, "duration", dur.String())
		metrics.ObserveJobRun(JobName, 0, dur, err)
		return
	}
	size := int64(0)
	if fi, statErr := os.Stat(path); statErr == nil {
		size = fi.Size()
	}
	removed, pruneErr := r.Prune()
	if pruneErr != nil {
		// Reported, not fatal: the dump itself succeeded, and losing the prune
		// costs disk rather than data.
		r.log.Error("pruning old backups failed", "error", pruneErr)
	}
	r.log.Info("backup written",
		"file", filepath.Base(path), "bytes", size, "pruned", removed, "duration", dur.String())
	metrics.ObserveJobRun(JobName, 1, dur, nil)
}

// Dump writes one dump and returns its path.
//
// pg_dump's custom format (-Fc) rather than plain SQL: it is compressed already,
// so there is no second gzip step to get wrong, and it is the only format
// pg_restore can restore selectively and in parallel — which matters precisely
// when someone is restoring under pressure.
func (r *Runner) Dump(ctx context.Context) (string, error) {
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		return "", fmt.Errorf("backup: create %s: %w", r.dir, err)
	}
	name := FilePrefix + r.now().UTC().Format(timeLayout) + fileExt
	final := filepath.Join(r.dir, name)
	tmp := final + partialExt

	// --no-owner/--no-acl so the dump restores into a database owned by whoever
	// is restoring it, rather than demanding the original role exist. That is
	// the difference between a restore working on a fresh machine and failing on
	// a missing user at the worst possible moment.
	// argv form, never a shell: no argument is word-split or glob-expanded, so
	// neither the generated tmp path nor DATABASE_URL can introduce a second
	// command. dumpCmd itself is "pg_dump" unless a test overrides it.
	cmd := exec.CommandContext(ctx, r.dumpCmd, //nolint:gosec // G204: argv form, arguments come from config and a generated path
		"--format=custom", "--no-owner", "--no-acl", "--file="+tmp, r.databaseURL)
	// The URL carries the password, so it must not reach a log. Only stderr is
	// captured, and it is truncated: pg_dump can be verbose, and an error string
	// is a log line, not a file.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		if msg == "" {
			return "", fmt.Errorf("backup: pg_dump failed: %w", err)
		}
		return "", fmt.Errorf("backup: pg_dump failed: %w: %s", err, msg)
	}
	// Rename last: until this succeeds the file carries .partial and Prune and
	// any operator listing the directory can tell it apart from a usable dump.
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("backup: finalise %s: %w", final, err)
	}
	return final, nil
}

// Prune deletes dumps beyond the retention rules and returns how many it
// removed.
//
// Two rules, and the order matters: the newest Keep dumps are retained
// unconditionally, and only what is left over is considered for age. A pure
// age rule empties the directory the moment dumping breaks for longer than
// MaxAge — the one situation in which the existing backups are the only thing
// left. Keep is the floor that cannot be argued away.
func (r *Runner) Prune() (int, error) {
	files, err := r.list()
	if err != nil {
		return 0, err
	}
	// Newest first.
	sort.Slice(files, func(i, j int) bool { return files[i].stamp.After(files[j].stamp) })

	removed := 0
	cutoff := r.now().Add(-r.maxAge)
	for i, f := range files {
		if i < r.keep {
			continue // inside the floor
		}
		if r.maxAge <= 0 || !f.stamp.Before(cutoff) {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return removed, fmt.Errorf("backup: remove %s: %w", f.path, err)
		}
		removed++
	}
	return removed, nil
}

type dumpFile struct {
	path  string
	stamp time.Time
}

// list returns the finished dumps in the directory, ignoring .partial files and
// anything that does not carry a parseable timestamp — the directory is a
// mounted volume and may hold whatever an operator put there.
func (r *Runner) list() ([]dumpFile, error) {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("backup: read %s: %w", r.dir, err)
	}
	var out []dumpFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stamp, ok := parseStamp(e.Name())
		if !ok {
			continue
		}
		out = append(out, dumpFile{path: filepath.Join(r.dir, e.Name()), stamp: stamp})
	}
	return out, nil
}

// legacyFilePrefix is the dump prefix from before the project was renamed from
// Uptura. Dumps written under it are still recognised, so retention keeps
// pruning them instead of leaving them on the volume forever.
const legacyFilePrefix = "uptura-"

// parseStamp reads the timestamp out of a finished dump's file name.
func parseStamp(name string) (time.Time, bool) {
	if !strings.HasSuffix(name, fileExt) {
		return time.Time{}, false
	}
	var mid string
	switch {
	case strings.HasPrefix(name, FilePrefix):
		mid = strings.TrimPrefix(name, FilePrefix)
	case strings.HasPrefix(name, legacyFilePrefix):
		mid = strings.TrimPrefix(name, legacyFilePrefix)
	default:
		return time.Time{}, false
	}
	mid = strings.TrimSuffix(mid, fileExt)
	t, err := time.Parse(timeLayout, mid)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}
