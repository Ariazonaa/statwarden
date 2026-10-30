// Package push implements the server side of push (heartbeat) monitors: a
// Recorder that turns an incoming push into a heartbeat and a Watchdog that
// marks a push monitor DOWN when its next heartbeat fails to arrive within the
// configured interval + grace. Both work against a small Store interface so
// they are testable without a live database.
package push

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/types/pushcheck"
	"github.com/Ariazonaa/statwarden/internal/safego"
)

// ErrNotFound is returned by Store.MonitorByPushToken when no monitor carries
// the token. The API layer maps it to 404.
var ErrNotFound = errors.New("push: monitor not found")

// PushMonitor is a passive monitor plus the time of its most recent heartbeat,
// as the watchdog needs it.
type PushMonitor struct {
	Monitor  domain.Monitor
	LastSeen time.Time
	// HasHeartbeat is false when the monitor has never checked in; the
	// watchdog then measures the deadline from the monitor's creation is
	// unknown here, so a never-seen monitor is left untouched until its first
	// heartbeat (matches Uptime-Kuma semantics).
	HasHeartbeat bool
}

// Store is the persistence surface push needs. PgStore implements it against
// PostgreSQL; MemStore backs tests.
type Store interface {
	// MonitorByPushToken resolves a push token to its monitor (including
	// current status and config). It resolves the token only for an active
	// push monitor; a paused monitor, a non-push monitor, or an unknown token
	// all return ErrNotFound, so recording is restricted to type=push && active.
	MonitorByPushToken(ctx context.Context, token string) (domain.Monitor, error)
	// SaveHeartbeat appends one heartbeat. It is used to seed data; the state
	// transitions go through ApplyBeat / TripDown so status stays consistent
	// with the latest heartbeat.
	SaveHeartbeat(ctx context.Context, hb domain.Heartbeat) error
	// UpdateMonitorStatus writes the denormalised status column.
	UpdateMonitorStatus(ctx context.Context, monitorID int64, status domain.MonitorStatus) error
	// ApplyBeat atomically appends hb and reconciles the monitor's status column
	// to hb.Status under a row lock, returning the status the monitor held
	// before. The stored heartbeat's Important flag is set under the same lock
	// to reflect whether this beat changed the status. Doing both writes in one
	// transaction keeps a concurrent watchdog sweep from interleaving between the
	// heartbeat and the status write.
	ApplyBeat(ctx context.Context, hb domain.Heartbeat) (domain.MonitorStatus, error)
	// TripDown atomically records the DOWN heartbeat hb and marks the monitor
	// DOWN, but only when no heartbeat newer than notAfter exists for it — i.e.
	// no push overtook the watchdog between its snapshot read and this write. It
	// reports whether it actually tripped. The staleness check and the writes
	// share one row lock, so a fresh push can never be clobbered to DOWN.
	TripDown(ctx context.Context, hb domain.Heartbeat, notAfter time.Time) (bool, error)
	// ListPushMonitors returns every active push monitor with its last-seen
	// time, for the watchdog sweep.
	ListPushMonitors(ctx context.Context) ([]PushMonitor, error)
}

// Beat is the payload an incoming push carries (all optional).
type Beat struct {
	// Status is "up" or "down"; anything else (or empty) is treated as up.
	Status string
	// Ping is the reported round-trip/latency in milliseconds; nil when the
	// caller did not send one.
	Ping *int
	// Message is a free-text status note ("backup ok, 2.3 GB in 84 s").
	Message string
}

// Suppressor evaluates the alert-suppression rules for a monitor — an active
// maintenance window, or a parent that is currently DOWN. *engine.Engine
// satisfies it through its exported Suppression method.
//
// Push monitors are fed by incoming heartbeats rather than by the scheduler, so
// they never pass through the engine's own emit path where suppression is
// applied. Without this hook a push monitor's DOWN, recovery and watchdog
// alerts go out during maintenance and while its parent is down, which is
// exactly what a maintenance window exists to prevent. Nil means nothing is
// ever suppressed.
type Suppressor interface {
	Suppression(m domain.Monitor, at time.Time) (suppressed bool, reason string)
}

// Recorder turns incoming pushes into heartbeats and drives state transitions.
type Recorder struct {
	store    Store
	clock    func() time.Time
	onEvent  func(domain.StateChange)
	suppress Suppressor
	log      *slog.Logger
}

// Options configures a Recorder / Watchdog.
type Options struct {
	Store Store
	// Clock defaults to time.Now.
	Clock func() time.Time
	// OnEvent receives state transitions (fed to the notification dispatcher
	// and the live dashboard, exactly like the engine's OnEvent). May be nil.
	OnEvent func(domain.StateChange)
	// Suppress applies the same alert-suppression rules the scheduler applies.
	// May be nil (nothing is ever suppressed).
	Suppress Suppressor
	Logger   *slog.Logger
}

// NewRecorder builds a Recorder.
func NewRecorder(o Options) *Recorder {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Recorder{
		store: o.Store, clock: o.Clock, onEvent: o.OnEvent,
		suppress: o.Suppress, log: o.Logger,
	}
}

// applySuppression stamps the suppression verdict onto an outgoing event. It
// mirrors engine.emit: the event is still delivered to the sink (heartbeats and
// dashboards must stay truthful) — only the ALERT is marked suppressed.
func applySuppression(s Suppressor, m domain.Monitor, ev *domain.StateChange) {
	if s == nil {
		return
	}
	ev.Suppressed, ev.SuppressReason = s.Suppression(m, ev.At)
}

// Record processes one incoming push identified by token. It records a
// heartbeat and, when the reported status differs from the stored one, updates
// the monitor status and emits a StateChange. It returns the affected monitor.
func (r *Recorder) Record(ctx context.Context, token string, beat Beat) (domain.Monitor, error) {
	m, err := r.store.MonitorByPushToken(ctx, token)
	if err != nil {
		return domain.Monitor{}, err
	}
	now := r.clock()

	status := domain.StatusUp
	if beat.Status == string(domain.StatusDown) {
		status = domain.StatusDown
	}
	if m.UpsideDown {
		if status == domain.StatusUp {
			status = domain.StatusDown
		} else {
			status = domain.StatusUp
		}
	}

	var latency *time.Duration
	if beat.Ping != nil {
		d := time.Duration(*beat.Ping) * time.Millisecond
		latency = &d
	}
	hb := domain.Heartbeat{
		MonitorID: m.ID,
		CheckedAt: now,
		Status:    status,
		Latency:   latency,
		Message:   beat.Message,
	}
	// ApplyBeat records the heartbeat and reconciles the status column in one
	// transaction, so the watchdog cannot slip a DOWN between the two writes.
	// It reports the status held before, which is what a real transition is
	// measured against.
	prev, err := r.store.ApplyBeat(ctx, hb)
	if err != nil {
		return domain.Monitor{}, err
	}
	if prev != status {
		r.emit(m, prev, status, beat.Message, now)
	}
	m.Status = status
	return m, nil
}

func (r *Recorder) emit(m domain.Monitor, from, to domain.MonitorStatus, msg string, at time.Time) {
	if r.onEvent == nil {
		return
	}
	ev := domain.StateChange{
		MonitorID:   m.ID,
		MonitorName: m.Name,
		From:        from,
		To:          to,
		At:          at,
		Message:     msg,
	}
	applySuppression(r.suppress, m, &ev)
	r.onEvent(ev)
}

// Watchdog periodically trips overdue push monitors DOWN.
type Watchdog struct {
	store    Store
	clock    func() time.Time
	onEvent  func(domain.StateChange)
	suppress Suppressor
	log      *slog.Logger
}

// NewWatchdog builds a Watchdog from the same Options as a Recorder.
func NewWatchdog(o Options) *Watchdog {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Watchdog{
		store: o.Store, clock: o.Clock, onEvent: o.OnEvent,
		suppress: o.Suppress, log: o.Logger,
	}
}

// Sweep marks every overdue push monitor DOWN once. It returns the number of
// monitors it tripped. A monitor already DOWN, or one that has never checked
// in, is left untouched.
func (w *Watchdog) Sweep(ctx context.Context) (int, error) {
	monitors, err := w.store.ListPushMonitors(ctx)
	if err != nil {
		return 0, err
	}
	now := w.clock()
	tripped := 0
	for _, pm := range monitors {
		if !pm.HasHeartbeat || pm.Monitor.Status == domain.StatusDown {
			continue
		}
		deadline, ok := pushcheck.Deadline(pm.Monitor.Config, pm.LastSeen)
		if !ok || !now.After(deadline) {
			continue
		}
		hb := domain.Heartbeat{
			MonitorID: pm.Monitor.ID,
			CheckedAt: now,
			Status:    domain.StatusDown,
			Message:   "no heartbeat received within expected interval + grace period",
			Important: true,
		}
		// TripDown records the DOWN heartbeat and status atomically, but only
		// when no push has arrived since this monitor's snapshot (pm.LastSeen).
		// A fresh push that raced in between the snapshot and now returns
		// changed=false and is left as-is, rather than being clobbered to DOWN.
		changed, err := w.store.TripDown(ctx, hb, pm.LastSeen)
		if err != nil {
			w.log.Error("push watchdog: trip down", "monitor_id", pm.Monitor.ID, "error", err)
			continue
		}
		if !changed {
			continue
		}
		if w.onEvent != nil {
			ev := domain.StateChange{
				MonitorID:   pm.Monitor.ID,
				MonitorName: pm.Monitor.Name,
				From:        pm.Monitor.Status,
				To:          domain.StatusDown,
				At:          now,
				Message:     hb.Message,
			}
			// The monitor is still tripped DOWN in the database above — the
			// suppression only silences the ALERT, exactly as it does for a
			// scheduled check. Data stays truthful, the pager stays quiet.
			applySuppression(w.suppress, pm.Monitor, &ev)
			w.onEvent(ev)
		}
		tripped++
	}
	return tripped, nil
}

// Run sweeps on every tick of interval until ctx is cancelled.
//
// The first sweep waits a full interval, deliberately, and unlike the rollup
// job, the certificate sweeper, the outbox worker and the retention pruner —
// which all run once immediately. This is the one periodic job that RAISES
// ALARMS, and at startup every push monitor whose deadline lapsed while the
// process was down looks overdue at once. Sweeping instantly would trip that
// whole set DOWN and page for all of them before any agent had a chance to
// report in. The interval is a grace window, not an oversight; an agent that
// checks in during it is spared, because TripDown re-reads the last heartbeat
// under the row lock and declines to trip a monitor that has since been seen.
//
// Each sweep is contained individually, not the loop around it. The caller wraps
// this whole function in safego.Go, which recovers a panic and then lets the
// goroutine END — so a single bad monitor config used to disable the watchdog for
// the life of the process. That is the quietest failure this subsystem has: the
// watchdog is the ONLY thing that trips a passive monitor DOWN, so a backup job
// that stops checking in would be reported UP forever, with one log line an hour
// earlier as the sole evidence. Sweep parses per-monitor JSON config
// (pushcheck.Deadline), which is exactly the kind of input that panics.
func (w *Watchdog) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = safego.Do(w.log, "push watchdog sweep", func() {
				if _, err := w.Sweep(ctx); err != nil {
					w.log.Error("push watchdog sweep failed", "error", err)
				}
			})
		}
	}
}

// GenerateToken returns a new random, URL-safe push token.
func GenerateToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// HashToken returns the hex-encoded SHA-256 of a push token. The token is a
// heartbeat credential, so it is treated like a password: only this hash is
// persisted (in monitors.push_token_hash), and the raw token is revealed once
// at mint time and never stored. A database dump therefore hands out no working
// tokens. A plain SHA-256 is the right primitive here, not a slow password KDF:
// the token is a 128-bit random value, so it is not brute-forceable, and the
// recorder must resolve an incoming token to its monitor with a single indexed
// lookup on every heartbeat. This is the same construction the auth package
// uses for session and API-key secrets.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
