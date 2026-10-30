// Package safego contains a panic from a background goroutine instead of
// letting it end the process.
//
// An unrecovered panic in ANY goroutine terminates a Go program. In a service
// that mostly answers HTTP that is largely theoretical, because net/http
// recovers panics per connection — but almost nothing here runs in an HTTP
// handler. The engine checks monitors, the dispatcher delivers alerts, the hub
// pushes updates, and several jobs run on timers, all on goroutines with
// nothing above them. Any one of them could take down the process whose entire
// job is to still be running when something else fails.
//
// The unit passed in should be one iteration of work, not the loop around it.
// A loop that is itself wrapped stops at the first panic, which turns "one bad
// event" into "notifications are off until someone restarts the service" —
// quieter than a crash and harder to notice.
package safego

import (
	"log/slog"
	"runtime/debug"
)

// Do runs fn and reports whether it panicked, logging the panic and its stack.
//
// The stack matters: the recovered value names what went wrong and never says
// where, and a panic in this codebase is a bug that somebody has to find.
func Do(log *slog.Logger, what string, fn func()) (panicked bool) {
	if log == nil {
		log = slog.Default()
	}
	defer func() {
		if rec := recover(); rec != nil {
			panicked = true
			log.Error("recovered from a panic", "in", what,
				"recover", rec, "stack", string(debug.Stack()))
		}
	}()
	fn()
	return false
}

// Go starts fn on a new goroutine under Do.
func Go(log *slog.Logger, what string, fn func()) {
	go func() { _ = Do(log, what, fn) }()
}
