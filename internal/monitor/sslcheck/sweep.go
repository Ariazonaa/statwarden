package sslcheck

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/Ariazonaa/statwarden/internal/safego"
)

// DefaultThresholds are the day marks at which expiry warnings fire, per
// docs/architecture.md.
var DefaultThresholds = []int{21, 14, 7, 3, 1}

// ActiveThreshold returns the tightest threshold that daysRemaining has
// crossed: the smallest t with daysRemaining <= t. ok is false while the
// certificate is still comfortably valid. Expired certificates (negative
// days) report threshold 0.
func ActiveThreshold(daysRemaining int, thresholds []int) (threshold int, ok bool) {
	if daysRemaining < 0 {
		return 0, true
	}
	sorted := append([]int(nil), thresholds...)
	sort.Ints(sorted)
	for _, t := range sorted {
		if daysRemaining <= t {
			return t, true
		}
	}
	return 0, false
}

// CertRecord is one certificate as seen by the sweep.
type CertRecord struct {
	MonitorID   int64
	MonitorName string
	Issuer      string
	ValidTo     time.Time
}

// Source lists the certificates to sweep; PgCertSource implements it against
// the cert_info table.
type Source interface {
	ListCertInfo(ctx context.Context) ([]CertRecord, error)
}

// Event is emitted when a certificate crosses an expiry threshold (or has
// expired). The notification dispatcher (a later work package) turns these
// into alerts.
type Event struct {
	MonitorID     int64
	MonitorName   string
	Issuer        string
	ValidTo       time.Time
	DaysRemaining int
	// Threshold is the crossed day mark; 0 together with Expired means
	// the certificate is already invalid.
	Threshold int
	Expired   bool
}

// Sweeper runs the daily certificate-expiry scan. It de-duplicates alerts in
// memory: each monitor fires once per crossed threshold, then again only when
// the next tighter threshold is crossed (or the certificate is renewed, which
// resets tracking).
type Sweeper struct {
	src        Source
	onEvent    func(Event)
	log        *slog.Logger
	thresholds []int
	interval   time.Duration
	now        func() time.Time

	// notified maps monitor ID to the last alert raised for its current
	// certificate; state is process-local by design — a restart re-alerts
	// at most once.
	notified map[int64]notifyState
}

// notifyState records the tightest threshold already alerted on for a
// specific certificate. ValidTo identifies the certificate: when a monitor's
// certificate is replaced (renewed, re-keyed) its expiry date changes, which
// resets the de-duplication so the new certificate alerts from scratch even
// if it is already inside the warning window.
type notifyState struct {
	threshold int
	validTo   time.Time
}

// SweeperOptions configures NewSweeper. Source is required.
type SweeperOptions struct {
	Source Source
	// OnEvent receives threshold crossings; must be fast and non-blocking.
	OnEvent func(Event)
	Logger  *slog.Logger
	// Thresholds defaults to DefaultThresholds.
	Thresholds []int
	// Interval defaults to 24h.
	Interval time.Duration
	// Now defaults to time.Now (overridable in tests).
	Now func() time.Time
}

// NewSweeper builds a Sweeper.
func NewSweeper(opts SweeperOptions) *Sweeper {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if len(opts.Thresholds) == 0 {
		opts.Thresholds = DefaultThresholds
	}
	if opts.Interval <= 0 {
		opts.Interval = 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sweeper{
		src:        opts.Source,
		onEvent:    opts.OnEvent,
		log:        opts.Logger,
		thresholds: opts.Thresholds,
		interval:   opts.Interval,
		now:        opts.Now,
		notified:   make(map[int64]notifyState),
	}
}

// Run performs one sweep immediately, then repeats every interval until ctx
// is cancelled. Intended to run as a goroutine next to the engine.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		// Contained per sweep, not around the loop. The caller wraps Run in
		// safego.Go, which recovers and then lets the goroutine end, so one panic
		// used to mean no certificate-expiry warning was ever emitted again — a
		// silent hole in exactly the alert whose whole purpose is to fire weeks
		// before anybody would otherwise notice.
		_ = safego.Do(s.log, "cert expiry sweep", func() {
			if err := s.RunOnce(ctx); err != nil {
				s.log.Error("cert expiry sweep failed", "error", err)
			}
		})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce scans all certificates once and emits events for newly crossed
// thresholds.
func (s *Sweeper) RunOnce(ctx context.Context) error {
	records, err := s.src.ListCertInfo(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	seen := make(map[int64]struct{}, len(records))
	for _, rec := range records {
		seen[rec.MonitorID] = struct{}{}
		days := DaysRemaining(rec.ValidTo, now)
		threshold, crossed := ActiveThreshold(days, s.thresholds)
		if !crossed {
			// Comfortably valid (again) — e.g. after renewal.
			delete(s.notified, rec.MonitorID)
			continue
		}
		if last, ok := s.notified[rec.MonitorID]; ok &&
			last.validTo.Equal(rec.ValidTo) && threshold >= last.threshold {
			continue // same cert, already alerted at this or a tighter threshold
		}
		s.notified[rec.MonitorID] = notifyState{threshold: threshold, validTo: rec.ValidTo}
		ev := Event{
			MonitorID:     rec.MonitorID,
			MonitorName:   rec.MonitorName,
			Issuer:        rec.Issuer,
			ValidTo:       rec.ValidTo,
			DaysRemaining: days,
			Threshold:     threshold,
			Expired:       days < 0,
		}
		s.log.Warn("certificate approaching expiry",
			"monitor_id", ev.MonitorID, "name", ev.MonitorName,
			"days_remaining", ev.DaysRemaining, "threshold", ev.Threshold, "expired", ev.Expired)
		if s.onEvent != nil {
			s.onEvent(ev)
		}
	}
	// Forget monitors whose certificates disappeared (deleted/paused).
	for id := range s.notified {
		if _, ok := seen[id]; !ok {
			delete(s.notified, id)
		}
	}
	return nil
}
