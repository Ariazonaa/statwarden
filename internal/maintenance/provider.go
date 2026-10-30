package maintenance

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Ariazonaa/statwarden/internal/safego"
)

// DefaultRefreshInterval is how often the provider rebuilds its snapshot when
// no interval is configured. Windows are edited by hand and evaluated to the
// second, so a minute of staleness is the worst a missed invalidation can cost.
const DefaultRefreshInterval = time.Minute

// Loader reads the current maintenance windows from persistence.
//
// It reports HARD failures only — the store being unreachable. A single row it
// cannot make sense of is the loader's own business to log and skip, because
// the Provider keeps the previous snapshot whenever a load fails and must not
// confuse "one unusable window" with "the database is gone".
type Loader func(ctx context.Context) ([]Window, error)

// ProviderOptions configures NewProvider. Loader is required.
type ProviderOptions struct {
	Loader   Loader
	Logger   *slog.Logger
	Interval time.Duration
}

// Provider owns the live Snapshot: it loads windows on an interval and on
// demand, and hands out lock-free reads.
//
// The engine asks InMaintenance on every state transition, so a read must not
// touch the database and must not contend on a mutex — the snapshot is swapped
// atomically and readers only ever load a pointer.
//
// A nil *Provider is usable and reports no maintenance, so a caller that never
// configured one (DEV_MODE, tests) needs no special case.
type Provider struct {
	loader   Loader
	log      *slog.Logger
	interval time.Duration
	snap     atomic.Pointer[Snapshot]
}

// NewProvider builds a provider around a loader. The snapshot starts empty;
// call Start (or Refresh) to fill it.
func NewProvider(o ProviderOptions) *Provider {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Interval <= 0 {
		o.Interval = DefaultRefreshInterval
	}
	return &Provider{loader: o.Loader, log: o.Logger, interval: o.Interval}
}

// Refresh rebuilds the snapshot now. Handlers call it after a maintenance
// window is created, updated or deleted so the change takes effect immediately
// instead of at the next tick.
//
// On a load failure the PREVIOUS snapshot is kept: a transient database error
// must not silently lift every maintenance window and let alerts through.
// Windows that fail to compile are skipped and logged, and the rest still take
// effect — one bad row cannot disable maintenance for everything else.
func (p *Provider) Refresh(ctx context.Context) error {
	if p == nil || p.loader == nil {
		return nil
	}
	windows, err := p.loader(ctx)
	if err != nil {
		return err
	}
	snap, compileErr := NewSnapshot(windows)
	if compileErr != nil {
		p.log.Error("maintenance: ignoring invalid windows", "error", compileErr)
	}
	p.snap.Store(snap)
	return nil
}

// Start refreshes once and then on the interval until ctx is done. The initial
// failure is logged rather than returned: a database hiccup at boot must not
// stop the process, and the next tick recovers.
func (p *Provider) Start(ctx context.Context) {
	if p == nil {
		return
	}
	if err := p.Refresh(ctx); err != nil {
		p.log.Error("maintenance: initial load", "error", err)
	}
	safego.Go(p.log, "maintenance provider", func() { p.run(ctx) })
}

func (p *Provider) run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = safego.Do(p.log, "maintenance refresh", func() {
				if err := p.Refresh(ctx); err != nil {
					p.log.Error("maintenance: refresh", "error", err)
				}
			})
		}
	}
}

// Snapshot returns the current snapshot, which may be nil before the first
// successful load. *Snapshot is nil-safe, so the result is always usable.
func (p *Provider) Snapshot() *Snapshot {
	if p == nil {
		return nil
	}
	return p.snap.Load()
}

// InMaintenance implements engine.MaintenanceEvaluator against the live
// snapshot. It suppresses ALERTS only; probing and heartbeat recording are
// unaffected.
func (p *Provider) InMaintenance(monitorID int64, at time.Time) bool {
	return p.Snapshot().InMaintenance(monitorID, at)
}

// ActiveAt returns the windows in effect at at. The public status page uses it
// so that "in maintenance" means there exactly what it means in the engine.
func (p *Provider) ActiveAt(at time.Time) []Window {
	return p.Snapshot().ActiveAt(at)
}
