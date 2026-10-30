package notify

import "time"

// Clock abstracts time for the dispatcher (resend, escalation and digest
// timers) so tests can drive it deterministically with a fake implementation.
// It mirrors the engine's Clock; the packages stay independent on purpose.
type Clock interface {
	Now() time.Time
	// AfterFunc arms a one-shot timer that runs f in its own goroutine
	// after d, mirroring time.AfterFunc.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the stoppable handle returned by Clock.AfterFunc.
type Timer interface {
	// Stop prevents the timer from firing; it reports whether the call
	// stopped it before it fired, mirroring time.Timer.Stop.
	Stop() bool
}

// realClock is the production Clock backed by package time.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
