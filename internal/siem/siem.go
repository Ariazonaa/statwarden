// Package siem forwards audit events to an external log sink — a SIEM
// collector, an HTTP log shipper, an automation platform — as one JSON POST per
// event.
//
// It is the push half of the audit-log export; the pull half is the CSV
// download in internal/api. Both read the same audit entries, and neither is a
// substitute for the other: the CSV is for someone assembling evidence after the
// fact, this is for a collector that has to see the event while it is happening.
//
// Delivery is best-effort and asynchronous by design. An audit event is written
// to the database first and forwarded second, so a SIEM that is down, slow or
// misconfigured can never fail, delay or roll back the operation being audited —
// the local audit_log stays the record of truth, and this feed is a copy of it.
// A dropped event is logged loudly rather than swallowed.
package siem

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/Ariazonaa/statwarden/internal/webhookq"
)

// Event is the JSON body POSTed per audit entry.
//
// The field names deliberately do NOT mirror the audit_log columns one for one:
// this is a wire contract a third-party collector parses, so it carries an
// explicit `event` discriminator (a collector receiving several Statwarden feeds on
// one endpoint needs to tell them apart) and a schema version, and it names the
// timestamp `at` like every other outbound payload in this codebase.
type Event struct {
	Event   string `json:"event"`   // always "audit"
	Version int    `json:"version"` // payload schema version, currently 1

	// ID is the audit_log row id, so a collector can deduplicate a replay and
	// line an alert up against the CSV export. Zero when the store did not
	// report one.
	ID int64 `json:"id,omitempty"`

	Action     string `json:"action"`                // e.g. monitor.create, user.login
	EntityType string `json:"entity_type,omitempty"` // monitor, user, incident, …
	EntityID   *int64 `json:"entity_id,omitempty"`

	// UserID is the acting user, absent for an unauthenticated action (a failed
	// login, a password-reset request). It is the id alone, not a name or an
	// address: it is enough to join against the local user table, and the actor
	// of every event is the one field that would otherwise repeat PII on every
	// single message.
	UserID *int64 `json:"user_id,omitempty"`

	IP string `json:"ip,omitempty"`

	// Details is the audit row's blob, forwarded verbatim.
	//
	// It is operator-facing context, not a secret store, and the call sites are
	// deliberately narrow about what they put there: names, types, counts, a
	// setting's KEY (never its value), a provider id. No password, token, API key
	// or channel config has ever been written to it, and none may be — this field
	// leaves the instance.
	//
	// It does carry PII where the event is about a person: `user.create` and
	// `user.password_reset_request` include the e-mail address, and the OIDC
	// events include the provider's subject claim. That is the point of an audit
	// feed rather than an oversight, but it is why the collector URL is an
	// admin-only setting and why this comment says so out loud.
	Details json.RawMessage `json:"details,omitempty"`

	At time.Time `json:"at"`

	// Instance names the sending Statwarden, so one collector can serve several.
	// Empty when the operator has not set an instance name.
	Instance string `json:"instance,omitempty"`
}

// eventVersion is the current Event schema version. Bump it only for a change a
// collector must notice; adding an optional field does not qualify.
const eventVersion = 1

// LogAttrs names the event in the queue's drop and failure logs. The action and
// the row id are enough to find the entry that did not make it; the details blob
// is deliberately left out of the log line.
func (e Event) LogAttrs() []any {
	return []any{"audit_id", e.ID, "action", e.Action}
}

// InstanceProvider yields the configured instance name. It is consulted at most
// once per instanceTTL (see below), not per event. May be nil.
//
// The error exists so a failed lookup is not cached as an answer. See
// Forwarder.cachedInstance.
type InstanceProvider func(ctx context.Context) (string, error)

// Options configures a Forwarder.
type Options struct {
	// URL yields the collector endpoint, re-read per delivery. Empty disables
	// forwarding, which is the default state of an instance nobody configured.
	URL webhookq.URLProvider
	// Secret yields the HMAC signing secret; empty or nil sends unsigned.
	Secret webhookq.SecretProvider
	// Instance yields the instance name stamped on each event. Optional.
	Instance InstanceProvider
	Client   *http.Client
	Timeout  time.Duration
	Logger   *slog.Logger
}

// instanceTTL is how long a resolved instance name is reused.
//
// The provider is a settings read, i.e. a database round trip, and HandleAudit
// runs inside an HTTP handler that has already paid for the audit INSERT. Every
// mutating request in the product goes through that path, so re-reading a value
// that changes when someone renames the instance would be a round trip per
// request for a string that is stable for months. A rename reaches the feed
// within this window instead of instantly, which is the right trade for a label.
const instanceTTL = 30 * time.Second

// Forwarder queues audit events for delivery to the configured collector.
type Forwarder struct {
	q        *webhookq.Queue[Event]
	instance InstanceProvider
	log      *slog.Logger

	mu           sync.Mutex
	instanceName string
	instanceAt   time.Time
	now          func() time.Time
}

// New builds a Forwarder.
func New(o Options) *Forwarder {
	if o.Instance == nil {
		o.Instance = func(context.Context) (string, error) { return "", nil }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Forwarder{
		q: webhookq.New[Event](webhookq.Options{
			Name:      "SIEM forwarder",
			URL:       o.URL,
			Secret:    o.Secret,
			Client:    o.Client,
			Timeout:   o.Timeout,
			Logger:    o.Logger,
			UserAgent: "Statwarden-Audit/1",
		}),
		instance: o.Instance,
		log:      o.Logger,
		now:      time.Now,
	}
}

// cachedInstance returns the instance name, refreshing it at most once per
// instanceTTL. The zero instanceAt makes the first call always a miss.
//
// The provider call happens with NO LOCK HELD, and that is the whole shape of
// this function. The provider is a settings read — a database round trip — and
// HandleAudit runs inside an HTTP handler that has already written its audit
// row. Holding the mutex across that read made every concurrent mutating request
// queue behind one query, after their own work had already succeeded; a database
// that merely got slow would have shown up as latency on every write in the
// product, contributed by the one component whose contract is "never blocks".
//
// A FAILED read is not an answer and is never cached. It used to be: the
// provider returned "" for a database it could not reach, that "" was written to
// the cache with a fresh timestamp, and every event for the next thirty seconds
// was stamped with no instance at all — on a collector serving several Statwardens,
// thirty seconds of events that cannot be attributed to any of them. The last
// good name is kept instead, stale but true, and the expiry is left in place so
// the next event retries.
//
// The concurrent-refresh window is deliberate and bounded by concurrency: two
// callers arriving together at expiry each do one indexed lookup. Whichever
// finishes last wins, and since both read the same row that is the same value —
// unless one of them failed, which is exactly why a failure returns early
// instead of writing.
func (f *Forwarder) cachedInstance(ctx context.Context) string {
	now := f.now()
	f.mu.Lock()
	name, at := f.instanceName, f.instanceAt
	f.mu.Unlock()
	if !at.IsZero() && now.Sub(at) < instanceTTL {
		return name
	}

	fresh, err := f.instance(ctx)
	if err != nil {
		// The NAME is never updated on a failure — a failed read is not an
		// answer, and caching "" as one stamped thirty seconds of events with no
		// instance at all.
		//
		// The ATTEMPT time is backed off, but ONLY when there is a last known
		// good name to serve in the meantime. Two things went wrong with the
		// first attempt at this:
		//
		//   - Backing off from a COLD cache served "" for the full TTL, silently,
		//     which is the very bug the paragraph above describes. A single
		//     cancelled request during startup was enough. With no name to fall
		//     back on there is nothing to protect, so it retries every event —
		//     noisy, but never wrong.
		//   - The timestamp was the one captured BEFORE the provider call, so a
		//     read that took longer than the TTL wrote a backoff that had already
		//     expired. Under a slow database — precisely the case the backoff
		//     exists for — the query storm came straight back. It is taken after
		//     the call returns.
		if !at.IsZero() {
			f.mu.Lock()
			f.instanceAt = f.now()
			f.mu.Unlock()
		}
		f.log.Warn("SIEM: cannot read the instance name, keeping the last known one",
			"error", err, "instance", name)
		return name
	}

	f.mu.Lock()
	// Stamped AFTER the call, for the same reason the failure path is: `now` was
	// captured before it, so a read slower than the TTL wrote an entry that was
	// already expired and the next event queried again. The failure path was
	// corrected and this one was left, on the identical slow-database condition.
	f.instanceName, f.instanceAt = fresh, f.now()
	f.mu.Unlock()
	return fresh
}

// Run delivers queued events until ctx ends. Started explicitly at boot so the
// delivery goroutine dies with the process and gets its shutdown drain; left to
// start itself lazily it would run on context.Background and simply vanish at
// exit, taking its queue with it.
func (f *Forwarder) Run(ctx context.Context) { f.q.Run(ctx) }

// Wait blocks until the delivery goroutine has finished its shutdown drain, or
// until ctx expires. See webhookq.Queue.Wait: the drain needs the database, so
// whoever closes the pool has to wait for it first.
func (f *Forwarder) Wait(ctx context.Context) error { return f.q.Wait(ctx) }

// HandleAudit queues one audit event. It never blocks and never fails: the
// caller is an HTTP handler that has already persisted the entry and must
// answer its request.
//
// The discriminator and version are stamped here rather than left to the caller
// so no call site can forget them, and an Instance the caller already set is
// respected — that is the seam a test writes through.
func (f *Forwarder) HandleAudit(ctx context.Context, e Event) {
	e.Event = "audit"
	e.Version = eventVersion
	if e.Instance == "" {
		e.Instance = f.cachedInstance(ctx)
	}
	f.q.Enqueue(e)
}
