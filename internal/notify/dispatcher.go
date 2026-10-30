// Package notify implements the notification core: message templating,
// quiet hours, digest batching, the delivery audit log and the Dispatcher —
// the consumer of monitor StateChange events that fans alerts out to the
// provider plugins registered in internal/notify/registry.
//
// Event flow: engine → HandleStateChange (a channel send, nothing more) → a
// recorder goroutine writes the event to the outbox → a
// single dispatch goroutine resolves the monitor's channel assignments,
// applies the policy layers (suppression, mute/snooze, digest batching,
// quiet hours) and fans out concurrently with per-provider error isolation.
// Every attempted delivery is recorded in the audit log.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/notify/registry"
	"github.com/Ariazonaa/statwarden/internal/safego"
)

// Defaults used when Options leave the corresponding field zero.
const (
	// DefaultSendTimeout bounds a single provider Send.
	DefaultSendTimeout = 30 * time.Second
	// DefaultQueueSize is the event queue length; the queue decouples the
	// engine's synchronous OnEvent callback from delivery work. It is sized for
	// a burst — a whole fleet transitioning at once, e.g. after a network blip —
	// because the dispatch loop is single-threaded and one slow provider holds it
	// for a full send timeout, during which everything else queues behind it.
	DefaultQueueSize = 4096
	// enqueueBlockTimeout is how long a caller waits for room in a full queue
	// before the event is dropped. It trades one briefly stalled engine worker
	// for not silently losing an alert.
	enqueueBlockTimeout = 5 * time.Second
	// recordTimeout bounds each audit-log write.
	recordTimeout = 10 * time.Second

	// Durable hand-off (see migration 0014 and RunOutbox).
	//
	// DefaultOutboxInterval is how often unfinished events are swept up.
	DefaultOutboxInterval = 30 * time.Second
	// outboxWriteTimeout bounds the record/clear writes. The record happens on
	// the engine's event path, so it must not hang there.
	outboxWriteTimeout = 5 * time.Second
	// outboxLease is how long a claim holds a row. It is comfortably above the
	// dispatcher's own send budget (SendAttempts × SendTimeout plus retry
	// delays) so a delivery that is merely slow is not retried underneath
	// itself, which would double-alert.
	outboxLease = 5 * time.Minute
	// outboxBatch bounds one sweep, so a large backlog after a long outage is
	// worked through steadily instead of in one burst.
	outboxBatch = 100
	// outboxMaxAttempts is when the dispatcher gives up on an event — loudly.
	outboxMaxAttempts = 10
	// intakeBuffer sizes the queue between the engine's worker goroutines and
	// the goroutine that records events.
	//
	// engine.Options.OnEvent is documented as "fast and non-blocking" and is
	// called on a checker worker, so the recording INSERT cannot happen there:
	// under a slow database every state transition would hold a worker for up
	// to outboxWriteTimeout, and a fleet-wide incident is exactly when there
	// are many transitions and the fewest workers to spare. Handing off through
	// a buffered channel keeps the engine's side to a channel send, while the
	// event is still recorded BEFORE it enters the dispatch queue — which is
	// what makes a full dispatch queue survivable.
	intakeBuffer = 1024
)

// outcome says whether an event is finished with or has to be tried again. It
// is the outbox's completion signal, so "done" must mean genuinely done:
// delivered, or deliberately not delivered. A provider that failed is NOT done.
type outcome int

const (
	// outcomeDone: nothing further will be attempted for this event, whether
	// because it was delivered, because policy said not to (muted, quiet
	// hours), or because there was nobody to deliver it to.
	outcomeDone outcome = iota
	// outcomeRetry: an intended recipient was not reached. The outbox row
	// stays, and the sweeper runs the event again once the lease lapses.
	outcomeRetry
)

// ErrStopped is returned by TestSend after the dispatcher shut down.
var ErrStopped = errors.New("notify: dispatcher stopped")

// DigestOptions configures the periodic batched-summary mode. When Interval
// is > 0 monitor alerts are not sent individually; they are buffered per
// channel and flushed as one digest message per interval.
type DigestOptions struct {
	Interval time.Duration
}

// Options configures a new Dispatcher. Store is required; everything else
// has a sensible default.
type Options struct {
	Logger *slog.Logger
	Store  Store
	// Registry resolves provider type names; defaults to registry.Default.
	Registry *registry.Registry
	// Clock defaults to the real time package.
	Clock Clock
	// SendTimeout bounds one provider Send call.
	SendTimeout time.Duration
	// SendAttempts is how often a failing Send is retried before the
	// delivery is recorded as failed; <= 0 means 1 (no retry).
	SendAttempts int
	// RetryDelay is the pause between attempts (0 = immediate).
	RetryDelay time.Duration
	// QuietHours suppresses deliveries inside a daily window; nil = none.
	QuietHours *QuietHours
	// Digest enables batched summaries when Interval > 0.
	Digest DigestOptions
	// QueueSize is the event queue length.
	QueueSize int
}

// CertExpiryEvent is the dispatcher-facing shape of a certificate-expiry
// threshold crossing (see internal/monitor/sslcheck).
type CertExpiryEvent struct {
	MonitorID     int64
	MonitorName   string
	Issuer        string
	ValidTo       time.Time
	DaysRemaining int
	Expired       bool
}

// downState tracks one ongoing outage: when it started, its resend timer, its
// pending escalation timers, and who has actually been told about it.
type downState struct {
	since       time.Time
	resendTimer Timer
	escTimers   []Timer
	// notified holds the NOTIFICATION IDs that actually received a DOWN alert.
	//
	// It used to be keyed by escalation delay, which conflated a level with the
	// channels on it. Two channels on the same level and only one delivery
	// succeeding meant both were later told "back online" — one of them about an
	// outage it had never heard of. Keying by channel also makes an assignment
	// that changes mid-outage behave sensibly: a channel added while the monitor
	// is down is simply not in the set, so it is not resolved to.
	//
	// It is the ONLY dedupe an alert needs. There used to be a second map keyed by
	// escalation delay, recording that a level's timer had run, and
	// processEscalation refused to run a level twice on the strength of it — which
	// is what made a failed escalation permanent, because the flag was set whether
	// or not anything had been delivered. Per-channel tracking subsumes it: a
	// channel that heard the alert is skipped, and one that did not is retried.
	notified map[int64]bool
	// resolving marks an outage whose "back online" alert has been attempted
	// but not confirmed.
	//
	// processRecovery used to delete the entry BEFORE delivering, and still
	// returned outcomeRetry when the fan-out failed. The retry then found no
	// tracked outage, logged "recovery without a tracked outage", returned
	// outcomeDone — and completeOutbox deleted the row. So any transient
	// provider failure lost the resolve permanently, and the machinery built to
	// retry it was what threw it away. The entry now survives the attempt; this
	// flag is what stops it being mistaken for a live outage in the meantime.
	resolving bool
}

func (st *downState) stopTimers() {
	if st.resendTimer != nil {
		st.resendTimer.Stop()
	}
	for _, t := range st.escTimers {
		t.Stop()
	}
}

// Dispatcher consumes monitor events and delivers notifications. Create with
// New, call Start, feed via the Handle* methods and shut down with Stop.
type Dispatcher struct {
	log          *slog.Logger
	store        Store
	reg          *registry.Registry
	clock        Clock
	renderer     *Renderer
	sendTimeout  time.Duration
	sendAttempts int
	retryDelay   time.Duration
	quiet        *QuietHours
	digest       DigestOptions

	baseCtx context.Context
	cancel  context.CancelFunc
	tasks   chan func(context.Context)
	// quit signals shutdown. It exists instead of closing tasks: enqueue may
	// block on a send while waiting for room in a full queue, and it cannot hold
	// d.mu while doing so (the dispatch loop takes the same mutex), so a Stop
	// that closed tasks would be a send on a closed channel — a panic, not a
	// race. Nothing ever closes tasks; the loop drains it after quit instead.
	quit chan struct{}
	done chan struct{}
	// intake carries accepted events from the engine's worker goroutines to the
	// goroutine that records them. See intakeBuffer for why the recording write
	// cannot happen on the caller's goroutine. intakeDone closes when the
	// recorder has drained.
	intake chan intakeItem
	// intakeClosed is set under mu BEFORE quitIntake is closed, so "no more
	// events can be accepted" is a fact the recorder can rely on rather than a
	// race it has to guess at.
	intakeClosed bool
	// quitIntake stops new events being accepted. It is closed one step BEFORE
	// quit: the recorder has to finish handing its backlog to a dispatch loop
	// that is still running, or events accepted on the promise of delivery
	// would only be found again by the next process's first sweep.
	quitIntake chan struct{}
	intakeDone chan struct{}
	// inflight counts events accepted but not yet handed to the dispatch queue.
	// It exists so a test can wait for the recorder to catch up; production
	// code never reads it.
	inflight sync.WaitGroup

	mu sync.Mutex
	// stopping is set the moment Stop is entered; stopped only once the intake
	// has drained and the dispatch loop is being wound down. They are separate
	// because enqueue refuses work when stopped is set, and the recorder still
	// has to enqueue its backlog in between.
	stopping bool
	stopped  bool
	down     map[int64]*downState
	mutes    map[int64]time.Time
	// forgotten holds monitors that no longer exist as far as the scheduler is
	// concerned, so that an event already in flight for one cannot resurrect it.
	//
	// Forget clearing the maps was not enough. processDown re-creates d.down[id]
	// and re-arms its resend timer with NO liveness check, so an event still
	// sitting in the dispatch queue — or, much later, one replayed from the
	// durable outbox — rebuilt everything Forget had just removed. Nothing could
	// ever clear it again: the recovery that would close the outage needs a
	// check, and the monitor is unscheduled. The result was "still DOWN"
	// reminders for a monitor in the recycle bin, for the life of the process.
	//
	// Entries are removed by Reset, which is what a restore calls. A purged
	// monitor's id stays for the life of the process: one int64 per purge, and
	// its id can never be reissued.
	forgotten map[int64]struct{}

	// digestBuf collects pending digest items per channel; digestTimer is
	// armed while the buffer is non-empty.
	digestBuf   map[int64]*digestBuffer
	digestTimer Timer
}

// New builds a Dispatcher from opts. It does not consume anything until
// Start is called.
func New(opts Options) *Dispatcher {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Registry == nil {
		opts.Registry = registry.Default
	}
	if opts.Clock == nil {
		opts.Clock = realClock{}
	}
	if opts.SendTimeout <= 0 {
		opts.SendTimeout = DefaultSendTimeout
	}
	if opts.SendAttempts <= 0 {
		opts.SendAttempts = 1
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Dispatcher{
		log:          opts.Logger,
		store:        opts.Store,
		reg:          opts.Registry,
		clock:        opts.Clock,
		renderer:     NewRenderer(opts.Logger),
		sendTimeout:  opts.SendTimeout,
		sendAttempts: opts.SendAttempts,
		retryDelay:   opts.RetryDelay,
		quiet:        opts.QuietHours,
		digest:       opts.Digest,
		baseCtx:      ctx,
		cancel:       cancel,
		tasks:        make(chan func(context.Context), opts.QueueSize),
		quit:         make(chan struct{}),
		done:         make(chan struct{}),
		intake:       make(chan intakeItem, intakeBuffer),
		quitIntake:   make(chan struct{}),
		intakeDone:   make(chan struct{}),
		down:         make(map[int64]*downState),
		forgotten:    make(map[int64]struct{}),
		mutes:        make(map[int64]time.Time),
		digestBuf:    make(map[int64]*digestBuffer),
	}
}

// Start launches the recorder and dispatch goroutines.
func (d *Dispatcher) Start() {
	safego.Go(d.log, "outbox recorder", d.recordLoop)
	safego.Go(d.log, "dispatch loop", func() {
		defer close(d.done)
		for {
			select {
			case task := <-d.tasks:
				d.runTask(task)
			case <-d.quit:
				// Drain what was already accepted before exiting: those events
				// were taken on the promise of being delivered, and Stop's whole
				// contract is to drain rather than discard.
				for {
					select {
					case task := <-d.tasks:
						d.runTask(task)
					default:
						return
					}
				}
			}
		}
	})
	d.log.Info("notification dispatcher started",
		"providers", d.reg.List(),
		"digest", d.digest.Interval > 0,
		"quiet_hours", d.quiet != nil)
}

// runTask executes one queued task with its panic contained.
//
// Per task, not around the loop: a loop that stopped at the first panic would
// leave the process alive and notifications silently off, which is harder to
// notice than a crash and no better.
func (d *Dispatcher) runTask(task func(context.Context)) {
	_ = safego.Do(d.log, "notification task", func() { task(d.baseCtx) })
}

// Restore re-adopts outages that were already in progress when this process
// started. Call it once, after Start.
//
// The tracking map is in-memory only, so without this a restart during an
// outage loses it completely — and it is never rebuilt, because the engine
// emits no event while a monitor merely STAYS down (Transition reports
// transitioned=false for down→down). The eventual down→up recovery is then
// dropped by processRecovery as "nobody was alerted", so the one alert that
// matters most after a long outage never arrives, and no resend reminder does
// either. That is the failure this method exists to prevent.
//
// An escalation level whose delay already elapsed is treated as already sent
// alerted again: the operator was told when the outage began, and a restart is
// not news. Levels still ahead keep a timer for the delay that remains, and the
// resend reminder restarts from now.
func (d *Dispatcher) Restore(ctx context.Context) error {
	outages, err := d.store.OpenOutages(ctx)
	if err != nil {
		return fmt.Errorf("notify: load open outages: %w", err)
	}
	now := d.clock.Now()
	restored := 0
	for _, o := range outages {
		assignments, aerr := d.store.Assignments(ctx, o.MonitorID)
		if aerr != nil {
			// One unreadable monitor must not cost the others their tracking.
			d.log.Error("notify: load assignments during restore",
				"monitor_id", o.MonitorID, "error", aerr)
			continue
		}
		settings := d.monitorSettings(ctx, o.MonitorID, "")
		elapsed := now.Sub(o.Since)

		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			return nil
		}
		if _, live := d.down[o.MonitorID]; live {
			// A real DOWN event landed while this ran. It carries the more
			// accurate start and already armed its timers, so leave it alone.
			d.mu.Unlock()
			continue
		}
		// Who was actually told comes from the delivery audit log, not from
		// arithmetic on escalation delays.
		//
		// The in-memory record dies with the process, and the first version of
		// this marked every channel whose level had come due as notified. That
		// invents recipients: a channel added during the outage, or one whose
		// provider was failing the whole time, would later be sent a "back
		// online" for an outage it had never heard of — the same defect the
		// per-channel tracking elsewhere in this file exists to prevent, quietly
		// reintroduced by the restart path.
		st := &downState{since: o.Since, notified: map[int64]bool{}}
		if told, terr := d.store.ChannelsAwaitingRecovery(ctx, o.MonitorID); terr != nil {
			// Falling back to the old guess rather than to nobody: an unanswered
			// recovery is the failure this whole branch is about, and a spurious
			// one is a nuisance. The log says which happened.
			d.log.Error("notify: could not read who was alerted; falling back to "+
				"assuming every due channel was",
				"monitor_id", o.MonitorID, "error", terr)
			for _, a := range assignments {
				if a.EscalationAfter <= elapsed {
					st.notified[a.Notification.ID] = true
				}
			}
		} else {
			for _, id := range told {
				st.notified[id] = true
			}
		}
		d.down[o.MonitorID] = st
		if settings.ResendInterval > 0 {
			st.resendTimer = d.clock.AfterFunc(settings.ResendInterval, func() {
				d.enqueue(func(ctx context.Context) { d.processResend(ctx, o.MonitorID) })
			})
		}
		for _, delay := range escalationDelays(assignments) {
			if delay <= elapsed {
				// Its moment passed while we were away. No timer is armed for it:
				// if the escalation never completed, its outbox row survived the
				// restart and the sweeper replays it — which is a better answer than
				// re-deriving the level from elapsed time, because the row knows
				// whether the alert actually went out and arithmetic does not.
				continue
			}
			st.escTimers = append(st.escTimers, d.clock.AfterFunc(delay-elapsed, func() {
				d.submitEscalation(o.MonitorID, delay)
			}))
		}
		d.mu.Unlock()
		restored++
	}
	if restored > 0 {
		d.log.Info("adopted outages already in progress at startup", "monitors", restored)
	}
	return nil
}

// Stop drains the queue (bounded by ctx), then cancels in-flight sends.
func (d *Dispatcher) Stop(ctx context.Context) error {
	d.mu.Lock()
	if d.stopping {
		d.mu.Unlock()
		// A second caller waits for the first to finish, but not forever: the
		// first may itself have given up on its deadline, and then done never
		// closes. Blocking unconditionally here turned a slow shutdown into a
		// hung one.
		select {
		case <-d.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.stopping = true
	d.mu.Unlock()

	// However this returns, the dispatch loop gets told to stop. Leaving on the
	// timeout path below without closing quit left the loop running for the
	// life of the process and done never closing — which is also what made a
	// second Stop hang.
	defer d.stopDispatchLoop()

	// Step one: stop accepting, and let the recorder hand its backlog to a
	// dispatch loop that is still running. Doing this the other way round —
	// stopping the loop first — would leave accepted events sitting in the
	// outbox for the next process to find, which is recovery, not shutdown.
	//
	// The flag goes up under the lock BEFORE the channel closes, so submit and
	// the recorder agree on where the boundary is.
	d.mu.Lock()
	d.intakeClosed = true
	d.mu.Unlock()
	close(d.quitIntake)
	select {
	case <-d.intakeDone:
	case <-ctx.Done():
		d.cancel()
		return ctx.Err()
	}

	// Step one and a half: flush whatever the digest buffer still holds.
	//
	// The buffer lives only in memory and does NOT keep its outbox row — deliver
	// returns outcomeDone the moment an event is buffered, deliberately, because
	// keeping the row would make a retry re-add an event the buffer still has and
	// list the outage twice. The documented consequence is that a CRASH loses the
	// batch.
	//
	// A graceful stop is not a crash and had no business behaving like one. It
	// was worse, in fact: stopDispatchLoop stops the digest timer, so nothing
	// ever flushed, and the batch vanished without even the per-monitor "a DOWN
	// alert was lost" line that the failed-send path emits. Shutdown has a
	// deadline measured in seconds and a working dispatch loop; there is time to
	// send.
	//
	// Called directly rather than enqueued, which is the second attempt: enqueue
	// deliberately BLOCKS for a bounded wait when the queue is full, and a full
	// queue is exactly the state a shutdown finds itself in. Putting it on the
	// queue made Stop wait on the very backlog it is trying to drain, and
	// TestStopWhileEnqueueWaitsForRoomDoesNotPanic said so.
	//
	// Direct is safe: flushDigest swaps the buffer under the lock and sends
	// outside it, and sendOne is already called concurrently from fanOut's
	// goroutines. A timer-driven flush racing this one finds the buffer empty and
	// does nothing. The sends honour ctx, so the shutdown deadline still bounds
	// this.
	d.flushDigest(ctx, true)

	// Step two: stop the dispatch loop, which drains what it has been given.
	d.stopDispatchLoop()

	select {
	case <-d.done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel() // abort stragglers
		return ctx.Err()
	}
}

// stopDispatchLoop refuses further work and signals the loop to drain and
// exit. Idempotent, because Stop calls it both on its normal path and from a
// deferred call that covers every early return.
func (d *Dispatcher) stopDispatchLoop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.stopped = true
	for _, st := range d.down {
		st.stopTimers()
	}
	if d.digestTimer != nil {
		d.digestTimer.Stop()
	}
	close(d.quit)
}

// enqueue hands a task to the dispatch loop.
//
// The fast path never blocks: HandleStateChange runs on an engine worker, whose
// contract is to return immediately. But dropping straight to the floor on a
// full queue loses an ALERT, which is the product — and the queue fills exactly
// when it matters, because the dispatch loop is single-threaded and one slow
// provider holds it for the whole send timeout. So a full queue buys a bounded
// wait before giving up: one stalled worker for a few seconds beats a silently
// missing page. Only after that does the event go, and then loudly enough to be
// found afterwards.
func (d *Dispatcher) enqueue(task func(context.Context)) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	select {
	case d.tasks <- task:
		d.mu.Unlock()
		return
	default:
	}
	d.mu.Unlock()

	// Slow path, off the lock: blocking while holding d.mu would deadlock the
	// dispatch loop, which takes the same mutex to do its work.
	// Real time, deliberately, not d.clock: this is a backpressure valve, not
	// scheduled behaviour, and a test driving a fake clock must not have to
	// advance it to let an overflowing queue drain.
	d.log.Warn("notification queue full, waiting for room", "timeout", enqueueBlockTimeout)
	timer := time.NewTimer(enqueueBlockTimeout)
	defer timer.Stop()
	select {
	case d.tasks <- task:
	case <-d.quit:
		// Shutting down while we waited; the loop is on its way out and nothing
		// would run this task.
	case <-d.baseCtx.Done():
	case <-timer.C:
		d.log.Error("notification queue still full after waiting, DROPPING an alert event",
			"queue_size", cap(d.tasks), "waited", enqueueBlockTimeout)
	}
}

// ---------------------------------------------------------------------------
// Event intake
// ---------------------------------------------------------------------------

// intakeItem is one accepted event waiting to be recorded: what to write to
// the outbox, and what to run once it is written.
type intakeItem struct {
	rec OutboxRecord
	run func(context.Context) outcome
}

// submit records an event durably and then queues it for delivery.
//
// The caller is an engine worker or a sweeper goroutine, so this only does a
// channel send. The INSERT happens on recordLoop, which keeps the engine's
// "OnEvent must be fast and non-blocking" contract intact while still getting
// the event into the outbox BEFORE it reaches the dispatch queue — the ordering
// that makes a full dispatch queue survivable rather than lossy.
//
// A full intake buffer means the database cannot keep up with transitions. The
// send then blocks, which is backpressure onto the checkers: unpleasant, and
// still the right answer, because the alternative is discarding alerts at the
// exact moment a fleet-wide incident is producing them.
func (d *Dispatcher) submit(rec OutboxRecord, run func(context.Context) outcome) {
	// Acceptance is decided under the lock, not by reading a channel.
	//
	// Selecting on a closed quitIntake alongside a channel with room picks
	// between them at random, so an event could be accepted into an intake the
	// recorder had already stopped draining — recorded nowhere and delivered
	// never, which is the one outcome this whole path exists to prevent. Taking
	// the decision under the same mutex Stop uses makes it an ordering rather
	// than a coin toss.
	d.mu.Lock()
	if d.intakeClosed {
		d.mu.Unlock()
		return
	}
	d.inflight.Add(1)
	d.mu.Unlock()

	select {
	case d.intake <- intakeItem{rec: rec, run: run}:
		// recordAndQueue owns the matching Done.
	case <-d.baseCtx.Done():
		// Only reached once the process is being torn down hard, and only if
		// the buffer is full — which already means the database cannot keep up.
		d.log.Error("notify: dropping an accepted event during shutdown",
			"kind", rec.Kind, "monitor_id", rec.MonitorID)
		d.inflight.Done()
	}
}

// recordLoop writes accepted events to the outbox and hands them on. One
// goroutine, so events reach the dispatch queue in the order they arrived.
func (d *Dispatcher) recordLoop() {
	defer close(d.intakeDone)
	for {
		select {
		case it := <-d.intake:
			d.recordOne(it)
		case <-d.quitIntake:
			// Drain to empty. Complete, not best-effort: intakeClosed was set
			// under the mutex before this channel closed, so nothing can be
			// accepted after this point and an empty buffer really is the end.
			for {
				select {
				case it := <-d.intake:
					d.recordOne(it)
				default:
					return
				}
			}
		}
	}
}

// recordOne is recordAndQueue with its panic contained, so one unencodable
// event cannot stop the recorder for every later one.
func (d *Dispatcher) recordOne(it intakeItem) {
	_ = safego.Do(d.log, "outbox record", func() { d.recordAndQueue(it) })
}

// recordAndQueue writes one event to the outbox and queues its delivery.
//
// A failure to record is not fatal: delivery still proceeds, because a database
// hiccup must not become a second way to lose the alert. It costs the retry
// guarantee for that one event, and says so.
func (d *Dispatcher) recordAndQueue(it intakeItem) {
	defer d.inflight.Done()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.baseCtx), outboxWriteTimeout)
	id, err := d.store.EnqueueOutbox(ctx, it.rec, outboxLease)
	cancel()
	if err != nil {
		d.log.Error("notify: could not record the event for retry; delivering without a safety net",
			"kind", it.rec.Kind, "monitor_id", it.rec.MonitorID, "error", err)
	}
	d.enqueue(func(ctx context.Context) {
		if it.run(ctx) == outcomeDone {
			d.completeOutbox(id)
		}
		// outcomeRetry deliberately leaves the row. Its lease was set on
		// insert, so it becomes due once that lapses and the sweeper runs the
		// event again — which is the whole difference between an outbox and a
		// log of things that were attempted once.
	})
}

// HandleStateChange is wired to engine.Options.OnEvent. It is called
// synchronously from engine worker goroutines and returns immediately.
func (d *Dispatcher) HandleStateChange(ev domain.StateChange) {
	payload, err := json.Marshal(ev)
	if err != nil {
		d.log.Error("notify: cannot encode state change", "monitor_id", ev.MonitorID, "error", err)
		return
	}
	d.submit(
		OutboxRecord{Kind: OutboxStateChange, MonitorID: ev.MonitorID, Payload: payload},
		func(ctx context.Context) outcome { return d.processStateChange(ctx, ev) },
	)
}

// completeOutbox removes a finished event. id == 0 means it was never recorded.
func (d *Dispatcher) completeOutbox(id int64) {
	if id == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.baseCtx), outboxWriteTimeout)
	defer cancel()
	if err := d.store.CompleteOutbox(ctx, id); err != nil {
		// The row stays and will be retried, so the cost of this failure is a
		// duplicate alert rather than a missing one — the right way round.
		d.log.Error("notify: could not clear a delivered event from the outbox; "+
			"it may be delivered again", "outbox_id", id, "error", err)
	}
}

// RunOutbox retries events whose processing never completed, until ctx ends.
//
// It is what turns the outbox from a log into a guarantee. A row is present
// only while its event is unfinished, so the first sweep after a restart picks
// up exactly what the previous process was holding when it died — no separate
// crash-recovery path, no bookkeeping to get out of step.
//
// Retries are at-least-once by construction: a lease that lapses while a
// delivery is genuinely still running produces a duplicate alert. That is the
// deliberate direction to fail in for a monitoring system, and the lease is
// sized well above the dispatcher's own send budget so it is rare.
func (d *Dispatcher) RunOutbox(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultOutboxInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Pick up whatever the last process left behind. Contained like every other
	// sweep: a panic on the first one would otherwise mean no retries at all
	// for the life of the process.
	_ = safego.Do(d.log, "outbox sweep", func() { d.sweepOutbox(ctx) })
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = safego.Do(d.log, "outbox sweep", func() { d.sweepOutbox(ctx) })
		}
	}
}

// sweepOutbox runs one retry pass.
func (d *Dispatcher) sweepOutbox(ctx context.Context) {
	// Give up loudly before retrying: an event that has been claimed this often
	// is not going to succeed, and silently deleting an alert is the failure
	// this whole mechanism exists to prevent.
	if dropped, err := d.store.DropExhaustedOutbox(ctx, outboxMaxAttempts); err != nil {
		d.log.Error("notify: outbox cleanup failed", "error", err)
	} else {
		for _, item := range dropped {
			d.log.Error("notify: GIVING UP on an alert after repeated failures",
				"outbox_id", item.ID, "kind", item.Kind,
				"monitor_id", item.MonitorID, "attempts", item.Attempts)
			// Drop any half-resolved outage the abandoned row was the retry for.
			// Left behind, the `resolving` entry lives for the process's life and
			// is what a later resend or escalation would act on.
			if item.MonitorID != 0 {
				d.mu.Lock()
				if st, ok := d.down[item.MonitorID]; ok && st.resolving {
					st.stopTimers()
					delete(d.down, item.MonitorID)
				}
				d.mu.Unlock()
			}
		}
	}

	items, err := d.store.ClaimOutbox(ctx, outboxLease, outboxBatch)
	if err != nil {
		d.log.Error("notify: could not claim outbox work", "error", err)
		return
	}
	for _, item := range items {
		run, err := d.replay(item)
		if err != nil {
			// The payload cannot be decoded, so no number of retries will help.
			// Drop it, loudly — a row that can never succeed would otherwise sit
			// there consuming a claim every sweep until the attempt budget runs
			// out, and the reason would never be stated.
			d.log.Error("notify: unreadable outbox row, discarding",
				"outbox_id", item.ID, "kind", item.Kind, "error", err)
			d.completeOutbox(item.ID)
			continue
		}
		d.log.Warn("notify: retrying an alert that did not complete",
			"outbox_id", item.ID, "kind", item.Kind,
			"monitor_id", item.MonitorID, "attempt", item.Attempts)
		// Through the dispatch queue, not called here. The dispatch loop is
		// single-threaded and everything it runs assumes that: the outage map,
		// the digest buffer and the escalation timers are all reached without a
		// lock in places. Running a retry on the sweeper's goroutine instead
		// would race every one of them.
		id := item.ID
		d.enqueue(func(ctx context.Context) {
			if run(ctx) == outcomeDone {
				d.completeOutbox(id)
			}
		})
	}
}

// replay turns a stored row back into the work it stands for.
func (d *Dispatcher) replay(item OutboxItem) (func(context.Context) outcome, error) {
	switch item.Kind {
	case OutboxStateChange:
		var ev domain.StateChange
		if err := json.Unmarshal(item.Payload, &ev); err != nil {
			return nil, err
		}
		return func(ctx context.Context) outcome { return d.processStateChange(ctx, ev) }, nil
	case OutboxCertExpiry:
		var ev CertExpiryEvent
		if err := json.Unmarshal(item.Payload, &ev); err != nil {
			return nil, err
		}
		return func(ctx context.Context) outcome { return d.processCertExpiry(ctx, ev) }, nil
	case OutboxMaintenance:
		var ev MaintenanceEvent
		if err := json.Unmarshal(item.Payload, &ev); err != nil {
			return nil, err
		}
		return func(ctx context.Context) outcome { return d.processMaintenance(ctx, ev) }, nil
	case OutboxEscalation:
		var ev escalationEvent
		if err := json.Unmarshal(item.Payload, &ev); err != nil {
			return nil, err
		}
		return func(ctx context.Context) outcome {
			return d.processEscalation(ctx, ev.MonitorID, ev.Delay)
		}, nil
	default:
		return nil, fmt.Errorf("unknown outbox kind %q", item.Kind)
	}
}

// escalationEvent is the outbox payload for one escalation level. The level is
// carried rather than re-derived, so a replay escalates the level whose timer
// actually fired even if the assignments have changed since.
type escalationEvent struct {
	MonitorID int64         `json:"monitor_id"`
	Delay     time.Duration `json:"delay_ns"`
}

// submitEscalation records an escalation level durably, then queues it.
//
// Through submit rather than enqueue, which is the whole fix: an escalation used
// to be enqueued directly with its outcome thrown away, making it the one alert
// path in this package with no second chance. A 503 from the on-call provider
// lost the page for good — the level was flagged as attempted even though nothing
// was reached, AfterFunc had already fired, and Restore treats a level whose
// delay has elapsed as done, so not even a restart retried it. Meanwhile the
// level-0 alert for the same outage was being retried ten times from the outbox.
func (d *Dispatcher) submitEscalation(monitorID int64, delay time.Duration) {
	payload, err := json.Marshal(escalationEvent{MonitorID: monitorID, Delay: delay})
	if err != nil {
		d.log.Error("notify: cannot encode escalation",
			"monitor_id", monitorID, "delay", delay, "error", err)
		return
	}
	d.submit(
		OutboxRecord{Kind: OutboxEscalation, MonitorID: monitorID, Payload: payload},
		func(ctx context.Context) outcome { return d.processEscalation(ctx, monitorID, delay) },
	)
}

// HandleCertExpiry is wired to the sslcheck sweeper's OnEvent.
func (d *Dispatcher) HandleCertExpiry(ev CertExpiryEvent) {
	payload, err := json.Marshal(ev)
	if err != nil {
		d.log.Error("notify: cannot encode cert expiry", "monitor_id", ev.MonitorID, "error", err)
		return
	}
	d.submit(
		OutboxRecord{Kind: OutboxCertExpiry, MonitorID: ev.MonitorID, Payload: payload},
		func(ctx context.Context) outcome { return d.processCertExpiry(ctx, ev) },
	)
}

// MaintenanceEvent is a maintenance-window start or end notice.
type MaintenanceEvent struct {
	MonitorID   int64     `json:"monitor_id"`
	MonitorName string    `json:"monitor_name"`
	Start       bool      `json:"start"`
	At          time.Time `json:"at"`
}

// HandleMaintenance delivers maintenance start/end notices for a monitor.
// The maintenance scheduler (a later work package) calls this at window
// boundaries.
func (d *Dispatcher) HandleMaintenance(monitorID int64, monitorName string, start bool, at time.Time) {
	ev := MaintenanceEvent{MonitorID: monitorID, MonitorName: monitorName, Start: start, At: at}
	payload, err := json.Marshal(ev)
	if err != nil {
		d.log.Error("notify: cannot encode maintenance notice", "monitor_id", monitorID, "error", err)
		return
	}
	d.submit(
		OutboxRecord{Kind: OutboxMaintenance, MonitorID: monitorID, Payload: payload},
		func(ctx context.Context) outcome { return d.processMaintenance(ctx, ev) },
	)
}

// processMaintenance delivers one maintenance notice.
func (d *Dispatcher) processMaintenance(ctx context.Context, me MaintenanceEvent) outcome {
	if d.isForgotten(me.MonitorID) {
		return outcomeDone
	}
	kind := domain.EventKindMaintenanceEnd
	if me.Start {
		kind = domain.EventKindMaintenanceStart
	}
	assignments, err := d.store.Assignments(ctx, me.MonitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", me.MonitorID, "error", err)
		return outcomeRetry
	}
	_, res := d.deliver(ctx, notificationsOf(assignments), domain.NotificationEvent{
		Kind:        kind,
		MonitorID:   me.MonitorID,
		MonitorName: me.MonitorName,
		At:          me.At,
	})
	return res
}

// Mute snoozes a monitor's alerts until the given time (auto-expiry).
func (d *Dispatcher) Mute(monitorID int64, until time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mutes[monitorID] = until
	d.log.Info("monitor muted", "monitor_id", monitorID, "until", until)
}

// Unmute lifts a monitor's snooze immediately.
func (d *Dispatcher) Unmute(monitorID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.mutes, monitorID)
}

// mutedUntil reports an active snooze; expired entries are cleaned lazily.
func (d *Dispatcher) mutedUntil(monitorID int64, now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	until, ok := d.mutes[monitorID]
	if !ok {
		return time.Time{}, false
	}
	if !until.After(now) {
		delete(d.mutes, monitorID) // auto-expired
		return time.Time{}, false
	}
	return until, true
}

// TestSend delivers a test message on a channel synchronously ("does this
// config work?"). It bypasses digest batching, quiet hours and mutes, and
// records the outcome in the audit log like any other delivery.
func (d *Dispatcher) TestSend(ctx context.Context, n Notification) error {
	d.mu.Lock()
	stopped := d.stopped
	d.mu.Unlock()
	if stopped {
		return ErrStopped
	}
	ev := domain.NotificationEvent{
		Kind: domain.EventKindTest,
		At:   d.clock.Now(),
	}
	return d.sendOne(ctx, n, ev)
}

// ---------------------------------------------------------------------------
// State-change processing: down / up, resend, escalation
// ---------------------------------------------------------------------------

func (d *Dispatcher) processStateChange(ctx context.Context, ev domain.StateChange) outcome {
	// A latency anomaly is not a transition. It arrives as up→up with the anomaly
	// payload attached, so it has to be routed before the switch below — which
	// filed it under "state change without alert" and dropped it, leaving the
	// whole feature detected, logged, recorded in the outbox and then discarded.
	//
	// A monitor that has been binned or purged has nothing to say, and saying it
	// anyway is worse than silence: the alert names something the operator can no
	// longer find. Returning outcomeDone also lets the caller complete the outbox
	// row, so a durable event for a deleted monitor is cleaned up rather than
	// replayed every sweep until its attempt budget runs out.
	//
	// Checked BEFORE the anomaly branch. Below it, every latency anomaly for a
	// binned monitor was still delivered — the one event kind that routes away
	// before the switch.
	if d.isForgotten(ev.MonitorID) {
		d.log.Info("alert dropped: monitor is deleted",
			"monitor_id", ev.MonitorID, "name", ev.MonitorName, "to", ev.To)
		return outcomeDone
	}
	if ev.Anomaly != nil {
		return d.processAnomaly(ctx, ev)
	}
	// Same reason as the anomaly: a flapping summary is up→up or down→down, and
	// the switch below would file it as a transition without an alert.
	if ev.Flapping != nil {
		return d.processFlapping(ctx, ev)
	}
	if ev.Suppressed {
		d.log.Info("alert suppressed",
			"monitor_id", ev.MonitorID, "name", ev.MonitorName,
			"from", ev.From, "to", ev.To, "reason", ev.SuppressReason)
		// A held-back change of a FLAPPING monitor keeps an announced outage
		// open. The engine resolves it once the flapping settles; clearing it
		// here would forget who was told DOWN, and the recovery that follows
		// would find nobody to tell.
		if ev.To != domain.StatusDown && ev.SuppressReason != domain.SuppressFlapping {
			d.clearDown(ev.MonitorID)
		}
		return outcomeDone
	}

	switch {
	case ev.To == domain.StatusDown:
		return d.processDown(ctx, ev)
	case ev.To == domain.StatusUp && ev.From == domain.StatusDown:
		return d.processRecovery(ctx, ev)
	default:
		// up→pending, pending→up (first success after start), … — the
		// state machine's intermediate steps never alert.
		if ev.To == domain.StatusUp {
			d.clearDown(ev.MonitorID)
		}
		d.log.Debug("state change without alert",
			"monitor_id", ev.MonitorID, "from", ev.From, "to", ev.To)
		return outcomeDone
	}
}

func (d *Dispatcher) processDown(ctx context.Context, sc domain.StateChange) outcome {
	now := d.clock.Now()
	settings := d.monitorSettings(ctx, sc.MonitorID, sc.MonitorName)
	assignments, err := d.store.Assignments(ctx, sc.MonitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", sc.MonitorID, "error", err)
		return outcomeRetry
	}

	// The outage began when the ENGINE said so, not when this function runs.
	//
	// They used to be the same instant. Now that a failed delivery is retried
	// from the outbox, this can run half an hour after the transition, and
	// taking the clock here would move the outage start forward on every
	// attempt — so the eventual recovery would report an outage far shorter
	// than the one that happened.
	startedAt := sc.At
	if startedAt.IsZero() {
		startedAt = now
	}

	// An existing entry means this is the SAME outage arriving again: a retry
	// from the outbox, a duplicate DOWN after an engine restart, or a row
	// adopted by Restore. It is carried forward rather than replaced.
	//
	// Replacing it discarded who had already been told, so every retry
	// re-alerted the channels that had succeeded: one permanently broken
	// provider turned into ten duplicate DOWN messages on every working
	// channel, which is its own kind of outage. It also restarted the resend
	// and escalation timers from the retry, pushing escalation later each time.
	//
	// Only the channels that have not heard yet are targeted below, which is
	// also what makes the retry terminate: once everyone has been reached there
	// is nothing left to send and the event is finished.
	d.mu.Lock()
	st, resumed := d.down[sc.MonitorID]
	if resumed && st.resolving {
		// The previous outage was over; its resolve just never got out. This is
		// a NEW outage, and inheriting the old `notified` set would make it the
		// silent one — every channel already marked as told.
		st.stopTimers()
		delete(d.down, sc.MonitorID)
		st, resumed = nil, false
	}
	if !resumed {
		// notified starts EMPTY. A channel is recorded only once its message is
		// out, further down: marking level 0 up front meant a DOWN swallowed by
		// a mute, by quiet hours or by a failing provider still resolved to
		// those channels later, so people were told "back online" about an
		// outage they were never told about.
		st = &downState{since: startedAt, notified: map[int64]bool{}}
		d.down[sc.MonitorID] = st
		// Timers are armed even when there are no immediate channels — an
		// escalation-only setup must still fire.
		if !d.stopped {
			if settings.ResendInterval > 0 {
				st.resendTimer = d.clock.AfterFunc(settings.ResendInterval, func() {
					d.enqueue(func(ctx context.Context) { d.processResend(ctx, sc.MonitorID) })
				})
			}
			for _, delay := range escalationDelays(assignments) {
				st.escTimers = append(st.escTimers, d.clock.AfterFunc(delay, func() {
					d.submitEscalation(sc.MonitorID, delay)
				}))
			}
		}
	}
	already := maps.Clone(st.notified)
	d.mu.Unlock()

	var targets []Notification
	for _, n := range notificationsAtLevel(assignments, 0) {
		if !already[n.ID] {
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		return outcomeDone
	}
	ev := d.buildMonitorEvent(domain.EventKindDown, sc.MonitorID, settings, sc.Message, sc.At)
	ev.Status = domain.StatusDown
	reached, res := d.deliver(ctx, targets, ev)
	d.markNotified(sc.MonitorID, st, reached)
	return res
}

// markNotified records WHICH CHANNELS an alert actually reached, so the
// recovery and the resend reminder go to exactly those and to nobody else.
//
// st is re-checked against the live map: the outage may have been resolved, or
// re-tracked as a new one, while the delivery was in flight.
func (d *Dispatcher) markNotified(monitorID int64, st *downState, reached []int64) {
	if len(reached) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, ok := d.down[monitorID]
	if !ok || cur != st {
		return
	}
	for _, id := range reached {
		st.notified[id] = true
	}
}

func (d *Dispatcher) processRecovery(ctx context.Context, sc domain.StateChange) outcome {
	d.mu.Lock()
	st, tracked := d.down[sc.MonitorID]
	var notified map[int64]bool
	var since time.Time
	if tracked {
		// Timers stop immediately — no more "still down" reminders for a monitor
		// that is back — but the entry itself stays until the resolve has been
		// delivered, so a retry can still find out who was owed one.
		st.stopTimers()
		st.resolving = true
		// Snapshot under the lock rather than reading st.notified further down.
		// Every writer used to be the single dispatch goroutine, which made an
		// unlocked read safe; Restore broke that invariant by writing the map
		// from the CALLER's goroutine at startup. Reading it unlocked is a data
		// race from that point on — and one `go test -race` catches.
		notified = maps.Clone(st.notified)
		since = st.since
	}
	d.mu.Unlock()
	if !tracked {
		// Nobody was alerted about this outage, so there is nothing to resolve.
		// Reachable when the outage was suppressed (maintenance, parent down) or
		// when it began before this process and Restore could not adopt it. It
		// used to return silently, which made a missing recovery alert leave no
		// trace at all — the one thing an operator needs to see.
		d.log.Warn("recovery without a tracked outage: no resolve notification sent",
			"monitor_id", sc.MonitorID, "name", sc.MonitorName, "from", sc.From)
		return outcomeDone
	}

	settings := d.monitorSettings(ctx, sc.MonitorID, sc.MonitorName)
	assignments, err := d.store.Assignments(ctx, sc.MonitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", sc.MonitorID, "error", err)
		return outcomeRetry
	}
	// Resolve only towards channels that were alerted about the outage.
	var targets []Notification
	for _, a := range assignments {
		if notified[a.Notification.ID] {
			targets = append(targets, a.Notification)
		}
	}
	if len(targets) == 0 {
		d.clearDown(sc.MonitorID)
		return outcomeDone
	}
	ev := d.buildMonitorEvent(domain.EventKindUp, sc.MonitorID, settings, sc.Message, sc.At)
	ev.Status = domain.StatusUp
	ev.DownSince = &since
	reached, res := d.deliver(ctx, targets, ev)
	if res == outcomeDone {
		// Delivered: only now is the outage finished with.
		d.clearDown(sc.MonitorID)
		return res
	}
	// A PARTIAL resolve: drop the channels that did hear it, so the retry
	// targets only the rest. Without this every sweep re-sent "back online" to
	// the channels that already had it, up to the attempt budget — processDown
	// and processEscalation both narrow their targets this way and this was the
	// one path that did not.
	if len(reached) > 0 {
		d.mu.Lock()
		if st, ok := d.down[sc.MonitorID]; ok {
			for _, id := range reached {
				delete(st.notified, id)
			}
		}
		d.mu.Unlock()
	}
	return res
}

// processResend repeats the DOWN notification while the outage lasts.
func (d *Dispatcher) processResend(ctx context.Context, monitorID int64) {
	now := d.clock.Now()
	d.mu.Lock()
	st, stillDown := d.down[monitorID]
	// A `resolving` entry is an outage that is OVER — it survives only so the
	// unfinished "back online" alert can be retried. Treating it as live meant
	// reminding people that a monitor is still down after it came back (and
	// escalating it), while re-arming the timer on the same entry, so the
	// reminders sustained themselves for the life of the process.
	if stillDown && st.resolving {
		stillDown = false
	}
	d.mu.Unlock()
	if !stillDown {
		return
	}

	settings := d.monitorSettings(ctx, monitorID, "")
	assignments, err := d.store.Assignments(ctx, monitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", monitorID, "error", err)
		return
	}

	d.mu.Lock()
	// The live entry has to be the one this task captured, not merely SOME entry
	// for the monitor — the check markNotified already makes.
	//
	// Testing only for presence meant a task that outlived its own outage re-armed
	// the timer on the orphaned downState while the outage that replaced it kept
	// the timer it armed for itself, leaving two live reminder chains that each
	// re-armed one of them: duplicate DOWN reminders for the rest of the outage.
	// Everything below is derived from st, too, so a stale st reminds the wrong
	// channels about the wrong outage.
	//
	// Reachable because the resend timer calls enqueue directly while a state
	// change goes through submit → recordLoop → an outbox INSERT first, so the
	// two do not queue in event order; a full queue makes enqueue block for up to
	// enqueueBlockTimeout, which is the window.
	if cur, ok := d.down[monitorID]; !ok || cur != st {
		d.mu.Unlock()
		return
	}
	// Re-arm first so a delivery error can never break the reminder loop.
	if !d.stopped && settings.ResendInterval > 0 {
		st.resendTimer = d.clock.AfterFunc(settings.ResendInterval, func() {
			d.enqueue(func(ctx context.Context) { d.processResend(ctx, monitorID) })
		})
	}
	// Remind every level that has already been alerted.
	var targets []Notification
	for _, a := range assignments {
		if st.notified[a.Notification.ID] {
			targets = append(targets, a.Notification)
		}
	}
	since := st.since
	d.mu.Unlock()

	if len(targets) == 0 {
		return
	}
	ev := d.buildMonitorEvent(domain.EventKindDown, monitorID, settings, "", now)
	ev.Status = domain.StatusDown
	ev.Reminder = true
	ev.DownSince = &since
	d.deliver(ctx, targets, ev)
}

// processEscalation alerts the channels of one escalation level once the monitor
// has been down for their configured delay.
//
// It mirrors processDown, including the retry contract: it targets only the
// channels on this level that have not heard yet, and reports whether every one
// of them was reached. That is what lets the outbox run it again — and what makes
// the retry terminate, since a channel reached on an earlier attempt drops out of
// the target set.
func (d *Dispatcher) processEscalation(ctx context.Context, monitorID int64, delay time.Duration) outcome {
	now := d.clock.Now()
	d.mu.Lock()
	st, stillDown := d.down[monitorID]
	// A `resolving` entry is an outage that is OVER — it survives only so the
	// unfinished "back online" alert can be retried. Treating it as live meant
	// reminding people that a monitor is still down after it came back (and
	// escalating it), while re-arming the timer on the same entry, so the
	// reminders sustained themselves for the life of the process.
	if stillDown && st.resolving {
		stillDown = false
	}
	if !stillDown {
		d.mu.Unlock()
		// The outage ended (or was never adopted). There is nothing to escalate,
		// and a retry would only ask the same question again, so the row goes.
		//
		// This relies on Restore having run before the first sweep, which is how
		// main.go orders them: adopting outages first means a replayed escalation
		// finds its outage instead of concluding it is over.
		return outcomeDone
	}
	since := st.since
	already := maps.Clone(st.notified)
	d.mu.Unlock()

	settings := d.monitorSettings(ctx, monitorID, "")
	assignments, err := d.store.Assignments(ctx, monitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", monitorID, "error", err)
		return outcomeRetry
	}
	var targets []Notification
	for _, n := range notificationsAtLevel(assignments, delay) {
		if !already[n.ID] {
			targets = append(targets, n)
		}
	}
	if len(targets) == 0 {
		return outcomeDone
	}
	ev := d.buildMonitorEvent(domain.EventKindDown, monitorID, settings, "", now)
	ev.Status = domain.StatusDown
	ev.Escalation = true
	ev.DownSince = &since
	reached, res := d.deliver(ctx, targets, ev)
	d.markNotified(monitorID, st, reached)
	return res
}

// processAnomaly delivers a response-time anomaly, or its recovery.
//
// Every assigned channel is targeted, like a certificate warning and unlike a
// DOWN: escalation levels exist to wake somebody up for an outage, and this is
// explicitly not one. Nothing is recorded in downState either — the monitor is
// UP throughout, so there is no outage to resolve later and an anomaly must never
// make a channel eligible for a recovery notification.
func (d *Dispatcher) processAnomaly(ctx context.Context, sc domain.StateChange) outcome {
	if sc.Suppressed {
		// A maintenance window silences a slow-response alert for the same reason
		// it silences a DOWN: the operator already knows.
		d.log.Info("latency anomaly alert suppressed",
			"monitor_id", sc.MonitorID, "name", sc.MonitorName,
			"cleared", sc.Anomaly.Cleared, "reason", sc.SuppressReason)
		return outcomeDone
	}
	kind := domain.EventKindLatencyAnomaly
	if sc.Anomaly.Cleared {
		kind = domain.EventKindLatencyNormal
	}
	assignments, err := d.store.Assignments(ctx, sc.MonitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", sc.MonitorID, "error", err)
		return outcomeRetry
	}
	settings := d.monitorSettings(ctx, sc.MonitorID, sc.MonitorName)
	ev := d.buildMonitorEvent(kind, sc.MonitorID, settings, sc.Message, sc.At)
	ev.Status = domain.StatusUp
	latency := sc.Anomaly.Latency
	ev.Latency = &latency
	ev.Anomaly = sc.Anomaly
	_, res := d.deliver(ctx, notificationsOf(assignments), ev)
	return res
}

// processFlapping delivers a flapping summary: the onset, which replaces the
// individual up/down alerts of a monitor changing state too often, or the
// settled report once it has held one state for a whole window.
//
// Every assigned channel is targeted, as for a latency anomaly: the summary
// stands in for alerts ALL level-0 channels would otherwise have received, and
// escalation is timed off real outages, which the engine still announces (or
// resolves) on its own once the flapping settles. Nothing is recorded in
// downState for the same reason.
func (d *Dispatcher) processFlapping(ctx context.Context, sc domain.StateChange) outcome {
	if sc.Suppressed {
		d.log.Info("flapping alert suppressed",
			"monitor_id", sc.MonitorID, "name", sc.MonitorName,
			"ended", sc.Flapping.Ended, "reason", sc.SuppressReason)
		return outcomeDone
	}
	kind := domain.EventKindFlapping
	if sc.Flapping.Ended {
		kind = domain.EventKindFlappingStable
	}
	assignments, err := d.store.Assignments(ctx, sc.MonitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", sc.MonitorID, "error", err)
		return outcomeRetry
	}
	settings := d.monitorSettings(ctx, sc.MonitorID, sc.MonitorName)
	ev := d.buildMonitorEvent(kind, sc.MonitorID, settings, sc.Message, sc.At)
	ev.Status = sc.Flapping.Status
	ev.Flapping = sc.Flapping
	_, res := d.deliver(ctx, notificationsOf(assignments), ev)
	return res
}

func (d *Dispatcher) processCertExpiry(ctx context.Context, ce CertExpiryEvent) outcome {
	// The tombstone applies to every alert kind, not just state changes. A
	// certificate warning for a monitor in the recycle bin names something the
	// operator cannot act on, and the event is durable so it would be replayed.
	if d.isForgotten(ce.MonitorID) {
		return outcomeDone
	}
	assignments, err := d.store.Assignments(ctx, ce.MonitorID)
	if err != nil {
		d.log.Error("notify: load assignments", "monitor_id", ce.MonitorID, "error", err)
		return outcomeRetry
	}
	settings := d.monitorSettings(ctx, ce.MonitorID, ce.MonitorName)
	msg := ""
	if ce.Issuer != "" {
		msg = "issued by " + ce.Issuer
	}
	ev := d.buildMonitorEvent(domain.EventKindCertExpiry, ce.MonitorID, settings, msg, d.clock.Now())
	if ev.MonitorName == "" {
		ev.MonitorName = ce.MonitorName
	}
	ev.CertDaysRemaining = ce.DaysRemaining
	ev.CertExpiresAt = ce.ValidTo
	_, res := d.deliver(ctx, notificationsOf(assignments), ev)
	return res
}

// Reset drops the dispatcher's per-monitor state for a monitor that is STILL
// there — one that was paused, or brought back from the recycle bin.
//
// Same clearing as Forget, without the tombstone: this monitor may legitimately
// alert again at any moment, so an event for it must not be dropped.
//
// Pausing needs this. Engine.Pause deliberately emits no event (a pause is not
// an outage), so nothing ever closed the downState of a monitor that was DOWN
// when it was paused — its resend timer went on announcing an outage on a
// monitor nobody was checking, and resuming it later met the swallowed-first-
// alert path, because `notified` was still full.
func (d *Dispatcher) Reset(monitorID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.forgotten, monitorID)
	d.clearDownLocked(monitorID)
	delete(d.mutes, monitorID)
}

// Forget drops every piece of per-monitor state the dispatcher holds, for a
// monitor that no longer exists as far as the scheduler is concerned.
//
// Deleting a monitor stops its checks, and stopping the checks means no
// recovery event will ever arrive to close an outage that was open at the
// moment of the delete. The downState survived, and with it its resend timer
// and its `notified` set. Two things followed. The resend timer kept firing
// "still down" alerts for a monitor sitting in the bin. And if that monitor was
// later RESTORED and went down again, processDown found the stale entry, saw
// every channel already in `notified`, and delivered nothing at all: the one
// alert that mattered, silently swallowed, with no error anywhere to show for
// it.
//
// The mute goes too. A mute is a snooze on a specific monitor; carrying it
// across a delete and a restore means a monitor comes back from the bin still
// silenced by a timer nobody remembers setting.
//
// Safe to call for a monitor that holds no state, which is the common case.
func (d *Dispatcher) Forget(monitorID int64) {
	// ONE critical section, with the tombstone set FIRST.
	//
	// Three separate acquisitions left a window between clearing the state and
	// recording that the monitor is gone. A processDown already sitting in the
	// dispatch queue could run inside it, rebuild d.down and re-arm the resend
	// timer, and the tombstone then landed too late to stop the reminders —
	// which is the failure this function exists to prevent, reintroduced by the
	// order of its own statements.
	d.mu.Lock()
	defer d.mu.Unlock()
	d.forgotten[monitorID] = struct{}{}
	d.clearDownLocked(monitorID)
	delete(d.mutes, monitorID)
}

// isForgotten reports whether this monitor has been binned or purged.
func (d *Dispatcher) isForgotten(monitorID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.forgotten[monitorID]
	return ok
}

// clearDown forgets an outage and stops its timers.
func (d *Dispatcher) clearDown(monitorID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clearDownLocked(monitorID)
}

// clearDownLocked is clearDown for a caller that already holds d.mu.
func (d *Dispatcher) clearDownLocked(monitorID int64) {
	if st, ok := d.down[monitorID]; ok {
		st.stopTimers()
		delete(d.down, monitorID)
	}
}

// monitorSettings loads the monitor's notification settings, degrading to a
// name-only stub when the store fails (an alert with less context still
// beats no alert).
func (d *Dispatcher) monitorSettings(ctx context.Context, monitorID int64, fallbackName string) MonitorSettings {
	settings, err := d.store.MonitorSettings(ctx, monitorID)
	if err != nil {
		d.log.Error("notify: load monitor settings", "monitor_id", monitorID, "error", err)
		return MonitorSettings{Name: fallbackName}
	}
	if settings.Name == "" {
		settings.Name = fallbackName
	}
	return settings
}

func (d *Dispatcher) buildMonitorEvent(kind domain.NotificationEventKind, monitorID int64, settings MonitorSettings, message string, at time.Time) domain.NotificationEvent {
	return domain.NotificationEvent{
		Kind:        kind,
		MonitorID:   monitorID,
		MonitorName: settings.Name,
		MonitorURL:  settings.URL,
		Message:     message,
		At:          at,
	}
}

// ---------------------------------------------------------------------------
// Delivery: mute → digest → quiet hours → concurrent fan-out
// ---------------------------------------------------------------------------

// deliver applies the policy layers and fans the event out to the targets. It
// reports whether the event actually reached a channel — the caller uses that
// to decide which channels count as notified (see downState.notified).
//
// Buffering into a digest counts: the item is queued and will go out. Mute,
// quiet hours and a fan-out where every provider failed do NOT: nobody was
// told, so recording the level as alerted would later resolve an outage to
// people who never heard it had started.
// It returns the channels actually reached, and whether the event is finished
// with. The two are not the same question: a muted monitor reaches nobody and
// is nonetheless finished, while a failing provider reaches nobody and must be
// tried again. Only the second is a reason to keep the outbox row.
func (d *Dispatcher) deliver(
	ctx context.Context, targets []Notification, ev domain.NotificationEvent,
) ([]int64, outcome) {
	if len(targets) == 0 {
		return nil, outcomeDone
	}
	now := d.clock.Now()
	if ev.MonitorID != 0 {
		if until, muted := d.mutedUntil(ev.MonitorID, now); muted {
			d.log.Info("alert skipped: monitor muted",
				"monitor_id", ev.MonitorID, "kind", ev.Kind, "muted_until", until)
			// A decision not to send, not a failure. Retrying would only wait
			// for the mute to expire and then deliver a stale alert.
			return nil, outcomeDone
		}
	}
	if d.digest.Interval > 0 && ev.Kind != domain.EventKindDigest {
		d.digestAdd(targets, ev)
		// Buffered is NOT reached. The message has not gone anywhere yet, and
		// treating the buffer as delivery meant a failed flush left the channel
		// marked as having heard about an outage it was never told about —
		// which then earned it a "back online" for something it never saw.
		// flushDigest marks these channels once the batch actually leaves.
		//
		// It counts as DONE for the outbox, and that is a real limitation
		// rather than a claim: the buffer is in memory, so a crash before the
		// flush loses the batch. Keeping the row would be worse, not better —
		// the retry would re-add an event the buffer still holds and the digest
		// would list the outage twice. Making digest mode durable means
		// persisting the buffer. A lost batch is at least logged per monitor.
		return nil, outcomeDone
	}
	if d.quiet != nil && d.quiet.Contains(now) {
		d.log.Info("alert skipped: quiet hours",
			"monitor_id", ev.MonitorID, "kind", ev.Kind)
		return nil, outcomeDone
	}
	reached := d.fanOut(ctx, targets, ev)
	if len(reached) < len(targets) {
		// At least one intended recipient did not get it.
		return reached, outcomeRetry
	}
	return reached, outcomeDone
}

// fanOut sends ev to every target concurrently and reports whether at least one
// delivery succeeded. Each channel is isolated: a slow or failing provider
// affects only its own delivery.
func (d *Dispatcher) fanOut(ctx context.Context, targets []Notification, ev domain.NotificationEvent) []int64 {
	var g errgroup.Group
	// One slot per target, written only by that target's own goroutine, so the
	// result needs no lock. A failed send leaves its slot at zero.
	reached := make([]int64, len(targets))
	for i, n := range targets {
		g.Go(func() error {
			err := d.sendOne(ctx, n, ev)
			if err == nil {
				reached[i] = n.ID
			}
			return err
		})
	}
	if err := g.Wait(); err != nil {
		d.log.Warn("one or more notification deliveries failed",
			"kind", ev.Kind, "monitor_id", ev.MonitorID, "error", err)
	}
	out := make([]int64, 0, len(reached))
	for _, id := range reached {
		if id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// safeSend performs one provider send and turns a panic inside it into an
// ordinary error.
//
// fanOut runs sends in an errgroup, which does not recover, and nothing above
// it does either — so a panic in any of the nineteen providers ended the
// process. The observable result was worse than a crash on its own: the
// channels that had already succeeded were delivered, then the monitoring
// system disappeared, with no final log line saying why. A provider returning
// malformed data is exactly the moment the component whose job is to raise the
// alarm must still be running.
//
// It becomes an ERROR, deliberately, not a silent success: the caller records a
// failed delivery, the channel is not counted as reached, and the outbox
// retries it. Swallowing the panic would mark the channel notified and later
// resolve a recovery to somebody who never heard about the outage.
func (d *Dispatcher) safeSend(
	ctx context.Context, notifier registry.Notifier, n Notification, ev domain.NotificationEvent,
) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("provider panicked: %v", rec)
			d.log.Error("notification provider panicked",
				"notification_id", n.ID, "type", n.Type,
				"recover", rec, "stack", string(debug.Stack()))
		}
	}()
	return notifier.Send(ctx, n.Config, ev)
}

// sendOne renders, sends (with retries) and records one delivery.
func (d *Dispatcher) sendOne(ctx context.Context, n Notification, ev domain.NotificationEvent) error {
	started := d.clock.Now()
	ev.Title, ev.Body = d.renderer.Render(ev, n.Templates)

	var err error
	attempts := 0
	notifier, ok := d.reg.Get(n.Type)
	if !ok {
		err = fmt.Errorf("unknown provider type %q", n.Type)
	} else {
		for attempts < d.sendAttempts {
			attempts++
			sendCtx, cancel := context.WithTimeout(ctx, d.sendTimeout)
			err = d.safeSend(sendCtx, notifier, n, ev)
			cancel()
			if err == nil || ctx.Err() != nil {
				break
			}
			if attempts < d.sendAttempts && d.retryDelay > 0 {
				if serr := d.sleep(ctx, d.retryDelay); serr != nil {
					break
				}
			}
		}
	}

	delivery := Delivery{
		NotificationID: n.ID,
		MonitorID:      ev.MonitorID,
		ProviderType:   n.Type,
		EventKind:      ev.Kind,
		Status:         DeliverySuccess,
		Attempts:       attempts,
		StartedAt:      started,
		FinishedAt:     d.clock.Now(),
	}
	if err != nil {
		delivery.Status = DeliveryFailed
		delivery.Error = err.Error()
		d.log.Error("notification delivery failed",
			"notification_id", n.ID, "name", n.Name, "type", n.Type,
			"kind", ev.Kind, "monitor_id", ev.MonitorID,
			"attempts", attempts, "error", err)
	} else {
		d.log.Info("notification delivered",
			"notification_id", n.ID, "name", n.Name, "type", n.Type,
			"kind", ev.Kind, "monitor_id", ev.MonitorID, "attempts", attempts)
	}

	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if rerr := d.store.RecordDelivery(recordCtx, delivery); rerr != nil {
		d.log.Error("notify: record delivery", "notification_id", n.ID, "error", rerr)
	}
	if err != nil {
		return fmt.Errorf("%s (%s): %w", n.Name, n.Type, err)
	}
	return nil
}

// sleep waits for dur on the dispatcher clock, aborting when ctx ends.
func (d *Dispatcher) sleep(ctx context.Context, dur time.Duration) error {
	ch := make(chan struct{})
	t := d.clock.AfterFunc(dur, func() { close(ch) })
	select {
	case <-ctx.Done():
		t.Stop()
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Assignment helpers
// ---------------------------------------------------------------------------

// notificationsOf strips the escalation metadata off a slice of assignments.
func notificationsOf(assignments []Assignment) []Notification {
	out := make([]Notification, 0, len(assignments))
	for _, a := range assignments {
		out = append(out, a.Notification)
	}
	return out
}

// notificationsAtLevel returns the channels configured for one escalation
// delay (0 = immediate).
func notificationsAtLevel(assignments []Assignment, delay time.Duration) []Notification {
	var out []Notification
	for _, a := range assignments {
		if a.EscalationAfter == delay {
			out = append(out, a.Notification)
		}
	}
	return out
}

// escalationDelays returns the distinct positive escalation delays.
func escalationDelays(assignments []Assignment) []time.Duration {
	seen := make(map[time.Duration]bool)
	var out []time.Duration
	for _, a := range assignments {
		if a.EscalationAfter > 0 && !seen[a.EscalationAfter] {
			seen[a.EscalationAfter] = true
			out = append(out, a.EscalationAfter)
		}
	}
	return out
}
