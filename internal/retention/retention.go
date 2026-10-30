// Package retention runs the scheduled heartbeat-pruning job that enforces the
// configured HEARTBEAT_RETENTION_DAYS window.
//
// The job is deliberately self-contained: it depends only on a small Store
// interface, so the scheduling logic is unit-tested against an in-memory fake
// with an injectable clock and never needs a live database. The production
// implementation lives in pgstore.go and is a thin wrapper over the
// sqlc-generated DeleteHeartbeatsBefore query.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/Ariazonaa/statwarden/internal/config"
	"github.com/Ariazonaa/statwarden/internal/metrics"

	"github.com/Ariazonaa/statwarden/internal/safego"
)

// Store is the persistence dependency of the pruner: delete every row older
// than cutoff and report how many were removed.
type Store interface {
	// PruneBefore deletes heartbeats recorded strictly before cutoff.
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// PruneDeliveriesBefore deletes notification-delivery rows finished
	// strictly before cutoff.
	//
	// A separate window from the heartbeats', because the two tables answer
	// different questions. Heartbeats are samples and there are millions of
	// them; deliveries are events, they arrive only on a state change, and they
	// are the record of who was told what — the thing an operator reads after
	// an incident.
	//
	// Implementations MUST keep the rows belonging to a monitor that is currently
	// down, regardless of the cutoff: notify.ChannelsAwaitingRecovery reads them to
	// decide who is owed a recovery alert, so pruning them turns a resolved outage
	// into one that is never announced. See PgStore.PruneDeliveriesBefore.
	PruneDeliveriesBefore(ctx context.Context, cutoff time.Time) (int64, error)
	// PruneBin hard-deletes monitors that have been in the recycle bin for
	// longer than olderThanDays, taking their heartbeats with them through the
	// schema's cascade. It returns what it destroyed.
	//
	// The bin (migration 0018) is what makes an accidental delete — especially
	// inside a bulk action — undoable. A grace period is what stops it becoming
	// an archive nobody empties: the whole point of deleting a monitor with three
	// years of 60-second heartbeats is to get that space back eventually.
	//
	// Unlike the other two passes this takes a WINDOW, not a cutoff instant. The
	// other two delete rows that can be re-recorded or that nobody reads; this
	// one is irreversible, and computing its cutoff in Go means computing it
	// against the APPLICATION's clock. Those are two different clocks — a
	// container whose time drifts, or a host whose NTP steps it forward, hands
	// Postgres a cutoff from the future and empties a bin that was supposed to
	// hold another fortnight. deleted_at is written by the database; the window
	// it is measured against belongs to the database too.
	//
	// Implementations MUST touch only rows with a non-NULL deleted_at. A sweep
	// that could reach a live monitor would be the single most destructive
	// statement in the product.
	PruneBin(ctx context.Context, olderThanDays int) ([]PrunedMonitor, error)
}

// PrunedMonitor is one monitor the bin sweep destroyed.
//
// The name is carried out of the DELETE for the log line. Everything else about
// the monitor is gone by then, and an id on its own tells an operator asking
// "what did it take?" precisely nothing.
type PrunedMonitor struct {
	ID   int64
	Name string
}

// DefaultInterval is how often the pruner runs when Options.Interval is unset.
// Retention is a coarse, day-granularity policy, so a few passes per day are
// plenty while still bounding how long expired rows linger after a long
// uptime.
const DefaultInterval = 6 * time.Hour

// Options configures a Pruner.
type Options struct {
	// Store performs the actual deletes. Required.
	Store Store
	// RetentionDays is the DEFAULT age threshold in days; heartbeats older than
	// this are deleted. Values <= 0 disable pruning entirely (the loop still runs
	// but does nothing), guarding against an accidental "delete everything". It
	// mirrors HEARTBEAT_RETENTION_DAYS and is the fallback when DaysFunc is unset
	// or yields no runtime override.
	RetentionDays int
	// TrashRetentionDays is how long a soft-deleted monitor stays in the recycle
	// bin before it is hard-deleted. Values <= 0 disable the sweep, so the bin
	// keeps everything until someone empties it by hand — which is a legitimate
	// choice on an instance with room to spare, and a deliberate one rather than
	// an accident, because the default is finite.
	TrashRetentionDays int
	// DeliveryRetentionDays is the age threshold for the notification delivery
	// log. Values <= 0 disable delivery pruning while leaving the heartbeat
	// pass running, so the two policies can be set independently. It mirrors
	// DELIVERY_RETENTION_DAYS and has no runtime override: nothing edits it in
	// Settings, and an audit-shaped window is not something to change casually
	// from a web form.
	DeliveryRetentionDays int
	// DaysFunc, when set, supplies the retention window at RUN TIME so an admin
	// can change it in Settings without a restart. It is consulted on every pass
	// and its result is clamped to [config.MinHeartbeatRetentionDays,
	// config.MaxHeartbeatRetentionDays]; a value <= 0 means "no override, use
	// RetentionDays". Wire it to read the "retention_days" setting.
	//
	// An ERROR means the override could not be READ, which is not the same as
	// "there is no override". Falling back to the env default on a failed read
	// deletes everything between the two windows — and the whole purpose of the
	// control is to run a LONGER one, so the fallback is destructive by
	// construction. The pass is skipped instead.
	DaysFunc func(context.Context) (int, error)
	// Interval between prune passes. Defaults to DefaultInterval.
	Interval time.Duration
	// Logger receives structured job logs. Defaults to slog.Default().
	Logger *slog.Logger
	// Now supplies the current time; injectable for deterministic tests.
	// Defaults to time.Now.
	Now func() time.Time
}

// Pruner periodically deletes heartbeats older than the retention window.
type Pruner struct {
	store         Store
	retentionDays int
	deliveryDays  int
	trashDays     int
	daysFunc      func(context.Context) (int, error)
	interval      time.Duration
	log           *slog.Logger
	now           func() time.Time
}

// New builds a Pruner from Options, applying defaults for the optional fields.
func New(o Options) *Pruner {
	if o.Interval <= 0 {
		o.Interval = DefaultInterval
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Pruner{
		store:         o.Store,
		retentionDays: o.RetentionDays,
		deliveryDays:  o.DeliveryRetentionDays,
		trashDays:     o.TrashRetentionDays,
		daysFunc:      o.DaysFunc,
		interval:      o.Interval,
		log:           o.Logger.With("job", "retention"),
		now:           o.Now,
	}
}

// Start launches the pruning loop in a background goroutine and returns
// immediately. It runs one pass right away (so a long-lived instance does not
// wait a full interval before its first cleanup), then once per interval, and
// exits cleanly when ctx is cancelled — matching the app's graceful-shutdown
// model.
func (p *Pruner) Start(ctx context.Context) {
	// A runtime source can raise the window above zero even when the static
	// default is off, so the loop must start whenever either can enable pruning.
	//
	// The delivery and recycle-bin windows count too: each is an independent
	// policy on its own timer-less schedule, and gating the whole loop on the
	// heartbeat window alone meant an operator who disabled that one silently
	// disabled the other two as well.
	if p.retentionDays <= 0 && p.daysFunc == nil && p.deliveryDays <= 0 && p.trashDays <= 0 {
		p.log.Info("every retention window is disabled; pruning loop not started")
		return
	}
	safego.Go(p.log, "retention job", func() { p.loop(ctx) })
}

func (p *Pruner) loop(ctx context.Context) {
	p.log.Info("heartbeat retention job started",
		"retention_days", p.retentionDays, "interval", p.interval.String())

	// First pass immediately, then on every tick.
	_ = safego.Do(p.log, "retention pass", func() { p.runOnce(ctx) })

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.log.Info("heartbeat retention job stopped")
			return
		case <-ticker.C:
			_ = safego.Do(p.log, "retention pass", func() { p.runOnce(ctx) })
		}
	}
}

// runOnce executes a single prune pass over all three tables, records the job
// metrics and logs the outcome. Errors are logged but never fatal: a transient
// DB hiccup must not take the process down, and the next tick simply tries
// again. The heartbeat pass reports itself to internal/metrics, success or
// failure, so an operator can see the job is alive, how long it took and how
// much it removed.
//
// The three passes are independent, and that independence runs BOTH ways. The
// heartbeat pass used to return early on error, which took the delivery pass
// with it — so the statement most likely to fail decided whether the other one
// ran at all. PruneBefore is a single unbounded DELETE over the largest table in
// the schema, so a statement timeout or lock contention hits it long before it
// hits the delivery log; the result was a delivery log that grew without bound
// for as long as the heartbeat delete kept failing, with nothing in the log
// saying so.
//
// Each pass gets its OWN recover, not just its own error handling. The caller
// wraps runOnce in one safego.Do, which recovers above this function — so a
// panic anywhere in the first pass returned from all three, and the same
// independence that errors have been given since would have been undone by the
// one failure mode nobody writes a test for. A nil map, an unexpected scan
// type: whatever it is, it belongs to one table.
func (p *Pruner) runOnce(ctx context.Context) {
	_ = safego.Do(p.log, "heartbeat prune", func() { p.pruneHeartbeats(ctx) })
	_ = safego.Do(p.log, "delivery log prune", func() { p.pruneDeliveries(ctx) })
	_ = safego.Do(p.log, "recycle bin prune", func() { p.pruneBin(ctx) })
}

// pruneHeartbeats trims the heartbeat table and reports the pass to metrics.
func (p *Pruner) pruneHeartbeats(ctx context.Context) {
	// time.Now (not p.now) times the pass: p.now is the injectable clock the
	// cutoff is derived from, which a test may pin, but the duration wants real
	// monotonic elapsed time.
	start := time.Now()
	days, err := p.resolveDays(ctx)
	if err != nil {
		// Skipped, not guessed. Deleting on a window we could not confirm is the
		// one outcome no later pass can undo.
		p.log.Error("heartbeat prune skipped: the retention override could not be read",
			"error", err)
		metrics.ObserveJobRun(metrics.JobRetention, 0, time.Since(start), err)
		return
	}
	deleted, err := p.pruneDays(ctx, days)
	dur := time.Since(start)

	metrics.ObserveJobRun(metrics.JobRetention, int(deleted), dur, err)

	if err != nil {
		p.log.Error("heartbeat prune failed",
			"error", err, "retention_days", days, "duration_ms", dur.Milliseconds())
		return
	}
	// One structured line per run. A run that removed nothing is routine, so it
	// stays at debug; a run that deleted rows is worth an info line.
	lvl := slog.LevelDebug
	if deleted > 0 {
		lvl = slog.LevelInfo
	}
	p.log.Log(ctx, lvl, "heartbeat prune complete",
		"deleted", deleted, "retention_days", days, "duration_ms", dur.Milliseconds())
}

// pruneBin empties the monitor recycle bin of anything binned longer ago than
// the grace period.
//
// Logged at INFO whenever it removes something, and it NAMES what it removed.
// The other two passes delete rows that nobody misses and that the system will
// produce again; this one destroys a monitor's entire history, irreversibly, on
// a timer. The log line is the only remaining trace, so it has to carry enough
// to answer "what was that?", and a count alone does not.
func (p *Pruner) pruneBin(ctx context.Context) {
	if p.trashDays <= 0 {
		return
	}
	start := time.Now()
	// A window, not an instant: the cutoff is computed by the database, on the
	// clock that wrote deleted_at. See Store.PruneBin.
	pruned, err := p.store.PruneBin(ctx, p.trashDays)
	dur := time.Since(start)
	if err != nil {
		p.log.Error("recycle bin prune failed",
			"error", err, "retention_days", p.trashDays, "duration_ms", dur.Milliseconds())
		return
	}
	if len(pruned) == 0 {
		p.log.Debug("recycle bin prune complete",
			"deleted", 0, "retention_days", p.trashDays, "duration_ms", dur.Milliseconds())
		return
	}
	// One line per monitor, not one line carrying a list: an operator greps this
	// log for a name, and a slice attribute renders differently in every handler.
	for _, m := range pruned {
		p.log.Info("recycle bin: monitor destroyed permanently",
			"monitor_id", m.ID, "monitor_name", m.Name, "retention_days", p.trashDays)
	}
	p.log.Info("recycle bin prune removed monitors permanently",
		"deleted", len(pruned), "retention_days", p.trashDays, "duration_ms", dur.Milliseconds())
}

// pruneDeliveries trims the notification delivery log to its own window.
//
// It runs after the heartbeat pass rather than beside it, but neither pass can
// stop the other: the two tables are unrelated, a delivery-log problem must not
// stop heartbeats being pruned, and a failing heartbeat delete must not leave
// the delivery log unbounded. A window of zero leaves the log alone entirely.
//
// The log is not merely housekeeping: ChannelsAwaitingRecovery reads it once per
// open outage on the startup path (see migration 0015), so an unbounded log is
// felt as a slow restart.
func (p *Pruner) pruneDeliveries(ctx context.Context) {
	if p.deliveryDays <= 0 {
		return
	}
	start := time.Now()
	cutoff := p.now().UTC().AddDate(0, 0, -p.deliveryDays)
	deleted, err := p.store.PruneDeliveriesBefore(ctx, cutoff)
	dur := time.Since(start)
	if err != nil {
		p.log.Error("delivery log prune failed",
			"error", err, "retention_days", p.deliveryDays, "duration_ms", dur.Milliseconds())
		return
	}
	lvl := slog.LevelDebug
	if deleted > 0 {
		lvl = slog.LevelInfo
	}
	p.log.Log(ctx, lvl, "delivery log prune complete",
		"deleted", deleted, "retention_days", p.deliveryDays, "duration_ms", dur.Milliseconds())
}

// Prune performs one prune pass and returns the number of heartbeats deleted.
// It is exported so it can be triggered directly (e.g. from a future admin
// endpoint or CLI) and asserted on in tests. With the effective window <= 0 it
// is a no-op returning (0, nil).
func (p *Pruner) Prune(ctx context.Context) (int64, error) {
	days, err := p.resolveDays(ctx)
	if err != nil {
		return 0, err
	}
	return p.pruneDays(ctx, days)
}

// pruneDays deletes heartbeats older than the given window. days <= 0 is a
// no-op, so an unset/disabled retention never turns into "delete everything".
func (p *Pruner) pruneDays(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := p.now().UTC().AddDate(0, 0, -days)
	return p.store.PruneBefore(ctx, cutoff)
}

// resolveDays picks the retention window for a pass: the runtime override from
// DaysFunc when it supplies a positive value (clamped to sane bounds), otherwise
// the static RetentionDays default.
func (p *Pruner) resolveDays(ctx context.Context) (int, error) {
	if p.daysFunc != nil {
		d, err := p.daysFunc(ctx)
		if err != nil {
			return 0, err
		}
		if d > 0 {
			return clampDays(d), nil
		}
	}
	return p.retentionDays, nil
}

// clampDays holds a runtime retention value inside [Min, Max]. The write path in
// Settings validates the same bounds, but the settings table is also writable
// directly in psql, so the pruner clamps again rather than trusting the row.
func clampDays(d int) int {
	if d < config.MinHeartbeatRetentionDays {
		return config.MinHeartbeatRetentionDays
	}
	if d > config.MaxHeartbeatRetentionDays {
		return config.MaxHeartbeatRetentionDays
	}
	return d
}
