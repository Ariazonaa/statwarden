// Package webhookq delivers JSON payloads to an operator-configured URL from a
// single background goroutine.
//
// It exists because two subsystems need the same machinery and neither may pay
// for it inline. internal/outbound POSTs a state change from an engine worker,
// whose contract (engine.Options.OnEvent) is to return immediately;
// internal/siem POSTs an audit event from an HTTP handler, which is holding a
// request open. In both cases the target is an operator-supplied URL — the least
// bounded thing in the process, because a target that accepts the connection and
// then says nothing holds the caller for the full timeout.
//
// So Enqueue never blocks and never fails: it hands the payload to one delivery
// goroutine and returns. One goroutine, not one per payload, so events reach the
// target in the order they happened.
package webhookq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Ariazonaa/statwarden/internal/safego"
)

// URLProvider yields the currently configured target URL. It is called once per
// delivery rather than captured at construction, so a settings change takes
// effect immediately. An empty string disables delivery.
//
// The error is what separates "nobody configured a target" from "the target
// could not be looked up". Both used to arrive as "", and the queue treated
// them the same way: silently. A settings read is a database round trip, so a
// database that is briefly unreachable — a failover, a restart, a connection
// storm — made every event in that window vanish with nothing in the log to
// show for it, which is the exact failure the drain and the queue-full warning
// exist to prevent. A provider that cannot answer must say so.
type URLProvider func(ctx context.Context) (string, error)

// Describer lets a payload name itself in the queue's log lines. A payload type
// that does not implement it is logged with the queue name alone, which says
// what was dropped but not which one.
type Describer interface {
	// LogAttrs returns alternating key/value pairs for slog.
	LogAttrs() []any
}

// Defaults applied when Options leaves a field zero.
const (
	// DefaultQueueSize bounds the hand-off to the delivery goroutine.
	// Generous, because these events are rare and dropping one is the failure
	// this buffer exists to avoid.
	DefaultQueueSize = 1024
	// DefaultTimeout bounds one POST.
	DefaultTimeout = 10 * time.Second
	// DefaultDrainBudget bounds how long shutdown spends flushing the queue.
	// An unresponsive target must not hold the process open.
	DefaultDrainBudget = 5 * time.Second
)

// Options configures a Queue.
type Options struct {
	// Name identifies the queue in log lines ("outbound webhook", "SIEM
	// forwarder"). Required in practice: it is the only thing distinguishing
	// two queues' messages.
	Name string
	// URL yields the target, re-read per delivery. Empty disables delivery.
	URL URLProvider
	// Secret yields the HMAC signing secret, re-read per delivery. Nil or an
	// empty secret sends unsigned; see SignatureHeader.
	Secret SecretProvider
	// Client sends the requests; defaults to a plain http.Client.
	Client *http.Client
	// Timeout bounds one POST.
	Timeout time.Duration
	// UserAgent is sent verbatim.
	UserAgent string
	// Logger receives delivery failures and drop warnings.
	Logger *slog.Logger
	// QueueSize and DrainBudget override the defaults above.
	QueueSize   int
	DrainBudget time.Duration
	// Now stamps signatures; defaults to time.Now. Tests pin it.
	Now func() time.Time
}

// Queue is a bounded, order-preserving delivery channel for payloads of one
// type. The zero value is not usable; call New.
type Queue[T any] struct {
	client      *http.Client
	urlFn       URLProvider
	secretFn    SecretProvider
	now         func() time.Time
	timeout     time.Duration
	drainBudget time.Duration
	userAgent   string
	name        string
	log         *slog.Logger

	queue chan T
	start sync.Once
	// started is set inside start.Do, so Wait can tell "the loop is running"
	// from "nobody ever wired this queue up" without racing the Once.
	started atomic.Bool
	// done is closed when the delivery loop returns, after its drain. Wait
	// blocks on it.
	done chan struct{}
	// stopped is set once the delivery loop has drained and returned. Enqueue
	// reads it so a payload arriving after that is reported rather than parked
	// in a channel nobody is reading any more; see Enqueue.
	stopped atomic.Bool
}

// New builds a Queue.
func New[T any](o Options) *Queue[T] {
	if o.Client == nil {
		o.Client = &http.Client{}
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.DrainBudget <= 0 {
		o.DrainBudget = DefaultDrainBudget
	}
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.URL == nil {
		o.URL = func(context.Context) (string, error) { return "", nil }
	}
	if o.Secret == nil {
		o.Secret = func(context.Context) (string, error) { return "", nil }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.UserAgent == "" {
		o.UserAgent = "Statwarden-Webhook/1"
	}
	if o.Name == "" {
		o.Name = "webhook"
	}
	return &Queue[T]{
		client: o.Client, urlFn: o.URL, secretFn: o.Secret, now: o.Now, timeout: o.Timeout,
		drainBudget: o.DrainBudget, userAgent: o.UserAgent,
		name: o.Name, log: o.Logger,
		queue: make(chan T, o.QueueSize),
		done:  make(chan struct{}),
	}
}

// Run delivers queued payloads until ctx ends, then drains what is left within
// the drain budget. It is started lazily by the first Enqueue, so a caller that
// never wires it up still works; starting it explicitly is how the delivery
// goroutine gets tied to the process lifetime instead of context.Background —
// which is what gives the queue a chance to flush at shutdown.
//
// Calling Run more than once is a no-op after the first: the loop belongs to
// whichever context started it.
func (q *Queue[T]) Run(ctx context.Context) {
	q.start.Do(func() {
		q.started.Store(true)
		safego.Go(q.log, q.name+" loop", func() { q.loop(ctx) })
	})
}

// Wait blocks until the delivery goroutine has finished its shutdown drain, or
// until ctx expires. It reports whether the drain completed.
//
// Nothing joined the goroutine before this existed, and the drain it performs
// is not free-standing: it POSTs, and the URL it POSTs to comes from a settings
// read, which needs the database. main defers pool.Close(), so the pool was
// being closed while the drain was still running — the flush that exists to
// stop a restart being a silent gap in the feed failed on a closed pool, which
// is the same gap with an error line in front of it. Whoever cancels the
// context has to wait for the goroutine that watches it.
//
// A queue that was never started has nothing to wait for and returns at once.
func (q *Queue[T]) Wait(ctx context.Context) error {
	if !q.started.Load() {
		return nil
	}
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		q.log.Warn(q.name + ": still draining when the shutdown budget ran out")
		return ctx.Err()
	}
}

func (q *Queue[T]) loop(ctx context.Context) {
	// Both deferred, so they hold however the loop ends — panic included, since
	// safego recovers ABOVE this function.
	//
	// stopped used to be set only on the ordinary path, after the drain. A panic
	// in drain's own logging left it false with no reader on the channel, so
	// Wait reported a clean drain and every later Enqueue parked a payload in a
	// queue nobody would ever read — silently, with not even the "shut down,
	// dropping a payload" line. It is set BEFORE done closes, so a caller
	// released by Wait can never observe the queue as still accepting.
	//
	// NOT covered by a test: reaching the drain's warn branch requires the loop
	// to lose a select race it wins most of the time, so every attempt at one
	// was green whether or not this line was here. It stays because it is one
	// word and the failure it prevents is silent.
	defer close(q.done)
	defer q.stopped.Store(true)

	// Sends never ride the lifecycle context.
	//
	// ctx says "stop looping"; it must not also cancel a request that has
	// already been taken off the queue. Using it for both meant a payload
	// picked up in the instant after cancellation was POSTed on a dead context
	// and failed immediately — the queue emptied and nothing arrived, which
	// looks exactly like delivery. Each send is bounded by q.timeout inside
	// Deliver, so shutdown is still bounded.
	sendCtx := context.WithoutCancel(ctx)

	for {
		select {
		case <-ctx.Done():
			q.drain(sendCtx)
			// Set here as well as in the defer: until the drain finishes, an
			// Enqueue still has a chance of being picked up, and calling those
			// lost would be the mirror image of the silence this flag exists to
			// end. The defer is the backstop for the panic path.
			q.stopped.Store(true)
			return
		case p := <-q.queue:
			q.deliverOne(sendCtx, p)
		}
	}
}

// deliverOne sends one payload with its panic contained, so a bug reached
// through one target's response cannot stop the feed for every later event.
func (q *Queue[T]) deliverOne(ctx context.Context, p T) {
	_ = safego.Do(q.log, q.name+" delivery", func() { q.send(ctx, p) })
}

// drain delivers what is still queued at shutdown, within a fixed budget.
//
// These payloads were accepted on the understanding that they would be sent;
// dropping them would make every restart a silent gap in the feed. The budget is
// what stops an unresponsive target holding the process open, and giving up is
// said out loud rather than left to be inferred.
func (q *Queue[T]) drain(sendCtx context.Context) {
	deadline := time.Now().Add(q.drainBudget)
	for {
		select {
		case p := <-q.queue:
			if time.Now().After(deadline) {
				q.log.Warn(q.name+": shutdown budget exhausted, dropping a queued payload",
					attrsOf(p)...)
				continue
			}
			q.deliverOne(sendCtx, p)
		default:
			return
		}
	}
}

func (q *Queue[T]) send(ctx context.Context, p T) {
	// The lookup is bounded by the same timeout as the delivery.
	//
	// sendCtx deliberately carries no cancellation (a shutdown must not abort a
	// POST that is already under way), and urlFn is a settings read — a database
	// round trip. Unbounded, a saturated pool wedged the delivery goroutine
	// here: it never returned to the select, never saw ctx.Done, never drained,
	// and Wait then burned the whole shutdown budget before main closed the pool
	// under it. Which is the exact failure Wait was added to remove.
	lookupCtx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	url, err := q.urlFn(lookupCtx)
	if err != nil {
		// Not the same as "no target configured", and it must not look like it:
		// the payload is being dropped because the lookup failed, and that is a
		// fixable condition somebody has to be told about.
		q.log.Error(q.name+": cannot read the target URL, dropping a payload",
			append(attrsOf(p), "error", err)...)
		return
	}
	if url == "" {
		return
	}
	// A secret that cannot be read drops the payload rather than sending it
	// unsigned. A receiver that verifies would reject it anyway, and one that
	// does not would be accepting exactly the unauthenticated POST the operator
	// configured a secret to rule out — a silent downgrade.
	secret, err := q.secretFn(lookupCtx)
	if err != nil {
		q.log.Error(q.name+": cannot read the signing secret, dropping a payload",
			append(attrsOf(p), "error", err)...)
		return
	}
	if err := q.Deliver(ctx, url, secret, p); err != nil {
		q.log.Error(q.name+": delivery failed", append(attrsOf(p), "error", err)...)
	}
}

// Enqueue queues one payload for delivery. It never blocks, and it is a no-op
// when no URL is configured — though the URL is read at delivery time, not here,
// so a settings change still takes effect immediately and the caller does not
// pay for the lookup.
func (q *Queue[T]) Enqueue(p T) {
	q.Run(context.Background())
	// After shutdown the loop is gone and nothing will ever read the channel
	// again, so a send here would park the payload in a buffer that is never
	// drained — lost, and silently, which is precisely what the drain and the
	// queue-full warning exist to prevent.
	//
	// This window is real rather than theoretical: the process context is
	// cancelled first and the HTTP server then finishes its in-flight requests,
	// every one of which can still audit. Best-effort about the exact instant —
	// a payload can lose the race between this check and the loop's last step —
	// but it turns the ordinary case from silence into a log line.
	if q.stopped.Load() {
		q.log.Warn(q.name+": shut down, dropping a payload queued after the drain",
			attrsOf(p)...)
		return
	}
	select {
	case q.queue <- p:
	default:
		// Only reachable when the target has been unresponsive long enough to
		// back up a full queue. Said out loud: a silent gap in an integration
		// feed is worse than a loud one.
		q.log.Warn(q.name+": queue full, dropping a payload", attrsOf(p)...)
	}
}

// Deliver POSTs one payload synchronously, signed with secret when it is not
// empty. Exported so a caller can send out-of-band — a "test this URL" button —
// without going through the queue.
//
// The signature covers the exact bytes sent: the body is marshalled once and
// both signed and written from that one slice, because a receiver verifies the
// raw body it got, and re-encoding it on either side is how a valid signature
// fails.
func (q *Queue[T]) Deliver(ctx context.Context, url, secret string, p T) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, q.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", q.userAgent)
	if secret != "" {
		req.Header.Set(SignatureHeader, Sign(secret, q.now(), body))
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: unexpected status %d", q.name, resp.StatusCode)
	}
	return nil
}

// attrsOf reads a payload's log attributes, or none when it does not describe
// itself.
func attrsOf(p any) []any {
	if d, ok := p.(Describer); ok {
		return d.LogAttrs()
	}
	return nil
}
