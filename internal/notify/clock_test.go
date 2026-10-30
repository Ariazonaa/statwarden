package notify

import (
	"sync"
	"time"
)

// fakeClock is a deterministic Clock for tests: time only moves when Advance
// is called, which fires due timers in chronological order. It mirrors the
// engine's test clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clk     *fakeClock
	when    time.Time
	f       func()
	fired   bool
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clk: c, when: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves the clock forward by d, firing due timers in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if t.fired || t.stopped || t.when.After(target) {
				continue
			}
			if next == nil || t.when.Before(next.when) {
				next = t
			}
		}
		if next == nil {
			break
		}
		if next.when.After(c.now) {
			c.now = next.when
		}
		next.fired = true
		f := next.f
		c.mu.Unlock()
		f()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

// SetTime jumps the clock to an absolute time without firing timers (used to
// place "now" inside/outside a quiet-hours window).
func (c *fakeClock) SetTime(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// LiveTimers counts the timers still due to fire. It is how a test sees that a
// monitor has exactly ONE reminder chain rather than several: an orphaned chain
// is invisible in delivery counts until it fires.
func (c *fakeClock) LiveTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.fired && !t.stopped {
			n++
		}
	}
	return n
}
