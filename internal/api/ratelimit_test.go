package api

import (
	"fmt"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock so the limiter's time-based sweep can
// be exercised deterministically without real sleeps.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(limit int, per time.Duration) (*rateLimiter, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newRateLimiter(limit, per)
	l.now = clk.now
	return l, clk
}

func newTestLockout(threshold int, duration time.Duration) (*lockout, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newLockout(threshold, duration)
	l.now = clk.now
	return l, clk
}

// TestLockoutSweepBoundsMap is the regression test for the second leak. The
// bucket key is (login as typed, client IP) and reset only runs on a SUCCESSFUL
// login, so every distinct identifier ever guessed from every source address
// used to stay resident forever — growth an unauthenticated caller controls.
func TestLockoutSweepBoundsMap(t *testing.T) {
	const (
		threshold = 5
		duration  = 15 * time.Minute
	)
	l, clk := newTestLockout(threshold, duration)

	// 50 windows, each with 100 fresh (login, IP) pairs that only ever fail.
	const windows, perWindow = 50, 100
	for wnd := 0; wnd < windows; wnd++ {
		for i := 0; i < perWindow; i++ {
			l.fail(loginKey(fmt.Sprintf("user%d-%d@example.test", wnd, i), fmt.Sprintf("10.%d.%d.1", wnd%256, i)))
		}
		clk.add(duration + time.Second)
	}

	l.mu.Lock()
	got := len(l.fails)
	l.mu.Unlock()

	if max := 2 * perWindow; got > max {
		t.Fatalf("lockout map grew to %d entries, want <= %d (expired buckets not swept)", got, max)
	}
}

// TestLockoutCounterDecays pins the behaviour change that came with the sweep:
// the threshold counts failures within one window, not failures ever. Four
// attempts a year apart used to add up towards the fifth.
func TestLockoutCounterDecays(t *testing.T) {
	const (
		threshold = 5
		duration  = 15 * time.Minute
	)
	l, clk := newTestLockout(threshold, duration)
	key := loginKey("ops@example.test", "203.0.113.9")

	// Four failures, one per window: never locks, however long this runs.
	for i := 0; i < 4; i++ {
		if l.fail(key) {
			t.Fatalf("attempt %d locked, but each is a window apart", i+1)
		}
		clk.add(duration + time.Second)
	}
	if l.locked(key) {
		t.Fatal("spaced-out failures locked the bucket")
	}

	// Consecutive failures inside one window still lock on the threshold.
	for i := 1; i < threshold; i++ {
		if l.fail(key) {
			t.Fatalf("locked early on attempt %d of %d", i, threshold)
		}
	}
	if !l.fail(key) {
		t.Fatalf("attempt %d did not lock", threshold)
	}
	if !l.locked(key) {
		t.Fatal("locked() disagrees with fail()")
	}

	// And the lock lifts once the cool-off elapses.
	clk.add(duration + time.Second)
	if l.locked(key) {
		t.Fatal("still locked after the cool-off elapsed")
	}
}

// TestLockoutSweepKeepsLiveLock guards the mirror image of the leak: a sweep
// triggered by unrelated traffic must not release a bucket that is still
// serving its cool-off.
func TestLockoutSweepKeepsLiveLock(t *testing.T) {
	const (
		threshold = 3
		duration  = 15 * time.Minute
	)
	l, clk := newTestLockout(threshold, duration)
	key := loginKey("victim@example.test", "198.51.100.4")

	for i := 1; i < threshold; i++ {
		l.fail(key)
	}
	if !l.fail(key) {
		t.Fatal("threshold reached but not locked")
	}

	// Force the next call to sweep, then look again from inside the cool-off.
	l.mu.Lock()
	l.lastSweep = clk.t.Add(-duration)
	l.mu.Unlock()
	clk.add(duration / 2)

	if !l.locked(key) {
		t.Fatal("sweep released a bucket that was still locked")
	}
}

// TestRateLimiterSweepBoundsMap is the regression test for the leak: expired IP
// keys must be reclaimed so that a stream of distinct source IPs across many
// windows cannot grow the map without bound. Without the sweep in allow, every
// distinct key stays resident forever and this assertion fails.
func TestRateLimiterSweepBoundsMap(t *testing.T) {
	const per = time.Minute
	l, clk := newTestLimiter(5, per)

	// Simulate 50 windows, each visited by 100 brand-new source IPs. That is
	// 5000 distinct keys over the run; a leaking limiter would retain all of
	// them. With sweeping, only the current (and at most the immediately
	// preceding) window's keys should ever be resident.
	const windows, perWindow = 50, 100
	for wnd := 0; wnd < windows; wnd++ {
		for i := 0; i < perWindow; i++ {
			l.allow(fmt.Sprintf("public:10.%d.%d.1", wnd%256, i))
		}
		clk.add(per + time.Second)
	}

	l.mu.Lock()
	got := len(l.hits)
	l.mu.Unlock()

	// Bound: keys from the last window plus, conservatively, the window before
	// it. Anything near windows*perWindow means expired keys were never freed.
	if max := 2 * perWindow; got > max {
		t.Fatalf("limiter map grew to %d entries, want <= %d (expired keys not swept)", got, max)
	}
}

// TestRateLimiterLimitsAfterSweep guards against a sweep that reclaims live
// keys or otherwise breaks enforcement: limiting must still work after a sweep
// has run.
func TestRateLimiterLimitsAfterSweep(t *testing.T) {
	const (
		limit = 3
		per   = time.Minute
	)
	l, clk := newTestLimiter(limit, per)

	// Fill the map with many one-shot keys, then advance past the window so a
	// subsequent allow triggers a sweep that clears them.
	for i := 0; i < 500; i++ {
		l.allow(fmt.Sprintf("public:172.16.%d.%d", i/256, i%256))
	}
	clk.add(per + time.Second)

	// First call in the new window triggers the sweep and counts as hit #1 for
	// the victim key.
	if !l.allow("public:203.0.113.7") {
		t.Fatal("first request after sweep should be allowed")
	}
	for i := 2; i <= limit; i++ {
		if !l.allow("public:203.0.113.7") {
			t.Fatalf("request %d should be within the limit of %d", i, limit)
		}
	}
	// The (limit+1)th request in the same window must be rejected: the sweep
	// must not have wiped the live counter for this key.
	if l.allow("public:203.0.113.7") {
		t.Fatalf("request %d should exceed the limit of %d", limit+1, limit)
	}

	// After the window elapses the key resets and is allowed again.
	clk.add(per + time.Second)
	if !l.allow("public:203.0.113.7") {
		t.Fatal("request should be allowed after the window resets")
	}
}
