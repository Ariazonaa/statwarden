// Package engine implements the monitor scheduler: drift-corrected per-monitor
// timers, a bounded worker pool, the UP → PENDING → DOWN → UP state machine,
// alert-suppression hooks and live configuration reload.
//
// The engine owns one job per monitor. A job's timer is only ever re-armed
// after its check completes, so the same monitor never runs twice
// concurrently; the next fire time is computed from the previous scheduled
// fire time, so check duration does not drift the interval. Raw heartbeats
// are always recorded; state transitions additionally update the monitor row
// and emit a StateChange event (suppressed while the parent is DOWN or a
// maintenance window is active — suppression affects alerts, never data).
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/anomaly"
	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/monitor/registry"
	"github.com/Ariazonaa/statwarden/internal/safego"
)

// Defaults used when Options leave the corresponding field zero.
const (
	// DefaultTimeout bounds a single check when neither the monitor nor
	// its checker type specifies one.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxStartupJitter caps the random delay before a monitor's
	// first check, spreading load after startup ("thundering herd").
	DefaultMaxStartupJitter = 10 * time.Second
	// persistTimeout bounds each store write.
	persistTimeout = 10 * time.Second
)

// Sentinel errors returned by the live-reload API.
var (
	ErrUnknownMonitor = errors.New("engine: unknown monitor")
	ErrMonitorPassive = errors.New("engine: monitor type is passive (push)")
	ErrMonitorPaused  = errors.New("engine: monitor is paused")
	// ErrCheckInProgress means a check for this monitor was already running, so
	// CheckNow started nothing.
	//
	// It used to return nil in that case, which reads as "started" and is not
	// distinguishable by any caller — the request quietly evaporated. Whether
	// that matters is the caller's call to make, and it cannot make it without
	// being told: the API treats it as success, because a check IS happening,
	// while a test driving checks one at a time has to wait and retry instead
	// of waiting for a result that will never come.
	ErrCheckInProgress = errors.New("engine: a check is already in progress")
)

// MaintenanceEvaluator reports whether a monitor is inside an active
// maintenance window. The full maintenance implementation is a later work
// package; the engine only needs this hook to suppress alerts.
type MaintenanceEvaluator interface {
	InMaintenance(monitorID int64, at time.Time) bool
}

// MaintenanceFunc adapts a plain function to MaintenanceEvaluator.
type MaintenanceFunc func(monitorID int64, at time.Time) bool

// InMaintenance implements MaintenanceEvaluator.
func (f MaintenanceFunc) InMaintenance(monitorID int64, at time.Time) bool {
	return f(monitorID, at)
}

// Options configures a new Engine. Store is required; everything else has a
// sensible default.
type Options struct {
	Logger *slog.Logger
	Store  Store
	// Registry resolves monitor type names; defaults to registry.Default.
	Registry *registry.Registry
	// Workers bounds concurrent checks; <= 0 falls back to 4×NumCPU via
	// config.EffectiveWorkerPoolSize at the call site — the engine itself
	// treats <= 0 as 1.
	Workers int
	// DefaultTimeout is the fallback per-check timeout.
	DefaultTimeout time.Duration
	// MaxStartupJitter caps the random first-fire delay. Negative
	// disables jitter entirely (used by tests).
	MaxStartupJitter time.Duration
	// Clock defaults to the real time package.
	Clock Clock
	// OnEvent receives every state transition. It is called synchronously
	// from worker goroutines and must be fast and non-blocking.
	OnEvent func(domain.StateChange)
	// OnHeartbeat receives every recorded heartbeat (not just transitions).
	// Like OnEvent it is called synchronously from worker goroutines and
	// must be fast and non-blocking; it feeds the live WebSocket dashboard.
	OnHeartbeat func(domain.Heartbeat)
	// Maintenance is the alert-suppression hook; nil = never in
	// maintenance.
	Maintenance MaintenanceEvaluator
	// Anomaly, when non-nil, enables response-time anomaly detection: every
	// UP check's latency is fed to the detector, and a sustained anomaly (or
	// its recovery) emits a StateChange flagged with a domain.LatencyAnomaly
	// through OnEvent — distinct from a DOWN transition. Nil leaves detection
	// off, so existing monitors are unaffected.
	Anomaly *anomaly.Detector
	// FlapThreshold and FlapWindow enable flapping damping: FlapThreshold
	// alertable changes (to DOWN, or DOWN back to UP) within FlapWindow put a
	// monitor into flapping mode, where one summary event replaces its
	// individual alerts until it has held one state for a whole FlapWindow.
	// Either one <= 0 disables it.
	FlapThreshold int
	FlapWindow    time.Duration
	// DownBackoffAfter and DownBackoffInterval slow the checks of a monitor
	// that has been DOWN for DownBackoffAfter to one every DownBackoffInterval
	// (never faster than its own interval). The first successful check ends
	// the backoff. DownBackoffAfter <= 0 disables it.
	DownBackoffAfter    time.Duration
	DownBackoffInterval time.Duration
}

// Engine is the monitor scheduler. Create with New, feed with Upsert (or
// Start, which loads all active monitors from the store) and shut down with
// Stop.
type Engine struct {
	log            *slog.Logger
	store          Store
	reg            *registry.Registry
	clock          Clock
	sem            chan struct{}
	defaultTimeout time.Duration
	maxJitter      time.Duration
	onEvent        func(domain.StateChange)
	onHeartbeat    func(domain.Heartbeat)
	maint          MaintenanceEvaluator
	anomaly        *anomaly.Detector

	flapThreshold   int
	flapWindow      time.Duration
	backoffAfter    time.Duration
	backoffInterval time.Duration

	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu       sync.Mutex
	jobs     map[int64]*job
	stopping bool
	// scheduled and unscheduled record the outcome of the last Start. They exist
	// so a failure to schedule is OBSERVABLE: a wrong or rotated ENCRYPTION_KEY
	// makes every config fail validation, and the process would otherwise report
	// itself healthy while checking nothing at all.
	scheduled   int
	unscheduled int
}

// Scheduling reports how many monitors the last Start managed to schedule and
// how many it had to skip. unscheduled > 0 always means something is wrong with
// those monitors' configuration; scheduled == 0 with unscheduled > 0 is the
// signature of a bad ENCRYPTION_KEY, because every config fails at once.
func (e *Engine) Scheduling() (scheduled, unscheduled int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.scheduled, e.unscheduled
}

// job is the per-monitor scheduling state. All fields are guarded by
// Engine.mu; the check itself runs outside the lock.
//
// gen is the job's generation (revision). Every lifecycle mutation that must
// invalidate an in-flight check — Pause, Resume, Remove and an Upsert that
// deactivates the monitor — bumps gen. A check captures gen when it fires; on
// completion it applies its result only if gen is unchanged, so a check that
// started before a Pause can never overwrite status or emit an event
// afterwards.
//
// Timer ownership has a single owner: while a check is running (running ==
// true) the check's completion is the sole code path that arms the next timer.
// Mutators never arm a timer while a check runs — that is what previously
// produced two concurrent timers when an Upsert landed during a check.
type job struct {
	monitor domain.Monitor
	passive bool
	// downSince is when the current outage actually began, kept alongside the
	// flag so a late announcement can report the real start.
	//
	// The re-emit used to pass `now`, and the dispatcher takes the event's
	// timestamp as the outage start: it stores it as `since`, ships it as
	// DownSince on the eventual resolve, and arms every resend and escalation
	// timer from it. So an outage that began five minutes into a four-hour
	// maintenance window was reported as having begun four hours later, and a
	// 30-minute escalation fired 4h30m after the real start.
	downSince time.Time
	// downSuppressed records that the DOWN this monitor is currently in was
	// SUPPRESSED when it was announced, so nobody was told about it.
	//
	// Suppression used to be evaluated once, on the transition edge, and never
	// again. The dispatcher drops a suppressed DOWN before processDown runs, so
	// no downState, no resend timer and no escalation were created — and the
	// engine emits nothing further while a monitor stays DOWN, because staying
	// DOWN is not a transition. A monitor that failed inside a maintenance
	// window was therefore never alerted on, not even after the window closed.
	// Same hole for a child whose parent was down at the time.
	//
	// This flag is what lets runCheck notice the un-suppression edge and announce
	// the outage that is still running. See the re-emit there.
	downSuppressed bool
	paused         bool
	removed        bool
	running        bool
	status         domain.MonitorStatus
	retries        int
	// successes counts consecutive successful checks while DOWN, towards the
	// monitor's RecoveryChecks. It is zero everywhere else.
	successes int
	// downAt is when the monitor last entered DOWN, as far as this process
	// knows — the clock the outage backoff runs on. A monitor that was already
	// DOWN at startup counts from its job's creation, so a restart never backs
	// off sooner than it would have. backedOff only exists so that entering the
	// backoff is logged once rather than on every slowed check.
	downAt    time.Time
	backedOff bool
	// Flapping damping. flapEdges holds the times of recent alertable changes
	// (pruned to the window); flapping is set once they reach the threshold,
	// and flapHeld counts the changes held back since. announcedDown records
	// whether the dispatcher was last TOLD this monitor is down, so settling
	// out of a flap can resolve an outage that was announced before it began.
	flapEdges     []time.Time
	flapping      bool
	flapHeld      int
	announcedDown bool
	gen           uint64
	timer         Timer
}

// New builds an Engine from opts. It does not schedule anything yet.
func New(opts Options) *Engine {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Registry == nil {
		opts.Registry = registry.Default
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = DefaultTimeout
	}
	if opts.MaxStartupJitter == 0 {
		opts.MaxStartupJitter = DefaultMaxStartupJitter
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		log:            opts.Logger,
		store:          opts.Store,
		reg:            opts.Registry,
		clock:          opts.Clock,
		sem:            make(chan struct{}, opts.Workers),
		defaultTimeout: opts.DefaultTimeout,
		maxJitter:      opts.MaxStartupJitter,
		onEvent:        opts.OnEvent,
		onHeartbeat:    opts.OnHeartbeat,
		maint:          opts.Maintenance,
		anomaly:        opts.Anomaly,
		// A threshold is meaningless without a window and vice versa, so a zero
		// in either switches damping off rather than producing a mode that can be
		// entered and never left.
		flapThreshold:   flapThreshold(opts),
		flapWindow:      opts.FlapWindow,
		backoffAfter:    max(opts.DownBackoffAfter, 0),
		backoffInterval: opts.DownBackoffInterval,
		baseCtx:         ctx,
		cancel:          cancel,
		jobs:            make(map[int64]*job),
	}
}

// Start loads all active monitors from the store and schedules them (with
// startup jitter). ctx bounds only the initial load.
func (e *Engine) Start(ctx context.Context) error {
	monitors, err := e.store.ListActiveMonitors(ctx)
	if err != nil {
		return fmt.Errorf("engine: load monitors: %w", err)
	}
	scheduled, skipped := 0, 0
	for _, m := range monitors {
		if err := e.Upsert(m); err != nil {
			e.log.Error("engine: skipping monitor", "monitor_id", m.ID, "name", m.Name, "error", err)
			skipped++
			continue
		}
		scheduled++
	}
	e.mu.Lock()
	e.scheduled, e.unscheduled = scheduled, skipped
	e.mu.Unlock()

	e.log.Info("monitor engine started",
		"monitors", scheduled, "unscheduled", skipped, "workers", cap(e.sem), "types", e.reg.List())
	// A per-monitor error line scrolls past; the aggregate is the one an operator
	// needs to see, because "some monitors are not being checked" is invisible
	// from the outside — the API still lists them as active.
	if skipped > 0 {
		e.log.Error("MONITORS ARE NOT BEING CHECKED: some monitors failed to schedule",
			"unscheduled", skipped, "scheduled", scheduled)
		if scheduled == 0 {
			e.log.Error("NO monitor could be scheduled at all — this is what a wrong or " +
				"rotated ENCRYPTION_KEY looks like, since every config then fails to decrypt")
		}
	}
	return nil
}

// Stop drains the engine: timers are disarmed immediately, then Stop waits
// (bounded by ctx) for in-flight checks to finish before cancelling their
// base context.
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	e.stopping = true
	for _, j := range e.jobs {
		if j.timer != nil {
			j.timer.Stop()
		}
	}
	e.mu.Unlock()

	done := make(chan struct{})
	safego.Go(e.log, "engine drain wait", func() {
		e.wg.Wait()
		close(done)
	})
	select {
	case <-done:
		e.cancel()
		return nil
	case <-ctx.Done():
		e.cancel() // abort stragglers
		return ctx.Err()
	}
}

// Upsert adds a monitor to the schedule or applies a changed configuration
// immediately (live reload). Runtime state (current status, retry counter)
// survives an update. Inactive monitors are kept as paused jobs.
func (e *Engine) Upsert(m domain.Monitor) error {
	checker, ok := e.reg.Get(m.Type)
	if !ok {
		return fmt.Errorf("engine: monitor %d %q: unknown type %q", m.ID, m.Name, m.Type)
	}
	if err := checker.Validate(m.Config); err != nil {
		return fmt.Errorf("engine: monitor %d %q: invalid config: %w", m.ID, m.Name, err)
	}
	_, passive := checker.(registry.PassiveChecker)

	e.mu.Lock()
	defer e.mu.Unlock()
	j, exists := e.jobs[m.ID]
	if !exists {
		status := m.Status
		if !status.Valid() || status == domain.StatusPaused || status == domain.StatusMaintenance {
			status = domain.StatusPending
		}
		j = &job{status: status}
		if status == domain.StatusDown {
			// Carried over from the row, i.e. from before a restart. The dispatcher
			// adopts such an outage on Restore, so treat it as announced: settling
			// out of a flap later then owes it a recovery.
			j.downAt = e.clock.Now()
			j.announcedDown = true
		}
		e.jobs[m.ID] = j
	}
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	// A change to what the check actually DOES invalidates a check already in
	// flight: its result describes the old target, so letting it land would
	// record a heartbeat — and possibly fire an alert — for an address this
	// monitor no longer points at. Renames, description and interval edits leave
	// an in-flight result perfectly valid, so they deliberately do not bump the
	// generation and the running check still counts.
	if exists && checkAffectingChange(j.monitor, m) {
		j.gen++
	}
	j.monitor = m
	j.passive = passive
	j.paused = !m.Active
	if j.paused || j.passive {
		// Passive monitors are fed by the push endpoint and its
		// watchdog (a later work package), not by timers. Deactivating a
		// monitor also invalidates any in-flight check so it cannot
		// persist a stale status or re-arm after this change.
		j.gen++
		return nil
	}
	if j.running {
		// A check is in flight and owns the timer; it re-arms with the
		// updated configuration when it completes. Arming here would
		// leave two concurrent timers for this monitor.
		return nil
	}
	e.scheduleLocked(j, e.jitter(m.Interval))
	return nil
}

// checkAffectingChange reports whether the difference between two revisions of
// a monitor changes what a check does or how its result is read. Everything
// else — name, description, link, interval, retry timing, parent, group — can
// change without invalidating a check that is already running.
func checkAffectingChange(old, updated domain.Monitor) bool {
	return old.Type != updated.Type ||
		old.Timeout != updated.Timeout ||
		old.UpsideDown != updated.UpsideDown ||
		!bytes.Equal(old.Config, updated.Config)
}

// Remove deletes a monitor from the schedule immediately. An in-flight check
// finishes but persists nothing.
func (e *Engine) Remove(id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok {
		return
	}
	j.removed = true
	j.gen++
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	delete(e.jobs, id)
	if e.anomaly != nil {
		e.anomaly.Forget(id)
	}
}

// Pause disarms a monitor's timer and records the paused status.
func (e *Engine) Pause(id int64) error {
	e.mu.Lock()
	j, ok := e.jobs[id]
	if !ok {
		e.mu.Unlock()
		return ErrUnknownMonitor
	}
	j.paused = true
	j.monitor.Active = false
	// Advance the generation so a check that started before this Pause is
	// discarded on completion instead of overwriting the paused status or
	// emitting a spurious event.
	j.gen++
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	j.status = domain.StatusPaused
	e.mu.Unlock()
	e.persistStatus(id, domain.StatusPaused)
	return nil
}

// Resume re-schedules a paused monitor; its state restarts at PENDING with a
// fresh retry counter and a jittered first check.
func (e *Engine) Resume(id int64) error {
	e.mu.Lock()
	j, ok := e.jobs[id]
	if !ok {
		e.mu.Unlock()
		return ErrUnknownMonitor
	}
	j.paused = false
	j.monitor.Active = true
	j.status = domain.StatusPending
	j.retries = 0
	j.successes = 0
	j.downAt, j.backedOff = time.Time{}, false
	// Pausing reset the dispatcher's view of this monitor (no open outage), so
	// the flapping history and the announced flag restart from nothing too.
	j.flapEdges, j.flapping, j.flapHeld, j.announcedDown = nil, false, 0, false
	// Advance the generation: any check still in flight from before the
	// pause is now stale. It will neither persist a result nor re-arm the
	// timer, so Resume takes over timer ownership.
	j.gen++
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	if !j.passive && !j.running {
		e.scheduleLocked(j, e.jitter(j.monitor.Interval))
	}
	// If a (now stale) check is still running it re-arms a fresh timer on
	// completion, keeping a single owner.
	e.mu.Unlock()
	e.persistStatus(id, domain.StatusPending)
	return nil
}

// CheckNow triggers an immediate check instead of waiting for the interval.
// A check already in flight satisfies the request (no queuing).
func (e *Engine) CheckNow(id int64) error {
	e.mu.Lock()
	j, ok := e.jobs[id]
	switch {
	case !ok:
		e.mu.Unlock()
		return ErrUnknownMonitor
	case j.passive:
		e.mu.Unlock()
		return ErrMonitorPassive
	case j.paused:
		e.mu.Unlock()
		return ErrMonitorPaused
	case j.running:
		e.mu.Unlock()
		return ErrCheckInProgress
	}
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	e.mu.Unlock()
	e.fire(j)
	return nil
}

// StatusOf returns the engine's in-memory state for a monitor.
func (e *Engine) StatusOf(id int64) (domain.MonitorStatus, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j, ok := e.jobs[id]
	if !ok {
		return "", false
	}
	// A PASSIVE monitor has no answer here, and saying so is the whole point.
	//
	// The engine never checks a push monitor, so nothing in this package ever
	// writes j.status for one after Upsert created the job — the push recorder
	// and its watchdog write the database row instead and never touch e.jobs.
	// Returning the frozen value made every reader that overlays StatusOf
	// (decorateStatus, the badge, the public status page, /metrics) replace the
	// TRUE row status with whatever the monitor happened to be at the moment its
	// job was created — usually "pending". A push monitor the watchdog had
	// tripped DOWN was reported as healthy on the public page and in Prometheus,
	// which for an uptime product is the worst available outcome.
	//
	// ok=false means "ask the row", and the row is exactly where the truth is.
	if j.passive {
		return "", false
	}
	return j.status, true
}

// jitter returns a random first-fire delay in [0, min(interval, maxJitter)).
func (e *Engine) jitter(interval time.Duration) time.Duration {
	maxDelay := e.maxJitter
	if maxDelay < 0 {
		return 0
	}
	if interval < maxDelay {
		maxDelay = interval
	}
	if maxDelay <= 0 {
		return 0
	}
	return rand.N(maxDelay) //nolint:gosec // spreads first fires over time; not a secret
}

// scheduleLocked arms j's timer; the caller holds e.mu.
func (e *Engine) scheduleLocked(j *job, delay time.Duration) {
	if e.stopping {
		return
	}
	if delay < 0 {
		delay = 0
	}
	j.timer = e.clock.AfterFunc(delay, func() { e.fire(j) })
}

// fire moves a due job into a worker goroutine. Overlap and lifecycle checks
// happen here so a stale timer can never start a second concurrent run.
func (e *Engine) fire(j *job) {
	e.mu.Lock()
	// j.passive is checked here too: a passive monitor is fed by the push
	// endpoint and its watchdog, never by a timer, so a timer that somehow
	// survived a type change must not start a check.
	if e.stopping || j.removed || j.paused || j.passive || j.running {
		e.mu.Unlock()
		return
	}
	j.running = true
	j.timer = nil
	m := j.monitor
	prev, retries, successes := j.status, j.retries, j.successes
	gen := j.gen
	// Add to the WaitGroup while the lock is still held. Stop() sets stopping
	// and then waits on this group, both under the same mutex discipline; an Add
	// after the unlock can land in the window where Wait has already returned
	// with a zero counter, so Stop would report a clean drain and a check would
	// start immediately afterwards — against a context nobody is waiting on.
	e.wg.Add(1)
	e.mu.Unlock()

	fireAt := e.clock.Now()
	// runCheck contains its own panics (safeCheck for the checker, a deferred
	// backstop for the rest) because it has cleanup that must run either way.
	// safego.Go here would recover ABOVE that cleanup and wedge the job.
	go e.runCheck(j, m, prev, retries, successes, fireAt, gen)
}

// safeCheck runs one check and turns a panic inside it into an ordinary error.
//
// The input a checker parses comes from the monitored target, and monitored
// targets are by definition the systems that misbehave — that is why they are
// being watched. Without this, a nil map or a slice bound in any of the
// seventeen checkers, or in the protocol libraries behind them, ends the
// process: an unrecovered panic in a goroutine takes the whole program with it,
// so one broken endpoint stopped ALL monitoring, including the alert that would
// have reported it.
//
// A panic becomes an error rather than a DOWN result directly, so everything
// downstream — the retry state machine, the heartbeat, the event, the re-arm —
// runs exactly as it does for a connection failure. Containment that skipped
// that would leave the monitor wedged, which is a quieter version of the same
// problem.
//
// The stack is logged because the error message alone names the panic value and
// not the line that produced it, and a panic here is a bug worth finding.
func (e *Engine) safeCheck(ctx context.Context, checker registry.Checker, cfg json.RawMessage) (res domain.CheckResult, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("check panicked: %v", rec)
			e.log.Error("monitor check panicked",
				"type", checker.Type(), "recover", rec, "stack", string(debug.Stack()))
		}
	}()
	return checker.Check(ctx, cfg)
}

// runCheck executes one check inside the bounded worker pool, applies the
// state machine, persists results, emits events and re-arms the timer
// drift-corrected against fireAt.
func (e *Engine) runCheck(j *job, m domain.Monitor, prev domain.MonitorStatus, retries, successes int, fireAt time.Time, gen uint64) {
	defer e.wg.Done()
	// Backstop for everything AFTER the checker call, which safeCheck already
	// covers: persisting the heartbeat, emitting the event, re-arming the timer.
	// A panic in any of those still ended the process.
	//
	// The recover clears the running flag, because the code that normally does
	// so sits below the panic point and will not run. Without that the monitor
	// is never scheduled again — contained, and silently never checked, which
	// is the failure this is meant to prevent wearing a quieter hat. The timer
	// is deliberately NOT re-armed here: the state this ran on is unknown after
	// a panic, and a monitor that stops until the next Upsert or restart is a
	// smaller lie than one that keeps reporting on a state machine that fell
	// over mid-update.
	defer func() {
		if rec := recover(); rec != nil {
			e.log.Error("panic while completing a check",
				"monitor_id", m.ID, "type", m.Type, "recover", rec,
				"stack", string(debug.Stack()))
			e.clearRunning(j)
		}
	}()

	// Bound concurrency: at most cap(sem) checks run at once.
	select {
	case e.sem <- struct{}{}:
	case <-e.baseCtx.Done():
		e.clearRunning(j)
		return
	}
	defer func() { <-e.sem }()

	checker, ok := e.reg.Get(m.Type)
	if !ok { // type vanished (cannot happen without a code change)
		e.clearRunning(j)
		return
	}

	ctx, cancel := context.WithTimeout(e.baseCtx, e.timeoutFor(m, checker))
	start := time.Now()
	res, err := e.safeCheck(ctx, checker, m.Config)
	elapsed := time.Since(start)
	cancel()
	if err != nil {
		res = domain.CheckResult{Status: domain.StatusDown, Latency: elapsed, Message: err.Error()}
	}

	up := res.Status == domain.StatusUp
	if m.UpsideDown {
		up = !up
	}
	next, newRetries, newSuccesses, transitioned := Step(prev, retries, successes, m.MaxRetries, m.RecoveryChecks, up)
	now := e.clock.Now()
	if up && next == domain.StatusDown {
		// A success that does not count yet. Without saying so the heartbeat
		// reads as a DOWN carrying a healthy message, which looks like a bug.
		msg := fmt.Sprintf("recovering: %d of %d successful checks", newSuccesses, m.RecoveryChecks)
		if res.Message != "" {
			msg += " — " + res.Message
		}
		res.Message = msg
	}

	// A check belongs to the generation it fired in. If Pause, Resume, Remove
	// or a deactivating Upsert advanced the generation while the check ran,
	// its result is stale: discard it entirely (no heartbeat, no status
	// write, no event) so it cannot overwrite state decided after it started.
	e.mu.Lock()
	current := !j.removed && j.gen == gen
	e.mu.Unlock()
	if current {
		e.persist(m, res, next, newRetries, transitioned, now)
		// A flap ends before this check's own transition is looked at: a change
		// arriving after a whole quiet window is an ordinary alert again, not one
		// more held back.
		e.settleFlapping(j, m, next, res.Message, now)
		suppressedDown, downSince := e.wasDownSuppressed(j)
		switch {
		case transitioned:
			if alertable(prev, next) {
				if started, changes := e.noteFlap(j, now); started {
					e.emitFlapping(m, false, changes, next, now)
				}
			}
			suppressed := e.emit(m, prev, next, res.Message, now)
			e.setDownSuppressed(j, next == domain.StatusDown && suppressed, now)
			if !suppressed {
				e.setAnnounced(j, next)
			}
		case next == domain.StatusDown && suppressedDown:
			// The outage is still running and nobody has ever been told about it,
			// because it began while a maintenance window was open or its parent
			// was down. Re-evaluate: if the suppressing condition has since gone,
			// announce it now.
			//
			// A DOWN→DOWN event is what the dispatcher wants — it routes on
			// `To == StatusDown`, and processDown finds no existing outage, so it
			// opens one and alerts exactly as it would have at the edge. The
			// announcement is late by at most one check interval, which is the
			// finest granularity this loop has.
			//
			// It carries downSince, not now: the dispatcher takes the event's
			// timestamp as the outage start, ships it as DownSince on the
			// eventual resolve, and arms every resend and escalation timer from
			// it. Passing `now` reported an outage that began five minutes into a
			// four-hour window as having begun four hours later, and pushed a
			// 30-minute escalation out to 4h30m.
			//
			// The flag is cleared on emit's OWN verdict, not on the pre-check.
			// Suppression is evaluated twice — here and inside emit — and if the
			// condition returns between the two, the event goes out suppressed and
			// the dispatcher drops it. Clearing the flag anyway made the outage
			// permanently unannounceable, because staying DOWN is not a
			// transition and nothing would emit for it again.
			if suppressed, _ := e.suppression(m, now); !suppressed {
				if !e.emit(m, domain.StatusDown, domain.StatusDown, res.Message, downSince) {
					e.setDownSuppressed(j, false, time.Time{})
					e.setAnnounced(j, domain.StatusDown)
				}
			}
		}
		// Feed the response-time baseline only for checks that came back UP:
		// a failed or timed-out check's latency is not a valid latency sample,
		// and a sustained slow-down is only meaningful while the monitor still
		// answers.
		if e.anomaly != nil && up {
			e.detectAnomaly(m, res.Latency, now)
		}
	}

	e.finishCheck(j, gen, next, newRetries, newSuccesses, current, fireAt, now)
}

// finishCheck applies the result to the job and re-arms its timer, under the
// lock.
//
// It is a function rather than a block inlined in runCheck for one reason: the
// deferred Unlock. Written inline with a plain e.mu.Unlock() at the end, a panic
// anywhere in here unwound with the mutex STILL HELD — and runCheck's recover
// then calls clearRunning, which takes that same mutex. A Go mutex is not
// reentrant, so the recover blocked forever while holding the engine's single
// lock, and every other monitor blocked behind it: one panic in one job stopped
// ALL monitoring, permanently and silently. That is the exact failure the
// backstop exists to prevent, produced by the backstop itself.
//
// Not theoretical — Clock is an injectable interface and both calls below reach
// it (e.clock.Now here, e.clock.AfterFunc via scheduleLocked), so
// TestPanicUnderTheLockDoesNotWedgeTheEngine reproduces it deterministically.
// Deferring the unlock releases the mutex during the unwind, which is all the
// recover needs.
func (e *Engine) finishCheck(j *job, gen uint64, next domain.MonitorStatus, newRetries, newSuccesses int, current bool, fireAt, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	j.running = false
	// Re-check under the lock: a lifecycle change may have landed while we
	// persisted and emitted outside it.
	current = current && !j.removed && j.gen == gen
	if current {
		j.status = next
		j.retries = newRetries
		j.successes = newSuccesses
		switch {
		case next != domain.StatusDown:
			j.downAt, j.backedOff = time.Time{}, false
		case j.downAt.IsZero():
			j.downAt = now
		}
	}
	// !j.passive matters here: a monitor switched to a passive type WHILE this
	// check was in flight reaches this point owning the re-arm. Without the test
	// the scheduler kept polling a push monitor forever — and pushcheck.Check
	// unconditionally reports UP, so those synthetic beats fought the watchdog
	// and masked a genuinely dead push monitor.
	if !j.removed && !j.paused && !j.passive && !e.stopping {
		var delay time.Duration
		if current {
			// Drift-corrected: the next fire time is based on when this
			// check was scheduled, not when it finished.
			delay = fireAt.Add(e.nextIntervalLocked(j, next, newSuccesses, now)).Sub(e.clock.Now())
		} else {
			// A lifecycle change (e.g. Resume) superseded this check while
			// it ran. Because mutators never arm a timer while a check is
			// running, this completion owns the re-arm: restart the
			// schedule fresh under the current configuration.
			delay = e.jitter(j.monitor.Interval)
		}
		e.scheduleLocked(j, delay)
	}
}

// nextIntervalLocked picks the interval to the next check; the caller holds
// e.mu. It reads j.monitor, not the check's copy, so an Upsert that landed
// during the check takes effect immediately.
func (e *Engine) nextIntervalLocked(j *job, next domain.MonitorStatus, successes int, now time.Time) time.Duration {
	interval := j.monitor.Interval
	retry := j.monitor.RetryInterval
	switch {
	case next == domain.StatusPending:
		if retry > 0 {
			interval = retry
		}
	case next == domain.StatusDown && successes > 0:
		// A recovery is being confirmed: at the retry cadence when there is one,
		// and never at the backed-off one — the first success is exactly what
		// the backoff is waiting to notice, so it ends here.
		if retry > 0 {
			interval = retry
		}
		j.backedOff = false
	case next == domain.StatusDown && e.backoffAfter > 0 && !j.downAt.IsZero() &&
		now.Sub(j.downAt) >= e.backoffAfter && e.backoffInterval > interval:
		if !j.backedOff {
			j.backedOff = true
			e.log.Info("monitor down for a long time: checking less often until it answers",
				"monitor_id", j.monitor.ID, "name", j.monitor.Name,
				"down_for", now.Sub(j.downAt).Round(time.Second), "interval", e.backoffInterval)
		}
		interval = e.backoffInterval
	}
	return interval
}

// clearRunning resets the running flag when a check aborts before executing.
func (e *Engine) clearRunning(j *job) {
	e.mu.Lock()
	j.running = false
	e.mu.Unlock()
}

// timeoutFor resolves the per-check timeout: monitor override → checker
// type default → engine default.
func (e *Engine) timeoutFor(m domain.Monitor, checker registry.Checker) time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	if td, ok := checker.(registry.TimeoutDefaulter); ok && td.DefaultTimeout() > 0 {
		return td.DefaultTimeout()
	}
	return e.defaultTimeout
}

// persist writes the heartbeat (always), the monitor status (on transitions)
// and observed certificate data. Store failures are logged, never fatal.
func (e *Engine) persist(m domain.Monitor, res domain.CheckResult, next domain.MonitorStatus, retriesNow int, transitioned bool, now time.Time) {
	ctx, cancel := context.WithTimeout(e.baseCtx, persistTimeout)
	defer cancel()

	latency := res.Latency
	hb := domain.Heartbeat{
		MonitorID: m.ID,
		CheckedAt: now,
		Status:    next,
		Latency:   &latency,
		Message:   res.Message,
		Important: transitioned,
		Retries:   retriesNow,
		Extra:     res.Extra,
	}
	if err := e.store.SaveHeartbeat(ctx, hb); err != nil {
		e.log.Error("engine: save heartbeat", "monitor_id", m.ID, "error", err)
	}
	if e.onHeartbeat != nil {
		e.onHeartbeat(hb)
	}
	if transitioned {
		if err := e.store.UpdateMonitorStatus(ctx, m.ID, next); err != nil {
			e.log.Error("engine: update monitor status", "monitor_id", m.ID, "error", err)
		}
	}
	if res.Cert != nil {
		if err := e.store.SaveCertInfo(ctx, m.ID, *res.Cert); err != nil {
			e.log.Error("engine: save cert info", "monitor_id", m.ID, "error", err)
		}
	}
}

// persistStatus writes just the status column (used by Pause/Resume).
func (e *Engine) persistStatus(id int64, status domain.MonitorStatus) {
	ctx, cancel := context.WithTimeout(e.baseCtx, persistTimeout)
	defer cancel()
	if err := e.store.UpdateMonitorStatus(ctx, id, status); err != nil {
		e.log.Error("engine: update monitor status", "monitor_id", id, "error", err)
	}
}

// emit delivers a StateChange to the event sink, applying the alert suppression
// rules (parent DOWN, active maintenance window), and reports whether the alert
// was suppressed — so the caller can remember that nobody was told.
func (e *Engine) emit(m domain.Monitor, from, to domain.MonitorStatus, message string, at time.Time) bool {
	ev := domain.StateChange{
		MonitorID:   m.ID,
		MonitorName: m.Name,
		From:        from,
		To:          to,
		At:          at,
		Message:     message,
	}
	ev.Suppressed, ev.SuppressReason = e.suppression(m, at)
	e.log.Info("monitor state change",
		"monitor_id", ev.MonitorID, "name", ev.MonitorName,
		"from", ev.From, "to", ev.To, "suppressed", ev.Suppressed)
	if e.onEvent != nil {
		e.onEvent(ev)
	}
	return ev.Suppressed
}

// setDownSuppressed records whether the current DOWN went unannounced, and when
// it started.
func (e *Engine) setDownSuppressed(j *job, v bool, at time.Time) {
	e.mu.Lock()
	j.downSuppressed = v
	if v {
		j.downSince = at
	}
	e.mu.Unlock()
}

// wasDownSuppressed reports whether the DOWN this monitor is in was suppressed
// when it happened.
func (e *Engine) wasDownSuppressed(j *job) (bool, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return j.downSuppressed, j.downSince
}

// setAnnounced records what the dispatcher was last told about this monitor.
// Only DOWN and UP are announcements; PENDING is never alerted on.
func (e *Engine) setAnnounced(j *job, st domain.MonitorStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch st {
	case domain.StatusDown:
		j.announcedDown = true
	case domain.StatusUp:
		j.announcedDown = false
	}
}

// alertable reports whether a transition is one the dispatcher alerts on: into
// DOWN, or out of DOWN back to UP. Those are the changes flapping counts — an
// up→pending→up wobble never reaches anybody, so it is not noise to damp.
func alertable(prev, next domain.MonitorStatus) bool {
	return next == domain.StatusDown || (prev == domain.StatusDown && next == domain.StatusUp)
}

// flapThreshold resolves the configured damping threshold: off unless both the
// threshold and the window are set.
func flapThreshold(opts Options) int {
	if opts.FlapThreshold <= 0 || opts.FlapWindow <= 0 {
		return 0
	}
	return opts.FlapThreshold
}

// noteFlap records one alertable change and reports whether it tipped the
// monitor into flapping mode, together with how many changes the window held at
// that moment. A change arriving while already flapping is counted as held back
// for the closing summary.
func (e *Engine) noteFlap(j *job, now time.Time) (started bool, changes int) {
	if e.flapThreshold <= 0 {
		return false, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := now.Add(-e.flapWindow)
	kept := j.flapEdges[:0]
	for _, at := range j.flapEdges {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	j.flapEdges = append(kept, now)
	if j.flapping {
		j.flapHeld++
		return false, 0
	}
	if len(j.flapEdges) < e.flapThreshold {
		return false, 0
	}
	j.flapping = true
	// The change that tripped the threshold is held back as well — the summary
	// replaces it — so it is the first one counted.
	j.flapHeld = 1
	return true, len(j.flapEdges)
}

// settleFlapping ends flapping mode once the monitor has gone a whole window
// without an alertable change, sends the closing summary, and squares the
// dispatcher's view with the state the monitor settled in.
//
// Settling UP after a DOWN that was announced before the flapping began owes
// its channels a recovery: the held-back UPs deliberately left that outage open
// (see domain.SuppressFlapping), so nothing else would ever close it. Settling
// DOWN needs nothing here — a held-back DOWN left downSuppressed set, and the
// re-announce in runCheck picks it up in this very check.
func (e *Engine) settleFlapping(j *job, m domain.Monitor, status domain.MonitorStatus, message string, now time.Time) {
	e.mu.Lock()
	if !j.flapping || len(j.flapEdges) == 0 || now.Sub(j.flapEdges[len(j.flapEdges)-1]) < e.flapWindow {
		e.mu.Unlock()
		return
	}
	held := j.flapHeld
	j.flapping, j.flapHeld, j.flapEdges = false, 0, nil
	owed := status == domain.StatusUp && j.announcedDown
	e.mu.Unlock()

	e.emitFlapping(m, true, held, status, now)
	if owed && !e.emit(m, domain.StatusDown, domain.StatusUp, message, now) {
		e.setAnnounced(j, domain.StatusUp)
	}
}

// emitFlapping sends a flapping summary. A maintenance window or a down parent
// suppresses it like any alert, flapping itself never does — it is the one
// message flapping mode still sends.
func (e *Engine) emitFlapping(m domain.Monitor, ended bool, changes int, status domain.MonitorStatus, at time.Time) {
	fl := &domain.Flapping{Ended: ended, Changes: changes, Window: e.flapWindow, Status: status}
	message := fmt.Sprintf("%d state changes within %s; individual alerts are paused until it is stable",
		changes, fl.WindowText())
	if ended {
		message = fmt.Sprintf("stable for %s, currently %s; %d state changes were held back",
			fl.WindowText(), status, changes)
	}
	ev := domain.StateChange{
		MonitorID:   m.ID,
		MonitorName: m.Name,
		From:        status,
		To:          status,
		At:          at,
		Message:     message,
		Flapping:    fl,
	}
	ev.Suppressed, ev.SuppressReason = e.baseSuppression(m, at)
	e.log.Info("monitor flapping",
		"monitor_id", m.ID, "name", m.Name, "ended", ended, "changes", changes,
		"status", status, "suppressed", ev.Suppressed)
	if e.onEvent != nil {
		e.onEvent(ev)
	}
}

// Suppression exposes the alert-suppression rules to subsystems that emit
// StateChange events without going through the engine's own emit path — the
// push recorder and its watchdog, which are driven by incoming heartbeats
// rather than by timers. Without a shared hook those alerts ignore maintenance
// windows and parent outages entirely, which is worse than a missing feature:
// the whole point of a maintenance window is that it silences the pager.
func (e *Engine) Suppression(m domain.Monitor, at time.Time) (suppressed bool, reason string) {
	return e.suppression(m, at)
}

// suppression evaluates the alert-suppression rules for monitor m at time at:
// an active maintenance window, a parent that is currently DOWN, or the monitor
// itself flapping.
func (e *Engine) suppression(m domain.Monitor, at time.Time) (suppressed bool, reason string) {
	if suppressed, reason = e.baseSuppression(m, at); suppressed {
		return suppressed, reason
	}
	e.mu.Lock()
	j, ok := e.jobs[m.ID]
	flapping := ok && j.flapping
	e.mu.Unlock()
	if flapping {
		return true, domain.SuppressFlapping
	}
	return false, ""
}

// baseSuppression is suppression without the flapping rule: the conditions that
// silence every alert, a flapping summary included.
func (e *Engine) baseSuppression(m domain.Monitor, at time.Time) (suppressed bool, reason string) {
	if e.maint != nil && e.maint.InMaintenance(m.ID, at) {
		return true, "maintenance"
	}
	if m.ParentID != nil {
		e.mu.Lock()
		parent, ok := e.jobs[*m.ParentID]
		// A passive parent's in-memory status is frozen at job creation (see
		// StatusOf), so reading it decided suppression from a value that can
		// never change: a push parent DOWN at process start suppressed its
		// children forever, and one that went DOWN later suppressed nothing.
		//
		// For a passive parent the ROW is the authority — the push recorder and
		// its watchdog write it — and the engine holds the monitor it was last
		// Upserted with, which the API rewrites on every change. Simply giving up
		// would have been the other half-fix: a push-monitored gateway going down
		// would then page for every child instead of once for the parent.
		parentDown := ok && parent.status == domain.StatusDown
		if ok && parent.passive {
			parentDown = parent.monitor.Status == domain.StatusDown
		}
		e.mu.Unlock()
		if parentDown {
			return true, "parent-down"
		}
	}
	return false, ""
}

// detectAnomaly feeds one UP check's latency to the anomaly detector and emits
// a StateChange on the raise/clear edges only — never on every anomalous check
// — so a sustained slow-down alerts once and its recovery alerts once.
func (e *Engine) detectAnomaly(m domain.Monitor, latency time.Duration, at time.Time) {
	r := e.anomaly.Observe(m.ID, float64(latency))
	if !r.Raised && !r.Cleared {
		return
	}
	la := &domain.LatencyAnomaly{
		Cleared:    r.Cleared,
		Latency:    latency,
		Baseline:   time.Duration(r.Baseline),
		Deviations: r.Score,
	}
	var message string
	switch {
	case r.Cleared:
		message = fmt.Sprintf("response time recovered to baseline %s", la.Baseline)
	case math.IsInf(r.Score, 1):
		message = fmt.Sprintf("response time %s far above baseline %s", latency, la.Baseline)
	default:
		message = fmt.Sprintf("response time %s is %.1fσ above baseline %s", latency, r.Score, la.Baseline)
	}
	ev := domain.StateChange{
		MonitorID:   m.ID,
		MonitorName: m.Name,
		From:        domain.StatusUp,
		To:          domain.StatusUp,
		At:          at,
		Message:     message,
		Anomaly:     la,
	}
	ev.Suppressed, ev.SuppressReason = e.suppression(m, at)
	e.log.Info("monitor latency anomaly",
		"monitor_id", m.ID, "name", m.Name, "cleared", r.Cleared,
		"latency", latency, "baseline", la.Baseline, "deviations", r.Score,
		"suppressed", ev.Suppressed)
	if e.onEvent != nil {
		e.onEvent(ev)
	}
}
