package api

import (
	"context"
	"time"

	"github.com/Ariazonaa/statwarden/internal/domain"
	"github.com/Ariazonaa/statwarden/internal/maintenance"
	"github.com/Ariazonaa/statwarden/internal/notify"
)

// Engine is the slice of the monitor engine the API drives. Creating or
// editing a monitor calls Upsert (live reload); deleting calls Remove;
// pause/resume and "check now" map to the matching methods. *engine.Engine
// satisfies this; handler tests use a recording fake.
type Engine interface {
	Upsert(m domain.Monitor) error
	Remove(id int64)
	Pause(id int64) error
	Resume(id int64) error
	CheckNow(id int64) error
	StatusOf(id int64) (domain.MonitorStatus, bool)
	// Scheduling reports how many monitors the engine has scheduled and how many
	// it had to skip. /readyz reads it: an uptime monitor that could not schedule
	// a single one of its monitors is not ready, however healthy its database
	// connection looks.
	Scheduling() (scheduled, unscheduled int)
}

// Dispatcher is the slice of the notification dispatcher the API drives: the
// "send a test message" button, and the per-monitor state it has to be told to
// drop. *notify.Dispatcher satisfies this.
type Dispatcher interface {
	TestSend(ctx context.Context, n notify.Notification) error
	// Forget drops the dispatcher's per-monitor state (an open outage with its
	// resend timer, a mute) AND records that the monitor is gone, so an event
	// already in flight for it — queued, or replayed from the durable outbox —
	// cannot rebuild that state. Called on a delete and on a purge.
	//
	// Without it the outage open at the moment of the delete never closes: the
	// recovery event that would close it needs a check, and the monitor is
	// unscheduled.
	Forget(monitorID int64)
	// Reset is Forget's counterpart for a monitor that is STILL there — paused,
	// or brought back from the bin. It clears the same state and leaves the
	// monitor able to alert immediately.
	//
	// Pausing needs it: the engine deliberately emits no event for a pause, so
	// nothing ever closed the outage of a monitor that was DOWN when it was
	// paused, and its resend timer went on announcing it.
	Reset(monitorID int64)
}

// MaintenanceProvider is the shared maintenance evaluator, the same instance
// the monitor engine suppresses alerts with. The API drives it from both ends:
// it refreshes it after every maintenance mutation, and it reads ActiveAt when
// rendering a public status page, so the page and the engine can never disagree
// about which windows are currently in effect.
//
// May be nil, in which case no window is ever in effect.
// *maintenance.Provider satisfies this.
type MaintenanceProvider interface {
	ActiveAt(at time.Time) []maintenance.Window
	Refresh(ctx context.Context) error
}
